#!/usr/bin/env bash
#
# Tears down everything scripts/setup.sh, scripts/setup_efs.sh, and
# scripts/setup_tiered.sh provisioned.
#
# Discovery strategy:
#   - VPC-side resources (VPC, subnets, SGs, EFS filesystem, mount targets,
#     access point, S3 gateway endpoint) are found by the `microvm=managed`
#     tag the setup scripts applied — we refuse to touch anything that isn't
#     tagged that way.
#   - AgentCore runtimes are looked up by name (`microvm_shell`,
#     `microvm_shell_efs`, `microvm_shell_tiered`) since AgentCore doesn't
#     surface tags in list output.
#   - ECR repo + IAM role are looked up by name (defaults match setup.sh).
#     For the IAM role we only delete the three inline policies setup created
#     (`ecr-pull`, `microvm-efs`, `microvm-tiered`) and detach the one managed
#     policy (`CloudWatchLogsFullAccess`). Other policies are left alone.
#   - S3 bucket is looked up by deterministic name
#     (`microvm-workspace-<account>-<region>`) and tag-checked before any
#     destructive action. If the bucket is non-empty, the user must type the
#     bucket name to confirm (unless --yes was passed).
#   - S3 Files filesystem + access point are looked up by tag.
#
# Order (children before parents):
#   1. AgentCore runtimes (microvm_shell, microvm_shell_efs, microvm_shell_tiered)
#   2. EFS access point
#   3. EFS mount targets (poll until describe-mount-targets is empty)
#   4. EFS filesystem
#   5. S3 Files association
#   6. S3 Files filesystem
#   7. S3 access point (microvm-workspace)
#   8. S3 bucket (empty + delete-bucket)
#   9. VPC gateway endpoint for S3
#  10. Security groups microvm-efs-sg, microvm-tiered-sg (depends on no ENIs attached)
#  11. Subnets (poll for no dependent ENIs)
#  12. VPC
#  13. IAM role inline policies + managed attachment, then the role
#  14. ECR images + repository
#
# Idempotent: a missing resource is logged and skipped. Pass --yes to skip
# the confirmation prompt.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
MICROVM_DIR="$(cd "${SCRIPT_DIR}/.." && pwd)"

readonly C_DIM=$'\033[2m'
readonly C_BOLD=$'\033[1m'
readonly C_GREEN=$'\033[32m'
readonly C_YELLOW=$'\033[33m'
readonly C_CYAN=$'\033[36m'
readonly C_RED=$'\033[31m'
readonly C_RESET=$'\033[0m'

step() { printf "\n${C_BOLD}${C_CYAN}==> %s${C_RESET}\n" "$*"; }
info() { printf "${C_DIM}%s${C_RESET}\n" "$*"; }
warn() { printf "${C_YELLOW}!  %s${C_RESET}\n" "$*"; }
ok()   { printf "${C_GREEN}+  %s${C_RESET}\n" "$*"; }
err()  { printf "${C_RED}x  %s${C_RESET}\n" "$*"; }

ASSUME_YES=0
for arg in "$@"; do
  case "$arg" in
    -y|--yes) ASSUME_YES=1 ;;
    -h|--help)
      cat <<EOF
Usage: $(basename "$0") [--yes]

Destroys AWS resources provisioned by scripts/setup.sh, scripts/setup_efs.sh,
and scripts/setup_tiered.sh.

Options:
  -y, --yes    Skip the confirmation prompt (still prints the summary).
  -h, --help   Show this help.
EOF
      exit 0
      ;;
    *)
      warn "Unknown arg: ${arg}"
      exit 1
      ;;
  esac
done

