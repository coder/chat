package linear

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

var _ error = (*RateLimited)(nil)

// noopObserver is the adapter's default Observer when none is configured: it
// records nothing so an unconfigured adapter keeps structured-slog-only behavior.
type noopObserver struct{}

func (noopObserver) Event(context.Context, chat.ObservationName, ...chat.Attr) {}

func (noopObserver) Dispatch(ctx context.Context, _ ...chat.Attr) (context.Context, chat.DispatchSpan) {
	return ctx, noopSpan{}
}

type noopSpan struct{}

func (noopSpan) End(chat.DispatchOutcome, ...chat.Attr) {}

// doJSONWithRetry sends the request, retrying on Linear throttling (HTTP 429 or
// a GraphQL RATELIMITED error) within the bounded RetryPolicy. Retry-After is
// honored and clamped to the policy bounds; the loop never sleeps past the
// request context deadline. Non-throttling errors return immediately. Every
// attempt emits an ObsAdapterCall and every throttle an ObsRateLimit through the
// configured Observer (ADR 0010); exhaustion is additionally logged as a
// structured slog record.
func (a *Adapter) doJSONWithRetry(req *http.Request, dest any) error {
	policy := a.retryPolicy
	bodyBytes, err := ratelimit.BufferRequestBody(req)
	if err != nil {
		return err
	}
	ctx := req.Context()

	var elapsed time.Duration
	for attempt := 1; ; attempt++ {
		a.observer.Event(ctx, chat.ObsAdapterCall, chat.AdapterAttr(adapterName))

		attemptReq := req
		if attempt > 1 {
			attemptReq = ratelimit.CloneRequest(req, bodyBytes)
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

		a.observer.Event(ctx, chat.ObsRateLimit, chat.AdapterAttr(adapterName))

		rateLimited := &RateLimited{Adapter: adapterName, RetryAfter: retryAfter, Attempts: attempt, Raw: json.RawMessage(payload)}
		if attempt >= policy.MaxAttempts {
			a.logRetry(attempt, retryAfter, "exhausted")
			return rateLimited
		}

		delay := ratelimit.BackoffDelay(policy.BaseDelay, policy.MaxDelay, attempt, retryAfter)
		if elapsed+delay > policy.MaxElapsed {
			a.logRetry(attempt, retryAfter, "ceiling")
			return rateLimited
		}
		// The single load-bearing invariant: never sleep past the request context
		// deadline. A Retry-After that would push the first Agent Activity Thought
		// past the first-thought window exhausts to a typed RateLimited instead.
		if deadline, ok := ctx.Deadline(); ok && a.now().Add(delay).After(deadline) {
			a.logRetry(attempt, retryAfter, "deadline")
			return rateLimited
		}
		a.logRetry(attempt, retryAfter, "retry")
		if err := ratelimit.SleepCtx(ctx, delay); err != nil {
			return err
		}
		elapsed += delay
	}
}

func (a *Adapter) logRetry(attempt int, retryAfter time.Duration, outcome string) {
	a.logger.Warn("linear rate limited",
		"adapter", adapterName, "attempt", attempt, "retry_after", retryAfter, "outcome", outcome)
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
