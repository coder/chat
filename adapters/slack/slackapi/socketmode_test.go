package slackapi_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/coder/chat/adapters/slack/slackapi"
	"github.com/coder/chat/adapters/slack/slackapi/slackapitest"
)

const (
	socketAppToken = "xapp-socket"
	socketWait     = 5 * time.Second
)

// socketClient returns a client for srv that sends the app-level token, with
// the default retry policy.
func socketClient(srv *slackapitest.Server) *slackapi.Client {
	return coreNewClient(srv, slackapi.Options{Token: socketAppToken})
}

// fastListen returns ListenOptions with short backoffs for tests.
func fastListen() slackapi.ListenOptions {
	return slackapi.ListenOptions{MinBackoff: time.Millisecond, MaxBackoff: 10 * time.Millisecond}
}

func ackAll(context.Context, *slackapi.Delivery) (any, error) { return nil, nil }

// sendTo returns a callback that sends to ch, and drops the value when ch is
// full, so that a callback never blocks Listen.
func sendTo[T any](ch chan T) func(T) {
	return func(v T) {
		select {
		case ch <- v:
		default:
		}
	}
}

func recv[T any](t *testing.T, ch <-chan T) T {
	t.Helper()
	select {
	case v := <-ch:
		return v
	case <-time.After(socketWait):
		t.Fatalf("timed out waiting for a %v", reflect.TypeFor[T]())
		panic("unreachable")
	}
}

func waitFor(t *testing.T, srv *slackapitest.Server, what string, cond func() bool) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), socketWait)
	defer cancel()
	if err := srv.Wait(ctx, cond); err != nil {
		t.Fatalf("wait for %s: %v", what, err)
	}
}

// handleOpens makes srv answer apps.connections.open with answer, which gets
// the number of the call from 1. It returns a function that lists the times
// of the calls.
func handleOpens(srv *slackapitest.Server, answer func(n int, call slackapitest.Call) any) func() []time.Time {
	var mu sync.Mutex
	var times []time.Time
	srv.Handle("apps.connections.open", func(call slackapitest.Call) any {
		mu.Lock()
		times = append(times, time.Now())
		n := len(times)
		mu.Unlock()
		return answer(n, call)
	})
	return func() []time.Time {
		mu.Lock()
		defer mu.Unlock()
		return slices.Clone(times)
	}
}

// listening is a Listen call that runs in a goroutine until the test ends.
type listening struct {
	cancel context.CancelFunc
	done   chan struct{}
	err    error
}

func startListen(t *testing.T, api *slackapi.Client, h slackapi.Handler, opts slackapi.ListenOptions) *listening {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	l := &listening{cancel: cancel, done: make(chan struct{})}
	go func() {
		defer close(l.done)
		l.err = api.Listen(ctx, h, opts)
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-l.done:
		case <-time.After(socketWait):
			t.Errorf("Listen did not return after its context ended")
		}
	})
	return l
}

// wait waits for Listen to return, and returns its error.
func (l *listening) wait(t *testing.T) error {
	t.Helper()
	select {
	case <-l.done:
		return l.err
	case <-time.After(socketWait):
		t.Fatalf("Listen did not return")
		return nil
	}
}

func (l *listening) stop(t *testing.T) error {
	t.Helper()
	l.cancel()
	return l.wait(t)
}

// requireCloseFrames checks that the client closed each connection that the
// fake did not drop, with a close frame and the normal status.
func requireCloseFrames(t *testing.T, sm *slackapitest.SocketMode) {
	t.Helper()
	for _, c := range sm.Conns() {
		if c.Dropped {
			continue
		}
		c = sm.WaitClosed(t, socketWait, c.ID)
		if !c.CloseFrame || c.CloseCode != websocket.StatusNormalClosure {
			t.Errorf("connection %d: CloseFrame = %v, CloseCode = %v, want a close frame with %v",
				c.ID, c.CloseFrame, c.CloseCode, websocket.StatusNormalClosure)
		}
	}
}

func requireNoError(t *testing.T, errs <-chan error) {
	t.Helper()
	select {
	case err := <-errs:
		t.Errorf("OnError(%v), want no call", err)
	default:
	}
}

func TestOpenConnection(t *testing.T) {
	t.Parallel()

	srv := slackapitest.NewServer(t)
	srv.SocketMode(slackapitest.SocketModeOptions{AppToken: socketAppToken})
	api := socketClient(srv)

	wsURL, err := api.OpenConnection(t.Context())
	if err != nil {
		t.Fatalf("OpenConnection: %v", err)
	}
	if !strings.HasPrefix(wsURL, "ws://") {
		t.Errorf("URL = %q, want a ws:// URL", wsURL)
	}
	calls := srv.Calls("apps.connections.open")
	if len(calls) != 1 {
		t.Fatalf("apps.connections.open calls = %d, want 1", len(calls))
	}
	if got, want := calls[0].Header.Get("Authorization"), "Bearer "+socketAppToken; got != want {
		t.Errorf("Authorization = %q, want %q", got, want)
	}
	if calls[0].Form == nil {
		t.Errorf("request body %q is not form encoded", calls[0].Body)
	}

	_, err = api.WithToken("xapp-other").OpenConnection(t.Context())
	if apiErr, ok := errors.AsType[*slackapi.APIError](err); !ok || apiErr.Code != "invalid_auth" {
		t.Errorf("OpenConnection with another token: err = %v, want an *APIError with code invalid_auth", err)
	}
}

