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
	// Like queue supersession, coalescing is per runtime instance: events for
	// one scope delivered to different instances sharing a State are not
	// superseded across instances (each instance dispatches its own final
	// event, serialized by the Thread Lock). Cross-instance coalescing needs
	// the wait/coalesce State-contract extension anticipated by ADR 0012.
	ConcurrencyDebounce
	// ConcurrencyBurst collects routed events for a scope while a
	// DebounceInterval window is open, then dispatches the whole batch — in the
	// order the events joined the window — under a single Thread Lock hold. No
	// event is skipped: every batch member runs under its own DetachTimeout
	// execution budget (collection time and earlier members never consume it),
	// with the batch as a whole bounded by Shutdown and by Lock Lease loss.
	// Join order follows each event's admission through the dispatch prelude;
	// ordering of concurrent webhook deliveries is platform-dependent and not
	// re-established by the runtime. Windows are per runtime instance (like
	// debounce coalescing), serialized across instances by the Thread Lock.
	// Requires deferred dispatch, like ConcurrencyDebounce.
	ConcurrencyBurst
	// ConcurrencyConcurrent is the explicit opt-out of per-scope serialization:
	// every routed event dispatches immediately in its own execution, bounded by
	// MaxConcurrent. No Thread Lock is taken, so the caller accepts interleaved
	// replies and races on Thread Application State.
	ConcurrencyConcurrent
)

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

// LockConflictHook is the force/steerability hook consulted on a Lock Conflict
// for a routed, accepted event. Returning true preempts the in-flight handler:
// the runtime cancels the local Detached Work Context with ErrPreempted, force
// releases the current Lock Lease through the state's LockForcer capability,
// and dispatches the new event under a fresh lease. Returning false falls back
// to the configured Concurrency Strategy. The hook must be fast and must not
// block dispatch; a panicking hook is recovered and treated as false.
type LockConflictHook func(context.Context, *Event) bool

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
	// DebounceInterval is the quiet period (ConcurrencyDebounce) or the
	// collection window (ConcurrencyBurst). It must be positive under those
	// strategies and is ignored otherwise.
	DebounceInterval time.Duration
	// MaxConcurrent bounds simultaneous handler executions under
	// ConcurrencyConcurrent. It must be positive under that strategy and is
	// ignored otherwise.
	MaxConcurrent int
	// OnLockConflict, when set, lets a new delivery preempt an in-flight handler
	// on a Lock Conflict instead of being dropped or queued. It requires
	// deferred dispatch (the Detached Work Context is what makes the preempted
	// handler cancellable), the drop or queue strategy, and a State that
	// implements LockForcer.
	OnLockConflict LockConflictHook
}

