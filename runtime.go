package chat

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

type ConcurrencyStrategy int

const (
	// ConcurrencyDrop acknowledges and drops a Lock Conflict. This is the default.
	ConcurrencyDrop ConcurrencyStrategy = iota
	// ConcurrencyQueue waits for the in-flight handler, then dispatches the most
	// recent superseded event for the scope.
	ConcurrencyQueue
	// ConcurrencyDebounce coalesces rapid follow-ups: each new routed event for a
	// scope supersedes the previous waiter and only the final event in a
	// DebounceInterval quiet period dispatches. Superseded events are surfaced as
	// skipped through Runtime Observation, never silently. Requires deferred
	// dispatch (a synchronous webhook cannot park an event past the platform's
	// acknowledgement deadline).
	//
	// "Final" follows the dispatch admission order on this instance: a delivery
	// delayed in its prelude never displaces a waiter admitted after it. The
	// quiet period is measured over registered waiters: a delivery stalled in
	// its prelude (validation, dedupe, routing) for longer than the interval
	// does not reset the running timer and dispatches separately afterwards —
	// still serialized by the Thread Lock and never lost, but not coalesced.
	//
	// Like queue supersession, coalescing is per runtime instance: events for
	// one scope delivered to different instances sharing a State are not
	// superseded across instances (each instance dispatches its own final
	// event, serialized by the Thread Lock). Per-instance supersession is the
	// decided v0.x contract (ADR 0015); cross-instance coalescing is rejected
	// for now behind that ADR's reopening bar.
	ConcurrencyDebounce
	// ConcurrencyConcurrent is the explicit opt-out of per-scope serialization:
	// every routed event dispatches immediately in its own execution, bounded by
	// MaxConcurrent. No Thread Lock is taken, so the caller accepts interleaved
	// replies and races on Thread Application State.
	ConcurrencyConcurrent
)

// The burst strategy and force/steerability names from ADR 0012 remain
// reserved. Per ADR 0015, burst revives as its own PR under the Admission
// Bound's invariants, while force/steerability is rejected pending that ADR's
// formal-design bar.

// LockScope selects what key the Thread Lock guards. The opaque Thread ID is
// unchanged; the scope only chooses the serialization key.
type LockScope int

const (
	// LockScopeThread serializes handlers per Thread. This is the default.
	LockScopeThread LockScope = iota
	// LockScopeChannel widens serialization from a single Thread to its whole
	// channel, for platforms whose model requires channel-wide ordering. A
	// Thread whose adapter reports no channel falls back to per-Thread locking
	// rather than sharing one adapter-wide key.
	LockScopeChannel
)

// DispatchMode selects whether the routed handler runs before or after the
// adapter acknowledges the platform.
type DispatchMode int

const (
	// DispatchSync runs the handler under the request context and acknowledges
	// after it returns. This is the default.
	DispatchSync DispatchMode = iota
	// DispatchDeferred acknowledges after the prelude and runs the handler on the
	// detached work context (ack-then-work).
	DispatchDeferred
)

type RuntimeOptions struct {
	DedupeTTL     time.Duration
	ThreadLockTTL time.Duration
	Concurrency   ConcurrencyStrategy
	Dispatch      DispatchMode
	DetachTimeout time.Duration
	// LockScope selects the Thread Lock key: per Thread (default) or per
	// channel.
	LockScope LockScope
	// DebounceInterval is the ConcurrencyDebounce quiet period. It must be
	// positive under that strategy and is ignored otherwise.
	DebounceInterval time.Duration
	// MaxConcurrent bounds simultaneous handler executions under
	// ConcurrencyConcurrent. It must be positive under that strategy and is
	// ignored otherwise.
	MaxConcurrent int
	// MaxDetached is the deferred-dispatch Admission Bound (ADR 0015): a
	// per-instance cap on admitted-but-incomplete deferred deliveries.
	// Everything a delivery retains under DispatchDeferred counts against it —
	// running detached tails, parked queue/debounce waiters, and concurrent
	// slot-waiters — and capacity frees only when that retention ends. A
	// delivery arriving at the cap is rejected with ErrAdmissionRejected
	// before acknowledgement and before dedupe marking, so a platform retry is
	// never deduped away. It must be positive under DispatchDeferred and is
	// ignored under DispatchSync; DefaultRuntimeOptions sets 1024.
	//
	// Sizing: the bound is a count, not bytes — the runtime cannot measure
	// retained platform payloads or handler closures. Budget roughly
	// MaxDetached x (platform payload ceiling + one goroutine stack + whatever
	// the handler closure pins) against the instance's memory; sustained
	// rejection is the platform-visible backpressure signal, so size the cap
	// to be reached only under genuine overload and keep front-door rate
	// limiting for fleet-level control.
	MaxDetached int
	// MaxDetachedPerTenant additionally caps any single installation's share
	// of MaxDetached, keyed on the delivery's (adapter, tenant) installation
	// identity (ADR 0006). It is a ceiling through the same rejection path,
	// not a reservation: a hot tenant is capped, but no capacity is guaranteed
	// to the others. Zero disables the ceiling; it must not be negative under
	// DispatchDeferred and is ignored under DispatchSync.
	MaxDetachedPerTenant int
}

func DefaultRuntimeOptions() RuntimeOptions {
	return RuntimeOptions{
		DedupeTTL:     24 * time.Hour,
		ThreadLockTTL: 2 * time.Minute,
		Concurrency:   ConcurrencyDrop,
		Dispatch:      DispatchSync,
		DetachTimeout: 0,
		LockScope:     LockScopeThread,
		MaxDetached:   1024,
	}
}

type Option func(*config)

type config struct {
	state    State
	adapters []Adapter
	logger   *slog.Logger
	observer Observer
	options  RuntimeOptions
}

func WithState(state State) Option {
	return func(cfg *config) {
		cfg.state = state
	}
}

func WithAdapter(adapter Adapter) Option {
	return func(cfg *config) {
		cfg.adapters = append(cfg.adapters, adapter)
	}
}

func WithLogger(logger *slog.Logger) Option {
	return func(cfg *config) {
		cfg.logger = logger
	}
}

func WithRuntimeOptions(options RuntimeOptions) Option {
	return func(cfg *config) {
		cfg.options = options
	}
}