func TestOpenConnectionNoURL(t *testing.T) {
	t.Parallel()

	srv := slackapitest.NewServer(t)
	srv.Respond("apps.connections.open", map[string]any{"ok": true})
	if _, err := socketClient(srv).OpenConnection(t.Context()); err == nil {
		t.Fatal("OpenConnection: err = nil, want an error for a response with no url")
	}
}

func TestListenConnects(t *testing.T) {
	t.Parallel()

	srv := slackapitest.NewServer(t)
	sm := srv.SocketMode(slackapitest.SocketModeOptions{AppToken: socketAppToken, AppID: "A42"})
	hellos := make(chan slackapi.Hello, 1)
	opts := fastListen()
	opts.OnConnect = sendTo(hellos)
	l := startListen(t, socketClient(srv), ackAll, opts)

	want := slackapi.Hello{NumConnections: 1, AppID: "A42", Host: "applink-1"}
	if got := recv(t, hellos); got != want {
		t.Errorf("OnConnect(%+v), want %+v", got, want)
	}
	if err := l.stop(t); err != nil {
		t.Fatalf("Listen: %v", err)
	}
	requireCloseFrames(t, sm)
}

func TestListenAcks(t *testing.T) {
	t.Parallel()

	srv := slackapitest.NewServer(t)
	sm := srv.SocketMode(slackapitest.SocketModeOptions{AppToken: socketAppToken, ResendAfter: -1})
	deliveries := make(chan slackapi.Delivery, 10)
	h := func(_ context.Context, d *slackapi.Delivery) (any, error) {
		deliveries <- *d
		if d.Type == slackapi.DeliverySlashCommands {
			return map[string]string{"text": "pong"}, nil
		}
		return nil, nil
	}
	l := startListen(t, socketClient(srv), h, fastListen())
	sm.WaitLive(t, socketWait, 1)

	tests := []struct {
		typ     string
		payload string
		wantAck string
	}{
		{typ: "slash_commands", payload: `{"command":"/ping","text":"hi"}`, wantAck: `{"text":"pong"}`},
		{typ: "events_api", payload: `{"type":"event_callback","n":1}`},
		{typ: "interactive", payload: `{"type":"block_actions"}`},
		{typ: "events_api", payload: `{"type":"event_callback","n":2}`},
		{typ: "some_future_type", payload: `{"n":3}`},
		{typ: "slash_commands", payload: `{"command":"/ping","text":"again"}`, wantAck: `{"text":"pong"}`},
	}
	for _, tt := range tests {
		id := sm.Send(tt.typ, json.RawMessage(tt.payload))
		env := sm.WaitAcked(t, socketWait, id)
		d := recv(t, deliveries)
		if d.Type != slackapi.DeliveryType(tt.typ) || string(d.Payload) != tt.payload || d.RetryAttempt != 0 || d.RetryReason != "" {
			t.Errorf("%s: handler got %+v, want type %q and payload %s", id, d, tt.typ, tt.payload)
		}
		if len(env.Acks) != 1 {
			t.Fatalf("%s: acks = %+v, want 1", id, env.Acks)
		}
		if got := string(env.Acks[0].Payload); got != tt.wantAck {
			t.Errorf("%s: ack payload = %s, want %q", id, got, tt.wantAck)
		}
	}

	if err := l.stop(t); err != nil {
		t.Fatalf("Listen: %v", err)
	}
	requireCloseFrames(t, sm)
}

// TestListenReadsLargePayloads checks that Listen reads an envelope larger than
// the 32 KiB default read limit of the websocket library.
func TestListenReadsLargePayloads(t *testing.T) {
	t.Parallel()

	srv := slackapitest.NewServer(t)
	sm := srv.SocketMode(slackapitest.SocketModeOptions{AppToken: socketAppToken, ResendAfter: -1})
	deliveries := make(chan slackapi.Delivery, 1)
	errs := make(chan error, 10)
	opts := fastListen()
	opts.OnError = sendTo(errs)
	l := startListen(t, socketClient(srv), func(_ context.Context, d *slackapi.Delivery) (any, error) {
		deliveries <- *d
		return nil, nil
	}, opts)
	sm.WaitLive(t, socketWait, 1)

	payload := `{"type":"block_actions","value":"` + strings.Repeat("x", 100<<10) + `"}`
	id := sm.Send("interactive", json.RawMessage(payload))
	if env := sm.WaitAcked(t, socketWait, id); env.Acks[0].Conn != 1 {
		t.Errorf("ack on connection %d, want 1", env.Acks[0].Conn)
	}
	if d := recv(t, deliveries); string(d.Payload) != payload {
		t.Errorf("handler got a payload of %d bytes, want %d", len(d.Payload), len(payload))
	}

	if err := l.stop(t); err != nil {
		t.Fatalf("Listen: %v", err)
	}
	requireCloseFrames(t, sm)
	requireNoError(t, errs)
}

