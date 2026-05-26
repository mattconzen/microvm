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
}
