package slackapitest

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"math/rand/v2"
	"net/http"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
)

// socketModePath is the websocket path of the URLs that Open returns. Slack
// uses the same path.
const socketModePath = "/link/"

const (
	defaultAppID          = "A0123456789"
	defaultPingInterval   = 10 * time.Second
	defaultResendAfter    = 3 * time.Second
	defaultMaxConnections = 10
	maxResends            = 3
	socketReadLimit       = 1 << 20
)

// SocketModeOptions configures a SocketMode.
type SocketModeOptions struct {
	// AppToken is the app-level token that apps.connections.open accepts. A
	// call with another token gets the error code invalid_auth. Empty accepts
	// every token. A call with no token gets not_authed.
	AppToken string
	// AppID is the connection_info.app_id value of hello. Empty uses
	// "A0123456789".
	AppID string
	// PingInterval is the time between two websocket pings on each
	// connection. 0 uses 10 seconds, as Slack does. A negative value sends no
	// pings.
	PingInterval time.Duration
	// ResendAfter is the time that SocketMode waits for an ack before it
	// resends an envelope. 0 uses 3 seconds, the time that Slack gives an app
	// to ack. A negative value turns resends off.
	ResendAfter time.Duration
	// MaxConnections is the largest number of live connections. A connection
	// over the limit gets a disconnect message with the reason
	// too_many_websockets instead of hello. 0 uses 10, the Slack limit.
	MaxConnections int
}

// SocketMode is a fake Slack Socket Mode on a Server. It makes
// apps.connections.open return a ws:// URL on the Server, accepts websocket
// connections to that URL, sends envelopes, and records acks and closes. It is
// safe for concurrent use.
//
// Each connection gets hello at once, then a websocket ping every
// PingInterval. Send sends an envelope to a random live connection, and
// resends it to a random live connection after ResendAfter until it gets an
// ack, at most 3 times, with retry_reason "timeout", as Slack does. A
// connection is live from its hello until it closes. SocketMode never closes a
// connection itself, also not after a disconnect message, except with Drop
// and at the end of the test.
//
// Snapshots of connections and envelopes carry sequence numbers (HelloSeq,
// CloseSeq, and Seq). They come from one counter, so a smaller number happened
// first.
type SocketMode struct {
	srv          *Server
	appID        string
	pingInterval time.Duration
	resendAfter  time.Duration
	maxConns     int
	ctx          context.Context
	cancel       context.CancelFunc
	wg           sync.WaitGroup

	mu        sync.Mutex
	stopped   bool
	appToken  string
	tickets   map[string]string
	conns     []*socketConn
	envelopes map[string]*socketEnvelope
	order     []*socketEnvelope
	seq       int
}

// SocketConn is a snapshot of one websocket connection to a SocketMode.
type SocketConn struct {
	// ID numbers the connections from 1, in the order that SocketMode
	// accepted them.
	ID int
	// Token is the app-level token of the apps.connections.open call that
	// gave the URL.
	Token string
	// Host is the debug_info.host value of the messages on the connection.
	// It is unique for each connection.
	Host string
	// Rejected reports that the connection got a too_many_websockets
	// disconnect message instead of hello.
	Rejected bool
	// HelloSeq is the sequence number of hello. It is 0 when the connection
	// got no hello.
	HelloSeq int
	// CloseSeq is the sequence number of the close. It is 0 while the
	// connection is open.
	CloseSeq int
	// CloseFrame reports that the client sent a close frame.
	CloseFrame bool
	// CloseCode is the status code in the close frame of the client. It is 0
	// when CloseFrame is false.
	CloseCode websocket.StatusCode
	// Dropped reports that SocketMode closed the connection without a close
	// frame: with Drop, or at the end of the test.
	Dropped bool
}

// Live reports whether the connection got hello and is not closed. Send
// sends envelopes only to live connections.
func (c SocketConn) Live() bool {
	return c.HelloSeq != 0 && c.CloseSeq == 0 && !c.Dropped
}

// SocketEnvelope is a snapshot of one envelope that Send sent.
type SocketEnvelope struct {
	// ID is the envelope_id.
	ID string
	// Type is the envelope type, for example "events_api".
	Type string
	// Payload is the payload of the envelope.
	Payload json.RawMessage
	// Deliveries lists the sends of the envelope in order. The first has
	// RetryAttempt 0.
	Deliveries []SocketDelivery
	// Acks lists the acks for the envelope in order.
	Acks []SocketAck
}

