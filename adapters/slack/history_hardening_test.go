package slack_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/coder/chat"
	"github.com/coder/chat/adapters/slack"
	"github.com/coder/chat/state/memory"
)

// newSlackHistoryRuntimeWithObserver builds a single-install Slack runtime whose
// adapter is wired to the given Observer, so history reads can be observed via the
// adapter-owned Observation Hook (ObsAdapterCall / ObsRateLimit). The HistoryReader
// is reached through Adapter Access, exactly as an application would.
func newSlackHistoryRuntimeWithObserver(t *testing.T, baseURL string, client *http.Client, obs chat.Observer) *chat.Chat {
	t.Helper()
	adapter, err := slack.New(context.Background(), slack.Options{
		SigningSecret: "secret",
		BotToken:      "xoxb-test",
		TeamID:        "T1",
		BotUserID:     "UBOT",
		BotID:         "BBOT",
		APIBaseURL:    baseURL,
		Client:        client,
		Observer:      obs,
	})
	if err != nil {
		t.Fatalf("new slack adapter: %v", err)
	}
	bot, err := chat.New(context.Background(), chat.WithState(memory.New()), chat.WithAdapter(adapter))
	if err != nil {
		t.Fatalf("new chat runtime: %v", err)
	}
	return bot
}

func slackHistoryReaderFor(t *testing.T, bot *chat.Chat) chat.HistoryReader {
	t.Helper()
	hr, ok := chat.AdapterAs[chat.HistoryReader](bot, "slack")
	if !ok {
		t.Fatal("slack adapter does not implement chat.HistoryReader")
	}
	return hr
}

// A history read inherits the adapter Observation Hook: the shared callWithToken
// seam emits ObsAdapterCall around the platform read, so observability is uniform
// with every other adapter API call (ADR 0010 + ADR 0009).
func TestSlackReadHistoryEmitsAdapterCallObservation(t *testing.T) {
	t.Parallel()

	api := newSlackAPIServer(t)
	api.historyResp = map[string]any{
		"ok":       true,
		"messages": []any{map[string]any{"type": "message", "user": "U1", "text": "hi", "ts": "111.000", "thread_ts": "111.000"}},
	}
	obs := &countingObserver{}
	bot := newSlackHistoryRuntimeWithObserver(t, api.URL, api.Client(), obs)
	hr := slackHistoryReaderFor(t, bot)

	id := slack.EncodeThreadReplyThreadIDForTest("T1", "C1", "111.000")
	if _, err := hr.ReadHistory(context.Background(), id, chat.HistoryQuery{Limit: 5}); err != nil {
		t.Fatalf("read history: %v", err)
	}
	if obs.count(chat.ObsAdapterCall) == 0 {
		t.Fatal("expected ObsAdapterCall observation around the history read")
	}
}

// A persistent 429 on a read exhausts adapter-owned bounded retry (ADR 0005) and
// surfaces a typed *slack.RateLimited, with the throttled attempt emitting
// ObsRateLimit through the shared seam, so read-side rate limiting is observable
// and never silently dropped. The read inherits the same retry path as outbound
// posts; MaxAttempts: 1 keeps this assertion deterministic and single-shot.
func TestSlackReadHistoryRateLimitObservedAndErrors(t *testing.T) {
	t.Parallel()

	var calls int
	var mu sync.Mutex
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/conversations.replies", "/conversations.history":
			mu.Lock()
			calls++
			mu.Unlock()
			w.Header().Set("Retry-After", "1")
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(`{"ok":false,"error":"ratelimited"}`))
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
			w.WriteHeader(http.StatusOK)
		}
	}))
	t.Cleanup(server.Close)

	obs := &countingObserver{}
	adapter, err := slack.New(context.Background(), slack.Options{
		SigningSecret: "secret",
		BotToken:      "xoxb-test",
		TeamID:        "T1",
		BotUserID:     "UBOT",
		BotID:         "BBOT",
		APIBaseURL:    server.URL,
		Client:        server.Client(),
		Observer:      obs,
		RetryPolicy:   slack.RetryPolicy{MaxAttempts: 1},
	})
	if err != nil {
		t.Fatalf("new slack adapter: %v", err)
	}
	bot, err := chat.New(context.Background(), chat.WithState(memory.New()), chat.WithAdapter(adapter))
	if err != nil {
		t.Fatalf("new chat runtime: %v", err)
	}
	hr := slackHistoryReaderFor(t, bot)

	id := slack.EncodeThreadReplyThreadIDForTest("T1", "C1", "111.000")
	msgs, err := hr.ReadHistory(context.Background(), id, chat.HistoryQuery{Limit: 5})
	if _, ok := errors.AsType[*slack.RateLimited](err); !ok {
		t.Fatalf("err = %v, want *slack.RateLimited on a throttled read", err)
	}
	if msgs != nil {
		t.Fatalf("messages = %#v, want nil on rate-limit error", msgs)
	}
	if obs.count(chat.ObsRateLimit) == 0 {
		t.Fatal("expected ObsRateLimit observation on 429 read")
	}
	mu.Lock()
	defer mu.Unlock()
	if calls != 1 {
		t.Fatalf("read calls = %d, want 1 (MaxAttempts: 1 is single-shot)", calls)
	}
}

// A read whose caller context carries a deadline cannot outlive that deadline: when
// the platform read blocks, the bounded context fires and ReadHistory returns
// promptly with a deadline error (ADR 0005: read backoff is bounded by the caller's
// context, never silently long-lived).
func TestSlackReadHistoryDeadlineBounded(t *testing.T) {
	t.Parallel()

	api := newSlackAPIServer(t)
	api.historyBlock = make(chan struct{}) // never closed: the server blocks until ctx fires
	bot := newSlackHistoryRuntimeWithObserver(t, api.URL, api.Client(), nil)
	hr := slackHistoryReaderFor(t, bot)

	id := slack.EncodeThreadReplyThreadIDForTest("T1", "C1", "111.000")
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	done := make(chan error, 1)
	go func() {
		_, err := hr.ReadHistory(ctx, id, chat.HistoryQuery{})
		done <- err
	}()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("expected deadline error, got nil")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("read outlived its caller deadline: did not return promptly")
	}
}

