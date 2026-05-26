package aws

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	awssdk "github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/bedrockagentcore"
	"github.com/aws/aws-sdk-go-v2/service/bedrockagentcore/types"
	"github.com/aws/aws-sdk-go-v2/service/bedrockagentcorecontrol"
	"github.com/aws/aws-sdk-go-v2/service/sts"

	"github.com/mattconzen/microvm/backend"
	"github.com/mattconzen/microvm/config"
	"github.com/mattconzen/microvm/obs"
	"github.com/mattconzen/microvm/state"
)

// Invoker abstracts the AgentCore data-plane client so tests can fake it.
type Invoker interface {
	InvokeAgentRuntime(ctx context.Context, params *bedrockagentcore.InvokeAgentRuntimeInput, optFns ...func(*bedrockagentcore.Options)) (*bedrockagentcore.InvokeAgentRuntimeOutput, error)
}

// Controller abstracts the AgentCore control-plane client.
type Controller interface {
	GetAgentRuntime(ctx context.Context, params *bedrockagentcorecontrol.GetAgentRuntimeInput, optFns ...func(*bedrockagentcorecontrol.Options)) (*bedrockagentcorecontrol.GetAgentRuntimeOutput, error)
}

// IdentityResolver abstracts STS for `microvm login`.
type IdentityResolver interface {
	GetCallerIdentity(ctx context.Context, params *sts.GetCallerIdentityInput, optFns ...func(*sts.Options)) (*sts.GetCallerIdentityOutput, error)
}

type Backend struct {
	cfg      *config.Config
	invoker  Invoker
	control  Controller
	identity IdentityResolver
	creds    CredentialsProvider // for SigV4-signed WebSocket handshakes (shell)
	region   string              // resolved AWS region for SigV4 signing
	store    *state.Store        // optional; set by WithStore for runtime-record persistence
	now      func() time.Time
}

// WithStore wires a state.Store into the backend so Login can persist a
// Runtime record. Returns the backend for chaining. Without a store, Login
// still works — the mode is recorded in config — but per-sandbox lookups have
// to read config instead of state.
func (b *Backend) WithStore(s *state.Store) *Backend {
	b.store = s
	return b
}

func New(cfg *config.Config, invoker Invoker, control Controller, identity IdentityResolver) *Backend {
	return &Backend{
		cfg:      cfg,
		invoker:  invoker,
		control:  control,
		identity: identity,
		now:      time.Now,
	}
}

// FromConfig builds a Backend using the real AWS SDK clients.
func FromConfig(ctx context.Context, cfg *config.Config) (*Backend, error) {
	opts := []func(*awsconfig.LoadOptions) error{}
	if cfg.AWS.Region != "" {
		opts = append(opts, awsconfig.WithRegion(cfg.AWS.Region))
	}
	awscfg, err := awsconfig.LoadDefaultConfig(ctx, opts...)
	if err != nil {
		return nil, fmt.Errorf("load aws config: %w", err)
	}
	b := New(
		cfg,
		bedrockagentcore.NewFromConfig(awscfg),
		bedrockagentcorecontrol.NewFromConfig(awscfg),
		sts.NewFromConfig(awscfg),
	)
	b.creds = awscfg.Credentials
	b.region = awscfg.Region
	return b, nil
}

func (b *Backend) Name() string { return "aws" }