// SocketDelivery is one send of an envelope.
type SocketDelivery struct {
	// Conn is the ID of the connection. It is 0 when no connection was live,
	// so the envelope went nowhere.
	Conn int
	// RetryAttempt is the retry_attempt value: 0 for the first send.
	RetryAttempt int
	// RetryReason is the retry_reason value: empty for the first send, else
	// "timeout".
	RetryReason string
	// Seq is the sequence number of the send.
	Seq int
}

// SocketAck is one ack that a client sent.
type SocketAck struct {
	// Conn is the ID of the connection that the ack came on.
	Conn int
	// Payload is the payload field of the ack as the client sent it. It is
	// nil when the ack has no payload.
	Payload json.RawMessage
	// Seq is the sequence number of the ack.
	Seq int
}

type socketConn struct {
	id           int
	ws           *websocket.Conn
	done         chan struct{}
	wake         chan struct{}
	pingsStopped atomic.Bool

	// info and queue are guarded by SocketMode.mu.
	info  SocketConn
	queue [][]byte
}

type socketEnvelope struct {
	// info and timer are guarded by SocketMode.mu.
	info                   SocketEnvelope
	acceptsResponsePayload bool
	timer                  *time.Timer
}

// SocketMode starts a fake Socket Mode on s with opts. It sets the response of
// apps.connections.open to Open, and stops with t.Cleanup: it closes the
// connections that are open without a close frame. Call it at most once for
// each Server.
func (s *Server) SocketMode(opts SocketModeOptions) *SocketMode {
	s.t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	m := &SocketMode{
		srv:          s,
		appID:        cmp.Or(opts.AppID, defaultAppID),
		pingInterval: cmp.Or(opts.PingInterval, defaultPingInterval),
		resendAfter:  cmp.Or(opts.ResendAfter, defaultResendAfter),
		maxConns:     cmp.Or(opts.MaxConnections, defaultMaxConnections),
		ctx:          ctx,
		cancel:       cancel,
		appToken:     opts.AppToken,
		tickets:      map[string]string{},
		envelopes:    map[string]*socketEnvelope{},
	}
	s.mu.Lock()
	twice := s.socketMode != nil
	if !twice {
		s.socketMode = m
	}
	s.mu.Unlock()
	if twice {
		cancel()
		s.t.Fatalf("slackapitest: SocketMode called twice on one Server")
	}
	s.Handle("apps.connections.open", m.Open)
	s.t.Cleanup(m.stop)
	return m
}

// Open answers an apps.connections.open call. SocketMode installs it with
// Handle. A test that sets another Handle function for apps.connections.open,
// for example to return a rate limit first, can call Open to give the normal
// answer: a ws:// URL on the Server, or the error code not_authed or
// invalid_auth.
func (m *SocketMode) Open(call Call) any {
	token, _ := strings.CutPrefix(call.Header.Get("Authorization"), "Bearer ")
	if token == "" {
		return map[string]any{"ok": false, "error": "not_authed"}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.appToken != "" && token != m.appToken {
		return map[string]any{"ok": false, "error": "invalid_auth"}
	}
	ticket := fmt.Sprintf("ticket-%d", len(m.tickets)+1)
	m.tickets[ticket] = token
	wsURL := "ws" + strings.TrimPrefix(m.srv.srv.URL, "http") + socketModePath + "?ticket=" + ticket + "&app_id=" + m.appID
	return map[string]any{"ok": true, "url": wsURL}
}

// SetAppToken changes the app-level token that apps.connections.open accepts.
// It does not change the connections that are open.
func (m *SocketMode) SetAppToken(token string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.appToken = token
}

// Send sends an envelope of type typ, for example "events_api", with payload
// to a random live connection, and returns its envelope ID. payload follows
// the rules of Respond: a json.RawMessage or []byte is sent as is, and any
// other value is sent as JSON. The envelope has accepts_response_payload true
// only for "slash_commands", as Slack sends it. When no connection is live,
// the envelope goes nowhere, and a resend can reach a later connection.
func (m *SocketMode) Send(typ string, payload any) string {
	raw, err := encodeJSON(payload)
	if err != nil {
		m.srv.t.Errorf("slackapitest: encode %s envelope payload: %v", typ, err)
		return ""
	}
	m.mu.Lock()
	env := &socketEnvelope{
		info: SocketEnvelope{
			ID:      fmt.Sprintf("envelope-%d", len(m.order)+1),
			Type:    typ,
			Payload: raw,
		},
		acceptsResponsePayload: typ == "slash_commands",
	}
	m.envelopes[env.info.ID] = env
	m.order = append(m.order, env)
	m.deliverLocked(env)
	m.mu.Unlock()
	m.srv.changes.notify()
	return env.info.ID
}

// Disconnect sends a disconnect message with reason, for example "warning",
// "refresh_requested", or "link_disabled", to every live connection.
func (m *SocketMode) Disconnect(reason string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, c := range m.conns {
		if c.info.Live() {
			m.disconnectLocked(c, reason)
		}
	}
}

// DisconnectConn sends a disconnect message with reason to the connection id.
func (m *SocketMode) DisconnectConn(id int, reason string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if c := m.connLocked(id); c != nil {
		m.disconnectLocked(c, reason)
	}
}

// StopPings stops the websocket pings on the connection id, so that a client
// sees a silent connection.
func (m *SocketMode) StopPings(id int) {
	m.mu.Lock()
	c := m.connLocked(id)
	m.mu.Unlock()
	if c != nil {
		c.pingsStopped.Store(true)
	}
}

// Drop closes the connection id without a close frame, as a broken network
// does.
func (m *SocketMode) Drop(id int) {
	m.mu.Lock()
	c := m.connLocked(id)
	if c != nil && c.info.CloseSeq == 0 {
		c.info.Dropped = true
	}
	m.mu.Unlock()
	if c != nil {
		_ = c.ws.CloseNow()
		m.srv.changes.notify()
	}
}

// Live returns the number of live connections.
func (m *SocketMode) Live() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.liveLocked())
}

