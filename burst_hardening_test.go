package chat_test

import (
	"context"
	"errors"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"weak"

	"github.com/coder/chat"
)

// This file is the burst lifecycle hardening suite: each test proves one of
// the burst-lifecycle findings from the ADR 0015 review history (PR #54) in
// code, per the issue #55 contract. The finding each test covers is named in
// its comment.

// countingLockState counts AcquireLock calls so tests can prove the burst
// prelude never touches the Thread Lock before acknowledgement.
type countingLockState struct {
	*fakeState
	acquires atomic.Int64
}

func (s *countingLockState) AcquireLock(ctx context.Context, key string, ttl time.Duration) (chat.LockLease, bool, error) {
	s.acquires.Add(1)
	return s.fakeState.AcquireLock(ctx, key, ttl)
}

// blockingReleaseState blocks the first ReleaseLock call on a gate so tests
// can observe admission accounting while a batch's lock cleanup is still in
// flight.
type blockingReleaseState struct {
	*fakeState
	mu       sync.Mutex
	blockOne bool
	started  chan struct{}
	gate     chan struct{}
}

func newBlockingReleaseState() *blockingReleaseState {
	return &blockingReleaseState{
		fakeState: newFakeState(),
		blockOne:  true,
		started:   make(chan struct{}),
		gate:      make(chan struct{}),
	}
}

func (s *blockingReleaseState) ReleaseLock(ctx context.Context, lease chat.LockLease) (bool, error) {
	s.mu.Lock()
	shouldBlock := s.blockOne
	s.blockOne = false
	s.mu.Unlock()
	if shouldBlock {
		close(s.started)
		<-s.gate
	}
	return s.fakeState.ReleaseLock(ctx, lease)
}

func outcomeCounts(observer *recordingObserver) map[chat.DispatchOutcome]int {
	counts := map[chat.DispatchOutcome]int{}
	for _, outcome := range observer.terminalOutcomes() {
		counts[outcome]++
	}
	return counts
}

// Finding: window anchor — the collection window is anchored at its first
// member's join and later arrivals never extend it, so a steady sub-window
// stream cannot defer dispatch indefinitely (latency starvation).
func TestBurstWindowAnchoredAtFirstMemberNotResetByArrivals(t *testing.T) {
	t.Parallel()

	state := newFakeState()
	adapter := newFakeAdapter("fake")
	var logs syncBuffer
	bot := newBurstRuntime(t, state, adapter, &logs, nil, func(o *chat.RuntimeOptions) {
		o.BurstWindow = 250 * time.Millisecond
	})

	recorder := &burstRecorder{}
	var firstHandled sync.Once
	firstBatch := make(chan struct{})
	bot.OnNewMention(func(ctx context.Context, ev *chat.MessageEvent) error {
		firstHandled.Do(func() { close(firstBatch) })
		return recorder.handle(ctx, ev)
	})

	// Post events faster than the window for far longer than one window: if
	// arrivals reset the timer (debounce semantics), no batch would dispatch
	// while the stream continues.
	posted := 0
	streaming := true
	deadline := time.Now().Add(6 * time.Second)
	for streaming && time.Now().Before(deadline) {
		id := "event-" + string(rune('a'+posted%26)) + "-" + time.Now().Format("150405.000000")
		if status := postEvent(t, bot, "fake", mentionEvent(id, "fake:v1:thread-1")); status != 200 {
			t.Fatalf("dispatch %s: status %d", id, status)
		}
		posted++
		select {
		case <-firstBatch:
			streaming = false
		case <-time.After(40 * time.Millisecond):
		}
	}
	if streaming {
		t.Fatalf("no batch dispatched while sub-window arrivals continued (%d posted); logs:\n%s", posted, logs.String())
	}

	// Delivery-preserving: every accepted member is eventually handled even
	// though the anchored window moved batch boundaries mid-stream.
	eventually(t, 10*time.Second, func() bool {
		return len(recorder.snapshot()) == posted
	}, "accepted members were dropped by window rolling")
}

