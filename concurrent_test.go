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

	state := newFakeState()
	adapter := newFakeAdapter("fake")
	var logs syncBuffer
	bot := newConcurrentRuntime(t, state, adapter, &logs, func(o *chat.RuntimeOptions) {
		o.MaxConcurrent = 1
	})

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

	if status := postEvent(t, bot, "fake", mentionEvent("first", "fake:v1:thread-1")); status != http.StatusOK {
		t.Fatalf("first status = %d", status)
	}
	select {
	case <-firstStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("first handler did not start")
	}

	// The second event is acked promptly but must wait for the single slot.
	if status := postEvent(t, bot, "fake", mentionEvent("second", "fake:v1:thread-2")); status != http.StatusOK {
		t.Fatalf("second status = %d", status)
	}
	time.Sleep(80 * time.Millisecond)
	mu.Lock()
	if len(handled) != 1 {
		got := append([]string(nil), handled...)
		mu.Unlock()
		t.Fatalf("MaxConcurrent=1 did not bound execution, handled = %v", got)
	}
	mu.Unlock()

	close(releaseFirst)

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
			t.Fatal("second handler did not run after the slot freed")
		case <-time.After(5 * time.Millisecond):
		}
	}
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
