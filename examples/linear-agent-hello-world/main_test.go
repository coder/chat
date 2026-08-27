package main

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/coder/chat"
	"github.com/coder/chat/adapters/linear"
	"github.com/coder/chat/state/memory"
)

func TestNewWebhookServer(t *testing.T) {
	server := newWebhookServer(":0", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))

	if server.Addr != ":0" {
		t.Fatalf("addr = %q, want :0", server.Addr)
	}
	if server.ReadHeaderTimeout != 5*time.Second {
		t.Fatalf("read header timeout = %s", server.ReadHeaderTimeout)
	}
	if server.ReadTimeout != 10*time.Second {
		t.Fatalf("read timeout = %s", server.ReadTimeout)
	}
	if server.WriteTimeout != 10*time.Second {
		t.Fatalf("write timeout = %s", server.WriteTimeout)
	}
	if server.IdleTimeout != 60*time.Second {
		t.Fatalf("idle timeout = %s", server.IdleTimeout)
	}

	req := httptest.NewRequest(http.MethodPost, "/webhooks/linear", nil)
	rec := httptest.NewRecorder()
	server.Handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusNoContent)
	}
}

func TestNewMentionHandlerRollsBackSubscriptionWhenReplyFails(t *testing.T) {
	ctx := context.Background()
	state := memory.New()
	postErr := errors.New("post failed")
	adapter := &testLinearAdapter{postErr: postErr}
	bot, err := chat.New(ctx, chat.WithState(state), chat.WithAdapter(adapter))
	if err != nil {
		t.Fatalf("new chat: %v", err)
	}
	defer func() {
		if err := bot.Shutdown(context.Background()); err != nil {
			t.Fatalf("shutdown chat: %v", err)
		}
	}()

	threadID := chat.ThreadID("linear:v1:thread-1")
	thread, err := bot.Thread(ctx, threadID)
	if err != nil {
		t.Fatalf("thread: %v", err)
	}

	err = newMentionHandler(adapter)(ctx, &chat.MessageEvent{
		Thread:  thread,
		Message: &chat.Message{Text: "hello"},
	})
	if !errors.Is(err, postErr) {
		t.Fatalf("handler error = %v, want %v", err, postErr)
	}

	subscribed, err := state.IsThreadSubscribed(ctx, threadID)
	if err != nil {
		t.Fatalf("check subscribed: %v", err)
	}
	if subscribed {
		t.Fatal("thread remains subscribed after failed reply")
	}
}

// testLinearAdapter is a fake chat.Adapter and linearAgentAccess that records
// what the handlers send so the worked examples in capabilities.go are testable
// without a live Linear org. Tests drive handlers synchronously, so no locking.
type testLinearAdapter struct {
	postErr        error
	posted         []string
	thoughts       []string
	elicitations   []linear.ElicitationInput
	sessionUpdates []linear.AgentSessionUpdateInput
	sessionCreates []linear.CreateSessionOnIssueInput
	suggestions    []linear.RepositorySuggestion
}

func (a *testLinearAdapter) Name() string { return "linear" }

func (a *testLinearAdapter) Init(context.Context) error { return nil }

func (a *testLinearAdapter) Shutdown(context.Context) error { return nil }

func (a *testLinearAdapter) Webhook(chat.DispatchFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
}

func (a *testLinearAdapter) ValidateThreadID(id chat.ThreadID) (chat.ThreadRef, error) {
	return chat.ThreadRef{ID: id, Adapter: "linear"}, nil
}

func (a *testLinearAdapter) PostMessage(_ context.Context, thread chat.ThreadRef, msg chat.PostableMessage) (*chat.SentMessage, error) {
	if a.postErr != nil {
		return nil, a.postErr
	}
	a.posted = append(a.posted, msg.Text)
	return &chat.SentMessage{ID: "sent-1", ThreadID: thread.ID}, nil
}

func (a *testLinearAdapter) BotActor() chat.Actor {
	return chat.Actor{Adapter: "linear", Tenant: "org", ID: "bot", BotKind: chat.BotBot}
}

func (a *testLinearAdapter) PostThought(_ context.Context, id chat.ThreadID, text string) (*chat.SentMessage, error) {
	a.thoughts = append(a.thoughts, text)
	return &chat.SentMessage{ID: "thought-1", ThreadID: id}, nil
}

func (a *testLinearAdapter) PostAction(_ context.Context, id chat.ThreadID, _ linear.ActionInput) (*chat.SentMessage, error) {
	return &chat.SentMessage{ID: "action-1", ThreadID: id}, nil
}

func (a *testLinearAdapter) PostElicitation(_ context.Context, id chat.ThreadID, in linear.ElicitationInput) (*chat.SentMessage, error) {
	a.elicitations = append(a.elicitations, in)
	return &chat.SentMessage{ID: "elicitation-1", ThreadID: id}, nil
}

func (a *testLinearAdapter) PostError(_ context.Context, id chat.ThreadID, _ linear.ErrorInput) (*chat.SentMessage, error) {
	return &chat.SentMessage{ID: "error-1", ThreadID: id}, nil
}

func (a *testLinearAdapter) UpdateSession(_ context.Context, _ chat.ThreadID, in linear.AgentSessionUpdateInput) error {
	a.sessionUpdates = append(a.sessionUpdates, in)
	return nil
}

func (a *testLinearAdapter) CreateSessionOnIssue(_ context.Context, in linear.CreateSessionOnIssueInput) (*linear.CreatedAgentSession, error) {
	a.sessionCreates = append(a.sessionCreates, in)
	return &linear.CreatedAgentSession{ThreadID: chat.ThreadID("linear:v1:new-session"), SessionID: "S-NEW", IssueID: "ISSUE-NEW"}, nil
}

func (a *testLinearAdapter) SuggestRepositories(context.Context, chat.ThreadID, []linear.CandidateRepository) ([]linear.RepositorySuggestion, error) {
	return a.suggestions, nil
}
