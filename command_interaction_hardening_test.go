package chat_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/coder/chat"
)

// hardenCommandRuntime builds a sync-dispatch runtime for command/interaction
// hardening cases. The name is unique to avoid clobbering newCommandRuntime.
func hardenCommandRuntime(t *testing.T, state chat.State, adapter chat.Adapter) *chat.Chat {
	t.Helper()
	bot, err := chat.New(context.Background(),
		chat.WithState(state),
		chat.WithAdapter(adapter),
		chat.WithRuntimeOptions(chat.RuntimeOptions{
			DedupeTTL:     time.Hour,
			ThreadLockTTL: time.Hour,
			Concurrency:   chat.ConcurrencyDrop,
		}),
	)
	if err != nil {
		t.Fatalf("new runtime: %v", err)
	}
	return bot
}

// TestCommandAndInteractionDedupedByEventIdentity proves a re-delivered Command
// Event / Interaction Event (same Event Identity) runs the handler once. PRDs
// 0003 and 0004 both list Event Identity dedupe for the non-message kinds.
func TestCommandAndInteractionDedupedByEventIdentity(t *testing.T) {
	t.Parallel()

	bot := hardenCommandRuntime(t, newFakeState(), newFakeAdapter("fake"))

	var commands, interactions int
	bot.OnCommand(func(context.Context, *chat.CommandEvent) error {
		commands++
		return nil
	})
	bot.OnInteraction(func(context.Context, *chat.InteractionEvent) error {
		interactions++
		return nil
	})

	cmd := commandEvent("cmd-dup", "fake:v1:thread-cmd")
	for i := 0; i < 3; i++ {
		if status := postEvent(t, bot, "fake", cmd); status != http.StatusOK {
			t.Fatalf("command delivery %d status = %d", i, status)
		}
	}
	intr := interactionEvent("int-dup", "fake:v1:thread-int")
	for i := 0; i < 3; i++ {
		if status := postEvent(t, bot, "fake", intr); status != http.StatusOK {
			t.Fatalf("interaction delivery %d status = %d", i, status)
		}
	}

	if commands != 1 {
		t.Fatalf("commands = %d, want 1 (deduped by Event Identity)", commands)
	}
	if interactions != 1 {
		t.Fatalf("interactions = %d, want 1 (deduped by Event Identity)", interactions)
	}
}

// TestCommandSerializesWithConcurrentMessageOnSameThread proves a Command Event
// and a Message Event on the same Thread share the per-Thread Lock: the command
// landing while a message handler holds the lock is acknowledge-and-dropped under
// the default drop strategy. Mixed-kind serialization is called for by PRD 0004.
func TestCommandSerializesWithConcurrentMessageOnSameThread(t *testing.T) {
	t.Parallel()

	state := newFakeState()
	bot := hardenCommandRuntime(t, state, newFakeAdapter("fake"))

	var commandCalls int
	bot.OnCommand(func(context.Context, *chat.CommandEvent) error {
		commandCalls++
		return nil
	})
	bot.OnNewMention(func(context.Context, *chat.MessageEvent) error {
		t.Fatal("message handler should not run while its lock is externally held")
		return nil
	})

	// Hold the shared Thread Lock as if a message handler were mid-flight, then
	// post a command for the same Thread: it must be dropped, still acked.
	lease, acquired, err := state.AcquireLock(context.Background(), "fake:v1:shared-thread", time.Hour)
	if err != nil || !acquired {
		t.Fatalf("acquire shared lock acquired=%v err=%v", acquired, err)
	}
	if status := postEvent(t, bot, "fake", commandEvent("cmd-blocked", "fake:v1:shared-thread")); status != http.StatusOK {
		t.Fatalf("blocked command status = %d", status)
	}
	if _, err := state.ReleaseLock(context.Background(), lease); err != nil {
		t.Fatalf("release: %v", err)
	}
	if commandCalls != 0 {
		t.Fatalf("command calls = %d, want 0 (dropped on lock conflict)", commandCalls)
	}

	// With the lock free, the same kind on the same Thread now runs.
	if status := postEvent(t, bot, "fake", commandEvent("cmd-free", "fake:v1:shared-thread")); status != http.StatusOK {
		t.Fatalf("free command status = %d", status)
	}
	if commandCalls != 1 {
		t.Fatalf("command calls = %d, want 1 after lock free", commandCalls)
	}
}

