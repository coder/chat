# state

## Overview

`state` groups the implementations of the `chat.State` interface from
`github.com/coder/chat`. The runtime uses that interface for thread
subscriptions, event dedupe (`MarkEvent`), and token-checked thread locks
(`AcquireLock`, `ExtendLock`, `ReleaseLock`). This directory has no Go files
of its own. `memory` is part of the root Go module. `nats`, `postgres`, and
`redis` are separate Go modules, each with its own `go.mod`, so their driver
dependencies stay out of the root module. Each package declares
`var _ chat.State = (*State)(nil)` and runs the shared conformance suite from
`github.com/coder/chat/internal/statetest` in its tests.

Shared behavior across all four backends:

- Empty event IDs, lock keys, and lease fields fail with a
  `"<backend> state: ... is required"` error. Non-positive TTLs fail with
  `"... must be positive"`. Empty thread IDs fail the same way, except in
  `memory`'s `IsThreadSubscribed`, which returns `false`.
- `MarkEvent` and `AcquireLock` are first-writer-wins. A duplicate event or a
  held lock returns `false` with a `nil` error.
- `ExtendLock` and `ReleaseLock` act only when the stored token equals
  `lease.Token`. A mismatch or a missing lock returns `false` with a `nil`
  error.
- Subscriptions never expire.
- `Shutdown` on the three networked backends closes the caller-supplied
  connection, pool, or client.

## Subdirectories & Boundaries

- memory: [./memory/README.md](./memory/README.md) - In-process maps under one
  mutex, for tests and local development. Expired records are pruned lazily.
  `Shutdown` makes later calls fail with `"memory state: closed"`.
- nats: [./nats/README.md](./nats/README.md) - NATS JetStream KV buckets
  `<prefix>_sub`, `<prefix>_event`, and `<prefix>_lock` (default prefix
  `chat`). Expiry uses bucket TTLs from `Options`, not per-call `ttl`
  arguments. Locks use revision-gated update and delete.
- postgres: [./postgres/README.md](./postgres/README.md) - Tables
  `chat_runtime_subscriptions`, `chat_runtime_events`, and
  `chat_runtime_locks`, created on `New` under an advisory lock. Rows are
  scoped by `Namespace` (default `chat`). Expiry uses the database `now()`.
- redis: [./redis/README.md](./redis/README.md) - Keys
  `<prefix>:sub:<id>`, `<prefix>:event:<id>`, and `<prefix>:lock:<key>`
  (default prefix `chat`). Expiry uses Redis key TTLs. Lua scripts check the
  token before `PEXPIRE` or `DEL`.

## Verification

- `go test ./state/memory/...` (from the repository root)
- `cd state/nats && go test ./...` (embedded NATS server, no Docker)
- `cd state/postgres && go test ./...` (Testcontainers, needs Docker)
- `cd state/redis && go test ./...` (Testcontainers test needs Docker, a
  miniredis test does not)
