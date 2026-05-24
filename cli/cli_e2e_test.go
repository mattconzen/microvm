package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mattconzen/microvm/backend"
	"github.com/mattconzen/microvm/config"
	"github.com/mattconzen/microvm/state"
)

type testEnv struct {
	app   *App
	fake  *fakeBackend
	store *state.Store
}

func newTestEnv(t *testing.T) *testEnv {
	t.Helper()
	t.Setenv("MICROVM_HOME", t.TempDir())
	store, err := state.Open()
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })

	fb := newFakeBackend()
	reg := backend.NewRegistry()
	reg.Register(fb)
	app := &App{
		Version:  "test",
		Config:   &config.Config{DefaultProvider: "aws"},
		Registry: reg,
		Store:    store,
	}
	return &testEnv{app: app, fake: fb, store: store}
}

func runCLI(t *testing.T, app *App, args ...string) (string, string, error) {
	t.Helper()
	cmd := NewRoot(context.Background(), app)
	var stdout, stderr bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)
	cmd.SetArgs(args)
	err := cmd.Execute()
	return stdout.String(), stderr.String(), err
}

func runCLIJSON(t *testing.T, app *App, args ...string) string {
	t.Helper()
	full := append([]string{"--output", "json"}, args...)
	stdout, _, err := runCLI(t, app, full...)
	require.NoError(t, err)
	return stdout
}

func createSandbox(t *testing.T, app *App, name string) state.Sandbox {
	t.Helper()
	out := runCLIJSON(t, app, "sbx", "create", "--name", name)
	var sb state.Sandbox
	require.NoError(t, json.Unmarshal([]byte(out), &sb))
	require.True(t, strings.HasPrefix(sb.ID, "mvm_"), "got id %q", sb.ID)
	return sb
}

func TestE2ECreateExecListGetTerminate(t *testing.T) {
	env := newTestEnv(t)

	sb := createSandbox(t, env.app, "alpha")
	assert.Equal(t, "aws", sb.Provider)
	assert.Equal(t, "alpha", sb.Name)

	stored, err := env.store.GetSandbox(sb.ID)
	require.NoError(t, err)
	assert.Equal(t, sb.SessionID, stored.SessionID)

	stdout, _, err := runCLI(t, env.app, "sbx", "exec", sb.ID, "--", "echo", "hi")
	require.NoError(t, err)
	assert.Equal(t, "ok\n", stdout)

	require.Len(t, env.fake.execs, 1)
	assert.Equal(t, sb.SessionID, env.fake.execs[0].SessionID)
	assert.Equal(t, []string{"echo", "hi"}, env.fake.execs[0].Cmd)

	postExec, err := env.store.GetSandbox(sb.ID)
	require.NoError(t, err)
	assert.True(t, !postExec.LastUsed.Before(stored.LastUsed),
		"last_used should not regress: was %s now %s", stored.LastUsed, postExec.LastUsed)

	listOut := runCLIJSON(t, env.app, "sbx", "list")
	var rows []state.Sandbox
	require.NoError(t, json.Unmarshal([]byte(listOut), &rows))
	require.Len(t, rows, 1)
	assert.Equal(t, sb.ID, rows[0].ID)

	getOut := runCLIJSON(t, env.app, "sbx", "get", sb.ID)
	var got state.Sandbox
	require.NoError(t, json.Unmarshal([]byte(getOut), &got))
	assert.Equal(t, sb.ID, got.ID)
	assert.Equal(t, sb.SessionID, got.SessionID)

	stdout, _, err = runCLI(t, env.app, "sbx", "terminate", sb.ID)
	require.NoError(t, err)
	assert.Contains(t, stdout, "terminated "+sb.ID)

	_, err = env.store.GetSandbox(sb.ID)
	assert.ErrorIs(t, err, state.ErrNotFound)
}

