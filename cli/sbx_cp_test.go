package cli

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestParseCpArg(t *testing.T) {
	cases := []struct {
		in         string
		wantID     string
		wantPath   string
		wantRemote bool
	}{
		{"./local.txt", "", "./local.txt", false},
		{"/abs/path", "", "/abs/path", false},
		{"mvm_abc123:/tmp/x", "mvm_abc123", "/tmp/x", true},
		{"mvm_abc123:relative", "mvm_abc123", "relative", true},
		// Non-mvm prefix shouldn't be misread as remote (e.g. accidental file with colon).
		{"file:with:colons", "", "file:with:colons", false},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			id, path, remote := parseCpArg(tc.in)
			assert.Equal(t, tc.wantID, id)
			assert.Equal(t, tc.wantPath, path)
			assert.Equal(t, tc.wantRemote, remote)
		})
	}
}
