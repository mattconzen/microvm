"""Tests for the shell-agent dispatch logic. Runs without the bedrock-agentcore SDK."""

from __future__ import annotations

import base64
import os
import shutil as _shutil
import tempfile

import pytest

import app


@pytest.fixture(autouse=True)
def reset_snapshotter(monkeypatch):
    # The module caches the Snapshotter on first use. Reset between tests so
    # MICROVM_SNAPSHOT_MODE env changes take effect and tests stay isolated.
    monkeypatch.delenv("MICROVM_SNAPSHOT_MODE", raising=False)
    app._snapshotter = None
    yield
    app._snapshotter = None


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
    assert out["locator"] == ""


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
    assert snap["alias"] == "sess-d"
    assert snap["name"] == "n"
    assert snap["locator"] == ""
    res = app.dispatch({"op": "resume", "alias": "sess-abc"})
    assert res == {"alias": "sess-abc"}


def test_dispatch_snapshot_forwards_sandbox_id(monkeypatch):
    monkeypatch.setenv("BEDROCK_AGENTCORE_SESSION_ID", "sess-d")
    captured = {}

    class Spy:
        mode = "spy"

        def snapshot(self, snap_id, name, sandbox_id=""):
            captured["snapshot"] = (snap_id, name, sandbox_id)
            return {"alias": "sess-d", "name": name, "locator": ""}

        def resume(self, locator, alias, sandbox_id=""):
            captured["resume"] = (locator, alias, sandbox_id)
            return {"alias": alias}

    app._snapshotter = Spy()
    app.dispatch({"op": "snapshot", "name": "n", "snap_id": "snp_1", "sandbox_id": "mvm_xyz"})
    assert captured["snapshot"] == ("snp_1", "n", "mvm_xyz")
    app.dispatch({"op": "resume", "alias": "sess-1", "locator": "{}", "sandbox_id": "mvm_xyz"})
    assert captured["resume"] == ("{}", "sess-1", "mvm_xyz")


def test_snapshot_surfaces_backend_errors(monkeypatch):
    # If make_snapshotter raises (bad env), handle_snapshot should return a
    # well-formed dict with the error captured rather than propagate.
    monkeypatch.setenv("MICROVM_SNAPSHOT_MODE", "bogus")
    out = app.handle_snapshot({"name": "demo"})
    assert out["alias"] == ""
    assert out["name"] == "demo"
    assert out["locator"] == ""
    assert "unknown MICROVM_SNAPSHOT_MODE" in out["error"]


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


def test_checkpoint_requires_sandbox_id():
    out = app.handle_checkpoint({})
    assert out["ok"] is False
    assert "sandbox_id required" in out["error"]


def test_checkpoint_rejects_non_tiered_mode(monkeypatch):
    monkeypatch.setenv("MICROVM_SNAPSHOT_MODE", "s3")
    out = app.handle_checkpoint({"sandbox_id": "mvm_a"})
    assert out["ok"] is False
    assert "tiered mode" in out["error"]


def test_checkpoint_rsyncs_promote_into_workspace(tmp_path, monkeypatch):
    monkeypatch.setenv("MICROVM_SNAPSHOT_MODE", "tiered")
    monkeypatch.setenv("MICROVM_CACHE_ROOT", str(tmp_path / "cache"))
    monkeypatch.setenv("MICROVM_S3FILES_MOUNT_PATH", str(tmp_path / "workspace"))

    promote = tmp_path / "cache" / "mvm_a" / "promote"
    promote.mkdir(parents=True)
    (promote / "artifact.txt").write_text("hello")

    if not _shutil.which("rsync"):
        pytest.skip("rsync not installed")

    out = app.handle_checkpoint({"sandbox_id": "mvm_a"})
    assert out["ok"] is True
    assert (tmp_path / "workspace" / "mvm_a" / "cache-promoted" / "artifact.txt").read_text() == "hello"


def test_checkpoint_rejects_traversal_sandbox_id():
    out = app.handle_checkpoint({"sandbox_id": "../etc"})
    assert out["ok"] is False
    assert "invalid" in out["error"]


def test_dispatch_routes_checkpoint(monkeypatch):
    monkeypatch.setenv("MICROVM_SNAPSHOT_MODE", "tiered")
    out = app.dispatch({"op": "checkpoint", "sandbox_id": "mvm_a"})
    # Either ok=True (rsync ran) or ok=False with an error;
    # what we're verifying is that dispatch routed to handle_checkpoint
    # (not "unknown op").
    assert "ok" in out


def test_shell_session_callable_exists():
    # Ensures the websocket handler is importable and async.
    import inspect

    assert inspect.iscoroutinefunction(app.shell_session)


def test_exec_default_cwd_is_workspace_in_tiered_mode(tmp_path, monkeypatch):
    monkeypatch.setenv("MICROVM_SNAPSHOT_MODE", "tiered")
    monkeypatch.setenv("MICROVM_S3FILES_MOUNT_PATH", str(tmp_path / "workspace"))
    out = app.handle_exec({"cmd": ["pwd"], "sandbox_id": "mvm_a"})
    assert out["exit"] == 0
    assert out["stdout"].strip() == str(tmp_path / "workspace" / "mvm_a")


def test_exec_inherits_cwd_in_non_tiered_mode(monkeypatch):
    monkeypatch.delenv("MICROVM_SNAPSHOT_MODE", raising=False)
    out = app.handle_exec({"cmd": ["pwd"], "sandbox_id": "mvm_a"})
    assert out["exit"] == 0
    assert "/workspace/" not in out["stdout"]


def test_exec_tiered_without_sandbox_id_inherits(monkeypatch):
    monkeypatch.setenv("MICROVM_SNAPSHOT_MODE", "tiered")
    out = app.handle_exec({"cmd": ["pwd"]})
    assert out["exit"] == 0
    assert "/workspace/" not in out["stdout"]
