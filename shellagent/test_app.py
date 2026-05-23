"""Tests for the shell-agent dispatch logic. Runs without the bedrock-agentcore SDK."""

from __future__ import annotations

import base64
import os
import tempfile

import app


def test_exec_basic():
    out = app.handle_exec({"cmd": ["sh", "-c", "echo hi && echo err >&2"]})
    assert out["stdout"].strip() == "hi"
    assert "err" in out["stderr"]
    assert out["exit"] == 0


def test_exec_nonzero():
    out = app.handle_exec({"cmd": ["sh", "-c", "exit 7"]})
    assert out["exit"] == 7


def test_exec_empty_cmd():
    out = app.handle_exec({"cmd": []})
    assert out["exit"] == 2
    assert out["error"]


def test_exec_command_not_found():
    out = app.handle_exec({"cmd": ["definitely-not-a-real-binary-xyz"]})
    assert out["exit"] == 127


def test_put_then_get_roundtrip():
    with tempfile.TemporaryDirectory() as d:
        path = os.path.join(d, "nested", "file.txt")
        payload = b"hello microvm \x00\x01\xff"
        put = app.handle_put({"path": path, "b64": base64.b64encode(payload).decode()})
        assert put["ok"] is True
        assert put["bytes"] == len(payload)

        got = app.handle_get({"path": path})
        assert base64.b64decode(got["b64"]) == payload
        assert got["bytes"] == len(payload)


def test_get_missing_file():
    out = app.handle_get({"path": "/nonexistent/path/xyz"})
    assert out["b64"] == ""
    assert out["error"]


def test_dispatch_unknown_op():
    out = app.dispatch({"op": "nope"})
    assert "unknown op" in out["error"]


def test_snapshot_returns_alias(monkeypatch):
    monkeypatch.setenv("BEDROCK_AGENTCORE_SESSION_ID", "sess-xyz")
    out = app.handle_snapshot({"name": "demo"})
    assert out["alias"] == "sess-xyz"
    assert out["name"] == "demo"


def test_snapshot_alias_empty_without_session_env(monkeypatch):
    monkeypatch.delenv("BEDROCK_AGENTCORE_SESSION_ID", raising=False)
    out = app.handle_snapshot({"name": "demo"})
    assert out["alias"] == ""


def test_resume_acks():
    out = app.handle_resume({"alias": "sess-1"})
    assert out["alias"] == "sess-1"


def test_resume_falls_back_to_env(monkeypatch):
    monkeypatch.setenv("BEDROCK_AGENTCORE_SESSION_ID", "sess-env")
    out = app.handle_resume({})
    assert out["alias"] == "sess-env"


def test_dispatch_routes_snapshot_resume(monkeypatch):
    monkeypatch.setenv("BEDROCK_AGENTCORE_SESSION_ID", "sess-d")
    snap = app.dispatch({"op": "snapshot", "name": "n"})
    assert snap == {"alias": "sess-d", "name": "n"}
    res = app.dispatch({"op": "resume", "alias": "sess-abc"})
    assert res == {"alias": "sess-abc"}


def test_terminate_returns_ok():
    out = app.handle_terminate({})
    assert out["ok"] is True


def test_dispatch_terminate():
    out = app.dispatch({"op": "terminate"})
    assert out["ok"] is True


def test_dispatch_no_longer_handles_shell():
    # Shell now flows through the WebSocket entrypoint, not dispatch().
    out = app.dispatch({"op": "shell"})
    assert "unknown op" in out["error"]


def test_parse_resize_dict():
    assert app.parse_resize({"type": "resize", "cols": 80, "rows": 24}) == (80, 24)


def test_parse_resize_json_str():
    assert app.parse_resize('{"type":"resize","cols":132,"rows":50}') == (132, 50)


def test_parse_resize_json_bytes():
    assert app.parse_resize(b'{"type":"resize","cols":120,"rows":40}') == (120, 40)


def test_parse_resize_rejects_other_types():
    assert app.parse_resize({"type": "other", "cols": 80, "rows": 24}) is None
    assert app.parse_resize({"cols": 80, "rows": 24}) is None
    assert app.parse_resize("not json") is None
    assert app.parse_resize({"type": "resize", "cols": 0, "rows": 24}) is None
    assert app.parse_resize({"type": "resize", "cols": "huh", "rows": 24}) is None
    assert app.parse_resize(123) is None


def test_shell_session_callable_exists():
    # Ensures the websocket handler is importable and async.
    import inspect

    assert inspect.iscoroutinefunction(app.shell_session)