func (b *Backend) Login(ctx context.Context, opts backend.LoginOpts) (err error) {
	t := obs.Time(ctx, obs.MetricLogin, "provider:aws", fmt.Sprintf("bootstrap:%v", opts.RuntimeArn != ""))
	defer t.Done(&err)

	// 1. Validate creds via STS.
	apiT := obs.Time(ctx, obs.MetricAPICall, "provider:aws", "op:GetCallerIdentity")
	out, err := b.identity.GetCallerIdentity(ctx, &sts.GetCallerIdentityInput{})
	apiT.Done(&err)
	if err != nil {
		return fmt.Errorf("sts get-caller-identity: %w", err)
	}
	obs.L(ctx).Info("login.identity", "account", awssdk.ToString(out.Account), "arn", awssdk.ToString(out.Arn))

	// 2. Either trust the runtime ARN the user supplied, or verify the one in config.
	arn := opts.RuntimeArn
	if arn == "" {
		arn = b.cfg.AWS.AgentRuntimeArn
	}
	if arn == "" {
		return errors.New("no agent runtime ARN configured.\n" +
			"  Bootstrap the shell-agent runtime once with:\n" +
			"    1) docker build -t microvm-shellagent apps/microvm/shellagent\n" +
			"    2) push it to ECR repo `microvm-shellagent`\n" +
			"    3) aws bedrock-agentcore-control create-agent-runtime --agent-runtime-name microvm-shell --agent-runtime-artifact ... --network-configuration NetworkMode=PUBLIC --role-arn <role>\n" +
			"    4) microvm login --runtime-arn <returned-arn>")
	}

	// Snapshot-mode dispatch. Empty mode resolves to "none" (today's
	// alias-only behavior). s3 requires --snapshot-bucket; efs/tiered are
	// stubbed for PR2/PR3 and fail fast with a useful pointer.
	prov, err := ProvisionerFor(opts.SnapshotMode, opts)
	if err != nil {
		return err
	}
	if err := prov.ValidateLoginOpts(opts); err != nil {
		return err
	}

	b.cfg.AWS.AgentRuntimeArn = arn
	if opts.ImageDigest != "" {
		b.cfg.AWS.ECRImageDigest = opts.ImageDigest
	}
	if opts.Region != "" {
		b.cfg.AWS.Region = opts.Region
	}
	b.cfg.AWS.SnapshotMode = prov.Mode()
	b.cfg.AWS.SnapshotBucket = opts.SnapshotBucket
	if err := config.Save(b.cfg); err != nil {
		return fmt.Errorf("save config: %w", err)
	}
	obs.L(ctx).Info("login.saved",
		"agent_runtime_arn", arn,
		"region", b.cfg.AWS.Region,
		"snapshot_mode", b.cfg.AWS.SnapshotMode,
	)

	// Persist a Runtime record so per-sandbox commands can resolve mode
	// without re-reading config. Best-effort: a store-less Backend (used by
	// some unit tests) still completes Login successfully.
	if b.store != nil {
		if err := b.store.PutRuntime(state.Runtime{
			Arn:            arn,
			Region:         b.cfg.AWS.Region,
			SnapshotMode:   b.cfg.AWS.SnapshotMode,
			SnapshotBucket: b.cfg.AWS.SnapshotBucket,
			ImageDigest:    b.cfg.AWS.ECRImageDigest,
			UpdatedAt:      b.now(),
		}); err != nil {
			return fmt.Errorf("persist runtime record: %w", err)
		}
	}
	return nil
}

func (b *Backend) Create(ctx context.Context, spec backend.SandboxSpec) (sb backend.Sandbox, err error) {
	t := obs.Time(ctx, obs.MetricCreate, "provider:aws")
	defer t.Done(&err)

	if b.cfg.AWS.AgentRuntimeArn == "" {
		return sb, errors.New("no agent runtime ARN configured — run `microvm login --runtime-arn <arn>` first")
	}
	if spec.Image != "" {
		obs.L(ctx).Warn("aws.image_ignored",
			"hint", "AgentCore runtime IS the image; --image is recorded but not used. Re-run `microvm login --rebuild` to change images.",
			"requested_image", spec.Image,
		)
	}
	// We mint our own IDs. The actual microVM is provisioned lazily on first Exec.
	return backend.Sandbox{
		Provider:  "aws",
		Image:     spec.Image,
		Name:      spec.Name,
		CPUs:      spec.CPUs,
		MemoryMB:  spec.MemoryMB,
		Mode:      normalizeMode(b.cfg.AWS.SnapshotMode),
		CreatedAt: b.now(),
	}, nil
}

func (b *Backend) Get(ctx context.Context, sb backend.Sandbox) (backend.Sandbox, error) {
	// AgentCore doesn't expose a session-inspection API for arbitrary session IDs;
	// the local state row is authoritative.
	return sb, nil
}

func (b *Backend) Exec(ctx context.Context, sb backend.Sandbox, cmd []string, io_ backend.ExecIO) (exitCode int, err error) {
	t := obs.Time(ctx, obs.MetricExec, "provider:aws", fmt.Sprintf("tty:%v", io_.TTY))
	defer t.Done(&err)

	req, err := ExecRequest(cmd)
	if err != nil {
		return -1, err
	}
	body, err := b.invoke(ctx, sb, req)
	if err != nil {
		return -1, err
	}
	var resp ExecResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		return -1, fmt.Errorf("decode exec response: %w (body: %q)", err, truncate(string(body), 512))
	}
	if resp.Error != "" {
		err = errors.New(resp.Error)
	}
	if io_.Stdout != nil {
		_, _ = io_.Stdout.Write([]byte(resp.Stdout))
	}
	if io_.Stderr != nil {
		_, _ = io_.Stderr.Write([]byte(resp.Stderr))
	}
	return resp.Exit, err
}

