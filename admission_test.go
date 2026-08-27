package chat_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/chat"
)

// newAdmissionRuntime builds a deferred runtime with an admission cap small
// enough to saturate deterministically, an observer, and optional option
// mutations.
func newAdmissionRuntime(t *testing.T, state chat.State, adapter chat.Adapter, observer chat.Observer, opts ...func(*chat.RuntimeOptions)) *chat.Chat {
	t.Helper()
	options := chat.RuntimeOptions{
		DedupeTTL:     time.Hour,
		ThreadLockTTL: time.Hour,
		Concurrency:   chat.ConcurrencyDrop,
		Dispatch:      chat.DispatchDeferred,
		MaxDetached:   1,
		DetachTimeout: 5 * time.Second,
	}
	for _, opt := range opts {
		opt(&options)
	}
	bot, err := chat.New(context.Background(),
		chat.WithState(state),
		chat.WithAdapter(adapter),
		chat.WithLogger(slog.New(slog.NewTextHandler(newSyncBuffer(), nil))),
		chat.WithObserver(observer),
		chat.WithRuntimeOptions(options),
	)
	if err != nil {
		t.Fatalf("new admission runtime: %v", err)
	}
	return bot
}

// tenantMentionEvent is mentionEvent with an explicit Platform Tenant.
func tenantMentionEvent(id string, threadID chat.ThreadID, tenant string) chat.Event {
	ev := mentionEvent(id, threadID)
	ev.Tenant = tenant
	return ev
}

// postEventBody posts one event through the fake adapter webhook and returns
// the status plus response body, so tests can assert the admission sentinel
// surfaced to the adapter.
func postEventBody(t *testing.T, bot *chat.Chat, adapter string, ev chat.Event) (int, string) {
	t.Helper()
	handler, err := bot.Webhook(adapter)
	if err != nil {
		t.Fatal(err)
	}
	body, err := json.Marshal(ev)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/webhook", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	return rec.Code, rec.Body.String()
}

// eventually polls cond until it holds or the deadline passes.
func eventually(t *testing.T, timeout time.Duration, cond func() bool, msg string) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal(msg)
}

// handlerRecorder tracks handled event IDs and blocks handlers on a shared
// gate until released.
type handlerRecorder struct {
	mu      sync.Mutex
	handled []string
	gate    chan struct{}
	started chan string
}

func newHandlerRecorder() *handlerRecorder {
	return &handlerRecorder{gate: make(chan struct{}), started: make(chan string, 64)}
}

func (h *handlerRecorder) handle(_ context.Context, ev *chat.MessageEvent) error {
	h.started <- ev.Event.ID
	<-h.gate
	h.mu.Lock()
	defer h.mu.Unlock()
	h.handled = append(h.handled, ev.Event.ID)
	return nil
}

func (h *handlerRecorder) release() { close(h.gate) }

func (h *handlerRecorder) handledIDs() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]string(nil), h.handled...)
}

func (h *handlerRecorder) waitStarted(t *testing.T, want string) {
	t.Helper()
	select {
	case got := <-h.started:
		if got != want {
			t.Fatalf("handler started for %q, want %q", got, want)
		}
	case <-time.After(time.Second):
		t.Fatalf("handler for %q did not start", want)
	}
}

// TestAdmissionRejectsAtMaxDetachedWithoutDedupeMark saturates MaxDetached=1
// and verifies the rejection contract: the webhook sees the typed sentinel
// (never a 2xx), the observation and terminal outcome are emitted with adapter
// and tenant, and — because rejection happens before dedupe marking — the same
// Event Identity is handled normally once capacity frees.
func TestAdmissionRejectsAtMaxDetachedWithoutDedupeMark(t *testing.T) {
	t.Parallel()

	state := newFakeState()
	adapter := newFakeAdapter("fake")
	observer := &recordingObserver{}
	bot := newAdmissionRuntime(t, state, adapter, observer)
	handlers := newHandlerRecorder()
	bot.OnNewMention(handlers.handle)

	if status := postEvent(t, bot, "fake", mentionEvent("event-1", "fake:v1:thread-1")); status != http.StatusOK {
		t.Fatalf("first event status = %d", status)
	}
	handlers.waitStarted(t, "event-1")

	status, body := postEventBody(t, bot, "fake", mentionEvent("event-2", "fake:v1:thread-2"))
	if status == http.StatusOK {
		t.Fatalf("saturated dispatch acknowledged 2xx, body = %q", body)
	}
	if !strings.Contains(body, "admission rejected") {
		t.Fatalf("rejection body = %q, want the admission sentinel", body)
	}
	if !observer.hasEvent(chat.ObsAdmissionRejected) {
		t.Fatalf("observations = %v, want admission_rejected", observer.eventNames())
	}
	assertAdmissionRejectedAttrs(t, observer, "fake", "tenant")
	if !hasOutcome(observer, chat.OutcomeAdmissionRejected) {
		t.Fatalf("outcomes = %v, want admission-rejected", observer.terminalOutcomes())
	}
	// The rejected delivery was never marked in Event Identity dedupe: once
	// capacity frees, redelivering the same event id runs the handler instead
	// of resolving as a duplicate.
	handlers.release()
	eventually(t, 5*time.Second, func() bool {
		status, _ := postEventBody(t, bot, "fake", mentionEvent("event-2", "fake:v1:thread-2"))
		return status == http.StatusOK
	}, "capacity did not free after handler completion")
	eventually(t, 5*time.Second, func() bool {
		for _, id := range handlers.handledIDs() {
			if id == "event-2" {
				return true
			}
		}
		return false
	}, "redelivered rejected event was not handled (dedupe-marked before rejection?)")
}

