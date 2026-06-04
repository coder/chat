package msteams_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/coder/chat"
	"github.com/coder/chat/adapters/msteams"
	"github.com/coder/chat/state/memory"
)

// TestRuntimeNewMentionDispatchesAndReplies wires the adapter behind the real chat
// runtime (memory state) and proves the whole turn: a validly-signed @mention
// Activity reaches the OnNewMention handler, and the handler's Thread.Post goes out
// as a separate authenticated Connector "send to conversation" call.
func TestRuntimeNewMentionDispatchesAndReplies(t *testing.T) {
	t.Parallel()
	bf := newFakeBotConnector(t)

	var posted map[string]any
	var postAuth string
	conn := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		postAuth = r.Header.Get("Authorization")
		_ = json.NewDecoder(r.Body).Decode(&posted)
		_ = json.NewEncoder(w).Encode(map[string]any{"id": "reply-1"})
	}))
	t.Cleanup(conn.Close)
	tok := fakeTokenServer(t, "out-token", nil)

	adapter := newTestAdapter(t, bf, func(o *msteams.Options) { o.TokenURL = tok.URL })
	bot, err := chat.New(context.Background(),
		chat.WithState(memory.New()),
		chat.WithAdapter(adapter),
	)
	if err != nil {
		t.Fatalf("chat.New: %v", err)
	}

	var handledThread chat.ThreadID
	bot.OnNewMention(func(ctx context.Context, ev *chat.MessageEvent) error {
		handledThread = ev.Thread.ID()
		_, postErr := ev.Thread.Post(ctx, chat.Markdown("hi back"))
		return postErr
	})

	handler, err := bot.Webhook("msteams")
	if err != nil {
		t.Fatalf("webhook: %v", err)
	}

	// Point both the Activity serviceUrl and the token's serviceurl claim at the
	// fake Connector so the reply is delivered there.
	act := messageActivity()
	act["serviceUrl"] = conn.URL
	claims := bf.validClaims()
	claims["serviceurl"] = conn.URL

	rec := postActivity(t, handler, "Bearer "+bf.sign(t, claims), act)
	if rec.Code != http.StatusOK {
		t.Fatalf("webhook status = %d, body %s", rec.Code, rec.Body.String())
	}
	if handledThread == "" {
		t.Fatal("OnNewMention handler did not run")
	}
	if posted == nil {
		t.Fatal("handler reply was not sent to the Connector")
	}
	if posted["text"] != "hi back" || posted["textFormat"] != "markdown" {
		t.Fatalf("posted reply = %#v", posted)
	}
	if postAuth != "Bearer out-token" {
		t.Fatalf("reply auth = %q, want bearer outbound token", postAuth)
	}
	if conv, _ := posted["conversation"].(map[string]any); conv["id"] != testConvID {
		t.Fatalf("reply conversation = %#v", posted["conversation"])
	}
}
