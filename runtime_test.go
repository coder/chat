package chat_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/coder/chat"
)

func TestRuntimeConstructionValidation(t *testing.T) {
	t.Parallel()

	state := newFakeState()

	if _, err := chat.New(context.Background(), chat.WithAdapter(newFakeAdapter("fake"))); err == nil {
		t.Fatal("expected missing state to fail")
	}

	if _, err := chat.New(context.Background(), chat.WithState(state)); err == nil {
		t.Fatal("expected missing adapter to fail")
	}

	if _, err := chat.New(context.Background(),
		chat.WithState(state),
		chat.WithAdapter(newFakeAdapter("fake")),
		chat.WithAdapter(newFakeAdapter("fake")),
	); err == nil {
		t.Fatal("expected duplicate adapters to fail")
	}

	if _, err := chat.New(context.Background(),
		chat.WithState(state),
		chat.WithAdapter(newFakeAdapter("fake")),
		chat.WithRuntimeOptions(chat.RuntimeOptions{DedupeTTL: 0, ThreadLockTTL: time.Minute}),
	); err == nil {
		t.Fatal("expected invalid runtime options to fail")
	}

	adapter := newFakeAdapter("fake")
	adapter.initErr = errors.New("init failed")
	if _, err := chat.New(context.Background(),
		chat.WithState(state),
		chat.WithAdapter(adapter),
	); err == nil {
		t.Fatal("expected adapter init failure")
	}

	initialized := newFakeAdapter("initialized")
	failing := newFakeAdapter("failing")
	failing.initErr = errors.New("init failed")
	if _, err := chat.New(context.Background(),
		chat.WithState(state),
		chat.WithAdapter(initialized),
		chat.WithAdapter(failing),
	); err == nil {
		t.Fatal("expected later adapter init failure")
	}
	if initialized.shutdowns != 1 || failing.shutdowns != 1 {
		t.Fatalf("cleanup counts initialized=%d failing=%d", initialized.shutdowns, failing.shutdowns)
	}
}

func TestRuntimeWebhookAndAcceptedRouting(t *testing.T) {
	t.Parallel()

	state := newFakeState()
	adapter := newFakeAdapter("fake")
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

	if _, err := bot.Webhook("missing"); err == nil {
		t.Fatal("expected unknown webhook mount to fail")
	}

	var mentions int
	bot.OnNewMention(func(ctx context.Context, ev *chat.MessageEvent) error {
		mentions++
		if ev.Event.ID != "event-1" {
			t.Fatalf("event id = %q", ev.Event.ID)
		}
		if ev.Thread.ID() != "fake:v1:thread-1" {
			t.Fatalf("thread id = %q", ev.Thread.ID())
		}
		if ev.Message.Text != "hello" {
			t.Fatalf("message text = %q", ev.Message.Text)
		}
		return ev.Thread.Subscribe(ctx)
	})

	bot.OnSubscribedMessage(func(ctx context.Context, ev *chat.MessageEvent) error {
		t.Fatal("subscribed handler should not run for first mention")
		return nil
	})

	status := postEvent(t, bot, "fake", chat.Event{
		ID:       "event-1",
		Adapter:  "fake",
		Tenant:   "tenant",
		ThreadID: "fake:v1:thread-1",
		Message: &chat.Message{
			ID:        "message-1",
			Text:      "hello",
			Mentioned: true,
			Author:    chat.Actor{Adapter: "fake", Tenant: "tenant", ID: "user-1", BotKind: chat.BotHuman},
		},
	})
	if status != http.StatusOK {
		t.Fatalf("status = %d", status)
	}
	if mentions != 1 {
		t.Fatalf("mentions = %d", mentions)
	}

	subscribed, err := state.IsThreadSubscribed(context.Background(), "fake:v1:thread-1")
	if err != nil {
		t.Fatalf("subscribed check: %v", err)
	}
	if !subscribed {
		t.Fatal("new mention handler should explicitly subscribe the thread")
	}
}

