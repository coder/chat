package slack

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/coder/chat"
)

const (
	adapterName         = "slack"
	maxWebhookBodyBytes = 1 << 20
)

type Options struct {
	SigningSecret string
	BotToken      string
	TeamID        string
	BotUserID     string
	BotID         string
	// InstallStore selects Multi-Tenant Adapter mode: per-workspace bot tokens are
	// resolved per webhook (and per Thread Handle reconstruction) from the store
	// instead of a static BotToken. Supplying both BotToken and InstallStore, or
	// neither, is a Runtime Construction error. SigningSecret stays required in both
	// modes: Slack signs with one app-level signing secret, never per-workspace.
	InstallStore           chat.InstallStore
	APIBaseURL             string
	Client                 *http.Client
	Now                    func() time.Time
	SignatureTolerance     time.Duration
	DisableNativeEphemeral bool
	Logger                 *slog.Logger
	// Observer receives adapter-facing observations (ObsAdapterCall, ObsRateLimit);
	// it is adapter-owned wiring, not on the core Adapter interface. Nil is a no-op.
	Observer chat.Observer
}

// SlackInstall is the adapter-specific Install.Credential payload for Slack, a
// Platform Escape Hatch for credentials. Slack verifies webhooks with one
// app-level signing secret (kept in Options.SigningSecret), so only the
// per-workspace bot token rides here; it is used for replies, never verification.
// BotUserID is optional; when set it makes self-filtering tenant-correct for the
// workspace, and takes second place behind Install.BotActorID.
type SlackInstall struct {
	BotToken  string
	BotUserID string
}

type Adapter struct {
	signingSecret          string
	botToken               string
	teamID                 string
	botUserID              string
	botID                  string
	installStore           chat.InstallStore
	apiBaseURL             string
	client                 *http.Client
	now                    func() time.Time
	signatureTolerance     time.Duration
	disableNativeEphemeral bool
	logger                 *slog.Logger
	observer               chat.Observer
}

// slackInstall is the resolved per-event credential the webhook and posting paths
// share: the bot token used for replies and the bot user id used for tenant-correct
// self-filtering.
type slackInstall struct {
	token     string
	botUserID string
}

func New(ctx context.Context, opts Options) (*Adapter, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if opts.SigningSecret == "" {
		return nil, errors.New("slack: signing secret is required")
	}
	// Mode selection by capability, validated fail-fast: exactly one of a static bot
	// token or an install store. Slack's shared signing secret stays in Options for
	// both modes.
	if opts.BotToken != "" && opts.InstallStore != nil {
		return nil, errors.New("slack: set either bot token or install store, not both")
	}
	if opts.BotToken == "" && opts.InstallStore == nil {
		return nil, errors.New("slack: bot token or install store is required")
	}
	client := opts.Client
	if client == nil {
		client = http.DefaultClient
	}
	apiBaseURL := strings.TrimRight(opts.APIBaseURL, "/")
	if apiBaseURL == "" {
		apiBaseURL = "https://slack.com/api"
	}
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	tolerance := opts.SignatureTolerance
	if tolerance == 0 {
		tolerance = 5 * time.Minute
	}
	if tolerance < 0 {
		return nil, errors.New("slack: signature tolerance must be non-negative")
	}
	logger := opts.Logger
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	observer := opts.Observer
	if observer == nil {
		observer = noopObserver{}
	}
	return &Adapter{
		signingSecret:          opts.SigningSecret,
		botToken:               opts.BotToken,
		teamID:                 opts.TeamID,
		botUserID:              opts.BotUserID,
		botID:                  opts.BotID,
		installStore:           opts.InstallStore,
		apiBaseURL:             apiBaseURL,
		client:                 client,
		now:                    now,
		signatureTolerance:     tolerance,
		disableNativeEphemeral: opts.DisableNativeEphemeral,
		logger:                 logger,
		observer:               observer,
	}, nil
}

