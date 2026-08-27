package chat_test

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/chat"
)

func newDeferredRuntime(t *testing.T, state chat.State, adapter chat.Adapter, opts ...func(*chat.RuntimeOptions)) *chat.Chat {
	t.Helper()
	options := chat.RuntimeOptions{
		DedupeTTL:     time.Hour,
		ThreadLockTTL: time.Hour,
		Concurrency:   chat.ConcurrencyDrop,
		Dispatch:      chat.DispatchDeferred,
		DetachTimeout: 5 * time.Second,
	}
	for _, opt := range opts {
		opt(&options)
	}
	bot, err := chat.New(context.Background(),
		chat.WithState(state),
		chat.WithAdapter(adapter),
		chat.WithLogger(slog.New(slog.NewTextHandler(newSyncBuffer(), nil))),
		chat.WithRuntimeOptions(options),
	)
	if err != nil {
		t.Fatalf("new deferred runtime: %v", err)
	}
	return bot
}

func mentionEvent(id string, threadID chat.ThreadID) chat.Event {
	return chat.Event{
		ID:       id,
		Adapter:  "fake",
		Tenant:   "tenant",
		ThreadID: threadID,
		Message: &chat.Message{
			ID:        id + "-message",
			Text:      "hello",
			Mentioned: true,
			Author:    chat.Actor{Adapter: "fake", Tenant: "tenant", ID: "user-1", BotKind: chat.BotHuman},
		},
	}
}

func TestDeferredDispatchAcksBeforeHandlerCompletes(t *testing.T) {
	t.Parallel()

	state := newFakeState()
	adapter := newFakeAdapter("fake")
	bot := newDeferredRuntime(t, state, adapter)

	handlerStarted := make(chan struct{})
	releaseHandler := make(chan struct{})
	handlerDone := make(chan struct{})
	bot.OnNewMention(func(ctx context.Context, ev *chat.MessageEvent) error {
		close(handlerStarted)
		<-releaseHandler
		close(handlerDone)
		return nil
	})

	status := postEvent(t, bot, "fake", mentionEvent("event-1", "fake:v1:thread-1"))
	if status != http.StatusOK {
		t.Fatalf("status = %d", status)
	}

	// Ack has returned (dispatch returned) before the handler is released.
	select {
	case <-handlerStarted:
	case <-time.After(time.Second):
		t.Fatal("deferred handler did not start")
	}
	select {
	case <-handlerDone:
		t.Fatal("handler completed before ack returned")
	default:
	}

	close(releaseHandler)
	select {
	case <-handlerDone:
	case <-time.After(time.Second):
		t.Fatal("deferred handler did not finish")
	}
}

func TestDeferredDispatchPreludeResolvedEventsAckWithoutDetachedWork(t *testing.T) {
	t.Parallel()

	state := newFakeState()
	adapter := newFakeAdapter("fake")
	bot := newDeferredRuntime(t, state, adapter)

	var mu sync.Mutex
	var calls int
	bot.OnNewMention(func(ctx context.Context, ev *chat.MessageEvent) error {
		mu.Lock()
		calls++
		mu.Unlock()
		return nil
	})

	// Unrouted message (no mention, not subscribed, not DM): resolved in prelude.
	unrouted := mentionEvent("unrouted", "fake:v1:thread-unrouted")
	unrouted.Message.Mentioned = false
	if status := postEvent(t, bot, "fake", unrouted); status != http.StatusOK {
		t.Fatalf("unrouted status = %d", status)
	}

	// Self message: resolved in prelude.
	self := mentionEvent("self", "fake:v1:thread-self")
	self.Message.Author = adapter.BotActor()
	if status := postEvent(t, bot, "fake", self); status != http.StatusOK {
		t.Fatalf("self status = %d", status)
	}

	// Nil message: resolved in prelude.
	nilMsg := mentionEvent("nil", "fake:v1:thread-nil")
	nilMsg.Message = nil
	if status := postEvent(t, bot, "fake", nilMsg); status != http.StatusOK {
		t.Fatalf("nil-message status = %d", status)
	}

	// Lock conflict (drop): resolved in prelude, acked.
	lease, acquired, err := state.AcquireLock(context.Background(), "fake:v1:thread-conflict", time.Hour)
	if err != nil || !acquired {
		t.Fatalf("acquire conflict lock acquired=%v err=%v", acquired, err)
	}
	if status := postEvent(t, bot, "fake", mentionEvent("conflict", "fake:v1:thread-conflict")); status != http.StatusOK {
		t.Fatalf("conflict status = %d", status)
	}
	if _, err := state.ReleaseLock(context.Background(), lease); err != nil {
		t.Fatalf("release conflict lock: %v", err)
	}

	time.Sleep(50 * time.Millisecond)
	mu.Lock()
	defer mu.Unlock()
	if calls != 0 {
		t.Fatalf("handler ran for prelude-resolved events, calls = %d", calls)
	}
}

