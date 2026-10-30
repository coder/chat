package chat_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/chat"
)

const preludePanic = "panicked in the prelude"

// panicServer serves a runtime's fake webhook on a real httptest.Server, so a
// panic in dispatch reaches net/http, which recovers it and aborts the request.
type panicServer struct {
	srv    *httptest.Server
	client *http.Client
	log    syncBuffer
}

type panicPost struct {
	status int
	err    error
}

func newPanicServer(t *testing.T, bot *chat.Chat) *panicServer {
	t.Helper()
	handler, err := bot.Webhook("fake")
	if err != nil {
		t.Fatalf("webhook: %v", err)
	}
	s := &panicServer{srv: httptest.NewUnstartedServer(handler)}
	s.srv.Config.ErrorLog = log.New(&s.log, "", 0)
	s.srv.Start()
	t.Cleanup(s.srv.Close)
	s.client = s.srv.Client()
	s.client.Timeout = 2 * time.Second
	return s
}

func (s *panicServer) post(ev chat.Event) panicPost {
	body, err := json.Marshal(ev)
	if err != nil {
		return panicPost{err: err}
	}
	resp, err := s.client.Post(s.srv.URL, "application/json", bytes.NewReader(body))
	if err != nil {
		return panicPost{err: err}
	}
	resp.Body.Close()
	return panicPost{status: resp.StatusCode}
}

func (s *panicServer) postAsync(ev chat.Event) <-chan panicPost {
	done := make(chan panicPost, 1)
	go func() { done <- s.post(ev) }()
	return done
}

func (s *panicServer) requireAborted(t *testing.T, res panicPost, name string) {
	t.Helper()
	if res.err == nil {
		t.Fatalf("%s: status = %d, want the panic to abort the request", name, res.status)
	}
	if !strings.Contains(s.log.String(), preludePanic) {
		t.Fatalf("%s: net/http did not recover the panic (%v), server log: %q", name, res.err, s.log.String())
	}
}

func newPanicRuntime(t *testing.T, options chat.RuntimeOptions, opts ...chat.Option) *chat.Chat {
	t.Helper()
	opts = append([]chat.Option{
		chat.WithAdapter(newFakeAdapter("fake")),
		chat.WithLogger(slog.New(slog.NewTextHandler(io.Discard, nil))),
		chat.WithRuntimeOptions(options),
	}, opts...)
	bot, err := chat.New(context.Background(), opts...)
	if err != nil {
		t.Fatalf("new runtime: %v", err)
	}
	return bot
}

func panicRuntimeOptions(concurrency chat.ConcurrencyStrategy, mode chat.DispatchMode) chat.RuntimeOptions {
	return chat.RuntimeOptions{
		DedupeTTL:     time.Hour,
		ThreadLockTTL: time.Hour,
		Concurrency:   concurrency,
		Dispatch:      mode,
		MaxDetached:   1024,
		DetachTimeout: 5 * time.Second,
	}
}

type firstMarkPanicState struct {
	*fakeState
	panicked atomic.Bool
}

func (s *firstMarkPanicState) MarkEvent(ctx context.Context, id string, ttl time.Duration) (bool, error) {
	if s.panicked.CompareAndSwap(false, true) {
		panic(preludePanic)
	}
	return s.fakeState.MarkEvent(ctx, id, ttl)
}

func (s *firstMarkPanicState) lockFree() bool {
	s.fakeState.mu.Lock()
	defer s.fakeState.mu.Unlock()
	return len(s.locked) == 0
}

type firstRecordPanicHandler struct {
	slog.Handler
	msg      string
	panicked atomic.Bool
}

func (h *firstRecordPanicHandler) Handle(ctx context.Context, r slog.Record) error {
	if r.Message == h.msg && h.panicked.CompareAndSwap(false, true) {
		panic(preludePanic)
	}
	return h.Handler.Handle(ctx, r)
}

