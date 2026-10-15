package slackapitest_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/coder/chat/adapters/slack/slackapi/slackapitest"
)

const (
	socketToken = "xapp-test"
	waitTimeout = 5 * time.Second
)

// socketMessage is any message that the fake sends on a connection.
type socketMessage struct {
	Type           string `json:"type"`
	Reason         string `json:"reason"`
	NumConnections int    `json:"num_connections"`
	ConnectionInfo struct {
		AppID string `json:"app_id"`
	} `json:"connection_info"`
	DebugInfo struct {
		Host string `json:"host"`
	} `json:"debug_info"`
	EnvelopeID             string          `json:"envelope_id"`
	Payload                json.RawMessage `json:"payload"`
	AcceptsResponsePayload bool            `json:"accepts_response_payload"`
	RetryAttempt           int             `json:"retry_attempt"`
	RetryReason            string          `json:"retry_reason"`
}

// socketClient is a minimal Socket Mode client: it reads every message into a
// channel, answers pings, and counts them.
type socketClient struct {
	ws      *websocket.Conn
	msgs    chan socketMessage
	pings   atomic.Int64
	pinged  chan struct{}
	readErr error
	done    chan struct{}
}

// openConnection calls apps.connections.open with token and returns the
// response.
func openConnection(t *testing.T, srv *slackapitest.Server, token string) (ok bool, wsURL, errCode string) {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, srv.URL()+"/apps.connections.open", nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatalf("apps.connections.open: %v", err)
	}
	defer resp.Body.Close()
	var body struct {
		OK    bool   `json:"ok"`
		URL   string `json:"url"`
		Error string `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode apps.connections.open: %v", err)
	}
	return body.OK, body.URL, body.Error
}

// dialSocket opens a connection with socketToken and closes it without a close
// frame when the test ends.
func dialSocket(t *testing.T, srv *slackapitest.Server) *socketClient {
	t.Helper()
	ok, wsURL, code := openConnection(t, srv, socketToken)
	if !ok {
		t.Fatalf("apps.connections.open error = %q", code)
	}
	return dialURL(t, srv, wsURL)
}

func dialURL(t *testing.T, srv *slackapitest.Server, wsURL string) *socketClient {
	t.Helper()
	c := &socketClient{
		msgs:   make(chan socketMessage, 256),
		pinged: make(chan struct{}, 256),
		done:   make(chan struct{}),
	}
	ws, _, err := websocket.Dial(t.Context(), wsURL, &websocket.DialOptions{
		HTTPClient: srv.Client(),
		OnPingReceived: func(context.Context, []byte) bool {
			c.pings.Add(1)
			select {
			case c.pinged <- struct{}{}:
			default:
			}
			return true
		},
	})
	if err != nil {
		t.Fatalf("dial %s: %v", wsURL, err)
	}
	c.ws = ws
	go c.read(t)
	t.Cleanup(func() {
		_ = ws.CloseNow()
		<-c.done
	})
	return c
}

func (c *socketClient) read(t *testing.T) {
	defer close(c.done)
	defer close(c.msgs)
	for {
		_, data, err := c.ws.Read(context.Background())
		if err != nil {
			c.readErr = err
			return
		}
		var msg socketMessage
		if err := json.Unmarshal(data, &msg); err != nil {
			t.Errorf("decode message %s: %v", data, err)
			return
		}
		c.msgs <- msg
	}
}

// next returns the next message and fails the test if none comes.
func (c *socketClient) next(t *testing.T) socketMessage {
	t.Helper()
	select {
	case msg, ok := <-c.msgs:
		if !ok {
			t.Fatalf("connection ended: %v", c.readErr)
		}
		return msg
	case <-time.After(waitTimeout):
		t.Fatal("no message")
	}
	return socketMessage{}
}

// hello reads hello and fails the test if the first message is not hello.
func (c *socketClient) hello(t *testing.T) socketMessage {
	t.Helper()
	msg := c.next(t)
	if msg.Type != "hello" {
		t.Fatalf("first message = %+v, want hello", msg)
	}
	return msg
}

func (c *socketClient) send(t *testing.T, v any) {
	t.Helper()
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("encode message: %v", err)
	}
	if err := c.ws.Write(t.Context(), websocket.MessageText, data); err != nil {
		t.Fatalf("write message: %v", err)
	}
}

func (c *socketClient) ack(t *testing.T, envelopeID string) {
	t.Helper()
	c.send(t, map[string]any{"envelope_id": envelopeID})
}

// waitPings waits until the client has received n pings in total.
func (c *socketClient) waitPings(t *testing.T, n int64) {
	t.Helper()
	timeout := time.After(waitTimeout)
	for c.pings.Load() < n {
		select {
		case <-c.pinged:
		case <-timeout:
			t.Fatalf("pings = %d, want %d", c.pings.Load(), n)
		}
	}
}

func TestSocketModeOpen(t *testing.T) {
	t.Parallel()

	srv := slackapitest.NewServer(t)
	sm := srv.SocketMode(slackapitest.SocketModeOptions{AppToken: "xapp-1", AppID: "A1"})

	if ok, _, code := openConnection(t, srv, ""); ok || code != "not_authed" {
		t.Fatalf("no token: ok = %v, error = %q, want not_authed", ok, code)
	}
	if ok, _, code := openConnection(t, srv, "xapp-2"); ok || code != "invalid_auth" {
		t.Fatalf("wrong token: ok = %v, error = %q, want invalid_auth", ok, code)
	}
	ok, wsURL, code := openConnection(t, srv, "xapp-1")
	if !ok {
		t.Fatalf("right token: error = %q", code)
	}
	u, err := url.Parse(wsURL)
	if err != nil {
		t.Fatalf("parse URL %q: %v", wsURL, err)
	}
	if u.Scheme != "ws" || u.Path != "/link/" || u.Query().Get("ticket") == "" || u.Query().Get("app_id") != "A1" {
		t.Fatalf("URL = %q, want ws://.../link/?ticket=...&app_id=A1", wsURL)
	}
	if !strings.HasPrefix(srv.URL(), "http://"+u.Host+"/") {
		t.Fatalf("URL host %q is not the Server host %q", u.Host, srv.URL())
	}

	sm.SetAppToken("xapp-new")
	if ok, _, code := openConnection(t, srv, "xapp-1"); ok || code != "invalid_auth" {
		t.Fatalf("old token after SetAppToken: ok = %v, error = %q, want invalid_auth", ok, code)
	}
	ok, newURL, _ := openConnection(t, srv, "xapp-new")
	if !ok {
		t.Fatal("new token after SetAppToken: not ok")
	}
	if calls := srv.Calls("apps.connections.open"); len(calls) != 5 {
		t.Fatalf("apps.connections.open calls = %d, want 5", len(calls))
	}

	// A URL from before SetAppToken still works, and each connection keeps
	// the token of its URL.
	dialURL(t, srv, wsURL).hello(t)
	dialURL(t, srv, newURL).hello(t)
	conns := sm.WaitLive(t, waitTimeout, 2)
	if conns[0].Token != "xapp-1" || conns[1].Token != "xapp-new" {
		t.Fatalf("tokens = %q, %q, want xapp-1, xapp-new", conns[0].Token, conns[1].Token)
	}
}

func TestSocketModeOpenAnyToken(t *testing.T) {
	t.Parallel()

	srv := slackapitest.NewServer(t)
	srv.SocketMode(slackapitest.SocketModeOptions{})
	if ok, _, code := openConnection(t, srv, "xapp-anything"); !ok {
		t.Fatalf("error = %q, want ok with an empty AppToken", code)
	}
}

func TestSocketModeOpenComposesWithHandle(t *testing.T) {
	t.Parallel()

	srv := slackapitest.NewServer(t)
	sm := srv.SocketMode(slackapitest.SocketModeOptions{})
	var calls atomic.Int64
	srv.Handle("apps.connections.open", func(call slackapitest.Call) any {
		if calls.Add(1) == 1 {
			return slackapitest.Response{
				StatusCode: http.StatusTooManyRequests,
				Header:     http.Header{"Retry-After": {"1"}},
				Body:       map[string]any{"ok": false, "error": "ratelimited"},
			}
		}
		return sm.Open(call)
	})

	if ok, _, code := openConnection(t, srv, socketToken); ok || code != "ratelimited" {
		t.Fatalf("first call: ok = %v, error = %q, want ratelimited", ok, code)
	}
	dialSocket(t, srv).hello(t)
}

func TestSocketModeUnknownTicket(t *testing.T) {
	t.Parallel()

	srv := slackapitest.NewServer(t)
	srv.SocketMode(slackapitest.SocketModeOptions{})
	wsURL := "ws" + strings.TrimPrefix(strings.TrimSuffix(srv.URL(), "/api"), "http") + "/link/?ticket=nope"
	_, resp, err := websocket.Dial(t.Context(), wsURL, &websocket.DialOptions{HTTPClient: srv.Client()})
	if err == nil {
		t.Fatal("dial with an unknown ticket succeeded")
	}
	if resp == nil || resp.StatusCode != http.StatusNotFound {
		t.Fatalf("response = %v, want 404", resp)
	}
}

func TestSocketModeHello(t *testing.T) {
	t.Parallel()

	srv := slackapitest.NewServer(t)
	sm := srv.SocketMode(slackapitest.SocketModeOptions{AppID: "A42", PingInterval: -1})

	first := dialSocket(t, srv).hello(t)
	if first.NumConnections != 1 || first.ConnectionInfo.AppID != "A42" || first.DebugInfo.Host != "applink-1" {
		t.Fatalf("first hello = %+v", first)
	}
	second := dialSocket(t, srv).hello(t)
	if second.NumConnections != 2 || second.DebugInfo.Host != "applink-2" {
		t.Fatalf("second hello = %+v", second)
	}

	conns := sm.WaitLive(t, waitTimeout, 2)
	if len(conns) != 2 || conns[0].ID != 1 || conns[1].ID != 2 {
		t.Fatalf("live connections = %+v", conns)
	}
	if conns[0].Token != socketToken || conns[0].Host != "applink-1" || !conns[0].Live() {
		t.Fatalf("connection 1 = %+v", conns[0])
	}
	if conns[0].HelloSeq == 0 || conns[0].HelloSeq >= conns[1].HelloSeq {
		t.Fatalf("HelloSeq = %d, %d, want increasing", conns[0].HelloSeq, conns[1].HelloSeq)
	}
	if got := sm.Live(); got != 2 {
		t.Fatalf("Live = %d, want 2", got)
	}
}

func TestSocketModeSendAndAck(t *testing.T) {
	t.Parallel()

	srv := slackapitest.NewServer(t)
	sm := srv.SocketMode(slackapitest.SocketModeOptions{PingInterval: -1, ResendAfter: -1})
	c := dialSocket(t, srv)
	c.hello(t)
	sm.WaitLive(t, waitTimeout, 1)

	id := sm.Send("events_api", map[string]any{"event_id": "Ev1"})
	msg := c.next(t)
	if msg.Type != "events_api" || msg.EnvelopeID != id || string(msg.Payload) != `{"event_id":"Ev1"}` {
		t.Fatalf("envelope = %+v", msg)
	}
	if msg.AcceptsResponsePayload || msg.RetryAttempt != 0 || msg.RetryReason != "" {
		t.Fatalf("envelope = %+v, want no response payload and no retry", msg)
	}
	if got := sm.Envelope(id); len(got.Acks) != 0 || len(got.Deliveries) != 1 || got.Deliveries[0].Conn != 1 {
		t.Fatalf("envelope before the ack = %+v", got)
	}
	c.send(t, map[string]any{"envelope_id": id, "payload": map[string]any{"text": "hi"}})
	env := sm.WaitAcked(t, waitTimeout, id)
	if env.ID != id || env.Type != "events_api" || string(env.Payload) != `{"event_id":"Ev1"}` {
		t.Fatalf("envelope = %+v", env)
	}
	if len(env.Acks) != 1 || env.Acks[0].Conn != 1 || string(env.Acks[0].Payload) != `{"text":"hi"}` {
		t.Fatalf("acks = %+v", env.Acks)
	}
	if env.Acks[0].Seq <= env.Deliveries[0].Seq {
		t.Fatalf("ack Seq %d is not after delivery Seq %d", env.Acks[0].Seq, env.Deliveries[0].Seq)
	}

	cmd := sm.Send("slash_commands", json.RawMessage(`{"command":"/hi"}`))
	msg = c.next(t)
	if msg.Type != "slash_commands" || !msg.AcceptsResponsePayload || string(msg.Payload) != `{"command":"/hi"}` {
		t.Fatalf("slash command envelope = %+v", msg)
	}
	c.ack(t, cmd)
	if acks := sm.WaitAcked(t, waitTimeout, cmd).Acks; acks[0].Payload != nil {
		t.Fatalf("ack payload = %s, want nil", acks[0].Payload)
	}

	other := sm.Send("hello_world", []byte(`"x"`))
	if msg := c.next(t); msg.Type != "hello_world" || msg.EnvelopeID != other {
		t.Fatalf("unknown type envelope = %+v", msg)
	}
	if envs := sm.Envelopes(); len(envs) != 3 || envs[0].ID != id || envs[1].ID != cmd || envs[2].ID != other {
		t.Fatalf("Envelopes = %+v", envs)
	}
}

func TestSocketModeResend(t *testing.T) {
	t.Parallel()

	srv := slackapitest.NewServer(t)
	sm := srv.SocketMode(slackapitest.SocketModeOptions{PingInterval: -1, ResendAfter: 10 * time.Millisecond})
	c := dialSocket(t, srv)
	c.hello(t)
	sm.WaitLive(t, waitTimeout, 1)

	id := sm.Send("events_api", map[string]any{})
	for attempt := range 4 {
		msg := c.next(t)
		wantReason := "timeout"
		if attempt == 0 {
			wantReason = ""
		}
		if msg.EnvelopeID != id || msg.RetryAttempt != attempt || msg.RetryReason != wantReason {
			t.Fatalf("send %d = %+v, want retry_attempt %d and retry_reason %q", attempt, msg, attempt, wantReason)
		}
	}
	// After 3 resends the fake gives up.
	select {
	case msg := <-c.msgs:
		t.Fatalf("unexpected message after the last resend: %+v", msg)
	case <-time.After(100 * time.Millisecond):
	}
	env := sm.Envelope(id)
	if len(env.Deliveries) != 4 || len(env.Acks) != 0 {
		t.Fatalf("envelope = %+v, want 4 sends and no ack", env)
	}

	// An ack after a resend stops the resends.
	id = sm.Send("events_api", map[string]any{})
	if msg := c.next(t); msg.RetryAttempt != 0 {
		t.Fatalf("first send = %+v", msg)
	}
	msg := c.next(t)
	if msg.RetryAttempt != 1 {
		t.Fatalf("resend = %+v", msg)
	}
	c.ack(t, id)
	sm.WaitAcked(t, waitTimeout, id)
	select {
	case msg := <-c.msgs:
		if msg.RetryAttempt > 2 {
			t.Fatalf("resend after the ack: %+v", msg)
		}
	case <-time.After(100 * time.Millisecond):
	}
}

func TestSocketModeSendWithNoLiveConnection(t *testing.T) {
	t.Parallel()

	srv := slackapitest.NewServer(t)
	// The client must connect before the last resend, so ResendAfter is not
	// too short.
	sm := srv.SocketMode(slackapitest.SocketModeOptions{PingInterval: -1, ResendAfter: 250 * time.Millisecond})

	id := sm.Send("events_api", map[string]any{})
	if env := sm.Envelope(id); len(env.Deliveries) != 1 || env.Deliveries[0].Conn != 0 {
		t.Fatalf("envelope = %+v, want one send to no connection", env)
	}
	c := dialSocket(t, srv)
	c.hello(t)
	msg := c.next(t)
	if msg.EnvelopeID != id || msg.RetryAttempt == 0 || msg.RetryReason != "timeout" {
		t.Fatalf("resend = %+v", msg)
	}
	c.ack(t, id)
	env := sm.WaitAcked(t, waitTimeout, id)
	if last := env.Deliveries[len(env.Deliveries)-1]; last.Conn != 1 || env.Acks[0].Conn != 1 {
		t.Fatalf("envelope = %+v, want the last send and the ack on connection 1", env)
	}
}

func TestSocketModeSendPicksRandomConnection(t *testing.T) {
	t.Parallel()

	srv := slackapitest.NewServer(t)
	sm := srv.SocketMode(slackapitest.SocketModeOptions{PingInterval: -1, ResendAfter: -1})
	dialSocket(t, srv).hello(t)
	dialSocket(t, srv).hello(t)
	sm.WaitLive(t, waitTimeout, 2)

	counts := map[int]int{}
	for range 40 {
		env := sm.Envelope(sm.Send("events_api", map[string]any{}))
		counts[env.Deliveries[0].Conn]++
	}
	if counts[1] == 0 || counts[2] == 0 || counts[1]+counts[2] != 40 {
		t.Fatalf("sends per connection = %v, want both connections", counts)
	}
}

func TestSocketModeMaxConnections(t *testing.T) {
	t.Parallel()

	srv := slackapitest.NewServer(t)
	sm := srv.SocketMode(slackapitest.SocketModeOptions{AppID: "A1", PingInterval: -1, ResendAfter: -1, MaxConnections: 1})
	first := dialSocket(t, srv)
	first.hello(t)

	second := dialSocket(t, srv)
	msg := second.next(t)
	if msg.Type != "disconnect" || msg.Reason != "too_many_websockets" || msg.NumConnections != 1 ||
		msg.ConnectionInfo.AppID != "A1" || msg.DebugInfo.Host != "applink-2" {
		t.Fatalf("over the limit = %+v, want a too_many_websockets disconnect", msg)
	}
	conns := sm.Conns()
	if len(conns) != 2 || !conns[1].Rejected || conns[1].HelloSeq != 0 || conns[1].Live() {
		t.Fatalf("connections = %+v, want connection 2 rejected", conns)
	}
	if got := sm.Live(); got != 1 {
		t.Fatalf("Live = %d, want 1", got)
	}
	for range 5 {
		if env := sm.Envelope(sm.Send("events_api", map[string]any{})); env.Deliveries[0].Conn != 1 {
			t.Fatalf("send went to connection %d, want 1", env.Deliveries[0].Conn)
		}
	}

	if err := second.ws.Close(websocket.StatusNormalClosure, ""); err != nil {
		t.Fatalf("close connection 2: %v", err)
	}
	if err := first.ws.Close(websocket.StatusNormalClosure, ""); err != nil {
		t.Fatalf("close connection 1: %v", err)
	}
	sm.WaitClosed(t, waitTimeout, 1)
	if msg := dialSocket(t, srv).next(t); msg.Type != "hello" || msg.NumConnections != 1 {
		t.Fatalf("after the close = %+v, want hello", msg)
	}
}

func TestSocketModeDisconnect(t *testing.T) {
	t.Parallel()

	srv := slackapitest.NewServer(t)
	sm := srv.SocketMode(slackapitest.SocketModeOptions{PingInterval: -1})
	first := dialSocket(t, srv)
	first.hello(t)
	second := dialSocket(t, srv)
	second.hello(t)
	sm.WaitLive(t, waitTimeout, 2)

	sm.DisconnectConn(2, "refresh_requested")
	if msg := second.next(t); msg.Type != "disconnect" || msg.Reason != "refresh_requested" || msg.DebugInfo.Host != "applink-2" {
		t.Fatalf("connection 2 = %+v, want a refresh_requested disconnect", msg)
	}
	sm.Disconnect("warning")
	if msg := first.next(t); msg.Type != "disconnect" || msg.Reason != "warning" || msg.DebugInfo.Host != "applink-1" {
		t.Fatalf("connection 1 = %+v, want only a warning disconnect", msg)
	}
	if msg := second.next(t); msg.Type != "disconnect" || msg.Reason != "warning" {
		t.Fatalf("connection 2 = %+v, want a warning disconnect", msg)
	}
	// A disconnect message does not close the connection.
	if got := sm.Live(); got != 2 {
		t.Fatalf("Live = %d, want 2", got)
	}
}

func TestSocketModePings(t *testing.T) {
	t.Parallel()

	srv := slackapitest.NewServer(t)
	sm := srv.SocketMode(slackapitest.SocketModeOptions{PingInterval: 5 * time.Millisecond})
	first := dialSocket(t, srv)
	first.hello(t)
	second := dialSocket(t, srv)
	second.hello(t)
	first.waitPings(t, 2)
	second.waitPings(t, 2)
	ctx, cancel := context.WithTimeout(t.Context(), waitTimeout)
	defer cancel()
	if err := srv.Wait(ctx, func() bool {
		conns := sm.Conns()
		return conns[0].Pings >= 2 && conns[1].Pings >= 2
	}); err != nil {
		t.Fatalf("wait for 2 answered pings on each connection: %v, conns = %+v", err, sm.Conns())
	}

	sm.StopPings(1)
	// One ping can be on the way when StopPings returns.
	stoppedAt := first.pings.Load() + 1
	second.waitPings(t, second.pings.Load()+10)
	if got := first.pings.Load(); got > stoppedAt {
		t.Fatalf("connection 1 pings = %d after StopPings, want at most %d", got, stoppedAt)
	}
	if got := sm.Live(); got != 2 {
		t.Fatalf("Live = %d, want 2", got)
	}
}

func TestSocketModeNoPings(t *testing.T) {
	t.Parallel()

	srv := slackapitest.NewServer(t)
	srv.SocketMode(slackapitest.SocketModeOptions{PingInterval: -1})
	c := dialSocket(t, srv)
	c.hello(t)
	time.Sleep(50 * time.Millisecond)
	if got := c.pings.Load(); got != 0 {
		t.Fatalf("pings = %d, want 0", got)
	}
}

func TestSocketModeCloses(t *testing.T) {
	t.Parallel()

	srv := slackapitest.NewServer(t)
	sm := srv.SocketMode(slackapitest.SocketModeOptions{PingInterval: -1})
	first := dialSocket(t, srv)
	first.hello(t)
	second := dialSocket(t, srv)
	second.hello(t)
	third := dialSocket(t, srv)
	third.hello(t)
	sm.WaitLive(t, waitTimeout, 3)

	if err := first.ws.Close(websocket.StatusNormalClosure, ""); err != nil {
		t.Fatalf("close connection 1: %v", err)
	}
	conn := sm.WaitClosed(t, waitTimeout, 1)
	if !conn.CloseFrame || conn.CloseCode != websocket.StatusNormalClosure || conn.Dropped || conn.Live() {
		t.Fatalf("connection 1 = %+v, want a normal close frame", conn)
	}
	if conns := sm.Conns(); conns[1].HelloSeq >= conn.CloseSeq {
		t.Fatalf("hello of connection 2 (%d) is not before the close of connection 1 (%d)", conns[1].HelloSeq, conn.CloseSeq)
	}

	if err := second.ws.CloseNow(); err != nil {
		t.Fatalf("close connection 2 without a close frame: %v", err)
	}
	if conn := sm.WaitClosed(t, waitTimeout, 2); conn.CloseFrame || conn.CloseCode != 0 || conn.Dropped {
		t.Fatalf("connection 2 = %+v, want no close frame", conn)
	}

	sm.Drop(3)
	if conn := sm.WaitClosed(t, waitTimeout, 3); !conn.Dropped || conn.CloseFrame {
		t.Fatalf("connection 3 = %+v, want dropped", conn)
	}
	<-third.done
	if code := websocket.CloseStatus(third.readErr); code != -1 {
		t.Fatalf("client of connection 3 got close code %d, want none", code)
	}

	if withFrame, withoutFrame := sm.Closes(); withFrame != 1 || withoutFrame != 1 {
		t.Fatalf("Closes = %d, %d, want 1, 1", withFrame, withoutFrame)
	}
	if got := sm.Live(); got != 0 {
		t.Fatalf("Live = %d, want 0", got)
	}
}

// cleanupTB runs the cleanups of a Server when the test calls cleanup, not at
// the end of the test.
type cleanupTB struct {
	testing.TB

	cleanups []func()
}

func (tb *cleanupTB) Cleanup(f func()) {
	tb.cleanups = append(tb.cleanups, f)
}

func (tb *cleanupTB) cleanup() {
	for _, f := range slices.Backward(tb.cleanups) {
		f()
	}
}

func TestSocketModeStopDropsOpenConnections(t *testing.T) {
	t.Parallel()

	tb := &cleanupTB{TB: t}
	srv := slackapitest.NewServer(tb)
	sm := srv.SocketMode(slackapitest.SocketModeOptions{PingInterval: time.Millisecond, ResendAfter: time.Millisecond})
	client := dialSocket(t, srv)
	sm.WaitLive(t, waitTimeout, 1)
	sm.Send("events_api", map[string]any{})

	tb.cleanup()
	select {
	case <-client.done:
	case <-time.After(waitTimeout):
		t.Fatal("connection still open after the cleanup of the Server")
	}
	if code := websocket.CloseStatus(client.readErr); code != -1 {
		t.Fatalf("close code = %d, want none", code)
	}
	if conns := sm.Conns(); !conns[0].Dropped || conns[0].CloseFrame {
		t.Fatalf("connection = %+v, want dropped", conns[0])
	}
}

func TestSocketModeInvalidMessages(t *testing.T) {
	t.Parallel()

	tb := &coreRecordingTB{TB: t}
	srv := slackapitest.NewServer(tb)
	sm := srv.SocketMode(slackapitest.SocketModeOptions{PingInterval: -1, ResendAfter: -1})
	c := dialSocket(t, srv)
	c.hello(t)
	sm.WaitLive(t, waitTimeout, 1)
	id := sm.Send("events_api", map[string]any{})
	c.next(t)

	c.ack(t, "envelope-404")
	c.send(t, map[string]any{"type": "pong"})
	if err := c.ws.Write(t.Context(), websocket.MessageBinary, []byte(`{}`)); err != nil {
		t.Fatalf("write binary message: %v", err)
	}
	c.ack(t, id)
	sm.WaitAcked(t, waitTimeout, id)
	if got := tb.errorCount(); got != 3 {
		t.Fatalf("Errorf calls = %d, want 3", got)
	}
	if got := sm.Envelope("envelope-404"); got.ID != "" || tb.errorCount() != 4 {
		t.Fatalf("unknown envelope = %+v, Errorf calls = %d, want zero and 4", got, tb.errorCount())
	}
}

func TestServerWait(t *testing.T) {
	t.Parallel()

	srv := slackapitest.NewServer(t)
	srv.Respond("auth.test", json.RawMessage(`{"ok":true}`))

	done := make(chan error, 1)
	go func() {
		done <- srv.Wait(t.Context(), func() bool { return len(srv.Calls("auth.test")) == 1 })
	}()
	corePost(t, srv, "auth.test", "application/json", `{}`)
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Wait = %v", err)
		}
	case <-time.After(waitTimeout):
		t.Fatal("Wait did not return after the call")
	}

	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Millisecond)
	defer cancel()
	if err := srv.Wait(ctx, func() bool { return false }); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Wait = %v, want context.DeadlineExceeded", err)
	}
}

func TestServerWaitSeesHandleChanges(t *testing.T) {
	t.Parallel()

	srv := slackapitest.NewServer(t)
	checks := make(chan struct{}, 10)
	var handled atomic.Bool
	srv.Handle("auth.test", func(slackapitest.Call) any {
		// Change the state only after Wait checked cond for the recorded
		// call, so that only the end of this function can wake Wait.
		select {
		case <-checks:
		case <-time.After(waitTimeout):
		}
		handled.Store(true)
		return json.RawMessage(`{"ok":true}`)
	})

	done := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(t.Context(), waitTimeout)
		defer cancel()
		done <- srv.Wait(ctx, func() bool {
			select {
			case checks <- struct{}{}:
			default:
			}
			return handled.Load()
		})
	}()
	<-checks
	corePost(t, srv, "auth.test", "application/json", `{}`)
	if err := <-done; err != nil {
		t.Fatalf("Wait = %v, want nil after the Handle function changed the state", err)
	}
}
