package chat_test

import (
	"context"
	"log/slog"
	"net/http"
	"runtime"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/coder/chat"
)

// lockCountingState wraps fakeState to prove the concurrent strategy never
// touches the Thread Lock.
type lockCountingState struct {
	*fakeState
	countMu  sync.Mutex
	acquires int
}

func (s *lockCountingState) AcquireLock(ctx context.Context, key string, ttl time.Duration) (chat.LockLease, bool, error) {
	s.countMu.Lock()
	s.acquires++
	s.countMu.Unlock()
	return s.fakeState.AcquireLock(ctx, key, ttl)
}

func (s *lockCountingState) acquireCalls() int {
	s.countMu.Lock()
	defer s.countMu.Unlock()
	return s.acquires
}

func newConcurrentRuntime(t *testing.T, state chat.State, adapter chat.Adapter, logs *syncBuffer, mutate ...func(*chat.RuntimeOptions)) *chat.Chat {
	t.Helper()
	options := chat.RuntimeOptions{
		DedupeTTL:     time.Hour,
		ThreadLockTTL: time.Hour,
		Concurrency:   chat.ConcurrencyConcurrent,
		MaxConcurrent: 4,
		Dispatch:      chat.DispatchDeferred,
		MaxDetached:   1024,
		DetachTimeout: 5 * time.Second,
	}
	for _, m := range mutate {
		m(&options)
	}
	bot, err := chat.New(context.Background(),
		chat.WithState(state),
		chat.WithAdapter(adapter),
		chat.WithLogger(slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))),
		chat.WithRuntimeOptions(options),
	)
	if err != nil {
		t.Fatalf("new concurrent runtime: %v", err)
	}
	return bot
}

func TestConcurrentRunsSameThreadHandlersInParallelWithoutLock(t *testing.T) {
	t.Parallel()

	state := &lockCountingState{fakeState: newFakeState()}
	adapter := newFakeAdapter("fake")
	var logs syncBuffer
	bot := newConcurrentRuntime(t, state, adapter, &logs)

	// The first handler only finishes once the second has started: proof that
	// two handlers for the SAME Thread run concurrently (no serialization).
	secondStarted := make(chan struct{})
	firstDone := make(chan struct{})
	bot.OnNewMention(func(ctx context.Context, ev *chat.MessageEvent) error {
		switch ev.Event.ID {
		case "first":
			select {
			case <-secondStarted:
				close(firstDone)
			case <-ctx.Done():
			}
		case "second":
			close(secondStarted)
		}
		return nil
	})

	for _, id := range []string{"first", "second"} {
		if status := postEvent(t, bot, "fake", mentionEvent(id, "fake:v1:thread-1")); status != http.StatusOK {
			t.Fatalf("%s status = %d", id, status)
		}
	}

	select {
	case <-firstDone:
	case <-time.After(2 * time.Second):
		t.Fatal("handlers for the same thread did not run concurrently")
	}

	if calls := state.acquireCalls(); calls != 0 {
		t.Fatalf("concurrent strategy acquired the thread lock %d times, want 0", calls)
	}
	if strings.Contains(logs.String(), "chat lock conflict dropped") {
		t.Fatalf("concurrent strategy surfaced a lock conflict; logs:\n%s", logs.String())
	}
}

func TestConcurrentBoundedByMaxConcurrent(t *testing.T) {
	t.Parallel()

	synctest.Test(t, func(t *testing.T) {
		state := newFakeState()
		adapter := newFakeAdapter("fake")
		var logs syncBuffer
		const detachTimeout = time.Hour
		bot := newConcurrentRuntime(t, state, adapter, &logs, func(o *chat.RuntimeOptions) {
			o.MaxConcurrent = 1
			o.DetachTimeout = detachTimeout
		})

		releaseFirst := make(chan struct{})
		release := sync.OnceFunc(func() { close(releaseFirst) })
		defer release()
		var mu sync.Mutex
		var handled []string
		bot.OnNewMention(func(ctx context.Context, ev *chat.MessageEvent) error {
			mu.Lock()
			handled = append(handled, ev.Event.ID)
			mu.Unlock()
			if ev.Event.ID == "first" {
				<-releaseFirst
			}
			return nil
		})
		handledIDs := func() []string {
			mu.Lock()
			defer mu.Unlock()
			return slices.Clone(handled)
		}

		if status := postEvent(t, bot, "fake", mentionEvent("first", "fake:v1:thread-1")); status != http.StatusOK {
			t.Fatalf("first status = %d", status)
		}
		synctest.Wait()
		if got := handledIDs(); !slices.Equal(got, []string{"first"}) {
			t.Fatalf("first handler did not start alone, handled = %v", got)
		}

		if status := postEvent(t, bot, "fake", mentionEvent("second", "fake:v1:thread-2")); status != http.StatusOK {
			t.Fatalf("second status = %d", status)
		}
		justBeforeSlotWaitDeadline := detachTimeout - time.Nanosecond
		time.Sleep(justBeforeSlotWaitDeadline)
		if got := handledIDs(); !slices.Equal(got, []string{"first"}) {
			t.Fatalf("MaxConcurrent=1 did not bound execution, handled = %v", got)
		}

		release()
		synctest.Wait()
		if got := handledIDs(); !slices.Equal(got, []string{"first", "second"}) {
			t.Fatalf("second handler did not run after the slot freed, handled = %v", got)
		}
	})
}

