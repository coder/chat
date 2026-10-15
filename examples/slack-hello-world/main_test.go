package main

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/coder/chat"
	"github.com/coder/chat/adapters/slack"
	"github.com/coder/chat/adapters/slack/slackapi"
	"github.com/coder/chat/adapters/slack/slackapi/slackapitest"
)

const (
	testAppToken = "xapp-test"
	testWait     = 5 * time.Second
)

// testOptions returns Slack options on srv with a known bot identity, so that
// adapter startup does not call auth.test.
func testOptions(srv *slackapitest.Server) slack.Options {
	return slack.Options{
		BotToken:   "xoxb-test",
		TeamID:     "T1",
		BotUserID:  "UBOT",
		BotID:      "BBOT",
		APIBaseURL: srv.URL(),
		Client:     srv.Client(),
		Logger:     slog.New(slog.DiscardHandler),
	}
}

// startServe builds the bot of opts and runs serve in a goroutine. stop
// cancels the context of serve and returns the error of serve. served gets
// the error of serve when serve returns on its own.
func startServe(t *testing.T, opts slack.Options, addr string) (bot *chat.Chat, stop func() error, served <-chan error) {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	bot, err := newBot(ctx, opts)
	if err != nil {
		t.Fatalf("new bot: %v", err)
	}
	t.Cleanup(func() {
		if err := bot.Shutdown(context.Background()); err != nil {
			t.Errorf("Shutdown: %v", err)
		}
	})

	result := make(chan error, 1)
	go func() { result <- serve(ctx, bot, opts, addr) }()
	stop = func() error {
		cancel()
		return wait(t, result)
	}
	return bot, stop, result
}

func wait(t *testing.T, served <-chan error) error {
	t.Helper()
	select {
	case err := <-served:
		return err
	case <-time.After(testWait):
		t.Fatal("serve did not return")
		return nil
	}
}

func TestServeRepliesOverSocketMode(t *testing.T) {
	t.Parallel()

	srv := slackapitest.NewServer(t)
	sm := srv.SocketMode(slackapitest.SocketModeOptions{AppToken: testAppToken, ResendAfter: -1})
	srv.Respond("chat.postMessage", map[string]any{"ok": true, "channel": "C1", "ts": "111.001"})
	opts := testOptions(srv)
	opts.AppToken = testAppToken
	bot, stop, _ := startServe(t, opts, "")

	sm.WaitLive(t, testWait, 1)
	sm.WaitAcked(t, testWait, sm.Send(string(slackapi.DeliveryEventsAPI), json.RawMessage(`{
		"type":"event_callback",
		"team_id":"T1",
		"event_id":"EvMention",
		"event":{"type":"app_mention","channel":"C1","user":"U1","text":"<@UBOT> hi","ts":"111.000"}
	}`)))

	calls := srv.Calls("chat.postMessage")
	if len(calls) != 1 {
		t.Fatalf("chat.postMessage calls = %d, want 1", len(calls))
	}
	var post slackapi.PostMessageRequest
	if err := json.Unmarshal(calls[0].Body, &post); err != nil {
		t.Fatalf("decode chat.postMessage body: %v", err)
	}
	if post.Channel != "C1" || post.ThreadTS != "111.000" || post.MarkdownText != "**hello** _world_" {
		t.Errorf("chat.postMessage = %+v, want **hello** _world_ in thread 111.000 of C1", post)
	}

	if err := stop(); err != nil {
		t.Errorf("serve: %v", err)
	}
	if c := sm.WaitClosed(t, testWait, 1); !c.CloseFrame || c.CloseCode != websocket.StatusNormalClosure {
		t.Errorf("connection = %+v, want a close frame with %v", c, websocket.StatusNormalClosure)
	}
	// serve stopped Listen with Runtime Shutdown, so Listen now returns at
	// once and opens no connection.
	ctx, cancel := context.WithTimeout(t.Context(), testWait)
	defer cancel()
	if err := bot.Listen(ctx, "slack"); err != nil {
		t.Errorf("Listen after serve: %v", err)
	}
	if calls := srv.Calls("apps.connections.open"); len(calls) != 1 {
		t.Errorf("apps.connections.open calls = %d, want 1", len(calls))
	}
}

func TestServeStopsWebhookServerWhenSocketModeFails(t *testing.T) {
	t.Parallel()

	srv := slackapitest.NewServer(t)
	srv.SocketMode(slackapitest.SocketModeOptions{AppToken: testAppToken})
	opts := testOptions(srv)
	opts.AppToken = "xapp-revoked"
	opts.SigningSecret = "secret"
	_, _, served := startServe(t, opts, "127.0.0.1:0")

	err := wait(t, served)
	if apiErr, ok := errors.AsType[*slackapi.APIError](err); !ok || apiErr.Code != "invalid_auth" {
		t.Errorf("serve: err = %v, want the invalid_auth error of Socket Mode", err)
	}
}

func TestServeWithoutAppTokenServesOnlyWebhooks(t *testing.T) {
	t.Parallel()

	srv := slackapitest.NewServer(t)
	opts := testOptions(srv)
	opts.SigningSecret = "secret"
	_, stop, _ := startServe(t, opts, "127.0.0.1:0")

	if err := stop(); err != nil {
		t.Errorf("serve: %v", err)
	}
	if calls := srv.Calls("apps.connections.open"); len(calls) != 0 {
		t.Errorf("apps.connections.open calls = %d, want 0", len(calls))
	}
}

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

	req := httptest.NewRequest(http.MethodPost, "/webhooks/slack", nil)
	rec := httptest.NewRecorder()
	server.Handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusNoContent)
	}
}

func TestMustAllowDemoMemoryStateRequiresExplicitOptIn(t *testing.T) {
	t.Setenv("CHAT_DEMO_IN_MEMORY_STATE", "")

	defer func() {
		if recovered := recover(); recovered != "CHAT_DEMO_IN_MEMORY_STATE=1 is required" {
			t.Fatalf("panic = %v, want CHAT_DEMO_IN_MEMORY_STATE=1 is required", recovered)
		}
	}()

	mustAllowDemoMemoryState()
}

func TestMustAllowDemoMemoryStateAcceptsExplicitOptIn(t *testing.T) {
	t.Setenv("CHAT_DEMO_IN_MEMORY_STATE", "1")

	mustAllowDemoMemoryState()
}
