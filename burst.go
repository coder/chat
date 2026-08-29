package chat

import (
	"context"
	"errors"
	"time"
)

// burstScope is one scope's burst coordination state: the currently collecting
// window, the FIFO of sealed batches awaiting dispatch, and whether a runner
// goroutine owns the scope. Exactly one runner is active per scope while any
// member is retained, which is what serializes rolled batches: a batch sealed
// while its predecessor dispatches waits in the FIFO and can never overtake it
// to the Thread Lock. An idle scope (no members, no runner work) deletes its
// entry, so scope cardinality never grows the map unboundedly.
type burstScope struct {
	// sealed is the FIFO of closed collection windows awaiting dispatch, in
	// seal order.
	sealed [][]preludeWork
	// open is the currently collecting window; nil when none is open. A
	// window exists only while it has members: it opens on its first member's
	// join and every member holds an Admission Bound slot, so parked burst
	// retention is always counted against MaxDetached.
	open []preludeWork
	// openedAt anchors the open window's deadline at its first member's join.
	// Later members never move it: the window seals at openedAt+BurstWindow
	// (or at MaxBurstBatch), so a steady sub-window stream cannot defer
	// dispatch indefinitely.
	openedAt time.Time
	// windowID identifies the open window so the runner's timer seal can
	// never close a successor window early: the timer carries the id of the
	// window it was armed for and a mismatch means that window already sealed.
	windowID uint64
	// wake tells a runner parked on the window timer that a window sealed at
	// its cap. Buffered so a seal never blocks a joining dispatch; a dropped
	// token is never a lost seal because a busy runner re-reads the FIFO when
	// it loops.
	wake chan struct{}
	// runnerActive is true while a runner goroutine owns the scope, so joins
	// never double-start a runner.
	runnerActive bool
}

// seal closes the open window into the sealed FIFO. Callers hold burstMu.
func (b *burstScope) seal() {
	assert(len(b.open) > 0, "sealed an empty burst window")
	b.sealed = append(b.sealed, b.open)
	b.open = nil
}

// joinBurstBatch parks a routed burst member in its scope's collection window,
// opening a new window (anchored now) when none is open, sealing at
// MaxBurstBatch with the cap-reaching member included, and starting the
// scope's runner when none is active. The member's admission slot travels with
// it: the runner releases it at the member's terminal disposition, so a parked
// member counts against MaxDetached for as long as it is retained (ADR 0015
// bounded retention).
func (c *Chat) joinBurstBatch(work preludeWork) {
	assert(work.releaseAdmission != nil, "burst member parked without a held admission slot")
	scope := work.scope
	c.burstMu.Lock()
	b := c.burstScopes[scope]
	if b == nil {
		b = &burstScope{wake: make(chan struct{}, 1)}
		c.burstScopes[scope] = b
	}
	// An open window whose anchored deadline already passed (the runner is
	// still dispatching a predecessor batch) seals before this member joins:
	// window boundaries follow the anchor, not the runner's availability, so
	// a late arrival never rides a window it missed.
	if b.open != nil && !time.Now().Before(b.openedAt.Add(c.options.BurstWindow)) {
		b.seal()
	}
	if b.open == nil {
		b.windowID++
		b.openedAt = time.Now()
	}
	b.open = append(b.open, work)
	if c.options.MaxBurstBatch > 0 && len(b.open) >= c.options.MaxBurstBatch {
		// The cap-reaching member seals its own window as the batch's last
		// member; the next arrival opens a rolled window dispatched strictly
		// after this one.
		b.seal()
		select {
		case b.wake <- struct{}{}:
		default:
		}
	}
	startRunner := !b.runnerActive
	if startRunner {
		b.runnerActive = true
		// The runner joins the shutdown drain while this member's admission
		// slot is still held, so no WaitGroup Add can happen after Shutdown's
		// admission-slot drain completes.
		c.inflight.Add(1)
	}
	c.burstMu.Unlock()
	c.logger.Debug("chat burst member joined", "adapter", work.event.Adapter, "event_id", work.event.ID, "thread_id", work.event.ThreadID, "route", work.route)
	if startRunner {
		go c.runBurstScope(scope)
	}
}

