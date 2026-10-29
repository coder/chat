# statetest

## Overview

Package `statetest` is the shared conformance suite for `chat.State` backends.
It lives in a non-test file (`state.go`) and imports `testing`, so only test code
uses it. Each backend's `TestStateConformance` calls `RunStateConformance` with a
factory that builds a fresh backend. These test files import it:

- `state/memory/memory_test.go` (`github.com/coder/chat/state/memory`)
- `state/nats/nats_test.go` (`github.com/coder/chat/state/nats`)
- `state/postgres/postgres_test.go` (`github.com/coder/chat/state/postgres`)
- `state/redis/redis_test.go` (`github.com/coder/chat/state/redis`, twice: `TestStateConformance` with miniredis and `TestStateConformanceWithRedisContainer`)

## Interface

- `type Harness struct`: the backend under test and its timing controls. `[state.go:Harness]`
  - `State chat.State`: the backend under test.
  - `AdvanceTime func(time.Duration)`: optional fake clock. When nil, the suite sleeps in real time.
  - `ShortTTL time.Duration`: optional TTL for entries that the suite lets expire. Zero means 25ms.
  - `ExpiryWait time.Duration`: optional time the suite advances to pass `ShortTTL`. Zero means 60ms.
- `func RunStateConformance(t *testing.T, newState func(*testing.T) Harness)`: runs the conformance subtests against harnesses from `newState`. `[state.go:RunStateConformance]`

## Invariants

The suite asserts these contracts on every `chat.State` backend:

- `subscriptions`: a new thread is not subscribed. After `SubscribeThread` it is subscribed. After `UnsubscribeThread` it is not. `[state.go:RunStateConformance]`
- `dedupe`: the first `MarkEvent` for an ID returns `true`. A duplicate within the TTL returns `false`. After the TTL expires, `MarkEvent` returns `true` again. `[state.go:RunStateConformance]`
- `locks`: a second `AcquireLock` on a held key fails. `ReleaseLock` with a stale token returns `false` and keeps the lock. The current owner can `ExtendLock` and `ReleaseLock`. After the TTL expires, a new owner can acquire the key, and the old lease's `ReleaseLock` returns `false` and keeps the newer lock. `[state.go:RunStateConformance]`
- `context cancellation`: with a cancelled context, `SubscribeThread`, `MarkEvent`, and `AcquireLock` return an error and do not change state. A later call with a live context succeeds as if the cancelled call never happened. `[state.go:RunStateConformance]`
- `newState` must return an independent backend on each call. Each of the four subtests calls it once and runs with `t.Parallel()`. `[state.go:RunStateConformance]`
- `ExpiryWait` must be longer than `ShortTTL`, because the suite expects entries created with `ShortTTL` to expire after one `ExpiryWait`. `[state.go:Harness.shortTTL]`, `[state.go:Harness.expiryWait]`
- `AdvanceTime`, when set, replaces the real sleep. `[state.go:advanceTime]`

## Side Effects

- Fails the calling test with `t.Fatal` or `t.Fatalf` on any contract violation. `[state.go:RunStateConformance]`
- Starts four parallel subtests with `t.Run` and `t.Parallel`. `[state.go:RunStateConformance]`
- Without `AdvanceTime`, blocks on `time.Sleep(ExpiryWait)` (60ms by default) in the `dedupe` and `locks` subtests. `[state.go:advanceTime]`
- Mutates the backend in `Harness.State`. Any I/O comes from that backend, not from this package. `[state.go:RunStateConformance]`

## Verification

- Run: `go test ./internal/statetest/...` (reports `[no test files]`; the suite has no tests of its own).
- The backends run the suite: `go test -run '^TestStateConformance$' ./state/memory/... ./state/nats/... ./state/redis/... ./state/postgres/...`. The memory, nats (embedded server), and redis (miniredis) runs need no Docker. The postgres run needs a healthy testcontainers (Docker) provider and skips without one. `TestStateConformanceWithRedisContainer` also needs Docker, and this `-run` filter excludes it.
