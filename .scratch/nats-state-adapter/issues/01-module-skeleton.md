# 01 - Module skeleton and workspace wiring

Status: ready-for-agent

## Summary

Create the `github.com/coder/chat/state/nats` Go module and wire it into the workspace, mirroring `state/redis` and `state/postgres`.

## Tasks

- Add `state/nats/go.mod` (module `github.com/coder/chat/state/nats`, go 1.26.3), requiring `github.com/coder/chat`, `github.com/nats-io/nats.go`, and (test) `github.com/nats-io/nats-server/v2`.
- Add `state/nats/doc.go` with the one-line package doc.
- Edit `go.work`: add `./state/nats` and `./examples/slack-nats-state` to `use(...)`, plus `replace github.com/coder/chat/state/nats v0.0.0 => ./state/nats`.
- Edit `mise.toml`: add a `test:nats` task; add the module to `test:adapters` and the example to `test:examples`.

## Acceptance

- `go work sync` and `go build ./...` succeed at the workspace root.

## Comments

Implemented on branch `nats-state-adapter-impl`.