type Chat struct {
	state    State
	adapters map[string]Adapter
	logger   *slog.Logger
	observer Observer
	options  RuntimeOptions

	handlersMu        sync.RWMutex
	newMention        MessageHandler
	subscribedMessage MessageHandler
	command           CommandHandler
	interaction       InteractionHandler
	acceptancesMu     sync.Mutex
	eventAcceptances  map[string]*eventAcceptance

	// baseCtx is the long-lived base for the detached work context: not a request
	// context, not context.Background(). Cancelled by Shutdown.
	baseCtx    context.Context
	baseCancel context.CancelFunc
	// inflight tracks detached tails so Shutdown drains them before state shutdown.
	inflight sync.WaitGroup

	// queueMu guards pending, the per-scope most-recent pending waiter.
	queueMu sync.Mutex
	pending map[string]*pendingWaiter
	// dispatchSeq orders deliveries by dispatch admission so a delivery delayed
	// in its prelude can never displace a newer pending waiter.
	dispatchSeq atomic.Uint64

	// concurrencySlots bounds simultaneous handler executions under
	// ConcurrencyConcurrent; nil under every other strategy.
	concurrencySlots chan struct{}

	// admission is the deferred-dispatch Admission Bound (ADR 0015); nil under
	// DispatchSync.
	admission *admissionGate

	shutdownMu   sync.Mutex
	shutdown     bool
	shutdownDone chan struct{}
}

type eventAcceptance struct {
	done chan struct{}
	err  error
}

func New(ctx context.Context, opts ...Option) (*Chat, error) {
	cfg := config{
		logger:   slog.Default(),
		observer: noopObserver{},
		options:  DefaultRuntimeOptions(),
	}
	for _, opt := range opts {
		if opt == nil {
			return nil, errors.New("chat: nil option")
		}
		opt(&cfg)
	}
	if cfg.state == nil {
		return nil, errors.New("chat: runtime state is required")
	}
	if len(cfg.adapters) == 0 {
		return nil, errors.New("chat: at least one adapter is required")
	}
	if cfg.logger == nil {
		return nil, errors.New("chat: logger is required")
	}
	if cfg.observer == nil {
		cfg.observer = noopObserver{}
	}
	if err := validateRuntimeOptions(cfg.options); err != nil {
		return nil, err
	}

	baseCtx, baseCancel := context.WithCancel(context.Background())
	chat := &Chat{
		state:            cfg.state,
		adapters:         map[string]Adapter{},
		logger:           cfg.logger,
		observer:         cfg.observer,
		options:          cfg.options,
		eventAcceptances: map[string]*eventAcceptance{},
		baseCtx:          baseCtx,
		baseCancel:       baseCancel,
		pending:          map[string]*pendingWaiter{},
	}
	if cfg.options.Concurrency == ConcurrencyConcurrent {
		chat.concurrencySlots = make(chan struct{}, cfg.options.MaxConcurrent)
	}
	if cfg.options.Dispatch == DispatchDeferred {
		chat.admission = newAdmissionGate(cfg.options.MaxDetached, cfg.options.MaxDetachedPerTenant)
	}
	for _, adapter := range cfg.adapters {
		if adapter == nil {
			baseCancel()
			return nil, errors.New("chat: nil adapter")
		}
		name := adapter.Name()
		if name == "" {
			baseCancel()
			return nil, errors.New("chat: adapter name is required")
		}
		if _, exists := chat.adapters[name]; exists {
			baseCancel()
			return nil, fmt.Errorf("chat: adapter %q registered more than once", name)
		}
		chat.adapters[name] = adapter
		if err := adapter.Init(ctx); err != nil {
			baseCancel()
			return nil, errors.Join(
				fmt.Errorf("chat: initialize adapter %q: %w", name, err),
				shutdownAdapters(ctx, chat.adapters),
			)
		}
	}
	return chat, nil
}

func validateRuntimeOptions(options RuntimeOptions) error {
	if options.DedupeTTL <= 0 {
		return errors.New("chat: dedupe ttl must be positive")
	}
	if options.ThreadLockTTL <= 0 {
		return errors.New("chat: thread lock ttl must be positive")
	}
	switch options.Concurrency {
	case ConcurrencyDrop, ConcurrencyQueue:
	case ConcurrencyDebounce:
		if options.DebounceInterval <= 0 {
			return errors.New("chat: debounce interval must be positive under the debounce strategy")
		}
		if options.Dispatch != DispatchDeferred {
			return errors.New("chat: debounce strategy requires deferred dispatch")
		}
		// The quiet-period wait runs inside the DetachTimeout-bounded Detached
		// Work Context: a timeout at or below the interval would abandon every
		// accepted event before its quiet period closes.
		if options.DetachTimeout <= options.DebounceInterval {
			return errors.New("chat: detach timeout must exceed the debounce interval under the debounce strategy")
		}
	case ConcurrencyConcurrent:
		if options.MaxConcurrent <= 0 {
			return errors.New("chat: max concurrent must be positive under the concurrent strategy")
		}
	default:
		return errors.New("chat: unsupported concurrency strategy")
	}
	switch options.LockScope {
	case LockScopeThread, LockScopeChannel:
	default:
		return errors.New("chat: unsupported lock scope")
	}
	switch options.Dispatch {
	case DispatchSync:
	case DispatchDeferred:
		if options.DetachTimeout <= 0 {
			return errors.New("chat: detach timeout must be positive under deferred dispatch")
		}
		if options.MaxDetached <= 0 {
			return errors.New("chat: max detached must be positive under deferred dispatch")
		}
		if options.MaxDetachedPerTenant < 0 {
			return errors.New("chat: max detached per tenant must not be negative under deferred dispatch")
		}
	default:
		return errors.New("chat: unsupported dispatch mode")
	}
	return nil
}

// OnNewMention installs or atomically replaces the single new-mention handler.
// This intentionally differs from Vercel Chat SDK's multiple-handler hooks.
func (c *Chat) OnNewMention(handler MessageHandler) {
	assert(c != nil, "OnNewMention called on nil runtime")
	c.handlersMu.Lock()
	defer c.handlersMu.Unlock()
	c.newMention = handler
}

// OnSubscribedMessage installs or atomically replaces the single subscribed-message handler.
// This intentionally differs from Vercel Chat SDK's multiple-handler hooks.
func (c *Chat) OnSubscribedMessage(handler MessageHandler) {
	assert(c != nil, "OnSubscribedMessage called on nil runtime")
	c.handlersMu.Lock()
	defer c.handlersMu.Unlock()
	c.subscribedMessage = handler
}

// OnCommand installs or atomically replaces the single command handler. A Command
// Event routes here and never to the message hooks, even in a Subscribed Thread.
// This intentionally differs from Vercel Chat SDK's multiple-handler hooks.
func (c *Chat) OnCommand(handler CommandHandler) {
	assert(c != nil, "OnCommand called on nil runtime")
	c.handlersMu.Lock()
	defer c.handlersMu.Unlock()
	c.command = handler
}

// OnInteraction installs or atomically replaces the single interaction handler. An
// Interaction Event routes here and never to the message hooks. This intentionally
// differs from Vercel Chat SDK's multiple-handler hooks.
func (c *Chat) OnInteraction(handler InteractionHandler) {
	assert(c != nil, "OnInteraction called on nil runtime")
	c.handlersMu.Lock()
	defer c.handlersMu.Unlock()
	c.interaction = handler
}