func TestDeferredDispatchPreludeFailureReturnsErrorAndDoesNotDedupe(t *testing.T) {
	t.Parallel()

	state := newFakeState()
	adapter := newFakeAdapter("fake")
	bot := newDeferredRuntime(t, state, adapter)

	var mu sync.Mutex
	var calls int
	bot.OnNewMention(func(ctx context.Context, ev *chat.MessageEvent) error {
		mu.Lock()
		calls++
		mu.Unlock()
		return nil
	})

	state.mu.Lock()
	state.acquireLockErr = errors.New("lock unavailable")
	state.mu.Unlock()

	event := mentionEvent("retry-event", "fake:v1:thread-1")
	if status := postEvent(t, bot, "fake", event); status != http.StatusInternalServerError {
		t.Fatalf("prelude failure status = %d, want 500", status)
	}

	state.mu.Lock()
	state.acquireLockErr = nil
	state.mu.Unlock()

	if status := postEvent(t, bot, "fake", event); status != http.StatusOK {
		t.Fatalf("retry status = %d", status)
	}

	deadline := time.After(time.Second)
	for {
		mu.Lock()
		n := calls
		mu.Unlock()
		if n == 1 {
			break
		}
		select {
		case <-deadline:
			t.Fatalf("handler ran %d times after retry, want 1", n)
		case <-time.After(5 * time.Millisecond):
		}
	}
}

func TestDeferredDispatchHoldsThreadLockAcrossTail(t *testing.T) {
	t.Parallel()

	state := newFakeState()
	adapter := newFakeAdapter("fake")
	bot := newDeferredRuntime(t, state, adapter)

	handlerStarted := make(chan struct{})
	releaseHandler := make(chan struct{})
	var mu sync.Mutex
	var calls int
	bot.OnNewMention(func(ctx context.Context, ev *chat.MessageEvent) error {
		mu.Lock()
		calls++
		first := calls == 1
		mu.Unlock()
		if first {
			close(handlerStarted)
			<-releaseHandler
		}
		return nil
	})

	if status := postEvent(t, bot, "fake", mentionEvent("first", "fake:v1:thread-1")); status != http.StatusOK {
		t.Fatalf("first status = %d", status)
	}
	select {
	case <-handlerStarted:
	case <-time.After(time.Second):
		t.Fatal("first handler did not start")
	}

	// Second delivery for the same thread while the first tail holds the lock:
	// dropped under the default drop strategy, acked, handler not invoked.
	if status := postEvent(t, bot, "fake", mentionEvent("second", "fake:v1:thread-1")); status != http.StatusOK {
		t.Fatalf("second status = %d", status)
	}

	mu.Lock()
	if calls != 1 {
		mu.Unlock()
		t.Fatalf("calls while first held lock = %d, want 1", calls)
	}
	mu.Unlock()

	close(releaseHandler)
}

func TestDeferredDispatchRefreshesLeaseAcrossLongTail(t *testing.T) {
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

	if status := postEvent(t, bot, "fake", mentionEvent("first", "fake:v1:thread-1")); status != http.StatusOK {
		t.Fatalf("first status = %d", status)
	}
	select {
	case <-handlerStarted:
	case <-time.After(time.Second):
		t.Fatal("handler did not start")
	}

	// Run well past the TTL; the refresh loop must keep the lease alive so a
	// competing delivery still conflicts the whole time.
	time.Sleep(150 * time.Millisecond)

	var calls int
	bot.OnNewMention(func(ctx context.Context, ev *chat.MessageEvent) error {
		calls++
		return nil
	})
	if status := postEvent(t, bot, "fake", mentionEvent("second", "fake:v1:thread-1")); status != http.StatusOK {
		t.Fatalf("second status = %d", status)
	}
	if calls != 0 {
		t.Fatalf("competing delivery ran despite held lease, calls = %d", calls)
	}

	// The lease is still held by the runtime token, so an outside acquire fails.
	if _, acquired, err := state.AcquireLock(context.Background(), "fake:v1:thread-1", time.Hour); err != nil || acquired {
		t.Fatalf("lease should still be held: acquired=%v err=%v", acquired, err)
	}

	close(releaseHandler)
}

