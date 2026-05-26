package aws

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mattconzen/microvm/backend"
)

func TestExecRequestRoundtrip(t *testing.T) {
	cases := []struct {
		name string
		cmd  []string
	}{
		{"simple", []string{"echo", "hi"}},
		{"shell", []string{"sh", "-lc", "echo hi && date"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			b, err := ExecRequest(tc.cmd)
			require.NoError(t, err)
			var got Request
			require.NoError(t, json.Unmarshal(b, &got))
			assert.Equal(t, OpExec, got.Op)
			assert.Equal(t, tc.cmd, got.Cmd)
		})
	}
}

func TestPutGetRoundtrip(t *testing.T) {
	payload := []byte{0, 1, 2, 0xff, 0x7f, 'h', 'i'}
	b, err := PutRequest("/tmp/x", payload)
	require.NoError(t, err)
	var got Request
	require.NoError(t, json.Unmarshal(b, &got))
	assert.Equal(t, OpPut, got.Op)
	assert.Equal(t, "/tmp/x", got.Path)
	decoded, err := DecodeB64(got.B64)
	require.NoError(t, err)
	assert.Equal(t, payload, decoded)

	b, err = GetRequest("/tmp/y")
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal(b, &got))
	assert.Equal(t, OpGet, got.Op)
	assert.Equal(t, "/tmp/y", got.Path)
}

func TestSnapshotResumeRequests(t *testing.T) {
	b, err := SnapshotRequest(backend.SnapshotSpec{ID: "snp_1", Name: "demo"}, "none")
	require.NoError(t, err)
	var got Request
	require.NoError(t, json.Unmarshal(b, &got))
	assert.Equal(t, OpSnapshot, got.Op)
	assert.Equal(t, "demo", got.Name)
	assert.Equal(t, "snp_1", got.SnapID)
	assert.Equal(t, "none", got.Mode)

	b, err = ResumeRequest("sess-1", `{"s3_uri":"s3://b/k"}`, "s3")
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal(b, &got))
	assert.Equal(t, OpResume, got.Op)
	assert.Equal(t, "sess-1", got.Alias)
	assert.Equal(t, `{"s3_uri":"s3://b/k"}`, got.Locator)
	assert.Equal(t, "s3", got.Mode)
}
