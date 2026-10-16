package chat_test

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/chat"
)

// listenerAdapter is a fake adapter with the Listener capability. listen runs
// for each Listen call.
type listenerAdapter struct {
	*fakeAdapter
	listen func(ctx context.Context, dispatch chat.DispatchFunc) error
	calls  atomic.Int32
}

var _ chat.Listener = (*listenerAdapter)(nil)

func (a *listenerAdapter) Listen(ctx context.Context, dispatch chat.DispatchFunc) error {
	a.calls.Add(1)
	return a.listen(ctx, dispatch)
}

func newListenRuntime(t *testing.T, state chat.State, adapter chat.Adapter, observer chat.Observer, mode chat.DispatchMode) *chat.Chat {
	t.Helper()
	options := chat.DefaultRuntimeOptions()
	options.Dispatch = mode
	options.DetachTimeout = 5 * time.Second
	bot, err := chat.New(context.Background(),
		chat.WithState(state),
		chat.WithAdapter(adapter),
		chat.WithLogger(slog.New(slog.NewTextHandler(newSyncBuffer(), nil))),
		chat.WithObserver(observer),
		chat.WithRuntimeOptions(options),
	)
	if err != nil {
		t.Fatalf("new listen runtime: %v", err)
	}
	return bot
}

// receive returns the next value from ch, or fails the test after 5 seconds.
func receive[T any](t *testing.T, ch <-chan T, what string) (v T) {
	t.Helper()
	select {
	case v = <-ch:
	case <-time.After(5 * time.Second):
		t.Fatalf("timed out waiting for %s", what)
	}
	return v
}

func TestListenRejectsUnknownAdapterAndMissingCapability(t *testing.T) {
	t.Parallel()

	bot := newListenRuntime(t, newFakeState(), newFakeAdapter("fake"), &recordingObserver{}, chat.DispatchSync)

	err := bot.Listen(t.Context(), "missing")
	_, webhookErr := bot.Webhook("missing")
	if err == nil || err.Error() != webhookErr.Error() {
		t.Fatalf("Listen(missing) = %v, want %v", err, webhookErr)
	}

	err = bot.Listen(t.Context(), "fake")
	if !errors.Is(err, chat.ErrUnsupportedCapability) || !strings.Contains(err.Error(), `"fake"`) {
		t.Fatalf("Listen(fake) = %v, want ErrUnsupportedCapability that names the adapter", err)
	}
}

func TestListenAfterShutdownReturnsNilWithoutCallingAdapter(t *testing.T) {
	t.Parallel()

	adapter := &listenerAdapter{
		fakeAdapter: newFakeAdapter("fake"),
		listen: func(context.Context, chat.DispatchFunc) error {
			return errors.New("adapter Listen called after Shutdown")
		},
	}
	bot := newListenRuntime(t, newFakeState(), adapter, &recordingObserver{}, chat.DispatchSync)
	if err := bot.Shutdown(t.Context()); err != nil {
		t.Fatalf("shutdown: %v", err)
	}

	if err := bot.Listen(t.Context(), "fake"); err != nil {
		t.Fatalf("Listen after Shutdown = %v, want nil", err)
	}
	if calls := adapter.calls.Load(); calls != 0 {
		t.Fatalf("adapter Listen calls = %d, want 0", calls)
	}
}

func TestListenReturnsNilOnCancelAndAdapterFatalError(t *testing.T) {
	t.Parallel()

	t.Run("cancel", func(t *testing.T) {
		t.Parallel()
		started := make(chan struct{})
		adapter := &listenerAdapter{
			fakeAdapter: newFakeAdapter("fake"),
			listen: func(ctx context.Context, _ chat.DispatchFunc) error {
				close(started)
				<-ctx.Done()
				return nil
			},
		}
		bot := newListenRuntime(t, newFakeState(), adapter, &recordingObserver{}, chat.DispatchSync)

		ctx, cancel := context.WithCancel(t.Context())
		listenDone := make(chan error, 1)
		go func() { listenDone <- bot.Listen(ctx, "fake") }()
		receive(t, started, "adapter Listen to start")
		cancel()
		if err := receive(t, listenDone, "Listen to return after cancel"); err != nil {
			t.Fatalf("Listen after cancel = %v, want nil", err)
		}
	})

	t.Run("fatal", func(t *testing.T) {
		t.Parallel()
		errFatal := errors.New("link disabled")
		adapter := &listenerAdapter{
			fakeAdapter: newFakeAdapter("fake"),
			listen: func(context.Context, chat.DispatchFunc) error {
				return errFatal
			},
		}
		bot := newListenRuntime(t, newFakeState(), adapter, &recordingObserver{}, chat.DispatchSync)

		err := bot.Listen(t.Context(), "fake")
		if !errors.Is(err, errFatal) || !strings.Contains(err.Error(), `"fake"`) {
			t.Fatalf("Listen = %v, want the adapter error wrapped with the adapter name", err)
		}
	})
}

