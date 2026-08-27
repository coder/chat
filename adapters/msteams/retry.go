package msteams

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

// RetryPolicy bounds outbound Connector rate-limit retry/backoff (ADR 0005). Retry
// is bounded by attempt count, cumulative elapsed backoff, and the caller's context
// deadline, honors a Connector Retry-After, and never sleeps past the deadline so
// in-line synchronous retry stays inside the Teams turn budget (~10-15s) rather than
// blowing it. The zero value applies a conservative default; MaxAttempts: 1 disables
// retry. It is per-adapter platform config, never Runtime Options.
type RetryPolicy struct {
	MaxAttempts int
	MaxElapsed  time.Duration
	BaseDelay   time.Duration
	MaxDelay    time.Duration
}

// withDefaults keeps the worst-case backoff well under the Teams turn budget so a
// reply or proactive post cannot retry its way past the window; a longer Retry-After
// exhausts to a typed RateLimited instead of sleeping past the deadline.
func (p RetryPolicy) withDefaults() RetryPolicy {
	out := p
	if out.MaxAttempts <= 0 {
		out.MaxAttempts = 3
	}
	if out.MaxElapsed <= 0 {
		out.MaxElapsed = 4 * time.Second
	}
	if out.BaseDelay <= 0 {
		out.BaseDelay = 500 * time.Millisecond
	}
	if out.MaxDelay <= 0 {
		out.MaxDelay = 2 * time.Second
	}
	return out
}

// RateLimited is returned when bounded retry is exhausted against a Connector 429,
// or when a single Retry-After would exceed the caller's deadline. It carries the
// adapter name, the last Retry-After, the attempt count, and the raw platform
// response as a Platform Escape Hatch, and is unwrappable to any underlying error.
type RateLimited struct {
	Adapter    string
	RetryAfter time.Duration
	Attempts   int
	Raw        any
	Err        error
}

func (e *RateLimited) Error() string {
	msg := fmt.Sprintf("msteams: rate limited after %d attempts", e.Attempts)
	if e.RetryAfter > 0 {
		msg += fmt.Sprintf(" (retry after %s)", e.RetryAfter)
	}
	if e.Err != nil {
		msg += ": " + e.Err.Error()
	}
	return msg
}

func (e *RateLimited) Unwrap() error { return e.Err }

// doWithRetry sends the request, retrying only on a Connector HTTP 429 within the
// bounded RetryPolicy and the caller's context deadline. Any other non-2xx is a
// ConnectorError returned immediately (never retried); a 2xx decodes into dest.
// Every attempt emits ObsAdapterCall and every throttle emits ObsRateLimit through
// the configured Observer (ADR 0010); exhaustion is additionally logged.
func (a *Adapter) doWithRetry(ctx context.Context, label string, req *http.Request, dest any) error {
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
			return fmt.Errorf("msteams: connector request: %w", doErr)
		}

		if status != http.StatusTooManyRequests {
			if status < 200 || status > 299 {
				return connectorErrorFor(status, payload)
			}
			if dest == nil || len(payload) == 0 {
				return nil
			}
			if err := json.Unmarshal(payload, dest); err != nil {
				return fmt.Errorf("msteams: decode connector response: %w", err)
			}
			return nil
		}

		a.observer.Event(ctx, chat.ObsRateLimit, chat.AdapterAttr(adapterName))

		retryAfter := parseRetryAfter(retryAfterHeader)
		rateLimited := &RateLimited{Adapter: adapterName, RetryAfter: retryAfter, Attempts: attempt, Raw: json.RawMessage(payload)}
		if attempt >= policy.MaxAttempts {
			a.logRetry(label, attempt, retryAfter, "exhausted")
			return rateLimited
		}

		delay := ratelimit.BackoffDelay(policy.BaseDelay, policy.MaxDelay, attempt, retryAfter)
		if elapsed+delay > policy.MaxElapsed {
			a.logRetry(label, attempt, retryAfter, "ceiling")
			return rateLimited
		}
		// The load-bearing invariant: never sleep past the caller's context deadline,
		// so retry under synchronous dispatch stays inside the Teams turn budget.
		if deadline, ok := ctx.Deadline(); ok && a.now().Add(delay).After(deadline) {
			a.logRetry(label, attempt, retryAfter, "deadline")
			return rateLimited
		}
		a.logRetry(label, attempt, retryAfter, "retry")
		if err := ratelimit.SleepCtx(ctx, delay); err != nil {
			return err
		}
		elapsed += delay
	}
}

func (a *Adapter) sendOnce(req *http.Request) (int, string, []byte, error) {
	resp, err := a.client.Do(req)
	if err != nil {
		return 0, "", nil, err
	}
	defer resp.Body.Close()
	payload, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return 0, "", nil, err
	}
	return resp.StatusCode, resp.Header.Get("Retry-After"), payload, nil
}

func (a *Adapter) logRetry(label string, attempt int, retryAfter time.Duration, outcome string) {
	a.logger.Warn("msteams rate limited",
		"adapter", adapterName, "endpoint", label, "attempt", attempt, "retry_after", retryAfter, "outcome", outcome)
}

// parseRetryAfter parses the Connector Retry-After header as an integer number of
// seconds. A missing or unparseable header yields zero, and computed backoff takes
// over. (HTTP-date form is not emitted by the Connector for throttling; left for the
// live spike to confirm.)
func parseRetryAfter(value string) time.Duration {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0
	}
	if seconds, err := strconv.ParseFloat(value, 64); err == nil && seconds > 0 {
		return time.Duration(seconds * float64(time.Second))
	}
	return 0
}
