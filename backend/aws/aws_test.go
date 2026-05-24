package aws_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"testing"

	awssdk "github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/bedrockagentcore"
	"github.com/aws/aws-sdk-go-v2/service/bedrockagentcore/types"
	"github.com/aws/aws-sdk-go-v2/service/bedrockagentcorecontrol"
	"github.com/aws/aws-sdk-go-v2/service/sts"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mattconzen/microvm/backend"
	awsbackend "github.com/mattconzen/microvm/backend/aws"
	"github.com/mattconzen/microvm/config"
)

type fakeInvoker struct {
	gotInput *bedrockagentcore.InvokeAgentRuntimeInput
	respond  func([]byte) []byte
}

func (f *fakeInvoker) InvokeAgentRuntime(_ context.Context, in *bedrockagentcore.InvokeAgentRuntimeInput, _ ...func(*bedrockagentcore.Options)) (*bedrockagentcore.InvokeAgentRuntimeOutput, error) {
	f.gotInput = in
	out := f.respond(in.Payload)
	return &bedrockagentcore.InvokeAgentRuntimeOutput{
		Response:    io.NopCloser(bytes.NewReader(out)),
		ContentType: awssdk.String("application/json"),
		StatusCode:  awssdk.Int32(200),
	}, nil
}

type fakeControl struct{}

func (fakeControl) GetAgentRuntime(_ context.Context, in *bedrockagentcorecontrol.GetAgentRuntimeInput, _ ...func(*bedrockagentcorecontrol.Options)) (*bedrockagentcorecontrol.GetAgentRuntimeOutput, error) {
	return &bedrockagentcorecontrol.GetAgentRuntimeOutput{}, nil
}

type fakeIdentity struct{}

func (fakeIdentity) GetCallerIdentity(_ context.Context, _ *sts.GetCallerIdentityInput, _ ...func(*sts.Options)) (*sts.GetCallerIdentityOutput, error) {
	return &sts.GetCallerIdentityOutput{
		Account: awssdk.String("123456789012"),
		Arn:     awssdk.String("arn:aws:iam::123456789012:user/test"),
		UserId:  awssdk.String("AIDATESTUSERID"),
	}, nil
}

func newTestBackend(t *testing.T, invoker awsbackend.Invoker) (*awsbackend.Backend, *config.Config) {
	t.Helper()
	t.Setenv("MICROVM_HOME", t.TempDir())
	cfg := &config.Config{
		DefaultProvider: "aws",
		AWS: config.AWSConfig{
			Region:          "us-east-1",
			AgentRuntimeArn: "arn:aws:bedrock-agentcore:us-east-1:123:runtime/microvm-shell",
		},
	}
	return awsbackend.New(cfg, invoker, fakeControl{}, fakeIdentity{}), cfg
}

func TestExecHappyPath(t *testing.T) {
	fi := &fakeInvoker{respond: func(_ []byte) []byte {
		b, _ := json.Marshal(awsbackend.ExecResponse{Stdout: "hi\n", Exit: 0})
		return b
	}}
	b, _ := newTestBackend(t, fi)

	var stdout, stderr bytes.Buffer
	code, err := b.Exec(context.Background(), backend.Sandbox{SessionID: "sess-1"}, []string{"echo", "hi"},
		backend.ExecIO{Stdout: &stdout, Stderr: &stderr})
	require.NoError(t, err)
	assert.Equal(t, 0, code)
	assert.Equal(t, "hi\n", stdout.String())
	require.NotNil(t, fi.gotInput)
	assert.Equal(t, "sess-1", awssdk.ToString(fi.gotInput.RuntimeSessionId))
}

func TestExecNonZeroExit(t *testing.T) {
	fi := &fakeInvoker{respond: func(_ []byte) []byte {
		b, _ := json.Marshal(awsbackend.ExecResponse{Stderr: "boom\n", Exit: 7})
		return b
	}}
	b, _ := newTestBackend(t, fi)
	var stderr bytes.Buffer
	code, err := b.Exec(context.Background(), backend.Sandbox{SessionID: "s"}, []string{"false"},
		backend.ExecIO{Stderr: &stderr})
	require.NoError(t, err)
	assert.Equal(t, 7, code)
	assert.Equal(t, "boom\n", stderr.String())
}

