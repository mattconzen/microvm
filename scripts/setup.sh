#!/usr/bin/env bash
#
# Interactive setup for the microvm CLI against real AWS Bedrock AgentCore.
#
# Walks you through:
#   1. Confirming AWS credentials and region
#   2. Creating an ECR repository for the shellagent image
#   3. Building+pushing the ARM64 shellagent image
#   4. Creating an IAM execution role for the runtime
#   5. Creating the AgentCore runtime
#   6. Binding the CLI via `microvm login`
#
# Each step shows the exact command, explains what it does, and asks before running.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
MICROVM_DIR="$(cd "${SCRIPT_DIR}/.." && pwd)"

readonly C_DIM=$'\033[2m'
readonly C_BOLD=$'\033[1m'
readonly C_GREEN=$'\033[32m'
readonly C_YELLOW=$'\033[33m'
readonly C_CYAN=$'\033[36m'
readonly C_RESET=$'\033[0m'

step() { printf "\n${C_BOLD}${C_CYAN}==> %s${C_RESET}\n" "$*"; }
info() { printf "${C_DIM}%s${C_RESET}\n" "$*"; }
warn() { printf "${C_YELLOW}!  %s${C_RESET}\n" "$*"; }
ok()   { printf "${C_GREEN}✓  %s${C_RESET}\n" "$*"; }

# Show a command and ask before running. Returns 0 if run, 1 if skipped.
confirm_run() {
  local explanation="$1"; shift
  printf "\n${C_DIM}%s${C_RESET}\n" "$explanation"
  printf "${C_BOLD}\$${C_RESET} "
  printf "%q " "$@"
  printf "\n"
  read -r -p "Run this? [Y/n/q] " ans
  case "${ans:-y}" in
    [Yy]*|"") "$@"; return 0 ;;
    [Qq]*) echo "Aborted by user."; exit 1 ;;
    *) warn "Skipped."; return 1 ;;
  esac
}

prompt() {
  local var="$1" question="$2" default="${3:-}"
  local val
  if [[ -n "$default" ]]; then
    read -r -p "$question [$default]: " val
    val="${val:-$default}"
  else
    read -r -p "$question: " val
  fi
  printf -v "$var" '%s' "$val"
}

require_cmd() {
  command -v "$1" >/dev/null 2>&1 || { warn "Missing required command: $1"; exit 1; }
}

# ---------------------------------------------------------------------------

cat <<'BANNER'

  microvm setup
  -------------
  This walks you through provisioning the AWS resources microvm needs:
  ECR repo, shellagent image, IAM role, AgentCore runtime, CLI config.

  Every step prints the command it's about to run and asks before running.
  You can answer 'n' to skip a step (useful for re-runs) or 'q' to quit.

BANNER

step "Checking prerequisites"
require_cmd aws
require_cmd jq

# Pick a container build tool that supports linux/arm64 cross-builds + push.
# Prefer podman (works out of the box on Fedora/RHEL with qemu-user-static).
# Fall back to docker buildx (macOS Docker Desktop, Ubuntu with buildx plugin).
if command -v podman >/dev/null 2>&1; then
  BUILD_TOOL="podman"
  ok "Using podman for ARM64 build/push."
elif command -v docker >/dev/null 2>&1 && docker buildx version >/dev/null 2>&1; then
  BUILD_TOOL="docker"
  ok "Using docker buildx for ARM64 build/push."
else
  warn "Need either 'podman' or 'docker' (with the buildx plugin) for linux/arm64 cross-builds."
  warn "On Fedora: 'sudo rpm-ostree install podman qemu-user-static && systemctl reboot' (rpm-ostree)"
  warn "         or 'sudo dnf install podman qemu-user-static' (dnf)."
  warn "On macOS / Ubuntu: install Docker Desktop or 'apt install docker-buildx-plugin'."
  exit 1
fi
ok "aws, jq, ${BUILD_TOOL} present"

step "AWS account + region"
info "Reads your default profile via 'aws sts get-caller-identity'."
ACCOUNT_ID="$(aws sts get-caller-identity --query Account --output text)"
ok "Account: ${ACCOUNT_ID}"
prompt REGION "AWS region (must support Bedrock AgentCore)" "us-east-1"

step "ECR repository for the shellagent image"
prompt REPO_NAME "ECR repository name" "microvm-shell"
ECR_URI="${ACCOUNT_ID}.dkr.ecr.${REGION}.amazonaws.com/${REPO_NAME}"
info "Will create ${ECR_URI} if it doesn't already exist."
if aws ecr describe-repositories --repository-names "${REPO_NAME}" --region "${REGION}" >/dev/null 2>&1; then
  ok "Repository ${REPO_NAME} already exists, skipping create."