// TestAdmissionSlotReleasedOnErroredAndResolvedPreludes floods a MaxDetached=1
// runtime with preludes that fail (state error) or resolve without detached
// work (ignored events) and proves none of them leak admission capacity.
func TestAdmissionSlotReleasedOnErroredAndResolvedPreludes(t *testing.T) {
	t.Parallel()

	state := newFakeState()
	adapter := newFakeAdapter("fake")
	observer := &recordingObserver{}
	bot := newAdmissionRuntime(t, state, adapter, observer)
	handlers := newHandlerRecorder()
	bot.OnNewMention(handlers.handle)

	// Errored preludes: AcquireLock fails, dispatch returns the error, and the
	// admission slot must come back every time.
	state.mu.Lock()
	state.acquireLockErr = errors.New("acquire lock boom")
	state.mu.Unlock()
	for i := range 3 {
		status, body := postEventBody(t, bot, "fake", mentionEvent("errored-"+string(rune('a'+i)), "fake:v1:thread-err"))
		if status == http.StatusOK {
			t.Fatalf("errored prelude %d acknowledged 2xx", i)
		}
		if strings.Contains(body, "admission rejected") {
			t.Fatalf("errored prelude %d hit the admission cap: slot leaked (body = %q)", i, body)
		}
	}
	state.mu.Lock()
	state.acquireLockErr = nil
	state.mu.Unlock()

	// Resolved preludes: ignored events (no payload handler routes them)
	// resolve synchronously and must release their slots too.
	for i := range 3 {
		ev := chat.Event{ID: "ignored-" + string(rune('a'+i)), Adapter: "fake", Tenant: "tenant", ThreadID: "fake:v1:thread-ign"}
		if status := postEvent(t, bot, "fake", ev); status != http.StatusOK {
			t.Fatalf("ignored event %d status = %d", i, status)
		}
	}

	// With every slot returned, a routed event is admitted.
	if status := postEvent(t, bot, "fake", mentionEvent("event-ok", "fake:v1:thread-ok")); status != http.StatusOK {
		t.Fatalf("post-flood event status = %d", status)
	}
	handlers.waitStarted(t, "event-ok")
	handlers.release()
}

// stalledReleaseState blocks ReleaseLock until unblocked, simulating stalled
// tail cleanup after the handler already returned.
type stalledReleaseState struct {
	*fakeState
	releaseStarted chan struct{}
	releaseGate    chan struct{}
	startOnce      sync.Once
}

func newStalledReleaseState() *stalledReleaseState {
	return &stalledReleaseState{
		fakeState:      newFakeState(),
		releaseStarted: make(chan struct{}),
		releaseGate:    make(chan struct{}),
	}
}

func (s *stalledReleaseState) ReleaseLock(ctx context.Context, lease chat.LockLease) (bool, error) {
	s.startOnce.Do(func() { close(s.releaseStarted) })
	<-s.releaseGate
	return s.fakeState.ReleaseLock(ctx, lease)
}