func (c *Chat) Webhook(adapterName string) (http.Handler, error) {
	assert(c != nil, "Webhook called on nil runtime")
	adapter, ok := c.adapters[adapterName]
	if !ok {
		return nil, fmt.Errorf("chat: adapter %q is not registered", adapterName)
	}
	return adapter.Webhook(c.dispatch), nil
}

func (c *Chat) Thread(ctx context.Context, id ThreadID) (*Thread, error) {
	assert(c != nil, "Thread called on nil runtime")
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	name, err := adapterNameFromThreadID(id)
	if err != nil {
		return nil, err
	}
	adapter, ok := c.adapters[name]
	if !ok {
		return nil, fmt.Errorf("chat: adapter %q is not registered", name)
	}
	ref, err := adapter.ValidateThreadID(id)
	if err != nil {
		return nil, fmt.Errorf("chat: validate thread id: %w", err)
	}
	return c.newThread(adapter, ref), nil
}

func AdapterAs[T any](c *Chat, adapterName string) (T, bool) {
	var zero T
	if c == nil {
		return zero, false
	}
	adapter, ok := c.adapters[adapterName]
	if !ok {
		return zero, false
	}
	typed, ok := adapter.(T)
	return typed, ok
}

func (c *Chat) Shutdown(ctx context.Context) error {
	assert(c != nil, "Shutdown called on nil runtime")
	c.shutdownMu.Lock()
	if c.shutdown {
		done := c.shutdownDone
		c.shutdownMu.Unlock()
		select {
		case <-done:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	c.shutdown = true
	done := make(chan struct{})
	c.shutdownDone = done
	c.shutdownMu.Unlock()
	defer close(done)

	// Close admission first so a delivery racing Shutdown is rejected with
	// ErrAdmissionRejected (the platform's retry covers it) instead of being
	// admitted into a runtime about to cancel its work. Then cancel detached
	// tails and drain (bounded by ctx) before shutting down adapters and state.
	//
	// The drain waits on admission slots before the tail WaitGroup: a delivery
	// that won the admission race is retained from admit until its tail
	// goroutine returns, so waiting for every slot covers deliveries still in
	// their synchronous prelude — which hold no WaitGroup count yet — and
	// guarantees no tail is spawned (and no WaitGroup Add happens) after the
	// slot drain completes.
	var admissionDrained <-chan struct{}
	if c.admission != nil {
		admissionDrained = c.admission.close()
	}
	c.baseCancel()
	var drainErr error
	drained := make(chan struct{})
	go func() {
		if admissionDrained != nil {
			<-admissionDrained
		}
		c.inflight.Wait()
		close(drained)
	}()
	select {
	case <-drained:
	case <-ctx.Done():
		drainErr = ctx.Err()
	}

	err := errors.Join(drainErr, shutdownAdapters(ctx, c.adapters))
	if stateErr := c.state.Shutdown(ctx); stateErr != nil {
		return errors.Join(err, fmt.Errorf("shutdown state: %w", stateErr))
	}
	return err
}

func shutdownAdapters(ctx context.Context, adapters map[string]Adapter) error {
	var errs []error
	for name, adapter := range adapters {
		if err := adapter.Shutdown(ctx); err != nil {
			errs = append(errs, fmt.Errorf("shutdown adapter %q: %w", name, err))
		}
	}
	return errors.Join(errs...)
}

func (c *Chat) dispatch(ctx context.Context, event *Event) error {
	// The admission sequence is assigned before any prelude work so pending
	// registration can order deliveries by admission: a delivery delayed in
	// validation/dedupe/routing can never displace a newer waiter.
	seq := c.dispatchSeq.Add(1)
	if c.options.Dispatch == DispatchDeferred {
		return c.dispatchDeferred(ctx, event, seq)
	}
	return c.dispatchSync(ctx, event, seq)
}

// dispatchSync runs the prelude and the routed handler inline under the request
// context, releasing the Thread Lock when the handler returns. Under the queue
// strategy a Lock Conflict waits inline (bounded by ctx) for the in-flight
// handler before the routed handler runs. Under the concurrent strategy the
// request waits inline for a MaxConcurrent slot instead of a lock.
func (c *Chat) dispatchSync(ctx context.Context, event *Event, seq uint64) error {
	work, resolved, err := c.prelude(ctx, event, seq)
	if err != nil || resolved {
		return err
	}
	if work.noLock {
		if !c.acquireConcurrencySlot(ctx, event) {
			c.safeEnd(work.span, OutcomeIgnored, RouteAttr(work.route))
			return nil
		}
		defer c.releaseConcurrencySlot()
	} else {
		if work.needsLock {
			// The steerability hook requires deferred dispatch, so a sync wait
			// never carries an ownership reservation.
			lease, outcome := c.queueForLock(ctx, work.scope, event, work.waitLabel)
			if outcome != acquireHeld {
				c.safeEnd(work.span, waitOutcome(outcome), RouteAttr(work.route))
				return nil
			}
			work.lease = lease
		}
		defer c.releaseLock(ctx, work.lease, event.ThreadID)
	}
	if err := work.run(ctx); err != nil {
		c.logger.Error("chat handler failed", "error", err, "adapter", event.Adapter, "event_id", event.ID, "route", work.route)
		c.safeEvent(ctx, ObsHandlerError, AdapterAttr(event.Adapter), RouteAttr(work.route))
		c.safeEnd(work.span, OutcomeError, RouteAttr(work.route))
		return nil
	}
	c.safeEnd(work.span, OutcomeHandled, RouteAttr(work.route))
	return nil
}

// dispatchDeferred runs the prelude under the request context and, on a routed
// event, launches the detached tail and returns so the adapter can acknowledge
// the platform (ack-then-work). The Admission Bound gate runs first, before
// any acknowledgement and before dedupe marking: a delivery rejected at the
// cap fails fast with ErrAdmissionRejected, is never marked in Event Identity
// (so a platform retry is not deduped away), and does no State work at all —
// under saturation even a redelivered duplicate receives the overload
// response, converging to the ordinary duplicate acknowledgement once
// capacity frees (ADR 0015).
func (c *Chat) dispatchDeferred(ctx context.Context, event *Event, seq uint64) error {
	if err := validateEvent(event); err != nil {
		return err
	}
	assert(c.admission != nil, "deferred dispatch requires the admission gate")
	release, admitted := c.admission.admit(event.Adapter, event.Tenant)
	if !admitted {
		spanCtx, span := c.safeDispatch(ctx, AdapterAttr(event.Adapter), TenantAttr(event.Tenant))
		c.logger.Warn("chat deferred dispatch admission rejected", "adapter", event.Adapter, "tenant", event.Tenant, "event_id", event.ID)
		c.safeEvent(spanCtx, ObsAdmissionRejected, AdapterAttr(event.Adapter), TenantAttr(event.Tenant))
		c.safeEnd(span, OutcomeAdmissionRejected)
		return fmt.Errorf("chat: deferred dispatch admission rejected for adapter %q: %w", event.Adapter, ErrAdmissionRejected)
	}
	work, resolved, err := c.prelude(ctx, event, seq)
	if err != nil || resolved {
		// The delivery retains nothing past the prelude: an errored or
		// resolved (duplicate, dropped, ignored, unrouted) prelude frees its
		// admission slot immediately.
		release()
		return err
	}
	work.releaseAdmission = release
	c.startDetachedTail(work)
	return nil
}

// preludeWork carries the routing decision from the prelude to the dispatch tail
// for a routed event.
type preludeWork struct {
	event *Event
	lease LockLease
	// run invokes the routed handler with its typed input (MessageEvent,
	// CommandEvent, or InteractionEvent); keeping it handler-type agnostic lets the
	// dispatch tails stay identical across Event kinds.
	run   func(context.Context) error
	route string
	scope string
	// needsLock is true when the prelude did not acquire the Thread Lock (a
	// queued Lock Conflict, or the debounce strategy); the tail must wait for
	// and acquire it before running the handler.
	needsLock bool
	// waitLabel names the coordination mode ("queue" or "debounce") in wait
	// observations so a log line names the strategy that produced it.
	waitLabel string
	// displaced closes when a newer event supersedes this pending waiter; the
	// debounce quiet-period wait exits promptly on it instead of parking through
	// its full interval.
	displaced <-chan struct{}
	// debounce is true when the tail must hold the event through the
	// DebounceInterval quiet period before waiting for the lock.
	debounce bool
	// noLock is true under the concurrent strategy: no Thread Lock is taken and
	// the run is bounded by a MaxConcurrent slot instead.
	noLock bool
	// releaseAdmission frees the delivery's Admission Bound slot. It is set
	// only under DispatchDeferred and runs when the detached tail goroutine
	// returns — not when the handler returns — so stalled cleanup (lock
	// release, lease refresh drain) still counts as retention.
	releaseAdmission func()
	// span is opened in the prelude and closed by the tail, so deferred dispatch
	// measures Ack-Then-Work latency to handler completion.
	span DispatchSpan
}

// prelude runs the synchronous portion of dispatch before ack: open the dispatch
// span, validate, dedupe, acquire the Thread Lock, validate the thread id, filter
// nil/self events, and route. A resolved event (duplicate, dropped Lock Conflict,
// ignored, unrouted) returns resolved=true with no work and closes the span with
// its terminal outcome. A failed prelude returns the error and, as today, leaves
// the event un-marked so a retry is not deduped away; its span is closed with the
// error outcome.
//
// Routing precedence: command-ness and interaction-ness are Event kinds and take
// precedence over subscription state, so a command/interaction in a Subscribed
// Thread still routes to its own hook, never to the message hooks. Only an Event
// with no Message, Command, or Interaction payload remains an Ignored Event.
//
// Under the queue strategy a Lock Conflict on a routed event does not block: the
// event is registered as pending (no lease) and returned with needsLock=true so
// the tail acquires the lock, keeping ack prompt under DispatchDeferred.
func (c *Chat) prelude(ctx context.Context, event *Event, seq uint64) (preludeWork, bool, error) {
	if err := validateEvent(event); err != nil {
		return preludeWork{}, true, err
	}

	spanCtx, span := c.safeDispatch(ctx, AdapterAttr(event.Adapter), TenantAttr(event.Tenant))
	ctx = spanCtx

	acceptance, primary := c.beginEventAcceptance(event.ID)
	if !primary {
		// The primary owns the terminal observation for the shared Event Identity.
		err := waitEventAcceptance(ctx, acceptance)
		c.safeEnd(span, OutcomeDuplicate)
		return preludeWork{}, true, err
	}
	finish := func(err error) error {
		c.finishEventAcceptance(event.ID, acceptance, err)
		return err
	}
	acceptEvent := func() (bool, error) {
		firstSeen, err := c.markAcceptedEvent(ctx, event)
		if err != nil {
			return false, finish(err)
		}
		c.finishEventAcceptance(event.ID, acceptance, nil)
		if !firstSeen {
			c.safeEvent(ctx, ObsDedupeHit, AdapterAttr(event.Adapter))
		}
		return firstSeen, nil
	}

	adapter, ok := c.adapters[event.Adapter]
	if !ok {
		err := finish(fmt.Errorf("chat: event adapter %q is not registered", event.Adapter))
		c.safeEnd(span, OutcomeError)
		return preludeWork{}, true, err
	}

	// The Thread ID is validated before the Thread Lock so the lock scope key
	// (which may be channel-wide) comes from the adapter-validated ThreadRef; an
	// event with an invalid Thread ID never touches the lock.
	ref, validateErr := adapter.ValidateThreadID(event.ThreadID)
	if validateErr != nil {
		err := finish(fmt.Errorf("chat: validate event thread id: %w", validateErr))
		c.safeEnd(span, OutcomeError)
		return preludeWork{}, true, err
	}
	scope := c.lockScopeKey(event, ref)

	// The prelude acquires the Thread Lock only under drop/queue; debounce
	// always coordinates in the tail, and concurrent takes no lock at all.
	var lease LockLease
	conflicted := false
	switch c.options.Concurrency {
	case ConcurrencyDrop, ConcurrencyQueue:
		acquiredLease, acquired, err := c.state.AcquireLock(ctx, scope, c.options.ThreadLockTTL)
		if err != nil {
			err := finish(fmt.Errorf("chat: acquire thread lock: %w", err))
			c.safeEnd(span, OutcomeError)
			return preludeWork{}, true, err
		}
		lease = acquiredLease
		if !acquired {
			if c.options.Concurrency == ConcurrencyDrop {
				accepted, err := acceptEvent()
				if err != nil {
					c.safeEnd(span, OutcomeError)
					return preludeWork{}, true, err
				}
				if !accepted {
					c.safeEnd(span, OutcomeDuplicate)
					return preludeWork{}, true, nil
				}
				c.logger.Info("chat lock conflict dropped", "adapter", event.Adapter, "event_id", event.ID, "thread_id", event.ThreadID)
				c.safeEvent(ctx, ObsLockConflict, AdapterAttr(event.Adapter))
				c.safeEnd(span, OutcomeDroppedLockConflict)
				return preludeWork{}, true, nil
			}
			conflicted = true
		}
	case ConcurrencyDebounce, ConcurrencyConcurrent:
	}

	// releaseOnResolve releases the lease when the event resolves here; an event
	// whose strategy defers lock coordination to the tail holds no lease yet.
	releaseOnResolve := func() {
		if lease == (LockLease{}) {
			return
		}
		c.releaseLock(ctx, lease, event.ThreadID)
	}
	// resolveError releases the lease and closes the span for a failed prelude.
	resolveError := func(err error) (preludeWork, bool, error) {
		releaseOnResolve()
		c.safeEnd(span, OutcomeError)
		return preludeWork{}, true, finish(err)
	}
	// resolveIgnored accepts (deduping), logs, and closes the span for an Ignored
	// Event; a duplicate closes as OutcomeDuplicate instead.
	resolveIgnored := func(reason, logMsg string, level slog.Level) (preludeWork, bool, error) {
		releaseOnResolve()
		accepted, err := acceptEvent()
		if err != nil {
			c.safeEnd(span, OutcomeError)
			return preludeWork{}, true, err
		}
		if !accepted {
			c.safeEnd(span, OutcomeDuplicate)
			return preludeWork{}, true, nil
		}
		c.logger.Log(ctx, level, logMsg, "adapter", event.Adapter, "event_id", event.ID)
		c.safeEvent(ctx, ObsIgnoredEvent, AdapterAttr(event.Adapter), ReasonAttr(reason))
		c.safeEnd(span, OutcomeIgnored, ReasonAttr(reason))
		return preludeWork{}, true, nil
	}

	thread := c.newThread(adapter, ref)
	bot := adapter.BotActor()

	// Non-message routing precedence: a Command Event or Interaction Event routes
	// to its own single-slot hook regardless of subscription state. Self-issued
	// commands/interactions are filtered like self messages so a bot cannot loop.
	switch {
	case event.Command != nil:
		if isSelfActor(event.Command.Actor, bot) {
			return resolveIgnored("self-command", "chat ignored self command", slog.LevelDebug)
		}
		c.handlersMu.RLock()
		handler := c.command
		c.handlersMu.RUnlock()
		if handler == nil {
			return resolveIgnored("no-command-handler", "chat ignored command with no handler", slog.LevelInfo)
		}
		cmdEvent := &CommandEvent{Event: event, Thread: thread, Command: event.Command}
		return c.routedWork(ctx, span, event, lease, scope, seq, conflicted, "command", func(ctx context.Context) error {
			return handler(ctx, cmdEvent)
		}, acceptEvent, releaseOnResolve)

	case event.Interaction != nil:
		if isSelfActor(event.Interaction.Actor, bot) {
			return resolveIgnored("self-interaction", "chat ignored self interaction", slog.LevelDebug)
		}
		c.handlersMu.RLock()
		handler := c.interaction
		c.handlersMu.RUnlock()
		if handler == nil {
			return resolveIgnored("no-interaction-handler", "chat ignored interaction with no handler", slog.LevelInfo)
		}
		intEvent := &InteractionEvent{Event: event, Thread: thread, Interaction: event.Interaction}
		return c.routedWork(ctx, span, event, lease, scope, seq, conflicted, "interaction", func(ctx context.Context) error {
			return handler(ctx, intEvent)
		}, acceptEvent, releaseOnResolve)
	}

	if event.Message == nil {
		return resolveIgnored("non-message", "chat ignored non-message event", slog.LevelInfo)
	}
	if isSelfActor(event.Message.Author, bot) {
		return resolveIgnored("self-message", "chat ignored self message", slog.LevelDebug)
	}

	handler, route, err := c.route(ctx, event)
	if err != nil {
		return resolveError(err)
	}
	if handler == nil {
		return resolveIgnored("unrouted", "chat ignored unrouted message", slog.LevelInfo)
	}

	msgEvent := &MessageEvent{Event: event, Thread: thread, Message: event.Message}
	return c.routedWork(ctx, span, event, lease, scope, seq, conflicted, route, func(ctx context.Context) error {
		return handler(ctx, msgEvent)
	}, acceptEvent, releaseOnResolve)
}

// routedWork accepts a routed event (deduping), applies the Concurrency
// Strategy's coordination decision (queue/debounce/concurrent), and builds the
// handler-agnostic preludeWork. A duplicate or a dedupe error resolves here
// instead.
func (c *Chat) routedWork(
	ctx context.Context,
	span DispatchSpan,
	event *Event,
	lease LockLease,
	scope string,
	seq uint64,
	conflicted bool,
	route string,
	run func(context.Context) error,
	acceptEvent func() (bool, error),
	releaseOnResolve func(),
) (preludeWork, bool, error) {
	accepted, err := acceptEvent()
	if err != nil {
		releaseOnResolve()
		c.safeEnd(span, OutcomeError)
		return preludeWork{}, true, err
	}
	if !accepted {
		releaseOnResolve()
		c.safeEnd(span, OutcomeDuplicate)
		return preludeWork{}, true, nil
	}

	work := preludeWork{
		event: event,
		lease: lease,
		run:   run,
		route: route,
		scope: scope,
		span:  span,
	}

	// resolveStale closes a routed event whose delivery was admitted before the
	// scope's current pending waiter: it never displaces the newer waiter.
	resolveStale := func(label string) (preludeWork, bool, error) {
		c.logger.Debug("chat "+label+" waiter superseded", "adapter", event.Adapter, "event_id", event.ID, "thread_id", event.ThreadID)
		c.safeEnd(span, OutcomeIgnored, RouteAttr(route))
		return preludeWork{}, true, nil
	}

	switch c.options.Concurrency {
	case ConcurrencyDrop, ConcurrencyQueue:
		if conflicted {
			// Only queue reaches routing with a conflict (drop resolves at the
			// conflict site): the event waits as its scope's pending waiter.
			work.needsLock = true
			work.waitLabel = "queue"
			displaced, registered := c.registerPending(scope, event, seq, work.waitLabel)
			if !registered {
				return resolveStale(work.waitLabel)
			}
			work.displaced = displaced
		}
		return work, false, nil

	case ConcurrencyDebounce:
		work.needsLock = true
		work.debounce = true
		work.waitLabel = "debounce"
		displaced, registered := c.registerPending(scope, event, seq, work.waitLabel)
		if !registered {
			return resolveStale(work.waitLabel)
		}
		work.displaced = displaced
		return work, false, nil

	case ConcurrencyConcurrent:
		work.noLock = true
		return work, false, nil
	}

	// Unreachable: the strategy set is validated at construction.
	releaseOnResolve()
	c.safeEnd(span, OutcomeError)
	return preludeWork{}, true, fmt.Errorf("chat: unsupported concurrency strategy %d", c.options.Concurrency)
}

// startDetachedTail runs the routed handler on the detached work context after
// ack: the Thread Lock is held across the tail, refreshed via ExtendLock, and
// released on exit. The context is derived from baseCtx, bounded by
// DetachTimeout, and cancelled by Shutdown. When needsLock, the tail first waits
// for the lock (after the debounce quiet period when the work asks for one); a
// superseded or abandoned waiter exits without running the handler. Concurrent
// tails wait for a MaxConcurrent slot rather than a lock.
func (c *Chat) startDetachedTail(work preludeWork) {
	assert(work.releaseAdmission != nil, "detached tail requires a held admission slot")
	tailCtx, tailCancel := context.WithTimeout(c.baseCtx, c.options.DetachTimeout)
	c.inflight.Add(1)
	go func() {
		// The admission slot is held until this goroutine returns: everything
		// the tail retains — the parked wait, the handler run, and cleanup
		// (lock release, refresh-loop drain) — counts against MaxDetached.
		defer work.releaseAdmission()
		defer c.inflight.Done()
		defer tailCancel()

		if work.noLock {
			if !c.acquireConcurrencySlot(tailCtx, work.event) {
				c.safeEnd(work.span, OutcomeIgnored, RouteAttr(work.route))
				return
			}
			defer c.releaseConcurrencySlot()
			c.logger.Info("chat deferred dispatch started", "adapter", work.event.Adapter, "event_id", work.event.ID, "route", work.route)
			err := work.run(tailCtx)
			c.endHandlerRun(tailCtx, work.event, work.route, work.span, err)
			return
		}

		if work.debounce && !c.waitDebounceQuietPeriod(tailCtx, work) {
			return
		}

		if work.needsLock {
			lease, outcome := c.queueForLock(tailCtx, work.scope, work.event, work.waitLabel)
			if outcome != acquireHeld {
				// A superseded or abandoned waiter never runs the handler; a state
				// failure is an error outcome, not a silent skip.
				c.safeEnd(work.span, waitOutcome(outcome), RouteAttr(work.route))
				return
			}
			work.lease = lease
		}

		c.logger.Info("chat deferred dispatch started", "adapter", work.event.Adapter, "event_id", work.event.ID, "route", work.route)
		c.runLockedTail(tailCtx, work)
	}()
}

// waitOutcome maps a non-held lock-wait result to its terminal DispatchOutcome:
// a backend failure surfaces as an error; supersession and abandonment close as
// ignored.
func waitOutcome(outcome acquireOutcome) DispatchOutcome {
	if outcome == acquireFailed {
		return OutcomeError
	}
	return OutcomeIgnored
}

// runLockedTail runs a lock-holding deferred handler: the Lock Lease refresh
// loop runs alongside and the lease is released on exit.
func (c *Chat) runLockedTail(tailCtx context.Context, work preludeWork) {
	// Every deferred lock holder is cancellable on lease loss (with cause
	// ErrPreempted): a lease that vanished — released by another runtime
	// instance, or expired — or that can no longer be refreshed means mutual
	// exclusion is gone, so the handler must stop rather than run alongside
	// the lease's next holder.
	runCtx, cancel := context.WithCancelCause(tailCtx)
	defer cancel(nil)

	stopRefresh, leaseLost := c.startLockRefresh(tailCtx, work.lease, work.event.ThreadID, cancel)
	err := work.run(runCtx)
	// The classification snapshot is taken before stopRefresh drains the loop:
	// a lease-loss result observed only after the handler already returned must
	// not reclassify its completed run (and must not suppress a real handler
	// error). Within the run, the outcome follows the cancellation cause, not
	// the handler's return convention: a handler that observes ctx.Done, shuts
	// down cleanly, and returns nil still lost its lease.
	preempted := errors.Is(context.Cause(runCtx), ErrPreempted)
	stopRefresh()
	if preempted && tailCtx.Err() == nil {
		c.logger.Info("chat handler preempted", "adapter", work.event.Adapter, "event_id", work.event.ID, "thread_id", work.event.ThreadID, "route", work.route)
		c.safeEnd(work.span, OutcomePreempted, RouteAttr(work.route))
	} else {
		c.endHandlerRun(tailCtx, work.event, work.route, work.span, err)
	}

	// A failed release is expected (lease gone) when the context was cancelled
	// or the refresh loop already lost the lease, so downgrade it from WARN.
	benignRelease := tailCtx.Err() != nil || leaseLost() || preempted
	c.releaseTailLock(tailCtx, work.lease, work.event.ThreadID, benignRelease)
}

// endHandlerRun closes one deferred handler run with the shared terminal
// logging, observation, and span outcome.
func (c *Chat) endHandlerRun(tailCtx context.Context, event *Event, route string, span DispatchSpan, err error) {
	if err != nil {
		if ctxErr := tailCtx.Err(); ctxErr != nil {
			c.logger.Error("chat deferred handler timed out", "error", err, "adapter", event.Adapter, "event_id", event.ID, "route", route)
		} else {
			c.logger.Error("chat handler failed", "error", err, "adapter", event.Adapter, "event_id", event.ID, "route", route, "mode", "deferred")
		}
		c.safeEvent(tailCtx, ObsHandlerError, AdapterAttr(event.Adapter), RouteAttr(route))
		c.safeEnd(span, OutcomeError, RouteAttr(route))
		return
	}
	c.safeEnd(span, OutcomeHandled, RouteAttr(route))
}

// waitDebounceQuietPeriod holds the routed event through its DebounceInterval
// quiet period. A superseded waiter exits promptly on its displacement signal
// (releasing its goroutine and event immediately instead of parking through the
// full interval), and an abandoned wait exits when the Detached Work Context
// ends. It reports false when the handler must not run (the span is closed
// here).
func (c *Chat) waitDebounceQuietPeriod(ctx context.Context, work preludeWork) bool {
	timer := time.NewTimer(c.options.DebounceInterval)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		c.clearPending(work.scope, work.event)
		c.logger.Info("chat debounce wait abandoned", "adapter", work.event.Adapter, "event_id", work.event.ID, "thread_id", work.event.ThreadID, "error", ctx.Err())
		c.safeEnd(work.span, OutcomeIgnored, RouteAttr(work.route))
		return false
	case <-work.displaced:
		// registerPending already emitted the superseded_by record.
		c.logger.Debug("chat debounce waiter superseded", "adapter", work.event.Adapter, "event_id", work.event.ID, "thread_id", work.event.ThreadID)
		c.safeEnd(work.span, OutcomeIgnored, RouteAttr(work.route))
		return false
	case <-timer.C:
		return true
	}
}

// acquireConcurrencySlot blocks until a MaxConcurrent slot frees, bounded by
// ctx. It reports false when the wait was abandoned.
func (c *Chat) acquireConcurrencySlot(ctx context.Context, event *Event) bool {
	assert(c.concurrencySlots != nil, "concurrency slots are only used under the concurrent strategy")
	select {
	case c.concurrencySlots <- struct{}{}:
		return true
	case <-ctx.Done():
		c.logger.Info("chat concurrent slot wait abandoned", "adapter", event.Adapter, "event_id", event.ID, "thread_id", event.ThreadID, "error", ctx.Err())
		return false
	}
}

func (c *Chat) releaseConcurrencySlot() {
	<-c.concurrencySlots
}

// lockScopeKey chooses what key the Thread Lock guards. Thread scope (the
// default) keys by the opaque Thread ID, unchanged from prior behavior. Channel
// scope widens serialization to the Thread's channel; a Thread whose adapter
// reports no channel falls back to per-Thread locking rather than sharing one
// adapter-wide key. Under channel scope every key carries a namespace prefix
// and length-prefixed fields, so channel keys are injective and a fallback
// Thread ID can never collide with a synthesized channel key.
func (c *Chat) lockScopeKey(event *Event, ref ThreadRef) string {
	if c.options.LockScope != LockScopeChannel {
		return string(event.ThreadID)
	}
	if ref.Channel == "" {
		return fmt.Sprintf("thread-scope/%d:%s", len(event.ThreadID), event.ThreadID)
	}
	return fmt.Sprintf("channel-scope/%d:%s/%d:%s/%d:%s",
		len(event.Adapter), event.Adapter,
		len(ref.Tenant), ref.Tenant,
		len(ref.Channel), ref.Channel,
	)
}

// startLockRefresh extends the Lock Lease on a cadence below ThreadLockTTL so it
// does not expire while the detached handler runs. Extend runs under
// context.WithoutCancel so a refresh in flight at cancellation still completes.
// stop halts the loop and blocks until it exits; leaseLost reports whether the
// loop could no longer vouch for the lease, and is safe to read once stop has
// returned. A lost lease — vanished (force released by a preempting delivery
// on any runtime instance, or expired) or unmaintainable (a failed refresh
// means it expires at TTL mid-handler) — also cancels the handler with
// ErrPreempted (onLeaseLost): mutual exclusion is gone or going, so the
// handler must stop rather than run alongside the lease's next holder.
func (c *Chat) startLockRefresh(ctx context.Context, lease LockLease, threadID ThreadID, onLeaseLost context.CancelCauseFunc) (stop func(), leaseLost func() bool) {
	interval := c.options.ThreadLockTTL / 2
	if interval <= 0 {
		interval = c.options.ThreadLockTTL
	}
	var lost atomic.Bool
	stopCh := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-stopCh:
				return
			case <-ctx.Done():
				return
			case <-ticker.C:
				extended, err := c.state.ExtendLock(context.WithoutCancel(ctx), lease, c.options.ThreadLockTTL)
				if err != nil {
					// A failed refresh means the lease can no longer be guaranteed:
					// it will expire at TTL while the handler is still running, so
					// the handler must stop rather than outlive its serialization.
					c.logger.Error("chat extend thread lock failed", "error", err, "thread_id", threadID)
					lost.Store(true)
					if onLeaseLost != nil {
						onLeaseLost(ErrPreempted)
					}
					return
				}
				if !extended {
					c.logger.Warn("chat thread lock lease lost", "thread_id", threadID)
					lost.Store(true)
					if onLeaseLost != nil {
						onLeaseLost(ErrPreempted)
					}
					return
				}
				c.logger.Debug("chat thread lock refreshed", "thread_id", threadID)
			}
		}
	}()
	return sync.OnceFunc(func() {
			close(stopCh)
			<-done
		}), func() bool {
			return lost.Load()
		}
}

