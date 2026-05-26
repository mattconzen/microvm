"""Tests for the pluggable Snapshotter backends."""

from __future__ import annotations

import json
import os
import pathlib
import shutil
import subprocess

import pytest

import snapshotter as sn


def test_alias_snapshotter_returns_session(monkeypatch):
    monkeypatch.setenv("BEDROCK_AGENTCORE_SESSION_ID", "sess-xyz")
    s = sn.AliasSnapshotter()
    out = s.snapshot("snp_1", "demo")
    assert out["alias"] == "sess-xyz"
    assert out["name"] == "demo"
    assert out["locator"] == ""


def test_alias_resume_uses_provided_alias():
    s = sn.AliasSnapshotter()
    out = s.resume("", "sess-from-arg")
    assert out["alias"] == "sess-from-arg"


def test_alias_resume_falls_back_to_env(monkeypatch):
    monkeypatch.setenv("BEDROCK_AGENTCORE_SESSION_ID", "sess-env")
    s = sn.AliasSnapshotter()
    out = s.resume("", "")
    assert out["alias"] == "sess-env"


def test_s3_snapshotter_roundtrips_workspace(monkeypatch, tmp_path):
    workspace = tmp_path / "workspace"
    workspace.mkdir()
    (workspace / "hello.txt").write_text("hi from sandbox")
    (workspace / "sub").mkdir()
    (workspace / "sub" / "deep.txt").write_text("nested")

    remote = tmp_path / "remote.tar.gz"
    uploads: list[tuple[str, str]] = []
    downloads: list[tuple[str, str]] = []

    class FakeS3(sn.S3Snapshotter):
        def _aws_cp_up(self, local, uri):
            uploads.append((local, uri))
            shutil.copy(local, str(remote))

        def _aws_cp_down(self, uri, local):
            downloads.append((uri, local))
            shutil.copy(str(remote), local)

    monkeypatch.setenv("BEDROCK_AGENTCORE_SESSION_ID", "sess-1")
    s = FakeS3(bucket="my-bucket", prefix="microvm/", workspace=str(workspace))
    out = s.snapshot("snp_abc", "baseline")

    assert out["alias"] == "sess-1"
    locator = json.loads(out["locator"])
    assert locator["s3_uri"] == "s3://my-bucket/microvm/snp_abc.tar.gz"
    assert len(uploads) == 1

    # Mutate workspace, then resume — should restore the original contents
    # (and remove the junk files added after the snapshot).
    (workspace / "hello.txt").write_text("DIRTY")
    (workspace / "junk.txt").write_text("delete me")
    (workspace / "junk_dir").mkdir()
    (workspace / "junk_dir" / "x").write_text("x")

    out2 = s.resume(out["locator"], "sess-1")
    assert out2["alias"] == "sess-1"
    assert (workspace / "hello.txt").read_text() == "hi from sandbox"
    assert (workspace / "sub" / "deep.txt").read_text() == "nested"
    assert not (workspace / "junk.txt").exists()
    assert not (workspace / "junk_dir").exists()
    assert len(downloads) == 1


def test_s3_resume_empty_locator_errors():
    s = sn.S3Snapshotter(bucket="b", prefix="", workspace="/tmp/_unused_microvm_test")
    out = s.resume("", "sess-1")
    assert out["alias"] == "sess-1"
    assert "empty locator" in out["error"]


def test_s3_resume_bad_locator_errors():
    s = sn.S3Snapshotter(bucket="b", prefix="", workspace="/tmp/_unused_microvm_test")
    out = s.resume("not-json", "sess-1")
    assert "bad locator" in out["error"]


def test_s3_resume_missing_s3_uri_errors():
    s = sn.S3Snapshotter(bucket="b", prefix="", workspace="/tmp/_unused_microvm_test")
    out = s.resume(json.dumps({"other": "x"}), "sess-1")
    assert "missing s3_uri" in out["error"]


def test_s3_key_normalizes_empty_prefix():
    s = sn.S3Snapshotter(bucket="b", prefix="", workspace="/tmp")
    assert s._uri("snp_x") == "s3://b/snp_x.tar.gz"


def test_s3_key_strips_trailing_slashes():
    s = sn.S3Snapshotter(bucket="b", prefix="a/b///", workspace="/tmp")
    assert s._uri("snp_x") == "s3://b/a/b/snp_x.tar.gz"


def test_make_snapshotter_default_is_alias(monkeypatch):
    monkeypatch.delenv("MICROVM_SNAPSHOT_MODE", raising=False)
    assert isinstance(sn.make_snapshotter(), sn.AliasSnapshotter)


def test_make_snapshotter_explicit_none(monkeypatch):
    monkeypatch.setenv("MICROVM_SNAPSHOT_MODE", "none")
    assert isinstance(sn.make_snapshotter(), sn.AliasSnapshotter)


def test_make_snapshotter_s3(monkeypatch):
    monkeypatch.setenv("MICROVM_SNAPSHOT_MODE", "s3")
    monkeypatch.setenv("MICROVM_SNAPSHOT_BUCKET", "my-bucket")
    monkeypatch.setenv("MICROVM_SNAPSHOT_PREFIX", "p/")
    got = sn.make_snapshotter()
    assert isinstance(got, sn.S3Snapshotter)
    assert got.bucket == "my-bucket"
    assert got.prefix == "p/"


