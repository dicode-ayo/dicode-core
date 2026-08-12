# /// script
# requires-python = ">=3.11"
# dependencies = ["pytest", "httpx>=0.27"]
# ///
"""task.test.py — unit tests for the Hello Python example task.

Run with:
    dicode task test examples/hello-python          # via the daemon (CLI -> IPC -> pkg/tasktest)
    uv run tasks/examples/hello-python/task.test.py  # directly, no daemon required

Demonstrates the Python test harness (../../sdk_test.py) end-to-end: params,
kv persistence across a single test, httpx mocking (hello-python's async
httpx.AsyncClient call), and per-test isolation via the autouse
`_dicode_harness_reset` fixture — kv/params/http mocks never leak between
the test functions below even though each run_task() call re-execs task.py's
body from scratch.
"""
import os
import sys

sys.path.insert(0, os.path.join(os.path.dirname(__file__), "..", ".."))

from sdk_test import (  # noqa: E402
    _dicode_harness_reset,  # noqa: F401 -- registers the autouse per-test reset fixture
    http,
    kv,
    params,
    run_pytest_main,
    run_task,
    setup_harness,
)

setup_harness(__file__)


def test_greets_by_name_and_reports_origin():
    params.set("name", "Ada")
    params.set("count", "3")
    http.mock("GET", "https://httpbin.org/get", {"status": 200, "body": {"origin": "127.0.0.1"}})

    result = run_task()

    assert result["greeting"] == "Hello, Ada! (run #3)"
    assert result["origin"] == "127.0.0.1"


def test_defaults_to_world_when_name_not_set():
    http.mock("GET", "https://httpbin.org/get", {"status": 200, "body": {"origin": "203.0.113.5"}})

    result = run_task()

    assert result["greeting"] == "Hello, World! (run #1)"
    assert result["origin"] == "203.0.113.5"


def test_remembers_previous_name_via_kv():
    kv.set("previous_name", "Grace")
    params.set("name", "Ada")
    http.mock("GET", "https://httpbin.org/get", {"status": 200, "body": {"origin": "127.0.0.1"}})

    run_task()

    assert kv.get("previous_name") == "Ada"


def test_previous_name_does_not_leak_from_earlier_test():
    # The kv write from test_remembers_previous_name_via_kv above must not
    # be visible here -- the autouse fixture resets kv before every test.
    params.set("name", "Nobody")
    http.mock("GET", "https://httpbin.org/get", {"status": 200, "body": {"origin": "127.0.0.1"}})

    result = run_task()

    assert result["greeting"] == "Hello, Nobody! (run #1)"
    assert kv.get("previous_name") == "Nobody"


def test_fails_loudly_on_unmocked_http_call():
    # No http.mock() registered for this test -- the harness must refuse to
    # let the call fall through to the real network, mirroring
    # tasks/sdk-test.ts's "no accidental real HTTP in tests" guarantee.
    params.set("name", "Nobody")
    try:
        run_task()
    except AssertionError as exc:
        assert "no http mock matches" in str(exc)
    else:
        raise AssertionError("expected run_task() to raise for an unmocked HTTP call")


if __name__ == "__main__":
    run_pytest_main(__file__)