# Show a command and ask before running. Returns 0 if run, 1 if skipped.
# Honors --yes (ASSUME_YES=1 -> always run, no prompt).
confirm_run() {
  local explanation="$1"; shift
  printf "\n${C_DIM}%s${C_RESET}\n" "$explanation"
  printf "${C_BOLD}\$${C_RESET} "
  printf "%q " "$@"
  printf "\n"
  if [[ "${ASSUME_YES}" -eq 1 ]]; then
    "$@"
    return 0
  fi
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

# Verify a resource carries the microvm=managed tag. Returns 0 if managed,
# 1 otherwise. Callers pass already-fetched tag JSON (an array of {Key,Value}).
has_managed_tag() {
  local tags_json="$1"
  echo "$tags_json" | jq -e \
    'any(.[]?; .Key == "microvm" and .Value == "managed")' \
    >/dev/null 2>&1
}

# ---------------------------------------------------------------------------

cat <<'BANNER'

  microvm teardown
  ----------------
  Destroys the AWS resources provisioned by scripts/setup.sh,
  scripts/setup_efs.sh, and scripts/setup_tiered.sh. Only resources
  tagged microvm=managed (or the named runtimes / ECR repo / IAM role
  / S3 bucket) are touched.

  This is destructive. A summary of every targeted resource is printed
  before any delete call. Pass --yes to skip the confirmation prompt.

BANNER

step "Checking prerequisites"
require_cmd aws
require_cmd jq
ok "aws, jq present"

step "AWS account + region"
ACCOUNT_ID="$(aws sts get-caller-identity --query Account --output text)"
ok "Account: ${ACCOUNT_ID}"
DEFAULT_REGION="${AWS_REGION:-$(aws configure get region 2>/dev/null || echo "")}"
prompt REGION "AWS region" "${DEFAULT_REGION:-us-east-1}"
[[ -z "${REGION}" ]] && { warn "No region set."; exit 1; }

TAG_FILTER="Name=tag:microvm,Values=managed"

# Names default to the same values setup.sh / setup_efs.sh / setup_tiered.sh
# prompt for.
prompt RUNTIME_NAME        "AgentCore runtime name (alias mode)"  "microvm_shell"
prompt RUNTIME_NAME_EFS    "AgentCore runtime name (EFS mode)"    "microvm_shell_efs"
prompt RUNTIME_NAME_TIERED "AgentCore runtime name (tiered mode)" "microvm_shell_tiered"
prompt REPO_NAME           "ECR repository name"                  "microvm-shell"
prompt ROLE_NAME           "IAM execution role name"              "microvm-shellagent-exec"

# Tiered-mode resource names follow the deterministic schema from
# setup_tiered.sh.
BUCKET_NAME="microvm-workspace-${ACCOUNT_ID}-${REGION}"
AP_NAME="microvm-workspace"

# ---------------------------------------------------------------------------
# Discovery phase: find what actually exists. We don't delete anything yet.
# ---------------------------------------------------------------------------

step "Discovering resources"

RUNTIME_ARN=""
RUNTIME_ID=""
if RUNTIME_ARN="$(aws bedrock-agentcore-control list-agent-runtimes \
    --region "${REGION}" \
    --query "agentRuntimes[?agentRuntimeName=='${RUNTIME_NAME}'].agentRuntimeArn | [0]" \
    --output text 2>/dev/null)" && [[ -n "${RUNTIME_ARN}" && "${RUNTIME_ARN}" != "None" ]]; then
  RUNTIME_ID="$(aws bedrock-agentcore-control list-agent-runtimes \
    --region "${REGION}" \
    --query "agentRuntimes[?agentRuntimeName=='${RUNTIME_NAME}'].agentRuntimeId | [0]" \
    --output text 2>/dev/null || echo "")"
fi

RUNTIME_ARN_EFS=""
RUNTIME_ID_EFS=""
if RUNTIME_ARN_EFS="$(aws bedrock-agentcore-control list-agent-runtimes \
    --region "${REGION}" \
    --query "agentRuntimes[?agentRuntimeName=='${RUNTIME_NAME_EFS}'].agentRuntimeArn | [0]" \
    --output text 2>/dev/null)" && [[ -n "${RUNTIME_ARN_EFS}" && "${RUNTIME_ARN_EFS}" != "None" ]]; then
  RUNTIME_ID_EFS="$(aws bedrock-agentcore-control list-agent-runtimes \
    --region "${REGION}" \
    --query "agentRuntimes[?agentRuntimeName=='${RUNTIME_NAME_EFS}'].agentRuntimeId | [0]" \
    --output text 2>/dev/null || echo "")"
fi

RUNTIME_ARN_TIERED=""
RUNTIME_ID_TIERED=""
if RUNTIME_ARN_TIERED="$(aws bedrock-agentcore-control list-agent-runtimes \
    --region "${REGION}" \
    --query "agentRuntimes[?agentRuntimeName=='${RUNTIME_NAME_TIERED}'].agentRuntimeArn | [0]" \
    --output text 2>/dev/null)" && [[ -n "${RUNTIME_ARN_TIERED}" && "${RUNTIME_ARN_TIERED}" != "None" ]]; then
  RUNTIME_ID_TIERED="$(aws bedrock-agentcore-control list-agent-runtimes \
    --region "${REGION}" \
    --query "agentRuntimes[?agentRuntimeName=='${RUNTIME_NAME_TIERED}'].agentRuntimeId | [0]" \
    --output text 2>/dev/null || echo "")"
fi

VPC_ID="$(aws ec2 describe-vpcs \
  --region "${REGION}" \
  --filters ${TAG_FILTER} \
  --query 'Vpcs[0].VpcId' \
  --output text 2>/dev/null || echo "None")"
[[ "${VPC_ID}" == "None" ]] && VPC_ID=""

SUBNET_IDS=()
if [[ -n "${VPC_ID}" ]]; then
  mapfile -t SUBNET_IDS < <(aws ec2 describe-subnets \
    --region "${REGION}" \
    --filters "Name=vpc-id,Values=${VPC_ID}" "${TAG_FILTER}" \
    --query 'Subnets[].SubnetId' \
    --output text 2>/dev/null | tr '\t' '\n' | grep -v '^$' || true)
fi

SG_ID=""
SG_ID_TIERED=""
if [[ -n "${VPC_ID}" ]]; then
  SG_ID="$(aws ec2 describe-security-groups \
    --region "${REGION}" \
    --filters "Name=vpc-id,Values=${VPC_ID}" "Name=group-name,Values=microvm-efs-sg" \
    --query 'SecurityGroups[0].GroupId' \
    --output text 2>/dev/null || echo "None")"
  [[ "${SG_ID}" == "None" ]] && SG_ID=""
  SG_ID_TIERED="$(aws ec2 describe-security-groups \
    --region "${REGION}" \
    --filters "Name=vpc-id,Values=${VPC_ID}" "Name=group-name,Values=microvm-tiered-sg" \
    --query 'SecurityGroups[0].GroupId' \
    --output text 2>/dev/null || echo "None")"
  [[ "${SG_ID_TIERED}" == "None" ]] && SG_ID_TIERED=""
fi

FS_ID="$(aws efs describe-file-systems \
  --region "${REGION}" \
  --query 'FileSystems[?Tags && length(Tags[?Key==`microvm` && Value==`managed`])>`0`].FileSystemId | [0]' \
  --output text 2>/dev/null || echo "None")"
[[ "${FS_ID}" == "None" ]] && FS_ID=""

MOUNT_TARGET_IDS=()
if [[ -n "${FS_ID}" ]]; then
  mapfile -t MOUNT_TARGET_IDS < <(aws efs describe-mount-targets \
    --region "${REGION}" \
    --file-system-id "${FS_ID}" \
    --query 'MountTargets[].MountTargetId' \
    --output text 2>/dev/null | tr '\t' '\n' | grep -v '^$' || true)
fi

AP_ID=""
if [[ -n "${FS_ID}" ]]; then
  AP_ID="$(aws efs describe-access-points \
    --region "${REGION}" \
    --file-system-id "${FS_ID}" \
    --query "AccessPoints[?Tags && length(Tags[?Key=='microvm' && Value=='managed'])>\`0\`].AccessPointId | [0]" \
    --output text 2>/dev/null || echo "None")"
  [[ "${AP_ID}" == "None" ]] && AP_ID=""
fi

# ---------------------------------------------------------------------------
# S3 Files filesystem (NEW API -- shape may shift; mirror setup_tiered.sh).
#
# TODO at first real run: if the `aws s3files` API doesn't match, adjust
# the command names / JMESPath here and in the delete steps below.
# ---------------------------------------------------------------------------
S3FILES_FS_ID="$(aws s3files list-file-systems \
  --region "${REGION}" \
  --query "fileSystems[?Tags && length(Tags[?Key=='microvm' && Value=='managed'])>\`0\`].fileSystemId | [0]" \
  --output text 2>/dev/null || echo "None")"
[[ "${S3FILES_FS_ID}" == "None" ]] && S3FILES_FS_ID=""

# S3 access point: deterministic name "microvm-workspace".
S3_AP_EXISTS=0
if aws s3control get-access-point \
    --account-id "${ACCOUNT_ID}" \
    --region "${REGION}" \
    --name "${AP_NAME}" >/dev/null 2>&1; then
  S3_AP_EXISTS=1
fi

# S3 bucket: deterministic name microvm-workspace-<account>-<region>.
S3_BUCKET_EXISTS=0
if aws s3api head-bucket --bucket "${BUCKET_NAME}" --region "${REGION}" >/dev/null 2>&1; then
  S3_BUCKET_EXISTS=1
fi

# VPC gateway endpoint for S3 (only if tagged microvm=managed).
S3_ENDPOINT_ID=""
if [[ -n "${VPC_ID}" ]]; then
  S3_ENDPOINT_ID="$(aws ec2 describe-vpc-endpoints \
    --region "${REGION}" \
    --filters "Name=vpc-id,Values=${VPC_ID}" "Name=service-name,Values=com.amazonaws.${REGION}.s3" "${TAG_FILTER}" \
    --query 'VpcEndpoints[0].VpcEndpointId' \
    --output text 2>/dev/null || echo "None")"
  [[ "${S3_ENDPOINT_ID}" == "None" ]] && S3_ENDPOINT_ID=""
fi

ROLE_EXISTS=0
if aws iam get-role --role-name "${ROLE_NAME}" >/dev/null 2>&1; then
  ROLE_EXISTS=1
fi

REPO_EXISTS=0
if aws ecr describe-repositories --region "${REGION}" --repository-names "${REPO_NAME}" >/dev/null 2>&1; then
  REPO_EXISTS=1
fi

# ---------------------------------------------------------------------------
# Summary + confirmation
# ---------------------------------------------------------------------------

step "Plan"
echo "Region: ${REGION}"
echo "Account: ${ACCOUNT_ID}"
echo
echo "Will attempt to delete (missing items skipped):"
printf "  AgentCore runtime  : %s -> %s\n" "${RUNTIME_NAME}"        "${RUNTIME_ID:-<not found>}"
printf "  AgentCore runtime  : %s -> %s\n" "${RUNTIME_NAME_EFS}"    "${RUNTIME_ID_EFS:-<not found>}"
printf "  AgentCore runtime  : %s -> %s\n" "${RUNTIME_NAME_TIERED}" "${RUNTIME_ID_TIERED:-<not found>}"
printf "  EFS access point   : %s\n" "${AP_ID:-<not found>}"
if [[ "${#MOUNT_TARGET_IDS[@]}" -gt 0 ]]; then
  printf "  EFS mount targets  : %s\n" "${MOUNT_TARGET_IDS[*]}"
else
  printf "  EFS mount targets  : <none>\n"
fi
printf "  EFS filesystem     : %s\n" "${FS_ID:-<not found>}"
printf "  S3 Files filesystem: %s\n" "${S3FILES_FS_ID:-<not found>}"
if [[ "${S3_AP_EXISTS}" -eq 1 ]]; then
  printf "  S3 access point    : %s\n" "${AP_NAME}"
else
  printf "  S3 access point    : <not found>\n"
fi
if [[ "${S3_BUCKET_EXISTS}" -eq 1 ]]; then
  printf "  S3 bucket          : %s (objects + versions will be deleted)\n" "${BUCKET_NAME}"
else
  printf "  S3 bucket          : %s (<not found>)\n" "${BUCKET_NAME}"
fi
printf "  S3 gateway endpoint: %s\n" "${S3_ENDPOINT_ID:-<not found>}"
printf "  Security group     : %s (microvm-efs-sg)\n"    "${SG_ID:-<not found>}"
printf "  Security group     : %s (microvm-tiered-sg)\n" "${SG_ID_TIERED:-<not found>}"
if [[ "${#SUBNET_IDS[@]}" -gt 0 ]]; then
  printf "  Subnets            : %s\n" "${SUBNET_IDS[*]}"
else
  printf "  Subnets            : <none>\n"
fi
printf "  VPC                : %s\n" "${VPC_ID:-<not found>}"
if [[ "${ROLE_EXISTS}" -eq 1 ]]; then
  printf "  IAM role           : %s (inline: ecr-pull, microvm-efs, microvm-tiered; detach: CloudWatchLogsFullAccess)\n" "${ROLE_NAME}"
else
  printf "  IAM role           : <not found>\n"
fi
if [[ "${REPO_EXISTS}" -eq 1 ]]; then
  printf "  ECR repository     : %s (and all images)\n" "${REPO_NAME}"
else
  printf "  ECR repository     : <not found>\n"
fi
echo

if [[ "${ASSUME_YES}" -ne 1 ]]; then
  warn "This is destructive and cannot be undone."
  read -r -p "Proceed? Type 'yes' to continue: " confirm
  if [[ "${confirm}" != "yes" ]]; then
    echo "Aborted."
    exit 1
  fi
fi

# ---------------------------------------------------------------------------
# 1. AgentCore runtimes
# ---------------------------------------------------------------------------

step "Delete AgentCore runtimes"
if [[ -n "${RUNTIME_ID}" ]]; then
  confirm_run "Deletes the alias-mode AgentCore runtime ${RUNTIME_NAME} (${RUNTIME_ID})." \
    aws bedrock-agentcore-control delete-agent-runtime \
      --region "${REGION}" \
      --agent-runtime-id "${RUNTIME_ID}" || warn "delete failed (continuing)"
else
  info "Runtime ${RUNTIME_NAME} not found, skipping."
fi
if [[ -n "${RUNTIME_ID_EFS}" ]]; then
  confirm_run "Deletes the EFS-mode AgentCore runtime ${RUNTIME_NAME_EFS} (${RUNTIME_ID_EFS})." \
    aws bedrock-agentcore-control delete-agent-runtime \
      --region "${REGION}" \
      --agent-runtime-id "${RUNTIME_ID_EFS}" || warn "delete failed (continuing)"
else
  info "Runtime ${RUNTIME_NAME_EFS} not found, skipping."
fi
if [[ -n "${RUNTIME_ID_TIERED}" ]]; then
  confirm_run "Deletes the tiered-mode AgentCore runtime ${RUNTIME_NAME_TIERED} (${RUNTIME_ID_TIERED})." \
    aws bedrock-agentcore-control delete-agent-runtime \
      --region "${REGION}" \
      --agent-runtime-id "${RUNTIME_ID_TIERED}" || warn "delete failed (continuing)"
else
  info "Runtime ${RUNTIME_NAME_TIERED} not found, skipping."
fi

# ---------------------------------------------------------------------------
# 2. EFS access point
# ---------------------------------------------------------------------------

step "Delete EFS access point"
if [[ -n "${AP_ID}" ]]; then
  AP_TAGS="$(aws efs describe-access-points \
    --region "${REGION}" \
    --access-point-id "${AP_ID}" \
    --query 'AccessPoints[0].Tags' \
    --output json 2>/dev/null || echo "[]")"
  if has_managed_tag "${AP_TAGS}"; then
    confirm_run "Deletes the EFS access point ${AP_ID}." \
      aws efs delete-access-point \
        --region "${REGION}" \
        --access-point-id "${AP_ID}" || warn "delete failed (continuing)"
  else
    err "Access point ${AP_ID} is missing the microvm=managed tag. Refusing to delete."
  fi
else
  info "No access point to delete."
fi

# ---------------------------------------------------------------------------
# 3. EFS mount targets (poll until gone)
# ---------------------------------------------------------------------------

step "Delete EFS mount targets"
if [[ "${#MOUNT_TARGET_IDS[@]}" -gt 0 ]]; then
  for MT in "${MOUNT_TARGET_IDS[@]}"; do
    confirm_run "Deletes mount target ${MT}." \
      aws efs delete-mount-target \
        --region "${REGION}" \
        --mount-target-id "${MT}" || warn "delete failed (continuing)"
  done

  info "Waiting for mount targets to fully detach (frees the ENIs the SG depends on)..."
  for _ in $(seq 1 60); do
    REMAINING="$(aws efs describe-mount-targets \
      --region "${REGION}" \
      --file-system-id "${FS_ID}" \
      --query 'length(MountTargets)' \
      --output text 2>/dev/null || echo "0")"
    if [[ "${REMAINING}" == "0" ]]; then
      ok "All mount targets cleared."
      break
    fi
    info "  ${REMAINING} mount target(s) still detaching, sleeping 5s..."
    sleep 5
  done
else
  info "No mount targets to delete."
fi

# ---------------------------------------------------------------------------
# 4. EFS filesystem
# ---------------------------------------------------------------------------

step "Delete EFS filesystem"
if [[ -n "${FS_ID}" ]]; then
  FS_TAGS="$(aws efs describe-file-systems \
    --region "${REGION}" \
    --file-system-id "${FS_ID}" \
    --query 'FileSystems[0].Tags' \
    --output json 2>/dev/null || echo "[]")"
  if has_managed_tag "${FS_TAGS}"; then
    confirm_run "Deletes the EFS filesystem ${FS_ID}." \
      aws efs delete-file-system \
        --region "${REGION}" \
        --file-system-id "${FS_ID}" || warn "delete failed (continuing)"
  else
    err "Filesystem ${FS_ID} is missing the microvm=managed tag. Refusing to delete."
  fi
else
  info "No filesystem to delete."
fi

# ---------------------------------------------------------------------------
# 5. S3 Files association (must be removed before the filesystem)
#
# TODO at first real run: if the `aws s3files` API doesn't match (the
# command name / flag shape / response field names), adjust here and in
# the steps below. setup_tiered.sh has the same TODO.
# ---------------------------------------------------------------------------

step "Delete S3 Files association"
if [[ -n "${S3FILES_FS_ID}" ]]; then
  # List associations; for each, disassociate. We don't tag-check the
  # filesystem here because the FS itself is tag-checked below and we
  # only got here via the tag-filtered discovery query.
  ASSOC_IDS_RAW="$(aws s3files list-associations \
    --region "${REGION}" \
    --filesystem-id "${S3FILES_FS_ID}" \
    --query 'associations[].associationId' \
    --output text 2>/dev/null || echo "")"
  if [[ -n "${ASSOC_IDS_RAW}" && "${ASSOC_IDS_RAW}" != "None" ]]; then
    for ASSOC in ${ASSOC_IDS_RAW}; do
      confirm_run "Disassociates ${ASSOC} from S3 Files filesystem ${S3FILES_FS_ID}." \
        aws s3files disassociate \
          --region "${REGION}" \
          --filesystem-id "${S3FILES_FS_ID}" \
          --association-id "${ASSOC}" || warn "disassociate failed (continuing)"
    done
  else
    info "No associations to remove."
  fi
else
  info "No S3 Files filesystem -- skipping association cleanup."
fi

# ---------------------------------------------------------------------------
# 6. S3 Files filesystem
# ---------------------------------------------------------------------------

step "Delete S3 Files filesystem"
if [[ -n "${S3FILES_FS_ID}" ]]; then
  # The discovery query already required microvm=managed; re-fetch the
  # tag block for an explicit pre-delete check (mirrors EFS pattern).
  S3FILES_TAGS="$(aws s3files list-file-systems \
    --region "${REGION}" \
    --query "fileSystems[?fileSystemId=='${S3FILES_FS_ID}'].Tags | [0]" \
    --output json 2>/dev/null || echo "[]")"
  if has_managed_tag "${S3FILES_TAGS}"; then
    confirm_run "Deletes the S3 Files filesystem ${S3FILES_FS_ID}." \
      aws s3files delete-file-system \
        --region "${REGION}" \
        --filesystem-id "${S3FILES_FS_ID}" || warn "delete failed (continuing)"
  else
    err "S3 Files filesystem ${S3FILES_FS_ID} is missing the microvm=managed tag. Refusing to delete."
  fi
else
  info "No S3 Files filesystem to delete."
fi

# ---------------------------------------------------------------------------
# 7. S3 access point (microvm-workspace)
# ---------------------------------------------------------------------------

step "Delete S3 access point"
if [[ "${S3_AP_EXISTS}" -eq 1 ]]; then
  # s3control tag listing requires the access-point ARN.
  S3_AP_ARN="$(aws s3control get-access-point \
    --account-id "${ACCOUNT_ID}" \
    --region "${REGION}" \
    --name "${AP_NAME}" \
    --query 'AccessPointArn' \
    --output text 2>/dev/null || echo "")"
  S3_AP_TAGS="[]"
  if [[ -n "${S3_AP_ARN}" && "${S3_AP_ARN}" != "None" ]]; then
    S3_AP_TAGS="$(aws s3control get-access-point-tagging \
      --account-id "${ACCOUNT_ID}" \
      --region "${REGION}" \
      --name "${AP_NAME}" \
      --query 'Tags' \
      --output json 2>/dev/null || echo "[]")"
  fi
  if has_managed_tag "${S3_AP_TAGS}"; then
    confirm_run "Deletes the S3 access point ${AP_NAME}." \
      aws s3control delete-access-point \
        --account-id "${ACCOUNT_ID}" \
        --region "${REGION}" \
        --name "${AP_NAME}" || warn "delete failed (continuing)"
  else
    err "S3 access point ${AP_NAME} is missing the microvm=managed tag. Refusing to delete."
  fi
else
  info "No S3 access point to delete."
fi

# ---------------------------------------------------------------------------
# 8. S3 bucket (microvm-workspace-<account>-<region>)
#
# Bucket has versioning ENABLED -- a plain `aws s3 rm --recursive` leaves
# object versions and delete-markers behind, which would block delete-bucket.
# We use list-object-versions + delete-objects to clear everything, then
# delete the bucket. If the bucket is non-empty when discovered, force the
# user to type the bucket name (unless --yes was passed).
# ---------------------------------------------------------------------------

step "Delete S3 bucket"
if [[ "${S3_BUCKET_EXISTS}" -eq 1 ]]; then
  BUCKET_TAGS="$(aws s3api get-bucket-tagging \
    --bucket "${BUCKET_NAME}" \
    --region "${REGION}" \
    --query 'TagSet' \
    --output json 2>/dev/null || echo "[]")"
  if ! has_managed_tag "${BUCKET_TAGS}"; then
    err "S3 bucket ${BUCKET_NAME} is missing the microvm=managed tag. Refusing to delete."
  else
    # Count objects (one page is enough -- we only need empty/non-empty).
    OBJ_COUNT="$(aws s3api list-object-versions \
      --bucket "${BUCKET_NAME}" \
      --region "${REGION}" \
      --max-items 1 \
      --query 'length(Versions || `[]`) + length(DeleteMarkers || `[]`)' \
      --output text 2>/dev/null || echo "0")"
    PROCEED=1
    if [[ "${OBJ_COUNT}" != "0" && "${ASSUME_YES}" -ne 1 ]]; then
      printf "\n${C_RED}${C_BOLD}!!  WARNING  !!${C_RESET}\n"
      printf "${C_YELLOW}Bucket ${BUCKET_NAME} is NOT empty.${C_RESET}\n"
      printf "${C_YELLOW}This bucket contains your workspaces and snapshots.${C_RESET}\n"
      printf "${C_YELLOW}Deleting it will permanently destroy all sandbox state.${C_RESET}\n\n"
      read -r -p "Type the bucket name (${BUCKET_NAME}) to confirm: " typed_name
      if [[ "${typed_name}" != "${BUCKET_NAME}" ]]; then
        warn "Bucket name did not match. Skipping bucket delete."
        PROCEED=0
      fi
    fi
    if [[ "${PROCEED}" -eq 1 ]]; then
      # Empty the bucket: delete all object versions + delete-markers in
      # batches of up to 1000 (the delete-objects API limit).
      info "Emptying ${BUCKET_NAME} (versions + delete-markers)..."
      while :; do
        VERSIONS_JSON="$(aws s3api list-object-versions \
          --bucket "${BUCKET_NAME}" \
          --region "${REGION}" \
          --max-items 1000 \
          --output json 2>/dev/null || echo '{}')"
        DELETE_PAYLOAD="$(echo "${VERSIONS_JSON}" | jq -c '
          {Objects: (
            ((.Versions // []) | map({Key, VersionId})) +
            ((.DeleteMarkers // []) | map({Key, VersionId}))
          ), Quiet: true}
        ')"
        N="$(echo "${DELETE_PAYLOAD}" | jq '.Objects | length')"
        if [[ "${N}" -eq 0 ]]; then
          break
        fi
        aws s3api delete-objects \
          --bucket "${BUCKET_NAME}" \
          --region "${REGION}" \
          --delete "${DELETE_PAYLOAD}" >/dev/null \
          || { warn "delete-objects batch failed (continuing)"; break; }
        info "  removed ${N} object version(s)/marker(s)..."
      done
      confirm_run "Deletes the S3 bucket ${BUCKET_NAME}." \
        aws s3api delete-bucket \
          --bucket "${BUCKET_NAME}" \
          --region "${REGION}" || warn "delete failed (continuing)"
    fi
  fi
else
  info "Bucket ${BUCKET_NAME} not found, skipping."
fi

# ---------------------------------------------------------------------------
# 9. VPC gateway endpoint for S3
# ---------------------------------------------------------------------------

step "Delete VPC gateway endpoint (S3)"
if [[ -n "${S3_ENDPOINT_ID}" ]]; then
  # Tag pre-check: the describe filter already required microvm=managed,
  # but be explicit for symmetry with the other resources.
  EP_TAGS="$(aws ec2 describe-vpc-endpoints \
    --region "${REGION}" \
    --vpc-endpoint-ids "${S3_ENDPOINT_ID}" \
    --query 'VpcEndpoints[0].Tags' \
    --output json 2>/dev/null || echo "[]")"
  if has_managed_tag "${EP_TAGS}"; then
    confirm_run "Deletes the S3 VPC gateway endpoint ${S3_ENDPOINT_ID}." \
      aws ec2 delete-vpc-endpoints \
        --region "${REGION}" \
        --vpc-endpoint-ids "${S3_ENDPOINT_ID}" || warn "delete failed (continuing)"
  else
    err "VPC endpoint ${S3_ENDPOINT_ID} is missing the microvm=managed tag. Refusing to delete."
  fi
else
  info "No S3 gateway endpoint to delete."
fi

# ---------------------------------------------------------------------------
# 10. Security groups (microvm-efs-sg, microvm-tiered-sg)
# ---------------------------------------------------------------------------

# Helper: tag-check + retry-delete a single SG. Mirrors the original
# retry loop -- ENIs from mount targets / VPC endpoints can linger a few
# seconds after their parents report deleted.
delete_sg() {
  local sg_id="$1"
  local sg_tags
  sg_tags="$(aws ec2 describe-security-groups \
    --region "${REGION}" \
    --group-ids "${sg_id}" \
    --query 'SecurityGroups[0].Tags' \
    --output json 2>/dev/null || echo "[]")"
  if ! has_managed_tag "${sg_tags}"; then
    err "SG ${sg_id} is missing the microvm=managed tag. Refusing to delete."
    return
  fi
  local sg_deleted=0
  for _ in $(seq 1 12); do
    if aws ec2 delete-security-group \
        --region "${REGION}" \
        --group-id "${sg_id}" 2>/dev/null; then
      sg_deleted=1
      ok "Deleted SG ${sg_id}."
      break
    fi
    info "  SG ${sg_id} still has dependents, sleeping 5s..."
    sleep 5
  done
  if [[ "${sg_deleted}" -ne 1 ]]; then
    warn "Could not delete SG ${sg_id} (still has dependents). Retry later or delete manually."
  fi
}

step "Delete security groups"
if [[ -n "${SG_ID}" ]]; then
  delete_sg "${SG_ID}"
else
  info "No microvm-efs-sg to delete."
fi
if [[ -n "${SG_ID_TIERED}" ]]; then
  delete_sg "${SG_ID_TIERED}"
else
  info "No microvm-tiered-sg to delete."
fi

# ---------------------------------------------------------------------------
# 11. Subnets
# ---------------------------------------------------------------------------

step "Delete subnets"
if [[ "${#SUBNET_IDS[@]}" -gt 0 ]]; then
  for SN in "${SUBNET_IDS[@]}"; do
    SN_TAGS="$(aws ec2 describe-subnets \
      --region "${REGION}" \
      --subnet-ids "${SN}" \
      --query 'Subnets[0].Tags' \
      --output json 2>/dev/null || echo "[]")"
    if ! has_managed_tag "${SN_TAGS}"; then
      err "Subnet ${SN} is missing the microvm=managed tag. Refusing to delete."
      continue
    fi
    SN_DELETED=0
    for _ in $(seq 1 12); do
      if aws ec2 delete-subnet \
          --region "${REGION}" \
          --subnet-id "${SN}" 2>/dev/null; then
        SN_DELETED=1
        ok "Deleted subnet ${SN}."
        break
      fi
      info "  subnet ${SN} still has dependents, sleeping 5s..."
      sleep 5
    done
    if [[ "${SN_DELETED}" -ne 1 ]]; then
      warn "Could not delete subnet ${SN}. Retry later or delete manually."
    fi
  done
else
  info "No subnets to delete."
fi

# ---------------------------------------------------------------------------
# 12. VPC
# ---------------------------------------------------------------------------

step "Delete VPC"
if [[ -n "${VPC_ID}" ]]; then
  VPC_TAGS="$(aws ec2 describe-vpcs \
    --region "${REGION}" \
    --vpc-ids "${VPC_ID}" \
    --query 'Vpcs[0].Tags' \
    --output json 2>/dev/null || echo "[]")"
  if has_managed_tag "${VPC_TAGS}"; then
    confirm_run "Deletes the VPC ${VPC_ID}." \
      aws ec2 delete-vpc \
        --region "${REGION}" \
        --vpc-id "${VPC_ID}" || warn "delete failed (VPC may still have dependents)"
  else
    err "VPC ${VPC_ID} is missing the microvm=managed tag. Refusing to delete."
  fi
else
  info "No VPC to delete."
fi

# ---------------------------------------------------------------------------
# 13. IAM role (only policies setup.sh / setup_efs.sh / setup_tiered.sh created)
# ---------------------------------------------------------------------------

step "Delete IAM role"
if [[ "${ROLE_EXISTS}" -eq 1 ]]; then
  # Inline policies created by the setup scripts.
  for POLICY in ecr-pull microvm-efs microvm-tiered; do
    if aws iam get-role-policy --role-name "${ROLE_NAME}" --policy-name "${POLICY}" >/dev/null 2>&1; then
      confirm_run "Deletes inline policy ${POLICY} from ${ROLE_NAME}." \
        aws iam delete-role-policy \
          --role-name "${ROLE_NAME}" \
          --policy-name "${POLICY}" || warn "delete failed (continuing)"
    else
      info "Inline policy ${POLICY} not present, skipping."
    fi
  done

  # Managed policy attached by setup.sh.
  MANAGED_ARN="arn:aws:iam::aws:policy/CloudWatchLogsFullAccess"
  if aws iam list-attached-role-policies --role-name "${ROLE_NAME}" \
        --query "AttachedPolicies[?PolicyArn=='${MANAGED_ARN}'] | [0].PolicyArn" \
        --output text 2>/dev/null | grep -Fxq "${MANAGED_ARN}"; then
    confirm_run "Detaches CloudWatchLogsFullAccess from ${ROLE_NAME}." \
      aws iam detach-role-policy \
        --role-name "${ROLE_NAME}" \
        --policy-arn "${MANAGED_ARN}" || warn "detach failed (continuing)"
  else
    info "CloudWatchLogsFullAccess not attached, skipping."
  fi

  # Sanity check: warn if any unexpected inline or managed policies remain.
  REMAINING_INLINE="$(aws iam list-role-policies --role-name "${ROLE_NAME}" \
    --query 'PolicyNames' --output text 2>/dev/null || echo "")"
  REMAINING_ATTACHED="$(aws iam list-attached-role-policies --role-name "${ROLE_NAME}" \
    --query 'AttachedPolicies[].PolicyArn' --output text 2>/dev/null || echo "")"
  if [[ -n "${REMAINING_INLINE}" && "${REMAINING_INLINE}" != "None" ]]; then
    warn "Role still has inline policies we did not create: ${REMAINING_INLINE}"
    warn "Leaving them in place. delete-role will fail unless you remove them."
  fi
  if [[ -n "${REMAINING_ATTACHED}" && "${REMAINING_ATTACHED}" != "None" ]]; then
    warn "Role still has attached managed policies we did not attach: ${REMAINING_ATTACHED}"
    warn "Leaving them in place. delete-role will fail unless you detach them."
  fi
  if [[ -z "${REMAINING_INLINE}" || "${REMAINING_INLINE}" == "None" ]] && \
     [[ -z "${REMAINING_ATTACHED}" || "${REMAINING_ATTACHED}" == "None" ]]; then
    confirm_run "Deletes the IAM role ${ROLE_NAME}." \
      aws iam delete-role \
        --role-name "${ROLE_NAME}" || warn "delete failed (continuing)"
  else
    warn "Skipping delete-role for ${ROLE_NAME} (foreign policies remain)."
  fi
else
  info "Role ${ROLE_NAME} not found, skipping."
fi

# ---------------------------------------------------------------------------
# 14. ECR repository
# ---------------------------------------------------------------------------

step "Delete ECR repository"
if [[ "${REPO_EXISTS}" -eq 1 ]]; then
  # Tag re-check: REPO_NAME defaults to "microvm-shell" which could collide with
  # an unrelated repo on a shared account. Only delete if we tagged it ourselves
  # in setup.sh (microvm=managed).
  REPO_ARN="$(aws ecr describe-repositories \
    --region "${REGION}" \
    --repository-names "${REPO_NAME}" \
    --query 'repositories[0].repositoryArn' \
    --output text 2>/dev/null || echo "")"
  REPO_TAGS=""
  if [[ -n "${REPO_ARN}" && "${REPO_ARN}" != "None" ]]; then
    REPO_TAGS="$(aws ecr list-tags-for-resource \
      --resource-arn "${REPO_ARN}" \
      --region "${REGION}" \
      --query "Tags[?Key=='microvm'].Value | [0]" \
      --output text 2>/dev/null || echo "")"
  fi
  if [[ "${REPO_TAGS}" == "managed" ]]; then
    confirm_run "Deletes ECR repository ${REPO_NAME} and all images it contains." \
      aws ecr delete-repository \
        --region "${REGION}" \
        --repository-name "${REPO_NAME}" \
        --force || warn "delete failed (continuing)"
  else
    warn "ECR repo ${REPO_NAME} exists but isn't tagged microvm=managed — refusing to delete; tag it manually or delete it yourself."
  fi
else
  info "Repository ${REPO_NAME} not found, skipping."
fi

cat <<EOF

${C_GREEN}${C_BOLD}Teardown complete.${C_RESET}

If any step warned about leftover dependents (ENIs, foreign IAM policies,
etc.), inspect those resources in the console and rerun this script — it
is safe to run multiple times.

EOF