def test_make_snapshotter_s3_requires_bucket(monkeypatch):
    monkeypatch.setenv("MICROVM_SNAPSHOT_MODE", "s3")
    monkeypatch.delenv("MICROVM_SNAPSHOT_BUCKET", raising=False)
    with pytest.raises(RuntimeError, match="MICROVM_SNAPSHOT_BUCKET"):
        sn.make_snapshotter()


import shutil as _shutil
_HAS_RSYNC = _shutil.which("rsync") is not None
_skip_if_no_rsync = pytest.mark.skipif(not _HAS_RSYNC, reason="rsync not installed")


@_skip_if_no_rsync
def test_efs_snapshotter_roundtrips_workspace(tmp_path):
    # We fake out the rsync subprocess to operate on local paths so the test
    # runs without an actual EFS mount.
    mount = tmp_path / "efs"
    sessions = mount / "sessions" / "mvm_abc"
    sessions.mkdir(parents=True)
    (sessions / "hello.txt").write_text("hi from sandbox")
    (sessions / "sub").mkdir()
    (sessions / "sub" / "deep.txt").write_text("nested")

    s = sn.EfsSnapshotter(mount=str(mount))
    out = s.snapshot("snp_xyz", "baseline", sandbox_id="mvm_abc")

    locator = json.loads(out["locator"])
    assert locator["efs_path"] == str(mount / "snapshots" / "snp_xyz")
    snap_path = mount / "snapshots" / "snp_xyz"
    assert (snap_path / "hello.txt").read_text() == "hi from sandbox"
    assert (snap_path / "sub" / "deep.txt").read_text() == "nested"
    # No leftover .tmp dir after a successful snapshot.
    assert not (mount / ".tmp" / "snp_xyz").exists()


@_skip_if_no_rsync
def test_efs_snapshotter_resume_into_new_session(tmp_path):
    mount = tmp_path / "efs"
    snap_dir = mount / "snapshots" / "snp_xyz"
    snap_dir.mkdir(parents=True)
    (snap_dir / "restored.txt").write_text("from snap")

    s = sn.EfsSnapshotter(mount=str(mount))
    locator = json.dumps({"efs_path": str(snap_dir)})
    # The shellagent reads its own sandbox_id from the envelope; pass it in.
    out = s.resume(locator, "sess-1", sandbox_id="mvm_new")
    assert out["alias"] == "sess-1"
    assert (mount / "sessions" / "mvm_new" / "restored.txt").read_text() == "from snap"


def test_efs_resume_empty_locator_errors(tmp_path):
    s = sn.EfsSnapshotter(mount=str(tmp_path))
    out = s.resume("", "sess-1", sandbox_id="mvm_x")
    assert "empty locator" in out["error"]


def test_efs_resume_missing_efs_path_errors(tmp_path):
    s = sn.EfsSnapshotter(mount=str(tmp_path))
    out = s.resume(json.dumps({"other": "x"}), "sess-1", sandbox_id="mvm_x")
    assert "missing efs_path" in out["error"]


def test_efs_snapshot_requires_sandbox_id(tmp_path):
    s = sn.EfsSnapshotter(mount=str(tmp_path))
    out = s.snapshot("snp_x", "n", sandbox_id="")
    assert "sandbox_id required" in out["error"]


def test_efs_snapshot_rejects_traversal_sandbox_id(tmp_path):
    s = sn.EfsSnapshotter(mount=str(tmp_path))
    out = s.snapshot("snp_x", "n", sandbox_id="../etc")
    assert "invalid" in out["error"]
    assert out["locator"] == ""


def test_efs_snapshot_rejects_traversal_snap_id(tmp_path):
    s = sn.EfsSnapshotter(mount=str(tmp_path))
    out = s.snapshot("../../etc", "n", sandbox_id="mvm_ok")
    assert "invalid" in out["error"]
    assert out["locator"] == ""


def test_efs_resume_rejects_traversal_sandbox_id(tmp_path):
    s = sn.EfsSnapshotter(mount=str(tmp_path))
    locator = json.dumps({"efs_path": str(tmp_path / "snapshots" / "snp_x")})
    out = s.resume(locator, "sess-1", sandbox_id="../etc")
    assert "invalid" in out["error"]


def test_efs_partial_snapshot_is_cleaned_up(tmp_path, monkeypatch):
    """If rsync fails mid-copy, the .tmp dir should be removed and no
    snapshot dir should appear at the published path."""
    mount = tmp_path / "efs"
    (mount / "sessions" / "mvm_a").mkdir(parents=True)

    class BoomEfs(sn.EfsSnapshotter):
        def _rsync(self, src, dst):
            # Simulate a partial write before raising.
            os.makedirs(dst, exist_ok=True)
            (pathlib.Path(dst) / "half-written.txt").write_text("oops")
            raise subprocess.CalledProcessError(1, ["rsync"], stderr="disk full")

    s = BoomEfs(mount=str(mount))
    out = s.snapshot("snp_bad", "n", sandbox_id="mvm_a")
    assert "rsync failed" in out["error"]
    assert not (mount / "snapshots" / "snp_bad").exists()
    assert not (mount / ".tmp" / "snp_bad").exists()


