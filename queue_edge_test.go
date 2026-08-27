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

func newQueueRuntime(t *testing.T, state chat.State, adapter chat.Adapter, logs *syncBuffer, mutate ...func(*chat.RuntimeOptions)) *chat.Chat {
	t.Helper()
	options := chat.RuntimeOptions{
		DedupeTTL:     time.Hour,
		ThreadLockTTL: 40 * time.Millisecond,
		Concurrency:   chat.ConcurrencyQueue,
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
		t.Fatalf("new queue runtime: %v", err)
	}
	return bot
}

func TestQueueSingleFollowUpRunsAfterInflightHandler(t *testing.T) {
	t.Parallel()

	state := newFakeState()
	adapter := newFakeAdapter("fake")
	var logs syncBuffer
	bot := newQueueRuntime(t, state, adapter, &logs)

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
	case <-time.After(time.Second):
		t.Fatal("first handler did not start")
	}

	// One follow-up arrives while the first handler holds the lock. It must
	// queue, not drop.
	followDone := make(chan postEventResult, 1)
	go func() { followDone <- postEventResultFor(bot, "fake", mentionEvent("follow", "fake:v1:thread-1")) }()

	// Give the follow-up time to register as a queued waiter, then release first.
	time.Sleep(30 * time.Millisecond)
	close(releaseFirst)

	select {
	case res := <-followDone:
		if res.err != nil || res.status != http.StatusOK {
			t.Fatalf("follow-up result status=%d err=%v", res.status, res.err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("follow-up dispatch did not return")
	}

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
			mu.Lock()
			got := append([]string(nil), handled...)
			mu.Unlock()
			t.Fatalf("follow-up did not run after in-flight handler, handled = %v", got)
		case <-time.After(5 * time.Millisecond):
		}
	}

	time.Sleep(40 * time.Millisecond)
	mu.Lock()
	defer mu.Unlock()
	if len(handled) != 2 {
		t.Fatalf("handled = %v, want exactly first then follow", handled)
	}
	if handled[0] != "first" || handled[1] != "follow" {
		t.Fatalf("handled order = %v, want [first follow]", handled)
	}
	// A lone follow-up was never superseded, so no supersession should be logged.
	if strings.Contains(logs.String(), "chat queue superseded") {
		t.Fatalf("lone follow-up should not be superseded; logs:\n%s", logs.String())
	}
}

func TestQueueSupersededEventSurfacesSupersededByObservation(t *testing.T) {
	t.Parallel()

	state := newFakeState()
	adapter := newFakeAdapter("fake")
	var logs syncBuffer
	bot := newQueueRuntime(t, state, adapter, &logs, func(o *chat.RuntimeOptions) {
		o.ThreadLockTTL = 60 * time.Millisecond
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
	case <-time.After(time.Second):
		t.Fatal("first handler did not start")
	}

	// "older" queues first, then "newer" supersedes it.
	var wg sync.WaitGroup
	for _, id := range []string{"older", "newer"} {
		wg.Add(1)
		go func(id string) {
			defer wg.Done()
			res := postEventResultFor(bot, "fake", mentionEvent(id, "fake:v1:thread-1"))
			if res.err != nil || res.status != http.StatusOK {
				t.Errorf("follow-up %s status=%d err=%v", id, res.status, res.err)
			}
		}(id)
		time.Sleep(15 * time.Millisecond)
	}

	close(releaseFirst)
	wg.Wait()

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
			t.Fatalf("queued winner did not run, handled = %v", handled)
		case <-time.After(5 * time.Millisecond):
		}
	}
	time.Sleep(40 * time.Millisecond)

	mu.Lock()
	defer mu.Unlock()
	if len(handled) != 2 {
		t.Fatalf("handled = %v, want first then the newest follow-up only", handled)
	}
	if handled[1] != "newer" {
		t.Fatalf("queued winner = %q, want newer (most recent)", handled[1])
	}

	out := logs.String()
	if !strings.Contains(out, "chat queue superseded") {
		t.Fatalf("superseded event not surfaced via observation; logs:\n%s", out)
	}
	// The displacing event id must be carried on superseded_by.
	if !strings.Contains(out, "superseded_by=newer") {
		t.Fatalf("supersession did not carry superseded_by=newer; logs:\n%s", out)
	}
}

