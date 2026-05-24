# PR4 — Sandbox lifecycle CLI verbs (fork, revert, from-snapshot)

> **For Claude:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task.

**Goal:** Land the user-visible CLI verbs that the snapshot-modes design promised but PR1–PR3 didn't ship: `microvm sbx fork`, `microvm sbx revert`, and the long-stubbed `microvm sbx create --from-snapshot` flag. Also tighten tiered mode's resume to mirror-exact semantics (a follow-up flagged in PR3).

**Architecture:** All three new verbs are CLI compositions over the existing `Snapshot`/`Resume` backend methods — no new shellagent ops, no new envelope ops, no new Backend interface methods. `fork` is `snapshot + resume`. `revert` is `resume` targeting the existing sandbox row (overwrites in-place; mode dispatch already knows how to wipe-and-restore destination). `--from-snapshot` on create dispatches to `resume` with a freshly-minted sandbox id. Tiered mode's resume is upgraded to use `aws s3 sync --delete` so revert and `--from-snapshot` produce a clean replica rather than a merge.

**Tech stack:** Go 1.22 (cobra, AWS SDK v2), Python 3.12, testify, pytest. No new dependencies.

**Reference docs:**
- [Cross-PR design](2026-05-23-snapshot-modes-design.md) — fork/revert/from-snapshot are listed as the user-visible surface
- [PR1 plan](2026-05-23-pr1-skeleton-s3.md), [PR2 plan](2026-05-24-pr2-efs.md), [PR3 plan](2026-05-24-pr3-tiered.md) — these established the seams this PR exercises
- Issue [#1](https://github.com/mattconzen/microvm/issues/1) §6 (EFS CLI surface), [#3](https://github.com/mattconzen/microvm/issues/3) "CLI surface changes" — both name fork/revert/from-snapshot

**Scope discipline.** This PR ships only the three CLI verbs and the tiered-resume tightening they depend on. **Out of scope** (still future work):
- Auto-snapshot (`--auto-snapshot 30m` flag and CLI daemon)
- Snapshot retention / GC (`microvm sbx snapshots prune --older-than 30d`)
- CLI-side snapshot routing for tiered (defense-in-depth IAM hardening)
- Interactive-shell init protocol (cwd for `microvm sbx shell` in tiered mode)
- `--include-cache` fork option (cache forking)

---

## Task 1: TieredSnapshotter.resume uses --delete semantics

**Why:** PR3's reviewer flagged that `aws s3 cp --recursive` is merge, not mirror. Today this is harmless because the destination prefix is always fresh-empty. After Task 3 (revert) and Task 4 (from-snapshot), the destination may contain pre-existing data — for revert it definitely does. Resume needs to be mirror-exact across all modes so the new verbs have consistent semantics.

S3/EFS already mirror-exact (`tar` rewrites whole tree; `rsync --delete`). Only tiered diverges. Fix it.

**Files:**
- Modify: `shellagent/snapshotter.py` (`TieredSnapshotter.resume` and add a `_aws_sync_delete` helper)
- Modify: `shellagent/test_snapshotter.py`

**Step 1 — Write failing test** in `test_snapshotter.py`:

```python
def test_tiered_resume_uses_sync_delete(monkeypatch):
    calls = []

    class FakeTiered(sn.TieredSnapshotter):
        def _aws_sync_delete(self, src, dst):
            calls.append(("sync", src, dst))

        def _aws_cp_recursive(self, src, dst):
            calls.append(("cp", src, dst))

    monkeypatch.setenv("BEDROCK_AGENTCORE_SESSION_ID", "sess-1")
    s = FakeTiered(bucket="microvm-fs", mount="/workspace")
    locator = json.dumps({
        "workspace_prefix": "s3://microvm-fs/sessions/mvm_old/",
        "snapshot_prefix":  "s3://microvm-fs/snapshots/snp_x/",
    })
    out = s.resume(locator, "sess-1", sandbox_id="mvm_new")
    assert out["alias"] == "sess-1"
    # Snapshot path stays as cp (server-side copy from empty -> snapshot prefix);
    # resume path uses sync --delete to mirror exactly.
    assert calls == [("sync", "s3://microvm-fs/snapshots/snp_x/", "s3://microvm-fs/sessions/mvm_new/")]


def test_tiered_snapshot_still_uses_cp(monkeypatch):
    calls = []

    class FakeTiered(sn.TieredSnapshotter):
        def _aws_sync_delete(self, src, dst):
            calls.append(("sync", src, dst))

        def _aws_cp_recursive(self, src, dst):
            calls.append(("cp", src, dst))

    monkeypatch.setenv("BEDROCK_AGENTCORE_SESSION_ID", "sess-1")
    s = FakeTiered(bucket="microvm-fs", mount="/workspace")
    out = s.snapshot("snp_x", "n", sandbox_id="mvm_a")
    # Snapshot creates a fresh prefix; cp is correct (no destination to mirror).
    assert calls == [("cp", "s3://microvm-fs/sessions/mvm_a/", "s3://microvm-fs/snapshots/snp_x/")]
```

**Step 2 — Run** `python3 -m pytest shellagent/test_snapshotter.py -v -k tiered` — expect FAIL (no `_aws_sync_delete` method yet, and resume currently calls `_aws_cp_recursive`).

**Step 3 — Implement** the change in `snapshotter.py`. In `TieredSnapshotter.resume`, replace the `self._aws_cp_recursive(snap_uri, dst)` call with `self._aws_sync_delete(snap_uri, dst)`. Add the new wrapper method below `_aws_cp_recursive`:

```python
def _aws_sync_delete(self, src: str, dst: str) -> None:
    # `aws s3 sync --delete` produces a mirror-exact copy of `src` at `dst`
    # — stale objects under `dst` not present in `src` are deleted.
    # Used for resume so a revert into an existing sandbox produces a clean
    # replica instead of a merge.
    subprocess.run(
        ["aws", "s3", "sync", "--delete", src, dst],
        check=True, capture_output=True, text=True,
    )
```

Don't change `_aws_cp_recursive` — it's still correct for the snapshot path (writing into a fresh empty prefix; `cp --recursive` is faster than `sync --delete` because it skips the source list/diff).

The error-path code in `resume` (the `subprocess.CalledProcessError` handler) catches the new call by accident since both wrappers raise the same exception type. Update the error message to say "aws s3 sync" instead of "aws s3 cp" in the resume branch:

```python
except subprocess.CalledProcessError as e:
    return {"alias": out, "error": f"tiered resume: aws s3 sync failed: {e.stderr or e}"}
```

**Step 4 — Run** `pytest shellagent/test_snapshotter.py -v -k tiered` — expect ALL PASS. Then run the full `pytest shellagent/` — confirm 65 passes (63 + 2 new).

**Step 5 — Commit:** `fix(shellagent): tiered resume uses aws s3 sync --delete for mirror-exact restore`

---

## Task 2: microvm sbx fork

**Why:** `fork` is a CLI shorthand for "snapshot this sandbox, then immediately create a new one from the snapshot." Issue #3 explicitly names it as the high-level UX for branching.

**Architecture:** Pure CLI composition — calls `b.Snapshot(...)` then `b.Resume(...)`. No new backend method. The intermediate snapshot is persisted to state (same as `microvm sbx snapshot <id>`), so the user can see it in `microvm sbx snapshots`.

**Files:**
- Create: `cli/sbx_fork.go`
- Modify: `cli/sbx.go` (register subcommand)
- Modify: `cli/cli_e2e_test.go` (e2e test)

**Step 1 — Create `cli/sbx_fork.go`:**

```go
package cli

import (
    "context"
    "fmt"
    "time"

    "github.com/spf13/cobra"

    "github.com/mattconzen/microvm/backend"
    "github.com/mattconzen/microvm/state"
)

func newForkCmd(ctx context.Context, app *App, g *GlobalFlags) *cobra.Command {
    var (
        name     string
        snapName string
    )
    cmd := &cobra.Command{
        Use:   "fork <id>",
        Short: "Snapshot a sandbox and immediately resume into a new one",
        Long: `Fork is shorthand for sbx snapshot + sbx resume. Use it to branch a
sandbox: the source sandbox keeps running unchanged, and a new sandbox is
created whose workspace starts as a copy of the source at the snapshot point.

The intermediate snapshot is persisted so you can see it with sbx snapshots
or use it to re-fork later.`,
        Args: cobra.ExactArgs(1),
        RunE: func(cmd *cobra.Command, args []string) error {
            b, sb, err := resolveBackendForID(app, args[0])
            if err != nil {
                return err
            }
            sbApi := backend.Sandbox{ID: sb.ID, Provider: sb.Provider, SessionID: sb.SessionID, Mode: sb.Mode}

            // 1) Snapshot the source.
            snapID := state.NewSnapshotID()
            snap, err := b.Snapshot(ctx, sbApi, backend.SnapshotSpec{ID: snapID, Name: snapName})
            if err != nil {
                return fmt.Errorf("fork: snapshot source: %w", err)
            }
            snapRec := state.Snapshot{
                ID:              snapID,
                SandboxID:       sb.ID,
                Provider:        b.Name(),
                TargetSessionID: snap.TargetSessionID,
                Kind:            snap.Kind,
                Mode:            snap.Mode,
                Locator:         snap.Locator,
                Name:            snap.Name,
                CreatedAt:       snap.CreatedAt,
            }
            if err := app.Store.PutSnapshot(snapRec); err != nil {
                return fmt.Errorf("fork: persist snapshot: %w", err)
            }

            // 2) Resume into a fresh sandbox.
            newID := state.NewSandboxID()
            forkedSb, err := b.Resume(ctx,
                backend.Snapshot{
                    ID:              snapRec.ID,
                    SandboxID:       snapRec.SandboxID,
                    Provider:        snapRec.Provider,
                    TargetSessionID: snapRec.TargetSessionID,
                    Kind:            snapRec.Kind,
                    Mode:            snapRec.Mode,
                    Locator:         snapRec.Locator,
                    Name:            snapRec.Name,
                },
                backend.SandboxSpec{Name: name, ID: newID},
            )
            if err != nil {
                return fmt.Errorf("fork: resume into new sandbox: %w", err)
            }

            rec := state.Sandbox{
                ID:        newID,
                Provider:  b.Name(),
                SessionID: forkedSb.SessionID,
                Name:      name,
                Mode:      forkedSb.Mode,
                CreatedAt: time.Now(),
                LastUsed:  time.Now(),
                Labels:    map[string]string{"forked_from": sb.ID, "via_snapshot": snapRec.ID},
            }
            if err := app.Store.PutSandbox(rec); err != nil {
                return err
            }
            return writeSandbox(cmd, g, rec)
        },
    }
    cmd.Flags().StringVar(&name, "name", "", "name for the new (forked) sandbox")
    cmd.Flags().StringVar(&snapName, "snapshot-name", "", "name for the intermediate snapshot (default: auto)")
    return cmd
}
```

**Step 2 — Register in `cli/sbx.go`** alongside the other sbx subcommands:

```go
cmd.AddCommand(newForkCmd(ctx, app, g))
```

**Step 3 — Add an e2e test** in `cli/cli_e2e_test.go`:

```go
func TestE2EFork(t *testing.T) {
    env := newTestEnv(t)
    env.fake.snapshotMode = "s3" // any non-none mode exercises the locator round-trip

    src := createSandbox(t, env.app, "src-sbx")
    forkOut := runCLIJSON(t, env.app, "sbx", "fork", src.ID, "--name", "forked")
    var forked state.Sandbox
    require.NoError(t, json.Unmarshal([]byte(forkOut), &forked))
    assert.NotEqual(t, src.ID, forked.ID, "fork mints a new sandbox id")
    assert.Equal(t, "forked", forked.Name)
    assert.Equal(t, src.ID, forked.Labels["forked_from"])
    assert.NotEmpty(t, forked.Labels["via_snapshot"])

    // The intermediate snapshot is visible in state.
    snap, err := env.store.GetSnapshot(forked.Labels["via_snapshot"])
    require.NoError(t, err)
    assert.Equal(t, src.ID, snap.SandboxID)
    assert.Equal(t, "s3", snap.Mode)
}
```

**Step 4 — Run** `go test ./cli/... -run TestE2EFork -v` — expect PASS.

**Step 5 — Commit:** `feat(cli): microvm sbx fork — snapshot + resume in one verb`

---

## Task 3: microvm sbx revert

**Why:** Revert restores a sandbox's workspace in place from a prior snapshot. Issue #3 §"CLI surface changes" defines it; useful for "I want a do-over." Distinguished from resume (which mints a new sandbox) by overwriting the existing state row.

**Architecture:** Calls `b.Resume(ctx, snap, SandboxSpec{ID: existingSandbox.ID, Name: existingSandbox.Name})` — same backend method as resume, but the SandboxSpec carries the EXISTING sandbox's ID. The shellagent's resume handler already does mirror-exact restore (Task 1 made this true for tiered too) into `sessions/<id>/`, so passing the existing id naturally overwrites in place. The session id stays the same (sticky session continues), only the on-disk workspace changes.

**Files:**
- Create: `cli/sbx_revert.go`
- Modify: `cli/sbx.go` (register)
- Modify: `cli/cli_e2e_test.go`

**Step 1 — Create `cli/sbx_revert.go`:**

```go
package cli

import (
    "context"
    "fmt"
    "time"

    "github.com/spf13/cobra"

    "github.com/mattconzen/microvm/backend"
    "github.com/mattconzen/microvm/state"
)

func newRevertCmd(ctx context.Context, app *App, g *GlobalFlags) *cobra.Command {
    var snapID string
    cmd := &cobra.Command{
        Use:   "revert <id> --snapshot <snap-id>",
        Short: "Restore a sandbox's workspace from a prior snapshot, in place",
        Long: `Revert overwrites the sandbox's workspace with the contents of the
named snapshot. The sandbox's id, session id, name, and cache tier (in
tiered mode) are unchanged; only the snapshottable workspace tier is
restored.

