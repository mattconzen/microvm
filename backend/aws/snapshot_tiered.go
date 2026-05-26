package aws

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/mattconzen/microvm/backend"
)

const (
	tieredDefaultMountPath = "/workspace"
	tieredCacheRoot        = "/var/microvm/cache"
)

// Resource segment of an S3 access-point ARN, e.g. accesspoint/microvm-files.
var s3AccessPointResourceRe = regexp.MustCompile(`^accesspoint/[a-z0-9]([a-z0-9-]{1,48}[a-z0-9])?$`)

type tieredProvisioner struct {
	accessPointArn string
	bucket         string
	mountPath      string
}

func newTieredProvisioner(opts backend.LoginOpts) (tieredProvisioner, error) {
	arn := strings.TrimSpace(opts.S3FilesAccessPointArn)
	if arn == "" {
		return tieredProvisioner{}, fmt.Errorf("snapshot mode %q requires --s3-files-access-point-arn", "tiered")
	}
	if !isValidS3AccessPointArn(arn) {
		return tieredProvisioner{}, fmt.Errorf("invalid S3 access point ARN %q", arn)
	}
	bucket := strings.TrimSpace(opts.S3FilesBucket)
	if bucket == "" {
		return tieredProvisioner{}, fmt.Errorf("snapshot mode %q requires --s3-files-bucket", "tiered")
	}
	if !isValidBucketName(bucket) {
		return tieredProvisioner{}, fmt.Errorf("invalid S3 bucket name %q", bucket)
	}
	mp := strings.TrimSpace(opts.S3FilesMountPath)
	if mp == "" {
		mp = tieredDefaultMountPath
	}
	if !strings.HasPrefix(mp, "/") {
		return tieredProvisioner{}, fmt.Errorf("S3 Files mount path %q must be absolute", mp)
	}
	return tieredProvisioner{accessPointArn: arn, bucket: bucket, mountPath: mp}, nil
}

func (tieredProvisioner) Mode() string { return "tiered" }

func (p tieredProvisioner) ValidateLoginOpts(opts backend.LoginOpts) error {
	if strings.TrimSpace(opts.S3FilesAccessPointArn) == "" {
		return fmt.Errorf("snapshot mode %q requires --s3-files-access-point-arn", "tiered")
	}
	if strings.TrimSpace(opts.S3FilesBucket) == "" {
		return fmt.Errorf("snapshot mode %q requires --s3-files-bucket", "tiered")
	}
	return nil
}

func (p tieredProvisioner) EnvOverrides() map[string]string {
	return map[string]string{
		"MICROVM_SNAPSHOT_MODE":      "tiered",
		"MICROVM_S3FILES_BUCKET":     p.bucket,
		"MICROVM_S3FILES_MOUNT_PATH": p.mountPath,
		"MICROVM_CACHE_ROOT":         tieredCacheRoot,
	}
}

func isValidS3AccessPointArn(arn string) bool {
	parts := strings.Split(arn, ":")
	if len(parts) != 6 {
		return false
	}
	if parts[0] != "arn" || parts[1] != "aws" || parts[2] != "s3" {
		return false
	}
	return s3AccessPointResourceRe.MatchString(parts[5])
}
