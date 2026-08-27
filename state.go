package chat

import (
	"context"
	"time"
)

type State interface {
	IsThreadSubscribed(context.Context, ThreadID) (bool, error)
	SubscribeThread(context.Context, ThreadID) error
	UnsubscribeThread(context.Context, ThreadID) error
	MarkEvent(context.Context, string, time.Duration) (bool, error)
	AcquireLock(context.Context, string, time.Duration) (LockLease, bool, error)
	ExtendLock(context.Context, LockLease, time.Duration) (bool, error)
	ReleaseLock(context.Context, LockLease) (bool, error)
	Shutdown(context.Context) error
}

type LockLease struct {
	Key   string
	Token string
}

// LockForcer is the Optional Capability for force-releasing a Thread Lock (the
// ADR 0012 force/steerability path). ForceReleaseLock invalidates the current
// Lock Lease for key regardless of owner so a preempting delivery can acquire a
// fresh lease; it reports whether a lease was invalidated. The token-owned
// lease invariant is preserved: the previous holder's ExtendLock and
// ReleaseLock fail cleanly instead of touching the preemptor's newer lock.
// States that support preemption implement it; the runtime requires it only
// when RuntimeOptions.OnLockConflict is configured.
type LockForcer interface {
	ForceReleaseLock(ctx context.Context, key string) (bool, error)
}