This is destructive: anything written to the workspace since the snapshot
is lost. Use sbx fork instead if you want to keep the current state too.`,
        Args: cobra.ExactArgs(1),
        RunE: func(cmd *cobra.Command, args []string) error {
            if snapID == "" {
                return fmt.Errorf("--snapshot is required")
            }
            b, sb, err := resolveBackendForID(app, args[0])
            if err != nil {
                return err
            }
            snap, err := app.Store.GetSnapshot(snapID)
            if err != nil {
                return fmt.Errorf("load snapshot: %w", err)
            }
            // Defensive: don't revert sandbox A to a snapshot of sandbox B.
            // (The mode-mismatch check in the backend covers the harder case
            // of cross-runtime snapshots; this is just a per-sandbox sanity
            // check.)
            if snap.SandboxID != sb.ID {
                return fmt.Errorf(
                    "snapshot %s is of sandbox %s, not %s; refusing cross-sandbox revert",
                    snap.ID, snap.SandboxID, sb.ID,
                )
            }

            // Resume targeting the existing sandbox id. The shellagent's
            // mirror-exact restore overwrites sessions/<id>/ in place; same
            // session id keeps the runtime sticky.
            _, err = b.Resume(ctx,
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
                backend.SandboxSpec{Name: sb.Name, ID: sb.ID},
            )
            if err != nil {
                return fmt.Errorf("revert: %w", err)
            }

            // Stamp last-used and a label so the revert is auditable.
            sb.LastUsed = time.Now()
            if sb.Labels == nil {
                sb.Labels = map[string]string{}
            }
            sb.Labels["reverted_to"] = snap.ID
            if err := app.Store.PutSandbox(sb); err != nil {
                return err
            }
            fmt.Fprintf(cmd.OutOrStdout(), "reverted %s to %s\n", sb.ID, snap.ID)
            return nil
        },
    }
    cmd.Flags().StringVar(&snapID, "snapshot", "", "snapshot id to restore from (required)")
    _ = cmd.MarkFlagRequired("snapshot")
    return cmd
}
```

