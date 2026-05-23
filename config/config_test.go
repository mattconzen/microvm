package config_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mattconzen/microvm/config"
)

func TestLoadMissingReturnsDefault(t *testing.T) {
	t.Setenv("MICROVM_HOME", t.TempDir())
	c, err := config.Load()
	require.NoError(t, err)
	assert.Equal(t, "aws", c.DefaultProvider)
}

func TestSaveAndReload(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("MICROVM_HOME", dir)
	in := &config.Config{
		DefaultProvider: "aws",
		AWS: config.AWSConfig{
			Region:          "us-east-1",
			AgentRuntimeArn: "arn:test",
			ECRImage:        "123.dkr.ecr.us-east-1.amazonaws.com/microvm-shellagent",
			ECRImageDigest:  "sha256:deadbeef",
		},
	}
	require.NoError(t, config.Save(in))

	// Verify file mode (0600).
	st, err := os.Stat(filepath.Join(dir, "config.yaml"))
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), st.Mode().Perm())

	out, err := config.Load()
	require.NoError(t, err)
	assert.Equal(t, in.AWS.Region, out.AWS.Region)
	assert.Equal(t, in.AWS.AgentRuntimeArn, out.AWS.AgentRuntimeArn)
	assert.Equal(t, in.AWS.ECRImageDigest, out.AWS.ECRImageDigest)
}
