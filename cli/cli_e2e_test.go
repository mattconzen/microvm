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

	"github.com/mattconzen/monorepo/apps/microvm/backend"
	"github.com/mattconzen/monorepo/apps/microvm/config"
	"github.com/mattconzen/monorepo/apps/microvm/state"
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
