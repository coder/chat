package slackapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand/v2"
	"net/url"
	"sync"
	"time"

	"github.com/coder/websocket"

	"github.com/coder/chat/internal/ratelimit"
)

const (
	defaultPingTimeout = 30 * time.Second
	defaultMinBackoff  = time.Second
	defaultMaxBackoff  = time.Minute
	// connectTimeout bounds one connect: apps.connections.open, the websocket
	// handshake, and the wait for hello.
	connectTimeout = 30 * time.Second
	// drainTimeout bounds the wait for running handlers before a close. It is
	// the time that Slack gives an app to ack.
	drainTimeout = 3 * time.Second
)

const reasonTooManyWebsockets = "too_many_websockets"

// ErrPingTimeout is the error that Listen gives to ListenOptions.OnError when
// Slack sends no ping on a connection for ListenOptions.PingTimeout. Listen
// then replaces the connection.
var ErrPingTimeout = errors.New("slack: socket mode ping timeout")

var errConnectTimeout = errors.New("slack: socket mode connect timeout")

// ListenOptions configures Listen. The zero value uses the defaults.
//
// OnConnect and OnError run on the goroutine that called Listen, so they must
// return quickly.
type ListenOptions struct {
	// OnConnect is called with the hello message of each connection, when the
	// connection is up. Nil is a no-op.
	OnConnect func(Hello)
	// OnError is called with each error that Listen recovers from: by a retry,
	// or by a replacement of a connection. It is not called with the error
	// that Listen returns. Nil is a no-op.
	OnError func(error)
	// PingTimeout is the longest time without a ping from Slack before Listen
	// replaces a connection. Listen starts to check it after the first ping.
	// 0 or less uses 30 seconds.
	PingTimeout time.Duration
	// MinBackoff is the backoff before the first retry. The backoff doubles
	// with each failure in a row, up to MaxBackoff. Each wait is a random
	// value from half the backoff to the full backoff. 0 or less uses 1
	// second.
	MinBackoff time.Duration
	// MaxBackoff is the largest backoff, and the backoff after
	// too_many_websockets. Each wait is a random value from half the backoff
	// to the full backoff. A connection that was up for at least MaxBackoff
	// starts the backoff again from MinBackoff, and reconnects at once when
	// it drops. 0 or less uses 1 minute.
	MaxBackoff time.Duration
}

func (o ListenOptions) withDefaults() ListenOptions {
	if o.OnConnect == nil {
		o.OnConnect = func(Hello) {}
	}
	if o.OnError == nil {
		o.OnError = func(error) {}
	}
	if o.PingTimeout <= 0 {
		o.PingTimeout = defaultPingTimeout
	}
	if o.MinBackoff <= 0 {
		o.MinBackoff = defaultMinBackoff
	}
	if o.MaxBackoff <= 0 {
		o.MaxBackoff = defaultMaxBackoff
	}
	o.MinBackoff = min(o.MinBackoff, o.MaxBackoff)
	return o
}

// backoff returns the wait before the next connect, after failures failed
// connects in a row. err is the last failure. The wait grows from MinBackoff
// to MaxBackoff, or is MaxBackoff after too_many_websockets, with equal
// jitter: a random value from half the wait to the full wait. A rate limit
// waits at least its RetryAfter.
func (o ListenOptions) backoff(failures int, err error) time.Duration {
	d := ratelimit.BackoffDelay(o.MinBackoff, o.MaxBackoff, failures, 0)
	if slackCode(err) == reasonTooManyWebsockets {
		d = o.MaxBackoff
	}
	d = d/2 + rand.N(d/2+1)
	if rl, ok := errors.AsType[*RateLimited](err); ok {
		d = max(d, rl.RetryAfter)
	}
	return d
}

// Hello is the hello message that starts a Socket Mode connection.
type Hello struct {
	// NumConnections is the number of Socket Mode connections that the app
	// has, this one included.
	NumConnections int
	// AppID is the ID of the app.
	AppID string
	// Host is the Slack host of the connection. It helps in logs.
	Host string
}

// DisconnectError is a disconnect message from Slack that Listen treats as an
// error. Listen returns it for the reason link_disabled (Socket Mode is off in
// the app settings). It gives it to ListenOptions.OnError for the reason
// too_many_websockets, and retries.
type DisconnectError struct {
	// Reason is the reason in the message, for example "link_disabled".
	Reason string
}