// multiTenant reports whether this adapter resolves per-workspace credentials
// through an InstallStore instead of a static bot token.
func (a *Adapter) multiTenant() bool {
	return a.installStore != nil
}

func (a *Adapter) Name() string {
	return adapterName
}

func (a *Adapter) Init(ctx context.Context) error {
	// Multi-tenant identity is per-install, resolved per webhook / from each install
	// record, so there is no single auth.test identity to discover at construction.
	if a.multiTenant() {
		return nil
	}
	if a.teamID != "" && a.botUserID != "" && a.botID != "" {
		return nil
	}

	var resp authTestResponse
	if err := a.call(ctx, "auth.test", map[string]any{}, &resp); err != nil {
		return err
	}
	if !resp.OK {
		return fmt.Errorf("slack: auth.test failed: %s", resp.Error)
	}
	if err := requireMatchingAuthField("team_id", a.teamID, resp.TeamID); err != nil {
		return err
	}
	if err := requireMatchingAuthField("user_id", a.botUserID, resp.UserID); err != nil {
		return err
	}
	if err := requireMatchingAuthField("bot_id", a.botID, resp.BotID); err != nil {
		return err
	}

	teamID := firstNonEmpty(a.teamID, resp.TeamID)
	botUserID := firstNonEmpty(a.botUserID, resp.UserID)
	botID := firstNonEmpty(a.botID, resp.BotID)
	if teamID == "" {
		return errors.New("slack: auth.test did not return team_id")
	}
	if botUserID == "" {
		return errors.New("slack: auth.test did not return user_id")
	}
	if botID == "" {
		return errors.New("slack: auth.test did not return bot_id")
	}

	a.teamID = teamID
	a.botUserID = botUserID
	a.botID = botID
	return nil
}

func (a *Adapter) Shutdown(context.Context) error {
	return nil
}

func (a *Adapter) Webhook(dispatch chat.DispatchFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}

		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxWebhookBodyBytes))
		if err != nil {
			var maxBytesErr *http.MaxBytesError
			if errors.As(err, &maxBytesErr) {
				http.Error(w, "slack payload too large", http.StatusRequestEntityTooLarge)
				return
			}
			http.Error(w, "read body", http.StatusBadRequest)
			return
		}
		// Slack signs with the shared app-level signing secret in both modes, so
		// verification stays before tenant parse; the per-workspace bot token is used
		// only for replies.
		if err := a.verifySignature(r, body); err != nil {
			http.Error(w, "invalid slack signature", http.StatusUnauthorized)
			return
		}

		// Commands and interactivity arrive as x-www-form-urlencoded; event callbacks
		// arrive as JSON. Branch only after signature verification.
		if isFormContentType(r.Header.Get("Content-Type")) {
			a.handleFormWebhook(w, r, dispatch, body)
			return
		}

		var envelope eventEnvelope
		if err := json.Unmarshal(body, &envelope); err != nil {
			http.Error(w, "invalid slack payload", http.StatusBadRequest)
			return
		}
		switch envelope.Type {
		case "url_verification":
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(envelope.Challenge))
		case "event_callback":
			install, found, err := a.resolveInstall(r.Context(), envelope.TeamID)
			if err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			if !found {
				// ErrInstallNotFound (or no tenant): Ignored Event. Acknowledge without
				// dispatch.
				w.WriteHeader(http.StatusOK)
				return
			}
			event, ok, err := a.normalizeEvent(r, envelope, body, install.botUserID)
			if err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			if ok {
				if err := dispatch(r.Context(), event); err != nil {
					http.Error(w, err.Error(), http.StatusInternalServerError)
					return
				}
			}
			w.WriteHeader(http.StatusOK)
		default:
			w.WriteHeader(http.StatusOK)
		}
	})
}

func isFormContentType(contentType string) bool {
	mediaType, _, err := mime.ParseMediaType(contentType)
	if err != nil {
		return false
	}
	return mediaType == "application/x-www-form-urlencoded"
}

