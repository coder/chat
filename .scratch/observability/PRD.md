# Runtime Observability (Metrics and Tracing)

Status: needs-triage

## Problem Statement

The MVP ships **Runtime Observation** as structured `slog` only: ignored events, duplicates, **Lock Conflict** drops, routing decisions, and handler failures are logged, nothing more. `CONTEXT.md` states it plainly: "The MVP includes **Runtime Observation** through structured logging, not middleware or a metrics framework," and the `Runtime Observation` term itself lists `metrics framework` under _Avoid_.

That was the right MVP call, but logs alone cannot answer operational questions an operator running a horizontally scaled Slack or Linear bot will ask: what is the p95 **Runtime Dispatch** latency, how often are we dropping on **Lock Conflict**, what is the dedupe hit rate, how many handler errors per route, and how many adapter API calls and rate-limit backoffs are we burning (ADR 0005). Those are counters, histograms, and spans, not log lines. Operators today must scrape logs into a metrics pipeline by hand, which is fragile and loses the dispatch span boundary entirely.

The problem is to add a metrics/tracing seam without (a) making OpenTelemetry a hard dependency of `github.com/coder/chat`, (b) turning the runtime into a metrics framework, or (c) breaking the load-bearing patterns (single-slot **Routing Hooks**, opaque **Thread ID**, the small **Adapter** interface, **Platform Escape Hatch** / **Optional Capability** over core widening). The reopened non-goal is exactly the MVP's "no metrics framework" line; see Out of Scope for the justification.

## Solution

Keep structured-`slog` **Runtime Observation** as the default and unchanged. Add a proposed narrow optional **Observation Hook**: a small Go interface the runtime calls at the same decision points it already logs, so an application can wire counters, histograms, and spans (OpenTelemetry, Prometheus, statsd, or a test double) without the runtime importing any of them.

The seam is deliberately shaped like the existing **Optional Capability** pattern: a narrow interface, capability-detected, with a no-op default. The runtime never depends on OTel; an OTel binding is a separate, optional module the application opts into.

Concretely the proposed surface covers:

- a `WithObserver` **Runtime Options**-adjacent construction option, defaulting to a no-op observer so existing code is unaffected;
- a small `Observer` interface with two halves: **point events** (counter-style: dedupe hit, **Lock Conflict** drop, ignored event by reason, handler error by route) and a **span** for the **Runtime Dispatch** lifetime (latency, terminal outcome);
- an adapter-facing observation path so **Platform Adapter** API calls and ADR 0005 rate-limit attempts/exhaustion surface through the same hook, reached through typed **Adapter Access**, not bolted onto the core `Adapter` interface;
- stable low-cardinality attribute keys (adapter name, route, outcome, **Platform Tenant**) defined once so backends get consistent dimensions and **Thread ID** / actor IDs never leak into metric labels.

This is a seam, not a framework. The runtime emits; the application aggregates and exports.

## User Stories

1. As a runtime operator, I want **Runtime Dispatch** latency recorded as a span, so that I can chart p50/p95/p99 dispatch time without parsing logs.
2. As a runtime operator, I want a counter for dedupe hits, so that I can see how much platform retry traffic the runtime absorbs.
3. As a runtime operator, I want a counter for **Lock Conflict** drops keyed by adapter, so that I can detect conversations that are too hot for the drop **Concurrency Strategy**.
4. As a runtime operator, I want handler errors counted by route (`new-mention`, `subscribed-message`), so that I can alert on handler failure rate per routing case.
5. As a runtime operator, I want **Ignored Event** counts broken down by reason (non-message, self-message, unrouted), so that I can tell expected noise from a routing bug.
6. As a runtime operator, I want adapter API calls and ADR 0005 rate-limit attempts/exhaustion surfaced through the same **Observation Hook**, so that outbound platform pressure is visible alongside dispatch metrics.
7. As a Go application developer, I want metrics and tracing to be opt-in, so that importing `github.com/coder/chat` does not pull OpenTelemetry into my build.
8. As a Go application developer, I want the default to stay structured `slog`, so that the MVP behavior I already rely on does not change when I upgrade.
9. As a Go application developer, I want a no-op default **Observer**, so that I can leave observation unconfigured and pay nothing.
10. As a Go application developer, I want to plug OpenTelemetry, Prometheus, or a statsd client behind the **Observer** interface, so that the runtime does not pick my metrics vendor.
11. As an SRE, I want stable, low-cardinality attribute keys (adapter, route, outcome, **Platform Tenant**), so that my metrics backend does not explode on per-**Thread ID** cardinality.
12. As a security-conscious operator, I want **Thread ID**, message text, and raw actor IDs kept out of metric labels and span names by default, so that observation does not become a data-leak surface.
13. As an adapter author, I want to report platform API timing and rate-limit events through a typed **Adapter Access** path, so that I do not have to widen the small core `Adapter` interface.
14. As a test author, I want to inject a recording **Observer** test double, so that I can assert the runtime emits the expected events without a real metrics backend.
15. As a runtime operator, I want each **Observer** call to be best-effort and panic-safe, so that a broken metrics exporter never fails an **Accepted Event** or crashes dispatch.
16. As a runtime operator, I want the dispatch span and the structured-`slog` records to share the same decision points, so that a log line and a metric never disagree about what happened.
17. As a future maintainer, I want the **Observation Hook** kept distinct from **Middleware**, so that observation cannot mutate or short-circuit **Runtime Dispatch**.
18. As a Go application developer running deferred dispatch (ADR 0002), I want the dispatch span to optionally span the **Detached Work Context**, so that **Ack-Then-Work** latency is measurable end-to-end rather than truncated at ack.
19. As a future maintainer, I want a tiny optional OTel binding module shipped separately, so that the mapping to OpenTelemetry exists without becoming a core dependency.

