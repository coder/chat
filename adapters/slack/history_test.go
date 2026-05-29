package slack_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/coder/chat"
	"github.com/coder/chat/adapters/slack"
)

func historyReader(t *testing.T, bot *chat.Chat) chat.HistoryReader {
	t.Helper()
	hr, ok := chat.AdapterAs[chat.HistoryReader](bot, "slack")
	if !ok {
		t.Fatal("slack adapter does not implement chat.HistoryReader")
	}
	return hr
}

// ReadHistory on a thread-rooted Thread ID reads conversations.replies with the
// channel and root ts decoded from the opaque Thread ID, and normalizes each Slack
// message into a chat.Message using the same actor mapping as inbound events.
func TestSlackReadHistoryThreadRepliesNormalization(t *testing.T) {
	t.Parallel()

	api := newSlackAPIServer(t)
	api.historyResp = map[string]any{
		"ok": true,
		"messages": []any{
			map[string]any{"type": "message", "user": "U1", "text": "hello", "ts": "111.000", "thread_ts": "111.000"},
			map[string]any{"type": "message", "bot_id": "BBOT", "subtype": "bot_message", "text": "hi back", "ts": "112.000", "thread_ts": "111.000"},
		},
	}
	bot := newSlackRuntime(t, api, slack.Options{
		SigningSecret: "secret",
		BotToken:      "xoxb-test",
		TeamID:        "T1",
		BotUserID:     "UBOT",
		BotID:         "BBOT",
	})
	hr := historyReader(t, bot)

	id := slack.EncodeThreadReplyThreadIDForTest("T1", "C1", "111.000")
	msgs, err := hr.ReadHistory(context.Background(), id, chat.HistoryQuery{Limit: 20})
	if err != nil {
		t.Fatalf("read history: %v", err)
	}
	if len(api.historyReqs) != 1 {
		t.Fatalf("history requests = %d, want 1", len(api.historyReqs))
	}
	req := api.historyReqs[0]
	if req.Method != "/conversations.replies" {
		t.Fatalf("method = %q, want conversations.replies", req.Method)
	}
	if req.Channel != "C1" || req.TS != "111.000" {
		t.Fatalf("request channel/ts = %q/%q, want C1/111.000", req.Channel, req.TS)
	}
	if req.Limit != 20 {
		t.Fatalf("request limit = %d, want 20", req.Limit)
	}

	if len(msgs) != 2 {
		t.Fatalf("messages = %d, want 2", len(msgs))
	}
	if msgs[0].ID != "111.000" || msgs[0].Text != "hello" {
		t.Fatalf("msg0 = %#v", msgs[0])
	}
	if msgs[0].Author.ID != "U1" || msgs[0].Author.BotKind != chat.BotHuman {
		t.Fatalf("msg0 author = %#v, want human U1", msgs[0].Author)
	}
	if msgs[0].Author.Adapter != "slack" || msgs[0].Author.Tenant != "T1" {
		t.Fatalf("msg0 author scope = %#v", msgs[0].Author)
	}
	if msgs[1].ID != "112.000" || msgs[1].Text != "hi back" {
		t.Fatalf("msg1 = %#v", msgs[1])
	}
	if msgs[1].Author.BotKind != chat.BotBot {
		t.Fatalf("msg1 author = %#v, want bot", msgs[1].Author)
	}
}