// handleFormWebhook decodes a slash command or interactivity payload from the
// verified form body and dispatches it through the same DispatchFunc seam as
// messages. The empty 200 is the Ack-Then-Work ack within Slack's 3-second budget.
func (a *Adapter) handleFormWebhook(w http.ResponseWriter, r *http.Request, dispatch chat.DispatchFunc, body []byte) {
	form, err := url.ParseQuery(string(body))
	if err != nil {
		http.Error(w, "invalid slack form payload", http.StatusBadRequest)
		return
	}

	switch {
	case form.Get("payload") != "":
		a.handleInteractionForm(w, r, dispatch, form.Get("payload"))
	case form.Get("command") != "":
		a.handleCommandForm(w, r, dispatch, form)
	default:
		http.Error(w, "unsupported slack form payload", http.StatusBadRequest)
	}
}

func (a *Adapter) ValidateThreadID(id chat.ThreadID) (chat.ThreadRef, error) {
	payload, err := decodeThreadID(id)
	if err != nil {
		return chat.ThreadRef{}, err
	}
	return chat.ThreadRef{
		ID:      id,
		Adapter: adapterName,
		Tenant:  payload.Team,
		Channel: payload.Channel,
		Root:    payload.Root,
		Direct:  payload.Direct,
		Raw:     payload,
	}, nil
}

func (a *Adapter) PostMessage(ctx context.Context, thread chat.ThreadRef, msg chat.PostableMessage) (*chat.SentMessage, error) {
	messageFields, err := slackMessageFields(msg)
	if err != nil {
		return nil, err
	}
	token, err := a.postToken(ctx, thread.Tenant)
	if err != nil {
		return nil, err
	}
	payload := postMessagePayload{
		Channel:      thread.Channel,
		Text:         messageFields.Text,
		MarkdownText: messageFields.MarkdownText,
		Mrkdwn:       messageFields.Mrkdwn,
	}
	if !thread.Direct {
		payload.ThreadTS = thread.Root
	}

	var resp postMessageResponse
	if err := a.callWithToken(ctx, token, "chat.postMessage", payload, &resp); err != nil {
		return nil, err
	}
	if !resp.OK {
		return nil, fmt.Errorf("slack: chat.postMessage failed: %s", resp.Error)
	}
	return &chat.SentMessage{ID: resp.TS, ThreadID: thread.ID, Raw: resp}, nil
}

func (a *Adapter) PostEphemeralMessage(ctx context.Context, thread chat.ThreadRef, actor chat.Actor, msg chat.PostableMessage, opts chat.EphemeralOptions) (*chat.SentMessage, error) {
	messageFields, err := slackMessageFields(msg)
	if err != nil {
		return nil, err
	}
	token, err := a.postToken(ctx, thread.Tenant)
	if err != nil {
		return nil, err
	}
	if !a.disableNativeEphemeral {
		payload := postEphemeralPayload{
			Channel:      thread.Channel,
			User:         actor.ID,
			Text:         messageFields.Text,
			MarkdownText: messageFields.MarkdownText,
			Mrkdwn:       messageFields.Mrkdwn,
		}
		if !thread.Direct {
			payload.ThreadTS = thread.Root
		}

		var resp postEphemeralResponse
		if err := a.callWithToken(ctx, token, "chat.postEphemeral", payload, &resp); err != nil {
			return nil, err
		}
		if resp.OK {
			return &chat.SentMessage{ID: resp.MessageTS, ThreadID: thread.ID, Raw: resp}, nil
		}
		if !opts.FallbackToDM {
			return nil, fmt.Errorf("slack: chat.postEphemeral failed: %s", resp.Error)
		}
	}

	if !opts.FallbackToDM {
		return nil, nil
	}
	return a.postEphemeralFallback(ctx, token, thread.Tenant, actor, msg)
}

func (a *Adapter) BotActor() chat.Actor {
	return chat.Actor{
		Adapter: adapterName,
		Tenant:  a.teamID,
		ID:      a.botUserID,
		BotKind: chat.BotBot,
	}
}

