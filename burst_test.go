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

func newBurstRuntime(t *testing.T, state chat.State, adapter chat.Adapter, logs *syncBuffer, mutate ...func(*chat.RuntimeOptions)) *chat.Chat {
	t.Helper()
	options := chat.RuntimeOptions{
		DedupeTTL:        time.Hour,
		ThreadLockTTL:    40 * time.Millisecond,
		Concurrency:      chat.ConcurrencyBurst,
		DebounceInterval: 60 * time.Millisecond,
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
		t.Fatalf("new burst runtime: %v", err)
	}
	return bot
}

func TestBurstDispatchesCollectedBatchInJoinOrder(t *testing.T) {
	t.Parallel()

	state := newFakeState()
	adapter := newFakeAdapter("fake")
	var logs syncBuffer
	bot := newBurstRuntime(t, state, adapter, &logs, func(o *chat.RuntimeOptions) {
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

	// Three events join one 150ms collection window: the whole batch
	// dispatches, in join order, once the window closes.
	start := time.Now()
	for _, id := range []string{"a", "b", "c"} {
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
		if n >= 3 {
			break
		}
		select {
		case <-deadline:
			mu.Lock()
			got := append([]string(nil), handled...)
			mu.Unlock()
			t.Fatalf("batch did not fully dispatch, handled = %v", got)
		case <-time.After(5 * time.Millisecond):
		}
	}
	if elapsed := time.Since(start); elapsed < 150*time.Millisecond {
		t.Fatalf("batch dispatched %v after the first event, before the window closed", elapsed)
	}

	time.Sleep(50 * time.Millisecond)
	mu.Lock()
	got := append([]string(nil), handled...)
	mu.Unlock()
	if !equalStrings(got, []string{"a", "b", "c"}) {
		t.Fatalf("handled = %v, want the full batch in join order [a b c]", got)
	}

	out := logs.String()
	if !strings.Contains(out, "chat burst batch dispatch") || !strings.Contains(out, "size=3") {
		t.Fatalf("batch dispatch (size=3) not surfaced via observation; logs:\n%s", out)
	}
	// Burst never skips or drops events.
	if strings.Contains(out, "superseded") || strings.Contains(out, "chat lock conflict dropped") {
		t.Fatalf("burst skipped or dropped a batch member; logs:\n%s", out)
	}
}

func TestBurstWindowOpenedDuringDispatchRunsAfterCurrentBatch(t *testing.T) {
	t.Parallel()

	state := newFakeState()
	adapter := newFakeAdapter("fake")
	var logs syncBuffer
	bot := newBurstRuntime(t, state, adapter, &logs, func(o *chat.RuntimeOptions) {
		o.DebounceInterval = 30 * time.Millisecond
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

	if status := postEvent(t, bot, "fake", mentionEvent("a", "fake:v1:thread-1")); status != http.StatusOK {
		t.Fatalf("a status = %d", status)
	}
	select {
	case <-firstStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("first batch did not start dispatching")
	}

	// A new window opens while the first batch is still dispatching; its batch
	// must run only after the current one finishes.
	if status := postEvent(t, bot, "fake", mentionEvent("b", "fake:v1:thread-1")); status != http.StatusOK {
		t.Fatalf("b status = %d", status)
	}
	time.Sleep(80 * time.Millisecond)
	mu.Lock()
	if len(handled) != 1 {
		got := append([]string(nil), handled...)
		mu.Unlock()
		t.Fatalf("successor window dispatched while the current batch was running, handled = %v", got)
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
			t.Fatal("successor batch did not dispatch after the current batch finished")
		case <-time.After(5 * time.Millisecond):
		}
	}

	mu.Lock()
	defer mu.Unlock()
	if !equalStrings(handled, []string{"a", "b"}) {
		t.Fatalf("handled = %v, want [a b] with windows serialized", handled)
	}
}

func TestBurstDispatchBudgetStartsWhenWindowCloses(t *testing.T) {
	t.Parallel()

	state := newFakeState()
	adapter := newFakeAdapter("fake")
	var logs syncBuffer
	// The window consumes most of one DetachTimeout; the batch must still get a
	// full execution budget of its own once the window closes.
	bot := newBurstRuntime(t, state, adapter, &logs, func(o *chat.RuntimeOptions) {
		o.DebounceInterval = 400 * time.Millisecond
		o.DetachTimeout = 500 * time.Millisecond
	})

	var mu sync.Mutex
	var handled []string
	bot.OnNewMention(func(ctx context.Context, ev *chat.MessageEvent) error {
		time.Sleep(60 * time.Millisecond)
		mu.Lock()
		handled = append(handled, ev.Event.ID)
		mu.Unlock()
		return nil
	})

	for _, id := range []string{"a", "b", "c"} {
		if status := postEvent(t, bot, "fake", mentionEvent(id, "fake:v1:thread-1")); status != http.StatusOK {
			t.Fatalf("%s status = %d", id, status)
		}
	}

	deadline := time.After(3 * time.Second)
	for {
		mu.Lock()
		n := len(handled)
		mu.Unlock()
		if n >= 3 {
			break
		}
		select {
		case <-deadline:
			mu.Lock()
			got := append([]string(nil), handled...)
			mu.Unlock()
			t.Fatalf("collection time consumed the batch's execution budget, handled = %v", got)
		case <-time.After(5 * time.Millisecond):
		}
	}
	if strings.Contains(logs.String(), "chat burst batch abandoned") {
		t.Fatalf("batch abandoned despite a fresh post-window budget; logs:\n%s", logs.String())
	}
}

func TestBurstEveryMemberGetsItsOwnExecutionBudget(t *testing.T) {
	t.Parallel()

	state := newFakeState()
	adapter := newFakeAdapter("fake")
	var logs syncBuffer
	// Five 60ms handlers = 300ms total, above the 200ms DetachTimeout: with a
	// shared batch deadline the tail members would be skipped, but each
	// accepted member gets its own budget.
	bot := newBurstRuntime(t, state, adapter, &logs, func(o *chat.RuntimeOptions) {
		o.DebounceInterval = 50 * time.Millisecond
		o.DetachTimeout = 200 * time.Millisecond
	})

	var mu sync.Mutex
	var handled []string
	bot.OnNewMention(func(ctx context.Context, ev *chat.MessageEvent) error {
		time.Sleep(60 * time.Millisecond)
		mu.Lock()
		handled = append(handled, ev.Event.ID)
		mu.Unlock()
		return nil
	})

	ids := []string{"a", "b", "c", "d", "e"}
	for _, id := range ids {
		if status := postEvent(t, bot, "fake", mentionEvent(id, "fake:v1:thread-1")); status != http.StatusOK {
			t.Fatalf("%s status = %d", id, status)
		}
	}

	deadline := time.After(3 * time.Second)
	for {
		mu.Lock()
		n := len(handled)
		mu.Unlock()
		if n >= len(ids) {
			break
		}
		select {
		case <-deadline:
			mu.Lock()
			got := append([]string(nil), handled...)
			mu.Unlock()
			t.Fatalf("accepted batch members were skipped on a shared deadline, handled = %v", got)
		case <-time.After(5 * time.Millisecond):
		}
	}
	if strings.Contains(logs.String(), "chat burst batch abandoned") {
		t.Fatalf("batch abandoned despite per-member budgets; logs:\n%s", logs.String())
	}
}

func TestBurstAbandonedWaiterSurfacesObservation(t *testing.T) {
	t.Parallel()

	state := newFakeState()
	adapter := newFakeAdapter("fake")
	var logs syncBuffer
	bot := newBurstRuntime(t, state, adapter, &logs, func(o *chat.RuntimeOptions) {
		o.ThreadLockTTL = time.Hour
		o.DebounceInterval = 20 * time.Millisecond
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

	if status := postEvent(t, bot, "fake", mentionEvent("abandoned", "fake:v1:thread-1")); status != http.StatusOK {
		t.Fatalf("status = %d", status)
	}

	deadline := time.After(2 * time.Second)
	for !strings.Contains(logs.String(), "chat burst wait abandoned") {
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

func TestBurstCollectingWindowDrainedByShutdown(t *testing.T) {
	t.Parallel()

	state := newFakeState()
	adapter := newFakeAdapter("fake")
	var logs syncBuffer
	bot := newBurstRuntime(t, state, adapter, &logs, func(o *chat.RuntimeOptions) {
		// Only Shutdown can close the collection window.
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

	if status := postEvent(t, bot, "fake", mentionEvent("collected", "fake:v1:thread-1")); status != http.StatusOK {
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
		t.Fatal("shutdown did not drain the collecting burst runner")
	}

	if !strings.Contains(logs.String(), "chat burst wait abandoned") {
		t.Fatalf("shutdown-cancelled window not surfaced as abandoned; logs:\n%s", logs.String())
	}
	mu.Lock()
	defer mu.Unlock()
	if calls != 0 {
		t.Fatalf("handler ran despite shutdown before the window closed, calls = %d", calls)
	}
}

func TestBurstConstructionValidation(t *testing.T) {
	t.Parallel()

	newRuntime := func(mutate func(*chat.RuntimeOptions)) error {
		options := chat.RuntimeOptions{
			DedupeTTL:        time.Hour,
			ThreadLockTTL:    time.Hour,
			Concurrency:      chat.ConcurrencyBurst,
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
		t.Fatalf("valid burst options rejected: %v", err)
	}
	if err := newRuntime(func(o *chat.RuntimeOptions) { o.DebounceInterval = 0 }); err == nil {
		t.Fatal("expected burst without an interval to fail")
	}
	if err := newRuntime(func(o *chat.RuntimeOptions) {
		o.Dispatch = chat.DispatchSync
		o.DetachTimeout = 0
	}); err == nil {
		t.Fatal("expected burst under sync dispatch to fail")
	}
	// A detach timeout at or below the interval would abandon every window
	// before it closes.
	if err := newRuntime(func(o *chat.RuntimeOptions) {
		o.DebounceInterval = time.Second
		o.DetachTimeout = time.Second
	}); err == nil {
		t.Fatal("expected a detach timeout at or below the collection window to fail")
	}
}