func TestRuntimeRoutingOrderAndNoOpHandlers(t *testing.T) {
	t.Parallel()

	state := newFakeState()
	adapter := newFakeAdapter("fake")
	bot, err := chat.New(context.Background(),
		chat.WithState(state),
		chat.WithAdapter(adapter),
	)
	if err != nil {
		t.Fatalf("new runtime: %v", err)
	}

	if status := postEvent(t, bot, "fake", chat.Event{
		ID:            "dm-1",
		Adapter:       "fake",
		Tenant:        "tenant",
		ThreadID:      "fake:v1:dm",
		DirectMessage: true,
		Message:       &chat.Message{ID: "m1", Text: "dm", Author: chat.Actor{Adapter: "fake", Tenant: "tenant", ID: "u1", BotKind: chat.BotHuman}},
	}); status != http.StatusOK {
		t.Fatalf("status = %d", status)
	}

	var routed []string
	bot.OnNewMention(func(ctx context.Context, ev *chat.MessageEvent) error {
		routed = append(routed, "mention:"+ev.Message.ID)
		return ev.Thread.Subscribe(ctx)
	})
	bot.OnNewMention(func(ctx context.Context, ev *chat.MessageEvent) error {
		routed = append(routed, "replacement:"+ev.Message.ID)
		return ev.Thread.Subscribe(ctx)
	})
	bot.OnSubscribedMessage(func(ctx context.Context, ev *chat.MessageEvent) error {
		routed = append(routed, "subscribed:"+ev.Message.ID)
		return nil
	})

	postEvent(t, bot, "fake", chat.Event{
		ID:            "dm-2",
		Adapter:       "fake",
		Tenant:        "tenant",
		ThreadID:      "fake:v1:dm",
		DirectMessage: true,
		Message:       &chat.Message{ID: "m2", Text: "dm", Author: chat.Actor{Adapter: "fake", Tenant: "tenant", ID: "u1", BotKind: chat.BotHuman}},
	})
	postEvent(t, bot, "fake", chat.Event{
		ID:       "dm-3",
		Adapter:  "fake",
		Tenant:   "tenant",
		ThreadID: "fake:v1:dm",
		Message:  &chat.Message{ID: "m3", Text: "dm", Mentioned: true, Author: chat.Actor{Adapter: "fake", Tenant: "tenant", ID: "u1", BotKind: chat.BotHuman}},
	})

	want := []string{"replacement:m2", "subscribed:m3"}
	if !equalStrings(routed, want) {
		t.Fatalf("routed = %#v, want %#v", routed, want)
	}
}

func TestRuntimeDedupeSelfMessageHandlerErrorAndLockConflictAreAcknowledged(t *testing.T) {
	t.Parallel()

	state := newFakeState()
	adapter := newFakeAdapter("fake")
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

	var calls int
	bot.OnNewMention(func(ctx context.Context, ev *chat.MessageEvent) error {
		calls++
		return errors.New("accepted handler error")
	})

	event := chat.Event{
		ID:       "event-1",
		Adapter:  "fake",
		Tenant:   "tenant",
		ThreadID: "fake:v1:thread-1",
		Message: &chat.Message{
			ID:        "message-1",
			Text:      "hello",
			Mentioned: true,
			Author:    chat.Actor{Adapter: "fake", Tenant: "tenant", ID: "user-1", BotKind: chat.BotHuman},
		},
	}

	if status := postEvent(t, bot, "fake", event); status != http.StatusOK {
		t.Fatalf("status = %d", status)
	}
	if status := postEvent(t, bot, "fake", event); status != http.StatusOK {
		t.Fatalf("duplicate status = %d", status)
	}

	self := cloneEvent(event)
	self.ID = "self"
	self.Message.ID = "self-message"
	self.Message.Author = adapter.BotActor()
	if status := postEvent(t, bot, "fake", self); status != http.StatusOK {
		t.Fatalf("self status = %d", status)
	}

	lease, acquired, err := state.AcquireLock(context.Background(), "fake:v1:thread-1", time.Hour)
	if err != nil {
		t.Fatalf("acquire conflict lock: %v", err)
	}
	if !acquired {
		t.Fatal("conflict lock was already held")
	}
	conflict := cloneEvent(event)
	conflict.ID = "conflict"
	conflict.Message.ID = "conflict-message"
	if status := postEvent(t, bot, "fake", conflict); status != http.StatusOK {
		t.Fatalf("conflict status = %d", status)
	}
	released, err := state.ReleaseLock(context.Background(), lease)
	if err != nil {
		t.Fatalf("release conflict lock: %v", err)
	}
	if !released {
		t.Fatal("conflict lock was not released")
	}
	if status := postEvent(t, bot, "fake", conflict); status != http.StatusOK {
		t.Fatalf("conflict duplicate status = %d", status)
	}

	if calls != 1 {
		t.Fatalf("calls = %d, want 1", calls)
	}
}