// Finding: cap-reaching-member ownership — the member that reaches
// MaxBurstBatch seals its own window as that batch's last member (the batch
// dispatches immediately, well before the window deadline), and the next
// event opens a rolled window.
func TestBurstCapReachingMemberSealsItsOwnWindow(t *testing.T) {
	t.Parallel()

	state := newFakeState()
	adapter := newFakeAdapter("fake")
	var logs syncBuffer
	bot := newBurstRuntime(t, state, adapter, &logs, nil, func(o *chat.RuntimeOptions) {
		// The window is far longer than the test: only the cap can seal it.
		o.BurstWindow = 10 * time.Second
		o.MaxBurstBatch = 3
	})

	recorder := &burstRecorder{}
	bot.OnNewMention(recorder.handle)

	for _, id := range []string{"event-1", "event-2", "event-3"} {
		if status := postEvent(t, bot, "fake", mentionEvent(id, "fake:v1:thread-1")); status != 200 {
			t.Fatalf("dispatch %s: status %d", id, status)
		}
	}
	eventually(t, 5*time.Second, func() bool {
		return len(recorder.snapshot()) == 3
	}, "cap-sealed batch did not dispatch before the window deadline")

	for _, id := range []string{"event-4", "event-5", "event-6"} {
		if status := postEvent(t, bot, "fake", mentionEvent(id, "fake:v1:thread-1")); status != 200 {
			t.Fatalf("dispatch %s: status %d", id, status)
		}
	}
	eventually(t, 5*time.Second, func() bool {
		return len(recorder.snapshot()) == 6
	}, "rolled window's cap-sealed batch did not dispatch")

	if got := recorder.snapshot(); !equalStrings(got, []string{"event-1", "event-2", "event-3", "event-4", "event-5", "event-6"}) {
		t.Fatalf("cap-sealed batches ran out of order: %v", got)
	}
	if got := strings.Count(logs.String(), "chat burst batch dispatch"); got != 2 {
		t.Fatalf("expected exactly 2 batch dispatches, got %d; logs:\n%s", got, logs.String())
	}
	if got := strings.Count(logs.String(), "size=3"); got != 2 {
		t.Fatalf("expected both batches to carry 3 members (cap-reaching member included); logs:\n%s", logs.String())
	}
}

// Finding: lock sequencing — a burst delivery acknowledges promptly without
// acquiring or contending on the Thread Lock in the synchronous prelude; the
// batch acquires the lock only after the window seals.
func TestBurstAcknowledgesPromptlyWithoutPreludeLockAcquisition(t *testing.T) {
	t.Parallel()

	state := &countingLockState{fakeState: newFakeState()}
	adapter := newFakeAdapter("fake")
	var logs syncBuffer
	bot := newBurstRuntime(t, state, adapter, &logs, nil, func(o *chat.RuntimeOptions) {
		o.BurstWindow = 300 * time.Millisecond
	})

	recorder := &burstRecorder{}
	bot.OnNewMention(recorder.handle)

	// A foreign holder owns the scope's lock; a pre-ack lock acquisition
	// would park or conflict the webhook request.
	lease, acquired, err := state.fakeState.AcquireLock(context.Background(), "fake:v1:thread-1", time.Minute)
	if err != nil || !acquired {
		t.Fatalf("foreign lock hold: acquired=%v err=%v", acquired, err)
	}

	if status := postEvent(t, bot, "fake", mentionEvent("event-1", "fake:v1:thread-1")); status != 200 {
		t.Fatalf("dispatch with held lock: status %d", status)
	}
	if got := state.acquires.Load(); got != 0 {
		t.Fatalf("prelude touched the Thread Lock %d times before acknowledgement", got)
	}

	if _, err := state.fakeState.ReleaseLock(context.Background(), lease); err != nil {
		t.Fatalf("release foreign lock: %v", err)
	}
	eventually(t, 5*time.Second, func() bool {
		return len(recorder.snapshot()) == 1
	}, "batch never dispatched after the foreign lock released")
}

// Finding: FIFO ordering of rolled batches — batches sealed in order for one
// scope acquire the lock and execute strictly in seal order, even when they
// seal while the scope's lock is held elsewhere.
func TestBurstRolledBatchesDispatchInFIFOOrder(t *testing.T) {
	t.Parallel()

	state := newFakeState()
	adapter := newFakeAdapter("fake")
	var logs syncBuffer
	bot := newBurstRuntime(t, state, adapter, &logs, nil, func(o *chat.RuntimeOptions) {
		o.BurstWindow = 10 * time.Second
		o.MaxBurstBatch = 2
	})

	recorder := &burstRecorder{}
	bot.OnNewMention(recorder.handle)

	// Both batches seal (at the cap) while a foreign holder owns the lock, so
	// they are simultaneously ready: only FIFO sequencing keeps batch 2 from
	// overtaking batch 1.
	lease, acquired, err := state.AcquireLock(context.Background(), "fake:v1:thread-1", time.Minute)
	if err != nil || !acquired {
		t.Fatalf("foreign lock hold: acquired=%v err=%v", acquired, err)
	}
	for _, id := range []string{"event-1", "event-2", "event-3", "event-4"} {
		if status := postEvent(t, bot, "fake", mentionEvent(id, "fake:v1:thread-1")); status != 200 {
			t.Fatalf("dispatch %s: status %d", id, status)
		}
	}
	time.Sleep(50 * time.Millisecond)
	if _, err := state.ReleaseLock(context.Background(), lease); err != nil {
		t.Fatalf("release foreign lock: %v", err)
	}

	eventually(t, 5*time.Second, func() bool {
		return len(recorder.snapshot()) == 4
	}, "rolled batches were not all dispatched")
	if got := recorder.snapshot(); !equalStrings(got, []string{"event-1", "event-2", "event-3", "event-4"}) {
		t.Fatalf("rolled batches ran out of FIFO order: %v", got)
	}
}

