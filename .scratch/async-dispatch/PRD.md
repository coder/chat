# Deferred Runtime Dispatch (ack-then-work)

Status: needs-triage

## Problem Statement

The **Go Chat Runtime** performs **Runtime Dispatch** synchronously: the adapter's **Webhook Handler** calls `DispatchFunc` and blocks until the handler returns, all under the inbound request's **Dispatch Context**. That context dies when the adapter writes its HTTP response.

Every real platform forces a fast acknowledgement, then expects work to continue out of band:

- Slack HTTP/Events API requires a 2xx within 3 seconds, then async work.
- Linear agent sessions require a first **Agent Activity Thought** within ~10s of `created`, with follow-ups allowed for ~30 minutes.
- Microsoft Teams (future) has its own turn/invoke ack contract.

Today an application that does long work (an LLM call, tool use, multi-step reasoning) inside `OnNewMention` or `OnSubscribedMessage` either blows the platform ack deadline or must hand-roll its own detach: spawn a goroutine, build a fresh `context.Background()`, and re-implement lock holding and state coordination outside the runtime. That hand-rolled path loses the **Thread Lock**, races other deliveries for the same **Thread**, and leaks the runtime's coordination invariants into application code. ADR-0001 and the MVP PRD explicitly punted this: "Long-running work must be explicitly detached or queued by application code."

The runtime needs deferred dispatch as a first-class primitive so the adapter can acknowledge fast and the handler can run long, under runtime-owned context and locking, without each adapter or application reinventing it.

This is a **principled divergence** from the upstream Vercel Chat SDK, which has no sync/deferred switch: it always acks-then-works via the serverless `waitUntil`/`after` primitive. Always-defer is a serverless artifact — a serverless function is killed once it writes its HTTP response, so `waitUntil` is the only way to keep working. The **Go Chat Runtime** mounts into a long-lived `net/http` server and does not share that constraint, so it keeps `DispatchSync` as the Go-idiomatic default and adds `DispatchDeferred` as an explicit opt-in. Per the repo divergence rule, this is justified: upstream's shape is dictated by a platform constraint Go does not have.

## Solution