func TestDeferredDispatchContextOutlivesRequestButHonorsTimeout(t *testing.T) {
	t.Parallel()

	state := newFakeState()
	adapter := newFakeAdapter("fake")
	bot := newDeferredRuntime(t, state, adapter, func(o *chat.RuntimeOptions) {
		o.DetachTimeout = 80 * time.Millisecond
	})

	t.Run("survives request cancellation", func(t *testing.T) {
		handlerDone := make(chan error, 1)
		bot.OnNewMention(func(ctx context.Context, ev *chat.MessageEvent) error {
			// Sleep past the request lifetime; ctx must not be done.
			time.Sleep(40 * time.Millisecond)
			handlerDone <- ctx.Err()
			return nil
		})
		if status := postEvent(t, bot, "fake", mentionEvent("survive", "fake:v1:thread-survive")); status != http.StatusOK {
			t.Fatalf("status = %d", status)
		}
		select {
		case err := <-handlerDone:
			if err != nil {
				t.Fatalf("handler context cancelled after request ended: %v", err)
			}
		case <-time.After(time.Second):
			t.Fatal("handler did not finish")
		}
	})

	t.Run("cancelled at detach timeout", func(t *testing.T) {
		timedOut := make(chan struct{})
		bot.OnNewMention(func(ctx context.Context, ev *chat.MessageEvent) error {
			select {
			case <-ctx.Done():
				close(timedOut)
				return ctx.Err()
			case <-time.After(2 * time.Second):
				return errors.New("handler not cancelled by detach timeout")
			}
		})
		if status := postEvent(t, bot, "fake", mentionEvent("timeout", "fake:v1:thread-timeout")); status != http.StatusOK {
			t.Fatalf("status = %d", status)
		}
		select {
		case <-timedOut:
		case <-time.After(time.Second):
			t.Fatal("handler was not cancelled at detach timeout")
		}
	})
}

