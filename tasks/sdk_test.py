"""sdk_test.py — pytest test harness for tasks/**/task.test.py (runtime: python).

Mirrors tasks/sdk-test.ts (the Deno harness) at the level of test-writing
ergonomics: in-memory params/env/kv mocks, an httpx interceptor, a
run_task() helper that runs the adjacent task.py's main() against those
mocks, and a MockDicode object shaped like the real `dicode` module
(pkg/runtime/python/sdk/dicode_sdk.py) for the handful of calls tasks
commonly make.

Usage in a task.test.py — which is itself a PEP 723 script (see
run_pytest_main's docstring for why this file must be run via
`uv run <file>`, not `pytest <file>`):

    # /// script
    # requires-python = ">=3.11"
    # dependencies = ["pytest", "httpx>=0.27"]
    # ///
    import os
    import sys

    sys.path.insert(0, os.path.join(os.path.dirname(__file__), "..", ".."))
    from sdk_test import (
        _dicode_harness_reset,  # noqa: F401 — registers the autouse fixture
        assert_http_called,
        dicode,
        env,
        http,
        kv,
        params,
        run_pytest_main,
        run_task,
        setup_harness,
    )

    setup_harness(__file__)


    def test_greets_by_name():
        params.set("name", "Ada")
        result = run_task()
        assert result["greeting"] == "Hello, Ada! (run #1)"


    if __name__ == "__main__":
        run_pytest_main(__file__)

See tasks/examples/hello-python/task.test.py for a complete, working example
and docs/concepts/testing.md for the full mock API reference.

Design notes / how this differs from the Deno harness (tasks/sdk-test.ts)
--------------------------------------------------------------------------
* Deno's harness dynamically `import()`s the sibling task.ts ONCE and reuses
  the loaded module across every test() case — module-level state persists
  unless a test explicitly resets it through the mocked globals. This
  harness instead re-reads and re-execs task.py's body fresh on every
  run_task() call. That's cheap for a script-sized file and gives
  *stronger* isolation than the Deno harness: there is no task-module-level
  state to leak between test cases in the first place.
* Deno's `fetch` is a single global function, so mocking it is one
  monkeypatch. Python tasks reach for whatever HTTP client they like
  (CLAUDE.md: "Python tasks use stdlib (urllib, requests, etc.) — there is
  no HTTP helper on the dicode module itself"). This harness mocks httpx —
  the client every Python example task in this repo uses today — by
  patching httpx.Client.request / httpx.AsyncClient.request, the method
  every convenience verb (get/post/put/...) funnels through. Tasks that
  reach for urllib or requests directly aren't intercepted by this harness;
  add a seam for those the same way if/when a task needs it.
* Every task.test.py must be a self-running PEP 723 script (see
  run_pytest_main below) because pkg/tasktest.runPython invokes it as a
  single `uv run <file>` with no extra flags — uv resolves the script's own
  declared `dependencies` (pytest plus whatever the adjacent task.py needs,
  e.g. httpx) into an ephemeral venv and executes the file as `__main__`,
  which in turn calls pytest against itself. This keeps the Go-side runner
  as simple as the Deno one (a single subprocess, no dependency
  introspection) while letting every test file declare exactly the
  dependencies it needs.
* Because pytest is not necessarily on this interpreter's path outside a
  `uv run` invocation, `import pytest` is deferred to inside
  run_pytest_main() rather than done at module import time.
"""

from __future__ import annotations

import asyncio
import inspect
import os
import re
from typing import Any, Dict, List, Optional

import pytest

# ─── mock state ─────────────────────────────────────────────────────────────


class _State:
    def __init__(self) -> None:
        self.params: Dict[str, str] = {}
        self.env: Dict[str, str] = {}
        self.kv: Dict[str, Any] = {}
        self.http_mocks: List[dict] = []
        self.http_calls: List[dict] = []
        self.task_source: Optional[str] = None
        self.task_path: Optional[str] = None
        self.dicode: "MockDicode" = None  # type: ignore[assignment] — set in reset_mocks()
        self._httpx_patched = False


_state = _State()


