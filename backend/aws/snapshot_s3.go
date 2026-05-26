package aws

import (
	"fmt"
	"strings"

	"github.com/mattconzen/microvm/backend"
)

const s3DefaultPrefix = "microvm/"

type s3Provisioner struct {
	bucket string
	prefix string
}

func newS3Provisioner(opts backend.LoginOpts) (s3Provisioner, error) {
	bucket := strings.TrimSpace(opts.SnapshotBucket)
	if bucket == "" {
		return s3Provisioner{}, fmt.Errorf("snapshot mode %q requires --snapshot-bucket", "s3")
	}
	if !isValidBucketName(bucket) {
		return s3Provisioner{}, fmt.Errorf("invalid S3 bucket name %q", bucket)
	}
	return s3Provisioner{bucket: bucket, prefix: s3DefaultPrefix}, nil
}

func (s3Provisioner) Mode() string { return "s3" }

func (p s3Provisioner) ValidateLoginOpts(opts backend.LoginOpts) error {
	if strings.TrimSpace(opts.SnapshotBucket) == "" {
		return fmt.Errorf("snapshot mode %q requires --snapshot-bucket", "s3")
	}
	return nil
}

func (p s3Provisioner) EnvOverrides() map[string]string {
	return map[string]string{
		"MICROVM_SNAPSHOT_MODE":   "s3",
		"MICROVM_SNAPSHOT_BUCKET": p.bucket,
		"MICROVM_SNAPSHOT_PREFIX": p.prefix,
	}
}

// isValidBucketName applies the conservative subset of S3 bucket-naming rules
// that we can validate without an AWS round-trip: 3–63 chars, lowercase
// letters/digits/dots/hyphens, no leading/trailing dot or hyphen, no
// whitespace, no path separators.
func isValidBucketName(name string) bool {
	if len(name) < 3 || len(name) > 63 {
		return false
	}
	if strings.ContainsAny(name, " \t\n\r/:_") {
		return false
	}
	if name[0] == '.' || name[0] == '-' || name[len(name)-1] == '.' || name[len(name)-1] == '-' {
		return false
	}
	for _, r := range name {
		if !(r >= 'a' && r <= 'z') && !(r >= '0' && r <= '9') && r != '-' && r != '.' {
			return false
		}
	}
	return true
}