// acquireOutcome is the result of waiting for the Thread Lock under the queue
// strategy.
type acquireOutcome int

const (
	// acquireHeld means the lock was acquired and is held by the returned lease.
	acquireHeld acquireOutcome = iota
	// acquireSuperseded means a newer follow-up for the scope overtook this waiter.
	acquireSuperseded
	// acquireAbandoned means the waiter's context ended before the lock freed.
	acquireAbandoned
	// acquireFailed means AcquireLock returned an error while polling.
	acquireFailed
)

// pendingWaiter is one scope's most-recent pending event plus a displacement
// signal: displaced closes when a newer event supersedes this waiter, so a
// parked (debounce) waiter exits promptly instead of holding its event through
// the full interval. seq is the delivery's dispatch admission sequence, which
// defines "newer".
type pendingWaiter struct {
	event     *Event
	seq       uint64
	displaced chan struct{}
}

// registerPending records event as the most-recent pending event for its scope,
// superseding (and signalling) any earlier waiter. "Most recent" follows the
// dispatch admission sequence, not registration time: a delivery delayed in
// its prelude (validation, dedupe, routing) never displaces a waiter admitted
// after it, and instead reports registered=false so the caller resolves it as
// superseded. Recording supersession here, where the displacing event's id is
// known, lets the record carry superseded_by. label names the coordination
// mode ("queue" or "debounce") so the observation names the strategy that
// displaced the waiter. The returned channel closes when this registration is
// itself superseded.
func (c *Chat) registerPending(scope string, event *Event, seq uint64, label string) (displaced <-chan struct{}, registered bool) {
	c.queueMu.Lock()
	defer c.queueMu.Unlock()
	if prev := c.pending[scope]; prev != nil {
		if prev.seq > seq {
			return nil, false
		}
		c.logger.Info("chat "+label+" superseded", "adapter", prev.event.Adapter, "event_id", prev.event.ID, "thread_id", prev.event.ThreadID, "superseded_by", event.ID)
		close(prev.displaced)
	}
	waiter := &pendingWaiter{event: event, seq: seq, displaced: make(chan struct{})}
	c.pending[scope] = waiter
	return waiter.displaced, true
}

