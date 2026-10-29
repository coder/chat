# memory

## Overview

Package `memory` (import path `github.com/coder/chat/state/memory`) is an
in-process implementation of the `chat.State` interface from
`github.com/coder/chat`. The package comment describes it as state "for tests
and local development" [doc.go:memory]. All data lives in Go maps guarded by
one mutex. Nothing survives a process restart, and nothing is shared between
processes. It is part of the root Go module, not a separate module.

## Interface

- `State` [memory.go:State]: in-process state. Unexported fields: `mu`
  (`sync.Mutex`), `subscribed`, `events`, `locks` maps, `closed`
  (`atomic.Bool`), and `now` (clock func).
- `var _ chat.State = (*State)(nil)` [memory.go:State]: compile-time check
  that `*State` implements `chat.State`.
- `func New() *State` [memory.go:New]: returns a `State` with empty maps and
  `now` set to `time.Now`. There are no options.
- `(*State).IsThreadSubscribed(ctx, id chat.ThreadID) (bool, error)`
  [memory.go:State.IsThreadSubscribed]
- `(*State).SubscribeThread(ctx, id chat.ThreadID) error`
  [memory.go:State.SubscribeThread]
- `(*State).UnsubscribeThread(ctx, id chat.ThreadID) error`
  [memory.go:State.UnsubscribeThread]
- `(*State).MarkEvent(ctx, id string, ttl time.Duration) (bool, error)`
  [memory.go:State.MarkEvent]: returns `true` the first time an ID is seen
  within its TTL.
- `(*State).AcquireLock(ctx, key string, ttl time.Duration) (chat.LockLease, bool, error)`
  [memory.go:State.AcquireLock]
- `(*State).ExtendLock(ctx, lease chat.LockLease, ttl time.Duration) (bool, error)`
  [memory.go:State.ExtendLock]
- `(*State).ReleaseLock(ctx, lease chat.LockLease) (bool, error)`
  [memory.go:State.ReleaseLock]
- `(*State).Shutdown(context.Context) error` [memory.go:State.Shutdown]

## Invariants

- Every method except `Shutdown` calls `checkOperation`. It returns
  `"memory state: nil state"` for a nil receiver, `ctx.Err()` for a done
  context, and `"memory state: closed"` after `Shutdown`
  [memory.go:State.checkOperation].
- `lockOperationAfterCheck` runs `checkOperation` again after it takes `mu`.
  An operation that waits on the mutex while `Shutdown` runs fails with
  `"memory state: closed"` and does not mutate state
  [memory.go:State.lockOperationAfterCheck].
- `SubscribeThread` and `UnsubscribeThread` reject an empty ID with
  `"memory state: thread id is required"`
  [memory.go:State.SubscribeThread] [memory.go:State.UnsubscribeThread].
  `IsThreadSubscribed` does not check for an empty ID
  [memory.go:State.IsThreadSubscribed].
- Subscriptions have no expiry [memory.go:State.SubscribeThread].
- `MarkEvent` rejects an empty ID with `"memory state: event id is required"`
  and `ttl <= 0` with `"memory state: dedupe ttl must be positive"`
  [memory.go:State.MarkEvent].
- `MarkEvent` on an unexpired ID returns `(false, nil)` and does not refresh
  its expiry [memory.go:State.MarkEvent].
- `AcquireLock` rejects an empty key with `"memory state: lock key is required"`
  and `ttl <= 0` with `"memory state: lock ttl must be positive"`. A token
  error is wrapped as `"memory state: create lock token: %w"`
  [memory.go:State.AcquireLock].
- `AcquireLock` on a key with an unexpired lock returns
  `(chat.LockLease{}, false, nil)`. At most one unexpired holder exists per key
  [memory.go:State.AcquireLock].
- `ExtendLock` and `ReleaseLock` reject a lease with an empty `Key` or `Token`
  with `"memory state: lock lease is required"`. `ExtendLock` also rejects
  `ttl <= 0` with `"memory state: lock ttl must be positive"`
  [memory.go:State.ExtendLock] [memory.go:State.ReleaseLock].
- `ExtendLock` and `ReleaseLock` act only when the stored token equals
  `lease.Token`. A missing, expired, or mismatched lock returns `(false, nil)`,
  not an error [memory.go:State.ExtendLock] [memory.go:State.ReleaseLock].
- An event or lock is expired when `!expiry.After(now)`, that is
  `expiry <= now` [memory.go:State.pruneExpiredEvents]
  [memory.go:State.pruneExpiredLocks].
- All map reads and writes happen while `mu` is held. The `closed` flag is an
  `atomic.Bool`, so `checkOperation` reads it without the mutex
  [memory.go:State].

## Side Effects

- `SubscribeThread` sets `subscribed[id] = true`. `UnsubscribeThread` deletes
  `subscribed[id]` [memory.go:State.SubscribeThread]
  [memory.go:State.UnsubscribeThread].
- `MarkEvent` scans all events, deletes expired ones, then stores
  `events[id] = now + ttl` for a new ID [memory.go:State.MarkEvent]
  [memory.go:State.pruneExpiredEvents].
- `AcquireLock`, `ExtendLock`, and `ReleaseLock` scan all locks and delete
  expired ones before they act [memory.go:State.pruneExpiredLocks].
- `AcquireLock` creates a token with `tokens.New()` from
  `github.com/coder/chat/internal/tokens`, then stores
  `locks[key] = lockRecord{token, now + ttl}` [memory.go:State.AcquireLock].
- `ExtendLock` overwrites the lock expiry with `now + ttl`
  [memory.go:State.ExtendLock]. `ReleaseLock` deletes `locks[key]`
  [memory.go:State.ReleaseLock].
- `Shutdown` stores `closed = true`, then takes and releases `mu`. It does not
  clear subscriptions, events, or locks, and always returns `nil`
  [memory.go:State.Shutdown].
- No goroutines, timers, or background cleanup exist. Expired records are
  removed only during `MarkEvent` and the lock methods [memory.go:State].

## Verification

- Run from the repository root: `go test ./state/memory/...`
