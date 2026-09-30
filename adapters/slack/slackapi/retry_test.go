package slackapi_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/chat"
	"github.com/coder/chat/adapters/slack/slackapi"
	"github.com/coder/chat/adapters/slack/slackapi/slackapitest"
)

// coreThrottleThenOK returns a Handle function that sends throttle for the first
// n calls and {"ok":true} after that.
func coreThrottleThenOK(n int32, throttle any) func(slackapitest.Call) any {
	var calls atomic.Int32
	return func(slackapitest.Call) any {
		if calls.Add(1) <= n {
			return throttle
		}
		return json.RawMessage(`{"ok":true,"ts":"1.2"}`)
	}
}

func TestCallRetriesThrottling(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		throttle any
	}{
		{
			name: "429 with Retry-After",
			throttle: slackapitest.Response{
				StatusCode: http.StatusTooManyRequests,
				Header:     http.Header{"Retry-After": {"1"}},
			},
		},
		{name: "ratelimited", throttle: json.RawMessage(`{"ok":false,"error":"ratelimited"}`)},
		{name: "rate_limited", throttle: json.RawMessage(`{"ok":false,"error":"rate_limited"}`)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			srv := slackapitest.NewServer(t)
			srv.Handle("chat.postMessage", coreThrottleThenOK(1, tt.throttle))
			client := coreNewClient(srv, slackapi.Options{RetryPolicy: coreFastPolicy(3)})

			var dest struct {
				TS string `json:"ts"`
			}
			if err := client.Call(t.Context(), "chat.postMessage", map[string]string{"channel": "C1"}, &dest); err != nil {
				t.Fatalf("Call: %v", err)
			}
			if dest.TS != "1.2" {
				t.Fatalf("ts = %q, want 1.2", dest.TS)
			}
			calls := srv.Calls("chat.postMessage")
			if len(calls) != 2 {
				t.Fatalf("calls = %d, want 2 (one throttled, one retry)", len(calls))
			}
			if !bytes.Equal(calls[0].Body, calls[1].Body) || string(calls[1].Body) != `{"channel":"C1"}` {
				t.Fatalf("retry body = %s, want the first body %s", calls[1].Body, calls[0].Body)
			}
			if got := calls[1].Header.Get("Authorization"); got != "Bearer xoxb-core" {
				t.Fatalf("retry Authorization = %q", got)
			}
		})
	}
}

func TestCallRetryExhausted(t *testing.T) {
	t.Parallel()

	srv := slackapitest.NewServer(t)
	srv.Respond("chat.postMessage", slackapitest.Response{
		StatusCode: http.StatusTooManyRequests,
		Header:     http.Header{"Retry-After": {"1"}},
		Body:       json.RawMessage(`{"ok":false,"error":"ratelimited"}`),
	})
	client := coreNewClient(srv, slackapi.Options{RetryPolicy: coreFastPolicy(3)})

	err := client.Call(t.Context(), "chat.postMessage", map[string]string{}, nil)
	limited, ok := errors.AsType[*slackapi.RateLimited](err)
	if !ok {
		t.Fatalf("err = %v, want *slackapi.RateLimited", err)
	}
	if limited.Adapter != "slack" || limited.Attempts != 3 || limited.RetryAfter != time.Second {
		t.Fatalf("RateLimited = %+v, want adapter slack, 3 attempts, retry after 1s", limited)
	}
	raw, ok := limited.Raw.(json.RawMessage)
	if !ok || string(raw) != `{"ok":false,"error":"ratelimited"}` {
		t.Fatalf("Raw = %#v, want the last response body", limited.Raw)
	}
	if got, want := err.Error(), "slack: rate limited after 3 attempts (retry after 1s)"; got != want {
		t.Fatalf("Error() = %q, want %q", got, want)
	}
	if got := len(srv.Calls("chat.postMessage")); got != 3 {
		t.Fatalf("calls = %d, want 3", got)
	}
}

func TestCallRetrySingleAttempt(t *testing.T) {
	t.Parallel()

	srv := slackapitest.NewServer(t)
	srv.Respond("chat.postMessage", json.RawMessage(`{"ok":false,"error":"rate_limited"}`))
	client := coreNewClient(srv, slackapi.Options{RetryPolicy: slackapi.RetryPolicy{MaxAttempts: 1}})

	err := client.Call(t.Context(), "chat.postMessage", map[string]string{}, nil)
	if limited, ok := errors.AsType[*slackapi.RateLimited](err); !ok || limited.Attempts != 1 {
		t.Fatalf("err = %v, want *slackapi.RateLimited after 1 attempt", err)
	}
	if got := len(srv.Calls("chat.postMessage")); got != 1 {
		t.Fatalf("calls = %d, want 1", got)
	}
}