// Finding: bounded pre-execution coordination — a batch whose lock wait
// exhausts its budget terminates: every member closes observably and frees
// its admission slot instead of leaking it behind an unavailable lock.
func TestBurstLockWaitBoundedAndMembersDisposedObservably(t *testing.T) {
	t.Parallel()

	state := newFakeState()
	adapter := newFakeAdapter("fake")
	var logs syncBuffer
	observer := &recordingObserver{}
	bot := newBurstRuntime(t, state, adapter, &logs, observer, func(o *chat.RuntimeOptions) {
		o.MaxDetached = 2
		o.DetachTimeout = 250 * time.Millisecond
	})

	recorder := &burstRecorder{}
	bot.OnNewMention(recorder.handle)

	// The foreign holder never releases: the sealed batch must give up after
	// its DetachTimeout lock-wait budget.
	if _, acquired, err := state.AcquireLock(context.Background(), "fake:v1:thread-1", time.Minute); err != nil || !acquired {
		t.Fatalf("foreign lock hold: acquired=%v err=%v", acquired, err)
	}
	for _, id := range []string{"event-1", "event-2"} {
		if status := postEvent(t, bot, "fake", mentionEvent(id, "fake:v1:thread-1")); status != 200 {
			t.Fatalf("dispatch %s: status %d", id, status)
		}
	}

	eventually(t, 5*time.Second, func() bool {
		return outcomeCounts(observer)[chat.OutcomeIgnored] == 2
	}, "abandoned batch members did not close observably")
	if !strings.Contains(logs.String(), "chat burst wait abandoned") {
		t.Fatalf("abandoned lock wait was not surfaced; logs:\n%s", logs.String())
	}
	if got := recorder.snapshot(); len(got) != 0 {
		t.Fatalf("members ran without the lock: %v", got)
	}

	// Both admission slots freed: the saturated instance admits new work.
	if status := postEvent(t, bot, "fake", mentionEvent("event-3", "fake:v1:thread-2")); status != 200 {
		t.Fatalf("admission slot leaked behind the abandoned batch: status %d", status)
	}
	if status := postEvent(t, bot, "fake", mentionEvent("event-4", "fake:v1:thread-2")); status != 200 {
		t.Fatalf("admission slot leaked behind the abandoned batch: status %d", status)
	}
}

// Finding: FIFO wait budget — a rolled batch's lock-wait budget starts when
// it reaches the head of its scope's FIFO, so a healthy predecessor batch
// whose members legitimately run longer than one DetachTimeout does not time
// the successor out.
func TestBurstQueuedBatchLockBudgetStartsAtHeadOfQueue(t *testing.T) {
	t.Parallel()

	state := newFakeState()
	adapter := newFakeAdapter("fake")
	var logs syncBuffer
	bot := newBurstRuntime(t, state, adapter, &logs, nil, func(o *chat.RuntimeOptions) {
		o.BurstWindow = 200 * time.Millisecond
		o.MaxBurstBatch = 2
		o.DetachTimeout = 700 * time.Millisecond
	})

	recorder := &burstRecorder{}
	bot.OnNewMention(func(ctx context.Context, ev *chat.MessageEvent) error {
		if ev.Event.ID == "event-1" || ev.Event.ID == "event-2" {
			// Each first-batch member consumes most of its own budget: the
			// batch as a whole runs ~900ms, past one 700ms DetachTimeout.
			time.Sleep(450 * time.Millisecond)
		}
		return recorder.handle(ctx, ev)
	})

	for _, id := range []string{"event-1", "event-2", "event-3", "event-4"} {
		if status := postEvent(t, bot, "fake", mentionEvent(id, "fake:v1:thread-1")); status != 200 {
			t.Fatalf("dispatch %s: status %d", id, status)
		}
	}

	eventually(t, 10*time.Second, func() bool {
		return len(recorder.snapshot()) == 4
	}, "successor batch timed out while its healthy predecessor was executing")
	if strings.Contains(logs.String(), "chat burst wait abandoned") {
		t.Fatalf("successor batch's lock budget was consumed by its predecessor; logs:\n%s", logs.String())
	}
	if got := recorder.snapshot(); !equalStrings(got, []string{"event-1", "event-2", "event-3", "event-4"}) {
		t.Fatalf("batches ran out of order: %v", got)
	}
}

