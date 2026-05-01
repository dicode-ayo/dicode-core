"""Unit tests for dicode_sdk.py.

Each test stands up a threaded fake server on a fresh Unix-domain socket,
reloads ``dicode_sdk`` so its module-level handshake + ``input`` fetch hit
the fake server, then drives the SDK and asserts what the fake server
received.

Run from this directory: ``python3 -m unittest -v test_dicode_sdk``.
"""
import asyncio
import importlib
import json
import os
import shutil
import socket
import struct
import sys
import tempfile
import threading
import time
import unittest

SDK_DIR = os.path.dirname(os.path.abspath(__file__))
if SDK_DIR not in sys.path:
    sys.path.insert(0, SDK_DIR)


class FakeServer:
    """Minimal Unix-socket server speaking dicode's length-prefixed JSON IPC."""

    def __init__(self, path):
        self.path = path
        self.sock = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
        self.sock.bind(path)
        self.sock.listen(1)
        self.sock.settimeout(2.0)

        self.received = []
        self.received_lock = threading.Lock()
        self.handlers = {}
        self.input_value = None
        self.caps = ["log", "params.read", "kv.read", "kv.write",
                     "input.read", "output.write"]

        self._client = None
        self._stop = threading.Event()
        self._thread = threading.Thread(target=self._serve, daemon=True)

    def start(self):
        self._thread.start()

    def stop(self):
        self._stop.set()
        for closer in (self._client, self.sock):
            try:
                if closer is not None:
                    closer.close()
            except OSError:
                pass
        try:
            os.unlink(self.path)
        except FileNotFoundError:
            pass

    def messages(self, method=None):
        with self.received_lock:
            if method is None:
                return list(self.received)
            return [m for m in self.received if m.get("method") == method]

    def wait_for(self, predicate, timeout=2.0):
        deadline = time.monotonic() + timeout
        while time.monotonic() < deadline:
            with self.received_lock:
                for m in self.received:
                    if predicate(m):
                        return m
            time.sleep(0.01)
        return None

    def _serve(self):
        try:
            client, _ = self.sock.accept()
        except (socket.timeout, OSError):
            return
        self._client = client
        try:
            handshake = self._read(client)
            if handshake is None:
                return
            self._write(client, {"proto": 1, "caps": self.caps})

            first = self._read(client)
            if first is None:
                return
            self._record(first)
            if first.get("method") == "input" and first.get("id") is not None:
                self._write(client, {"id": first["id"], "result": self.input_value})

            while not self._stop.is_set():
                msg = self._read(client)
                if msg is None:
                    return
                self._record(msg)
                rid = msg.get("id")
                handler = self.handlers.get(msg.get("method"))
                result = handler(msg) if handler is not None else None
                if rid is not None:
                    self._write(client, {"id": rid, "result": result})
        except (OSError, ConnectionError):
            return

    def _record(self, msg):
        with self.received_lock:
            self.received.append(msg)

    @staticmethod
    def _read(client):
        hdr = b""
        while len(hdr) < 4:
            chunk = client.recv(4 - len(hdr))
            if not chunk:
                return None
            hdr += chunk
        (size,) = struct.unpack("<I", hdr)
        body = b""
        while len(body) < size:
            chunk = client.recv(size - len(body))
            if not chunk:
                return None
            body += chunk
        return json.loads(body)

    @staticmethod
    def _write(client, obj):
        body = json.dumps(obj, separators=(",", ":")).encode()
        client.sendall(struct.pack("<I", len(body)) + body)


class SDKTestBase(unittest.TestCase):
    def setUp(self):
        self.tmpdir = tempfile.mkdtemp(prefix="dicode-sdk-test-")
        self.socket_path = os.path.join(self.tmpdir, "ipc.sock")
        self.server = FakeServer(self.socket_path)
        self.server.start()
        os.environ["DICODE_SOCKET"] = self.socket_path
        os.environ["DICODE_TOKEN"] = "test-token"

    def tearDown(self):
        sdk = sys.modules.get("dicode_sdk")
        if sdk is not None:
            self._shutdown_sdk(sdk)
        self.server.stop()
        shutil.rmtree(self.tmpdir, ignore_errors=True)

    @staticmethod
    def _shutdown_sdk(sdk):
        # The SDK has no shutdown hook (it's designed to live for the task
        # subprocess's lifetime). For tests that reload the module, cancel
        # outstanding tasks on the leaked loop so we don't get
        # "Task was destroyed but it is pending" warnings.
        loop = getattr(sdk, "_loop", None)
        if loop is None or not loop.is_running():
            return

        async def _cancel_all():
            for task in asyncio.all_tasks(loop):
                if task is not asyncio.current_task():
                    task.cancel()

        try:
            asyncio.run_coroutine_threadsafe(_cancel_all(), loop).result(timeout=1.0)
        except Exception:
            pass

    def load_sdk(self):
        sys.modules.pop("dicode_sdk", None)
        return importlib.import_module("dicode_sdk")


class LogTests(SDKTestBase):
    def test_log_sends_correct_json(self):
        sdk = self.load_sdk()
        sdk.log.info("hello")
        msg = self.server.wait_for(lambda m: m.get("method") == "log")
        self.assertIsNotNone(msg, "log message never arrived")
        self.assertEqual(msg["level"], "info")
        self.assertEqual(msg["message"], "hello")
        self.assertNotIn("id", msg)