// Conns returns a snapshot of every connection, in ID order.
func (m *SocketMode) Conns() []SocketConn {
	m.mu.Lock()
	defer m.mu.Unlock()
	conns := make([]SocketConn, len(m.conns))
	for i, c := range m.conns {
		conns[i] = c.info
	}
	return conns
}

// Closes counts the connections that the client closed: withFrame with a
// close frame and withoutFrame without one. It does not count connections
// that SocketMode dropped.
func (m *SocketMode) Closes() (withFrame, withoutFrame int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, c := range m.conns {
		switch {
		case c.info.CloseSeq == 0 || c.info.Dropped:
		case c.info.CloseFrame:
			withFrame++
		default:
			withoutFrame++
		}
	}
	return withFrame, withoutFrame
}

// Envelope returns a snapshot of the envelope id. An unknown id fails the test
// and returns the zero SocketEnvelope.
func (m *SocketMode) Envelope(id string) SocketEnvelope {
	m.mu.Lock()
	env, ok := m.envelopes[id]
	var snap SocketEnvelope
	if ok {
		snap = env.snapshot()
	}
	m.mu.Unlock()
	if !ok {
		m.srv.t.Errorf("slackapitest: unknown envelope %q", id)
	}
	return snap
}

// Envelopes returns a snapshot of every envelope, in the order of Send.
func (m *SocketMode) Envelopes() []SocketEnvelope {
	m.mu.Lock()
	defer m.mu.Unlock()
	envs := make([]SocketEnvelope, len(m.order))
	for i, env := range m.order {
		envs[i] = env.snapshot()
	}
	return envs
}

// WaitLive waits until at least n connections are live, and returns the live
// connections in ID order. It fails the test with tb.Fatalf when timeout
// passes first, so call it from the test goroutine.
func (m *SocketMode) WaitLive(tb testing.TB, timeout time.Duration, n int) []SocketConn {
	tb.Helper()
	var live []SocketConn
	m.wait(tb, timeout, fmt.Sprintf("%d live connections", n), func() bool {
		live = slices.DeleteFunc(m.Conns(), func(c SocketConn) bool { return !c.Live() })
		return len(live) >= n
	})
	return live
}

// WaitClosed waits until the connection id is closed, and returns it. The
// connection does not need to exist yet. It fails the test with tb.Fatalf when
// timeout passes first, so call it from the test goroutine.
func (m *SocketMode) WaitClosed(tb testing.TB, timeout time.Duration, id int) SocketConn {
	tb.Helper()
	var conn SocketConn
	m.wait(tb, timeout, fmt.Sprintf("close of connection %d", id), func() bool {
		m.mu.Lock()
		defer m.mu.Unlock()
		if id >= 1 && id <= len(m.conns) {
			conn = m.conns[id-1].info
		}
		return conn.CloseSeq != 0
	})
	return conn
}

