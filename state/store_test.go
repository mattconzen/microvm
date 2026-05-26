package state_test

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mattconzen/microvm/state"
)

func openStore(t *testing.T) *state.Store {
	t.Helper()
	t.Setenv("MICROVM_HOME", t.TempDir())
	s, err := state.Open()
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func TestSandboxRoundtrip(t *testing.T) {
	s := openStore(t)
	id := state.NewSandboxID()
	in := state.Sandbox{
		ID:        id,
		Provider:  "aws",
		SessionID: state.NewSessionID(),
		Name:      "alpha",
		CreatedAt: time.Now().UTC().Truncate(time.Second),
		LastUsed:  time.Now().UTC().Truncate(time.Second),
	}
	require.NoError(t, s.PutSandbox(in))

	got, err := s.GetSandbox(id)
	require.NoError(t, err)
	assert.Equal(t, in.ID, got.ID)
	assert.Equal(t, in.Provider, got.Provider)
	assert.Equal(t, in.SessionID, got.SessionID)

	all, err := s.ListSandboxes()
	require.NoError(t, err)
	assert.Len(t, all, 1)

	require.NoError(t, s.DeleteSandbox(id))
	_, err = s.GetSandbox(id)
	assert.ErrorIs(t, err, state.ErrNotFound)
}

func TestSnapshotRoundtrip(t *testing.T) {
	s := openStore(t)
	snap := state.Snapshot{
		ID:              state.NewSnapshotID(),
		SandboxID:       "mvm_x",
		Provider:        "aws",
		TargetSessionID: "sess-1",
		Kind:            "alias",
		Name:            "demo",
		CreatedAt:       time.Now().UTC().Truncate(time.Second),
	}
	require.NoError(t, s.PutSnapshot(snap))
	got, err := s.GetSnapshot(snap.ID)
	require.NoError(t, err)
	assert.Equal(t, snap.TargetSessionID, got.TargetSessionID)
}

func TestSnapshotPersistsModeAndLocator(t *testing.T) {
	s := openStore(t)
	snap := state.Snapshot{
		ID:              state.NewSnapshotID(),
		SandboxID:       "mvm_x",
		Provider:        "aws",
		TargetSessionID: "sess-1",
		Kind:            "s3",
		Mode:            "s3",
		Locator:         `{"s3_uri":"s3://b/microvm/snp_x.tar.gz"}`,
		Name:            "demo",
		CreatedAt:       time.Now().UTC().Truncate(time.Second),
	}
	require.NoError(t, s.PutSnapshot(snap))
	got, err := s.GetSnapshot(snap.ID)
	require.NoError(t, err)
	assert.Equal(t, "s3", got.Mode)
	assert.Contains(t, got.Locator, "s3://b/microvm/snp_x.tar.gz")
}

func TestRuntimeRoundtrip(t *testing.T) {
	s := openStore(t)
	rt := state.Runtime{
		Arn:            "arn:aws:bedrock-agentcore:us-east-1:123:runtime/microvm-shell",
		Region:         "us-east-1",
		SnapshotMode:   "s3",
		SnapshotBucket: "my-bucket",
		ImageDigest:    "sha256:deadbeef",
		UpdatedAt:      time.Now().UTC().Truncate(time.Second),
	}
	require.NoError(t, s.PutRuntime(rt))
	got, err := s.GetRuntime(rt.Arn)
	require.NoError(t, err)
	assert.Equal(t, rt, got)
}

func TestRuntimeNotFound(t *testing.T) {
	s := openStore(t)
	_, err := s.GetRuntime("arn:does-not-exist")
	assert.ErrorIs(t, err, state.ErrNotFound)
}

func TestIDFormat(t *testing.T) {
	id := state.NewSandboxID()
	assert.True(t, strings.HasPrefix(id, "mvm_"), "got %q", id)
	assert.Equal(t, len("mvm_")+8, len(id))

	sid := state.NewSnapshotID()
	assert.True(t, strings.HasPrefix(sid, "snp_"))

	sess := state.NewSessionID()
	assert.True(t, strings.HasPrefix(sess, "mvm-sess-"))
	assert.GreaterOrEqual(t, len(sess), 33, "AgentCore requires session id >= 33 chars")
}