func (b *Backend) CopyTo(ctx context.Context, sb backend.Sandbox, localPath, remotePath string) (n int64, err error) {
	t := obs.Time(ctx, obs.MetricCp, "provider:aws", "direction:to")
	defer t.Done(&err)

	data, err := os.ReadFile(localPath)
	if err != nil {
		return 0, fmt.Errorf("read %s: %w", localPath, err)
	}
	req, err := PutRequest(remotePath, data)
	if err != nil {
		return 0, err
	}
	body, err := b.invoke(ctx, sb, req)
	if err != nil {
		return 0, err
	}
	var resp PutResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		return 0, fmt.Errorf("decode put response: %w", err)
	}
	if resp.Error != "" {
		return 0, errors.New(resp.Error)
	}
	_ = obs.M().Histogram(obs.MetricCpBytes, float64(len(data)), []string{"provider:aws", "direction:to"}, 1)
	return int64(len(data)), nil
}

func (b *Backend) CopyFrom(ctx context.Context, sb backend.Sandbox, remotePath, localPath string) (n int64, err error) {
	t := obs.Time(ctx, obs.MetricCp, "provider:aws", "direction:from")
	defer t.Done(&err)

	req, err := GetRequest(remotePath)
	if err != nil {
		return 0, err
	}
	body, err := b.invoke(ctx, sb, req)
	if err != nil {
		return 0, err
	}
	var resp GetResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		return 0, fmt.Errorf("decode get response: %w", err)
	}
	if resp.Error != "" {
		return 0, errors.New(resp.Error)
	}
	data, err := DecodeB64(resp.B64)
	if err != nil {
		return 0, fmt.Errorf("decode b64: %w", err)
	}
	if err := os.WriteFile(localPath, data, 0o644); err != nil {
		return 0, fmt.Errorf("write %s: %w", localPath, err)
	}
	_ = obs.M().Histogram(obs.MetricCpBytes, float64(len(data)), []string{"provider:aws", "direction:from"}, 1)
	return int64(len(data)), nil
}

// Shell opens a full-duplex PTY session over the AgentCore WebSocket
// invocation endpoint. IAM note: the caller's role needs
// bedrock-agentcore:InvokeAgentRuntimeWithWebSocketStream in addition to the
// usual InvokeAgentRuntime permission.
func (b *Backend) Shell(ctx context.Context, sb backend.Sandbox, tty backend.TTY) (err error) {
	t := obs.Time(ctx, obs.MetricShellSession, "provider:aws")
	defer t.Done(&err)

	if b.cfg.AWS.AgentRuntimeArn == "" {
		return errors.New("aws backend: agent runtime ARN not configured")
	}
	if b.creds == nil {
		return errors.New("aws backend: no credentials provider configured for shell")
	}
	region := b.region
	if region == "" {
		region = b.cfg.AWS.Region
	}
	apiT := obs.Time(ctx, obs.MetricAPICall, "provider:aws", "op:InvokeAgentRuntimeWithWebSocketStream")
	creds, err := b.creds.Retrieve(ctx)
	if err != nil {
		apiT.Done(&err)
		return fmt.Errorf("retrieve aws credentials: %w", err)
	}
	err = runShell(ctx, shellWsDial, region, b.cfg.AWS.AgentRuntimeArn, sb.SessionID, creds, b.now(), nil, tty.In, tty.Out, tty.Cols, tty.Rows)
	apiT.Done(&err)
	return err
}

func (b *Backend) Snapshot(ctx context.Context, sb backend.Sandbox, spec backend.SnapshotSpec) (snap backend.Snapshot, err error) {
	mode := normalizeMode(b.cfg.AWS.SnapshotMode)
	t := obs.Time(ctx, obs.MetricSnapshot, "provider:aws", "mode:"+mode)
	defer t.Done(&err)

	if mode == "none" {
		// AgentCore has no checkpoint API in alias mode. The "snapshot" is just
		// a sticky-session pointer; eviction loses everything. Warn loudly so
		// users don't mistake the success for durability.
		obs.L(ctx).Warn("aws.snapshot.alias",
			"note", "AWS snapshots are session aliases, not durable filesystem checkpoints. State persists only as long as the sticky session does.",
			"session_id", sb.SessionID,
		)
	}

	req, err := SnapshotRequest(spec, mode)
	if err != nil {
		return snap, err
	}
	body, err := b.invoke(ctx, sb, req)
	if err != nil {
		return snap, err
	}
	var resp SnapshotResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		return snap, fmt.Errorf("decode snapshot response: %w (body: %q)", err, truncate(string(body), 512))
	}
	if resp.Error != "" {
		return snap, errors.New(resp.Error)
	}
	target := resp.Alias
	if target == "" {
		target = sb.SessionID
	}
	// Kind preserved for backward-compat with pre-mode records; "alias" stays
	// the legacy spelling for mode=none, and we use the mode name otherwise.
	kind := "alias"
	if mode != "none" {
		kind = mode
	}
	return backend.Snapshot{
		SandboxID:       sb.ID,
		Provider:        "aws",
		TargetSessionID: target,
		Kind:            kind,
		Mode:            mode,
		Locator:         resp.Locator,
		Name:            spec.Name,
		CreatedAt:       b.now(),
	}, nil
}

