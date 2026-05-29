package slack_test

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"testing"
	"time"

	"github.com/coder/chat"
	"github.com/coder/chat/adapters/slack"
	"github.com/coder/chat/state/memory"
)

// mtHardenSlackRuntime builds a multi-tenant Slack runtime, returning the runtime and
// its adapter via Adapter Access for the edge-case harden tests below.
func mtHardenSlackRuntime(t *testing.T, api *slackAPIServer, store chat.InstallStore, now time.Time) (*chat.Chat, *slack.Adapter) {
	t.Helper()
	adapter, err := slack.New(context.Background(), slack.Options{
		SigningSecret: "secret",
		InstallStore:  store,
		APIBaseURL:    api.URL,
		Client:        api.Client(),
		Now:           func() time.Time { return now },
	})
	if err != nil {
		t.Fatalf("new multi-tenant slack adapter: %v", err)
	}
	bot, err := chat.New(context.Background(), chat.WithState(memory.New()), chat.WithAdapter(adapter))
	if err != nil {
		t.Fatalf("new chat runtime: %v", err)
	}
	return bot, adapter
}

// signSlackFormReq signs an x-www-form-urlencoded Slack body (commands and
// interactivity) so the verified form path can be exercised in multi-tenant mode.
func signSlackFormReq(req *http.Request, secret string, now time.Time, body []byte) {
	timestamp := strconv.FormatInt(now.Unix(), 10)
	base := append([]byte("v0:"+timestamp+":"), body...)
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = mac.Write(base)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("X-Slack-Request-Timestamp", timestamp)
	req.Header.Set("X-Slack-Signature", "v0="+hex.EncodeToString(mac.Sum(nil)))
}

func serveSlackForm(t *testing.T, handler http.Handler, secret string, now time.Time, form url.Values) *httptest.ResponseRecorder {
	t.Helper()
	body := []byte(form.Encode())
	req := httptest.NewRequest(http.MethodPost, "/slack", bytes.NewReader(body))
	signSlackFormReq(req, secret, now, body)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	return rec
}

// TestSlackMultiTenantInvalidSignatureNoLookup proves Slack verifies with the shared
// app-level signing secret BEFORE any install resolution: a forged signature is 401
// and the InstallStore is never consulted (no routing read on unverified Slack input,
// matching the ADR's note that Slack signs per-app so verify stays first).
func TestSlackMultiTenantInvalidSignatureNoLookup(t *testing.T) {
	t.Parallel()
	api := newSlackAPIServer(t)
	now := time.Unix(1_700_000_000, 0)
	store := newFakeInstallStore()
	store.set("T1", chat.Install{Tenant: "T1", Credential: slack.SlackInstall{BotToken: "xoxb-T1", BotUserID: "UBOT1"}})
	bot, _ := mtHardenSlackRuntime(t, api, store, now)
	bot.OnNewMention(func(context.Context, *chat.MessageEvent) error {
		t.Fatal("forged event reached dispatch")
		return nil
	})

	handler, err := bot.Webhook("slack")
	if err != nil {
		t.Fatalf("webhook: %v", err)
	}
	body := []byte(slackEventBody("T1", "EvForged", "U1", "<@UBOT1> hi"))
	req := httptest.NewRequest(http.MethodPost, "/slack", bytes.NewReader(body))
	timestamp := strconv.FormatInt(now.Unix(), 10)
	mac := hmac.New(sha256.New, []byte("WRONG-secret"))
	_, _ = mac.Write(append([]byte("v0:"+timestamp+":"), body...))
	req.Header.Set("X-Slack-Request-Timestamp", timestamp)
	req.Header.Set("X-Slack-Signature", "v0="+hex.EncodeToString(mac.Sum(nil)))

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("forged signature status = %d, want 401", rec.Code)
	}
	if store.callCount() != 0 {
		t.Fatalf("forged signature triggered %d install lookups, want 0 (verify before lookup)", store.callCount())
	}
}

// TestSlackMultiTenantUrlVerificationNoLookup proves the url_verification handshake is
// answered with the challenge before (and without) any install lookup.
func TestSlackMultiTenantUrlVerificationNoLookup(t *testing.T) {
	t.Parallel()
	api := newSlackAPIServer(t)
	now := time.Unix(1_700_000_000, 0)
	store := newFakeInstallStore()
	bot, _ := mtHardenSlackRuntime(t, api, store, now)

	handler, err := bot.Webhook("slack")
	if err != nil {
		t.Fatalf("webhook: %v", err)
	}
	rec := serveSignedSlackWebhook(t, handler, now, `{"type":"url_verification","challenge":"abc123"}`, "", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("url_verification status = %d, want 200", rec.Code)
	}
	if rec.Body.String() != "abc123" {
		t.Fatalf("url_verification body = %q, want challenge echoed", rec.Body.String())
	}
	if store.callCount() != 0 {
		t.Fatalf("url_verification triggered %d lookups, want 0", store.callCount())
	}
}