## Implementation Decisions

- Keep `github.com/coder/chat` free of any OpenTelemetry, Prometheus, or statsd import. The core defines the interface; bindings live elsewhere.
- Keep structured `slog` **Runtime Observation** as the default and unchanged. The **Observation Hook** is additive; it does not replace, gate, or reformat the existing log lines.
- Add a construction option (e.g. `WithObserver(Observer)`) alongside the existing `WithLogger`. Default is an internal no-op observer, so unconfigured runtimes behave exactly as today.
- Model the hook as a small Go interface, detected and stored at **Runtime Construction**, consistent with the **Optional Capability** pattern. No string registry, no flags, no plugin framework.
- Split the interface into a counter-style point-event method and a span method so backends can map cleanly to OTel metrics + traces, Prometheus counters/histograms, or statsd:

  ```go
  // Observer receives runtime observation signals. All methods are
  // best-effort and must not block dispatch or mutate the event.
  type Observer interface {
      // Event records a discrete observation (counter-style) with
      // low-cardinality attributes.
      Event(ctx context.Context, name ObservationName, attrs ...Attr)

      // Dispatch starts a span for one Runtime Dispatch and returns a
      // DispatchSpan whose End records the terminal outcome and latency.
      Dispatch(ctx context.Context, attrs ...Attr) (context.Context, DispatchSpan)
  }

  type DispatchSpan interface {
      End(outcome DispatchOutcome, attrs ...Attr)
  }
  ```

- Define a closed set of `ObservationName` constants for the points the runtime already logs, so backends get stable metric names: `ObsDedupeHit`, `ObsLockConflict`, `ObsLockReleaseFailed`, `ObsIgnoredEvent`, `ObsHandlerError`, plus adapter-facing `ObsAdapterCall` and `ObsRateLimit` (ADR 0005).
- Define a closed `DispatchOutcome` enum (`handled`, `ignored`, `dropped-lock-conflict`, `duplicate`, `error`) mirroring the terminal branches in `dispatch`.
- Define a small `Attr` (key, value) type with documented, stable, low-cardinality keys: `adapter`, `route`, `reason`, `outcome`, `tenant`. **Thread ID**, message text, and raw actor IDs are never emitted as attributes by default; an application that wants high-cardinality dimensions opts in through its own `Observer`, owning the cardinality risk.
- Instrument exactly the existing decision points in `dispatch`, `markAcceptedEvent`, and the lock-release defer: open the dispatch span at entry, emit `ObsDedupeHit` where duplicates are dropped, `ObsLockConflict` on the not-acquired branch, `ObsIgnoredEvent` (with a `reason` attr) on the non-message / self-message / unrouted branches, `ObsHandlerError` on handler failure, `ObsLockReleaseFailed` on release failure, and close the span with the terminal `DispatchOutcome`.
- Every `Observer` call is wrapped so a panic or slow exporter cannot fail an **Accepted Event**; observation is strictly best-effort and side-effect-free with respect to acknowledgement semantics.
- Adapter API call and rate-limit observation is reached through typed **Adapter Access**, not added to the core `Adapter` interface. An adapter that wants to report timing/backoff accepts an `Observer` (or a narrow adapter-internal observation interface) at its own construction, keeping the small `Adapter` interface intact.
- Under deferred dispatch (ADR 0002), the dispatch span follows the **Detached Work Context**, not the dead request context, so **Ack-Then-Work** latency is measured to handler completion. This is a hook-placement note only; ADR 0002 owns the dispatch-mode decision.
- The **Observation Hook** is observe-only. It receives signals; it cannot alter routing, acknowledgement, or handler flow. **Middleware** (dispatch mutation) stays deferred and out of scope; this is the explicit boundary `CONTEXT.md` draws between **Runtime Observation** and **Middleware**.
- Ship a small, separate, optional OTel binding module (e.g. `observability/otel`) that adapts an OpenTelemetry meter + tracer to the `Observer` interface, so the canonical mapping exists without the core importing OTel. Prometheus/statsd bindings are left to applications or later modules.
- Update GoDoc and README to document the seam, the no-op default, the OTel-is-optional stance, and the intentional difference from a metrics framework.