else
  confirm_run "Creates an ECR repository to hold the shellagent container image." \
    aws ecr create-repository \
      --repository-name "${REPO_NAME}" \
      --region "${REGION}" \
      --image-scanning-configuration scanOnPush=true
fi

step "Build and push the shellagent image (ARM64)"
info "AgentCore microVMs are ARM64. Pushing an amd64 image will fail to start."
info "Uses '${BUILD_TOOL}' with --platform linux/arm64."
confirm_run "Logs ${BUILD_TOOL} in to your ECR registry." \
  bash -c "aws ecr get-login-password --region '${REGION}' | ${BUILD_TOOL} login --username AWS --password-stdin '${ACCOUNT_ID}.dkr.ecr.${REGION}.amazonaws.com'"

IMAGE_TAG="latest"
if [[ "${BUILD_TOOL}" == "podman" ]]; then
  confirm_run "Builds the shellagent image for linux/arm64 with podman. Takes a minute or two on first run (qemu emulation)." \
    podman build \
      --platform linux/arm64 \
      -t "${ECR_URI}:${IMAGE_TAG}" \
      "${MICROVM_DIR}/shellagent"
  confirm_run "Pushes the ARM64 image to ECR using the Docker v2s2 manifest format (ECR's preferred schema)." \
    podman push --format=v2s2 "${ECR_URI}:${IMAGE_TAG}"
else
  confirm_run "Builds the shellagent image for linux/arm64 with docker buildx and pushes to ECR. Takes a minute or two on first run." \
    docker buildx build \
      --platform linux/arm64 \
      --provenance=false \
      -t "${ECR_URI}:${IMAGE_TAG}" \
      "${MICROVM_DIR}/shellagent" \
      --push
fi

info "Resolving the image digest to pin the runtime against."
IMAGE_DIGEST="$(aws ecr describe-images \
  --repository-name "${REPO_NAME}" \
  --image-ids imageTag="${IMAGE_TAG}" \
  --region "${REGION}" \
  --query 'imageDetails[0].imageDigest' \
  --output text)"
ok "Digest: ${IMAGE_DIGEST}"

step "IAM execution role for the AgentCore runtime"
info "AgentCore assumes this role when it spins up your container."
info "It needs CloudWatch Logs perms; add more if your workload calls other AWS APIs."
prompt ROLE_NAME "IAM role name" "microvm-shellagent-exec"
ROLE_ARN="arn:aws:iam::${ACCOUNT_ID}:role/${ROLE_NAME}"

if aws iam get-role --role-name "${ROLE_NAME}" >/dev/null 2>&1; then
  ok "Role ${ROLE_NAME} already exists, skipping create."
else
  TRUST_DOC="$(mktemp)"
  cat >"${TRUST_DOC}" <<EOF
{
  "Version": "2012-10-17",
  "Statement": [{
    "Effect": "Allow",
    "Principal": { "Service": "bedrock-agentcore.amazonaws.com" },
    "Action": "sts:AssumeRole"
  }]
}
EOF
  confirm_run "Creates the role with a trust policy for bedrock-agentcore.amazonaws.com." \
    aws iam create-role \
      --role-name "${ROLE_NAME}" \
      --assume-role-policy-document "file://${TRUST_DOC}"
  rm -f "${TRUST_DOC}"

  confirm_run "Attaches the AWS-managed CloudWatchLogsFullAccess policy so the runtime can emit logs." \
    aws iam attach-role-policy \
      --role-name "${ROLE_NAME}" \
      --policy-arn "arn:aws:iam::aws:policy/CloudWatchLogsFullAccess"
  IAM_NEEDS_PROPAGATION=1
fi