**Step 2 — Register in `cli/sbx.go`.**

**Step 3 — Add e2e tests:**

```go
func TestE2ERevert(t *testing.T) {
    env := newTestEnv(t)
    env.fake.snapshotMode = "efs"

    sb := createSandbox(t, env.app, "revertme")
    snapOut := runCLIJSON(t, env.app, "sbx", "snapshot", sb.ID, "--name", "before")
    var snap state.Snapshot
    require.NoError(t, json.Unmarshal([]byte(snapOut), &snap))

    stdout, _, err := runCLI(t, env.app, "sbx", "revert", sb.ID, "--snapshot", snap.ID)
    require.NoError(t, err)
    assert.Contains(t, stdout, "reverted "+sb.ID+" to "+snap.ID)

    after, err := env.store.GetSandbox(sb.ID)
    require.NoError(t, err)
    assert.Equal(t, sb.SessionID, after.SessionID, "revert preserves session id")
    assert.Equal(t, snap.ID, after.Labels["reverted_to"])
}

func TestE2ERevertRejectsCrossSandbox(t *testing.T) {
    env := newTestEnv(t)
    env.fake.snapshotMode = "efs"

    a := createSandbox(t, env.app, "a")
    b := createSandbox(t, env.app, "b")
    snapOut := runCLIJSON(t, env.app, "sbx", "snapshot", a.ID)
    var snapA state.Snapshot
    require.NoError(t, json.Unmarshal([]byte(snapOut), &snapA))

    // Try to revert sandbox b to a snapshot of sandbox a.
    _, stderr, err := runCLI(t, env.app, "sbx", "revert", b.ID, "--snapshot", snapA.ID)
    require.Error(t, err)
    assert.Contains(t, stderr+err.Error(), "cross-sandbox revert")
}

func TestE2ERevertRequiresSnapshotFlag(t *testing.T) {
    env := newTestEnv(t)
    sb := createSandbox(t, env.app, "rsbx")
    _, _, err := runCLI(t, env.app, "sbx", "revert", sb.ID)
    require.Error(t, err) // cobra rejects missing required flag
}
```

