package chat_test

import (
	"context"
	"net/http"
	"reflect"
	"testing"
	"time"

	"github.com/coder/chat"
)

// historyFakeAdapter is a minimal Adapter that ALSO implements the HistoryReader
// Optional Capability, used to verify capability detection through Adapter Access.
type historyFakeAdapter struct {
	name     string
	messages []chat.Message
	reads    int
}

func (a *historyFakeAdapter) Name() string                   { return a.name }
func (a *historyFakeAdapter) Init(context.Context) error     { return nil }
func (a *historyFakeAdapter) Shutdown(context.Context) error { return nil }
func (a *historyFakeAdapter) Webhook(chat.DispatchFunc) http.Handler {
	return http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})
}
func (a *historyFakeAdapter) ValidateThreadID(id chat.ThreadID) (chat.ThreadRef, error) {
	return chat.ThreadRef{ID: id, Adapter: a.name, Tenant: "tenant", Channel: string(id)}, nil
}
func (a *historyFakeAdapter) PostMessage(context.Context, chat.ThreadRef, chat.PostableMessage) (*chat.SentMessage, error) {
	return &chat.SentMessage{}, nil
}
func (a *historyFakeAdapter) BotActor() chat.Actor {
	return chat.Actor{Adapter: a.name, Tenant: "tenant", ID: "bot", BotKind: chat.BotBot}
}

func (a *historyFakeAdapter) ReadHistory(_ context.Context, _ chat.ThreadID, _ chat.HistoryQuery) ([]chat.Message, error) {
	a.reads++
	return a.messages, nil
}

// plainFakeAdapter is an Adapter that does NOT implement HistoryReader, used to
// verify that absence of the capability is the explicit unsupported result.
type plainFakeAdapter struct{ name string }

func (a *plainFakeAdapter) Name() string                   { return a.name }
func (a *plainFakeAdapter) Init(context.Context) error     { return nil }
func (a *plainFakeAdapter) Shutdown(context.Context) error { return nil }
func (a *plainFakeAdapter) Webhook(chat.DispatchFunc) http.Handler {
	return http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})
}
func (a *plainFakeAdapter) ValidateThreadID(id chat.ThreadID) (chat.ThreadRef, error) {
	return chat.ThreadRef{ID: id, Adapter: a.name}, nil
}
func (a *plainFakeAdapter) PostMessage(context.Context, chat.ThreadRef, chat.PostableMessage) (*chat.SentMessage, error) {
	return &chat.SentMessage{}, nil
}
func (a *plainFakeAdapter) BotActor() chat.Actor {
	return chat.Actor{Adapter: a.name, ID: "bot", BotKind: chat.BotBot}
}

// HistoryReader is detected and reached only through typed Adapter Access. Absence
// of the capability is the explicit unsupported result (ok == false), distinct from
// a real empty []Message, and a wrong adapter name is also ok == false.
func TestHistoryReaderCapabilityDetection(t *testing.T) {
	t.Parallel()

	withHistory := &historyFakeAdapter{name: "withhistory", messages: []chat.Message{{ID: "m1", Text: "hi"}}}
	without := &plainFakeAdapter{name: "nohistory"}

	bot, err := chat.New(context.Background(),
		chat.WithState(newFakeState()),
		chat.WithAdapter(withHistory),
		chat.WithAdapter(without),
	)
	if err != nil {
		t.Fatalf("new runtime: %v", err)
	}

	// Adapter implements HistoryReader: reachable via Adapter Access.
	hr, ok := chat.AdapterAs[chat.HistoryReader](bot, "withhistory")
	if !ok {
		t.Fatal("expected withhistory to be reachable as HistoryReader")
	}
	msgs, err := hr.ReadHistory(context.Background(), chat.ThreadID("t"), chat.HistoryQuery{})
	if err != nil {
		t.Fatalf("read history: %v", err)
	}
	if len(msgs) != 1 || msgs[0].ID != "m1" {
		t.Fatalf("messages = %#v", msgs)
	}

	// Adapter does NOT implement HistoryReader: explicit unsupported result
	// (ok == false), NOT an empty slice masquerading as "no history".
	_, ok = chat.AdapterAs[chat.HistoryReader](bot, "nohistory")
	if ok {
		t.Fatal("expected nohistory to be unsupported (ok == false), not an empty-slice no-history")
	}

	// Wrong adapter name: also ok == false.
	if _, ok := chat.AdapterAs[chat.HistoryReader](bot, "nope"); ok {
		t.Fatal("expected unknown adapter name to be ok == false")
	}

	// The interface{ chat.HistoryReader } structural form also resolves, mirroring
	// the documented usage.
	if _, ok := chat.AdapterAs[interface {
		chat.HistoryReader
	}](bot, "withhistory"); !ok {
		t.Fatal("expected structural HistoryReader interface to resolve")
	}
}

