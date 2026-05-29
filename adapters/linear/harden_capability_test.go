package linear_test

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/chat"
	"github.com/coder/chat/adapters/linear"
)

// TestAdapterAccessPresentAndAbsent verifies the Optional Capability is reachable
// via chat.AdapterAs for the right name and concrete type, and absent (ok=false)
// for an unknown name or a wrong type assertion.
func TestAdapterAccessPresentAndAbsent(t *testing.T) {
	t.Parallel()

	api := newLinearAPIServer(t, 3600)
	now := time.UnixMilli(1_700_000_000_000)
	bot, adapter := newLinearRuntime(t, api, linear.Options{WebhookSecret: "whsec", Now: func() time.Time { return now }})

	got, ok := chat.AdapterAs[*linear.Adapter](bot, "linear")
	if !ok || got != adapter {
		t.Fatalf("AdapterAs present = (%v, %v), want the linear adapter", got, ok)
	}
	if _, ok := chat.AdapterAs[*linear.Adapter](bot, "nope"); ok {
		t.Fatal("AdapterAs for unknown name returned ok=true")
	}
	// Wrong concrete type for the right name.
	if _, ok := chat.AdapterAs[wrongType](bot, "linear"); ok {
		t.Fatal("AdapterAs for wrong type returned ok=true")
	}
	if _, ok := chat.AdapterAs[*linear.Adapter](nil, "linear"); ok {
		t.Fatal("AdapterAs on nil runtime returned ok=true")
	}
}

type wrongType struct{}

// TestMarkdownAndTextPassThroughUnchanged verifies Plain Text and Portable
// Markdown bodies reach Linear verbatim with no conversion layer, on both thread
// kinds.
func TestMarkdownAndTextPassThroughUnchanged(t *testing.T) {
	t.Parallel()

	api := newLinearAPIServer(t, 3600)
	now := time.UnixMilli(1_700_000_000_000)
	bot, _ := newLinearRuntime(t, api, linear.Options{WebhookSecret: "whsec", Now: func() time.Time { return now }})

	const md = "# Heading\n\n- *bullet* with `code` and [link](https://x)"

	// Agent-session response.
	sessionThread := agentSessionThread(t, bot, api, now)
	st, err := bot.Thread(context.Background(), sessionThread)
	if err != nil {
		t.Fatalf("session thread: %v", err)
	}
	if _, err := st.Post(context.Background(), chat.Markdown(md)); err != nil {
		t.Fatalf("post markdown response: %v", err)
	}
	api.assertActivity(t, 0, linearActivity{AgentSessionID: "S1", Content: activityContent{Type: "response", Body: md}})

	// Issue comment.
	var commentThread chat.ThreadID
	bot.OnNewMention(func(_ context.Context, ev *chat.MessageEvent) error {
		if r, _ := linear.RawMessageFrom(ev.Message); r != nil && r.Kind == "comment" {
			commentThread = ev.Thread.ID()
		}
		return nil
	})
	postLinearEvent(t, bot, "whsec", commentPayload(now, "CM1", "@APP1 hi", "U1", "User One", "ISSUE1", ""))
	ct, err := bot.Thread(context.Background(), commentThread)
	if err != nil {
		t.Fatalf("comment thread: %v", err)
	}
	if _, err := ct.Post(context.Background(), chat.Markdown(md)); err != nil {
		t.Fatalf("post markdown comment: %v", err)
	}
	if got := api.lastComment(t)["body"]; got != md {
		t.Fatalf("comment body = %q, want verbatim markdown", got)
	}
}

// TestGraphQLEmptyQueryRejected verifies the GraphQL escape hatch rejects an empty
// query before any network call.
func TestGraphQLEmptyQueryRejected(t *testing.T) {
	t.Parallel()

	api := newLinearAPIServer(t, 3600)
	now := time.UnixMilli(1_700_000_000_000)
	_, adapter := newLinearRuntime(t, api, linear.Options{WebhookSecret: "whsec", Now: func() time.Time { return now }})

	if err := adapter.GraphQL(context.Background(), "", nil, nil); err == nil {
		t.Fatal("expected empty query to be rejected")
	}
}

// TestPostMessageValidation verifies PostMessage rejects empty text and
// unsupported formats before any API call (scoped-in portable bodies only).
func TestPostMessageValidation(t *testing.T) {
	t.Parallel()

	api := newLinearAPIServer(t, 3600)
	now := time.UnixMilli(1_700_000_000_000)
	bot, adapter := newLinearRuntime(t, api, linear.Options{WebhookSecret: "whsec", Now: func() time.Time { return now }})
	threadID := agentSessionThread(t, bot, api, now)
	ref, err := adapter.ValidateThreadID(threadID)
	if err != nil {
		t.Fatalf("validate: %v", err)
	}

	if _, err := adapter.PostMessage(context.Background(), ref, chat.PostableMessage{Text: ""}); err == nil {
		t.Fatal("expected empty text to be rejected")
	}
	if _, err := adapter.PostMessage(context.Background(), ref, chat.PostableMessage{Text: "x", Format: chat.MessageFormat(99)}); err == nil {
		t.Fatal("expected unsupported format to be rejected")
	}
	if api.activityCount() != 0 {
		t.Fatalf("invalid PostMessage reached the API: %d", api.activityCount())
	}
}