func TestRuntimeRetryablePreAcceptanceFailuresDoNotDedupeEvent(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name          string
		id            string
		firstThreadID chat.ThreadID
		retryThreadID chat.ThreadID
		fail          func(*fakeState)
		clear         func(*fakeState)
	}{
		{
			name:          "lock error",
			id:            "retry-lock-error",
			firstThreadID: "fake:v1:thread-1",
			retryThreadID: "fake:v1:thread-1",
			fail: func(state *fakeState) {
				state.mu.Lock()
				defer state.mu.Unlock()
				state.acquireLockErr = errors.New("lock unavailable")
			},
			clear: func(state *fakeState) {
				state.mu.Lock()
				defer state.mu.Unlock()
				state.acquireLockErr = nil
			},
		},
		{
			name:          "subscription error",
			id:            "retry-subscription-error",
			firstThreadID: "fake:v1:thread-1",
			retryThreadID: "fake:v1:thread-1",
			fail: func(state *fakeState) {
				state.mu.Lock()
				defer state.mu.Unlock()
				state.isThreadSubscribedErr = errors.New("subscription unavailable")
			},
			clear: func(state *fakeState) {
				state.mu.Lock()
				defer state.mu.Unlock()
				state.isThreadSubscribedErr = nil
			},
		},
		{
			name:          "thread validation error",
			id:            "retry-thread-validation-error",
			firstThreadID: "fake:bad",
			retryThreadID: "fake:v1:thread-1",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			state := newFakeState()
			adapter := newFakeAdapter("fake")
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

			var calls int
			bot.OnNewMention(func(ctx context.Context, ev *chat.MessageEvent) error {
				calls++
				return nil
			})

			newEvent := func(threadID chat.ThreadID) chat.Event {
				return chat.Event{
					ID:       tc.id,
					Adapter:  "fake",
					Tenant:   "tenant",
					ThreadID: threadID,
					Message: &chat.Message{
						ID:        tc.id + "-message",
						Text:      "hello",
						Mentioned: true,
						Author:    chat.Actor{Adapter: "fake", Tenant: "tenant", ID: "user-1", BotKind: chat.BotHuman},
					},
				}
			}

			if tc.fail != nil {
				tc.fail(state)
			}
			if status := postEvent(t, bot, "fake", newEvent(tc.firstThreadID)); status != http.StatusInternalServerError {
				t.Fatalf("first status = %d", status)
			}
			if tc.clear != nil {
				tc.clear(state)
			}

			retry := newEvent(tc.retryThreadID)
			if status := postEvent(t, bot, "fake", retry); status != http.StatusOK {
				t.Fatalf("retry status = %d", status)
			}
			if calls != 1 {
				t.Fatalf("calls after retry = %d, want 1", calls)
			}

			if status := postEvent(t, bot, "fake", retry); status != http.StatusOK {
				t.Fatalf("duplicate status = %d", status)
			}
			if calls != 1 {
				t.Fatalf("calls after duplicate = %d, want 1", calls)
			}
		})
	}
}

func TestRuntimeConcurrentDuplicateWaitsForPrimaryAcceptance(t *testing.T) {
	t.Parallel()

	state := newFakeState()
	subscriptionStarted := make(chan struct{})
	continueSubscription := make(chan struct{})
	state.isThreadSubscribedStarted = subscriptionStarted
	state.continueIsThreadSubscribed = continueSubscription
	adapter := newFakeAdapter("fake")
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

	var calls int
	bot.OnNewMention(func(ctx context.Context, ev *chat.MessageEvent) error {
		calls++
		return nil
	})

	event := chat.Event{
		ID:       "concurrent-duplicate",
		Adapter:  "fake",
		Tenant:   "tenant",
		ThreadID: "fake:v1:thread-1",
		Message: &chat.Message{
			ID:        "concurrent-duplicate-message",
			Text:      "hello",
			Mentioned: true,
			Author:    chat.Actor{Adapter: "fake", Tenant: "tenant", ID: "user-1", BotKind: chat.BotHuman},
		},
	}

	primaryDone := make(chan postEventResult, 1)
	go func() {
		primaryDone <- postEventResultFor(bot, "fake", event)
	}()

	select {
	case <-subscriptionStarted:
	case <-time.After(time.Second):
		t.Fatal("primary dispatch did not reach subscription check")
	}

	duplicateDone := make(chan postEventResult, 1)
	go func() {
		duplicateDone <- postEventResultFor(bot, "fake", event)
	}()

	close(continueSubscription)

	select {
	case result := <-primaryDone:
		if result.err != nil || result.status != http.StatusOK {
			t.Fatalf("primary result status=%d err=%v", result.status, result.err)
		}
	case <-time.After(time.Second):
		t.Fatal("primary dispatch did not finish")
	}
	select {
	case result := <-duplicateDone:
		if result.err != nil || result.status != http.StatusOK {
			t.Fatalf("duplicate result status=%d err=%v", result.status, result.err)
		}
	case <-time.After(time.Second):
		t.Fatal("duplicate dispatch did not finish")
	}
	if calls != 1 {
		t.Fatalf("calls = %d, want 1", calls)
	}
}

