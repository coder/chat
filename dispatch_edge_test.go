package chat_test

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/coder/chat"
)

// orderingAdapter records, via a monotonic sequence counter shared with the
// handler, when its ack fires relative to the routed handler doing its work.
type orderingAdapter struct {
	*fakeAdapter

	mu      sync.Mutex
	seq     int
	ackSeq  int
	workSeq int
}

func newOrderingAdapter(name string) *orderingAdapter {
	return &orderingAdapter{fakeAdapter: newFakeAdapter(name)}
}

// nextSeq returns a strictly increasing tick so ack and handler-work order is
// observable without wall-clock flakiness.
func (a *orderingAdapter) nextSeq() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.seq++
	return a.seq
}

// markWork records the sequence tick at which the handler performed its work.
func (a *orderingAdapter) markWork() {
	s := a.nextSeq()
	a.mu.Lock()
	defer a.mu.Unlock()
	a.workSeq = s
}

func (a *orderingAdapter) ackPoint() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.ackSeq
}

func (a *orderingAdapter) workPoint() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.workSeq
}

func (a *orderingAdapter) Webhook(dispatch chat.DispatchFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var ev chat.Event
		if err := json.NewDecoder(r.Body).Decode(&ev); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if err := dispatch(r.Context(), &ev); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		// Ack is written here, after dispatch returns; capture the tick.
		s := a.nextSeq()
		a.mu.Lock()
		a.ackSeq = s
		a.mu.Unlock()
		w.WriteHeader(http.StatusOK)
	})
}

func TestDeferredDispatchAckPrecedesHandlerWorkWithRecordingAdapter(t *testing.T) {
	t.Parallel()

	state := newFakeState()
	adapter := newOrderingAdapter("fake")
	bot, err := chat.New(context.Background(),
		chat.WithState(state),
		chat.WithAdapter(adapter),
		chat.WithLogger(slog.New(slog.NewTextHandler(newSyncBuffer(), nil))),
		chat.WithRuntimeOptions(chat.RuntimeOptions{
			DedupeTTL:     time.Hour,
			ThreadLockTTL: time.Hour,
			Concurrency:   chat.ConcurrencyDrop,
			Dispatch:      chat.DispatchDeferred,
			DetachTimeout: 5 * time.Second,
		}),
	)
	if err != nil {
		t.Fatalf("new runtime: %v", err)
	}

	releaseHandler := make(chan struct{})
	workDone := make(chan struct{})
	bot.OnNewMention(func(ctx context.Context, ev *chat.MessageEvent) error {
		// Block until the test confirms the ack already fired, then do work.
		<-releaseHandler
		adapter.markWork()
		close(workDone)
		return nil
	})

	if status := postEvent(t, bot, "fake", mentionEvent("ack-order", "fake:v1:thread-1")); status != http.StatusOK {
		t.Fatalf("status = %d", status)
	}

	// The ack point must already be recorded: dispatch returned before the
	// handler (still blocked) ran its work.
	if ack := adapter.ackPoint(); ack == 0 {
		t.Fatal("ack did not fire before handler work")
	}
	if work := adapter.workPoint(); work != 0 {
		t.Fatalf("handler did work (tick %d) before ack returned", work)
	}

	close(releaseHandler)
	select {
	case <-workDone:
	case <-time.After(time.Second):
		t.Fatal("handler work did not complete")
	}

	ack := adapter.ackPoint()
	work := adapter.workPoint()
	if !(ack < work) {
		t.Fatalf("ack tick %d should precede handler work tick %d", ack, work)
	}
}