// isPending reports whether event is still its scope's most-recent pending
// waiter.
func (c *Chat) isPending(scope string, event *Event) bool {
	c.queueMu.Lock()
	defer c.queueMu.Unlock()
	waiter := c.pending[scope]
	return waiter != nil && waiter.event == event
}

// queueForLock waits for the Thread Lock on behalf of an event already
// registered as a pending waiter for its scope, polling AcquireLock until the
// in-flight handler releases. It returns acquireSuperseded if a newer follow-up
// displaced this waiter, acquireAbandoned if ctx ended, acquireFailed on an
// AcquireLock error, or the lease (acquireHeld) once acquired. label names the
// coordination mode in the wait observations.
func (c *Chat) queueForLock(ctx context.Context, scope string, event *Event, label string) (LockLease, acquireOutcome) {
	superseded := func() bool {
		return !c.isPending(scope, event)
	}
	lease, outcome, err := c.pollForLock(ctx, scope, superseded)
	switch outcome {
	case acquireHeld:
		// Re-check ownership after the acquire: a newer event registering while
		// AcquireLock was in flight must win, or a superseded debounce/queue
		// waiter would dispatch alongside the final one. Once the holder passes
		// this check it is committed; later arrivals wait on the lock.
		if !c.isPending(scope, event) {
			c.releaseLock(ctx, lease, event.ThreadID)
			c.logger.Debug("chat "+label+" waiter superseded", "adapter", event.Adapter, "event_id", event.ID, "thread_id", event.ThreadID)
			return LockLease{}, acquireSuperseded
		}
		c.clearPending(scope, event)
	case acquireSuperseded:
		// registerPending already emitted the superseded_by record; this waiter
		// just stops.
		c.logger.Debug("chat "+label+" waiter superseded", "adapter", event.Adapter, "event_id", event.ID, "thread_id", event.ThreadID)
	case acquireAbandoned:
		c.clearPending(scope, event)
		c.logger.Info("chat "+label+" wait abandoned", "adapter", event.Adapter, "event_id", event.ID, "thread_id", event.ThreadID, "error", ctx.Err())
	case acquireFailed:
		c.clearPending(scope, event)
		c.logger.Error("chat "+label+" acquire lock failed", "error", err, "adapter", event.Adapter, "event_id", event.ID, "thread_id", event.ThreadID)
	}
	return lease, outcome
}