def reset_mocks() -> None:
    """Reset every mock to a fresh, empty state. Wired to run automatically
    before each test case via the autouse `_dicode_harness_reset` fixture
    below — task.test.py files don't normally need to call this directly.

    Resets `ctx` in place (not by rebinding the module-level name) because a
    task.test.py imports `ctx` by value at `from sdk_test import ctx` time —
    rebinding `sdk_test.ctx` to a new `_Ctx()` here would leave that already
    -bound reference stale. Mutating the existing object's fields is what
    every importer actually observes on the next test."""
    _state.params = {}
    _state.env = {}
    _state.kv = {}
    _state.http_mocks = []
    _state.http_calls = []
    _state.dicode = MockDicode()
    ctx.state = None
    ctx.input = None
    ctx._resumed = False
    ctx._step = None


@pytest.fixture(autouse=True)
def _dicode_harness_reset():
    """Import this fixture by name into a task.test.py (`from sdk_test import
    _dicode_harness_reset  # noqa: F401`) to get it applied automatically to
    every test in that file — pytest discovers fixtures via a test's
    enclosing module namespace, so a plain import is enough to register it,
    no re-declaration needed."""
    reset_mocks()
    yield


# ─── params / env / kv / log / output (shape matches dicode_sdk.py) ────────


class _Params:
    def set(self, key: str, value: str) -> None:
        _state.params[key] = str(value)

    def get(self, key: str, default: Any = None) -> Any:
        return _state.params.get(key, default)

    def all(self) -> Dict[str, str]:
        return dict(_state.params)

    async def get_async(self, key: str, default: Any = None) -> Any:
        return self.get(key, default)

    async def all_async(self) -> Dict[str, str]:
        return self.all()


class _Env:
    def set(self, key: str, value: str) -> None:
        _state.env[key] = str(value)

    def get(self, key: str, default: Any = None) -> Any:
        return _state.env.get(key, default)


class _KV:
    def set(self, key: str, value: Any) -> None:
        _state.kv[key] = value

    def get(self, key: str) -> Any:
        return _state.kv.get(key)

    def delete(self, key: str) -> None:
        _state.kv.pop(key, None)

    def list(self, prefix: str = "") -> Dict[str, Any]:
        return {k: v for k, v in _state.kv.items() if k.startswith(prefix)}

    async def set_async(self, key: str, value: Any) -> None:
        self.set(key, value)

    async def get_async(self, key: str) -> Any:
        return self.get(key)

    async def delete_async(self, key: str) -> None:
        self.delete(key)

    async def list_async(self, prefix: str = "") -> Dict[str, Any]:
        return self.list(prefix)


class _Log:
    """Swallows everything — real tests don't usually assert on log output,
    but a task calling log.info(...) shouldn't blow up because the mock is
    missing a method."""

    def _emit(self, level: str, *args: Any) -> None:
        pass

    def info(self, *args: Any) -> None:
        self._emit("info", *args)

    def warn(self, *args: Any) -> None:
        self._emit("warn", *args)

    def error(self, *args: Any) -> None:
        self._emit("error", *args)

    def debug(self, *args: Any) -> None:
        self._emit("debug", *args)

    async def info_async(self, *args: Any) -> None:
        self.info(*args)

    async def warn_async(self, *args: Any) -> None:
        self.warn(*args)

    async def error_async(self, *args: Any) -> None:
        self.error(*args)

    async def debug_async(self, *args: Any) -> None:
        self.debug(*args)


class _Output:
    """Records calls instead of firing them over IPC — there is no daemon in
    these tests. `calls` is a plain list a test can inspect if it wants to
    assert on what the task produced."""

    def __init__(self) -> None:
        self.calls: List[dict] = []

    def html(self, content: Any, data: Any = None) -> None:
        self.calls.append({"contentType": "text/html", "content": content, "data": data})

    def text(self, content: Any) -> None:
        self.calls.append({"contentType": "text/plain", "content": content})

    def image(self, mime: Optional[str], content: Any) -> None:
        self.calls.append({"contentType": mime or "image/png", "content": content})

    def file(self, name: str, content: Any, mime: Optional[str] = None) -> None:
        self.calls.append(
            {
                "contentType": mime or "application/octet-stream",
                "content": content,
                "data": {"filename": name},
            }
        )

    async def html_async(self, content: Any, data: Any = None) -> None:
        self.html(content, data)

    async def text_async(self, content: Any) -> None:
        self.text(content)

    async def image_async(self, mime: Optional[str], content: Any) -> None:
        self.image(mime, content)

    async def file_async(self, name: str, content: Any, mime: Optional[str] = None) -> None:
        self.file(name, content, mime)