// Finding: per-member execution budget — every member's handler starts with a
// fresh DetachTimeout, regardless of how much of theirs earlier members used.
func TestBurstEveryMemberGetsItsOwnExecutionBudget(t *testing.T) {
	t.Parallel()

	state := newFakeState()
	adapter := newFakeAdapter("fake")
	var logs syncBuffer
	bot := newBurstRuntime(t, state, adapter, &logs, nil, func(o *chat.RuntimeOptions) {
		o.BurstWindow = 250 * time.Millisecond
		o.DetachTimeout = 400 * time.Millisecond
	})

	var remaining atomic.Int64
	recorder := &burstRecorder{}
	bot.OnNewMention(func(ctx context.Context, ev *chat.MessageEvent) error {
		if ev.Event.ID == "event-1" {
			// The first member consumes most of one DetachTimeout.
			time.Sleep(250 * time.Millisecond)
		}
		if ev.Event.ID == "event-2" {
			deadline, ok := ctx.Deadline()
			if !ok {
				t.Error("member context has no deadline")
			}
			remaining.Store(int64(time.Until(deadline)))
		}
		return recorder.handle(ctx, ev)
	})

	for _, id := range []string{"event-1", "event-2"} {
		if status := postEvent(t, bot, "fake", mentionEvent(id, "fake:v1:thread-1")); status != 200 {
			t.Fatalf("dispatch %s: status %d", id, status)
		}
	}
	eventually(t, 5*time.Second, func() bool {
		return len(recorder.snapshot()) == 2
	}, "batch members were not all handled")
	// A shared budget would leave event-2 ~150ms; a fresh one leaves ~400ms.
	if got := time.Duration(remaining.Load()); got < 300*time.Millisecond {
		t.Fatalf("second member inherited a consumed budget: %v remaining", got)
	}
}

// Finding: collection time is not execution time — the window wait consumes
// no DetachTimeout, so a window longer than DetachTimeout still dispatches
// its members with full budgets.
func TestBurstDispatchBudgetStartsWhenWindowCloses(t *testing.T) {
	t.Parallel()

	state := newFakeState()
	adapter := newFakeAdapter("fake")
	var logs syncBuffer
	observer := &recordingObserver{}
	bot := newBurstRuntime(t, state, adapter, &logs, observer, func(o *chat.RuntimeOptions) {
		// The window alone exceeds DetachTimeout: a budget that started at
		// admission would already be exhausted when the member runs.
		o.BurstWindow = 300 * time.Millisecond
		o.DetachTimeout = 150 * time.Millisecond
	})

	recorder := &burstRecorder{}
	bot.OnNewMention(func(ctx context.Context, ev *chat.MessageEvent) error {
		time.Sleep(80 * time.Millisecond)
		return recorder.handle(ctx, ev)
	})

	if status := postEvent(t, bot, "fake", mentionEvent("event-1", "fake:v1:thread-1")); status != 200 {
		t.Fatalf("dispatch event-1: status %d", status)
	}
	eventually(t, 5*time.Second, func() bool {
		return outcomeCounts(observer)[chat.OutcomeHandled] == 1
	}, "member's execution budget was consumed by the collection window")
}

// Finding: cooperative cancellation — a member that ignores cancellation until
// its DetachTimeout expires records a timeout outcome when it returns and does
// not starve the members behind it, which run with fresh budgets.
func TestBurstUncooperativeMemberTimesOutWithoutStarvingSuccessors(t *testing.T) {
	t.Parallel()

	state := newFakeState()
	adapter := newFakeAdapter("fake")
	var logs syncBuffer
	observer := &recordingObserver{}
	bot := newBurstRuntime(t, state, adapter, &logs, observer, func(o *chat.RuntimeOptions) {
		o.BurstWindow = 250 * time.Millisecond
		o.DetachTimeout = 250 * time.Millisecond
	})

	recorder := &burstRecorder{}
	bot.OnNewMention(func(ctx context.Context, ev *chat.MessageEvent) error {
		if ev.Event.ID == "event-1" {
			// Cooperative-cancellation worst case: the handler only yields
			// when its member context expires.
			<-ctx.Done()
			return ctx.Err()
		}
		return recorder.handle(ctx, ev)
	})

	for _, id := range []string{"event-1", "event-2"} {
		if status := postEvent(t, bot, "fake", mentionEvent(id, "fake:v1:thread-1")); status != 200 {
			t.Fatalf("dispatch %s: status %d", id, status)
		}
	}

	eventually(t, 5*time.Second, func() bool {
		counts := outcomeCounts(observer)
		return counts[chat.OutcomeError] == 1 && counts[chat.OutcomeHandled] == 1
	}, "timed-out member and its successor did not both close")
	if !strings.Contains(logs.String(), "chat deferred handler timed out") {
		t.Fatalf("member timeout was not surfaced; logs:\n%s", logs.String())
	}
	if got := recorder.snapshot(); !equalStrings(got, []string{"event-2"}) {
		t.Fatalf("successor member did not run after the timed-out member: %v", got)
	}
}