func TestSyncDispatchAckFollowsHandlerWorkWithRecordingAdapter(t *testing.T) {
	t.Parallel()

	state := newFakeState()
	adapter := newOrderingAdapter("fake")
	// No Dispatch/DetachTimeout set: this is the zero-value DispatchSync default.
	bot, err := chat.New(context.Background(),
		chat.WithState(state),
		chat.WithAdapter(adapter),
		chat.WithLogger(slog.New(slog.NewTextHandler(newSyncBuffer(), nil))),
		chat.WithRuntimeOptions(chat.RuntimeOptions{
			DedupeTTL:     time.Hour,
			ThreadLockTTL: time.Hour,
		}),
	)
	if err != nil {
		t.Fatalf("new runtime: %v", err)
	}

	bot.OnNewMention(func(ctx context.Context, ev *chat.MessageEvent) error {
		adapter.markWork()
		return nil
	})

	if status := postEvent(t, bot, "fake", mentionEvent("sync-order", "fake:v1:thread-1")); status != http.StatusOK {
		t.Fatalf("status = %d", status)
	}

	ack := adapter.ackPoint()
	work := adapter.workPoint()
	if work == 0 {
		t.Fatal("handler did not run under sync dispatch")
	}
	if !(work < ack) {
		t.Fatalf("under sync dispatch handler work tick %d must precede ack tick %d", work, ack)
	}
}

func TestSyncDispatchReleasesLockOnHandlerExitRegression(t *testing.T) {
	t.Parallel()

	state := newFakeState()
	adapter := newFakeAdapter("fake")
	bot, err := chat.New(context.Background(),
		chat.WithState(state),
		chat.WithAdapter(adapter),
		chat.WithRuntimeOptions(chat.RuntimeOptions{
			DedupeTTL:     time.Hour,
			ThreadLockTTL: time.Hour,
		}),
	)
	if err != nil {
		t.Fatalf("new runtime: %v", err)
	}

	var calls int
	bot.OnNewMention(func(ctx context.Context, ev *chat.MessageEvent) error {
		calls++
		// While the handler runs synchronously the lock must be held: an outside
		// acquire for the same thread must fail.
		if _, acquired, err := state.AcquireLock(context.Background(), string(ev.Event.ThreadID), time.Hour); err != nil || acquired {
			t.Errorf("lock not held during sync handler: acquired=%v err=%v", acquired, err)
		}
		return nil
	})

	// Two sequential deliveries to the same thread (distinct event IDs). Because
	// sync dispatch releases the lock on handler exit, both run.
	if status := postEvent(t, bot, "fake", mentionEvent("sync-1", "fake:v1:thread-seq")); status != http.StatusOK {
		t.Fatalf("first status = %d", status)
	}
	if status := postEvent(t, bot, "fake", mentionEvent("sync-2", "fake:v1:thread-seq")); status != http.StatusOK {
		t.Fatalf("second status = %d", status)
	}
	if calls != 2 {
		t.Fatalf("calls = %d, want 2 (lock released on each handler exit)", calls)
	}

	// After both handlers exited the lock is free for an outside acquire.
	if _, acquired, err := state.AcquireLock(context.Background(), "fake:v1:thread-seq", time.Hour); err != nil || !acquired {
		t.Fatalf("lock should be free after sync handlers exit: acquired=%v err=%v", acquired, err)
	}
}

func TestDeferredDispatchDuplicateEventAcksWithoutDetachedWork(t *testing.T) {
	t.Parallel()

	state := newFakeState()
	adapter := newFakeAdapter("fake")
	bot := newDeferredRuntime(t, state, adapter)

	var mu sync.Mutex
	var calls int
	handlerStarted := make(chan struct{}, 4)
	releaseHandler := make(chan struct{})
	bot.OnNewMention(func(ctx context.Context, ev *chat.MessageEvent) error {
		mu.Lock()
		calls++
		mu.Unlock()
		handlerStarted <- struct{}{}
		<-releaseHandler
		return nil
	})

	event := mentionEvent("dup-event", "fake:v1:thread-dup")

	// First delivery: routed, detached handler starts and holds.
	if status := postEvent(t, bot, "fake", event); status != http.StatusOK {
		t.Fatalf("first status = %d", status)
	}
	select {
	case <-handlerStarted:
	case <-time.After(time.Second):
		t.Fatal("first handler did not start")
	}

	// Duplicate delivery (same event ID) on a DIFFERENT thread so it cannot be a
	// lock conflict — it must be resolved as a duplicate in the prelude (already
	// marked) and acknowledged with no new detached work.
	dup := event
	dup.ThreadID = "fake:v1:thread-dup-other"
	if status := postEvent(t, bot, "fake", dup); status != http.StatusOK {
		t.Fatalf("duplicate status = %d", status)
	}

	// No second handler should have started.
	select {
	case <-handlerStarted:
		t.Fatal("duplicate event started a second detached handler")
	case <-time.After(50 * time.Millisecond):
	}

	close(releaseHandler)
	mu.Lock()
	defer mu.Unlock()
	if calls != 1 {
		t.Fatalf("calls = %d, want 1 (duplicate must not run a handler)", calls)
	}
}

