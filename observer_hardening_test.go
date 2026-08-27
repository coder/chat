package chat_test

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/coder/chat"
)

// hardenObservedRuntime is a unique-named observed-runtime builder for the
// observer hardening cases, configurable with extra runtime-option mutators.
func hardenObservedRuntime(t *testing.T, state chat.State, adapter chat.Adapter, observer chat.Observer, mutate ...func(*chat.RuntimeOptions)) *chat.Chat {
	t.Helper()
	options := chat.RuntimeOptions{
		DedupeTTL:     time.Hour,
		ThreadLockTTL: time.Hour,
		Concurrency:   chat.ConcurrencyDrop,
	}
	for _, m := range mutate {
		m(&options)
	}
	bot, err := chat.New(context.Background(),
		chat.WithState(state),
		chat.WithAdapter(adapter),
		chat.WithObserver(observer),
		chat.WithRuntimeOptions(options),
	)
	if err != nil {
		t.Fatalf("new runtime: %v", err)
	}
	return bot
}

// TestObserverCommandAndInteractionIgnoredReasons proves the ObsIgnoredEvent
// reason attribute is correct for the non-message ignore branches: a missing
// handler and a self-issued command/interaction. ADR 0010 fixes the reason set;
// the existing observer test only covers message reasons.
func TestObserverCommandAndInteractionIgnoredReasons(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		setupBot   func(bot *chat.Chat, adapter *fakeAdapter)
		event      func(adapter *fakeAdapter) chat.Event
		wantReason string
	}{
		{
			name:     "no command handler",
			setupBot: func(bot *chat.Chat, _ *fakeAdapter) {},
			event: func(_ *fakeAdapter) chat.Event {
				return commandEvent("ign-cmd-nohandler", "fake:v1:thread-1")
			},
			wantReason: "no-command-handler",
		},
		{
			name:     "no interaction handler",
			setupBot: func(bot *chat.Chat, _ *fakeAdapter) {},
			event: func(_ *fakeAdapter) chat.Event {
				return interactionEvent("ign-int-nohandler", "fake:v1:thread-1")
			},
			wantReason: "no-interaction-handler",
		},
		{
			name: "self command",
			setupBot: func(bot *chat.Chat, _ *fakeAdapter) {
				bot.OnCommand(func(context.Context, *chat.CommandEvent) error { return nil })
			},
			event: func(adapter *fakeAdapter) chat.Event {
				e := commandEvent("ign-cmd-self", "fake:v1:thread-1")
				e.Command.Actor = adapter.BotActor()
				return e
			},
			wantReason: "self-command",
		},
		{
			name: "self interaction",
			setupBot: func(bot *chat.Chat, _ *fakeAdapter) {
				bot.OnInteraction(func(context.Context, *chat.InteractionEvent) error { return nil })
			},
			event: func(adapter *fakeAdapter) chat.Event {
				e := interactionEvent("ign-int-self", "fake:v1:thread-1")
				e.Interaction.Actor = adapter.BotActor()
				return e
			},
			wantReason: "self-interaction",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			obs := &recordingObserver{}
			adapter := newFakeAdapter("fake")
			bot := hardenObservedRuntime(t, newFakeState(), adapter, obs)
			tc.setupBot(bot, adapter)

			if status := postEvent(t, bot, "fake", tc.event(adapter)); status != http.StatusOK {
				t.Fatalf("status = %d", status)
			}
			assertReason(t, obs, tc.wantReason)
			if outcomes := obs.terminalOutcomes(); len(outcomes) != 1 || outcomes[0] != chat.OutcomeIgnored {
				t.Fatalf("outcomes = %#v, want [ignored]", outcomes)
			}
		})
	}
}

