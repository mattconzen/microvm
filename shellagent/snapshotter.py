"""Pluggable snapshot backends for the shell agent.

Strategy is chosen at process start from MICROVM_SNAPSHOT_MODE:
  none   -- alias-only (returns the AgentCore session id, no I/O)
  s3     -- tar+gzip /workspace -> s3://<bucket>/<prefix>/<id>.tar.gz
  efs    -- rsync between <mount>/sessions/<sb>/ and <mount>/snapshots/<snap>/
  tiered -- TODO (PR3)
"""

from __future__ import annotations

import json
import os
import shutil
import subprocess
import tarfile
import tempfile
from abc import ABC, abstractmethod
from typing import Any

WORKSPACE = "/workspace"


class Snapshotter(ABC):
    mode: str

    @abstractmethod
    def snapshot(self, snap_id: str, name: str, sandbox_id: str = "") -> dict[str, Any]:
        """Capture a snapshot; return {"alias", "locator", "name"}."""

    @abstractmethod
    def resume(self, locator: str, alias: str, sandbox_id: str = "") -> dict[str, Any]:
        """Materialise the snapshot under WORKSPACE; return {"alias"}."""


class AliasSnapshotter(Snapshotter):
    """Legacy behavior: no filesystem state, just echo the session id."""

    mode = "none"

    def snapshot(self, snap_id: str, name: str, sandbox_id: str = "") -> dict[str, Any]:
        alias = os.environ.get("BEDROCK_AGENTCORE_SESSION_ID", "")
        return {"alias": alias, "name": name, "locator": ""}

    def resume(self, locator: str, alias: str, sandbox_id: str = "") -> dict[str, Any]:
        out = alias or os.environ.get("BEDROCK_AGENTCORE_SESSION_ID", "")
        return {"alias": out}


class S3Snapshotter(Snapshotter):
    """tar+gzip /workspace to s3://<bucket>/<prefix>/<snap_id>.tar.gz."""

    mode = "s3"

    def __init__(self, bucket: str, prefix: str, *, workspace: str = WORKSPACE):
        self.bucket = bucket
        # Normalize prefix to always end with "/" so we can concat cleanly.
        # Empty prefix means objects land at the bucket root.
        self.prefix = (prefix.rstrip("/") + "/") if prefix else ""
        self.workspace = workspace

    def _key(self, snap_id: str) -> str:
        return f"{self.prefix}{snap_id}.tar.gz"

    def _uri(self, snap_id: str) -> str:
        return f"s3://{self.bucket}/{self._key(snap_id)}"

    def snapshot(self, snap_id: str, name: str, sandbox_id: str = "") -> dict[str, Any]:
        alias = os.environ.get("BEDROCK_AGENTCORE_SESSION_ID", "")
        os.makedirs(self.workspace, exist_ok=True)
        with tempfile.NamedTemporaryFile(suffix=".tar.gz", delete=False) as tmp:
            tmp_path = tmp.name
        try:
            with tarfile.open(tmp_path, "w:gz") as tar:
                tar.add(self.workspace, arcname=".")
            self._aws_cp_up(tmp_path, self._uri(snap_id))
        finally:
            try:
                os.unlink(tmp_path)
            except OSError:
                pass
        locator = json.dumps({"s3_uri": self._uri(snap_id)})
        return {"alias": alias, "name": name, "locator": locator}

    def resume(self, locator: str, alias: str, sandbox_id: str = "") -> dict[str, Any]:
        out = alias or os.environ.get("BEDROCK_AGENTCORE_SESSION_ID", "")
        if not locator:
            return {"alias": out, "error": "s3 resume: empty locator"}
        try:
            info = json.loads(locator)
        except (ValueError, json.JSONDecodeError) as e:
            return {"alias": out, "error": f"s3 resume: bad locator: {e}"}
        uri = info.get("s3_uri")
        if not uri:
            return {"alias": out, "error": "s3 resume: locator missing s3_uri"}
        with tempfile.NamedTemporaryFile(suffix=".tar.gz", delete=False) as tmp:
            tmp_path = tmp.name
        try:
            self._aws_cp_down(uri, tmp_path)
            self._wipe_workspace()
            with tarfile.open(tmp_path, "r:gz") as tar:
                tar.extractall(self.workspace)
        finally:
            try:
                os.unlink(tmp_path)
            except OSError:
                pass
        return {"alias": out}

    def _wipe_workspace(self) -> None:
        os.makedirs(self.workspace, exist_ok=True)
        for entry in os.scandir(self.workspace):
            if entry.is_dir(follow_symlinks=False):
                shutil.rmtree(entry.path, ignore_errors=True)
            else:
                try:
                    os.unlink(entry.path)
                except OSError:
                    pass

    # Shell out to the AWS CLI rather than depending on boto3. AgentCore
    # images already include the CLI, and this keeps the shellagent dep
    # surface to bedrock-agentcore + stdlib. Override in tests.
    def _aws_cp_up(self, local: str, uri: str) -> None:
        subprocess.run(["aws", "s3", "cp", local, uri], check=True)

    def _aws_cp_down(self, uri: str, local: str) -> None:
        subprocess.run(["aws", "s3", "cp", uri, local], check=True)


