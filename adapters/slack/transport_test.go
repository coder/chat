package slack_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/coder/chat"
	"github.com/coder/chat/adapters/slack"
	"github.com/coder/chat/adapters/slack/slackapi"
	"github.com/coder/chat/adapters/slack/slackapi/slackapitest"
	"github.com/coder/chat/state/memory"
)

const (
	transportSecret   = "secret"
	transportAppToken = "xapp-test"
	transportWait     = 5 * time.Second
)

var transportNow = time.Unix(1_700_000_000, 0)

// Payloads in the Socket Mode shape. serveDelivery sends each one over HTTP in
// the shape that Slack uses there.
const (
	mentionPayload = `{
		"type":"event_callback",
		"team_id":"T1",
		"event_id":"EvMention",
		"event":{"type":"app_mention","channel":"C1","user":"U1","text":"<@UBOT> hi","ts":"111.000"}
	}`
	commandPayload = `{
		"command":"/deploy",
		"text":"prod now",
		"team_id":"T1",
		"channel_id":"C1",
		"channel_name":"general",
		"user_id":"U1",
		"trigger_id":"trigger-1",
		"response_url":"https://hooks.slack.com/commands/T1/1",
		"api_app_id":"A1"
	}`
	blockActionsPayload = `{
		"type":"block_actions",
		"team":{"id":"T1"},
		"user":{"id":"U1"},
		"channel":{"id":"C1"},
		"container":{"channel_id":"C1","message_ts":"333.000"},
		"trigger_id":"trigger-2",
		"response_url":"https://hooks.slack.com/actions/T1/2",
		"actions":[{"action_id":"approve","block_id":"b1","value":"yes","type":"button","action_ts":"333.100"}]
	}`
)

// newTransportAdapter returns an adapter with both transports, on srv. A
// single-install adapter serves team T1. A multi-tenant adapter has an install
// only for team T2.
func newTransportAdapter(t *testing.T, srv *slackapitest.Server, multiTenant bool) *slack.Adapter {
	t.Helper()
	opts := slack.Options{
		SigningSecret: transportSecret,
		AppToken:      transportAppToken,
		APIBaseURL:    srv.URL(),
		Client:        srv.Client(),
		Now:           func() time.Time { return transportNow },
	}
	if multiTenant {
		store := newFakeInstallStore()
		store.set("T2", chat.Install{Tenant: "T2", Credential: slack.SlackInstall{BotToken: "xoxb-T2", BotUserID: "UBOT2"}})
		opts.InstallStore = store
	} else {
		opts.BotToken = "xoxb-test"
		opts.TeamID = "T1"
		opts.BotUserID = "UBOT"
		opts.BotID = "BBOT"
	}
	adapter, err := slack.New(t.Context(), opts)
	if err != nil {
		t.Fatalf("new slack adapter: %v", err)
	}
	return adapter
}

// socketPayload returns payload as the fake Socket Mode sends it. The fake
// encodes each envelope with encoding/json, which compacts the payload and
// escapes HTML characters such as <. The tests send the same bytes over HTTP,
// so that the Raw fields of the Events are equal.
func socketPayload(t *testing.T, payload string) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(json.RawMessage(payload))
	if err != nil {
		t.Fatalf("encode payload: %v", err)
	}
	return b
}

