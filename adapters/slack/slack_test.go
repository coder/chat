package slack_test

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/coder/chat"
	"github.com/coder/chat/adapters/slack"
	"github.com/coder/chat/state/memory"
)

func TestWebhookVerifiesSlackSignatureAndHandlesURLVerification(t *testing.T) {
	t.Parallel()

	api := newSlackAPIServer(t)
	now := time.Unix(1_700_000_000, 0)
	bot := newSlackRuntime(t, api, slack.Options{
		SigningSecret: "secret",
		BotToken:      "xoxb-test",
		Now:           func() time.Time { return now },
	})

	handler, err := bot.Webhook("slack")
	if err != nil {
		t.Fatalf("webhook: %v", err)
	}

	body := []byte(`{"type":"url_verification","challenge":"challenge-value"}`)
	req := httptest.NewRequest(http.MethodPost, "/slack", bytes.NewReader(body))
	signSlackRequest(req, "secret", now, body)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if rec.Body.String() != "challenge-value" {
		t.Fatalf("challenge response = %q", rec.Body.String())
	}

	badReq := httptest.NewRequest(http.MethodPost, "/slack", bytes.NewReader(body))
	badReq.Header.Set("X-Slack-Request-Timestamp", "1700000000")
	badReq.Header.Set("X-Slack-Signature", "v0=bad")
	badRec := httptest.NewRecorder()
	handler.ServeHTTP(badRec, badReq)
	if badRec.Code != http.StatusUnauthorized {
		t.Fatalf("invalid signature status = %d", badRec.Code)
	}

	oldReq := httptest.NewRequest(http.MethodPost, "/slack", bytes.NewReader(body))
	signSlackRequest(oldReq, "secret", now.Add(-10*time.Minute), body)
	oldRec := httptest.NewRecorder()
	handler.ServeHTTP(oldRec, oldReq)
	if oldRec.Code != http.StatusUnauthorized {
		t.Fatalf("expired signature status = %d", oldRec.Code)
	}
}

func TestWebhookRejectsOversizedPayloadBeforeSignature(t *testing.T) {
	t.Parallel()

	now := time.Unix(1_700_000_000, 0)
	adapter, err := slack.New(context.Background(), slack.Options{
		SigningSecret: "secret",
		BotToken:      "xoxb-test",
		Now:           func() time.Time { return now },
	})
	if err != nil {
		t.Fatalf("new slack adapter: %v", err)
	}

	handler := adapter.Webhook(func(context.Context, *chat.Event) error {
		t.Fatal("oversized payload reached dispatch")
		return nil
	})

	body := make([]byte, 2<<20)
	req := httptest.NewRequest(http.MethodPost, "/slack", bytes.NewReader(body))
	req.Header.Set("X-Slack-Request-Timestamp", formatUnix(now))
	req.Header.Set("X-Slack-Signature", "v0=bad")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized payload status = %d, body = %s", rec.Code, rec.Body.String())
	}
}

func TestWebhookRejectsMalformedSupportedSlackEvents(t *testing.T) {
	t.Parallel()

	api := newSlackAPIServer(t)
	now := time.Unix(1_700_000_000, 0)
	bot := newSlackRuntime(t, api, slack.Options{
		SigningSecret: "secret",
		BotToken:      "xoxb-test",
		Now:           func() time.Time { return now },
	})

	handler, err := bot.Webhook("slack")
	if err != nil {
		t.Fatalf("webhook: %v", err)
	}
	body := []byte(`{
		"type":"event_callback",
		"team_id":"T1",
		"event_id":"Bad1",
		"event":{
			"type":"app_mention",
			"channel":"C1",
			"user":"U1",
			"text":"missing timestamp"
		}
	}`)
	req := httptest.NewRequest(http.MethodPost, "/slack", bytes.NewReader(body))
	signSlackRequest(req, "secret", now, body)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("malformed supported event status = %d, body = %s", rec.Code, rec.Body.String())
	}
}

