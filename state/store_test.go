package state_test

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mattconzen/monorepo/apps/microvm/state"
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
