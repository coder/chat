package chat_test

import (
	"context"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/chat"
)

// newBurstRuntime builds a burst runtime with a short collection window and a
// fast lock poll cadence, optional observer, and optional option mutations.
func newBurstRuntime(t *testing.T, state chat.State, adapter chat.Adapter, logs *syncBuffer, observer chat.Observer, mutate ...func(*chat.RuntimeOptions)) *chat.Chat {
	t.Helper()
	options := chat.RuntimeOptions{
		DedupeTTL:   time.Hour,
		Concurrency: chat.ConcurrencyBurst,
		BurstWindow: 60 * time.Millisecond,
		Dispatch:    chat.DispatchDeferred,
		MaxDetached: 1024,
		// ThreadLockTTL/20 is the lock poll cadence, so a short TTL keeps
		// batch lock acquisition prompt in tests.
		ThreadLockTTL: 200 * time.Millisecond,
		DetachTimeout: 5 * time.Second,
	}
	for _, m := range mutate {
		m(&options)
	}
	opts := []chat.Option{
		chat.WithState(state),
		chat.WithAdapter(adapter),
		chat.WithLogger(slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))),
		chat.WithRuntimeOptions(options),
	}
	if observer != nil {
		opts = append(opts, chat.WithObserver(observer))
	}
	bot, err := chat.New(context.Background(), opts...)
	if err != nil {
		t.Fatalf("new burst runtime: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := bot.Shutdown(ctx); err != nil {
			t.Errorf("shutdown burst runtime: %v", err)
		}
	})
	return bot
}

// burstRecorder tracks handled event IDs in handler completion order.
type burstRecorder struct {
	mu      sync.Mutex
	handled []string
}

func (r *burstRecorder) handle(_ context.Context, ev *chat.MessageEvent) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.handled = append(r.handled, ev.Event.ID)
	return nil
}

func (r *burstRecorder) snapshot() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.handled...)
}

func TestBurstDispatchesCollectedBatchInJoinOrder(t *testing.T) {
	t.Parallel()

	state := newFakeState()
	adapter := newFakeAdapter("fake")
	var logs syncBuffer
	bot := newBurstRuntime(t, state, adapter, &logs, nil, func(o *chat.RuntimeOptions) {
		o.BurstWindow = 250 * time.Millisecond
	})

	recorder := &burstRecorder{}
	bot.OnNewMention(recorder.handle)

	// Three events arrive well inside one collection window: they dispatch as
	// one batch, in join order, under a single Thread Lock hold.
	for _, id := range []string{"event-1", "event-2", "event-3"} {
		if status := postEvent(t, bot, "fake", mentionEvent(id, "fake:v1:thread-1")); status != 200 {
			t.Fatalf("dispatch %s: status %d", id, status)
		}
	}

	eventually(t, 5*time.Second, func() bool {
		return len(recorder.snapshot()) == 3
	}, "batch members were not all handled")
	if got := recorder.snapshot(); !equalStrings(got, []string{"event-1", "event-2", "event-3"}) {
		t.Fatalf("batch ran out of join order: %v", got)
	}
	if !strings.Contains(logs.String(), "chat burst batch dispatch") || !strings.Contains(logs.String(), "size=3") {
		t.Fatalf("expected one size-3 batch dispatch; logs:\n%s", logs.String())
	}
}

func TestBurstWindowOpenedDuringDispatchRunsAfterCurrentBatch(t *testing.T) {
	t.Parallel()

	state := newFakeState()
	adapter := newFakeAdapter("fake")
	var logs syncBuffer
	bot := newBurstRuntime(t, state, adapter, &logs, nil)

	started := make(chan string, 8)
	gate := make(chan struct{})
	var mu sync.Mutex
	var handled []string
	bot.OnNewMention(func(ctx context.Context, ev *chat.MessageEvent) error {
		started <- ev.Event.ID
		if ev.Event.ID == "event-1" {
			<-gate
		}
		mu.Lock()
		handled = append(handled, ev.Event.ID)
		mu.Unlock()
		return nil
	})

	if status := postEvent(t, bot, "fake", mentionEvent("event-1", "fake:v1:thread-1")); status != 200 {
		t.Fatalf("dispatch event-1: status %d", status)
	}
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("first batch never started")
	}

	// The first batch is mid-dispatch (its member blocked): new arrivals open
	// a successor window that dispatches strictly after the current batch.
	if status := postEvent(t, bot, "fake", mentionEvent("event-2", "fake:v1:thread-1")); status != 200 {
		t.Fatalf("dispatch event-2: status %d", status)
	}
	if status := postEvent(t, bot, "fake", mentionEvent("event-3", "fake:v1:thread-1")); status != 200 {
		t.Fatalf("dispatch event-3: status %d", status)
	}
	close(gate)

	eventually(t, 5*time.Second, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(handled) == 3
	}, "successor window members were not handled")
	mu.Lock()
	got := append([]string(nil), handled...)
	mu.Unlock()
	if got[0] != "event-1" {
		t.Fatalf("successor batch overtook the dispatching batch: %v", got)
	}
	if got[1] == "event-1" || got[2] == "event-1" {
		t.Fatalf("first batch member ran twice: %v", got)
	}
}

func TestBurstConstructionValidation(t *testing.T) {
	t.Parallel()

	base := func() chat.RuntimeOptions {
		return chat.RuntimeOptions{
			DedupeTTL:     time.Hour,
			ThreadLockTTL: time.Minute,
			Concurrency:   chat.ConcurrencyBurst,
			BurstWindow:   50 * time.Millisecond,
			Dispatch:      chat.DispatchDeferred,
			MaxDetached:   1024,
			DetachTimeout: time.Second,
		}
	}
	cases := []struct {
		name    string
		mutate  func(*chat.RuntimeOptions)
		wantErr string
	}{
		{
			name:    "zero burst window",
			mutate:  func(o *chat.RuntimeOptions) { o.BurstWindow = 0 },
			wantErr: "burst window must be positive",
		},
		{
			name:    "negative burst window",
			mutate:  func(o *chat.RuntimeOptions) { o.BurstWindow = -time.Second },
			wantErr: "burst window must be positive",
		},
		{
			name:    "negative max burst batch",
			mutate:  func(o *chat.RuntimeOptions) { o.MaxBurstBatch = -1 },
			wantErr: "max burst batch must not be negative",
		},
		{
			name:    "synchronous dispatch",
			mutate:  func(o *chat.RuntimeOptions) { o.Dispatch = chat.DispatchSync },
			wantErr: "burst strategy requires deferred dispatch",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			options := base()
			tc.mutate(&options)
			_, err := chat.New(context.Background(),
				chat.WithState(newFakeState()),
				chat.WithAdapter(newFakeAdapter("fake")),
				chat.WithRuntimeOptions(options),
			)
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("expected %q construction error, got %v", tc.wantErr, err)
			}
		})
	}
}
