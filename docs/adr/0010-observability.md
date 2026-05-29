# ADR 0010: Runtime Observability (Metrics and Tracing)

## Status

Proposed

## Context

The Go Chat Runtime ships **Runtime Observation** as structured `slog` only. The MVP made this explicit: `CONTEXT.md` records "The MVP includes **Runtime Observation** through structured logging, not middleware or a metrics framework," and the `Runtime Observation` glossary entry lists `metrics framework` under _Avoid_. The runtime already logs the decision points an operator cares about: dedupe hits, **Lock Conflict** drops, ignored events, routing, handler failures, and lock-release failures (see `dispatch`, `markAcceptedEvent`, and the lock-release defer in `runtime.go`).

Logs alone do not answer the operational questions for a horizontally scaled Slack or Linear bot: **Runtime Dispatch** latency percentiles, **Lock Conflict** rate, dedupe hit rate, handler error rate per route, and outbound **Platform Adapter** API/rate-limit pressure. Those are counters, histograms, and spans. Operators must currently reconstruct them by scraping logs, which is lossy (no dispatch span boundary) and fragile.

The constraint is to add a metric/trace seam without making OpenTelemetry a hard dependency of `github.com/coder/chat`, without turning the runtime into a metrics framework, and without disturbing the four load-bearing patterns: single-slot **Routing Hooks**, opaque adapter-produced **Thread ID**, the small **Adapter** interface, and **Platform Escape Hatch** / **Optional Capability** over core widening.

**Reopened non-goal.** This ADR reopens exactly one documented MVP non-goal: that **Runtime Observation** is "structured logging only (no metrics framework or middleware)" (`CONTEXT.md` relationship line; `Runtime Observation` _Avoid_: `metrics framework`). It is reopened because operators of a production-shaped, multi-instance bot need counters and spans that logs cannot provide. It is reopened narrowly: this adds an optional emission seam, not a framework. The "no metrics framework" and "no **Middleware**" intents are preserved (no bundled exporters in core, no scrape endpoint, no dispatch mutation); only the "no seam at all" stance changes.

Related decisions, not redefined here: deferred dispatch and the **Detached Work Context** are ADR 0002 (async-dispatch); outbound rate-limit retry/backoff and its attempt/exhaustion signals are ADR 0005 (rate-limit-handling), which already names this **Observation Hook** as the surface those signals feed; **Middleware** (dispatch mutation) remains deferred.

## Decision

Keep structured-`slog` **Runtime Observation** as the default and unchanged. Add a proposed narrow optional **Observation Hook**: a small Go interface the runtime calls at the decision points it already logs, modeled as an **Optional Capability** with a no-op default.

The runtime will:

- keep `github.com/coder/chat` free of any OpenTelemetry, Prometheus, or statsd import; the core defines the interface, bindings live in separate optional modules;
- add a `WithObserver(Observer)` construction option beside the existing `WithLogger`, defaulting to an internal no-op observer so unconfigured runtimes behave exactly as today;
- define a small two-part `Observer` interface: a counter-style `Event` method for discrete points and a `Dispatch` method that opens a span for one **Runtime Dispatch** and returns a `DispatchSpan` whose `End` records the terminal outcome and latency;
- define a closed `ObservationName` set for the existing decision points (`ObsDedupeHit`, `ObsLockConflict`, `ObsLockReleaseFailed`, `ObsIgnoredEvent`, `ObsHandlerError`) plus adapter-facing `ObsAdapterCall` and `ObsRateLimit`;
- define a closed `DispatchOutcome` enum (`handled`, `ignored`, `dropped-lock-conflict`, `duplicate`, `error`) mirroring the terminal branches in `dispatch`;
- define a small `Attr` type with documented, stable, low-cardinality keys (`adapter`, `route`, `reason`, `outcome`, `tenant`); **Thread ID**, message text, and raw actor IDs are never emitted as default attributes;
- instrument exactly the existing decision points: open the dispatch span at entry, emit point events on the dedupe/lock-conflict/ignored/handler-error/lock-release branches, and close the span with the terminal outcome;
- wrap every `Observer` call so a panic or slow exporter is best-effort and can never fail an **Accepted Event** or alter acknowledgement;
- surface **Platform Adapter** API calls and ADR 0005 rate-limit attempts/exhaustion through the same hook, reached via typed **Adapter Access** and adapter-owned `Observer` wiring, NOT by widening the core `Adapter` interface;
- under deferred dispatch (ADR 0002), follow the **Detached Work Context** so **Ack-Then-Work** latency is measured to handler completion;
- ship a small separate optional OTel binding module (e.g. `observability/otel`) as the reference mapping to an OpenTelemetry meter and tracer.

