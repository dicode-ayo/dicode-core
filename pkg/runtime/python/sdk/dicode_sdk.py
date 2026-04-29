# dicode SDK shim — injected before every Python task script.
# Provides: log, params, env, kv, input, output, dicode, mcp.
# To return a value from a task, assign: result = <value>
#
# Protocol: length-prefixed JSON over a single persistent Unix socket.
#   Frame:  [4-byte little-endian length][JSON bytes]
#
# Handshake (first exchange after connect):
#   Client → { "token": "<DICODE_TOKEN>" }
#   Server → { "proto": 1, "caps": ["log", "params.read", ...] }
#
# After handshake, same request/response pattern as before:
#   Fire-and-forget (no id):    log, kv.set, kv.delete, output
#   Request/response (id req):  params, input, kv.get, kv.list, return,
#                               dicode.run_task, dicode.list_tasks,
#                               dicode.get_runs,
#                               mcp.list_tools, mcp.call
import asyncio
import json
import os
import struct
import sys
import threading

# ── async IO loop ─────────────────────────────────────────────────────────────
# A single background event loop owns the socket.  All IPC operations are
# coroutines dispatched onto it via run_coroutine_threadsafe().

_loop = asyncio.new_event_loop()
threading.Thread(target=_loop.run_forever, daemon=True, name="dicode-io").start()


async def _open_conn():
    return await asyncio.open_unix_connection(os.environ["DICODE_SOCKET"])

_reader_obj, _writer = asyncio.run_coroutine_threadsafe(_open_conn(), _loop).result(timeout=10)


# ── framing helpers ───────────────────────────────────────────────────────────

async def _read_msg():
    hdr = await _reader_obj.readexactly(4)
    (size,) = struct.unpack_from("<I", hdr)
    body = await _reader_obj.readexactly(size)
    return json.loads(body)


def _write_msg(obj):
    body = json.dumps(obj, separators=(",", ":")).encode()
    hdr = struct.pack("<I", len(body))
    _writer.write(hdr + body)


# ── handshake ─────────────────────────────────────────────────────────────────

async def _handshake():
    _write_msg({"token": os.environ["DICODE_TOKEN"]})
    await _writer.drain()
    resp = await _read_msg()
    if resp.get("error"):
        raise RuntimeError(f"ipc handshake failed: {resp['error']}")
    return resp.get("caps", [])

_caps = asyncio.run_coroutine_threadsafe(_handshake(), _loop).result(timeout=10)


# ── read loop ─────────────────────────────────────────────────────────────────

_pending = {}   # id -> asyncio.Future (set on the IO loop)


async def _read_loop():
    try:
        while True:
            msg = await _read_msg()
            rid = msg.get("id")
            if rid:
                fut = _pending.pop(rid, None)
                if fut is not None:
                    fut.set_result(msg)
    except Exception as exc:
        # Cancel all pending futures so callers fail fast instead of
        # blocking for 30s each waiting for a response that will never arrive.
        err = RuntimeError(f"dicode SDK: IO loop terminated: {exc}")
        for fut in list(_pending.values()):
            if not fut.done():
                fut.set_exception(err)
        _pending.clear()

asyncio.run_coroutine_threadsafe(_read_loop(), _loop)


# ── call helpers ──────────────────────────────────────────────────────────────

_nid = 0
# _next_id is only called from _async_call, which is a coroutine on _loop.
# asyncio is single-threaded within a loop, so no cross-thread contention exists.
# The lock is kept as a safeguard if callers ever change.
_nid_lock = threading.Lock()


def _next_id():
    global _nid
    with _nid_lock:
        _nid += 1
        return str(_nid)


async def _async_call(req):
    rid = _next_id()
    fut = _loop.create_future()
    _pending[rid] = fut
    _write_msg({**req, "id": rid})
    await _writer.drain()
    msg = await asyncio.wait_for(fut, timeout=30)
    if msg.get("error"):
        raise RuntimeError(msg["error"])
    return msg.get("result")


def _call(req):
    """Sync: block until the response arrives. Raises RuntimeError after 30s."""
    fut = asyncio.run_coroutine_threadsafe(_async_call(req), _loop)
    return fut.result(timeout=30)


async def _call_async(req):
    """Async: await without occupying a thread.

    Uses asyncio.wrap_future to bridge the concurrent.futures.Future returned
    by run_coroutine_threadsafe into the caller's event loop — no thread pool.
    """
    return await asyncio.wrap_future(
        asyncio.run_coroutine_threadsafe(_async_call(req), _loop)
    )


def _fire(req):
    """Send a fire-and-forget message (no response expected)."""
    async def _do():
        _write_msg(req)
        await _writer.drain()
    asyncio.run_coroutine_threadsafe(_do(), _loop)