// Each returned Message preserves the verbatim per-message JSON via the Platform
// Escape Hatch (Message.Raw).
func TestSlackReadHistoryPreservesRaw(t *testing.T) {
	t.Parallel()

	api := newSlackAPIServer(t)
	api.historyResp = map[string]any{
		"ok": true,
		"messages": []any{
			map[string]any{"type": "message", "user": "U1", "text": "hello", "ts": "111.000", "reactions": []any{map[string]any{"name": "wave"}}},
		},
	}
	bot := newSlackRuntime(t, api, slack.Options{
		SigningSecret: "secret",
		BotToken:      "xoxb-test",
		TeamID:        "T1",
		BotUserID:     "UBOT",
		BotID:         "BBOT",
	})
	hr := historyReader(t, bot)

	id := slack.EncodeThreadReplyThreadIDForTest("T1", "C1", "111.000")
	msgs, err := hr.ReadHistory(context.Background(), id, chat.HistoryQuery{})
	if err != nil {
		t.Fatalf("read history: %v", err)
	}
	if len(msgs) != 1 {
		t.Fatalf("messages = %d, want 1", len(msgs))
	}
	raw, ok := msgs[0].Raw.(json.RawMessage)
	if !ok {
		t.Fatalf("Raw type = %T, want json.RawMessage", msgs[0].Raw)
	}
	var decoded struct {
		TS        string `json:"ts"`
		Reactions []struct {
			Name string `json:"name"`
		} `json:"reactions"`
	}
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("decode raw: %v", err)
	}
	if decoded.TS != "111.000" {
		t.Fatalf("raw ts = %q", decoded.TS)
	}
	if len(decoded.Reactions) != 1 || decoded.Reactions[0].Name != "wave" {
		t.Fatalf("raw did not preserve reactions: %s", raw)
	}
}

// Limit clamping is adapter-owned: above Slack's max it clamps to 1000, and <= 0
// uses the adapter default (100).
func TestSlackReadHistoryClampsLimit(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name      string
		limit     int
		wantLimit int
	}{
		{"above-max", 5000, 1000},
		{"zero-default", 0, 100},
		{"negative-default", -3, 100},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			api := newSlackAPIServer(t)
			bot := newSlackRuntime(t, api, slack.Options{
				SigningSecret: "secret",
				BotToken:      "xoxb-test",
				TeamID:        "T1",
				BotUserID:     "UBOT",
				BotID:         "BBOT",
			})
			hr := historyReader(t, bot)
			id := slack.EncodeThreadReplyThreadIDForTest("T1", "C1", "111.000")
			if _, err := hr.ReadHistory(context.Background(), id, chat.HistoryQuery{Limit: tc.limit}); err != nil {
				t.Fatalf("read history: %v", err)
			}
			if len(api.historyReqs) != 1 {
				t.Fatalf("history requests = %d, want 1", len(api.historyReqs))
			}
			if got := api.historyReqs[0].Limit; got != tc.wantLimit {
				t.Fatalf("limit = %d, want %d", got, tc.wantLimit)
			}
		})
	}
}

// The Before cursor (a Message.ID) maps to Slack latest=<ts> with inclusive=false,
// paging toward older messages.
func TestSlackReadHistoryBeforeCursor(t *testing.T) {
	t.Parallel()

	api := newSlackAPIServer(t)
	bot := newSlackRuntime(t, api, slack.Options{
		SigningSecret: "secret",
		BotToken:      "xoxb-test",
		TeamID:        "T1",
		BotUserID:     "UBOT",
		BotID:         "BBOT",
	})
	hr := historyReader(t, bot)
	id := slack.EncodeThreadReplyThreadIDForTest("T1", "C1", "100.000")
	if _, err := hr.ReadHistory(context.Background(), id, chat.HistoryQuery{Before: "111.000"}); err != nil {
		t.Fatalf("read history: %v", err)
	}
	req := api.historyReqs[0]
	if req.Latest != "111.000" {
		t.Fatalf("latest = %q, want 111.000", req.Latest)
	}
	if req.Inclusive {
		t.Fatalf("inclusive = true, want false")
	}
}

// A direct-message Thread ID reads conversations.history, not conversations.replies.
func TestSlackReadHistoryDirectUsesHistory(t *testing.T) {
	t.Parallel()

	api := newSlackAPIServer(t)
	bot := newSlackRuntime(t, api, slack.Options{
		SigningSecret: "secret",
		BotToken:      "xoxb-test",
		TeamID:        "T1",
		BotUserID:     "UBOT",
		BotID:         "BBOT",
	})
	hr := historyReader(t, bot)
	id := slack.EncodeDirectThreadIDForTest("T1", "D1")
	if _, err := hr.ReadHistory(context.Background(), id, chat.HistoryQuery{}); err != nil {
		t.Fatalf("read history: %v", err)
	}
	req := api.historyReqs[0]
	if req.Method != "/conversations.history" {
		t.Fatalf("method = %q, want conversations.history", req.Method)
	}
	if req.Channel != "D1" {
		t.Fatalf("channel = %q, want D1", req.Channel)
	}
	if req.TS != "" {
		t.Fatalf("history request carried ts = %q, want empty", req.TS)
	}
}