func TestWebhookRejectsSlackEventsForMismatchedTeam(t *testing.T) {
	t.Parallel()

	now := time.Unix(1_700_000_000, 0)
	adapter, err := slack.New(context.Background(), slack.Options{
		SigningSecret: "secret",
		BotToken:      "xoxb-test",
		TeamID:        "T1",
		BotUserID:     "UBOT",
		Now:           func() time.Time { return now },
	})
	if err != nil {
		t.Fatalf("new slack adapter: %v", err)
	}

	handler := adapter.Webhook(func(context.Context, *chat.Event) error {
		t.Fatal("mismatched team event reached dispatch")
		return nil
	})

	rec := serveSignedSlackWebhook(t, handler, now, `{
		"type":"event_callback",
		"team_id":"T2",
		"event_id":"WrongTeam1",
		"event":{
			"type":"app_mention",
			"channel":"C1",
			"user":"U1",
			"text":"<@UBOT> hi",
			"ts":"111.000"
		}
	}`, "", "")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("mismatched team status = %d, body = %s", rec.Code, rec.Body.String())
	}
}

func TestWebhookNormalizesMentionsDirectMessagesRetriesAndSelfMessages(t *testing.T) {
	t.Parallel()

	api := newSlackAPIServer(t)
	now := time.Unix(1_700_000_000, 0)
	bot := newSlackRuntime(t, api, slack.Options{
		SigningSecret: "secret",
		BotToken:      "xoxb-test",
		Now:           func() time.Time { return now },
	})

	var seen []string
	var firstThread chat.ThreadID
	bot.OnNewMention(func(ctx context.Context, ev *chat.MessageEvent) error {
		seen = append(seen, "new:"+ev.Message.ID+":"+ev.Event.Retry.Num)
		firstThread = ev.Thread.ID()
		if ev.Message.Author != (chat.Actor{Adapter: "slack", Tenant: "T1", ID: "U1", BotKind: chat.BotHuman}) {
			t.Fatalf("author = %#v", ev.Message.Author)
		}
		if ev.Thread.ID() == "" {
			t.Fatal("thread id should be present")
		}
		return ev.Thread.Subscribe(ctx)
	})
	bot.OnSubscribedMessage(func(ctx context.Context, ev *chat.MessageEvent) error {
		seen = append(seen, "subscribed:"+ev.Message.ID)
		return nil
	})

	postSlackEvent(t, bot, now, `{
		"type":"event_callback",
		"team_id":"T1",
		"event_id":"Ev1",
		"event":{
			"type":"app_mention",
			"channel":"C1",
			"user":"U1",
			"text":"<@UBOT> hi",
			"ts":"111.000"
		}
	}`, "1", "http_timeout")

	postSlackEvent(t, bot, now, `{
		"type":"event_callback",
		"team_id":"T1",
		"event_id":"Ev2",
		"event":{
			"type":"message",
			"channel_type":"im",
			"channel":"D1",
			"user":"U1",
			"text":"hello in dm",
			"ts":"222.000"
		}
	}`, "", "")

	postSlackEvent(t, bot, now, `{
		"type":"event_callback",
		"team_id":"T1",
		"event_id":"Ev3",
		"event":{
			"type":"message",
			"channel_type":"im",
			"channel":"D1",
			"user":"U1",
			"text":"second dm",
			"ts":"223.000"
		}
	}`, "", "")

	postSlackEvent(t, bot, now, `{
		"type":"event_callback",
		"team_id":"T1",
		"event_id":"Ev4",
		"event":{
			"type":"message",
			"channel":"C1",
			"user":"UBOT",
			"text":"bot echo",
			"ts":"224.000"
		}
	}`, "", "")

	want := []string{"new:111.000:1", "new:222.000:", "subscribed:223.000"}
	if !equalStrings(seen, want) {
		t.Fatalf("seen = %#v, want %#v", seen, want)
	}

	thread, err := bot.Thread(context.Background(), firstThread)
	if err != nil {
		t.Fatalf("thread handle from normalized id: %v", err)
	}
	if thread.ID() != firstThread {
		t.Fatalf("thread id = %q, want %q", thread.ID(), firstThread)
	}

	if status := postSlackEvent(t, bot, now, `{
		"type":"event_callback",
		"team_id":"T1",
		"event_id":"Ev5",
		"event":{"type":"reaction_added","user":"U1"}
	}`, "", ""); status != http.StatusOK {
		t.Fatalf("unsupported event status = %d", status)
	}
}

