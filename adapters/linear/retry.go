package linear

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// RetryPolicy bounds outbound Linear rate-limit retry/backoff (ADR 0005). Retry
// is bounded by attempt count and cumulative elapsed backoff, honors Linear's
// Retry-After, and never sleeps past the request context deadline so it cannot
// violate the Agent Session Timing Contract first-thought window (ADR 0008).
type RetryPolicy struct {
	// MaxAttempts is the total number of attempts including the first. Zero applies
	// a conservative default; 1 disables retry.
	MaxAttempts int
	// MaxElapsed caps the cumulative backoff sleep across attempts.
	MaxElapsed time.Duration
	// BaseDelay is the first backoff delay; it doubles each attempt up to MaxDelay.
	BaseDelay time.Duration
	// MaxDelay caps a single backoff delay.
	MaxDelay time.Duration
}

// Default retry bounds are deliberately conservative so the worst-case backoff
// (about 200ms + 400ms = 600ms over two retries, capped at MaxElapsed) stays
// well under the ~10s first-thought window.
func (p RetryPolicy) withDefaults() RetryPolicy {
	out := p
	if out.MaxAttempts <= 0 {
		out.MaxAttempts = 3
	}
	if out.MaxElapsed <= 0 {
		out.MaxElapsed = 3 * time.Second
	}
	if out.BaseDelay <= 0 {
		out.BaseDelay = 200 * time.Millisecond
	}
	if out.MaxDelay <= 0 {
		out.MaxDelay = time.Second
	}
	return out
}

// RateLimited is returned when bounded retry is exhausted against a Linear rate
// limit. It is unwrappable to any underlying transport error.
type RateLimited struct {
	Adapter    string
	RetryAfter time.Duration
	Attempts   int
	Raw        any
	Err        error
}

func (e *RateLimited) Error() string {
	msg := fmt.Sprintf("linear: rate limited after %d attempts", e.Attempts)
	if e.RetryAfter > 0 {
		msg += fmt.Sprintf(" (retry after %s)", e.RetryAfter)
	}
	if e.Err != nil {
		msg += ": " + e.Err.Error()
	}
	return msg
}

func (e *RateLimited) Unwrap() error { return e.Err }

// doJSONWithRetry sends the request, retrying on Linear throttling (HTTP 429 or
// a GraphQL RATELIMITED error) within the bounded RetryPolicy. Retry-After is
// honored and clamped to the policy bounds; the loop never sleeps past the
// request context deadline. Non-throttling errors return immediately. Every
// attempt and the final exhaustion are reported as structured slog records, the
// Runtime Observation surface available today.
func (a *Adapter) doJSONWithRetry(req *http.Request, dest any) error {
	policy := a.retryPolicy
	bodyBytes, err := bufferRequestBody(req)
	if err != nil {
		return err
	}
	ctx := req.Context()

	var elapsed time.Duration
	for attempt := 1; ; attempt++ {
		attemptReq := req
		if attempt > 1 {
			attemptReq = cloneRequest(req, bodyBytes)
		}
		status, payload, doErr := a.sendOnce(attemptReq)
		if doErr != nil {
			return doErr
		}

		retryAfter, throttled := rateLimitedResponse(status, payload)
		if !throttled {
			if status < 200 || status > 299 {
				return fmt.Errorf("status %d", status)
			}
			if dest == nil {
				return nil
			}
			if err := json.Unmarshal(payload, dest); err != nil {
				return err
			}
			return nil
		}

		if attempt >= policy.MaxAttempts {
			a.logger.Warn("linear rate limit retry exhausted",
				"adapter", adapterName, "attempt", attempt, "retry_after", retryAfter, "outcome", "exhausted")
			return &RateLimited{Adapter: adapterName, RetryAfter: retryAfter, Attempts: attempt, Raw: json.RawMessage(payload)}
		}

		delay := backoffDelay(policy, attempt, retryAfter)
		if elapsed+delay > policy.MaxElapsed {
			a.logger.Warn("linear rate limit backoff ceiling reached",
				"adapter", adapterName, "attempt", attempt, "retry_after", retryAfter, "outcome", "exhausted")
			return &RateLimited{Adapter: adapterName, RetryAfter: retryAfter, Attempts: attempt, Raw: json.RawMessage(payload)}
		}
		a.logger.Warn("linear rate limited, retrying",
			"adapter", adapterName, "attempt", attempt, "retry_after", retryAfter, "delay", delay, "outcome", "retry")
		if err := sleepCtx(ctx, delay); err != nil {
			return err
		}
		elapsed += delay
	}
}