// TestAdmissionSlotHeldUntilTailGoroutineReturns pins the bounded-retention
// invariant: a handler that returned but whose tail goroutine is stalled in
// cleanup (a blocked lock release) still occupies its admission slot.
func TestAdmissionSlotHeldUntilTailGoroutineReturns(t *testing.T) {
	t.Parallel()

	state := newStalledReleaseState()
	adapter := newFakeAdapter("fake")
	observer := &recordingObserver{}
	bot := newAdmissionRuntime(t, state, adapter, observer)
	handlers := newHandlerRecorder()
	handlers.release() // handlers return immediately; only cleanup stalls
	bot.OnNewMention(handlers.handle)

	if status := postEvent(t, bot, "fake", mentionEvent("event-1", "fake:v1:thread-1")); status != http.StatusOK {
		t.Fatalf("first event status = %d", status)
	}
	select {
	case <-state.releaseStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("tail cleanup did not reach ReleaseLock")
	}

	// The handler has returned, but the tail goroutine is parked in cleanup:
	// the slot must still be held.
	status, body := postEventBody(t, bot, "fake", mentionEvent("event-2", "fake:v1:thread-2"))
	if status == http.StatusOK || !strings.Contains(body, "admission rejected") {
		t.Fatalf("stalled-cleanup dispatch = %d %q, want admission rejection", status, body)
	}

	close(state.releaseGate)
	eventually(t, 5*time.Second, func() bool {
		status, _ := postEventBody(t, bot, "fake", mentionEvent("event-3", "fake:v1:thread-3"))
		return status == http.StatusOK
	}, "slot did not free after the tail goroutine returned")
}

// TestAdmissionClosedOnShutdown pins the shutdown contract: once Runtime
// Shutdown begins, new deferred deliveries are rejected with the admission
// sentinel (observable, platform retry covers them) instead of being admitted
// into a runtime that is cancelling its work.
func TestAdmissionClosedOnShutdown(t *testing.T) {
	t.Parallel()

	state := newFakeState()
	adapter := newFakeAdapter("fake")
	observer := &recordingObserver{}
	bot := newAdmissionRuntime(t, state, adapter, observer, func(o *chat.RuntimeOptions) {
		o.MaxDetached = 8
	})

	if err := bot.Shutdown(context.Background()); err != nil {
		t.Fatalf("shutdown: %v", err)
	}

	status, body := postEventBody(t, bot, "fake", mentionEvent("late-event", "fake:v1:thread-1"))
	if status == http.StatusOK || !strings.Contains(body, "admission rejected") {
		t.Fatalf("post-shutdown dispatch = %d %q, want admission rejection", status, body)
	}
	if !observer.hasEvent(chat.ObsAdmissionRejected) {
		t.Fatalf("observations = %v, want admission_rejected", observer.eventNames())
	}
	if !hasOutcome(observer, chat.OutcomeAdmissionRejected) {
		t.Fatalf("outcomes = %v, want admission-rejected", observer.terminalOutcomes())
	}
}

// TestAdmissionOptionValidation pins constructor validation: MaxDetached must
// be positive and MaxDetachedPerTenant non-negative under DispatchDeferred,
// while DispatchSync ignores both (the DetachTimeout precedent).
func TestAdmissionOptionValidation(t *testing.T) {
	t.Parallel()

	if got := chat.DefaultRuntimeOptions().MaxDetached; got != 1024 {
		t.Fatalf("default MaxDetached = %d, want 1024", got)
	}

	newWith := func(options chat.RuntimeOptions) error {
		_, err := chat.New(context.Background(),
			chat.WithState(newFakeState()),
			chat.WithAdapter(newFakeAdapter("fake")),
			chat.WithRuntimeOptions(options),
		)
		return err
	}
	deferred := func(mutate func(*chat.RuntimeOptions)) chat.RuntimeOptions {
		options := chat.RuntimeOptions{
			DedupeTTL:     time.Hour,
			ThreadLockTTL: time.Hour,
			Dispatch:      chat.DispatchDeferred,
			DetachTimeout: time.Second,
			MaxDetached:   1,
		}
		mutate(&options)
		return options
	}

	for name, tt := range map[string]struct {
		options chat.RuntimeOptions
		wantErr string
	}{
		"zero max detached under deferred": {
			options: deferred(func(o *chat.RuntimeOptions) { o.MaxDetached = 0 }),
			wantErr: "max detached must be positive",
		},
		"negative max detached under deferred": {
			options: deferred(func(o *chat.RuntimeOptions) { o.MaxDetached = -1 }),
			wantErr: "max detached must be positive",
		},
		"negative per-tenant ceiling under deferred": {
			options: deferred(func(o *chat.RuntimeOptions) { o.MaxDetachedPerTenant = -1 }),
			wantErr: "max detached per tenant must not be negative",
		},
		"valid per-tenant ceiling under deferred": {
			options: deferred(func(o *chat.RuntimeOptions) { o.MaxDetachedPerTenant = 4 }),
		},
		"sync ignores both": {
			options: chat.RuntimeOptions{
				DedupeTTL:            time.Hour,
				ThreadLockTTL:        time.Hour,
				MaxDetached:          0,
				MaxDetachedPerTenant: -1,
			},
		},
	} {
		t.Run(name, func(t *testing.T) {
			err := newWith(tt.options)
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("error = %v, want %q", err, tt.wantErr)
			}
		})
	}
}