func TestConfiguredSlackIdentityDiscoversBotIDForSelfMessages(t *testing.T) {
	t.Parallel()

	api := newSlackAPIServer(t)
	now := time.Unix(1_700_000_000, 0)
	bot := newSlackRuntime(t, api, slack.Options{
		SigningSecret: "secret",
		BotToken:      "xoxb-test",
		TeamID:        "T1",
		BotUserID:     "UBOT",
		Now:           func() time.Time { return now },
	})
	if authCalls := api.authCallCount(); authCalls != 1 {
		t.Fatalf("auth.test calls = %d, want 1", authCalls)
	}

	handled := 0
	bot.OnNewMention(func(context.Context, *chat.MessageEvent) error {
		handled++
		return nil
	})

	postSlackEvent(t, bot, now, `{
		"type":"event_callback",
		"team_id":"T1",
		"event_id":"SelfBotMessage1",
		"event":{
			"type":"message",
			"subtype":"bot_message",
			"channel":"C1",
			"bot_id":"BBOT",
			"text":"<@UBOT> bot echo",
			"ts":"225.000"
		}
	}`, "", "")

	if handled != 0 {
		t.Fatalf("self bot_message reached handler %d time(s)", handled)
	}
}

func TestSlackInitRejectsConflictingAuthIdentity(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		options      slack.Options
		authResponse map[string]any
		wantErr      string
	}{
		{
			name: "team id",
			options: slack.Options{
				TeamID:    "T1",
				BotUserID: "UBOT",
			},
			authResponse: map[string]any{"ok": true, "team_id": "T2", "user_id": "UBOT", "bot_id": "BBOT"},
			wantErr:      `slack: auth.test returned team_id "T2", expected "T1"`,
		},
		{
			name: "bot user id",
			options: slack.Options{
				TeamID:    "T1",
				BotUserID: "UBOT",
			},
			authResponse: map[string]any{"ok": true, "team_id": "T1", "user_id": "UOTHER", "bot_id": "BBOT"},
			wantErr:      `slack: auth.test returned user_id "UOTHER", expected "UBOT"`,
		},
		{
			name: "bot id",
			options: slack.Options{
				TeamID: "T1",
				BotID:  "BBOT",
			},
			authResponse: map[string]any{"ok": true, "team_id": "T1", "user_id": "UBOT", "bot_id": "BOTHER"},
			wantErr:      `slack: auth.test returned bot_id "BOTHER", expected "BBOT"`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			api := newSlackAPIServer(t)
			api.setAuthResponse(tt.authResponse)
			tt.options.SigningSecret = "secret"
			tt.options.BotToken = "xoxb-test"
			tt.options.APIBaseURL = api.URL
			tt.options.Client = api.Client()

			adapter, err := slack.New(context.Background(), tt.options)
			if err != nil {
				t.Fatalf("new slack adapter: %v", err)
			}
			err = adapter.Init(context.Background())
			if err == nil || err.Error() != tt.wantErr {
				t.Fatalf("init error = %v, want %q", err, tt.wantErr)
			}
		})
	}
}

func TestSlackInitDoesNotPartiallyAssignFailedDiscovery(t *testing.T) {
	t.Parallel()

	api := newSlackAPIServer(t)
	api.setAuthResponse(map[string]any{"ok": true, "team_id": "T1", "user_id": "UBOT"})
	adapter, err := slack.New(context.Background(), slack.Options{
		SigningSecret: "secret",
		BotToken:      "xoxb-test",
		APIBaseURL:    api.URL,
		Client:        api.Client(),
	})
	if err != nil {
		t.Fatalf("new slack adapter: %v", err)
	}
	err = adapter.Init(context.Background())
	if err == nil || err.Error() != "slack: auth.test did not return bot_id" {
		t.Fatalf("init error = %v, want missing bot_id", err)
	}
	if bot := adapter.BotActor(); bot.Tenant != "" || bot.ID != "" {
		t.Fatalf("bot actor after failed init = %#v, want empty identity", bot)
	}

	api.setAuthResponse(map[string]any{"ok": true, "team_id": "T2", "user_id": "U2", "bot_id": "B2"})
	if err := adapter.Init(context.Background()); err != nil {
		t.Fatalf("retry init: %v", err)
	}
	if bot := adapter.BotActor(); bot.Tenant != "T2" || bot.ID != "U2" {
		t.Fatalf("bot actor after retry = %#v, want T2/U2", bot)
	}
}

