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

// recordingObserver captures Event calls and dispatch span open/close for
// assertions. It is the Observer test double called for in ADR 0010.
type recordingObserver struct {
	mu       sync.Mutex
	events   []recordedEvent
	opened   int
	outcomes []chat.DispatchOutcome
	attrs    [][]chat.Attr
}

type recordedEvent struct {
	name  chat.ObservationName
	attrs []chat.Attr
}

func (o *recordingObserver) Event(_ context.Context, name chat.ObservationName, attrs ...chat.Attr) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.events = append(o.events, recordedEvent{name: name, attrs: append([]chat.Attr(nil), attrs...)})
}

func (o *recordingObserver) Dispatch(ctx context.Context, attrs ...chat.Attr) (context.Context, chat.DispatchSpan) {
	o.mu.Lock()
	o.opened++
	o.mu.Unlock()
	return ctx, &recordingSpan{observer: o, openAttrs: append([]chat.Attr(nil), attrs...)}
}

func (o *recordingObserver) eventNames() []chat.ObservationName {
	o.mu.Lock()
	defer o.mu.Unlock()
	names := make([]chat.ObservationName, 0, len(o.events))
	for _, e := range o.events {
		names = append(names, e.name)
	}
	return names
}

func (o *recordingObserver) hasEvent(name chat.ObservationName) bool {
	for _, got := range o.eventNames() {
		if got == name {
			return true
		}
	}
	return false
}

func (o *recordingObserver) terminalOutcomes() []chat.DispatchOutcome {
	o.mu.Lock()
	defer o.mu.Unlock()
	return append([]chat.DispatchOutcome(nil), o.outcomes...)
}

type recordingSpan struct {
	observer  *recordingObserver
	openAttrs []chat.Attr
}

func (s *recordingSpan) End(outcome chat.DispatchOutcome, attrs ...chat.Attr) {
	s.observer.mu.Lock()
	defer s.observer.mu.Unlock()
	s.observer.outcomes = append(s.observer.outcomes, outcome)
	all := append(append([]chat.Attr(nil), s.openAttrs...), attrs...)
	s.observer.attrs = append(s.observer.attrs, all)
}

func newObservedRuntime(t *testing.T, state chat.State, adapter chat.Adapter, observer chat.Observer) *chat.Chat {
	t.Helper()
	bot, err := chat.New(context.Background(),
		chat.WithState(state),
		chat.WithAdapter(adapter),
		chat.WithObserver(observer),
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

func TestObserverHandledDispatchOpensAndClosesOneSpan(t *testing.T) {
	t.Parallel()

	obs := &recordingObserver{}
	bot := newObservedRuntime(t, newFakeState(), newFakeAdapter("fake"), obs)
	bot.OnNewMention(func(ctx context.Context, ev *chat.MessageEvent) error {
		return nil
	})

	if status := postEvent(t, bot, "fake", mentionEvent("h1", "fake:v1:thread-1")); status != http.StatusOK {
		t.Fatalf("status = %d", status)
	}
	if obs.opened != 1 {
		t.Fatalf("spans opened = %d, want 1", obs.opened)
	}
	if outcomes := obs.terminalOutcomes(); len(outcomes) != 1 || outcomes[0] != chat.OutcomeHandled {
		t.Fatalf("outcomes = %#v, want [handled]", outcomes)
	}
}

func TestObserverDuplicateLockConflictAndIgnoredReasons(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		setup       func(t *testing.T, state *fakeState, bot *chat.Chat)
		event       chat.Event
		wantEvent   chat.ObservationName
		wantOutcome chat.DispatchOutcome
	}{
		{
			name:        "non-message ignored",
			event:       chat.Event{ID: "ign-nonmsg", Adapter: "fake", Tenant: "tenant", ThreadID: "fake:v1:thread-1"},
			wantEvent:   chat.ObsIgnoredEvent,
			wantOutcome: chat.OutcomeIgnored,
		},
		{
			name: "unrouted ignored",
			event: func() chat.Event {
				e := mentionEvent("ign-unrouted", "fake:v1:thread-1")
				e.Message.Mentioned = false
				return e
			}(),
			wantEvent:   chat.ObsIgnoredEvent,
			wantOutcome: chat.OutcomeIgnored,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			obs := &recordingObserver{}
			bot := newObservedRuntime(t, newFakeState(), newFakeAdapter("fake"), obs)
			bot.OnSubscribedMessage(func(context.Context, *chat.MessageEvent) error { return nil })

			if status := postEvent(t, bot, "fake", tc.event); status != http.StatusOK {
				t.Fatalf("status = %d", status)
			}
			if !obs.hasEvent(tc.wantEvent) {
				t.Fatalf("missing %q in %#v", tc.wantEvent, obs.eventNames())
			}
			if outcomes := obs.terminalOutcomes(); len(outcomes) != 1 || outcomes[0] != tc.wantOutcome {
				t.Fatalf("outcomes = %#v, want [%s]", outcomes, tc.wantOutcome)
			}
		})
	}
}