// TestInteractionHandlerAtomicReplaceRaceSafe mirrors the OnCommand atomic-replace
// test for OnInteraction, which PRD 0004 lists for all the new hooks.
func TestInteractionHandlerAtomicReplaceRaceSafe(t *testing.T) {
	t.Parallel()

	bot := hardenCommandRuntime(t, newFakeState(), newFakeAdapter("fake"))

	var mu sync.Mutex
	var routed []string
	bot.OnInteraction(func(ctx context.Context, ev *chat.InteractionEvent) error {
		mu.Lock()
		routed = append(routed, "first:"+ev.Event.ID)
		mu.Unlock()
		return nil
	})
	bot.OnInteraction(func(ctx context.Context, ev *chat.InteractionEvent) error {
		mu.Lock()
		routed = append(routed, "replacement:"+ev.Event.ID)
		mu.Unlock()
		return nil
	})

	if status := postEvent(t, bot, "fake", interactionEvent("int-replace", "fake:v1:thread-1")); status != http.StatusOK {
		t.Fatalf("status = %d", status)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(routed) != 1 || routed[0] != "replacement:int-replace" {
		t.Fatalf("routed = %#v, want only the replacement", routed)
	}
}

// TestInteractionAckedOnHandlerError proves an Interaction Event is an Accepted
// Event acknowledged even when its handler fails (mirrors the command case).
func TestInteractionAckedOnHandlerError(t *testing.T) {
	t.Parallel()

	bot := hardenCommandRuntime(t, newFakeState(), newFakeAdapter("fake"))

	var calls int
	bot.OnInteraction(func(context.Context, *chat.InteractionEvent) error {
		calls++
		return errors.New("interaction handler error")
	})

	if status := postEvent(t, bot, "fake", interactionEvent("int-err", "fake:v1:thread-1")); status != http.StatusOK {
		t.Fatalf("handler-error status = %d, want acked 200", status)
	}
	if calls != 1 {
		t.Fatalf("calls = %d, want 1", calls)
	}
}

// TestUnsetInteractionHandlerWithCommandHandlerSetIsNoOpAck proves the missing
// hook is the only thing that no-ops: a command handler being set does not make
// an interaction with no handler route to it.
func TestUnsetInteractionHandlerWithCommandHandlerSetIsNoOpAck(t *testing.T) {
	t.Parallel()

	bot := hardenCommandRuntime(t, newFakeState(), newFakeAdapter("fake"))
	bot.OnCommand(func(context.Context, *chat.CommandEvent) error {
		t.Fatal("interaction must not fall through to the command hook")
		return nil
	})

	if status := postEvent(t, bot, "fake", interactionEvent("int-noslot", "fake:v1:thread-x")); status != http.StatusOK {
		t.Fatalf("interaction no-handler status = %d", status)
	}
}

// TestCommandContextCancellationUnderSyncDispatch proves a Command Event handler
// runs under the request context, so cancelling that context surfaces to the
// handler; the event is still acknowledged. PRD 0003 lists context cancellation.
func TestCommandContextCancellationUnderSyncDispatch(t *testing.T) {
	t.Parallel()

	bot := hardenCommandRuntime(t, newFakeState(), newFakeAdapter("fake"))

	ctx, cancel := context.WithCancel(context.Background())
	handlerStarted := make(chan struct{})
	handlerSawCancel := make(chan error, 1)
	bot.OnCommand(func(hctx context.Context, ev *chat.CommandEvent) error {
		// The handler runs under the request context: signal it started (so the
		// test cancels only after the prelude completed and the handler is live),
		// then block until that context is cancelled.
		close(handlerStarted)
		<-hctx.Done()
		handlerSawCancel <- hctx.Err()
		return hctx.Err()
	})

	handler, err := bot.Webhook("fake")
	if err != nil {
		t.Fatalf("webhook: %v", err)
	}

	done := make(chan int, 1)
	go func() {
		done <- serveEventWithContext(handler, ctx, commandEvent("cmd-cancel", "fake:v1:thread-cancel"))
	}()

	select {
	case <-handlerStarted:
	case <-time.After(time.Second):
		t.Fatal("command handler did not start under the request context")
	}
	cancel()

	select {
	case cerr := <-handlerSawCancel:
		if !errors.Is(cerr, context.Canceled) {
			t.Fatalf("handler ctx err = %v, want context.Canceled", cerr)
		}
	case <-time.After(time.Second):
		t.Fatal("command handler did not observe cancellation")
	}
	select {
	case status := <-done:
		// The adapter still acknowledges (the runtime swallows handler errors).
		if status != http.StatusOK {
			t.Fatalf("status after cancellation = %d, want 200", status)
		}
	case <-time.After(time.Second):
		t.Fatal("request did not complete after cancellation")
	}
}

// TestInteractionRoutesThroughDeferredDetachedTail proves the Interaction Event
// kind also rides the ADR 0002 detached tail: the ack returns before the handler
// completes and the handler runs under the Detached Work Context (proven by a
// State mutation succeeding after ack). The command case is already covered by
// TestCommandRoutesThroughDeferredDetachedTail.
func TestInteractionRoutesThroughDeferredDetachedTail(t *testing.T) {
	t.Parallel()

	state := newFakeState()
	bot := newDeferredRuntime(t, state, newFakeAdapter("fake"))

	started := make(chan struct{})
	release := make(chan struct{})
	done := make(chan struct{})
	bot.OnInteraction(func(ctx context.Context, ev *chat.InteractionEvent) error {
		close(started)
		<-release
		if err := ev.Thread.Subscribe(ctx); err != nil {
			return err
		}
		close(done)
		return nil
	})

	if status := postEvent(t, bot, "fake", interactionEvent("int-deferred", "fake:v1:thread-i")); status != http.StatusOK {
		t.Fatalf("status = %d", status)
	}
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("deferred interaction handler did not start")
	}
	select {
	case <-done:
		t.Fatal("handler completed before ack returned")
	default:
	}
	close(release)
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("deferred interaction handler did not finish")
	}
}

