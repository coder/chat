package slack_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"reflect"
	"slices"
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

func TestSlackReadHistoryThreadRepliesNormalization(t *testing.T) {
	t.Parallel()

	api := newSlackAPIServer(t)
	api.historyMessages = []map[string]any{
		{"type": "message", "user": "U1", "text": "hello", "ts": "111.000", "thread_ts": "111.000"},
		{"type": "message", "bot_id": "BBOT", "subtype": "bot_message", "text": "hi back", "ts": "112.000", "thread_ts": "111.000"},
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
	if len(msgs) != 2 {
		t.Fatalf("messages = %d, want 2", len(msgs))
	}
	if msgs[0].ID != "112.000" || msgs[0].Text != "hi back" {
		t.Fatalf("msg0 = %#v", msgs[0])
	}
	if msgs[0].Author.BotKind != chat.BotBot {
		t.Fatalf("msg0 author = %#v, want bot", msgs[0].Author)
	}
	if msgs[1].ID != "111.000" || msgs[1].Text != "hello" {
		t.Fatalf("msg1 = %#v", msgs[1])
	}
	if msgs[1].Author.ID != "U1" || msgs[1].Author.BotKind != chat.BotHuman {
		t.Fatalf("msg1 author = %#v, want human U1", msgs[1].Author)
	}
	if msgs[1].Author.Adapter != "slack" || msgs[1].Author.Tenant != "T1" {
		t.Fatalf("msg1 author scope = %#v", msgs[1].Author)
	}
}

// Each returned Message preserves the verbatim per-message JSON via the Platform
// Escape Hatch (Message.Raw).
func TestSlackReadHistoryPreservesRaw(t *testing.T) {
	t.Parallel()

	api := newSlackAPIServer(t)
	api.historyMessages = []map[string]any{
		{"type": "message", "user": "U1", "text": "hello", "ts": "111.000", "reactions": []any{map[string]any{"name": "wave"}}},
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

func TestSlackReadHistoryRequest(t *testing.T) {
	t.Parallel()

	opts := slack.Options{
		SigningSecret: "secret",
		BotToken:      "xoxb-test",
		TeamID:        "T1",
		BotUserID:     "UBOT",
		BotID:         "BBOT",
	}
	thread := slack.EncodeThreadReplyThreadIDForTest("T1", "C1", "111.000")
	cases := []struct {
		name       string
		id         chat.ThreadID
		query      chat.HistoryQuery
		wantMethod string
		wantForm   url.Values
	}{
		{
			name:       "replies",
			id:         thread,
			query:      chat.HistoryQuery{Limit: 20},
			wantMethod: "conversations.replies",
			wantForm:   url.Values{"channel": {"C1"}, "ts": {"111.000"}, "limit": {"20"}},
		},
		{
			name:       "default limit",
			id:         thread,
			query:      chat.HistoryQuery{},
			wantMethod: "conversations.replies",
			wantForm:   url.Values{"channel": {"C1"}, "ts": {"111.000"}, "limit": {"100"}},
		},
		{
			name:       "negative limit",
			id:         thread,
			query:      chat.HistoryQuery{Limit: -3},
			wantMethod: "conversations.replies",
			wantForm:   url.Values{"channel": {"C1"}, "ts": {"111.000"}, "limit": {"100"}},
		},
		{
			name:       "clamped limit",
			id:         thread,
			query:      chat.HistoryQuery{Limit: 5000},
			wantMethod: "conversations.replies",
			wantForm:   url.Values{"channel": {"C1"}, "ts": {"111.000"}, "limit": {"1000"}},
		},
		{
			name:       "before",
			id:         thread,
			query:      chat.HistoryQuery{Before: "115.000"},
			wantMethod: "conversations.replies",
			wantForm:   url.Values{"channel": {"C1"}, "ts": {"111.000"}, "latest": {"115.000"}, "limit": {"100"}},
		},
		{
			name:       "direct",
			id:         slack.EncodeDirectThreadIDForTest("T1", "D1"),
			query:      chat.HistoryQuery{},
			wantMethod: "conversations.history",
			wantForm:   url.Values{"channel": {"D1"}, "limit": {"100"}},
		},
		{
			name:       "direct, before, clamped limit",
			id:         slack.EncodeDirectThreadIDForTest("T1", "D1"),
			query:      chat.HistoryQuery{Limit: 9000, Before: "222.000"},
			wantMethod: "conversations.history",
			wantForm:   url.Values{"channel": {"D1"}, "latest": {"222.000"}, "limit": {"1000"}},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			api := newSlackAPIServer(t)
			hr := historyReader(t, newSlackRuntime(t, api, opts))
			if _, err := hr.ReadHistory(context.Background(), tc.id, tc.query); err != nil {
				t.Fatalf("read history: %v", err)
			}
			if len(api.historyReqs) != 1 {
				t.Fatalf("history requests = %d, want 1", len(api.historyReqs))
			}
			req := api.historyReqs[0]
			if req.Method != tc.wantMethod || !reflect.DeepEqual(req.Form, tc.wantForm) {
				t.Fatalf("request = %s %v, want %s %v", req.Method, req.Form, tc.wantMethod, tc.wantForm)
			}
		})
	}
}

func TestSlackReadHistoryPagesNewestFirst(t *testing.T) {
	t.Parallel()

	opts := slack.Options{
		SigningSecret: "secret",
		BotToken:      "xoxb-test",
		TeamID:        "T1",
		BotUserID:     "UBOT",
		BotID:         "BBOT",
	}
	thread := slack.EncodeThreadReplyThreadIDForTest("T1", "C1", "100.000")
	conversation := func(first, last int) []map[string]any {
		var msgs []map[string]any
		for i := first; i <= last; i++ {
			msgs = append(msgs, map[string]any{"type": "message", "user": "U1", "text": "m", "ts": fmt.Sprintf("%d.000", i)})
		}
		return msgs
	}
	newestFirst := func(newest, oldest int) []string {
		var ids []string
		for i := newest; i >= oldest; i-- {
			ids = append(ids, fmt.Sprintf("%d.000", i))
		}
		return ids
	}
	cases := []struct {
		name       string
		id         chat.ThreadID
		messages   []map[string]any
		wantIDs    []string
		wantLatest []string
	}{
		{
			name:       "replies, short last page",
			id:         thread,
			messages:   conversation(100, 107),
			wantIDs:    newestFirst(107, 100),
			wantLatest: []string{"", "105.000", "102.000"},
		},
		{
			name:       "replies, full last page",
			id:         thread,
			messages:   conversation(100, 106),
			wantIDs:    newestFirst(106, 100),
			wantLatest: []string{"", "104.000"},
		},
		{
			name:       "direct",
			id:         slack.EncodeDirectThreadIDForTest("T1", "D1"),
			messages:   conversation(101, 107),
			wantIDs:    newestFirst(107, 101),
			wantLatest: []string{"", "105.000", "102.000", "101.000"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			api := newSlackAPIServer(t)
			api.historyMessages = tc.messages
			hr := historyReader(t, newSlackRuntime(t, api, opts))

			var ids []string
			var before string
			for pages := 0; ; pages++ {
				if pages == 10 {
					t.Fatalf("no empty page after %d pages, IDs so far %v", pages, ids)
				}
				msgs, err := hr.ReadHistory(context.Background(), tc.id, chat.HistoryQuery{Limit: 3, Before: before})
				if err != nil {
					t.Fatalf("read history page %d: %v", pages, err)
				}
				if len(msgs) == 0 {
					break
				}
				for _, msg := range msgs {
					ids = append(ids, msg.ID)
				}
				before = msgs[len(msgs)-1].ID
			}
			if !slices.Equal(ids, tc.wantIDs) {
				t.Fatalf("message IDs = %v, want %v", ids, tc.wantIDs)
			}
			var latest []string
			for _, req := range api.historyReqs {
				latest = append(latest, req.Form.Get("latest"))
			}
			if !slices.Equal(latest, tc.wantLatest) {
				t.Fatalf("latest per request = %q, want %q", latest, tc.wantLatest)
			}
		})
	}
}

// A cancelled context aborts the platform read promptly (the read never outlives the
// caller's deadline).
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
	api.historyError = "channel_not_found"
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
