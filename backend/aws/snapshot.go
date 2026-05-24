package aws

import (
	"fmt"
	"strings"

	"github.com/mattconzen/microvm/backend"
)

// snapshotProvisioner captures the per-mode differences at runtime-registration
// time: which env vars the shellagent container needs, and what login-time
// validation applies. The runtime invocation path (Snapshot/Resume RPCs into
// the shellagent) stays mode-agnostic; this interface only touches the bits
// that vary across {none, s3, efs, tiered}.
type snapshotProvisioner interface {
	Mode() string
	ValidateLoginOpts(opts backend.LoginOpts) error
	EnvOverrides() map[string]string
}

// ProvisionerFor returns the provisioner matching mode. Empty mode resolves to
// "none" so existing deployments and tests keep working unchanged.
func ProvisionerFor(mode string, opts backend.LoginOpts) (snapshotProvisioner, error) {
	mode = strings.TrimSpace(mode)
	if mode == "" {
		mode = "none"
	}
	switch mode {
	case "none":
		return aliasProvisioner{}, nil
	case "s3":
		return newS3Provisioner(opts)
	case "efs":
		return newEfsProvisioner(opts)
	case "tiered":
		return newTieredProvisioner(opts)
	default:
		return nil, fmt.Errorf(
			"unknown snapshot mode %q (want one of: none, s3, efs, tiered)",
			mode,
		)
	}
}

type aliasProvisioner struct{}

func (aliasProvisioner) Mode() string                                { return "none" }
func (aliasProvisioner) ValidateLoginOpts(_ backend.LoginOpts) error { return nil }
func (aliasProvisioner) EnvOverrides() map[string]string             { return nil }