// TestAdmissionPerTenantCeilingKeysOnInstallation proves MaxDetachedPerTenant
// caps one (adapter, tenant) installation without starving others, and that an
// empty tenant is a countable key rather than an escape from the ceiling.
func TestAdmissionPerTenantCeilingKeysOnInstallation(t *testing.T) {
	t.Parallel()

	state := newFakeState()
	adapter := newFakeAdapter("fake")
	observer := &recordingObserver{}
	bot := newAdmissionRuntime(t, state, adapter, observer, func(o *chat.RuntimeOptions) {
		o.MaxDetached = 10
		o.MaxDetachedPerTenant = 1
	})
	handlers := newHandlerRecorder()
	bot.OnNewMention(handlers.handle)

	if status := postEvent(t, bot, "fake", tenantMentionEvent("empty-1", "fake:v1:thread-1", "")); status != http.StatusOK {
		t.Fatalf("first empty-tenant event status = %d", status)
	}
	handlers.waitStarted(t, "empty-1")

	status, body := postEventBody(t, bot, "fake", tenantMentionEvent("empty-2", "fake:v1:thread-2", ""))
	if status == http.StatusOK || !strings.Contains(body, "admission rejected") {
		t.Fatalf("empty-tenant ceiling dispatch = %d %q, want admission rejection", status, body)
	}
	assertAdmissionRejectedAttrs(t, observer, "fake", "")

	// A different installation still has headroom.
	if status := postEvent(t, bot, "fake", tenantMentionEvent("acme-1", "fake:v1:thread-3", "acme")); status != http.StatusOK {
		t.Fatalf("other-tenant event status = %d", status)
	}
	handlers.waitStarted(t, "acme-1")

	handlers.release()
	eventually(t, 5*time.Second, func() bool {
		status, _ := postEventBody(t, bot, "fake", tenantMentionEvent("empty-3", "fake:v1:thread-4", ""))
		return status == http.StatusOK
	}, "per-tenant capacity did not free after handler completion")
}

// TestAdmissionPerTenantCounterGC proves the per-tenant counters are garbage
// collected: once a tenant's deliveries all release capacity, no map entry
// remains for it.
func TestAdmissionPerTenantCounterGC(t *testing.T) {
	t.Parallel()

	state := newFakeState()
	adapter := newFakeAdapter("fake")
	observer := &recordingObserver{}
	bot := newAdmissionRuntime(t, state, adapter, observer, func(o *chat.RuntimeOptions) {
		o.MaxDetached = 10
		o.MaxDetachedPerTenant = 2
	})
	handlers := newHandlerRecorder()
	bot.OnNewMention(handlers.handle)

	for _, ev := range []chat.Event{
		tenantMentionEvent("t1-a", "fake:v1:thread-1", "tenant-1"),
		tenantMentionEvent("t1-b", "fake:v1:thread-2", "tenant-1"),
		tenantMentionEvent("t2-a", "fake:v1:thread-3", "tenant-2"),
	} {
		if status := postEvent(t, bot, "fake", ev); status != http.StatusOK {
			t.Fatalf("event %s status = %d", ev.ID, status)
		}
	}
	for range 3 {
		<-handlers.started
	}
	if entries := chat.AdmissionTenantEntries(bot); entries != 2 {
		t.Fatalf("in-flight tenant entries = %d, want 2", entries)
	}

	handlers.release()
	eventually(t, 5*time.Second, func() bool {
		return chat.AdmissionTenantEntries(bot) == 0
	}, "tenant counter entries were not garbage collected after release")
}

