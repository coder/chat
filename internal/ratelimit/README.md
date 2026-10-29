# ratelimit

## Overview

Package `ratelimit` holds the transport-level retry mechanics that the per-adapter
rate-limit retry loops share: request body buffering, backoff math, and a
context-bounded sleep. The package doc says the `RetryPolicy` and `RateLimited`
types stay per-adapter and public, because throttling detection is
platform-specific. Only the identical mechanics live here
(`[ratelimit.go:package ratelimit]`, see `docs/adr/0005-rate-limit-handling.md`).

These packages import it:

| Importer | Symbols used |
| --- | --- |
| `github.com/coder/chat/adapters/linear` (`retry.go`) | `BufferRequestBody`, `CloneRequest`, `BackoffDelay`, `SleepCtx` |
| `github.com/coder/chat/adapters/slack/slackapi` (`retry.go`) | `BufferRequestBody`, `CloneRequest`, `BackoffDelay`, `SleepCtx` |
| `github.com/coder/chat/adapters/slack/slackapi` (`socketmode.go`) | `BackoffDelay` |

## Interface

- `func BufferRequestBody(req *http.Request) ([]byte, error)`: reads the request body so each retry can replay it, and rewinds `req` to the buffered bytes. `[ratelimit.go:BufferRequestBody]`
- `func CloneRequest(req *http.Request, body []byte) *http.Request`: returns a copy of `req` whose body is rewound to `body`, for retry attempts after the first. `[ratelimit.go:CloneRequest]`
- `func BackoffDelay(base, maxDelay time.Duration, attempt int, retryAfter time.Duration) time.Duration`: computes the next backoff delay. `[ratelimit.go:BackoffDelay]`
- `func SleepCtx(ctx context.Context, d time.Duration) error`: sleeps for `d`, or returns the context error if `ctx` ends first. `[ratelimit.go:SleepCtx]`

## Invariants

- Keep platform-specific throttling detection (`RetryPolicy`, `RateLimited`) in the adapters. Put only mechanics that are identical across adapters in this package. `[ratelimit.go:package ratelimit]`
- A nil `req.Body` returns `(nil, nil)` and leaves `req` unchanged. `[ratelimit.go:BufferRequestBody]`
- After a successful read, `req.Body` is a fresh reader over the buffered bytes and `req.ContentLength` equals `len(body)`. `[ratelimit.go:BufferRequestBody]`
- A nil `body` leaves the clone's body as `req.Clone` produced it. A non-nil `body` replaces the clone's body and sets `ContentLength` to `len(body)`. `[ratelimit.go:CloneRequest]`
- `attempt` is 1-based: the delay is `base << (attempt - 1)`, so attempt 1 starts from `base`. An `attempt` below 1 gives a negative shift count, and Go panics at run time. The code does not guard this. `[ratelimit.go:BackoffDelay]`
- A shifted delay that is non-positive (shift overflow) or above `maxDelay` becomes `maxDelay`. `[ratelimit.go:BackoffDelay]`
- The result is `min(max(delay, retryAfter), maxDelay)`. A platform `retryAfter` can raise the delay but never above `maxDelay`. `[ratelimit.go:BackoffDelay]`
- `d <= 0` returns `ctx.Err()` at once without waiting. `[ratelimit.go:SleepCtx]`
- The sleep never outlasts the caller's context. The doc comment says this keeps retry inside the platform ack window. That holds only when the caller's context deadline fits that window. `[ratelimit.go:SleepCtx]`

## Side Effects

- Reads the whole request body into memory with `io.ReadAll`, closes the original body, and mutates `req.Body` and `req.ContentLength` in place. `[ratelimit.go:BufferRequestBody]`
- Allocates a new request with `req.Clone(req.Context())`. The original request is not mutated. `[ratelimit.go:CloneRequest]`
- Blocks the calling goroutine on a `time.NewTimer` until the timer fires or `ctx` is done. A deferred `timer.Stop` releases the timer. `[ratelimit.go:SleepCtx]`
- No package-level state, goroutines, locks, network, or disk access. `[ratelimit.go]`

## Verification

- Run: `go test ./internal/ratelimit/...` (reports `[no test files]`; this package has no tests of its own).
- The importers' retry tests exercise it: `go test ./adapters/linear/... ./adapters/slack/slackapi/...` (for example `TestCallRetriesThrottling` and `TestCallRetryStopsBeforeDeadline` in `adapters/slack/slackapi/retry_test.go`).