func TestConcurrentSlotWaiterDrainedByShutdown(t *testing.T) {
	t.Parallel()

	state := newFakeState()
	adapter := newFakeAdapter("fake")
	var logs syncBuffer
	bot := newConcurrentRuntime(t, state, adapter, &logs, func(o *chat.RuntimeOptions) {
		o.MaxConcurrent = 1
		o.DetachTimeout = time.Hour
	})

	firstStarted := make(chan struct{})
	bot.OnNewMention(func(ctx context.Context, ev *chat.MessageEvent) error {
		if ev.Event.ID == "first" {
			close(firstStarted)
			// Occupy the only slot until the detached context is cancelled.
			<-ctx.Done()
			return ctx.Err()
		}
		return nil
	})

	if status := postEvent(t, bot, "fake", mentionEvent("first", "fake:v1:thread-1")); status != http.StatusOK {
		t.Fatalf("first status = %d", status)
	}
	select {
	case <-firstStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("first handler did not start")
	}
	if status := postEvent(t, bot, "fake", mentionEvent("waiter", "fake:v1:thread-2")); status != http.StatusOK {
		t.Fatalf("waiter status = %d", status)
	}
	time.Sleep(40 * time.Millisecond)

	done := make(chan error, 1)
	go func() { done <- bot.Shutdown(context.Background()) }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("shutdown error: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("shutdown did not drain the slot waiter")
	}

	if !strings.Contains(logs.String(), "chat concurrent slot wait abandoned") {
		t.Fatalf("abandoned slot wait not surfaced via observation; logs:\n%s", logs.String())
	}
}

func TestConcurrentSlotWaiterAbandonedAtDetachTimeout(t *testing.T) {
	t.Parallel()

	synctest.Test(t, func(t *testing.T) {
		var logs syncBuffer
		observer := &recordingObserver{}
		const detachTimeout = time.Hour
		bot, err := chat.New(context.Background(),
			chat.WithState(newFakeState()),
			chat.WithAdapter(newFakeAdapter("fake")),
			chat.WithLogger(slog.New(slog.NewTextHandler(&logs, nil))),
			chat.WithObserver(observer),
			chat.WithRuntimeOptions(chat.RuntimeOptions{
				DedupeTTL:     time.Hour,
				ThreadLockTTL: time.Hour,
				Concurrency:   chat.ConcurrencyConcurrent,
				MaxConcurrent: 1,
				Dispatch:      chat.DispatchDeferred,
				MaxDetached:   1024,
				DetachTimeout: detachTimeout,
			}),
		)
		if err != nil {
			t.Fatalf("new concurrent runtime: %v", err)
		}

		releaseFirst := make(chan struct{})
		release := sync.OnceFunc(func() { close(releaseFirst) })
		defer release()
		var waiterRan atomic.Bool
		bot.OnNewMention(func(_ context.Context, ev *chat.MessageEvent) error {
			if ev.Event.ID == "first" {
				<-releaseFirst
				return nil
			}
			waiterRan.Store(true)
			return nil
		})

		if status := postEvent(t, bot, "fake", mentionEvent("first", "fake:v1:thread-1")); status != http.StatusOK {
			t.Fatalf("first status = %d", status)
		}
		synctest.Wait()
		if status := postEvent(t, bot, "fake", mentionEvent("waiter", "fake:v1:thread-2")); status != http.StatusOK {
			t.Fatalf("waiter status = %d", status)
		}
		time.Sleep(detachTimeout)
		synctest.Wait()

		if waiterRan.Load() {
			t.Fatalf("waiter ran its handler after its DetachTimeout expired; logs:\n%s", logs.String())
		}
		abandoned := slices.ContainsFunc(strings.Split(logs.String(), "\n"), func(line string) bool {
			return strings.Contains(line, "chat concurrent slot wait abandoned") &&
				strings.Contains(line, "event_id=waiter") &&
				strings.Contains(line, "context deadline exceeded")
		})
		if !abandoned {
			t.Fatalf("waiter's expired slot wait not logged as abandoned; logs:\n%s", logs.String())
		}
		if got := observer.terminalOutcomes(); !slices.Equal(got, []chat.DispatchOutcome{chat.OutcomeIgnored}) {
			t.Fatalf("outcomes while the first handler holds the slot = %v, want [ignored]", got)
		}

		release()
		synctest.Wait()
		if waiterRan.Load() {
			t.Fatalf("waiter ran its handler once the slot freed; logs:\n%s", logs.String())
		}
		if got := observer.terminalOutcomes(); !slices.Equal(got, []chat.DispatchOutcome{chat.OutcomeIgnored, chat.OutcomeHandled}) {
			t.Fatalf("outcomes after the first handler returned = %v, want [ignored handled]", got)
		}
	})
}