// TestRateLimitRetryDisabledWithMaxAttemptsOne verifies MaxAttempts=1 disables
// retry: a single throttled response returns a typed *RateLimited immediately.
func TestRateLimitRetryDisabledWithMaxAttemptsOne(t *testing.T) {
	t.Parallel()

	api := newLinearAPIServer(t, 3600)
	api.rateLimit = 100
	now := time.UnixMilli(1_700_000_000_000)
	bot, adapter := newLinearRuntime(t, api, linear.Options{
		WebhookSecret: "whsec",
		Now:           func() time.Time { return now },
		RetryPolicy:   linear.RetryPolicy{MaxAttempts: 1},
	})
	threadID := agentSessionThread(t, bot, api, now)

	_, err := adapter.PostThought(context.Background(), threadID, "thinking")
	var rl *linear.RateLimited
	if !errors.As(err, &rl) {
		t.Fatalf("err = %v, want *linear.RateLimited", err)
	}
	if rl.Attempts != 1 {
		t.Fatalf("attempts = %d, want 1 (retry disabled)", rl.Attempts)
	}
}

// TestRateLimitGraphQLRatelimitedRetries verifies that a GraphQL-level
// RATELIMITED error (HTTP 200 with errors[].extensions.code) is recognized as
// throttling and retried, then succeeds.
func TestRateLimitGraphQLRatelimitedRetries(t *testing.T) {
	t.Parallel()

	api := newGraphQLRateLimitServer(t)
	now := time.UnixMilli(1_700_000_000_000)
	bot, adapter := newLinearRuntime(t, api, linear.Options{
		WebhookSecret: "whsec",
		Now:           func() time.Time { return now },
		RetryPolicy:   linear.RetryPolicy{MaxAttempts: 3, MaxElapsed: time.Second, BaseDelay: time.Millisecond, MaxDelay: 5 * time.Millisecond},
	})
	threadID := agentSessionThread(t, bot, api, now)

	if _, err := adapter.PostThought(context.Background(), threadID, "thinking"); err != nil {
		t.Fatalf("graphql ratelimited retry: %v", err)
	}
	if api.activityCount() != 1 {
		t.Fatalf("activity count = %d, want 1 after graphql-ratelimited retry", api.activityCount())
	}
}

// TestRateLimitRetryStopsAtContextCancel verifies bounded retry never sleeps past
// the request context: a cancelled context aborts the retry loop with the context
// error rather than a RateLimited (the first-thought window is never violated).
func TestRateLimitRetryStopsAtContextCancel(t *testing.T) {
	t.Parallel()

	api := newLinearAPIServer(t, 3600)
	api.rateLimit = 100
	now := time.UnixMilli(1_700_000_000_000)
	bot, adapter := newLinearRuntime(t, api, linear.Options{
		WebhookSecret: "whsec",
		Now:           func() time.Time { return now },
		RetryPolicy:   linear.RetryPolicy{MaxAttempts: 5, MaxElapsed: time.Hour, BaseDelay: time.Hour, MaxDelay: time.Hour},
	})
	threadID := agentSessionThread(t, bot, api, now)

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // already cancelled; the first backoff sleep must abort immediately.
	_, err := adapter.PostThought(ctx, threadID, "thinking")
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled (retry must not outlast ctx)", err)
	}
}

// newGraphQLRateLimitServer returns a server whose first AgentActivityCreate call
// responds HTTP 200 with a GraphQL RATELIMITED error, then succeeds.
func newGraphQLRateLimitServer(t *testing.T) *linearAPIServer {
	t.Helper()
	api := &linearAPIServer{expires: 3600, tokenScope: "read write app:mentionable app:assignable"}
	api.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/oauth/token":
			api.mu.Lock()
			api.tokens++
			token := fmt.Sprintf("token-%d", api.tokens)
			api.mu.Unlock()
			writeJSON(t, w, map[string]any{"access_token": token, "expires_in": api.expires, "scope": api.tokenScope})
		case "/graphql":
			var req graphQLRequest
			decodeJSON(t, r.Body, &req)
			if strings.Contains(req.Query, "ViewerIdentity") {
				writeJSON(t, w, map[string]any{"data": map[string]any{"viewer": map[string]any{"id": "APP1", "name": "Linear Bot", "displayName": "Linear Bot", "organization": map[string]any{"id": "ORG1"}}}})
				return
			}
			if strings.Contains(req.Query, "AgentActivityCreate") {
				api.mu.Lock()
				first := api.rateLimitSeen == 0
				api.rateLimitSeen++
				api.mu.Unlock()
				if first {
					writeJSON(t, w, map[string]any{"errors": []map[string]any{{"message": "rate limit exceeded", "extensions": map[string]any{"code": "RATELIMITED"}}}})
					return
				}
				input, _ := req.Variables["input"].(map[string]any)
				api.mu.Lock()
				api.activity = append(api.activity, linearActivity{})
				api.rawActivity = append(api.rawActivity, input)
				id := fmt.Sprintf("ACT%d", len(api.activity))
				api.mu.Unlock()
				writeJSON(t, w, map[string]any{"data": map[string]any{"agentActivityCreate": map[string]any{"success": true, "agentActivity": map[string]any{"id": id}}}})
				return
			}
			t.Fatalf("unexpected query: %s", req.Query)
		default:
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
	}))
	t.Cleanup(api.Close)
	return api
}
