package msteams_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/chat"
	"github.com/coder/chat/adapters/msteams"
)

// postTarget builds an adapter wired to a fake token endpoint and returns a
// ThreadRef whose serviceUrl points at the given Connector server, so PostMessage
// hits the fake.
func postTarget(t *testing.T, bf *fakeBotConnector, connURL string, mutate func(*msteams.Options)) (*msteams.Adapter, chat.ThreadRef) {
	t.Helper()
	tok := fakeTokenServer(t, "out-token", nil)
	a := newTestAdapter(t, bf, func(o *msteams.Options) {
		o.TokenURL = tok.URL
		o.BotName = "Bot"
		if mutate != nil {
			mutate(o)
		}
	})
	id := msteams.EncodeThreadIDForTest(connURL, testConvID, testTenant, "msteams", "channel", true)
	ref, err := a.ValidateThreadID(id)
	if err != nil {
		t.Fatalf("validate thread id: %v", err)
	}
	return a, ref
}

func TestPostMessageSendToConversation(t *testing.T) {
	t.Parallel()
	bf := newFakeBotConnector(t)

	var gotAuth, gotMethod, gotPath string
	var gotBody map[string]any
	conn := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth, gotMethod, gotPath = r.Header.Get("Authorization"), r.Method, r.URL.Path
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		_ = json.NewEncoder(w).Encode(map[string]any{"id": "posted-99"})
	}))
	t.Cleanup(conn.Close)

	a, ref := postTarget(t, bf, conn.URL, nil)
	sent, err := a.PostMessage(context.Background(), ref, chat.Markdown("**hi there**"))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	if sent.ID != "posted-99" || sent.ThreadID != ref.ID {
		t.Fatalf("sent = %#v", sent)
	}
	if gotMethod != http.MethodPost || gotAuth != "Bearer out-token" {
		t.Fatalf("method = %q auth = %q", gotMethod, gotAuth)
	}
	if !strings.Contains(gotPath, "/v3/conversations/") || !strings.HasSuffix(gotPath, "/activities") {
		t.Fatalf("path = %q", gotPath)
	}
	if gotBody["type"] != "message" || gotBody["text"] != "**hi there**" || gotBody["textFormat"] != "markdown" {
		t.Fatalf("body = %#v", gotBody)
	}
	if conv, _ := gotBody["conversation"].(map[string]any); conv["id"] != testConvID {
		t.Fatalf("conversation = %#v", gotBody["conversation"])
	}
}

func TestPostMessagePlainTextFormat(t *testing.T) {
	t.Parallel()
	bf := newFakeBotConnector(t)

	var gotBody map[string]any
	conn := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		_ = json.NewEncoder(w).Encode(map[string]any{"id": "posted-1"})
	}))
	t.Cleanup(conn.Close)

	a, ref := postTarget(t, bf, conn.URL, nil)
	if _, err := a.PostMessage(context.Background(), ref, chat.Text("plain words")); err != nil {
		t.Fatalf("post: %v", err)
	}
	if gotBody["textFormat"] != "plain" || gotBody["text"] != "plain words" {
		t.Fatalf("body = %#v", gotBody)
	}
}

func TestPostMessageMapsProactiveErrors(t *testing.T) {
	t.Parallel()
	bf := newFakeBotConnector(t)

	cases := []struct {
		name    string
		status  int
		code    string
		wantErr error
	}{
		{"not installed", http.StatusForbidden, "ForbiddenOperationException", msteams.ErrBotNotInstalled},
		{"writes blocked", http.StatusForbidden, "MessageWritesBlocked", msteams.ErrMessageWritesBlocked},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			conn := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tc.status)
				_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{"code": tc.code, "message": "nope"}})
			}))
			t.Cleanup(conn.Close)

			a, ref := postTarget(t, bf, conn.URL, nil)
			_, err := a.PostMessage(context.Background(), ref, chat.Text("hi"))
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("err = %v, want errors.Is %v", err, tc.wantErr)
			}
		})
	}
}

func TestPostMessageGenericErrorIsConnectorError(t *testing.T) {
	t.Parallel()
	bf := newFakeBotConnector(t)

	conn := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{"code": "BadArgument", "message": "bad"}})
	}))
	t.Cleanup(conn.Close)

	a, ref := postTarget(t, bf, conn.URL, nil)
	_, err := a.PostMessage(context.Background(), ref, chat.Text("hi"))
	var ce *msteams.ConnectorError
	if !errors.As(err, &ce) {
		t.Fatalf("err = %v, want *ConnectorError", err)
	}
	if ce.Status != http.StatusBadRequest || ce.Code != "BadArgument" {
		t.Fatalf("connector error = %#v", ce)
	}
	if errors.Is(err, msteams.ErrBotNotInstalled) || errors.Is(err, msteams.ErrMessageWritesBlocked) {
		t.Fatal("generic error should not match the proactive sentinels")
	}
}

func TestConnectorRetriesOn429ThenSucceeds(t *testing.T) {
	t.Parallel()
	bf := newFakeBotConnector(t)

	var attempts int32
	conn := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if atomic.AddInt32(&attempts, 1) < 3 {
			w.Header().Set("Retry-After", "0")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"id": "ok-after-retry"})
	}))
	t.Cleanup(conn.Close)

	a, ref := postTarget(t, bf, conn.URL, func(o *msteams.Options) {
		o.RetryPolicy = msteams.RetryPolicy{MaxAttempts: 5, BaseDelay: time.Millisecond, MaxDelay: 2 * time.Millisecond, MaxElapsed: time.Second}
	})
	sent, err := a.PostMessage(context.Background(), ref, chat.Text("hi"))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	if sent.ID != "ok-after-retry" {
		t.Fatalf("sent id = %q", sent.ID)
	}
	if got := atomic.LoadInt32(&attempts); got != 3 {
		t.Fatalf("attempts = %d, want 3", got)
	}
}

func TestConnectorRateLimitedExhausts(t *testing.T) {
	t.Parallel()
	bf := newFakeBotConnector(t)

	conn := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Retry-After", "0")
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	t.Cleanup(conn.Close)

	a, ref := postTarget(t, bf, conn.URL, func(o *msteams.Options) {
		o.RetryPolicy = msteams.RetryPolicy{MaxAttempts: 2, BaseDelay: time.Millisecond, MaxDelay: 2 * time.Millisecond, MaxElapsed: time.Second}
	})
	_, err := a.PostMessage(context.Background(), ref, chat.Text("hi"))
	var rl *msteams.RateLimited
	if !errors.As(err, &rl) {
		t.Fatalf("err = %v, want *RateLimited", err)
	}
	if rl.Attempts != 2 || rl.Adapter != "msteams" {
		t.Fatalf("rate limited = %#v", rl)
	}
}
