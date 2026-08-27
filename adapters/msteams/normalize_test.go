package msteams_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/coder/chat"
	"github.com/coder/chat/adapters/msteams"
)

// dispatchActivity posts a validly-signed Activity through the Webhook and returns
// the dispatched Event (nil if the Activity was an Ignored Event) and the HTTP code.
func dispatchActivity(t *testing.T, a *msteams.Adapter, bf *fakeBotConnector, activity map[string]any) (*chat.Event, int) {
	t.Helper()
	var got *chat.Event
	h := a.Webhook(func(_ context.Context, ev *chat.Event) error {
		got = ev
		return nil
	})
	rec := postActivity(t, h, "Bearer "+bf.sign(t, bf.validClaims()), activity)
	return got, rec.Code
}

func TestNormalizeChannelMention(t *testing.T) {
	t.Parallel()
	bf := newFakeBotConnector(t)
	a := newTestAdapter(t, bf, nil)

	got, code := dispatchActivity(t, a, bf, messageActivity())
	if code != http.StatusOK {
		t.Fatalf("status = %d", code)
	}
	if got == nil {
		t.Fatal("event was not dispatched")
	}
	if got.Adapter != "msteams" || got.Tenant != testTenant || got.ID != "activity-1" {
		t.Fatalf("event envelope = %#v", got)
	}
	if got.DirectMessage {
		t.Fatal("channel message marked as direct")
	}
	if got.Message == nil {
		t.Fatal("message is nil")
	}
	if got.Message.Text != "hello there" {
		t.Fatalf("text = %q, want %q (bot mention should be stripped)", got.Message.Text, "hello there")
	}
	if !got.Message.Mentioned {
		t.Fatal("bot mention entity should set Mentioned")
	}
	wantAuthor := chat.Actor{Adapter: "msteams", Tenant: testTenant, ID: "aad-alice", Name: "Alice", BotKind: chat.BotHuman}
	if got.Message.Author != wantAuthor {
		t.Fatalf("author = %#v, want %#v", got.Message.Author, wantAuthor)
	}

	// The opaque Thread ID round-trips through ValidateThreadID to the conversation.
	ref, err := a.ValidateThreadID(got.ThreadID)
	if err != nil {
		t.Fatalf("validate thread id: %v", err)
	}
	if ref.Channel != testConvID || ref.Tenant != testTenant || ref.Direct {
		t.Fatalf("thread ref = %#v", ref)
	}
}

func TestNormalizeDirectMessage(t *testing.T) {
	t.Parallel()
	bf := newFakeBotConnector(t)
	a := newTestAdapter(t, bf, nil)

	act := messageActivity()
	act["text"] = "hello in dm"
	act["entities"] = []map[string]any{}
	act["conversation"] = map[string]any{
		"id":               "19:dm-conv",
		"tenantId":         testTenant,
		"conversationType": "personal",
		"isGroup":          false,
	}

	got, code := dispatchActivity(t, a, bf, act)
	if code != http.StatusOK || got == nil {
		t.Fatalf("status = %d event = %v", code, got)
	}
	if !got.DirectMessage {
		t.Fatal("personal conversation should be a direct message")
	}
	if got.Message.Mentioned {
		t.Fatal("no mention entity, Mentioned should be false")
	}
}

func TestNormalizeMentionRequiresEntity(t *testing.T) {
	t.Parallel()
	bf := newFakeBotConnector(t)
	a := newTestAdapter(t, bf, nil)

	// A channel message whose text contains the bot's display name but carries no
	// mention entity must NOT be treated as a mention.
	act := messageActivity()
	act["text"] = "hey Bot are you there"
	act["entities"] = []map[string]any{}

	got, code := dispatchActivity(t, a, bf, act)
	if code != http.StatusOK || got == nil {
		t.Fatalf("status = %d event = %v", code, got)
	}
	if got.Message.Mentioned {
		t.Fatal("substring of bot name must not set Mentioned")
	}
	if got.Message.Text != "hey Bot are you there" {
		t.Fatalf("text = %q", got.Message.Text)
	}
}

