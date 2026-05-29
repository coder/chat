package chat_test

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/coder/chat"
)

func newCommandRuntime(t *testing.T, state chat.State, adapter chat.Adapter) *chat.Chat {
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

func commandEvent(id string, threadID chat.ThreadID) chat.Event {
	return chat.Event{
		ID:       id,
		Adapter:  "fake",
		Tenant:   "tenant",
		ThreadID: threadID,
		Command: &chat.Command{
			Name:  "/deploy",
			Text:  "staging now",
			Args:  []string{"staging", "now"},
			Actor: chat.Actor{Adapter: "fake", Tenant: "tenant", ID: "user-1", BotKind: chat.BotHuman},
		},
	}
}

func interactionEvent(id string, threadID chat.ThreadID) chat.Event {
	return chat.Event{
		ID:       id,
		Adapter:  "fake",
		Tenant:   "tenant",
		ThreadID: threadID,
		Interaction: &chat.Interaction{
			Kind:     chat.InteractionBlockAction,
			ActionID: "approve",
			Actor:    chat.Actor{Adapter: "fake", Tenant: "tenant", ID: "user-1", BotKind: chat.BotHuman},
		},
	}
}

func TestCommandRoutesToOnCommandNeverMessageHooks(t *testing.T) {
	t.Parallel()

	bot := newCommandRuntime(t, newFakeState(), newFakeAdapter("fake"))

	var commands int
	bot.OnCommand(func(ctx context.Context, ev *chat.CommandEvent) error {
		commands++
		if ev.Command.Name != "/deploy" {
			t.Fatalf("command name = %q", ev.Command.Name)
		}
		if ev.Event.ID != "cmd-1" {
			t.Fatalf("event id = %q", ev.Event.ID)
		}
		if ev.Thread.ID() != "fake:v1:thread-1" {
			t.Fatalf("thread id = %q", ev.Thread.ID())
		}
		if len(ev.Command.Args) != 2 {
			t.Fatalf("args = %#v", ev.Command.Args)
		}
		return nil
	})
	bot.OnNewMention(func(context.Context, *chat.MessageEvent) error {
		t.Fatal("command must not route to OnNewMention")
		return nil
	})
	bot.OnSubscribedMessage(func(context.Context, *chat.MessageEvent) error {
		t.Fatal("command must not route to OnSubscribedMessage")
		return nil
	})
	bot.OnInteraction(func(context.Context, *chat.InteractionEvent) error {
		t.Fatal("command must not route to OnInteraction")
		return nil
	})

	if status := postEvent(t, bot, "fake", commandEvent("cmd-1", "fake:v1:thread-1")); status != http.StatusOK {
		t.Fatalf("status = %d", status)
	}
	if commands != 1 {
		t.Fatalf("commands = %d", commands)
	}
}

func TestCommandInSubscribedThreadStillRoutesToOnCommand(t *testing.T) {
	t.Parallel()

	state := newFakeState()
	bot := newCommandRuntime(t, state, newFakeAdapter("fake"))
	if err := state.SubscribeThread(context.Background(), "fake:v1:thread-1"); err != nil {
		t.Fatalf("subscribe: %v", err)
	}

	var commands int
	bot.OnCommand(func(context.Context, *chat.CommandEvent) error {
		commands++
		return nil
	})
	bot.OnSubscribedMessage(func(context.Context, *chat.MessageEvent) error {
		t.Fatal("command in subscribed thread must still route to OnCommand")
		return nil
	})

	if status := postEvent(t, bot, "fake", commandEvent("cmd-sub", "fake:v1:thread-1")); status != http.StatusOK {
		t.Fatalf("status = %d", status)
	}
	if commands != 1 {
		t.Fatalf("commands = %d", commands)
	}
}

func TestInteractionRoutesToOnInteraction(t *testing.T) {
	t.Parallel()

	bot := newCommandRuntime(t, newFakeState(), newFakeAdapter("fake"))

	var interactions int
	bot.OnInteraction(func(ctx context.Context, ev *chat.InteractionEvent) error {
		interactions++
		if ev.Interaction.ActionID != "approve" {
			t.Fatalf("action id = %q", ev.Interaction.ActionID)
		}
		if ev.Interaction.Kind != chat.InteractionBlockAction {
			t.Fatalf("kind = %v", ev.Interaction.Kind)
		}
		if ev.Thread.ID() != "fake:v1:thread-1" {
			t.Fatalf("thread id = %q", ev.Thread.ID())
		}
		return nil
	})
	bot.OnNewMention(func(context.Context, *chat.MessageEvent) error {
		t.Fatal("interaction must not route to a message hook")
		return nil
	})

	if status := postEvent(t, bot, "fake", interactionEvent("int-1", "fake:v1:thread-1")); status != http.StatusOK {
		t.Fatalf("status = %d", status)
	}
	if interactions != 1 {
		t.Fatalf("interactions = %d", interactions)
	}
}

func TestUnsetCommandAndInteractionHandlersAreNoOpAcks(t *testing.T) {
	t.Parallel()

	bot := newCommandRuntime(t, newFakeState(), newFakeAdapter("fake"))

	if status := postEvent(t, bot, "fake", commandEvent("cmd-noop", "fake:v1:thread-1")); status != http.StatusOK {
		t.Fatalf("command no-op status = %d", status)
	}
	if status := postEvent(t, bot, "fake", interactionEvent("int-noop", "fake:v1:thread-2")); status != http.StatusOK {
		t.Fatalf("interaction no-op status = %d", status)
	}
}

func TestEventWithNoPayloadStaysIgnored(t *testing.T) {
	t.Parallel()

	bot := newCommandRuntime(t, newFakeState(), newFakeAdapter("fake"))
	bot.OnCommand(func(context.Context, *chat.CommandEvent) error {
		t.Fatal("payload-less event must not route to OnCommand")
		return nil
	})
	bot.OnInteraction(func(context.Context, *chat.InteractionEvent) error {
		t.Fatal("payload-less event must not route to OnInteraction")
		return nil
	})

	ev := chat.Event{ID: "empty", Adapter: "fake", Tenant: "tenant", ThreadID: "fake:v1:thread-1"}
	if status := postEvent(t, bot, "fake", ev); status != http.StatusOK {
		t.Fatalf("status = %d", status)
	}
}

func TestCommandHandlerAtomicReplaceRaceSafe(t *testing.T) {
	t.Parallel()

	bot := newCommandRuntime(t, newFakeState(), newFakeAdapter("fake"))

	var mu sync.Mutex
	var routed []string
	bot.OnCommand(func(ctx context.Context, ev *chat.CommandEvent) error {
		mu.Lock()
		routed = append(routed, "first:"+ev.Event.ID)
		mu.Unlock()
		return nil
	})
	bot.OnCommand(func(ctx context.Context, ev *chat.CommandEvent) error {
		mu.Lock()
		routed = append(routed, "replacement:"+ev.Event.ID)
		mu.Unlock()
		return nil
	})

	if status := postEvent(t, bot, "fake", commandEvent("cmd-replace", "fake:v1:thread-1")); status != http.StatusOK {
		t.Fatalf("status = %d", status)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(routed) != 1 || routed[0] != "replacement:cmd-replace" {
		t.Fatalf("routed = %#v, want only the replacement", routed)
	}
}

func TestCommandAckedOnHandlerErrorAndDedupedAndSelfFiltered(t *testing.T) {
	t.Parallel()

	state := newFakeState()
	adapter := newFakeAdapter("fake")
	bot := newCommandRuntime(t, state, adapter)

	var calls int
	bot.OnCommand(func(context.Context, *chat.CommandEvent) error {
		calls++
		return errors.New("command handler error")
	})

	event := commandEvent("cmd-err", "fake:v1:thread-1")
	if status := postEvent(t, bot, "fake", event); status != http.StatusOK {
		t.Fatalf("handler-error status = %d", status)
	}
	// Duplicate by Event Identity: handler runs once.
	if status := postEvent(t, bot, "fake", event); status != http.StatusOK {
		t.Fatalf("duplicate status = %d", status)
	}

	// Self command (bot-issued) is filtered: no handler call, still acked.
	self := commandEvent("cmd-self", "fake:v1:thread-self")
	self.Command.Actor = adapter.BotActor()
	if status := postEvent(t, bot, "fake", self); status != http.StatusOK {
		t.Fatalf("self command status = %d", status)
	}

	if calls != 1 {
		t.Fatalf("calls = %d, want 1", calls)
	}
}

func TestInteractionSelfFilteredAndLockConflictDropped(t *testing.T) {
	t.Parallel()

	state := newFakeState()
	adapter := newFakeAdapter("fake")
	bot := newCommandRuntime(t, state, adapter)

	var calls int
	bot.OnInteraction(func(context.Context, *chat.InteractionEvent) error {
		calls++
		return nil
	})

	// Self interaction filtered.
	self := interactionEvent("int-self", "fake:v1:thread-self")
	self.Interaction.Actor = adapter.BotActor()
	if status := postEvent(t, bot, "fake", self); status != http.StatusOK {
		t.Fatalf("self interaction status = %d", status)
	}

	// Lock conflict: hold the lock, then post; acknowledge-and-drop.
	lease, acquired, err := state.AcquireLock(context.Background(), "fake:v1:thread-conflict", time.Hour)
	if err != nil || !acquired {
		t.Fatalf("acquire conflict lock acquired=%v err=%v", acquired, err)
	}
	if status := postEvent(t, bot, "fake", interactionEvent("int-conflict", "fake:v1:thread-conflict")); status != http.StatusOK {
		t.Fatalf("conflict status = %d", status)
	}
	if _, err := state.ReleaseLock(context.Background(), lease); err != nil {
		t.Fatalf("release: %v", err)
	}

	if calls != 0 {
		t.Fatalf("calls = %d, want 0", calls)
	}
}

func TestCommandRoutesThroughDeferredDetachedTail(t *testing.T) {
	t.Parallel()

	state := newFakeState()
	adapter := newFakeAdapter("fake")
	bot := newDeferredRuntime(t, state, adapter)

	handlerStarted := make(chan struct{})
	releaseHandler := make(chan struct{})
	handlerDone := make(chan struct{})
	bot.OnCommand(func(ctx context.Context, ev *chat.CommandEvent) error {
		close(handlerStarted)
		<-releaseHandler
		// State mutation under the detached work context must still succeed.
		if err := ev.Thread.Subscribe(ctx); err != nil {
			return err
		}
		close(handlerDone)
		return nil
	})

	if status := postEvent(t, bot, "fake", commandEvent("cmd-deferred", "fake:v1:thread-1")); status != http.StatusOK {
		t.Fatalf("status = %d", status)
	}

	select {
	case <-handlerStarted:
	case <-time.After(time.Second):
		t.Fatal("deferred command handler did not start")
	}
	// Ack returned before the handler completed (ack-then-work).
	select {
	case <-handlerDone:
		t.Fatal("handler completed before ack returned")
	default:
	}

	close(releaseHandler)
	select {
	case <-handlerDone:
	case <-time.After(time.Second):
		t.Fatal("deferred command handler did not finish")
	}
}
