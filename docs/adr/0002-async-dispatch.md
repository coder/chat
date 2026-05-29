# ADR 0002: Deferred Runtime Dispatch (ack-then-work)

## Status

Proposed

## Context

The **Go Chat Runtime** performs **Runtime Dispatch** synchronously. The adapter's **Webhook Handler** calls `DispatchFunc`, which blocks until the **Routing Hook** handler returns, all under the inbound request's **Dispatch Context**. That context is cancelled when the adapter writes its HTTP response. The MVP preserved a boundary for future deferred execution but never built it.

Every target platform forces a fast acknowledgement and expects subsequent work out of band:

- Slack HTTP/Events API: acknowledge with a 2xx within 3 seconds, then do work async.
- Linear agent sessions: emit a first **Agent Activity Thought** within ~10s of `created`, with follow-ups for ~30 minutes.
- Microsoft Teams (future, ADR 0007): its own turn/invoke ack contract.

Under synchronous dispatch an application that runs an LLM call or multi-step tool use inside a handler either blows the platform deadline or hand-rolls a detach: a goroutine, a fresh `context.Background()`, and a private re-implementation of **Thread Lock** holding and **Runtime State** coordination. That hand-rolled path drops the lock, races other deliveries for the same **Thread**, and leaks runtime invariants into application code.

This ADR reopens two documented non-goals, surfaced explicitly per `docs/agents/domain.md`:

- **ADR-0001 alternative "Dispatch Linear webhooks asynchronously"** (Rejected for MVP): rejected because "the current Go runtime defines synchronous dispatch. Long-running Linear agents should post an early thought and enqueue follow-up work in application code until a runtime-level deferred dispatch model is designed." This ADR is that runtime-level model, so the precondition for reopening is met.
- **MVP PRD Implementation Decision: "Runtime Dispatch is synchronous in the MVP ... Long-running work must be explicitly detached or queued by application code."** Reopened because the per-application detach it mandates cannot hold the **Thread Lock** or honor the **Detached Work Context**'s coordination, which the runtime owns. The synchronous default is preserved; deferred dispatch is added beside it, not in place of it.

Neither reopening contradicts the prior decisions: synchronous dispatch remains the default and unchanged. Both prior docs scoped async out *for the MVP* and pointed at a future runtime design. This is that design.

**Upstream precedent and where we diverge.** The Vercel Chat SDK has *no* sync/deferred switch: it always acknowledges then works via the serverless `waitUntil`/`after` primitive, holds the per-**Thread** lock across the *entire* post-ack handler (including the LLM call), and treats post-ack work as best-effort/in-process (durable execution is a separate product, Vercel Workflow). Two of those validate decisions below — holding the lock across the tail, and a non-durable in-process tail. But "always defer" is a serverless artifact: a serverless function is killed once it writes its HTTP response, so `waitUntil` is the only way to keep working. The **Go Chat Runtime** mounts into a long-lived `net/http` server and has no such constraint, so it keeps synchronous dispatch as the Go-idiomatic default — a 2xx that reflects the actual handler outcome for handlers that finish within the platform deadline — and adds **Ack-Then-Work** as an explicit opt-in for work that cannot. Per `docs/agents/domain.md`'s divergence rule, this is a justified divergence: upstream's shape is dictated by a platform constraint Go does not share.

Related ADRs (decisions not redefined here): command/interaction events and their long work (ADR 0003, ADR 0004) reuse this primitive; outbound rate-limit backoff (ADR 0005) is adapter-owned and bounded by a caller context; multi-tenant install (ADR 0006) is unaffected; Teams proactive posting (ADR 0007) reuses **Ack-Then-Work**; Linear's **Agent Session Timing Contract** (ADR 0008) is satisfied by it; observation of deferred dispatch extends through the ADR 0010 **Observation Hook**; resumable streaming (ADR 0011) is deferred (not foreclosed) and points at this seam for long generation. The **Concurrency Strategy** expansion that makes `DispatchDeferred` usable for chatty **Threads** (`queue`/`burst`/`debounce`/`concurrent` plus `lockScope` and a force/steerability hook) is ADR 0012; this ADR depends on it for the `queue` companion strategy.

## Decision

Add a proposed **Dispatch Mode** to **Runtime Options**, **orthogonal** to **Concurrency Strategy**:

