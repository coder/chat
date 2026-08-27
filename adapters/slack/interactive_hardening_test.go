package slack_test

import (
	"context"
	"encoding/json"
	"errors"
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

// signedCommandForm builds the canonical valid slash-command form for hardening
// cases; callers override fields before posting.
func signedCommandForm() url.Values {
	form := url.Values{}
	form.Set("command", "/deploy")
	form.Set("text", "staging now")
	form.Set("team_id", "T1")
	form.Set("channel_id", "C1")
	form.Set("channel_name", "general")
	form.Set("user_id", "U1")
	form.Set("trigger_id", "trigger-123")
	form.Set("response_url", "https://hooks.slack.com/commands/T1/123")
	return form
}

// TestSlackCommandDedupedByEventIdentity proves a re-delivered slash command
// (identical golden payload) is deduped by the adapter-derived Event Identity and
// runs the handler once, while each delivery still acks 200. PRD 0003.
func TestSlackCommandDedupedByEventIdentity(t *testing.T) {
	t.Parallel()

	api := newSlackAPIServer(t)
	now := time.Unix(1_700_000_000, 0)
	bot := newSlackRuntime(t, api, slack.Options{
		SigningSecret: "secret",
		BotToken:      "xoxb-test",
		Now:           func() time.Time { return now },
	})
	var calls int
	bot.OnCommand(func(context.Context, *chat.CommandEvent) error {
		calls++
		return nil
	})
	handler, err := bot.Webhook("slack")
	if err != nil {
		t.Fatalf("webhook: %v", err)
	}

	form := signedCommandForm()
	for i := 0; i < 3; i++ {
		rec := serveSignedSlackForm(t, handler, now, form)
		if rec.Code != http.StatusOK {
			t.Fatalf("delivery %d status = %d", i, rec.Code)
		}
	}
	if calls != 1 {
		t.Fatalf("command handler calls = %d, want 1 (deduped)", calls)
	}
}

// TestSlackInteractionDedupedByEventIdentity proves a re-delivered block_actions
// click is deduped and runs the handler once. PRD 0004 (retried click runs once).
func TestSlackInteractionDedupedByEventIdentity(t *testing.T) {
	t.Parallel()

	api := newSlackAPIServer(t)
	now := time.Unix(1_700_000_000, 0)
	bot := newSlackRuntime(t, api, slack.Options{
		SigningSecret: "secret",
		BotToken:      "xoxb-test",
		Now:           func() time.Time { return now },
	})
	var calls int
	bot.OnInteraction(func(context.Context, *chat.InteractionEvent) error {
		calls++
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
	for i := 0; i < 3; i++ {
		rec := serveSignedSlackInteractivity(t, handler, now, payload)
		if rec.Code != http.StatusOK {
			t.Fatalf("delivery %d status = %d", i, rec.Code)
		}
	}
	if calls != 1 {
		t.Fatalf("interaction handler calls = %d, want 1 (deduped)", calls)
	}
}

// TestSlackInteractionAckIsEmpty200 proves the immediate interactivity ack is an
// empty 200 (the block_actions counterpart of TestSlackCommandAckIsEmpty200).
func TestSlackInteractionAckIsEmpty200(t *testing.T) {
	t.Parallel()

	api := newSlackAPIServer(t)
	now := time.Unix(1_700_000_000, 0)
	bot := newSlackRuntime(t, api, slack.Options{
		SigningSecret: "secret",
		BotToken:      "xoxb-test",
		Now:           func() time.Time { return now },
	})
	bot.OnInteraction(func(context.Context, *chat.InteractionEvent) error { return nil })
	handler, err := bot.Webhook("slack")
	if err != nil {
		t.Fatalf("webhook: %v", err)
	}

	payload := `{
		"type":"block_actions",
		"team":{"id":"T1"},
		"user":{"id":"U1"},
		"container":{"channel_id":"C1","message_ts":"333.000"},
		"actions":[{"action_id":"approve","type":"button"}]
	}`
	rec := serveSignedSlackInteractivity(t, handler, now, payload)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if strings.TrimSpace(rec.Body.String()) != "" {
		t.Fatalf("ack body = %q, want empty", rec.Body.String())
	}
}

// TestSlackInteractionRequiredFieldValidation proves a block_actions payload
// missing a required field (team, user, channel, action_id, or message ts) is a
// 400, never a silent ignore. PRD 0004 lists required-field validation; the
// existing tests only golden-test command validation.
func TestSlackInteractionRequiredFieldValidation(t *testing.T) {
	t.Parallel()

	api := newSlackAPIServer(t)
	now := time.Unix(1_700_000_000, 0)
	bot := newSlackRuntime(t, api, slack.Options{
		SigningSecret: "secret",
		BotToken:      "xoxb-test",
		Now:           func() time.Time { return now },
	})
	bot.OnInteraction(func(context.Context, *chat.InteractionEvent) error {
		t.Fatal("invalid interaction must not dispatch")
		return nil
	})
	handler, err := bot.Webhook("slack")
	if err != nil {
		t.Fatalf("webhook: %v", err)
	}

	cases := map[string]string{
		"missing team": `{
			"type":"block_actions","user":{"id":"U1"},
			"container":{"channel_id":"C1","message_ts":"1.0"},
			"actions":[{"action_id":"a","type":"button"}]}`,
		"missing user": `{
			"type":"block_actions","team":{"id":"T1"},
			"container":{"channel_id":"C1","message_ts":"1.0"},
			"actions":[{"action_id":"a","type":"button"}]}`,
		"missing channel": `{
			"type":"block_actions","team":{"id":"T1"},"user":{"id":"U1"},
			"actions":[{"action_id":"a","type":"button"}]}`,
		"missing action id": `{
			"type":"block_actions","team":{"id":"T1"},"user":{"id":"U1"},
			"container":{"channel_id":"C1","message_ts":"1.0"},
			"actions":[{"type":"button"}]}`,
		"missing message ts in channel": `{
			"type":"block_actions","team":{"id":"T1"},"user":{"id":"U1"},
			"channel":{"id":"C1"},
			"actions":[{"action_id":"a","type":"button"}]}`,
	}
	for name, payload := range cases {
		t.Run(name, func(t *testing.T) {
			rec := serveSignedSlackInteractivity(t, handler, now, payload)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400 (body=%s)", rec.Code, rec.Body.String())
			}
		})
	}
}

// TestSlackInteractionTenantMismatchRejected proves a cross-tenant block_actions
// payload is a 400, mirroring the command tenant-mismatch case.
func TestSlackInteractionTenantMismatchRejected(t *testing.T) {
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
		t.Fatal("cross-tenant interaction reached dispatch")
		return nil
	})

	payload := `{
		"type":"block_actions","team":{"id":"T2"},"user":{"id":"U1"},
		"container":{"channel_id":"C1","message_ts":"1.0"},
		"actions":[{"action_id":"a","type":"button"}]}`
	rec := serveSignedSlackInteractivity(t, handler, now, payload)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("cross-tenant status = %d, want 400", rec.Code)
	}
}