// TestObserverCommandHandlerErrorOutcome proves a failing Command Event handler
// emits ObsHandlerError with the command route and closes the span as error.
func TestObserverCommandHandlerErrorOutcome(t *testing.T) {
	t.Parallel()

	obs := &recordingObserver{}
	bot := hardenObservedRuntime(t, newFakeState(), newFakeAdapter("fake"), obs)
	bot.OnCommand(func(context.Context, *chat.CommandEvent) error {
		return errors.New("boom")
	})

	if status := postEvent(t, bot, "fake", commandEvent("obs-cmd-err", "fake:v1:thread-1")); status != http.StatusOK {
		t.Fatalf("status = %d", status)
	}
	if !obs.hasEvent(chat.ObsHandlerError) {
		t.Fatalf("missing handler error in %#v", obs.eventNames())
	}
	if outcomes := obs.terminalOutcomes(); len(outcomes) != 1 || outcomes[0] != chat.OutcomeError {
		t.Fatalf("outcomes = %#v, want [error]", outcomes)
	}
	assertRoutePresent(t, obs, "command")
}

// TestObserverDeferredCommandSpanFollowsDetachedTail proves the dispatch span
// for a deferred Command Event closes only after the detached handler completes,
// so Ack-Then-Work latency is measured to handler completion (ADR 0010 + 0002).
func TestObserverDeferredCommandSpanFollowsDetachedTail(t *testing.T) {
	t.Parallel()

	obs := &recordingObserver{}
	bot, err := chat.New(context.Background(),
		chat.WithState(newFakeState()),
		chat.WithAdapter(newFakeAdapter("fake")),
		chat.WithObserver(obs),
		chat.WithRuntimeOptions(chat.RuntimeOptions{
			DedupeTTL:     time.Hour,
			ThreadLockTTL: time.Hour,
			Concurrency:   chat.ConcurrencyDrop,
			Dispatch:      chat.DispatchDeferred,
			MaxDetached:   1024,
			DetachTimeout: 5 * time.Second,
		}),
	)
	if err != nil {
		t.Fatalf("new deferred observed runtime: %v", err)
	}

	started := make(chan struct{})
	release := make(chan struct{})
	bot.OnCommand(func(ctx context.Context, ev *chat.CommandEvent) error {
		close(started)
		<-release
		return nil
	})

	if status := postEvent(t, bot, "fake", commandEvent("obs-cmd-deferred", "fake:v1:thread-1")); status != http.StatusOK {
		t.Fatalf("status = %d", status)
	}
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("deferred command handler did not start")
	}
	// The span opened before ack but must not have closed yet.
	if outcomes := obs.terminalOutcomes(); len(outcomes) != 0 {
		t.Fatalf("span closed before handler finished: %#v", outcomes)
	}
	close(release)

	deadline := time.After(time.Second)
	for {
		if outcomes := obs.terminalOutcomes(); len(outcomes) == 1 {
			if outcomes[0] != chat.OutcomeHandled {
				t.Fatalf("outcome = %v, want handled", outcomes[0])
			}
			break
		}
		select {
		case <-deadline:
			t.Fatal("span did not close after detached handler completed")
		case <-time.After(time.Millisecond):
		}
	}
}