```go
type DispatchMode int

const (
	DispatchSync     DispatchMode = iota // default: today's synchronous behavior
	DispatchDeferred                     // ack-then-work
)

type RuntimeOptions struct {
	DedupeTTL     time.Duration
	ThreadLockTTL time.Duration
	Concurrency   ConcurrencyStrategy
	Dispatch      DispatchMode  // default DispatchSync
	DetachTimeout time.Duration // bounds the Detached Work Context under DispatchDeferred
}
```

**Concurrency Strategy** answers "what happens on a **Lock Conflict**" — `drop` today; `queue`/`burst`/`debounce`/`concurrent` plus `lockScope` and a force/steerability hook are designed in ADR 0012. **Dispatch Mode** answers "does the handler run before or after the adapter acknowledges the platform". They are independent axes; `Concurrency` is not overloaded to mean async. Both are runtime-wide **Runtime Options**, not per-**Routing Hook** settings — matching upstream, which exposes deferral and concurrency only as global `Chat`-instance properties, never per handler.

Define a proposed **Ack-Then-Work** contract for `DispatchDeferred`. The runtime splits dispatch into a synchronous prelude and a detached tail:

- **Prelude** (request context, before ack): `validateEvent`, dedupe via `MarkEvent`, `AcquireLock`, `ValidateThreadID`, **Self Message** filtering, and the route decision. Duplicate, **Lock Conflict**, ignored, and unrouted events resolve here and are acknowledged with no detached work. A failed prelude returns an error to `DispatchFunc`.
- **Tail** (proposed **Detached Work Context**, after ack): the resolved **Routing Hook** handler runs.

The adapter acknowledges the platform within its deadline after the prelude returns, exactly as **Platform Handshake** ack is adapter-owned. `DispatchFunc` keeps its signature; only *when* it returns and *which* context the handler runs under change.

The **Detached Work Context** is a runtime-derived `context.Context` established from a long-lived runtime base context at construction, NOT the request context and NOT `context.Background()`. It carries runtime values, is bounded by `DetachTimeout`, and is cancelled by **Runtime Shutdown**.

Lock and state behavior when work outlives the request:

- The **Thread Lock** is acquired in the prelude (before ack) and held across the detached tail. The runtime refreshes the **Lock Lease** via `ExtendLock` on a cadence derived from `ThreadLockTTL`; the 2m TTL is a lease, not a hard work deadline. Release happens when the tail exits, not when the request context dies.
- **Runtime State** mutations in the tail follow the **Detached Work Context**. Lock release and extend in the tail run under `context.WithoutCancel` over that context so cleanup still happens on cancellation, mirroring today's synchronous release.
- The token-owned **Lock Lease** invariant is unchanged: release and extend verify the ownership token so an expired holder cannot affect a newer holder.

Deferred dispatch is a runtime concern, not an **Optional Capability**. Adapters implement no new interface method; they only acknowledge around `DispatchFunc` within the platform deadline. The small **Adapter** interface, opaque adapter-produced **Thread ID**, single-slot **Routing Hooks**, **Postable Message**, and the **Platform Escape Hatch** / **Adapter Access** paths are unchanged.

## Consequences