func TestPostingTextMarkdownEphemeralAndExplicitFallback(t *testing.T) {
	t.Parallel()

	api := newSlackAPIServer(t)
	now := time.Unix(1_700_000_000, 0)
	bot := newSlackRuntime(t, api, slack.Options{
		SigningSecret:          "secret",
		BotToken:               "xoxb-test",
		Now:                    func() time.Time { return now },
		DisableNativeEphemeral: true,
	})

	var threadID chat.ThreadID
	bot.OnNewMention(func(ctx context.Context, ev *chat.MessageEvent) error {
		threadID = ev.Thread.ID()
		if _, err := ev.Thread.Post(ctx, chat.Text("plain reply")); err != nil {
			return err
		}
		return nil
	})

	postSlackEvent(t, bot, now, `{
		"type":"event_callback",
		"team_id":"T1",
		"event_id":"Ev1",
		"event":{
			"type":"app_mention",
			"channel":"C1",
			"user":"U1",
			"text":"<@UBOT> hi",
			"ts":"111.000"
		}
	}`, "", "")

	thread, err := bot.Thread(context.Background(), threadID)
	if err != nil {
		t.Fatalf("thread handle: %v", err)
	}
	if _, err := thread.Post(context.Background(), chat.Markdown("**portable**")); err != nil {
		t.Fatalf("markdown post: %v", err)
	}
	sent, err := thread.PostEphemeral(context.Background(),
		chat.Actor{Adapter: "slack", Tenant: "T1", ID: "U1", BotKind: chat.BotHuman},
		chat.Text("private"),
		chat.EphemeralOptions{},
	)
	if err != nil {
		t.Fatalf("ephemeral without fallback should not error when native is disabled: %v", err)
	}
	if sent != nil {
		t.Fatalf("ephemeral without fallback sent = %#v, want nil", sent)
	}
	sent, err = thread.PostEphemeral(context.Background(),
		chat.Actor{Adapter: "slack", Tenant: "T1", ID: "U1", BotKind: chat.BotHuman},
		chat.Markdown("**private fallback**"),
		chat.EphemeralOptions{FallbackToDM: true},
	)
	if err != nil {
		t.Fatalf("ephemeral fallback: %v", err)
	}
	if sent == nil || sent.ID == "" {
		t.Fatalf("fallback sent = %#v", sent)
	}

	api.assertPost(t, 0, slackPost{Channel: "C1", ThreadTS: "111.000", Text: "plain reply", Mrkdwn: boolPtr(false)})
	api.assertPost(t, 1, slackPost{Channel: "C1", ThreadTS: "111.000", MarkdownText: "**portable**"})
	api.assertPost(t, 2, slackPost{Channel: "D-fallback", MarkdownText: "**private fallback**"})
}

