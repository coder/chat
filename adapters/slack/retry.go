package slack

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
// disables retry for callers that want raw single-shot behavior.
type RetryPolicy struct {
	// MaxAttempts is the total number of attempts including the first. Zero applies
	// a conservative default; 1 disables retry.
	MaxAttempts int
	// MaxElapsed caps the cumulative backoff sleep across attempts.
	MaxElapsed time.Duration
	// BaseDelay is the first backoff delay when Slack sends no Retry-After; it
	// doubles each attempt up to MaxDelay.
	BaseDelay time.Duration
	// MaxDelay caps a single backoff delay.
	MaxDelay time.Duration
}

// Default retry bounds are deliberately conservative so the worst-case backoff
// (about 200ms + 400ms = 600ms over two retries, capped at MaxElapsed) stays well
// under Slack's 3-second ack window. A longer Retry-After exhausts to a typed
// RateLimited rather than sleeping past the deadline.
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
// limit, or when a single Retry-After would exceed the caller's deadline. It
// carries the adapter name, the last Retry-After, the attempt count, and the raw
// platform response as a Platform Escape Hatch. It is unwrappable to any
// underlying transport error.
type RateLimited struct {
	Adapter    string
	RetryAfter time.Duration
	Attempts   int
	Raw        any
	Err        error
}

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

func (e *RateLimited) Unwrap() error { return e.Err }

var _ error = (*RateLimited)(nil)

// doWithRetry sends the request, retrying on Slack throttling (HTTP 429 or the
// ratelimited API error on a 200 envelope) within the bounded RetryPolicy.
// Retry-After is honored and clamped to the policy bounds; the loop never sleeps
// past the caller's context deadline. Non-throttling errors (auth, validation,
// not-found, transport) return immediately and are never retried. Every attempt
// is reported through Runtime Observation as an ObsAdapterCall, and every
// throttle as an ObsRateLimit, feeding the ADR 0010 Observation Hook; exhaustion
// is additionally logged as a structured slog record.
func (a *Adapter) doWithRetry(ctx context.Context, method string, req *http.Request, dest any) error {
	policy := a.retryPolicy
	bodyBytes, err := ratelimit.BufferRequestBody(req)
	if err != nil {
		return err
	}

	var elapsed time.Duration
	for attempt := 1; ; attempt++ {
		a.observer.Event(ctx, chat.ObsAdapterCall, chat.AdapterAttr(adapterName))

		attemptReq := req
		if attempt > 1 {
			attemptReq = ratelimit.CloneRequest(req, bodyBytes)
		}
		status, retryAfterHeader, payload, doErr := a.sendOnce(attemptReq)
		if doErr != nil {
			return fmt.Errorf("slack: %s request: %w", method, doErr)
		}

		retryAfter, throttled := slackThrottle(status, retryAfterHeader, payload)
		if !throttled {
			if status < 200 || status > 299 {
				return fmt.Errorf("slack: %s status %d", method, status)
			}
			if dest == nil {
				return nil
			}
			if err := json.Unmarshal(payload, dest); err != nil {
				return fmt.Errorf("slack: decode %s response: %w", method, err)
			}
			return nil
		}

		a.observer.Event(ctx, chat.ObsRateLimit, chat.AdapterAttr(adapterName))

		rateLimited := &RateLimited{Adapter: adapterName, RetryAfter: retryAfter, Attempts: attempt, Raw: json.RawMessage(payload)}
		if attempt >= policy.MaxAttempts {
			a.logRetry(method, attempt, retryAfter, "exhausted")
			return rateLimited
		}

		delay := ratelimit.BackoffDelay(policy.BaseDelay, policy.MaxDelay, attempt, retryAfter)
		if elapsed+delay > policy.MaxElapsed {
			a.logRetry(method, attempt, retryAfter, "ceiling")
			return rateLimited
		}
		// The single load-bearing invariant: never sleep past the caller's context
		// deadline. A Retry-After that would miss the ack exhausts to a typed
		// RateLimited instead of blowing the window.
		if deadline, ok := ctx.Deadline(); ok && a.now().Add(delay).After(deadline) {
			a.logRetry(method, attempt, retryAfter, "deadline")
			return rateLimited
		}
		a.logRetry(method, attempt, retryAfter, "retry")
		if err := ratelimit.SleepCtx(ctx, delay); err != nil {
			return err
		}
		elapsed += delay
	}
}

func (a *Adapter) logRetry(method string, attempt int, retryAfter time.Duration, outcome string) {
	a.logger.Warn("slack rate limited",
		"adapter", adapterName, "method", method, "attempt", attempt, "retry_after", retryAfter, "outcome", outcome)
}

func (a *Adapter) sendOnce(req *http.Request) (int, string, []byte, error) {
	resp, err := a.client.Do(req)
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

// slackThrottle reports whether the response indicates Slack throttling, either an
// HTTP 429 (Retry-After header, seconds) or the ratelimited API error on a 200
// envelope, and the suggested Retry-After when present. Non-throttling responses
// (any other status, or a 200 with a different error) report false so they return
// immediately without retry.
func slackThrottle(status int, retryAfterHeader string, payload []byte) (time.Duration, bool) {
	if status == http.StatusTooManyRequests {
		return parseRetryAfterHeader(retryAfterHeader), true
	}
	if status < 200 || status > 299 {
		return 0, false
	}
	var envelope struct {
		OK    bool   `json:"ok"`
		Error string `json:"error"`
	}
	if err := json.Unmarshal(payload, &envelope); err != nil {
		return 0, false
	}
	if !envelope.OK && strings.EqualFold(envelope.Error, "ratelimited") {
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
