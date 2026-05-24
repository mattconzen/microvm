package backend

import (
	"context"
	"errors"
	"io"
	"time"
)

var ErrNotSupported = errors.New("operation not supported by this backend")

type Sandbox struct {
	ID        string
	Provider  string
	SessionID string
	Image     string
	Name      string
	CPUs      float64
	MemoryMB  int
	Mode      string // snapshot mode the owning runtime was registered with
	CreatedAt time.Time
	LastUsed  time.Time
}

type Snapshot struct {
	ID              string
	SandboxID       string
	Provider        string
	TargetSessionID string
	Kind            string // legacy field; "alias" for pre-mode records
	Mode            string // "" (legacy) | "none" | "s3" | "efs" | "tiered"
	Locator         string // mode-decoded JSON blob; opaque to shared code
	Name            string
	CreatedAt       time.Time
}

type SandboxSpec struct {
	Image    string
	Name     string
	CPUs     float64
	MemoryMB int
	FromSnap string
	// ID is the new ID for the resumed sandbox. Optional for Create; required
	// for Resume so the shellagent can resolve per-sandbox storage.
	ID string
}

type SnapshotSpec struct {
	ID   string
	Name string
}

type LoginOpts struct {
	Region         string
	RuntimeArn     string
	ImageDigest    string
	Rebuild        bool
	SnapshotMode   string // "" | "none" | "s3" | "efs" | "tiered"
	SnapshotBucket string // required when SnapshotMode is "s3" or "tiered"
	// EFS-only. Required when SnapshotMode == "efs"; ignored otherwise.
	EFSAccessPointArn string
	EFSMountPath      string // default "/mnt/efs"

	// Tiered-only. Required when SnapshotMode == "tiered". Ignored otherwise.
	S3FilesAccessPointArn string
	S3FilesBucket         string // S3 bucket backing the access point (used to mint locators)
	S3FilesMountPath      string // default "/workspace"
}

type ExecIO struct {
	Stdin  io.Reader
	Stdout io.Writer
	Stderr io.Writer
	TTY    bool
}

type TTY struct {
	In   io.Reader
	Out  io.Writer
	Cols uint16
	Rows uint16
}

type Backend interface {
	Name() string
	Login(ctx context.Context, opts LoginOpts) error
	Create(ctx context.Context, spec SandboxSpec) (Sandbox, error)
	Get(ctx context.Context, sb Sandbox) (Sandbox, error)
	Exec(ctx context.Context, sb Sandbox, cmd []string, io ExecIO) (exitCode int, err error)
	CopyTo(ctx context.Context, sb Sandbox, localPath, remotePath string) (int64, error)
	CopyFrom(ctx context.Context, sb Sandbox, remotePath, localPath string) (int64, error)
	Shell(ctx context.Context, sb Sandbox, tty TTY) error
	Snapshot(ctx context.Context, sb Sandbox, spec SnapshotSpec) (Snapshot, error)
	Resume(ctx context.Context, snap Snapshot, spec SandboxSpec) (Sandbox, error)
	Terminate(ctx context.Context, sb Sandbox) error
	// Checkpoint promotes tier-1 cache content into the tier-2 workspace so
	// the next Snapshot includes it. Tiered-mode only; other modes return an
	// error.
	Checkpoint(ctx context.Context, sb Sandbox) error
}
