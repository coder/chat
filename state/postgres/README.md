# postgres

## Overview

Package `postgres` (module and import path
`github.com/coder/chat/state/postgres`) is a Postgres-backed implementation of
the `chat.State` interface from `github.com/coder/chat` [doc.go:postgres]. It
stores subscriptions, dedupe events, and thread locks in three tables that it
creates on startup. All expiry checks use the database clock `now()`. Every
row is scoped by a namespace string. It is a separate Go module with its own
`go.mod` [go.mod:module]. `postgres.go` imports `github.com/coder/chat`,
`github.com/coder/chat/internal/tokens`, and
`github.com/jackc/pgx/v5/pgxpool`.

## Interface

- `Options` [postgres.go:Options]: `Pool *pgxpool.Pool`, `Namespace string`.
- `State` [postgres.go:State]: unexported fields `pool`, `namespace`, and
  `once` (`sync.Once`).
- `var _ chat.State = (*State)(nil)` [postgres.go:State]: compile-time check
  that `*State` implements `chat.State`.
- `func New(ctx context.Context, opts Options) (*State, error)`
  [postgres.go:New]: pings the pool and creates the schema.
- `(*State).IsThreadSubscribed(ctx, id chat.ThreadID) (bool, error)`
  [postgres.go:State.IsThreadSubscribed]
- `(*State).SubscribeThread(ctx, id chat.ThreadID) error`
  [postgres.go:State.SubscribeThread]
- `(*State).UnsubscribeThread(ctx, id chat.ThreadID) error`
  [postgres.go:State.UnsubscribeThread]
- `(*State).MarkEvent(ctx, id string, ttl time.Duration) (bool, error)`
  [postgres.go:State.MarkEvent]
- `(*State).AcquireLock(ctx, key string, ttl time.Duration) (chat.LockLease, bool, error)`
  [postgres.go:State.AcquireLock]
- `(*State).ExtendLock(ctx, lease chat.LockLease, ttl time.Duration) (bool, error)`
  [postgres.go:State.ExtendLock]
- `(*State).ReleaseLock(ctx, lease chat.LockLease) (bool, error)`
  [postgres.go:State.ReleaseLock]
- `(*State).Shutdown(context.Context) error` [postgres.go:State.Shutdown]

## Invariants

- `New` requires `opts.Pool`. A nil pool fails with
  `"postgres state: pool is required"` before any database call
  [postgres.go:New].
- `New` trims whitespace from `Namespace`. An empty result becomes `"chat"`
  [postgres.go:New].
- Every table's primary key starts with `namespace`
  [postgres.go:schemaStatements], and every query in the `State` methods
  filters on it [postgres.go:State.MarkEvent] [postgres.go:State.AcquireLock].
- Validation errors: `"postgres state: thread id is required"` (empty thread
  ID in the three subscription methods), `"postgres state: event id is required"`,
  `"postgres state: lock key is required"`, and
  `"postgres state: lock lease is required"` (empty `Key` or `Token`)
  [postgres.go:State.IsThreadSubscribed] [postgres.go:State.MarkEvent]
  [postgres.go:State.AcquireLock] [postgres.go:State.ExtendLock]
  [postgres.go:State.ReleaseLock].
- `positiveMicros` rejects `ttl <= 0` with
  `"postgres state: dedupe ttl must be positive"` or
  `"postgres state: lock ttl must be positive"`. It converts the TTL with
  `Duration.Microseconds()` and rounds a positive sub-microsecond TTL up to
  `1` [postgres.go:positiveMicros].
- `SubscribeThread` is idempotent through `ON CONFLICT (namespace, thread_id) DO NOTHING`
  [postgres.go:State.SubscribeThread]. `UnsubscribeThread` returns `nil` when
  no row exists [postgres.go:State.UnsubscribeThread].
- `MarkEvent` returns `true` only when it inserts a row or replaces a row whose
  `expires_at <= now()`. An unexpired row returns `(false, nil)`
  [postgres.go:State.MarkEvent].
