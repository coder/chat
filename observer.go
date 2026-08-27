package chat

import "context"

// ObservationName is a closed, stable set of discrete observation points the
// runtime emits. The set mirrors the decision points the runtime already logs
// via structured slog, so a log line and a metric cannot disagree about what
// happened.
type ObservationName string

const (
	// ObsDedupeHit is emitted when a duplicate Event (by Event Identity) is
	// dropped.
	ObsDedupeHit ObservationName = "dedupe_hit"
	// ObsLockConflict is emitted when an Event hits a Lock Conflict.
	ObsLockConflict ObservationName = "lock_conflict"
	// ObsLockReleaseFailed is emitted when releasing a Thread Lock fails or finds
	// the lease already gone.
	ObsLockReleaseFailed ObservationName = "lock_release_failed"
	// ObsIgnoredEvent is emitted when a verified, Accepted Event is ignored
	// (non-message with no payload handler, self, or unrouted). Carries a reason
	// attribute.
	ObsIgnoredEvent ObservationName = "ignored_event"
	// ObsHandlerError is emitted when a routed handler returns an error. Carries a
	// route attribute.
	ObsHandlerError ObservationName = "handler_error"
	// ObsAdapterCall is an adapter-facing observation emitted around a Platform
	// Adapter API call. It is reached through typed Adapter Access and
	// adapter-owned Observer wiring, not the core Adapter interface.
	ObsAdapterCall ObservationName = "adapter_call"
	// ObsRateLimit is an adapter-facing observation emitted when an adapter
	// observes platform rate limiting (ADR 0005). Adapter-owned, like
	// ObsAdapterCall.
	ObsRateLimit ObservationName = "rate_limit"
	// ObsLockPreempted is emitted when a new delivery preempts an in-flight
	// handler via the OnLockConflict steerability hook: the current Lock Lease
	// is force released and the local Detached Work Context is cancelled.
	ObsLockPreempted ObservationName = "lock_preempted"
)

// DispatchOutcome is the closed set of terminal outcomes for one Runtime
// Dispatch, mirroring the terminal branches in dispatch.
type DispatchOutcome string

const (
	OutcomeHandled             DispatchOutcome = "handled"
	OutcomeIgnored             DispatchOutcome = "ignored"
	OutcomeDroppedLockConflict DispatchOutcome = "dropped-lock-conflict"
	OutcomeDuplicate           DispatchOutcome = "duplicate"
	OutcomeError               DispatchOutcome = "error"
	// OutcomePreempted is the terminal outcome of a handler that stopped because
	// a new delivery preempted it via the OnLockConflict steerability hook.
	OutcomePreempted DispatchOutcome = "preempted"
)

// Attribute key constants form the documented, stable, low-cardinality set.
// Thread ID, message text, and raw actor IDs are deliberately never emitted as
// default attributes to keep metric cardinality bounded and avoid leaking
// conversation content.
const (
	AttrAdapter = "adapter"
	AttrRoute   = "route"
	AttrReason  = "reason"
	AttrOutcome = "outcome"
	AttrTenant  = "tenant"
)

// Attr is a single low-cardinality observation attribute. Construct attrs with
// the helper constructors so keys stay within the documented set.
type Attr struct {
	Key   string
	Value string
}

// AdapterAttr tags an observation with the originating adapter name.
func AdapterAttr(adapter string) Attr { return Attr{Key: AttrAdapter, Value: adapter} }

// RouteAttr tags an observation with the routing decision (new-mention,
// subscribed-message, command, interaction).
func RouteAttr(route string) Attr { return Attr{Key: AttrRoute, Value: route} }

// ReasonAttr tags an ignored-event observation with why it was ignored.
func ReasonAttr(reason string) Attr { return Attr{Key: AttrReason, Value: reason} }

// OutcomeAttr tags an observation with a terminal DispatchOutcome.
func OutcomeAttr(outcome DispatchOutcome) Attr {
	return Attr{Key: AttrOutcome, Value: string(outcome)}
}

// TenantAttr tags an observation with the Platform Tenant.
func TenantAttr(tenant string) Attr { return Attr{Key: AttrTenant, Value: tenant} }

// Observer is the optional Observation Hook: a narrow, observe-only seam the
// runtime calls at the decision points it already logs. It is modeled as an
// Optional Capability with a no-op default; the core never imports OpenTelemetry,
// Prometheus, or statsd. An Observer cannot mutate routing, acknowledgement, or
// handler flow, which is what keeps it distinct from Middleware.
//
// All methods must be best-effort and must not block dispatch. The runtime wraps
// every call so a panicking or slow Observer can never fail an Accepted Event or
// alter acknowledgement.
type Observer interface {
	// Event records a discrete, counter-style observation with low-cardinality
	// attributes.
	Event(ctx context.Context, name ObservationName, attrs ...Attr)
	// Dispatch opens a span for one Runtime Dispatch and returns a DispatchSpan
	// whose End records the terminal outcome and latency. Under deferred dispatch
	// the span follows the Detached Work Context so Ack-Then-Work latency is
	// measured to handler completion.
	Dispatch(ctx context.Context, attrs ...Attr) (context.Context, DispatchSpan)
}

// DispatchSpan is the open span for one Runtime Dispatch; End closes it with the
// terminal outcome.
type DispatchSpan interface {
	End(outcome DispatchOutcome, attrs ...Attr)
}

// WithObserver installs an Observer. The default is an internal no-op observer,
// so an unconfigured runtime behaves exactly as today: structured slog only,
// with no third-party metrics dependency.
func WithObserver(observer Observer) Option {
	return func(cfg *config) {
		cfg.observer = observer
	}
}

// noopObserver is the default: it records nothing and returns a no-op span.
type noopObserver struct{}

func (noopObserver) Event(context.Context, ObservationName, ...Attr) {}

func (noopObserver) Dispatch(ctx context.Context, _ ...Attr) (context.Context, DispatchSpan) {
	return ctx, noopSpan{}
}

type noopSpan struct{}

func (noopSpan) End(DispatchOutcome, ...Attr) {}

// safeEvent invokes Observer.Event best-effort, recovering from a panic so
// observation can never fail an Accepted Event or alter acknowledgement.
func (c *Chat) safeEvent(ctx context.Context, name ObservationName, attrs ...Attr) {
	defer c.recoverObserver("Event")
	c.observer.Event(ctx, name, attrs...)
}

// safeDispatch opens the dispatch span best-effort, falling back to the original
// context and a no-op span on panic.
func (c *Chat) safeDispatch(ctx context.Context, attrs ...Attr) (outCtx context.Context, span DispatchSpan) {
	outCtx, span = ctx, noopSpan{}
	defer c.recoverObserver("Dispatch")
	outCtx, span = c.observer.Dispatch(ctx, attrs...)
	if span == nil {
		span = noopSpan{}
	}
	return outCtx, span
}

// safeEnd closes the dispatch span best-effort.
func (c *Chat) safeEnd(span DispatchSpan, outcome DispatchOutcome, attrs ...Attr) {
	if span == nil {
		return
	}
	defer c.recoverObserver("DispatchSpan.End")
	span.End(outcome, attrs...)
}

func (c *Chat) recoverObserver(method string) {
	if r := recover(); r != nil {
		c.logger.Warn("chat observer panicked", "method", method, "recovered", r)
	}
}
