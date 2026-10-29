# tokens

## Overview

Package `tokens` creates random opaque token strings. The state backends use it to
create the owner token of a lock lease in `AcquireLock`. These packages import it:

- `github.com/coder/chat/state/memory` (`memory.go`)
- `github.com/coder/chat/state/nats` (`nats.go`)
- `github.com/coder/chat/state/postgres` (`postgres.go`)

`github.com/coder/chat/state/redis` does not import it.

## Interface

- `func New() (string, error)`: returns a new random token. `[tokens.go:New]`

## Invariants

- On success, the token is the lowercase hex encoding of 16 random bytes, so it is 32 characters long. `[tokens.go:New]`
- If `crypto/rand.Read` fails, `New` returns `""` and the error unchanged. `[tokens.go:New]`

## Side Effects

- Reads 16 bytes from `crypto/rand` (operating-system entropy). `[tokens.go:New]`
- No package-level state, goroutines, locks, network, or disk access. `[tokens.go]`

## Verification

- Run: `go test ./internal/tokens/...` (reports `[no test files]`; this package has no tests of its own).
- The lock tests of the state backends exercise it: `go test ./state/memory/... ./state/nats/... ./state/postgres/...`. The `state/postgres` conformance test needs a healthy testcontainers (Docker) provider and skips without one.