func TestPreludePanicFinishesItsEventAcceptance(t *testing.T) {
	t.Parallel()

	for _, tc := range queueDispatchModes {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			state := newGatedAcquireState(1)
			state.gatePanic = preludePanic
			observer := &recordingObserver{}
			bot := newPanicRuntime(t, panicRuntimeOptions(chat.ConcurrencyDrop, tc.mode),
				chat.WithState(state), chat.WithObserver(observer))
			var handled atomic.Int32
			bot.OnNewMention(func(context.Context, *chat.MessageEvent) error {
				handled.Add(1)
				return nil
			})
			srv := newPanicServer(t, bot)
			openGate := sync.OnceFunc(func() { close(state.release) })
			t.Cleanup(openGate)
			event := mentionEvent("A", queueThread)

			first := srv.postAsync(event)
			waitClosed(t, state.reached, "A's prelude did not reach the Thread Lock")
			duplicate := srv.postAsync(event)
			eventually(t, 2*time.Second, func() bool {
				observer.mu.Lock()
				defer observer.mu.Unlock()
				return observer.opened == 2
			}, "the duplicate of A did not start its dispatch")
			openGate()

			srv.requireAborted(t, <-first, "A")
			if res := <-duplicate; res.err == nil && res.status == http.StatusOK && state.acquireCalls() == 1 {
				t.Fatal("the duplicate of A was acknowledged, but the panicked delivery never accepted A")
			}
			if res := srv.post(event); res.err != nil || res.status != http.StatusOK {
				t.Fatalf("the redelivery of A: status=%d err=%v, want 200", res.status, res.err)
			}
			eventually(t, 2*time.Second, func() bool { return handled.Load() == 1 },
				"the redelivery of A did not run its handler")
		})
	}
}

func TestPreludePanicReleasesItsThreadLock(t *testing.T) {
	t.Parallel()

	for _, tc := range queueDispatchModes {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			state := &firstMarkPanicState{fakeState: newFakeState()}
			observer := &recordingObserver{}
			bot := newPanicRuntime(t, panicRuntimeOptions(chat.ConcurrencyQueue, tc.mode),
				chat.WithState(state), chat.WithObserver(observer))
			var handled atomic.Int32
			bot.OnNewMention(func(_ context.Context, ev *chat.MessageEvent) error {
				if ev.Event.ID == "B" {
					handled.Add(1)
				}
				return nil
			})
			srv := newPanicServer(t, bot)

			srv.requireAborted(t, srv.post(mentionEvent("A", queueThread)), "A")
			if res := srv.post(mentionEvent("B", queueThread)); res.err != nil || res.status != http.StatusOK {
				t.Fatalf("B: status=%d err=%v, want 200", res.status, res.err)
			}
			eventually(t, 2*time.Second, func() bool { return handled.Load() == 1 },
				"B did not run: A's MarkEvent panic kept the Thread Lock, whose TTL is an hour")
			eventually(t, 2*time.Second, state.lockFree, "B did not release the Thread Lock")
			if res := srv.post(mentionEvent("B", queueThread)); res.err != nil || res.status != http.StatusOK {
				t.Fatalf("the redelivery of B: status=%d err=%v, want 200", res.status, res.err)
			}
			if observer.hasEvent(chat.ObsLockReleaseFailed) {
				t.Fatal("a Thread Lock release failed: the prelude released a lease twice")
			}
		})
	}
}

func TestQueueSupersedeLoggerPanicReleasesItsThreadLock(t *testing.T) {
	t.Parallel()

	for _, tc := range queueDispatchModes {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			const (
				aPreludeAcquire = 1 + iota
				bPreludeAcquire
				bFirstPoll
			)
			state := newGatedAcquireState(bFirstPoll)
			logger := slog.New(&firstRecordPanicHandler{
				Handler: slog.NewTextHandler(io.Discard, nil),
				msg:     "chat queue superseded",
			})
			bot := newPanicRuntime(t, panicRuntimeOptions(chat.ConcurrencyQueue, tc.mode),
				chat.WithState(state), chat.WithLogger(logger))
			aStarted := make(chan struct{})
			releaseA := make(chan struct{})
			var dHandled atomic.Bool
			bot.OnNewMention(func(_ context.Context, ev *chat.MessageEvent) error {
				switch ev.Event.ID {
				case "A":
					close(aStarted)
					<-releaseA
				case "B":
					t.Error("B ran, but C superseded it")
				case "D":
					dHandled.Store(true)
				}
				return nil
			})
			srv := newPanicServer(t, bot)
			finishA := sync.OnceFunc(func() { close(releaseA) })
			openGate := sync.OnceFunc(func() { close(state.release) })
			t.Cleanup(func() {
				finishA()
				openGate()
			})

			a := srv.postAsync(mentionEvent("A", queueThread))
			waitClosed(t, aStarted, "A's handler did not start")
			b := srv.postAsync(mentionEvent("B", queueThread))
			waitClosed(t, state.reached, "B did not poll for the lock")
			finishA()
			if res := <-a; res.err != nil || res.status != http.StatusOK {
				t.Fatalf("A: status=%d err=%v, want 200", res.status, res.err)
			}
			eventually(t, 2*time.Second, func() bool { return !state.lockHeld(string(queueThread)) },
				"A did not release the Thread Lock")
			srv.requireAborted(t, srv.post(mentionEvent("C", queueThread)), "C")
			openGate()
			if res := <-b; res.err != nil || res.status != http.StatusOK {
				t.Fatalf("B: status=%d err=%v, want 200 for a superseded waiter", res.status, res.err)
			}
			if res := srv.post(mentionEvent("D", queueThread)); res.err != nil || res.status != http.StatusOK {
				t.Fatalf("D: status=%d err=%v, want 200", res.status, res.err)
			}
			eventually(t, 2*time.Second, dHandled.Load,
				"D did not run: C's supersede log panicked while C held the Thread Lock")
		})
	}
}

