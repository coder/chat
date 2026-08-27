package linear_test

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/coder/chat"
	"github.com/coder/chat/adapters/linear"
	"github.com/coder/chat/state/memory"
)

// TestAdmissionRejectionAnswersRetryStatus proves the Linear mapping end to
// end: Linear redelivers webhooks on non-2xx, so a delivery the runtime's
// Admission Bound rejects (ADR 0015) is answered with a retry-inducing 503 —
// never acknowledged as handled.
func TestAdmissionRejectionAnswersRetryStatus(t *testing.T) {
	t.Parallel()

	api := newLinearAPIServer(t, 3600)
	now := time.UnixMilli(1_700_000_000_000)

	adapter, err := linear.New(context.Background(), linear.Options{
		WebhookSecret:     "whsec",
		ClientCredentials: linear.ClientCredentials{ClientID: "client", ClientSecret: "secret"},
		APIBaseURL:        api.URL,
		Client:            api.Client(),
		Now:               func() time.Time { return now },
	})
	if err != nil {
		t.Fatalf("new adapter: %v", err)
	}
	bot, err := chat.New(context.Background(),
		chat.WithState(memory.New()),
		chat.WithAdapter(adapter),
		chat.WithRuntimeOptions(chat.RuntimeOptions{
			DedupeTTL:     time.Hour,
			ThreadLockTTL: time.Hour,
			Dispatch:      chat.DispatchDeferred,
			MaxDetached:   1,
			DetachTimeout: 5 * time.Second,
		}),
	)
	if err != nil {
		t.Fatalf("new runtime: %v", err)
	}
	defer func() { _ = bot.Shutdown(context.Background()) }()

	var wg sync.WaitGroup
	wg.Add(1)
	release := make(chan struct{})
	bot.OnNewMention(func(ctx context.Context, ev *chat.MessageEvent) error {
		defer wg.Done()
		<-release
		return nil
	})

	handler, err := bot.Webhook("linear")
	if err != nil {
		t.Fatalf("webhook: %v", err)
	}
	serve := func(body string) *httptest.ResponseRecorder {
		bodyBytes := []byte(body)
		req := httptest.NewRequest(http.MethodPost, "/linear", bytes.NewReader(bodyBytes))
		signLinearRequest(req, "whsec", bodyBytes)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		return rec
	}

	// First delivery saturates MaxDetached=1.
	if rec := serve(createdPayload(now, "C1", "hello", "U1", "User One", "APP1")); rec.Code != http.StatusOK {
		t.Fatalf("first delivery status = %d, body = %s", rec.Code, rec.Body.String())
	}
	// Second delivery hits the Admission Bound: retry-inducing 503, so
	// Linear's redelivery covers the event.
	rec := serve(createdPayloadForOrganization(now, "ORG1", "C2", "hello again", "U1", "User One", "APP1"))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("saturated delivery status = %d, want 503 (body = %s)", rec.Code, rec.Body.String())
	}

	close(release)
	wg.Wait()
}
