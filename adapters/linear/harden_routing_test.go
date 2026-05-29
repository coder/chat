package linear_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/coder/chat"
	"github.com/coder/chat/adapters/linear"
)

// postHardenRaw posts a signed webhook and returns the recorder without
// asserting status, so ack-semantics tests can inspect non-200 responses too.
func postHardenRaw(t *testing.T, bot *chat.Chat, secret, body string) *httptest.ResponseRecorder {
	t.Helper()
	handler, err := bot.Webhook("linear")
	if err != nil {
		t.Fatalf("webhook: %v", err)
	}
	bodyBytes := []byte(body)
	req := httptest.NewRequest(http.MethodPost, "/linear", bytes.NewReader(bodyBytes))
	signLinearRequest(req, secret, bodyBytes)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	return rec
}

// TestRoutingKindPrecedence verifies that the inbound webhook type drives which
// normalization path runs and that each path mints the correct thread kind: an
// AgentSessionEvent becomes an agent-session thread, a Comment becomes a comment
// thread, even when they share the same issue.
func TestRoutingKindPrecedence(t *testing.T) {
	t.Parallel()

	api := newLinearAPIServer(t, 3600)
	now := time.UnixMilli(1_700_000_000_000)
	bot, _ := newLinearRuntime(t, api, linear.Options{WebhookSecret: "whsec", Now: func() time.Time { return now }})

	kinds := map[string]string{}
	bot.OnNewMention(func(_ context.Context, ev *chat.MessageEvent) error {
		raw, ok := linear.RawMessageFrom(ev.Message)
		if !ok {
			t.Fatal("missing raw message")
		}
		kinds[ev.Message.ID] = raw.Kind
		return nil
	})

	postLinearEvent(t, bot, "whsec", createdPayload(now, "C1", "hello", "U1", "User One", "APP1"))
	postLinearEvent(t, bot, "whsec", commentPayload(now, "CM1", "@APP1 over here", "U2", "User Two", "ISSUE1", ""))

	if kinds["C1"] != "agent_session" {
		t.Fatalf("agent-session event kind = %q", kinds["C1"])
	}
	if kinds["CM1"] != "comment" {
		t.Fatalf("comment event kind = %q", kinds["CM1"])
	}
}

// TestPromptedFromBotIsSelfFiltered verifies a prompted agent-session event whose
// activity author is the app actor is dropped by the runtime Self Message guard.
func TestPromptedFromBotIsSelfFiltered(t *testing.T) {
	t.Parallel()

	api := newLinearAPIServer(t, 3600)
	now := time.UnixMilli(1_700_000_000_000)
	bot, _ := newLinearRuntime(t, api, linear.Options{WebhookSecret: "whsec", Now: func() time.Time { return now }})

	var subscribed int
	bot.OnNewMention(func(ctx context.Context, ev *chat.MessageEvent) error {
		return ev.Thread.Subscribe(ctx)
	})
	bot.OnSubscribedMessage(func(context.Context, *chat.MessageEvent) error { subscribed++; return nil })

	postLinearEvent(t, bot, "whsec", createdPayload(now, "C1", "hello", "U1", "User One", "APP1"))
	postLinearEvent(t, bot, "whsec", promptedByActorPayload(now, "C2", "echo", "APP1", "Linear Bot", "bot"))
	if subscribed != 0 {
		t.Fatalf("self-authored prompted routed: %d", subscribed)
	}
	postLinearEvent(t, bot, "whsec", promptedByActorPayload(now, "C3", "real", "U1", "User One", "user"))
	if subscribed != 1 {
		t.Fatalf("human prompted routed = %d, want 1", subscribed)
	}
}

