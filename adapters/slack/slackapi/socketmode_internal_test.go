package slackapi

import (
	"errors"
	"testing"
	"time"
)

func TestListenBackoff(t *testing.T) {
	t.Parallel()

	opts := ListenOptions{MinBackoff: time.Second, MaxBackoff: 10 * time.Second}.withDefaults()
	dropped := errors.New("connection closed")
	tests := []struct {
		name     string
		opts     ListenOptions
		failures int
		err      error
		min, max time.Duration
	}{
		{name: "first failure", opts: opts, failures: 1, err: dropped, min: 500 * time.Millisecond, max: time.Second},
		{name: "third failure", opts: opts, failures: 3, err: dropped, min: 2 * time.Second, max: 4 * time.Second},
		{name: "capped", opts: opts, failures: 5, err: dropped, min: 5 * time.Second, max: 10 * time.Second},
		{name: "many failures", opts: opts, failures: 1000, err: dropped, min: 5 * time.Second, max: 10 * time.Second},
		{
			name: "too_many_websockets disconnect", opts: opts, failures: 1,
			err: &DisconnectError{Reason: "too_many_websockets"}, min: 5 * time.Second, max: 10 * time.Second,
		},
		{
			name: "too_many_websockets API error", opts: opts, failures: 1,
			err: &APIError{Code: "too_many_websockets"}, min: 5 * time.Second, max: 10 * time.Second,
		},
		{
			name: "Retry-After above the backoff", opts: opts, failures: 1,
			err: &RateLimited{RetryAfter: 30 * time.Second}, min: 30 * time.Second, max: 30 * time.Second,
		},
		{
			name: "Retry-After below the backoff", opts: opts, failures: 3,
			err: &RateLimited{RetryAfter: 100 * time.Millisecond}, min: 2 * time.Second, max: 4 * time.Second,
		},
		{
			name: "MinBackoff above MaxBackoff", opts: ListenOptions{MinBackoff: time.Hour, MaxBackoff: time.Second}.withDefaults(),
			failures: 1, err: dropped, min: 500 * time.Millisecond, max: time.Second,
		},
		{
			name: "defaults", opts: ListenOptions{MinBackoff: -1}.withDefaults(),
			failures: 1, err: dropped, min: 500 * time.Millisecond, max: time.Second,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			for range 100 {
				if got := tt.opts.backoff(tt.failures, tt.err); got < tt.min || got > tt.max {
					t.Fatalf("backoff(%d, %v) = %s, want from %s to %s", tt.failures, tt.err, got, tt.min, tt.max)
				}
			}
		})
	}
}
