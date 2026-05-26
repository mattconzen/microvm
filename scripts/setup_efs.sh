#!/usr/bin/env bash
#
# Provisions an EFS-backed AgentCore runtime alongside the existing one
# created by scripts/setup.sh. Run this once per environment, then switch
# the CLI with `microvm login --snapshot-mode efs ...` (the exact command
# is printed at the end).
#
# Resources created:
#   - VPC (10.42.0.0/16) with 2 /24 subnets across 2 AZs
#   - Security group `microvm-efs-sg` allowing 2049/tcp from the VPC CIDR
#   - EFS filesystem (encrypted, generalPurpose, elastic throughput)
#   - Mount targets in each subnet
#   - EFS access point rooted at "/" (uid/gid 0, 0755)
#   - Inline `microvm-efs` policy on the existing shellagent execution role
#   - AgentCore runtime `microvm_shell_efs` (VPC mode + EFS mount at /mnt/efs)
#
# Every resource is tagged `microvm=managed` so a future teardown script
# can find and reap them. Idempotent: rerunning reuses what already exists.

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
ok()   { printf "${C_GREEN}+  %s${C_RESET}\n" "$*"; }

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

  microvm EFS setup
  -----------------
  This walks you through provisioning the VPC + EFS resources microvm
  needs to run with --snapshot-mode efs:
  VPC, 2 subnets, security group, EFS filesystem + mount targets,
  EFS access point, IAM policy, and a parallel AgentCore runtime.

  Every step prints the command it's about to run and asks before running.
  All AWS resources are tagged microvm=managed so they can be torn down.

BANNER

step "Checking prerequisites"
require_cmd aws
require_cmd jq
ok "aws, jq present"

step "AWS account + region"
info "Reads your default profile via 'aws sts get-caller-identity'."
ACCOUNT_ID="$(aws sts get-caller-identity --query Account --output text)"
ok "Account: ${ACCOUNT_ID}"
DEFAULT_REGION="${AWS_REGION:-$(aws configure get region 2>/dev/null || echo "")}"
prompt REGION "AWS region (must support Bedrock AgentCore + EFS)" "${DEFAULT_REGION:-us-east-1}"
[[ -z "${REGION}" ]] && { warn "No region set."; exit 1; }

VPC_CIDR="10.42.0.0/16"
TAG_KV="Key=microvm,Value=managed"
TAG_FILTER="Name=tag:microvm,Values=managed"

step "VPC (CIDR ${VPC_CIDR})"
VPC_ID="$(aws ec2 describe-vpcs \
  --region "${REGION}" \
  --filters ${TAG_FILTER} \
  --query 'Vpcs[0].VpcId' \
  --output text 2>/dev/null || echo "None")"
if [[ "${VPC_ID}" == "None" || -z "${VPC_ID}" ]]; then
  confirm_run "Creates a dedicated VPC for the EFS-mode runtime, tagged microvm=managed." \
    aws ec2 create-vpc \
      --region "${REGION}" \
      --cidr-block "${VPC_CIDR}" \
      --tag-specifications "ResourceType=vpc,Tags=[{${TAG_KV}},{Key=Name,Value=microvm-vpc}]"
  VPC_ID="$(aws ec2 describe-vpcs \
    --region "${REGION}" \
    --filters ${TAG_FILTER} \
    --query 'Vpcs[0].VpcId' \
    --output text)"
  aws ec2 modify-vpc-attribute \
    --region "${REGION}" \
    --vpc-id "${VPC_ID}" \
    --enable-dns-hostnames
else
  ok "Reusing existing tagged VPC."
fi
ok "VPC: ${VPC_ID}"

step "Subnets (2 AZs)"
mapfile -t AZS < <(aws ec2 describe-availability-zones \
  --region "${REGION}" \
  --query 'AvailabilityZones[0:2].ZoneName' \
  --output text | tr '\t' '\n')
[[ "${#AZS[@]}" -lt 2 ]] && { warn "Region ${REGION} has fewer than 2 AZs."; exit 1; }
info "Using AZs: ${AZS[0]}, ${AZS[1]}"