func TestRuntimeThreadHandlePostingSubscriptionAndAdapterAccess(t *testing.T) {
	t.Parallel()

	state := newFakeState()
	adapter := newFakeAdapter("fake")
	bot, err := chat.New(context.Background(), chat.WithState(state), chat.WithAdapter(adapter))
	if err != nil {
		t.Fatalf("new runtime: %v", err)
	}

	if got, ok := chat.AdapterAs[*fakeAdapter](bot, "fake"); !ok || got != adapter {
		t.Fatalf("typed adapter access failed")
	}
	if _, ok := chat.AdapterAs[*fakeAdapter](bot, "missing"); ok {
		t.Fatal("missing adapter access should fail")
	}

	thread, err := bot.Thread(context.Background(), "fake:v1:thread-1")
	if err != nil {
		t.Fatalf("thread handle: %v", err)
	}
	if err := thread.Subscribe(context.Background()); err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	if err := thread.Unsubscribe(context.Background()); err != nil {
		t.Fatalf("unsubscribe: %v", err)
	}
	sent, err := thread.Post(context.Background(), chat.Markdown("**hello**"))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	if sent.ID != "sent-1" {
		t.Fatalf("sent id = %q", sent.ID)
	}
	if adapter.posts[0].Message.Format != chat.MessageFormatMarkdown {
		t.Fatalf("post format = %v", adapter.posts[0].Message.Format)
	}

	if _, err := bot.Thread(context.Background(), "missing:v1:thread"); err == nil {
		t.Fatal("expected unknown adapter thread id to fail")
	}
	if _, err := bot.Thread(context.Background(), "fake:bad"); err == nil {
		t.Fatal("expected malformed thread id to fail")
	}
}

func TestRuntimeEphemeralUnsupportedCapability(t *testing.T) {
	t.Parallel()

	bot, err := chat.New(context.Background(),
		chat.WithState(newFakeState()),
		chat.WithAdapter(newFakeAdapter("fake")),
	)
	if err != nil {
		t.Fatalf("new runtime: %v", err)
	}
	thread, err := bot.Thread(context.Background(), "fake:v1:thread-1")
	if err != nil {
		t.Fatalf("thread handle: %v", err)
	}
	_, err = thread.PostEphemeral(context.Background(),
		chat.Actor{Adapter: "fake", Tenant: "tenant", ID: "user-1", BotKind: chat.BotHuman},
		chat.Text("private"),
		chat.EphemeralOptions{},
	)
	if !errors.Is(err, chat.ErrUnsupportedCapability) {
		t.Fatalf("ephemeral error = %v, want unsupported capability", err)
	}
}

func TestRuntimeShutdownAttemptsAllCleanup(t *testing.T) {
	t.Parallel()

	state := newFakeState()
	stateErr := errors.New("state failed")
	state.shutdownErr = stateErr
	adapterA := newFakeAdapter("a")
	adapterErr := errors.New("adapter a failed")
	adapterA.shutdownErr = adapterErr
	adapterB := newFakeAdapter("b")
	bot, err := chat.New(context.Background(),
		chat.WithState(state),
		chat.WithAdapter(adapterA),
		chat.WithAdapter(adapterB),
	)
	if err != nil {
		t.Fatalf("new runtime: %v", err)
	}

	err = bot.Shutdown(context.Background())
	if err == nil {
		t.Fatal("expected joined shutdown error")
	}
	if !errors.Is(err, adapterErr) || !errors.Is(err, stateErr) {
		t.Fatalf("shutdown error = %v, want adapter and state errors", err)
	}
	if adapterA.shutdowns != 1 || adapterB.shutdowns != 1 || state.shutdowns != 1 {
		t.Fatalf("cleanup counts adapterA=%d adapterB=%d state=%d", adapterA.shutdowns, adapterB.shutdowns, state.shutdowns)
	}
	if err := bot.Shutdown(context.Background()); err != nil {
		t.Fatalf("idempotent shutdown should return nil after first attempt, got %v", err)
	}
}