// runBurstScope is the per-scope burst runner: it dispatches sealed batches in
// FIFO order, waits out the open window's anchored deadline when the FIFO is
// empty, and retires (deleting the scope entry) when the scope holds no work.
// The runner is bounded only by Runtime Shutdown — per-batch coordination and
// per-member execution carry their own DetachTimeout budgets — and on
// shutdown it disposes every parked member observably before exiting.
func (c *Chat) runBurstScope(scope string) {
	defer c.inflight.Done()
	for {
		batch, wait, wake, windowID, retired := c.nextBurstWork(scope)
		if retired {
			return
		}
		if batch != nil {
			c.dispatchBurstBatch(scope, batch)
			continue
		}
		timer := time.NewTimer(wait)
		select {
		case <-c.baseCtx.Done():
			timer.Stop()
			c.drainBurstScope(scope)
			return
		case <-wake:
			timer.Stop()
		case <-timer.C:
			c.sealBurstWindow(scope, windowID)
		}
	}
}

// nextBurstWork hands the runner its next unit of work: the oldest sealed
// batch when one waits, otherwise the open window's remaining anchored wait,
// otherwise retirement — the runner flag clears and the idle scope's entry is
// deleted so an inactive scope retains nothing. Retirement and joins serialize
// under burstMu, so a join racing retirement either sees the active runner or
// starts a fresh one; a runner is never lost or doubled.
func (c *Chat) nextBurstWork(scope string) (batch []preludeWork, wait time.Duration, wake <-chan struct{}, windowID uint64, retired bool) {
	c.burstMu.Lock()
	defer c.burstMu.Unlock()
	b := c.burstScopes[scope]
	assert(b != nil && b.runnerActive, "burst runner without live scope state")
	if len(b.sealed) > 0 {
		batch = b.sealed[0]
		b.sealed[0] = nil
		b.sealed = b.sealed[1:]
		return batch, 0, nil, 0, false
	}
	if b.open != nil {
		return nil, time.Until(b.openedAt.Add(c.options.BurstWindow)), b.wake, b.windowID, false
	}
	b.runnerActive = false
	delete(c.burstScopes, scope)
	return nil, 0, nil, 0, true
}

// sealBurstWindow closes the open window when the runner's window timer fires.
// The window id guards the seal: a cap-sealed window whose successor already
// opened must not lose its successor's remaining collection time to a stale
// timer.
func (c *Chat) sealBurstWindow(scope string, windowID uint64) {
	c.burstMu.Lock()
	defer c.burstMu.Unlock()
	b := c.burstScopes[scope]
	assert(b != nil, "burst window sealed without scope state")
	if b.open == nil || b.windowID != windowID {
		return
	}
	b.seal()
}