func TestNativeEphemeralPosting(t *testing.T) {
	t.Parallel()

	api := newSlackAPIServer(t)
	now := time.Unix(1_700_000_000, 0)
	bot := newSlackRuntime(t, api, slack.Options{
		SigningSecret: "secret",
		BotToken:      "xoxb-test",
		Now:           func() time.Time { return now },
	})

	var threadID chat.ThreadID
	bot.OnNewMention(func(ctx context.Context, ev *chat.MessageEvent) error {
		threadID = ev.Thread.ID()
		return nil
	})

	postSlackEvent(t, bot, now, `{
		"type":"event_callback",
		"team_id":"T1",
		"event_id":"Ev1",
		"event":{
			"type":"app_mention",
			"channel":"C1",
			"user":"U1",
			"text":"<@UBOT> hi",
			"ts":"111.000"
		}
	}`, "", "")

	thread, err := bot.Thread(context.Background(), threadID)
	if err != nil {
		t.Fatalf("thread handle: %v", err)
	}
	sent, err := thread.PostEphemeral(context.Background(),
		chat.Actor{Adapter: "slack", Tenant: "T1", ID: "U1", BotKind: chat.BotHuman},
		chat.Markdown("**private**"),
		chat.EphemeralOptions{},
	)
	if err != nil {
		t.Fatalf("native ephemeral: %v", err)
	}
	if sent == nil || sent.ID != "998.000" {
		t.Fatalf("native ephemeral sent = %#v", sent)
	}
	api.assertPost(t, 0, slackPost{Channel: "C1", ThreadTS: "111.000", User: "U1", MarkdownText: "**private**"})
}

func newSlackRuntime(t *testing.T, api *slackAPIServer, opts slack.Options) *chat.Chat {
	t.Helper()

	opts.APIBaseURL = api.URL
	opts.Client = api.Client()
	adapter, err := slack.New(context.Background(), opts)
	if err != nil {
		t.Fatalf("new slack adapter: %v", err)
	}
	bot, err := chat.New(context.Background(),
		chat.WithState(memory.New()),
		chat.WithAdapter(adapter),
	)
	if err != nil {
		t.Fatalf("new chat runtime: %v", err)
	}
	return bot
}

func postSlackEvent(t *testing.T, bot *chat.Chat, now time.Time, body string, retryNum string, retryReason string) int {
	t.Helper()

	handler, err := bot.Webhook("slack")
	if err != nil {
		t.Fatalf("webhook: %v", err)
	}
	rec := serveSignedSlackWebhook(t, handler, now, body, retryNum, retryReason)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	return rec.Code
}

func serveSignedSlackWebhook(t *testing.T, handler http.Handler, now time.Time, body string, retryNum string, retryReason string) *httptest.ResponseRecorder {
	t.Helper()

	bodyBytes := []byte(body)
	req := httptest.NewRequest(http.MethodPost, "/slack", bytes.NewReader(bodyBytes))
	signSlackRequest(req, "secret", now, bodyBytes)
	if retryNum != "" {
		req.Header.Set("X-Slack-Retry-Num", retryNum)
	}
	if retryReason != "" {
		req.Header.Set("X-Slack-Retry-Reason", retryReason)
	}
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	return rec
}

func signSlackRequest(req *http.Request, secret string, now time.Time, body []byte) {
	timestamp := []byte("1700000000")
	if !now.IsZero() {
		timestamp = []byte(formatUnix(now))
	}
	base := append([]byte("v0:"), timestamp...)
	base = append(base, ':')
	base = append(base, body...)
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(base)

	req.Header.Set("X-Slack-Request-Timestamp", string(timestamp))
	req.Header.Set("X-Slack-Signature", "v0="+hex.EncodeToString(mac.Sum(nil)))
}

func formatUnix(t time.Time) string {
	return strconv.FormatInt(t.Unix(), 10)
}

type slackAPIServer struct {
	*httptest.Server
	mu             sync.Mutex
	posts          []slackPost
	postAuth       []string
	authResponse   map[string]any
	authCalls      int
	openViewTrigID string
	historyReqs    []historyRequest
	historyResp    map[string]any
	historyBlock   chan struct{}
}

// historyRequest records a decoded conversations.history / conversations.replies
// request so history tests can assert Thread ID to read mapping, limit clamping,
// and cursor handling.
type historyRequest struct {
	Method    string
	Channel   string `json:"channel"`
	TS        string `json:"ts"`
	Limit     int    `json:"limit"`
	Latest    string `json:"latest"`
	Inclusive bool   `json:"inclusive"`
	Auth      string
}

