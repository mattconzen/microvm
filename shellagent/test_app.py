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


def test_terminate_returns_ok():
    out = app.handle_terminate({})
    assert out["ok"] is True


def test_dispatch_terminate():
    out = app.dispatch({"op": "terminate"})
    assert out["ok"] is True