func DefaultRuntimeOptions() RuntimeOptions {
	return RuntimeOptions{
		DedupeTTL:     24 * time.Hour,
		ThreadLockTTL: 2 * time.Minute,
		Concurrency:   ConcurrencyDrop,
		Dispatch:      DispatchSync,
		DetachTimeout: 0,
		LockScope:     LockScopeThread,
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

	// burstMu guards burstScopes, the per-scope burst collection windows.
	burstMu     sync.Mutex
	burstScopes map[string]*burstScope

	// inflightMu guards inflightCancels, the per-scope local lease ownership
	// reservations (acquiring or holding), used by preemption.
	inflightMu      sync.Mutex
	inflightCancels map[string]map[*inflightCancel]struct{}

	// concurrencySlots bounds simultaneous handler executions under
	// ConcurrencyConcurrent; nil under every other strategy.
	concurrencySlots chan struct{}

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
	if cfg.options.OnLockConflict != nil {
		if _, ok := cfg.state.(LockForcer); !ok {
			return nil, errors.New("chat: on-lock-conflict hook requires a State implementing LockForcer")
		}
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
		burstScopes:      map[string]*burstScope{},
		inflightCancels:  map[string]map[*inflightCancel]struct{}{},
	}
	if cfg.options.Concurrency == ConcurrencyConcurrent {
		chat.concurrencySlots = make(chan struct{}, cfg.options.MaxConcurrent)
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
	case ConcurrencyDebounce, ConcurrencyBurst:
		if options.DebounceInterval <= 0 {
			return errors.New("chat: debounce interval must be positive under the debounce and burst strategies")
		}
		if options.Dispatch != DispatchDeferred {
			return errors.New("chat: debounce and burst strategies require deferred dispatch")
		}
		// The interval wait runs inside the DetachTimeout-bounded Detached Work
		// Context: a timeout at or below the interval would abandon every
		// accepted event before its quiet period or collection window closes.
		if options.DetachTimeout <= options.DebounceInterval {
			return errors.New("chat: detach timeout must exceed the debounce interval under the debounce and burst strategies")
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
	if options.OnLockConflict != nil {
		if options.Dispatch != DispatchDeferred {
			return errors.New("chat: on-lock-conflict hook requires deferred dispatch")
		}
		switch options.Concurrency {
		case ConcurrencyDrop, ConcurrencyQueue:
		default:
			return errors.New("chat: on-lock-conflict hook requires the drop or queue strategy")
		}
	}
	switch options.Dispatch {
	case DispatchSync:
	case DispatchDeferred:
		if options.DetachTimeout <= 0 {
			return errors.New("chat: detach timeout must be positive under deferred dispatch")
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

	// Cancel detached tails, then drain (bounded by ctx) before shutting down
	// adapters and state.
	c.baseCancel()
	var drainErr error
	drained := make(chan struct{})
	go func() {
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
	if c.options.Dispatch == DispatchDeferred {
		return c.dispatchDeferred(ctx, event)
	}
	return c.dispatchSync(ctx, event)
}

// dispatchSync runs the prelude and the routed handler inline under the request
// context, releasing the Thread Lock when the handler returns. Under the queue
// strategy a Lock Conflict waits inline (bounded by ctx) for the in-flight
// handler before the routed handler runs. Under the concurrent strategy the
// request waits inline for a MaxConcurrent slot instead of a lock.
func (c *Chat) dispatchSync(ctx context.Context, event *Event) error {
	work, resolved, err := c.prelude(ctx, event)
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
			lease, _, outcome := c.queueForLock(ctx, work.scope, event, work.waitLabel)
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
// the platform (ack-then-work).
func (c *Chat) dispatchDeferred(ctx context.Context, event *Event) error {
	work, resolved, err := c.prelude(ctx, event)
	if err != nil || resolved {
		return err
	}
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
	// queued/preempting Lock Conflict, or the debounce strategy); the tail must
	// wait for and acquire it before running the handler.
	needsLock bool
	// waitLabel names the coordination mode ("queue", "debounce", or "preempt")
	// in wait observations so a log line names the strategy that produced it.
	waitLabel string
	// displaced closes when a newer event supersedes this pending waiter; the
	// debounce quiet-period wait exits promptly on it instead of parking through
	// its full interval.
	displaced <-chan struct{}
	// debounce is true when the tail must hold the event through the
	// DebounceInterval quiet period before waiting for the lock.
	debounce bool
	// preempt is true when the OnLockConflict hook elected to preempt the
	// in-flight handler: the tail cancels it locally and force releases the
	// current Lock Lease before waiting for a fresh one.
	preempt bool
	// noLock is true under the concurrent strategy: no Thread Lock is taken and
	// the run is bounded by a MaxConcurrent slot instead.
	noLock bool
	// inflight is the scope ownership reservation made when the lease was
	// acquired (preemptible runtimes only); the tail arms it with the running
	// handler's cancel and retires it after release.
	inflight *inflightCancel
	// burstRunner marks a scope-level burst runner tail rather than an
	// event-level dispatch; only event and scope are set.
	burstRunner bool
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
func (c *Chat) prelude(ctx context.Context, event *Event) (preludeWork, bool, error) {
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

	// The prelude acquires the Thread Lock only under drop/queue; debounce and
	// burst always coordinate in the tail, and concurrent takes no lock at all.
	var lease LockLease
	var reservation *inflightCancel
	conflicted := false
	switch c.options.Concurrency {
	case ConcurrencyDrop, ConcurrencyQueue:
		if c.options.OnLockConflict != nil {
			// Local ownership is reserved BEFORE the acquire so a lease can never
			// exist locally without a visible reservation: a preemptor arriving at
			// any point after acquisition finds the local owner and waits instead
			// of force-releasing the fresh lease.
			reservation = c.reserveInflight(scope)
		}
		acquiredLease, acquired, err := c.state.AcquireLock(ctx, scope, c.options.ThreadLockTTL)
		if err != nil {
			if reservation != nil {
				c.retireInflight(scope, reservation)
			}
			err := finish(fmt.Errorf("chat: acquire thread lock: %w", err))
			c.safeEnd(span, OutcomeError)
			return preludeWork{}, true, err
		}
		lease = acquiredLease
		if acquired && reservation != nil {
			reservation.markHeld()
		}
		if !acquired {
			if reservation != nil {
				c.retireInflight(scope, reservation)
				reservation = nil
			}
			if c.options.Concurrency == ConcurrencyDrop && c.options.OnLockConflict == nil {
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
	case ConcurrencyDebounce, ConcurrencyBurst, ConcurrencyConcurrent:
	}

	// releaseOnResolve releases the lease (and retires its ownership
	// reservation) when the event resolves here; an event whose strategy defers
	// lock coordination to the tail holds no lease yet.
	releaseOnResolve := func() {
		if lease == (LockLease{}) {
			return
		}
		c.releaseLock(ctx, lease, event.ThreadID)
		if reservation != nil {
			c.retireInflight(scope, reservation)
		}
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
		return c.routedWork(ctx, span, event, lease, reservation, scope, conflicted, "command", func(ctx context.Context) error {
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
		return c.routedWork(ctx, span, event, lease, reservation, scope, conflicted, "interaction", func(ctx context.Context) error {
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
	return c.routedWork(ctx, span, event, lease, reservation, scope, conflicted, route, func(ctx context.Context) error {
		return handler(ctx, msgEvent)
	}, acceptEvent, releaseOnResolve)
}

// routedWork accepts a routed event (deduping), applies the Concurrency
// Strategy's coordination decision (queue/preempt/debounce/burst/concurrent),
// and builds the handler-agnostic preludeWork. A duplicate, a dedupe error, or
// a dropped Lock Conflict resolves here instead.
func (c *Chat) routedWork(
	ctx context.Context,
	span DispatchSpan,
	event *Event,
	lease LockLease,
	reservation *inflightCancel,
	scope string,
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
		event:    event,
		lease:    lease,
		run:      run,
		route:    route,
		scope:    scope,
		span:     span,
		inflight: reservation,
	}

	switch c.options.Concurrency {
	case ConcurrencyDrop, ConcurrencyQueue:
		if !conflicted {
			return work, false, nil
		}
		// A Lock Conflict on a routed, accepted event: the steerability hook may
		// preempt the in-flight handler; otherwise the strategy decides.
		work.needsLock = true
		if c.options.OnLockConflict != nil && c.safeLockConflictHook(ctx, event) {
			work.preempt = true
			work.waitLabel = "preempt"
			work.displaced = c.registerPending(scope, event, work.waitLabel)
			return work, false, nil
		}
		if c.options.Concurrency == ConcurrencyQueue {
			work.waitLabel = "queue"
			work.displaced = c.registerPending(scope, event, work.waitLabel)
			return work, false, nil
		}
		// Drop fallback: the hook declined, so the conflict is acknowledged and
		// dropped exactly like the plain drop strategy.
		c.logger.Info("chat lock conflict dropped", "adapter", event.Adapter, "event_id", event.ID, "thread_id", event.ThreadID)
		c.safeEvent(ctx, ObsLockConflict, AdapterAttr(event.Adapter))
		c.safeEnd(span, OutcomeDroppedLockConflict)
		return preludeWork{}, true, nil

	case ConcurrencyDebounce:
		work.needsLock = true
		work.debounce = true
		work.waitLabel = "debounce"
		work.displaced = c.registerPending(scope, event, work.waitLabel)
		return work, false, nil

	case ConcurrencyBurst:
		// The event joins its scope's collection window; the runner tail (started
		// by at most one joining event per window chain) owns every joined span.
		if c.joinBurstBatch(scope, work) {
			return preludeWork{event: event, scope: scope, burstRunner: true}, false, nil
		}
		return preludeWork{}, true, nil

	case ConcurrencyConcurrent:
		work.noLock = true
		return work, false, nil
	}

	// Unreachable: the strategy set is validated at construction.
	releaseOnResolve()
	c.safeEnd(span, OutcomeError)
	return preludeWork{}, true, fmt.Errorf("chat: unsupported concurrency strategy %d", c.options.Concurrency)
}

// safeLockConflictHook invokes the steerability hook best-effort: a panicking
// hook is recovered and treated as "follow the configured strategy".
func (c *Chat) safeLockConflictHook(ctx context.Context, event *Event) (preempt bool) {
	defer func() {
		if r := recover(); r != nil {
			c.logger.Warn("chat lock conflict hook panicked", "recovered", r, "adapter", event.Adapter, "event_id", event.ID)
			preempt = false
		}
	}()
	return c.options.OnLockConflict(ctx, event)
}

// startDetachedTail runs the routed handler on the detached work context after
// ack: the Thread Lock is held across the tail, refreshed via ExtendLock, and
// released on exit. The context is derived from baseCtx, bounded by
// DetachTimeout, and cancelled by Shutdown. When needsLock, the tail first waits
// for the lock (after the debounce quiet period and/or preemption when the work
// asks for them); a superseded or abandoned waiter exits without running the
// handler. Burst runner tails coordinate their scope's batch instead of a
// single event, and concurrent tails wait for a MaxConcurrent slot rather than
// a lock.
func (c *Chat) startDetachedTail(work preludeWork) {
	tailCtx, tailCancel := context.WithTimeout(c.baseCtx, c.options.DetachTimeout)
	c.inflight.Add(1)
	go func() {
		defer c.inflight.Done()
		defer tailCancel()

		if work.burstRunner {
			c.runBurstScope(tailCtx, work.scope)
			return
		}

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

		if work.preempt {
			c.preemptScope(tailCtx, work)
		}

		if work.needsLock {
			lease, reservation, outcome := c.queueForLock(tailCtx, work.scope, work.event, work.waitLabel)
			if outcome != acquireHeld {
				// A superseded or abandoned waiter never runs the handler; a state
				// failure is an error outcome, not a silent skip.
				c.safeEnd(work.span, waitOutcome(outcome), RouteAttr(work.route))
				return
			}
			work.lease = lease
			work.inflight = reservation
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
// loop runs alongside, the handler's cancel is armed on the scope's ownership
// reservation when the steerability hook is configured, and the lease is
// released on exit. The reservation is retired only after the lease release so
// a waiting preemptor observes a scope whose lock is genuinely free.
func (c *Chat) runLockedTail(tailCtx context.Context, work preludeWork) {
	// Every deferred lock holder is cancellable on lease loss (with cause
	// ErrPreempted), regardless of the local hook configuration: a lease force
	// released by another runtime instance — or expired — means mutual
	// exclusion is already gone, so the handler must stop rather than run
	// alongside the lease's next holder.
	runCtx, cancel := context.WithCancelCause(tailCtx)
	defer cancel(nil)
	if c.options.OnLockConflict != nil {
		assert(work.inflight != nil, "preemptible lock-holding work must carry an ownership reservation")
		if !work.inflight.arm(cancel) {
			// Preempted between acquisition and start: the handler never runs.
			c.logger.Info("chat handler preempted", "adapter", work.event.Adapter, "event_id", work.event.ID, "thread_id", work.event.ThreadID, "route", work.route, "started", false)
			c.safeEnd(work.span, OutcomePreempted, RouteAttr(work.route))
			c.releaseTailLock(tailCtx, work.lease, work.event.ThreadID, true)
			c.retireInflight(work.scope, work.inflight)
			return
		}
	}

	stopRefresh, leaseLost := c.startLockRefresh(tailCtx, work.lease, work.event.ThreadID, cancel)
	err := work.run(runCtx)
	stopRefresh()

	// The preemption outcome follows the cancellation cause, not the handler's
	// return convention: a handler that observes ctx.Done, shuts down cleanly,
	// and returns nil was still preempted (or lost its lease).
	preempted := errors.Is(context.Cause(runCtx), ErrPreempted)
	if preempted && tailCtx.Err() == nil {
		c.logger.Info("chat handler preempted", "adapter", work.event.Adapter, "event_id", work.event.ID, "thread_id", work.event.ThreadID, "route", work.route, "started", true)
		c.safeEnd(work.span, OutcomePreempted, RouteAttr(work.route))
	} else {
		c.endHandlerRun(tailCtx, work.event, work.route, work.span, err)
	}

	// A failed release is expected (lease gone) when the context was cancelled,
	// the refresh loop already lost the lease, or the lease was force released
	// by a preempting delivery, so downgrade it from WARN.
	benignRelease := tailCtx.Err() != nil || leaseLost() || preempted
	c.releaseTailLock(tailCtx, work.lease, work.event.ThreadID, benignRelease)
	if work.inflight != nil {
		c.retireInflight(work.scope, work.inflight)
	}
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

// preemptScope preempts the scope's in-flight handler on behalf of a new
// delivery. A local victim is cancelled with ErrPreempted and awaited: it
// releases its own lease, so the preemptor then acquires normally and the
// force capability is never invoked — a lease acquired by a fresh handler
// after the victim's release is never destroyed. Only when no local victim is
// registered (the conflicting lease belongs to another runtime instance or is
// orphaned) is the lease force released through LockForcer so this delivery
// can acquire a fresh one. The token-owned lease invariant is preserved: the
// lease is explicitly invalidated, never deleted by presenting another
// holder's token.
//
// Waiting (bounded by the Detached Work Context) for the local victim keeps
// drop/queue per-scope serialization intact for handlers on this instance. A
// remote holder can only be fenced by the lease: it observes the force release
// on its next refresh, which cancels its handler, so cross-instance overlap is
// bounded by one refresh interval. The same fencing bounds the instruction-
// scale window in which a fresh holder could acquire between the pending
// re-check and the force call.
func (c *Chat) preemptScope(ctx context.Context, work preludeWork) {
	// A waiter superseded between routing and this tail must not destroy the
	// in-flight handler on behalf of an event that will never dispatch; the
	// newest waiter performs its own preemption if its hook elected one.
	for {
		// A waiter superseded (or abandoned) at any point yields silently: the
		// newest waiter owns the preemption.
		if ctx.Err() != nil {
			return
		}
		// Validation and destructive cancellation are atomic under the pending
		// registry lock: a stale preemptor (displaced by a newer registration)
		// can never cancel the active holder — or poison a newer waiter's
		// reservation — on behalf of an event that will not dispatch.
		victims, stillPending := c.preemptLocalIfPending(work.scope, work.event)
		if !stillPending {
			return
		}
		if len(victims) == 0 {
			break
		}
		anyHeld := false
		for _, victim := range victims {
			select {
			case <-victim.done:
			case <-ctx.Done():
				// The abandoned preemptor exits through the lock wait; the victim
				// keeps its lease.
				return
			}
			if victim.wasHeld() {
				anyHeld = true
			}
		}
		if anyHeld {
			// A local owner was preempted and has released; this delivery
			// acquires through the normal lock wait — never by force.
			if !c.isPending(work.scope, work.event) {
				return
			}
			c.logger.Info("chat lock preempted", "adapter", work.event.Adapter, "event_id", work.event.ID, "thread_id", work.event.ThreadID, "forced", false)
			c.safeEvent(ctx, ObsLockPreempted, AdapterAttr(work.event.Adapter))
			return
		}
		// Only non-holding acquirers came and went; look again for a holder
		// before concluding the conflicting lease is remote.
	}
	// No local reservation: re-validate ownership, then force-invalidate the
	// remote or orphaned lease.
	if !c.isPending(work.scope, work.event) {
		return
	}
	forcer, ok := c.state.(LockForcer)
	assert(ok, "preempt requires a LockForcer state (validated at construction)")
	// The force release stays bounded by the Detached Work Context so a stalled
	// state backend cannot park this tail past DetachTimeout.
	released, err := forcer.ForceReleaseLock(ctx, work.scope)
	if err != nil {
		// The lock wait that follows still polls; a stale lease expires at TTL.
		c.logger.Error("chat force release thread lock failed", "error", err, "adapter", work.event.Adapter, "event_id", work.event.ID, "thread_id", work.event.ThreadID)
		return
	}
	c.logger.Info("chat lock preempted", "adapter", work.event.Adapter, "event_id", work.event.ID, "thread_id", work.event.ThreadID, "forced", true, "released", released)
	c.safeEvent(ctx, ObsLockPreempted, AdapterAttr(work.event.Adapter))
}

// inflightCancel is one local lease ownership reservation for a scope,
// registered BEFORE AcquireLock is attempted (preemptible runtimes only) so a
// lease can never exist locally without a visible reservation: a preemptor can
// never mistake a freshly acquired (or still-acquiring) local lease for a
// remote one. held records whether the reservation's acquisition succeeded.
// done closes once the reservation is retired (its lease, if any, released),
// so a preemptor can wait for local completion.
type inflightCancel struct {
	mu sync.Mutex
	// cancel is nil until the handler starts running (arm); a reservation
	// preempted before arming prevents the handler from starting at all.
	cancel    context.CancelCauseFunc
	preempted bool
	held      bool
	done      chan struct{}
}

// markHeld records that the reservation's AcquireLock succeeded.
func (e *inflightCancel) markHeld() {
	e.mu.Lock()
	e.held = true
	e.mu.Unlock()
}

// wasHeld reports whether the reservation ever held the lease; stable once
// done has closed.
func (e *inflightCancel) wasHeld() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.held
}

// preempt marks the reservation preempted, cancelling the running handler when
// one is armed.
func (e *inflightCancel) preempt() {
	e.mu.Lock()
	e.preempted = true
	cancel := e.cancel
	e.mu.Unlock()
	if cancel != nil {
		cancel(ErrPreempted)
	}
}

// arm installs the running handler's cancel. It reports false when the
// reservation was preempted before the handler started; the handler must not
// run.
func (e *inflightCancel) arm(cancel context.CancelCauseFunc) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.preempted {
		return false
	}
	e.cancel = cancel
	return true
}

// reserveInflight registers a local ownership reservation for the scope. It
// must be called BEFORE every AcquireLock attempt on a preemptible runtime
// (so acquisition and registration cannot be reordered by scheduling), marked
// held on success, and retired (retireInflight) once the reservation ends —
// after the lease release when one was held.
func (c *Chat) reserveInflight(scope string) *inflightCancel {
	entry := &inflightCancel{done: make(chan struct{})}
	c.inflightMu.Lock()
	set := c.inflightCancels[scope]
	if set == nil {
		set = map[*inflightCancel]struct{}{}
		c.inflightCancels[scope] = set
	}
	set[entry] = struct{}{}
	c.inflightMu.Unlock()
	return entry
}

// retireInflight removes the reservation and signals waiting preemptors. Call
// it only after the reserved lease (if any) has been released.
func (c *Chat) retireInflight(scope string, entry *inflightCancel) {
	c.inflightMu.Lock()
	if set := c.inflightCancels[scope]; set != nil {
		delete(set, entry)
		if len(set) == 0 {
			delete(c.inflightCancels, scope)
		}
	}
	c.inflightMu.Unlock()
	close(entry.done)
}

// preemptLocalIfPending atomically re-validates that event still owns the
// scope's pending slot and, only then, preempts every current local
// reservation (holders and in-flight acquirers alike), returning the preempted
// entries so the preemptor can wait for their completion. The pending registry
// lock is held across the marking so a newer registration cannot interleave
// between validation and cancellation. Lock order is queueMu > inflightMu >
// entry.mu, nested nowhere else in reverse.
func (c *Chat) preemptLocalIfPending(scope string, event *Event) (victims []*inflightCancel, stillPending bool) {
	c.queueMu.Lock()
	defer c.queueMu.Unlock()
	waiter := c.pending[scope]
	if waiter == nil || waiter.event != event {
		return nil, false
	}
	c.inflightMu.Lock()
	set := c.inflightCancels[scope]
	victims = make([]*inflightCancel, 0, len(set))
	for entry := range set {
		victims = append(victims, entry)
	}
	c.inflightMu.Unlock()
	for _, entry := range victims {
		entry.preempt()
	}
	return victims, true
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
// the full interval.
type pendingWaiter struct {
	event     *Event
	displaced chan struct{}
}

// registerPending records event as the most-recent pending event for its scope,
// superseding (and signalling) any earlier waiter. Recording supersession here,
// where the displacing event's id is known, lets the record carry
// superseded_by. label names the coordination mode ("queue", "debounce", or
// "preempt") so the observation names the strategy that displaced the waiter.
// The returned channel closes when this registration is itself superseded.
func (c *Chat) registerPending(scope string, event *Event, label string) <-chan struct{} {
	c.queueMu.Lock()
	defer c.queueMu.Unlock()
	if prev := c.pending[scope]; prev != nil {
		c.logger.Info("chat "+label+" superseded", "adapter", prev.event.Adapter, "event_id", prev.event.ID, "thread_id", prev.event.ThreadID, "superseded_by", event.ID)
		close(prev.displaced)
	}
	waiter := &pendingWaiter{event: event, displaced: make(chan struct{})}
	c.pending[scope] = waiter
	return waiter.displaced
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
func (c *Chat) queueForLock(ctx context.Context, scope string, event *Event, label string) (LockLease, *inflightCancel, acquireOutcome) {
	// The wait itself is reserved (preemptible runtimes) BEFORE any acquire
	// attempt, so a lease can never exist locally without a visible
	// reservation. A reservation preempted while still waiting is displaced by
	// the preemptor's pending registration and exits superseded.
	var reservation *inflightCancel
	if c.options.OnLockConflict != nil {
		reservation = c.reserveInflight(scope)
	}
	retire := func() {
		if reservation != nil {
			c.retireInflight(scope, reservation)
			reservation = nil
		}
	}
	superseded := func() bool {
		return !c.isPending(scope, event)
	}
	lease, outcome, err := c.pollForLock(ctx, scope, superseded)
	switch outcome {
	case acquireHeld:
		if reservation != nil {
			reservation.markHeld()
		}
		// Re-check ownership after the acquire: a newer event registering while
		// AcquireLock was in flight must win, or a superseded debounce/queue
		// waiter would dispatch alongside the final one. Once the holder passes
		// this check it is committed; later arrivals wait on the lock.
		if !c.isPending(scope, event) {
			c.releaseLock(ctx, lease, event.ThreadID)
			retire()
			c.logger.Debug("chat "+label+" waiter superseded", "adapter", event.Adapter, "event_id", event.ID, "thread_id", event.ThreadID)
			return LockLease{}, nil, acquireSuperseded
		}
		c.clearPending(scope, event)
	case acquireSuperseded:
		// registerPending already emitted the superseded_by record; this waiter
		// just stops.
		retire()
		c.logger.Debug("chat "+label+" waiter superseded", "adapter", event.Adapter, "event_id", event.ID, "thread_id", event.ThreadID)
	case acquireAbandoned:
		retire()
		c.clearPending(scope, event)
		c.logger.Info("chat "+label+" wait abandoned", "adapter", event.Adapter, "event_id", event.ID, "thread_id", event.ThreadID, "error", ctx.Err())
	case acquireFailed:
		retire()
		c.clearPending(scope, event)
		c.logger.Error("chat "+label+" acquire lock failed", "error", err, "adapter", event.Adapter, "event_id", event.ID, "thread_id", event.ThreadID)
	}
	return lease, reservation, outcome
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

// burstScope is one scope's burst collection state: the currently open window
// (if any) and whether a runner tail owns the scope. runnerActive stays true
// across a runner handoff so arrivals never double-start a runner, and a window
// opened while a batch dispatches is handed to a fresh successor runner so
// windows never race each other for the Thread Lock.
type burstScope struct {
	open         bool
	openedAt     time.Time
	batch        []preludeWork
	runnerActive bool
}

// joinBurstBatch appends the routed work to its scope's collection window,
// opening a new window when none is open. It reports whether the caller must
// start the scope's runner tail (no runner is active for the scope).
func (c *Chat) joinBurstBatch(scope string, work preludeWork) (startRunner bool) {
	c.burstMu.Lock()
	defer c.burstMu.Unlock()
	b := c.burstScopes[scope]
	if b == nil {
		b = &burstScope{}
		c.burstScopes[scope] = b
	}
	if !b.open {
		b.open = true
		b.openedAt = time.Now()
	}
	b.batch = append(b.batch, work)
	if b.runnerActive {
		return false
	}
	b.runnerActive = true
	return true
}

// runBurstScope is the detached burst runner for one collection window: it
// sleeps until the window closes, takes the batch, acquires the scope's Thread
// Lock once, and runs every collected handler in join order under the single
// hold. It owns every joined work's span.
func (c *Chat) runBurstScope(ctx context.Context, scope string) {
	c.burstMu.Lock()
	b := c.burstScopes[scope]
	assert(b != nil && b.open, "burst runner started without an open window")
	deadline := b.openedAt.Add(c.options.DebounceInterval)
	c.burstMu.Unlock()

	timer := time.NewTimer(time.Until(deadline))
	defer timer.Stop()
	select {
	case <-ctx.Done():
		c.abandonBurstWindow(ctx, scope)
		return
	case <-timer.C:
	}

	c.burstMu.Lock()
	batch := b.batch
	b.batch = nil
	b.open = false
	c.burstMu.Unlock()
	assert(len(batch) > 0, "burst window closed with no collected events")

	// The lock wait gets a fresh DetachTimeout starting when the window closes,
	// so time spent collecting can never consume it. Derived from baseCtx so
	// Shutdown still cancels it.
	waitCtx, waitCancel := context.WithTimeout(c.baseCtx, c.options.DetachTimeout)
	defer waitCancel()

	first := batch[0].event
	lease, outcome, err := c.pollForLock(waitCtx, scope, nil)
	if outcome != acquireHeld {
		if outcome == acquireFailed {
			c.logger.Error("chat burst acquire lock failed", "error", err, "adapter", first.Adapter, "thread_id", first.ThreadID, "size", len(batch))
		} else {
			c.logger.Info("chat burst wait abandoned", "adapter", first.Adapter, "thread_id", first.ThreadID, "size", len(batch), "error", waitCtx.Err())
		}
		for _, work := range batch {
			c.safeEnd(work.span, waitOutcome(outcome), RouteAttr(work.route))
		}
		c.finishBurstScope(scope)
		return
	}

	// Every accepted batch member gets its own DetachTimeout execution budget,
	// so a member is never skipped merely because earlier members consumed a
	// shared deadline; the batch as a whole is bounded by Shutdown (batchCtx)
	// and by lease loss, which cancels the remaining members.
	batchCtx, batchCancel := context.WithCancelCause(c.baseCtx)
	defer batchCancel(nil)

	c.logger.Info("chat burst batch dispatch", "adapter", first.Adapter, "thread_id", first.ThreadID, "size", len(batch))
	stopRefresh, leaseLost := c.startLockRefresh(batchCtx, lease, first.ThreadID, batchCancel)
	for i, work := range batch {
		if batchCtx.Err() != nil {
			c.logger.Info("chat burst batch abandoned", "adapter", first.Adapter, "thread_id", first.ThreadID, "remaining", len(batch)-i, "error", context.Cause(batchCtx))
			for _, rest := range batch[i:] {
				c.safeEnd(rest.span, OutcomeIgnored, RouteAttr(rest.route))
			}
			break
		}
		memberCtx, memberCancel := context.WithTimeout(batchCtx, c.options.DetachTimeout)
		c.logger.Info("chat deferred dispatch started", "adapter", work.event.Adapter, "event_id", work.event.ID, "route", work.route)
		memberErr := work.run(memberCtx)
		// Like runLockedTail, the preemption outcome follows the cancellation
		// cause: a member stopped by lease loss is preempted, whatever it
		// returned.
		if errors.Is(context.Cause(memberCtx), ErrPreempted) {
			c.logger.Info("chat handler preempted", "adapter", work.event.Adapter, "event_id", work.event.ID, "thread_id", work.event.ThreadID, "route", work.route, "started", true)
			c.safeEnd(work.span, OutcomePreempted, RouteAttr(work.route))
		} else {
			c.endHandlerRun(memberCtx, work.event, work.route, work.span, memberErr)
		}
		memberCancel()
	}
	stopRefresh()
	benignRelease := batchCtx.Err() != nil || leaseLost()
	c.releaseTailLock(batchCtx, lease, first.ThreadID, benignRelease)
	c.finishBurstScope(scope)
}

// abandonBurstWindow surfaces an abandoned (cancelled or timed-out) collection
// window: every collected work's span closes as ignored, and the scope is
// finished so a window opened afterwards still gets a runner.
func (c *Chat) abandonBurstWindow(ctx context.Context, scope string) {
	c.burstMu.Lock()
	b := c.burstScopes[scope]
	batch := b.batch
	b.batch = nil
	b.open = false
	c.burstMu.Unlock()
	if len(batch) > 0 {
		first := batch[0].event
		c.logger.Info("chat burst wait abandoned", "adapter", first.Adapter, "thread_id", first.ThreadID, "size", len(batch), "error", ctx.Err())
	}
	for _, work := range batch {
		c.safeEnd(work.span, OutcomeIgnored, RouteAttr(work.route))
	}
	c.finishBurstScope(scope)
}

// finishBurstScope retires the scope's runner: a window opened while the batch
// dispatched is handed to a fresh successor runner (with its own DetachTimeout);
// otherwise the scope goes idle. runnerActive stays true across the handoff so
// an arrival can never double-start a runner, and the successor is started
// before this runner's inflight slot is released so Shutdown still drains it.
func (c *Chat) finishBurstScope(scope string) {
	c.burstMu.Lock()
	b := c.burstScopes[scope]
	if b.open {
		c.burstMu.Unlock()
		c.startDetachedTail(preludeWork{scope: scope, burstRunner: true})
		return
	}
	b.runnerActive = false
	delete(c.burstScopes, scope)
	c.burstMu.Unlock()
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
