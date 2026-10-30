// The emulate tests run the Slack adapter end to end against emulate.dev
// (github.com/vercel-labs/emulate), a local, stateful Slack emulator. They
// skip unless this variable is set:
//
//	CHAT_EMULATE_SLACK_URL  base URL of the emulated Slack, for example http://127.0.0.1:4710
//
// Start the emulator in one terminal and run the tests in another, from the
// repository root:
//
//	mise run emulate
//	mise run test:emulate
//
// The emulate constants below must match testdata/emulate/seed.yaml. emulate
// is not the source of truth for Slack's rules: slackapitest and the TestLive
// tests are. CONTRIBUTING.md lists the gaps that emulate does not cover.

package slack_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/coder/chat"
	"github.com/coder/chat/adapters/slack"
	"github.com/coder/chat/adapters/slack/slackapi"
	"github.com/coder/chat/adapters/slack/slackapi/slackapitest"
	"github.com/coder/chat/state/memory"
)

const (
	emulateSigningSecret = "emulate-signing-secret"
	emulateBotToken      = "xoxb-emulate-bot"
	emulateUserToken     = "xoxp-emulate-alice"
	emulateUserName      = "alice"
	emulateChannelName   = "chat-emulate"
	// emulateInteractivityAddr is the host and port of the interactivity_url
	// in the seed. emulate sends block-action clicks there.
	emulateInteractivityAddr = "127.0.0.1:4799"
)

// emulateSlack is one test's connection to the emulated Slack workspace.
type emulateSlack struct {
	baseURL string
	bot     *chat.Chat
	adapter *slack.Adapter
	botAPI  *slackapi.Client // Web API client with the bot token
	userAPI *slackapi.Client // Web API client with the user token
	userID  string
	channel string
}

// newEmulateSlack skips the test when CHAT_EMULATE_SLACK_URL is unset.
// Otherwise it builds a runtime with the Slack adapter pointed at the emulator
// and looks up the seeded user and channel.
func newEmulateSlack(t *testing.T, ctx context.Context) *emulateSlack {
	t.Helper()

	baseURL := strings.TrimRight(os.Getenv("CHAT_EMULATE_SLACK_URL"), "/")
	if baseURL == "" {
		t.Skip("set CHAT_EMULATE_SLACK_URL and run `mise run emulate` to run the emulate.dev Slack tests")
	}
	apiURL := baseURL + "/api"

	adapter, err := slack.New(ctx, slack.Options{
		SigningSecret: emulateSigningSecret,
		BotToken:      emulateBotToken,
		APIBaseURL:    apiURL,
	})
	if err != nil {
		t.Fatalf("new slack adapter: %v", err)
	}
	// chat.New calls Init, which reads the bot identity from auth.test.
	bot, err := chat.New(ctx, chat.WithState(memory.New()), chat.WithAdapter(adapter))
	if err != nil {
		t.Fatalf("new chat runtime against %s: %v", baseURL, err)
	}

	botAPI := slackapi.New(slackapi.Options{Token: emulateBotToken, BaseURL: apiURL})
	userAPI := botAPI.WithToken(emulateUserToken)
	user, err := userAPI.AuthTest(ctx)
	if err != nil {
		t.Fatalf("auth.test with the user token: %v", err)
	}
	if user.UserID == "" || user.BotID != "" {
		t.Fatalf("auth.test with the user token = %+v, want the seeded human user %q", user, emulateUserName)
	}

	var list struct {
		Channels []struct {
			ID   string `json:"id"`
			Name string `json:"name"`
		} `json:"channels"`
	}
	if err := botAPI.Call(ctx, "conversations.list", nil, &list); err != nil {
		t.Fatalf("conversations.list: %v", err)
	}
	var channel string
	for _, c := range list.Channels {
		if c.Name == emulateChannelName {
			channel = c.ID
		}
	}
	if channel == "" {
		t.Fatalf("seeded channel #%s not found; is emulate running with testdata/emulate/seed.yaml?", emulateChannelName)
	}
	// emulate adds the seeded users to the seeded channels, but not the bot user
	// that the OAuth app creates.
	if err := botAPI.Call(ctx, "conversations.join", map[string]string{"channel": channel}, nil); err != nil {
		t.Fatalf("conversations.join: %v", err)
	}

	return &emulateSlack{
		baseURL: baseURL,
		bot:     bot,
		adapter: adapter,
		botAPI:  botAPI,
		userAPI: userAPI,
		userID:  user.UserID,
		channel: channel,
	}
}