// TestShutdownStopsListenersAfterAdmissionClosesAndDeferredWorkDrains pins the
// Shutdown order: when a listener sees its context done, admission is closed
// and the deferred handler that was in flight has finished.
func TestShutdownStopsListenersAfterAdmissionClosesAndDeferredWorkDrains(t *testing.T) {
	t.Parallel()

	type stopView struct {
		dispatchErr error
		handlerDone bool
	}
	handlerStarted := make(chan struct{})
	handlerDone := make(chan struct{})
	dispatches := make(chan chat.DispatchFunc, 1)
	stopped := make(chan stopView, 1)
	adapter := &listenerAdapter{
		fakeAdapter: newFakeAdapter("fake"),
		listen: func(ctx context.Context, dispatch chat.DispatchFunc) error {
			dispatches <- dispatch
			<-ctx.Done()
			late := mentionEvent("late", "fake:v1:thread-2")
			view := stopView{dispatchErr: dispatch(context.Background(), &late)}
			select {
			case <-handlerDone:
				view.handlerDone = true
			default:
			}
			stopped <- view
			return nil
		},
	}
	bot := newListenRuntime(t, newFakeState(), adapter, &recordingObserver{}, chat.DispatchDeferred)
	bot.OnNewMention(func(ctx context.Context, _ *chat.MessageEvent) error {
		close(handlerStarted)
		<-ctx.Done()
		// Linger, so a listener stopped before the drain sees this handler
		// still running.
		time.Sleep(50 * time.Millisecond)
		close(handlerDone)
		return ctx.Err()
	})

	listenDone := make(chan error, 1)
	go func() { listenDone <- bot.Listen(t.Context(), "fake") }()
	dispatch := receive(t, dispatches, "adapter Listen to start")
	inflight := mentionEvent("inflight", "fake:v1:thread-1")
	if err := dispatch(t.Context(), &inflight); err != nil {
		t.Fatalf("dispatch before Shutdown: %v", err)
	}
	receive(t, handlerStarted, "deferred handler to start")

	if err := bot.Shutdown(t.Context()); err != nil {
		t.Fatalf("shutdown: %v", err)
	}
	view := receive(t, stopped, "listener to see its context done")
	if !errors.Is(view.dispatchErr, chat.ErrAdmissionRejected) {
		t.Fatalf("dispatch when the listener stopped = %v, want ErrAdmissionRejected", view.dispatchErr)
	}
	if !view.handlerDone {
		t.Fatal("listener stopped before the in-flight deferred handler finished")
	}
	if err := receive(t, listenDone, "Listen to return"); err != nil {
		t.Fatalf("Listen = %v, want nil", err)
	}
}

func TestShutdownWaitsForListenBoundedByCtx(t *testing.T) {
	t.Parallel()

	t.Run("waits", func(t *testing.T) {
		t.Parallel()
		const listeners = 2
		started := make(chan struct{}, listeners)
		var returned atomic.Int32
		inner := newFakeAdapter("fake")
		var returnedAtAdapterShutdown int32
		inner.shutdownHook = func() { returnedAtAdapterShutdown = returned.Load() }
		adapter := &listenerAdapter{
			fakeAdapter: inner,
			listen: func(ctx context.Context, _ chat.DispatchFunc) error {
				started <- struct{}{}
				<-ctx.Done()
				// Linger, so a Shutdown that does not wait shuts the
				// adapter down first.
				time.Sleep(50 * time.Millisecond)
				returned.Add(1)
				return nil
			},
		}
		state := newFakeState()
		bot := newListenRuntime(t, state, adapter, &recordingObserver{}, chat.DispatchSync)

		listenDone := make(chan error, listeners)
		for range listeners {
			go func() { listenDone <- bot.Listen(t.Context(), "fake") }()
		}
		for range listeners {
			receive(t, started, "adapter Listen to start")
		}
		if err := bot.Shutdown(t.Context()); err != nil {
			t.Fatalf("shutdown: %v", err)
		}
		if returnedAtAdapterShutdown != listeners {
			t.Fatalf("Listen calls returned before adapter shutdown = %d, want %d", returnedAtAdapterShutdown, listeners)
		}
		for range listeners {
			if err := receive(t, listenDone, "Listen to return"); err != nil {
				t.Fatalf("Listen = %v, want nil", err)
			}
		}
	})

	t.Run("bounded", func(t *testing.T) {
		t.Parallel()
		started := make(chan struct{})
		release := make(chan struct{})
		adapter := &listenerAdapter{
			fakeAdapter: newFakeAdapter("fake"),
			listen: func(context.Context, chat.DispatchFunc) error {
				close(started)
				<-release // ignore ctx, so the wait must time out
				return nil
			},
		}
		state := newFakeState()
		bot := newListenRuntime(t, state, adapter, &recordingObserver{}, chat.DispatchSync)

		listenDone := make(chan error, 1)
		go func() { listenDone <- bot.Listen(t.Context(), "fake") }()
		receive(t, started, "adapter Listen to start")

		ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
		defer cancel()
		if err := bot.Shutdown(ctx); !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("shutdown = %v, want context.DeadlineExceeded", err)
		}
		if adapter.shutdowns != 1 || state.shutdowns != 1 {
			t.Fatalf("cleanup counts adapter=%d state=%d, want 1 and 1", adapter.shutdowns, state.shutdowns)
		}
		close(release)
		if err := receive(t, listenDone, "Listen to return"); err != nil {
			t.Fatalf("Listen = %v, want nil", err)
		}
	})
}

