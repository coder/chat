// Package ratelimit holds the transport-level helpers shared by the per-adapter
// rate-limit retry loops (ADR 0005). The RetryPolicy and RateLimited types stay
// per-adapter and public, since throttling detection is platform-specific; only
// these mechanics (request buffering, backoff math, deadline-bounded sleep) are
// identical across adapters and live here to avoid divergent copies.
package ratelimit

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"time"
)

// BufferRequestBody reads the request body so it can be replayed on each retry,
// rewinding the original request to the buffered bytes. A nil body buffers to nil.
func BufferRequestBody(req *http.Request) ([]byte, error) {
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

// CloneRequest returns a copy of req with body rewound to the buffered bytes, for
// a retry attempt after the first.
func CloneRequest(req *http.Request, body []byte) *http.Request {
	clone := req.Clone(req.Context())
	if body != nil {
		clone.Body = io.NopCloser(bytes.NewReader(body))
		clone.ContentLength = int64(len(body))
	}
	return clone
}

// BackoffDelay computes the next backoff: exponential from base (doubling per
// attempt) capped at maxDelay, overridden by retryAfter when the platform
// signals a longer wait, then clamped to maxDelay.
func BackoffDelay(base, maxDelay time.Duration, attempt int, retryAfter time.Duration) time.Duration {
	delay := base << (attempt - 1)
	if delay <= 0 || delay > maxDelay {
		// Shift overflow (or wrap to non-positive) also lands on the cap.
		delay = maxDelay
	}
	return min(max(delay, retryAfter), maxDelay)
}

// SleepCtx sleeps for d but returns the context error if the context ends first,
// so retry never outlasts the caller's context (and thus never blows the platform
// ack window).
func SleepCtx(ctx context.Context, d time.Duration) error {
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
