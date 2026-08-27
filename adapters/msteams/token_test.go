package msteams_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/coder/chat/adapters/msteams"
)

func TestNewRequiresAppCredentials(t *testing.T) {
	t.Parallel()
	if _, err := msteams.New(context.Background(), msteams.Options{MicrosoftAppPassword: "secret"}); err == nil {
		t.Fatal("missing app id should be a construction error")
	}
	if _, err := msteams.New(context.Background(), msteams.Options{MicrosoftAppID: "app"}); err == nil {
		t.Fatal("missing app password should be a construction error")
	}
}

func TestInitMintsAndCachesOutboundToken(t *testing.T) {
	t.Parallel()
	bf := newFakeBotConnector(t)
	now := testClock
	calls := 0
	srv := fakeTokenServer(t, "tok-1", &calls)
	a := newTestAdapter(t, bf, func(o *msteams.Options) {
		o.Now = func() time.Time { return now }
		o.TokenURL = srv.URL
	})

	if err := a.Init(context.Background()); err != nil {
		t.Fatalf("init: %v", err)
	}
	if err := a.Init(context.Background()); err != nil {
		t.Fatalf("init (cached): %v", err)
	}
	if calls != 1 {
		t.Fatalf("token mints = %d, want 1 (cached on the second Init)", calls)
	}

	// Advance past expiry (expires_in 3600s) so the next use re-mints lazily.
	now = testClock.Add(2 * time.Hour)
	if err := a.Init(context.Background()); err != nil {
		t.Fatalf("init (refresh): %v", err)
	}
	if calls != 2 {
		t.Fatalf("token mints after expiry = %d, want 2", calls)
	}
}

func TestInitFailsOnBadCredentials(t *testing.T) {
	t.Parallel()
	bf := newFakeBotConnector(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":"invalid_client","error_description":"bad secret"}`))
	}))
	t.Cleanup(srv.Close)
	a := newTestAdapter(t, bf, func(o *msteams.Options) { o.TokenURL = srv.URL })

	if err := a.Init(context.Background()); err == nil {
		t.Fatal("Init should fail fast on invalid credentials")
	}
}