func TestDeferredDispatchStateMutationsUnderDetachedContext(t *testing.T) {
	t.Parallel()

	state := newFakeState()
	adapter := newFakeAdapter("fake")
	bot := newDeferredRuntime(t, state, adapter)

	done := make(chan error, 1)
	bot.OnNewMention(func(ctx context.Context, ev *chat.MessageEvent) error {
		// State mutations under the detached work context must succeed even
		// after the request context is gone.
		if err := ev.Thread.Subscribe(ctx); err != nil {
			done <- err
			return err
		}
		if err := ev.Thread.Unsubscribe(ctx); err != nil {
			done <- err
			return err
		}
		done <- nil
		return nil
	})

	if status := postEvent(t, bot, "fake", mentionEvent("state", "fake:v1:thread-state")); status != http.StatusOK {
		t.Fatalf("status = %d", status)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("detached state mutation failed: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("detached handler did not finish")
	}
}

func TestDeferredDispatchShutdownDrainsInflight(t *testing.T) {
	t.Parallel()

	state := newFakeState()
	adapter := newFakeAdapter("fake")
	bot := newDeferredRuntime(t, state, adapter)

	handlerStarted := make(chan struct{})
	handlerObservedCancel := make(chan struct{})
	const handlerCleanup = 80 * time.Millisecond
	bot.OnNewMention(func(ctx context.Context, ev *chat.MessageEvent) error {
		close(handlerStarted)
		<-ctx.Done()
		close(handlerObservedCancel)
		// Linger so the drain must wait for this tail.
		time.Sleep(handlerCleanup)
		return ctx.Err()
	})

	if status := postEvent(t, bot, "fake", mentionEvent("drain", "fake:v1:thread-1")); status != http.StatusOK {
		t.Fatalf("status = %d", status)
	}
	select {
	case <-handlerStarted:
	case <-time.After(time.Second):
		t.Fatal("handler did not start")
	}

	shutdownDone := make(chan error, 1)
	go func() {
		shutdownDone <- bot.Shutdown(context.Background())
	}()

	// Shutdown must cancel the detached work context.
	select {
	case <-handlerObservedCancel:
	case <-time.After(time.Second):
		t.Fatal("shutdown did not cancel the detached work context")
	}

	// Shutdown must wait for the in-flight tail before returning.
	select {
	case <-shutdownDone:
		t.Fatal("shutdown returned before draining in-flight handler")
	case <-time.After(handlerCleanup / 2):
	}

	select {
	case err := <-shutdownDone:
		if err != nil {
			t.Fatalf("shutdown returned error: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("shutdown did not finish after handler returned")
	}
	if state.shutdowns != 1 {
		t.Fatalf("state shutdowns = %d, want 1", state.shutdowns)
	}

	// Idempotent: a second shutdown returns nil.
	if err := bot.Shutdown(context.Background()); err != nil {
		t.Fatalf("idempotent shutdown = %v", err)
	}
}

func TestDeferredDispatchShutdownBoundedDrainReturnsCtxErr(t *testing.T) {
	t.Parallel()

	state := newFakeState()
	adapter := newFakeAdapter("fake")
	bot := newDeferredRuntime(t, state, adapter, func(o *chat.RuntimeOptions) {
		o.DetachTimeout = 5 * time.Second
	})

	handlerStarted := make(chan struct{})
	release := make(chan struct{})
	bot.OnNewMention(func(ctx context.Context, ev *chat.MessageEvent) error {
		close(handlerStarted)
		<-release // ignore cancellation to force the drain to time out
		return nil
	})
	defer close(release)

	if status := postEvent(t, bot, "fake", mentionEvent("slow", "fake:v1:thread-1")); status != http.StatusOK {
		t.Fatalf("status = %d", status)
	}
	select {
	case <-handlerStarted:
	case <-time.After(time.Second):
		t.Fatal("handler did not start")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	err := bot.Shutdown(ctx)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("bounded drain shutdown error = %v, want deadline exceeded", err)
	}
}

func TestRuntimeConstructionValidatesDispatchAndConcurrency(t *testing.T) {
	t.Parallel()

	newRuntime := func(options chat.RuntimeOptions) error {
		_, err := chat.New(context.Background(),
			chat.WithState(newFakeState()),
			chat.WithAdapter(newFakeAdapter("fake")),
			chat.WithRuntimeOptions(options),
		)
		return err
	}

	// Deferred requires a positive DetachTimeout.
	if err := newRuntime(chat.RuntimeOptions{
		DedupeTTL:     time.Hour,
		ThreadLockTTL: time.Hour,
		Dispatch:      chat.DispatchDeferred,
		DetachTimeout: 0,
	}); err == nil {
		t.Fatal("expected deferred dispatch without detach timeout to fail")
	}

	// Sync ignores DetachTimeout.
	if err := newRuntime(chat.RuntimeOptions{
		DedupeTTL:     time.Hour,
		ThreadLockTTL: time.Hour,
		Dispatch:      chat.DispatchSync,
		DetachTimeout: 0,
	}); err != nil {
		t.Fatalf("sync dispatch should ignore detach timeout: %v", err)
	}

	// Queue concurrency is accepted.
	if err := newRuntime(chat.RuntimeOptions{
		DedupeTTL:     time.Hour,
		ThreadLockTTL: time.Hour,
		Concurrency:   chat.ConcurrencyQueue,
	}); err != nil {
		t.Fatalf("queue concurrency should be accepted: %v", err)
	}

	// Unknown strategy values remain rejected.
	if err := newRuntime(chat.RuntimeOptions{
		DedupeTTL:     time.Hour,
		ThreadLockTTL: time.Hour,
		Concurrency:   chat.ConcurrencyStrategy(99),
	}); err == nil {
		t.Fatal("expected unimplemented concurrency strategy to fail")
	}

	// Unknown dispatch mode is rejected.
	if err := newRuntime(chat.RuntimeOptions{
		DedupeTTL:     time.Hour,
		ThreadLockTTL: time.Hour,
		Dispatch:      chat.DispatchMode(99),
		DetachTimeout: time.Second,
	}); err == nil {
		t.Fatal("expected unknown dispatch mode to fail")
	}
}

func TestQueueStrategyDispatchesMostRecentSupersededFollowUp(t *testing.T) {
	t.Parallel()

	state := newFakeState()
	adapter := newFakeAdapter("fake")
	var logs syncBuffer
	bot, err := chat.New(context.Background(),
		chat.WithState(state),
		chat.WithAdapter(adapter),
		chat.WithLogger(slog.New(slog.NewTextHandler(&logs, nil))),
		chat.WithRuntimeOptions(chat.RuntimeOptions{
			DedupeTTL:     time.Hour,
			ThreadLockTTL: 40 * time.Millisecond,
			Concurrency:   chat.ConcurrencyQueue,
			Dispatch:      chat.DispatchDeferred,
			DetachTimeout: 5 * time.Second,
		}),
	)
	if err != nil {
		t.Fatalf("new runtime: %v", err)
	}

	firstStarted := make(chan struct{})
	releaseFirst := make(chan struct{})
	var mu sync.Mutex
	var handled []string
	bot.OnNewMention(func(ctx context.Context, ev *chat.MessageEvent) error {
		mu.Lock()
		first := len(handled) == 0
		handled = append(handled, ev.Event.ID)
		mu.Unlock()
		if first {
			close(firstStarted)
			<-releaseFirst
		}
		return nil
	})

	// First event acquires the lock and blocks.
	if status := postEvent(t, bot, "fake", mentionEvent("first", "fake:v1:thread-1")); status != http.StatusOK {
		t.Fatalf("first status = %d", status)
	}
	select {
	case <-firstStarted:
	case <-time.After(time.Second):
		t.Fatal("first handler did not start")
	}

	// Three follow-ups queue while the first handler holds the lock; only the most
	// recent dispatches, the older two are superseded.
	var wg sync.WaitGroup
	for _, id := range []string{"q1", "q2", "q3"} {
		wg.Add(1)
		go func(id string) {
			defer wg.Done()
			res := postEventResultFor(bot, "fake", mentionEvent(id, "fake:v1:thread-1"))
			if res.err != nil || res.status != http.StatusOK {
				t.Errorf("follow-up %s status=%d err=%v", id, res.status, res.err)
			}
		}(id)
		// Stagger so the supersession order is deterministic (q3 is newest).
		time.Sleep(10 * time.Millisecond)
	}

	close(releaseFirst)
	wg.Wait()

	// Wait for the queued winner to run.
	deadline := time.After(2 * time.Second)
	for {
		mu.Lock()
		n := len(handled)
		mu.Unlock()
		if n >= 2 {
			break
		}
		select {
		case <-deadline:
			t.Fatalf("queued follow-up did not run, handled = %v", handled)
		case <-time.After(5 * time.Millisecond):
		}
	}

	// Give a moment to ensure no extra (superseded) handlers run.
	time.Sleep(50 * time.Millisecond)
	mu.Lock()
	defer mu.Unlock()
	if len(handled) != 2 {
		t.Fatalf("handled = %v, want exactly the first and the most-recent follow-up", handled)
	}
	if handled[0] != "first" {
		t.Fatalf("first handled = %q, want first", handled[0])
	}
	if handled[1] != "q3" {
		t.Fatalf("queued winner = %q, want q3 (most recent)", handled[1])
	}
	if !strings.Contains(logs.String(), "chat queue superseded") {
		t.Fatal("expected superseded events surfaced via Runtime Observation")
	}
}

func TestQueueStrategyBoundedWaitIsAcknowledged(t *testing.T) {
	t.Parallel()

	state := newFakeState()
	adapter := newFakeAdapter("fake")
	bot := newDeferredRuntime(t, state, adapter, func(o *chat.RuntimeOptions) {
		o.Concurrency = chat.ConcurrencyQueue
		o.ThreadLockTTL = time.Hour
		// The queue wait is bounded by DetachTimeout; keep it short so it gives up
		// promptly.
		o.DetachTimeout = 80 * time.Millisecond
	})

	var mu sync.Mutex
	var calls int
	bot.OnNewMention(func(ctx context.Context, ev *chat.MessageEvent) error {
		mu.Lock()
		calls++
		mu.Unlock()
		return nil
	})

	// Hold the lock with an outside token that never releases.
	if _, acquired, err := state.AcquireLock(context.Background(), "fake:v1:thread-1", time.Hour); err != nil || !acquired {
		t.Fatalf("seed lock acquired=%v err=%v", acquired, err)
	}

	// Ack is prompt: the request is acked after the prelude without blocking on the
	// lock; the queued follow-up then waits on the detached context.
	if status := postEvent(t, bot, "fake", mentionEvent("queued", "fake:v1:thread-1")); status != http.StatusOK {
		t.Fatalf("bounded queue wait status = %d, want 200", status)
	}

	// The bounded waiter gives up at DetachTimeout without ever running the
	// handler. Allow the detach timeout to elapse plus a margin.
	time.Sleep(200 * time.Millisecond)
	mu.Lock()
	defer mu.Unlock()
	if calls != 0 {
		t.Fatalf("handler ran despite never acquiring lock, calls = %d", calls)
	}
}

func TestDeferredDispatchObservationRecords(t *testing.T) {
	t.Parallel()

	state := newFakeState()
	adapter := newFakeAdapter("fake")
	var logs syncBuffer
	bot, err := chat.New(context.Background(),
		chat.WithState(state),
		chat.WithAdapter(adapter),
		chat.WithLogger(slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))),
		chat.WithRuntimeOptions(chat.RuntimeOptions{
			DedupeTTL:     time.Hour,
			ThreadLockTTL: 40 * time.Millisecond,
			Concurrency:   chat.ConcurrencyDrop,
			Dispatch:      chat.DispatchDeferred,
			DetachTimeout: 5 * time.Second,
		}),
	)
	if err != nil {
		t.Fatalf("new runtime: %v", err)
	}

	release := make(chan struct{})
	started := make(chan struct{})
	bot.OnNewMention(func(ctx context.Context, ev *chat.MessageEvent) error {
		close(started)
		<-release
		return errors.New("post-ack failure")
	})

	if status := postEvent(t, bot, "fake", mentionEvent("obs", "fake:v1:thread-1")); status != http.StatusOK {
		t.Fatalf("status = %d", status)
	}
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("handler did not start")
	}
	// Let at least one lease refresh happen.
	time.Sleep(80 * time.Millisecond)
	close(release)

	// Wait for the handler error to be recorded.
	deadline := time.After(time.Second)
	for !strings.Contains(logs.String(), "chat handler failed") {
		select {
		case <-deadline:
			t.Fatalf("handler error not observed; logs:\n%s", logs.String())
		case <-time.After(5 * time.Millisecond):
		}
	}

	out := logs.String()
	for _, want := range []string{
		"chat deferred dispatch started",
		"chat thread lock refreshed",
		"chat handler failed",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing observation %q in logs:\n%s", want, out)
		}
	}
}

