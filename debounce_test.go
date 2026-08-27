package chat_test

import (
	"context"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/chat"
)

func newDebounceRuntime(t *testing.T, state chat.State, adapter chat.Adapter, logs *syncBuffer, mutate ...func(*chat.RuntimeOptions)) *chat.Chat {
	t.Helper()
	options := chat.RuntimeOptions{
		DedupeTTL:        time.Hour,
		ThreadLockTTL:    40 * time.Millisecond,
		Concurrency:      chat.ConcurrencyDebounce,
		DebounceInterval: 50 * time.Millisecond,
		Dispatch:         chat.DispatchDeferred,
		DetachTimeout:    5 * time.Second,
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
		t.Fatalf("new debounce runtime: %v", err)
	}
	return bot
}

func TestDebounceCoalescesRapidFollowUpsToFinalEvent(t *testing.T) {
	t.Parallel()

	state := newFakeState()
	adapter := newFakeAdapter("fake")
	var logs syncBuffer
	bot := newDebounceRuntime(t, state, adapter, &logs, func(o *chat.RuntimeOptions) {
		o.DebounceInterval = 150 * time.Millisecond
	})

	var mu sync.Mutex
	var handled []string
	bot.OnNewMention(func(ctx context.Context, ev *chat.MessageEvent) error {
		mu.Lock()
		handled = append(handled, ev.Event.ID)
		mu.Unlock()
		return nil
	})

	// Three events arrive well inside one 150ms quiet period: only the final
	// one may dispatch.
	for _, id := range []string{"first", "second", "third"} {
		if status := postEvent(t, bot, "fake", mentionEvent(id, "fake:v1:thread-1")); status != http.StatusOK {
			t.Fatalf("%s status = %d", id, status)
		}
		time.Sleep(5 * time.Millisecond)
	}

	deadline := time.After(2 * time.Second)
	for {
		mu.Lock()
		n := len(handled)
		mu.Unlock()
		if n >= 1 {
			break
		}
		select {
		case <-deadline:
			t.Fatal("debounce winner did not dispatch after the quiet period")
		case <-time.After(5 * time.Millisecond):
		}
	}

	// Give any (incorrect) superseded dispatch time to surface.
	time.Sleep(100 * time.Millisecond)
	mu.Lock()
	got := append([]string(nil), handled...)
	mu.Unlock()
	if len(got) != 1 || got[0] != "third" {
		t.Fatalf("handled = %v, want only the final event [third]", got)
	}

	out := logs.String()
	if !strings.Contains(out, "chat debounce superseded") {
		t.Fatalf("superseded events not surfaced via observation; logs:\n%s", out)
	}
	if !strings.Contains(out, "superseded_by=third") {
		t.Fatalf("supersession did not carry superseded_by=third; logs:\n%s", out)
	}
	// A debounce coalesce is never a Lock Conflict drop.
	if strings.Contains(out, "chat lock conflict dropped") {
		t.Fatalf("debounce surfaced a lock-conflict drop; logs:\n%s", out)
	}
}

