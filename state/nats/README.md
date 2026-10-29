# nats

## Overview

Package `nats` (module and import path `github.com/coder/chat/state/nats`) is a
NATS JetStream-backed implementation of the `chat.State` interface from
`github.com/coder/chat` [doc.go:nats]. It stores subscriptions, dedupe events,
and thread locks in three JetStream key-value (KV) buckets. Expiry comes from
per-bucket TTLs set at construction, not from the per-call `ttl` arguments
[nats.go:Options]. It is a separate Go module with its own `go.mod`
[go.mod:module]. `nats.go` imports `github.com/coder/chat`,
`github.com/coder/chat/internal/tokens`, `github.com/nats-io/nats.go`, and
`github.com/nats-io/nats.go/jetstream`.

## Interface

- `Options` [nats.go:Options]: `Conn *natsgo.Conn`, `Prefix string`,
  `DedupeTTL time.Duration`, `ThreadLockTTL time.Duration`.
- `State` [nats.go:State]: unexported fields `nc`, `sub`, `event`, `lock`
  (`jetstream.KeyValue`), and `once` (`sync.Once`).
- `var _ chat.State = (*State)(nil)` [nats.go:State]: compile-time check that
  `*State` implements `chat.State`.
- `func New(ctx context.Context, opts Options) (*State, error)` [nats.go:New]
- `(*State).IsThreadSubscribed(ctx, id chat.ThreadID) (bool, error)`
  [nats.go:State.IsThreadSubscribed]
- `(*State).SubscribeThread(ctx, id chat.ThreadID) error`
  [nats.go:State.SubscribeThread]
- `(*State).UnsubscribeThread(ctx, id chat.ThreadID) error`
  [nats.go:State.UnsubscribeThread]
- `(*State).MarkEvent(ctx, id string, ttl time.Duration) (bool, error)`
  [nats.go:State.MarkEvent]
- `(*State).AcquireLock(ctx, key string, ttl time.Duration) (chat.LockLease, bool, error)`
  [nats.go:State.AcquireLock]
- `(*State).ExtendLock(ctx, lease chat.LockLease, ttl time.Duration) (bool, error)`
  [nats.go:State.ExtendLock]
- `(*State).ReleaseLock(ctx, lease chat.LockLease) (bool, error)`
  [nats.go:State.ReleaseLock]
- `(*State).Shutdown(context.Context) error` [nats.go:State.Shutdown]

## Invariants

- `New` requires `opts.Conn`. A nil connection fails with
  `"nats state: conn is required"` [nats.go:New].
- `New` computes the prefix as
  `strings.TrimRight(strings.TrimSpace(opts.Prefix), "_")`. An empty result
  becomes `"chat"` [nats.go:New].
- The prefix must contain only ASCII letters, digits, `-`, and `_`. Otherwise
  `New` fails with `"nats state: prefix %q contains invalid characters"`
  [nats.go:validBucketToken] [nats.go:New].
- `DedupeTTL <= 0` defaults to `24 * time.Hour`. `ThreadLockTTL <= 0`
  defaults to `2 * time.Minute` [nats.go:New].
- Bucket names are `prefix + "_" + name` for `sub`, `event`, and `lock`. With
  the default prefix they are `chat_sub`, `chat_event`, and `chat_lock`
  [nats.go:New].
- Bucket config: `sub` has TTL `0` (no expiry), `event` has the dedupe TTL,
  `lock` has the lock TTL. Every bucket has `History: 1`, so TTL expiry clears
  the subject [nats.go:New].
- `ensureBucket` never mutates an existing bucket. If the bucket exists with a
  different TTL or history, `New` fails with an error that ends in
  `"delete the bucket to change its TTL"` or
  `"delete the bucket to change its history"` [nats.go:ensureBucket].
- The per-call `ttl` arguments to `MarkEvent`, `AcquireLock`, and `ExtendLock`
  must be positive, but the code does not use them for expiry
  [nats.go:Options] [nats.go:State.MarkEvent] [nats.go:State.AcquireLock]
  [nats.go:State.ExtendLock].