# AgentCore needs to pull the image from ECR when starting the microVM.
# put-role-policy is idempotent, so this runs whether the role is new or pre-existing.
info "Granting the role permission to pull the shellagent image from ECR."
ECR_POLICY_DOC="$(mktemp)"
cat >"${ECR_POLICY_DOC}" <<EOF
{
  "Version": "2012-10-17",
  "Statement": [
    {
      "Effect": "Allow",
      "Action": "ecr:GetAuthorizationToken",
      "Resource": "*"
    },
    {
      "Effect": "Allow",
      "Action": [
        "ecr:BatchGetImage",
        "ecr:GetDownloadUrlForLayer"
      ],
      "Resource": "arn:aws:ecr:${REGION}:${ACCOUNT_ID}:repository/${REPO_NAME}"
    }
  ]
}
EOF
confirm_run "Attaches an inline 'ecr-pull' policy granting ecr:GetAuthorizationToken, ecr:BatchGetImage, and ecr:GetDownloadUrlForLayer (the perms AgentCore validates against)." \
  aws iam put-role-policy \
    --role-name "${ROLE_NAME}" \
    --policy-name "ecr-pull" \
    --policy-document "file://${ECR_POLICY_DOC}"
rm -f "${ECR_POLICY_DOC}"

if [[ -n "${IAM_NEEDS_PROPAGATION:-}" ]]; then
  info "Sleeping 10s for IAM propagation."
  sleep 10
fi

step "Create the AgentCore runtime"
info "Name must match [a-zA-Z][a-zA-Z0-9_]{0,47} — letters, digits, underscores; no dashes."
while :; do
  prompt RUNTIME_NAME "Runtime name" "microvm_shell"
  if [[ "${RUNTIME_NAME}" =~ ^[a-zA-Z][a-zA-Z0-9_]{0,47}$ ]]; then
    break
  fi
  warn "Invalid: must start with a letter, only [A-Za-z0-9_], max 48 chars (no dashes)."
done

if aws bedrock-agentcore-control get-agent-runtime \
    --agent-runtime-name "${RUNTIME_NAME}" \
    --region "${REGION}" >/dev/null 2>&1; then
  ok "Runtime ${RUNTIME_NAME} already exists, fetching its ARN."
  RUNTIME_ARN="$(aws bedrock-agentcore-control get-agent-runtime \
    --agent-runtime-name "${RUNTIME_NAME}" \
    --region "${REGION}" \
    --query 'agentRuntimeArn' --output text)"
else
  ARTIFACT_JSON=$(jq -n --arg uri "${ECR_URI}@${IMAGE_DIGEST}" \
    '{containerConfiguration: {containerUri: $uri}}')
  NETWORK_JSON='{"networkMode":"PUBLIC"}'
  confirm_run "Creates a Bedrock AgentCore runtime that points at the image you just pushed." \
    aws bedrock-agentcore-control create-agent-runtime \
      --agent-runtime-name "${RUNTIME_NAME}" \
      --agent-runtime-artifact "${ARTIFACT_JSON}" \
      --network-configuration "${NETWORK_JSON}" \
      --role-arn "${ROLE_ARN}" \
      --region "${REGION}"
  RUNTIME_ARN="$(aws bedrock-agentcore-control get-agent-runtime \
    --agent-runtime-name "${RUNTIME_NAME}" \
    --region "${REGION}" \
    --query 'agentRuntimeArn' --output text)"
fi
ok "Runtime ARN: ${RUNTIME_ARN}"

step "Bind the microvm CLI to this runtime"
info "Writes region + runtime ARN to ~/.microvm/config.yaml so subsequent commands skip the flags."
confirm_run "Runs 'microvm login' to validate the runtime is reachable and persist config." \
  go run "${MICROVM_DIR}" login \
    --region "${REGION}" \
    --runtime-arn "${RUNTIME_ARN}" \
    --image-digest "${IMAGE_DIGEST}"

cat <<EOF

${C_GREEN}${C_BOLD}Setup complete.${C_RESET}

You can now exercise the CLI:

  ${C_DIM}# create a sandbox${C_RESET}
  go run ${MICROVM_DIR} create my-sbx

  ${C_DIM}# run a command in it${C_RESET}
  go run ${MICROVM_DIR} exec my-sbx -- uname -a

  ${C_DIM}# round-trip a file${C_RESET}
  echo hello > /tmp/x && go run ${MICROVM_DIR} cp /tmp/x my-sbx:/tmp/x
  go run ${MICROVM_DIR} exec my-sbx -- cat /tmp/x

  ${C_DIM}# snapshot + resume${C_RESET}
  go run ${MICROVM_DIR} snapshot my-sbx snap1
  go run ${MICROVM_DIR} resume snap1 my-sbx-2

  ${C_DIM}# clean up${C_RESET}
  go run ${MICROVM_DIR} terminate my-sbx

${C_DIM}Cold start is 5-15s for the first invocation; subsequent calls within
a session are fast. AgentCore bills per invocation + compute time.${C_RESET}

EOF
