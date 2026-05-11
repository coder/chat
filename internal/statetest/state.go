package statetest

import (
	"context"
	"testing"
	"time"

	"github.com/coder/chat"
)

type Harness struct {
	State       chat.State
	AdvanceTime func(time.Duration)
	ShortTTL    time.Duration
	ExpiryWait  time.Duration
}

func RunStateConformance(t *testing.T, newState func(*testing.T) Harness) {
	t.Helper()

	t.Run("subscriptions", func(t *testing.T) {
		t.Parallel()
		harness := newState(t)
		state := harness.State
		threadID := chat.ThreadID("fake:v1:thread-1")

		subscribed, err := state.IsThreadSubscribed(context.Background(), threadID)
		if err != nil {
			t.Fatalf("initial subscription check: %v", err)
		}
		if subscribed {
			t.Fatal("new thread should not be subscribed")
		}

		if err := state.SubscribeThread(context.Background(), threadID); err != nil {
			t.Fatalf("subscribe: %v", err)
		}
		subscribed, err = state.IsThreadSubscribed(context.Background(), threadID)
		if err != nil {
			t.Fatalf("subscribed check: %v", err)
		}
		if !subscribed {
			t.Fatal("thread should be subscribed")
		}

		if err := state.UnsubscribeThread(context.Background(), threadID); err != nil {
			t.Fatalf("unsubscribe: %v", err)
		}
		subscribed, err = state.IsThreadSubscribed(context.Background(), threadID)
		if err != nil {
			t.Fatalf("unsubscribed check: %v", err)
		}
		if subscribed {
			t.Fatal("thread should not be subscribed after unsubscribe")
		}
	})

	t.Run("dedupe", func(t *testing.T) {
		t.Parallel()
		harness := newState(t)
		state := harness.State
		shortTTL := harness.shortTTL()

		first, err := state.MarkEvent(context.Background(), "event-1", shortTTL)
		if err != nil {
			t.Fatalf("mark first event: %v", err)
		}
		if !first {
			t.Fatal("first event should be accepted")
		}
		first, err = state.MarkEvent(context.Background(), "event-1", time.Minute)
		if err != nil {
			t.Fatalf("mark duplicate event: %v", err)
		}
		if first {
			t.Fatal("duplicate event should not be accepted")
		}

		advanceTime(harness, harness.expiryWait())
		first, err = state.MarkEvent(context.Background(), "event-1", time.Minute)
		if err != nil {
			t.Fatalf("mark expired event: %v", err)
		}
		if !first {
			t.Fatal("expired event should be accepted again")
		}
	})

	t.Run("locks", func(t *testing.T) {
		t.Parallel()
		harness := newState(t)
		state := harness.State
		shortTTL := harness.shortTTL()

		lease, acquired, err := state.AcquireLock(context.Background(), "thread-1", shortTTL)
		if err != nil {
			t.Fatalf("acquire lock: %v", err)
		}
		if !acquired {
			t.Fatal("first lock acquire should succeed")
		}

		_, acquired, err = state.AcquireLock(context.Background(), "thread-1", time.Minute)
		if err != nil {
			t.Fatalf("conflicting lock acquire: %v", err)
		}
		if acquired {
			t.Fatal("conflicting lock acquire should fail")
		}

		stale := chat.LockLease{Key: lease.Key, Token: lease.Token + "-stale"}
		released, err := state.ReleaseLock(context.Background(), stale)
		if err != nil {
			t.Fatalf("stale release: %v", err)
		}
		if released {
			t.Fatal("stale release should not release current lock")
		}

		extended, err := state.ExtendLock(context.Background(), lease, time.Minute)
		if err != nil {
			t.Fatalf("extend lock: %v", err)
		}
		if !extended {
			t.Fatal("current owner should extend lock")
		}

		released, err = state.ReleaseLock(context.Background(), lease)
		if err != nil {
			t.Fatalf("release lock: %v", err)
		}
		if !released {
			t.Fatal("current owner should release lock")
		}

		lease, acquired, err = state.AcquireLock(context.Background(), "thread-1", shortTTL)
		if err != nil {
			t.Fatalf("second acquire lock: %v", err)
		}
		if !acquired {
			t.Fatal("second acquire should succeed after release")
		}
		advanceTime(harness, harness.expiryWait())

		_, acquired, err = state.AcquireLock(context.Background(), "thread-1", time.Minute)
		if err != nil {
			t.Fatalf("acquire expired lock: %v", err)
		}
		if !acquired {
			t.Fatal("acquire should succeed after lock ttl")
		}

		released, err = state.ReleaseLock(context.Background(), lease)
		if err != nil {
			t.Fatalf("release expired stale lock: %v", err)
		}
		if released {
			t.Fatal("expired stale release should not release newer lock")
		}
	})

	t.Run("context cancellation", func(t *testing.T) {
		t.Parallel()
		harness := newState(t)
		state := harness.State
		ctx, cancel := context.WithCancel(context.Background())
		cancel()

		if err := state.SubscribeThread(ctx, "fake:v1:thread"); err == nil {
			t.Fatal("expected cancelled context to stop state mutation")
		}
		if _, err := state.MarkEvent(ctx, "event", time.Minute); err == nil {
			t.Fatal("expected cancelled context to stop dedupe mutation")
		}
		if _, _, err := state.AcquireLock(ctx, "thread", time.Minute); err == nil {
			t.Fatal("expected cancelled context to stop lock mutation")
		}
	})
}

func advanceTime(harness Harness, duration time.Duration) {
	if harness.AdvanceTime != nil {
		harness.AdvanceTime(duration)
		return
	}
	time.Sleep(duration)
}

func (h Harness) shortTTL() time.Duration {
	if h.ShortTTL != 0 {
		return h.ShortTTL
	}
	return 25 * time.Millisecond
}

func (h Harness) expiryWait() time.Duration {
	if h.ExpiryWait != 0 {
		return h.ExpiryWait
	}
	return 60 * time.Millisecond
}
