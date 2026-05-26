# PR1 — Snapshot mode skeleton + S3 Files backend

> **For Claude:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task.

**Goal:** Land the cross-cutting `--snapshot-mode` flag, the Go + Python dispatch seams, and a working S3 Files backend end-to-end. EFS and Tiered land in PR2/PR3 against the same seams. Existing alias-only behavior (`mode=none`) is preserved unchanged.

**Architecture:** Mode is a property of the AgentCore runtime, set at `microvm login --snapshot-mode …`. Go side adds a `snapshotProvisioner` interface for runtime-registration deltas; Python side adds a `Snapshotter` ABC selected from `MICROVM_SNAPSHOT_MODE` env. Snapshot records gain `Mode` + opaque `Locator` fields.

**Tech stack:** Go 1.22 (cobra, bbolt, AWS SDK v2), Python 3.11 (boto3), testify, pytest.

**Reference docs:** [Design](2026-05-23-snapshot-modes-design.md), [Issue #2 — S3 Files](https://github.com/mattconzen/microvm/issues/2).

---

## Task 1: Extend the backend interface with Mode + Locator

**Files:**
- Modify: `backend/backend.go`

**Changes:**

Add `Mode` and `Locator` to `Snapshot`; add `Mode` to `Sandbox`; add `SnapshotMode` and `SnapshotBucket` to `LoginOpts`.

```go
type Sandbox struct {
    ...existing fields...
    Mode string  // "" | "none" | "s3" | "efs" | "tiered"
}

type Snapshot struct {
    ...existing fields...
    Mode    string
    Locator string  // mode-decoded JSON; opaque to shared code
}

type LoginOpts struct {
    ...existing fields...
    SnapshotMode   string  // "" | "none" | "s3" | "efs" | "tiered"
    SnapshotBucket string  // required when mode in {s3, tiered}
}
```

**Step 1 — Edit `backend/backend.go`** to add the three fields above.

**Step 2 — Run `go build ./...`** to confirm nothing breaks. New zero values are backward-compatible with existing callers.

**Step 3 — Commit:** `feat(backend): add Mode + Locator fields to Sandbox/Snapshot/LoginOpts`

---

## Task 2: Add Runtime record to state

**Files:**
- Modify: `state/store.go`
- Modify: `state/store_test.go`

**Changes:**

Add `Runtime` type, `bucketRuntimes`, `PutRuntime`/`GetRuntime`, and `Mode`/`Locator` on `Sandbox`/`Snapshot` records. Existing rows continue to decode (zero values).

```go
const bucketRuntimes = "runtimes"

type Runtime struct {
    Arn            string    `json:"arn"`
    Region         string    `json:"region"`
    SnapshotMode   string    `json:"snapshot_mode,omitempty"`
    SnapshotBucket string    `json:"snapshot_bucket,omitempty"`
    ImageDigest    string    `json:"image_digest,omitempty"`
    UpdatedAt      time.Time `json:"updated_at"`
}

// In Snapshot:
Mode    string `json:"mode,omitempty"`
Locator string `json:"locator,omitempty"`

// In Sandbox:
Mode string `json:"mode,omitempty"`
```

Also: a singleton-style `PutRuntime`/`GetActiveRuntime` — the runtime ARN is the key, but for convenience we also store `"active"` → ARN in a meta bucket. Simpler: just key by ARN, and pass the active ARN from `cfg.AWS.AgentRuntimeArn` at lookup time.

**Step 1 — Write failing test in `state/store_test.go`:**

```go
func TestPutAndGetRuntime(t *testing.T) {
    s := newTestStore(t)
    rt := state.Runtime{
        Arn:            "arn:aws:bedrock-agentcore:us-east-1:123:runtime/x",
        Region:         "us-east-1",
        SnapshotMode:   "s3",
        SnapshotBucket: "my-bucket",
        UpdatedAt:      time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
    }
    require.NoError(t, s.PutRuntime(rt))
    got, err := s.GetRuntime(rt.Arn)
    require.NoError(t, err)
    assert.Equal(t, rt, got)
}

func TestGetRuntimeNotFound(t *testing.T) {
    s := newTestStore(t)
    _, err := s.GetRuntime("arn:does-not-exist")
    assert.ErrorIs(t, err, state.ErrNotFound)
}
```

(Reuse the existing `newTestStore` helper if present; otherwise inline a `state.Open()` with `t.TempDir()` + `MICROVM_HOME` env.)

**Step 2 — Run test, expect FAIL** (`undefined: state.Runtime`).

**Step 3 — Implement `Runtime`, `PutRuntime`, `GetRuntime` in `state/store.go`**; add `bucketRuntimes` to the bucket-create loop; add `Mode` to `Sandbox` and `Mode`/`Locator` to `Snapshot`.

**Step 4 — Run `go test ./state/...`** — expect PASS.

**Step 5 — Commit:** `feat(state): add Runtime record and Mode/Locator on snapshots`

---

## Task 3: snapshotProvisioner interface + alias (no-op) impl

**Files:**
- Create: `backend/aws/snapshot.go`
- Create: `backend/aws/snapshot_test.go`

**Step 1 — Write failing test in `snapshot_test.go`:**

```go
func TestProvisionerForMode_Alias(t *testing.T) {
    p, err := awsbackend.ProvisionerFor("none", backend.LoginOpts{})
    require.NoError(t, err)
    assert.Equal(t, "none", p.Mode())
}

func TestProvisionerForMode_Unknown(t *testing.T) {
    _, err := awsbackend.ProvisionerFor("nonsense", backend.LoginOpts{})
    require.Error(t, err)
    assert.Contains(t, err.Error(), "unknown snapshot mode")
}

func TestProvisionerFor_DefaultsToNone(t *testing.T) {
    p, err := awsbackend.ProvisionerFor("", backend.LoginOpts{})
    require.NoError(t, err)
    assert.Equal(t, "none", p.Mode())
}
```

**Step 2 — Run test, expect FAIL**.

**Step 3 — Implement** in `snapshot.go`:

```go
package aws

import (
    "fmt"

    "github.com/mattconzen/microvm/backend"
)

type snapshotProvisioner interface {
    Mode() string
    ValidateLoginOpts(opts backend.LoginOpts) error
    EnvOverrides() map[string]string   // env vars to inject into shellagent container
}

func ProvisionerFor(mode string, opts backend.LoginOpts) (snapshotProvisioner, error) {
    if mode == "" {
        mode = "none"
    }
    switch mode {
    case "none":
        return aliasProvisioner{}, nil
    case "s3":
        return newS3Provisioner(opts)
    case "efs":
        return nil, fmt.Errorf("snapshot mode %q not implemented yet (PR2 — see docs/plans/2026-05-23-snapshot-modes-design.md)", mode)
    case "tiered":
        return nil, fmt.Errorf("snapshot mode %q not implemented yet (PR3 — see docs/plans/2026-05-23-snapshot-modes-design.md)", mode)
    default:
        return nil, fmt.Errorf("unknown snapshot mode %q (want one of: none, s3, efs, tiered)", mode)
    }
}

type aliasProvisioner struct{}

func (aliasProvisioner) Mode() string                                   { return "none" }
func (aliasProvisioner) ValidateLoginOpts(_ backend.LoginOpts) error    { return nil }
func (aliasProvisioner) EnvOverrides() map[string]string                { return nil }
```

**Step 4 — Run test** — expect PASS (note: s3 impl is missing; the s3 case will panic). To prevent that during this step, temporarily return `nil, fmt.Errorf("not yet impl")` for `case "s3"` — the next task replaces it. Actually skip that — just add a third test that asserts `s3` returns "not implemented yet" for now, then Task 4 will update it.

Actually simpler: Skip the s3 test in Task 3. Task 4 adds the s3 path and its tests together.

**Step 5 — Commit:** `feat(backend/aws): add snapshotProvisioner interface and alias mode`

---

## Task 4: S3 Files provisioner + envelope additions

**Files:**
- Create: `backend/aws/snapshot_s3.go`
- Modify: `backend/aws/snapshot.go` (wire s3 case)
- Modify: `backend/aws/snapshot_test.go`
- Modify: `backend/aws/envelope.go` (add `Mode`, `Locator` fields on requests/responses)

**Envelope additions** (additive; old shellagents ignore unknown fields):

```go
// In Request:
Mode    string `json:"mode,omitempty"`
Locator string `json:"locator,omitempty"`

// In SnapshotResponse:
Locator string `json:"locator,omitempty"`
Mode    string `json:"mode,omitempty"`

// In ResumeResponse: (no change needed — alias already covered)
```

**S3 provisioner** in `snapshot_s3.go`:

```go
package aws

import (
    "fmt"
    "strings"

    "github.com/mattconzen/microvm/backend"
)

type s3Provisioner struct {
    bucket string
    prefix string
}

func newS3Provisioner(opts backend.LoginOpts) (s3Provisioner, error) {
    bucket := strings.TrimSpace(opts.SnapshotBucket)
    if bucket == "" {
        return s3Provisioner{}, fmt.Errorf("snapshot mode \"s3\" requires --snapshot-bucket")
    }
    if strings.ContainsAny(bucket, " \t\n/:") {
        return s3Provisioner{}, fmt.Errorf("invalid bucket name %q", bucket)
    }
    return s3Provisioner{bucket: bucket, prefix: "microvm/"}, nil
}

func (s3Provisioner) Mode() string { return "s3" }

func (p s3Provisioner) ValidateLoginOpts(opts backend.LoginOpts) error {
    if strings.TrimSpace(opts.SnapshotBucket) == "" {
        return fmt.Errorf("snapshot mode \"s3\" requires --snapshot-bucket")
    }
    return nil
}

func (p s3Provisioner) EnvOverrides() map[string]string {
    return map[string]string{
        "MICROVM_SNAPSHOT_MODE":   "s3",
        "MICROVM_SNAPSHOT_BUCKET": p.bucket,
        "MICROVM_SNAPSHOT_PREFIX": p.prefix,
    }
}
```

**Tests in `snapshot_test.go`:**

```go
func TestS3Provisioner_RequiresBucket(t *testing.T) {
    _, err := awsbackend.ProvisionerFor("s3", backend.LoginOpts{})
    require.Error(t, err)
    assert.Contains(t, err.Error(), "--snapshot-bucket")
}

func TestS3Provisioner_RejectsInvalidBucket(t *testing.T) {
    _, err := awsbackend.ProvisionerFor("s3", backend.LoginOpts{SnapshotBucket: "bad name/with slash"})
    require.Error(t, err)
}

func TestS3Provisioner_EnvVars(t *testing.T) {
    p, err := awsbackend.ProvisionerFor("s3", backend.LoginOpts{SnapshotBucket: "my-bucket"})
    require.NoError(t, err)
    env := p.EnvOverrides()
    assert.Equal(t, "s3", env["MICROVM_SNAPSHOT_MODE"])
    assert.Equal(t, "my-bucket", env["MICROVM_SNAPSHOT_BUCKET"])
    assert.Equal(t, "microvm/", env["MICROVM_SNAPSHOT_PREFIX"])
}
```

**Steps:** failing test → implement → passing test → commit.

**Commit:** `feat(backend/aws): add S3 Files snapshot provisioner`

---

## Task 5: Wire Login to validate + persist mode

**Files:**
- Modify: `backend/aws/aws.go` (`Login`)
- Modify: `backend/aws/aws_test.go`
- Modify: `cli/login.go` (new flags)
- Modify: `state/store.go` (already done — just consume here)

**Login changes:**

```go
func (b *Backend) Login(ctx context.Context, opts backend.LoginOpts) (err error) {
    ...existing identity + arn resolution...

    prov, err := ProvisionerFor(opts.SnapshotMode, opts)
    if err != nil {
        return err
    }
    if err := prov.ValidateLoginOpts(opts); err != nil {
        return err
    }

    b.cfg.AWS.AgentRuntimeArn = arn
    b.cfg.AWS.SnapshotMode = prov.Mode()
    b.cfg.AWS.SnapshotBucket = opts.SnapshotBucket
    ...rest unchanged...

    obs.L(ctx).Info("login.saved",
        "agent_runtime_arn", arn,
        "snapshot_mode", b.cfg.AWS.SnapshotMode,
    )

    // Persist runtime record so per-sandbox commands can look up the mode
    // without reading config.
    if b.store != nil {
        _ = b.store.PutRuntime(state.Runtime{
            Arn:            arn,
            Region:         b.cfg.AWS.Region,
            SnapshotMode:   b.cfg.AWS.SnapshotMode,
            SnapshotBucket: b.cfg.AWS.SnapshotBucket,
            ImageDigest:    b.cfg.AWS.ECRImageDigest,
            UpdatedAt:      b.now(),
        })
    }
    return nil
}
```

Wait — `Backend` doesn't currently know about `*state.Store`. Decision: add a `Store` field and a `WithStore` setter (rather than a constructor change, to avoid touching every caller). The CLI wires it up in `main.go` / `cli/root.go`.

```go
type Backend struct {
    ...existing...
    store *state.Store
}

func (b *Backend) WithStore(s *state.Store) *Backend {
    b.store = s
    return b
}
```

**Config additions** (`config/config.go`):

```go
type AWSConfig struct {
    ...existing...
    SnapshotMode   string `yaml:"snapshot_mode,omitempty"`
    SnapshotBucket string `yaml:"snapshot_bucket,omitempty"`
}
```

**CLI flags** (`cli/login.go`):

```go
cmd.Flags().StringVar(&opts.SnapshotMode, "snapshot-mode", "", "snapshot backend: none|s3|efs|tiered (default: none)")
cmd.Flags().StringVar(&opts.SnapshotBucket, "snapshot-bucket", "", "S3 bucket for snapshot storage (required if --snapshot-mode is s3 or tiered)")
```

**Tests** added to `aws_test.go`:

```go
func TestLoginPersistsSnapshotMode(t *testing.T) {
    t.Setenv("MICROVM_HOME", t.TempDir())
    cfg := &config.Config{DefaultProvider: "aws"}
    b := awsbackend.New(cfg, &fakeInvoker{}, fakeControl{}, fakeIdentity{})
    err := b.Login(context.Background(), backend.LoginOpts{
        RuntimeArn:     "arn:aws:bedrock-agentcore:us-east-1:123:runtime/microvm-shell",
        SnapshotMode:   "s3",
        SnapshotBucket: "my-bucket",
    })
    require.NoError(t, err)
    assert.Equal(t, "s3", cfg.AWS.SnapshotMode)
    assert.Equal(t, "my-bucket", cfg.AWS.SnapshotBucket)
}

func TestLoginRejectsS3WithoutBucket(t *testing.T) {
    t.Setenv("MICROVM_HOME", t.TempDir())
    cfg := &config.Config{DefaultProvider: "aws"}
    b := awsbackend.New(cfg, &fakeInvoker{}, fakeControl{}, fakeIdentity{})
    err := b.Login(context.Background(), backend.LoginOpts{
        RuntimeArn:   "arn:aws:bedrock-agentcore:us-east-1:123:runtime/microvm-shell",
        SnapshotMode: "s3",
    })
    require.Error(t, err)
    assert.Contains(t, err.Error(), "--snapshot-bucket")
}
```

**Commit:** `feat: wire snapshot-mode through login (CLI + AWS backend + config)`

---

## Task 6: Pipe Mode through Snapshot/Resume on Go side

**Files:**
- Modify: `backend/aws/aws.go` (`Snapshot`, `Resume`)
- Modify: `cli/sbx_snapshot.go` (persist Mode + Locator)
- Modify: `cli/sbx_resume.go` (mode-mismatch check, pass Locator)
- Modify: `cli/sbx_create.go` (default new sandbox Mode from runtime)
- Modify: `backend/aws/aws_test.go`

**`Snapshot` reads mode from config, sends in envelope, stores returned Locator:**

```go
func (b *Backend) Snapshot(ctx context.Context, sb backend.Sandbox, name string) (snap backend.Snapshot, err error) {
    t := obs.Time(ctx, obs.MetricSnapshot, "provider:aws", "mode:"+b.cfg.AWS.SnapshotMode)
    defer t.Done(&err)

    mode := b.cfg.AWS.SnapshotMode
    if mode == "" {
        mode = "none"
    }
    req, err := SnapshotRequest(name, mode)
    if err != nil {
        return snap, err
    }
    body, err := b.invoke(ctx, sb, req)
    if err != nil {
        return snap, err
    }
    var resp SnapshotResponse
    if err := json.Unmarshal(body, &resp); err != nil {
        return snap, fmt.Errorf("decode snapshot response: %w (body: %q)", err, truncate(string(body), 512))
    }
    if resp.Error != "" {
        return snap, errors.New(resp.Error)
    }

    kind := "alias"
    if mode != "" && mode != "none" {
        kind = mode  // "s3" | "efs" | "tiered"
    }
    target := resp.Alias
    if target == "" {
        target = sb.SessionID
    }
    return backend.Snapshot{
        SandboxID:       sb.ID,
        Provider:        "aws",
        TargetSessionID: target,
        Kind:            kind,
        Mode:            mode,
        Locator:         resp.Locator,
        Name:            name,
        CreatedAt:       b.now(),
    }, nil
}
```

**`Resume`** rejects mode mismatch:

```go
func (b *Backend) Resume(ctx context.Context, snap backend.Snapshot, spec backend.SandboxSpec) (sb backend.Sandbox, err error) {
    t := obs.Time(ctx, obs.MetricResume, "provider:aws", "mode:"+snap.Mode)
    defer t.Done(&err)

    runtimeMode := b.cfg.AWS.SnapshotMode
    if runtimeMode == "" {
        runtimeMode = "none"
    }
    snapMode := snap.Mode
    if snapMode == "" {
        snapMode = "none"
    }
    if snapMode != runtimeMode {
        return sb, fmt.Errorf(
            "snapshot mode %q does not match active runtime mode %q. "+
                "Re-register with `microvm login --snapshot-mode %s ...` to resume this snapshot.",
            snapMode, runtimeMode, snapMode,
        )
    }

    req, err := ResumeRequest(snap.TargetSessionID, snap.Locator, snap.Mode)
    if err != nil {
        return sb, err
    }
    ...rest mostly unchanged but include Mode on the returned Sandbox...
}
```

**Update `envelope.go`** `SnapshotRequest`/`ResumeRequest` to include `Mode` and `Locator`:

```go
func SnapshotRequest(name, mode string) ([]byte, error) {
    return json.Marshal(Request{Op: OpSnapshot, Name: name, Mode: mode})
}

func ResumeRequest(alias, locator, mode string) ([]byte, error) {
    return json.Marshal(Request{Op: OpResume, Alias: alias, Locator: locator, Mode: mode})
}
```

Adjust call sites in `aws.go` accordingly.

**Update `cli/sbx_snapshot.go`** to persist `Mode` + `Locator`:

```go
rec := state.Snapshot{
    ID:              state.NewSnapshotID(),
    SandboxID:       sb.ID,
    Provider:        b.Name(),
    TargetSessionID: snap.TargetSessionID,
    Kind:            snap.Kind,
    Mode:            snap.Mode,
    Locator:         snap.Locator,
    Name:            snap.Name,
    CreatedAt:       snap.CreatedAt,
}
```

**Update `cli/sbx_resume.go`** to pass `Mode` + `Locator` into `backend.Snapshot{...}`:

```go
sb, err := b.Resume(ctx,
    backend.Snapshot{
        ID:              snap.ID,
        SandboxID:       snap.SandboxID,
        Provider:        snap.Provider,
        TargetSessionID: snap.TargetSessionID,
        Kind:            snap.Kind,
        Mode:            snap.Mode,
        Locator:         snap.Locator,
        Name:            snap.Name,
    },
    backend.SandboxSpec{Name: name},
)
```

**Tests:** update existing `TestSnapshotIsAlias` to assert `Mode == "none"`, add `TestSnapshotIncludesS3Mode`, `TestResumeRejectsModeMismatch`. Existing `TestResumeRejectsUnsupportedKind` needs adjustment because we now key behavior on mode, not kind — keep it as a `Kind == "checkpoint"` reject test only if `Kind` is still validated; otherwise delete it (the mode check replaces the kind check).

Decision: keep `Kind` as-is for backward compat (legacy records use `Kind="alias"`), but the active check is `Mode`. The `TestResumeRejectsUnsupportedKind` test can be removed since unsupported kinds are no longer rejected at the Go layer — they're just unknown to the shellagent, which returns an error. Replace with the mode-mismatch test.

**Commit:** `feat(backend/aws): persist snapshot mode + locator; reject cross-mode resume`

---

## Task 7: Python Snapshotter ABC + S3 implementation

**Files:**
- Create: `shellagent/snapshotter.py`
- Modify: `shellagent/app.py`
- Modify: `shellagent/test_app.py`
- Create: `shellagent/test_snapshotter.py`

**`snapshotter.py`:**

```python
"""Pluggable snapshot backends for the shell agent.

The strategy is chosen at process start from MICROVM_SNAPSHOT_MODE:
  none   -- alias-only (returns the AgentCore session id, no I/O)
  s3     -- tar+stream working tree to/from s3://<bucket>/<prefix>/<id>.tar.zst
  efs    -- TODO (PR2)
  tiered -- TODO (PR3)
"""

from __future__ import annotations

import json
import os
import subprocess
import tarfile
import tempfile
from abc import ABC, abstractmethod
from typing import Any

WORKSPACE = "/workspace"


class Snapshotter(ABC):
    mode: str

    @abstractmethod
    def snapshot(self, snap_id: str, name: str) -> dict[str, Any]:
        """Return {"alias": <session_id>, "locator": <json-string>, "name": <name>}."""

    @abstractmethod
    def resume(self, locator: str, alias: str) -> dict[str, Any]:
        """Materialise the snapshot under WORKSPACE; return {"alias": <session_id>}."""


class AliasSnapshotter(Snapshotter):
    """Legacy behavior: no filesystem state, just echo the session id."""

    mode = "none"

    def snapshot(self, snap_id: str, name: str) -> dict[str, Any]:
        alias = os.environ.get("BEDROCK_AGENTCORE_SESSION_ID", "")
        return {"alias": alias, "name": name, "locator": ""}

    def resume(self, locator: str, alias: str) -> dict[str, Any]:
        out = alias or os.environ.get("BEDROCK_AGENTCORE_SESSION_ID", "")
        return {"alias": out}


class S3Snapshotter(Snapshotter):
    """tar+gzip /workspace to s3://<bucket>/<prefix>/<snap_id>.tar.gz."""

    mode = "s3"

    def __init__(self, bucket: str, prefix: str, *, workspace: str = WORKSPACE):
        self.bucket = bucket
        self.prefix = prefix.rstrip("/") + "/" if prefix else ""
        self.workspace = workspace

    def _key(self, snap_id: str) -> str:
        return f"{self.prefix}{snap_id}.tar.gz"

    def _uri(self, snap_id: str) -> str:
        return f"s3://{self.bucket}/{self._key(snap_id)}"

    def snapshot(self, snap_id: str, name: str) -> dict[str, Any]:
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

    def resume(self, locator: str, alias: str) -> dict[str, Any]:
        out = alias or os.environ.get("BEDROCK_AGENTCORE_SESSION_ID", "")
        if not locator:
            return {"alias": out, "error": "s3 resume: empty locator"}
        info = json.loads(locator)
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
                subprocess.run(["rm", "-rf", entry.path], check=True)
            else:
                try:
                    os.unlink(entry.path)
                except OSError:
                    pass

    def _aws_cp_up(self, local: str, uri: str) -> None:
        subprocess.run(["aws", "s3", "cp", local, uri], check=True)

    def _aws_cp_down(self, uri: str, local: str) -> None:
        subprocess.run(["aws", "s3", "cp", uri, local], check=True)


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
        raise NotImplementedError("MICROVM_SNAPSHOT_MODE=efs not yet implemented (PR2)")
    if mode == "tiered":
        raise NotImplementedError("MICROVM_SNAPSHOT_MODE=tiered not yet implemented (PR3)")
    raise RuntimeError(f"unknown MICROVM_SNAPSHOT_MODE: {mode!r}")
```

**`app.py` changes** — replace `handle_snapshot`/`handle_resume`:

```python
from snapshotter import make_snapshotter, Snapshotter

_snapshotter: Snapshotter | None = None


def _get_snapshotter() -> Snapshotter:
    global _snapshotter
    if _snapshotter is None:
        _snapshotter = make_snapshotter()
    return _snapshotter


def handle_snapshot(req: dict) -> dict:
    snap_id = req.get("snap_id") or req.get("name", "") or "snap"
    try:
        return _get_snapshotter().snapshot(snap_id, req.get("name", ""))
    except Exception as e:  # noqa: BLE001
        return {"alias": "", "name": req.get("name", ""), "locator": "", "error": str(e)}


def handle_resume(req: dict) -> dict:
    try:
        return _get_snapshotter().resume(req.get("locator", ""), req.get("alias", ""))
    except Exception as e:  # noqa: BLE001
        return {"alias": "", "error": str(e)}
```

Wait — the Go side currently passes `name` but not `snap_id`. The shellagent only needs an ID for S3 key generation. Use the Go-minted snapshot id. Update the envelope: send `snap_id` from Go.

Actually — looking again, `Request.Name` is what we currently pass. The Go side mints the snapshot ID *after* the Snapshot call returns. We need to flip that: mint the ID in the CLI *before* calling the backend, pass it down, and the shellagent uses it.

Refactor: add `SandboxSpec`-like `SnapshotSpec{ID, Name}` to `backend.Backend.Snapshot`, or just add an `ID` parameter. Cleanest: change `Snapshot(ctx, sb, name)` to `Snapshot(ctx, sb, spec SnapshotSpec)`. That's a bigger interface change but worth it for the seam.

**Decision:** add `SnapshotSpec{ID, Name}` and change `Snapshot(ctx, sb, SnapshotSpec)`. Update `fake_backend_test.go` accordingly.

```go
// backend/backend.go
type SnapshotSpec struct {
    ID   string
    Name string
}

// Backend interface
Snapshot(ctx context.Context, sb Sandbox, spec SnapshotSpec) (Snapshot, error)
```

CLI mints `state.NewSnapshotID()` *before* calling the backend, and passes it in. The backend includes the ID in the envelope.

Update `envelope.go`:

```go
func SnapshotRequest(spec SnapshotSpec, mode string) ([]byte, error) {
    return json.Marshal(Request{
        Op:     OpSnapshot,
        SnapID: spec.ID,
        Name:   spec.Name,
        Mode:   mode,
    })
}
```

Add `SnapID string \`json:"snap_id,omitempty"\`` to `Request`.

**Tests** in `test_snapshotter.py`:

```python
import json
import os
import subprocess
import tarfile

import pytest

import snapshotter as sn


def test_alias_snapshotter_returns_session(monkeypatch):
    monkeypatch.setenv("BEDROCK_AGENTCORE_SESSION_ID", "sess-xyz")
    s = sn.AliasSnapshotter()
    out = s.snapshot("snp_1", "demo")
    assert out["alias"] == "sess-xyz"
    assert out["name"] == "demo"
    assert out["locator"] == ""


def test_s3_snapshotter_tars_and_uploads(monkeypatch, tmp_path):
    workspace = tmp_path / "workspace"
    workspace.mkdir()
    (workspace / "hello.txt").write_text("hi from sandbox")

    uploads: list[tuple[str, str]] = []
    downloads: list[tuple[str, str]] = []

    class FakeS3(sn.S3Snapshotter):
        def _aws_cp_up(self, local, uri):
            uploads.append((local, uri))
            # Simulate upload by copying the file into a fake "remote" location.
            import shutil
            shutil.copy(local, str(tmp_path / "remote.tar.gz"))

        def _aws_cp_down(self, uri, local):
            downloads.append((uri, local))
            import shutil
            shutil.copy(str(tmp_path / "remote.tar.gz"), local)

    s = FakeS3(bucket="my-bucket", prefix="microvm/", workspace=str(workspace))
    monkeypatch.setenv("BEDROCK_AGENTCORE_SESSION_ID", "sess-1")
    out = s.snapshot("snp_abc", "baseline")

    assert out["alias"] == "sess-1"
    locator = json.loads(out["locator"])
    assert locator["s3_uri"] == "s3://my-bucket/microvm/snp_abc.tar.gz"
    assert len(uploads) == 1

    # Mutate workspace, then resume — should restore the original contents.
    (workspace / "hello.txt").write_text("DIRTY")
    (workspace / "junk.txt").write_text("delete me")

    out2 = s.resume(out["locator"], "sess-1")
    assert out2["alias"] == "sess-1"
    assert (workspace / "hello.txt").read_text() == "hi from sandbox"
    assert not (workspace / "junk.txt").exists()
    assert len(downloads) == 1


def test_make_snapshotter_default_alias(monkeypatch):
    monkeypatch.delenv("MICROVM_SNAPSHOT_MODE", raising=False)
    assert isinstance(sn.make_snapshotter(), sn.AliasSnapshotter)


def test_make_snapshotter_s3_requires_bucket(monkeypatch):
    monkeypatch.setenv("MICROVM_SNAPSHOT_MODE", "s3")
    monkeypatch.delenv("MICROVM_SNAPSHOT_BUCKET", raising=False)
    with pytest.raises(RuntimeError, match="MICROVM_SNAPSHOT_BUCKET"):
        sn.make_snapshotter()


def test_make_snapshotter_efs_not_implemented(monkeypatch):
    monkeypatch.setenv("MICROVM_SNAPSHOT_MODE", "efs")
    with pytest.raises(NotImplementedError):
        sn.make_snapshotter()
```

Update `test_app.py` to reset the cached snapshotter between tests:

```python
@pytest.fixture(autouse=True)
def reset_snapshotter():
    app._snapshotter = None
    yield
    app._snapshotter = None
```

Existing snapshot tests still pass (default mode = none = AliasSnapshotter, same observable behavior).

**Commit:** `feat(shellagent): pluggable Snapshotter ABC with S3 backend`

---

## Task 8: End-to-end CLI test using fake backend

**Files:**
- Modify: `cli/fake_backend_test.go` (record snapshot calls, support locator round-trip)
- Modify: `cli/cli_e2e_test.go` (new test asserting snapshot+resume preserves Mode/Locator)

**fake_backend_test.go** — track snapshots:

```go
type snapshotCall struct {
    SessionID string
    Spec      backend.SnapshotSpec
    Mode      string // backend's idea of mode (set via WithMode helper)
}

type fakeBackend struct {
    ...existing...
    snapshotMode string
    snapshots    []snapshotCall
}

func (f *fakeBackend) Snapshot(_ context.Context, sb backend.Sandbox, spec backend.SnapshotSpec) (backend.Snapshot, error) {
    f.mu.Lock()
    f.snapshots = append(f.snapshots, snapshotCall{SessionID: sb.SessionID, Spec: spec, Mode: f.snapshotMode})
    f.mu.Unlock()
    locator := ""
    kind := "alias"
    if f.snapshotMode == "s3" {
        locator = fmt.Sprintf(`{"s3_uri":"s3://fake/%s.tar.gz"}`, spec.ID)
        kind = "s3"
    }
    return backend.Snapshot{
        SandboxID:       sb.ID,
        Provider:        f.name,
        TargetSessionID: sb.SessionID,
        Kind:            kind,
        Mode:            f.snapshotMode,
        Locator:         locator,
        Name:            spec.Name,
        CreatedAt:       f.now(),
    }, nil
}
```

**E2E test:**

```go
func TestSnapshotPersistsModeAndLocator(t *testing.T) {
    app, fake := newTestApp(t)
    fake.snapshotMode = "s3"

    // create + snapshot
    out := runCmd(t, app, "sbx", "create")
    sbxID := parseSandboxID(t, out)
    snapOut := runCmd(t, app, "sbx", "snapshot", sbxID, "--name", "demo")
    snapID := parseSnapshotID(t, snapOut)

    // verify state.db has Mode + Locator
    snap, err := app.Store.GetSnapshot(snapID)
    require.NoError(t, err)
    assert.Equal(t, "s3", snap.Mode)
    assert.Contains(t, snap.Locator, "s3://fake/")
}

func TestResumeRejectsModeMismatch(t *testing.T) {
    app, fake := newTestApp(t)
    fake.snapshotMode = "s3"
    out := runCmd(t, app, "sbx", "create")
    sbxID := parseSandboxID(t, out)
    snapOut := runCmd(t, app, "sbx", "snapshot", sbxID)
    snapID := parseSnapshotID(t, snapOut)

    // Flip runtime mode so resume sees a mismatch.
    fake.snapshotMode = "none"
    _, err := tryRunCmd(t, app, "sbx", "resume", snapID)
    require.Error(t, err)
    assert.Contains(t, err.Error(), "does not match active runtime mode")
}
```

The fake backend's `Resume` needs to do the mismatch check too (since the real check lives in `backend/aws/aws.go`). Add:

```go
func (f *fakeBackend) Resume(_ context.Context, snap backend.Snapshot, spec backend.SandboxSpec) (backend.Sandbox, error) {
    if snap.Mode != "" && snap.Mode != f.snapshotMode {
        return backend.Sandbox{}, fmt.Errorf("snapshot mode %q does not match active runtime mode %q", snap.Mode, f.snapshotMode)
    }
    ...rest existing...
}
```

**Commit:** `test(cli): e2e tests for snapshot mode persistence and resume mismatch`

---

## Task 9: Update README + add mode docs

**Files:**
- Modify: `README.md` — add Snapshot Modes section
- (Design doc already exists.)

**Step 1 — Add to README.md** near the existing feature list:

```markdown
### Snapshot modes

Pick a snapshot backend at runtime registration:

    microvm login --snapshot-mode s3 --snapshot-bucket my-bucket
    microvm login --snapshot-mode efs --efs-id fs-0123...    # PR2 (in progress)
    microvm login --snapshot-mode tiered --snapshot-bucket b # PR3 (in progress)

Modes:

- `none` (default) — session aliases only. Compatible with existing
  deployments; matches today's behavior.
- `s3` — tar + upload working tree to S3 on snapshot, download + restore
  on resume. Durable across runtime evictions. Requires `--snapshot-bucket`.
- `efs` — EFS-backed snapshots via `cp -a` on the runtime's EFS mount. (PR2)
- `tiered` — fast session-local tier + async S3 durability. (PR3)

See [docs/plans/2026-05-23-snapshot-modes-design.md](docs/plans/2026-05-23-snapshot-modes-design.md)
for the architecture and rationale.
```

**Commit:** `docs: snapshot modes section in README`

---

## Task 10: Run full test suite + tidy

**Step 1:** `go test ./...` — expect all PASS.
**Step 2:** `cd shellagent && pytest` — expect all PASS.
**Step 3:** `go vet ./...` — expect clean.
**Step 4:** `git push -u origin feature/snapshot-modes-skeleton-s3`.
**Step 5:** Open PR with title `feat(snapshots): mode dispatch seams + S3 Files backend`.

PR body template:

```markdown
## Summary

Implements PR1 of the snapshot-modes plan
([design](docs/plans/2026-05-23-snapshot-modes-design.md)):

- `--snapshot-mode` flag on `microvm login` (`none`|`s3`|`efs`|`tiered`)
- Go `snapshotProvisioner` interface + S3 + alias impls
- Python `Snapshotter` ABC + S3 + alias impls
- New `Runtime` record in state.db
- `Mode` + `Locator` fields on Sandbox/Snapshot records
- Mode-mismatch error on cross-mode resume
- README + design + plan docs

EFS (#1) and Tiered (#3) follow in PR2 and PR3 against the same seams.
Default mode (`none`) preserves existing alias-only behavior unchanged.

Closes #2 (S3 Files design — implementation).

## Test plan

- [x] `go test ./...`
- [x] `pytest shellagent/`
- [ ] Live AWS smoke test: register runtime with `--snapshot-mode s3`,
      snapshot + resume a workspace with a known file, verify restore.
```

---

## Done criteria

- All listed tests pass on both Go and Python sides.
- `microvm login --snapshot-mode s3 --snapshot-bucket X` persists mode in config + state.
- `microvm sbx snapshot` records Mode + Locator.
- `microvm sbx resume` errors clearly when modes mismatch.
- Default (`mode=none`) behavior is byte-for-byte unchanged.
- README has a snapshot modes section.
- PR opened with design + plan docs included.
