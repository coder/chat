package memory_test

import (
	"testing"

	"github.com/coder/chat/internal/statetest"
	"github.com/coder/chat/state/memory"
)

func TestStateConformance(t *testing.T) {
	statetest.RunStateConformance(t, func(t *testing.T) statetest.Harness {
		t.Helper()
		return statetest.Harness{State: memory.New()}
	})
}