// serveDelivery sends payload to h as Slack sends it over HTTP: a JSON body for
// an Events API payload, and a form for a slash command or an interaction. A
// retry attempt above 0 sets the retry headers.
func serveDelivery(t *testing.T, h http.Handler, typ slackapi.DeliveryType, payload []byte, attempt int, reason string) *httptest.ResponseRecorder {
	t.Helper()
	body, contentType := payload, "application/json"
	switch typ {
	case slackapi.DeliveryEventsAPI:
	case slackapi.DeliveryInteractive:
		body, contentType = []byte(url.Values{"payload": {string(payload)}}.Encode()), "application/x-www-form-urlencoded"
	case slackapi.DeliverySlashCommands:
		var fields map[string]string
		if err := json.Unmarshal(payload, &fields); err != nil {
			t.Fatalf("decode command payload: %v", err)
		}
		form := url.Values{}
		for key, value := range fields {
			form.Set(key, value)
		}
		body, contentType = []byte(form.Encode()), "application/x-www-form-urlencoded"
	default:
		t.Fatalf("no HTTP shape for delivery type %q", typ)
	}
	req := httptest.NewRequest(http.MethodPost, "/slack", bytes.NewReader(body))
	req.Header.Set("Content-Type", contentType)
	if attempt > 0 {
		req.Header.Set("X-Slack-Retry-Num", strconv.Itoa(attempt))
		req.Header.Set("X-Slack-Retry-Reason", reason)
	}
	slackapitest.SignRequest(req, body, transportSecret, transportNow)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// recorder is a chat.DispatchFunc that records each Event and returns err.
type recorder struct {
	events chan *chat.Event
	err    error
}

func newRecorder(err error) *recorder {
	return &recorder{events: make(chan *chat.Event, 16), err: err}
}

func (r *recorder) dispatch(_ context.Context, ev *chat.Event) error {
	r.events <- ev
	return r.err
}

// wait returns the next n Events.
func (r *recorder) wait(t *testing.T, n int) []*chat.Event {
	t.Helper()
	events := make([]*chat.Event, 0, n)
	for range n {
		select {
		case ev := <-r.events:
			events = append(events, ev)
		case <-time.After(transportWait):
			t.Fatalf("got %d dispatched events, want %d", len(events), n)
		}
	}
	return events
}

// drain returns the Events that wait has not returned.
func (r *recorder) drain() []*chat.Event {
	var events []*chat.Event
	for {
		select {
		case ev := <-r.events:
			events = append(events, ev)
		default:
			return events
		}
	}
}

// listen runs l.Listen in a goroutine. The returned function cancels it, waits
// for it to return, and fails the test when it returns an error.
func listen(t *testing.T, l chat.Listener, dispatch chat.DispatchFunc) (stop func()) {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- l.Listen(ctx, dispatch) }()
	end := sync.OnceValue(func() error {
		cancel()
		select {
		case err := <-done:
			return err
		case <-time.After(transportWait):
			return errors.New("Listen did not return after its context ended")
		}
	})
	t.Cleanup(func() { _ = end() })
	return func() {
		t.Helper()
		if err := end(); err != nil {
			t.Fatalf("Listen: %v", err)
		}
	}
}

func requireEqualEvents(t *testing.T, overHTTP, overSocket []*chat.Event) {
	t.Helper()
	if len(overHTTP) != len(overSocket) {
		t.Fatalf("Webhook dispatched %d events, Listen dispatched %d", len(overHTTP), len(overSocket))
	}
	for i := range overHTTP {
		if !reflect.DeepEqual(overHTTP[i], overSocket[i]) {
			t.Errorf("event %d:\nWebhook: %#v\nListen:  %#v", i, overHTTP[i], overSocket[i])
		}
	}
}

// One payload gives the same Event and the same ack over Webhook and over
// Listen.
func TestTransportParity(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		typ         slackapi.DeliveryType
		payload     string
		multiTenant bool
		// wantTenant is the Tenant of the dispatched Event. It is empty when
		// the adapter dispatches no Event.
		wantTenant string
	}{
		{name: "app mention", typ: slackapi.DeliveryEventsAPI, payload: mentionPayload, wantTenant: "T1"},
		{
			name: "direct message",
			typ:  slackapi.DeliveryEventsAPI,
			payload: `{
				"type":"event_callback",
				"team_id":"T1",
				"event_id":"EvDM",
				"event":{"type":"message","channel":"D1","channel_type":"im","user":"U1","text":"hello","ts":"222.000"}
			}`,
			wantTenant: "T1",
		},
		{name: "slash command", typ: slackapi.DeliverySlashCommands, payload: commandPayload, wantTenant: "T1"},
		{name: "block_actions interaction", typ: slackapi.DeliveryInteractive, payload: blockActionsPayload, wantTenant: "T1"},
		{
			name: "ignored event",
			typ:  slackapi.DeliveryEventsAPI,
			payload: `{
				"type":"event_callback",
				"team_id":"T1",
				"event_id":"EvReaction",
				"event":{"type":"reaction_added","user":"U1","reaction":"thumbsup","item":{"type":"message","channel":"C1","ts":"111.000"}}
			}`,
		},
		{
			name:    "ignored interaction",
			typ:     slackapi.DeliveryInteractive,
			payload: `{"type":"view_submission","team":{"id":"T1"},"user":{"id":"U1"},"view":{"id":"V1"}}`,
		},
		{
			name:        "workspace with no install",
			typ:         slackapi.DeliveryEventsAPI,
			payload:     strings.ReplaceAll(mentionPayload, `"T1"`, `"TX"`),
			multiTenant: true,
		},
		{
			name: "multi-tenant install",
			typ:  slackapi.DeliveryEventsAPI,
			payload: `{
				"type":"event_callback",
				"team_id":"T2",
				"event_id":"EvT2",
				"event":{"type":"app_mention","channel":"C2","user":"U2","text":"<@UBOT2> hi","ts":"444.000"}
			}`,
			multiTenant: true,
			wantTenant:  "T2",
		},
		{
			name:        "multi-tenant slash command",
			typ:         slackapi.DeliverySlashCommands,
			payload:     strings.ReplaceAll(commandPayload, `"T1"`, `"T2"`),
			multiTenant: true,
			wantTenant:  "T2",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			srv := slackapitest.NewServer(t)
			sm := srv.SocketMode(slackapitest.SocketModeOptions{AppToken: transportAppToken, ResendAfter: -1})
			adapter := newTransportAdapter(t, srv, tt.multiTenant)
			payload := socketPayload(t, tt.payload)

			overHTTP := newRecorder(nil)
			rec := serveDelivery(t, adapter.Webhook(overHTTP.dispatch), tt.typ, payload, 0, "")
			if rec.Code != http.StatusOK || rec.Body.Len() != 0 {
				t.Errorf("Webhook answered %d %q, want 200 with no body", rec.Code, rec.Body.String())
			}

			overSocket := newRecorder(nil)
			stop := listen(t, adapter, overSocket.dispatch)
			sm.WaitLive(t, transportWait, 1)
			env := sm.WaitAcked(t, transportWait, sm.Send(string(tt.typ), payload))
			stop()
			if len(env.Acks) != 1 || env.Acks[0].Payload != nil {
				t.Errorf("Listen acks = %+v, want one ack with no payload", env.Acks)
			}

			events := overHTTP.drain()
			requireEqualEvents(t, events, overSocket.drain())
			switch {
			case tt.wantTenant == "" && len(events) != 0:
				t.Errorf("dispatched %d events, want an Ignored Event", len(events))
			case tt.wantTenant != "" && (len(events) != 1 || events[0].Tenant != tt.wantTenant):
				t.Errorf("dispatched %#v, want one event of tenant %s", events, tt.wantTenant)
			}
		})
	}
}