// Finding: mid-batch lease loss — losing the Lock Lease cancels the running
// member with ErrPreempted and skips the remaining members rather than
// running them unserialized.
func TestBurstLeaseLossCancelsRunningMemberAndSkipsRemaining(t *testing.T) {
	t.Parallel()

	state := newFakeState()
	adapter := newFakeAdapter("fake")
	var logs syncBuffer
	observer := &recordingObserver{}
	bot := newBurstRuntime(t, state, adapter, &logs, observer, func(o *chat.RuntimeOptions) {
		o.BurstWindow = 250 * time.Millisecond
		// A short TTL keeps the refresh cadence (TTL/2) fast so the loss is
		// detected mid-member.
		o.ThreadLockTTL = 120 * time.Millisecond
	})

	firstStarted := make(chan struct{})
	var preemptedErr atomic.Bool
	recorder := &burstRecorder{}
	bot.OnNewMention(func(ctx context.Context, ev *chat.MessageEvent) error {
		if ev.Event.ID == "event-1" {
			close(firstStarted)
			<-ctx.Done()
			preemptedErr.Store(errors.Is(context.Cause(ctx), chat.ErrPreempted))
			return ctx.Err()
		}
		return recorder.handle(ctx, ev)
	})

	for _, id := range []string{"event-1", "event-2", "event-3"} {
		if status := postEvent(t, bot, "fake", mentionEvent(id, "fake:v1:thread-1")); status != 200 {
			t.Fatalf("dispatch %s: status %d", id, status)
		}
	}
	select {
	case <-firstStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("first member never started")
	}
	// The lease vanishes out from under the batch mid-member.
	if expired, err := state.expireLock(context.Background(), "fake:v1:thread-1"); err != nil || !expired {
		t.Fatalf("expire lease: expired=%v err=%v", expired, err)
	}

	eventually(t, 5*time.Second, func() bool {
		counts := outcomeCounts(observer)
		return counts[chat.OutcomePreempted] == 1 && counts[chat.OutcomeIgnored] == 2
	}, "lease loss did not preempt the running member and skip the rest")
	if !preemptedErr.Load() {
		t.Fatal("running member's cancellation cause was not ErrPreempted")
	}
	if got := recorder.snapshot(); len(got) != 0 {
		t.Fatalf("members ran after the lease was lost: %v", got)
	}
	if !strings.Contains(logs.String(), "chat burst batch abandoned") || !strings.Contains(logs.String(), "remaining=2") {
		t.Fatalf("skipped members were not surfaced; logs:\n%s", logs.String())
	}
}

// Finding: observable terminal outcomes — every admitted member of an aborted
// batch reaches exactly one terminal outcome; none is dropped from telemetry
// and un-started members are not misreported as preempted.
func TestBurstAbortedBatchRecordsTerminalOutcomeForEveryMember(t *testing.T) {
	t.Parallel()

	state := newFakeState()
	adapter := newFakeAdapter("fake")
	var logs syncBuffer
	observer := &recordingObserver{}
	bot := newBurstRuntime(t, state, adapter, &logs, observer, func(o *chat.RuntimeOptions) {
		o.BurstWindow = 250 * time.Millisecond
		o.ThreadLockTTL = 120 * time.Millisecond
	})

	firstStarted := make(chan struct{})
	recorder := &burstRecorder{}
	bot.OnNewMention(func(ctx context.Context, ev *chat.MessageEvent) error {
		if ev.Event.ID == "event-1" {
			close(firstStarted)
			<-ctx.Done()
			return ctx.Err()
		}
		return recorder.handle(ctx, ev)
	})

	for _, id := range []string{"event-1", "event-2", "event-3"} {
		if status := postEvent(t, bot, "fake", mentionEvent(id, "fake:v1:thread-1")); status != 200 {
			t.Fatalf("dispatch %s: status %d", id, status)
		}
	}
	select {
	case <-firstStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("first member never started")
	}
	if expired, err := state.expireLock(context.Background(), "fake:v1:thread-1"); err != nil || !expired {
		t.Fatalf("expire lease: expired=%v err=%v", expired, err)
	}
	eventually(t, 5*time.Second, func() bool {
		return len(observer.terminalOutcomes()) == 3
	}, "aborted batch members were dropped from telemetry")

	// A healthy follow-up batch on the same scope still closes cleanly.
	if status := postEvent(t, bot, "fake", mentionEvent("event-4", "fake:v1:thread-1")); status != 200 {
		t.Fatalf("dispatch event-4: status %d", status)
	}
	eventually(t, 5*time.Second, func() bool {
		return len(observer.terminalOutcomes()) == 4
	}, "follow-up batch member did not close")

	counts := outcomeCounts(observer)
	if counts[chat.OutcomePreempted] != 1 || counts[chat.OutcomeIgnored] != 2 || counts[chat.OutcomeHandled] != 1 {
		t.Fatalf("terminal outcomes misreported: %v", counts)
	}
}

