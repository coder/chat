package slackapi

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/coder/chat"
	"github.com/coder/chat/internal/ratelimit"
)

// RetryPolicy bounds outbound Slack rate-limit retry/backoff (ADR 0005). Retry is
// bounded by attempt count and cumulative elapsed backoff, honors Slack's
// Retry-After header, and never sleeps past the caller's context deadline so
// in-line synchronous retry cannot outlive the platform ack window (Slack's
// 3-second budget). The zero value applies a conservative default; MaxAttempts: 1
// disables retry for callers that want raw single-shot behavior. A Retry-After
// longer than MaxDelay is capped at MaxDelay, so the retry comes earlier than
// Slack asked. Callers that must honor long Retry-After values should raise
// MaxDelay and MaxElapsed.
type RetryPolicy struct {
	// MaxAttempts is the total number of attempts including the first. Zero applies
	// a conservative default; 1 disables retry.
	MaxAttempts int
	// MaxElapsed caps the cumulative backoff sleep across attempts.
	MaxElapsed time.Duration
	// BaseDelay is the first backoff delay when Slack sends no Retry-After; it
	// doubles each attempt up to MaxDelay.
	BaseDelay time.Duration
	// MaxDelay caps a single backoff delay, including a delay that Retry-After
	// asks for.
	MaxDelay time.Duration
}

// Default retry bounds are deliberately conservative so the worst-case backoff
// (about 200ms + 400ms = 600ms over two retries, capped at MaxElapsed) stays well
// under Slack's 3-second ack window. A longer Retry-After is capped at MaxDelay.
// When the next delay would pass MaxElapsed or the context deadline, retry stops
// with a typed RateLimited rather than sleeping past the deadline.
func (p RetryPolicy) withDefaults() RetryPolicy {
	out := p
	if out.MaxAttempts <= 0 {
		out.MaxAttempts = 3
	}
	if out.MaxElapsed <= 0 {
		out.MaxElapsed = 2 * time.Second
	}
	if out.BaseDelay <= 0 {
		out.BaseDelay = 200 * time.Millisecond
	}
	if out.MaxDelay <= 0 {
		out.MaxDelay = time.Second
	}
	return out
}

// RateLimited is returned when bounded retry is exhausted against a Slack rate
// limit, or when the next backoff delay would pass MaxElapsed or the caller's
// deadline. It carries the adapter name, the last Retry-After, the attempt
// count, and the raw platform response as a Platform Escape Hatch. Transport
// errors are not wrapped in RateLimited; they return directly.
type RateLimited struct {
	// Adapter is always "slack".
	Adapter string
	// RetryAfter is the last Retry-After that Slack sent, or zero.
	RetryAfter time.Duration
	// Attempts is the number of attempts made, including the first.
	Attempts int
	// Raw is the last response body as a json.RawMessage.
	Raw any
	// Err is an optional underlying error that Error and Unwrap expose. The
	// Client never sets it.
	Err error
}

// Error describes the attempt count, the last Retry-After, and Err.
func (e *RateLimited) Error() string {
	msg := fmt.Sprintf("slack: rate limited after %d attempts", e.Attempts)
	if e.RetryAfter > 0 {
		msg += fmt.Sprintf(" (retry after %s)", e.RetryAfter)
	}
	if e.Err != nil {
		msg += ": " + e.Err.Error()
	}
	return msg
}

// Unwrap returns Err.
func (e *RateLimited) Unwrap() error { return e.Err }

var _ error = (*RateLimited)(nil)