// TestWebhookAcksIgnoredAndUnsupportedShapes verifies the ack contract: ignored
// or unsupported webhook shapes are acknowledged with 200 OK and never reach a
// handler.
func TestWebhookAcksIgnoredAndUnsupportedShapes(t *testing.T) {
	t.Parallel()

	api := newLinearAPIServer(t, 3600)
	now := time.UnixMilli(1_700_000_000_000)
	bot, _ := newLinearRuntime(t, api, linear.Options{WebhookSecret: "whsec", Now: func() time.Time { return now }})

	var handled int
	bot.OnNewMention(func(context.Context, *chat.MessageEvent) error { handled++; return nil })
	bot.OnSubscribedMessage(func(context.Context, *chat.MessageEvent) error { handled++; return nil })

	ts := now.UnixMilli()
	cases := []struct {
		name string
		body string
	}{
		{"unknown type", fmt.Sprintf(`{"type":"Reaction","action":"create","organizationId":"ORG1","webhookTimestamp":%d}`, ts)},
		{"agent session unknown action", fmt.Sprintf(`{"type":"AgentSessionEvent","action":"deleted","organizationId":"ORG1","webhookTimestamp":%d,"agentSession":{"id":"S1","issueId":"ISSUE1","appUserId":"APP1"}}`, ts)},
		{"prompted without activity", fmt.Sprintf(`{"type":"AgentSessionEvent","action":"prompted","organizationId":"ORG1","webhookTimestamp":%d,"agentSession":{"id":"S1","issueId":"ISSUE1","appUserId":"APP1"}}`, ts)},
		{"comment without data", fmt.Sprintf(`{"type":"Comment","action":"create","organizationId":"ORG1","webhookTimestamp":%d}`, ts)},
		{"comment without issue", fmt.Sprintf(`{"type":"Comment","action":"create","organizationId":"ORG1","webhookTimestamp":%d,"data":{"id":"CM1","body":"@APP1"}}`, ts)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := postHardenRaw(t, bot, "whsec", tc.body)
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200 (ignored ack), body = %s", rec.Code, rec.Body.String())
			}
		})
	}
	if handled != 0 {
		t.Fatalf("ignored shapes reached a handler: %d", handled)
	}
}

// TestWebhookAcksHandlerError pins the ack contract under DispatchSync: an
// application handler error is logged by the runtime but the webhook is still
// acknowledged 2xx (a handler-level failure is an app concern, not a delivery
// failure Linear should retry). Infra/auth failures returning non-2xx are covered
// by the signature / oversize / timestamp tests in linear_test.go.
func TestWebhookAcksHandlerError(t *testing.T) {
	t.Parallel()

	api := newLinearAPIServer(t, 3600)
	now := time.UnixMilli(1_700_000_000_000)
	bot, _ := newLinearRuntime(t, api, linear.Options{WebhookSecret: "whsec", Now: func() time.Time { return now }})

	var calls int
	bot.OnNewMention(func(context.Context, *chat.MessageEvent) error {
		calls++
		return errors.New("handler boom")
	})

	rec := postHardenRaw(t, bot, "whsec", createdPayload(now, "C1", "hello", "U1", "User One", "APP1"))
	if rec.Code != http.StatusOK {
		t.Fatalf("handler error status = %d, want 200 (handler failure is acked, not retried)", rec.Code)
	}
	if calls != 1 {
		t.Fatalf("handler calls = %d, want 1", calls)
	}
}

// TestWebhookRejectsNonPost verifies the handler only accepts POST.
func TestWebhookRejectsNonPost(t *testing.T) {
	t.Parallel()

	api := newLinearAPIServer(t, 3600)
	now := time.UnixMilli(1_700_000_000_000)
	bot, _ := newLinearRuntime(t, api, linear.Options{WebhookSecret: "whsec", Now: func() time.Time { return now }})
	handler, err := bot.Webhook("linear")
	if err != nil {
		t.Fatalf("webhook: %v", err)
	}
	req := httptest.NewRequest(http.MethodGet, "/linear", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET status = %d, want 405", rec.Code)
	}
}

func promptedByActorPayload(now time.Time, commentID, body, actorID, actorName, actorType string) string {
	return fmt.Sprintf(`{
		"type":"AgentSessionEvent",
		"action":"prompted",
		"organizationId":"ORG1",
		"createdAt":"2026-05-12T00:00:01Z",
		"webhookTimestamp":%d,
		"agentSession":{
			"id":"S1",
			"issueId":"ISSUE1",
			"appUserId":"APP1",
			"comment":{"id":"C1","body":"hello"}
		},
		"agentActivity":{
			"id":"A1",
			"sourceCommentId":"%s",
			"createdAt":"2026-05-12T00:00:01Z",
			"content":{"type":"prompt","body":"%s"},
			"user":{"id":"%s","type":"%s","name":"%s"}
		}
	}`, now.UnixMilli(), commentID, body, actorID, actorType, actorName)
}
