package linear_test

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/coder/chat"
	"github.com/coder/chat/adapters/linear"
)

func TestRawMessageFirstThoughtDeadlineOnCreated(t *testing.T) {
	t.Parallel()

	api := newLinearAPIServer(t, 3600)
	now := time.UnixMilli(1_700_000_000_000)
	bot, _ := newLinearRuntime(t, api, linear.Options{WebhookSecret: "whsec", Now: func() time.Time { return now }})

	var raw *linear.RawMessage
	bot.OnNewMention(func(_ context.Context, ev *chat.MessageEvent) error {
		r, ok := linear.RawMessageFrom(ev.Message)
		if !ok {
			t.Fatal("RawMessageFrom returned false")
		}
		raw = r
		return nil
	})
	postLinearEvent(t, bot, "whsec", createdPayload(now, "C1", "hello", "U1", "User One", "APP1"))

	if raw == nil {
		t.Fatal("no raw message captured")
	}
	if raw.Kind != "agent_session" || raw.Action != "created" {
		t.Fatalf("raw kind/action = %q/%q", raw.Kind, raw.Action)
	}
	if raw.Session == nil || raw.Session.FirstThoughtDeadline == nil {
		t.Fatal("missing first thought deadline")
	}
	d := raw.Session.FirstThoughtDeadline
	created, _ := time.Parse(time.RFC3339, "2026-05-12T00:00:00Z")
	if !d.SessionCreatedAt.Equal(created) {
		t.Fatalf("session created at = %s, want %s", d.SessionCreatedAt, created)
	}
	if d.Budget != 10*time.Second {
		t.Fatalf("budget = %s, want 10s", d.Budget)
	}
	if !d.Deadline.Equal(created.Add(10 * time.Second)) {
		t.Fatalf("deadline = %s", d.Deadline)
	}
	if len(raw.Envelope) == 0 {
		t.Fatal("envelope not preserved")
	}
}

func TestRawMessagePreservesStopSignalAndContext(t *testing.T) {
	t.Parallel()

	api := newLinearAPIServer(t, 3600)
	now := time.UnixMilli(1_700_000_000_000)
	bot, _ := newLinearRuntime(t, api, linear.Options{WebhookSecret: "whsec", Now: func() time.Time { return now }})

	// First, a created event subscribes the thread so the prompted event routes.
	bot.OnNewMention(func(ctx context.Context, ev *chat.MessageEvent) error {
		return ev.Thread.Subscribe(ctx)
	})
	var raw *linear.RawMessage
	bot.OnSubscribedMessage(func(_ context.Context, ev *chat.MessageEvent) error {
		r, _ := linear.RawMessageFrom(ev.Message)
		raw = r
		return nil
	})
	postLinearEvent(t, bot, "whsec", createdPayload(now, "C1", "hello", "U1", "User One", "APP1"))
	postLinearEvent(t, bot, "whsec", promptedWithStopPayload(now, "C2", "stop now", "U1", "User One"))

	if raw == nil {
		t.Fatal("no prompted raw message captured")
	}
	if !raw.StopRequested() {
		t.Fatalf("stop not detected, signal = %q", raw.Signal)
	}
	var meta map[string]any
	if err := json.Unmarshal(raw.SignalMetadata, &meta); err != nil {
		t.Fatalf("signal metadata: %v", err)
	}
	if meta["reason"] != "user-cancel" {
		t.Fatalf("signal metadata = %#v", meta)
	}
	if raw.Session == nil || raw.Session.Guidance != "be quick" {
		t.Fatalf("guidance not preserved: %#v", raw.Session)
	}
	if len(raw.Session.PreviousComments) == 0 {
		t.Fatal("previous comments not preserved")
	}
	if len(raw.Session.Issue) == 0 {
		t.Fatal("issue not preserved")
	}
	// prompted carries no first-thought deadline.
	if raw.Session.FirstThoughtDeadline != nil {
		t.Fatal("prompted should not carry a first-thought deadline")
	}
}

func TestRawMessageFromForeignMessage(t *testing.T) {
	t.Parallel()
	if _, ok := linear.RawMessageFrom(&chat.Message{Raw: "not-linear"}); ok {
		t.Fatal("expected false for foreign raw")
	}
	if _, ok := linear.RawMessageFrom(nil); ok {
		t.Fatal("expected false for nil message")
	}
}

func promptedWithStopPayload(now time.Time, commentID string, body string, userID string, userName string) string {
	return fmt.Sprintf(`{
		"type":"AgentSessionEvent",
		"action":"prompted",
		"organizationId":"ORG1",
		"createdAt":"2026-05-12T00:00:01Z",
		"guidance":"be quick",
		"previousComments":[{"id":"P1","body":"earlier"}],
		"webhookTimestamp":%d,
		"agentSession":{
			"id":"S1",
			"issueId":"ISSUE1",
			"appUserId":"APP1",
			"createdAt":"2026-05-12T00:00:00Z",
			"issue":{"id":"ISSUE1","title":"An issue"},
			"comment":{"id":"C1","body":"hello"}
		},
		"agentActivity":{
			"id":"A1",
			"sourceCommentId":"%s",
			"createdAt":"2026-05-12T00:00:01Z",
			"content":{"type":"prompt","body":"%s"},
			"signal":"stop",
			"signalMetadata":{"reason":"user-cancel"},
			"user":{"id":"%s","type":"user","name":"%s"}
		}
	}`, now.UnixMilli(), commentID, body, userID, userName)
}
