package slackapi_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/coder/chat/adapters/slack/slackapi"
	"github.com/coder/chat/adapters/slack/slackapi/slackapitest"
)

const webhookSecret = "webhook-secret"

var webhookNow = time.Unix(1_700_000_000, 0)

// serveWebhook sends a signed POST with body to a Webhook and returns the
// recorder and the deliveries that the handler got.
func serveWebhook(t *testing.T, contentType, body string, header http.Header, handler slackapi.Handler) (*httptest.ResponseRecorder, []*slackapi.Delivery) {
	t.Helper()
	var got []*slackapi.Delivery
	webhook := &slackapi.Webhook{
		SigningSecret: webhookSecret,
		Now:           func() time.Time { return webhookNow },
		Handler: func(ctx context.Context, d *slackapi.Delivery) (any, error) {
			got = append(got, d)
			if handler == nil {
				return nil, nil
			}
			return handler(ctx, d)
		},
	}
	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/slack", strings.NewReader(body))
	req.Header.Set("Content-Type", contentType)
	for key, values := range header {
		req.Header[key] = values
	}
	slackapitest.SignRequest(req, []byte(body), webhookSecret, webhookNow)
	rec := httptest.NewRecorder()
	webhook.ServeHTTP(rec, req)
	return rec, got
}

func TestWebhookDeliveries(t *testing.T) {
	t.Parallel()

	const eventBody = `{"type":"event_callback","team_id":"T1","event_id":"Ev1","event":{"type":"app_mention"}}`
	const interaction = `{"type":"block_actions","team":{"id":"T1"}}`
	tests := []struct {
		name        string
		contentType string
		body        string
		header      http.Header
		want        slackapi.Delivery
	}{
		{
			name:        "events api",
			contentType: "application/json",
			body:        eventBody,
			want:        slackapi.Delivery{Type: slackapi.DeliveryEventsAPI, Payload: json.RawMessage(eventBody)},
		},
		{
			name:        "events api retry",
			contentType: "application/json",
			body:        eventBody,
			header:      http.Header{"X-Slack-Retry-Num": {"2"}, "X-Slack-Retry-Reason": {"http_timeout"}},
			want:        slackapi.Delivery{Type: slackapi.DeliveryEventsAPI, Payload: json.RawMessage(eventBody), RetryAttempt: 2, RetryReason: "http_timeout"},
		},
		{
			name:        "other envelope type",
			contentType: "application/json",
			body:        `{"type":"app_rate_limited"}`,
			want:        slackapi.Delivery{Type: slackapi.DeliveryEventsAPI, Payload: json.RawMessage(`{"type":"app_rate_limited"}`)},
		},
		{
			name:        "interactivity",
			contentType: "application/x-www-form-urlencoded",
			body:        url.Values{"payload": {interaction}}.Encode(),
			want:        slackapi.Delivery{Type: slackapi.DeliveryInteractive, Payload: json.RawMessage(interaction)},
		},
		{
			name:        "slash command",
			contentType: "application/x-www-form-urlencoded; charset=utf-8",
			body:        url.Values{"command": {"/deploy"}, "text": {"api now"}, "team_id": {"T1"}}.Encode(),
			want:        slackapi.Delivery{Type: slackapi.DeliverySlashCommands, Payload: json.RawMessage(`{"command":"/deploy","team_id":"T1","text":"api now"}`)},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			rec, got := serveWebhook(t, tt.contentType, tt.body, tt.header, nil)
			if rec.Code != http.StatusOK || rec.Body.Len() != 0 {
				t.Fatalf("response = %d %q, want 200 with no body", rec.Code, rec.Body.String())
			}
			if len(got) != 1 {
				t.Fatalf("deliveries = %d, want 1", len(got))
			}
			d := got[0]
			if d.Type != tt.want.Type || string(d.Payload) != string(tt.want.Payload) ||
				d.RetryAttempt != tt.want.RetryAttempt || d.RetryReason != tt.want.RetryReason {
				t.Fatalf("delivery = %+v (payload %s), want %+v (payload %s)", *d, d.Payload, tt.want, tt.want.Payload)
			}
		})
	}
}