**Step 4 — Run** `go test ./cli/... -run TestE2ERevert -v` — expect ALL PASS.

**Step 5 — Commit:** `feat(cli): microvm sbx revert — restore workspace from snapshot in place`

---

## Task 4: Wire microvm sbx create --from-snapshot

**Why:** The `--from-snapshot` flag has been on `microvm sbx create` since the initial CLI commit but has always been a no-op. Wire it now.

**Architecture:** When `--from-snapshot` is set, the CLI dispatches to `b.Resume(...)` instead of `b.Create(...)`. State row gets the new sandbox id and a `created_from` label pointing at the source snapshot.

**Files:**
- Modify: `cli/sbx_create.go`
- Modify: `cli/cli_e2e_test.go`

**Step 1 — Update `cli/sbx_create.go`'s RunE.** Around line 26 the current logic is:

```go
spec := backend.SandboxSpec{
    Image:    image,
    Name:     name,
    CPUs:     cpus,
    MemoryMB: mem,
    FromSnap: fromSnap,
}
sb, err := b.Create(ctx, spec)
```

Replace with mode-aware dispatch — if `fromSnap != ""`, look up the snapshot and call Resume; otherwise the existing Create path:

```go
if fromSnap != "" {
    snap, err := app.Store.GetSnapshot(fromSnap)
    if err != nil {
        return fmt.Errorf("load snapshot: %w", err)
    }
    newID := state.NewSandboxID()
    resumedSb, err := b.Resume(ctx,
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
        backend.SandboxSpec{Name: name, ID: newID},
    )
    if err != nil {
        return fmt.Errorf("create --from-snapshot: %w", err)
    }
    rec := state.Sandbox{
        ID:        newID,
        Provider:  b.Name(),
        SessionID: resumedSb.SessionID,
        Image:     image,
        Name:      name,
        CPUs:      cpus,
        MemoryMB:  mem,
        Mode:      resumedSb.Mode,
        CreatedAt: time.Now(),
        LastUsed:  time.Now(),
        Labels:    map[string]string{"created_from": snap.ID},
    }
    if err := app.Store.PutSandbox(rec); err != nil {
        return err
    }
    return writeSandbox(cmd, g, rec)
}

// Existing Create path (unchanged):
spec := backend.SandboxSpec{
    Image:    image,
    Name:     name,
    CPUs:     cpus,
    MemoryMB: mem,
    FromSnap: fromSnap, // kept for API symmetry; ignored by Create in dispatch above
}
sb, err := b.Create(ctx, spec)
// ...rest unchanged...
```