func TestNormalizeActorFallsBackToFromID(t *testing.T) {
	t.Parallel()
	bf := newFakeBotConnector(t)
	a := newTestAdapter(t, bf, nil)

	act := messageActivity()
	act["from"] = map[string]any{"id": "29:user-bbb", "name": "Bob"} // no aadObjectId

	got, code := dispatchActivity(t, a, bf, act)
	if code != http.StatusOK || got == nil {
		t.Fatalf("status = %d event = %v", code, got)
	}
	if got.Message.Author.ID != "29:user-bbb" {
		t.Fatalf("author id = %q, want fallback to from.id", got.Message.Author.ID)
	}
}

func TestNormalizeMentionStripFallbackAndForeignMention(t *testing.T) {
	t.Parallel()
	bf := newFakeBotConnector(t)
	a := newTestAdapter(t, bf, nil)

	t.Run("bot mention with empty entity text uses leading-tag fallback", func(t *testing.T) {
		act := messageActivity()
		act["text"] = "<at>Bot</at> do the thing"
		act["entities"] = []map[string]any{
			{"type": "mention", "mentioned": map[string]any{"id": testBotID, "name": "Bot"}}, // no text
		}
		got, code := dispatchActivity(t, a, bf, act)
		if code != http.StatusOK || got == nil {
			t.Fatalf("status = %d event = %v", code, got)
		}
		if !got.Message.Mentioned {
			t.Fatal("bot should be detected as mentioned via entity")
		}
		if got.Message.Text != "do the thing" {
			t.Fatalf("text = %q, want %q", got.Message.Text, "do the thing")
		}
	})

	t.Run("foreign leading mention is not stripped", func(t *testing.T) {
		act := messageActivity()
		act["text"] = "<at>Other</at> look at this bot"
		act["entities"] = []map[string]any{
			{"type": "mention", "text": "<at>Other</at>", "mentioned": map[string]any{"id": "29:other-user", "name": "Other"}},
		}
		got, code := dispatchActivity(t, a, bf, act)
		if code != http.StatusOK || got == nil {
			t.Fatalf("status = %d event = %v", code, got)
		}
		if got.Message.Mentioned {
			t.Fatal("bot is not mentioned; Mentioned should be false")
		}
		if got.Message.Text != "<at>Other</at> look at this bot" {
			t.Fatalf("text = %q, foreign mention must be preserved", got.Message.Text)
		}
	})
}

func TestNormalizeSelfMessageIgnored(t *testing.T) {
	t.Parallel()
	bf := newFakeBotConnector(t)
	a := newTestAdapter(t, bf, nil)

	// The bot's own echo: from.id is the bot id. It must be dropped before dispatch
	// (acked with 200, never routed) so the bot cannot loop.
	act := messageActivity()
	act["from"] = map[string]any{"id": testBotID, "name": "Bot"}

	got, code := dispatchActivity(t, a, bf, act)
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200 ack", code)
	}
	if got != nil {
		t.Fatalf("self message reached dispatch: %#v", got)
	}
}

func TestNormalizeNonMessageIgnored(t *testing.T) {
	t.Parallel()
	bf := newFakeBotConnector(t)
	a := newTestAdapter(t, bf, nil)

	act := messageActivity()
	act["type"] = "conversationUpdate"

	got, code := dispatchActivity(t, a, bf, act)
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200 ack", code)
	}
	if got != nil {
		t.Fatalf("non-message reached dispatch: %#v", got)
	}
}

func TestWebhookRejectsNonPost(t *testing.T) {
	t.Parallel()
	bf := newFakeBotConnector(t)
	a := newTestAdapter(t, bf, nil)
	h, _ := recordingHandler(a)

	req := httptest.NewRequest(http.MethodGet, "/api/messages", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET status = %d, want 405", rec.Code)
	}
}