- Every KV key is `base64.RawURLEncoding` of the raw ID or lock key. The NATS
  KV key charset forbids `:` and treats `.` as a subject separator
  [nats.go:encodeKey].
- Validation errors: `"nats state: thread id is required"` (empty thread ID in
  the three subscription methods), `"nats state: event id is required"`,
  `"nats state: dedupe ttl must be positive"`, `"nats state: lock key is required"`,
  `"nats state: lock ttl must be positive"`, and
  `"nats state: lock lease is required"` (empty `Key` or `Token`)
  [nats.go:State.IsThreadSubscribed] [nats.go:State.MarkEvent]
  [nats.go:State.AcquireLock] [nats.go:State.ExtendLock]
  [nats.go:State.ReleaseLock].
- `IsThreadSubscribed` maps `jetstream.ErrKeyNotFound` to `(false, nil)`.
  `UnsubscribeThread` maps it to `nil` [nats.go:State.IsThreadSubscribed]
  [nats.go:State.UnsubscribeThread].
- `MarkEvent` and `AcquireLock` use KV `Create`, so the first writer wins.
  `jetstream.ErrKeyExists` returns `(false, nil)` or
  `(chat.LockLease{}, false, nil)` [nats.go:State.MarkEvent]
  [nats.go:State.AcquireLock].
- The lock value is the lease token from `tokens.New()`
  [nats.go:State.AcquireLock].
- `ExtendLock` and `ReleaseLock` read the entry first. A missing key or a
  value that is not `lease.Token` returns `(false, nil)`
  [nats.go:State.ExtendLock] [nats.go:State.ReleaseLock].
- `ExtendLock` writes with `Update(..., entry.Revision())`. `ReleaseLock`
  deletes with `jetstream.LastRevision(entry.Revision())`. A
  `JSErrCodeStreamWrongLastSequence` error returns `(false, nil)`, so a lock
  that changed between the read and the write is not touched
  [nats.go:State.ExtendLock] [nats.go:State.ReleaseLock]
  [nats.go:isWrongLastSequence].
- `Shutdown` closes the connection at most once and always returns `nil`
  [nats.go:State.Shutdown].

## Side Effects

- `New` creates a JetStream context with `jetstream.New(opts.Conn)` and calls
  `js.AccountInfo(ctx)`. Failure returns `"nats state: jetstream account: %w"`
  [nats.go:New].
- `New` creates or opens the three KV buckets with `js.CreateKeyValue`, then
  `js.KeyValue` and `kv.Status` on `jetstream.ErrBucketExists`. Failure
  returns `"nats state: setup %s bucket: %w"` [nats.go:New]
  [nats.go:ensureBucket].
- `IsThreadSubscribed` runs KV `Get` on the `sub` bucket
  [nats.go:State.IsThreadSubscribed].
- `SubscribeThread` runs KV `Put` of value `1` on the `sub` bucket
  [nats.go:State.SubscribeThread].
- `UnsubscribeThread` runs KV `Delete` on the `sub` bucket
  [nats.go:State.UnsubscribeThread].
- `MarkEvent` runs KV `Create` of value `1` on the `event` bucket. The entry
  expires after the bucket TTL [nats.go:State.MarkEvent].
- `AcquireLock` runs KV `Create` of the token on the `lock` bucket. The entry
  expires after the bucket TTL [nats.go:State.AcquireLock].
- `ExtendLock` runs KV `Get`, then KV `Update` with the same token. The new
  write resets the entry age, which renews the lease under the bucket TTL
  [nats.go:State.ExtendLock].
- `ReleaseLock` runs KV `Get`, then a revision-gated KV `Delete`
  [nats.go:State.ReleaseLock].
- `Shutdown` calls `Close` on the caller-supplied `*natsgo.Conn`
  [nats.go:State.Shutdown].

## Verification

- Run: `cd state/nats && go test ./...`
- The tests start an embedded NATS server, so they need no Docker or external
  server (`test:nats` task in the root `mise.toml`).