// TestSlackMultiTenantEmptyTeamIsIgnored proves an event_callback with an empty
// team_id cannot be routed to an install and is an Ignored Event (200, no dispatch),
// without a store lookup for the empty tenant.
func TestSlackMultiTenantEmptyTeamIsIgnored(t *testing.T) {
	t.Parallel()
	api := newSlackAPIServer(t)
	now := time.Unix(1_700_000_000, 0)
	store := newFakeInstallStore()
	bot, _ := mtHardenSlackRuntime(t, api, store, now)
	dispatched := 0
	bot.OnNewMention(func(context.Context, *chat.MessageEvent) error {
		dispatched++
		return nil
	})
	body := `{"type":"event_callback","team_id":"","event_id":"EvEmpty","event":{"type":"app_mention","channel":"C1","user":"U1","text":"hi","ts":"111.000"}}`
	status := postSlackEvent(t, bot, now, body, "", "")
	if status != http.StatusOK {
		t.Fatalf("empty team status = %d, want 200", status)
	}
	if dispatched != 0 {
		t.Fatalf("empty team dispatched %d times", dispatched)
	}
	if store.callCount() != 0 {
		t.Fatalf("empty team triggered %d lookups, want 0", store.callCount())
	}
}

// TestSlackMultiTenantNoInstallSurfaceOnState is a compile-time + behavioral guard
// that Runtime State is not expanded for install records: the runtime serves a
// multi-tenant webhook over plain coordination State, and the only credential source
// is the app-owned InstallStore (the post still carries the per-workspace token).
func TestSlackMultiTenantNoInstallSurfaceOnState(t *testing.T) {
	t.Parallel()
	api := newSlackAPIServer(t)
	now := time.Unix(1_700_000_000, 0)
	store := newFakeInstallStore()
	store.set("T1", chat.Install{Tenant: "T1", Credential: slack.SlackInstall{BotToken: "xoxb-T1", BotUserID: "UBOT1"}})
	// A bare memory State with no credential surface; if the runtime needed to persist
	// install records it would need a richer State, which the ADR forbids.
	adapter, err := slack.New(context.Background(), slack.Options{
		SigningSecret: "secret",
		InstallStore:  store,
		APIBaseURL:    api.URL,
		Client:        api.Client(),
		Now:           func() time.Time { return now },
	})
	if err != nil {
		t.Fatalf("adapter: %v", err)
	}
	bot, err := chat.New(context.Background(), chat.WithState(memory.New()), chat.WithAdapter(adapter))
	if err != nil {
		t.Fatalf("runtime: %v", err)
	}
	bot.OnNewMention(func(ctx context.Context, ev *chat.MessageEvent) error {
		_, err := ev.Thread.Post(ctx, chat.Text("ok"))
		return err
	})
	postSlackEvent(t, bot, now, slackEventBody("T1", "EvState", "U1", "<@UBOT1> hi"), "", "")
	if got := api.authForPost(t, 0); got != "Bearer xoxb-T1" {
		t.Fatalf("post auth = %q, want Bearer xoxb-T1 (credential from store, not State)", got)
	}
}

