package slack_test

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
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
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	return rec.Code
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
	mu    sync.Mutex
	posts []slackPost
}

type slackPost struct {
	Channel      string `json:"channel"`
	ThreadTS     string `json:"thread_ts,omitempty"`
	User         string `json:"user,omitempty"`
	Text         string `json:"text,omitempty"`
	MarkdownText string `json:"markdown_text,omitempty"`
	Mrkdwn       *bool  `json:"mrkdwn,omitempty"`
}

func newSlackAPIServer(t *testing.T) *slackAPIServer {
	t.Helper()

	api := &slackAPIServer{}
	api.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/auth.test":
			writeJSON(t, w, map[string]any{"ok": true, "team_id": "T1", "user_id": "UBOT", "bot_id": "BBOT"})
		case "/chat.postMessage":
			var post slackPost
			decodeJSON(t, r.Body, &post)
			api.mu.Lock()
			api.posts = append(api.posts, post)
			api.mu.Unlock()
			writeJSON(t, w, map[string]any{"ok": true, "channel": post.Channel, "ts": "999.000"})
		case "/chat.postEphemeral":
			var post slackPost
			decodeJSON(t, r.Body, &post)
			api.mu.Lock()
			api.posts = append(api.posts, post)
			api.mu.Unlock()
			writeJSON(t, w, map[string]any{"ok": true, "message_ts": "998.000"})
		case "/conversations.open":
			var payload struct {
				Users string `json:"users"`
			}
			decodeJSON(t, r.Body, &payload)
			if payload.Users != "U1" {
				t.Fatalf("conversations.open users = %q", payload.Users)
			}
			writeJSON(t, w, map[string]any{"ok": true, "channel": map[string]any{"id": "D-fallback"}})
		default:
			t.Fatalf("unexpected Slack API path %s", r.URL.Path)
		}
	}))
	t.Cleanup(api.Close)
	return api
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
