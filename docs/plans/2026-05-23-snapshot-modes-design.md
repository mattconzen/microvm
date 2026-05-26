# Snapshot modes — cross-PR design

Status: accepted (2026-05-23)
Tracks: #1 (EFS), #2 (S3 Files), #3 (Tiered)

## Problem

Today, `Snapshot` and `Resume` in the AWS backend only alias the AgentCore
`runtimeSessionId` ([`backend/aws/aws.go`](../../backend/aws/aws.go),
`Snapshot`/`Resume`). The shellagent's `handle_snapshot`/`handle_resume`
return the existing session id with no side effects
([`shellagent/app.py`](../../shellagent/app.py)). When AgentCore evicts
the session, all on-disk state is lost. `--from-snapshot` is wired into
the CLI ([`cli/sbx_create.go`](../../cli/sbx_create.go)) but the backend
ignores it.

We need real filesystem snapshots, with three different backends
([#1](https://github.com/mattconzen/microvm/issues/1),
[#2](https://github.com/mattconzen/microvm/issues/2),
[#3](https://github.com/mattconzen/microvm/issues/3)) and a CLI option
to pick between them.

## Decisions

### 1. Mode is a property of the AgentCore Runtime, not the sandbox

EFS mounts are declared via `filesystemConfigurations` on the runtime
([AWS docs](https://docs.aws.amazon.com/bedrock-agentcore/latest/devguide/runtime-filesystem-configurations.html)).
S3 Files mode needs different IAM and env wiring on the runtime.
Per-sandbox switching would force every runtime to support every mode,
which is a real cost (extra mounts, broader IAM, more env vars). One
runtime = one mode matches the underlying AWS shape.

Users who want to A/B modes register multiple runtimes (`microvm login
--snapshot-mode efs ...` and `microvm login --snapshot-mode s3 ...`)
and switch between them with the existing runtime-selection mechanism.

Mode is set by:

```
microvm login --snapshot-mode {none,s3,efs,tiered} [--snapshot-bucket NAME] [...]
```

`none` is the default and preserves today's alias-only behavior so
existing deployments are unaffected.

### 2. Dispatch happens on both Go and Python sides

**Go (`backend/aws`):** a `snapshotProvisioner` interface handles the
runtime-registration-time differences (filesystemConfigurations, IAM
policies, env vars on the shellagent container). The invocation path
(`Snapshot`/`Resume` RPCs into the shellagent) stays mode-agnostic.

```go
type snapshotProvisioner interface {
    Mode() string
    RuntimeOverrides(ctx context.Context, in RuntimeRegistration) (RuntimeRegistration, error)
    ValidateLoginOpts(opts backend.LoginOpts) error
}
```

**Python (`shellagent`):** a `Snapshotter` ABC selected at process
start from `MICROVM_SNAPSHOT_MODE`. `handle_snapshot`/`handle_resume`
delegate.

```python
class Snapshotter(ABC):
    mode: str
    def snapshot(self, snap_id: str, source: str) -> dict: ...   # returns Locator
    def resume(self, locator: dict, target: str) -> None: ...
```

### 3. Snapshot records carry mode + opaque locator

```go
type Snapshot struct {
    ID              string
    SandboxID       string
    Provider        string
    TargetSessionID string
    Kind            string  // unchanged; "alias" for legacy records
    Mode            string  // "none" | "s3" | "efs" | "tiered"
    Locator         string  // mode-decoded JSON blob (e.g. {"s3_uri":"..."})
    Name            string
    CreatedAt       time.Time
}
```

The `Locator` is opaque to everything except the matching `Snapshotter`
on the Python side and the matching provisioner on the Go side. This
keeps the seam tight: new backends add a new locator schema without
touching shared code.

Cross-mode resume (mode of snapshot ≠ mode of current runtime) returns
a clear error. No auto-migration. The user can resume against a
matching runtime instead.

### 4. Local state gains a Runtime record

`state.db` gets a `runtimes` bucket so the CLI can read "what mode is
the active runtime?" without round-tripping AWS on every command.

```go
type Runtime struct {
    Arn           string
    Region        string
    SnapshotMode  string
    SnapshotBucket string  // empty unless mode in {s3, tiered}
    ImageDigest   string
    UpdatedAt     time.Time
}
```

## Per-backend specs (summaries; full designs in linked issues)

### S3 Files (PR1, issue #2)

- Runtime gets IAM permission to `s3:GetObject`, `s3:PutObject`,
  `s3:DeleteObject` on `arn:aws:s3:::<bucket>/<prefix>/*` plus
  `s3:ListBucket` scoped to that prefix.
- `MICROVM_SNAPSHOT_MODE=s3`,
  `MICROVM_SNAPSHOT_BUCKET=<bucket>`,
  `MICROVM_SNAPSHOT_PREFIX=microvm/` injected into the shellagent.
- `snapshot`: tar-stream `/workspace` → `s3://bucket/microvm/<snap_id>.tar.zst`.
  Returns locator `{"s3_uri": "s3://..."}`.
- `resume`: download + unpack into `/workspace` (wiped first).

### EFS (PR2, issue #1)

- Runtime registration sets `filesystemConfigurations` to mount an EFS
  filesystem at `/efs`.
- Snapshots live at `/efs/snapshots/<snap_id>/`. Working tree at
  `/efs/sandbox/<sandbox_id>/`.
- `snapshot`: `cp -a /efs/sandbox/<id>/ /efs/snapshots/<snap_id>/`.
- `resume`: `cp -a /efs/snapshots/<snap_id>/ /efs/sandbox/<new_id>/`,
  then bind-mount or symlink `/workspace` at that path.
- Locator: `{"efs_path": "/efs/snapshots/<snap_id>"}`.

### Tiered (PR3, issue #3)

- Runtime gets both S3 IAM and AgentCore session storage.
- Active working tree lives on AgentCore session storage (fast SSD).
- `snapshot`: copy to a session-local snapshot dir, then async-upload
  to S3 for durability.
- `resume`: try session-local first; fall back to S3 download on miss
  (post-eviction).
- Locator: `{"session_path": "...", "s3_uri": "s3://..."}`.

## CLI surface

```
# Choose mode at runtime registration:
microvm login --snapshot-mode s3 --snapshot-bucket my-bucket
microvm login --snapshot-mode efs --efs-id fs-0123456789abcdef0
microvm login --snapshot-mode tiered --snapshot-bucket my-bucket

# Use normally — mode is invisible to per-sandbox operations:
microvm sbx create
microvm sbx snapshot <sbx_id> --name baseline
microvm sbx create --from-snapshot <snap_id>
microvm sbx resume <snap_id>
```

Errors surface mode mismatches at the CLI:

```
$ microvm sbx resume snp_abc12345
Error: snapshot mode "s3" does not match active runtime mode "efs".
Use a runtime registered with --snapshot-mode s3 to resume this snapshot.
```

## Sequencing

- **PR1** (#2 / S3 Files): all seam plumbing + S3 Files end-to-end.
  Other modes return "not implemented" with a useful message.
- **PR2** (#1 / EFS): EFS provisioner + snapshotter against the
  stable seam.
- **PR3** (#3 / Tiered): tiered provisioner + snapshotter; depends
  on PR1's S3 work.

## Out of scope (across all 3 PRs)

- Cross-mode auto-migration of snapshots.
- Snapshot GC / retention policies.
- In-memory process state (CRIU etc.).
- Multi-region replication.

## Open questions

None blocking. Snapshot retention/GC will come in a follow-up once we
have real users on at least one mode.