Illustrative shape (design only, not implementation):

```go
type Observer interface {
    Event(ctx context.Context, name ObservationName, attrs ...Attr)
    Dispatch(ctx context.Context, attrs ...Attr) (context.Context, DispatchSpan)
}

type DispatchSpan interface {
    End(outcome DispatchOutcome, attrs ...Attr)
}
```

The **Observation Hook** is observe-only: it receives signals and cannot mutate routing, acknowledgement, or handler flow. That boundary is what keeps it distinct from **Middleware**.

## Consequences

Operators get **Runtime Dispatch** latency spans and counters for dedupe, **Lock Conflict**, ignored events by reason, handler errors by route, and adapter API/rate-limit pressure, behind any backend they choose. The structured-`slog` default is unchanged, so existing deployments are unaffected until they opt in.

The core module gains no new dependency. OpenTelemetry, Prometheus, and statsd stay out of the import graph of `github.com/coder/chat`; the only canonical OTel mapping is a separate optional module. This keeps the library cheap to adopt and avoids forcing a vendor on consumers.

The seam aligns logs and metrics: both fire at the same decision points, so a log line and a metric cannot disagree about what happened. ADR 0005 rate-limit observation has a real home, and ADR 0002 deferred dispatch can be measured end-to-end through the **Detached Work Context**.

The cost: the runtime now carries a small stable observation vocabulary (`ObservationName`, `DispatchOutcome`, `Attr`) it must keep backward-compatible, and every new decision point must remember to emit. Attribute hygiene (keeping **Thread ID** and message text out of labels) is a standing discipline, enforced by test. The seam is deliberately not a framework, so applications still own aggregation, export, dashboards, and alerting.

Deliberate Vercel Chat SDK divergence: the upstream app/template bundles observability as part of the product stack; here it is an optional emission seam on a library, never a bundled exporter or pipeline.

`**Observation Hook**` is a proposed term; add it to `CONTEXT.md` vocabulary only if this ADR is accepted.

## Alternatives Considered

### Depend on OpenTelemetry directly in the core

Wire the OTel metric and trace SDK straight into `github.com/coder/chat`. Rejected: it forces OTel (and its transitive dependencies and versioning churn) on every consumer, including those who want only `slog` or a different backend. It contradicts the Go-idiomatic small-interface stance and the "no metrics framework" intent. The optional binding module gives the same OTel mapping without the hard dependency.

### Keep structured `slog` only and let operators parse logs

Do nothing; tell operators to derive metrics from log scraping. Rejected: it loses the **Runtime Dispatch** span boundary entirely, makes latency percentiles and rates fragile to compute, and leaves ADR 0005 rate-limit signals with no counter home. The reopened non-goal exists precisely because this is insufficient for a production-shaped bot.

### Emit metrics through middleware

Let applications wrap dispatch with **Middleware** that records timings and counts. Rejected: **Middleware** is dispatch mutation and is deferred from the MVP by `CONTEXT.md`; observability must not be able to alter routing or acknowledgement. An observe-only hook is strictly narrower and safer, and does not pull the deferred **Middleware** decision forward.

### Add observation methods to the core Adapter interface

Put `ObserveAPICall` / rate-limit reporting on the `Adapter` interface so every adapter reports uniformly. Rejected: it widens the small **Adapter** interface that every adapter must implement, for a concern only some adapters and some operators care about. Routing adapter observation through typed **Adapter Access** and adapter-owned `Observer` wiring keeps the core interface minimal, consistent with the **Optional Capability** pattern.

### String/registry-based metrics with free-form labels

Expose a generic `Counter(name string, labels map[string]string)` style API. Rejected: free-form names and labels invite cardinality blowups (especially **Thread ID** leaking into labels) and drift from the structured-`slog` decision points. A closed `ObservationName` / `DispatchOutcome` set with a documented low-cardinality `Attr` key list is more idiomatic, safer, and keeps logs and metrics in lockstep.

### Bundle a Prometheus scrape endpoint in core

Ship a ready `/metrics` `http.Handler` from the runtime. Rejected: the runtime does not own HTTP server, router, or transport concerns (`CONTEXT.md`: it exposes **Webhook Handlers** only). Bundling an exporter would re-create the metrics-framework the MVP deliberately excluded. Export stays an application/binding concern.