params = _Params()
env = _Env()
kv = _KV()
log = _Log()
output = _Output()


# ─── dicode / mcp / ctx mocks ───────────────────────────────────────────────


class _Runs:
    def replay(self, run_id: str, task_name: Optional[str] = None) -> Any:
        return {}

    async def replay_async(self, run_id: str, task_name: Optional[str] = None) -> Any:
        return {}


class _Tasks:
    def test(self, task_id: str) -> Any:
        return {}

    async def test_async(self, task_id: str) -> Any:
        return {}


class _Sources:
    def set_dev_mode(self, *args: Any, **kwargs: Any) -> Any:
        return {}

    async def set_dev_mode_async(self, *args: Any, **kwargs: Any) -> Any:
        return {}


class _Git:
    def commit_push(self, *args: Any, **kwargs: Any) -> Any:
        return {}

    async def commit_push_async(self, *args: Any, **kwargs: Any) -> Any:
        return {}


class _Secrets:
    """Presence-only, like the real dicode_sdk.py's _Secrets — always
    reports "not present" unless a test overrides `.has`."""

    def has(self, key: str) -> bool:
        return False

    async def has_async(self, key: str) -> bool:
        return False


class _Audit:
    def query(self, **kwargs: Any) -> list:
        return []

    async def query_async(self, **kwargs: Any) -> list:
        return []


class MockDicode:
    """Mirrors the shape of pkg/runtime/python/sdk/dicode_sdk.py's `dicode`
    module-level singleton. suspend()/resume simulation is intentionally out
    of scope for this harness — see the module docstring."""

    def __init__(self) -> None:
        self.runs = _Runs()
        self.tasks = _Tasks()
        self.sources = _Sources()
        self.git = _Git()
        self.secrets = _Secrets()
        self.audit = _Audit()
        # Records every set_group() call so tests can assert on the labels
        # written, mirroring tasks/sdk-test.ts's _setGroupCalls. Last write
        # wins in production; the list preserves call ordering for tests.
        self.group_calls: List[str] = []

    def run_task(self, task_id: str, params: Optional[dict] = None, mcp_context: bool = False) -> Any:
        return {}

    async def run_task_async(
        self, task_id: str, params: Optional[dict] = None, mcp_context: bool = False
    ) -> Any:
        return {}

    def list_tasks(self, mcp_context: bool = False) -> list:
        return []

    async def list_tasks_async(self, mcp_context: bool = False) -> list:
        return []

    def get_runs(self, task_id: str, limit: int = 10) -> list:
        return []

    async def get_runs_async(self, task_id: str, limit: int = 10) -> list:
        return []

    def set_group(self, label: Any) -> None:
        self.group_calls.append(str(label or ""))

    async def set_group_async(self, label: Any) -> None:
        self.set_group(label)


class _MCP:
    def list_tools(self, name: str) -> list:
        return []

    def call(self, name: str, tool: str, args: Optional[dict] = None) -> Any:
        return {}

    async def list_tools_async(self, name: str) -> list:
        return []

    async def call_async(self, name: str, tool: str, args: Optional[dict] = None) -> Any:
        return {}


mcp = _MCP()


class _Ctx:
    """Minimal resume-context stand-in. Real tasks read ctx.state / ctx.input
    on a resume; this harness only exercises first-run behaviour (state is
    always None, input is whatever run_task's caller sets via
    `sdk_test.set_input(...)`). Simulating suspend/resume flows is out of
    scope for this pass — see the module docstring."""

    def __init__(self) -> None:
        self.state: Any = None
        self.input: Any = None
        self._resumed = False
        self._step: Optional[str] = None


ctx = _Ctx()


def set_input(value: Any) -> None:
    """Set the value `run_task()`'s injected `input` global (and `ctx.input`)
    will see on the next call — mirrors tasks/sdk-test.ts's `input` global.
    `ctx` is the single source of truth; reset_mocks() clears it every test."""
    ctx.input = value


# ─── http mocking (httpx) ───────────────────────────────────────────────────


