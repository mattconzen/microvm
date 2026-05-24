package aws

import (
	"fmt"
	"strings"

	"github.com/mattconzen/microvm/backend"
)

const efsDefaultMountPath = "/mnt/efs"

type efsProvisioner struct {
	accessPointArn string
	mountPath      string
}

func newEfsProvisioner(opts backend.LoginOpts) (efsProvisioner, error) {
	arn := strings.TrimSpace(opts.EFSAccessPointArn)
	if arn == "" {
		return efsProvisioner{}, fmt.Errorf("snapshot mode %q requires --efs-access-point-arn", "efs")
	}
	if !isValidEfsAccessPointArn(arn) {
		return efsProvisioner{}, fmt.Errorf("invalid EFS access point ARN %q (expected arn:aws:elasticfilesystem:<region>:<acct>:access-point/fsap-...)", arn)
	}
	mp := strings.TrimSpace(opts.EFSMountPath)
	if mp == "" {
		mp = efsDefaultMountPath
	}
	if !strings.HasPrefix(mp, "/") {
		return efsProvisioner{}, fmt.Errorf("EFS mount path %q must be absolute", mp)
	}
	return efsProvisioner{accessPointArn: arn, mountPath: mp}, nil
}

func (efsProvisioner) Mode() string { return "efs" }

func (p efsProvisioner) ValidateLoginOpts(opts backend.LoginOpts) error {
	if strings.TrimSpace(opts.EFSAccessPointArn) == "" {
		return fmt.Errorf("snapshot mode %q requires --efs-access-point-arn", "efs")
	}
	return nil
}

func (p efsProvisioner) EnvOverrides() map[string]string {
	return map[string]string{
		"MICROVM_SNAPSHOT_MODE":    "efs",
		"MICROVM_EFS_MOUNT_PATH":   p.mountPath,
		"MICROVM_EFS_ACCESS_POINT": p.accessPointArn, // informational; the shellagent uses the mount path
	}
}

// isValidEfsAccessPointArn checks shape only (no AWS round-trip).
// Expected: arn:aws:elasticfilesystem:<region>:<account>:access-point/fsap-<hex>
func isValidEfsAccessPointArn(arn string) bool {
	parts := strings.Split(arn, ":")
	if len(parts) != 6 {
		return false
	}
	if parts[0] != "arn" || parts[1] != "aws" || parts[2] != "elasticfilesystem" {
		return false
	}
	resource := parts[5]
	if !strings.HasPrefix(resource, "access-point/fsap-") {
		return false
	}
	return true
}