def test_make_snapshotter_efs(monkeypatch):
    monkeypatch.setenv("MICROVM_SNAPSHOT_MODE", "efs")
    monkeypatch.setenv("MICROVM_EFS_MOUNT_PATH", "/data")
    got = sn.make_snapshotter()
    assert isinstance(got, sn.EfsSnapshotter)
    assert got.mount == "/data"


def test_make_snapshotter_efs_defaults_mount(monkeypatch):
    monkeypatch.setenv("MICROVM_SNAPSHOT_MODE", "efs")
    monkeypatch.delenv("MICROVM_EFS_MOUNT_PATH", raising=False)
    got = sn.make_snapshotter()
    assert got.mount == "/mnt/efs"


def test_tiered_snapshotter_emits_prefix_locator(monkeypatch):
    calls = []

    class FakeTiered(sn.TieredSnapshotter):
        def _aws_cp_recursive(self, src, dst):
            calls.append((src, dst))

    monkeypatch.setenv("BEDROCK_AGENTCORE_SESSION_ID", "sess-1")
    s = FakeTiered(bucket="microvm-fs", mount="/workspace")
    out = s.snapshot("snp_x", "baseline", sandbox_id="mvm_a")

    assert out["alias"] == "sess-1"
    locator = json.loads(out["locator"])
    assert locator == {
        "workspace_prefix": "s3://microvm-fs/sessions/mvm_a/",
        "snapshot_prefix":  "s3://microvm-fs/snapshots/snp_x/",
    }
    assert calls == [("s3://microvm-fs/sessions/mvm_a/", "s3://microvm-fs/snapshots/snp_x/")]


def test_tiered_snapshotter_resume_copies_back(monkeypatch):
    calls = []

    class FakeTiered(sn.TieredSnapshotter):
        def _aws_cp_recursive(self, src, dst):
            calls.append((src, dst))

    monkeypatch.setenv("BEDROCK_AGENTCORE_SESSION_ID", "sess-2")
    s = FakeTiered(bucket="microvm-fs", mount="/workspace")
    locator = json.dumps({
        "workspace_prefix": "s3://microvm-fs/sessions/mvm_old/",
        "snapshot_prefix":  "s3://microvm-fs/snapshots/snp_x/",
    })
    out = s.resume(locator, "sess-2", sandbox_id="mvm_new")
    assert out["alias"] == "sess-2"
    assert calls == [("s3://microvm-fs/snapshots/snp_x/", "s3://microvm-fs/sessions/mvm_new/")]


def test_tiered_snapshot_requires_sandbox_id():
    s = sn.TieredSnapshotter(bucket="b", mount="/workspace")
    out = s.snapshot("snp_x", "n", sandbox_id="")
    assert "sandbox_id required" in out["error"]


def test_tiered_resume_empty_locator_errors():
    s = sn.TieredSnapshotter(bucket="b", mount="/workspace")
    out = s.resume("", "sess-1", sandbox_id="mvm_x")
    assert "empty locator" in out["error"]


def test_tiered_resume_missing_snapshot_prefix_errors():
    s = sn.TieredSnapshotter(bucket="b", mount="/workspace")
    out = s.resume(json.dumps({"workspace_prefix": "s3://b/sessions/o/"}), "sess-1", sandbox_id="mvm_x")
    assert "missing snapshot_prefix" in out["error"]


def test_tiered_rejects_traversal_sandbox_id():
    s = sn.TieredSnapshotter(bucket="b", mount="/workspace")
    out = s.snapshot("snp_x", "n", sandbox_id="../etc")
    assert "invalid" in out["error"]


def test_make_snapshotter_tiered(monkeypatch):
    monkeypatch.setenv("MICROVM_SNAPSHOT_MODE", "tiered")
    monkeypatch.setenv("MICROVM_S3FILES_BUCKET", "microvm-fs")
    monkeypatch.setenv("MICROVM_S3FILES_MOUNT_PATH", "/workspace")
    got = sn.make_snapshotter()
    assert isinstance(got, sn.TieredSnapshotter)
    assert got.bucket == "microvm-fs"
    assert got.mount == "/workspace"


def test_make_snapshotter_tiered_requires_bucket(monkeypatch):
    monkeypatch.setenv("MICROVM_SNAPSHOT_MODE", "tiered")
    monkeypatch.delenv("MICROVM_S3FILES_BUCKET", raising=False)
    with pytest.raises(RuntimeError, match="MICROVM_S3FILES_BUCKET"):
        sn.make_snapshotter()


def test_make_snapshotter_unknown(monkeypatch):
    monkeypatch.setenv("MICROVM_SNAPSHOT_MODE", "bogus")
    with pytest.raises(RuntimeError, match="unknown MICROVM_SNAPSHOT_MODE"):
        sn.make_snapshotter()