func (a *Adapter) sendOnce(req *http.Request) (int, []byte, error) {
	resp, err := a.client.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	payload, err := io.ReadAll(resp.Body)
	if err != nil {
		return 0, nil, err
	}
	return resp.StatusCode, payload, nil
}

func bufferRequestBody(req *http.Request) ([]byte, error) {
	if req.Body == nil {
		return nil, nil
	}
	body, err := io.ReadAll(req.Body)
	if err != nil {
		return nil, err
	}
	_ = req.Body.Close()
	req.Body = io.NopCloser(bytes.NewReader(body))
	req.ContentLength = int64(len(body))
	return body, nil
}

func cloneRequest(req *http.Request, body []byte) *http.Request {
	clone := req.Clone(req.Context())
	if body != nil {
		clone.Body = io.NopCloser(bytes.NewReader(body))
		clone.ContentLength = int64(len(body))
	}
	return clone
}

// rateLimitedResponse reports whether the response indicates Linear throttling,
// either an HTTP 429 or a GraphQL RATELIMITED error, and the suggested
// Retry-After when present.
func rateLimitedResponse(status int, payload []byte) (time.Duration, bool) {
	if status == http.StatusTooManyRequests {
		return parseRetryAfterPayload(payload), true
	}
	if status < 200 || status > 299 {
		return 0, false
	}
	var envelope struct {
		Errors []struct {
			Message    string `json:"message"`
			Extensions struct {
				Code string `json:"code"`
				Type string `json:"type"`
			} `json:"extensions"`
		} `json:"errors"`
	}
	if err := json.Unmarshal(payload, &envelope); err != nil {
		return 0, false
	}
	for _, e := range envelope.Errors {
		code := strings.ToUpper(e.Extensions.Code)
		typ := strings.ToUpper(e.Extensions.Type)
		if code == "RATELIMITED" || typ == "RATELIMITED" || strings.Contains(strings.ToLower(e.Message), "rate limit") {
			return parseRetryAfterPayload(payload), true
		}
	}
	return 0, false
}

func parseRetryAfterPayload(payload []byte) time.Duration {
	var envelope struct {
		RetryAfter json.RawMessage `json:"retryAfter"`
	}
	if err := json.Unmarshal(payload, &envelope); err != nil {
		return 0
	}
	return parseRetryAfter(string(envelope.RetryAfter))
}

func parseRetryAfter(value string) time.Duration {
	value = strings.TrimSpace(strings.Trim(value, `"`))
	if value == "" {
		return 0
	}
	if seconds, err := strconv.ParseFloat(value, 64); err == nil && seconds > 0 {
		return time.Duration(seconds * float64(time.Second))
	}
	return 0
}

func backoffDelay(policy RetryPolicy, attempt int, retryAfter time.Duration) time.Duration {
	delay := policy.BaseDelay << (attempt - 1)
	if delay <= 0 || delay > policy.MaxDelay {
		delay = policy.MaxDelay
	}
	if retryAfter > delay {
		delay = retryAfter
	}
	if delay > policy.MaxDelay {
		delay = policy.MaxDelay
	}
	return delay
}

// sleepCtx sleeps for d but returns the context error if it ends first, so retry
// never outlasts the request context (and thus never violates the first-thought
// window).
func sleepCtx(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

var _ error = (*RateLimited)(nil)