func TestDebounceAcksPromptlyAndDispatchesAfterQuietPeriod(t *testing.T) {
	t.Parallel()

	state := newFakeState()
	adapter := newFakeAdapter("fake")
	var logs syncBuffer
	bot := newDebounceRuntime(t, state, adapter, &logs, func(o *chat.RuntimeOptions) {
		o.DebounceInterval = 400 * time.Millisecond
	})

	handled := make(chan string, 1)
	bot.OnNewMention(func(ctx context.Context, ev *chat.MessageEvent) error {
		handled <- ev.Event.ID
		return nil
	})

	start := time.Now()
	if status := postEvent(t, bot, "fake", mentionEvent("lone", "fake:v1:thread-1")); status != http.StatusOK {
		t.Fatalf("status = %d", status)
	}
	if ackLatency := time.Since(start); ackLatency > 200*time.Millisecond {
		t.Fatalf("ack blocked on the quiet period (%v); debounce must be ack-then-work", ackLatency)
	}

	select {
	case id := <-handled:
		if id != "lone" {
			t.Fatalf("handled %q, want lone", id)
		}
		if elapsed := time.Since(start); elapsed < 400*time.Millisecond {
			t.Fatalf("handler ran %v after post, before the quiet period elapsed", elapsed)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("debounced event never dispatched")
	}
}

func TestDebounceWinnerWaitsForInflightLockRelease(t *testing.T) {
	t.Parallel()

	state := newFakeState()
	adapter := newFakeAdapter("fake")
	var logs syncBuffer
	bot := newDebounceRuntime(t, state, adapter, &logs, func(o *chat.RuntimeOptions) {
		o.ThreadLockTTL = time.Hour
		o.DebounceInterval = 20 * time.Millisecond
	})

	handled := make(chan string, 1)
	bot.OnNewMention(func(ctx context.Context, ev *chat.MessageEvent) error {
		handled <- ev.Event.ID
		return nil
	})

	// Hold the scope's lock with an outside token so the quiet-period winner
	// must wait for release.
	lease, acquired, err := state.AcquireLock(context.Background(), "fake:v1:thread-1", time.Hour)
	if err != nil || !acquired {
		t.Fatalf("seed lock acquired=%v err=%v", acquired, err)
	}

	if status := postEvent(t, bot, "fake", mentionEvent("waiter", "fake:v1:thread-1")); status != http.StatusOK {
		t.Fatalf("status = %d", status)
	}

	// Well past the quiet period the handler must still be blocked on the lock.
	time.Sleep(100 * time.Millisecond)
	select {
	case id := <-handled:
		t.Fatalf("handler %q ran while the lock was held", id)
	default:
	}

	if released, err := state.ReleaseLock(context.Background(), lease); err != nil || !released {
		t.Fatalf("release seed lock released=%v err=%v", released, err)
	}

	select {
	case id := <-handled:
		if id != "waiter" {
			t.Fatalf("handled %q, want waiter", id)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("debounce winner did not run after the lock released")
	}
}

func TestDebounceAbandonedWaiterSurfacesObservation(t *testing.T) {
	t.Parallel()

	state := newFakeState()
	adapter := newFakeAdapter("fake")
	var logs syncBuffer
	bot := newDebounceRuntime(t, state, adapter, &logs, func(o *chat.RuntimeOptions) {
		o.ThreadLockTTL = time.Hour
		o.DebounceInterval = 20 * time.Millisecond
		// The detached waiter abandons at DetachTimeout while the outside lock
		// never releases.
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

	if _, acquired, err := state.AcquireLock(context.Background(), "fake:v1:thread-1", time.Hour); err != nil || !acquired {
		t.Fatalf("seed lock acquired=%v err=%v", acquired, err)
	}

	if status := postEvent(t, bot, "fake", mentionEvent("abandoned", "fake:v1:thread-1")); status != http.StatusOK {
		t.Fatalf("status = %d", status)
	}

	deadline := time.After(2 * time.Second)
	for !strings.Contains(logs.String(), "chat debounce wait abandoned") {
		select {
		case <-deadline:
			t.Fatalf("abandonment not surfaced via observation; logs:\n%s", logs.String())
		case <-time.After(5 * time.Millisecond):
		}
	}

	mu.Lock()
	defer mu.Unlock()
	if calls != 0 {
		t.Fatalf("handler ran despite never acquiring the lock, calls = %d", calls)
	}
}

func TestDebounceSleepingWaiterDrainedByShutdown(t *testing.T) {
	t.Parallel()

	state := newFakeState()
	adapter := newFakeAdapter("fake")
	var logs syncBuffer
	bot := newDebounceRuntime(t, state, adapter, &logs, func(o *chat.RuntimeOptions) {
		// Only Shutdown can end the quiet-period sleep.
		o.DebounceInterval = time.Hour
		o.DetachTimeout = 2 * time.Hour
	})

	var mu sync.Mutex
	var calls int
	bot.OnNewMention(func(ctx context.Context, ev *chat.MessageEvent) error {
		mu.Lock()
		calls++
		mu.Unlock()
		return nil
	})

	if status := postEvent(t, bot, "fake", mentionEvent("sleeper", "fake:v1:thread-1")); status != http.StatusOK {
		t.Fatalf("status = %d", status)
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
		t.Fatal("shutdown did not drain the sleeping debounce waiter")
	}

	if !strings.Contains(logs.String(), "chat debounce wait abandoned") {
		t.Fatalf("shutdown-cancelled waiter not surfaced as abandoned; logs:\n%s", logs.String())
	}
	mu.Lock()
	defer mu.Unlock()
	if calls != 0 {
		t.Fatalf("handler ran despite shutdown before the quiet period, calls = %d", calls)
	}
}

// acquireGatedState wraps fakeState so a test can hold a tail's AcquireLock
// call open and register a newer event while the acquire is in flight.
type acquireGatedState struct {
	*fakeState
	mu      sync.Mutex
	gate    chan struct{}
	entered chan struct{}
	armed   bool
}

func newAcquireGatedState() *acquireGatedState {
	return &acquireGatedState{
		fakeState: newFakeState(),
		gate:      make(chan struct{}),
		entered:   make(chan struct{}),
		armed:     true,
	}
}

func (s *acquireGatedState) AcquireLock(ctx context.Context, key string, ttl time.Duration) (chat.LockLease, bool, error) {
	s.mu.Lock()
	first := s.armed
	s.armed = false
	s.mu.Unlock()
	if first {
		close(s.entered)
		<-s.gate
	}
	return s.fakeState.AcquireLock(ctx, key, ttl)
}

func TestDebounceWaiterDisplacedDuringAcquireDoesNotDispatch(t *testing.T) {
	t.Parallel()

	state := newAcquireGatedState()
	adapter := newFakeAdapter("fake")
	var logs syncBuffer
	bot := newDebounceRuntime(t, state, adapter, &logs, func(o *chat.RuntimeOptions) {
		o.DebounceInterval = 20 * time.Millisecond
	})

	var mu sync.Mutex
	var handled []string
	bot.OnNewMention(func(ctx context.Context, ev *chat.MessageEvent) error {
		mu.Lock()
		handled = append(handled, ev.Event.ID)
		mu.Unlock()
		return nil
	})

	if status := postEvent(t, bot, "fake", mentionEvent("older", "fake:v1:thread-1")); status != http.StatusOK {
		t.Fatalf("older status = %d", status)
	}

	// Hold older's post-quiet-period AcquireLock open, then register the newer
	// event while the acquire is in flight.
	select {
	case <-state.entered:
	case <-time.After(2 * time.Second):
		t.Fatal("older waiter never reached AcquireLock")
	}
	if status := postEvent(t, bot, "fake", mentionEvent("newer", "fake:v1:thread-1")); status != http.StatusOK {
		t.Fatalf("newer status = %d", status)
	}
	close(state.gate)

	deadline := time.After(2 * time.Second)
	for {
		mu.Lock()
		n := len(handled)
		mu.Unlock()
		if n >= 1 {
			break
		}
		select {
		case <-deadline:
			t.Fatal("debounce winner did not dispatch")
		case <-time.After(5 * time.Millisecond):
		}
	}
	time.Sleep(60 * time.Millisecond)

	mu.Lock()
	defer mu.Unlock()
	if !equalStrings(handled, []string{"newer"}) {
		t.Fatalf("handled = %v, want only [newer]: a waiter displaced mid-acquire must not dispatch", handled)
	}
}

func TestDebounceConstructionValidation(t *testing.T) {
	t.Parallel()

	newRuntime := func(mutate func(*chat.RuntimeOptions)) error {
		options := chat.RuntimeOptions{
			DedupeTTL:        time.Hour,
			ThreadLockTTL:    time.Hour,
			Concurrency:      chat.ConcurrencyDebounce,
			DebounceInterval: 50 * time.Millisecond,
			Dispatch:         chat.DispatchDeferred,
			DetachTimeout:    time.Second,
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
		t.Fatalf("valid debounce options rejected: %v", err)
	}
	if err := newRuntime(func(o *chat.RuntimeOptions) { o.DebounceInterval = 0 }); err == nil {
		t.Fatal("expected debounce without an interval to fail")
	}
	if err := newRuntime(func(o *chat.RuntimeOptions) {
		o.Dispatch = chat.DispatchSync
		o.DetachTimeout = 0
	}); err == nil {
		t.Fatal("expected debounce under sync dispatch to fail")
	}
	// A detach timeout at or below the interval would abandon every event
	// before its quiet period elapses.
	if err := newRuntime(func(o *chat.RuntimeOptions) {
		o.DebounceInterval = time.Second
		o.DetachTimeout = time.Second
	}); err == nil {
		t.Fatal("expected a detach timeout at or below the debounce interval to fail")
	}
}

func TestDebounceSupersededWaiterExitsBeforeItsInterval(t *testing.T) {
	t.Parallel()

	state := newFakeState()
	adapter := newFakeAdapter("fake")
	var logs syncBuffer
	bot := newDebounceRuntime(t, state, adapter, &logs, func(o *chat.RuntimeOptions) {
		// The interval is far longer than the test: only the displacement signal
		// can explain a prompt exit.
		o.DebounceInterval = time.Hour
		o.DetachTimeout = 2 * time.Hour
	})

	var mu sync.Mutex
	var calls int
	bot.OnNewMention(func(ctx context.Context, ev *chat.MessageEvent) error {
		mu.Lock()
		calls++
		mu.Unlock()
		return nil
	})

	if status := postEvent(t, bot, "fake", mentionEvent("older", "fake:v1:thread-1")); status != http.StatusOK {
		t.Fatalf("older status = %d", status)
	}
	if status := postEvent(t, bot, "fake", mentionEvent("newer", "fake:v1:thread-1")); status != http.StatusOK {
		t.Fatalf("newer status = %d", status)
	}

	// The superseded waiter must release its goroutine and event promptly, not
	// park through the hour-long interval.
	deadline := time.After(2 * time.Second)
	for !strings.Contains(logs.String(), "chat debounce waiter superseded") {
		select {
		case <-deadline:
			t.Fatalf("superseded waiter did not exit promptly; logs:\n%s", logs.String())
		case <-time.After(5 * time.Millisecond):
		}
	}

	mu.Lock()
	defer mu.Unlock()
	if calls != 0 {
		t.Fatalf("handler ran before any quiet period elapsed, calls = %d", calls)
	}
}