// Error returns "slack: socket mode disconnect: <reason>".
func (e *DisconnectError) Error() string {
	return "slack: socket mode disconnect: " + e.Reason
}

var _ error = (*DisconnectError)(nil)

// slackCode returns the Slack error code of err: the code of an *APIError or
// the reason of a *DisconnectError.
func slackCode(err error) string {
	if e, ok := errors.AsType[*APIError](err); ok {
		return e.Code
	}
	if e, ok := errors.AsType[*DisconnectError](err); ok {
		return e.Reason
	}
	return ""
}

// fatal reports whether err is an error that a retry cannot fix: the token is
// not valid for Socket Mode, or Socket Mode is off.
func fatal(err error) bool {
	switch slackCode(err) {
	case "not_authed", "invalid_auth", "account_inactive", "token_revoked", "token_expired",
		"user_removed_from_team", "team_disabled", "not_allowed_token_type", "missing_scope",
		"link_disabled":
		return true
	}
	return false
}

// OpenConnection calls apps.connections.open with the token of c, which must
// be an app-level token (xapp-) with the connections:write scope. It returns
// the websocket URL for a new Socket Mode connection. Listen calls it for
// each connection.
func (c *Client) OpenConnection(ctx context.Context) (string, error) {
	const method = "apps.connections.open"
	var resp struct {
		URL string `json:"url"`
	}
	if err := c.Call(ctx, method, url.Values{}, &resp); err != nil {
		return "", err
	}
	if resp.URL == "" {
		return "", fmt.Errorf("slack: %s returned no url", method)
	}
	return resp.URL, nil
}

// Listen receives deliveries from Slack over Socket Mode, and gives each one
// to h in its own goroutine. c must send the app-level token (xapp-), for
// example api.WithToken(appToken). The HTTP client of c opens the websocket,
// so its proxy settings apply.
//
// Listen keeps one connection up. It acks each envelope on the connection
// that the envelope came on, as the result of h tells (see Handler). When
// Slack asks for a refresh, or sends no ping for ListenOptions.PingTimeout,
// Listen opens a new connection, and closes the old one only when the new one
// is up. The old connection keeps acking until it closes. A connection closes
// in this order: it waits at most 3 seconds for its running handlers, sends a
// close frame, and waits for its last message. Then the context of its
// handlers is cancelled. The context of a handler is not cancelled when ctx
// is done.
//
// Listen gives every other error to ListenOptions.OnError, and retries with
// backoff (see ListenOptions). A rate limit waits at least its Retry-After.
// Listen does not use the retry policy of c: its own backoff retries
// apps.connections.open. It returns the error at once, and does not retry,
// when Slack rejects the token (an *APIError with a code such as invalid_auth
// or token_revoked) or turns Socket Mode off (a *DisconnectError with the
// reason link_disabled). When ctx is done, it returns nil. Before it returns,
// Listen closes every connection and waits for every handler.
//
// Listen logs connects, refreshes, and retries to the logger of c.
func (c *Client) Listen(ctx context.Context, h Handler, opts ListenOptions) error {
	// The retry policy caps a wait at MaxDelay, so it can retry before the
	// Retry-After of a rate limit. The backoff of Listen does not.
	api := *c
	api.retryPolicy = RetryPolicy{MaxAttempts: 1}.withDefaults()
	l := &listener{
		api:     &api,
		handler: h,
		opts:    opts.withDefaults(),
		events:  make(chan connEvent),
		stopped: make(chan struct{}),
	}
	err := l.run(ctx)
	close(l.stopped)
	l.wg.Wait()
	return err
}

// listener is the state of one Listen call. run owns the fields after wg.
type listener struct {
	api     *Client
	handler Handler
	opts    ListenOptions
	events  chan connEvent
	stopped chan struct{}
	wg      sync.WaitGroup

	active      *socketConn
	activeSince time.Time
	replacing   bool
	pending     *socketConn
	retry       <-chan time.Time
	failures    int
}

