#!/usr/bin/env bash
#
# Provisions a tiered-mode AgentCore runtime alongside the existing ones
# created by scripts/setup.sh and scripts/setup_efs.sh. Run this once per
# environment, then switch the CLI with:
#
#   microvm login --snapshot-mode tiered --s3-files-access-point-arn ... \
#                 --s3-files-bucket ...
#
# (the exact command is printed at the end).
#
# Architecture: two filesystems inside the runtime,
#   - Tier 1 (cache):     /var/microvm/cache/<sandbox_id>/  (AgentCore-managed)
#   - Tier 2 (workspace): /workspace/<sandbox_id>/          (S3 Files mount)
#
# Resources created (idempotent; rerunning reuses what already exists):
#   - VPC (10.42.0.0/16) with 2 /24 subnets across 2 AZs (reused if
#     setup_efs.sh already created them)
#   - Security group `microvm-tiered-sg` (egress 443 only -- S3 traffic
#     goes via the VPC gateway endpoint; no NFS ingress needed)
#   - S3 bucket `microvm-workspace-<account>-<region>` with versioning on,
#     SSE-S3, and block-public-access on
#   - S3 access point `microvm-workspace` rooted at the bucket
#   - S3 Files filesystem + association to the access point
#   - VPC gateway endpoint for S3 (free; avoids NAT egress for S3 traffic)
#   - Inline `microvm-tiered` policy on the existing shellagent execution
#     role granting S3 Files client + S3 object ops + ENI perms
#   - AgentCore runtime `microvm_shell_tiered` (VPC mode + S3 Files mount
#     at /workspace)
#
# Every resource is tagged `microvm=managed` so scripts/teardown.sh can
# find and reap them.

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

  microvm tiered setup
  --------------------
  This walks you through provisioning the VPC + S3 + S3 Files resources
  microvm needs to run with --snapshot-mode tiered:
  VPC (or reuses the EFS one), 2 subnets, security group, S3 bucket,
  S3 access point, S3 Files filesystem + association, S3 gateway endpoint,
  IAM policy, and a parallel AgentCore runtime named microvm_shell_tiered.

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
prompt REGION "AWS region (must support Bedrock AgentCore + S3 Files)" "${DEFAULT_REGION:-us-east-1}"
[[ -z "${REGION}" ]] && { warn "No region set."; exit 1; }

VPC_CIDR="10.42.0.0/16"
TAG_KV="Key=microvm,Value=managed"
TAG_FILTER="Name=tag:microvm,Values=managed"

step "VPC (CIDR ${VPC_CIDR})"
info "Reuses an existing microvm=managed VPC if setup_efs.sh already created one."
VPC_ID="$(aws ec2 describe-vpcs \
  --region "${REGION}" \
  --filters ${TAG_FILTER} \
  --query 'Vpcs[0].VpcId' \
  --output text 2>/dev/null || echo "None")"
if [[ "${VPC_ID}" == "None" || -z "${VPC_ID}" ]]; then
  confirm_run "Creates a dedicated VPC for the tiered-mode runtime, tagged microvm=managed." \
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

step "Security group (microvm-tiered-sg)"
SG_ID="$(aws ec2 describe-security-groups \
  --region "${REGION}" \
  --filters "Name=vpc-id,Values=${VPC_ID}" "Name=group-name,Values=microvm-tiered-sg" \
  --query 'SecurityGroups[0].GroupId' \
  --output text 2>/dev/null || echo "None")"
if [[ "${SG_ID}" == "None" || -z "${SG_ID}" ]]; then
  confirm_run "Creates the microvm-tiered-sg security group in the VPC (egress 443 only; S3 traffic uses the gateway endpoint, no NFS ingress)." \
    aws ec2 create-security-group \
      --region "${REGION}" \
      --vpc-id "${VPC_ID}" \
      --group-name "microvm-tiered-sg" \
      --description "microvm tiered-mode runtime egress (S3 via gateway endpoint)" \
      --tag-specifications "ResourceType=security-group,Tags=[{${TAG_KV}},{Key=Name,Value=microvm-tiered-sg}]"
  SG_ID="$(aws ec2 describe-security-groups \
    --region "${REGION}" \
    --filters "Name=vpc-id,Values=${VPC_ID}" "Name=group-name,Values=microvm-tiered-sg" \
    --query 'SecurityGroups[0].GroupId' \
    --output text)"
else
  ok "Reusing existing security group."
fi
ok "SG: ${SG_ID}"

step "S3 bucket (microvm-workspace-${ACCOUNT_ID}-${REGION})"
BUCKET="microvm-workspace-${ACCOUNT_ID}-${REGION}"
if aws s3api head-bucket --bucket "${BUCKET}" --region "${REGION}" >/dev/null 2>&1; then
  ok "Bucket ${BUCKET} already exists; reusing."