func TestCopyToReadsLocalAndSends(t *testing.T) {
	var captured awsbackend.Request
	fi := &fakeInvoker{respond: func(req []byte) []byte {
		_ = json.Unmarshal(req, &captured)
		b, _ := json.Marshal(awsbackend.PutResponse{OK: true, Bytes: 5})
		return b
	}}
	b, _ := newTestBackend(t, fi)

	local := filepath.Join(t.TempDir(), "local.txt")
	require.NoError(t, os.WriteFile(local, []byte("hello"), 0o644))

	n, err := b.CopyTo(context.Background(), backend.Sandbox{SessionID: "s"}, local, "/tmp/hello")
	require.NoError(t, err)
	assert.EqualValues(t, 5, n)
	assert.Equal(t, awsbackend.OpPut, captured.Op)
	assert.Equal(t, "/tmp/hello", captured.Path)
	decoded, err := awsbackend.DecodeB64(captured.B64)
	require.NoError(t, err)
	assert.Equal(t, []byte("hello"), decoded)
}

func TestCopyFromWritesLocal(t *testing.T) {
	fi := &fakeInvoker{respond: func(_ []byte) []byte {
		b, _ := json.Marshal(awsbackend.GetResponse{B64: "aGVsbG8=", Bytes: 5})
		return b
	}}
	b, _ := newTestBackend(t, fi)
	local := filepath.Join(t.TempDir(), "out.txt")
	n, err := b.CopyFrom(context.Background(), backend.Sandbox{SessionID: "s"}, "/tmp/x", local)
	require.NoError(t, err)
	assert.EqualValues(t, 5, n)
	got, err := os.ReadFile(local)
	require.NoError(t, err)
	assert.Equal(t, []byte("hello"), got)
}

func TestSnapshotIsAlias(t *testing.T) {
	var captured awsbackend.Request
	fi := &fakeInvoker{respond: func(req []byte) []byte {
		_ = json.Unmarshal(req, &captured)
		b, _ := json.Marshal(awsbackend.SnapshotResponse{Alias: "sess-1", Name: "demo"})
		return b
	}}
	b, _ := newTestBackend(t, fi)
	snap, err := b.Snapshot(
		context.Background(),
		backend.Sandbox{ID: "mvm_abc", SessionID: "sess-1"},
		backend.SnapshotSpec{ID: "snp_1", Name: "demo"},
	)
	require.NoError(t, err)
	assert.Equal(t, awsbackend.OpSnapshot, captured.Op)
	assert.Equal(t, "demo", captured.Name)
	assert.Equal(t, "snp_1", captured.SnapID)
	assert.Equal(t, "none", captured.Mode)
	assert.Equal(t, "alias", snap.Kind)
	assert.Equal(t, "none", snap.Mode)
	assert.Empty(t, snap.Locator)
	assert.Equal(t, "sess-1", snap.TargetSessionID)
	assert.Equal(t, "mvm_abc", snap.SandboxID)
	assert.Equal(t, "demo", snap.Name)
	require.NotNil(t, fi.gotInput)
	assert.Equal(t, "sess-1", awssdk.ToString(fi.gotInput.RuntimeSessionId))
}

func TestSnapshotFallsBackToSessionIDWhenAliasEmpty(t *testing.T) {
	fi := &fakeInvoker{respond: func(_ []byte) []byte {
		b, _ := json.Marshal(awsbackend.SnapshotResponse{})
		return b
	}}
	b, _ := newTestBackend(t, fi)
	snap, err := b.Snapshot(
		context.Background(),
		backend.Sandbox{ID: "mvm_abc", SessionID: "sess-fallback"},
		backend.SnapshotSpec{ID: "snp_2", Name: "x"},
	)
	require.NoError(t, err)
	assert.Equal(t, "sess-fallback", snap.TargetSessionID)
}