func TestE2ESnapshotResume(t *testing.T) {
	env := newTestEnv(t)
	sb := createSandbox(t, env.app, "snapme")

	snapOut := runCLIJSON(t, env.app, "sbx", "snapshot", sb.ID, "--name", "checkpoint-1")
	var snap state.Snapshot
	require.NoError(t, json.Unmarshal([]byte(snapOut), &snap))
	assert.Equal(t, sb.ID, snap.SandboxID)
	assert.Equal(t, "alias", snap.Kind)
	assert.Equal(t, sb.SessionID, snap.TargetSessionID)

	stored, err := env.store.GetSnapshot(snap.ID)
	require.NoError(t, err)
	assert.Equal(t, sb.SessionID, stored.TargetSessionID)

	resumeOut := runCLIJSON(t, env.app, "sbx", "resume", snap.ID, "--name", "resumed")
	var resumed state.Sandbox
	require.NoError(t, json.Unmarshal([]byte(resumeOut), &resumed))
	assert.NotEqual(t, sb.ID, resumed.ID, "resume mints a new sandbox id")
	assert.Equal(t, sb.SessionID, resumed.SessionID, "resumed sandbox reuses snapshot's session id")
	assert.Equal(t, "resumed", resumed.Name)
	assert.Equal(t, snap.ID, resumed.Labels["resumed_from"])

	all, err := env.store.ListSandboxes()
	require.NoError(t, err)
	assert.Len(t, all, 2)
}

func TestE2ESnapshotPersistsModeAndLocator(t *testing.T) {
	env := newTestEnv(t)
	env.fake.snapshotMode = "s3"

	sb := createSandbox(t, env.app, "modeful")
	// Sandbox carries the runtime mode it was created under.
	assert.Equal(t, "s3", sb.Mode)

	snapOut := runCLIJSON(t, env.app, "sbx", "snapshot", sb.ID, "--name", "baseline")
	var snap state.Snapshot
	require.NoError(t, json.Unmarshal([]byte(snapOut), &snap))
	assert.Equal(t, "s3", snap.Mode)
	assert.Equal(t, "s3", snap.Kind)
	assert.Contains(t, snap.Locator, "s3_uri")

	// State.db row must carry the same mode/locator so post-restart resumes work.
	stored, err := env.store.GetSnapshot(snap.ID)
	require.NoError(t, err)
	assert.Equal(t, "s3", stored.Mode)
	assert.Equal(t, snap.Locator, stored.Locator)

	// fakeBackend recorded the snapshot call with the runtime mode.
	require.Len(t, env.fake.snapshots, 1)
	call := env.fake.snapshots[0]
	assert.Equal(t, "s3", call.Mode)
	assert.Equal(t, "baseline", call.Spec.Name)
	assert.True(t, strings.HasPrefix(call.Spec.ID, "snp_"), "spec.ID should be minted before backend call, got %q", call.Spec.ID)
}

func TestE2EEfsModeRoundTrip(t *testing.T) {
	env := newTestEnv(t)
	env.fake.snapshotMode = "efs"

	sb := createSandbox(t, env.app, "efs-sbx")
	assert.Equal(t, "efs", sb.Mode)

	snapOut := runCLIJSON(t, env.app, "sbx", "snapshot", sb.ID, "--name", "ckpt")
	var snap state.Snapshot
	require.NoError(t, json.Unmarshal([]byte(snapOut), &snap))
	assert.Equal(t, "efs", snap.Mode)
	assert.Equal(t, "efs", snap.Kind)
	assert.Contains(t, snap.Locator, "efs_path")
	assert.Contains(t, snap.Locator, "/mnt/efs/snapshots/")

	stored, err := env.store.GetSnapshot(snap.ID)
	require.NoError(t, err)
	assert.Equal(t, snap.Locator, stored.Locator)

	resumeOut := runCLIJSON(t, env.app, "sbx", "resume", snap.ID, "--name", "resumed")
	var resumed state.Sandbox
	require.NoError(t, json.Unmarshal([]byte(resumeOut), &resumed))
	assert.Equal(t, "efs", resumed.Mode)
	assert.Equal(t, snap.ID, resumed.Labels["resumed_from"])
}