func TestListenAcksOnTheSameConnection(t *testing.T) {
	t.Parallel()

	srv := slackapitest.NewServer(t)
	sm := srv.SocketMode(slackapitest.SocketModeOptions{AppToken: socketAppToken, ResendAfter: -1})
	started := make(chan struct{}, 1)
	release := make(chan struct{})
	h := func(ctx context.Context, _ *slackapi.Delivery) (any, error) {
		started <- struct{}{}
		select {
		case <-release:
		case <-ctx.Done():
		}
		return nil, nil
	}
	hellos := make(chan slackapi.Hello, 2)
	opts := fastListen()
	opts.OnConnect = sendTo(hellos)
	l := startListen(t, socketClient(srv), h, opts)
	recv(t, hellos)
	id := sm.Send("events_api", map[string]string{"n": "slow"})
	recv(t, started)

	// Listen calls OnConnect after it makes connection 2 the active
	// connection. The ack must still go on connection 1.
	sm.DisconnectConn(1, "refresh_requested")
	if h := recv(t, hellos); h.Host != "applink-2" {
		t.Fatalf("OnConnect(%+v), want connection 2", h)
	}
	if c := sm.Conns()[0]; c.CloseSeq != 0 {
		t.Fatalf("connection 1 = %+v, want it open while its handler runs", c)
	}
	close(release)

	ack := sm.WaitAcked(t, socketWait, id).Acks[0]
	old := sm.WaitClosed(t, socketWait, 1)
	if ack.Conn != 1 || ack.Seq > old.CloseSeq {
		t.Errorf("ack = %+v, connection 1 closed at seq %d: want the ack on connection 1 before it closed", ack, old.CloseSeq)
	}
	if conns := sm.Conns(); conns[1].HelloSeq == 0 || conns[1].HelloSeq > old.CloseSeq {
		t.Errorf("connections = %+v, want connection 2 up before connection 1 closed", conns)
	}

	if err := l.stop(t); err != nil {
		t.Fatalf("Listen: %v", err)
	}
	requireCloseFrames(t, sm)
}

func TestListenHandlerErrorSendsNoAck(t *testing.T) {
	t.Parallel()

	srv := slackapitest.NewServer(t)
	sm := srv.SocketMode(slackapitest.SocketModeOptions{AppToken: socketAppToken, ResendAfter: 50 * time.Millisecond})
	failures := map[string]error{
		"busy":    slackapi.ErrBusy,
		"invalid": fmt.Errorf("bad command: %w", slackapi.ErrInvalidPayload),
		"other":   errors.New("store down"),
	}
	retries := make(chan slackapi.Delivery, 10)
	h := func(_ context.Context, d *slackapi.Delivery) (any, error) {
		if d.RetryAttempt == 0 {
			var p struct {
				Case string `json:"case"`
			}
			if err := json.Unmarshal(d.Payload, &p); err != nil {
				return nil, err
			}
			return nil, failures[p.Case]
		}
		retries <- *d
		return nil, nil
	}
	l := startListen(t, socketClient(srv), h, fastListen())
	sm.WaitLive(t, socketWait, 1)

	var ids []string
	for name := range failures {
		ids = append(ids, sm.Send("events_api", map[string]string{"case": name}))
	}
	for _, id := range ids {
		env := sm.WaitAcked(t, socketWait, id)
		if len(env.Deliveries) < 2 {
			t.Fatalf("%s: deliveries = %+v, want a resend", id, env.Deliveries)
		}
		resend := env.Deliveries[1]
		if resend.RetryAttempt != 1 || resend.RetryReason != "timeout" {
			t.Errorf("%s: resend = %+v, want retry attempt 1 with reason timeout", id, resend)
		}
		if ack := env.Acks[0]; ack.Seq < resend.Seq {
			t.Errorf("%s: ack %+v came before the resend %+v, want no ack for the failed delivery", id, ack, resend)
		}
	}
	for range ids {
		if d := recv(t, retries); d.RetryAttempt < 1 || d.RetryReason != "timeout" {
			t.Errorf("handler got retry attempt %d with reason %q, want a resend with reason timeout", d.RetryAttempt, d.RetryReason)
		}
	}

	if err := l.stop(t); err != nil {
		t.Fatalf("Listen: %v", err)
	}
	requireCloseFrames(t, sm)
}

func TestListenRefresh(t *testing.T) {
	t.Parallel()

	for _, reason := range []string{"warning", "refresh_requested", "a_future_reason"} {
		t.Run(reason, func(t *testing.T) {
			t.Parallel()

			srv := slackapitest.NewServer(t)
			sm := srv.SocketMode(slackapitest.SocketModeOptions{AppToken: socketAppToken, ResendAfter: -1})
			gate := make(chan struct{})
			release := sync.OnceFunc(func() { close(gate) })
			t.Cleanup(release)
			handleOpens(srv, func(n int, call slackapitest.Call) any {
				if n == 2 {
					<-gate
				}
				return sm.Open(call)
			})
			errs := make(chan error, 10)
			opts := fastListen()
			opts.OnError = sendTo(errs)
			l := startListen(t, socketClient(srv), ackAll, opts)
			sm.WaitLive(t, socketWait, 1)

			sm.DisconnectConn(1, reason)
			// The Handle function of the call blocks, so wait for the call
			// that the Server records.
			waitFor(t, srv, "the replacement connect", func() bool { return len(srv.Calls("apps.connections.open")) == 2 })
			during := sm.Send("events_api", map[string]string{"n": "during"})
			if env := sm.WaitAcked(t, socketWait, during); env.Acks[0].Conn != 1 {
				t.Errorf("envelope during the refresh acked on connection %d, want 1", env.Acks[0].Conn)
			}

			release()
			old := sm.WaitClosed(t, socketWait, 1)
			conns := sm.Conns()
			if len(conns) != 2 || conns[1].HelloSeq == 0 || conns[1].HelloSeq > old.CloseSeq {
				t.Fatalf("connections = %+v, want connection 2 up before connection 1 closed", conns)
			}
			after := sm.Send("events_api", map[string]string{"n": "after"})
			if env := sm.WaitAcked(t, socketWait, after); env.Acks[0].Conn != 2 {
				t.Errorf("envelope after the refresh acked on connection %d, want 2", env.Acks[0].Conn)
			}

			if err := l.stop(t); err != nil {
				t.Fatalf("Listen: %v", err)
			}
			requireCloseFrames(t, sm)
			requireNoError(t, errs)
		})
	}
}