## Testing Decisions

- Tests assert external behavior and public contracts, not private wiring.
- Add a recording `Observer` test double in test code that captures `Event` calls and span open/close, used to assert emission at each decision point.
- Add dispatch-instrumentation tests proving: a handled message opens and closes one dispatch span with outcome `handled`; a duplicate emits `ObsDedupeHit` and closes with `duplicate`; a **Lock Conflict** emits `ObsLockConflict` and closes with `dropped-lock-conflict`; non-message, self-message, and unrouted events emit `ObsIgnoredEvent` with the correct `reason` and close with `ignored`; a handler error emits `ObsHandlerError` and closes with `error`.
- Add a default-observer test proving an unconfigured runtime uses the no-op observer and behaves identically to current MVP dispatch (no behavior change, no allocation surprises in the hot path).
- Add a best-effort/panic-safety test proving an `Observer` that panics or blocks does not change acknowledgement semantics, does not fail an **Accepted Event**, and does not deadlock dispatch.
- Add an attribute-hygiene test proving **Thread ID**, message text, and raw actor IDs are not present in emitted attributes by default, and that attribute keys are drawn from the documented stable set.
- Add a parity test proving the **Observation Hook** decision points line up with the existing `slog` **Runtime Observation** lines, so logs and metrics cannot diverge.
- Add adapter-facing tests (Slack and/or Linear, mocking at the HTTP boundary) proving `ObsAdapterCall` and `ObsRateLimit` are emitted around platform API calls and ADR 0005 backoff, and that the core `Adapter` interface is unchanged.
- Add an OTel-binding test in the separate module proving the binding maps `Event`/`Dispatch` to OTel instruments, kept out of the core module's dependency graph.
- Run the existing root, adapter, and example test commands. The core module's dependency set must not gain OTel; assert this (e.g. via a module-graph check) so the no-hard-dependency invariant is enforced.

## Out of Scope

- **Reopened non-goal:** the MVP's "**Runtime Observation** through structured logging, not middleware or a metrics framework" (`CONTEXT.md`; `Runtime Observation` _Avoid_: `metrics framework`). This PRD adds a metric/trace seam, NOT a metrics framework: it is a narrow optional interface with a no-op default, no bundled exporters in core, no scrape endpoint, no vendor lock. The "no metrics framework" intent holds; only the "no seam at all" stance is reopened, because operators of a horizontally scaled bot need counters/spans that logs cannot provide.
- A hard OpenTelemetry dependency in `github.com/coder/chat`. OTel is reachable only through an optional separate binding module.
- A built-in metrics HTTP endpoint, Prometheus scrape handler, exporter configuration, or push pipeline. The runtime emits; the application exports.
- **Middleware** / dispatch mutation. The **Observation Hook** is observe-only and cannot alter routing or acknowledgement; **Middleware** stays deferred per `CONTEXT.md`.
- High-cardinality labels in the default attribute set (**Thread ID**, message text, raw actor IDs). Applications may add them through their own `Observer` at their own cardinality risk.
- Logging changes. Structured-`slog` **Runtime Observation** is unchanged; this is purely additive.
- Owning backoff or retry policy. Outbound rate-limit retry/backoff lives in the **Platform Adapter** per ADR 0005; this PRD only adds the hook that surfaces its attempts/exhaustion.
- The dispatch-mode decision itself. Deferred dispatch and the **Detached Work Context** are ADR 0002; this PRD only notes how the span follows that context.
- A cross-platform metrics schema standard. Attribute keys are stabilized for this runtime's signals, not proposed as an industry contract.

## Further Notes

- The four load-bearing patterns are preserved: single-slot **Routing Hooks** (untouched), opaque adapter-produced **Thread ID** (kept out of metric labels), the small **Adapter** interface (unchanged; adapter observation rides typed **Adapter Access**), and **Platform Escape Hatch** / **Optional Capability** over core widening (the **Observer** is exactly an optional capability with a no-op default).
- This is an intentional Vercel Chat SDK divergence in shape: the upstream app/template bundles observability as part of the product; here observability is an optional seam on a library, not a bundled stack.
- Cross-references: deferred dispatch / **Detached Work Context** is ADR 0002; rate-limit attempts/exhaustion that feed `ObsRateLimit` are ADR 0005; the decision record for this seam is ADR 0010.
- The deepest seam here is the `Observer` interface plus the closed `ObservationName` / `DispatchOutcome` / `Attr` vocabulary: small surface, defined once, mappable to any backend. The optional OTel binding is the reference mapping, deliberately kept outside the core module.
- `**Observation Hook**` is a proposed term; add it to `CONTEXT.md` vocabulary only if ADR 0010 is accepted.