func TestQueueDropRemainsDefaultRegression(t *testing.T) {
	t.Parallel()

	state := newFakeState()
	adapter := newFakeAdapter("fake")
	var logs syncBuffer
	// Deferred dispatch but Concurrency left as the zero value (ConcurrencyDrop).
	bot, err := chat.New(context.Background(),
		chat.WithState(state),
		chat.WithAdapter(adapter),
		chat.WithLogger(slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))),
		chat.WithRuntimeOptions(chat.RuntimeOptions{
			DedupeTTL:     time.Hour,
			ThreadLockTTL: time.Hour,
			Dispatch:      chat.DispatchDeferred,
			MaxDetached:   1024,
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

	if status := postEvent(t, bot, "fake", mentionEvent("first", "fake:v1:thread-1")); status != http.StatusOK {
		t.Fatalf("first status = %d", status)
	}
	select {
	case <-firstStarted:
	case <-time.After(time.Second):
		t.Fatal("first handler did not start")
	}

	// Follow-up under drop: acked and dropped, never queued.
	if status := postEvent(t, bot, "fake", mentionEvent("dropped", "fake:v1:thread-1")); status != http.StatusOK {
		t.Fatalf("dropped follow-up status = %d", status)
	}

	mu.Lock()
	if len(handled) != 1 {
		got := append([]string(nil), handled...)
		mu.Unlock()
		t.Fatalf("drop strategy ran a follow-up: handled = %v", got)
	}
	mu.Unlock()

	out := logs.String()
	if !strings.Contains(out, "chat lock conflict dropped") {
		t.Fatalf("drop did not surface a lock-conflict-dropped observation; logs:\n%s", out)
	}
	if strings.Contains(out, "chat queue superseded") || strings.Contains(out, "chat queue wait abandoned") {
		t.Fatalf("queue observations leaked under the drop default; logs:\n%s", out)
	}

	close(releaseFirst)

	// Let the first handler finish; no second handler should ever run.
	deadline := time.Now().Add(200 * time.Millisecond)
	for time.Now().Before(deadline) {
		mu.Lock()
		n := len(handled)
		mu.Unlock()
		if n > 1 {
			t.Fatalf("dropped follow-up ran after first completed")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestQueueWaitAbandonedSurfacesObservation(t *testing.T) {
	t.Parallel()

	state := newFakeState()
	adapter := newFakeAdapter("fake")
	var logs syncBuffer
	bot := newQueueRuntime(t, state, adapter, &logs, func(o *chat.RuntimeOptions) {
		o.ThreadLockTTL = time.Hour
		// The detached waiter abandons at DetachTimeout; keep it short so the
		// abandonment fires quickly without relying on the request lifetime.
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

	// Ack is prompt (ack-then-work): the post returns 200 well before the
	// detached waiter abandons at DetachTimeout.
	if status := postEvent(t, bot, "fake", mentionEvent("abandoned", "fake:v1:thread-1")); status != http.StatusOK {
		t.Fatalf("abandoned queue wait status = %d, want 200", status)
	}

	// The abandonment must be observable once the Detached Work Context expires.
	deadline := time.After(2 * time.Second)
	for !strings.Contains(logs.String(), "chat queue wait abandoned") {
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

func TestQueueDeferredFollowUpAcksPromptlyWhileInflightWorks(t *testing.T) {
	t.Parallel()

	state := newFakeState()
	adapter := newFakeAdapter("fake")
	var logs syncBuffer
	bot := newQueueRuntime(t, state, adapter, &logs, func(o *chat.RuntimeOptions) {
		o.ThreadLockTTL = time.Hour
		o.DetachTimeout = 5 * time.Second
	})

	firstStarted := make(chan struct{})
	releaseFirst := make(chan struct{})
	defer close(releaseFirst)
	var mu sync.Mutex
	var handled []string
	bot.OnNewMention(func(ctx context.Context, ev *chat.MessageEvent) error {
		mu.Lock()
		first := len(handled) == 0
		handled = append(handled, ev.Event.ID)
		mu.Unlock()
		if first {
			close(firstStarted)
			<-releaseFirst // hold the lock indefinitely
		}
		return nil
	})

	if status := postEvent(t, bot, "fake", mentionEvent("first", "fake:v1:thread-1")); status != http.StatusOK {
		t.Fatalf("first status = %d", status)
	}
	select {
	case <-firstStarted:
	case <-time.After(time.Second):
		t.Fatal("first handler did not start")
	}

	// The follow-up's post (ack) must return promptly even though the in-flight
	// handler still holds the lock and never releases it. If the queue wait still
	// blocked the request, this post would not return until releaseFirst (never).
	acked := make(chan int, 1)
	go func() { acked <- postEvent(t, bot, "fake", mentionEvent("follow", "fake:v1:thread-1")) }()
	select {
	case status := <-acked:
		if status != http.StatusOK {
			t.Fatalf("queued follow-up ack status = %d, want 200", status)
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("queued follow-up ack blocked on the in-flight handler (not ack-then-work)")
	}

	// The follow-up's handler must NOT have run yet: it is waiting on the detached
	// context for the lock the first handler still holds.
	mu.Lock()
	if len(handled) != 1 {
		got := append([]string(nil), handled...)
		mu.Unlock()
		t.Fatalf("queued follow-up ran before the in-flight handler released, handled = %v", got)
	}
	mu.Unlock()
}

func TestQueueDeferredWaiterDrainedAndCancelledByShutdown(t *testing.T) {
	t.Parallel()

	state := newFakeState()
	adapter := newFakeAdapter("fake")
	var logs syncBuffer
	bot := newQueueRuntime(t, state, adapter, &logs, func(o *chat.RuntimeOptions) {
		o.ThreadLockTTL = time.Hour
		// Long enough that only Shutdown (not the timeout) can end the wait.
		o.DetachTimeout = time.Hour
	})

	var mu sync.Mutex
	var calls int
	bot.OnNewMention(func(ctx context.Context, ev *chat.MessageEvent) error {
		mu.Lock()
		calls++
		mu.Unlock()
		return nil
	})

	// Hold the lock with an outside token that never releases, so the queued
	// waiter can only ever be ended by Shutdown.
	if _, acquired, err := state.AcquireLock(context.Background(), "fake:v1:thread-1", time.Hour); err != nil || !acquired {
		t.Fatalf("seed lock acquired=%v err=%v", acquired, err)
	}

	if status := postEvent(t, bot, "fake", mentionEvent("waiter", "fake:v1:thread-1")); status != http.StatusOK {
		t.Fatalf("waiter status = %d", status)
	}

	// Give the detached waiter time to enter its poll loop.
	time.Sleep(40 * time.Millisecond)

	// Shutdown must return (drain completes) even though the lock never frees,
	// because baseCancel cancels the waiter's detached context and the inflight
	// WaitGroup tracks it.
	done := make(chan error, 1)
	go func() { done <- bot.Shutdown(context.Background()) }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("shutdown error: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("shutdown did not drain the queued waiter (not cancelled by baseCancel / not tracked by inflight)")
	}

	if !strings.Contains(logs.String(), "chat queue wait abandoned") {
		t.Fatalf("shutdown-cancelled waiter not surfaced as abandoned; logs:\n%s", logs.String())
	}
	mu.Lock()
	defer mu.Unlock()
	if calls != 0 {
		t.Fatalf("handler ran despite the lock never freeing, calls = %d", calls)
	}
}