type slackPost struct {
	Channel      string `json:"channel"`
	ThreadTS     string `json:"thread_ts,omitempty"`
	User         string `json:"user,omitempty"`
	Text         string `json:"text,omitempty"`
	MarkdownText string `json:"markdown_text,omitempty"`
	Mrkdwn       *bool  `json:"mrkdwn,omitempty"`
	Blocks       any    `json:"blocks,omitempty"`
}

func newSlackAPIServer(t *testing.T) *slackAPIServer {
	t.Helper()

	api := &slackAPIServer{
		authResponse: map[string]any{"ok": true, "team_id": "T1", "user_id": "UBOT", "bot_id": "BBOT"},
	}
	api.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/auth.test":
			api.mu.Lock()
			api.authCalls++
			response := maps.Clone(api.authResponse)
			api.mu.Unlock()
			writeJSON(t, w, response)
		case "/chat.postMessage":
			var post slackPost
			decodeJSON(t, r.Body, &post)
			api.mu.Lock()
			api.posts = append(api.posts, post)
			api.postAuth = append(api.postAuth, r.Header.Get("Authorization"))
			api.mu.Unlock()
			writeJSON(t, w, map[string]any{"ok": true, "channel": post.Channel, "ts": "999.000"})
		case "/chat.postEphemeral":
			var post slackPost
			decodeJSON(t, r.Body, &post)
			api.mu.Lock()
			api.posts = append(api.posts, post)
			api.postAuth = append(api.postAuth, r.Header.Get("Authorization"))
			api.mu.Unlock()
			writeJSON(t, w, map[string]any{"ok": true, "message_ts": "998.000"})
		case "/views.open":
			var payload struct {
				TriggerID string `json:"trigger_id"`
			}
			decodeJSON(t, r.Body, &payload)
			api.mu.Lock()
			api.openViewTrigID = payload.TriggerID
			api.mu.Unlock()
			writeJSON(t, w, map[string]any{"ok": true})
		case "/conversations.open":
			var payload struct {
				Users string `json:"users"`
			}
			decodeJSON(t, r.Body, &payload)
			if payload.Users != "U1" {
				t.Fatalf("conversations.open users = %q", payload.Users)
			}
			writeJSON(t, w, map[string]any{"ok": true, "channel": map[string]any{"id": "D-fallback"}})
		case "/conversations.replies", "/conversations.history":
			var req historyRequest
			decodeJSON(t, r.Body, &req)
			req.Method = r.URL.Path
			req.Auth = r.Header.Get("Authorization")
			api.mu.Lock()
			api.historyReqs = append(api.historyReqs, req)
			block := api.historyBlock
			response := maps.Clone(api.historyResp)
			api.mu.Unlock()
			if block != nil {
				select {
				case <-block:
				case <-r.Context().Done():
					return
				}
			}
			if response == nil {
				response = map[string]any{"ok": true, "messages": []any{}}
			}
			writeJSON(t, w, response)
		default:
			t.Fatalf("unexpected Slack API path %s", r.URL.Path)
		}
	}))
	t.Cleanup(api.Close)
	return api
}

func (s *slackAPIServer) setAuthResponse(response map[string]any) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.authResponse = response
}

func (s *slackAPIServer) authCallCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.authCalls
}

func (s *slackAPIServer) assertPost(t *testing.T, index int, want slackPost) {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.posts) <= index {
		t.Fatalf("missing post %d in %#v", index, s.posts)
	}
	got := s.posts[index]
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("post %d = %#v, want %#v", index, got, want)
	}
}

func (s *slackAPIServer) authForPost(t *testing.T, index int) string {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.postAuth) <= index {
		t.Fatalf("missing post auth %d in %#v", index, s.postAuth)
	}
	return s.postAuth[index]
}

func boolPtr(value bool) *bool {
	return &value
}

func decodeJSON(t *testing.T, body io.Reader, dest any) {
	t.Helper()
	if err := json.NewDecoder(body).Decode(dest); err != nil {
		t.Fatalf("decode json: %v", err)
	}
}

func writeJSON(t *testing.T, w http.ResponseWriter, value any) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(value); err != nil {
		t.Fatalf("write json: %v", err)
	}
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