Add a **Dispatch Mode** to **Runtime Options**: `DispatchSync` (default, today's behavior) and `DispatchDeferred`. The mode is **orthogonal** to **Concurrency Strategy** (`ConcurrencyDrop` today; `queue`/`burst`/`debounce`/`concurrent` plus `lockScope` and a force/steerability hook are a separate work item, ADR 0012). Concurrency answers "what happens on a **Lock Conflict**"; **Dispatch Mode** answers "does the handler run before or after the adapter acknowledges the platform". Do not overload `Concurrency` to mean async, and do not redesign concurrency here — this PRD depends on ADR 0012 for the `queue` companion strategy.

**Dispatch Mode** is a single runtime-wide **Runtime Options** value (global, not per-**Routing Hook**), matching upstream, which exposes deferral and concurrency only as global `Chat`-instance properties, never per handler.

Define **Ack-Then-Work** as the contract for `DispatchDeferred`:

1. The runtime dedupes the **Event**, acquires the **Thread Lock** **before** ack, and (if it must run a handler) starts the handler on a runtime-managed **Detached Work Context**.
2. `DispatchFunc` returns to the adapter immediately after the synchronous prelude (verify, dedupe, lock, route decision) succeeds, so the adapter can acknowledge within the platform deadline.
3. The handler continues on the **Detached Work Context**, which outlives the dead request context. The **Thread Lock** is held and refreshed via `ExtendLock` across the detached work; **Runtime State** mutations follow the **Detached Work Context**, not the request context.

The **Detached Work Context** is a runtime-derived `context.Context`, NOT `context.Background()`. It carries runtime values, is bounded by a configurable deadline, and is cancelled on **Runtime Shutdown** so detached work drains.

The **Thread Lock** is held across the entire detached tail. Under the default `drop` **Concurrency Strategy**, this means a follow-up message in the same **Thread** sent while a deferred handler is still working hits a **Lock Conflict** and is dropped (acked, not handled). That is an acceptable *default* but the wrong behavior for a chatty "thinking" agent: the `queue` strategy (ADR 0012) is the intended companion to `DispatchDeferred`, and applications expecting mid-work follow-ups should select it.

This is a runtime-level seam the **Platform Adapter** opts into through how it implements `Webhook`/`DispatchFunc`. It does not change the small **Adapter** interface, the opaque adapter-produced **Thread ID**, single-slot **Routing Hooks**, or the **Platform Escape Hatch** / **Optional Capability** path. It replaces the per-adapter "post a thought then enqueue follow-up in app code" workaround with one shared primitive.

### Shapes (illustrative, not implementation)

```go
type DispatchMode int

const (
	DispatchSync     DispatchMode = iota // default: handler runs under the request context, ack after handler
	DispatchDeferred                     // ack-then-work: handler runs under the Detached Work Context
)

type RuntimeOptions struct {
	DedupeTTL     time.Duration
	ThreadLockTTL time.Duration
	Concurrency   ConcurrencyStrategy
	Dispatch      DispatchMode  // default DispatchSync
	DetachTimeout time.Duration // bound on the Detached Work Context; only meaningful under DispatchDeferred
}
```

`DispatchFunc` keeps its signature; under `DispatchDeferred` the runtime returns to the adapter after the synchronous prelude and runs the handler on the **Detached Work Context** it owns.

```go
type DispatchFunc func(context.Context, *Event) error
```

## User Stories

1. As a Go application developer, I want a `DispatchDeferred` **Dispatch Mode**, so that my handler can call an LLM and use tools without blowing the platform ack deadline.
2. As a Go application developer, I want **Dispatch Mode** to default to `DispatchSync`, so that existing bots keep today's exact synchronous behavior with no code change.
3. As a Go application developer, I want **Dispatch Mode** to be orthogonal to **Concurrency Strategy**, so that I can choose deferred dispatch without changing my **Lock Conflict** policy.
4. As a Slack bot developer, I want the adapter to acknowledge Slack within 3 seconds, so that Slack does not mark my app unresponsive or retry.
5. As a Linear app developer, I want **Ack-Then-Work** to let me post the first **Agent Activity Thought** within ~10s and a final **Agent Activity Response** later, so that long agent sessions stay within Linear's **Agent Session Timing Contract** (ADR 0008).
6. As a runtime operator, I want the **Thread Lock** acquired before ack and held across detached work, so that two deliveries for the same **Thread** do not run concurrently under deferred dispatch.
7. As a runtime operator, I want the **Thread Lock** refreshed via `ExtendLock` during long work, so that the 2m `ThreadLockTTL` lease does not expire mid-handler and let another delivery in.
8. As a runtime operator, I want the **Lock Lease** ownership token honored across detach, so that release and extend still verify ownership exactly as in synchronous dispatch.
9. As a Go application developer, I want handler work to run under a **Detached Work Context**, not `context.Background()`, so that runtime-derived values and cancellation still apply after the request ends.
10. As a Go application developer, I want **Runtime State** mutations in a detached handler to follow the **Detached Work Context**, so that `Subscribe`/`Unsubscribe`/`ExtendLock` do not fail against a dead request context.
11. As a runtime operator, I want a configurable `DetachTimeout` bound on the **Detached Work Context**, so that a stuck handler cannot hold a **Thread Lock** or leak a goroutine forever.
12. As a runtime operator, I want **Runtime Shutdown** to cancel the **Detached Work Context** and drain in-flight detached work, so that deploys do not abandon running handlers silently.
13. As a runtime operator, I want a **Lock Conflict** under deferred dispatch handled by the same **Concurrency Strategy** as synchronous dispatch (drop by default, acknowledged), so that conflict behavior is consistent across modes — and I understand that under the default `drop` strategy a follow-up in the same **Thread** sent while a deferred handler works is dropped, so a chatty agent should select the `queue` companion strategy (ADR 0012).
14. As a Go application developer, I want the synchronous prelude (verify, dedupe, lock, route decision) to complete before ack, so that a duplicate or locked event is still acknowledged correctly even though the handler runs later.
15. As a Go application developer, I want detached handler errors recorded through **Runtime Observation**, so that failures after ack are still explainable even though the platform already got a 2xx.
16. As a runtime operator, I want deferred dispatch latency, detach starts, **Lock Conflict** counts, lock-extend refreshes, and detach timeouts surfaced through **Runtime Observation**, so that I can see ack-then-work behavior in production (extended by the ADR 0010 **Observation Hook**).
17. As an adapter author, I want **Ack-Then-Work** to be a runtime-owned primitive, so that I do not hand-roll goroutines, fresh contexts, and lock holding in each adapter.
18. As a future adapter author (Teams), I want to reuse **Ack-Then-Work** and the **Detached Work Context** for proactive posting, so that the Teams turn/ack contract (ADR 0007) does not need a bespoke detach mechanism.
19. As a Go application developer, I want command and interaction handlers (ADR 0003/0004) to use the same **Ack-Then-Work** primitive, so that long command work does not need its own detach path.
20. As a future maintainer, I want resumable AI streaming to remain deferred (not foreclosed) and pointed at this seam (ADR 0011), so that long generation uses **Detached Work Context** today, not a runtime stream layer.

## Implementation Decisions

- Add `Dispatch DispatchMode` to **Runtime Options** as a single runtime-wide value (global, not per-**Routing Hook**), defaulting to `DispatchSync`. This matches upstream, which exposes deferral and concurrency only as global `Chat`-instance properties. `DispatchSync` is exactly today's behavior: handler runs under the request **Dispatch Context**, ack after the handler returns.
- Keep **Dispatch Mode** orthogonal to **Concurrency Strategy**. Do not extend the `Concurrency` enum to express async; do not let `Dispatch` change **Lock Conflict** policy. The `queue`/`burst`/`debounce`/`concurrent` expansion (plus `lockScope` and force/steerability) is a separate work item, ADR 0012, not redesigned here; this PRD depends on ADR 0012 for the `queue` companion to `DispatchDeferred`.
- Add `DetachTimeout time.Duration` to **Runtime Options**, used only under `DispatchDeferred` to bound the **Detached Work Context**. Validate it is positive when `Dispatch == DispatchDeferred`; ignore it under `DispatchSync`.
- Under `DispatchDeferred`, split dispatch into a synchronous prelude and a detached tail:
  - Prelude (under the request context, before ack): `validateEvent`, dedupe via `MarkEvent`, `AcquireLock`, `ValidateThreadID`, self-message filtering, and the route decision. A failed prelude returns an error to `DispatchFunc` so the adapter can choose its HTTP status; a duplicate, **Lock Conflict**, ignored, or unrouted event is resolved in the prelude and acknowledged, with no detached work started.
  - Tail (under the **Detached Work Context**, after ack): the resolved **Routing Hook** handler runs.
- Acquire the **Thread Lock** in the prelude (before ack) and release it when the detached tail exits, not when the request context ends. This moves lock release off the request `defer` and onto the **Detached Work Context** lifecycle.
- Hold and refresh the **Thread Lock** across the entire detached tail. The runtime refreshes the **Lock Lease** via `ExtendLock` on a cadence derived from `ThreadLockTTL` (the existing 2m TTL is a lease, not a hard work deadline). Release still verifies the ownership token; the token-owned "release/extend verify ownership" invariant from CONTEXT.md is unchanged. Because the lock is held across the tail, under the default `drop` **Concurrency Strategy** a follow-up message in the same **Thread** sent mid-handler is dropped (acked, not handled); the `queue` strategy (ADR 0012) is the intended companion for chatty agents.
- Derive the **Detached Work Context** from a long-lived runtime base context (established at construction), NOT from the request and NOT from `context.Background()`. It is bounded by `DetachTimeout` and cancelled by **Runtime Shutdown**.
- **Runtime State** mutations in the detached tail use the **Detached Work Context**. The lock-release and lock-extend operations in the tail use `context.WithoutCancel` over the **Detached Work Context** so cleanup still runs when the work context is cancelled, mirroring today's synchronous release.
- Track in-flight detached work so **Runtime Shutdown** can cancel the **Detached Work Context** and wait (bounded) for handlers to drain before state shutdown, consistent with idempotent attempt-all shutdown.
- The detached tail does not change acknowledgement semantics for the platform: an **Accepted Event** is acknowledged by default; a handler that fails after ack is recorded through **Runtime Observation** and is not retried by the platform (the platform already received its 2xx). The runtime does not invent its own retry.
- Keep `DispatchFunc` unchanged in signature. The adapter still calls `DispatchFunc(ctx, event)`; under `DispatchDeferred` the runtime returns after the prelude. The adapter decides its ack ordering relative to the `DispatchFunc` call, the same way **Platform Handshake** ack is adapter-owned.
- Deferred dispatch is a runtime concern, not an adapter capability flag. Adapters do not implement a new interface method to support it; they only need a `Webhook` implementation that acknowledges around `DispatchFunc` within the platform deadline.
- Do not change the small **Adapter** interface, opaque **Thread ID**, single-slot **Routing Hooks**, **Postable Message**, or the **Platform Escape Hatch** / **Adapter Access** paths.
- Cross-reference: the **Concurrency Strategy** expansion that makes `DispatchDeferred` usable for chatty **Threads** is ADR 0012, and this PRD depends on it for the `queue` companion; command/interaction long work (ADR 0003/0004) reuses this primitive; Teams proactive posting (ADR 0007) reuses this primitive; Linear's **Agent Session Timing Contract** (ADR 0008) is satisfied by it; resumable streaming (ADR 0011) is deferred (not foreclosed) and points here; observation of deferred dispatch extends through the ADR 0010 **Observation Hook**.

## Testing Decisions

- Tests assert external behavior and public contracts, not private dispatch internals.
- **Dispatch Mode** default tests: with no `Dispatch` set, behavior is byte-for-byte `DispatchSync`; the request context still bounds the handler; lock release still happens on handler exit.
- Ack-ordering tests under `DispatchDeferred`: a fake adapter records when its ack fires relative to handler start and proves ack precedes long handler work, and that ack still fires for duplicate, **Lock Conflict**, ignored, and unrouted events resolved in the prelude.
- Prelude-failure tests: a failed dedupe/lock/validate in the prelude returns an error to `DispatchFunc` and starts no detached work.
- **Thread Lock** holding tests: under deferred dispatch a second delivery for the same **Thread** while the first handler runs hits a **Lock Conflict** and is dropped (default **Concurrency Strategy**), proving the lock is held across detached work.
- Lock-refresh tests: a detached handler running longer than `ThreadLockTTL` keeps the **Lock Lease** alive via `ExtendLock`, and a competing delivery still conflicts; ownership-token compare is still enforced on extend and release.
- **Detached Work Context** tests: the handler context is not the request context (request cancellation does not cancel the handler), is not `context.Background()` (it carries runtime values and is cancellable), and is cancelled at `DetachTimeout`.
- State-under-detach tests: `Subscribe`/`Unsubscribe`/`ExtendLock` issued by a detached handler succeed against the **Detached Work Context** after the request context is cancelled, and lock release/extend still run during work-context cancellation.
- Shutdown tests: **Runtime Shutdown** cancels the **Detached Work Context**, waits (bounded) for in-flight detached handlers, then runs state shutdown; joined errors are returned; shutdown remains idempotent.
- Observation tests: deferred dispatch latency, detach start, lock-extend refresh, **Lock Conflict**, detach timeout, and post-ack handler error are surfaced through **Runtime Observation**.
- Validation tests: `DetachTimeout` must be positive when `Dispatch == DispatchDeferred`; **Runtime Construction** fails otherwise. `DispatchSync` ignores `DetachTimeout`.
- Adapter integration tests: the Slack adapter acknowledges within 3s under deferred dispatch using a fake HTTP boundary; the Linear adapter posts a first thought within the **Agent Session Timing Contract** window (cross-referenced to ADR 0008's tests, not redefined here).

## Out of Scope

- The `queue`/`burst`/`debounce`/`concurrent` **Concurrency Strategy** expansion (plus `lockScope` and force/steerability). That is a separate work item, ADR 0012. **Dispatch Mode** is orthogonal and does not implement it; this PRD only depends on ADR 0012 for the `queue` companion to `DispatchDeferred`.
- A durable job queue, retry-with-backoff of failed handlers, or at-least-once handler delivery after ack. The **Detached Work Context** is in-process work, not a persistent task system. Apps that need durable retries own their own queue keyed by **Thread ID**.
- Cross-process resumption of detached work after a crash. If the process dies mid-detach, the **Thread Lock** lease simply expires and the platform may or may not redeliver per its own rules; the runtime does not persist handler progress.
- Resumable AI streaming (client resume + server pub/sub + Redis stream persistence). Deferred, not foreclosed (ADR 0011); the runtime posts finished **Postable Messages**, not token streams today, and long generation uses **Ack-Then-Work** / **Detached Work Context**.
- New **Routing Hooks** or changes to single-slot semantics. Command/interaction hooks are ADR 0003/0004; this PRD only provides the dispatch primitive they reuse.
- Changing **Postable Message**, the **Adapter** interface, **Thread ID** opacity, or **Adapter Access**.
- A new tenant identifier or any change to **Platform Tenant** scoping.
- Platform-specific ack mechanics (Slack `response_url`/`trigger_id`, Teams invoke responses). Those stay adapter-owned, consistent with **Platform Handshake**.

## Further Notes

- The synchronous prelude / detached tail split keeps every existing dispatch invariant: dedupe by **Event Identity**, token-owned **Lock Lease**, **Lock Conflict** acknowledged-and-dropped by default, **Self Message** filtering, **Accepted Event** acknowledged even on handler failure. Only *when* the handler runs and *which* context it runs under change.
- `DispatchSync` remains the default precisely so the existing Slack and Linear MVPs are unaffected until they opt in. The MVP PRD's "Runtime Dispatch is synchronous" decision is reopened deliberately (see ADR 0002 Context), not contradicted: synchronous stays the default, deferred is added beside it.
- Keeping `DispatchSync` the default is a **principled divergence** from upstream's always-defer (serverless `waitUntil`) model, justified because always-defer is a serverless artifact a long-lived Go server does not share. The rationale is **truthful ack**: under `DispatchSync` the platform 2xx reflects the real handler outcome for handlers that finish within the deadline, and a crash before ack lets the platform's own retry (deduped by **Event Identity**) cover it. `DispatchDeferred` decouples ack from completion and loses work on a crash mid-tail with no platform retry (it already got its 2xx). Choosing deferred is choosing fast-ack over outcome-truthful-ack, knowingly.
- The 2m `ThreadLockTTL` was always documented as a **Lock Lease**, not a hard deadline. Deferred dispatch makes the lease-refresh path (`ExtendLock`) load-bearing rather than incidental.
- This is the shared primitive behind Slack 3s, Linear 10s, and the future Teams turn deadline. Designing it once here avoids three divergent per-adapter detach hacks.