func TestListenPingTimeout(t *testing.T) {
	t.Parallel()

	srv := slackapitest.NewServer(t)
	sm := srv.SocketMode(slackapitest.SocketModeOptions{AppToken: socketAppToken, PingInterval: 5 * time.Millisecond})
	errs := make(chan error, 10)
	opts := fastListen()
	opts.PingTimeout = 100 * time.Millisecond
	opts.OnError = sendTo(errs)
	l := startListen(t, socketClient(srv), ackAll, opts)
	sm.WaitLive(t, socketWait, 1)
	waitFor(t, srv, "a ping on connection 1", func() bool { return sm.Conns()[0].Pings > 0 })

	sm.StopPings(1)
	if err := recv(t, errs); !errors.Is(err, slackapi.ErrPingTimeout) {
		t.Errorf("OnError(%v), want ErrPingTimeout", err)
	}
	old := sm.WaitClosed(t, socketWait, 1)
	if conns := sm.Conns(); len(conns) < 2 || conns[1].HelloSeq == 0 || conns[1].HelloSeq > old.CloseSeq {
		t.Fatalf("connections = %+v, want connection 2 up before connection 1 closed", conns)
	}

	if err := l.stop(t); err != nil {
		t.Fatalf("Listen: %v", err)
	}
	requireCloseFrames(t, sm)
}

func TestListenPingTimeoutStartsAfterTheFirstPing(t *testing.T) {
	t.Parallel()

	const pingTimeout = 50 * time.Millisecond
	srv := slackapitest.NewServer(t)
	sm := srv.SocketMode(slackapitest.SocketModeOptions{AppToken: socketAppToken, PingInterval: -1})
	errs := make(chan error, 10)
	opts := fastListen()
	opts.PingTimeout = pingTimeout
	opts.OnError = sendTo(errs)
	l := startListen(t, socketClient(srv), ackAll, opts)
	sm.WaitLive(t, socketWait, 1)

	time.Sleep(4 * pingTimeout)
	if conns := sm.Conns(); len(conns) != 1 || !conns[0].Live() {
		t.Errorf("connections = %+v, want only connection 1, and live", conns)
	}
	requireNoError(t, errs)

	if err := l.stop(t); err != nil {
		t.Fatalf("Listen: %v", err)
	}
	requireCloseFrames(t, sm)
}

func TestListenFatalErrors(t *testing.T) {
	t.Parallel()

	requireReturns := func(t *testing.T, srv *slackapitest.Server, l *listening, errs <-chan error, want string) {
		t.Helper()
		err := l.wait(t)
		if got := slackCodeOf(err); got != want {
			t.Errorf("Listen: err = %v, want code %s", err, want)
		}
		if n := len(srv.Calls("apps.connections.open")); n != 1 {
			t.Errorf("apps.connections.open calls = %d, want 1", n)
		}
		requireNoError(t, errs)
	}

	t.Run("invalid_auth", func(t *testing.T) {
		t.Parallel()
		srv := slackapitest.NewServer(t)
		srv.SocketMode(slackapitest.SocketModeOptions{AppToken: "xapp-other"})
		errs := make(chan error, 10)
		opts := fastListen()
		opts.OnError = sendTo(errs)
		l := startListen(t, socketClient(srv), ackAll, opts)
		requireReturns(t, srv, l, errs, "invalid_auth")
	})

	for _, code := range []string{
		"not_authed", "account_inactive", "token_revoked", "token_expired", "user_removed_from_team",
		"team_disabled", "not_allowed_token_type", "missing_scope",
	} {
		t.Run(code, func(t *testing.T) {
			t.Parallel()
			srv := slackapitest.NewServer(t)
			srv.Respond("apps.connections.open", map[string]any{"ok": false, "error": code})
			errs := make(chan error, 10)
			opts := fastListen()
			opts.OnError = sendTo(errs)
			l := startListen(t, socketClient(srv), ackAll, opts)
			requireReturns(t, srv, l, errs, code)
		})
	}

	t.Run("link_disabled", func(t *testing.T) {
		t.Parallel()
		srv := slackapitest.NewServer(t)
		sm := srv.SocketMode(slackapitest.SocketModeOptions{AppToken: socketAppToken})
		errs := make(chan error, 10)
		opts := fastListen()
		opts.OnError = sendTo(errs)
		l := startListen(t, socketClient(srv), ackAll, opts)
		sm.WaitLive(t, socketWait, 1)

		sm.Disconnect("link_disabled")
		requireReturns(t, srv, l, errs, "link_disabled")
		if _, ok := errors.AsType[*slackapi.DisconnectError](l.err); !ok {
			t.Errorf("Listen: err = %T, want *DisconnectError", l.err)
		}
		requireCloseFrames(t, sm)
	})
}

