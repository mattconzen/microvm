# microvm

A simple PoC CLI for testing microvm / agent sandbox provisioning on various substrates, starting with AWS AgentCore Runtime — for exploration only.

## Build

```sh
go build ./...
```

## Usage

```sh
./microvm --help
```

## Snapshot modes

Pick a snapshot backend at runtime registration:

```sh
microvm login --snapshot-mode s3 --snapshot-bucket my-bucket
microvm login --snapshot-mode efs --efs-id fs-0123...   # PR2 (in progress)
microvm login --snapshot-mode tiered --snapshot-bucket b # PR3 (in progress)
```

Modes:

- `none` (default) — session aliases only. Compatible with existing deployments;
  matches today's behavior. Snapshot/resume are no-ops at the AWS layer.
- `s3` — tar+gzip the working tree to S3 on snapshot, download+restore on resume.
  Durable across runtime evictions. Requires `--snapshot-bucket`.
- `efs` — EFS-backed snapshots via `rsync` on a shared access point.
  Requires VPC-mode runtime (set up via `scripts/setup_efs.sh`).
  Requires `--efs-access-point-arn`.
- `tiered` — fast session-local tier + async S3 durability. (PR3 — in progress)

Mode is a property of the AgentCore runtime, not the sandbox: every sandbox
under a runtime uses the same snapshot mode. Resuming a snapshot taken under
a different mode is rejected with a clear error. See
[docs/plans/2026-05-23-snapshot-modes-design.md](docs/plans/2026-05-23-snapshot-modes-design.md)
for the architecture and rationale.

### EFS setup

Provision EFS once per environment:

    bash scripts/setup_efs.sh

The script creates a VPC, two subnets, an EFS filesystem with mount targets in
both AZs, a security group, an EFS access point, the IAM permissions, and a
new AgentCore runtime named `microvm-shell-efs` configured for VPC mode +
EFS mount at `/mnt/efs`. It prints the `microvm login` invocation to run.

### Teardown

To destroy all managed AWS resources (runtimes, EFS filesystems, mount targets,
VPCs, subnets, security groups, IAM roles, ECR repos) created by the setup
scripts, run:

    bash scripts/teardown.sh

Pass `--yes` to skip the confirmation prompt. The script is tag-scoped: it only
deletes resources tagged `microvm=managed` (or, for runtimes, the well-known
names created by the setup scripts), so it will not touch anything you
provisioned by hand.

## Layout

- `cli/` — cobra commands (`sbx create`, `sbx exec`, `sbx cp`, `sbx snapshot`, etc.)
- `backend/` — pluggable substrate backends; currently `backend/aws` targets AgentCore Runtime
- `shellagent/` — Python agent image that runs inside the sandbox
- `scripts/setup.sh` — interactive AWS provisioning helper
- `config/`, `state/`, `obs/` — config loading, local state (bbolt), logging/metrics