// doWithRetry sends the request, retrying on Slack throttling (HTTP 429, or the
// ratelimited or rate_limited API error on a 2xx envelope) within the bounded
// RetryPolicy. Retry-After is honored and clamped to the policy bounds; the loop
// never sleeps past the caller's context deadline. It returns the status and body
// of the first response that is not throttling; the caller checks both.
// Transport errors return immediately and are never retried. Every attempt is
// reported through Runtime Observation as an ObsAdapterCall, and every throttle
// as an ObsRateLimit, feeding the ADR 0010 Observation Hook; each throttle is
// also logged as a structured slog record.
func (c *Client) doWithRetry(ctx context.Context, method string, req *http.Request) (int, []byte, error) {
	policy := c.retryPolicy
	bodyBytes, err := ratelimit.BufferRequestBody(req)
	if err != nil {
		return 0, nil, err
	}

	var elapsed time.Duration
	for attempt := 1; ; attempt++ {
		c.observer.Event(ctx, chat.ObsAdapterCall, chat.AdapterAttr(adapterName))

		attemptReq := req
		if attempt > 1 {
			attemptReq = ratelimit.CloneRequest(req, bodyBytes)
		}
		status, retryAfterHeader, payload, doErr := c.sendOnce(attemptReq)
		if doErr != nil {
			return 0, nil, fmt.Errorf("slack: %s request: %w", method, doErr)
		}

		retryAfter, throttled := slackThrottle(status, retryAfterHeader, payload)
		if !throttled {
			return status, payload, nil
		}

		c.observer.Event(ctx, chat.ObsRateLimit, chat.AdapterAttr(adapterName))

		rateLimited := &RateLimited{Adapter: adapterName, RetryAfter: retryAfter, Attempts: attempt, Raw: json.RawMessage(payload)}
		if attempt >= policy.MaxAttempts {
			c.logRetry(method, attempt, retryAfter, "exhausted")
			return 0, nil, rateLimited
		}

		delay := ratelimit.BackoffDelay(policy.BaseDelay, policy.MaxDelay, attempt, retryAfter)
		if elapsed+delay > policy.MaxElapsed {
			c.logRetry(method, attempt, retryAfter, "ceiling")
			return 0, nil, rateLimited
		}
		// The single load-bearing invariant: never sleep past the caller's context
		// deadline. A delay that would miss the ack returns a typed RateLimited
		// instead of blowing the window.
		if deadline, ok := ctx.Deadline(); ok && time.Now().Add(delay).After(deadline) {
			c.logRetry(method, attempt, retryAfter, "deadline")
			return 0, nil, rateLimited
		}
		c.logRetry(method, attempt, retryAfter, "retry")
		if err := ratelimit.SleepCtx(ctx, delay); err != nil {
			return 0, nil, err
		}
		elapsed += delay
	}
}

func (c *Client) logRetry(method string, attempt int, retryAfter time.Duration, outcome string) {
	c.logger.Warn("slack rate limited",
		"adapter", adapterName, "method", method, "attempt", attempt, "retry_after", retryAfter, "outcome", outcome)
}

func (c *Client) sendOnce(req *http.Request) (int, string, []byte, error) {
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return 0, "", nil, err
	}
	defer resp.Body.Close()
	payload, err := io.ReadAll(resp.Body)
	if err != nil {
		return 0, "", nil, err
	}
	return resp.StatusCode, resp.Header.Get("Retry-After"), payload, nil
}

// slackThrottle reports whether the response indicates Slack throttling, and the
// suggested Retry-After when present. Throttling is an HTTP 429 (Retry-After
// header, seconds), or a 2xx envelope with the ratelimited error or the
// rate_limited error (chat.postMessage returns it for the workspace message cap).
// Non-throttling responses (any other status, or a 2xx with a different error)
// report false so they return immediately without retry.
func slackThrottle(status int, retryAfterHeader string, payload []byte) (time.Duration, bool) {
	if status == http.StatusTooManyRequests {
		return parseRetryAfterHeader(retryAfterHeader), true
	}
	if !successStatus(status) {
		return 0, false
	}
	var envelope struct {
		OK    bool   `json:"ok"`
		Error string `json:"error"`
	}
	if err := json.Unmarshal(payload, &envelope); err != nil {
		return 0, false
	}
	if !envelope.OK && (strings.EqualFold(envelope.Error, "ratelimited") || strings.EqualFold(envelope.Error, "rate_limited")) {
		return parseRetryAfterHeader(retryAfterHeader), true
	}
	return 0, false
}

// parseRetryAfterHeader parses Slack's Retry-After header, an integer number of
// seconds. A missing or unparseable header yields zero, and computed backoff takes
// over.
func parseRetryAfterHeader(value string) time.Duration {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0
	}
	if seconds, err := strconv.ParseFloat(value, 64); err == nil && seconds > 0 {
		return time.Duration(seconds * float64(time.Second))
	}
	return 0
}