// TestDeferredHandlerCancelledOnLeaseLossWithoutHook proves that every
// deferred lock holder stops on lease loss, even when the runtime has no
// OnLockConflict hook of its own: a lease force released elsewhere (or
// expired) means mutual exclusion is gone, and the handler observes
// chat.ErrPreempted as its cancellation cause.
func TestDeferredHandlerCancelledOnLeaseLossWithoutHook(t *testing.T) {
	t.Parallel()

	state := newFakeState()
	adapter := newFakeAdapter("fake")
	var logs syncBuffer
	bot, err := chat.New(context.Background(),
		chat.WithState(state),
		chat.WithAdapter(adapter),
		chat.WithLogger(slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))),
		chat.WithRuntimeOptions(chat.RuntimeOptions{
			DedupeTTL: time.Hour,
			// A short TTL keeps the refresh cadence (TTL/2) fast.
			ThreadLockTTL: 100 * time.Millisecond,
			Concurrency:   chat.ConcurrencyDrop,
			Dispatch:      chat.DispatchDeferred,
			DetachTimeout: 5 * time.Second,
		}),
	)
	if err != nil {
		t.Fatalf("new runtime: %v", err)
	}

	started := make(chan struct{})
	cause := make(chan error, 1)
	bot.OnNewMention(func(ctx context.Context, ev *chat.MessageEvent) error {
		close(started)
		<-ctx.Done()
		cause <- context.Cause(ctx)
		return ctx.Err()
	})

	if status := postEvent(t, bot, "fake", mentionEvent("holder", "fake:v1:thread-1")); status != http.StatusOK {
		t.Fatalf("status = %d", status)
	}
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("handler did not start")
	}

	// Another runtime instance force releases the lease out from under the
	// handler; the next refresh must observe the loss and cancel it.
	if released, err := state.ForceReleaseLock(context.Background(), "fake:v1:thread-1"); err != nil || !released {
		t.Fatalf("external force release released=%v err=%v", released, err)
	}

	select {
	case got := <-cause:
		if !errors.Is(got, chat.ErrPreempted) {
			t.Fatalf("cancellation cause = %v, want chat.ErrPreempted", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("handler was not cancelled after its lease was force released")
	}

	deadline := time.After(2 * time.Second)
	for !strings.Contains(logs.String(), "chat handler preempted") {
		select {
		case <-deadline:
			t.Fatalf("lease-loss cancellation not surfaced; logs:\n%s", logs.String())
		case <-time.After(5 * time.Millisecond):
		}
	}
}

// syncBuffer is a goroutine-safe bytes.Buffer for capturing logs from detached
// tails.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func newSyncBuffer() *syncBuffer {
	return &syncBuffer{}
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}
