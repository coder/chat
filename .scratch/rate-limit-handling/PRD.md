# Platform API Rate-Limit Handling

Status: needs-triage

## Problem Statement

The **Go Chat Runtime** posts replies through a **Platform Adapter**, and every platform throttles its outbound API. Slack Web API returns HTTP 429 with a `Retry-After` header; Linear GraphQL enforces request and complexity rate limits; a future Teams connector throttles through the Bot Framework REST connector. Today none of the adapters retry: the MVP README lists "no Slack Web API rate-limit retry/backoff policy" as an intentional gap (README.md "Intentional MVP Gaps"), so a single 429 surfaces as a failed `PostMessage` and a dropped reply.

That gap is now a production-hardening concern, not a scoping nicety. A bot under load that drops every throttled post is unusable. But the naive fix is worse than the gap: a hidden retry loop inside an adapter that blocks on `Retry-After` will silently blow past the platform ack deadline. Synchronous **Runtime Dispatch** runs under the inbound request's **Dispatch Context** (Slack 3s, Linear ~10s first-thought, Teams turn), so any backoff that outlives that deadline causes a platform retry storm and duplicate work — the exact failure dedupe and the **Thread Lock** exist to prevent.

The question this PRD answers: where does retry/backoff live, how is it bounded, how does it honor `Retry-After`, and how does it stay visible through **Runtime Observation** without becoming an unbounded loop that violates ack deadlines.

## Solution

Put outbound rate-limit retry/backoff in the **Platform Adapter**, because the adapter owns the platform API, its error shapes, and its `Retry-After` semantics. The runtime and the calling handler do not own backoff. Specifically:

- Each **Platform Adapter** wraps its own outbound calls (`PostMessage`, ephemeral delivery, Linear agent activity creation, future Teams proactive posts) with **bounded** retry on platform throttling responses.
- The adapter honors the platform's own retry signal first: Slack `Retry-After` (seconds), Linear's documented limit/reset semantics, Teams connector `Retry-After`. When the platform gives no signal, the adapter uses bounded exponential backoff with jitter.
- Retries are bounded by **both** an attempt cap **and** the caller-supplied `context.Context` deadline. The adapter never sleeps past the context deadline; if the next `Retry-After` would exceed it, the adapter returns the throttling error immediately rather than sleeping into a missed ack.
- A new proposed **Retry Policy** lives in the adapter's options (per-adapter, not in **Runtime Options**), defaulting to a small attempt cap and a max cumulative backoff well under the platform ack deadline. The default is conservative so the synchronous path never silently exceeds its deadline. Bounded retry is opt-out, not opt-in: it stays on by default so adapters are hardened out of the box, and `MaxAttempts: 1` disables it for callers that want raw single-shot behavior.
- Under the default `DispatchSync`, bounded retry can only absorb throttles whose `Retry-After` fits inside the ack window; a longer throttle surfaces as a typed `RateLimited` error for the caller to defer or drop. The full benefit — waiting out a multi-second throttle with the **Lock Lease** held — lands under `DispatchDeferred`.
- Exhaustion is explicit: when bounded retries run out, the adapter returns a typed `RateLimited` error carrying the last `Retry-After`, the attempt count, and the **Platform Escape Hatch** raw response. Callers decide whether to give up, defer, or surface to the user.
- Attempts and exhaustion surface through **Runtime Observation** structured-slog by default, and through the proposed **Observation Hook** metric/trace surface from ADR 0010 (adapter API / rate-limit call counters). This is how operators see throttling without reading platform dashboards.

For work that genuinely needs to wait out a long `Retry-After` (seconds-to-minutes), the answer is **not** a longer in-line sleep. It is the **Ack-Then-Work** + **Detached Work Context** seam from ADR 0002 (`DispatchDeferred`): acknowledge within the platform deadline, then run bounded retry under the **Detached Work Context** that outlives the dead request, with the **Lock Lease** held and refreshed via `ExtendLock`. In-line synchronous retry stays short by design; long waits move to detached work, not a hidden unbounded loop.

This is deliberately a **semantic subset**: no global token-bucket scheduler, no cross-adapter shared limiter, no request queue. Bounded per-call retry that honors `Retry-After` and respects the caller's deadline is the whole scope.

## User Stories