- `AcquireLock` takes over an existing row only when its `expires_at <= now()`.
  It then writes a new token. When the lock is held it returns
  `(chat.LockLease{}, false, nil)` [postgres.go:State.AcquireLock].
- `ExtendLock` and `ReleaseLock` match on `namespace`, `lock_key`, `token`,
  and `expires_at > now()`. They return `true` only when `RowsAffected() == 1`.
  An expired lease cannot be extended or released
  [postgres.go:State.ExtendLock] [postgres.go:State.ReleaseLock].
- Schema setup runs in one transaction that first runs
  `SELECT pg_advisory_xact_lock(hashtext($1)::bigint)` with
  `$1 = "chat_runtime_state_schema"`. This serializes concurrent setup calls
  against one database [postgres.go:State.setup].
- Query errors from the state methods are returned unwrapped. `New` wraps its
  errors as `"postgres state: ping: %w"` and `"postgres state: setup: %w"`
  [postgres.go:New]. `AcquireLock` wraps a token error as
  `"postgres state: create lock token: %w"` [postgres.go:State.AcquireLock].
- `Shutdown` closes the pool at most once and always returns `nil`
  [postgres.go:State.Shutdown].

## Side Effects

- `New` calls `pool.Ping(ctx)`. If ping or setup fails, `New` does not close
  the pool [postgres.go:New].
- `setup` runs this DDL from `schemaStatements` inside the advisory-locked
  transaction. No `CREATE INDEX` exists, so the only indexes are the primary
  keys. Each statement runs as its own `tx.Exec`. This README adds the
  semicolons [postgres.go:State.setup] [postgres.go:schemaStatements].

  ```sql
  CREATE TABLE IF NOT EXISTS chat_runtime_subscriptions (
      namespace text NOT NULL,
      thread_id text NOT NULL,
      PRIMARY KEY (namespace, thread_id)
  );
  CREATE TABLE IF NOT EXISTS chat_runtime_events (
      namespace text NOT NULL,
      event_id text NOT NULL,
      expires_at timestamptz NOT NULL,
      PRIMARY KEY (namespace, event_id)
  );
  CREATE TABLE IF NOT EXISTS chat_runtime_locks (
      namespace text NOT NULL,
      lock_key text NOT NULL,
      token text NOT NULL,
      expires_at timestamptz NOT NULL,
      PRIMARY KEY (namespace, lock_key)
  );
  ```

- `IsThreadSubscribed` reads `chat_runtime_subscriptions` with
  `SELECT EXISTS (...)` [postgres.go:State.IsThreadSubscribed].
- `SubscribeThread` inserts into `chat_runtime_subscriptions`
  [postgres.go:State.SubscribeThread]. `UnsubscribeThread` deletes from it
  [postgres.go:State.UnsubscribeThread].
- `MarkEvent` upserts into `chat_runtime_events` with
  `expires_at = now() + ($3::bigint * interval '1 microsecond')`
  [postgres.go:State.MarkEvent].
- `AcquireLock` upserts `token` and `expires_at` into `chat_runtime_locks`
  [postgres.go:State.AcquireLock]. The token comes from `tokens.New()` in
  `github.com/coder/chat/internal/tokens`.
- `ExtendLock` updates `expires_at` in `chat_runtime_locks`
  [postgres.go:State.ExtendLock]. `ReleaseLock` deletes the row
  [postgres.go:State.ReleaseLock].
- Expired rows are never deleted. They stay in the tables until a later
  `MarkEvent` or `AcquireLock` overwrites them [postgres.go:State.MarkEvent]
  [postgres.go:State.AcquireLock].
- `Shutdown` calls `Close` on the caller-supplied `*pgxpool.Pool`
  [postgres.go:State.Shutdown].

## Verification

- Run: `cd state/postgres && go test ./...`
- The tests start Postgres with Testcontainers, so they need a running Docker
  daemon (`test:postgres` task in the root `mise.toml`). The test skips when
  no healthy container provider exists.