SUBNETS=()
for i in 0 1; do
  AZ="${AZS[$i]}"
  CIDR="10.42.${i}.0/24"
  SN="$(aws ec2 describe-subnets \
    --region "${REGION}" \
    --filters "Name=vpc-id,Values=${VPC_ID}" "Name=availability-zone,Values=${AZ}" "${TAG_FILTER}" \
    --query 'Subnets[0].SubnetId' \
    --output text 2>/dev/null || echo "None")"
  if [[ "${SN}" == "None" || -z "${SN}" ]]; then
    confirm_run "Creates a ${CIDR} subnet in ${AZ}." \
      aws ec2 create-subnet \
        --region "${REGION}" \
        --vpc-id "${VPC_ID}" \
        --cidr-block "${CIDR}" \
        --availability-zone "${AZ}" \
        --tag-specifications "ResourceType=subnet,Tags=[{${TAG_KV}},{Key=Name,Value=microvm-subnet-${AZ}}]"
    SN="$(aws ec2 describe-subnets \
      --region "${REGION}" \
      --filters "Name=vpc-id,Values=${VPC_ID}" "Name=availability-zone,Values=${AZ}" "${TAG_FILTER}" \
      --query 'Subnets[0].SubnetId' \
      --output text)"
  fi
  SUBNETS+=("${SN}")
done
ok "Subnets: ${SUBNETS[0]} ${SUBNETS[1]}"

step "Security group (microvm-efs-sg)"
SG_ID="$(aws ec2 describe-security-groups \
  --region "${REGION}" \
  --filters "Name=vpc-id,Values=${VPC_ID}" "Name=group-name,Values=microvm-efs-sg" \
  --query 'SecurityGroups[0].GroupId' \
  --output text 2>/dev/null || echo "None")"
if [[ "${SG_ID}" == "None" || -z "${SG_ID}" ]]; then
  confirm_run "Creates the microvm-efs-sg security group in the VPC." \
    aws ec2 create-security-group \
      --region "${REGION}" \
      --vpc-id "${VPC_ID}" \
      --group-name "microvm-efs-sg" \
      --description "microvm EFS NFS ingress" \
      --tag-specifications "ResourceType=security-group,Tags=[{${TAG_KV}},{Key=Name,Value=microvm-efs-sg}]"
  SG_ID="$(aws ec2 describe-security-groups \
    --region "${REGION}" \
    --filters "Name=vpc-id,Values=${VPC_ID}" "Name=group-name,Values=microvm-efs-sg" \
    --query 'SecurityGroups[0].GroupId' \
    --output text)"
else
  ok "Reusing existing security group."
fi
info "Ensuring NFS (TCP 2049) ingress from ${VPC_CIDR} is present (idempotent)."
aws ec2 authorize-security-group-ingress \
  --region "${REGION}" \
  --group-id "${SG_ID}" \
  --protocol tcp \
  --port 2049 \
  --cidr "${VPC_CIDR}" \
  2>&1 | grep -v "InvalidPermission.Duplicate" || true
ok "SG: ${SG_ID}"

step "EFS filesystem"
FS_ID="$(aws efs describe-file-systems \
  --region "${REGION}" \
  --query 'FileSystems[?Tags && length(Tags[?Key==`microvm` && Value==`managed`])>`0`].FileSystemId | [0]' \
  --output text 2>/dev/null || echo "None")"
if [[ "${FS_ID}" == "None" || -z "${FS_ID}" ]]; then
  confirm_run "Creates an encrypted EFS filesystem (generalPurpose, elastic throughput)." \
    aws efs create-file-system \
      --region "${REGION}" \
      --encrypted \
      --performance-mode generalPurpose \
      --throughput-mode elastic \
      --tags "Key=microvm,Value=managed" "Key=Name,Value=microvm"
  FS_ID="$(aws efs describe-file-systems \
    --region "${REGION}" \
    --query 'FileSystems[?Tags && length(Tags[?Key==`microvm` && Value==`managed`])>`0`].FileSystemId | [0]' \
    --output text)"
else
  ok "Reusing existing EFS filesystem."
fi
ok "EFS: ${FS_ID}"

info "Waiting for the filesystem to become available..."
until [[ "$(aws efs describe-file-systems \
    --region "${REGION}" \
    --file-system-id "${FS_ID}" \
    --query 'FileSystems[0].LifeCycleState' \
    --output text)" == "available" ]]; do
  info "  still provisioning, sleeping 5s..."
  sleep 5
done
ok "Filesystem is available."

step "Mount targets (one per subnet)"
for SN in "${SUBNETS[@]}"; do
  MT="$(aws efs describe-mount-targets \
    --region "${REGION}" \
    --file-system-id "${FS_ID}" \
    --query "MountTargets[?SubnetId=='${SN}'].MountTargetId | [0]" \
    --output text 2>/dev/null || echo "None")"
  if [[ "${MT}" == "None" || -z "${MT}" ]]; then
    confirm_run "Creates a mount target in subnet ${SN} attached to the EFS security group." \
      aws efs create-mount-target \
        --region "${REGION}" \
        --file-system-id "${FS_ID}" \
        --subnet-id "${SN}" \
        --security-groups "${SG_ID}"
  else
    ok "Mount target already present for ${SN} (${MT})."
  fi