You may need to add `"time"` to the import list if it isn't already.

**Step 2 — Add e2e test:**

```go
func TestE2ECreateFromSnapshot(t *testing.T) {
    env := newTestEnv(t)
    env.fake.snapshotMode = "tiered"

    src := createSandbox(t, env.app, "src")
    snapOut := runCLIJSON(t, env.app, "sbx", "snapshot", src.ID, "--name", "baseline")
    var snap state.Snapshot
    require.NoError(t, json.Unmarshal([]byte(snapOut), &snap))

    forkedOut := runCLIJSON(t, env.app, "sbx", "create", "--name", "from-snap", "--from-snapshot", snap.ID)
    var forked state.Sandbox
    require.NoError(t, json.Unmarshal([]byte(forkedOut), &forked))
    assert.NotEqual(t, src.ID, forked.ID, "minted a new sandbox id")
    assert.Equal(t, "from-snap", forked.Name)
    assert.Equal(t, snap.ID, forked.Labels["created_from"])
    assert.Equal(t, "tiered", forked.Mode)
}
```

**Step 3 — Run** `go test ./cli/... -run TestE2ECreateFromSnapshot -v` — expect PASS.

**Step 4 — Commit:** `feat(cli): wire microvm sbx create --from-snapshot`

---

## Task 5: README updates

**Files:**
- Modify: `README.md`

**Step 1 — Add a "Sandbox lifecycle" sub-section** under Snapshot Modes (or just below it — pick a sensible place; the existing README is small):

