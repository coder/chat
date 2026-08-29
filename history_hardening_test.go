package chat_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/coder/chat"
)

// dispatchingHistoryAdapter is a minimal Adapter that ALSO implements HistoryReader
// and drives a real inbound dispatch from its Webhook. It is used to prove the
// runtime never auto-invokes ReadHistory while routing an inbound Event: history is
// reached only through Adapter Access, never on the dispatch path (ADR 0009).
type dispatchingHistoryAdapter struct {
	name        string
	historyHits atomic.Int32
}

func (a *dispatchingHistoryAdapter) Name() string                   { return a.name }
func (a *dispatchingHistoryAdapter) Init(context.Context) error     { return nil }
func (a *dispatchingHistoryAdapter) Shutdown(context.Context) error { return nil }

func (a *dispatchingHistoryAdapter) Webhook(dispatch chat.DispatchFunc) http.Handler {
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

func (a *dispatchingHistoryAdapter) ValidateThreadID(id chat.ThreadID) (chat.ThreadRef, error) {
	return chat.ThreadRef{ID: id, Adapter: a.name, Tenant: "tenant", Channel: string(id)}, nil
}

func (a *dispatchingHistoryAdapter) PostMessage(context.Context, chat.ThreadRef, chat.PostableMessage) (*chat.SentMessage, error) {
	return &chat.SentMessage{}, nil
}

func (a *dispatchingHistoryAdapter) BotActor() chat.Actor {
	return chat.Actor{Adapter: a.name, Tenant: "tenant", ID: "bot", BotKind: chat.BotBot}
}

func (a *dispatchingHistoryAdapter) ReadHistory(context.Context, chat.ThreadID, chat.HistoryQuery) ([]chat.Message, error) {
	a.historyHits.Add(1)
	return nil, nil
}

// History is never auto-invoked during Runtime Dispatch. Routing an inbound message
// Event (which the handler also processes) must not call the adapter's ReadHistory:
// history is not a Routing Hook input and never runs on the dispatch path.
func TestHistoryReaderNotInvokedDuringDispatch(t *testing.T) {
	t.Parallel()

	adapter := &dispatchingHistoryAdapter{name: "histdispatch"}
	bot, err := chat.New(context.Background(),
		chat.WithState(newFakeState()),
		chat.WithAdapter(adapter),
	)
	if err != nil {
		t.Fatalf("new runtime: %v", err)
	}

	var handled atomic.Int32
	bot.OnNewMention(func(context.Context, *chat.MessageEvent) error {
		handled.Add(1)
		return nil
	})

	handler, err := bot.Webhook("histdispatch")
	if err != nil {
		t.Fatalf("webhook: %v", err)
	}

	ev := chat.Event{
		ID:       "evt-1",
		Adapter:  "histdispatch",
		Tenant:   "tenant",
		ThreadID: chat.ThreadID("thread-1"),
		Message: &chat.Message{
			ID:        "m-1",
			Text:      "hey bot",
			Mentioned: true,
			Author:    chat.Actor{Adapter: "histdispatch", Tenant: "tenant", ID: "U1", BotKind: chat.BotHuman},
		},
	}
	body, err := json.Marshal(ev)
	if err != nil {
		t.Fatalf("marshal event: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/webhook", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("dispatch status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if got := handled.Load(); got != 1 {
		t.Fatalf("handler invocations = %d, want 1 (event must route)", got)
	}
	if got := adapter.historyHits.Load(); got != 0 {
		t.Fatalf("ReadHistory invoked %d times during dispatch, want 0", got)
	}
}

// A live read of an empty Thread returns a real, non-error empty result. The
// unsupported-capability signal (AdapterAs ok == false) is the ONLY "no history
// available" signal; a present capability that finds nothing returns successfully
// with no messages and a nil error, so the two are never conflated.
func TestHistoryReaderEmptyThreadIsNotUnsupported(t *testing.T) {
	t.Parallel()

	adapter := &historyFakeAdapter{name: "emptyhist", messages: nil}
	bot, err := chat.New(context.Background(),
		chat.WithState(newFakeState()),
		chat.WithAdapter(adapter),
	)
	if err != nil {
		t.Fatalf("new runtime: %v", err)
	}

	hr, ok := chat.AdapterAs[chat.HistoryReader](bot, "emptyhist")
	if !ok {
		t.Fatal("expected emptyhist to be reachable as HistoryReader (capability present)")
	}
	msgs, err := hr.ReadHistory(context.Background(), chat.ThreadID("t"), chat.HistoryQuery{})
	if err != nil {
		t.Fatalf("read history of empty thread: %v", err)
	}
	if len(msgs) != 0 {
		t.Fatalf("messages = %#v, want empty", msgs)
	}
	if adapter.reads != 1 {
		t.Fatalf("ReadHistory reads = %d, want 1 (it is a live read, not short-circuited)", adapter.reads)
	}
}

// A propagated read error surfaces to the caller unchanged: the capability is
// present (ok == true) and the error is the read's error, distinct from the
// capability-absent path. This keeps "capability missing" and "read failed"
// separate failure modes.
func TestHistoryReaderReadErrorSurfaces(t *testing.T) {
	t.Parallel()

	wantErr := errors.New("platform boom")
	adapter := &erroringHistoryAdapter{name: "errhist", err: wantErr}
	bot, err := chat.New(context.Background(),
		chat.WithState(newFakeState()),
		chat.WithAdapter(adapter),
	)
	if err != nil {
		t.Fatalf("new runtime: %v", err)
	}

	hr, ok := chat.AdapterAs[chat.HistoryReader](bot, "errhist")
	if !ok {
		t.Fatal("expected errhist HistoryReader capability present")
	}
	msgs, err := hr.ReadHistory(context.Background(), chat.ThreadID("t"), chat.HistoryQuery{})
	if !errors.Is(err, wantErr) {
		t.Fatalf("error = %v, want %v", err, wantErr)
	}
	if msgs != nil {
		t.Fatalf("messages = %#v, want nil on read error", msgs)
	}
}

// erroringHistoryAdapter implements HistoryReader but always fails the read.
type erroringHistoryAdapter struct {
	name string
	err  error
}

func (a *erroringHistoryAdapter) Name() string                   { return a.name }
func (a *erroringHistoryAdapter) Init(context.Context) error     { return nil }
func (a *erroringHistoryAdapter) Shutdown(context.Context) error { return nil }
func (a *erroringHistoryAdapter) Webhook(chat.DispatchFunc) http.Handler {
	return http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})
}
func (a *erroringHistoryAdapter) ValidateThreadID(id chat.ThreadID) (chat.ThreadRef, error) {
	return chat.ThreadRef{ID: id, Adapter: a.name, Tenant: "tenant"}, nil
}
func (a *erroringHistoryAdapter) PostMessage(context.Context, chat.ThreadRef, chat.PostableMessage) (*chat.SentMessage, error) {
	return &chat.SentMessage{}, nil
}
func (a *erroringHistoryAdapter) BotActor() chat.Actor {
	return chat.Actor{Adapter: a.name, Tenant: "tenant", ID: "bot", BotKind: chat.BotBot}
}
func (a *erroringHistoryAdapter) ReadHistory(context.Context, chat.ThreadID, chat.HistoryQuery) ([]chat.Message, error) {
	return nil, a.err
}
