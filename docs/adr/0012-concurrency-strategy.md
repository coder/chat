# ADR 0012: Concurrency Strategy Expansion (queue / burst / debounce / concurrent)

## Status

Proposed

## Context

The MVP shipped a single **Concurrency Strategy**, `drop`, while deliberately reserving option names for `queue`, `debounce`, `force`, and `concurrent` (MVP PRD: "keep names compatible with future queue, debounce, force, or concurrent strategies"). No design exists for those yet.

ADR 0002 (deferred dispatch) changes the calculus. Under `DispatchDeferred` the **Thread Lock** is held across the entire detached tail — potentially minutes of LLM/tool work — so the previously-rare **Lock Conflict** becomes the common case: any follow-up message in the same **Thread** sent while a deferred handler is still working is, under `drop`, acknowledged and silently dropped. `drop` is a safe default but the wrong behavior for a chatty "thinking" agent. Deferred dispatch needs at least a `queue` strategy to be usable.

The Vercel Chat SDK has proven the full set in production: `drop` (default), `queue`, `burst`, `debounce`, and `concurrent`, plus a `lockScope` (default thread; `channel` for platforms like WhatsApp/Telegram) and a `forceReleaseLock` / `onLockConflict` steerability hook that lets a new message preempt an in-flight handler. Because the repo targets **Semantic Compatibility** with upstream's core model, this is exactly the surface to mirror rather than invent.

This ADR reopens one documented non-goal, surfaced explicitly per `docs/agents/domain.md`:

- **MVP PRD Out of Scope: "Queue, debounce, force, or concurrent Concurrency Strategy implementations."** Reopened because ADR 0002 makes a non-`drop` strategy a functional prerequisite for deferred dispatch, and because the MVP already reserved these names for exactly this expansion. `drop` remains the default and is unchanged.

## Decision

Expand **Concurrency Strategy** to the upstream-aligned set, keeping `drop` the default and unchanged:

- **`drop`** (default): on a **Lock Conflict** the new **Webhook Event** is acknowledged and recorded as unhandled contention. Today's behavior.
- **`queue`**: the new event waits for the in-flight handler to finish, then dispatches within the same lock scope. Only the most recent queued event for a scope is dispatched; superseded events are surfaced as skipped through **Runtime Observation**. This is the companion to `DispatchDeferred`.
- **`debounce`**: each new event resets a timer (`debounceMs`); only the final event in a quiet-period dispatches. For rapid edits/typing.
- **`burst`**: waits `debounceMs` on an idle scope, then dispatches the collected batch. A batch-oriented sibling of `queue`/`debounce`.
- **`concurrent`**: the explicit opt-out of per-scope serialization — every event dispatches immediately in its own execution, bounded by `maxConcurrent`. No **Thread Lock** is taken.

Add a proposed **Lock Scope** option to **Runtime Options** (default `thread`; `channel` available) so an adapter or application can widen serialization from a single **Thread** to a whole channel where the platform's model requires it. The opaque **Thread ID** is unchanged; `lockScope` only chooses what key the **Thread Lock** guards.

Add a proposed **force/steerability** path: a `ForceReleaseLock` on the **Runtime State** contract plus a runtime config hook that lets a new delivery preempt an in-flight handler instead of being dropped or queued. Preemption requires the **Detached Work Context** (ADR 0002) to be cancellable so the preempted handler actually stops; the token-owned **Lock Lease** invariant is preserved — a force release explicitly invalidates the current lease so the preempting delivery can acquire a fresh one, rather than letting an arbitrary holder delete another's lock.

All five strategy names match upstream for **Semantic Compatibility**. `concurrent` is the only mode that abandons the per-scope serialization the **Thread Lock** guarantees, and it does so explicitly.

## Consequences

- `queue` makes `DispatchDeferred` usable for chatty **Threads**: follow-ups sent while the agent is working are handled after the current turn rather than dropped. This is the direct fix for the ADR 0002 default-`drop` follow-up gap.
- `concurrent` lets two handlers for the same **Thread** run and post concurrently; the caller accepts interleaved replies and races on **Thread Application State**. It exists for high-throughput, stateless handlers that do not need serialization.
- `debounce`/`burst` coalesce bursts; superseded events do not run, so handlers must tolerate "the user sent three messages, I only see the last (or the batch)." Skipped events are observable, never silent.
- The **Runtime State** contract grows beyond `drop`'s single `AcquireLock`: `queue`/`debounce`/`burst` need wait/coalesce coordination and `force` needs `ForceReleaseLock`. This touches every state implementation (**Memory State**, **Redis State**, **Postgres State**) and the shared conformance suite — the largest cost of this ADR and the reason it is separate from 0002.
- `force`/steerability is only meaningful with a cancellable detached handler; it is therefore coupled to `DispatchDeferred` + the **Detached Work Context**.
- New surface: additional `ConcurrencyStrategy` values, a `LockScope` option, a `maxConcurrent` bound, and a steerability hook — all under **Runtime Options**, none per-**Routing Hook**.
- `drop` stays the default, so nothing prior changes until an application opts into another strategy.

## Alternatives Considered

### Keep `drop` only (status quo)

Rejected. ADR 0002 holds the **Thread Lock** across long detached work, so `drop` silently drops every mid-work follow-up in a conversation. A bot that thinks for 90 seconds cannot accept "wait, also do X" without `queue`. Deferred dispatch is not usable for chatty agents on `drop` alone.

### Invent Go-original strategy names

Rejected. Upstream's `drop`/`queue`/`burst`/`debounce`/`concurrent` vocabulary is proven and is what the MVP already reserved. **Semantic Compatibility** favors reusing the upstream names so the mental model transfers, rather than coining synonyms the glossary would then have to avoid.

### Fold this into ADR 0002

Rejected. It is a substantial, independently-decidable surface — five strategies, `lockScope`, `maxConcurrent`, a steerability hook, and **Runtime State** contract changes that ripple through the conformance suite. ADR 0002 only depends on the `queue` strategy *existing*; bundling the whole expansion into it would make a focused dispatch decision unreviewable.

### Implement only `queue` (the minimum deferred dispatch needs)

Rejected as the ADR scope, accepted as the likely *implementation* order. The decision records the full upstream-aligned surface so the names and the **Runtime State** contract are designed once; `queue` is expected to land first as the deferred-dispatch companion, with `debounce`/`burst`/`concurrent`/`force` following as separate implementation steps. Designing them together avoids a second contract migration later.