func (b *Backend) Resume(ctx context.Context, snap backend.Snapshot, spec backend.SandboxSpec) (sb backend.Sandbox, err error) {
	snapMode := normalizeMode(snap.Mode)
	runtimeMode := normalizeMode(b.cfg.AWS.SnapshotMode)
	t := obs.Time(ctx, obs.MetricResume, "provider:aws", "mode:"+snapMode)
	defer t.Done(&err)

	if snapMode != runtimeMode {
		return sb, fmt.Errorf(
			"snapshot mode %q does not match active runtime mode %q. "+
				"Re-register with `microvm login --snapshot-mode %s ...` to resume this snapshot.",
			snapMode, runtimeMode, snapMode,
		)
	}

	req, err := ResumeRequest(snap.TargetSessionID, snap.Locator, snapMode)
	if err != nil {
		return sb, err
	}
	body, err := b.invoke(ctx, backend.Sandbox{SessionID: snap.TargetSessionID}, req)
	if err != nil {
		return sb, err
	}
	var resp ResumeResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		return sb, fmt.Errorf("decode resume response: %w (body: %q)", err, truncate(string(body), 512))
	}
	if resp.Error != "" {
		return sb, errors.New(resp.Error)
	}
	sessionID := resp.Alias
	if sessionID == "" {
		sessionID = snap.TargetSessionID
	}
	return backend.Sandbox{
		Provider:  "aws",
		SessionID: sessionID,
		Name:      spec.Name,
		Mode:      snapMode,
		CreatedAt: b.now(),
	}, nil
}

func (b *Backend) Terminate(ctx context.Context, sb backend.Sandbox) (err error) {
	t := obs.Time(ctx, obs.MetricTerminate, "provider:aws")
	defer t.Done(&err)

	req, err := TerminateRequest()
	if err != nil {
		return err
	}
	body, err := b.invoke(ctx, sb, req)
	if err != nil {
		var notFound *types.ResourceNotFoundException
		if errors.As(err, &notFound) {
			err = nil
			return nil
		}
		return err
	}
	var resp TerminateResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		return fmt.Errorf("decode terminate response: %w (body: %q)", err, truncate(string(body), 512))
	}
	if resp.Error != "" {
		return errors.New(resp.Error)
	}
	return nil
}

// invoke is the shared one-shot JSON round-trip used by exec/put/get.
func (b *Backend) invoke(ctx context.Context, sb backend.Sandbox, payload []byte) ([]byte, error) {
	if b.cfg.AWS.AgentRuntimeArn == "" {
		return nil, errors.New("aws backend: agent runtime ARN not configured")
	}
	apiT := obs.Time(ctx, obs.MetricAPICall, "provider:aws", "op:InvokeAgentRuntime")
	out, err := b.invoker.InvokeAgentRuntime(ctx, &bedrockagentcore.InvokeAgentRuntimeInput{
		AgentRuntimeArn:  awssdk.String(b.cfg.AWS.AgentRuntimeArn),
		RuntimeSessionId: awssdk.String(sb.SessionID),
		ContentType:      awssdk.String("application/json"),
		Accept:           awssdk.String("application/json"),
		Payload:          payload,
	})
	apiT.Done(&err)
	if err != nil {
		return nil, fmt.Errorf("invoke agent runtime: %w", err)
	}
	defer out.Response.Close()
	body, err := io.ReadAll(out.Response)
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}
	if out.StatusCode != nil && *out.StatusCode >= 400 {
		return body, fmt.Errorf("agent runtime returned status %d", *out.StatusCode)
	}
	return body, nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

// normalizeMode collapses the empty/legacy spelling to "none" so the mode
// comparison and dispatch logic only ever sees the canonical names.
func normalizeMode(mode string) string {
	if mode == "" {
		return "none"
	}
	return mode
}
