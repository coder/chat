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
