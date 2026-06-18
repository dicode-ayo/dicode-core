# dicode permission guard — PEP 578 audit hook enforcing the task's declared
# permissions.{fs,net,run}. Injected after the SDK shim (so the SDK's own
# socket setup is ungoverned) and before the task body.
#
# Guardrail, not a security boundary: the task author is trusted. The hook
# enforces declared intent and catches accidental over-reach; escapability is
# acceptable.
#
# Known limitations:
#   - fs reads are not enforced: the interpreter, stdlib, and site-packages
#     read files constantly, so an in-interpreter read allowlist would break
#     normal execution.
#   - net checks rely on socket.connect / socket.getaddrinfo audit events;
#     pooled connections or some async stacks may not re-emit them, and IP
#     literals pass the allowlist (hostname vetting happens at getaddrinfo,
#     and the subsequent connect arrives with the resolved IP, which cannot
#     be correlated back to the hostname).
#   - C extensions that touch files or sockets without going through os/io
#     (e.g. sqlite3's own file I/O) do not emit audit events.
def _dicode_install_guard():
    import ipaddress as _ipaddress
    import json as _json
    import os as _os
    import socket as _socket
    import sys as _sys

    policy = _json.loads("__DICODE_POLICY__")

    net_mode = policy["net"]["mode"]  # unrestricted | allowlist | deny
    run_mode = policy["run"]["mode"]  # allow | allowlist | deny

    net_rules = []  # (lowercase host, port or None)
    for entry in policy["net"].get("hosts") or []:
        e = entry.strip()
        if e.startswith("["):  # [ipv6] or [ipv6]:port
            host, _, rest = e[1:].partition("]")
            port = rest[1:] if rest.startswith(":") else ""
        elif e.count(":") == 1:  # host:port (bare IPv6 has >1 colon)
            host, _, port = e.partition(":")
        else:
            host, port = e, ""
        net_rules.append((host.lower(), int(port) if port.isdigit() else None))

    run_allowed = set(policy["run"].get("commands") or [])

    # The interpreter writes bytecode caches and venv metadata under its own
    # prefixes during normal imports; those writes are always permitted.
    write_roots = [_os.path.abspath(p) for p in policy.get("fs_write") or []]
    for p in (_sys.prefix, _sys.base_prefix, _sys.exec_prefix):
        if p:
            write_roots.append(_os.path.abspath(p))

    # Approval-gate state (dicode.lock, dicode.yaml). These must never be
    # writable even when a broad write grant covers their directory, so the
    # deny set is checked before the allowlist. Paths are canonicalised
    # (realpath) so a write reaching the lock via a symlinked config dir still
    # matches the deny entry.
    write_denied = {_os.path.realpath(p) for p in policy.get("fs_deny") or []}

    sep = _os.sep
    af_unix = getattr(_socket, "AF_UNIX", None)
    write_flags = (
        _os.O_WRONLY | _os.O_RDWR | _os.O_APPEND | _os.O_CREAT | _os.O_TRUNC
    )

    def _to_str(v):
        if isinstance(v, bytes):
            return _os.fsdecode(v)
        return v if isinstance(v, str) else None

    def _write_allowed(p):
        n = _to_str(p)
        if n is None:
            return True  # fd-based op; no path to match against
        # Compare against the deny set on the canonical (realpath) form so a
        # write reaching a protected path through a symlink still matches.
        if _os.path.realpath(n) in write_denied:
            return False
        n = _os.path.abspath(n)
        if n.endswith(".pyc") or (sep + "__pycache__") in n:
            return True
        for root in write_roots:
            if n == root or n.startswith(root + sep):
                return True
        return False

    def _check_write(p, op):
        if not _write_allowed(p):
            raise PermissionError(
                f"dicode: {op} on {p!r} denied — declare the path with "
                f'permission "w" or "rw" under permissions.fs in task.yaml'
            )

    def _is_ip_literal(h):
        try:
            _ipaddress.ip_address(h.strip("[]"))
            return True
        except ValueError:
            return False

    def _host_allowed(host, port):
        h = _to_str(host)
        if h is None:
            return True
        h = h.lower()
        if _is_ip_literal(h):
            return True
        for rule_host, rule_port in net_rules:
            if h == rule_host and (
                rule_port is None or port is None or rule_port == port
            ):
                return True
        return False

    def _deny_net(host, port):
        raise PermissionError(
            f"dicode: network access to {host!r} port {port!r} denied — add "
            f'the host to permissions.net in task.yaml (or use ["*"])'
        )

    def _check_run(cmd):
        if run_mode == "allow":
            return
        c = _to_str(cmd)
        if c is not None and (
            c in run_allowed or _os.path.basename(c) in run_allowed
        ):
            return
        raise PermissionError(
            f"dicode: spawning {cmd!r} denied — add the command to "
            f'permissions.run in task.yaml (or use ["*"])'
        )

    def _hook(event, args):
        if event == "open":
            mode, flags = args[1], args[2]
            if mode is None:
                is_write = bool((flags or 0) & write_flags)
            else:
                is_write = any(c in mode for c in "wax+")
            if is_write:
                _check_write(args[0], "open for writing")
        elif event in (
            "os.remove",
            "os.rmdir",
            "os.mkdir",
            "os.truncate",
            "shutil.rmtree",
        ):
            _check_write(args[0], event)
        elif event in ("os.rename", "shutil.move"):
            _check_write(args[0], event)
            _check_write(args[1], event)
        elif event in ("os.symlink", "os.link"):
            # The mutation is the new link at the destination (args[1]).
            _check_write(args[1], event)
        elif event in ("os.chmod", "os.chown"):
            _check_write(args[0], event)
        elif event == "socket.connect":
            if net_mode == "unrestricted":
                return
            sock, addr = args[0], args[1]
            # AF_UNIX (including the dicode IPC socket) is always allowed.
            if af_unix is not None and getattr(sock, "family", None) == af_unix:
                return
            if isinstance(addr, (str, bytes)):
                return  # unix path address
            host = addr[0] if isinstance(addr, tuple) and len(addr) > 0 else None
            port = addr[1] if isinstance(addr, tuple) and len(addr) > 1 else None
            if net_mode == "deny" or not _host_allowed(host, port):
                _deny_net(host, port)
        elif event == "socket.getaddrinfo":
            if net_mode == "unrestricted":
                return
            host, port = args[0], args[1]
            if host is None:
                return  # local binds resolve with host=None
            if net_mode == "deny" or not _host_allowed(
                host, port if isinstance(port, int) else None
            ):
                _deny_net(host, port)
        elif event == "subprocess.Popen":
            cmd = args[0]  # the explicit executable= argument, usually None
            if cmd is None:
                popen_args = args[1]
                if isinstance(popen_args, (list, tuple)):
                    cmd = popen_args[0] if popen_args else None
                else:
                    parts = (_to_str(popen_args) or "").split()
                    cmd = parts[0] if parts else None
            _check_run(cmd)
        elif event in ("os.exec", "os.posix_spawn"):
            _check_run(args[0])
        elif event == "os.spawn":
            _check_run(args[1])
        elif event == "os.system":
            parts = (_to_str(args[0]) or "").split()
            _check_run(parts[0] if parts else args[0])

    _sys.addaudithook(_hook)


_dicode_install_guard()
del _dicode_install_guard
