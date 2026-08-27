package slack_test

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/coder/chat"
	"github.com/coder/chat/adapters/slack"
)

// newAdmissionTestAdapter builds a single-tenant adapter with static identity
// so webhook shapes reach dispatch without any Slack API traffic.
func newAdmissionTestAdapter(t *testing.T, now time.Time) *slack.Adapter {
	t.Helper()
	adapter, err := slack.New(context.Background(), slack.Options{
		SigningSecret: "secret",
		BotToken:      "xoxb-test",
		TeamID:        "T1",
		BotUserID:     "UBOT",
		BotID:         "BBOT",
		Now:           func() time.Time { return now },
	})
	if err != nil {
		t.Fatalf("new slack adapter: %v", err)
	}
	return adapter
}

// rejectingDispatch simulates the runtime's Admission Bound rejection (ADR
// 0015), wrapped the way dispatch wraps its sentinel.
func rejectingDispatch(context.Context, *chat.Event) error {
	return fmt.Errorf("dispatch: %w", chat.ErrAdmissionRejected)
}

// TestEventAdmissionRejectionAnswersRetryStatus pins the Events API mapping:
// Slack redelivers event callbacks, so an admission rejection answers a
// retry-inducing 503 instead of acknowledging work the runtime refused.
func TestEventAdmissionRejectionAnswersRetryStatus(t *testing.T) {
	t.Parallel()

	now := time.Unix(1_700_000_000, 0)
	handler := newAdmissionTestAdapter(t, now).Webhook(rejectingDispatch)

	rec := serveSignedSlackWebhook(t, handler, now, `{
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
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("event admission rejection status = %d, want 503 (body = %s)", rec.Code, rec.Body.String())
	}
}

// TestCommandAdmissionRejectionAnswersBusySignal pins the slash-command
// mapping: Slack does not redeliver commands, so the shape's ack body carries
// a truthful, user-visible busy message instead of a retry-inducing status.
func TestCommandAdmissionRejectionAnswersBusySignal(t *testing.T) {
	t.Parallel()

	now := time.Unix(1_700_000_000, 0)
	handler := newAdmissionTestAdapter(t, now).Webhook(rejectingDispatch)

	form := url.Values{}
	form.Set("command", "/deploy")
	form.Set("team_id", "T1")
	form.Set("channel_id", "C1")
	form.Set("user_id", "U1")
	form.Set("trigger_id", "trig")
	rec := serveSignedSlackForm(t, handler, now, form)
	if rec.Code != http.StatusOK {
		t.Fatalf("command admission rejection status = %d, want 200 with a busy body (body = %s)", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, "at capacity") || !strings.Contains(body, "was not run") {
		t.Fatalf("command admission rejection body = %q, want a truthful busy signal", body)
	}
}

// TestInteractionAdmissionRejectionAnswersRetryStatus pins the block_actions
// mapping: the ack body is not rendered to the user, so the truthful signal is
// a non-2xx ack Slack surfaces as a visible failure on the component.
func TestInteractionAdmissionRejectionAnswersRetryStatus(t *testing.T) {
	t.Parallel()

	now := time.Unix(1_700_000_000, 0)
	handler := newAdmissionTestAdapter(t, now).Webhook(rejectingDispatch)

	rec := serveSignedSlackInteractivity(t, handler, now, `{
		"type":"block_actions",
		"team":{"id":"T1"},
		"user":{"id":"U1"},
		"channel":{"id":"C1"},
		"container":{"channel_id":"C1","message_ts":"333.000"},
		"trigger_id":"trigger-999",
		"response_url":"https://hooks.slack.com/actions/T1/999",
		"actions":[{"action_id":"approve","block_id":"b1","value":"yes","type":"button"}]
	}`)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("interaction admission rejection status = %d, want 503 (body = %s)", rec.Code, rec.Body.String())
	}
}
