package memory

import (
	"context"
	"testing"
	"time"

	"github.com/coder/chat"
)

func TestTTLOperationsReadClockUnderStateMutex(t *testing.T) {
	ctx := context.Background()
	expiry := time.Unix(1000, 0)
	beforeExpiry := expiry.Add(-time.Nanosecond)
	afterExpiry := expiry.Add(time.Nanosecond)

	installMutexSensitiveClock := func(t *testing.T, state *State) {
		t.Helper()
		state.now = func() time.Time {
			if state.mu.TryLock() {
				state.mu.Unlock()
				return beforeExpiry
			}
			return afterExpiry
		}
	}

	t.Run("MarkEvent treats expired event as new", func(t *testing.T) {
		state := New()
		state.events["event"] = expiry
		installMutexSensitiveClock(t, state)

		first, err := state.MarkEvent(ctx, "event", time.Minute)
		if err != nil {
			t.Fatalf("mark event: %v", err)
		}
		if !first {
			t.Fatal("expected expired event to be marked as new")
		}
	})

	t.Run("AcquireLock replaces expired lock", func(t *testing.T) {
		state := New()
		state.locks["thread"] = lockRecord{token: "old-token", expiry: expiry}
		installMutexSensitiveClock(t, state)

		_, acquired, err := state.AcquireLock(ctx, "thread", time.Minute)
		if err != nil {
			t.Fatalf("acquire lock: %v", err)
		}
		if !acquired {
			t.Fatal("expected expired lock to be acquired")
		}
	})

	t.Run("ExtendLock rejects expired lock", func(t *testing.T) {
		state := New()
		lease := chat.LockLease{Key: "thread", Token: "token"}
		state.locks[lease.Key] = lockRecord{token: lease.Token, expiry: expiry}
		installMutexSensitiveClock(t, state)

		extended, err := state.ExtendLock(ctx, lease, time.Minute)
		if err != nil {
			t.Fatalf("extend lock: %v", err)
		}
		if extended {
			t.Fatal("expected expired lock extension to be rejected")
		}
	})

	t.Run("ReleaseLock rejects expired lock", func(t *testing.T) {
		state := New()
		lease := chat.LockLease{Key: "thread", Token: "token"}
		state.locks[lease.Key] = lockRecord{token: lease.Token, expiry: expiry}
		installMutexSensitiveClock(t, state)

		released, err := state.ReleaseLock(ctx, lease)
		if err != nil {
			t.Fatalf("release lock: %v", err)
		}
		if released {
			t.Fatal("expected expired lock release to be rejected")
		}
	})
}

func TestTTLOperationsPruneExpiredRecords(t *testing.T) {
	ctx := context.Background()

	t.Run("MarkEvent prunes expired events", func(t *testing.T) {
		now := time.Unix(1000, 0)
		state := New()
		state.now = func() time.Time { return now }

		for _, id := range []string{"expired-1", "expired-2"} {
			first, err := state.MarkEvent(ctx, id, time.Minute)
			if err != nil {
				t.Fatalf("mark event %q: %v", id, err)
			}
			if !first {
				t.Fatalf("expected event %q to be new", id)
			}
		}

		now = now.Add(2 * time.Minute)
		state.events["fresh"] = now.Add(time.Minute)
		first, err := state.MarkEvent(ctx, "new", time.Minute)
		if err != nil {
			t.Fatalf("mark new event: %v", err)
		}
		if !first {
			t.Fatal("expected new event to be marked as new")
		}

		if _, ok := state.events["fresh"]; !ok {
			t.Fatal("expected fresh event to remain")
		}
		if _, ok := state.events["new"]; !ok {
			t.Fatal("expected new event to be stored")
		}
		if got := len(state.events); got != 2 {
			t.Fatalf("expected 2 events after pruning, got %d", got)
		}
	})

	t.Run("AcquireLock prunes expired locks", func(t *testing.T) {
		now := time.Unix(1000, 0)
		state := New()
		state.now = func() time.Time { return now }

		for _, key := range []string{"expired-1", "expired-2"} {
			_, acquired, err := state.AcquireLock(ctx, key, time.Minute)
			if err != nil {
				t.Fatalf("acquire lock %q: %v", key, err)
			}
			if !acquired {
				t.Fatalf("expected lock %q to be acquired", key)
			}
		}

		now = now.Add(2 * time.Minute)
		state.locks["fresh"] = lockRecord{token: "fresh-token", expiry: now.Add(time.Minute)}
		_, acquired, err := state.AcquireLock(ctx, "new", time.Minute)
		if err != nil {
			t.Fatalf("acquire new lock: %v", err)
		}
		if !acquired {
			t.Fatal("expected new lock to be acquired")
		}

		if _, ok := state.locks["fresh"]; !ok {
			t.Fatal("expected fresh lock to remain")
		}
		if _, ok := state.locks["new"]; !ok {
			t.Fatal("expected new lock to be stored")
		}
		if got := len(state.locks); got != 2 {
			t.Fatalf("expected 2 locks after pruning, got %d", got)
		}
	})
}