// TestListenRacingShutdown starts Listen calls while Shutdown runs. Each call
// either returns at once or is stopped by Shutdown, and no adapter Listen is
// still running when Shutdown shuts the adapter down.
func TestListenRacingShutdown(t *testing.T) {
	t.Parallel()

	var active atomic.Int32
	inner := newFakeAdapter("fake")
	var activeAtAdapterShutdown int32
	inner.shutdownHook = func() { activeAtAdapterShutdown = active.Load() }
	adapter := &listenerAdapter{
		fakeAdapter: inner,
		listen: func(ctx context.Context, _ chat.DispatchFunc) error {
			active.Add(1)
			defer active.Add(-1)
			<-ctx.Done()
			return nil
		},
	}
	bot := newListenRuntime(t, newFakeState(), adapter, &recordingObserver{}, chat.DispatchSync)

	const callers = 32
	listenDone := make(chan error, callers)
	for range callers {
		go func() { listenDone <- bot.Listen(t.Context(), "fake") }()
	}
	if err := bot.Shutdown(t.Context()); err != nil {
		t.Fatalf("shutdown: %v", err)
	}
	if activeAtAdapterShutdown != 0 {
		t.Fatalf("adapter Listen calls running at adapter shutdown = %d, want 0", activeAtAdapterShutdown)
	}
	for range callers {
		if err := receive(t, listenDone, "Listen to return"); err != nil {
			t.Fatalf("Listen = %v, want nil", err)
		}
	}
}

// TestSyncDispatchRejectedAfterShutdownStarts pins admission in sync dispatch
// mode: once Shutdown starts, a new delivery gets ErrAdmissionRejected and
// does not run its handler.
func TestSyncDispatchRejectedAfterShutdownStarts(t *testing.T) {
	t.Parallel()

	dispatches := make(chan chat.DispatchFunc, 1)
	stoppedErr := make(chan error, 1)
	adapter := &listenerAdapter{
		fakeAdapter: newFakeAdapter("fake"),
		listen: func(ctx context.Context, dispatch chat.DispatchFunc) error {
			dispatches <- dispatch
			<-ctx.Done()
			late := mentionEvent("during-shutdown", "fake:v1:thread-2")
			stoppedErr <- dispatch(context.Background(), &late)
			return nil
		},
	}
	observer := &recordingObserver{}
	bot := newListenRuntime(t, newFakeState(), adapter, observer, chat.DispatchSync)
	var handled atomic.Int32
	bot.OnNewMention(func(context.Context, *chat.MessageEvent) error {
		handled.Add(1)
		return nil
	})

	listenDone := make(chan error, 1)
	go func() { listenDone <- bot.Listen(t.Context(), "fake") }()
	dispatch := receive(t, dispatches, "adapter Listen to start")
	before := mentionEvent("before-shutdown", "fake:v1:thread-1")
	if err := dispatch(t.Context(), &before); err != nil {
		t.Fatalf("dispatch before Shutdown: %v", err)
	}
	if got := handled.Load(); got != 1 {
		t.Fatalf("handled before Shutdown = %d, want 1", got)
	}

	if err := bot.Shutdown(t.Context()); err != nil {
		t.Fatalf("shutdown: %v", err)
	}
	if err := receive(t, stoppedErr, "listener to see its context done"); !errors.Is(err, chat.ErrAdmissionRejected) {
		t.Fatalf("dispatch during Shutdown = %v, want ErrAdmissionRejected", err)
	}
	after := mentionEvent("after-shutdown", "fake:v1:thread-3")
	if err := dispatch(t.Context(), &after); !errors.Is(err, chat.ErrAdmissionRejected) {
		t.Fatalf("dispatch after Shutdown = %v, want ErrAdmissionRejected", err)
	}
	if got := handled.Load(); got != 1 {
		t.Fatalf("handled after Shutdown = %d, want 1 (no handler runs for a rejected delivery)", got)
	}
	if !observer.hasEvent(chat.ObsAdmissionRejected) || !hasOutcome(observer, chat.OutcomeAdmissionRejected) {
		t.Fatalf("observations = %v, outcomes = %v, want admission_rejected", observer.eventNames(), observer.terminalOutcomes())
	}
	if err := receive(t, listenDone, "Listen to return"); err != nil {
		t.Fatalf("Listen = %v, want nil", err)
	}
}