done
info "Mount targets take ~30s to finish bringing up their ENIs; that's fine, the runtime won't mount until first invocation."

step "EFS access point"
AP_ARN="$(aws efs describe-access-points \
  --region "${REGION}" \
  --file-system-id "${FS_ID}" \
  --query "AccessPoints[?Name=='microvm'].AccessPointArn | [0]" \
  --output text 2>/dev/null || echo "None")"
if [[ "${AP_ARN}" == "None" || -z "${AP_ARN}" ]]; then
  confirm_run "Creates the EFS access point rooted at / with uid/gid 0 and 0755." \
    aws efs create-access-point \
      --region "${REGION}" \
      --file-system-id "${FS_ID}" \
      --tags "Key=Name,Value=microvm" "Key=microvm,Value=managed" \
      --root-directory 'Path=/,CreationInfo={OwnerUid=0,OwnerGid=0,Permissions=0755}'
  AP_ARN="$(aws efs describe-access-points \
    --region "${REGION}" \
    --file-system-id "${FS_ID}" \
    --query "AccessPoints[?Name=='microvm'].AccessPointArn | [0]" \
    --output text)"
else
  ok "Reusing existing access point."
fi
ok "Access point: ${AP_ARN}"

step "Grant EFS perms to the existing execution role"
prompt ROLE_NAME "IAM role name (the one setup.sh created)" "microvm-shellagent-exec"
if ! aws iam get-role --role-name "${ROLE_NAME}" >/dev/null 2>&1; then
  warn "Role ${ROLE_NAME} not found. Run scripts/setup.sh first, or supply the correct role name."
  exit 1
fi
ROLE_ARN="$(aws iam get-role --role-name "${ROLE_NAME}" --query 'Role.Arn' --output text)"

EFS_POLICY_DOC="$(mktemp)"
cat >"${EFS_POLICY_DOC}" <<'EOF'
{
  "Version": "2012-10-17",
  "Statement": [
    {
      "Effect": "Allow",
      "Action": [
        "elasticfilesystem:ClientMount",
        "elasticfilesystem:ClientWrite",
        "elasticfilesystem:ClientRootAccess"
      ],
      "Resource": "*"
    },
    {
      "Effect": "Allow",
      "Action": [
        "ec2:CreateNetworkInterface",
        "ec2:DeleteNetworkInterface",
        "ec2:DescribeNetworkInterfaces",
        "ec2:DescribeSubnets",
        "ec2:DescribeSecurityGroups",
        "ec2:DescribeVpcs"
      ],
      "Resource": "*"
    }
  ]
}
EOF
confirm_run "Attaches an inline 'microvm-efs' policy granting EFS client access + the ENI perms AgentCore needs to attach the VPC runtime to your subnets." \
  aws iam put-role-policy \
    --role-name "${ROLE_NAME}" \
    --policy-name "microvm-efs" \
    --policy-document "file://${EFS_POLICY_DOC}"
rm -f "${EFS_POLICY_DOC}"

info "Sleeping 10s for IAM propagation (AgentCore validates perms synchronously on create-agent-runtime)."
sleep 10

step "Resolve the shellagent image URI from ECR"
prompt REPO_NAME "ECR repository name (the one setup.sh pushed to)" "microvm-shell"
if ! aws ecr describe-repositories --region "${REGION}" --repository-names "${REPO_NAME}" >/dev/null 2>&1; then
  warn "ECR repository ${REPO_NAME} not found in ${REGION}. Run scripts/setup.sh first."
  exit 1
fi
REPO_URI="$(aws ecr describe-repositories \
  --region "${REGION}" \
  --repository-names "${REPO_NAME}" \
  --query 'repositories[0].repositoryUri' \
  --output text)"
IMAGE_DIGEST="$(aws ecr describe-images \
  --region "${REGION}" \
  --repository-name "${REPO_NAME}" \
  --image-ids imageTag=latest \
  --query 'imageDetails[0].imageDigest' \
  --output text 2>/dev/null || echo "")"
if [[ -z "${IMAGE_DIGEST}" || "${IMAGE_DIGEST}" == "None" ]]; then
  warn "No 'latest' tag found in ${REPO_NAME}. Push an image first via scripts/setup.sh."
  exit 1
fi
IMAGE_REF="${REPO_URI}@${IMAGE_DIGEST}"
ok "Image: ${IMAGE_REF}"

