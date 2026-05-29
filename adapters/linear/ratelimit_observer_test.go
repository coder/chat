package linear_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/coder/chat"
	"github.com/coder/chat/adapters/linear"
)

// rlCountingObserver counts adapter-facing observations so the Linear retry path's
// Observation Hook wiring (ADR 0010) can be asserted: an ObsAdapterCall per attempt
// and an ObsRateLimit per observed throttle.
type rlCountingObserver struct {
	mu     sync.Mutex
	counts map[chat.ObservationName]int
}

func (o *rlCountingObserver) Event(_ context.Context, name chat.ObservationName, _ ...chat.Attr) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.counts == nil {
		o.counts = map[chat.ObservationName]int{}
	}
	o.counts[name]++
}

func (o *rlCountingObserver) Dispatch(ctx context.Context, _ ...chat.Attr) (context.Context, chat.DispatchSpan) {
	return ctx, rlCountingSpan{}
}

func (o *rlCountingObserver) count(name chat.ObservationName) int {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.counts[name]
}

type rlCountingSpan struct{}

func (rlCountingSpan) End(chat.DispatchOutcome, ...chat.Attr) {}

// A throttled-then-successful Agent Activity create emits the adapter Observation
// Hook events through the configured Observer: one ObsAdapterCall per attempt and
// one ObsRateLimit for the single throttle. This proves Linear's retry path feeds
// the same ADR 0010 surface as Slack, reconciled to one shape (ADR 0005).
func TestLinearRetryEmitsObservationHookEvents(t *testing.T) {
	t.Parallel()

	api := newLinearAPIServer(t, 3600)
	api.rateLimit = 1 // first activity throttled, then succeeds
	now := time.UnixMilli(1_700_000_000_000)
	obs := &rlCountingObserver{}
	bot, adapter := newLinearRuntime(t, api, linear.Options{
		WebhookSecret: "whsec",
		Now:           func() time.Time { return now },
		RetryPolicy:   linear.RetryPolicy{MaxAttempts: 3, MaxElapsed: time.Second, BaseDelay: time.Millisecond, MaxDelay: 5 * time.Millisecond},
		Observer:      obs,
	})
	threadID := agentSessionThread(t, bot, api, now)

	if _, err := adapter.PostThought(context.Background(), threadID, "thinking"); err != nil {
		t.Fatalf("thought after retry: %v", err)
	}
	// Two activity attempts (one throttled + one success); the agent-session bootstrap
	// also issues calls, so assert at least the retry-path attempts were observed.
	if obs.count(chat.ObsAdapterCall) < 2 {
		t.Fatalf("ObsAdapterCall = %d, want >= 2 (one per attempt)", obs.count(chat.ObsAdapterCall))
	}
	if obs.count(chat.ObsRateLimit) != 1 {
		t.Fatalf("ObsRateLimit = %d, want 1 (single throttle)", obs.count(chat.ObsRateLimit))
	}
}

// An unconfigured Linear adapter (no Observer) defaults to a no-op observer: retry
// still works and surfaces no panic, proving the Observation Hook is optional.
func TestLinearRetryWithoutObserverIsNoop(t *testing.T) {
	t.Parallel()

	api := newLinearAPIServer(t, 3600)
	api.rateLimit = 1
	now := time.UnixMilli(1_700_000_000_000)
	bot, adapter := newLinearRuntime(t, api, linear.Options{
		WebhookSecret: "whsec",
		Now:           func() time.Time { return now },
		RetryPolicy:   linear.RetryPolicy{MaxAttempts: 3, MaxElapsed: time.Second, BaseDelay: time.Millisecond, MaxDelay: 5 * time.Millisecond},
	})
	threadID := agentSessionThread(t, bot, api, now)

	if _, err := adapter.PostThought(context.Background(), threadID, "thinking"); err != nil {
		t.Fatalf("thought after retry without observer: %v", err)
	}
}