// Finding: handler-error member disposition — a member whose handler fails
// records the error and does not abort its batch while the lease is intact.
func TestBurstMemberHandlerErrorDoesNotAbortBatch(t *testing.T) {
	t.Parallel()

	state := newFakeState()
	adapter := newFakeAdapter("fake")
	var logs syncBuffer
	observer := &recordingObserver{}
	bot := newBurstRuntime(t, state, adapter, &logs, observer, func(o *chat.RuntimeOptions) {
		o.BurstWindow = 250 * time.Millisecond
	})

	recorder := &burstRecorder{}
	bot.OnNewMention(func(ctx context.Context, ev *chat.MessageEvent) error {
		if ev.Event.ID == "event-1" {
			return errors.New("boom")
		}
		return recorder.handle(ctx, ev)
	})

	for _, id := range []string{"event-1", "event-2"} {
		if status := postEvent(t, bot, "fake", mentionEvent(id, "fake:v1:thread-1")); status != 200 {
			t.Fatalf("dispatch %s: status %d", id, status)
		}
	}

	eventually(t, 5*time.Second, func() bool {
		counts := outcomeCounts(observer)
		return counts[chat.OutcomeError] == 1 && counts[chat.OutcomeHandled] == 1
	}, "failing member aborted its batch")
	if !observer.hasEvent(chat.ObsHandlerError) {
		t.Fatal("handler error was not observed")
	}
	if got := recorder.snapshot(); !equalStrings(got, []string{"event-2"}) {
		t.Fatalf("successor member did not run after the failing member: %v", got)
	}
}

// Finding: member-reference clearing — a disposed member's event payload is
// collectable while later members of the same batch still execute, so batch
// retention shrinks as the batch drains.
func TestBurstDisposedMemberReleasesEventForGC(t *testing.T) {
	t.Parallel()

	state := newFakeState()
	adapter := newFakeAdapter("fake")
	var logs syncBuffer
	bot := newBurstRuntime(t, state, adapter, &logs, nil, func(o *chat.RuntimeOptions) {
		o.BurstWindow = 250 * time.Millisecond
	})

	var weakEvent atomic.Pointer[weak.Pointer[chat.Event]]
	secondStarted := make(chan struct{})
	gate := make(chan struct{})
	var release sync.Once
	t.Cleanup(func() { release.Do(func() { close(gate) }) })
	bot.OnNewMention(func(ctx context.Context, ev *chat.MessageEvent) error {
		switch ev.Event.ID {
		case "event-1":
			ref := weak.Make(ev.Event)
			weakEvent.Store(&ref)
		case "event-2":
			close(secondStarted)
			<-gate
		}
		return nil
	})

	for _, id := range []string{"event-1", "event-2"} {
		if status := postEvent(t, bot, "fake", mentionEvent(id, "fake:v1:thread-1")); status != 200 {
			t.Fatalf("dispatch %s: status %d", id, status)
		}
	}
	select {
	case <-secondStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("second member never started")
	}

	// The first member is disposed while the second still executes: its event
	// payload must be unreachable.
	eventually(t, 10*time.Second, func() bool {
		runtime.GC()
		ref := weakEvent.Load()
		return ref != nil && ref.Value() == nil
	}, "disposed member's event payload was still retained mid-batch")
	release.Do(func() { close(gate) })
}

