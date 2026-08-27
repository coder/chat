package chat_test

import (
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

func newPreemptRuntime(t *testing.T, state chat.State, adapter chat.Adapter, logs *syncBuffer, hook chat.LockConflictHook, mutate ...func(*chat.RuntimeOptions)) *chat.Chat {
	t.Helper()
	options := chat.RuntimeOptions{
		DedupeTTL:      time.Hour,
		ThreadLockTTL:  time.Hour,
		Concurrency:    chat.ConcurrencyDrop,
		Dispatch:       chat.DispatchDeferred,
		DetachTimeout:  5 * time.Second,
		OnLockConflict: hook,
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
		t.Fatalf("new preempt runtime: %v", err)
	}
	return bot
}

func TestPreemptCancelsInflightHandlerAndDispatchesNewEvent(t *testing.T) {
	t.Parallel()

	state := newFakeState()
	adapter := newFakeAdapter("fake")
	var logs syncBuffer
	bot := newPreemptRuntime(t, state, adapter, &logs, func(ctx context.Context, ev *chat.Event) bool {
		return true
	})

	firstStarted := make(chan struct{})
	firstCause := make(chan error, 1)
	var mu sync.Mutex
	var handled []string
	bot.OnNewMention(func(ctx context.Context, ev *chat.MessageEvent) error {
		mu.Lock()
		handled = append(handled, ev.Event.ID)
		mu.Unlock()
		if ev.Event.ID == "first" {
			close(firstStarted)
			// A preemptible handler observes cancellation and stops.
			<-ctx.Done()
			firstCause <- context.Cause(ctx)
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

	// The follow-up preempts: the in-flight handler is cancelled with
	// ErrPreempted and the new event dispatches under a fresh lease.
	if status := postEvent(t, bot, "fake", mentionEvent("preemptor", "fake:v1:thread-1")); status != http.StatusOK {
		t.Fatalf("preemptor status = %d", status)
	}

	select {
	case cause := <-firstCause:
		if !errors.Is(cause, chat.ErrPreempted) {
			t.Fatalf("first handler cancellation cause = %v, want chat.ErrPreempted", cause)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("in-flight handler was not cancelled by the preempting delivery")
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
			t.Fatal("preempting event did not dispatch")
		case <-time.After(5 * time.Millisecond):
		}
	}
	mu.Lock()
	if !equalStrings(handled, []string{"first", "preemptor"}) {
		got := append([]string(nil), handled...)
		mu.Unlock()
		t.Fatalf("handled = %v, want [first preemptor]", got)
	}
	mu.Unlock()

	deadline = time.After(2 * time.Second)
	for {
		out := logs.String()
		if strings.Contains(out, "chat lock preempted") && strings.Contains(out, "chat handler preempted") {
			break
		}
		select {
		case <-deadline:
			t.Fatalf("preemption not surfaced via observation; logs:\n%s", logs.String())
		case <-time.After(5 * time.Millisecond):
		}
	}

	// The victim's failed lock release is benign (its lease was force
	// released), never a WARN.
	time.Sleep(50 * time.Millisecond)
	if strings.Contains(logs.String(), `level=WARN msg="chat thread lock was not released"`) {
		t.Fatalf("preempted handler's release surfaced as WARN; logs:\n%s", logs.String())
	}
}

func TestPreemptWaitsForLocalVictimBeforeDispatching(t *testing.T) {
	t.Parallel()

	state := newFakeState()
	adapter := newFakeAdapter("fake")
	var logs syncBuffer
	bot := newPreemptRuntime(t, state, adapter, &logs, func(ctx context.Context, ev *chat.Event) bool {
		return true
	})

	firstStarted := make(chan struct{})
	var mu sync.Mutex
	var sequence []string
	bot.OnNewMention(func(ctx context.Context, ev *chat.MessageEvent) error {
		if ev.Event.ID == "first" {
			close(firstStarted)
			<-ctx.Done()
			// Cancellation-aware cleanup still runs; the preemptor must not
			// overlap it.
			time.Sleep(75 * time.Millisecond)
			mu.Lock()
			sequence = append(sequence, "victim-cleanup-done")
			mu.Unlock()
			return ctx.Err()
		}
		mu.Lock()
		sequence = append(sequence, "preemptor-start")
		mu.Unlock()
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

	if status := postEvent(t, bot, "fake", mentionEvent("preemptor", "fake:v1:thread-1")); status != http.StatusOK {
		t.Fatalf("preemptor status = %d", status)
	}

	deadline := time.After(2 * time.Second)
	for {
		mu.Lock()
		n := len(sequence)
		mu.Unlock()
		if n >= 2 {
			break
		}
		select {
		case <-deadline:
			mu.Lock()
			got := append([]string(nil), sequence...)
			mu.Unlock()
			t.Fatalf("preemption did not complete, sequence = %v", got)
		case <-time.After(5 * time.Millisecond):
		}
	}

	mu.Lock()
	defer mu.Unlock()
	if !equalStrings(sequence, []string{"victim-cleanup-done", "preemptor-start"}) {
		t.Fatalf("sequence = %v, want the local victim to finish before the preemptor dispatches", sequence)
	}
}

func TestPreemptHookDecliningFallsBackToDrop(t *testing.T) {
	t.Parallel()

	state := newFakeState()
	adapter := newFakeAdapter("fake")
	var logs syncBuffer
	var hookMu sync.Mutex
	hookCalls := 0
	bot := newPreemptRuntime(t, state, adapter, &logs, func(ctx context.Context, ev *chat.Event) bool {
		hookMu.Lock()
		hookCalls++
		hookMu.Unlock()
		return false
	})

	firstStarted := make(chan struct{})
	releaseFirst := make(chan struct{})
	var mu sync.Mutex
	var handled []string
	firstInterrupted := false
	bot.OnNewMention(func(ctx context.Context, ev *chat.MessageEvent) error {
		mu.Lock()
		first := len(handled) == 0
		handled = append(handled, ev.Event.ID)
		mu.Unlock()
		if first {
			close(firstStarted)
			<-releaseFirst
			if ctx.Err() != nil {
				mu.Lock()
				firstInterrupted = true
				mu.Unlock()
			}
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

	if status := postEvent(t, bot, "fake", mentionEvent("declined", "fake:v1:thread-1")); status != http.StatusOK {
		t.Fatalf("declined status = %d", status)
	}

	deadline := time.After(2 * time.Second)
	for !strings.Contains(logs.String(), "chat lock conflict dropped") {
		select {
		case <-deadline:
			t.Fatalf("declined conflict was not dropped; logs:\n%s", logs.String())
		case <-time.After(5 * time.Millisecond):
		}
	}
	hookMu.Lock()
	if hookCalls != 1 {
		hookMu.Unlock()
		t.Fatalf("hook consulted %d times, want 1", hookCalls)
	}
	hookMu.Unlock()

	close(releaseFirst)
	time.Sleep(50 * time.Millisecond)

	mu.Lock()
	defer mu.Unlock()
	if len(handled) != 1 || handled[0] != "first" {
		t.Fatalf("handled = %v, want only [first] with the follow-up dropped", handled)
	}
	if firstInterrupted {
		t.Fatal("declined preemption cancelled the in-flight handler")
	}
	if strings.Contains(logs.String(), "chat lock preempted") {
		t.Fatalf("declined hook still force released the lock; logs:\n%s", logs.String())
	}
}

func TestPreemptHookDecliningFallsBackToQueue(t *testing.T) {
	t.Parallel()

	state := newFakeState()
	adapter := newFakeAdapter("fake")
	var logs syncBuffer
	bot := newPreemptRuntime(t, state, adapter, &logs, func(ctx context.Context, ev *chat.Event) bool {
		return false
	}, func(o *chat.RuntimeOptions) {
		o.Concurrency = chat.ConcurrencyQueue
		o.ThreadLockTTL = 40 * time.Millisecond
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

	if status := postEvent(t, bot, "fake", mentionEvent("queued", "fake:v1:thread-1")); status != http.StatusOK {
		t.Fatalf("queued status = %d", status)
	}
	time.Sleep(30 * time.Millisecond)
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
			t.Fatal("declined preemption did not fall back to the queue strategy")
		case <-time.After(5 * time.Millisecond):
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if !equalStrings(handled, []string{"first", "queued"}) {
		t.Fatalf("handled = %v, want [first queued]", handled)
	}
}

func TestPreemptHookPanicIsRecoveredAndFallsBack(t *testing.T) {
	t.Parallel()

	state := newFakeState()
	adapter := newFakeAdapter("fake")
	var logs syncBuffer
	bot := newPreemptRuntime(t, state, adapter, &logs, func(ctx context.Context, ev *chat.Event) bool {
		panic("hook exploded")
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

	// A panicking hook is recovered and treated as declining: the conflict
	// drops and the runtime stays healthy.
	if status := postEvent(t, bot, "fake", mentionEvent("panicked", "fake:v1:thread-1")); status != http.StatusOK {
		t.Fatalf("panicked status = %d", status)
	}
	deadline := time.After(2 * time.Second)
	for {
		out := logs.String()
		if strings.Contains(out, "chat lock conflict hook panicked") && strings.Contains(out, "chat lock conflict dropped") {
			break
		}
		select {
		case <-deadline:
			t.Fatalf("panicking hook not recovered and dropped; logs:\n%s", logs.String())
		case <-time.After(5 * time.Millisecond):
		}
	}

	close(releaseFirst)
	time.Sleep(50 * time.Millisecond)

	// The runtime must still dispatch after the panic.
	if status := postEvent(t, bot, "fake", mentionEvent("after", "fake:v1:thread-1")); status != http.StatusOK {
		t.Fatalf("after status = %d", status)
	}
	deadline = time.After(2 * time.Second)
	for {
		mu.Lock()
		n := len(handled)
		mu.Unlock()
		if n >= 2 {
			break
		}
		select {
		case <-deadline:
			t.Fatal("runtime did not dispatch after a hook panic")
		case <-time.After(5 * time.Millisecond):
		}
	}
}

func TestPreemptHookNotConsultedForSelfEvents(t *testing.T) {
	t.Parallel()

	state := newFakeState()
	adapter := newFakeAdapter("fake")
	var logs syncBuffer
	var hookMu sync.Mutex
	hookCalls := 0
	bot := newPreemptRuntime(t, state, adapter, &logs, func(ctx context.Context, ev *chat.Event) bool {
		hookMu.Lock()
		hookCalls++
		hookMu.Unlock()
		return true
	})

	firstStarted := make(chan struct{})
	releaseFirst := make(chan struct{})
	defer close(releaseFirst)
	bot.OnNewMention(func(ctx context.Context, ev *chat.MessageEvent) error {
		close(firstStarted)
		select {
		case <-releaseFirst:
		case <-ctx.Done():
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

	// A self message during the conflict resolves as ignored: it must never
	// preempt the in-flight handler.
	self := mentionEvent("self", "fake:v1:thread-1")
	self.Message.Author = adapter.BotActor()
	if status := postEvent(t, bot, "fake", self); status != http.StatusOK {
		t.Fatalf("self status = %d", status)
	}

	deadline := time.After(2 * time.Second)
	for !strings.Contains(logs.String(), "chat ignored self message") {
		select {
		case <-deadline:
			t.Fatalf("self message not ignored; logs:\n%s", logs.String())
		case <-time.After(5 * time.Millisecond):
		}
	}
	hookMu.Lock()
	defer hookMu.Unlock()
	if hookCalls != 0 {
		t.Fatalf("hook consulted %d times for a self event, want 0", hookCalls)
	}
	if strings.Contains(logs.String(), "chat lock preempted") {
		t.Fatalf("self event force released the lock; logs:\n%s", logs.String())
	}
}

func TestPreemptConstructionValidation(t *testing.T) {
	t.Parallel()

	hook := func(ctx context.Context, ev *chat.Event) bool { return true }
	newRuntime := func(state chat.State, mutate func(*chat.RuntimeOptions)) error {
		options := chat.RuntimeOptions{
			DedupeTTL:      time.Hour,
			ThreadLockTTL:  time.Hour,
			Concurrency:    chat.ConcurrencyDrop,
			Dispatch:       chat.DispatchDeferred,
			DetachTimeout:  time.Second,
			OnLockConflict: hook,
		}
		mutate(&options)
		_, err := chat.New(context.Background(),
			chat.WithState(state),
			chat.WithAdapter(newFakeAdapter("fake")),
			chat.WithRuntimeOptions(options),
		)
		return err
	}

	if err := newRuntime(newFakeState(), func(o *chat.RuntimeOptions) {}); err != nil {
		t.Fatalf("valid preemption options rejected: %v", err)
	}
	if err := newRuntime(newFakeState(), func(o *chat.RuntimeOptions) {
		o.Dispatch = chat.DispatchSync
		o.DetachTimeout = 0
	}); err == nil {
		t.Fatal("expected the hook under sync dispatch to fail")
	}
	if err := newRuntime(newFakeState(), func(o *chat.RuntimeOptions) {
		o.Concurrency = chat.ConcurrencyConcurrent
		o.MaxConcurrent = 2
	}); err == nil {
		t.Fatal("expected the hook under the concurrent strategy to fail")
	}
	// A State without the LockForcer capability cannot support preemption.
	noForce := struct{ chat.State }{State: newFakeState()}
	if err := newRuntime(noForce, func(o *chat.RuntimeOptions) {}); err == nil {
		t.Fatal("expected the hook without a LockForcer state to fail")
	}
}