func TestCallRetryStopsBeforeDeadline(t *testing.T) {
	t.Parallel()

	srv := slackapitest.NewServer(t)
	srv.Respond("chat.postMessage", slackapitest.Response{
		StatusCode: http.StatusTooManyRequests,
		Header:     http.Header{"Retry-After": {"3600"}},
	})
	client := coreNewClient(srv, slackapi.Options{RetryPolicy: slackapi.RetryPolicy{
		MaxAttempts: 5,
		MaxElapsed:  2 * time.Hour,
		BaseDelay:   time.Hour,
		MaxDelay:    time.Hour,
	}})

	// A one-hour Retry-After overshoots any deadline far below an hour, so the
	// client returns without sleeping. The 10s timeout only leaves headroom for
	// the localhost round trip under -race load.
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()

	err := client.Call(ctx, "chat.postMessage", map[string]string{}, nil)
	limited, ok := errors.AsType[*slackapi.RateLimited](err)
	if !ok {
		t.Fatalf("err = %v, want *slackapi.RateLimited (bounded by the deadline, not slept off)", err)
	}
	if limited.Attempts != 1 || limited.RetryAfter != time.Hour {
		t.Fatalf("RateLimited = %+v, want 1 attempt and retry after 1h", limited)
	}
}

func TestCallRetryStopsAtElapsedCeiling(t *testing.T) {
	t.Parallel()

	srv := slackapitest.NewServer(t)
	srv.Respond("chat.postMessage", json.RawMessage(`{"ok":false,"error":"ratelimited"}`))
	client := coreNewClient(srv, slackapi.Options{RetryPolicy: slackapi.RetryPolicy{
		MaxAttempts: 5,
		MaxElapsed:  time.Millisecond,
		BaseDelay:   5 * time.Millisecond,
		MaxDelay:    5 * time.Millisecond,
	}})

	err := client.Call(t.Context(), "chat.postMessage", map[string]string{}, nil)
	if limited, ok := errors.AsType[*slackapi.RateLimited](err); !ok || limited.Attempts != 1 {
		t.Fatalf("err = %v, want *slackapi.RateLimited after 1 attempt", err)
	}
}

func TestCallRetryCanceledContext(t *testing.T) {
	t.Parallel()

	srv := slackapitest.NewServer(t)
	ctx, cancel := context.WithCancel(t.Context())
	srv.Handle("chat.postMessage", func(slackapitest.Call) any {
		cancel()
		return json.RawMessage(`{"ok":false,"error":"ratelimited"}`)
	})
	client := coreNewClient(srv, slackapi.Options{RetryPolicy: slackapi.RetryPolicy{
		MaxAttempts: 3,
		MaxElapsed:  time.Hour,
		BaseDelay:   time.Hour,
		MaxDelay:    time.Hour,
	}})

	err := client.Call(ctx, "chat.postMessage", map[string]string{}, nil)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
}

func TestCallDoesNotRetryOtherErrors(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		resp any
	}{
		{name: "api error", resp: json.RawMessage(`{"ok":false,"error":"channel_not_found"}`)},
		{name: "unauthorized", resp: slackapitest.Response{StatusCode: http.StatusUnauthorized, Body: json.RawMessage(`{"ok":false,"error":"ratelimited"}`)}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			srv := slackapitest.NewServer(t)
			srv.Respond("chat.postMessage", tt.resp)
			obs := &coreObserver{}
			client := coreNewClient(srv, slackapi.Options{RetryPolicy: coreFastPolicy(3), Observer: obs})

			err := client.Call(t.Context(), "chat.postMessage", map[string]string{}, nil)
			if _, ok := errors.AsType[*slackapi.APIError](err); !ok {
				t.Fatalf("err = %v, want *slackapi.APIError", err)
			}
			if got := len(srv.Calls("chat.postMessage")); got != 1 {
				t.Fatalf("calls = %d, want 1", got)
			}
			if got := obs.count(chat.ObsRateLimit); got != 0 {
				t.Fatalf("ObsRateLimit = %d, want 0", got)
			}
		})
	}
}

func TestCallRetryObservationsAndLog(t *testing.T) {
	t.Parallel()

	srv := slackapitest.NewServer(t)
	srv.Handle("chat.postMessage", coreThrottleThenOK(1, json.RawMessage(`{"ok":false,"error":"ratelimited"}`)))
	obs := &coreObserver{}
	var logs bytes.Buffer
	client := coreNewClient(srv, slackapi.Options{
		RetryPolicy: coreFastPolicy(3),
		Observer:    obs,
		Logger:      slog.New(slog.NewTextHandler(&logs, nil)),
	})

	if err := client.Call(t.Context(), "chat.postMessage", map[string]string{}, nil); err != nil {
		t.Fatalf("Call: %v", err)
	}

	var names []chat.ObservationName
	for _, ev := range obs.all() {
		names = append(names, ev.name)
		if !slices.Equal(ev.attrs, []chat.Attr{chat.AdapterAttr("slack")}) {
			t.Fatalf("%s attrs = %v, want adapter slack", ev.name, ev.attrs)
		}
	}
	want := []chat.ObservationName{chat.ObsAdapterCall, chat.ObsRateLimit, chat.ObsAdapterCall}
	if !slices.Equal(names, want) {
		t.Fatalf("observations = %v, want %v", names, want)
	}

	line := logs.String()
	for _, part := range []string{`msg="slack rate limited"`, "adapter=slack", "method=chat.postMessage", "attempt=1", "outcome=retry"} {
		if !strings.Contains(line, part) {
			t.Fatalf("log = %q, want it to contain %q", line, part)
		}
	}
}
