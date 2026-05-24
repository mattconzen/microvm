"""Tests for the pluggable Snapshotter backends."""

from __future__ import annotations

import json
import shutil

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


def test_make_snapshotter_efs_not_implemented(monkeypatch):
    monkeypatch.setenv("MICROVM_SNAPSHOT_MODE", "efs")
    with pytest.raises(NotImplementedError, match="PR2"):
        sn.make_snapshotter()


def test_make_snapshotter_tiered_not_implemented(monkeypatch):
    monkeypatch.setenv("MICROVM_SNAPSHOT_MODE", "tiered")
    with pytest.raises(NotImplementedError, match="PR3"):
        sn.make_snapshotter()


def test_make_snapshotter_unknown(monkeypatch):
    monkeypatch.setenv("MICROVM_SNAPSHOT_MODE", "bogus")
    with pytest.raises(RuntimeError, match="unknown MICROVM_SNAPSHOT_MODE"):
        sn.make_snapshotter()