// The direct-message path (conversations.history) honors Before (latest=<ts>,
// inclusive=false) and clamped limit together, proving cursor + clamping are wired
// on both read paths, not only conversations.replies.
func TestSlackReadHistoryDirectCursorAndClamp(t *testing.T) {
	t.Parallel()

	api := newSlackAPIServer(t)
	bot := newSlackHistoryRuntimeWithObserver(t, api.URL, api.Client(), nil)
	hr := slackHistoryReaderFor(t, bot)

	id := slack.EncodeDirectThreadIDForTest("T1", "D1")
	if _, err := hr.ReadHistory(context.Background(), id, chat.HistoryQuery{Limit: 9000, Before: "222.000"}); err != nil {
		t.Fatalf("read history: %v", err)
	}
	if len(api.historyReqs) != 1 {
		t.Fatalf("history requests = %d, want 1", len(api.historyReqs))
	}
	req := api.historyReqs[0]
	if req.Method != "/conversations.history" {
		t.Fatalf("method = %q, want conversations.history", req.Method)
	}
	if req.Channel != "D1" {
		t.Fatalf("channel = %q, want D1", req.Channel)
	}
	if req.TS != "" {
		t.Fatalf("conversations.history carried ts = %q, want empty (no thread root)", req.TS)
	}
	if req.Latest != "222.000" || req.Inclusive {
		t.Fatalf("cursor mapping = latest %q inclusive %v, want 222.000/false", req.Latest, req.Inclusive)
	}
	if req.Limit != 1000 {
		t.Fatalf("limit = %d, want clamped 1000", req.Limit)
	}
}

// Golden multi-message payload: ordering is preserved exactly as Slack returns it
// (newest-first, adapter-owned), the bot's own message normalizes to BotBot with the
// bot user ID, history Messages are never Mentioned, and every message preserves its
// verbatim raw JSON via the Platform Escape Hatch.
func TestSlackReadHistoryGoldenPayloadNormalization(t *testing.T) {
	t.Parallel()

	api := newSlackAPIServer(t)
	api.historyResp = map[string]any{
		"ok": true,
		"messages": []any{
			map[string]any{"type": "message", "user": "UBOT", "text": "I am the bot", "ts": "113.000", "thread_ts": "111.000"},
			map[string]any{"type": "message", "bot_id": "BOTHER", "subtype": "bot_message", "text": "another bot", "ts": "112.500", "thread_ts": "111.000"},
			map[string]any{"type": "message", "user": "U2", "text": "<@UBOT> hey", "ts": "112.000", "thread_ts": "111.000", "edited": map[string]any{"user": "U2", "ts": "112.100"}},
			map[string]any{"type": "message", "user": "U1", "text": "root", "ts": "111.000", "thread_ts": "111.000"},
		},
	}
	bot := newSlackHistoryRuntimeWithObserver(t, api.URL, api.Client(), nil)
	hr := slackHistoryReaderFor(t, bot)

	id := slack.EncodeThreadReplyThreadIDForTest("T1", "C1", "111.000")
	msgs, err := hr.ReadHistory(context.Background(), id, chat.HistoryQuery{Limit: 50})
	if err != nil {
		t.Fatalf("read history: %v", err)
	}
	if len(msgs) != 4 {
		t.Fatalf("messages = %d, want 4", len(msgs))
	}

	// Ordering is preserved exactly as returned (newest-first here).
	wantIDs := []string{"113.000", "112.500", "112.000", "111.000"}
	for i, want := range wantIDs {
		if msgs[i].ID != want {
			t.Fatalf("msg[%d].ID = %q, want %q (ordering must be preserved)", i, msgs[i].ID, want)
		}
		// History Messages are never Mentioned: history is not a routing surface.
		if msgs[i].Mentioned {
			t.Fatalf("msg[%d].Mentioned = true; history Messages must never be Mentioned", i)
		}
	}

	// The bot's own message: BotBot, normalized to the bot user ID.
	if msgs[0].Author.BotKind != chat.BotBot || msgs[0].Author.ID != "UBOT" {
		t.Fatalf("bot-self author = %#v, want BotBot/UBOT", msgs[0].Author)
	}
	// A different bot (bot_message subtype): BotBot.
	if msgs[1].Author.BotKind != chat.BotBot {
		t.Fatalf("other-bot author = %#v, want BotBot", msgs[1].Author)
	}
	// A human who text-mentions the bot is still BotHuman and NOT Mentioned: history
	// normalization does not compute mention routing.
	if msgs[2].Author.BotKind != chat.BotHuman || msgs[2].Author.ID != "U2" {
		t.Fatalf("human author = %#v, want BotHuman/U2", msgs[2].Author)
	}

	// Every message preserves verbatim raw JSON, including fields the normalized
	// shape drops (edited).
	raw, ok := msgs[2].Raw.(json.RawMessage)
	if !ok {
		t.Fatalf("raw type = %T, want json.RawMessage", msgs[2].Raw)
	}
	var decoded struct {
		TS     string `json:"ts"`
		Edited struct {
			User string `json:"user"`
		} `json:"edited"`
	}
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("decode raw: %v", err)
	}
	if decoded.TS != "112.000" || decoded.Edited.User != "U2" {
		t.Fatalf("raw did not preserve full message shape: %s", raw)
	}
}
