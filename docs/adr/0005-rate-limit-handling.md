# ADR 0005: Platform API Rate-Limit Handling

## Status

Accepted

## Context

The **Go Chat Runtime** posts outbound replies through a **Platform Adapter**. Every platform throttles its API: Slack Web API returns HTTP 429 with a `Retry-After` header (seconds); Linear GraphQL enforces request and complexity limits with documented reset hints; a future Teams adapter (ADR 0007) is throttled by the Bot Framework REST connector, which also uses `Retry-After`.

The MVP adapters do not retry. README.md ("Intentional MVP Gaps") records "no Slack Web API rate-limit retry/backoff policy" as a deliberate deferral, so a single 429 fails `PostMessage` and drops the reply. That is acceptable for a first slice but not for production load.

This ADR reopens that deferred non-goal:

- **README.md "Intentional MVP Gaps": "no Slack Web API rate-limit retry/backoff policy."** This was a scoped MVP deferral, reopened here as net-new production hardening. No PRD "Out of Scope" bullet named rate limiting, so this is additive — it does not contradict an Accepted decision, it fills a documented gap. (Per docs/agents/domain.md, a reopened non-goal is surfaced explicitly rather than silently overridden.)

The hard constraint is the ack deadline. **Runtime Dispatch** is synchronous in the MVP and runs under the inbound request's **Dispatch Context** (Slack 3s; Linear ~10s first-thought per the **Agent Session Timing Contract**, ADR 0008; Teams turn). A retry loop that blocks on `Retry-After` inside that context will blow past the deadline, the platform will redeliver, and the runtime will do duplicate work — the failure mode dedupe and the **Thread Lock** exist to prevent. So the design question is not only *where* retry lives but how it stays *bounded* and *deadline-safe* and how it stays *visible*.

Related decisions, not redefined here: deferred dispatch and the **Detached Work Context** are ADR 0002 (async-dispatch); the **Observation Hook** metric/trace surface is ADR 0010 (observability); the Linear timing contract is ADR 0008; Teams connector specifics are ADR 0007; multi-tenant credential lookup is ADR 0006.

## Decision

Outbound rate-limit retry/backoff lives in the **Platform Adapter**. It owns the platform API, its error shapes, and its `Retry-After` semantics, so it owns throttling. The runtime and the calling handler do not own backoff.

1. **Bounded retry at the adapter's outbound call sites.** Each adapter wraps its own `PostMessage`, ephemeral delivery, Linear **Agent Activity** creation, and future Teams proactive posts with bounded retry on platform throttling responses. The core `chat` package gains no retry loop, scheduler, or limiter.

2. **Honor the platform signal first.** The adapter uses the platform's `Retry-After` / reset hint when present; otherwise bounded exponential backoff with jitter. The platform signal overrides computed backoff but is still clamped to the bounds below.

3. **Two hard bounds, plus a deadline.** Retry is bounded by an attempt cap and a cumulative-backoff ceiling (a proposed per-adapter **Retry Policy**), and by the caller's `context.Context` deadline. The single load-bearing invariant: the adapter never sleeps past the caller's context deadline. If the next wait would exceed the deadline (or the ceiling), it does not sleep and returns the throttling error. This keeps synchronous retry inside the ack window.

4. **Per-adapter Retry Policy, conservative by default.** The **Retry Policy** lives in each adapter's own options, not in **Runtime Options** — rate-limit behavior is platform-specific, and **Runtime Options** stays coordination-only (DedupeTTL, ThreadLockTTL, Concurrency, and the ADR 0002 Dispatch Mode). The default attempt cap and cumulative ceiling sit comfortably under the tightest relevant ack deadline, so the synchronous path is safe without tuning. Retry is opt-out (`MaxAttempts: 1` disables it), not opt-in.

5. **Explicit, typed exhaustion.** When bounded retry is exhausted, or a single `Retry-After` exceeds the caller's deadline, the adapter returns a typed `RateLimited` error carrying the adapter name, last `Retry-After`, attempt count, and the raw platform response as a **Platform Escape Hatch**. Callers branch on it to defer, drop, or notify; they do not string-match a generic error.

6. **Long throttles move to detached work, not longer sleeps.** Waiting out a multi-second-to-minutes `Retry-After` is not done by widening the synchronous policy. It uses the **Ack-Then-Work** + **Detached Work Context** seam from ADR 0002: acknowledge within the platform deadline, then run bounded retry under the **Detached Work Context** with the **Lock Lease** held and refreshed via `ExtendLock`. In-line synchronous retry stays short by design.