// TestObserverParityWithSlogDecisionPoints proves the Observation Hook fires at
// the same decision points the runtime logs via slog, so a log line and a metric
// cannot disagree (ADR 0010 parity test). It runs each branch with both an
// observer and a captured slog handler and asserts both record the branch.
func TestObserverParityWithSlogDecisionPoints(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		run       func(t *testing.T, bot *chat.Chat, state *fakeState, adapter *fakeAdapter)
		wantEvent chat.ObservationName
		wantLog   string
	}{
		{
			name: "dedupe hit",
			run: func(t *testing.T, bot *chat.Chat, _ *fakeState, _ *fakeAdapter) {
				bot.OnNewMention(func(context.Context, *chat.MessageEvent) error { return nil })
				ev := mentionEvent("parity-dup", "fake:v1:thread-1")
				postEvent(t, bot, "fake", ev)
				postEvent(t, bot, "fake", ev)
			},
			wantEvent: chat.ObsDedupeHit,
			wantLog:   "duplicate event dropped",
		},
		{
			name: "lock conflict",
			run: func(t *testing.T, bot *chat.Chat, state *fakeState, _ *fakeAdapter) {
				bot.OnNewMention(func(context.Context, *chat.MessageEvent) error { return nil })
				lease, _, err := state.AcquireLock(context.Background(), "fake:v1:thread-1", time.Hour)
				if err != nil {
					t.Fatalf("acquire: %v", err)
				}
				postEvent(t, bot, "fake", mentionEvent("parity-conflict", "fake:v1:thread-1"))
				_, _ = state.ReleaseLock(context.Background(), lease)
			},
			wantEvent: chat.ObsLockConflict,
			wantLog:   "lock conflict dropped",
		},
		{
			name: "ignored event",
			run: func(t *testing.T, bot *chat.Chat, _ *fakeState, _ *fakeAdapter) {
				bot.OnNewMention(func(context.Context, *chat.MessageEvent) error { return nil })
				postEvent(t, bot, "fake", chat.Event{ID: "parity-ign", Adapter: "fake", Tenant: "tenant", ThreadID: "fake:v1:thread-1"})
			},
			wantEvent: chat.ObsIgnoredEvent,
			wantLog:   "ignored non-message event",
		},
		{
			name: "handler error",
			run: func(t *testing.T, bot *chat.Chat, _ *fakeState, _ *fakeAdapter) {
				bot.OnNewMention(func(context.Context, *chat.MessageEvent) error { return errors.New("x") })
				postEvent(t, bot, "fake", mentionEvent("parity-err", "fake:v1:thread-1"))
			},
			wantEvent: chat.ObsHandlerError,
			wantLog:   "handler failed",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			obs := &recordingObserver{}
			state := newFakeState()
			adapter := newFakeAdapter("fake")
			logs := newSyncBuffer()
			bot, err := chat.New(context.Background(),
				chat.WithState(state),
				chat.WithAdapter(adapter),
				chat.WithObserver(obs),
				chat.WithLogger(slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))),
				chat.WithRuntimeOptions(chat.RuntimeOptions{
					DedupeTTL:     time.Hour,
					ThreadLockTTL: time.Hour,
					Concurrency:   chat.ConcurrencyDrop,
				}),
			)
			if err != nil {
				t.Fatalf("new runtime: %v", err)
			}

			tc.run(t, bot, state, adapter)

			if !obs.hasEvent(tc.wantEvent) {
				t.Fatalf("observer missing %q in %#v", tc.wantEvent, obs.eventNames())
			}
			if !strings.Contains(logs.String(), tc.wantLog) {
				t.Fatalf("slog missing %q; got:\n%s", tc.wantLog, logs.String())
			}
		})
	}
}

// TestAdapterAsAbsentCapabilityAndWrongType proves typed Adapter Access returns a
// no-panic explicit-false result for a wrong type, a missing adapter, and a nil
// runtime, which is the absent-Optional-Capability contract (ADR 0004 / 0010).
func TestAdapterAsAbsentCapabilityAndWrongType(t *testing.T) {
	t.Parallel()

	bot := hardenCommandRuntime(t, newFakeState(), newFakeAdapter("fake"))

	// The fake adapter does not implement NativeContentPoster: absent capability.
	if _, ok := chat.AdapterAs[chat.NativeContentPoster](bot, "fake"); ok {
		t.Fatal("fake adapter must not satisfy NativeContentPoster")
	}
	// Missing adapter name.
	if _, ok := chat.AdapterAs[chat.NativeContentPoster](bot, "nope"); ok {
		t.Fatal("missing adapter must return false")
	}
	// Nil runtime must not panic.
	if _, ok := chat.AdapterAs[chat.NativeContentPoster](nil, "fake"); ok {
		t.Fatal("nil runtime must return false")
	}
}

// TestCoreModuleHasNoOTelDependency enforces the load-bearing ADR 0010 invariant:
// github.com/coder/chat must not pull OpenTelemetry, Prometheus, or statsd into
// its import graph. It is the module-graph check the PRD calls for.
func TestCoreModuleHasNoOTelDependency(t *testing.T) {
	t.Parallel()

	out, err := exec.Command("go", "list", "-deps", "github.com/coder/chat").CombinedOutput()
	if err != nil {
		t.Fatalf("go list -deps: %v\n%s", err, out)
	}
	for _, banned := range []string{
		"go.opentelemetry.io",
		"opentelemetry",
		"prometheus",
		"statsd",
	} {
		if strings.Contains(string(out), banned) {
			t.Fatalf("core module import graph contains %q, violating the no-hard-dependency invariant", banned)
		}
	}
}