// pollForLock polls AcquireLock until it is held, the optional superseded check
// trips, ctx ends, or the state fails. Waiters poll because State does not
// signal release.
func (c *Chat) pollForLock(ctx context.Context, scope string, superseded func() bool) (LockLease, acquireOutcome, error) {
	ticker := time.NewTicker(c.queuePollInterval())
	defer ticker.Stop()
	for {
		if superseded != nil && superseded() {
			return LockLease{}, acquireSuperseded, nil
		}

		lease, acquired, err := c.state.AcquireLock(ctx, scope, c.options.ThreadLockTTL)
		if err != nil {
			if ctx.Err() != nil {
				return LockLease{}, acquireAbandoned, ctx.Err()
			}
			return LockLease{}, acquireFailed, err
		}
		if acquired {
			return lease, acquireHeld, nil
		}

		select {
		case <-ctx.Done():
			return LockLease{}, acquireAbandoned, ctx.Err()
		case <-ticker.C:
		}
	}
}

func (c *Chat) clearPending(scope string, event *Event) {
	c.queueMu.Lock()
	defer c.queueMu.Unlock()
	if waiter := c.pending[scope]; waiter != nil && waiter.event == event {
		delete(c.pending, scope)
	}
}

// queuePollInterval bounds how often a queued waiter retries AcquireLock at
// ThreadLockTTL/20, clamped to [1ms, 1s]. Waiters poll because State does not
// signal release.
func (c *Chat) queuePollInterval() time.Duration {
	interval := c.options.ThreadLockTTL / 20
	const (
		minInterval = time.Millisecond
		maxInterval = time.Second
	)
	if interval < minInterval {
		return minInterval
	}
	if interval > maxInterval {
		return maxInterval
	}
	return interval
}

