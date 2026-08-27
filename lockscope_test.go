package chat_test

import (
	"context"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/chat"
)

// channelAdapter wraps fakeAdapter so thread ids of the form
// "fake:v1:<channel>:<thread>" report a channel on their ThreadRef; shorter ids
// and ids carrying the runtime's synthesized channel-key prefix (used to prove
// fallback keys cannot collide) report no channel.
type channelAdapter struct {
	*fakeAdapter
}

func (a *channelAdapter) ValidateThreadID(id chat.ThreadID) (chat.ThreadRef, error) {
	ref, err := a.fakeAdapter.ValidateThreadID(id)
	if err != nil {
		return ref, err
	}
	ref.Channel = ""
	if parts := strings.Split(string(id), ":"); len(parts) >= 4 && !strings.HasPrefix(string(id), "channel-scope/") {
		ref.Channel = parts[2]
	}
	return ref, nil
}

func newLockScopeRuntime(t *testing.T, state chat.State, adapter chat.Adapter, logs *syncBuffer, mutate ...func(*chat.RuntimeOptions)) *chat.Chat {
	t.Helper()
	options := chat.RuntimeOptions{
		DedupeTTL:     time.Hour,
		ThreadLockTTL: time.Hour,
		Concurrency:   chat.ConcurrencyDrop,
		LockScope:     chat.LockScopeChannel,
		Dispatch:      chat.DispatchDeferred,
		MaxDetached:   1024,
		DetachTimeout: 5 * time.Second,
	}
	for _, m := range mutate {
		m(&options)
	}
	bot, err := chat.New(context.Background(),
		chat.WithState(state),
		chat.WithAdapter(adapter),
		chat.WithLogger(slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))),
		chat.WithRuntimeOptions(options),
	)
	if err != nil {
		t.Fatalf("new lock scope runtime: %v", err)
	}
	return bot
}

func TestChannelScopeSerializesThreadsSharingAChannel(t *testing.T) {
	t.Parallel()

	state := newFakeState()
	adapter := &channelAdapter{fakeAdapter: newFakeAdapter("fake")}
	var logs syncBuffer
	bot := newLockScopeRuntime(t, state, adapter, &logs)

	firstStarted := make(chan struct{})
	releaseFirst := make(chan struct{})
	defer close(releaseFirst)
	var mu sync.Mutex
	var handled []string
	bot.OnNewMention(func(ctx context.Context, ev *chat.MessageEvent) error {
		mu.Lock()
		first := len(handled) == 0
		handled = append(handled, ev.Event.ID)
		mu.Unlock()
		if first {
			close(firstStarted)
			<-releaseFirst
		}
		return nil
	})

	if status := postEvent(t, bot, "fake", mentionEvent("first", "fake:v1:chan-a:thread-1")); status != http.StatusOK {
		t.Fatalf("first status = %d", status)
	}
	select {
	case <-firstStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("first handler did not start")
	}

	// A DIFFERENT thread in the SAME channel is a Lock Conflict under channel
	// scope and drops.
	if status := postEvent(t, bot, "fake", mentionEvent("same-channel", "fake:v1:chan-a:thread-2")); status != http.StatusOK {
		t.Fatalf("same-channel status = %d", status)
	}
	deadline := time.After(2 * time.Second)
	for !strings.Contains(logs.String(), "chat lock conflict dropped") {
		select {
		case <-deadline:
			t.Fatalf("channel-scope conflict not surfaced; logs:\n%s", logs.String())
		case <-time.After(5 * time.Millisecond):
		}
	}

	// A thread in ANOTHER channel does not conflict and runs.
	if status := postEvent(t, bot, "fake", mentionEvent("other-channel", "fake:v1:chan-b:thread-3")); status != http.StatusOK {
		t.Fatalf("other-channel status = %d", status)
	}
	deadline = time.After(2 * time.Second)
	for {
		mu.Lock()
		n := len(handled)
		mu.Unlock()
		if n >= 2 {
			break
		}
		select {
		case <-deadline:
			t.Fatal("other-channel event did not run despite a distinct channel scope")
		case <-time.After(5 * time.Millisecond):
		}
	}

	mu.Lock()
	defer mu.Unlock()
	if !equalStrings(handled, []string{"first", "other-channel"}) {
		t.Fatalf("handled = %v, want [first other-channel] (same-channel dropped)", handled)
	}
}