// When the runtime rejects a delivery at admission, Webhook and Listen give
// the same answer: no ack for an event or an interaction, so Slack can deliver
// it again, and the busy reply for a slash command. Each redelivery gives the
// same Event over both transports.
func TestTransportParityAdmissionRejected(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		typ     slackapi.DeliveryType
		payload string
		// wantBusyReply is true when the delivery gets the busy reply, and
		// false when it gets no ack.
		wantBusyReply bool
	}{
		{name: "app mention", typ: slackapi.DeliveryEventsAPI, payload: mentionPayload},
		{name: "block_actions interaction", typ: slackapi.DeliveryInteractive, payload: blockActionsPayload},
		{name: "slash command", typ: slackapi.DeliverySlashCommands, payload: commandPayload, wantBusyReply: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			srv := slackapitest.NewServer(t)
			// The fake sends an envelope at most 4 times. A short resend time
			// lets the test see every send of an envelope that gets no ack.
			resendAfter := 20 * time.Millisecond
			deliveries := 4
			if tt.wantBusyReply {
				resendAfter, deliveries = -1, 1
			}
			sm := srv.SocketMode(slackapitest.SocketModeOptions{AppToken: transportAppToken, ResendAfter: resendAfter})
			adapter := newTransportAdapter(t, srv, false)
			payload := socketPayload(t, tt.payload)

			overSocket := newRecorder(chat.ErrAdmissionRejected)
			stop := listen(t, adapter, overSocket.dispatch)
			sm.WaitLive(t, transportWait, 1)
			id := sm.Send(string(tt.typ), payload)
			socketEvents := overSocket.wait(t, deliveries)
			// Listen waits for its handlers and sends their acks before it
			// closes the connection, so the fake has every ack after the close.
			stop()
			sm.WaitClosed(t, transportWait, 1)
			env := sm.Envelope(id)
			if len(env.Deliveries) != deliveries {
				t.Fatalf("Listen got %d sends of the envelope, want %d", len(env.Deliveries), deliveries)
			}

			overHTTP := newRecorder(chat.ErrAdmissionRejected)
			h := adapter.Webhook(overHTTP.dispatch)
			var rec *httptest.ResponseRecorder
			for _, d := range env.Deliveries {
				rec = serveDelivery(t, h, tt.typ, payload, d.RetryAttempt, d.RetryReason)
				if !tt.wantBusyReply && rec.Code != http.StatusServiceUnavailable {
					t.Errorf("attempt %d: Webhook status = %d, want 503", d.RetryAttempt, rec.Code)
				}
			}
			requireEqualEvents(t, overHTTP.drain(), socketEvents)

			if !tt.wantBusyReply {
				if len(env.Acks) != 0 {
					t.Errorf("Listen acks = %+v, want no ack", env.Acks)
				}
				return
			}
			if rec.Code != http.StatusOK || rec.Header().Get("Content-Type") != "application/json" {
				t.Fatalf("Webhook answered %d with Content-Type %q, want 200 with a JSON body", rec.Code, rec.Header().Get("Content-Type"))
			}
			if len(env.Acks) != 1 || string(env.Acks[0].Payload) != rec.Body.String() {
				t.Fatalf("Listen acks = %+v, want one ack with the Webhook body %s as payload", env.Acks, rec.Body.String())
			}
			var reply struct {
				Text string `json:"text"`
			}
			if err := json.Unmarshal(env.Acks[0].Payload, &reply); err != nil || !strings.Contains(reply.Text, "at capacity") {
				t.Errorf("busy reply = %s, want a JSON object with the busy text", env.Acks[0].Payload)
			}
		})
	}
}