// TestQueuedCommandRunsAfterInFlightHandler proves the ADR 0003 / 0004 guidance
// that interactive bots should select the queue Concurrency Strategy: a Command
// Event landing while a deferred handler holds the Thread Lock is queued and run
// after the in-flight turn, not dropped. The foundation queue tests cover messages;
// this evidences the queue path for the new Command Event kind.
func TestQueuedCommandRunsAfterInFlightHandler(t *testing.T) {
	t.Parallel()

	bot, err := chat.New(context.Background(),
		chat.WithState(newFakeState()),
		chat.WithAdapter(newFakeAdapter("fake")),
		chat.WithRuntimeOptions(chat.RuntimeOptions{
			DedupeTTL:     time.Hour,
			ThreadLockTTL: time.Hour,
			Concurrency:   chat.ConcurrencyQueue,
			Dispatch:      chat.DispatchDeferred,
			DetachTimeout: 5 * time.Second,
		}),
	)
	if err != nil {
		t.Fatalf("new runtime: %v", err)
	}

	mentionStarted := make(chan struct{})
	releaseMention := make(chan struct{})
	commandDone := make(chan struct{})
	bot.OnNewMention(func(ctx context.Context, ev *chat.MessageEvent) error {
		close(mentionStarted)
		<-releaseMention
		return nil
	})
	bot.OnCommand(func(ctx context.Context, ev *chat.CommandEvent) error {
		close(commandDone)
		return nil
	})

	// The mention acquires the Thread Lock and blocks the detached tail.
	if status := postEvent(t, bot, "fake", mentionEvent("queue-mention", "fake:v1:thread-q")); status != http.StatusOK {
		t.Fatalf("mention status = %d", status)
	}
	select {
	case <-mentionStarted:
	case <-time.After(time.Second):
		t.Fatal("mention handler did not start")
	}

	// The command lands on the same Thread mid-turn: under queue it is registered as
	// pending rather than dropped, so its handler has not run yet.
	cmdDone := make(chan struct{})
	go func() {
		defer close(cmdDone)
		res := postEventResultFor(bot, "fake", commandEvent("queue-command", "fake:v1:thread-q"))
		if res.err != nil || res.status != http.StatusOK {
			t.Errorf("queued command status=%d err=%v", res.status, res.err)
		}
	}()
	select {
	case <-commandDone:
		t.Fatal("queued command ran before the in-flight handler released the lock")
	case <-time.After(50 * time.Millisecond):
	}

	// Release the mention; the queued command now acquires the lock and runs.
	close(releaseMention)
	select {
	case <-commandDone:
	case <-time.After(2 * time.Second):
		t.Fatal("queued command did not run after the in-flight handler completed")
	}
	<-cmdDone
}

// serveEventWithContext posts ev through handler under ctx and returns the HTTP
// status. It mirrors postEventResultFor but lets a test supply its own context
// so cancellation is observable end to end.
func serveEventWithContext(handler http.Handler, ctx context.Context, ev chat.Event) int {
	body, err := json.Marshal(ev)
	if err != nil {
		return -1
	}
	req := httptest.NewRequest(http.MethodPost, "/webhook", bytes.NewReader(body)).WithContext(ctx)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	return rec.Code
}