class ParamsTests(SDKTestBase):
    def test_params_get(self):
        self.server.handlers["params"] = lambda _: {"k": "v", "n": "2"}
        sdk = self.load_sdk()
        self.assertEqual(sdk.params.get("k"), "v")
        self.assertEqual(sdk.params.get("missing", "default"), "default")
        self.assertEqual(len(self.server.messages("params")), 1,
                         "params should be fetched once and cached")

    def test_params_all(self):
        self.server.handlers["params"] = lambda _: {"a": "1", "b": "2"}
        sdk = self.load_sdk()
        self.assertEqual(sdk.params.all(), {"a": "1", "b": "2"})


class KVTests(SDKTestBase):
    def test_kv_set_get_delete(self):
        store = {}
        self.server.handlers["kv.get"] = lambda m: store.get(m["key"])
        self.server.handlers["kv.set"] = lambda m: store.update({m["key"]: m["value"]}) or None
        self.server.handlers["kv.delete"] = lambda m: store.pop(m["key"], None) or None

        sdk = self.load_sdk()
        sdk.kv.set("foo", "bar")
        self.assertIsNotNone(self.server.wait_for(
            lambda m: m.get("method") == "kv.set" and m.get("key") == "foo"))
        self.assertEqual(sdk.kv.get("foo"), "bar")
        sdk.kv.delete("foo")
        self.assertIsNotNone(self.server.wait_for(
            lambda m: m.get("method") == "kv.delete" and m.get("key") == "foo"))
        self.assertIsNone(sdk.kv.get("foo"))

    def test_kv_list(self):
        def kv_list(msg):
            prefix = msg.get("prefix", "")
            all_keys = ["foo", "bar", "foo:1"]
            return [k for k in all_keys if k.startswith(prefix)]
        self.server.handlers["kv.list"] = kv_list

        sdk = self.load_sdk()
        self.assertEqual(sorted(sdk.kv.list()), sorted(["foo", "bar", "foo:1"]))
        self.assertEqual(sorted(sdk.kv.list("foo")), sorted(["foo", "foo:1"]))


class InputTests(SDKTestBase):
    def test_input_none(self):
        self.server.input_value = None
        sdk = self.load_sdk()
        self.assertIsNone(sdk.input)

    def test_input_value(self):
        self.server.input_value = {"foo": "bar", "n": 7}
        sdk = self.load_sdk()
        self.assertEqual(sdk.input, {"foo": "bar", "n": 7})


class OutputTests(SDKTestBase):
    def test_output_html(self):
        sdk = self.load_sdk()
        sdk.output.html("<p>hi</p>", data={"k": "v"})
        msg = self.server.wait_for(lambda m: m.get("method") == "output")
        self.assertIsNotNone(msg, "output message never arrived")
        self.assertEqual(msg["contentType"], "text/html")
        self.assertEqual(msg["content"], "<p>hi</p>")
        self.assertEqual(msg["data"], {"k": "v"})


class DicodeTests(SDKTestBase):
    def test_dicode_run_task(self):
        captured = {}

        def run_task(msg):
            captured["taskID"] = msg.get("taskID")
            captured["params"] = msg.get("params")
            return {"runID": "r1", "status": "ok"}
        self.server.handlers["dicode.run_task"] = run_task

        sdk = self.load_sdk()
        result = sdk.dicode.run_task("my-task", {"x": 1})
        self.assertEqual(captured, {"taskID": "my-task", "params": {"x": 1}})
        self.assertEqual(result, {"runID": "r1", "status": "ok"})

    def test_dicode_list_tasks(self):
        self.server.handlers["dicode.list_tasks"] = lambda _: [
            {"id": "a"}, {"id": "b"},
        ]
        sdk = self.load_sdk()
        self.assertEqual(sdk.dicode.list_tasks(), [{"id": "a"}, {"id": "b"}])

    def test_dicode_get_runs(self):
        captured = {}

        def get_runs(msg):
            captured["taskID"] = msg.get("taskID")
            captured["limit"] = msg.get("limit")
            return [{"runID": "r1"}, {"runID": "r2"}]
        self.server.handlers["dicode.get_runs"] = get_runs

        sdk = self.load_sdk()
        result = sdk.dicode.get_runs("my-task", limit=5)
        self.assertEqual(captured, {"taskID": "my-task", "limit": 5})
        self.assertEqual(result, [{"runID": "r1"}, {"runID": "r2"}])


class MCPTests(SDKTestBase):
    def test_mcp_list_tools(self):
        captured = {}

        def list_tools(msg):
            captured["mcpName"] = msg.get("mcpName")
            return [{"name": "search"}, {"name": "fetch"}]
        self.server.handlers["mcp.list_tools"] = list_tools

        sdk = self.load_sdk()
        tools = sdk.mcp.list_tools("my-mcp")
        self.assertEqual(captured["mcpName"], "my-mcp")
        self.assertEqual(tools, [{"name": "search"}, {"name": "fetch"}])

    def test_mcp_call(self):
        captured = {}

        def call(msg):
            captured["mcpName"] = msg.get("mcpName")
            captured["tool"] = msg.get("tool")
            captured["args"] = msg.get("args")
            return {"output": "result"}
        self.server.handlers["mcp.call"] = call

        sdk = self.load_sdk()
        result = sdk.mcp.call("my-mcp", "search", {"q": "go"})
        self.assertEqual(captured, {
            "mcpName": "my-mcp",
            "tool": "search",
            "args": {"q": "go"},
        })
        self.assertEqual(result, {"output": "result"})


class ReturnTests(SDKTestBase):
    def test_set_return(self):
        captured = {}

        def on_return(msg):
            captured["value"] = msg.get("value")
            return None
        self.server.handlers["return"] = on_return

        sdk = self.load_sdk()
        sdk._set_return({"answer": 42})
        self.assertEqual(captured.get("value"), {"answer": 42})


if __name__ == "__main__":
    unittest.main()