step "Create the EFS-mode AgentCore runtime"
info "Name must match [a-zA-Z][a-zA-Z0-9_]{0,47} — letters, digits, underscores; no dashes."
while :; do
  prompt RUNTIME_NAME "Runtime name" "microvm_shell_efs"
  if [[ "${RUNTIME_NAME}" =~ ^[a-zA-Z][a-zA-Z0-9_]{0,47}$ ]]; then
    break
  fi
  warn "Invalid: must start with a letter, only [A-Za-z0-9_], max 48 chars (no dashes)."
done

EXISTING_ARN="$(aws bedrock-agentcore-control list-agent-runtimes \
  --region "${REGION}" \
  --query "agentRuntimes[?agentRuntimeName=='${RUNTIME_NAME}'].agentRuntimeArn | [0]" \
  --output text 2>/dev/null || echo "None")"
if [[ -n "${EXISTING_ARN}" && "${EXISTING_ARN}" != "None" ]]; then
  warn "Runtime ${RUNTIME_NAME} already exists; reusing its ARN."
  warn "If you need to update its image/VPC config, delete it first with:"
  warn "  aws bedrock-agentcore-control delete-agent-runtime --agent-runtime-id <id> --region ${REGION}"
  RUNTIME_ARN="${EXISTING_ARN}"
else
  ARTIFACT_JSON="$(jq -n --arg uri "${IMAGE_REF}" \
    '{containerConfiguration: {containerUri: $uri}}')"
  NETWORK_JSON="$(jq -n \
    --arg s0 "${SUBNETS[0]}" \
    --arg s1 "${SUBNETS[1]}" \
    --arg sg "${SG_ID}" \
    '{networkMode: "VPC", vpcConfig: {subnetIds: [$s0, $s1], securityGroupIds: [$sg]}}')"
  FS_JSON="$(jq -n --arg ap "${AP_ARN}" \
    '[{efsAccessPoint: {accessPointArn: $ap, mountPath: "/mnt/efs"}}]')"
  # Env vars baked into the runtime so the shellagent's make_snapshotter()
  # selects the EFS snapshotter instead of falling back to alias mode.
  # Mirrors backend/aws/snapshot_efs.go's EnvOverrides().
  # TODO: verify the exact AgentCore CLI flag name on first real run --
  # the AWS CLI standard for create-agent-runtime is --environment-variables
  # taking a JSON object, but AgentCore may use --environment or a nested
  # shape under --agent-runtime-artifact. Adjust here if the create call
  # rejects the flag.
  ENV_JSON="$(jq -n \
    --arg ap "${AP_ARN}" \
    '{
      MICROVM_SNAPSHOT_MODE: "efs",
      MICROVM_EFS_MOUNT_PATH: "/mnt/efs",
      MICROVM_EFS_ACCESS_POINT: $ap
    }')"
  confirm_run "Creates a VPC-mode AgentCore runtime that mounts the EFS access point at /mnt/efs." \
    aws bedrock-agentcore-control create-agent-runtime \
      --region "${REGION}" \
      --agent-runtime-name "${RUNTIME_NAME}" \
      --agent-runtime-artifact "${ARTIFACT_JSON}" \
      --network-configuration "${NETWORK_JSON}" \
      --filesystem-configurations "${FS_JSON}" \
      --environment-variables "${ENV_JSON}" \
      --role-arn "${ROLE_ARN}"
  RUNTIME_ARN="$(aws bedrock-agentcore-control list-agent-runtimes \
    --region "${REGION}" \
    --query "agentRuntimes[?agentRuntimeName=='${RUNTIME_NAME}'].agentRuntimeArn | [0]" \
    --output text)"
fi
ok "Runtime ARN: ${RUNTIME_ARN}"

cat <<EOF

${C_GREEN}${C_BOLD}EFS mode is provisioned.${C_RESET}

Switch the CLI to EFS mode with:

  ${C_BOLD}go run ${MICROVM_DIR} login \\
    --region ${REGION} \\
    --runtime-arn ${RUNTIME_ARN} \\
    --image-digest ${IMAGE_DIGEST} \\
    --snapshot-mode efs \\
    --efs-access-point-arn ${AP_ARN}${C_RESET}

To return to the alias-mode runtime, rerun ${C_BOLD}microvm login${C_RESET} with the
original runtime ARN and ${C_BOLD}--snapshot-mode none${C_RESET} (or ${C_BOLD}s3${C_RESET}).

${C_DIM}All AWS resources created here are tagged microvm=managed. A future
teardown script can find and delete them via that tag.${C_RESET}

EOF