// TestSlackBlockActionsInDirectMessage proves a block_actions click in a DM
// channel normalizes to a direct Thread (no message-ts root requirement), so DM
// interactions dispatch. The existing tests only cover channel/threaded clicks.
func TestSlackBlockActionsInDirectMessage(t *testing.T) {
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

	// DM channel id starts with D; no message_ts is required to root a direct
	// Thread.
	payload := `{
		"type":"block_actions","team":{"id":"T1"},"user":{"id":"U1"},
		"channel":{"id":"D9"},
		"actions":[{"action_id":"approve","type":"button"}]}`
	rec := serveSignedSlackInteractivity(t, handler, now, payload)
	if rec.Code != http.StatusOK {
		t.Fatalf("dm interaction status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if got == nil {
		t.Fatal("dm interaction did not dispatch")
	}
	if !got.Event.DirectMessage {
		t.Fatalf("dm interaction not marked direct: %#v", got.Event)
	}
}

// TestSlackCommandInDirectMessageRootsAtChannel proves a slash command in a DM
// builds a direct Thread, exercising the command DM branch alongside the existing
// channel-rooted command test.
func TestSlackCommandInDirectMessageRootsAtChannel(t *testing.T) {
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

	form := signedCommandForm()
	form.Set("channel_id", "D7")
	form.Set("channel_name", "directmessage")
	rec := serveSignedSlackForm(t, handler, now, form)
	if rec.Code != http.StatusOK {
		t.Fatalf("dm command status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if got == nil {
		t.Fatal("dm command did not dispatch")
	}
	if !got.Event.DirectMessage {
		t.Fatalf("dm command not marked direct: %#v", got.Event)
	}
}

// TestSlackPortablePostNeverEmitsNativePayload proves the portable Plain Text /
// Portable Markdown path stays unchanged and never carries a blocks payload, the
// invariant PRD 0004 calls for (native content only via the Optional Capability).
func TestSlackPortablePostNeverEmitsNativePayload(t *testing.T) {
	t.Parallel()

	api := newSlackAPIServer(t)
	now := time.Unix(1_700_000_000, 0)
	bot := newSlackRuntime(t, api, slack.Options{
		SigningSecret: "secret",
		BotToken:      "xoxb-test",
		Now:           func() time.Time { return now },
	})
	var threadID chat.ThreadID
	bot.OnNewMention(func(ctx context.Context, ev *chat.MessageEvent) error {
		threadID = ev.Thread.ID()
		if _, err := ev.Thread.Post(ctx, chat.Text("plain")); err != nil {
			return err
		}
		_, err := ev.Thread.Post(ctx, chat.Markdown("**md**"))
		return err
	})

	postSlackEvent(t, bot, now, `{
		"type":"event_callback","team_id":"T1","event_id":"Ev1",
		"event":{"type":"app_mention","channel":"C1","user":"U1","text":"<@UBOT> hi","ts":"111.000"}
	}`, "", "")
	if threadID == "" {
		t.Fatal("handler did not run")
	}

	api.mu.Lock()
	defer api.mu.Unlock()
	if len(api.posts) == 0 {
		t.Fatal("no posts recorded")
	}
	for i, p := range api.posts {
		if p.Blocks != nil {
			t.Fatalf("portable post %d carried a native blocks payload: %#v", i, p.Blocks)
		}
	}
}

// TestSlackRespondURLPostsToEscapeHatchURL proves the RespondURL Optional
// Capability posts a portable reply to the response_url carried on Command.Raw,
// and that a Raw with no response_url is an explicit error (not a silent no-op).
func TestSlackRespondURLPostsToEscapeHatchURL(t *testing.T) {
	t.Parallel()

	api := newSlackAPIServer(t)
	now := time.Unix(1_700_000_000, 0)

	var mu sync.Mutex
	var bodies []map[string]any
	respServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		mu.Lock()
		bodies = append(bodies, body)
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	defer respServer.Close()

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
	form := signedCommandForm()
	form.Set("response_url", respServer.URL)
	if rec := serveSignedSlackForm(t, handler, now, form); rec.Code != http.StatusOK {
		t.Fatalf("command status = %d", rec.Code)
	}
	if captured == nil {
		t.Fatal("command Raw not captured")
	}

	if err := adapter.RespondURL(context.Background(), captured, chat.Text("queued")); err != nil {
		t.Fatalf("respond url: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(bodies) != 1 {
		t.Fatalf("response_url received %d bodies, want 1", len(bodies))
	}
	if bodies[0]["text"] != "queued" {
		t.Fatalf("response_url body = %#v", bodies[0])
	}

	// A Raw escape hatch with no response_url is an explicit error.
	if err := adapter.RespondURL(context.Background(), struct{}{}, chat.Text("x")); err == nil {
		t.Fatal("RespondURL with no response_url must error")
	}
}

// TestSlackRespondURLRetriesThrottleThenTypedRateLimited proves the response_url
// escape-hatch post shares the bounded rate-limit retry seam (ADR 0005): a
// persistent 429 with Retry-After is retried within the RetryPolicy and surfaces a
// typed *slack.RateLimited on exhaustion, not a generic error, so a caller can
// defer or notify.
func TestSlackRespondURLRetriesThrottleThenTypedRateLimited(t *testing.T) {
	t.Parallel()

	api := newSlackAPIServer(t)
	now := time.Unix(1_700_000_000, 0)

	var mu sync.Mutex
	var hits int
	respServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		hits++
		mu.Unlock()
		w.Header().Set("Retry-After", "0")
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer respServer.Close()

	bot := newSlackRuntime(t, api, slack.Options{
		SigningSecret: "secret",
		BotToken:      "xoxb-test",
		Now:           func() time.Time { return now },
		RetryPolicy:   slack.RetryPolicy{MaxAttempts: 3, MaxElapsed: time.Second, BaseDelay: time.Millisecond, MaxDelay: 5 * time.Millisecond},
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
	form := signedCommandForm()
	form.Set("response_url", respServer.URL)
	if rec := serveSignedSlackForm(t, handler, now, form); rec.Code != http.StatusOK {
		t.Fatalf("command status = %d", rec.Code)
	}

	err = adapter.RespondURL(context.Background(), captured, chat.Text("queued"))
	var limited *slack.RateLimited
	if !errors.As(err, &limited) {
		t.Fatalf("err = %v, want *slack.RateLimited on a throttled response_url", err)
	}
	if limited.Attempts != 3 {
		t.Fatalf("attempts = %d, want 3 (== MaxAttempts)", limited.Attempts)
	}
	mu.Lock()
	defer mu.Unlock()
	if hits != 3 {
		t.Fatalf("response_url hits = %d, want 3 (bounded by MaxAttempts)", hits)
	}
}

// TestSlackOpenModalNilViewErrors proves modal-open validates its inputs (the
// nil-view path), complementing the existing empty-trigger_id case.
func TestSlackOpenModalNilViewErrors(t *testing.T) {
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
	if err := adapter.OpenModal(context.Background(), "trigger-1", nil); err == nil {
		t.Fatal("nil modal view must error")
	}
}

// TestSlackOpenModalFromRawValidation proves OpenModalFromRaw validates its
// inputs: a foreign raw (not this adapter's escape hatch) errors, an interaction
// escape hatch carrying no trigger_id errors, and a nil view errors — never a
// silent views.open call with an empty trigger_id.
func TestSlackOpenModalFromRawValidation(t *testing.T) {
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
	view := map[string]any{"type": "modal"}

	if err := adapter.OpenModalFromRaw(context.Background(), nil, view); err == nil {
		t.Fatal("nil raw must error")
	}
	if err := adapter.OpenModalFromRaw(context.Background(), "not an escape hatch", view); err == nil {
		t.Fatal("foreign raw must error")
	}

	// A block_actions payload without a trigger_id normalizes fine (commands
	// require one, interactions do not); opening a modal from it must error.
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
		"actions":[{"action_id":"approve","type":"button"}]
	}`
	if rec := serveSignedSlackInteractivity(t, handler, now, payload); rec.Code != http.StatusOK {
		t.Fatalf("interaction status = %d", rec.Code)
	}
	if captured == nil {
		t.Fatal("interaction handler not called")
	}
	if err := adapter.OpenModalFromRaw(context.Background(), captured, view); err == nil {
		t.Fatal("escape hatch without trigger_id must error")
	}
	if err := adapter.OpenModalFromRaw(context.Background(), captured, nil); err == nil {
		t.Fatal("nil modal view must error")
	}
}

// TestSlackPostNativeNilPayloadErrors proves PostNative rejects a nil native
// payload, complementing the adapter-mismatch case.
func TestSlackPostNativeNilPayloadErrors(t *testing.T) {
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
	ref := nativeThreadRef(t, adapter)
	if _, err := adapter.PostNative(context.Background(), ref, chat.NativeContent{Adapter: "slack", Payload: nil}); err == nil {
		t.Fatal("nil native payload must error")
	}
}

// TestSlackNativeContentCapabilityAbsentForOtherAdapter proves typed Adapter
// Access for NativeContentPoster returns false for an adapter that is not the
// Slack adapter (absent-capability contract), with no panic.
func TestSlackNativeContentCapabilityAbsentForOtherAdapter(t *testing.T) {
	t.Parallel()

	api := newSlackAPIServer(t)
	now := time.Unix(1_700_000_000, 0)
	bot := newSlackRuntime(t, api, slack.Options{
		SigningSecret: "secret",
		BotToken:      "xoxb-test",
		Now:           func() time.Time { return now },
	})
	// A name that is not registered returns false.
	if _, ok := chat.AdapterAs[chat.NativeContentPoster](bot, "teams"); ok {
		t.Fatal("missing adapter must not satisfy NativeContentPoster")
	}
	// The Slack adapter does satisfy it (capability present).
	if _, ok := chat.AdapterAs[chat.NativeContentPoster](bot, "slack"); !ok {
		t.Fatal("slack adapter must satisfy NativeContentPoster")
	}
}