// slackCodeOf returns the code of an *APIError or the reason of a
// *DisconnectError.
func slackCodeOf(err error) string {
	if e, ok := errors.AsType[*slackapi.APIError](err); ok {
		return e.Code
	}
	if e, ok := errors.AsType[*slackapi.DisconnectError](err); ok {
		return e.Reason
	}
	return ""
}

func TestListenTooManyWebsockets(t *testing.T) {
	t.Parallel()

	const maxBackoff = 200 * time.Millisecond
	type stamped struct {
		err error
		at  time.Time
	}
	stampErrors := func(ch chan stamped) func(error) {
		send := sendTo(ch)
		return func(err error) { send(stamped{err: err, at: time.Now()}) }
	}

	t.Run("disconnect", func(t *testing.T) {
		t.Parallel()
		srv := slackapitest.NewServer(t)
		sm := srv.SocketMode(slackapitest.SocketModeOptions{AppToken: socketAppToken, MaxConnections: 1})
		opens := handleOpens(srv, func(_ int, call slackapitest.Call) any { return sm.Open(call) })
		api := socketClient(srv)
		hellos := make(chan slackapi.Hello, 1)
		holderOpts := fastListen()
		holderOpts.OnConnect = sendTo(hellos)
		holder := startListen(t, api, ackAll, holderOpts)
		recv(t, hellos)

		errs := make(chan stamped, 10)
		extra := startListen(t, api, ackAll, slackapi.ListenOptions{
			MinBackoff: time.Millisecond,
			MaxBackoff: maxBackoff,
			OnError:    stampErrors(errs),
		})
		first := recv(t, errs)
		if got := slackCodeOf(first.err); got != "too_many_websockets" {
			t.Fatalf("OnError(%v), want too_many_websockets", first.err)
		}
		if rejected := sm.WaitClosed(t, socketWait, 2); !rejected.Rejected {
			t.Errorf("connection 2 = %+v, want a rejected connection", rejected)
		}
		if got := slackCodeOf(recv(t, errs).err); got != "too_many_websockets" {
			t.Fatalf("second OnError code = %q, want too_many_websockets", got)
		}
		if wait := opens()[2].Sub(first.at); wait < maxBackoff/2 {
			t.Errorf("retry after %s, want at least %s", wait, maxBackoff/2)
		}

		for _, l := range []*listening{extra, holder} {
			if err := l.stop(t); err != nil {
				t.Fatalf("Listen: %v", err)
			}
		}
		requireCloseFrames(t, sm)
	})

	t.Run("disconnect after hello", func(t *testing.T) {
		t.Parallel()
		srv := slackapitest.NewServer(t)
		sm := srv.SocketMode(slackapitest.SocketModeOptions{AppToken: socketAppToken})
		opens := handleOpens(srv, func(_ int, call slackapitest.Call) any { return sm.Open(call) })
		errs := make(chan stamped, 10)
		hellos := make(chan slackapi.Hello, 2)
		l := startListen(t, socketClient(srv), ackAll, slackapi.ListenOptions{
			MinBackoff: time.Millisecond,
			MaxBackoff: maxBackoff,
			OnConnect:  sendTo(hellos),
			OnError:    stampErrors(errs),
		})
		recv(t, hellos)

		sm.DisconnectConn(1, "too_many_websockets")
		first := recv(t, errs)
		if got := slackCodeOf(first.err); got != "too_many_websockets" {
			t.Fatalf("OnError(%v), want too_many_websockets", first.err)
		}
		if h := recv(t, hellos); h.Host != "applink-2" {
			t.Fatalf("OnConnect(%+v), want connection 2", h)
		}
		if wait := opens()[1].Sub(first.at); wait < maxBackoff/2 {
			t.Errorf("reconnect after %s, want at least %s", wait, maxBackoff/2)
		}
		// Listen closes connection 1 at once, and does not keep it until
		// connection 2 is up.
		if conns := sm.Conns(); conns[0].CloseSeq == 0 || conns[0].CloseSeq > conns[1].HelloSeq {
			t.Errorf("connections = %+v, want connection 1 closed before connection 2 is up", conns)
		}

		if err := l.stop(t); err != nil {
			t.Fatalf("Listen: %v", err)
		}
		requireCloseFrames(t, sm)
	})

	t.Run("disconnect during a replacement", func(t *testing.T) {
		t.Parallel()
		srv := slackapitest.NewServer(t)
		sm := srv.SocketMode(slackapitest.SocketModeOptions{AppToken: socketAppToken})
		gate := make(chan struct{})
		release := sync.OnceFunc(func() { close(gate) })
		t.Cleanup(release)
		handleOpens(srv, func(n int, call slackapitest.Call) any {
			if n == 2 {
				<-gate
			}
			return sm.Open(call)
		})
		errs := make(chan error, 10)
		hellos := make(chan slackapi.Hello, 3)
		// The backoff after too_many_websockets is longer than the test waits.
		l := startListen(t, socketClient(srv), ackAll, slackapi.ListenOptions{
			MinBackoff: time.Millisecond,
			MaxBackoff: time.Minute,
			OnConnect:  sendTo(hellos),
			OnError:    sendTo(errs),
		})
		recv(t, hellos)

		sm.DisconnectConn(1, "warning")
		waitFor(t, srv, "the replacement connect", func() bool { return len(srv.Calls("apps.connections.open")) == 2 })
		sm.DisconnectConn(1, "too_many_websockets")
		if got := slackCodeOf(recv(t, errs)); got != "too_many_websockets" {
			t.Fatalf("OnError code = %q, want too_many_websockets", got)
		}
		release()
		if h := recv(t, hellos); h.Host != "applink-2" {
			t.Fatalf("OnConnect(%+v), want connection 2", h)
		}
		// Connection 2 is up, so the backoff of connection 1 is over, and a
		// refresh of connection 2 connects at once.
		sm.DisconnectConn(2, "refresh_requested")
		if h := recv(t, hellos); h.Host != "applink-3" {
			t.Fatalf("OnConnect(%+v), want connection 3", h)
		}

		if err := l.stop(t); err != nil {
			t.Fatalf("Listen: %v", err)
		}
		requireCloseFrames(t, sm)
	})

	t.Run("api error", func(t *testing.T) {
		t.Parallel()
		srv := slackapitest.NewServer(t)
		sm := srv.SocketMode(slackapitest.SocketModeOptions{AppToken: socketAppToken})
		opens := handleOpens(srv, func(n int, call slackapitest.Call) any {
			if n == 1 {
				return map[string]any{"ok": false, "error": "too_many_websockets"}
			}
			return sm.Open(call)
		})
		errs := make(chan stamped, 10)
		hellos := make(chan slackapi.Hello, 1)
		l := startListen(t, socketClient(srv), ackAll, slackapi.ListenOptions{
			MinBackoff: time.Millisecond,
			MaxBackoff: maxBackoff,
			OnConnect:  sendTo(hellos),
			OnError:    stampErrors(errs),
		})
		recv(t, hellos)
		if got := slackCodeOf(recv(t, errs).err); got != "too_many_websockets" {
			t.Errorf("OnError code = %q, want too_many_websockets", got)
		}
		if times := opens(); times[1].Sub(times[0]) < maxBackoff/2 {
			t.Errorf("retry after %s, want at least %s", times[1].Sub(times[0]), maxBackoff/2)
		}

		if err := l.stop(t); err != nil {
			t.Fatalf("Listen: %v", err)
		}
		requireCloseFrames(t, sm)
	})
}

