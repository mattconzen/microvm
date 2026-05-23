# microvm

A CLI for provisioning and using AWS Bedrock AgentCore microVMs as remote
sandboxes. Create a sandbox, exec commands in it, copy files, snapshot/resume,
terminate.

```
microvm create my-sbx
microvm exec my-sbx -- uname -a
microvm cp ./localfile my-sbx:/tmp/x
microvm snapshot my-sbx snap1
microvm resume snap1 my-sbx-2
microvm terminate my-sbx
```

## Architecture

```
┌────────────┐   InvokeAgentRuntime    ┌─────────────────────┐
│ microvm CLI│ ──────────────────────► │ AgentCore microVM   │
│  (Go)      │      JSON envelopes     │  ┌───────────────┐  │
│            │ ◄────────────────────── │  │ shellagent.py │  │
└────────────┘   responses / streams   │  │  (PTY, exec,  │  │
                                       │  │   put, get)   │  │
                                       │  └───────────────┘  │
                                       └─────────────────────┘
```

- **CLI** speaks to AgentCore's `InvokeAgentRuntime` HTTPS API, sends a JSON
  envelope describing the op (`exec`, `put`, `get`, `snapshot`, `resume`,
  `terminate`).
- **shellagent** is a tiny Python container (in `shellagent/`) that runs
  inside the microVM, accepts those envelopes via the `bedrock-agentcore`
  SDK, and dispatches to handlers that fork a PTY, run subprocesses, or
  read/write files.
- **State** lives in a local bbolt DB at `~/.microvm/sandboxes.db`. The CLI
  is the source of truth for sandbox name → AgentCore session ID mappings.

## Prerequisites

| Tool | Why |
|---|---|
| Go 1.22+ | Build the CLI |
| `aws` CLI v2 | Provisioning + auth |
| `podman` *or* `docker` (with buildx) | Build the ARM64 shellagent image. On Fedora, `podman` is already installed; on macOS/Ubuntu, Docker Desktop or the `docker-buildx-plugin` package. |
| `jq` | Used by `scripts/setup.sh` |
| An AWS account | Bedrock AgentCore access (preview in some regions; check availability) |

## Setup (interactive)

The fastest path is the interactive setup script. It walks you through
every AWS resource the CLI needs, prints each command before running it,
and asks for confirmation.

```
./scripts/setup.sh
```

What it provisions:

1. **ECR repository** to hold the shellagent image
2. **shellagent container** built for `linux/arm64` and pushed to ECR
3. **IAM execution role** assumed by AgentCore when running your container
   (trust policy for `bedrock-agentcore.amazonaws.com`, CloudWatch Logs
   permissions)
4. **AgentCore runtime** pointing at the pushed image digest
5. **CLI binding** via `microvm login`, writing region + runtime ARN to
   `~/.microvm/config.yaml`

If you re-run the script, it detects existing resources and skips the
create step (idempotent).

## Setup (manual)

If you'd rather provision by hand, the commands the script runs are:

```bash
ACCOUNT_ID=$(aws sts get-caller-identity --query Account --output text)
REGION=us-east-1
REPO=microvm-shell

# 1. ECR repo
aws ecr create-repository --repository-name $REPO --region $REGION

# 2. Build + push (ARM64 is mandatory)
# Pick whichever container tool you have. Both work; podman is the default on Fedora.
TOOL=${TOOL:-podman}  # or 'docker'
aws ecr get-login-password --region $REGION | \
  $TOOL login --username AWS --password-stdin $ACCOUNT_ID.dkr.ecr.$REGION.amazonaws.com

if [ "$TOOL" = "podman" ]; then
  podman build --platform linux/arm64 \
    -t $ACCOUNT_ID.dkr.ecr.$REGION.amazonaws.com/$REPO:latest ./shellagent
  podman push $ACCOUNT_ID.dkr.ecr.$REGION.amazonaws.com/$REPO:latest
else
  docker buildx build --platform linux/arm64 --provenance=false \
    -t $ACCOUNT_ID.dkr.ecr.$REGION.amazonaws.com/$REPO:latest \
    ./shellagent --push
fi

# 3. IAM role
aws iam create-role --role-name microvm-shellagent-exec \
  --assume-role-policy-document '{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Principal":{"Service":"bedrock-agentcore.amazonaws.com"},"Action":"sts:AssumeRole"}]}'
aws iam attach-role-policy --role-name microvm-shellagent-exec \
  --policy-arn arn:aws:iam::aws:policy/CloudWatchLogsFullAccess

# 4. AgentCore runtime
DIGEST=$(aws ecr describe-images --repository-name $REPO --image-ids imageTag=latest \
  --region $REGION --query 'imageDetails[0].imageDigest' --output text)
aws bedrock-agentcore-control create-agent-runtime \
  --agent-runtime-name microvm-shell \
  --agent-runtime-artifact "containerConfiguration={containerUri=$ACCOUNT_ID.dkr.ecr.$REGION.amazonaws.com/$REPO@$DIGEST}" \
  --network-configuration networkMode=PUBLIC \
  --role-arn arn:aws:iam::$ACCOUNT_ID:role/microvm-shellagent-exec \
  --region $REGION

# 5. CLI login
RUNTIME_ARN=$(aws bedrock-agentcore-control get-agent-runtime \
  --agent-runtime-name microvm-shell --region $REGION \
  --query 'agentRuntimeArn' --output text)
go run . login --region $REGION --runtime-arn $RUNTIME_ARN --image-digest $DIGEST
```

