package chat_test

import (
	"net/http"
	"testing"
	"time"

	"github.com/coder/chat"
)

func TestQueueIdleScopeRetainsNoTurnRecord(t *testing.T) {
	t.Parallel()

	for _, tc := range queueDispatchModes {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			const bFirstPoll = 3 // after the prelude acquires of A and B
			state := newGatedAcquireState(0)
			run := newGatedQueueRun(t, state, tc.mode, "A")

			run.post("A")
			waitClosed(t, run.blockStarted, "A's handler did not start")
			run.post("B")
			eventually(t, 2*time.Second, func() bool { return state.acquireCalls() >= bFirstPoll },
				"B did not park and poll for the lock")
			if res := postEventResultFor(run.bot, "fake", mentionEvent("D", "fake:v1:thread-2")); res.err != nil || res.status != http.StatusOK {
				t.Fatalf("D: status=%d err=%v", res.status, res.err)
			}
			eventually(t, 2*time.Second, func() bool { return chat.QueueTurnCount(run.bot) == 1 },
				"D's scope kept its turn record after D finished")

			close(run.releaseBlock)
			eventually(t, 4*time.Second, func() bool { return len(run.handledIDs()) == 3 },
				"B did not run after A")
			if res := <-run.post("A"); res.err != nil || res.status != http.StatusOK {
				t.Fatalf("redelivery of A: status=%d err=%v", res.status, res.err)
			}
			eventually(t, 2*time.Second, func() bool { return chat.QueueTurnCount(run.bot) == 0 },
				"an idle scope kept its turn record")
		})
	}
}

func TestQueueDeferredReleaseCalledTwiceLeavesItsScopeOnce(t *testing.T) {
	t.Parallel()

	var logs syncBuffer
	bot := newQueueRuntime(t, newFakeState(), newFakeAdapter("fake"), &logs)
	first := chat.QueueDeferredRelease(bot, "scope-1")
	second := chat.QueueDeferredRelease(bot, "scope-1")

	first()
	first()
	if got := chat.QueueTurnCount(bot); got != 1 {
		t.Fatalf("turn records = %d after one of two deliveries released twice, want 1", got)
	}
	second()
	second()
	if got := chat.QueueTurnCount(bot); got != 0 {
		t.Fatalf("turn records = %d after both deliveries released, want 0", got)
	}
}

type recoveredPost struct {
	postEventResult
	panicked any
}

// postRecovering posts id to the queue thread and recovers a panic from
// dispatch, as net/http does for a handler.
func (r *gatedQueueRun) postRecovering(id string) <-chan recoveredPost {
	done := make(chan recoveredPost, 1)
	go func() {
		var res recoveredPost
		defer func() {
			res.panicked = recover()
			done <- res
		}()
		res.postEventResult = postEventResultFor(r.bot, "fake", mentionEvent(id, queueThread))
	}()
	return done
}

func receivePost(t *testing.T, done <-chan recoveredPost, name string) recoveredPost {
	t.Helper()
	select {
	case res := <-done:
		return res
	case <-time.After(2 * time.Second):
		t.Fatalf("%s did not return", name)
		return recoveredPost{}
	}
}

func TestQueueStatePanicWhileWaiterPollsLeavesNoStaleWaiter(t *testing.T) {
	t.Parallel()

	const (
		bFirstPoll = 3 // after the prelude acquires of A and B
		statePanic = "state AcquireLock panicked"
	)
	state := newGatedAcquireState(bFirstPoll)
	state.gatePanic = statePanic
	run := newGatedQueueRun(t, state, chat.DispatchSync, "A")

	a := run.postRecovering("A")
	waitClosed(t, run.blockStarted, "A's handler did not start")
	b := run.postRecovering("B")
	waitClosed(t, state.reached, "B did not poll for the lock")
	close(run.releaseBlock)
	if res := receivePost(t, a, "A"); res.panicked != nil || res.status != http.StatusOK {
		t.Fatalf("A: status=%d panic=%v", res.status, res.panicked)
	}

	close(state.release)
	if res := receivePost(t, b, "B"); res.panicked != statePanic {
		t.Errorf("B's dispatch panicked with %v, want the State's own panic", res.panicked)
	}
	if res := receivePost(t, run.postRecovering("A"), "the redelivery of A"); res.panicked != nil || res.status != http.StatusOK {
		t.Fatalf("the redelivery of A after the State panic: status=%d panic=%v", res.status, res.panicked)
	}
	if got := chat.QueueTurnCount(run.bot); got != 0 {
		t.Fatalf("turn records = %d after every delivery finished, want 0", got)
	}
}