func TestE2ETieredModeRoundTrip(t *testing.T) {
	env := newTestEnv(t)
	env.fake.snapshotMode = "tiered"

	sb := createSandbox(t, env.app, "tiered-sbx")
	assert.Equal(t, "tiered", sb.Mode)

	snapOut := runCLIJSON(t, env.app, "sbx", "snapshot", sb.ID, "--name", "ckpt")
	var snap state.Snapshot
	require.NoError(t, json.Unmarshal([]byte(snapOut), &snap))
	assert.Equal(t, "tiered", snap.Mode)
	assert.Equal(t, "tiered", snap.Kind)
	assert.Contains(t, snap.Locator, "workspace_prefix")
	assert.Contains(t, snap.Locator, "snapshot_prefix")
	assert.Contains(t, snap.Locator, "s3://fake/sessions/"+sb.ID+"/")
	assert.Contains(t, snap.Locator, "s3://fake/snapshots/"+snap.ID+"/")

	resumeOut := runCLIJSON(t, env.app, "sbx", "resume", snap.ID, "--name", "resumed")
	var resumed state.Sandbox
	require.NoError(t, json.Unmarshal([]byte(resumeOut), &resumed))
	assert.Equal(t, "tiered", resumed.Mode)
	assert.Equal(t, snap.ID, resumed.Labels["resumed_from"])
}

func TestE2ECheckpoint(t *testing.T) {
	env := newTestEnv(t)
	env.fake.snapshotMode = "tiered"

	sb := createSandbox(t, env.app, "ckpt-sbx")
	stdout, _, err := runCLI(t, env.app, "sbx", "checkpoint", sb.ID)
	require.NoError(t, err)
	assert.Contains(t, stdout, "checkpointed "+sb.ID)
	require.Len(t, env.fake.checkpoints, 1)
	assert.Equal(t, sb.SessionID, env.fake.checkpoints[0].SessionID)
}

func TestE2EResumeRejectsModeMismatch(t *testing.T) {
	env := newTestEnv(t)
	env.fake.snapshotMode = "s3"

	sb := createSandbox(t, env.app, "modeful")
	snapOut := runCLIJSON(t, env.app, "sbx", "snapshot", sb.ID, "--name", "x")
	var snap state.Snapshot
	require.NoError(t, json.Unmarshal([]byte(snapOut), &snap))
	require.Equal(t, "s3", snap.Mode)

	// Flip the runtime mode so the snapshot now lives in the wrong runtime.
	env.fake.snapshotMode = "none"
	_, stderr, err := runCLI(t, env.app, "sbx", "resume", snap.ID, "--name", "should-fail")
	require.Error(t, err)
	combined := stderr + err.Error()
	assert.Contains(t, combined, "does not match active runtime mode")
}

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

func TestE2ECopyRoundTrip(t *testing.T) {
	env := newTestEnv(t)
	sb := createSandbox(t, env.app, "copyme")

	env.fake.execFn = func(_ backend.Sandbox, cmd []string, io backend.ExecIO) (int, error) {
		require.Equal(t, "cat", cmd[0])
		data := env.fake.files[cmd[1]]
		if io.Stdout != nil {
			_, _ = io.Stdout.Write(data)
		}
		return 0, nil
	}

	dir := t.TempDir()
	src := filepath.Join(dir, "src.txt")
	payload := []byte("round-trip payload\n")
	require.NoError(t, os.WriteFile(src, payload, 0o644))

	stdout, _, err := runCLI(t, env.app, "sbx", "cp", src, sb.ID+":/work/file.txt")
	require.NoError(t, err)
	assert.Contains(t, stdout, "copied")
	assert.Equal(t, payload, env.fake.files["/work/file.txt"])

	stdout, _, err = runCLI(t, env.app, "sbx", "exec", sb.ID, "--", "cat", "/work/file.txt")
	require.NoError(t, err)
	assert.Equal(t, string(payload), stdout)

	dst := filepath.Join(dir, "dst.txt")
	stdout, _, err = runCLI(t, env.app, "sbx", "cp", sb.ID+":/work/file.txt", dst)
	require.NoError(t, err)
	assert.Contains(t, stdout, "copied")

	gotBytes, err := os.ReadFile(dst)
	require.NoError(t, err)
	assert.Equal(t, payload, gotBytes)
}