// TestAdmissionCountsParkedQueueWaiters pins the queue row of the ADR 0015
// retention table: a parked waiter holds an admission slot just like a running
// tail.
func TestAdmissionCountsParkedQueueWaiters(t *testing.T) {
	t.Parallel()

	state := newFakeState()
	adapter := newFakeAdapter("fake")
	observer := &recordingObserver{}
	bot := newAdmissionRuntime(t, state, adapter, observer, func(o *chat.RuntimeOptions) {
		o.Concurrency = chat.ConcurrencyQueue
		o.MaxDetached = 2
	})
	handlers := newHandlerRecorder()
	bot.OnNewMention(handlers.handle)

	// Slot 1: running tail on thread-1. Slot 2: parked waiter behind it.
	if status := postEvent(t, bot, "fake", mentionEvent("event-1", "fake:v1:thread-1")); status != http.StatusOK {
		t.Fatalf("first event status = %d", status)
	}
	handlers.waitStarted(t, "event-1")
	if status := postEvent(t, bot, "fake", mentionEvent("event-2", "fake:v1:thread-1")); status != http.StatusOK {
		t.Fatalf("queued event status = %d", status)
	}

	// The parked waiter counts: a third delivery for a completely idle thread
	// is rejected.
	status, body := postEventBody(t, bot, "fake", mentionEvent("event-3", "fake:v1:thread-2"))
	if status == http.StatusOK || !strings.Contains(body, "admission rejected") {
		t.Fatalf("dispatch with parked waiter = %d %q, want admission rejection", status, body)
	}

	handlers.release()
	eventually(t, 5*time.Second, func() bool {
		for _, id := range handlers.handledIDs() {
			if id == "event-2" {
				return true
			}
		}
		return false
	}, "queued waiter did not run after the in-flight handler finished")
}

// TestAdmissionCountsConcurrentSlotWaiters pins the concurrent row of the ADR
// 0015 retention table: a tail waiting for a MaxConcurrent slot holds an
// admission slot.
func TestAdmissionCountsConcurrentSlotWaiters(t *testing.T) {
	t.Parallel()

	state := newFakeState()
	adapter := newFakeAdapter("fake")
	observer := &recordingObserver{}
	bot := newAdmissionRuntime(t, state, adapter, observer, func(o *chat.RuntimeOptions) {
		o.Concurrency = chat.ConcurrencyConcurrent
		o.MaxConcurrent = 1
		o.MaxDetached = 2
	})
	handlers := newHandlerRecorder()
	bot.OnNewMention(handlers.handle)

	if status := postEvent(t, bot, "fake", mentionEvent("event-1", "fake:v1:thread-1")); status != http.StatusOK {
		t.Fatalf("first event status = %d", status)
	}
	handlers.waitStarted(t, "event-1")
	// Slot-waiter: admitted, parked behind MaxConcurrent.
	if status := postEvent(t, bot, "fake", mentionEvent("event-2", "fake:v1:thread-2")); status != http.StatusOK {
		t.Fatalf("slot-waiting event status = %d", status)
	}

	status, body := postEventBody(t, bot, "fake", mentionEvent("event-3", "fake:v1:thread-3"))
	if status == http.StatusOK || !strings.Contains(body, "admission rejected") {
		t.Fatalf("dispatch with slot-waiter = %d %q, want admission rejection", status, body)
	}

	handlers.release()
	eventually(t, 5*time.Second, func() bool {
		return len(handlers.handledIDs()) == 2
	}, "slot-waiter did not run after a concurrency slot freed")
}

// hasOutcome reports whether the observer recorded the terminal outcome.
func hasOutcome(observer *recordingObserver, want chat.DispatchOutcome) bool {
	for _, outcome := range observer.terminalOutcomes() {
		if outcome == want {
			return true
		}
	}
	return false
}

// assertAdmissionRejectedAttrs verifies the admission_rejected observation
// carries the adapter and tenant attributes required by ADR 0015.
func assertAdmissionRejectedAttrs(t *testing.T, observer *recordingObserver, adapter, tenant string) {
	t.Helper()
	observer.mu.Lock()
	defer observer.mu.Unlock()
	for _, ev := range observer.events {
		if ev.name != chat.ObsAdmissionRejected {
			continue
		}
		var gotAdapter, gotTenant bool
		for _, attr := range ev.attrs {
			if attr.Key == chat.AttrAdapter && attr.Value == adapter {
				gotAdapter = true
			}
			if attr.Key == chat.AttrTenant && attr.Value == tenant {
				gotTenant = true
			}
		}
		if gotAdapter && gotTenant {
			return
		}
	}
	t.Fatalf("no admission_rejected observation carried adapter=%q tenant=%q", adapter, tenant)
}