// dispatchBurstBatch runs one sealed batch: a single Thread Lock hold with the
// lease refreshed across the batch, members in join order, each with its own
// DetachTimeout execution budget. The lock-wait budget is a fresh
// DetachTimeout starting when the batch reaches the head of its scope's FIFO,
// so neither collection time nor a predecessor batch's execution consumes it.
// Every member reaches an observable terminal outcome and releases its
// admission slot at disposition; the final member's slot is retained until the
// batch's lock cleanup completes so the tail work stays counted against
// MaxDetached.
func (c *Chat) dispatchBurstBatch(scope string, batch []preludeWork) {
	assert(len(batch) > 0, "burst batch dispatched with no members")
	// Only the identifying strings are copied out of the first member: holding
	// its *Event here would keep the payload reachable after the member's
	// references are cleared below.
	adapter, threadID := batch[0].event.Adapter, batch[0].event.ThreadID

	waitCtx, waitCancel := context.WithTimeout(c.baseCtx, c.options.DetachTimeout)
	lease, outcome, err := c.pollForLock(waitCtx, scope, nil)
	waitErr := waitCtx.Err()
	waitCancel()
	if outcome != acquireHeld {
		// A batch that never held the lock ran nothing: every member closes
		// observably — a state failure as an error outcome, an abandoned wait
		// as ignored — and frees its admission slot. Never silent (ADR 0015).
		if outcome == acquireFailed {
			c.logger.Error("chat burst acquire lock failed", "error", err, "adapter", adapter, "thread_id", threadID, "size", len(batch))
		} else {
			c.logger.Info("chat burst wait abandoned", "adapter", adapter, "thread_id", threadID, "size", len(batch), "error", waitErr)
		}
		for i := range batch {
			c.safeEnd(batch[i].span, waitOutcome(outcome), RouteAttr(batch[i].route))
			batch[i].releaseAdmission()
			batch[i] = preludeWork{}
		}
		return
	}

	// Like runLockedTail, every member is cancellable on lease loss with cause
	// ErrPreempted: mutual exclusion gone means the batch must stop, not run
	// alongside the lease's next holder.
	batchCtx, batchCancel := context.WithCancelCause(c.baseCtx)
	defer batchCancel(nil)
	c.logger.Info("chat burst batch dispatch", "adapter", adapter, "thread_id", threadID, "size", len(batch))
	stopRefresh, leaseLost := c.startLockRefresh(batchCtx, lease, threadID, batchCancel)

	// finalRelease is the last member's admission release, deferred past the
	// lock cleanup below.
	var finalRelease func()
	for i := range batch {
		work := batch[i]
		if batchCtx.Err() != nil {
			// Lease loss or Runtime Shutdown: members that never started are
			// skipped, never run unserialized — each closes observably as
			// ignored, distinct from the preempted outcome of a member that
			// was actually cancelled mid-run.
			c.logger.Info("chat burst batch abandoned", "adapter", adapter, "thread_id", threadID, "remaining", len(batch)-i, "error", context.Cause(batchCtx))
			for j := i; j < len(batch); j++ {
				rest := batch[j]
				c.safeEnd(rest.span, OutcomeIgnored, RouteAttr(rest.route))
				if j == len(batch)-1 {
					finalRelease = rest.releaseAdmission
				} else {
					rest.releaseAdmission()
				}
				batch[j] = preludeWork{}
			}
			break
		}
		// Each member gets its own DetachTimeout execution budget: a slow
		// earlier member never consumes a later member's allowance. A member
		// whose handler ignores cancellation blocks here cooperatively, like
		// every deferred tail; its timeout outcome is recorded when it
		// returns.
		memberCtx, memberCancel := context.WithTimeout(batchCtx, c.options.DetachTimeout)
		c.logger.Info("chat deferred dispatch started", "adapter", work.event.Adapter, "event_id", work.event.ID, "route", work.route)
		runErr := work.run(memberCtx)
		// The classification snapshot mirrors runLockedTail: within the run,
		// the outcome follows the cancellation cause, not the handler's return
		// convention — a member that observed ctx.Done, shut down cleanly, and
		// returned nil still lost its lease. A handler error without lease
		// loss is recorded and does not abort the batch: the remaining members
		// are accepted deliveries and the lease is intact.
		if errors.Is(context.Cause(memberCtx), ErrPreempted) {
			c.logger.Info("chat handler preempted", "adapter", work.event.Adapter, "event_id", work.event.ID, "thread_id", work.event.ThreadID, "route", work.route)
			c.safeEnd(work.span, OutcomePreempted, RouteAttr(work.route))
		} else {
			c.endHandlerRun(memberCtx, work.event, work.route, work.span, runErr)
		}
		memberCancel()
		if i == len(batch)-1 {
			finalRelease = work.releaseAdmission
		} else {
			work.releaseAdmission()
		}
		// A disposed member's references clear immediately so its payload and
		// closure are collectable while later members still execute: batch
		// retention shrinks as the batch drains instead of pinning every
		// member until the tail exits.
		batch[i] = preludeWork{}
	}
	stopRefresh()
	benign := batchCtx.Err() != nil || leaseLost()
	c.releaseTailLock(batchCtx, lease, threadID, benign)
	assert(finalRelease != nil, "burst batch finished without a final admission release")
	finalRelease()
}

// drainBurstScope disposes every parked member — sealed batches and the open
// window — when Runtime Shutdown cancels the runner: each member's span closes
// as ignored (the same observable abandonment contract debounce waiters
// follow) and its admission slot releases, so Shutdown's admission drain can
// complete and no admitted delivery is silently lost.
func (c *Chat) drainBurstScope(scope string) {
	c.burstMu.Lock()
	b := c.burstScopes[scope]
	assert(b != nil && b.runnerActive, "burst drain without live scope state")
	var members []preludeWork
	for _, batch := range b.sealed {
		members = append(members, batch...)
	}
	members = append(members, b.open...)
	b.sealed, b.open = nil, nil
	b.runnerActive = false
	delete(c.burstScopes, scope)
	c.burstMu.Unlock()
	for i := range members {
		work := members[i]
		c.logger.Info("chat burst wait abandoned", "adapter", work.event.Adapter, "event_id", work.event.ID, "thread_id", work.event.ThreadID, "error", c.baseCtx.Err())
		c.safeEnd(work.span, OutcomeIgnored, RouteAttr(work.route))
		work.releaseAdmission()
		members[i] = preludeWork{}
	}
}
