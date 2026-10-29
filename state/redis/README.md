# redis

## Overview

Package `redis` (module and import path `github.com/coder/chat/state/redis`) is
a Redis-backed implementation of the `chat.State` interface from
`github.com/coder/chat` [doc.go:redis]. It stores subscriptions, dedupe events,
and thread locks as plain string keys under one prefix. Expiry uses native
Redis key TTLs from the per-call `ttl` arguments. Lock extend and release use
Lua scripts that compare the lease token first. It is a separate Go module
with its own `go.mod` [go.mod:module]. `redis.go` imports
`github.com/coder/chat` and `github.com/redis/go-redis/v9`.

## Interface

- `Options` [redis.go:Options]: `Client redis.UniversalClient`,
  `Prefix string`.
- `State` [redis.go:State]: unexported fields `client`, `prefix`, and `once`
  (`sync.Once`).
- `var _ chat.State = (*State)(nil)` [redis.go:State]: compile-time check that
  `*State` implements `chat.State`.
- `func New(ctx context.Context, opts Options) (*State, error)` [redis.go:New]:
  pings Redis.
- `(*State).IsThreadSubscribed(ctx, id chat.ThreadID) (bool, error)`
  [redis.go:State.IsThreadSubscribed]
- `(*State).SubscribeThread(ctx, id chat.ThreadID) error`
  [redis.go:State.SubscribeThread]
- `(*State).UnsubscribeThread(ctx, id chat.ThreadID) error`
  [redis.go:State.UnsubscribeThread]
- `(*State).MarkEvent(ctx, id string, ttl time.Duration) (bool, error)`
  [redis.go:State.MarkEvent]
- `(*State).AcquireLock(ctx, key string, ttl time.Duration) (chat.LockLease, bool, error)`
  [redis.go:State.AcquireLock]
- `(*State).ExtendLock(ctx, lease chat.LockLease, ttl time.Duration) (bool, error)`
  [redis.go:State.ExtendLock]
- `(*State).ReleaseLock(ctx, lease chat.LockLease) (bool, error)`
  [redis.go:State.ReleaseLock]
- `(*State).Shutdown(context.Context) error` [redis.go:State.Shutdown]

## Invariants

- `New` requires `opts.Client`. A nil client fails with
  `"redis state: client is required"` [redis.go:New].
- An empty `Prefix` becomes `"chat"`. `New` then trims trailing `:` characters.
  It does not trim whitespace [redis.go:New].
- Every key is `prefix + ":" + kind + ":" + value`, with kind `sub`, `event`,
  or `lock`. With the default prefix the keys are `chat:sub:<thread id>`,
  `chat:event:<event id>`, and `chat:lock:<lock key>` [redis.go:State.key].
  IDs are not encoded or escaped.
- Validation errors: `"redis state: thread id is required"` (empty thread ID in
  the three subscription methods), `"redis state: event id is required"`,
  `"redis state: dedupe ttl must be positive"`, `"redis state: lock key is required"`,
  `"redis state: lock ttl must be positive"`, and
  `"redis state: lock lease is required"` (empty `Key` or `Token`)
  [redis.go:State.IsThreadSubscribed] [redis.go:State.MarkEvent]
  [redis.go:State.AcquireLock] [redis.go:State.ExtendLock]
  [redis.go:State.ReleaseLock].
- `IsThreadSubscribed` returns `true` only when `EXISTS` returns `1`
  [redis.go:State.IsThreadSubscribed].
- Subscription keys have no expiry [redis.go:State.SubscribeThread].
- `MarkEvent` and `AcquireLock` use `SetNX`, so the first writer wins. A held
  lock returns `(chat.LockLease{}, false, nil)` [redis.go:State.MarkEvent]
  [redis.go:State.AcquireLock].
- Lock tokens are 16 bytes from `crypto/rand`, hex-encoded to 32 characters.
  A token error is wrapped as `"redis state: create lock token: %w"`
  [redis.go:newToken] [redis.go:State.AcquireLock].
- `extendScript` runs `PEXPIRE` and `releaseScript` runs `DEL` only when
  `GET KEYS[1] == ARGV[1]` (the lease token). Otherwise they return `0`, and
  the Go methods return `false` [redis.go:extendScript]
  [redis.go:releaseScript] [redis.go:State.ExtendLock]
  [redis.go:State.ReleaseLock].
- `ExtendLock` passes `ttl.Milliseconds()` to the script, so it truncates
  sub-millisecond precision [redis.go:State.ExtendLock].
- A positive `ttl` under 1 ms therefore reaches the script as `0`. Redis
  deletes a key given a non-positive `PEXPIRE` and returns `1`, so
  `ExtendLock` deletes the lock and still returns `true`
  [redis.go:State.ExtendLock] [redis.go:extendScript]. The runtime renews
  with `ThreadLockTTL` (default 2 minutes), so only a sub-millisecond
  configuration reaches this path.
- Redis errors from the state methods are returned unwrapped. `New` wraps a
  ping error as `"redis state: ping: %w"` [redis.go:New].
- `Shutdown` closes the client at most once. It returns the `Close` error from
  the first call and `nil` from later calls [redis.go:State.Shutdown].

## Side Effects

- `New` sends `PING` [redis.go:New].
- `IsThreadSubscribed` sends `EXISTS <prefix>:sub:<id>`
  [redis.go:State.IsThreadSubscribed].
- `SubscribeThread` sets `<prefix>:sub:<id>` to `"1"` with expiration `0`
  [redis.go:State.SubscribeThread].
- `UnsubscribeThread` sends `DEL <prefix>:sub:<id>`
  [redis.go:State.UnsubscribeThread].
- `MarkEvent` runs `SetNX` on `<prefix>:event:<id>` with value `"1"` and the
  caller's `ttl` [redis.go:State.MarkEvent].
- `AcquireLock` reads `crypto/rand`, then runs `SetNX` on `<prefix>:lock:<key>`
  with the token and the caller's `ttl` [redis.go:State.AcquireLock]
  [redis.go:newToken].
- `ExtendLock` sends `EVAL extendScript` on `<prefix>:lock:<key>` with
  `ARGV = [token, ttl in ms]` [redis.go:State.ExtendLock].
- `ReleaseLock` sends `EVAL releaseScript` on `<prefix>:lock:<key>` with
  `ARGV = [token]` [redis.go:State.ReleaseLock].
- `Shutdown` calls `Close` on the caller-supplied `redis.UniversalClient`
  [redis.go:State.Shutdown].

## Verification

- Run: `cd state/redis && go test ./...`
- `TestStateConformanceWithRedisContainer` uses Testcontainers and needs a
  running Docker daemon (`test:redis` task in the root `mise.toml`). It skips
  when no healthy container provider exists. The other conformance run uses an
  in-process miniredis server.
