package chat

import "errors"

var (
	ErrUnsupportedCapability = errors.New("chat: unsupported adapter capability")
	// ErrPreempted is the cancellation cause a stopped handler observes via
	// context.Cause: its Lock Lease was lost mid-run (released by another
	// runtime instance, expired, or no longer refreshable), so mutual
	// exclusion could no longer be guaranteed.
	ErrPreempted = errors.New("chat: handler preempted")
)

func assert(ok bool, message string) {
	if !ok {
		panic("chat: assertion failed: " + message)
	}
}