// connEvent is a message from a connection to run: a hello, a disconnect
// message, or the end of the connection with err.
type connEvent struct {
	conn       *socketConn
	hello      *Hello
	disconnect *DisconnectError
	err        error
}

// run keeps a connection up until ctx is done or a fatal error.
func (l *listener) run(ctx context.Context) error {
	defer func() {
		for _, c := range []*socketConn{l.active, l.pending} {
			if c != nil {
				l.retire(c)
			}
		}
	}()
	for ctx.Err() == nil {
		if (l.active == nil || l.replacing) && l.pending == nil && l.retry == nil {
			l.connect(ctx)
		}
		var connectDone, silent <-chan struct{}
		if l.pending != nil {
			connectDone = l.pending.connectCtx.Done()
		}
		if l.active != nil && !l.replacing {
			silent = l.active.silent
		}
		var err error
		select {
		case <-ctx.Done():
		case <-l.retry:
			l.retry = nil
		case <-connectDone:
			if ctx.Err() == nil {
				err = l.connectFailed(context.Cause(l.pending.connectCtx))
			}
		case <-silent:
			l.api.logger.Warn("slack socket mode ping timeout", "adapter", adapterName, "host", l.active.host)
			l.opts.OnError(ErrPingTimeout)
			l.replace()
		case ev := <-l.events:
			if ctx.Err() == nil {
				err = l.handle(ev)
			}
		}
		if err != nil {
			return err
		}
	}
	return nil
}

func (l *listener) connect(ctx context.Context) {
	c := newSocketConn(ctx, l)
	l.pending = c
	l.wg.Go(c.run)
}

func (l *listener) handle(ev connEvent) error {
	switch ev.conn {
	case l.pending:
		if ev.hello != nil {
			l.promote(*ev.hello)
			return nil
		}
		if ev.disconnect != nil {
			return l.connectFailed(ev.disconnect)
		}
		return l.connectFailed(ev.err)
	case l.active:
		if ev.hello != nil {
			return nil
		}
		if ev.disconnect != nil {
			return l.disconnected(ev.disconnect)
		}
		// No backoff when a replacement already runs, or when the connection
		// was healthy.
		again := l.replacing || l.settle()
		l.drop()
		if !again {
			return l.failed(ev.err)
		}
		l.api.logger.Info("slack socket mode reconnect", "adapter", adapterName, "error", ev.err)
		l.opts.OnError(ev.err)
	}
	return nil
}

// promote makes the pending connection the active one, and retires the old
// active connection.
func (l *listener) promote(h Hello) {
	c := l.pending
	c.cancelConnect()
	c.host = h.Host
	l.pending = nil
	if l.active != nil {
		l.retire(l.active)
	}
	l.active, l.activeSince, l.replacing = c, time.Now(), false
	l.api.logger.Info("slack socket mode connected",
		"adapter", adapterName, "host", h.Host, "num_connections", h.NumConnections, "app_id", h.AppID)
	l.opts.OnConnect(h)
}

// disconnected handles a disconnect message on the active connection.
func (l *listener) disconnected(err *DisconnectError) error {
	if fatal(err) || err.Reason == reasonTooManyWebsockets {
		l.settle()
		l.drop()
		return l.failed(err)
	}
	l.api.logger.Info("slack socket mode refresh", "adapter", adapterName, "reason", err.Reason, "host", l.active.host)
	l.replace()
	return nil
}

// replace starts a replacement of the active connection. The active
// connection keeps reading and acking until the replacement is up.
func (l *listener) replace() {
	l.settle()
	l.replacing = true
}

func (l *listener) connectFailed(err error) error {
	l.retire(l.pending)
	l.pending = nil
	return l.failed(err)
}

// failed returns err when it is fatal. Else it gives err to OnError and starts
// the backoff before the next connect.
func (l *listener) failed(err error) error {
	if fatal(err) {
		return err
	}
	l.failures++
	delay := l.opts.backoff(l.failures, err)
	l.api.logger.Warn("slack socket mode retry",
		"adapter", adapterName, "error", err, "failures", l.failures, "delay", delay)
	l.opts.OnError(err)
	l.retry = time.After(delay)
	return nil
}