// History is never reachable outside Adapter Access: there is no bot.ReadHistory and
// no Thread.History / Thread.ReadHistory method on the core surface.
func TestHistoryHasNoCoreSurface(t *testing.T) {
	t.Parallel()

	chatType := reflect.TypeFor[*chat.Chat]()
	for _, name := range []string{"ReadHistory", "History"} {
		if _, ok := chatType.MethodByName(name); ok {
			t.Fatalf("*chat.Chat must not expose %s; history is Adapter-Access only", name)
		}
	}

	threadType := reflect.TypeFor[*chat.Thread]()
	for _, name := range []string{"ReadHistory", "History"} {
		if _, ok := threadType.MethodByName(name); ok {
			t.Fatalf("*chat.Thread must not expose %s; history is Adapter-Access only", name)
		}
	}
}

// recordingState wraps a State and counts every method call, so a ReadHistory can be
// proven to perform no Runtime State writes (no subscription change, no MarkEvent
// dedupe entry, no Lock Lease acquisition).
type recordingState struct {
	inner chat.State
	calls int
}

func (s *recordingState) IsThreadSubscribed(ctx context.Context, id chat.ThreadID) (bool, error) {
	s.calls++
	return s.inner.IsThreadSubscribed(ctx, id)
}
func (s *recordingState) SubscribeThread(ctx context.Context, id chat.ThreadID) error {
	s.calls++
	return s.inner.SubscribeThread(ctx, id)
}
func (s *recordingState) UnsubscribeThread(ctx context.Context, id chat.ThreadID) error {
	s.calls++
	return s.inner.UnsubscribeThread(ctx, id)
}
func (s *recordingState) MarkEvent(ctx context.Context, key string, ttl time.Duration) (bool, error) {
	s.calls++
	return s.inner.MarkEvent(ctx, key, ttl)
}
func (s *recordingState) AcquireLock(ctx context.Context, key string, ttl time.Duration) (chat.LockLease, bool, error) {
	s.calls++
	return s.inner.AcquireLock(ctx, key, ttl)
}
func (s *recordingState) ExtendLock(ctx context.Context, lease chat.LockLease, ttl time.Duration) (bool, error) {
	s.calls++
	return s.inner.ExtendLock(ctx, lease, ttl)
}
func (s *recordingState) ReleaseLock(ctx context.Context, lease chat.LockLease) (bool, error) {
	s.calls++
	return s.inner.ReleaseLock(ctx, lease)
}
func (s *recordingState) Shutdown(ctx context.Context) error {
	s.calls++
	return s.inner.Shutdown(ctx)
}

// ReadHistory performs no Runtime State access at all: it is a live platform read,
// never plumbed through c.state.
func TestHistoryReaderPerformsNoStateWrites(t *testing.T) {
	t.Parallel()

	spy := &recordingState{inner: newFakeState()}
	adapter := &historyFakeAdapter{name: "withhistory", messages: []chat.Message{{ID: "m1"}}}
	bot, err := chat.New(context.Background(),
		chat.WithState(spy),
		chat.WithAdapter(adapter),
	)
	if err != nil {
		t.Fatalf("new runtime: %v", err)
	}

	hr, ok := chat.AdapterAs[chat.HistoryReader](bot, "withhistory")
	if !ok {
		t.Fatal("expected HistoryReader capability")
	}
	if _, err := hr.ReadHistory(context.Background(), chat.ThreadID("t"), chat.HistoryQuery{Limit: 10}); err != nil {
		t.Fatalf("read history: %v", err)
	}
	if spy.calls != 0 {
		t.Fatalf("ReadHistory touched Runtime State %d times, want 0", spy.calls)
	}
}