// resolveInstall returns the per-tenant credential for an inbound webhook. In
// single-install mode it returns the static token and discovered bot identity. In
// multi-tenant mode it calls the InstallStore: ErrInstallNotFound (or an empty
// tenant) signals an Ignored Event (found=false, nil error, caller acks without
// dispatch); any other store error is a transport failure (found=false, non-nil
// error, caller returns 5xx). The same resolver serves Thread Handle posting via
// postToken, keyed by the Platform Tenant decoded from the Thread ID.
func (a *Adapter) resolveInstall(ctx context.Context, tenant string) (slackInstall, bool, error) {
	if !a.multiTenant() {
		return slackInstall{token: a.botToken, botUserID: a.botUserID}, true, nil
	}
	if tenant == "" {
		return slackInstall{}, false, nil
	}
	install, err := a.installStore.Lookup(ctx, adapterName, tenant)
	if err != nil {
		if errors.Is(err, chat.ErrInstallNotFound) {
			return slackInstall{}, false, nil
		}
		return slackInstall{}, false, fmt.Errorf("slack: install lookup: %w", err)
	}
	cred, err := slackCredential(install.Credential)
	if err != nil {
		return slackInstall{}, false, err
	}
	if cred.BotToken == "" {
		return slackInstall{}, false, errors.New("slack: install credential has no bot token")
	}
	return slackInstall{
		token:     cred.BotToken,
		botUserID: firstNonEmpty(install.BotActorID, cred.BotUserID),
	}, true, nil
}

// postToken resolves the per-tenant bot token for an out-of-webhook post (Thread
// Handle reconstruction). ErrInstallNotFound surfaces as a clean error here rather
// than an Ignored Event: there is no platform request to acknowledge.
func (a *Adapter) postToken(ctx context.Context, tenant string) (string, error) {
	if !a.multiTenant() {
		return a.botToken, nil
	}
	if tenant == "" {
		return "", errors.New("slack: thread tenant is required")
	}
	install, err := a.installStore.Lookup(ctx, adapterName, tenant)
	if err != nil {
		return "", fmt.Errorf("slack: install lookup: %w", err)
	}
	cred, err := slackCredential(install.Credential)
	if err != nil {
		return "", err
	}
	if cred.BotToken == "" {
		return "", errors.New("slack: install credential has no bot token")
	}
	return cred.BotToken, nil
}

func slackCredential(credential any) (SlackInstall, error) {
	switch cred := credential.(type) {
	case SlackInstall:
		return cred, nil
	case *SlackInstall:
		if cred == nil {
			return SlackInstall{}, errors.New("slack: install credential is nil")
		}
		return *cred, nil
	default:
		return SlackInstall{}, fmt.Errorf("slack: install credential is not slack.SlackInstall, got %T", credential)
	}
}

