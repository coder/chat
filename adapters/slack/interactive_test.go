package slack_test

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/chat"
	"github.com/coder/chat/adapters/slack"
)

func serveSignedSlackForm(t *testing.T, handler http.Handler, now time.Time, form url.Values) *httptest.ResponseRecorder {
	t.Helper()
	body := []byte(form.Encode())
	req := httptest.NewRequest(http.MethodPost, "/slack", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	signSlackRequest(req, "secret", now, body)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	return rec
}

func serveSignedSlackInteractivity(t *testing.T, handler http.Handler, now time.Time, payload string) *httptest.ResponseRecorder {
	t.Helper()
	form := url.Values{}
	form.Set("payload", payload)
	return serveSignedSlackForm(t, handler, now, form)
}

func TestSlackSlashCommandNormalization(t *testing.T) {
	t.Parallel()

	api := newSlackAPIServer(t)
	now := time.Unix(1_700_000_000, 0)
	bot := newSlackRuntime(t, api, slack.Options{
		SigningSecret: "secret",
		BotToken:      "xoxb-test",
		Now:           func() time.Time { return now },
	})

	var got *chat.CommandEvent
	bot.OnCommand(func(ctx context.Context, ev *chat.CommandEvent) error {
		got = ev
		return nil
	})

	handler, err := bot.Webhook("slack")
	if err != nil {
		t.Fatalf("webhook: %v", err)
	}

	form := url.Values{}
	form.Set("command", "/deploy")
	form.Set("text", "staging now")
	form.Set("team_id", "T1")
	form.Set("channel_id", "C1")
	form.Set("channel_name", "general")
	form.Set("user_id", "U1")
	form.Set("trigger_id", "trigger-123")
	form.Set("response_url", "https://hooks.slack.com/commands/T1/123")
	form.Set("api_app_id", "A1")

	rec := serveSignedSlackForm(t, handler, now, form)
	if rec.Code != http.StatusOK {
		t.Fatalf("command status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if got == nil {
		t.Fatal("command handler not called")
	}
	if got.Command.Name != "/deploy" {
		t.Fatalf("command name = %q", got.Command.Name)
	}
	if got.Command.Text != "staging now" {
		t.Fatalf("command text = %q", got.Command.Text)
	}
	if len(got.Command.Args) != 2 || got.Command.Args[0] != "staging" || got.Command.Args[1] != "now" {
		t.Fatalf("args = %#v", got.Command.Args)
	}
	wantActor := chat.Actor{Adapter: "slack", Tenant: "T1", ID: "U1", BotKind: chat.BotHuman}
	if got.Command.Actor != wantActor {
		t.Fatalf("actor = %#v", got.Command.Actor)
	}
	if got.Event.Tenant != "T1" {
		t.Fatalf("tenant = %q", got.Event.Tenant)
	}
	if got.Thread.ID() == "" {
		t.Fatal("thread id should be present")
	}
	// response_url preserved on the Platform Escape Hatch; trigger_id
	// preservation is proven end-to-end by TestSlackOpenModalFromRawCommand.
	if slack.ResponseURLForTest(got.Command.Raw) != "https://hooks.slack.com/commands/T1/123" {
		t.Fatalf("response_url not preserved on command escape hatch: %q", slack.ResponseURLForTest(got.Command.Raw))
	}
}

func TestSlackSlashCommandRequiredFieldValidation(t *testing.T) {
	t.Parallel()

	api := newSlackAPIServer(t)
	now := time.Unix(1_700_000_000, 0)
	bot := newSlackRuntime(t, api, slack.Options{
		SigningSecret: "secret",
		BotToken:      "xoxb-test",
		Now:           func() time.Time { return now },
	})
	bot.OnCommand(func(context.Context, *chat.CommandEvent) error {
		t.Fatal("invalid command must not dispatch")
		return nil
	})
	handler, err := bot.Webhook("slack")
	if err != nil {
		t.Fatalf("webhook: %v", err)
	}

	base := func() url.Values {
		form := url.Values{}
		form.Set("command", "/deploy")
		form.Set("team_id", "T1")
		form.Set("channel_id", "C1")
		form.Set("user_id", "U1")
		form.Set("trigger_id", "trig")
		return form
	}

	for _, missing := range []string{"command", "team_id", "channel_id", "user_id", "trigger_id"} {
		form := base()
		form.Del(missing)
		rec := serveSignedSlackForm(t, handler, now, form)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("missing %q status = %d, want 400", missing, rec.Code)
		}
	}
}

func TestSlackSlashCommandTenantMismatchRejected(t *testing.T) {
	t.Parallel()

	now := time.Unix(1_700_000_000, 0)
	api := newSlackAPIServer(t)
	adapter, err := slack.New(context.Background(), slack.Options{
		SigningSecret: "secret",
		BotToken:      "xoxb-test",
		TeamID:        "T1",
		BotUserID:     "UBOT",
		BotID:         "BBOT",
		APIBaseURL:    api.URL,
		Client:        api.Client(),
		Now:           func() time.Time { return now },
	})
	if err != nil {
		t.Fatalf("new adapter: %v", err)
	}
	handler := adapter.Webhook(func(context.Context, *chat.Event) error {
		t.Fatal("cross-tenant command reached dispatch")
		return nil
	})

	form := url.Values{}
	form.Set("command", "/deploy")
	form.Set("team_id", "T2")
	form.Set("channel_id", "C1")
	form.Set("user_id", "U1")
	form.Set("trigger_id", "trig")
	rec := serveSignedSlackForm(t, handler, now, form)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("cross-tenant status = %d, want 400", rec.Code)
	}
}