func TestObserverSelfMessageIgnoredReason(t *testing.T) {
	t.Parallel()

	obs := &recordingObserver{}
	adapter := newFakeAdapter("fake")
	bot := newObservedRuntime(t, newFakeState(), adapter, obs)
	bot.OnNewMention(func(context.Context, *chat.MessageEvent) error { return nil })

	self := mentionEvent("self", "fake:v1:thread-1")
	self.Message.Author = adapter.BotActor()
	if status := postEvent(t, bot, "fake", self); status != http.StatusOK {
		t.Fatalf("status = %d", status)
	}
	assertReason(t, obs, "self-message")
	if outcomes := obs.terminalOutcomes(); len(outcomes) != 1 || outcomes[0] != chat.OutcomeIgnored {
		t.Fatalf("outcomes = %#v", outcomes)
	}
}

func TestObserverDuplicate(t *testing.T) {
	t.Parallel()

	obs := &recordingObserver{}
	bot := newObservedRuntime(t, newFakeState(), newFakeAdapter("fake"), obs)
	bot.OnNewMention(func(context.Context, *chat.MessageEvent) error { return nil })

	event := mentionEvent("dup", "fake:v1:thread-1")
	postEvent(t, bot, "fake", event)
	postEvent(t, bot, "fake", event)

	if !obs.hasEvent(chat.ObsDedupeHit) {
		t.Fatalf("missing dedupe hit in %#v", obs.eventNames())
	}
	outcomes := obs.terminalOutcomes()
	if len(outcomes) != 2 || outcomes[1] != chat.OutcomeDuplicate {
		t.Fatalf("outcomes = %#v, want second duplicate", outcomes)
	}
}

func TestObserverLockConflict(t *testing.T) {
	t.Parallel()

	obs := &recordingObserver{}
	state := newFakeState()
	bot := newObservedRuntime(t, state, newFakeAdapter("fake"), obs)
	bot.OnNewMention(func(context.Context, *chat.MessageEvent) error { return nil })

	lease, _, err := state.AcquireLock(context.Background(), "fake:v1:thread-1", time.Hour)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	if status := postEvent(t, bot, "fake", mentionEvent("conflict", "fake:v1:thread-1")); status != http.StatusOK {
		t.Fatalf("status = %d", status)
	}
	if _, err := state.ReleaseLock(context.Background(), lease); err != nil {
		t.Fatalf("release: %v", err)
	}

	if !obs.hasEvent(chat.ObsLockConflict) {
		t.Fatalf("missing lock conflict in %#v", obs.eventNames())
	}
	if outcomes := obs.terminalOutcomes(); len(outcomes) != 1 || outcomes[0] != chat.OutcomeDroppedLockConflict {
		t.Fatalf("outcomes = %#v, want [dropped-lock-conflict]", outcomes)
	}
}

func TestObserverHandlerError(t *testing.T) {
	t.Parallel()

	obs := &recordingObserver{}
	bot := newObservedRuntime(t, newFakeState(), newFakeAdapter("fake"), obs)
	bot.OnNewMention(func(context.Context, *chat.MessageEvent) error {
		return errors.New("handler error")
	})

	if status := postEvent(t, bot, "fake", mentionEvent("err", "fake:v1:thread-1")); status != http.StatusOK {
		t.Fatalf("status = %d", status)
	}
	if !obs.hasEvent(chat.ObsHandlerError) {
		t.Fatalf("missing handler error in %#v", obs.eventNames())
	}
	if outcomes := obs.terminalOutcomes(); len(outcomes) != 1 || outcomes[0] != chat.OutcomeError {
		t.Fatalf("outcomes = %#v, want [error]", outcomes)
	}
}

func TestObserverCommandAndInteractionRoutes(t *testing.T) {
	t.Parallel()

	obs := &recordingObserver{}
	bot := newObservedRuntime(t, newFakeState(), newFakeAdapter("fake"), obs)
	bot.OnCommand(func(context.Context, *chat.CommandEvent) error { return nil })
	bot.OnInteraction(func(context.Context, *chat.InteractionEvent) error { return nil })

	postEvent(t, bot, "fake", commandEvent("c1", "fake:v1:thread-1"))
	postEvent(t, bot, "fake", interactionEvent("i1", "fake:v1:thread-2"))

	if outcomes := obs.terminalOutcomes(); len(outcomes) != 2 ||
		outcomes[0] != chat.OutcomeHandled || outcomes[1] != chat.OutcomeHandled {
		t.Fatalf("outcomes = %#v, want two handled", outcomes)
	}
	assertRoutePresent(t, obs, "command")
	assertRoutePresent(t, obs, "interaction")
}

