package aws

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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

func TestShellRequest(t *testing.T) {
	b, err := ShellRequest(120, 40)
	require.NoError(t, err)
	var got Request
	require.NoError(t, json.Unmarshal(b, &got))
	assert.Equal(t, OpShell, got.Op)
	assert.True(t, got.TTY)
	assert.Equal(t, uint16(120), got.Cols)
	assert.Equal(t, uint16(40), got.Rows)
}