func TestConcurrentSyncSlotWaiterAbandonedWhenRequestEnds(t *testing.T) {
	t.Parallel()

	synctest.Test(t, func(t *testing.T) {
		var logs syncBuffer
		observer := &recordingObserver{}
		bot, err := chat.New(context.Background(),
			chat.WithState(newFakeState()),
			chat.WithAdapter(newFakeAdapter("fake")),
			chat.WithLogger(slog.New(slog.NewTextHandler(&logs, nil))),
			chat.WithObserver(observer),
			chat.WithRuntimeOptions(chat.RuntimeOptions{
				DedupeTTL:     time.Hour,
				ThreadLockTTL: time.Hour,
				Concurrency:   chat.ConcurrencyConcurrent,
				MaxConcurrent: 1,
				Dispatch:      chat.DispatchSync,
				DetachTimeout: 0,
			}),
		)
		if err != nil {
			t.Fatalf("new concurrent runtime: %v", err)
		}
		handler, err := bot.Webhook("fake")
		if err != nil {
			t.Fatalf("webhook: %v", err)
		}

		releaseFirst := make(chan struct{})
		release := sync.OnceFunc(func() { close(releaseFirst) })
		defer release()
		var waiterRan atomic.Bool
		bot.OnNewMention(func(_ context.Context, ev *chat.MessageEvent) error {
			if ev.Event.ID == "first" {
				<-releaseFirst
				return nil
			}
			waiterRan.Store(true)
			return nil
		})

		firstStatus := make(chan int, 1)
		go func() {
			firstStatus <- serveEventWithContext(handler, context.Background(), mentionEvent("first", "fake:v1:thread-1"))
		}()
		synctest.Wait()

		waiterCtx, cancel := context.WithCancel(context.Background())
		defer cancel()
		waiterStatus := make(chan int, 1)
		go func() {
			waiterStatus <- serveEventWithContext(handler, waiterCtx, mentionEvent("waiter", "fake:v1:thread-2"))
		}()
		synctest.Wait()

		cancel()
		synctest.Wait()
		select {
		case status := <-waiterStatus:
			if status != http.StatusOK {
				t.Fatalf("waiter status = %d", status)
			}
		default:
			t.Fatal("waiter request did not return after its context ended")
		}
		if waiterRan.Load() {
			t.Fatalf("waiter ran its handler after its request ended; logs:\n%s", logs.String())
		}
		abandoned := slices.ContainsFunc(strings.Split(logs.String(), "\n"), func(line string) bool {
			return strings.Contains(line, "chat concurrent slot wait abandoned") &&
				strings.Contains(line, "event_id=waiter") &&
				strings.Contains(line, "context canceled")
		})
		if !abandoned {
			t.Fatalf("waiter's ended slot wait not logged as abandoned; logs:\n%s", logs.String())
		}
		if got := observer.terminalOutcomes(); !slices.Equal(got, []chat.DispatchOutcome{chat.OutcomeIgnored}) {
			t.Fatalf("outcomes while the first handler holds the slot = %v, want [ignored]", got)
		}

		release()
		synctest.Wait()
		if waiterRan.Load() {
			t.Fatalf("waiter ran its handler once the slot freed; logs:\n%s", logs.String())
		}
		select {
		case status := <-firstStatus:
			if status != http.StatusOK {
				t.Fatalf("first status = %d", status)
			}
		default:
			t.Fatal("first request did not return after its handler finished")
		}
		if got := observer.terminalOutcomes(); !slices.Equal(got, []chat.DispatchOutcome{chat.OutcomeIgnored, chat.OutcomeHandled}) {
			t.Fatalf("outcomes after the first handler returned = %v, want [ignored handled]", got)
		}
	})
}

