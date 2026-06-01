# ADR 0014: NATS JetStream State Adapter

## Status

Accepted

## Context

The repo ships three **Runtime State** implementations: **Memory State** (dev/test), **Redis State**, and **Postgres State**. Each satisfies the small `chat.State` contract — thread subscriptions, webhook dedupe, and token-owned lease locks — over a different backend. Deployments standardized on NATS, already operating JetStream for messaging, currently have to run a separate Redis or Postgres purely to back runtime coordination.

NATS JetStream Key-Value provides exactly the primitives the contract needs: atomic create-if-absent (`Create`, the analog of Redis `SETNX` and Postgres insert-if-absent), revision-gated compare-and-swap (`Update`), and revision-gated delete (`Delete` with `LastRevision`). The open question was TTL: dedupe marks and lock leases expire, subscriptions do not. JetStream offers two TTL mechanisms — a bucket-level TTL (the stream MaxAge, available since NATS Server 2.9) and a per-key TTL (NATS Server 2.11, `KeyTTL` on `Create`).

The runtime uses **uniform** TTLs: every `AcquireLock`/`ExtendLock` call passes `Options.ThreadLockTTL` and the single `MarkEvent` call passes `Options.DedupeTTL`. The per-call `ttl` argument is, in practice, one constant per concern. This makes the simpler bucket-level TTL sufficient and removes any need for the newer per-key TTL surface.

## Decision

Add a fourth **Runtime State**, **NATS State**, as its own module `github.com/coder/chat/state/nats`, wired into the workspace like the other adapters.

Back it with JetStream KV using **bucket-level TTL**, one bucket per concern, each created if absent with `History: 1` (an existing bucket is opened, never reconfigured — see Consequences):

- `<prefix>_sub` — TTL 0 (never expires): thread subscriptions.
- `<prefix>_event` — TTL = `DedupeTTL`: event dedupe.
- `<prefix>_lock` — TTL = `ThreadLockTTL`: locks.

`History: 1` ensures an expired or deleted key fully clears its subject so a later `Create` on the same key succeeds. The adapter validates each per-call `ttl` as positive but governs expiry by the bucket TTL; operators set the adapter's `DedupeTTL`/`ThreadLockTTL` to match the runtime's options.

Method mapping, all on the public `jetstream` API:

- subscribe / unsubscribe / check → `Put` / `Delete` / `Get` (a `Get` returning `ErrKeyNotFound` means not subscribed).
- `MarkEvent` → `Create`; `ErrKeyExists` means already seen.
- `AcquireLock` → `Create` storing a random token (reusing `internal/tokens`); `ErrKeyExists` means not acquired.
- `ExtendLock` → `Get` to confirm the **Lock Lease** token, then a revision-gated `Update` — a fresh write renews the entry's age under the bucket TTL.
- `ReleaseLock` → `Get` to confirm the token, then `Delete` with `LastRevision` (fencing-safe).

A revision-gated `Update`/`Delete` that fails with JetStream `wrong last sequence` (code 10071) is treated as "no longer the owner" and returns `false`, not an error. Keys are base64 RawURL-encoded because NATS KV keys may not contain the `:` present in a **Thread ID**. `New` takes a caller-owned `*nats.Conn`, verifies JetStream availability with `AccountInfo`, and `Shutdown` closes the connection once — the adapter owns the conn's lifecycle, consistent with **Redis State** closing its client and **Postgres State** closing its pool.

This is **Semantic Compatibility** work: **NATS State** satisfies the existing `chat.State` contract and the shared conformance suite without changing the contract or the runtime.

## Consequences

- NATS shops can back **Runtime State** with infrastructure they already run, instead of standing up Redis or Postgres for coordination alone.
- Expiry is governed by the bucket TTL, so the per-call `ttl` value is ignored. This is correct only while the runtime passes a uniform TTL per concern (it does). Operators set the adapter's `DedupeTTL`/`ThreadLockTTL` to match the runtime's.
- `New` never reconfigures an existing bucket. If a bucket already exists with a TTL different from the configured one, construction fails with a clear error (`bucket "<name>" exists with TTL X but Y was configured; delete the bucket to change its TTL`) rather than silently rewriting expiry for every client that shares the bucket. A plain `CreateOrUpdateKeyValue` would update the TTL in place, so a later `New` with default or different TTLs would silently widen dedupe to 24h / locks to 2m — avoided. Changing a bucket's TTL is therefore a deliberate migration: delete the bucket to apply a new one. This matches **Postgres State**'s `CREATE TABLE IF NOT EXISTS` (it does not alter an existing schema) and the project's fallible-construction posture.
- A crashed lock holder's **Thread Lock** becomes re-acquirable only after the bucket TTL **plus** the JetStream age-reaper interval (~1s), because an expired-but-not-yet-reaped message still occupies its subject. For a 2m lock this lag is negligible. The same gap means dedupe/lock re-acceptance is effective at TTL + reap latency, and a lock that has passed its TTL but not yet been reaped can still be extended by its holder — Strategy A has no per-entry expiry timestamp to gate against, unlike **Postgres State**'s `expires_at > now()`. The deferred steal-if-expired design (see Alternatives) would close both gaps.
- The never-expiring `_sub` bucket (MaxAge 0) accumulates one delete-marker tombstone per unsubscribed thread, never reclaimed — unlike Redis `DEL` / Postgres `DELETE`, which remove the row outright. For typical chat usage (threads are rarely unsubscribed) the growth is slow and small; a workload with heavy subscribe/unsubscribe churn should periodically purge the bucket, or set a delete-marker TTL on NATS 2.11+.
- The conformance test is the slowest of the four adapters: there is no fake clock for a real JetStream server, so expiry assertions wait on the real reaper (bucket TTL 1s, expiry wait 3s).
- New module, example (`slack-nats-state`), and `go.work`/`mise` wiring; a new `github.com/nats-io/nats.go` dependency (and `nats-io/nats-server/v2` for the embedded test server).
- Requires NATS Server >= 2.9 (JetStream KV). Per-key TTL (2.11) is intentionally not used.

## Alternatives Considered

### Per-key TTL (NATS Server 2.11 `KeyTTL`)

Rejected. Per-key TTL would honor an arbitrary per-call `ttl` exactly, but it requires NATS Server >= 2.11 and nats.go >= v1.42, mandates `History: 1`, and — because `KeyTTL` is create-only and `Update` drops the TTL — forces lock renewal to drop below the high-level KV API to a revision-gated republish carrying the `Nats-TTL` and `Nats-Expected-Last-Subject-Sequence` headers. The runtime's uniform TTLs make all of this unnecessary; bucket-level TTL is simpler and runs on older servers.

### Core NATS (no JetStream)

Rejected. Core NATS has no durable KV, no atomic create-if-absent, and no revision CAS, so it cannot back dedupe or locks without inventing a coordination layer on top.

### Postgres-style "steal-if-expired" lock

Deferred to a possible v2. Storing `expires_at` in the value and doing a CAS steal when the recorded expiry has passed (as **Postgres State** does) would make lock recovery independent of the server's reap cadence. It adds wall-clock comparison and complexity for a lag that is negligible at the configured lock TTL, so v1 relies on the bucket TTL and the reaper instead.