// WaitAcked waits until the envelope id has an ack, and returns it. It fails
// the test with tb.Fatalf when timeout passes first, so call it from the test
// goroutine.
func (m *SocketMode) WaitAcked(tb testing.TB, timeout time.Duration, id string) SocketEnvelope {
	tb.Helper()
	var env SocketEnvelope
	m.wait(tb, timeout, fmt.Sprintf("ack of %s", id), func() bool {
		env = m.Envelope(id)
		return len(env.Acks) > 0
	})
	return env
}

func (m *SocketMode) wait(tb testing.TB, timeout time.Duration, what string, cond func() bool) {
	tb.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	if err := m.srv.Wait(ctx, cond); err != nil {
		tb.Fatalf("slackapitest: wait for %s: %v", what, err)
	}
}

// serve accepts a websocket connection to a URL from Open, and runs it until
// it closes.
func (m *SocketMode) serve(w http.ResponseWriter, r *http.Request) {
	m.mu.Lock()
	token, ok := m.tickets[r.URL.Query().Get("ticket")]
	if !ok || m.stopped {
		m.mu.Unlock()
		http.NotFound(w, r)
		return
	}
	m.wg.Add(1)
	m.mu.Unlock()
	defer m.wg.Done()

	ws, err := websocket.Accept(w, r, nil)
	if err != nil {
		return
	}
	ws.SetReadLimit(socketReadLimit)

	m.mu.Lock()
	if m.stopped {
		m.mu.Unlock()
		_ = ws.CloseNow()
		return
	}
	id := len(m.conns) + 1
	c := &socketConn{
		id:   id,
		ws:   ws,
		done: make(chan struct{}),
		wake: make(chan struct{}, 1),
		info: SocketConn{ID: id, Token: token, Host: fmt.Sprintf("applink-%d", id)},
	}
	m.conns = append(m.conns, c)
	live := len(m.liveLocked())
	if live >= m.maxConns {
		c.info.Rejected = true
		c.enqueueLocked(m.marshal(map[string]any{
			"type":            "disconnect",
			"reason":          "too_many_websockets",
			"num_connections": live,
			"debug_info":      map[string]any{"host": c.info.Host},
			"connection_info": map[string]any{"app_id": m.appID},
		}))
	} else {
		c.info.HelloSeq = m.nextSeqLocked()
		c.enqueueLocked(m.marshal(map[string]any{
			"type":            "hello",
			"num_connections": live + 1,
			"debug_info":      map[string]any{"host": c.info.Host},
			"connection_info": map[string]any{"app_id": m.appID},
		}))
	}
	m.mu.Unlock()
	m.srv.changes.notify()

	m.wg.Go(func() { m.write(c) })
	m.wg.Go(func() { m.ping(c) })
	m.read(c)
}

func (m *SocketMode) read(c *socketConn) {
	defer close(c.done)
	for {
		typ, data, err := c.ws.Read(m.ctx)
		if err != nil {
			m.closed(c, err)
			return
		}
		if typ != websocket.MessageText {
			m.srv.t.Errorf("slackapitest: socket mode connection %d sent a binary message", c.id)
			continue
		}
		m.ack(c, data)
	}
}

func (m *SocketMode) closed(c *socketConn, err error) {
	code := websocket.CloseStatus(err)
	_ = c.ws.CloseNow()
	m.mu.Lock()
	c.info.CloseSeq = m.nextSeqLocked()
	if code != -1 && !c.info.Dropped {
		c.info.CloseFrame = true
		c.info.CloseCode = code
	}
	m.mu.Unlock()
	m.srv.changes.notify()
}

func (m *SocketMode) ack(c *socketConn, data []byte) {
	var msg struct {
		EnvelopeID string          `json:"envelope_id"`
		Payload    json.RawMessage `json:"payload"`
	}
	if err := json.Unmarshal(data, &msg); err != nil || msg.EnvelopeID == "" {
		m.srv.t.Errorf("slackapitest: socket mode connection %d sent %s, want an ack with an envelope_id", c.id, data)
		return
	}
	m.mu.Lock()
	env := m.envelopes[msg.EnvelopeID]
	if env != nil {
		env.info.Acks = append(env.info.Acks, SocketAck{Conn: c.id, Payload: msg.Payload, Seq: m.nextSeqLocked()})
		if env.timer != nil {
			env.timer.Stop()
		}
	}
	m.mu.Unlock()
	if env == nil {
		m.srv.t.Errorf("slackapitest: socket mode connection %d sent an ack for unknown envelope %q", c.id, msg.EnvelopeID)
		return
	}
	m.srv.changes.notify()
}