func TestWebhookNeedsSigningSecret(t *testing.T) {
	t.Parallel()

	adapter, err := slack.New(t.Context(), slack.Options{AppToken: transportAppToken, BotToken: "xoxb-test", TeamID: "T1", BotUserID: "UBOT", BotID: "BBOT"})
	if err != nil {
		t.Fatalf("new slack adapter: %v", err)
	}
	overHTTP := newRecorder(nil)
	rec := serveDelivery(t, adapter.Webhook(overHTTP.dispatch), slackapi.DeliveryEventsAPI, []byte(mentionPayload), 0, "")
	if rec.Code != http.StatusNotFound {
		t.Errorf("Webhook with no signing secret answered %d, want 404", rec.Code)
	}
	if events := overHTTP.drain(); len(events) != 0 {
		t.Errorf("Webhook with no signing secret dispatched %d events", len(events))
	}
}

func TestListenNeedsAppToken(t *testing.T) {
	t.Parallel()

	srv := slackapitest.NewServer(t)
	adapter, err := slack.New(t.Context(), slack.Options{
		SigningSecret: transportSecret,
		BotToken:      "xoxb-test",
		TeamID:        "T1",
		BotUserID:     "UBOT",
		BotID:         "BBOT",
		APIBaseURL:    srv.URL(),
		Client:        srv.Client(),
	})
	if err != nil {
		t.Fatalf("new slack adapter: %v", err)
	}
	if err := adapter.Listen(t.Context(), newRecorder(nil).dispatch); err == nil || !strings.Contains(err.Error(), "app token") {
		t.Errorf("Adapter.Listen with no app token: err = %v, want an app token error", err)
	}

	bot, err := chat.New(t.Context(), chat.WithState(memory.New()), chat.WithAdapter(adapter))
	if err != nil {
		t.Fatalf("new chat runtime: %v", err)
	}
	if err := bot.Listen(t.Context(), "slack"); err == nil || !strings.Contains(err.Error(), "app token") {
		t.Errorf("Chat.Listen with no app token: err = %v, want an app token error", err)
	}
	if calls := srv.Calls("apps.connections.open"); len(calls) != 0 {
		t.Errorf("apps.connections.open calls = %d, want 0", len(calls))
	}
}

// Chat.Listen runs the Slack adapter over Socket Mode: a delivery reaches its
// handler and gets its ack, and Runtime Shutdown stops Listen, which closes
// the connection with a close frame.
func TestChatListenRunsSocketMode(t *testing.T) {
	t.Parallel()

	srv := slackapitest.NewServer(t)
	sm := srv.SocketMode(slackapitest.SocketModeOptions{AppToken: transportAppToken, ResendAfter: -1})
	bot, err := chat.New(t.Context(), chat.WithState(memory.New()), chat.WithAdapter(newTransportAdapter(t, srv, false)))
	if err != nil {
		t.Fatalf("new chat runtime: %v", err)
	}
	mentions := make(chan *chat.MessageEvent, 1)
	bot.OnNewMention(func(_ context.Context, ev *chat.MessageEvent) error {
		mentions <- ev
		return nil
	})
	done := make(chan error, 1)
	// The Listen context never ends, so only Shutdown can stop Listen.
	go func() { done <- bot.Listen(context.Background(), "slack") }()

	sm.WaitLive(t, transportWait, 1)
	env := sm.WaitAcked(t, transportWait, sm.Send(string(slackapi.DeliveryEventsAPI), socketPayload(t, mentionPayload)))
	select {
	case ev := <-mentions:
		if ev.Event.ID != "EvMention" || ev.Message.Text != "<@UBOT> hi" {
			t.Errorf("mention = %+v, want event EvMention", ev.Event)
		}
	default:
		t.Errorf("envelope %s got an ack, but no mention reached the handler", env.ID)
	}

	if err := bot.Shutdown(t.Context()); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Chat.Listen: %v", err)
		}
	case <-time.After(transportWait):
		t.Fatal("Chat.Listen did not return after Shutdown")
	}
	if c := sm.WaitClosed(t, transportWait, 1); !c.CloseFrame || c.CloseCode != websocket.StatusNormalClosure {
		t.Errorf("connection = %+v, want a close frame with %v", c, websocket.StatusNormalClosure)
	}
}
