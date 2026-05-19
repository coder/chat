package memory

import (
	"context"
	"sync"
	"testing"
	"time"
)

func TestLockOperationRejectsShutdownRequestedWhileWaitingForMutex(t *testing.T) {
	state := New()
	state.mu.Lock()

	checkedContext := make(chan struct{})
	ctx := &operationCheckedContext{
		Context: context.Background(),
		checked: checkedContext,
	}
	done := make(chan error, 1)
	go func() {
		err := state.lockOperation(ctx)
		if err == nil {
			state.mu.Unlock()
		}
		done <- err
	}()

	select {
	case <-checkedContext:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for operation to start")
	}

	shutdownDone := make(chan error, 1)
	go func() {
		shutdownDone <- state.Shutdown(context.Background())
	}()
	waitForShutdownRequested(t, state)
	state.mu.Unlock()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("expected waiting operation to be rejected after shutdown")
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for operation")
	}

	select {
	case err := <-shutdownDone:
		if err != nil {
			t.Fatalf("shutdown: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for shutdown")
	}
}

type operationCheckedContext struct {
	context.Context
	once    sync.Once
	checked chan<- struct{}
}

func (c *operationCheckedContext) Err() error {
	c.once.Do(func() {
		close(c.checked)
	})
	return c.Context.Err()
}

func waitForShutdownRequested(t *testing.T, state *State) {
	t.Helper()
	deadline := time.NewTimer(time.Second)
	defer deadline.Stop()
	tick := time.NewTicker(time.Millisecond)
	defer tick.Stop()

	for {
		if state.closed.Load() {
			return
		}
		select {
		case <-deadline.C:
			t.Fatal("timed out waiting for shutdown request")
		case <-tick.C:
		}
	}
}