func TestListenHonorsRetryAfter(t *testing.T) {
	t.Parallel()

	// The Retry-After is longer than the MaxDelay of the default retry policy
	// (1 second), so a retry of the Client would come too early.
	const retryAfter = 1500 * time.Millisecond
	srv := slackapitest.NewServer(t)
	sm := srv.SocketMode(slackapitest.SocketModeOptions{AppToken: socketAppToken})
	opens := handleOpens(srv, func(n int, call slackapitest.Call) any {
		if n == 1 {
			return slackapitest.Response{
				StatusCode: http.StatusTooManyRequests,
				Header:     http.Header{"Retry-After": {"1.5"}},
			}
		}
		return sm.Open(call)
	})
	errs := make(chan error, 10)
	hellos := make(chan slackapi.Hello, 1)
	opts := fastListen()
	opts.OnConnect = sendTo(hellos)
	opts.OnError = sendTo(errs)
	l := startListen(t, socketClient(srv), ackAll, opts)
	recv(t, hellos)

	if rl, ok := errors.AsType[*slackapi.RateLimited](recv(t, errs)); !ok || rl.RetryAfter != retryAfter || rl.Attempts != 1 {
		t.Errorf("OnError(%v), want *RateLimited after 1 attempt with RetryAfter %s", rl, retryAfter)
	}
	if times := opens(); times[1].Sub(times[0]) < retryAfter {
		t.Errorf("retry after %s, want at least the Retry-After of %s", times[1].Sub(times[0]), retryAfter)
	}

	if err := l.stop(t); err != nil {
		t.Fatalf("Listen: %v", err)
	}
	requireCloseFrames(t, sm)
}

func TestListenReconnectsAfterDrop(t *testing.T) {
	t.Parallel()

	// MinBackoff and MaxBackoff are the same, so a backoff waits at least
	// half of it. It is long, so that a reconnect at once has time to spare
	// on a loaded machine.
	const backoff = time.Second
	tests := []struct {
		name string
		// up is the time that connection 1 is up before the drop.
		up time.Duration
		// backsOff reports whether the reconnect waits for the backoff.
		backsOff bool
	}{
		// A connection that was up for less than MaxBackoff counts as a
		// failure, so that a connection that flaps backs off.
		{name: "short-lived connection", up: 0, backsOff: true},
		{name: "long-lived connection", up: backoff, backsOff: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			srv := slackapitest.NewServer(t)
			sm := srv.SocketMode(slackapitest.SocketModeOptions{AppToken: socketAppToken})
			opens := handleOpens(srv, func(_ int, call slackapitest.Call) any { return sm.Open(call) })
			errs := make(chan error, 10)
			hellos := make(chan slackapi.Hello, 10)
			l := startListen(t, socketClient(srv), ackAll, slackapi.ListenOptions{
				OnConnect:  sendTo(hellos),
				OnError:    sendTo(errs),
				MinBackoff: backoff,
				MaxBackoff: backoff,
			})
			recv(t, hellos)
			time.Sleep(tt.up)

			dropped := time.Now()
			sm.Drop(1)
			if err := recv(t, errs); err == nil {
				t.Error("OnError(nil), want the error of the dropped connection")
			}
			if h := recv(t, hellos); h.Host != "applink-2" {
				t.Errorf("OnConnect(%+v), want connection 2", h)
			}
			wait := opens()[1].Sub(dropped)
			if tt.backsOff && wait < backoff/2 {
				t.Errorf("reconnect after %s, want at least %s", wait, backoff/2)
			}
			if !tt.backsOff && wait >= backoff/2 {
				t.Errorf("reconnect after %s, want at once (less than %s)", wait, backoff/2)
			}

			if err := l.stop(t); err != nil {
				t.Fatalf("Listen: %v", err)
			}
			requireCloseFrames(t, sm)
		})
	}
}