def _glob_match(pattern: str, url: str) -> bool:
    if pattern == url:
        return True
    if "*" not in pattern:
        return False
    escaped = re.escape(pattern).replace(r"\*", ".*")
    return re.match(f"^{escaped}$", url) is not None


class _Http:
    def mock(self, method: str, pattern: str, response: dict) -> None:
        self._register(method, pattern, response, once=False)

    def mock_once(self, method: str, pattern: str, response: dict) -> None:
        self._register(method, pattern, response, once=True)

    def _register(self, method: str, pattern: str, response: dict, once: bool) -> None:
        _state.http_mocks.append(
            {"method": method.upper(), "pattern": pattern, "response": response, "once": once, "consumed": False}
        )

    def last_request_body(self, method: str, pattern: str) -> Any:
        for call in reversed(_state.http_calls):
            if call["method"] == method.upper() and _glob_match(pattern, call["url"]):
                return call.get("body")
        return None


http = _Http()


def _record_http_call(method: str, url: str, body: Any) -> None:
    _state.http_calls.append({"method": method.upper(), "url": url, "body": body})


def _find_http_mock(method: str, url: str) -> Optional[dict]:
    for m in _state.http_mocks:
        if m["consumed"]:
            continue
        if m["method"] != method.upper():
            continue
        if not _glob_match(m["pattern"], url):
            continue
        if m["once"]:
            m["consumed"] = True
        return m
    return None


def _fake_httpx_response(httpx_mod: Any, method: str, url: str, kwargs: dict) -> Any:
    body_sent = kwargs.get("json")
    if body_sent is None:
        body_sent = kwargs.get("data")
    _record_http_call(method, url, body_sent)

    m = _find_http_mock(method, url)
    if m is None:
        seen = ", ".join(f'{c["method"]} {c["url"]}' for c in _state.http_calls)
        raise AssertionError(f"[sdk_test] no http mock matches {method} {url} (calls so far: {seen})")

    resp = m["response"]
    status = resp.get("status", 200)
    headers = resp.get("headers") or {}
    req = httpx_mod.Request(method, url)
    body = resp.get("body")
    if isinstance(body, (dict, list)):
        return httpx_mod.Response(status, request=req, json=body, headers=headers)
    return httpx_mod.Response(status, request=req, text=body if isinstance(body, str) else "", headers=headers)


def _install_httpx_mock() -> None:
    """Monkeypatch httpx.Client.request / httpx.AsyncClient.request — the
    method every convenience verb (get/post/put/delete/...) funnels through
    — so http.mock()/mock_once() intercept calls made through either client.
    A no-op if httpx isn't installed in this test's environment (a task.test.py
    that doesn't need HTTP has no reason to declare httpx as a dependency)."""
    if _state._httpx_patched:
        return
    try:
        import httpx
    except ImportError:
        return

    async def fake_async_request(self, method, url, *args, **kwargs):  # noqa: ANN001
        return _fake_httpx_response(httpx, method, str(url), kwargs)

    def fake_sync_request(self, method, url, *args, **kwargs):  # noqa: ANN001
        return _fake_httpx_response(httpx, method, str(url), kwargs)

    httpx.AsyncClient.request = fake_async_request
    httpx.Client.request = fake_sync_request
    _state._httpx_patched = True


# ─── assert helpers ──────────────────────────────────────────────────────────
# `assert` is a Python keyword, so these can't be namespaced under an
# `assert` object the way tasks/sdk-test.ts does it (`assert.httpCalled`).
# Use plain `assert` statements for equality/truthiness checks — pytest
# rewrites them with full introspection on failure, which is strictly more
# useful than a hand-rolled assert.equal/assert.ok. These free functions
# cover the HTTP-specific assertions tasks/sdk-test.ts's `assert.*` provides.


def assert_http_called(method: str, pattern: str) -> None:
    hit = any(c["method"] == method.upper() and _glob_match(pattern, c["url"]) for c in _state.http_calls)
    if not hit:
        seen = ", ".join(f'{c["method"]} {c["url"]}' for c in _state.http_calls)
        raise AssertionError(f"assert_http_called: no {method} {pattern} in [{seen}]")