func TestConcurrentSyncDispatchRunsWithoutLock(t *testing.T) {
	t.Parallel()

	state := &lockCountingState{fakeState: newFakeState()}
	adapter := newFakeAdapter("fake")
	var logs syncBuffer
	bot := newConcurrentRuntime(t, state, adapter, &logs, func(o *chat.RuntimeOptions) {
		o.Dispatch = chat.DispatchSync
		o.DetachTimeout = 0
	})

	handled := make(chan string, 1)
	bot.OnNewMention(func(ctx context.Context, ev *chat.MessageEvent) error {
		handled <- ev.Event.ID
		return nil
	})

	if status := postEvent(t, bot, "fake", mentionEvent("sync", "fake:v1:thread-1")); status != http.StatusOK {
		t.Fatalf("status = %d", status)
	}
	select {
	case id := <-handled:
		if id != "sync" {
			t.Fatalf("handled %q, want sync", id)
		}
	default:
		t.Fatal("sync dispatch returned before the handler ran")
	}
	if calls := state.acquireCalls(); calls != 0 {
		t.Fatalf("concurrent strategy acquired the thread lock %d times, want 0", calls)
	}
}

func TestConcurrentConstructionValidation(t *testing.T) {
	t.Parallel()

	newRuntime := func(mutate func(*chat.RuntimeOptions)) error {
		options := chat.RuntimeOptions{
			DedupeTTL:     time.Hour,
			ThreadLockTTL: time.Hour,
			Concurrency:   chat.ConcurrencyConcurrent,
			MaxConcurrent: 2,
		}
		mutate(&options)
		_, err := chat.New(context.Background(),
			chat.WithState(newFakeState()),
			chat.WithAdapter(newFakeAdapter("fake")),
			chat.WithRuntimeOptions(options),
		)
		return err
	}

	if err := newRuntime(func(o *chat.RuntimeOptions) {}); err != nil {
		t.Fatalf("valid concurrent options rejected: %v", err)
	}
	if err := newRuntime(func(o *chat.RuntimeOptions) { o.MaxConcurrent = 0 }); err == nil {
		t.Fatal("expected concurrent without a max concurrent bound to fail")
	}
	if err := newRuntime(func(o *chat.RuntimeOptions) { o.MaxConcurrent = -1 }); err == nil {
		t.Fatal("expected a negative max concurrent bound to fail")
	}
}

// TestConcurrentSlotFreedDuringShutdownIsAbandoned pins that a waiter whose
// slot frees while Shutdown cancels the tails abandons the wait. The tails
// cancel in random order, so the test repeats the scenario.
func TestConcurrentSlotFreedDuringShutdownIsAbandoned(t *testing.T) {
	t.Parallel()
	if runtime.GOMAXPROCS(0) < 2 {
		t.Skip("the first handler must run while Shutdown cancels the tails")
	}

	for i := range 10 {
		state := newFakeState()
		adapter := newFakeAdapter("fake")
		var logs syncBuffer
		bot := newConcurrentRuntime(t, state, adapter, &logs, func(o *chat.RuntimeOptions) {
			o.MaxConcurrent = 1
			o.DetachTimeout = time.Hour
		})

		children := make([]context.CancelFunc, 0, 20000)
		firstStarted := make(chan struct{})
		var waiterRan atomic.Bool
		bot.OnNewMention(func(ctx context.Context, ev *chat.MessageEvent) error {
			if ev.Event.ID != "first" {
				waiterRan.Store(true)
				return nil
			}
			// Shutdown cancels these after it wakes this handler, so the
			// waiter's context stays live while this handler frees the slot.
			for range cap(children) {
				_, cancel := context.WithCancel(ctx)
				children = append(children, cancel)
			}
			close(firstStarted)
			<-ctx.Done()
			return ctx.Err()
		})

		if status := postEvent(t, bot, "fake", mentionEvent("first", "fake:v1:thread-1")); status != http.StatusOK {
			t.Fatalf("first status = %d", status)
		}
		select {
		case <-firstStarted:
		case <-time.After(5 * time.Second):
			t.Fatal("first handler did not start")
		}
		if status := postEvent(t, bot, "fake", mentionEvent("waiter", "fake:v1:thread-2")); status != http.StatusOK {
			t.Fatalf("waiter status = %d", status)
		}
		if err := bot.Shutdown(context.Background()); err != nil {
			t.Fatalf("shutdown error: %v", err)
		}
		for _, cancel := range children {
			cancel()
		}

		if waiterRan.Load() {
			t.Fatalf("iteration %d: waiter ran its handler during Shutdown; logs:\n%s", i, logs.String())
		}
		if !strings.Contains(logs.String(), "chat concurrent slot wait abandoned") {
			t.Fatalf("iteration %d: abandoned slot wait not surfaced via observation; logs:\n%s", i, logs.String())
		}
	}
}