func TestRuntimeConcurrentShutdownWaitsForCleanup(t *testing.T) {
	t.Parallel()

	state := newFakeState()
	adapter, shutdownStarted, continueShutdown := newBlockingFakeAdapter("fake")
	defer continueShutdown()
	bot, err := chat.New(context.Background(),
		chat.WithState(state),
		chat.WithAdapter(adapter),
	)
	if err != nil {
		t.Fatalf("new runtime: %v", err)
	}

	firstDone := make(chan error, 1)
	go func() {
		firstDone <- bot.Shutdown(context.Background())
	}()

	select {
	case <-shutdownStarted:
	case <-time.After(time.Second):
		t.Fatal("first shutdown did not reach adapter cleanup")
	}

	secondDone := make(chan error, 1)
	go func() {
		secondDone <- bot.Shutdown(context.Background())
	}()

	canceledCtx, cancel := context.WithCancel(context.Background())
	cancel()
	canceledDone := make(chan error, 1)
	go func() {
		canceledDone <- bot.Shutdown(canceledCtx)
	}()

	select {
	case err := <-canceledDone:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("canceled concurrent shutdown error = %v, want context canceled", err)
		}
	case <-time.After(time.Second):
		t.Fatal("canceled concurrent shutdown did not return")
	}
	select {
	case err := <-secondDone:
		t.Fatalf("concurrent shutdown returned before cleanup completed: %v", err)
	default:
	}

	continueShutdown()

	select {
	case err := <-firstDone:
		if err != nil {
			t.Fatalf("first shutdown: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("first shutdown did not finish")
	}
	select {
	case err := <-secondDone:
		if err != nil {
			t.Fatalf("second shutdown: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("second shutdown did not finish")
	}
	if adapter.shutdowns != 1 || state.shutdowns != 1 {
		t.Fatalf("cleanup counts adapter=%d state=%d", adapter.shutdowns, state.shutdowns)
	}
}

func postEvent(t *testing.T, bot *chat.Chat, adapter string, ev chat.Event) int {
	t.Helper()

	result := postEventResultFor(bot, adapter, ev)
	if result.err != nil {
		t.Fatal(result.err)
	}
	return result.status
}

type postEventResult struct {
	status int
	err    error
}

func postEventResultFor(bot *chat.Chat, adapter string, ev chat.Event) postEventResult {
	handler, err := bot.Webhook(adapter)
	if err != nil {
		return postEventResult{err: err}
	}
	body, err := json.Marshal(ev)
	if err != nil {
		return postEventResult{err: err}
	}
	req := httptest.NewRequest(http.MethodPost, "/webhook", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	return postEventResult{status: rec.Code}
}

func cloneEvent(ev chat.Event) chat.Event {
	if ev.Message != nil {
		msg := *ev.Message
		ev.Message = &msg
	}
	return ev
}

type fakeAdapter struct {
	name         string
	initErr      error
	shutdownErr  error
	shutdownHook func()
	shutdowns    int
	posts        []fakePost
}

type fakePost struct {
	Thread  chat.ThreadRef
	Message chat.PostableMessage
}

func newFakeAdapter(name string) *fakeAdapter {
	return &fakeAdapter{name: name}
}

func newBlockingFakeAdapter(name string) (*fakeAdapter, <-chan struct{}, func()) {
	started := make(chan struct{})
	continued := make(chan struct{})

	adapter := newFakeAdapter(name)
	adapter.shutdownHook = func() {
		close(started)
		<-continued
	}
	return adapter, started, sync.OnceFunc(func() {
		close(continued)
	})
}

func (a *fakeAdapter) Name() string {
	return a.name
}

func (a *fakeAdapter) Init(context.Context) error {
	return a.initErr
}

func (a *fakeAdapter) Shutdown(context.Context) error {
	a.shutdowns++
	if a.shutdownHook != nil {
		a.shutdownHook()
	}
	return a.shutdownErr
}

func (a *fakeAdapter) Webhook(dispatch chat.DispatchFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var ev chat.Event
		if err := json.NewDecoder(r.Body).Decode(&ev); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if err := dispatch(r.Context(), &ev); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
	})
}

func (a *fakeAdapter) ValidateThreadID(id chat.ThreadID) (chat.ThreadRef, error) {
	if id == "fake:bad" {
		return chat.ThreadRef{}, errors.New("malformed")
	}
	if id == "" {
		return chat.ThreadRef{}, errors.New("empty")
	}
	return chat.ThreadRef{ID: id, Adapter: a.name, Tenant: "tenant", Channel: string(id)}, nil
}

func (a *fakeAdapter) PostMessage(ctx context.Context, thread chat.ThreadRef, msg chat.PostableMessage) (*chat.SentMessage, error) {
	a.posts = append(a.posts, fakePost{Thread: thread, Message: msg})
	return &chat.SentMessage{ID: "sent-1", ThreadID: thread.ID}, nil
}

func (a *fakeAdapter) BotActor() chat.Actor {
	return chat.Actor{Adapter: a.name, Tenant: "tenant", ID: "bot", BotKind: chat.BotBot}
}

type fakeState struct {
	mu                         sync.Mutex
	subscribed                 map[chat.ThreadID]bool
	seen                       map[string]bool
	locked                     map[string]chat.LockLease
	lockSeq                    int
	acquireLockErr             error
	isThreadSubscribedErr      error
	isThreadSubscribedStarted  chan struct{}
	continueIsThreadSubscribed chan struct{}
	shutdowns                  int
	shutdownErr                error
}

func newFakeState() *fakeState {
	return &fakeState{
		subscribed: map[chat.ThreadID]bool{},
		seen:       map[string]bool{},
		locked:     map[string]chat.LockLease{},
	}
}

func (s *fakeState) IsThreadSubscribed(ctx context.Context, id chat.ThreadID) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	s.mu.Lock()
	if s.isThreadSubscribedErr != nil {
		s.mu.Unlock()
		return false, s.isThreadSubscribedErr
	}
	started := s.isThreadSubscribedStarted
	if started != nil {
		s.isThreadSubscribedStarted = nil
	}
	continueCh := s.continueIsThreadSubscribed
	s.mu.Unlock()
	if started != nil {
		close(started)
		<-continueCh
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.subscribed[id], nil
}

func (s *fakeState) SubscribeThread(ctx context.Context, id chat.ThreadID) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.subscribed[id] = true
	return nil
}

func (s *fakeState) UnsubscribeThread(ctx context.Context, id chat.ThreadID) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.subscribed, id)
	return nil
}