else
  if [[ "${REGION}" == "us-east-1" ]]; then
    confirm_run "Creates the workspace bucket in us-east-1 (no LocationConstraint)." \
      aws s3api create-bucket \
        --region "${REGION}" \
        --bucket "${BUCKET}"
  else
    confirm_run "Creates the workspace bucket in ${REGION}." \
      aws s3api create-bucket \
        --region "${REGION}" \
        --bucket "${BUCKET}" \
        --create-bucket-configuration "LocationConstraint=${REGION}"
  fi
fi
ok "Bucket: ${BUCKET}"

info "Tagging bucket microvm=managed (idempotent)."
aws s3api put-bucket-tagging \
  --region "${REGION}" \
  --bucket "${BUCKET}" \
  --tagging "TagSet=[{Key=microvm,Value=managed},{Key=Name,Value=microvm-workspace}]"

info "Enabling block-public-access on the bucket (idempotent)."
aws s3api put-public-access-block \
  --region "${REGION}" \
  --bucket "${BUCKET}" \
  --public-access-block-configuration \
    "BlockPublicAcls=true,IgnorePublicAcls=true,BlockPublicPolicy=true,RestrictPublicBuckets=true"

info "Enabling versioning on the bucket (idempotent)."
aws s3api put-bucket-versioning \
  --region "${REGION}" \
  --bucket "${BUCKET}" \
  --versioning-configuration "Status=Enabled"

info "Enabling SSE-S3 default encryption on the bucket (idempotent)."
aws s3api put-bucket-encryption \
  --region "${REGION}" \
  --bucket "${BUCKET}" \
  --server-side-encryption-configuration \
    '{"Rules":[{"ApplyServerSideEncryptionByDefault":{"SSEAlgorithm":"AES256"},"BucketKeyEnabled":true}]}'
ok "Bucket configured (versioning + SSE-S3 + block-public-access)."

step "S3 access point (microvm-workspace)"
AP_NAME="microvm-workspace"
AP_ARN="$(aws s3control get-access-point \
  --region "${REGION}" \
  --account-id "${ACCOUNT_ID}" \
  --name "${AP_NAME}" \
  --query 'AccessPointArn' \
  --output text 2>/dev/null || echo "None")"
if [[ "${AP_ARN}" == "None" || -z "${AP_ARN}" ]]; then
  confirm_run "Creates the S3 access point rooted at the bucket." \
    aws s3control create-access-point \
      --region "${REGION}" \
      --account-id "${ACCOUNT_ID}" \
      --name "${AP_NAME}" \
      --bucket "${BUCKET}"
  AP_ARN="$(aws s3control get-access-point \
    --region "${REGION}" \
    --account-id "${ACCOUNT_ID}" \
    --name "${AP_NAME}" \
    --query 'AccessPointArn' \
    --output text)"
else
  ok "Reusing existing access point."
fi
ok "S3 access point: ${AP_ARN}"