// Finding: admission slot retention through the batch tail — members free
// their slots at disposition, except the batch's final member, whose slot is
// held until the batch's lock cleanup (ReleaseLock) completes.
func TestBurstFinalMemberSlotHeldThroughLockRelease(t *testing.T) {
	t.Parallel()

	state := newBlockingReleaseState()
	adapter := newFakeAdapter("fake")
	var logs syncBuffer
	bot := newBurstRuntime(t, state, adapter, &logs, nil, func(o *chat.RuntimeOptions) {
		o.BurstWindow = 10 * time.Second
		o.MaxBurstBatch = 2
		o.MaxDetached = 2
	})
	var release sync.Once
	t.Cleanup(func() { release.Do(func() { close(state.gate) }) })

	recorder := &burstRecorder{}
	bot.OnNewMention(recorder.handle)

	// Two members fill both admission slots and cap-seal one batch.
	for _, id := range []string{"event-1", "event-2"} {
		if status := postEvent(t, bot, "fake", mentionEvent(id, "fake:v1:thread-1")); status != 200 {
			t.Fatalf("dispatch %s: status %d", id, status)
		}
	}
	select {
	case <-state.started:
	case <-time.After(5 * time.Second):
		t.Fatal("batch lock release never started")
	}

	// Both members completed but ReleaseLock is still in flight: the first
	// member's slot is free, the final member's is not.
	if status := postEvent(t, bot, "fake", mentionEvent("probe-1", "fake:v1:thread-1")); status != 200 {
		t.Fatalf("first slot was not freed at member disposition: status %d", status)
	}
	status, body := postEventBody(t, bot, "fake", mentionEvent("probe-2", "fake:v1:thread-1"))
	if status == 200 || !strings.Contains(body, "admission rejected") {
		t.Fatalf("final member's slot was freed before lock cleanup completed: status %d body %q", status, body)
	}

	release.Do(func() { close(state.gate) })
	// With the cleanup finished, the final slot frees: the next probe both
	// admits and cap-seals the probe window so it dispatches.
	eventually(t, 5*time.Second, func() bool {
		status, _ := postEventBody(t, bot, "fake", mentionEvent("probe-3", "fake:v1:thread-1"))
		return status == 200
	}, "final member's slot never freed after lock cleanup")
	eventually(t, 5*time.Second, func() bool {
		return len(recorder.snapshot()) >= 4
	}, "probe batch never dispatched after the final slot freed")
}

// ADR 0015 bounded retention: parked burst members occupy Admission Bound
// slots, so a window full of parked payloads saturates MaxDetached and new
// deliveries are honestly rejected until the batch disposes.
func TestBurstParkedMembersOccupyAdmissionSlots(t *testing.T) {
	t.Parallel()

	state := newFakeState()
	adapter := newFakeAdapter("fake")
	var logs syncBuffer
	observer := &recordingObserver{}
	bot := newBurstRuntime(t, state, adapter, &logs, observer, func(o *chat.RuntimeOptions) {
		o.BurstWindow = 400 * time.Millisecond
		o.MaxDetached = 2
	})

	recorder := &burstRecorder{}
	bot.OnNewMention(recorder.handle)

	for _, id := range []string{"event-1", "event-2"} {
		if status := postEvent(t, bot, "fake", mentionEvent(id, "fake:v1:thread-1")); status != 200 {
			t.Fatalf("dispatch %s: status %d", id, status)
		}
	}
	// Both slots are held by parked members: the next delivery is rejected
	// before acknowledgement and before dedupe marking.
	status, body := postEventBody(t, bot, "fake", mentionEvent("event-3", "fake:v1:thread-1"))
	if status == 200 || !strings.Contains(body, "admission rejected") {
		t.Fatalf("parked members did not occupy admission slots: status %d body %q", status, body)
	}
	if !observer.hasEvent(chat.ObsAdmissionRejected) {
		t.Fatal("admission rejection was not observed")
	}

	// Once the batch disposes, capacity frees and the same event admits (it
	// was never dedupe-marked).
	eventually(t, 5*time.Second, func() bool {
		return len(recorder.snapshot()) == 2
	}, "parked batch never dispatched")
	eventually(t, 5*time.Second, func() bool {
		status, _ := postEventBody(t, bot, "fake", mentionEvent("event-3", "fake:v1:thread-1"))
		return status == 200
	}, "capacity never freed after the batch disposed")
}