func (s *fakeState) MarkEvent(ctx context.Context, id string, ttl time.Duration) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.seen[id] {
		return false, nil
	}
	s.seen[id] = true
	return true, nil
}

func (s *fakeState) AcquireLock(ctx context.Context, key string, ttl time.Duration) (chat.LockLease, bool, error) {
	if err := ctx.Err(); err != nil {
		return chat.LockLease{}, false, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.acquireLockErr != nil {
		return chat.LockLease{}, false, s.acquireLockErr
	}
	if _, ok := s.locked[key]; ok {
		return chat.LockLease{}, false, nil
	}
	// Tokens are unique per acquisition: a stale lease must never match a newer
	// lock for the same key (the token-owned Lock Lease invariant).
	s.lockSeq++
	lease := chat.LockLease{Key: key, Token: fmt.Sprintf("%s-token-%d", key, s.lockSeq)}
	s.locked[key] = lease
	return lease, true, nil
}

func (s *fakeState) ExtendLock(ctx context.Context, lease chat.LockLease, ttl time.Duration) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	held, ok := s.locked[lease.Key]
	return ok && held.Token == lease.Token, nil
}

// expireLock simulates the lease for key vanishing out from under its holder
// (another instance releasing it, or TTL expiry).
func (s *fakeState) expireLock(ctx context.Context, key string) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.locked[key]; !ok {
		return false, nil
	}
	delete(s.locked, key)
	return true, nil
}

func (s *fakeState) ReleaseLock(ctx context.Context, lease chat.LockLease) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	held, ok := s.locked[lease.Key]
	if !ok || held.Token != lease.Token {
		return false, nil
	}
	delete(s.locked, lease.Key)
	return true, nil
}

func (s *fakeState) Shutdown(context.Context) error {
	s.shutdowns++
	return s.shutdownErr
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
