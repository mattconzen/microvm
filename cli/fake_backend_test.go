package cli

import (
	"context"
	"fmt"
	"os"
	"sync"
	"time"

	"github.com/mattconzen/microvm/backend"
)

type execCall struct {
	SessionID string
	Cmd       []string
}

type snapshotCall struct {
	SessionID string
	Spec      backend.SnapshotSpec
	Mode      string
}

type fakeBackend struct {
	mu           sync.Mutex
	name         string
	files        map[string][]byte
	execs        []execCall
	snapshots    []snapshotCall
	snapshotMode string // runtime mode the fake claims to have been registered with
	execFn       func(sb backend.Sandbox, cmd []string, io backend.ExecIO) (int, error)
	now          func() time.Time
}

func newFakeBackend() *fakeBackend {
	return &fakeBackend{
		name:  "aws",
		files: map[string][]byte{},
		now:   func() time.Time { return time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC) },
	}
}

func (f *fakeBackend) Name() string { return f.name }

func (f *fakeBackend) Login(_ context.Context, _ backend.LoginOpts) error { return nil }

func (f *fakeBackend) Create(_ context.Context, spec backend.SandboxSpec) (backend.Sandbox, error) {
	mode := f.snapshotMode
	if mode == "" {
		mode = "none"
	}
	return backend.Sandbox{
		Provider:  f.name,
		Image:     spec.Image,
		Name:      spec.Name,
		CPUs:      spec.CPUs,
		MemoryMB:  spec.MemoryMB,
		Mode:      mode,
		CreatedAt: f.now(),
	}, nil
}

func (f *fakeBackend) Get(_ context.Context, sb backend.Sandbox) (backend.Sandbox, error) {
	return sb, nil
}

func (f *fakeBackend) Exec(_ context.Context, sb backend.Sandbox, cmd []string, io backend.ExecIO) (int, error) {
	f.mu.Lock()
	f.execs = append(f.execs, execCall{SessionID: sb.SessionID, Cmd: append([]string{}, cmd...)})
	fn := f.execFn
	f.mu.Unlock()
	if fn != nil {
		return fn(sb, cmd, io)
	}
	if io.Stdout != nil {
		_, _ = io.Stdout.Write([]byte("ok\n"))
	}
	return 0, nil
}

func (f *fakeBackend) CopyTo(_ context.Context, _ backend.Sandbox, localPath, remotePath string) (int64, error) {
	data, err := os.ReadFile(localPath)
	if err != nil {
		return 0, err
	}
	f.mu.Lock()
	f.files[remotePath] = data
	f.mu.Unlock()
	return int64(len(data)), nil
}

func (f *fakeBackend) CopyFrom(_ context.Context, _ backend.Sandbox, remotePath, localPath string) (int64, error) {
	f.mu.Lock()
	data, ok := f.files[remotePath]
	f.mu.Unlock()
	if !ok {
		return 0, fmt.Errorf("fake: no file at %s", remotePath)
	}
	if err := os.WriteFile(localPath, data, 0o644); err != nil {
		return 0, err
	}
	return int64(len(data)), nil
}

func (f *fakeBackend) Shell(_ context.Context, _ backend.Sandbox, _ backend.TTY) error { return nil }

func (f *fakeBackend) Snapshot(_ context.Context, sb backend.Sandbox, spec backend.SnapshotSpec) (backend.Snapshot, error) {
	mode := f.snapshotMode
	if mode == "" {
		mode = "none"
	}
	kind := "alias"
	locator := ""
	// Fake locator schema is mode-specific but opaque to shared code; the
	// only requirement is it round-trips through state and back to Resume.
	switch mode {
	case "efs":
		kind = mode
		locator = fmt.Sprintf(`{"efs_path":"/mnt/efs/snapshots/%s"}`, spec.ID)
	case "s3":
		kind = mode
		locator = fmt.Sprintf(`{"s3_uri":"fake://s3/%s"}`, spec.ID)
	default:
		// none mode: kind stays "alias" and locator stays empty.
	}
	f.mu.Lock()
	f.snapshots = append(f.snapshots, snapshotCall{SessionID: sb.SessionID, Spec: spec, Mode: mode})
	f.mu.Unlock()
	return backend.Snapshot{
		SandboxID:       sb.ID,
		Provider:        f.name,
		TargetSessionID: sb.SessionID,
		Kind:            kind,
		Mode:            mode,
		Locator:         locator,
		Name:            spec.Name,
		CreatedAt:       f.now(),
	}, nil
}

func (f *fakeBackend) Resume(_ context.Context, snap backend.Snapshot, spec backend.SandboxSpec) (backend.Sandbox, error) {
	runtimeMode := f.snapshotMode
	if runtimeMode == "" {
		runtimeMode = "none"
	}
	snapMode := snap.Mode
	if snapMode == "" {
		snapMode = "none"
	}
	if snapMode != runtimeMode {
		return backend.Sandbox{}, fmt.Errorf(
			"snapshot mode %q does not match active runtime mode %q",
			snapMode, runtimeMode,
		)
	}
	return backend.Sandbox{
		ID:        spec.ID,
		Provider:  f.name,
		SessionID: snap.TargetSessionID,
		Name:      spec.Name,
		Mode:      snapMode,
		CreatedAt: f.now(),
	}, nil
}

func (f *fakeBackend) Terminate(_ context.Context, _ backend.Sandbox) error {
	return nil
}