```markdown
## Sandbox lifecycle

The snapshot modes above are the durability layer. These verbs are the
user-facing operations on top:

| Verb | Effect |
|------|--------|
| `microvm sbx snapshot <id> [--name N]` | capture the workspace at a point in time |
| `microvm sbx resume <snap-id> [--name N]` | create a new sandbox whose workspace starts as that snapshot |
| `microvm sbx fork <id> [--name N]` | shorthand: snapshot + resume in one call |
| `microvm sbx revert <id> --snapshot <snap-id>` | restore an existing sandbox's workspace in place (destructive) |
| `microvm sbx create --from-snapshot <snap-id>` | create a new sandbox starting from a prior snapshot |
| `microvm sbx checkpoint <id>` | (tiered mode) promote cache `promote/` artifacts into the snapshottable workspace |

`fork` and `create --from-snapshot` are equivalent for tier-2 content;
`fork` is the convenience when you already have a running sandbox to
branch from, `create --from-snapshot` is the convenience when you're
starting from a saved snapshot id.

`revert` overwrites the existing sandbox in place. Use `fork` if you
want to keep the current state too.
```

**Step 2 — Run the full suite:**

```sh
go test ./...
cd shellagent && python3 -m pytest -q && cd ..
go vet ./...
```

Everything green.

**Step 3 — Push and open PR** with base = `feature/snapshot-modes-pr3-tiered`:

```sh
git push -u origin feature/snapshot-modes-pr4-cli-verbs
gh pr create --base feature/snapshot-modes-pr3-tiered \
  --head feature/snapshot-modes-pr4-cli-verbs \
  --title "feat(cli): sandbox lifecycle verbs — fork, revert, --from-snapshot" \
  --body "<see body template below>"
```

PR body:

```markdown
## Summary

Adds the three user-visible sandbox lifecycle verbs the cross-PR design promised but PR1–PR3 didn't ship. Layered on PR3 (#6).

- `microvm sbx fork <id> [--name N]` — snapshot + resume in one call. Returns a new sandbox whose workspace is a copy of the source at the snapshot point. Intermediate snapshot is persisted.
- `microvm sbx revert <id> --snapshot <snap-id>` — restores an existing sandbox's workspace in place from a prior snapshot. Same session id, same sandbox id; only the on-disk workspace changes. Refuses cross-sandbox reverts.
- `microvm sbx create --from-snapshot <snap-id>` — the long-stubbed flag is wired: when set, create dispatches to resume so a new sandbox can be born from a saved snapshot id.

Plus one PR3 follow-up: `TieredSnapshotter.resume` now uses `aws s3 sync --delete` for mirror-exact restore. This was harmless before (resume always wrote into a fresh empty prefix) but matters now that revert overwrites pre-existing prefixes.

All three verbs are pure CLI compositions over existing `Snapshot`/`Resume` backend methods — no new backend interface, no new shellagent ops.

## Test plan

- [x] `go test ./...` — all pass
- [x] `pytest shellagent/` — all pass
- [x] `go vet ./...` — clean
- [ ] Live AWS smoke test: snapshot a tiered sandbox, write garbage into the workspace, revert, verify workspace matches the snapshot exactly (no stale files left over).

🤖 Generated with [Claude Code](https://claude.com/claude-code)
```

**Step 4 — Commit (README only):** `docs: README sandbox lifecycle verbs section`

---

## Done criteria

- `microvm sbx fork <id> --name N` creates a new sandbox whose workspace mirrors the source's at the moment of fork. Source sandbox is unchanged.
- `microvm sbx revert <id> --snapshot <snap-id>` restores the workspace and leaves the sandbox id / session id intact; a `reverted_to` label records the operation.
- `microvm sbx create --from-snapshot <snap-id>` mints a fresh sandbox seeded from the snapshot, with a `created_from` label.
- Cross-sandbox revert is rejected.
- Tiered mode resume produces a clean mirror (no stale files leftover from prior workspace content).
- All Go and Python tests pass.
- README documents the three new verbs in a single Sandbox lifecycle section.

## Out of scope

- Auto-snapshot (`--auto-snapshot 30m` flag + scheduler daemon)
- Snapshot retention / GC (`microvm sbx snapshots prune --older-than 30d`)
- CLI-side snapshot routing for tiered (defense-in-depth IAM)
- Interactive-shell init protocol for tiered cwd
- `--include-cache` fork option (cache tier forking)

## Open questions

None blocking. All three verbs are CLI-only changes over already-shipped seams; semantics are well-defined by issues #1 and #3.
