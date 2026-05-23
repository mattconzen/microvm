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

	"github.com/mattconzen/monorepo/apps/microvm/backend"
	awsbackend "github.com/mattconzen/monorepo/apps/microvm/backend/aws"
	"github.com/mattconzen/monorepo/apps/microvm/config"
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
	b, _ := newTestBackend(t, &fakeInvoker{respond: func(_ []byte) []byte { return nil }})
	snap, err := b.Snapshot(context.Background(), backend.Sandbox{ID: "mvm_abc", SessionID: "sess-1"}, "demo")
	require.NoError(t, err)
	assert.Equal(t, "alias", snap.Kind)
	assert.Equal(t, "sess-1", snap.TargetSessionID)
	assert.Equal(t, "mvm_abc", snap.SandboxID)
	assert.Equal(t, "demo", snap.Name)
}

func TestResumeFromAliasReusesSession(t *testing.T) {
	b, _ := newTestBackend(t, &fakeInvoker{respond: func(_ []byte) []byte { return nil }})
	sb, err := b.Resume(context.Background(),
		backend.Snapshot{Kind: "alias", TargetSessionID: "sess-1", Provider: "aws"},
		backend.SandboxSpec{Name: "resumed"},
	)
	require.NoError(t, err)
	assert.Equal(t, "sess-1", sb.SessionID)
	assert.Equal(t, "resumed", sb.Name)
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
}