1. As a Slack bot developer, I want `PostMessage` to retry a Slack 429 within bounds, so that a transient throttle does not silently drop a reply.
2. As a Slack bot developer, I want the adapter to honor Slack's `Retry-After` header, so that retries match Slack's own backoff guidance instead of guessing.
3. As a Linear app-actor developer, I want **Agent Activity Response** and **Agent Activity Thought** creation to retry Linear GraphQL rate-limit responses within bounds, so that throttling does not break an agent session.
4. As a runtime operator, I want retries bounded by an attempt cap, so that a persistently throttled platform cannot spin forever.
5. As a runtime operator, I want retries bounded by the caller's `context.Context` deadline, so that synchronous backoff never outlives the platform ack deadline and triggers a platform retry storm.
6. As a runtime operator, I want a conservative default **Retry Policy**, so that the synchronous dispatch path stays inside Slack's 3s / Linear's ~10s window without per-app tuning.
7. As a Go application developer, I want a typed `RateLimited` exhaustion error carrying `Retry-After` and attempt count, so that I can decide to defer, drop, or notify rather than guess from a generic error.
8. As a runtime operator, I want rate-limit attempts and exhaustion in **Runtime Observation** logs, so that throttling is explainable without reading the platform's dashboard.
9. As a runtime operator, I want rate-limit calls exposed through the **Observation Hook** surface, so that I can count and alert on throttling through my existing metrics pipeline (ADR 0010).
10. As a Go application developer with long-throttle work, I want to move bounded retry onto the **Detached Work Context** (ADR 0002), so that waiting out a multi-second `Retry-After` does not block the inbound ack.
11. As an adapter author, I want retry/backoff to live behind my outbound call sites, so that the runtime and handlers never reimplement per-platform throttling logic.
12. As a future Teams adapter author, I want the same bounded-retry-with-`Retry-After` contract, so that the connector's throttling is handled the same way as Slack and Linear.
13. As a maintainer, I want a documented non-goal of a cross-adapter global rate limiter, so that the bounded per-call model is an intentional choice, not an oversight.

## Implementation Decisions

- Retry/backoff lives **inside the Platform Adapter**, at its outbound call sites. The core `chat` package gains no retry loop, no scheduler, and no limiter. This matches the existing rule that the adapter owns platform auth, verification, rendering, and now throttling.
- Each adapter exposes a proposed **Retry Policy** through its own options struct (e.g. `slack.WithRetryPolicy(...)`), **not** through **Runtime Options**. Rate-limit behavior is platform-specific; **Runtime Options** stays coordination-only (DedupeTTL, ThreadLockTTL, Concurrency, and the ADR 0002 Dispatch Mode).
- The **Retry Policy** shape is small and Go-idiomatic:

  ```go
  // RetryPolicy bounds outbound rate-limit retries for one Platform Adapter.
  // Zero value = sensible conservative default. Disable with MaxAttempts: 1.
  type RetryPolicy struct {
      MaxAttempts int           // total attempts incl. the first; <=1 disables retry
      MaxElapsed  time.Duration // cumulative backoff ceiling, independent of ctx
      BaseDelay   time.Duration // initial backoff when no Retry-After is given
      MaxDelay    time.Duration // per-attempt backoff ceiling
      // Retry-After from the platform always overrides computed backoff,
      // but is still clamped to the caller's ctx deadline and MaxElapsed.
  }
  ```

- The caller's `context.Context` is the hard bound. Before each sleep the adapter computes the wait (the larger of platform `Retry-After` and backoff, clamped to `MaxDelay`); if `wait` would pass the context deadline or `MaxElapsed`, it does **not** sleep and returns the throttling error. This is the single invariant that keeps synchronous retry inside the ack deadline.
- Defaults are conservative so the synchronous path is safe without tuning: a small attempt cap and a `MaxElapsed` comfortably under the tightest relevant ack deadline. Under the default `DispatchSync` this means bounded retry only absorbs throttles whose `Retry-After` fits inside the ack window; a longer throttle exhausts to `RateLimited`. An app that wants to wait out longer throttles raises the policy **and** moves the work to the **Detached Work Context** (ADR 0002, `DispatchDeferred`); it does not just widen the synchronous policy.
- Exhaustion returns a typed error in the `chat` package so callers can branch without string-matching:

  ```go
  // RateLimited is returned when bounded retry is exhausted or a single
  // Retry-After exceeds the caller's deadline. It is a Platform Escape Hatch
  // for the raw throttling response.
  type RateLimited struct {
      Adapter    string
      RetryAfter time.Duration // platform signal, 0 if none
      Attempts   int
      Raw        any           // raw platform response (Platform Escape Hatch)
      Err        error
  }
  func (e *RateLimited) Error() string { /* ... */ }
  func (e *RateLimited) Unwrap() error { return e.Err }
  ```

