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

## Layout

- `cli/` — cobra commands (`sbx create`, `sbx exec`, `sbx cp`, `sbx snapshot`, etc.)
- `backend/` — pluggable substrate backends; currently `backend/aws` targets AgentCore Runtime
- `shellagent/` — Python agent image that runs inside the sandbox
- `scripts/setup.sh` — interactive AWS provisioning helper
- `config/`, `state/`, `obs/` — config loading, local state (bbolt), logging/metrics