def assert_http_not_called(method: str, pattern: str) -> None:
    hit = any(c["method"] == method.upper() and _glob_match(pattern, c["url"]) for c in _state.http_calls)
    if hit:
        raise AssertionError(f"assert_http_not_called: unexpected {method} {pattern}")


def assert_http_called_with(method: str, pattern: str, body: Any = None) -> None:
    for c in _state.http_calls:
        if c["method"] == method.upper() and _glob_match(pattern, c["url"]):
            if body is not None and c.get("body") != body:
                raise AssertionError(f"assert_http_called_with: body {c.get('body')!r} != {body!r}")
            return
    raise AssertionError(f"assert_http_called_with: no {method} {pattern}")


# ─── harness setup + run_task ────────────────────────────────────────────────


def _strip_pep723(src: str) -> str:
    """Remove a PEP 723 inline-metadata block (`# /// script` … `# ///`) from
    a script's source, mirroring pkg/runtime/python/runtime.go's
    extractPEP723 — task.py's own header would otherwise end up exec()'d as
    dead comments (harmless) but stripping it keeps run_task()'s namespace
    exactly what the production wrapper would build."""
    lines = src.split("\n")
    start = end = -1
    for i, line in enumerate(lines):
        t = line.strip()
        if start == -1 and t == "# /// script":
            start = i
            continue
        if start != -1 and end == -1 and t == "# ///":
            end = i
            break
    if start == -1 or end == -1:
        return src
    return "\n".join(lines[:start] + lines[end + 1 :])


def setup_harness(test_file: str, task_filename: str = "task.py") -> None:
    """Load the sibling task script's source (default: ./task.py next to
    test_file) and install the httpx interceptor. Call once at module level
    in a task.test.py, before any test function runs."""
    task_dir = os.path.dirname(os.path.abspath(test_file))
    task_path = os.path.join(task_dir, task_filename)
    with open(task_path, "r", encoding="utf-8") as f:
        raw = f.read()
    _state.task_source = _strip_pep723(raw)
    _state.task_path = task_path
    _install_httpx_mock()
    reset_mocks()


def run_task() -> Any:
    """Exec the adjacent task.py's body in a fresh namespace seeded with the
    mocked SDK globals, then call its `main()` (sync or async — detected the
    same way pkg/runtime/python/sdk/dicode_sdk.py's _call_handler does) and
    return its result. Falls back to the module-level `result` variable for
    a no-main task, matching the production dispatch's final fallback."""
    if _state.task_source is None:
        raise RuntimeError("run_task: setup_harness(__file__) must be called before run_task()")

    namespace: Dict[str, Any] = {
        "__name__": "__dicode_task__",
        "params": params,
        "env": env,
        "kv": kv,
        "log": log,
        "output": output,
        "dicode": _state.dicode,
        "mcp": mcp,
        "ctx": ctx,
        "input": ctx.input,
    }
    code = compile(_state.task_source, _state.task_path or "<task.py>", "exec")
    exec(code, namespace)  # noqa: S102 — intentional: this *is* the harness's job.

    main = namespace.get("main")
    if main is None:
        return namespace.get("result")
    if inspect.iscoroutinefunction(main):
        return asyncio.run(main())
    result = main()
    if inspect.iscoroutine(result):
        return asyncio.run(result)
    return result


def run_pytest_main(test_file: str) -> None:
    """Entry point for a task.test.py's `if __name__ == "__main__":` block —
    required so `pkg/tasktest.runPython`'s single `uv run <file>` invocation
    actually runs the tests (see that function's doc comment for the full
    contract).

    --import-mode=importlib is not optional: pytest's default ("prepend")
    import mode derives a dotted module name from a file's basename by
    stripping only the final ".py" suffix. For "task.test.py" that leaves
    "task.test", which Python's import machinery then treats as a request to
    import submodule `test` of package `task` — raising
    `ModuleNotFoundError: No module named 'task'` before a single test can
    run. --import-mode=importlib assigns each collected file a synthetic
    module name instead, sidestepping the dotted-basename collision
    entirely. (Confirmed against a real pytest run while building this
    harness — pkg/tasktest/tasktest_test.go's TestRun_Python fixture bakes in
    the same flag.)
    """
    import pytest as _pytest

    raise SystemExit(_pytest.main([test_file, "-q", "--no-header", "--import-mode=importlib"]))