- Which platform responses count as throttling is **adapter-owned**: Slack maps HTTP 429 (+ `Retry-After`) and the `ratelimited` API error; Linear maps its GraphQL rate-limit / complexity errors and reset hints; Teams maps connector 429 (+ `Retry-After`). Non-throttling errors (auth, validation, not-found) are returned immediately and never retried.
- Slack: honor the `Retry-After` header in seconds; retry idempotent `chat.postMessage`-family calls within bounds. Do not retry calls already known to be non-idempotent without a platform idempotency key.
- Linear: honor documented rate-limit reset/`Retry-After` semantics for GraphQL; retry **Agent Activity** creation and thread posts within bounds, respecting the **Agent Session Timing Contract** (~10s first thought) so retry never pushes the first **Agent Activity Thought** past the unresponsive threshold (ADR 0008).
- Teams (spike-gated, ADR 0007): honor connector `Retry-After`; exact retry surface and idempotency are spike-required before implementation. The contract is defined here; the connector specifics are not yet verified.
- Observation: every retry attempt and every exhaustion is a **Runtime Observation** structured-slog record (adapter name, attempt number, `Retry-After`, outcome). The same events feed the proposed **Observation Hook** counters from ADR 0010 (adapter API / rate-limit calls). No new bespoke logging path.
- Retry is opt-out, not opt-in: the conservative default is on so adapters are hardened by default; `MaxAttempts: 1` disables it for callers that want raw single-shot behavior.

## Testing Decisions

- Tests assert external behavior at the HTTP boundary: mock a platform returning 429 + `Retry-After`, then 200, and assert the adapter posts successfully within the attempt cap.
- Backoff timing tests use an injected clock / sleep function so retries are deterministic and fast, not wall-clock dependent.
- Deadline-bound tests: with a short `context.Context` deadline and a long `Retry-After`, assert the adapter returns `RateLimited` **without** sleeping past the deadline. This is the core ack-deadline-safety test.
- Exhaustion tests: a platform that throttles forever returns `RateLimited` after exactly `MaxAttempts`, carrying the last `Retry-After`, attempt count, and raw response.
- Non-throttling errors (auth, validation, 404) are returned on the first attempt with zero retries.
- `Retry-After` precedence tests: when the platform sends `Retry-After`, it overrides computed backoff but is still clamped to the context deadline and `MaxElapsed`.
- Default-policy tests assert the zero-value / default policy keeps `MaxElapsed` under the relevant platform ack deadline for Slack and Linear.
- Observation tests assert a structured **Runtime Observation** record per attempt and on exhaustion, and that the **Observation Hook** counter fires (ADR 0010).
- Linear timing test: assert bounded retry on the first **Agent Activity Thought** does not push it past the ~10s **Agent Session Timing Contract** under the default policy.
- Detached-work test (cross-ref ADR 0002): when retry runs under a **Detached Work Context**, assert it is bounded by that context's deadline, not the dead request context, and that the **Lock Lease** is still owned (refreshed via `ExtendLock`).
- Disable test: `MaxAttempts: 1` produces single-shot behavior identical to today's no-retry adapters.

## Out of Scope

- A cross-adapter or global token-bucket / leaky-bucket rate limiter. Each **Platform Adapter** retries its own bounded calls; the runtime does not schedule or pace outbound traffic globally.
- A runtime-owned outbound request queue. Deferral of long-throttle work uses the ADR 0002 **Detached Work Context**, not a new runtime queue.
- Proactive rate-limit avoidance (pre-emptive throttling, request budgeting, header-driven pacing before a 429). Only reactive bounded retry on observed throttling is in scope.
- Retrying non-throttling failures (auth, validation, not-found, network). Those return immediately.
- Putting **Retry Policy** into **Runtime Options**. Rate-limit behavior is per-adapter platform config, kept orthogonal to coordination timing and the ADR 0002 Dispatch Mode.
- Inbound webhook rate limiting / ingress throttling. This PRD is outbound platform API calls only.
- Changing the **Postable Message** surface, dedupe, **Thread Lock** semantics, or **Concurrency Strategy**. Throttling retry is additive at the adapter call site.
- Resumable streaming or token-stream backpressure (deferred, not foreclosed — ADR 0011). This runtime posts finished messages.

## Further Notes

- Reopened non-goal: README.md "Intentional MVP Gaps" lists "no Slack Web API rate-limit retry/backoff policy." That was a scoped MVP deferral, not a permanent decision; this PRD reopens it as net-new production hardening and replaces it with a bounded, deadline-safe, observable policy. No prior PRD "Out of Scope" bullet named rate limiting, so this is additive rather than contradicting an accepted decision.
- Cross-references: deferred dispatch and the **Detached Work Context** for long-throttle work are ADR 0002 (async-dispatch). The **Observation Hook** metric/trace surface is ADR 0010 (observability). Linear's **Agent Session Timing Contract** is ADR 0008; Teams connector specifics are ADR 0007 (spike-gated). Multi-tenant credential lookup is ADR 0006 and does not change where retry lives.
- The load-bearing patterns are preserved: the small **Adapter** interface is unchanged (retry is internal to outbound calls), **Thread ID** stays opaque, **Routing Hooks** stay single-slot, and rate-limit exhaustion surfaces through the typed `RateLimited` error and **Platform Escape Hatch** rather than widening the core.
- The defining invariant, stated once: in-line synchronous retry never sleeps past the caller's `context.Context` deadline. Everything else (attempt cap, `MaxElapsed`, `Retry-After` precedence) is a tighter bound layered on top.