func TestWebhookURLVerification(t *testing.T) {
	t.Parallel()

	rec, got := serveWebhook(t, "application/json", `{"type":"url_verification","challenge":"abc123"}`, nil, nil)
	if rec.Code != http.StatusOK || rec.Body.String() != "abc123" {
		t.Fatalf("response = %d %q, want 200 with the challenge", rec.Code, rec.Body.String())
	}
	if len(got) != 0 {
		t.Fatalf("handler got %d deliveries, want 0", len(got))
	}
}

func TestWebhookRejectsRequests(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		contentType string
		body        string
		want        int
	}{
		{name: "invalid JSON", contentType: "application/json", body: `{`, want: http.StatusBadRequest},
		{name: "invalid interactivity JSON", contentType: "application/x-www-form-urlencoded", body: url.Values{"payload": {"{"}}.Encode(), want: http.StatusBadRequest},
		{name: "unsupported form", contentType: "application/x-www-form-urlencoded", body: "foo=bar", want: http.StatusBadRequest},
		{name: "too large", contentType: "application/json", body: strings.Repeat("a", 1<<20+1), want: http.StatusRequestEntityTooLarge},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			rec, got := serveWebhook(t, tt.contentType, tt.body, nil, nil)
			if rec.Code != tt.want {
				t.Fatalf("status = %d, want %d (body %q)", rec.Code, tt.want, rec.Body.String())
			}
			if len(got) != 0 {
				t.Fatalf("handler got %d deliveries, want 0", len(got))
			}
		})
	}
}

func TestWebhookRejectsUnsignedAndNonPost(t *testing.T) {
	t.Parallel()

	webhook := &slackapi.Webhook{
		SigningSecret: webhookSecret,
		Handler: func(context.Context, *slackapi.Delivery) (any, error) {
			t.Error("handler must not run")
			return nil, nil
		},
	}
	body := `{"type":"event_callback"}`

	unsigned := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/slack", strings.NewReader(body))
	rec := httptest.NewRecorder()
	webhook.ServeHTTP(rec, unsigned)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("unsigned status = %d, want 401", rec.Code)
	}

	get := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/slack", nil)
	rec = httptest.NewRecorder()
	webhook.ServeHTTP(rec, get)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET status = %d, want 405", rec.Code)
	}
}

func TestWebhookHandlerOutcomes(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		response any
		err      error
		status   int
		body     string
	}{
		{name: "ack", status: http.StatusOK},
		{name: "ack with response", response: map[string]string{"text": "hi"}, status: http.StatusOK, body: `{"text":"hi"}`},
		{name: "busy", err: fmt.Errorf("queue full: %w", slackapi.ErrBusy), status: http.StatusServiceUnavailable},
		{name: "invalid payload", err: fmt.Errorf("%w: no team", slackapi.ErrInvalidPayload), status: http.StatusBadRequest},
		{name: "other error", err: errors.New("store down"), status: http.StatusInternalServerError},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			rec, _ := serveWebhook(t, "application/json", `{"type":"event_callback"}`, nil, func(context.Context, *slackapi.Delivery) (any, error) {
				return tt.response, tt.err
			})
			if rec.Code != tt.status {
				t.Fatalf("status = %d, want %d", rec.Code, tt.status)
			}
			if tt.body != "" {
				if got := rec.Body.String(); got != tt.body {
					t.Fatalf("body = %q, want %q", got, tt.body)
				}
				if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
					t.Fatalf("Content-Type = %q, want application/json", ct)
				}
			}
			if tt.status == http.StatusOK && tt.body == "" && rec.Body.Len() != 0 {
				t.Fatalf("body = %q, want empty", rec.Body.String())
			}
		})
	}
}