func TestSnapshotReturnsAgentError(t *testing.T) {
	fi := &fakeInvoker{respond: func(_ []byte) []byte {
		b, _ := json.Marshal(awsbackend.SnapshotResponse{Error: "no can do"})
		return b
	}}
	b, _ := newTestBackend(t, fi)
	_, err := b.Snapshot(
		context.Background(),
		backend.Sandbox{ID: "mvm_abc", SessionID: "s"},
		backend.SnapshotSpec{ID: "snp_3", Name: "x"},
	)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no can do")
}

func TestSnapshotIncludesS3ModeAndLocator(t *testing.T) {
	var captured awsbackend.Request
	fi := &fakeInvoker{respond: func(req []byte) []byte {
		_ = json.Unmarshal(req, &captured)
		b, _ := json.Marshal(awsbackend.SnapshotResponse{
			Alias:   "sess-1",
			Locator: `{"s3_uri":"s3://my-bucket/microvm/snp_xyz.tar.gz"}`,
		})
		return b
	}}
	b, cfg := newTestBackend(t, fi)
	cfg.AWS.SnapshotMode = "s3"
	cfg.AWS.SnapshotBucket = "my-bucket"

	snap, err := b.Snapshot(
		context.Background(),
		backend.Sandbox{ID: "mvm_abc", SessionID: "sess-1"},
		backend.SnapshotSpec{ID: "snp_xyz", Name: "baseline"},
	)
	require.NoError(t, err)
	assert.Equal(t, "s3", captured.Mode)
	assert.Equal(t, "snp_xyz", captured.SnapID)
	assert.Equal(t, "s3", snap.Mode)
	assert.Equal(t, "s3", snap.Kind)
	assert.Contains(t, snap.Locator, "s3://my-bucket/")
}

func TestResumeFromAliasReusesSession(t *testing.T) {
	var captured awsbackend.Request
	fi := &fakeInvoker{respond: func(req []byte) []byte {
		_ = json.Unmarshal(req, &captured)
		b, _ := json.Marshal(awsbackend.ResumeResponse{Alias: "sess-1"})
		return b
	}}
	b, _ := newTestBackend(t, fi)
	sb, err := b.Resume(context.Background(),
		backend.Snapshot{Kind: "alias", TargetSessionID: "sess-1", Provider: "aws"},
		backend.SandboxSpec{Name: "resumed"},
	)
	require.NoError(t, err)
	assert.Equal(t, awsbackend.OpResume, captured.Op)
	assert.Equal(t, "sess-1", captured.Alias)
	assert.Equal(t, "sess-1", sb.SessionID)
	assert.Equal(t, "resumed", sb.Name)
	require.NotNil(t, fi.gotInput)
	assert.Equal(t, "sess-1", awssdk.ToString(fi.gotInput.RuntimeSessionId))
}

func TestResumeIncludesSandboxIDInEnvelope(t *testing.T) {
	var captured awsbackend.Request
	fi := &fakeInvoker{respond: func(req []byte) []byte {
		_ = json.Unmarshal(req, &captured)
		b, _ := json.Marshal(awsbackend.ResumeResponse{Alias: "sess-1"})
		return b
	}}
	b, _ := newTestBackend(t, fi)
	sb, err := b.Resume(context.Background(),
		backend.Snapshot{Kind: "alias", TargetSessionID: "sess-1", Provider: "aws"},
		backend.SandboxSpec{Name: "resumed", ID: "mvm_resumed"},
	)
	require.NoError(t, err)
	assert.Equal(t, awsbackend.OpResume, captured.Op)
	// The minted sandbox id must reach the shellagent via the envelope so
	// EFS resume can materialise into the right per-sandbox subdir.
	assert.Equal(t, "mvm_resumed", captured.SandboxID)
	// And the returned Sandbox should echo the id back so callers (e.g.
	// the CLI persisting state) can read it without re-minting.
	assert.Equal(t, "mvm_resumed", sb.ID)
}