// TestSlackMultiTenantCommandFormPaths covers the slash-command form path under
// multi-tenant mode: an installed tenant dispatches a Command Event, a not-installed
// tenant is an Ignored Event (200, no dispatch), and a store transport error is 5xx.
func TestSlackMultiTenantCommandFormPaths(t *testing.T) {
	t.Parallel()
	api := newSlackAPIServer(t)
	now := time.Unix(1_700_000_000, 0)
	store := newFakeInstallStore()
	store.set("T1", chat.Install{Tenant: "T1", Credential: slack.SlackInstall{BotToken: "xoxb-T1", BotUserID: "UBOT1"}})
	bot, _ := mtHardenSlackRuntime(t, api, store, now)

	var commands int
	bot.OnCommand(func(context.Context, *chat.CommandEvent) error {
		commands++
		return nil
	})
	handler, err := bot.Webhook("slack")
	if err != nil {
		t.Fatalf("webhook: %v", err)
	}

	form := func(team string) url.Values {
		return url.Values{
			"command":    {"/deploy"},
			"text":       {"now"},
			"team_id":    {team},
			"channel_id": {"C1"},
			"user_id":    {"U1"},
			"trigger_id": {"TRIG1"},
		}
	}

	if rec := serveSlackForm(t, handler, "secret", now, form("T1")); rec.Code != http.StatusOK {
		t.Fatalf("installed command status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if commands != 1 {
		t.Fatalf("installed command dispatched %d times, want 1", commands)
	}
	if rec := serveSlackForm(t, handler, "secret", now, form("TX")); rec.Code != http.StatusOK {
		t.Fatalf("uninstalled command status = %d, want 200", rec.Code)
	}
	if commands != 1 {
		t.Fatalf("uninstalled command dispatched (commands=%d)", commands)
	}
	store.transport = errors.New("store offline")
	if rec := serveSlackForm(t, handler, "secret", now, form("T1")); rec.Code < 500 {
		t.Fatalf("transport command status = %d, want 5xx", rec.Code)
	}
}

// TestSlackMultiTenantInteractionFormPaths covers the block_actions interactivity path
// under multi-tenant mode: installed tenant dispatches an Interaction Event,
// not-installed is Ignored, and a store transport error is 5xx.
func TestSlackMultiTenantInteractionFormPaths(t *testing.T) {
	t.Parallel()
	api := newSlackAPIServer(t)
	now := time.Unix(1_700_000_000, 0)
	store := newFakeInstallStore()
	store.set("T1", chat.Install{Tenant: "T1", Credential: slack.SlackInstall{BotToken: "xoxb-T1", BotUserID: "UBOT1"}})
	bot, _ := mtHardenSlackRuntime(t, api, store, now)

	var interactions int
	bot.OnInteraction(func(context.Context, *chat.InteractionEvent) error {
		interactions++
		return nil
	})
	handler, err := bot.Webhook("slack")
	if err != nil {
		t.Fatalf("webhook: %v", err)
	}

	payload := func(team string) url.Values {
		return url.Values{"payload": {fmt.Sprintf(`{
			"type":"block_actions",
			"team":{"id":"%s"},
			"user":{"id":"U1"},
			"channel":{"id":"C1"},
			"container":{"channel_id":"C1","message_ts":"123.456"},
			"actions":[{"action_id":"approve","type":"button"}],
			"trigger_id":"TRIG2"
		}`, team)}}
	}

	if rec := serveSlackForm(t, handler, "secret", now, payload("T1")); rec.Code != http.StatusOK {
		t.Fatalf("installed interaction status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if interactions != 1 {
		t.Fatalf("installed interaction dispatched %d times, want 1", interactions)
	}
	if rec := serveSlackForm(t, handler, "secret", now, payload("TX")); rec.Code != http.StatusOK {
		t.Fatalf("uninstalled interaction status = %d, want 200", rec.Code)
	}
	if interactions != 1 {
		t.Fatalf("uninstalled interaction dispatched (interactions=%d)", interactions)
	}
	store.transport = errors.New("store offline")
	if rec := serveSlackForm(t, handler, "secret", now, payload("T1")); rec.Code < 500 {
		t.Fatalf("transport interaction status = %d, want 5xx", rec.Code)
	}
}

// TestSlackOpenModalRejectedInMultiTenant proves OpenModal (no tenant) is rejected in
// multi-tenant mode, OpenModalForTenant resolves the per-workspace token and posts,
// and OpenModalForTenant for an uninstalled tenant fails cleanly.
func TestSlackOpenModalRejectedInMultiTenant(t *testing.T) {
	t.Parallel()
	api := newSlackAPIServer(t)
	now := time.Unix(1_700_000_000, 0)
	store := newFakeInstallStore()
	store.set("T2", chat.Install{Tenant: "T2", Credential: slack.SlackInstall{BotToken: "xoxb-T2"}})
	_, adapter := mtHardenSlackRuntime(t, api, store, now)

	if err := adapter.OpenModal(context.Background(), "TRIG", map[string]any{"type": "modal"}); err == nil {
		t.Fatal("OpenModal must require a workspace token in multi-tenant mode")
	}
	if err := adapter.OpenModalForTenant(context.Background(), "T2", "TRIG", map[string]any{"type": "modal"}); err != nil {
		t.Fatalf("OpenModalForTenant: %v", err)
	}
	if err := adapter.OpenModalForTenant(context.Background(), "T_GONE", "TRIG", map[string]any{"type": "modal"}); err == nil {
		t.Fatal("OpenModalForTenant must fail for an uninstalled tenant")
	}
}

// TestSlackMultiTenantEmptyBotTokenIsError proves an install record whose credential
// carries no bot token is a clean error (5xx), not a silent empty-Bearer post.
func TestSlackMultiTenantEmptyBotTokenIsError(t *testing.T) {
	t.Parallel()
	api := newSlackAPIServer(t)
	now := time.Unix(1_700_000_000, 0)
	store := newFakeInstallStore()
	store.set("T1", chat.Install{Tenant: "T1", Credential: slack.SlackInstall{BotToken: ""}})
	bot, _ := mtHardenSlackRuntime(t, api, store, now)
	bot.OnNewMention(func(context.Context, *chat.MessageEvent) error { return nil })

	handler, err := bot.Webhook("slack")
	if err != nil {
		t.Fatalf("webhook: %v", err)
	}
	rec := serveSignedSlackWebhook(t, handler, now, slackEventBody("T1", "EvNoTok", "U1", "<@UBOT> hi"), "", "")
	if rec.Code < 500 {
		t.Fatalf("empty bot token status = %d, want 5xx", rec.Code)
	}
}

// TestSlackMultiTenantPointerCredential proves a *SlackInstall credential is accepted
// (the Platform Escape Hatch decodes pointer and value forms), and a typed nil pointer
// is a clean error rather than a panic.
func TestSlackMultiTenantPointerCredential(t *testing.T) {
	t.Parallel()
	api := newSlackAPIServer(t)
	now := time.Unix(1_700_000_000, 0)
	store := newFakeInstallStore()
	store.set("T1", chat.Install{Tenant: "T1", Credential: &slack.SlackInstall{BotToken: "xoxb-T1"}})
	store.set("T2", chat.Install{Tenant: "T2", Credential: (*slack.SlackInstall)(nil)})
	bot, _ := mtHardenSlackRuntime(t, api, store, now)

	bot.OnNewMention(func(ctx context.Context, ev *chat.MessageEvent) error {
		_, err := ev.Thread.Post(ctx, chat.Text("ok"))
		return err
	})
	postSlackEvent(t, bot, now, slackEventBody("T1", "EvPtr", "U1", "<@UBOT> hi"), "", "")
	if got := api.authForPost(t, 0); got != "Bearer xoxb-T1" {
		t.Fatalf("pointer credential post auth = %q, want Bearer xoxb-T1", got)
	}

	handler, err := bot.Webhook("slack")
	if err != nil {
		t.Fatalf("webhook: %v", err)
	}
	rec := serveSignedSlackWebhook(t, handler, now, slackEventBody("T2", "EvNil", "U2", "<@UBOT> hi"), "", "")
	if rec.Code < 500 {
		t.Fatalf("nil pointer credential status = %d, want 5xx", rec.Code)
	}
}

// TestSlackSingleInstallNeverTouchesStore proves the default Single-Install Adapter is
// purely additive: it serves a webhook and posts with the static token, with no
// InstallStore configured at all.
func TestSlackSingleInstallNeverTouchesStore(t *testing.T) {
	t.Parallel()
	api := newSlackAPIServer(t)
	now := time.Unix(1_700_000_000, 0)
	adapter, err := slack.New(context.Background(), slack.Options{
		SigningSecret: "secret",
		BotToken:      "xoxb-static",
		TeamID:        "T1",
		BotUserID:     "UBOT",
		BotID:         "BBOT",
		APIBaseURL:    api.URL,
		Client:        api.Client(),
		Now:           func() time.Time { return now },
	})
	if err != nil {
		t.Fatalf("single-install adapter: %v", err)
	}
	bot, err := chat.New(context.Background(), chat.WithState(memory.New()), chat.WithAdapter(adapter))
	if err != nil {
		t.Fatalf("runtime: %v", err)
	}
	bot.OnNewMention(func(ctx context.Context, ev *chat.MessageEvent) error {
		_, err := ev.Thread.Post(ctx, chat.Text("reply"))
		return err
	})
	postSlackEvent(t, bot, now, slackEventBody("T1", "EvStatic", "U1", "<@UBOT> hi"), "", "")
	if got := api.authForPost(t, 0); got != "Bearer xoxb-static" {
		t.Fatalf("single-install post auth = %q, want Bearer xoxb-static", got)
	}
}
