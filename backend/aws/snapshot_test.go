package aws_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mattconzen/microvm/backend"
	awsbackend "github.com/mattconzen/microvm/backend/aws"
)

func TestProvisionerFor_DefaultsToNone(t *testing.T) {
	p, err := awsbackend.ProvisionerFor("", backend.LoginOpts{})
	require.NoError(t, err)
	assert.Equal(t, "none", p.Mode())
	assert.Empty(t, p.EnvOverrides())
}

func TestProvisionerFor_AliasMode(t *testing.T) {
	p, err := awsbackend.ProvisionerFor("none", backend.LoginOpts{})
	require.NoError(t, err)
	assert.Equal(t, "none", p.Mode())
}

func TestProvisionerFor_EfsMode(t *testing.T) {
	p, err := awsbackend.ProvisionerFor("efs", backend.LoginOpts{
		EFSAccessPointArn: "arn:aws:elasticfilesystem:us-east-1:123:access-point/fsap-abc123",
		EFSMountPath:      "/mnt/efs",
	})
	require.NoError(t, err)
	assert.Equal(t, "efs", p.Mode())
}

func TestEfsProvisioner_RequiresAccessPointArn(t *testing.T) {
	_, err := awsbackend.ProvisionerFor("efs", backend.LoginOpts{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "--efs-access-point-arn")
}

func TestEfsProvisioner_RejectsInvalidArn(t *testing.T) {
	cases := []string{
		"not-an-arn",
		"arn:aws:s3:::bucket", // wrong service
		"arn:aws:elasticfilesystem:us-east-1:123:file-system/fs-abc",  // FS ARN, not AP ARN
		"arn:aws:elasticfilesystem:us-east-1:123:access-point/fsap-",  // empty hex suffix
		"arn:aws:elasticfilesystem:us-east-1:123:access-point/fsap-XYZ", // uppercase hex (fsap IDs are lowercase)
	}
	for _, a := range cases {
		_, err := awsbackend.ProvisionerFor("efs", backend.LoginOpts{EFSAccessPointArn: a})
		require.Errorf(t, err, "expected %q to be rejected", a)
	}
}

func TestEfsProvisioner_DefaultsMountPath(t *testing.T) {
	p, err := awsbackend.ProvisionerFor("efs", backend.LoginOpts{
		EFSAccessPointArn: "arn:aws:elasticfilesystem:us-east-1:123:access-point/fsap-abc123",
	})
	require.NoError(t, err)
	env := p.EnvOverrides()
	assert.Equal(t, "efs", env["MICROVM_SNAPSHOT_MODE"])
	assert.Equal(t, "/mnt/efs", env["MICROVM_EFS_MOUNT_PATH"])
}

func TestEfsProvisioner_RespectsCustomMountPath(t *testing.T) {
	p, err := awsbackend.ProvisionerFor("efs", backend.LoginOpts{
		EFSAccessPointArn: "arn:aws:elasticfilesystem:us-east-1:123:access-point/fsap-abc123",
		EFSMountPath:      "/data",
	})
	require.NoError(t, err)
	assert.Equal(t, "/data", p.EnvOverrides()["MICROVM_EFS_MOUNT_PATH"])
}

func TestProvisionerFor_TieredNotImplemented(t *testing.T) {
	_, err := awsbackend.ProvisionerFor("tiered", backend.LoginOpts{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not implemented")
	assert.Contains(t, err.Error(), "PR3")
}

func TestProvisionerFor_UnknownMode(t *testing.T) {
	_, err := awsbackend.ProvisionerFor("nonsense", backend.LoginOpts{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unknown snapshot mode")
}

func TestS3Provisioner_RequiresBucket(t *testing.T) {
	_, err := awsbackend.ProvisionerFor("s3", backend.LoginOpts{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "--snapshot-bucket")
}

func TestS3Provisioner_RejectsInvalidBucket(t *testing.T) {
	cases := []string{
		"AB",                  // uppercase + too short
		"bucket name",         // whitespace
		"bucket/with/slashes", // path separators
		"-leadinghyphen",
		"trailinghyphen-",
		".leadingdot",
		"trailingdot.",
		"under_score",
	}
	for _, b := range cases {
		_, err := awsbackend.ProvisionerFor("s3", backend.LoginOpts{SnapshotBucket: b})
		require.Errorf(t, err, "expected %q to be rejected", b)
	}
}

func TestS3Provisioner_EnvVars(t *testing.T) {
	p, err := awsbackend.ProvisionerFor("s3", backend.LoginOpts{SnapshotBucket: "my-bucket"})
	require.NoError(t, err)
	env := p.EnvOverrides()
	assert.Equal(t, "s3", env["MICROVM_SNAPSHOT_MODE"])
	assert.Equal(t, "my-bucket", env["MICROVM_SNAPSHOT_BUCKET"])
	assert.Equal(t, "microvm/", env["MICROVM_SNAPSHOT_PREFIX"])
}

func TestS3Provisioner_ValidateLoginOpts(t *testing.T) {
	p, err := awsbackend.ProvisionerFor("s3", backend.LoginOpts{SnapshotBucket: "my-bucket"})
	require.NoError(t, err)
	require.NoError(t, p.ValidateLoginOpts(backend.LoginOpts{SnapshotBucket: "my-bucket"}))
	err = p.ValidateLoginOpts(backend.LoginOpts{SnapshotBucket: ""})
	require.Error(t, err)
}
