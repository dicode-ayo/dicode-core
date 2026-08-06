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
        # Denied entries may be individual files (dicode.lock) or directories
        # (the approval-snapshot cache), so use the same prefix-match idiom
        # as write_roots below rather than exact membership — otherwise a
        # write to a file nested inside a denied directory would slip
        # through.
        rn = _os.path.realpath(n)
        for denied in write_denied:
            if rn == denied or rn.startswith(denied + sep):
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

    def _check_no_dir_fd(dir_fd, op):
        # dir_fd resolves a relative path against an arbitrary open directory
        # fd instead of the process cwd, which lets a task escape fs_write /
        # fs_deny entirely (open a handle to a denied or unwritable directory,
        # then pass its fd here with a bare relative name). It has no
        # legitimate use in ordinary dicode automation tasks (it exists for
        # *at()-family race-free filesystem ops), so it is rejected outright
        # rather than resolved.
        if dir_fd is not None and dir_fd != -1:
            raise PermissionError(
                f"dicode: {op} with dir_fd={dir_fd!r} denied — dir_fd-relative "
                f"filesystem operations are not permitted for dicode tasks"
            )

    # os.open cannot be policed via the "open" audit event: that event's args
    # are documented as exactly (path, mode, flags) and never include dir_fd,
    # so a call like os.open("name", flags, dir_fd=denied_fd) is invisible to
    # the hook below — the hook only ever sees the bare relative name and has
    # no way to know it resolves inside a denied (or otherwise unwritable)
    # directory. Wrap os.open itself so the dir_fd rejection happens before
    # the real syscall runs, regardless of whether the task calls it via
    # `import os; os.open(...)` or any other binding of the same module
    # object.
    _real_os_open = _os.open

    def _guarded_os_open(path, flags, mode=0o777, *, dir_fd=None):
        _check_no_dir_fd(dir_fd, "os.open")
        return _real_os_open(path, flags, mode)

    # _os IS the os module object (import os as _os), so patching the
    # attribute here is visible to every other binding of the same module,
    # including the task's own `import os; os.open(...)`.
    _os.open = _guarded_os_open

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
        elif event in ("os.remove", "os.rmdir"):
            # Audit args are (path, dir_fd).
            _check_no_dir_fd(args[1], event)
            _check_write(args[0], event)
        elif event == "os.mkdir":
            # Audit args are (path, mode, dir_fd).
            _check_no_dir_fd(args[2], event)
            _check_write(args[0], event)
        elif event == "os.truncate":
            # Audit args are (path, length) — os.truncate has no dir_fd
            # parameter (the path form already accepts an fd directly).
            _check_write(args[0], event)
        elif event == "shutil.rmtree":
            _check_write(args[0], event)
        elif event in ("os.rename", "shutil.move"):
            # os.rename's audit args are (src, dst, src_dir_fd, dst_dir_fd);
            # shutil.move's are (src, dst) with no dir_fd slots, so guard the
            # index lookup.
            if len(args) > 2:
                _check_no_dir_fd(args[2], event)
            if len(args) > 3:
                _check_no_dir_fd(args[3], event)
            _check_write(args[0], event)
            _check_write(args[1], event)
        elif event == "os.symlink":
            # Audit args are (src, dst, dir_fd). The mutation is the new
            # link at the destination (args[1]). args[0] is the symlink
            # target string, which need not exist as a real path (and often
            # doesn't), so it is not checked.
            _check_no_dir_fd(args[2], event)
            _check_write(args[1], event)
        elif event == "os.link":
            # Audit args are (src, dst, src_dir_fd, dst_dir_fd). Unlike
            # os.symlink, args[0] here is an existing file being aliased —
            # a hardlink makes dst a second name for the SAME inode as src
            # (no symlink-style indirection, so realpath resolution in
            # _write_allowed can't "unmask" it the way it does for
            # writes-through-a-symlink). Without checking the source, a task
            # could hardlink a denied file (e.g. an approval-snapshot) into
            # its own writable directory and then write through the alias to
            # mutate the denied file's real content. Check both ends, same
            # as os.rename/shutil.move above.
            _check_no_dir_fd(args[2], event)
            _check_no_dir_fd(args[3], event)
            _check_write(args[0], event)
            _check_write(args[1], event)
        elif event == "os.chmod":
            # Audit args are (path, mode, dir_fd).
            _check_no_dir_fd(args[2], event)
            _check_write(args[0], event)
        elif event == "os.chown":
            # Audit args are (path, uid, gid, dir_fd).
            _check_no_dir_fd(args[3], event)
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

    # Env-read guardrail: restrict os.environ reads to declared names + essential vars.
    env_allowed = policy.get("env_allowed")
    if env_allowed is not None:
        import collections.abc as _cabc
        # Mutable set: task writes (os.environ["K"] = v) extend the allowed set
        # so the task can read back what it wrote without a KeyError.
        _env_allowed_set = set(env_allowed)
        _real_env = _os.environ  # keep the original MutableMapping

        class _FilteredEnv(_cabc.MutableMapping):
            """Filters os.environ reads to env_allowed_set; writes go through."""
            def __getitem__(self, key):
                if key in _env_allowed_set:
                    return _real_env[key]
                raise KeyError(key)
            def __setitem__(self, key, value):
                _real_env[key] = value
                _env_allowed_set.add(key)  # task can read back what it wrote
            def __delitem__(self, key):
                if key not in _env_allowed_set:
                    raise KeyError(key)
                del _real_env[key]
                _env_allowed_set.discard(key)
            def __iter__(self):
                return (k for k in _real_env if k in _env_allowed_set)
            def __len__(self):
                # Iterate the smaller allowed set (O(|allowed|)) rather than
                # all of _real_env (O(|real_env|)) which can be much larger.
                return sum(1 for k in _env_allowed_set if k in _real_env)
            def __contains__(self, key):
                return key in _env_allowed_set and key in _real_env
            def copy(self):
                return dict(self.items())

        _os.environ = _FilteredEnv()

        # Also cover the bytes env API so os.environb / os.getenvb cannot be
        # used to bypass the allowlist on platforms that expose them (Linux).
        if hasattr(_os, "environb"):
            _real_environb = _os.environb

            class _FilteredEnvB(_cabc.MutableMapping):
                """Bytes-key view of _FilteredEnv; mirrors the allowlist filtering."""
                def __getitem__(self, key):
                    if key.decode(errors="replace") in _env_allowed_set:
                        return _real_environb[key]
                    raise KeyError(key)
                def __setitem__(self, key, value):
                    _real_environb[key] = value
                    _env_allowed_set.add(key.decode(errors="replace"))
                def __delitem__(self, key):
                    if key.decode(errors="replace") not in _env_allowed_set:
                        raise KeyError(key)
                    del _real_environb[key]
                    _env_allowed_set.discard(key.decode(errors="replace"))
                def __iter__(self):
                    return (k for k in _real_environb if k.decode(errors="replace") in _env_allowed_set)
                def __len__(self):
                    return sum(1 for k in _real_environb if k.decode(errors="replace") in _env_allowed_set)
                def __contains__(self, key):
                    return key.decode(errors="replace") in _env_allowed_set and key in _real_environb

            _os.environb = _FilteredEnvB()

        if hasattr(_os, "getenvb"):
            _real_getenvb = _os.getenvb
            def _getenvb(key, default=None):
                if key.decode(errors="replace") in _env_allowed_set:
                    return _real_getenvb(key, default)
                return default
            _os.getenvb = _getenvb

    _sys.addaudithook(_hook)


_dicode_install_guard()
del _dicode_install_guard
