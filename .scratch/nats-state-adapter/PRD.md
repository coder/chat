# NATS JetStream State Adapter

Status: ready-for-agent

## Problem Statement

The **Go Chat Runtime** ships three **Runtime State** implementations: **Memory State** (dev/test), **Redis State**, and **Postgres State**. Deployments standardized on NATS — already running JetStream for messaging — must stand up a separate Redis or Postgres purely to back runtime subscriptions, dedupe, and locks. There is no NATS-backed **Runtime State**, so NATS shops pay for and operate an extra coordination store the rest of their stack does not need.

The `chat.State` contract is small and maps cleanly onto NATS JetStream Key-Value: a durable subscription set, an idempotent create-with-TTL dedupe mark, and a token-owned lease lock with extend/release. JetStream KV provides atomic create-if-absent (`Create`), revision-gated compare-and-swap (`Update`), and revision-gated delete — exactly the primitives the other two adapters build on.

## Solution

Add a fourth **Runtime State** implementation, **NATS State**, as its own module `github.com/coder/chat/state/nats`, alongside `state/redis` and `state/postgres`.

Use JetStream KV with **bucket-level TTL** (the stream MaxAge), one bucket per concern:

- `<prefix>_sub` — TTL 0 (never expires): thread subscriptions.
- `<prefix>_event` — TTL = DedupeTTL: event dedupe.
- `<prefix>_lock` — TTL = ThreadLockTTL: locks.

All buckets use `History: 1` so an expired or deleted key fully clears its subject. The adapter validates each per-call `ttl` as positive but governs expiry by the bucket TTL — sound because the runtime always passes a single uniform TTL per concern (`DedupeTTL`, `ThreadLockTTL`). Operators set the adapter's `DedupeTTL`/`ThreadLockTTL` options to match the runtime's. `New` creates each bucket if absent and otherwise opens it without reconfiguring; if an existing bucket's TTL differs from the configured one, construction fails with a clear error instead of silently rewriting expiry (changing a bucket TTL is a deliberate migration — delete the bucket).

Method mapping (public `jetstream` API, NATS Server >= 2.9):
- subscribe/unsubscribe/check → `Put` / `Delete` / `Get`.
- `MarkEvent` → `Create` (create-if-absent); `ErrKeyExists` means already seen.
- `AcquireLock` → `Create` with a random token (reusing `internal/tokens`).
- `ExtendLock` → `Get` (verify token) then revision-gated `Update` (a fresh write renews the entry's age under the bucket TTL).
- `ReleaseLock` → `Get` (verify token) then `Delete` with `LastRevision` (fencing-safe).

Keys are base64 RawURL-encoded because NATS KV keys may not contain the `:` present in **Thread ID**s.

This is **Semantic Compatibility** work: it satisfies the existing `chat.State` contract and passes the shared state conformance suite; it does not change the contract or the runtime.

## User Stories

1. As an operator running NATS/JetStream, I want a NATS-backed **Runtime State**, so that I do not have to run Redis or Postgres solely for runtime coordination.
2. As a bot developer, I want **NATS State** to satisfy the same `chat.State` contract as **Redis State** and **Postgres State**, so that switching backends is a construction change only.
3. As a bot developer, I want subscriptions to persist indefinitely and dedupe/lock keys to expire on their bucket TTL, so that behavior matches the other adapters.
4. As a runtime operator, I want a crashed lock holder's **Thread Lock** to become re-acquirable after its TTL, so that a dead process does not wedge a **Thread**.
5. As a contributor, I want a `slack-nats-state` example mirroring the Redis/Postgres examples, so that I can run the adapter end-to-end locally.

## Implementation Decisions

- **NATS State** is its own Go module (`state/nats`), wired into `go.work` with a `replace`, matching `state/redis` and `state/postgres`.
- Strategy: **bucket-level TTL** (stream MaxAge), not NATS 2.11 per-key TTL — simpler, stays on the stable public KV API, and runs on NATS Server >= 2.9. Justified because the runtime uses uniform per-concern TTLs.
- Three buckets (`_sub`/`_event`/`_lock`), all `History: 1`; each created if absent and otherwise opened. An existing bucket is never reconfigured: a TTL mismatch fails construction with a clear error rather than silently rewriting expiry (a plain `CreateOrUpdateKeyValue` would overwrite it).
- `New(ctx, Options)` takes a caller-owned `*nats.Conn`, verifies JetStream availability with `AccountInfo` (the readiness probe, analogous to the Redis/Postgres `Ping`), then sets up buckets. Error messages are prefixed `nats state:`. `var _ chat.State = (*State)(nil)` enforces the contract.
- Lock tokens reuse `github.com/coder/chat/internal/tokens` (importable across modules; **Postgres State** already does).
- Revision-gated `Update`/`Delete` failures (JetStream `wrong last sequence`, code 10071) are treated as "not the owner" → return `false`, not an error.
- `Shutdown` closes the connection once (`sync.Once`), taking ownership of the supplied conn's lifecycle (consistent with **Redis State** closing its client and **Postgres State** closing its pool). It uses `Close` rather than `Drain` because `Drain` returns before the connection actually closes, which would make `Shutdown` report success while the conn is still open.
- Keys are base64 RawURL-encoded at the NATS boundary; the raw key is kept in the returned **Lock Lease**.

## Testing Decisions

- The adapter runs the shared `internal/statetest.RunStateConformance` suite (subscriptions, dedupe, locks, context cancellation).
- The conformance harness uses an in-process embedded `nats-server` with JetStream (a `t.TempDir()` store), one server per subtest for isolation — the analog of **Redis State**'s miniredis path.
- Timing: bucket TTL = harness `ShortTTL` = 1s; `ExpiryWait` = 3s. `ExpiryWait` must exceed the bucket TTL plus the JetStream age-reaper interval (~1s), because an expired-but-not-yet-reaped message still occupies its subject and keeps `Create` returning `ErrKeyExists`. There is no fake clock, so the harness uses real `time.Sleep`.
- A focused test asserts that reconstructing `New` over an existing bucket with a changed (or omitted/default) TTL fails loudly, while reopening with the same TTL is idempotent.
- `mise` gains a `test:nats` task; `test:adapters` and `test:examples` include the new module and example.

## Out of Scope

- NATS 2.11 per-key TTL — deferred; the uniform-TTL runtime does not need it.
- A Postgres-style "steal-if-expired" lock (stored `expires_at` + CAS) that would remove the dependence on the server's reap cadence — noted as a future enhancement, not built in v1.
- Honoring arbitrary per-call TTLs that differ from the bucket TTL.
- A testcontainers-based integration test (the embedded server covers conformance); can follow later.

## Further Notes

- Design and verification grew out of two cited research passes on JetStream KV (per-key vs bucket TTL, atomic Create/Update CAS, revision-gated delete, TTL granularity), grounded against the existing `chat.State` call sites in `runtime.go`.
- ADR 0014 records the decision and the rejected alternatives.