func (a *Adapter) normalizeEvent(r *http.Request, envelope eventEnvelope, raw []byte, botUserID string) (*chat.Event, bool, error) {
	if envelope.TeamID == "" {
		return nil, false, errors.New("slack: team_id is required")
	}
	// The cross-tenant guard only applies to a single-install adapter pinned to one
	// team; in multi-tenant mode every workspace the store serves is valid.
	if !a.multiTenant() && a.teamID != "" && envelope.TeamID != a.teamID {
		return nil, false, fmt.Errorf("slack: team_id %q does not match configured team", envelope.TeamID)
	}
	if envelope.EventID == "" {
		return nil, false, errors.New("slack: event_id is required")
	}

	var ev slackEvent
	if err := json.Unmarshal(envelope.Event, &ev); err != nil {
		return nil, false, fmt.Errorf("slack: invalid event: %w", err)
	}
	if !supportedMessageEvent(ev) {
		return nil, false, nil
	}
	if ev.Channel == "" {
		return nil, false, errors.New("slack: event channel is required")
	}
	if ev.TS == "" {
		return nil, false, errors.New("slack: event ts is required")
	}

	threadID, direct, err := a.threadIDForEvent(envelope.TeamID, ev)
	if err != nil {
		return nil, false, err
	}
	author := a.actorForEvent(envelope.TeamID, ev, botUserID)
	if author.ID == "" {
		return nil, false, errors.New("slack: event author is required")
	}
	// Multi-tenant self-filtering is tenant-correct from the per-install bot id: the
	// runtime's BotActor() filter cannot match because there is no single bot
	// identity, so drop the bot's own messages here as an Ignored Event.
	if a.multiTenant() && author.BotKind == chat.BotBot && botUserID != "" && author.ID == botUserID {
		return nil, false, nil
	}

	return &chat.Event{
		ID:            envelope.EventID,
		Adapter:       adapterName,
		Tenant:        envelope.TeamID,
		ThreadID:      threadID,
		DirectMessage: direct,
		Retry: chat.RetryMetadata{
			Num:    r.Header.Get("X-Slack-Retry-Num"),
			Reason: r.Header.Get("X-Slack-Retry-Reason"),
		},
		Raw: json.RawMessage(raw),
		Message: &chat.Message{
			ID:        ev.TS,
			Text:      ev.Text,
			Author:    author,
			Mentioned: direct || ev.Type == "app_mention" || strings.Contains(ev.Text, "<@"+botUserID+">"),
			Raw:       ev.Raw,
		},
	}, true, nil
}

func (a *Adapter) actorForEvent(teamID string, ev slackEvent, botUserID string) chat.Actor {
	id := ev.User
	kind := chat.BotHuman
	if (botUserID != "" && ev.User == botUserID) || (a.botID != "" && ev.BotID == a.botID) {
		id = botUserID
		kind = chat.BotBot
	} else if ev.BotID != "" || ev.Subtype == "bot_message" {
		id = firstNonEmpty(id, ev.BotID)
		kind = chat.BotBot
	}
	return chat.Actor{
		Adapter: adapterName,
		Tenant:  teamID,
		ID:      id,
		BotKind: kind,
	}
}

func (a *Adapter) threadIDForEvent(teamID string, ev slackEvent) (chat.ThreadID, bool, error) {
	direct := ev.ChannelType == "im" || strings.HasPrefix(ev.Channel, "D")
	root := ""
	if !direct {
		root = firstNonEmpty(ev.ThreadTS, ev.TS)
	}
	id, err := encodeThreadID(threadPayload{
		Team:    teamID,
		Channel: ev.Channel,
		Root:    root,
		Direct:  direct,
	})
	return id, direct, err
}

func supportedMessageEvent(ev slackEvent) bool {
	if ev.Type == "app_mention" {
		return true
	}
	if ev.Type != "message" {
		return false
	}
	return ev.Subtype == "" || ev.Subtype == "bot_message"
}

func (a *Adapter) verifySignature(r *http.Request, body []byte) error {
	timestampHeader := r.Header.Get("X-Slack-Request-Timestamp")
	if timestampHeader == "" {
		return errors.New("slack: missing signature timestamp")
	}
	timestamp, err := strconv.ParseInt(timestampHeader, 10, 64)
	if err != nil {
		return errors.New("slack: invalid signature timestamp")
	}
	signedAt := time.Unix(timestamp, 0)
	if a.signatureTolerance > 0 && absDuration(a.now().Sub(signedAt)) > a.signatureTolerance {
		return errors.New("slack: signature timestamp outside tolerance")
	}

	base := []byte("v0:" + timestampHeader + ":")
	base = append(base, body...)
	mac := hmac.New(sha256.New, []byte(a.signingSecret))
	_, _ = mac.Write(base)
	expected := "v0=" + hex.EncodeToString(mac.Sum(nil))
	got := r.Header.Get("X-Slack-Signature")
	if !hmac.Equal([]byte(expected), []byte(got)) {
		return errors.New("slack: signature mismatch")
	}
	return nil
}