// write sends the queued messages of c in order, so that hello is the first
// message on a connection.
func (m *SocketMode) write(c *socketConn) {
	for {
		select {
		case <-c.done:
			return
		case <-c.wake:
		}
		m.mu.Lock()
		queue := c.queue
		c.queue = nil
		m.mu.Unlock()
		for _, msg := range queue {
			if err := c.ws.Write(m.ctx, websocket.MessageText, msg); err != nil {
				return
			}
		}
	}
}

func (m *SocketMode) ping(c *socketConn) {
	if m.pingInterval < 0 {
		return
	}
	ticker := time.NewTicker(m.pingInterval)
	defer ticker.Stop()
	for {
		select {
		case <-c.done:
			return
		case <-ticker.C:
		}
		if c.pingsStopped.Load() {
			return
		}
		// Ping has no timeout: websocket closes the connection when the
		// context of a Ping ends during its write.
		_ = c.ws.Ping(m.ctx)
	}
}

// deliverLocked sends env to a random live connection and arms its resend.
func (m *SocketMode) deliverLocked(env *socketEnvelope) {
	d := SocketDelivery{RetryAttempt: len(env.info.Deliveries)}
	if d.RetryAttempt > 0 {
		d.RetryReason = "timeout"
	}
	if live := m.liveLocked(); len(live) > 0 {
		c := live[rand.IntN(len(live))]
		d.Conn = c.id
		c.enqueueLocked(m.marshal(map[string]any{
			"type":                     env.info.Type,
			"envelope_id":              env.info.ID,
			"payload":                  env.info.Payload,
			"accepts_response_payload": env.acceptsResponsePayload,
			"retry_attempt":            d.RetryAttempt,
			"retry_reason":             d.RetryReason,
		}))
	}
	d.Seq = m.nextSeqLocked()
	env.info.Deliveries = append(env.info.Deliveries, d)
	if d.RetryAttempt < maxResends && m.resendAfter > 0 && !m.stopped {
		env.timer = time.AfterFunc(m.resendAfter, func() { m.resend(env) })
	}
}

func (m *SocketMode) resend(env *socketEnvelope) {
	m.mu.Lock()
	if m.stopped || len(env.info.Acks) > 0 {
		m.mu.Unlock()
		return
	}
	m.deliverLocked(env)
	m.mu.Unlock()
	m.srv.changes.notify()
}

func (m *SocketMode) disconnectLocked(c *socketConn, reason string) {
	c.enqueueLocked(m.marshal(map[string]any{
		"type":       "disconnect",
		"reason":     reason,
		"debug_info": map[string]any{"host": c.info.Host},
	}))
}

func (c *socketConn) enqueueLocked(msg []byte) {
	c.queue = append(c.queue, msg)
	select {
	case c.wake <- struct{}{}:
	default:
	}
}

func (m *SocketMode) liveLocked() []*socketConn {
	var live []*socketConn
	for _, c := range m.conns {
		if c.info.Live() {
			live = append(live, c)
		}
	}
	return live
}

func (m *SocketMode) connLocked(id int) *socketConn {
	if id < 1 || id > len(m.conns) {
		m.srv.t.Errorf("slackapitest: unknown socket mode connection %d", id)
		return nil
	}
	return m.conns[id-1]
}

func (m *SocketMode) nextSeqLocked() int {
	m.seq++
	return m.seq
}

func (m *SocketMode) marshal(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		m.srv.t.Errorf("slackapitest: encode socket mode message: %v", err)
	}
	return b
}

// stop closes every open connection without a close frame and waits for the
// goroutines of SocketMode.
func (m *SocketMode) stop() {
	m.mu.Lock()
	m.stopped = true
	for _, env := range m.order {
		if env.timer != nil {
			env.timer.Stop()
		}
	}
	for _, c := range m.conns {
		if c.info.CloseSeq == 0 {
			c.info.Dropped = true
		}
	}
	m.mu.Unlock()
	m.cancel()
	m.wg.Wait()
}

func (env *socketEnvelope) snapshot() SocketEnvelope {
	info := env.info
	info.Deliveries = slices.Clone(info.Deliveries)
	info.Acks = slices.Clone(info.Acks)
	return info
}

// encodeJSON encodes payload with the rules of Respond.
func encodeJSON(payload any) (json.RawMessage, error) {
	var raw []byte
	switch p := payload.(type) {
	case json.RawMessage:
		raw = p
	case []byte:
		raw = p
	default:
		return json.Marshal(p)
	}
	if !json.Valid(raw) {
		return nil, fmt.Errorf("payload %q is not valid JSON", raw)
	}
	return raw, nil
}
