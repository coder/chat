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
}

func DefaultRuntimeOptions() RuntimeOptions {
	return RuntimeOptions{
		DedupeTTL:     24 * time.Hour,
		ThreadLockTTL: 2 * time.Minute,
		Concurrency:   ConcurrencyDrop,
		Dispatch:      DispatchSync,
		DetachTimeout: 0,
	}
}

type Option func(*config)

type config struct {
	state    State
	adapters []Adapter
	logger   *slog.Logger
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
	options  RuntimeOptions

	handlersMu        sync.RWMutex
	newMention        MessageHandler
	subscribedMessage MessageHandler
	acceptancesMu     sync.Mutex
	eventAcceptances  map[string]*eventAcceptance

	// baseCtx is the long-lived base for the detached work context: not a request
	// context, not context.Background(). Cancelled by Shutdown.
	baseCtx    context.Context
	baseCancel context.CancelFunc
	// inflight tracks detached tails so Shutdown drains them before state shutdown.
	inflight sync.WaitGroup

	// queueMu guards pending, the per-scope most-recent queued event.
	queueMu sync.Mutex
	pending map[string]*Event

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
		logger:  slog.Default(),
		options: DefaultRuntimeOptions(),
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
	if err := validateRuntimeOptions(cfg.options); err != nil {
		return nil, err
	}

	baseCtx, baseCancel := context.WithCancel(context.Background())
	chat := &Chat{
		state:            cfg.state,
		adapters:         map[string]Adapter{},
		logger:           cfg.logger,
		options:          cfg.options,
		eventAcceptances: map[string]*eventAcceptance{},
		baseCtx:          baseCtx,
		baseCancel:       baseCancel,
		pending:          map[string]*Event{},
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
	default:
		return errors.New("chat: unsupported concurrency strategy")
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
// handler before the routed handler runs.
func (c *Chat) dispatchSync(ctx context.Context, event *Event) error {
	work, resolved, err := c.prelude(ctx, event)
	if err != nil || resolved {
		return err
	}
	if work.needsLock {
		lease, outcome := c.queueForLock(ctx, work.scope, event)
		if outcome != acquireHeld {
			return nil
		}
		work.lease = lease
	}
	defer c.releaseLock(ctx, work.lease, event.ThreadID)
	if err := work.handler(ctx, work.msgEvent); err != nil {
		c.logger.Error("chat handler failed", "error", err, "adapter", event.Adapter, "event_id", event.ID, "route", work.route)
	}
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
	event   *Event
	lease   LockLease
	handler MessageHandler
	route   string
	scope   string
	// needsLock is true when the prelude registered the event as pending under the
	// queue strategy without holding the lock; the tail must wait for and acquire
	// it before running the handler.
	needsLock bool
	msgEvent  *MessageEvent
}

// prelude runs the synchronous portion of dispatch before ack: validate, dedupe,
// acquire the Thread Lock, validate the thread id, filter nil/self messages, and
// route. A resolved event (duplicate, dropped Lock Conflict, ignored, unrouted)
// returns resolved=true with no work. A failed prelude returns the error and,
// as today, leaves the event un-marked so a retry is not deduped away.
//
// Under the queue strategy a Lock Conflict on a routed event does not block: the
// event is registered as pending (no lease) and returned with needsLock=true so
// the tail acquires the lock, keeping ack prompt under DispatchDeferred.
func (c *Chat) prelude(ctx context.Context, event *Event) (preludeWork, bool, error) {
	if err := validateEvent(event); err != nil {
		return preludeWork{}, true, err
	}
	acceptance, primary := c.beginEventAcceptance(event.ID)
	if !primary {
		return preludeWork{}, true, waitEventAcceptance(ctx, acceptance)
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
		return firstSeen, nil
	}

	adapter, ok := c.adapters[event.Adapter]
	if !ok {
		return preludeWork{}, true, finish(fmt.Errorf("chat: event adapter %q is not registered", event.Adapter))
	}

	scope := string(event.ThreadID)
	lease, acquired, err := c.state.AcquireLock(ctx, scope, c.options.ThreadLockTTL)
	if err != nil {
		return preludeWork{}, true, finish(fmt.Errorf("chat: acquire thread lock: %w", err))
	}
	queued := false
	if !acquired {
		if c.options.Concurrency != ConcurrencyQueue {
			if accepted, err := acceptEvent(); err != nil || !accepted {
				return preludeWork{}, true, err
			}
			c.logger.Info("chat lock conflict dropped", "adapter", event.Adapter, "event_id", event.ID, "thread_id", event.ThreadID)
			return preludeWork{}, true, nil
		}
		queued = true
	}

	// releaseOnResolve releases the lease when the event resolves here; a queued
	// event holds no lease yet.
	releaseOnResolve := func() {
		if queued {
			return
		}
		c.releaseLock(ctx, lease, event.ThreadID)
	}

	ref, err := adapter.ValidateThreadID(event.ThreadID)
	if err != nil {
		releaseOnResolve()
		return preludeWork{}, true, finish(fmt.Errorf("chat: validate event thread id: %w", err))
	}
	thread := c.newThread(adapter, ref)
	if event.Message == nil {
		releaseOnResolve()
		if accepted, err := acceptEvent(); err != nil || !accepted {
			return preludeWork{}, true, err
		}
		c.logger.Info("chat ignored non-message event", "adapter", event.Adapter, "event_id", event.ID)
		return preludeWork{}, true, nil
	}
	if isSelfMessage(event.Message.Author, adapter.BotActor()) {
		releaseOnResolve()
		if accepted, err := acceptEvent(); err != nil || !accepted {
			return preludeWork{}, true, err
		}
		c.logger.Debug("chat ignored self message", "adapter", event.Adapter, "event_id", event.ID)
		return preludeWork{}, true, nil
	}

	handler, route, err := c.route(ctx, event)
	if err != nil {
		releaseOnResolve()
		return preludeWork{}, true, finish(err)
	}
	if handler == nil {
		releaseOnResolve()
		if accepted, err := acceptEvent(); err != nil || !accepted {
			return preludeWork{}, true, err
		}
		c.logger.Info("chat ignored unrouted message", "adapter", event.Adapter, "event_id", event.ID)
		return preludeWork{}, true, nil
	}

	if accepted, err := acceptEvent(); err != nil || !accepted {
		releaseOnResolve()
		return preludeWork{}, true, err
	}

	if queued {
		c.registerPending(scope, event)
	}

	return preludeWork{
		event:     event,
		lease:     lease,
		handler:   handler,
		route:     route,
		scope:     scope,
		needsLock: queued,
		msgEvent: &MessageEvent{
			Event:   event,
			Thread:  thread,
			Message: event.Message,
		},
	}, false, nil
}

// startDetachedTail runs the routed handler on the detached work context after
// ack: the Thread Lock is held across the tail, refreshed via ExtendLock, and
// released on exit. The context is derived from baseCtx, bounded by
// DetachTimeout, and cancelled by Shutdown. When needsLock, the tail first waits
// for the lock under the queue strategy; a superseded or abandoned waiter exits
// without running the handler.
func (c *Chat) startDetachedTail(work preludeWork) {
	tailCtx, tailCancel := context.WithTimeout(c.baseCtx, c.options.DetachTimeout)
	c.inflight.Add(1)
	go func() {
		defer c.inflight.Done()
		defer tailCancel()

		if work.needsLock {
			lease, outcome := c.queueForLock(tailCtx, work.scope, work.event)
			if outcome != acquireHeld {
				return
			}
			work.lease = lease
		}

		c.logger.Info("chat deferred dispatch started", "adapter", work.event.Adapter, "event_id", work.event.ID, "route", work.route)

		stopRefresh, leaseLost := c.startLockRefresh(tailCtx, work.lease, work.event.ThreadID)
		err := work.handler(tailCtx, work.msgEvent)
		stopRefresh()

		if err != nil {
			if ctxErr := tailCtx.Err(); ctxErr != nil {
				c.logger.Error("chat deferred handler timed out", "error", err, "adapter", work.event.Adapter, "event_id", work.event.ID, "route", work.route)
			} else {
				c.logger.Error("chat handler failed", "error", err, "adapter", work.event.Adapter, "event_id", work.event.ID, "route", work.route, "mode", "deferred")
			}
		}

		// A failed release is expected (lease gone) when the context was cancelled
		// or the refresh loop already lost the lease, so downgrade it from WARN.
		benignRelease := tailCtx.Err() != nil || leaseLost()
		c.releaseTailLock(tailCtx, work.lease, work.event.ThreadID, benignRelease)
	}()
}

// startLockRefresh extends the Lock Lease on a cadence below ThreadLockTTL so it
// does not expire while the detached handler runs. Extend runs under
// context.WithoutCancel so a refresh in flight at cancellation still completes.
// stop halts the loop and blocks until it exits; leaseLost reports whether the
// loop saw the lease already gone, and is safe to read once stop has returned.
func (c *Chat) startLockRefresh(ctx context.Context, lease LockLease, threadID ThreadID) (stop func(), leaseLost func() bool) {
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
					c.logger.Error("chat extend thread lock failed", "error", err, "thread_id", threadID)
					return
				}
				if !extended {
					c.logger.Warn("chat thread lock lease lost", "thread_id", threadID)
					lost.Store(true)
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

// registerPending records event as the most-recent pending event for its scope,
// superseding any earlier waiter. Recording supersession here, where the
// displacing event's id is known, lets the record carry superseded_by.
func (c *Chat) registerPending(scope string, event *Event) {
	c.queueMu.Lock()
	defer c.queueMu.Unlock()
	if prev := c.pending[scope]; prev != nil {
		c.logger.Info("chat queue superseded", "adapter", prev.Adapter, "event_id", prev.ID, "thread_id", prev.ThreadID, "superseded_by", event.ID)
	}
	c.pending[scope] = event
}

// queueForLock waits for the Thread Lock on behalf of an event already
// registered as a pending waiter for its scope, polling AcquireLock until the
// in-flight handler releases. It returns acquireSuperseded if a newer follow-up
// displaced this waiter, acquireAbandoned if ctx ended, acquireFailed on an
// AcquireLock error, or the lease (acquireHeld) once acquired.
func (c *Chat) queueForLock(ctx context.Context, scope string, event *Event) (LockLease, acquireOutcome) {
	ticker := time.NewTicker(c.queuePollInterval())
	defer ticker.Stop()
	abandon := func() (LockLease, acquireOutcome) {
		c.clearPending(scope, event)
		c.logger.Info("chat queue wait abandoned", "adapter", event.Adapter, "event_id", event.ID, "thread_id", event.ThreadID, "error", ctx.Err())
		return LockLease{}, acquireAbandoned
	}
	for {
		c.queueMu.Lock()
		superseded := c.pending[scope] != event
		c.queueMu.Unlock()
		if superseded {
			// registerPending already emitted the superseded_by record; this waiter
			// just stops.
			c.logger.Debug("chat queue waiter superseded", "adapter", event.Adapter, "event_id", event.ID, "thread_id", event.ThreadID)
			return LockLease{}, acquireSuperseded
		}

		lease, acquired, err := c.state.AcquireLock(ctx, scope, c.options.ThreadLockTTL)
		if err != nil {
			if ctx.Err() != nil {
				return abandon()
			}
			c.clearPending(scope, event)
			c.logger.Error("chat queue acquire lock failed", "error", err, "adapter", event.Adapter, "event_id", event.ID, "thread_id", event.ThreadID)
			return LockLease{}, acquireFailed
		}
		if acquired {
			c.clearPending(scope, event)
			return lease, acquireHeld
		}

		select {
		case <-ctx.Done():
			return abandon()
		case <-ticker.C:
		}
	}
}

func (c *Chat) clearPending(scope string, event *Event) {
	c.queueMu.Lock()
	defer c.queueMu.Unlock()
	if c.pending[scope] == event {
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
	} else if !released {
		c.logger.Warn("chat thread lock was not released", "thread_id", threadID)
	}
}

// releaseTailLock mirrors releaseLock for the deferred tail but, when benign,
// downgrades the "not released" case from WARN to Debug so a normal drain stays
// quiet.
func (c *Chat) releaseTailLock(ctx context.Context, lease LockLease, threadID ThreadID, benign bool) {
	if released, err := c.state.ReleaseLock(context.WithoutCancel(ctx), lease); err != nil {
		c.logger.Error("chat release thread lock failed", "error", err, "thread_id", threadID)
	} else if !released {
		if benign {
			c.logger.Debug("chat thread lock was not released", "thread_id", threadID)
		} else {
			c.logger.Warn("chat thread lock was not released", "thread_id", threadID)
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

func isSelfMessage(author Actor, bot Actor) bool {
	return author.BotKind == BotBot &&
		author.Adapter == bot.Adapter &&
		author.Tenant == bot.Tenant &&
		author.ID == bot.ID
}
