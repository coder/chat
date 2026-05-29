# Concurrency Strategy Expansion

Status: needs-triage

## Problem Statement

The **Go Chat Runtime** ships one **Concurrency Strategy**, `drop`: when a **Webhook Event** arrives for a **Thread** whose **Thread Lock** is held, it is acknowledged and recorded as unhandled contention. The MVP reserved names for `queue`, `debounce`, `force`, and `concurrent` but designed none of them.

`drop` is correct only while the lock window is tiny. ADR 0002 (deferred dispatch) holds the **Thread Lock** across the entire detached tail — potentially minutes of LLM/tool work — so a **Lock Conflict** stops being rare and becomes the normal case for an active conversation. Under `drop`, every follow-up a user sends while the bot is "thinking" is silently dropped. A deferred-dispatch agent is not usable for chatty conversations without a non-`drop` strategy.

## Solution

Expand **Concurrency Strategy** to the upstream-aligned set, keeping `drop` the default and unchanged, and reusing the existing token-owned **Lock Lease**:

- **`queue`** — wait for the in-flight handler, then dispatch the most recent superseded event; the companion to `DispatchDeferred`.
- **`debounce`** — only the final event in a quiet period dispatches.
- **`burst`** — dispatch the collected batch after an idle gap.
- **`concurrent`** — explicit opt-out of per-scope serialization, bounded by `maxConcurrent`; no **Thread Lock** taken.

Add a **Lock Scope** option (`thread` default; `channel`) so serialization can widen where a platform requires it, without changing the opaque **Thread ID**. Add a **force/steerability** path (`ForceReleaseLock` on **Runtime State** plus a runtime hook) so a new delivery can preempt an in-flight deferred handler rather than be dropped or queued; preemption cancels the **Detached Work Context** and preserves the **Lock Lease** ownership-token invariant.

This is **Semantic Compatibility** work: the names and behaviors mirror the upstream Chat SDK rather than inventing new ones.

## User Stories

1. As a deferred-dispatch bot developer, I want a `queue` **Concurrency Strategy**, so that a follow-up message sent while the agent is working is handled after the current turn instead of being dropped.
2. As a bot developer, I want a `debounce` strategy, so that a user editing or sending several quick messages triggers one handler run on the final message.
3. As a bot developer, I want a `burst` strategy, so that a rapid batch of messages is collected and handled together after a short idle gap.
4. As a high-throughput integration author, I want a `concurrent` strategy bounded by `maxConcurrent`, so that independent stateless events in one **Thread** are not serialized when I explicitly accept interleaving.
5. As an adapter author for a channel-centric platform, I want a `channel` **Lock Scope**, so that serialization matches the platform's conversation model without changing the **Thread ID**.
6. As a bot developer running long deferred handlers, I want a force/steerability option, so that a new user message can interrupt an in-flight handler instead of waiting or being dropped.
7. As a runtime operator, I want superseded and skipped events surfaced through **Runtime Observation**, so that coalescing is explainable and never silent.
8. As a runtime operator, I want `drop` to remain the default, so that existing deployments are unchanged until they opt in.

## Implementation Decisions

- `drop` semantics are unchanged; the new strategies are additive **Runtime Options** values.
- Strategy names match the upstream Chat SDK (`drop`/`queue`/`burst`/`debounce`/`concurrent`) for **Semantic Compatibility**; the glossary gains these as proposed terms only when this ADR is accepted.
- `queue`/`debounce`/`burst` coordinate within the lock scope and rely on the **Detached Work Context** (ADR 0002) for any work that outlives the request.
- `concurrent` takes no **Thread Lock**; it is the only mode that abandons per-scope serialization, and it is explicit.
- **Lock Scope** chooses the **Thread Lock** key (thread vs channel); it does not alter the opaque **Thread ID**.
- The **Runtime State** contract grows: wait/coalesce coordination for `queue`/`debounce`/`burst` and `ForceReleaseLock` for steerability. **Memory State**, **Redis State**, and **Postgres State** must all implement it, and the shared state conformance suite covers it.
- Force release preserves the **Lock Lease** ownership-token invariant: it invalidates the current lease explicitly so a preempting delivery acquires a fresh one; it is not an untokened delete.
- All options live under **Runtime Options**, not per-**Routing Hook**, matching ADR 0002's global dispatch surface.
- `queue` is expected to land first (the deferred-dispatch companion); the others follow as separate implementation steps under one designed contract.

## Testing Decisions

- State conformance tests cover each strategy's coordination against **Memory State**, **Redis State**, and **Postgres State**.
- `queue` tests cover supersession (only the latest queued event dispatches), ordering, and skipped-event observation.
- `debounce`/`burst` tests cover timer reset, batch collection, and that superseded events are surfaced, not silently lost.
- `concurrent` tests cover no-lock dispatch, the `maxConcurrent` bound, and that **Thread Application State** races are the caller's responsibility.
- Force/steerability tests cover preemption cancelling the **Detached Work Context**, the **Lock Lease** ownership-token invariant under force release, and that a stale holder cannot affect a newer lease.
- `lockScope` tests cover thread-vs-channel serialization keys.
- Regression: `drop` remains the default and byte-for-byte unchanged.

## Out of Scope

- Durable/at-least-once queuing across process restarts — the queue is in-process, consistent with ADR 0002's rejection of a durable job system. Durable retries remain application-owned, keyed by **Thread ID**.
- Per-**Routing Hook** strategy selection — strategy is a runtime-wide **Runtime Options** setting.
- Cross-**Thread** global concurrency limits beyond `maxConcurrent` for the `concurrent` strategy.
- Priority queues or fairness policies between **Threads**.

## Further Notes

- This work item was surfaced while grilling ADR 0002: deferred dispatch holding the lock across the tail makes `drop` drop follow-ups, so a `queue` strategy is a functional prerequisite rather than a nice-to-have.
- ADR 0012 records the decision and the full upstream-aligned surface; ADR 0002 depends only on the `queue` strategy existing.
- Use the upstream Chat SDK concurrency model as behavioral precedent; diverge only where its serverless execution model (not its concurrency semantics) leaks in.
