package slack

import "github.com/coder/chat/adapters/slack/slackapi"

// RetryPolicy bounds outbound Slack rate-limit retry/backoff (ADR 0005). It is
// slackapi.RetryPolicy: retry honors Slack's Retry-After header and never sleeps
// past the caller's context deadline, so it cannot outlive Slack's 3-second ack
// window. The zero value applies a conservative default; MaxAttempts: 1 disables
// retry.
type RetryPolicy = slackapi.RetryPolicy

// RateLimited is returned when bounded retry is exhausted against a Slack rate
// limit (HTTP 429, or the ratelimited or rate_limited API error), or when a single
// Retry-After would exceed the caller's deadline. It is slackapi.RateLimited and
// carries the last Retry-After, the attempt count, and the raw platform response.
type RateLimited = slackapi.RateLimited