// TestEmulateSlackEventReply sends a signed Events API message event to the
// adapter's HTTP handler. The mention handler replies in the thread, and
// conversations.replies on the emulator must show the reply.
//
// emulate cannot register an Events API request URL from its CLI or seed, so
// the test posts the user message to the emulator and then signs and sends the
// event_callback for it itself.
func TestEmulateSlackEventReply(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	e := newEmulateSlack(t, ctx)
	botUser := e.adapter.BotActor()

	e.bot.OnNewMention(func(ctx context.Context, ev *chat.MessageEvent) error {
		_, err := ev.Thread.Post(ctx, chat.Text("pong: "+ev.Message.Text))
		return err
	})
	webhook, err := e.bot.Webhook("slack")
	if err != nil {
		t.Fatalf("webhook: %v", err)
	}
	srv := httptest.NewServer(webhook)
	defer srv.Close()

	text := "<@" + botUser.ID + "> ping " + strconv.FormatInt(time.Now().UnixNano(), 10)
	posted, err := e.userAPI.PostMessage(ctx, slackapi.PostMessageRequest{Channel: e.channel, Text: text})
	if err != nil {
		t.Fatalf("post user message: %v", err)
	}

	body, err := json.Marshal(map[string]any{
		"type":       "event_callback",
		"team_id":    botUser.Tenant,
		"api_app_id": "A0EMULATE1",
		"event_id":   "Ev" + strings.ReplaceAll(posted.TS, ".", ""),
		"event_time": time.Now().Unix(),
		"event": map[string]any{
			"type":         "message",
			"channel":      e.channel,
			"channel_type": "channel",
			"user":         e.userID,
			"text":         text,
			"ts":           posted.TS,
			"event_ts":     posted.TS,
		},
	})
	if err != nil {
		t.Fatalf("encode event: %v", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, srv.URL, bytes.NewReader(body))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	slackapitest.SignRequest(req, body, emulateSigningSecret, time.Now())
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatalf("deliver event: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("event delivery status = %d, want 200", resp.StatusCode)
	}

	// The default dispatch mode runs the handler before the webhook returns, so
	// the reply is already stored.
	page, err := e.botAPI.ConversationReplies(ctx, slackapi.ConversationRepliesRequest{Channel: e.channel, TS: posted.TS})
	if err != nil {
		t.Fatalf("conversations.replies: %v", err)
	}
	want := "pong: " + text
	for _, m := range page.Messages {
		if m.TS != posted.TS && m.ThreadTS == posted.TS && m.Text == want && m.BotID != "" {
			return
		}
	}
	t.Fatalf("conversations.replies has no bot reply %q in thread %s: %+v", want, posted.TS, page.Messages)
}

// TestEmulateSlackBlockAction clicks a button with emulate's block-action
// simulator. emulate sends the signed interaction to the seeded
// interactivity_url, where the adapter's HTTP handler must call the
// interaction handler with the clicked action and the clicking user.
func TestEmulateSlackBlockAction(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	e := newEmulateSlack(t, ctx)

	got := make(chan *chat.Interaction, 1)
	e.bot.OnInteraction(func(_ context.Context, ev *chat.InteractionEvent) error {
		got <- ev.Interaction
		return nil
	})
	webhook, err := e.bot.Webhook("slack")
	if err != nil {
		t.Fatalf("webhook: %v", err)
	}
	ln, err := net.Listen("tcp", emulateInteractivityAddr)
	if err != nil {
		t.Fatalf("listen on the seeded interactivity address %s: %v", emulateInteractivityAddr, err)
	}
	srv := httptest.NewUnstartedServer(webhook)
	srv.Listener.Close()
	srv.Listener = ln
	srv.Start()
	defer srv.Close()

	value := "approve-" + strconv.FormatInt(time.Now().UnixNano(), 10)
	var posted slackapi.PostMessageResponse
	if err := e.botAPI.Call(ctx, "chat.postMessage", map[string]any{
		"channel": e.channel,
		"text":    "Approve?",
		"blocks": []any{map[string]any{
			"type": "actions",
			"elements": []any{map[string]any{
				"type":      "button",
				"action_id": "emulate_approve",
				"value":     value,
				"text":      map[string]any{"type": "plain_text", "text": "Approve"},
			}},
		}},
	}, &posted); err != nil {
		t.Fatalf("post button message: %v", err)
	}

	// The simulator is not a Web API method, so this client calls it from the
	// emulator root instead of /api.
	simulator := slackapi.New(slackapi.Options{Token: emulateUserToken, BaseURL: e.baseURL})
	if err := simulator.Call(ctx, "_slack/simulate/block-actions", map[string]string{
		"channel":    e.channel,
		"message_ts": posted.TS,
		"action_id":  "emulate_approve",
		"user":       emulateUserName,
	}, nil); err != nil {
		t.Fatalf("simulate block action: %v", err)
	}

	select {
	case interaction := <-got:
		if interaction.ActionID != "emulate_approve" || interaction.Value != value {
			t.Fatalf("interaction action = %q value = %q, want emulate_approve %q", interaction.ActionID, interaction.Value, value)
		}
		if interaction.Actor.ID != e.userID || interaction.Actor.BotKind != chat.BotHuman {
			t.Fatalf("interaction actor = %+v, want human %s", interaction.Actor, e.userID)
		}
	case <-ctx.Done():
		t.Fatal("interaction handler was not called")
	}
}