# ── log ───────────────────────────────────────────────────────────────────────

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

    # Async variants — _fire is non-blocking, no executor needed.
    async def info_async(self, *args):   self._emit("info",  *args)
    async def warn_async(self, *args):   self._emit("warn",  *args)
    async def error_async(self, *args):  self._emit("error", *args)
    async def debug_async(self, *args):  self._emit("debug", *args)


log = _Log()


# ── params (lazy-fetched, cached) ─────────────────────────────────────────────

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

    async def get_async(self, key, default=None):
        global _params_cache
        if _params_cache is None:
            _params_cache = await _call_async({"method": "params"}) or {}
        return _params_cache.get(key, default)

    async def all_async(self):
        global _params_cache
        if _params_cache is None:
            _params_cache = await _call_async({"method": "params"}) or {}
        return dict(_params_cache)


params = _Params()


# ── env ───────────────────────────────────────────────────────────────────────

class _Env:
    def get(self, key, default=None):
        return os.environ.get(key, default)


env = _Env()


# ── kv ────────────────────────────────────────────────────────────────────────

class _KV:
    def get(self, key):
        return _call({"method": "kv.get", "key": key})

    def set(self, key, value):
        _fire({"method": "kv.set", "key": key, "value": value})

    def delete(self, key):
        _fire({"method": "kv.delete", "key": key})

    def list(self, prefix=""):
        return _call({"method": "kv.list", "prefix": prefix}) or []

    # Async variants for use inside async def main() tasks.
    async def get_async(self, key):
        return await _call_async({"method": "kv.get", "key": key})

    async def set_async(self, key, value):
        self.set(key, value)  # _fire is non-blocking

    async def delete_async(self, key):
        self.delete(key)  # _fire is non-blocking

    async def list_async(self, prefix=""):
        return await _call_async({"method": "kv.list", "prefix": prefix}) or []


kv = _KV()


# ── input (fetched once at import time) ───────────────────────────────────────

input = _call({"method": "input"})


# ── output ────────────────────────────────────────────────────────────────────


class _Output:
    """
    Callable + method object. Calling output({...}, secret=True) flags the
    map for daemon-side redaction + provider-response routing (issue #119).
    Method calls (output.html, output.text, ...) preserve the legacy
    structured-output API.
    """

    def __call__(self, value, secret=False):
        if not secret:
            raise TypeError(
                "output(value) requires secret=True. "
                "Use output.html / output.text / output.image / output.file "
                "for non-secret structured output."
            )
        if not isinstance(value, dict):
            raise TypeError("output(map, secret=True): value must be a dict")
        for k, v in value.items():
            if not isinstance(k, str):
                raise TypeError(
                    f"output(map, secret=True): key {k!r} is not a string"
                )
            if not isinstance(v, str):
                raise TypeError(
                    f"output(map, secret=True): value for {k!r} is not a string"
                )
        _fire({"method": "output", "secret": True, "secretMap": value})

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

    # Async variants — _fire is non-blocking, no executor needed.
    async def html_async(self, content, data=None):  self.html(content, data)
    async def text_async(self, content):              self.text(content)
    async def image_async(self, mime, content):       self.image(mime, content)
    async def file_async(self, name, content, mime=None): self.file(name, content, mime)


output = _Output()


# ── dicode — task orchestration helpers ───────────────────────────────────────

class _Dicode:
    def run_task(self, task_id, params=None):
        return _call({"method": "dicode.run_task", "taskID": task_id,
                      "params": params or {}})

    def list_tasks(self):
        return _call({"method": "dicode.list_tasks"}) or []

    def get_runs(self, task_id, limit=10):
        return _call({"method": "dicode.get_runs", "taskID": task_id,
                      "limit": limit}) or []

    async def run_task_async(self, task_id, params=None):
        return await _call_async({"method": "dicode.run_task", "taskID": task_id,
                                  "params": params or {}})

    async def list_tasks_async(self):
        return await _call_async({"method": "dicode.list_tasks"}) or []

    async def get_runs_async(self, task_id, limit=10):
        return await _call_async({"method": "dicode.get_runs", "taskID": task_id,
                                  "limit": limit}) or []


dicode = _Dicode()


# ── mcp — Model Context Protocol tool access ──────────────────────────────────

class _MCP:
    def list_tools(self, name):
        return _call({"method": "mcp.list_tools", "mcpName": name}) or []

    def call(self, name, tool, args=None):
        return _call({"method": "mcp.call", "mcpName": name, "tool": tool,
                      "args": args or {}})

    async def list_tools_async(self, name):
        return await _call_async({"method": "mcp.list_tools", "mcpName": name}) or []

    async def call_async(self, name, tool, args=None):
        return await _call_async({"method": "mcp.call", "mcpName": name, "tool": tool,
                                  "args": args or {}})


mcp = _MCP()


# ── internal: send task return value (called by the wrapper, not user code) ───

def _set_return(value):
    try:
        _call({"method": "return", "value": value})
    except Exception:
        pass