// A cancelled context aborts the platform read promptly (the read never outlives the
// caller's deadline). callWithToken threads ctx into the HTTP request.
func TestSlackReadHistoryContextCancellation(t *testing.T) {
	t.Parallel()

	api := newSlackAPIServer(t)
	api.historyBlock = make(chan struct{}) // server blocks until ctx is cancelled
	bot := newSlackRuntime(t, api, slack.Options{
		SigningSecret: "secret",
		BotToken:      "xoxb-test",
		TeamID:        "T1",
		BotUserID:     "UBOT",
		BotID:         "BBOT",
	})
	hr := historyReader(t, bot)
	id := slack.EncodeThreadReplyThreadIDForTest("T1", "C1", "111.000")

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	done := make(chan error, 1)
	go func() {
		_, err := hr.ReadHistory(ctx, id, chat.HistoryQuery{})
		done <- err
	}()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("expected context cancellation error, got nil")
		}
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("error = %v, want context.Canceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("read did not return promptly on cancelled context")
	}
}

// An ok:false Slack response surfaces as an error, never a silent empty slice.
func TestSlackReadHistoryAPIErrorSurfaces(t *testing.T) {
	t.Parallel()

	api := newSlackAPIServer(t)
	api.historyResp = map[string]any{"ok": false, "error": "channel_not_found"}
	bot := newSlackRuntime(t, api, slack.Options{
		SigningSecret: "secret",
		BotToken:      "xoxb-test",
		TeamID:        "T1",
		BotUserID:     "UBOT",
		BotID:         "BBOT",
	})
	hr := historyReader(t, bot)
	id := slack.EncodeThreadReplyThreadIDForTest("T1", "C1", "111.000")
	msgs, err := hr.ReadHistory(context.Background(), id, chat.HistoryQuery{})
	if err == nil {
		t.Fatal("expected error for ok:false response")
	}
	if msgs != nil {
		t.Fatalf("messages = %#v, want nil on error", msgs)
	}
}

// A malformed Thread ID returns the decode error, never an empty slice.
func TestSlackReadHistoryMalformedThreadID(t *testing.T) {
	t.Parallel()

	api := newSlackAPIServer(t)
	bot := newSlackRuntime(t, api, slack.Options{
		SigningSecret: "secret",
		BotToken:      "xoxb-test",
		TeamID:        "T1",
		BotUserID:     "UBOT",
		BotID:         "BBOT",
	})
	hr := historyReader(t, bot)
	msgs, err := hr.ReadHistory(context.Background(), chat.ThreadID("not-a-slack-id"), chat.HistoryQuery{})
	if err == nil {
		t.Fatal("expected decode error for malformed thread id")
	}
	if msgs != nil {
		t.Fatalf("messages = %#v, want nil on error", msgs)
	}
	if len(api.historyReqs) != 0 {
		t.Fatalf("history requests = %d, want 0 (no platform call for malformed id)", len(api.historyReqs))
	}
}

// In multi-tenant mode the read resolves the per-workspace bot token from the
// Thread ID's team via the InstallStore (reusing postToken), proving no new
// credential plumbing.
func TestSlackReadHistoryMultiTenantToken(t *testing.T) {
	t.Parallel()

	api := newSlackAPIServer(t)
	now := time.Unix(1_700_000_000, 0)
	store := newFakeInstallStore()
	store.set("T2", chat.Install{Tenant: "T2", Credential: slack.SlackInstall{BotToken: "xoxb-T2", BotUserID: "UBOT2"}})
	bot := newMultiTenantSlackRuntime(t, api, store, now)
	hr := historyReader(t, bot)

	id := slack.EncodeThreadReplyThreadIDForTest("T2", "C9", "200.000")
	if _, err := hr.ReadHistory(context.Background(), id, chat.HistoryQuery{}); err != nil {
		t.Fatalf("read history: %v", err)
	}
	if got := api.historyReqs[0].Auth; got != "Bearer xoxb-T2" {
		t.Fatalf("authorization = %q, want Bearer xoxb-T2", got)
	}
}