func TestExtendAndReleaseEnforceOwnershipToken(t *testing.T) {
	t.Parallel()

	state := newFakeState()

	lease, acquired, err := state.AcquireLock(context.Background(), "fake:v1:thread-token", time.Hour)
	if err != nil || !acquired {
		t.Fatalf("seed acquire acquired=%v err=%v", acquired, err)
	}

	stale := chat.LockLease{Key: lease.Key, Token: lease.Token + "-stale"}

	// A stale token cannot extend the held lease.
	if extended, err := state.ExtendLock(context.Background(), stale, time.Hour); err != nil || extended {
		t.Fatalf("stale token extended lease: extended=%v err=%v", extended, err)
	}
	// The genuine token extends it.
	if extended, err := state.ExtendLock(context.Background(), lease, time.Hour); err != nil || !extended {
		t.Fatalf("genuine token failed to extend: extended=%v err=%v", extended, err)
	}

	// A stale token cannot release the held lease (the lock stays held).
	if released, err := state.ReleaseLock(context.Background(), stale); err != nil || released {
		t.Fatalf("stale token released lease: released=%v err=%v", released, err)
	}
	if extended, err := state.ExtendLock(context.Background(), lease, time.Hour); err != nil || !extended {
		t.Fatalf("lease should still be held after stale release attempt: extended=%v err=%v", extended, err)
	}

	// The genuine token releases it.
	if released, err := state.ReleaseLock(context.Background(), lease); err != nil || !released {
		t.Fatalf("genuine token failed to release: released=%v err=%v", released, err)
	}
	// Once released the genuine token can no longer extend.
	if extended, err := state.ExtendLock(context.Background(), lease, time.Hour); err != nil || extended {
		t.Fatalf("extended a released lease: extended=%v err=%v", extended, err)
	}
}

