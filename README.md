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
microvm login --snapshot-mode efs --efs-access-point-arn arn:aws:elasticfilesystem:us-east-1:123:access-point/fsap-...
microvm login --snapshot-mode tiered --s3-files-access-point-arn arn:aws:s3:us-east-1:123:accesspoint/ap-... --s3-files-bucket my-bucket
```

Modes:

- `none` (default) — session aliases only. Compatible with existing deployments;
  matches today's behavior. Snapshot/resume are no-ops at the AWS layer.
- `s3` — tar+gzip the working tree to S3 on snapshot, download+restore on resume.
  Durable across runtime evictions. Requires `--snapshot-bucket`.
- `efs` — EFS-backed snapshots via `rsync` on a shared access point.
  Requires VPC-mode runtime (set up via `scripts/setup_efs.sh`).
  Requires `--efs-access-point-arn`.
- `tiered` — two-tier durable storage: a fast POSIX cache tier
  (`/var/microvm/cache/<sandbox_id>/`, AgentCore-managed) plus a snapshottable
  S3 Files tier (`/workspace/<sandbox_id>/`). Snapshots are server-side S3
  prefix copies; use `microvm sbx checkpoint <id>` to promote cache artifacts
  into the workspace before snapshotting. Requires `--s3-files-access-point-arn`
  and `--s3-files-bucket`.

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
new AgentCore runtime named `microvm_shell_efs` configured for VPC mode +
EFS mount at `/mnt/efs`. It prints the `microvm login` invocation to run.

(The runtime name uses underscores because AgentCore's name regex
forbids hyphens.)

### Tiered setup

Provision tiered mode once per environment:

    bash scripts/setup_tiered.sh

The script provisions a VPC (or reuses the EFS one), an S3 bucket, an S3
Files filesystem + access point, a VPC gateway endpoint for S3, IAM
permissions, and a new AgentCore runtime named `microvm_shell_tiered`
configured for VPC mode with the S3 Files mount at `/workspace`. It prints
the `microvm login` invocation to run.

### Working with the cache tier (tiered mode)

In tiered mode, two paths are visible inside the sandbox:

- `/workspace/<sandbox_id>/` (default cwd for `microvm sbx exec`) — durable,
  snapshotted. S3-backed, so no rename, no random writes, no SQLite-WAL.
- `/var/microvm/cache/<sandbox_id>/` — fast POSIX scratch. AgentCore-managed,
  preserved for the session lifetime, lost on eviction.

Tools that need real POSIX (`git`, `npm install`, `pip install`, SQLite) run
faster from the cache. To include cache artifacts in the next snapshot, place
them under `cache/<sandbox_id>/promote/` and run `microvm sbx checkpoint <id>`
to rsync them into the workspace.

### Teardown

To destroy all managed AWS resources (runtimes, EFS filesystems, mount targets,
VPCs, subnets, security groups, IAM roles, ECR repos) created by the setup
scripts, run:

    bash scripts/teardown.sh

Pass `--yes` to skip the confirmation prompt. The script is tag-scoped: it only
deletes resources tagged `microvm=managed` (or, for runtimes, the well-known
names created by the setup scripts), so it will not touch anything you
provisioned by hand.

## Sandbox lifecycle

The snapshot modes above are the durability layer. These verbs are the
user-facing operations on top:

| Verb | Effect |
|------|--------|
| `microvm sbx snapshot <id> [--name N]` | capture the workspace at a point in time |
| `microvm sbx resume <snap-id> [--name N]` | create a new sandbox whose workspace starts as that snapshot |
| `microvm sbx fork <id> [--name N]` | shorthand: snapshot + resume in one call |
| `microvm sbx revert <id> --snapshot <snap-id>` | restore an existing sandbox's workspace in place (destructive) |
| `microvm sbx create --from-snapshot <snap-id>` | create a new sandbox starting from a prior snapshot |
| `microvm sbx checkpoint <id>` | (tiered mode) promote cache `promote/` artifacts into the snapshottable workspace |

`fork` and `create --from-snapshot` are equivalent for tier-2 content;
`fork` is the convenience when you already have a running sandbox to
branch from, `create --from-snapshot` is the convenience when you're
starting from a saved snapshot id.

`revert` overwrites the existing sandbox in place. Use `fork` if you
want to keep the current state too.

## Layout

- `cli/` — cobra commands (`sbx create`, `sbx exec`, `sbx cp`, `sbx snapshot`, etc.)
- `backend/` — pluggable substrate backends; currently `backend/aws` targets AgentCore Runtime
- `shellagent/` — Python agent image that runs inside the sandbox
- `scripts/setup.sh` — interactive AWS provisioning helper
- `config/`, `state/`, `obs/` — config loading, local state (bbolt), logging/metrics