// releaseLock releases the Thread Lock under context.WithoutCancel so release
// still happens after cancellation. A lock that was not released (token gone) is
// surfaced as a WARN.
func (c *Chat) releaseLock(ctx context.Context, lease LockLease, threadID ThreadID) {
	if released, err := c.state.ReleaseLock(context.WithoutCancel(ctx), lease); err != nil {
		c.logger.Error("chat release thread lock failed", "error", err, "thread_id", threadID)
		c.safeEvent(ctx, ObsLockReleaseFailed)
	} else if !released {
		c.logger.Warn("chat thread lock was not released", "thread_id", threadID)
		c.safeEvent(ctx, ObsLockReleaseFailed)
	}
}

// releaseTailLock mirrors releaseLock for the deferred tail but, when benign,
// downgrades the "not released" case from WARN to Debug so a normal drain stays
// quiet.
func (c *Chat) releaseTailLock(ctx context.Context, lease LockLease, threadID ThreadID, benign bool) {
	if released, err := c.state.ReleaseLock(context.WithoutCancel(ctx), lease); err != nil {
		c.logger.Error("chat release thread lock failed", "error", err, "thread_id", threadID)
		c.safeEvent(ctx, ObsLockReleaseFailed)
	} else if !released {
		if benign {
			c.logger.Debug("chat thread lock was not released", "thread_id", threadID)
		} else {
			c.logger.Warn("chat thread lock was not released", "thread_id", threadID)
			c.safeEvent(ctx, ObsLockReleaseFailed)
		}
	}
}