func TestDeferredDispatchTokenSurvivesStaleExtendDuringTail(t *testing.T) {
	t.Parallel()

	state := newFakeState()
	adapter := newFakeAdapter("fake")
	bot := newDeferredRuntime(t, state, adapter, func(o *chat.RuntimeOptions) {
		o.ThreadLockTTL = 40 * time.Millisecond
		o.DetachTimeout = 5 * time.Second
	})

	handlerStarted := make(chan struct{})
	releaseHandler := make(chan struct{})
	bot.OnNewMention(func(ctx context.Context, ev *chat.MessageEvent) error {
		close(handlerStarted)
		<-releaseHandler
		return nil
	})

	if status := postEvent(t, bot, "fake", mentionEvent("token-tail", "fake:v1:thread-token-tail")); status != http.StatusOK {
		t.Fatalf("status = %d", status)
	}
	select {
	case <-handlerStarted:
	case <-time.After(time.Second):
		t.Fatal("handler did not start")
	}

	// Across more than a TTL, a holder presenting a guessed/stale token can
	// neither extend nor release the runtime-held lease, and an outside acquire
	// still conflicts because the runtime refresh keeps the lease alive.
	stale := chat.LockLease{Key: "fake:v1:thread-token-tail", Token: "fake:v1:thread-token-tail-token-stale"}
	deadline := time.Now().Add(120 * time.Millisecond)
	for time.Now().Before(deadline) {
		if extended, err := state.ExtendLock(context.Background(), stale, time.Hour); err != nil || extended {
			t.Fatalf("stale token extended runtime lease mid-tail: extended=%v err=%v", extended, err)
		}
		if released, err := state.ReleaseLock(context.Background(), stale); err != nil || released {
			t.Fatalf("stale token released runtime lease mid-tail: released=%v err=%v", released, err)
		}
		if _, acquired, err := state.AcquireLock(context.Background(), "fake:v1:thread-token-tail", time.Hour); err != nil || acquired {
			t.Fatalf("outside acquire succeeded mid-tail (lease not refreshed): acquired=%v err=%v", acquired, err)
		}
		time.Sleep(15 * time.Millisecond)
	}

	close(releaseHandler)

	// After the tail exits, the runtime's genuine token released the lease, so an
	// outside acquire now succeeds.
	released := false
	for i := 0; i < 100 && !released; i++ {
		if _, acquired, err := state.AcquireLock(context.Background(), "fake:v1:thread-token-tail", time.Hour); err == nil && acquired {
			released = true
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if !released {
		t.Fatal("runtime did not release the lease after the tail exited")
	}
}

func TestDeferredWorkContextIsNotBackground(t *testing.T) {
	t.Parallel()

	state := newFakeState()
	adapter := newFakeAdapter("fake")
	bot := newDeferredRuntime(t, state, adapter, func(o *chat.RuntimeOptions) {
		o.DetachTimeout = 2 * time.Second
	})

	type ctxProbe struct {
		hasDeadline  bool
		isBackground bool
	}
	probed := make(chan ctxProbe, 1)
	release := make(chan struct{})
	bot.OnNewMention(func(ctx context.Context, ev *chat.MessageEvent) error {
		_, hasDeadline := ctx.Deadline()
		probed <- ctxProbe{
			hasDeadline:  hasDeadline,
			isBackground: ctx == context.Background(),
		}
		<-release
		return nil
	})

	if status := postEvent(t, bot, "fake", mentionEvent("ctx-probe", "fake:v1:thread-ctx")); status != http.StatusOK {
		t.Fatalf("status = %d", status)
	}

	var p ctxProbe
	select {
	case p = <-probed:
	case <-time.After(time.Second):
		t.Fatal("handler did not run")
	}
	if !p.hasDeadline {
		t.Fatal("detached work context has no deadline; it must be bounded by DetachTimeout, not context.Background()")
	}
	if p.isBackground {
		t.Fatal("detached work context is context.Background(); it must be the runtime-derived bounded context")
	}

	close(release)
}

func TestDeferredWorkContextCancelledOnShutdownNotJustTimeout(t *testing.T) {
	t.Parallel()

	state := newFakeState()
	adapter := newFakeAdapter("fake")
	bot := newDeferredRuntime(t, state, adapter, func(o *chat.RuntimeOptions) {
		// DetachTimeout far longer than the test: only Shutdown can cancel in time.
		o.DetachTimeout = time.Hour
	})

	handlerStarted := make(chan struct{})
	observedCancel := make(chan error, 1)
	bot.OnNewMention(func(ctx context.Context, ev *chat.MessageEvent) error {
		close(handlerStarted)
		<-ctx.Done()
		observedCancel <- ctx.Err()
		return ctx.Err()
	})

	if status := postEvent(t, bot, "fake", mentionEvent("shutdown-cancel", "fake:v1:thread-1")); status != http.StatusOK {
		t.Fatalf("status = %d", status)
	}
	select {
	case <-handlerStarted:
	case <-time.After(time.Second):
		t.Fatal("handler did not start")
	}

	shutdownDone := make(chan error, 1)
	go func() { shutdownDone <- bot.Shutdown(context.Background()) }()

	select {
	case err := <-observedCancel:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("handler observed %v at shutdown, want context.Canceled (not a deadline)", err)
		}
	case <-time.After(time.Second):
		t.Fatal("shutdown did not cancel the detached work context before its hour-long timeout")
	}

	select {
	case err := <-shutdownDone:
		if err != nil {
			t.Fatalf("shutdown error: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("shutdown did not finish")
	}
}

func TestDeferredShutdownDrainsThenRunsStateShutdown(t *testing.T) {
	t.Parallel()

	state := newOrderingState()
	adapter := newFakeAdapter("fake")
	bot := newDeferredRuntime(t, state, adapter)

	handlerStarted := make(chan struct{})
	const cleanup = 80 * time.Millisecond
	bot.OnNewMention(func(ctx context.Context, ev *chat.MessageEvent) error {
		close(handlerStarted)
		<-ctx.Done()
		// Linger so the drain must wait; record that state shutdown has NOT run.
		time.Sleep(cleanup)
		if state.shutdownCalled() {
			t.Error("state shutdown ran before the in-flight detached tail finished")
		}
		state.markHandlerDone()
		return ctx.Err()
	})

	if status := postEvent(t, bot, "fake", mentionEvent("drain-order", "fake:v1:thread-1")); status != http.StatusOK {
		t.Fatalf("status = %d", status)
	}
	select {
	case <-handlerStarted:
	case <-time.After(time.Second):
		t.Fatal("handler did not start")
	}

	if err := bot.Shutdown(context.Background()); err != nil {
		t.Fatalf("shutdown error: %v", err)
	}

	if !state.handlerDoneBeforeShutdown() {
		t.Fatal("state shutdown did not observe the drained handler ordering")
	}
	if state.shutdowns != 1 {
		t.Fatalf("state shutdowns = %d, want 1", state.shutdowns)
	}
}

func TestDeferredShutdownAlreadyCancelledCtxReturnsCtxErr(t *testing.T) {
	t.Parallel()

	state := newFakeState()
	adapter := newFakeAdapter("fake")
	bot := newDeferredRuntime(t, state, adapter)

	handlerStarted := make(chan struct{})
	release := make(chan struct{})
	bot.OnNewMention(func(ctx context.Context, ev *chat.MessageEvent) error {
		close(handlerStarted)
		<-release // ignore cancellation so the bounded drain must time out
		return nil
	})
	defer close(release)

	if status := postEvent(t, bot, "fake", mentionEvent("cancelled-shutdown", "fake:v1:thread-1")); status != http.StatusOK {
		t.Fatalf("status = %d", status)
	}
	select {
	case <-handlerStarted:
	case <-time.After(time.Second):
		t.Fatal("handler did not start")
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := bot.Shutdown(ctx)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("shutdown with cancelled ctx error = %v, want context.Canceled", err)
	}
}

// orderingState embeds fakeState and records whether the detached handler
// finished before State.Shutdown ran.
type orderingState struct {
	*fakeState

	orderMu     sync.Mutex
	shutdownAt  bool
	handlerDone bool
	orderOK     bool
}

func newOrderingState() *orderingState {
	return &orderingState{fakeState: newFakeState()}
}

func (s *orderingState) shutdownCalled() bool {
	s.orderMu.Lock()
	defer s.orderMu.Unlock()
	return s.shutdownAt
}

func (s *orderingState) markHandlerDone() {
	s.orderMu.Lock()
	defer s.orderMu.Unlock()
	s.handlerDone = true
}

func (s *orderingState) handlerDoneBeforeShutdown() bool {
	s.orderMu.Lock()
	defer s.orderMu.Unlock()
	return s.orderOK
}

func (s *orderingState) Shutdown(ctx context.Context) error {
	s.orderMu.Lock()
	s.shutdownAt = true
	// The drain must have finished the handler before state shutdown runs.
	s.orderOK = s.handlerDone
	s.orderMu.Unlock()
	return s.fakeState.Shutdown(ctx)
}

var (
	_ chat.Adapter = (*orderingAdapter)(nil)
	_ chat.State   = (*orderingState)(nil)
)
