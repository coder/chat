package slack_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/coder/chat"
	"github.com/coder/chat/adapters/slack"
)

// rateLimitServer is a Slack Web API stub for ADR 0005 retry tests. It throttles
// the first throttleN chat.postMessage calls (HTTP 429 + optional Retry-After, or
// a 200 ratelimited envelope when graphqlStyle is set) and then succeeds, so a
// test can assert bounded retry, Retry-After precedence, exhaustion, and the
// deadline bound at the HTTP boundary. It is independent of the shared
// newSlackAPIServer (which always returns 200).
type rateLimitServer struct {
	*httptest.Server

	mu          sync.Mutex
	postCalls   int
	throttleN   int    // throttle the first N postMessage calls
	retryAfter  string // Retry-After header value on a throttled response ("" = none)
	envelope429 bool   // when true, return a 200 ratelimited envelope instead of HTTP 429
}

func newSlackRateLimitServer(t *testing.T) *rateLimitServer {
	t.Helper()
	rl := &rateLimitServer{}
	rl.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/chat.postMessage" {
			t.Errorf("unexpected Slack API path %s", r.URL.Path)
			w.WriteHeader(http.StatusOK)
			return
		}
		rl.mu.Lock()
		rl.postCalls++
		throttle := rl.postCalls <= rl.throttleN
		retryAfter := rl.retryAfter
		envelope := rl.envelope429
		rl.mu.Unlock()

		if throttle {
			if retryAfter != "" {
				w.Header().Set("Retry-After", retryAfter)
			}
			if envelope {
				// Slack's 200-with-ratelimited shape.
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write([]byte(`{"ok":false,"error":"ratelimited"}`))
				return
			}
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(`{"ok":false,"error":"ratelimited"}`))
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true,"channel":"C1","ts":"999.000"}`))
	}))
	t.Cleanup(rl.Server.Close)
	return rl
}

func (rl *rateLimitServer) calls() int {
	rl.mu.Lock()
	defer rl.mu.Unlock()
	return rl.postCalls
}

// newRateLimitAdapter builds a single-install Slack adapter pointed at the stub,
// with the given retry policy and observer. Init is skipped (the identity is
// pinned via Options), so no auth.test call is needed.
func newRateLimitAdapter(t *testing.T, rl *rateLimitServer, policy slack.RetryPolicy, obs chat.Observer) *slack.Adapter {
	t.Helper()
	adapter, err := slack.New(context.Background(), slack.Options{
		SigningSecret: "secret",
		BotToken:      "xoxb-test",
		TeamID:        "T1",
		BotUserID:     "UBOT",
		BotID:         "BBOT",
		APIBaseURL:    rl.URL,
		Client:        rl.Client(),
		RetryPolicy:   policy,
		Observer:      obs,
	})
	if err != nil {
		t.Fatalf("new slack adapter: %v", err)
	}
	return adapter
}

func rateLimitThreadRef(t *testing.T, adapter *slack.Adapter) chat.ThreadRef {
	t.Helper()
	ref, err := adapter.ValidateThreadID(slack.EncodeChannelThreadIDForTest("T1", "C1"))
	if err != nil {
		t.Fatalf("validate thread id: %v", err)
	}
	return ref
}

// fastRetryPolicy is the deterministic, sub-millisecond policy the retry tests use
// so backoff never depends on wall-clock latency.
func fastRetryPolicy(maxAttempts int) slack.RetryPolicy {
	return slack.RetryPolicy{
		MaxAttempts: maxAttempts,
		MaxElapsed:  time.Second,
		BaseDelay:   time.Millisecond,
		MaxDelay:    5 * time.Millisecond,
	}
}

// A transient Slack 429 with Retry-After is retried within the attempt cap and the
// post ultimately succeeds, hardening the adapter by default (ADR 0005 user story 1).
func TestSlackPostRetriesThrottledThenSucceeds(t *testing.T) {
	t.Parallel()

	rl := newSlackRateLimitServer(t)
	rl.throttleN = 1 // first call throttled, then succeeds
	rl.retryAfter = "0"
	obs := &countingObserver{}
	adapter := newRateLimitAdapter(t, rl, fastRetryPolicy(3), obs)
	ref := rateLimitThreadRef(t, adapter)

	sent, err := adapter.PostMessage(context.Background(), ref, chat.Text("hi"))
	if err != nil {
		t.Fatalf("post after retry: %v", err)
	}
	if sent == nil || sent.ID != "999.000" {
		t.Fatalf("sent = %#v, want ts 999.000", sent)
	}
	if rl.calls() != 2 {
		t.Fatalf("post calls = %d, want 2 (one throttle + one success)", rl.calls())
	}
	// Every attempt is an ObsAdapterCall; the single throttle is one ObsRateLimit.
	if obs.count(chat.ObsAdapterCall) != 2 {
		t.Fatalf("ObsAdapterCall = %d, want 2", obs.count(chat.ObsAdapterCall))
	}
	if obs.count(chat.ObsRateLimit) != 1 {
		t.Fatalf("ObsRateLimit = %d, want 1", obs.count(chat.ObsRateLimit))
	}
}

// The 200-with-ratelimited envelope (not an HTTP 429) is also recognized as Slack
// throttling and retried.
func TestSlackPostRetriesRatelimitedEnvelope(t *testing.T) {
	t.Parallel()

	rl := newSlackRateLimitServer(t)
	rl.throttleN = 1
	rl.envelope429 = true // 200 + {"ok":false,"error":"ratelimited"}
	adapter := newRateLimitAdapter(t, rl, fastRetryPolicy(3), nil)
	ref := rateLimitThreadRef(t, adapter)

	if _, err := adapter.PostMessage(context.Background(), ref, chat.Text("hi")); err != nil {
		t.Fatalf("post after ratelimited-envelope retry: %v", err)
	}
	if rl.calls() != 2 {
		t.Fatalf("post calls = %d, want 2", rl.calls())
	}
}

// A platform that throttles forever exhausts to a typed *slack.RateLimited after
// exactly MaxAttempts, carrying the attempt count, last Retry-After, and the raw
// platform response as a Platform Escape Hatch (ADR 0005 user story 7).
func TestSlackPostExhaustionReturnsTypedRateLimited(t *testing.T) {
	t.Parallel()

	rl := newSlackRateLimitServer(t)
	rl.throttleN = 100 // always throttle
	rl.retryAfter = "0"
	obs := &countingObserver{}
	adapter := newRateLimitAdapter(t, rl, fastRetryPolicy(3), obs)
	ref := rateLimitThreadRef(t, adapter)

	_, err := adapter.PostMessage(context.Background(), ref, chat.Text("hi"))
	var limited *slack.RateLimited
	if !errors.As(err, &limited) {
		t.Fatalf("err = %v, want *slack.RateLimited", err)
	}
	if limited.Attempts != 3 {
		t.Fatalf("attempts = %d, want 3 (== MaxAttempts)", limited.Attempts)
	}
	if limited.Adapter != "slack" {
		t.Fatalf("adapter = %q, want slack", limited.Adapter)
	}
	if limited.Raw == nil {
		t.Fatal("Raw is nil; the raw platform response must ride as a Platform Escape Hatch")
	}
	if rl.calls() != 3 {
		t.Fatalf("post calls = %d, want 3 (bounded by MaxAttempts)", rl.calls())
	}
	if obs.count(chat.ObsRateLimit) != 3 {
		t.Fatalf("ObsRateLimit = %d, want 3 (one per throttled attempt)", obs.count(chat.ObsRateLimit))
	}
}

// MaxAttempts: 1 disables retry: a single throttle returns immediately with a
// *RateLimited and exactly one attempt, identical to the pre-ADR no-retry adapter.
func TestSlackPostRetryDisabledWithMaxAttemptsOne(t *testing.T) {
	t.Parallel()

	rl := newSlackRateLimitServer(t)
	rl.throttleN = 100
	rl.retryAfter = "1"
	adapter := newRateLimitAdapter(t, rl, slack.RetryPolicy{MaxAttempts: 1}, nil)
	ref := rateLimitThreadRef(t, adapter)

	_, err := adapter.PostMessage(context.Background(), ref, chat.Text("hi"))
	var limited *slack.RateLimited
	if !errors.As(err, &limited) {
		t.Fatalf("err = %v, want *slack.RateLimited", err)
	}
	if limited.Attempts != 1 {
		t.Fatalf("attempts = %d, want 1 (retry disabled)", limited.Attempts)
	}
	if rl.calls() != 1 {
		t.Fatalf("post calls = %d, want 1 (single-shot)", rl.calls())
	}
}

// The load-bearing invariant: a Retry-After larger than the caller's remaining
// context deadline must NOT be slept off. The adapter returns *RateLimited
// promptly rather than blowing the platform ack window (ADR 0005 user story 5).
func TestSlackPostNeverSleepsPastContextDeadline(t *testing.T) {
	t.Parallel()

	rl := newSlackRateLimitServer(t)
	rl.throttleN = 100
	rl.retryAfter = "3600" // one hour: far beyond any ack window
	adapter := newRateLimitAdapter(t, rl, slack.RetryPolicy{
		MaxAttempts: 5,
		MaxElapsed:  time.Hour,
		BaseDelay:   time.Hour,
		MaxDelay:    time.Hour,
	}, nil)
	ref := rateLimitThreadRef(t, adapter)

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	done := make(chan error, 1)
	start := time.Now()
	go func() {
		_, err := adapter.PostMessage(ctx, ref, chat.Text("hi"))
		done <- err
	}()

	select {
	case err := <-done:
		var limited *slack.RateLimited
		if !errors.As(err, &limited) {
			t.Fatalf("err = %v, want *slack.RateLimited (deadline-bounded, not slept off)", err)
		}
		if elapsed := time.Since(start); elapsed > time.Second {
			t.Fatalf("post took %s; it must not sleep past the context deadline", elapsed)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("post slept past its caller deadline: a long Retry-After was not deadline-bounded")
	}
}

// A non-throttling failure (auth error: HTTP 401) is returned on the first attempt
// with zero retries; only throttling responses are retried (ADR 0005).
func TestSlackPostDoesNotRetryNonThrottlingError(t *testing.T) {
	t.Parallel()

	var calls int
	var mu sync.Mutex
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		calls++
		mu.Unlock()
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"ok":false,"error":"invalid_auth"}`))
	}))
	t.Cleanup(server.Close)

	adapter, err := slack.New(context.Background(), slack.Options{
		SigningSecret: "secret",
		BotToken:      "xoxb-test",
		TeamID:        "T1",
		BotUserID:     "UBOT",
		BotID:         "BBOT",
		APIBaseURL:    server.URL,
		Client:        server.Client(),
		RetryPolicy:   fastRetryPolicy(5),
	})
	if err != nil {
		t.Fatalf("new slack adapter: %v", err)
	}
	ref := rateLimitThreadRef(t, adapter)

	_, postErr := adapter.PostMessage(context.Background(), ref, chat.Text("hi"))
	if postErr == nil {
		t.Fatal("expected an error from a 401 auth failure")
	}
	var limited *slack.RateLimited
	if errors.As(postErr, &limited) {
		t.Fatal("a non-throttling 401 must not surface as RateLimited")
	}
	mu.Lock()
	defer mu.Unlock()
	if calls != 1 {
		t.Fatalf("post calls = %d, want 1 (non-throttling error is never retried)", calls)
	}
}

