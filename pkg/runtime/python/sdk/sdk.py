# dicode SDK shim — injected before every Python task script.
# Provides: log, params, env, kv, input, output.
# To return a value from a task, assign: result = <value>
#
# Protocol: newline-delimited JSON over a single persistent Unix socket.
#   Fire-and-forget (no id):    log, kv.set, kv.delete, output
#   Request/response (id req):  params, input, kv.get, kv.list, return
import json
import os
import socket
import threading

_sock = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
_sock.connect(os.environ["DICODE_SOCKET"])
_file = _sock.makefile("rwb", buffering=0)

_pending = {}       # id -> threading.Event
_results = {}       # id -> msg
_nid = 0
_lock = threading.Lock()
_recv_lock = threading.Lock()


def _next_id():
    global _nid
    with _lock:
        _nid += 1
        return str(_nid)


def _send(obj):
    line = json.dumps(obj, separators=(",", ":")) + "\n"
    with _lock:
        _file.write(line.encode())
        _file.flush()


def _call(req):
    """Send a request and block until the response arrives."""
    rid = _next_id()
    ev = threading.Event()
    with _lock:
        _pending[rid] = ev
    _send({**req, "id": rid})
    ev.wait()
    msg = _results.pop(rid, {})
    if msg.get("error"):
        raise RuntimeError(msg["error"])
    return msg.get("result")


def _fire(req):
    """Send a fire-and-forget message (no id, no response expected)."""
    _send(req)


def _reader():
    """Background thread: read response lines and wake waiting _call()s."""
    buf = b""
    while True:
        try:
            chunk = _file.read(4096)
        except Exception:
            break
        if not chunk:
            break
        buf += chunk
        while b"\n" in buf:
            line, buf = buf.split(b"\n", 1)
            line = line.strip()
            if not line:
                continue
            try:
                msg = json.loads(line)
            except Exception:
                continue
            rid = msg.get("id")
            if rid and rid in _pending:
                _results[rid] = msg
                _pending.pop(rid).set()


_reader_thread = threading.Thread(target=_reader, daemon=True)
_reader_thread.start()


# ---------------------------------------------------------------------------
# log
# ---------------------------------------------------------------------------

class _Log:
    @staticmethod
    def _emit(level, *args):
        msg = " ".join(
            json.dumps(a) if isinstance(a, (dict, list)) else str(a)
            for a in args
        )
        _fire({"method": "log", "level": level, "message": msg})

    def info(self, *args):   self._emit("info",  *args)
    def warn(self, *args):   self._emit("warn",  *args)
    def error(self, *args):  self._emit("error", *args)
    def debug(self, *args):  self._emit("debug", *args)


log = _Log()


# ---------------------------------------------------------------------------
# params (lazy-fetched, cached)
# ---------------------------------------------------------------------------

_params_cache = None
_params_once = threading.Lock()


def _get_params():
    global _params_cache
    if _params_cache is None:
        with _params_once:
            if _params_cache is None:
                _params_cache = _call({"method": "params"}) or {}
    return _params_cache


class _Params:
    def get(self, key, default=None):
        return _get_params().get(key, default)

    def all(self):
        return dict(_get_params())


params = _Params()


# ---------------------------------------------------------------------------
# env
# ---------------------------------------------------------------------------

class _Env:
    def get(self, key, default=None):
        return os.environ.get(key, default)


env = _Env()


# ---------------------------------------------------------------------------
# kv
# ---------------------------------------------------------------------------

class _KV:
    def get(self, key):
        return _call({"method": "kv.get", "key": key})

    def set(self, key, value):
        _fire({"method": "kv.set", "key": key, "value": value})

    def delete(self, key):
        _fire({"method": "kv.delete", "key": key})

    def list(self, prefix=""):
        return _call({"method": "kv.list", "prefix": prefix}) or []


kv = _KV()


# ---------------------------------------------------------------------------
# input (fetched once at import time)
# ---------------------------------------------------------------------------

input = _call({"method": "input"})


# ---------------------------------------------------------------------------
# output
# ---------------------------------------------------------------------------

class _Output:
    def html(self, content, data=None):
        _fire({"method": "output", "contentType": "text/html",
               "content": content, "data": data})

    def text(self, content):
        _fire({"method": "output", "contentType": "text/plain",
               "content": content})

    def image(self, mime, content):
        _fire({"method": "output", "contentType": mime or "image/png",
               "content": content})

    def file(self, name, content, mime=None):
        _fire({"method": "output",
               "contentType": mime or "application/octet-stream",
               "content": content, "data": {"filename": name}})


output = _Output()


# ---------------------------------------------------------------------------
# Internal: send the task return value (called by the wrapper, not user code).
# ---------------------------------------------------------------------------

def _set_return(value):
    try:
        _call({"method": "return", "value": value})
    except Exception:
        pass
