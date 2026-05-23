package backend_test

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mattconzen/monorepo/apps/microvm/backend"
)

type stubBackend struct{ name string }

func (s *stubBackend) Name() string                                                    { return s.name }
func (s *stubBackend) Login(context.Context, backend.LoginOpts) error                  { return nil }
func (s *stubBackend) Create(context.Context, backend.SandboxSpec) (backend.Sandbox, error) {
	return backend.Sandbox{Provider: s.name}, nil
}
func (s *stubBackend) Get(_ context.Context, sb backend.Sandbox) (backend.Sandbox, error) {
	return sb, nil
}
func (s *stubBackend) Exec(context.Context, backend.Sandbox, []string, backend.ExecIO) (int, error) {
	return 0, nil
}
func (s *stubBackend) CopyTo(context.Context, backend.Sandbox, string, string) (int64, error) {
	return 0, nil
}
func (s *stubBackend) CopyFrom(context.Context, backend.Sandbox, string, string) (int64, error) {
	return 0, nil
}
func (s *stubBackend) Shell(context.Context, backend.Sandbox, backend.TTY) error { return nil }
func (s *stubBackend) Snapshot(context.Context, backend.Sandbox, string) (backend.Snapshot, error) {
	return backend.Snapshot{Provider: s.name}, nil
}
func (s *stubBackend) Resume(context.Context, backend.Snapshot, backend.SandboxSpec) (backend.Sandbox, error) {
	return backend.Sandbox{Provider: s.name}, nil
}
func (s *stubBackend) Terminate(context.Context, backend.Sandbox) error { return nil }

func TestRegistryRegisterAndGet(t *testing.T) {
	cases := []struct {
		name    string
		lookup  string
		wantErr bool
	}{
		{"registered backend resolves", "aws", false},
		{"unknown backend errors", "ibm-cloud", true},
		{"empty lookup errors", "", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := backend.NewRegistry()
			r.Register(&stubBackend{name: "aws"})
			got, err := r.Get(tc.lookup)
			if tc.wantErr {
				assert.Error(t, err)
				assert.Nil(t, got)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, "aws", got.Name())
		})
	}
}

func TestRegistryNames(t *testing.T) {
	r := backend.NewRegistry()
	r.Register(&stubBackend{name: "aws"})
	r.Register(&stubBackend{name: "fake"})
	names := r.Names()
	assert.ElementsMatch(t, []string{"aws", "fake"}, names)
}

func TestErrNotSupported(t *testing.T) {
	// Should be wrappable / comparable by callers.
	wrapped := errAs(backend.ErrNotSupported)
	assert.True(t, errors.Is(wrapped, backend.ErrNotSupported))
}

func errAs(e error) error { return e }
