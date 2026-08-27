package chat

import "errors"

var (
	ErrUnsupportedCapability = errors.New("chat: unsupported adapter capability")
	// ErrPreempted is the cancellation cause a stopped handler observes via
	// context.Cause: its Lock Lease was lost mid-run (released by another
	// runtime instance, expired, or no longer refreshable), so mutual
	// exclusion could no longer be guaranteed.
	ErrPreempted = errors.New("chat: handler preempted")
	// ErrAdmissionRejected is returned by deferred dispatch when the Admission
	// Bound rejects a delivery: the instance is at MaxDetached, the delivery's
	// (adapter, tenant) installation is at MaxDetachedPerTenant, or Runtime
	// Shutdown has closed admission. The delivery was rejected before
	// acknowledgement and before dedupe marking — it is never recorded in
	// Event Identity, so a platform retry is not deduped away. Adapters map it
	// to a shape-aware overload response: a retry-inducing status for
	// platform-redelivered shapes, a truthful busy signal for direct
	// invocations the platform does not redeliver (ADR 0015).
	ErrAdmissionRejected = errors.New("chat: deferred dispatch admission rejected")
)

func assert(ok bool, message string) {
	if !ok {
		panic("chat: assertion failed: " + message)
	}
}