func TestListenBacksOffDuringAReplacement(t *testing.T) {
	t.Parallel()

	const (
		minBackoff = 20 * time.Millisecond
		maxBackoff = 300 * time.Millisecond
		// failing is the number of apps.connections.open calls that fail
		// after the replacement starts.
		failing = 4
	)
	tests := []struct {
		name        string
		pingTimeout time.Duration
		start       func(sm *slackapitest.SocketMode)
	}{
		{name: "refresh", start: func(sm *slackapitest.SocketMode) { sm.DisconnectConn(1, "refresh_requested") }},
		{name: "ping timeout", pingTimeout: 100 * time.Millisecond, start: func(sm *slackapitest.SocketMode) { sm.StopPings(1) }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			srv := slackapitest.NewServer(t)
			sm := srv.SocketMode(slackapitest.SocketModeOptions{AppToken: socketAppToken, PingInterval: 10 * time.Millisecond})
			opens := handleOpens(srv, func(n int, call slackapitest.Call) any {
				if n > 1 && n <= 1+failing {
					return map[string]any{"ok": false, "error": "internal_error"}
				}
				return sm.Open(call)
			})
			hellos := make(chan slackapi.Hello, 2)
			l := startListen(t, socketClient(srv), ackAll, slackapi.ListenOptions{
				OnConnect:   sendTo(hellos),
				PingTimeout: tt.pingTimeout,
				MinBackoff:  minBackoff,
				MaxBackoff:  maxBackoff,
			})
			recv(t, hellos)
			// Connection 1 is up for MaxBackoff, so it counts as healthy and
			// resets the failure count. The reset happens once, when the
			// replacement starts, and not at each failure.
			time.Sleep(maxBackoff)

			tt.start(sm)
			if h := recv(t, hellos); h.Host != "applink-2" {
				t.Fatalf("OnConnect(%+v), want connection 2", h)
			}
			// The old connection stays active during the replacement, but
			// each failure in a row doubles the wait.
			times := opens()
			for n := 1; n <= failing; n++ {
				wait := times[n+1].Sub(times[n])
				if want := (minBackoff << (n - 1)) / 2; wait < want {
					t.Errorf("wait after failure %d = %s, want at least %s", n, wait, want)
				}
			}

			if err := l.stop(t); err != nil {
				t.Fatalf("Listen: %v", err)
			}
			requireCloseFrames(t, sm)
		})
	}
}

func TestListenStopDrainsHandlers(t *testing.T) {
	t.Parallel()

	srv := slackapitest.NewServer(t)
	sm := srv.SocketMode(slackapitest.SocketModeOptions{AppToken: socketAppToken, ResendAfter: -1})
	started := make(chan context.Context, 1)
	release := make(chan struct{})
	ctxErrs := make(chan error, 1)
	h := func(ctx context.Context, d *slackapi.Delivery) (any, error) {
		if string(d.Payload) == `{"n":"slow"}` {
			started <- ctx
			<-release
			ctxErrs <- ctx.Err()
		}
		return nil, nil
	}
	l := startListen(t, socketClient(srv), h, fastListen())
	sm.WaitLive(t, socketWait, 1)
	slow := sm.Send("events_api", json.RawMessage(`{"n":"slow"}`))
	handlerCtx := recv(t, started)

	l.cancel()
	fast := sm.Send("events_api", json.RawMessage(`{"n":"fast"}`))
	if env := sm.WaitAcked(t, socketWait, fast); env.Acks[0].Conn != 1 {
		t.Errorf("envelope during the drain acked on connection %d, want 1", env.Acks[0].Conn)
	}
	if c := sm.Conns()[0]; c.CloseSeq != 0 {
		t.Fatalf("connection 1 = %+v, want it open while a handler runs", c)
	}

	close(release)
	if err := l.wait(t); err != nil {
		t.Fatalf("Listen: %v", err)
	}
	if err := recv(t, ctxErrs); err != nil {
		t.Errorf("handler context error = %v before the close, want nil", err)
	}
	if handlerCtx.Err() == nil {
		t.Error("handler context is not cancelled after Listen returned")
	}
	closed := sm.WaitClosed(t, socketWait, 1)
	if env := sm.Envelope(slow); len(env.Acks) != 1 || env.Acks[0].Seq > closed.CloseSeq {
		t.Errorf("slow envelope acks = %+v, close seq %d: want an ack before the close", env.Acks, closed.CloseSeq)
	}
	requireCloseFrames(t, sm)
}

