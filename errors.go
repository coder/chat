package chat

import "errors"

var (
	ErrUnsupportedCapability = errors.New("chat: unsupported adapter capability")
	// ErrPreempted is the cancellation cause a stopped handler observes via
	// context.Cause: a new delivery preempted it through the OnLockConflict
	// steerability hook, or its Lock Lease was lost (force released by another
	// runtime instance, or expired) so mutual exclusion could no longer be
	// guaranteed.
	ErrPreempted = errors.New("chat: handler preempted")
)

func assert(ok bool, message string) {
	if !ok {
		panic("chat: assertion failed: " + message)
	}
}
