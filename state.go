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