class EfsSnapshotter(Snapshotter):
    """rsync-based snapshots between subdirs of a shared EFS mount.

    Layout: <mount>/sessions/<sandbox_id>/ is each sandbox's working tree;
    <mount>/snapshots/<snapshot_id>/ holds frozen copies. We rsync into
    <mount>/.tmp/<snapshot_id>/ first and rename to the final path so a
    half-written snapshot never appears under /snapshots/.
    """

    mode = "efs"

    def __init__(self, mount: str = "/mnt/efs"):
        self.mount = mount

    def _session_dir(self, sandbox_id: str) -> str:
        return os.path.join(self.mount, "sessions", sandbox_id)

    def _snap_dir(self, snap_id: str) -> str:
        return os.path.join(self.mount, "snapshots", snap_id)

    def _tmp_dir(self, snap_id: str) -> str:
        return os.path.join(self.mount, ".tmp", snap_id)

    def snapshot(self, snap_id: str, name: str, sandbox_id: str = "") -> dict[str, Any]:
        alias = os.environ.get("BEDROCK_AGENTCORE_SESSION_ID", "")
        if not sandbox_id:
            return {"alias": alias, "name": name, "locator": "", "error": "efs snapshot: sandbox_id required"}
        src = self._session_dir(sandbox_id)
        if not os.path.isdir(src):
            # First-ever snapshot of a fresh sandbox — nothing on disk yet.
            os.makedirs(src, exist_ok=True)
        tmp = self._tmp_dir(snap_id)
        final = self._snap_dir(snap_id)
        os.makedirs(os.path.dirname(tmp), exist_ok=True)
        os.makedirs(os.path.dirname(final), exist_ok=True)
        # Flush dirty pages before snapshotting (best-effort; can't fsfreeze EFS).
        subprocess.run(["sync"], check=False)
        try:
            self._rsync(src + os.sep, tmp + os.sep)
        except subprocess.CalledProcessError as e:
            shutil.rmtree(tmp, ignore_errors=True)
            return {"alias": alias, "name": name, "locator": "", "error": f"efs snapshot: rsync failed: {e.stderr or e}"}
        # Atomic publish.
        if os.path.exists(final):
            shutil.rmtree(final, ignore_errors=True)
        os.rename(tmp, final)
        locator = json.dumps({"efs_path": final})
        return {"alias": alias, "name": name, "locator": locator}

    def resume(self, locator: str, alias: str, sandbox_id: str = "") -> dict[str, Any]:
        out = alias or os.environ.get("BEDROCK_AGENTCORE_SESSION_ID", "")
        if not locator:
            return {"alias": out, "error": "efs resume: empty locator"}
        if not sandbox_id:
            return {"alias": out, "error": "efs resume: sandbox_id required"}
        try:
            info = json.loads(locator)
        except (ValueError, json.JSONDecodeError) as e:
            return {"alias": out, "error": f"efs resume: bad locator: {e}"}
        src = info.get("efs_path")
        if not src:
            return {"alias": out, "error": "efs resume: locator missing efs_path"}
        if not os.path.isdir(src):
            return {"alias": out, "error": f"efs resume: snapshot path missing: {src}"}
        dst = self._session_dir(sandbox_id)
        os.makedirs(os.path.dirname(dst), exist_ok=True)
        try:
            self._rsync(src + os.sep, dst + os.sep)
        except subprocess.CalledProcessError as e:
            return {"alias": out, "error": f"efs resume: rsync failed: {e.stderr or e}"}
        return {"alias": out}

    def _rsync(self, src: str, dst: str) -> None:
        # -a archive, -H hardlinks, -A ACLs, -X xattrs, --delete makes dst
        # exactly match src (the snapshot is a frozen point-in-time copy).
        subprocess.run(
            ["rsync", "-aHAX", "--delete", "--numeric-ids", src, dst],
            check=True, capture_output=True, text=True,
        )


def make_snapshotter() -> Snapshotter:
    mode = os.environ.get("MICROVM_SNAPSHOT_MODE", "none") or "none"
    if mode == "none":
        return AliasSnapshotter()
    if mode == "s3":
        bucket = os.environ.get("MICROVM_SNAPSHOT_BUCKET", "")
        prefix = os.environ.get("MICROVM_SNAPSHOT_PREFIX", "microvm/")
        if not bucket:
            raise RuntimeError("MICROVM_SNAPSHOT_MODE=s3 requires MICROVM_SNAPSHOT_BUCKET")
        return S3Snapshotter(bucket=bucket, prefix=prefix)
    if mode == "efs":
        mount = os.environ.get("MICROVM_EFS_MOUNT_PATH", "/mnt/efs")
        return EfsSnapshotter(mount=mount)
    if mode == "tiered":
        raise NotImplementedError(
            "MICROVM_SNAPSHOT_MODE=tiered not yet implemented (PR3 — see docs/plans/2026-05-23-snapshot-modes-design.md)"
        )
    raise RuntimeError(f"unknown MICROVM_SNAPSHOT_MODE: {mode!r}")