func TestDefaultNoOpObserverDoesNotChangeBehavior(t *testing.T) {
	t.Parallel()

	// No WithObserver: the no-op default must leave routing identical.
	bot, err := chat.New(context.Background(),
		chat.WithState(newFakeState()),
		chat.WithAdapter(newFakeAdapter("fake")),
	)
	if err != nil {
		t.Fatalf("new runtime: %v", err)
	}
	var calls int
	bot.OnNewMention(func(context.Context, *chat.MessageEvent) error {
		calls++
		return nil
	})
	if status := postEvent(t, bot, "fake", mentionEvent("noop", "fake:v1:thread-1")); status != http.StatusOK {
		t.Fatalf("status = %d", status)
	}
	if calls != 1 {
		t.Fatalf("calls = %d", calls)
	}
}

// panicObserver panics on every call; it must never fail an Accepted Event or
// alter acknowledgement.
type panicObserver struct{}

func (panicObserver) Event(context.Context, chat.ObservationName, ...chat.Attr) {
	panic("observer event panic")
}

func (panicObserver) Dispatch(context.Context, ...chat.Attr) (context.Context, chat.DispatchSpan) {
	panic("observer dispatch panic")
}

func TestPanickingObserverDoesNotAffectAck(t *testing.T) {
	t.Parallel()

	bot := newObservedRuntime(t, newFakeState(), newFakeAdapter("fake"), panicObserver{})
	var calls int
	bot.OnNewMention(func(context.Context, *chat.MessageEvent) error {
		calls++
		return nil
	})

	if status := postEvent(t, bot, "fake", mentionEvent("panic", "fake:v1:thread-1")); status != http.StatusOK {
		t.Fatalf("status = %d, panicking observer must not change ack", status)
	}
	if calls != 1 {
		t.Fatalf("calls = %d, want 1", calls)
	}
}

func TestObserverAttributeHygiene(t *testing.T) {
	t.Parallel()

	obs := &recordingObserver{}
	bot := newObservedRuntime(t, newFakeState(), newFakeAdapter("fake"), obs)
	bot.OnNewMention(func(context.Context, *chat.MessageEvent) error { return nil })

	threadID := "fake:v1:thread-1"
	event := mentionEvent("hygiene", chat.ThreadID(threadID))
	if status := postEvent(t, bot, "fake", event); status != http.StatusOK {
		t.Fatalf("status = %d", status)
	}

	allowedKeys := map[string]bool{
		chat.AttrAdapter: true,
		chat.AttrRoute:   true,
		chat.AttrReason:  true,
		chat.AttrOutcome: true,
		chat.AttrTenant:  true,
	}

	obs.mu.Lock()
	defer obs.mu.Unlock()
	var attrSets [][]chat.Attr
	for _, e := range obs.events {
		attrSets = append(attrSets, e.attrs)
	}
	attrSets = append(attrSets, obs.attrs...)

	for _, set := range attrSets {
		for _, a := range set {
			if !allowedKeys[a.Key] {
				t.Fatalf("attribute key %q outside documented set", a.Key)
			}
			if a.Value == threadID {
				t.Fatalf("thread id leaked into attribute %q", a.Key)
			}
			if a.Value == event.Message.Text || a.Value == event.Message.Author.ID {
				t.Fatalf("message text or raw actor id leaked into attribute %q", a.Key)
			}
		}
	}
}

func assertReason(t *testing.T, obs *recordingObserver, reason string) {
	t.Helper()
	obs.mu.Lock()
	defer obs.mu.Unlock()
	for _, e := range obs.events {
		if e.name != chat.ObsIgnoredEvent {
			continue
		}
		for _, a := range e.attrs {
			if a.Key == chat.AttrReason && a.Value == reason {
				return
			}
		}
	}
	t.Fatalf("ignored event with reason %q not found in %#v", reason, obs.events)
}

func assertRoutePresent(t *testing.T, obs *recordingObserver, route string) {
	t.Helper()
	obs.mu.Lock()
	defer obs.mu.Unlock()
	for _, set := range obs.attrs {
		for _, a := range set {
			if a.Key == chat.AttrRoute && a.Value == route {
				return
			}
		}
	}
	t.Fatalf("route %q not present in span attrs %#v", route, obs.attrs)
}