func (a *Adapter) postEphemeralFallback(ctx context.Context, token string, tenant string, actor chat.Actor, msg chat.PostableMessage) (*chat.SentMessage, error) {
	messageFields, err := slackMessageFields(msg)
	if err != nil {
		return nil, err
	}
	var openResp openConversationResponse
	if err := a.callWithToken(ctx, token, "conversations.open", openConversationPayload{Users: actor.ID}, &openResp); err != nil {
		return nil, err
	}
	if !openResp.OK {
		return nil, fmt.Errorf("slack: conversations.open failed: %s", openResp.Error)
	}
	if openResp.Channel.ID == "" {
		return nil, errors.New("slack: conversations.open did not return channel id")
	}

	threadID, err := encodeThreadID(threadPayload{Team: tenant, Channel: openResp.Channel.ID, Direct: true})
	if err != nil {
		return nil, err
	}
	var postResp postMessageResponse
	if err := a.callWithToken(ctx, token, "chat.postMessage", postMessagePayload{
		Channel:      openResp.Channel.ID,
		Text:         messageFields.Text,
		MarkdownText: messageFields.MarkdownText,
		Mrkdwn:       messageFields.Mrkdwn,
	}, &postResp); err != nil {
		return nil, err
	}
	if !postResp.OK {
		return nil, fmt.Errorf("slack: fallback chat.postMessage failed: %s", postResp.Error)
	}
	return &chat.SentMessage{ID: postResp.TS, ThreadID: threadID, Raw: postResp}, nil
}

type slackMessage struct {
	Text         string
	MarkdownText string
	Mrkdwn       *bool
}

func slackMessageFields(msg chat.PostableMessage) (slackMessage, error) {
	switch msg.Format {
	case chat.MessageFormatText:
		mrkdwn := false
		return slackMessage{Text: msg.Text, Mrkdwn: &mrkdwn}, nil
	case chat.MessageFormatMarkdown:
		return slackMessage{MarkdownText: msg.Text}, nil
	default:
		return slackMessage{}, fmt.Errorf("slack: unsupported message format %d", msg.Format)
	}
}

// call posts to the Slack Web API with the single-install bot token. Multi-tenant
// callers resolve a per-workspace token and use callWithToken instead.
func (a *Adapter) call(ctx context.Context, method string, payload any, dest any) error {
	return a.callWithToken(ctx, a.botToken, method, payload, dest)
}