func TestResumeRebindsToNewAlias(t *testing.T) {
	fi := &fakeInvoker{respond: func(_ []byte) []byte {
		b, _ := json.Marshal(awsbackend.ResumeResponse{Alias: "sess-2"})
		return b
	}}
	b, _ := newTestBackend(t, fi)
	sb, err := b.Resume(context.Background(),
		backend.Snapshot{Kind: "alias", TargetSessionID: "sess-1", Provider: "aws"},
		backend.SandboxSpec{Name: "resumed"},
	)
	require.NoError(t, err)
	assert.Equal(t, "sess-2", sb.SessionID)
}

func TestResumeRejectsModeMismatch(t *testing.T) {
	b, cfg := newTestBackend(t, &fakeInvoker{respond: func(_ []byte) []byte { return nil }})
	cfg.AWS.SnapshotMode = "s3"
	cfg.AWS.SnapshotBucket = "my-bucket"

	// Snapshot was taken in "none" mode but the active runtime is "s3".
	_, err := b.Resume(context.Background(),
		backend.Snapshot{Kind: "alias", Mode: "none", TargetSessionID: "sess-1", Provider: "aws"},
		backend.SandboxSpec{},
	)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "does not match active runtime mode")
	assert.Contains(t, err.Error(), "--snapshot-mode none")
}

func TestResumeLegacySnapshotInNoneMode(t *testing.T) {
	// Legacy records may have Mode="" (pre-mode). With the runtime also in
	// none mode, both normalize to "none" and resume succeeds.
	fi := &fakeInvoker{respond: func(_ []byte) []byte {
		b, _ := json.Marshal(awsbackend.ResumeResponse{Alias: "sess-1"})
		return b
	}}
	b, _ := newTestBackend(t, fi)
	sb, err := b.Resume(context.Background(),
		backend.Snapshot{Kind: "alias", TargetSessionID: "sess-1", Provider: "aws"},
		backend.SandboxSpec{Name: "resumed"},
	)
	require.NoError(t, err)
	assert.Equal(t, "sess-1", sb.SessionID)
	assert.Equal(t, "none", sb.Mode)
}

func TestTerminateSendsEnvelope(t *testing.T) {
	var captured awsbackend.Request
	fi := &fakeInvoker{respond: func(req []byte) []byte {
		_ = json.Unmarshal(req, &captured)
		b, _ := json.Marshal(awsbackend.TerminateResponse{OK: true})
		return b
	}}
	b, _ := newTestBackend(t, fi)

	err := b.Terminate(context.Background(), backend.Sandbox{ID: "mvm_x", SessionID: "sess-9"})
	require.NoError(t, err)
	assert.Equal(t, awsbackend.OpTerminate, captured.Op)
	require.NotNil(t, fi.gotInput)
	assert.Equal(t, "sess-9", awssdk.ToString(fi.gotInput.RuntimeSessionId))
}

type notFoundInvoker struct{}

func (notFoundInvoker) InvokeAgentRuntime(_ context.Context, _ *bedrockagentcore.InvokeAgentRuntimeInput, _ ...func(*bedrockagentcore.Options)) (*bedrockagentcore.InvokeAgentRuntimeOutput, error) {
	return nil, &types.ResourceNotFoundException{Message: awssdk.String("session gone")}
}

func TestTerminateIdempotentOnInvokeError(t *testing.T) {
	b, _ := newTestBackend(t, notFoundInvoker{})
	err := b.Terminate(context.Background(), backend.Sandbox{ID: "mvm_x", SessionID: "sess-gone"})
	require.NoError(t, err)
}