func TestThreadScopeDefaultDoesNotSerializeAcrossThreads(t *testing.T) {
	t.Parallel()

	state := newFakeState()
	adapter := &channelAdapter{fakeAdapter: newFakeAdapter("fake")}
	var logs syncBuffer
	// Identical setup, but with the default thread scope: two threads in one
	// channel never conflict.
	bot := newLockScopeRuntime(t, state, adapter, &logs, func(o *chat.RuntimeOptions) {
		o.LockScope = chat.LockScopeThread
	})

	firstStarted := make(chan struct{})
	releaseFirst := make(chan struct{})
	defer close(releaseFirst)
	var mu sync.Mutex
	var handled []string
	bot.OnNewMention(func(ctx context.Context, ev *chat.MessageEvent) error {
		mu.Lock()
		first := len(handled) == 0
		handled = append(handled, ev.Event.ID)
		mu.Unlock()
		if first {
			close(firstStarted)
			<-releaseFirst
		}
		return nil
	})

	if status := postEvent(t, bot, "fake", mentionEvent("first", "fake:v1:chan-a:thread-1")); status != http.StatusOK {
		t.Fatalf("first status = %d", status)
	}
	select {
	case <-firstStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("first handler did not start")
	}

	if status := postEvent(t, bot, "fake", mentionEvent("sibling", "fake:v1:chan-a:thread-2")); status != http.StatusOK {
		t.Fatalf("sibling status = %d", status)
	}
	deadline := time.After(2 * time.Second)
	for {
		mu.Lock()
		n := len(handled)
		mu.Unlock()
		if n >= 2 {
			break
		}
		select {
		case <-deadline:
			t.Fatal("sibling thread blocked under the default thread scope")
		case <-time.After(5 * time.Millisecond):
		}
	}
	if strings.Contains(logs.String(), "chat lock conflict dropped") {
		t.Fatalf("thread scope surfaced a cross-thread conflict; logs:\n%s", logs.String())
	}
}

func TestChannelScopeFallsBackToThreadWhenChannelEmpty(t *testing.T) {
	t.Parallel()

	state := newFakeState()
	adapter := &channelAdapter{fakeAdapter: newFakeAdapter("fake")}
	var logs syncBuffer
	bot := newLockScopeRuntime(t, state, adapter, &logs)

	firstStarted := make(chan struct{})
	releaseFirst := make(chan struct{})
	defer close(releaseFirst)
	var mu sync.Mutex
	var handled []string
	bot.OnNewMention(func(ctx context.Context, ev *chat.MessageEvent) error {
		mu.Lock()
		first := len(handled) == 0
		handled = append(handled, ev.Event.ID)
		mu.Unlock()
		if first {
			close(firstStarted)
			<-releaseFirst
		}
		return nil
	})

	// Short thread ids report no channel: each falls back to its own
	// per-thread key rather than sharing one adapter-wide lock.
	if status := postEvent(t, bot, "fake", mentionEvent("first", "fake:v1:thread-1")); status != http.StatusOK {
		t.Fatalf("first status = %d", status)
	}
	select {
	case <-firstStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("first handler did not start")
	}

	if status := postEvent(t, bot, "fake", mentionEvent("second", "fake:v1:thread-2")); status != http.StatusOK {
		t.Fatalf("second status = %d", status)
	}
	deadline := time.After(2 * time.Second)
	for {
		mu.Lock()
		n := len(handled)
		mu.Unlock()
		if n >= 2 {
			break
		}
		select {
		case <-deadline:
			t.Fatal("channelless threads shared one lock; fallback to thread scope failed")
		case <-time.After(5 * time.Millisecond):
		}
	}
}

func TestChannelScopeFallbackKeyCannotCollideWithChannelKey(t *testing.T) {
	t.Parallel()

	state := newFakeState()
	adapter := &channelAdapter{fakeAdapter: newFakeAdapter("fake")}
	var logs syncBuffer
	bot := newLockScopeRuntime(t, state, adapter, &logs)

	firstStarted := make(chan struct{})
	releaseFirst := make(chan struct{})
	defer close(releaseFirst)
	var mu sync.Mutex
	var handled []string
	bot.OnNewMention(func(ctx context.Context, ev *chat.MessageEvent) error {
		mu.Lock()
		first := len(handled) == 0
		handled = append(handled, ev.Event.ID)
		mu.Unlock()
		if first {
			close(firstStarted)
			<-releaseFirst
		}
		return nil
	})

	// The first event locks the synthesized key for channel chan-a.
	if status := postEvent(t, bot, "fake", mentionEvent("first", "fake:v1:chan-a:thread-1")); status != http.StatusOK {
		t.Fatalf("first status = %d", status)
	}
	select {
	case <-firstStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("first handler did not start")
	}

	// A channelless thread whose opaque ID is exactly the synthesized channel
	// key must not be treated as contention on chan-a.
	collider := mentionEvent("collider", chat.ThreadID("channel-scope/4:fake/6:tenant/6:chan-a"))
	if status := postEvent(t, bot, "fake", collider); status != http.StatusOK {
		t.Fatalf("collider status = %d", status)
	}
	deadline := time.After(2 * time.Second)
	for {
		mu.Lock()
		n := len(handled)
		mu.Unlock()
		if n >= 2 {
			break
		}
		select {
		case <-deadline:
			t.Fatalf("crafted thread id collided with the channel key; logs:\n%s", logs.String())
		case <-time.After(5 * time.Millisecond):
		}
	}
	if strings.Contains(logs.String(), "chat lock conflict dropped") {
		t.Fatalf("fallback key collided with a channel key; logs:\n%s", logs.String())
	}
}

func TestLockScopeConstructionValidation(t *testing.T) {
	t.Parallel()

	_, err := chat.New(context.Background(),
		chat.WithState(newFakeState()),
		chat.WithAdapter(newFakeAdapter("fake")),
		chat.WithRuntimeOptions(chat.RuntimeOptions{
			DedupeTTL:     time.Hour,
			ThreadLockTTL: time.Hour,
			LockScope:     chat.LockScope(99),
		}),
	)
	if err == nil {
		t.Fatal("expected an unknown lock scope to fail construction")
	}
}