// Finding: open-window shutdown drain — Runtime Shutdown disposes parked
// members promptly and observably instead of waiting out their window or
// dropping them silently.
func TestBurstOpenWindowDrainedByShutdown(t *testing.T) {
	t.Parallel()

	state := newFakeState()
	adapter := newFakeAdapter("fake")
	var logs syncBuffer
	observer := &recordingObserver{}
	bot := newBurstRuntime(t, state, adapter, &logs, observer, func(o *chat.RuntimeOptions) {
		o.BurstWindow = 10 * time.Second
	})

	recorder := &burstRecorder{}
	bot.OnNewMention(recorder.handle)

	for _, id := range []string{"event-1", "event-2"} {
		if status := postEvent(t, bot, "fake", mentionEvent(id, "fake:v1:thread-1")); status != 200 {
			t.Fatalf("dispatch %s: status %d", id, status)
		}
	}

	start := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := bot.Shutdown(ctx); err != nil {
		t.Fatalf("shutdown with an open window: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("shutdown waited out the collection window: %v", elapsed)
	}
	counts := outcomeCounts(observer)
	if counts[chat.OutcomeIgnored] != 2 {
		t.Fatalf("parked members were not observably disposed: %v", counts)
	}
	if !strings.Contains(logs.String(), "chat burst wait abandoned") {
		t.Fatalf("abandoned members were not surfaced; logs:\n%s", logs.String())
	}
	if got := chat.BurstScopeCount(bot); got != 0 {
		t.Fatalf("burst scope state survived shutdown: %d entries", got)
	}
	if got := recorder.snapshot(); len(got) != 0 {
		t.Fatalf("members ran during shutdown drain: %v", got)
	}
}

// Finding: admission-to-drain race — deliveries racing Runtime Shutdown are
// either honestly rejected or admitted and drained to a terminal outcome; no
// burst state survives shutdown.
func TestBurstDispatchRacingShutdownRejectedOrDrained(t *testing.T) {
	t.Parallel()

	state := newFakeState()
	adapter := newFakeAdapter("fake")
	var logs syncBuffer
	observer := &recordingObserver{}
	bot := newBurstRuntime(t, state, adapter, &logs, observer, func(o *chat.RuntimeOptions) {
		o.BurstWindow = 30 * time.Millisecond
	})
	bot.OnNewMention(func(context.Context, *chat.MessageEvent) error { return nil })

	const deliveries = 12
	statuses := make([]int, deliveries)
	bodies := make([]string, deliveries)
	var wg sync.WaitGroup
	for i := 0; i < deliveries; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			id := "race-event-" + string(rune('a'+i))
			statuses[i], bodies[i] = postEventBody(t, bot, "fake", mentionEvent(id, "fake:v1:thread-race"))
		}(i)
	}
	time.Sleep(10 * time.Millisecond)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := bot.Shutdown(ctx); err != nil {
		t.Fatalf("shutdown racing dispatch: %v", err)
	}
	wg.Wait()

	for i, status := range statuses {
		if status != 200 && !strings.Contains(bodies[i], "admission rejected") {
			t.Fatalf("delivery %d neither admitted nor honestly rejected: status %d body %q", i, status, bodies[i])
		}
	}
	// Every opened dispatch span closes: admitted deliveries drained to a
	// terminal outcome, rejected ones closed as admission-rejected.
	eventually(t, 5*time.Second, func() bool {
		return len(observer.terminalOutcomes()) == deliveries
	}, "a racing delivery was lost without a terminal outcome")
	if got := chat.BurstScopeCount(bot); got != 0 {
		t.Fatalf("burst scope state survived shutdown: %d entries", got)
	}
}

// Finding: idle-coordinator GC — a scope whose batches all disposed retains no
// coordinator map entry, so scope cardinality cannot leak memory.
func TestBurstIdleScopeCoordinatorRemoved(t *testing.T) {
	t.Parallel()

	state := newFakeState()
	adapter := newFakeAdapter("fake")
	var logs syncBuffer
	bot := newBurstRuntime(t, state, adapter, &logs, nil, func(o *chat.RuntimeOptions) {
		o.BurstWindow = 40 * time.Millisecond
	})

	recorder := &burstRecorder{}
	bot.OnNewMention(recorder.handle)

	threads := []chat.ThreadID{"fake:v1:t1", "fake:v1:t2", "fake:v1:t3", "fake:v1:t4", "fake:v1:t5"}
	for i, thread := range threads {
		if status := postEvent(t, bot, "fake", mentionEvent("event-"+string(rune('a'+i)), thread)); status != 200 {
			t.Fatalf("dispatch to %s: status %d", thread, status)
		}
	}
	eventually(t, 5*time.Second, func() bool {
		return len(recorder.snapshot()) == len(threads)
	}, "scope batches were not all dispatched")
	eventually(t, 5*time.Second, func() bool {
		return chat.BurstScopeCount(bot) == 0
	}, "idle scope coordinators were not garbage collected")
}