- A handler under `DispatchDeferred` can call an LLM and use tools without blowing Slack's 3s, Linear's 10s, or Teams' turn deadline, while the runtime still owns dedupe, locking, and state coordination.
- The **Thread Lock** now spans potentially minutes of detached work, so `ExtendLock` becomes load-bearing rather than incidental. A bug in lease refresh would let a competing delivery in mid-handler; this is covered by tests.
- Under the default `drop` **Concurrency Strategy**, holding the lock across the tail means a follow-up message in the same **Thread** sent while a deferred handler is still working hits a **Lock Conflict** and is dropped (acknowledged, not handled). That is an acceptable *default* but the wrong behavior for a chatty "thinking" agent: the `queue` strategy (ADR 0012) is the intended companion to `DispatchDeferred`, and an application expecting mid-work follow-ups should select it. Upstream pairs its always-deferred dispatch with exactly this strategy choice.
- `DispatchSync` stays the default, so existing Slack and Linear MVPs are byte-for-byte unaffected until they opt in. The reopened non-goals stay honored as defaults.
- `DispatchSync` keeps the platform's 2xx *truthful* for handlers that finish within the deadline: the ack reflects the real handler outcome, and a crash before ack lets the platform's own retry (deduped by **Event Identity**) cover it. `DispatchDeferred` trades that away — ack is decoupled from completion, and a crash mid-tail loses the work with no platform retry (it already received its 2xx). Choosing deferred is choosing fast-ack over outcome-truthful-ack, knowingly.
- Acknowledgement semantics are preserved: an **Accepted Event** is acknowledged by default; a handler that fails *after* ack is recorded through **Runtime Observation** and not retried by the runtime (the platform already received its 2xx). The runtime invents no retry of its own.
- **Runtime Shutdown** gains a drain step: it cancels the **Detached Work Context** and waits (bounded) for in-flight detached handlers before state shutdown, returning joined errors and staying idempotent. A crash mid-detach abandons that work; the **Lock Lease** simply expires.
- This single primitive is reused by command/interaction long work (ADR 0003/0004), Teams proactive posting (ADR 0007), and Linear session timing (ADR 0008), avoiding three divergent per-adapter detach hacks.
- New surface area: one **Runtime Options** field pair (`Dispatch`, `DetachTimeout`), a runtime base context, a refresh loop, and shutdown drain. No new public types beyond the `DispatchMode` enum.

## Alternatives Considered

### Keep dispatch synchronous; require app-code detach (the MVP status quo)

Rejected. This is exactly what the reopened non-goals mandated for the MVP, and it does not scale past the MVP: app-code detach cannot hold or refresh the **Thread Lock**, so concurrent deliveries for the same **Thread** race, and each application re-implements context plumbing the runtime should own.

### Always ack-then-work and drop the Dispatch Mode enum (mirror upstream exactly)

Rejected. Upstream has no sync/deferred switch because its serverless host kills the function after the HTTP response, so deferral via `waitUntil` is the only option. The **Go Chat Runtime** runs in a long-lived process and has no such constraint. Always acking first would discard synchronous dispatch — today's default and the only mode where the platform's 2xx reflects the actual handler outcome — and force every bot through goroutine, lease-refresh, and shutdown-drain machinery even when its handlers are sub-second. `DispatchSync` stays the default; `DispatchDeferred` is the opt-in. This is a deliberate, justified divergence from upstream per the repo's divergence rule.

### Overload Concurrency Strategy with an async/queue value

Rejected. **Concurrency Strategy** is the **Lock Conflict** policy; async is about ack timing, an independent axis. A `ConcurrencyAsync` value would conflate "run later" with "what to do when the **Thread** is busy" and break the reserved queue/debounce/force/concurrent naming. **Dispatch Mode** stays orthogonal per the shared design.

### Run detached work on context.Background()

Rejected. `context.Background()` drops runtime-derived values, cannot be bounded, and cannot be cancelled on **Runtime Shutdown**, so a stuck handler leaks a goroutine and holds a **Thread Lock** forever. The **Detached Work Context** is runtime-derived, deadline-bounded, and shutdown-cancellable instead.

### Acknowledge before acquiring the Thread Lock

Rejected. Acking first and locking inside the detached tail opens a window where two deliveries for the same **Thread** both ack and both start work, breaking the per-**Thread** serialization the **Thread Lock** guarantees. The lock is acquired in the synchronous prelude, before ack.

### Make deferred dispatch an Optional Capability the adapter implements

Rejected. The detach lifecycle, lock holding, lease refresh, context derivation, and shutdown drain are runtime coordination, not platform rendering. Pushing them behind an adapter interface would re-leak runtime invariants into adapters, the opposite of the goal. Adapters only need to acknowledge around `DispatchFunc` within the platform deadline.

### Build a durable job queue with at-least-once handler delivery

Rejected for this ADR. Durable retry, crash resumption, and at-least-once delivery are a persistent task system, not an in-process dispatch mode. The **Detached Work Context** is in-process; applications needing durable retries own their own queue keyed by **Thread ID**. A future ADR may revisit a durable variant, but it must not block ack-then-work.

### Add a runtime AI-stream layer for long generation

Rejected. The runtime posts finished **Postable Messages**, not token streams; client resume, server pub/sub, and Redis stream persistence are app/LLM concerns (ADR 0011). Long generation uses **Ack-Then-Work** / **Detached Work Context**, not a runtime stream layer.