// The zero-value / default RetryPolicy keeps its cumulative backoff ceiling under
// Slack's 3-second ack window, so the synchronous DispatchSync path is safe
// without tuning (ADR 0005 user story 6, default-policy testing decision).
func TestSlackDefaultPolicyCeilingUnderAckWindow(t *testing.T) {
	t.Parallel()

	// MaxAttempts default plus a Retry-After honored on every throttle still cannot
	// exceed the ack window because MaxElapsed bounds cumulative backoff. We drive
	// it with a throttle-forever server and assert the synchronous call returns well
	// inside the 3-second Slack budget.
	rl := newSlackRateLimitServer(t)
	rl.throttleN = 100
	rl.retryAfter = "0"
	adapter := newRateLimitAdapter(t, rl, slack.RetryPolicy{}, nil) // zero value = default
	ref := rateLimitThreadRef(t, adapter)

	start := time.Now()
	_, err := adapter.PostMessage(context.Background(), ref, chat.Text("hi"))
	elapsed := time.Since(start)

	var limited *slack.RateLimited
	if !errors.As(err, &limited) {
		t.Fatalf("err = %v, want *slack.RateLimited", err)
	}
	if elapsed >= 3*time.Second {
		t.Fatalf("default-policy synchronous retry took %s; it must stay under Slack's 3s ack window", elapsed)
	}
}