// settle resets the failure count when the active connection was up for at
// least MaxBackoff, so that only a connection that flaps backs off. It runs
// once for each active connection, when the connection stops being good: it
// drops, Slack disconnects it, or a replacement starts. During a replacement
// it does nothing, so the failures of the replacement add up. It reports
// whether it reset the count.
func (l *listener) settle() bool {
	if l.active == nil || l.replacing || time.Since(l.activeSince) < l.opts.MaxBackoff {
		return false
	}
	l.failures = 0
	return true
}

// drop retires the active connection.
func (l *listener) drop() {
	l.retire(l.active)
	l.active = nil
	l.replacing = false
}

// retire closes c in the background. Listen retires each connection once.
func (l *listener) retire(c *socketConn) {
	l.wg.Go(c.close)
}

// socketConn is one Socket Mode connection.
type socketConn struct {
	l             *listener
	connectCtx    context.Context
	cancelConnect context.CancelFunc
	// ctx is the context of the handlers. cancel runs after the close.
	ctx    context.Context
	cancel context.CancelFunc
	// ws is set before dialed is closed. It stays nil when the dial fails.
	ws     *websocket.Conn
	dialed chan struct{}
	// done is closed when run returns.
	done chan struct{}
	// silent is closed when no ping came for PingTimeout.
	silent chan struct{}
	// host belongs to listener.run.
	host string

	pingMu    sync.Mutex
	pingTimer *time.Timer
	closing   bool

	mu       sync.Mutex
	inflight int
	idle     chan struct{}
}

func newSocketConn(ctx context.Context, l *listener) *socketConn {
	connectCtx, cancelConnect := context.WithTimeoutCause(ctx, connectTimeout, errConnectTimeout)
	handlerCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	return &socketConn{
		l:             l,
		connectCtx:    connectCtx,
		cancelConnect: cancelConnect,
		ctx:           handlerCtx,
		cancel:        cancel,
		dialed:        make(chan struct{}),
		done:          make(chan struct{}),
		silent:        make(chan struct{}),
	}
}

func (c *socketConn) run() {
	defer close(c.done)
	ws, err := c.dial()
	c.ws = ws
	close(c.dialed)
	if err != nil {
		c.send(connEvent{conn: c, err: err})
		return
	}
	c.read()
}

func (c *socketConn) dial() (*websocket.Conn, error) {
	api := c.l.api
	wsURL, err := api.OpenConnection(c.connectCtx)
	if err != nil {
		return nil, err
	}
	// When Listen stops during the handshake, the cancel closes the TCP
	// connection with no close frame. Listen accepts this short window. A
	// handshake that it cannot cancel could delay its return by up to
	// connectTimeout.
	ws, _, err := websocket.Dial(c.connectCtx, wsURL, &websocket.DialOptions{
		HTTPClient:     api.httpClient,
		OnPingReceived: c.onPing,
	})
	if err != nil {
		return nil, fmt.Errorf("slack: socket mode dial: %w", err)
	}
	ws.SetReadLimit(maxPayloadBytes)
	return ws, nil
}

// socketMessage holds the fields of every message that Slack sends over
// Socket Mode.
type socketMessage struct {
	Type           string          `json:"type"`
	EnvelopeID     string          `json:"envelope_id"`
	Payload        json.RawMessage `json:"payload"`
	RetryAttempt   int             `json:"retry_attempt"`
	RetryReason    string          `json:"retry_reason"`
	Reason         string          `json:"reason"`
	NumConnections int             `json:"num_connections"`
	ConnectionInfo struct {
		AppID string `json:"app_id"`
	} `json:"connection_info"`
	DebugInfo struct {
		Host string `json:"host"`
	} `json:"debug_info"`
}

type socketAck struct {
	EnvelopeID string `json:"envelope_id"`
	Payload    any    `json:"payload,omitempty"`
}