7. **Visible through Runtime Observation.** Every retry attempt and every exhaustion is a structured-slog **Runtime Observation** record (adapter, attempt, `Retry-After`, outcome), and feeds the proposed **Observation Hook** adapter API / rate-limit counters from ADR 0010. No bespoke per-adapter logging path.

Illustrative shapes (design only):

```go
// Per-adapter; zero value is the conservative default. MaxAttempts: 1 disables.
type RetryPolicy struct {
    MaxAttempts int           // total attempts incl. first
    MaxElapsed  time.Duration // cumulative backoff ceiling
    BaseDelay   time.Duration // initial backoff when no Retry-After
    MaxDelay    time.Duration // per-attempt backoff ceiling
}

// Returned on exhaustion; Raw is a Platform Escape Hatch.
type RateLimited struct {
    Adapter    string
    RetryAfter time.Duration
    Attempts   int
    Raw        any
    Err        error
}
```

Throttling detection is adapter-owned: Slack maps 429 + `Retry-After` and the `ratelimited` API error; Linear maps its GraphQL rate-limit / complexity errors and reset hints; Teams maps connector 429 (spike-gated, ADR 0007). Non-throttling errors (auth, validation, not-found, network) return immediately and are never retried.

## Consequences

- Adapters are hardened by default: a transient Slack 429 or Linear GraphQL throttle no longer drops a reply, with no app code change. Under the default `DispatchSync` the bounded retry can only absorb throttles whose `Retry-After` fits inside the ack window; a longer throttle surfaces as a typed `RateLimited` for the caller to defer or drop. The full benefit — waiting out a multi-second throttle with the **Lock Lease** held — lands under `DispatchDeferred`.
- The ack deadline is protected by construction. Synchronous retry cannot outlive the **Dispatch Context**, so throttling does not cause platform redelivery storms or duplicate handler runs.
- The small **Adapter** interface is unchanged — retry is internal to existing outbound calls. The core stays a **semantic subset**; no global limiter or queue is added.
- Callers gain an explicit `RateLimited` branch point: defer to a **Detached Work Context** (ADR 0002), drop, or notify. The raw response stays reachable via the **Platform Escape Hatch**.
- Operators see throttling in **Runtime Observation** and through ADR 0010 **Observation Hook** counters, without reading platform dashboards.
- Linear's first **Agent Activity Thought** stays inside the ~10s **Agent Session Timing Contract** because the default policy's ceiling is below it (ADR 0008).
- Cost: a per-adapter **Retry Policy** is one more option surface, and each adapter must classify its own throttling responses. This is accepted because throttling semantics are genuinely platform-specific and do not belong in the runtime.
- Apps that truly need to wait out long throttles must restructure to **Ack-Then-Work** rather than getting a free unbounded wait. This is intended: it keeps the synchronous path honest.

## Alternatives Considered

### Put retry/backoff in the runtime (around Runtime Dispatch or PostMessage)

Rejected. The runtime does not know a platform's throttling response shape or `Retry-After` semantics, and centralizing it would force the core to model every platform's limits. It also conflates coordination (**Runtime Options**) with platform transport. The **Platform Adapter** already owns auth, verification, and rendering; throttling belongs with them.

### Leave it to the caller (return the raw 429, let handlers retry)

Rejected as the default. Every app would reimplement per-platform `Retry-After` parsing and backoff, and most would get the ack-deadline bound wrong. The typed `RateLimited` error still lets callers take over deliberately, but bounded retry is on by default so adapters are safe out of the box.

### Unbounded retry until the post succeeds

Rejected outright. This is the exact failure the PRD guards against: an unbounded loop blocks past the ack deadline, the platform redelivers, and dedupe / the **Thread Lock** absorb duplicate work that should never have been generated. Retry must be bounded by both an attempt cap and the caller's `context.Context` deadline.

### A global / cross-adapter token-bucket rate limiter

Rejected for this slice. A shared outbound scheduler is a large concern (fairness, persistence across instances, per-tenant budgets) far beyond reacting to an observed 429. Bounded reactive retry that honors `Retry-After` covers the production need; proactive global pacing can be a separately justified slice if real load demands it.

### A runtime-owned outbound queue for throttled posts

Rejected. Deferral of long-throttle work already has a home: the ADR 0002 **Detached Work Context**. Adding a second runtime-owned queue would duplicate that seam and pull persistence and replay concerns into the core. The runtime defines no outbound queue.

### Put Retry Policy in Runtime Options

Rejected. **Runtime Options** is coordination-only (dedupe, lock timing, **Concurrency Strategy**, and the ADR 0002 Dispatch Mode). Rate-limit behavior is platform-specific and varies per adapter, so it lives in each adapter's own options, kept orthogonal to runtime coordination — the same way the new Dispatch Mode is kept orthogonal to **Concurrency Strategy**.