## IAM permissions for the calling user/role

The principal that runs `microvm` (not the runtime's execution role) needs:

```json
{
  "Version": "2012-10-17",
  "Statement": [
    {
      "Effect": "Allow",
      "Action": [
        "bedrock-agentcore:InvokeAgentRuntime",
        "bedrock-agentcore:InvokeAgentRuntimeWithWebSocketStream",
        "bedrock-agentcore-control:CreateAgentRuntime",
        "bedrock-agentcore-control:GetAgentRuntime",
        "bedrock-agentcore-control:UpdateAgentRuntime"
      ],
      "Resource": "*"
    },
    {
      "Effect": "Allow",
      "Action": [
        "ecr:GetAuthorizationToken",
        "ecr:CreateRepository",
        "ecr:BatchCheckLayerAvailability",
        "ecr:PutImage",
        "ecr:InitiateLayerUpload",
        "ecr:UploadLayerPart",
        "ecr:CompleteLayerUpload",
        "ecr:DescribeRepositories",
        "ecr:DescribeImages"
      ],
      "Resource": "*"
    },
    {
      "Effect": "Allow",
      "Action": [
        "iam:CreateRole",
        "iam:GetRole",
        "iam:AttachRolePolicy",
        "iam:PassRole"
      ],
      "Resource": "arn:aws:iam::*:role/microvm-*"
    }
  ]
}
```

Tighten resource ARNs once you know your account + region.

## Configuration

Config lives at `~/.microvm/config.yaml` (override with `MICROVM_HOME`):

```yaml
default_provider: aws
aws:
  region: us-east-1
  agent_runtime_arn: arn:aws:bedrock-agentcore:us-east-1:123456789012:runtime/microvm-shell
  ecr_image: 123456789012.dkr.ecr.us-east-1.amazonaws.com/microvm-shell
  ecr_image_digest: sha256:abc123...
```

State (the bbolt DB tracking your sandboxes) lives at `~/.microvm/sandboxes.db`.

## Commands

| Command | What it does |
|---|---|
| `microvm login` | Validate creds, persist runtime ARN to config |
| `microvm create <name>` | Open a new sandbox (AgentCore session) under `<name>` |
| `microvm list` | List local sandboxes and their session IDs |
| `microvm get <name>` | Show details for one sandbox |
| `microvm exec <name> -- <cmd...>` | Run a command in the sandbox |
| `microvm cp <src> <dst>` | Copy files. Source or dest can be `<name>:/path` |
| `microvm shell <name>` | Interactive PTY (uses AgentCore WebSocket — see Limitations) |
| `microvm snapshot <name> <snap-name>` | Save the session under a snapshot name |
| `microvm resume <snap-name> <new-name>` | Re-open a snapshot as a new sandbox |
| `microvm terminate <name>` | Close the session and remove from state |

## Limitations / gotchas

- **ARM64 only.** AgentCore microVMs are ARM. Pushing an amd64 image fails
  to start with no obvious error.
- **Cold start ~5-15s** on the first invocation of a session. Warm calls
  within the same session are fast.
- **AgentCore is not in every region.** Check
  [the AWS regional services list](https://docs.aws.amazon.com/general/latest/gr/bedrock-agentcore.html)
  before picking one.
- **Costs.** AgentCore charges per invocation + compute time. Idle runtimes
  don't bill, but active sessions do.
- **`microvm shell` is being rewritten** onto AgentCore's WebSocket
  endpoint (`InvokeAgentRuntimeWithWebSocketStream`). The previous SSE-only
  implementation couldn't send stdin. See task #6.

## Development

```bash
# Unit + integration tests (no AWS calls)
go test ./...

# Python shellagent tests
cd shellagent && python -m pytest -v

# CI runs both via .github/workflows/{go-lint,shellagent-ci}.yml
```

The CLI is fully testable without AWS: `cli/cli_e2e_test.go` drives the
whole CLI through a `fakeBackend` against a real bbolt store.