// read reads messages until the connection ends. Its context is never
// cancelled, because a cancelled read closes the connection without a close
// frame. Only Close ends it.
func (c *socketConn) read() {
	logger := c.l.api.logger
	for {
		typ, data, err := c.ws.Read(context.Background())
		if err != nil {
			c.send(connEvent{conn: c, err: fmt.Errorf("slack: socket mode connection closed: %w", err)})
			return
		}
		var msg socketMessage
		if typ != websocket.MessageText || json.Unmarshal(data, &msg) != nil {
			logger.Debug("slack socket mode message ignored: not JSON text", "adapter", adapterName)
			continue
		}
		switch msg.Type {
		case "hello":
			c.send(connEvent{conn: c, hello: &Hello{
				NumConnections: msg.NumConnections,
				AppID:          msg.ConnectionInfo.AppID,
				Host:           msg.DebugInfo.Host,
			}})
		case "disconnect":
			c.send(connEvent{conn: c, disconnect: &DisconnectError{Reason: msg.Reason}})
		default:
			if msg.EnvelopeID == "" {
				logger.Debug("slack socket mode message ignored: no envelope_id", "adapter", adapterName, "type", msg.Type)
				continue
			}
			c.begin()
			c.l.wg.Go(func() { c.handle(msg) })
		}
	}
}

// handle runs the handler for an envelope, and acks it when the handler
// returns no error.
func (c *socketConn) handle(msg socketMessage) {
	defer c.end()
	logger := c.l.api.logger
	resp, err := c.l.handler(c.ctx, &Delivery{
		Type:         DeliveryType(msg.Type),
		Payload:      msg.Payload,
		RetryAttempt: msg.RetryAttempt,
		RetryReason:  msg.RetryReason,
	})
	if err != nil {
		logger.Debug("slack socket mode envelope not acked",
			"adapter", adapterName, "type", msg.Type, "envelope_id", msg.EnvelopeID, "error", err)
		return
	}
	ack, err := json.Marshal(socketAck{EnvelopeID: msg.EnvelopeID, Payload: resp})
	if err != nil {
		logger.Warn("slack socket mode envelope not acked: encode response",
			"adapter", adapterName, "type", msg.Type, "envelope_id", msg.EnvelopeID, "error", err)
		return
	}
	if err := c.ws.Write(c.ctx, websocket.MessageText, ack); err != nil {
		logger.Debug("slack socket mode ack failed",
			"adapter", adapterName, "type", msg.Type, "envelope_id", msg.EnvelopeID, "error", err)
	}
}

// send gives ev to listener.run, or drops it when Listen has stopped.
func (c *socketConn) send(ev connEvent) {
	select {
	case c.l.events <- ev:
	case <-c.l.stopped:
	}
}

// onPing resets the ping timer. The first ping starts it. The timer fires at
// most once, because onPing resets it only when it has not fired.
func (c *socketConn) onPing(context.Context, []byte) bool {
	c.pingMu.Lock()
	defer c.pingMu.Unlock()
	switch {
	case c.closing:
	case c.pingTimer == nil:
		c.pingTimer = time.AfterFunc(c.l.opts.PingTimeout, func() { close(c.silent) })
	case c.pingTimer.Stop():
		c.pingTimer.Reset(c.l.opts.PingTimeout)
	}
	return true
}

func (c *socketConn) begin() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.inflight == 0 {
		c.idle = make(chan struct{})
	}
	c.inflight++
}

func (c *socketConn) end() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.inflight--
	if c.inflight == 0 {
		close(c.idle)
	}
}

// close closes the connection: it waits for the running handlers, sends the
// close frame, waits for the read loop to end, and cancels the context of the
// handlers.
func (c *socketConn) close() {
	c.cancelConnect()
	c.pingMu.Lock()
	c.closing = true
	if c.pingTimer != nil {
		c.pingTimer.Stop()
	}
	c.pingMu.Unlock()
	<-c.dialed
	if c.ws != nil {
		c.drain()
		if err := c.ws.Close(websocket.StatusNormalClosure, ""); err != nil {
			c.l.api.logger.Debug("slack socket mode close failed", "adapter", adapterName, "error", err)
		}
		<-c.done
	}
	c.cancel()
}

// drain waits until no handler runs, the read loop ends, or drainTimeout
// passes. Envelopes that come during the wait also run, and drain waits for
// them too.
func (c *socketConn) drain() {
	timer := time.NewTimer(drainTimeout)
	defer timer.Stop()
	for {
		c.mu.Lock()
		inflight, idle := c.inflight, c.idle
		c.mu.Unlock()
		if inflight == 0 {
			return
		}
		select {
		case <-idle:
		case <-c.done:
			return
		case <-timer.C:
			return
		}
	}
}