# ---------------------------------------------------------------------------
# S3 Files filesystem + association.
#
# TODO at implementation time: verify the exact `aws s3files` CLI surface.
# The S3 Files API is new and the command names/flag shapes below are
# best-effort based on the PR3 plan; touch them up against the
# current AWS CLI reference when you run this script for real.
# ---------------------------------------------------------------------------
step "S3 Files filesystem + association (NEW API -- verify shape)"
S3FILES_FS_ID="$(aws s3files list-file-systems \
  --region "${REGION}" \
  --query "fileSystems[?Tags && length(Tags[?Key=='microvm' && Value=='managed'])>\`0\`].fileSystemId | [0]" \
  --output text 2>/dev/null || echo "None")"
if [[ "${S3FILES_FS_ID}" == "None" || -z "${S3FILES_FS_ID}" ]]; then
  # TODO: verify exact create-file-system flag set; may need --bucket / region args.
  confirm_run "Creates an S3 Files filesystem tagged microvm=managed." \
    aws s3files create-file-system \
      --region "${REGION}" \
      --tags "Key=microvm,Value=managed" "Key=Name,Value=microvm-workspace"
  S3FILES_FS_ID="$(aws s3files list-file-systems \
    --region "${REGION}" \
    --query "fileSystems[?Tags && length(Tags[?Key=='microvm' && Value=='managed'])>\`0\`].fileSystemId | [0]" \
    --output text 2>/dev/null || echo "")"
else
  ok "Reusing existing S3 Files filesystem."
fi
if [[ -z "${S3FILES_FS_ID}" || "${S3FILES_FS_ID}" == "None" ]]; then
  warn "S3 Files filesystem ID could not be resolved. Aborting."
  warn "This may be because the 'aws s3files' API surface has changed since this script was written."
  exit 1
fi
ok "S3 Files filesystem: ${S3FILES_FS_ID}"

# TODO: confirm the association command name. Plan suggests
#       `aws s3files associate --filesystem-id ... --access-point-arn ...`.
info "Associating the access point with the S3 Files filesystem (idempotent best-effort)."
# S3FILES_FS_ID is guaranteed non-empty here (asserted above).
ASSOC_EXISTS="$(aws s3files list-associations \
  --region "${REGION}" \
  --filesystem-id "${S3FILES_FS_ID}" \
  --query "associations[?accessPointArn=='${AP_ARN}'] | [0].associationId" \
  --output text 2>/dev/null || echo "None")"
if [[ "${ASSOC_EXISTS}" == "None" || -z "${ASSOC_EXISTS}" ]]; then
  confirm_run "Associates the access point with the S3 Files filesystem." \
    aws s3files associate \
      --region "${REGION}" \
      --filesystem-id "${S3FILES_FS_ID}" \
      --access-point-arn "${AP_ARN}"
else
  ok "Association already exists (${ASSOC_EXISTS})."
fi

# AgentCore expects the access point ARN for filesystemConfigurations; we
# already have it in ${AP_ARN}. If the S3 Files API returns a separate
# "fs-scoped" ARN we should prefer that -- TODO: confirm at implementation
# time and overwrite RUNTIME_AP_ARN below if needed.
RUNTIME_AP_ARN="${AP_ARN}"

step "VPC gateway endpoint for S3 (free; avoids NAT egress)"
# Find the main route table for the VPC explicitly via association.main=true
# so we don't accidentally attach the endpoint to a custom/NAT table.
RTB_ID="$(aws ec2 describe-route-tables \
  --region "${REGION}" \
  --filters "Name=vpc-id,Values=${VPC_ID}" "Name=association.main,Values=true" \
  --query 'RouteTables[0].RouteTableId' \
  --output text 2>/dev/null || echo "None")"
if [[ "${RTB_ID}" == "None" || -z "${RTB_ID}" ]]; then
  warn "No route table found in VPC ${VPC_ID}; skipping S3 gateway endpoint."
else
  S3_ENDPOINT_ID="$(aws ec2 describe-vpc-endpoints \
    --region "${REGION}" \
    --filters "Name=vpc-id,Values=${VPC_ID}" "Name=service-name,Values=com.amazonaws.${REGION}.s3" "${TAG_FILTER}" \
    --query 'VpcEndpoints[0].VpcEndpointId' \
    --output text 2>/dev/null || echo "None")"
  if [[ "${S3_ENDPOINT_ID}" == "None" || -z "${S3_ENDPOINT_ID}" ]]; then
    confirm_run "Creates a free VPC gateway endpoint for S3 attached to route table ${RTB_ID}." \
      aws ec2 create-vpc-endpoint \
        --region "${REGION}" \
        --vpc-id "${VPC_ID}" \
        --service-name "com.amazonaws.${REGION}.s3" \
        --vpc-endpoint-type Gateway \
        --route-table-ids "${RTB_ID}" \
        --tag-specifications "ResourceType=vpc-endpoint,Tags=[{${TAG_KV}},{Key=Name,Value=microvm-s3-endpoint}]"
    S3_ENDPOINT_ID="$(aws ec2 describe-vpc-endpoints \
      --region "${REGION}" \
      --filters "Name=vpc-id,Values=${VPC_ID}" "Name=service-name,Values=com.amazonaws.${REGION}.s3" "${TAG_FILTER}" \
      --query 'VpcEndpoints[0].VpcEndpointId' \
      --output text 2>/dev/null || echo "")"
  else
    ok "Reusing existing S3 gateway endpoint."
  fi
  ok "S3 endpoint: ${S3_ENDPOINT_ID}"
fi

step "Grant tiered-mode perms to the existing execution role"
prompt ROLE_NAME "IAM role name (the one setup.sh created)" "microvm-shellagent-exec"
if ! aws iam get-role --role-name "${ROLE_NAME}" >/dev/null 2>&1; then
  warn "Role ${ROLE_NAME} not found. Run scripts/setup.sh first, or supply the correct role name."
  exit 1
fi
ROLE_ARN="$(aws iam get-role --role-name "${ROLE_NAME}" --query 'Role.Arn' --output text)"

# Build the filesystem ARN for the S3 Files client policy statement.
# TODO at implementation time: confirm the exact ARN format. The PR3 plan
# expects `s3files:ClientMount`/`s3files:ClientWrite` on the filesystem ARN;
# adjust this format string if the real API uses a different shape.
# S3FILES_FS_ID is guaranteed non-empty here (asserted above).
S3FILES_FS_ARN="arn:aws:s3files:${REGION}:${ACCOUNT_ID}:filesystem/${S3FILES_FS_ID}"

# Note: s3files:ClientRootAccess mirrors the EFS pattern
# (elasticfilesystem:ClientRootAccess in setup_efs.sh) since the workload
# container runs as uid 0. If the real S3 Files API surface does not include
# this action, drop it from the Action list on first run.
TIERED_POLICY_DOC="$(mktemp)"
cat >"${TIERED_POLICY_DOC}" <<EOF
{
  "Version": "2012-10-17",
  "Statement": [
    {
      "Sid": "S3FilesClient",
      "Effect": "Allow",
      "Action": [
        "s3files:ClientMount",
        "s3files:ClientWrite",
        "s3files:ClientRootAccess"
      ],
      "Resource": "${S3FILES_FS_ARN}"
    },
    {
      "Sid": "S3Objects",
      "Effect": "Allow",
      "Action": [
        "s3:GetObject",
        "s3:PutObject",
        "s3:DeleteObject",
        "s3:CopyObject"
      ],
      "Resource": "arn:aws:s3:::${BUCKET}/*"
    },
    {
      "Sid": "S3ListBucketPrefixed",
      "Effect": "Allow",
      "Action": "s3:ListBucket",
      "Resource": "arn:aws:s3:::${BUCKET}",
      "Condition": {
        "StringLike": {
          "s3:prefix": [
            "sessions/*",
            "snapshots/*"
          ]
        }
      }
    },
    {
      "Sid": "VpcEnis",
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
confirm_run "Attaches an inline 'microvm-tiered' policy granting S3 Files client + S3 object ops + ENI perms AgentCore needs to attach the VPC runtime." \
  aws iam put-role-policy \
    --role-name "${ROLE_NAME}" \
    --policy-name "microvm-tiered" \
    --policy-document "file://${TIERED_POLICY_DOC}"
rm -f "${TIERED_POLICY_DOC}"

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

step "Create the tiered-mode AgentCore runtime"
info "Name must match [a-zA-Z][a-zA-Z0-9_]{0,47} -- letters, digits, underscores; no dashes."
while :; do
  prompt RUNTIME_NAME "Runtime name" "microvm_shell_tiered"
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
  # TODO at implementation time: confirm the filesystemConfigurations shape
  # for S3 Files. The plan uses an `s3FilesAccessPoint` key analogous to the
  # EFS one (`efsAccessPoint`); the real key name may differ -- check
  # AgentCore docs (runtime-filesystem-configurations) before running.
  FS_JSON="$(jq -n --arg ap "${RUNTIME_AP_ARN}" \
    '[{s3FilesAccessPoint: {accessPointArn: $ap, mountPath: "/workspace"}}]')"
  confirm_run "Creates a VPC-mode AgentCore runtime that mounts the S3 Files access point at /workspace." \
    aws bedrock-agentcore-control create-agent-runtime \
      --region "${REGION}" \
      --agent-runtime-name "${RUNTIME_NAME}" \
      --agent-runtime-artifact "${ARTIFACT_JSON}" \
      --network-configuration "${NETWORK_JSON}" \
      --filesystem-configurations "${FS_JSON}" \
      --role-arn "${ROLE_ARN}"
  RUNTIME_ARN="$(aws bedrock-agentcore-control list-agent-runtimes \
    --region "${REGION}" \
    --query "agentRuntimes[?agentRuntimeName=='${RUNTIME_NAME}'].agentRuntimeArn | [0]" \
    --output text)"
fi
ok "Runtime ARN: ${RUNTIME_ARN}"

cat <<EOF

${C_GREEN}${C_BOLD}Tiered mode is provisioned.${C_RESET}

Switch the CLI to tiered mode with:

  ${C_BOLD}go run ${MICROVM_DIR} login \\
    --region ${REGION} \\
    --runtime-arn ${RUNTIME_ARN} \\
    --image-digest ${IMAGE_DIGEST} \\
    --snapshot-mode tiered \\
    --s3-files-access-point-arn ${RUNTIME_AP_ARN} \\
    --s3-files-bucket ${BUCKET}${C_RESET}

To return to the alias-mode or EFS-mode runtimes, rerun ${C_BOLD}microvm login${C_RESET}
with the original runtime ARN and ${C_BOLD}--snapshot-mode none${C_RESET} (or ${C_BOLD}s3${C_RESET}/${C_BOLD}efs${C_RESET}).

${C_DIM}All AWS resources created here are tagged microvm=managed. A future
teardown script can find and delete them via that tag.${C_RESET}

EOF