func TestListenStopDrainTimeout(t *testing.T) {
	if testing.Short() {
		t.Skip("waits for the drain timeout of 3 seconds")
	}
	t.Parallel()

	const drainTimeout = 3 * time.Second
	srv := slackapitest.NewServer(t)
	sm := srv.SocketMode(slackapitest.SocketModeOptions{AppToken: socketAppToken, ResendAfter: -1})
	started := make(chan struct{}, 1)
	h := func(ctx context.Context, _ *slackapi.Delivery) (any, error) {
		started <- struct{}{}
		<-ctx.Done()
		return nil, ctx.Err()
	}
	l := startListen(t, socketClient(srv), h, fastListen())
	sm.WaitLive(t, socketWait, 1)
	stuck := sm.Send("events_api", map[string]string{"n": "stuck"})
	recv(t, started)

	stopped := time.Now()
	l.cancel()
	sm.WaitClosed(t, socketWait, 1)
	if wait := time.Since(stopped); wait < drainTimeout || wait > drainTimeout+time.Second {
		t.Errorf("connection 1 closed %s after the stop, want after the drain timeout of %s", wait, drainTimeout)
	}
	// The handler returns only when its context is cancelled, and Listen
	// waits for it.
	if err := l.wait(t); err != nil {
		t.Fatalf("Listen: %v", err)
	}
	if env := sm.Envelope(stuck); len(env.Acks) != 0 {
		t.Errorf("stuck envelope acks = %+v, want none", env.Acks)
	}
	requireCloseFrames(t, sm)
}

// holdUpgrade is an http.RoundTripper that holds the response to a websocket
// upgrade until release is closed. If the request context is done then, it
// closes the connection, as http.Transport does when the context ends before
// it reads the response.
type holdUpgrade struct {
	base    http.RoundTripper
	held    chan struct{}
	release chan struct{}
}

func (h *holdUpgrade) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := h.base.RoundTrip(req)
	if err != nil || resp.StatusCode != http.StatusSwitchingProtocols {
		return resp, err
	}
	close(h.held)
	<-h.release
	if err := req.Context().Err(); err != nil {
		_ = resp.Body.Close()
		return nil, err
	}
	return resp, nil
}

func TestListenStopDuringTheHandshake(t *testing.T) {
	t.Parallel()

	srv := slackapitest.NewServer(t)
	sm := srv.SocketMode(slackapitest.SocketModeOptions{AppToken: socketAppToken})
	hold := &holdUpgrade{base: srv.Client().Transport, held: make(chan struct{}), release: make(chan struct{})}
	api := slackapi.New(slackapi.Options{
		Token:      socketAppToken,
		BaseURL:    srv.URL(),
		HTTPClient: &http.Client{Transport: hold},
	})
	l := startListen(t, api, ackAll, fastListen())
	recv(t, hold.held)

	// Slack accepted the connection, but the client has not read the
	// response yet.
	l.cancel()
	close(hold.release)
	if err := l.wait(t); err != nil {
		t.Fatalf("Listen: %v", err)
	}
	if conns := sm.Conns(); len(conns) != 1 {
		t.Fatalf("connections = %+v, want 1", conns)
	}
	requireCloseFrames(t, sm)
}

func TestListenDialErrorHidesTheTicket(t *testing.T) {
	t.Parallel()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	closed := ln.Addr().String()
	ln.Close()
	srv := slackapitest.NewServer(t)
	srv.Respond("apps.connections.open", map[string]any{"ok": true, "url": "ws://" + closed + "/link/?ticket=secret-ticket"})
	errs := make(chan error, 10)
	opts := fastListen()
	opts.OnError = sendTo(errs)
	l := startListen(t, socketClient(srv), ackAll, opts)

	err = recv(t, errs)
	if strings.Contains(err.Error(), "secret-ticket") {
		t.Errorf("OnError(%v), want no ticket", err)
	}
	if _, ok := errors.AsType[*net.OpError](err); !ok {
		t.Errorf("OnError(%v), want the *net.OpError of the dial", err)
	}
	if err := l.stop(t); err != nil {
		t.Fatalf("Listen: %v", err)
	}
}

func TestListenUsesTheProxyOfTheHTTPClient(t *testing.T) {
	t.Parallel()

	srv := slackapitest.NewServer(t)
	sm := srv.SocketMode(slackapitest.SocketModeOptions{AppToken: socketAppToken})
	var mu sync.Mutex
	var proxied []string
	proxy := httptest.NewServer(&httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			mu.Lock()
			proxied = append(proxied, pr.In.URL.Path+" "+pr.In.Header.Get("Upgrade"))
			mu.Unlock()
			pr.Out.Host = pr.In.Host
		},
	})
	t.Cleanup(proxy.Close)
	proxyURL, err := url.Parse(proxy.URL)
	if err != nil {
		t.Fatal(err)
	}
	transport := &http.Transport{Proxy: http.ProxyURL(proxyURL)}
	t.Cleanup(transport.CloseIdleConnections)
	api := slackapi.New(slackapi.Options{
		Token:      socketAppToken,
		BaseURL:    srv.URL(),
		HTTPClient: &http.Client{Transport: transport},
	})
	hellos := make(chan slackapi.Hello, 1)
	opts := fastListen()
	opts.OnConnect = sendTo(hellos)
	l := startListen(t, api, ackAll, opts)
	recv(t, hellos)
	if err := l.stop(t); err != nil {
		t.Fatalf("Listen: %v", err)
	}
	requireCloseFrames(t, sm)

	mu.Lock()
	defer mu.Unlock()
	for _, want := range []string{"/api/apps.connections.open ", "/link/ websocket"} {
		if !slices.Contains(proxied, want) {
			t.Errorf("proxied requests = %q, want %q", proxied, want)
		}
	}
}