func TestLoginRequiresRuntimeArn(t *testing.T) {
	t.Setenv("MICROVM_HOME", t.TempDir())
	cfg := &config.Config{DefaultProvider: "aws"}
	b := awsbackend.New(cfg, &fakeInvoker{}, fakeControl{}, fakeIdentity{})
	err := b.Login(context.Background(), backend.LoginOpts{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "agent runtime ARN")
}

func TestLoginWithRuntimeArnPersists(t *testing.T) {
	t.Setenv("MICROVM_HOME", t.TempDir())
	cfg := &config.Config{DefaultProvider: "aws"}
	b := awsbackend.New(cfg, &fakeInvoker{}, fakeControl{}, fakeIdentity{})
	err := b.Login(context.Background(), backend.LoginOpts{
		Region:     "us-west-2",
		RuntimeArn: "arn:aws:bedrock-agentcore:us-west-2:123:runtime/microvm-shell",
	})
	require.NoError(t, err)
	assert.Equal(t, "us-west-2", cfg.AWS.Region)
	assert.Contains(t, cfg.AWS.AgentRuntimeArn, "microvm-shell")
	// Default snapshot mode normalizes to "none".
	assert.Equal(t, "none", cfg.AWS.SnapshotMode)
}

func TestLoginPersistsSnapshotMode(t *testing.T) {
	t.Setenv("MICROVM_HOME", t.TempDir())
	cfg := &config.Config{DefaultProvider: "aws"}
	b := awsbackend.New(cfg, &fakeInvoker{}, fakeControl{}, fakeIdentity{})
	err := b.Login(context.Background(), backend.LoginOpts{
		RuntimeArn:     "arn:aws:bedrock-agentcore:us-east-1:123:runtime/microvm-shell",
		SnapshotMode:   "s3",
		SnapshotBucket: "my-bucket",
	})
	require.NoError(t, err)
	assert.Equal(t, "s3", cfg.AWS.SnapshotMode)
	assert.Equal(t, "my-bucket", cfg.AWS.SnapshotBucket)
}

func TestLoginRejectsS3WithoutBucket(t *testing.T) {
	t.Setenv("MICROVM_HOME", t.TempDir())
	cfg := &config.Config{DefaultProvider: "aws"}
	b := awsbackend.New(cfg, &fakeInvoker{}, fakeControl{}, fakeIdentity{})
	err := b.Login(context.Background(), backend.LoginOpts{
		RuntimeArn:   "arn:aws:bedrock-agentcore:us-east-1:123:runtime/microvm-shell",
		SnapshotMode: "s3",
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "--snapshot-bucket")
}

func TestLoginPersistsEfsMode(t *testing.T) {
	t.Setenv("MICROVM_HOME", t.TempDir())
	cfg := &config.Config{DefaultProvider: "aws"}
	b := awsbackend.New(cfg, &fakeInvoker{}, fakeControl{}, fakeIdentity{})
	err := b.Login(context.Background(), backend.LoginOpts{
		RuntimeArn:        "arn:aws:bedrock-agentcore:us-east-1:123:runtime/microvm-shell",
		SnapshotMode:      "efs",
		EFSAccessPointArn: "arn:aws:elasticfilesystem:us-east-1:123:access-point/fsap-abc",
	})
	require.NoError(t, err)
	assert.Equal(t, "efs", cfg.AWS.SnapshotMode)
}

func TestLoginRejectsEfsWithoutAccessPoint(t *testing.T) {
	t.Setenv("MICROVM_HOME", t.TempDir())
	cfg := &config.Config{DefaultProvider: "aws"}
	b := awsbackend.New(cfg, &fakeInvoker{}, fakeControl{}, fakeIdentity{})
	err := b.Login(context.Background(), backend.LoginOpts{
		RuntimeArn:   "arn:aws:bedrock-agentcore:us-east-1:123:runtime/microvm-shell",
		SnapshotMode: "efs",
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "--efs-access-point-arn")
}

func TestLoginRejectsUnknownMode(t *testing.T) {
	t.Setenv("MICROVM_HOME", t.TempDir())
	cfg := &config.Config{DefaultProvider: "aws"}
	b := awsbackend.New(cfg, &fakeInvoker{}, fakeControl{}, fakeIdentity{})
	err := b.Login(context.Background(), backend.LoginOpts{
		RuntimeArn:   "arn:aws:bedrock-agentcore:us-east-1:123:runtime/microvm-shell",
		SnapshotMode: "bogus",
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unknown snapshot mode")
}