func TestSlackBlockActionsInteraction(t *testing.T) {
	t.Parallel()

	api := newSlackAPIServer(t)
	now := time.Unix(1_700_000_000, 0)
	bot := newSlackRuntime(t, api, slack.Options{
		SigningSecret: "secret",
		BotToken:      "xoxb-test",
		Now:           func() time.Time { return now },
	})

	var got *chat.InteractionEvent
	bot.OnInteraction(func(ctx context.Context, ev *chat.InteractionEvent) error {
		got = ev
		return nil
	})
	handler, err := bot.Webhook("slack")
	if err != nil {
		t.Fatalf("webhook: %v", err)
	}

	payload := `{
		"type":"block_actions",
		"team":{"id":"T1"},
		"user":{"id":"U1"},
		"channel":{"id":"C1"},
		"container":{"channel_id":"C1","message_ts":"333.000"},
		"trigger_id":"trigger-999",
		"response_url":"https://hooks.slack.com/actions/T1/999",
		"actions":[{"action_id":"approve","block_id":"b1","value":"yes","type":"button"}]
	}`
	rec := serveSignedSlackInteractivity(t, handler, now, payload)
	if rec.Code != http.StatusOK {
		t.Fatalf("interaction status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if got == nil {
		t.Fatal("interaction handler not called")
	}
	if got.Interaction.ActionID != "approve" {
		t.Fatalf("action id = %q", got.Interaction.ActionID)
	}
	if got.Interaction.Kind != chat.InteractionBlockAction {
		t.Fatalf("kind = %v", got.Interaction.Kind)
	}
	if got.Interaction.Value != "yes" {
		t.Fatalf("button value = %q, want %q", got.Interaction.Value, "yes")
	}
	if got.Interaction.Values != nil {
		t.Fatalf("button values = %#v, want nil", got.Interaction.Values)
	}
	wantActor := chat.Actor{Adapter: "slack", Tenant: "T1", ID: "U1", BotKind: chat.BotHuman}
	if got.Interaction.Actor != wantActor {
		t.Fatalf("actor = %#v", got.Interaction.Actor)
	}
	if slack.ResponseURLForTest(got.Interaction.Raw) == "" {
		t.Fatal("response_url missing from interaction escape hatch")
	}
}

func TestSlackMenuSelectionInteraction(t *testing.T) {
	t.Parallel()

	api := newSlackAPIServer(t)
	now := time.Unix(1_700_000_000, 0)
	bot := newSlackRuntime(t, api, slack.Options{
		SigningSecret: "secret",
		BotToken:      "xoxb-test",
		Now:           func() time.Time { return now },
	})
	var got *chat.Interaction
	bot.OnInteraction(func(ctx context.Context, ev *chat.InteractionEvent) error {
		got = ev.Interaction
		return nil
	})
	handler, err := bot.Webhook("slack")
	if err != nil {
		t.Fatalf("webhook: %v", err)
	}

	// Static/overflow menu selection threaded under a parent message.
	payload := `{
		"type":"block_actions",
		"team":{"id":"T1"},
		"user":{"id":"U1"},
		"container":{"channel_id":"C2","thread_ts":"100.000","message_ts":"444.000"},
		"actions":[{"action_id":"pick","type":"static_select",
			"selected_option":{"text":{"type":"plain_text","text":"Staging"},"value":"staging"}}]
	}`
	rec := serveSignedSlackInteractivity(t, handler, now, payload)
	if rec.Code != http.StatusOK {
		t.Fatalf("menu status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if got == nil {
		t.Fatal("menu interaction not dispatched")
	}
	if got.ActionID != "pick" {
		t.Fatalf("action id = %q", got.ActionID)
	}
	if got.Value != "staging" {
		t.Fatalf("selected value = %q, want %q", got.Value, "staging")
	}
}

func TestSlackViewSubmissionAndUnknownTypesIgnored(t *testing.T) {
	t.Parallel()

	api := newSlackAPIServer(t)
	now := time.Unix(1_700_000_000, 0)
	bot := newSlackRuntime(t, api, slack.Options{
		SigningSecret: "secret",
		BotToken:      "xoxb-test",
		Now:           func() time.Time { return now },
	})
	bot.OnInteraction(func(context.Context, *chat.InteractionEvent) error {
		t.Fatal("view_submission / unknown type must be ignored in this slice")
		return nil
	})
	handler, err := bot.Webhook("slack")
	if err != nil {
		t.Fatalf("webhook: %v", err)
	}

	for _, payload := range []string{
		`{"type":"view_submission","team":{"id":"T1"},"user":{"id":"U1"},"view":{"id":"V1"}}`,
		`{"type":"shortcut","team":{"id":"T1"},"user":{"id":"U1"}}`,
		`{"type":"some_future_type","team":{"id":"T1"}}`,
	} {
		rec := serveSignedSlackInteractivity(t, handler, now, payload)
		if rec.Code != http.StatusOK {
			t.Fatalf("ignored type status = %d, body = %s", rec.Code, rec.Body.String())
		}
	}
}

func TestSlackInteractivityMalformedAndBadSignatureRejected(t *testing.T) {
	t.Parallel()

	api := newSlackAPIServer(t)
	now := time.Unix(1_700_000_000, 0)
	bot := newSlackRuntime(t, api, slack.Options{
		SigningSecret: "secret",
		BotToken:      "xoxb-test",
		Now:           func() time.Time { return now },
	})
	handler, err := bot.Webhook("slack")
	if err != nil {
		t.Fatalf("webhook: %v", err)
	}

	// Malformed JSON payload (valid signature) -> 400.
	rec := serveSignedSlackInteractivity(t, handler, now, `{not json`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("malformed payload status = %d, want 400", rec.Code)
	}

	// Bad signature -> 401, never an ignore.
	body := []byte(url.Values{"payload": {`{"type":"block_actions"}`}}.Encode())
	badReq := httptest.NewRequest(http.MethodPost, "/slack", bytes.NewReader(body))
	badReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	badReq.Header.Set("X-Slack-Request-Timestamp", formatUnix(now))
	badReq.Header.Set("X-Slack-Signature", "v0=bad")
	badRec := httptest.NewRecorder()
	handler.ServeHTTP(badRec, badReq)
	if badRec.Code != http.StatusUnauthorized {
		t.Fatalf("bad signature status = %d, want 401", badRec.Code)
	}
}

func TestSlackPostNativeAndAdapterMismatch(t *testing.T) {
	t.Parallel()

	api := newSlackAPIServer(t)
	now := time.Unix(1_700_000_000, 0)
	bot := newSlackRuntime(t, api, slack.Options{
		SigningSecret: "secret",
		BotToken:      "xoxb-test",
		Now:           func() time.Time { return now },
	})

	adapter, ok := chat.AdapterAs[*slack.Adapter](bot, "slack")
	if !ok {
		t.Fatal("typed adapter access failed")
	}
	var _ chat.NativeContentPoster = adapter

	ref := nativeThreadRef(t, adapter)

	blocks := []any{map[string]any{"type": "section", "text": map[string]any{"type": "mrkdwn", "text": "hi"}}}
	sent, err := adapter.PostNative(context.Background(), ref, chat.NativeContent{
		Adapter: "slack",
		Payload: blocks,
	})
	if err != nil {
		t.Fatalf("post native: %v", err)
	}
	if sent == nil || sent.ID != "999.000" {
		t.Fatalf("native sent = %#v", sent)
	}

	api.mu.Lock()
	lastBlocks := api.posts[len(api.posts)-1].Blocks
	api.mu.Unlock()
	if lastBlocks == nil {
		t.Fatal("native post did not send blocks")
	}

	// Adapter mismatch is an error, never a silent downgrade.
	if _, err := adapter.PostNative(context.Background(), ref, chat.NativeContent{
		Adapter: "teams",
		Payload: blocks,
	}); err == nil {
		t.Fatal("adapter mismatch must error")
	}
}

func nativeThreadRef(t *testing.T, adapter *slack.Adapter) chat.ThreadRef {
	t.Helper()
	ref, err := adapter.ValidateThreadID(slack.EncodeChannelThreadIDForTest("T1", "C1"))
	if err != nil {
		t.Fatalf("validate thread id: %v", err)
	}
	return ref
}

func TestSlackOpenModalUsesTriggerID(t *testing.T) {
	t.Parallel()

	api := newSlackAPIServer(t)
	now := time.Unix(1_700_000_000, 0)
	bot := newSlackRuntime(t, api, slack.Options{
		SigningSecret: "secret",
		BotToken:      "xoxb-test",
		Now:           func() time.Time { return now },
	})
	adapter, ok := chat.AdapterAs[*slack.Adapter](bot, "slack")
	if !ok {
		t.Fatal("typed adapter access failed")
	}

	view := map[string]any{"type": "modal", "title": map[string]any{"type": "plain_text", "text": "Hi"}}
	if err := adapter.OpenModal(context.Background(), "trigger-abc", view); err != nil {
		t.Fatalf("open modal: %v", err)
	}
	api.mu.Lock()
	gotTrigger := api.openViewTrigID
	api.mu.Unlock()
	if gotTrigger != "trigger-abc" {
		t.Fatalf("views.open trigger_id = %q", gotTrigger)
	}

	if err := adapter.OpenModal(context.Background(), "", view); err == nil {
		t.Fatal("empty trigger_id must error")
	}
}

// TestSlackOpenModalFromRawCommand proves the modal flow is reachable from
// application code with a Command Event's Raw escape hatch alone: the preserved
// trigger_id feeds views.open without the caller ever handling it (issue #12).
func TestSlackOpenModalFromRawCommand(t *testing.T) {
	t.Parallel()

	api := newSlackAPIServer(t)
	now := time.Unix(1_700_000_000, 0)
	bot := newSlackRuntime(t, api, slack.Options{
		SigningSecret: "secret",
		BotToken:      "xoxb-test",
		Now:           func() time.Time { return now },
	})
	adapter, ok := chat.AdapterAs[*slack.Adapter](bot, "slack")
	if !ok {
		t.Fatal("typed adapter access failed")
	}

	var captured any
	bot.OnCommand(func(ctx context.Context, ev *chat.CommandEvent) error {
		captured = ev.Command.Raw
		return nil
	})
	handler, err := bot.Webhook("slack")
	if err != nil {
		t.Fatalf("webhook: %v", err)
	}

	form := url.Values{}
	form.Set("command", "/deploy")
	form.Set("team_id", "T1")
	form.Set("channel_id", "C1")
	form.Set("user_id", "U1")
	form.Set("trigger_id", "trigger-123")
	if rec := serveSignedSlackForm(t, handler, now, form); rec.Code != http.StatusOK {
		t.Fatalf("command status = %d", rec.Code)
	}
	if captured == nil {
		t.Fatal("command handler not called")
	}

	view := map[string]any{"type": "modal", "title": map[string]any{"type": "plain_text", "text": "Hi"}}
	if err := adapter.OpenModalFromRaw(context.Background(), captured, view); err != nil {
		t.Fatalf("open modal from raw: %v", err)
	}
	api.mu.Lock()
	gotTrigger := api.openViewTrigID
	api.mu.Unlock()
	if gotTrigger != "trigger-123" {
		t.Fatalf("views.open trigger_id = %q, want the trigger preserved on the command escape hatch", gotTrigger)
	}
}

// TestSlackOpenModalFromRawInteraction proves the same flow for an Interaction
// Event's Raw escape hatch (the block_actions counterpart).
func TestSlackOpenModalFromRawInteraction(t *testing.T) {
	t.Parallel()

	api := newSlackAPIServer(t)
	now := time.Unix(1_700_000_000, 0)
	bot := newSlackRuntime(t, api, slack.Options{
		SigningSecret: "secret",
		BotToken:      "xoxb-test",
		Now:           func() time.Time { return now },
	})
	adapter, ok := chat.AdapterAs[*slack.Adapter](bot, "slack")
	if !ok {
		t.Fatal("typed adapter access failed")
	}

	var captured any
	bot.OnInteraction(func(ctx context.Context, ev *chat.InteractionEvent) error {
		captured = ev.Interaction.Raw
		return nil
	})
	handler, err := bot.Webhook("slack")
	if err != nil {
		t.Fatalf("webhook: %v", err)
	}

	payload := `{
		"type":"block_actions",
		"team":{"id":"T1"},
		"user":{"id":"U1"},
		"channel":{"id":"C1"},
		"container":{"channel_id":"C1","message_ts":"333.000"},
		"trigger_id":"trigger-999",
		"actions":[{"action_id":"approve","block_id":"b1","value":"yes","type":"button"}]
	}`
	if rec := serveSignedSlackInteractivity(t, handler, now, payload); rec.Code != http.StatusOK {
		t.Fatalf("interaction status = %d", rec.Code)
	}
	if captured == nil {
		t.Fatal("interaction handler not called")
	}

	view := map[string]any{"type": "modal"}
	if err := adapter.OpenModalFromRaw(context.Background(), captured, view); err != nil {
		t.Fatalf("open modal from raw: %v", err)
	}
	api.mu.Lock()
	gotTrigger := api.openViewTrigID
	api.mu.Unlock()
	if gotTrigger != "trigger-999" {
		t.Fatalf("views.open trigger_id = %q, want the trigger preserved on the interaction escape hatch", gotTrigger)
	}
}

func TestSlackAdapterObserverEmitsAdapterCall(t *testing.T) {
	t.Parallel()

	api := newSlackAPIServer(t)
	now := time.Unix(1_700_000_000, 0)
	obs := &countingObserver{}
	adapter, err := slack.New(context.Background(), slack.Options{
		SigningSecret: "secret",
		BotToken:      "xoxb-test",
		TeamID:        "T1",
		BotUserID:     "UBOT",
		BotID:         "BBOT",
		APIBaseURL:    api.URL,
		Client:        api.Client(),
		Now:           func() time.Time { return now },
		Observer:      obs,
	})
	if err != nil {
		t.Fatalf("new adapter: %v", err)
	}
	ref := nativeThreadRef(t, adapter)
	if _, err := adapter.PostNative(context.Background(), ref, chat.NativeContent{
		Adapter: "slack",
		Payload: []any{map[string]any{"type": "divider"}},
	}); err != nil {
		t.Fatalf("post native: %v", err)
	}
	if obs.count(chat.ObsAdapterCall) == 0 {
		t.Fatal("expected ObsAdapterCall around adapter API call")
	}
}

// countingObserver counts emitted point events; spans are no-ops.
type countingObserver struct {
	mu     sync.Mutex
	counts map[chat.ObservationName]int
}

func (o *countingObserver) Event(_ context.Context, name chat.ObservationName, _ ...chat.Attr) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.counts == nil {
		o.counts = map[chat.ObservationName]int{}
	}
	o.counts[name]++
}

func (o *countingObserver) Dispatch(ctx context.Context, _ ...chat.Attr) (context.Context, chat.DispatchSpan) {
	return ctx, countingSpan{}
}

func (o *countingObserver) count(name chat.ObservationName) int {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.counts[name]
}

type countingSpan struct{}

func (countingSpan) End(chat.DispatchOutcome, ...chat.Attr) {}

// verify the immediate ack body is empty (within the 3s budget assertion is at
// the HTTP boundary: status 200, no body).
func TestSlackCommandAckIsEmpty200(t *testing.T) {
	t.Parallel()

	api := newSlackAPIServer(t)
	now := time.Unix(1_700_000_000, 0)
	bot := newSlackRuntime(t, api, slack.Options{
		SigningSecret: "secret",
		BotToken:      "xoxb-test",
		Now:           func() time.Time { return now },
	})
	bot.OnCommand(func(context.Context, *chat.CommandEvent) error { return nil })
	handler, err := bot.Webhook("slack")
	if err != nil {
		t.Fatalf("webhook: %v", err)
	}
	form := url.Values{}
	form.Set("command", "/deploy")
	form.Set("team_id", "T1")
	form.Set("channel_id", "C1")
	form.Set("user_id", "U1")
	form.Set("trigger_id", "trig")
	rec := serveSignedSlackForm(t, handler, now, form)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if strings.TrimSpace(rec.Body.String()) != "" {
		t.Fatalf("ack body = %q, want empty", rec.Body.String())
	}
}