func (c *Chat) beginEventAcceptance(eventID string) (*eventAcceptance, bool) {
	c.acceptancesMu.Lock()
	defer c.acceptancesMu.Unlock()
	if acceptance, ok := c.eventAcceptances[eventID]; ok {
		return acceptance, false
	}
	acceptance := &eventAcceptance{done: make(chan struct{})}
	c.eventAcceptances[eventID] = acceptance
	return acceptance, true
}

func (c *Chat) finishEventAcceptance(eventID string, acceptance *eventAcceptance, err error) {
	c.acceptancesMu.Lock()
	defer c.acceptancesMu.Unlock()
	if c.eventAcceptances[eventID] == acceptance {
		delete(c.eventAcceptances, eventID)
	}
	acceptance.err = err
	close(acceptance.done)
}

func waitEventAcceptance(ctx context.Context, acceptance *eventAcceptance) error {
	select {
	case <-acceptance.done:
		return acceptance.err
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (c *Chat) markAcceptedEvent(ctx context.Context, event *Event) (bool, error) {
	firstSeen, err := c.state.MarkEvent(ctx, event.ID, c.options.DedupeTTL)
	if err != nil {
		return false, fmt.Errorf("chat: mark event: %w", err)
	}
	if !firstSeen {
		c.logger.Info("chat duplicate event dropped", "adapter", event.Adapter, "event_id", event.ID)
		return false, nil
	}
	return true, nil
}

func validateEvent(event *Event) error {
	if event == nil {
		return errors.New("chat: nil event")
	}
	if event.ID == "" {
		return errors.New("chat: event id is required")
	}
	if event.Adapter == "" {
		return errors.New("chat: event adapter is required")
	}
	if event.ThreadID == "" {
		return errors.New("chat: event thread id is required")
	}
	return nil
}

func (c *Chat) route(ctx context.Context, event *Event) (MessageHandler, string, error) {
	subscribed, err := c.state.IsThreadSubscribed(ctx, event.ThreadID)
	if err != nil {
		return nil, "", fmt.Errorf("chat: check subscription: %w", err)
	}

	c.handlersMu.RLock()
	defer c.handlersMu.RUnlock()

	if subscribed {
		return c.subscribedMessage, "subscribed-message", nil
	}
	if event.DirectMessage || event.Message.Mentioned {
		return c.newMention, "new-mention", nil
	}
	return nil, "", nil
}

func adapterNameFromThreadID(id ThreadID) (string, error) {
	name, _, ok := strings.Cut(string(id), ":")
	if !ok || name == "" {
		return "", fmt.Errorf("chat: malformed thread id %q", id)
	}
	return name, nil
}

// isSelfActor reports whether actor is the adapter's own bot, used to filter
// self messages, self commands, and self interactions so a bot cannot loop.
func isSelfActor(actor Actor, bot Actor) bool {
	return actor.BotKind == BotBot &&
		actor.Adapter == bot.Adapter &&
		actor.Tenant == bot.Tenant &&
		actor.ID == bot.ID
}