func TestBurstMemberLoggerPanicStillRunsItsScope(t *testing.T) {
	t.Parallel()

	logger := slog.New(&firstRecordPanicHandler{
		Handler: slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelDebug}),
		msg:     "chat burst member joined",
	})
	options := panicRuntimeOptions(chat.ConcurrencyBurst, chat.DispatchDeferred)
	options.MaxDetached = 1
	options.BurstWindow = time.Millisecond
	options.ThreadLockTTL = 200 * time.Millisecond
	bot := newPanicRuntime(t, options, chat.WithState(newFakeState()), chat.WithLogger(logger))
	started := make(chan string, 2)
	releaseA := make(chan struct{})
	bot.OnNewMention(func(_ context.Context, ev *chat.MessageEvent) error {
		started <- ev.Event.ID
		if ev.Event.ID == "A" {
			<-releaseA
		}
		return nil
	})
	srv := newPanicServer(t, bot)
	waitStarted := func(want, msg string) {
		t.Helper()
		select {
		case got := <-started:
			if got != want {
				t.Fatalf("handler started for %s, want %s", got, want)
			}
		case <-time.After(2 * time.Second):
			t.Fatal(msg)
		}
	}

	srv.requireAborted(t, srv.post(mentionEvent("A", queueThread)), "A")
	waitStarted("A", "A's burst member did not run after the logger panicked")
	if res := srv.post(mentionEvent("B", queueThread)); res.err != nil || res.status == http.StatusOK {
		t.Fatalf("B: status=%d err=%v, want an admission rejection while A holds the only slot", res.status, res.err)
	}
	close(releaseA)
	eventually(t, 2*time.Second, func() bool {
		return srv.post(mentionEvent("B", queueThread)).status == http.StatusOK
	}, "B was not admitted after A finished")
	waitStarted("B", "B's burst member did not run")

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := bot.Shutdown(ctx); err != nil {
		t.Fatalf("Shutdown after the logger panicked: %v", err)
	}
}

func TestPreludePanicEndsItsDispatchSpan(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		opts func() []chat.Option
	}{
		{
			name: "MarkEvent",
			opts: func() []chat.Option {
				return []chat.Option{chat.WithState(&firstMarkPanicState{fakeState: newFakeState()})}
			},
		},
		{
			name: "ignored log",
			opts: func() []chat.Option {
				return []chat.Option{chat.WithState(newFakeState()), chat.WithLogger(slog.New(&firstRecordPanicHandler{
					Handler: slog.NewTextHandler(io.Discard, nil),
					msg:     "chat ignored unrouted message",
				}))}
			},
		},
	}
	for _, tc := range cases {
		for _, mode := range queueDispatchModes {
			t.Run(tc.name+"/"+mode.name, func(t *testing.T) {
				t.Parallel()

				observer := &recordingObserver{}
				bot := newPanicRuntime(t, panicRuntimeOptions(chat.ConcurrencyQueue, mode.mode),
					append(tc.opts(), chat.WithObserver(observer))...)
				srv := newPanicServer(t, bot)

				srv.requireAborted(t, srv.post(mentionEvent("A", queueThread)), "A")
				eventually(t, 2*time.Second, func() bool { return len(observer.terminalOutcomes()) == 1 },
					"A's dispatch span did not end after its prelude panicked")
				if got := observer.terminalOutcomes(); got[0] != chat.OutcomeError {
					t.Fatalf("A's dispatch span ended %q, want %q", got[0], chat.OutcomeError)
				}
			})
		}
	}
}