// callWithToken posts to the Slack Web API authorizing with the given per-workspace
// bot token. It is the single outbound seam both modes share.
func (a *Adapter) callWithToken(ctx context.Context, token string, method string, payload any, dest any) error {
	a.observer.Event(ctx, chat.ObsAdapterCall, chat.AdapterAttr(adapterName))

	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("slack: encode %s request: %w", method, err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, a.apiBaseURL+"/"+method, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")

	resp, err := a.client.Do(req)
	if err != nil {
		return fmt.Errorf("slack: %s request: %w", method, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusTooManyRequests {
		a.observer.Event(ctx, chat.ObsRateLimit, chat.AdapterAttr(adapterName))
		return fmt.Errorf("slack: %s status %d", method, resp.StatusCode)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return fmt.Errorf("slack: %s status %d", method, resp.StatusCode)
	}
	if err := json.NewDecoder(resp.Body).Decode(dest); err != nil {
		return fmt.Errorf("slack: decode %s response: %w", method, err)
	}
	return nil
}

type eventEnvelope struct {
	Type      string          `json:"type"`
	Challenge string          `json:"challenge"`
	TeamID    string          `json:"team_id"`
	EventID   string          `json:"event_id"`
	Event     json.RawMessage `json:"event"`
}

type slackEvent struct {
	Type        string          `json:"type"`
	Subtype     string          `json:"subtype"`
	Channel     string          `json:"channel"`
	ChannelType string          `json:"channel_type"`
	User        string          `json:"user"`
	BotID       string          `json:"bot_id"`
	Text        string          `json:"text"`
	TS          string          `json:"ts"`
	ThreadTS    string          `json:"thread_ts"`
	Raw         json.RawMessage `json:"-"`
}

func (e *slackEvent) UnmarshalJSON(data []byte) error {
	type alias slackEvent
	var decoded alias
	if err := json.Unmarshal(data, &decoded); err != nil {
		return err
	}
	*e = slackEvent(decoded)
	e.Raw = append(json.RawMessage(nil), data...)
	return nil
}

type threadPayload struct {
	Team    string `json:"team"`
	Channel string `json:"channel"`
	Root    string `json:"root,omitempty"`
	Direct  bool   `json:"direct,omitempty"`
}

func encodeThreadID(payload threadPayload) (chat.ThreadID, error) {
	if payload.Team == "" {
		return "", errors.New("slack: thread team is required")
	}
	if payload.Channel == "" {
		return "", errors.New("slack: thread channel is required")
	}
	if !payload.Direct && payload.Root == "" {
		return "", errors.New("slack: thread root is required")
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return "", err
	}
	return chat.ThreadID("slack:v1:" + base64.RawURLEncoding.EncodeToString(body)), nil
}

func decodeThreadID(id chat.ThreadID) (threadPayload, error) {
	const prefix = "slack:v1:"
	if !strings.HasPrefix(string(id), prefix) {
		return threadPayload{}, fmt.Errorf("slack: malformed thread id %q", id)
	}
	body, err := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(string(id), prefix))
	if err != nil {
		return threadPayload{}, fmt.Errorf("slack: decode thread id: %w", err)
	}
	var payload threadPayload
	if err := json.Unmarshal(body, &payload); err != nil {
		return threadPayload{}, fmt.Errorf("slack: parse thread id: %w", err)
	}
	if payload.Team == "" || payload.Channel == "" || (!payload.Direct && payload.Root == "") {
		return threadPayload{}, fmt.Errorf("slack: invalid thread id %q", id)
	}
	return payload, nil
}

type authTestResponse struct {
	OK     bool   `json:"ok"`
	Error  string `json:"error"`
	TeamID string `json:"team_id"`
	UserID string `json:"user_id"`
	BotID  string `json:"bot_id"`
}

type postMessagePayload struct {
	Channel      string `json:"channel"`
	ThreadTS     string `json:"thread_ts,omitempty"`
	Text         string `json:"text,omitempty"`
	MarkdownText string `json:"markdown_text,omitempty"`
	Mrkdwn       *bool  `json:"mrkdwn,omitempty"`
	Blocks       any    `json:"blocks,omitempty"`
}

type postMessageResponse struct {
	OK      bool   `json:"ok"`
	Error   string `json:"error"`
	Channel string `json:"channel"`
	TS      string `json:"ts"`
}

type postEphemeralPayload struct {
	Channel      string `json:"channel"`
	ThreadTS     string `json:"thread_ts,omitempty"`
	User         string `json:"user"`
	Text         string `json:"text,omitempty"`
	MarkdownText string `json:"markdown_text,omitempty"`
	Mrkdwn       *bool  `json:"mrkdwn,omitempty"`
}

type postEphemeralResponse struct {
	OK        bool   `json:"ok"`
	Error     string `json:"error"`
	MessageTS string `json:"message_ts"`
}

type openConversationPayload struct {
	Users string `json:"users"`
}

type openConversationResponse struct {
	OK      bool   `json:"ok"`
	Error   string `json:"error"`
	Channel struct {
		ID string `json:"id"`
	} `json:"channel"`
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}

func requireMatchingAuthField(name string, configured string, discovered string) error {
	if configured != "" && discovered != "" && configured != discovered {
		return fmt.Errorf("slack: auth.test returned %s %q, expected %q", name, discovered, configured)
	}
	return nil
}

func absDuration(duration time.Duration) time.Duration {
	if duration < 0 {
		return -duration
	}
	return duration
}
