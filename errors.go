package chat

import "errors"

var (
	ErrUnsupportedCapability = errors.New("chat: unsupported adapter capability")
	// ErrPreempted is the cancellation cause a preempted handler observes via
	// context.Cause: its Lock Lease was force released (or lost) because a new
	// delivery preempted it through the OnLockConflict steerability hook.
	ErrPreempted = errors.New("chat: handler preempted")
)

func assert(ok bool, message string) {
	if !ok {
		panic("chat: assertion failed: " + message)
	}
}
