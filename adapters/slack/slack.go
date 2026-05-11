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
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/coder/chat"
)

const adapterName = "slack"

type Options struct {
	SigningSecret          string
	BotToken               string
	TeamID                 string
	BotUserID              string
	BotID                  string
	APIBaseURL             string
	Client                 *http.Client
	Now                    func() time.Time
	SignatureTolerance     time.Duration
	DisableNativeEphemeral bool
}

type Adapter struct {
	signingSecret          string
	botToken               string
	teamID                 string
	botUserID              string
	botID                  string
	apiBaseURL             string
	client                 *http.Client
	now                    func() time.Time
	signatureTolerance     time.Duration
	disableNativeEphemeral bool
}

func New(ctx context.Context, opts Options) (*Adapter, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if opts.SigningSecret == "" {
		return nil, errors.New("slack: signing secret is required")
	}
	if opts.BotToken == "" {
		return nil, errors.New("slack: bot token is required")
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
	return &Adapter{
		signingSecret:          opts.SigningSecret,
		botToken:               opts.BotToken,
		teamID:                 opts.TeamID,
		botUserID:              opts.BotUserID,
		botID:                  opts.BotID,
		apiBaseURL:             apiBaseURL,
		client:                 client,
		now:                    now,
		signatureTolerance:     tolerance,
		disableNativeEphemeral: opts.DisableNativeEphemeral,
	}, nil
}

func (a *Adapter) Name() string {
	return adapterName
}

func (a *Adapter) Init(ctx context.Context) error {
	if a.teamID != "" && a.botUserID != "" {
		return nil
	}

	var resp authTestResponse
	if err := a.call(ctx, "auth.test", map[string]any{}, &resp); err != nil {
		return err
	}
	if !resp.OK {
		return fmt.Errorf("slack: auth.test failed: %s", resp.Error)
	}
	if resp.TeamID == "" {
		return errors.New("slack: auth.test did not return team_id")
	}
	if resp.UserID == "" {
		return errors.New("slack: auth.test did not return user_id")
	}
	a.teamID = firstNonEmpty(a.teamID, resp.TeamID)
	a.botUserID = firstNonEmpty(a.botUserID, resp.UserID)
	a.botID = firstNonEmpty(a.botID, resp.BotID)
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

		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, "read body", http.StatusBadRequest)
			return
		}
		if err := a.verifySignature(r, body); err != nil {
			http.Error(w, "invalid slack signature", http.StatusUnauthorized)
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
			event, ok, err := a.normalizeEvent(r, envelope, body)
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
	payload := postMessagePayload{
		Channel: thread.Channel,
		Text:    msg.Text,
		Mrkdwn:  msg.Format == chat.MessageFormatMarkdown,
	}
	if !thread.Direct {
		payload.ThreadTS = thread.Root
	}

	var resp postMessageResponse
	if err := a.call(ctx, "chat.postMessage", payload, &resp); err != nil {
		return nil, err
	}
	if !resp.OK {
		return nil, fmt.Errorf("slack: chat.postMessage failed: %s", resp.Error)
	}
	return &chat.SentMessage{ID: resp.TS, ThreadID: thread.ID, Raw: resp}, nil
}

func (a *Adapter) PostEphemeralMessage(ctx context.Context, thread chat.ThreadRef, actor chat.Actor, msg chat.PostableMessage, opts chat.EphemeralOptions) (*chat.SentMessage, error) {
	if !a.disableNativeEphemeral {
		payload := postEphemeralPayload{
			Channel: thread.Channel,
			User:    actor.ID,
			Text:    msg.Text,
			Mrkdwn:  msg.Format == chat.MessageFormatMarkdown,
		}
		if !thread.Direct {
			payload.ThreadTS = thread.Root
		}

		var resp postEphemeralResponse
		if err := a.call(ctx, "chat.postEphemeral", payload, &resp); err != nil {
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
	return a.postEphemeralFallback(ctx, thread.Tenant, actor, msg)
}

func (a *Adapter) BotActor() chat.Actor {
	return chat.Actor{
		Adapter: adapterName,
		Tenant:  a.teamID,
		ID:      a.botUserID,
		BotKind: chat.BotBot,
	}
}

func (a *Adapter) normalizeEvent(r *http.Request, envelope eventEnvelope, raw []byte) (*chat.Event, bool, error) {
	if envelope.TeamID == "" {
		return nil, false, errors.New("slack: team_id is required")
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
	author := a.actorForEvent(envelope.TeamID, ev)
	if author.ID == "" {
		return nil, false, errors.New("slack: event author is required")
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
			Mentioned: direct || ev.Type == "app_mention" || strings.Contains(ev.Text, "<@"+a.botUserID+">"),
			Raw:       ev.Raw,
		},
	}, true, nil
}

func (a *Adapter) actorForEvent(teamID string, ev slackEvent) chat.Actor {
	id := ev.User
	kind := chat.BotHuman
	if ev.User == a.botUserID || (a.botID != "" && ev.BotID == a.botID) {
		id = a.botUserID
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

func (a *Adapter) postEphemeralFallback(ctx context.Context, tenant string, actor chat.Actor, msg chat.PostableMessage) (*chat.SentMessage, error) {
	var openResp openConversationResponse
	if err := a.call(ctx, "conversations.open", openConversationPayload{Users: actor.ID}, &openResp); err != nil {
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
	if err := a.call(ctx, "chat.postMessage", postMessagePayload{
		Channel: openResp.Channel.ID,
		Text:    msg.Text,
		Mrkdwn:  msg.Format == chat.MessageFormatMarkdown,
	}, &postResp); err != nil {
		return nil, err
	}
	if !postResp.OK {
		return nil, fmt.Errorf("slack: fallback chat.postMessage failed: %s", postResp.Error)
	}
	return &chat.SentMessage{ID: postResp.TS, ThreadID: threadID, Raw: postResp}, nil
}

func (a *Adapter) call(ctx context.Context, method string, payload any, dest any) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("slack: encode %s request: %w", method, err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, a.apiBaseURL+"/"+method, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+a.botToken)
	req.Header.Set("Content-Type", "application/json")

	resp, err := a.client.Do(req)
	if err != nil {
		return fmt.Errorf("slack: %s request: %w", method, err)
	}
	defer resp.Body.Close()
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
	Channel  string `json:"channel"`
	ThreadTS string `json:"thread_ts,omitempty"`
	Text     string `json:"text"`
	Mrkdwn   bool   `json:"mrkdwn"`
}

type postMessageResponse struct {
	OK      bool   `json:"ok"`
	Error   string `json:"error"`
	Channel string `json:"channel"`
	TS      string `json:"ts"`
}

type postEphemeralPayload struct {
	Channel  string `json:"channel"`
	ThreadTS string `json:"thread_ts,omitempty"`
	User     string `json:"user"`
	Text     string `json:"text"`
	Mrkdwn   bool   `json:"mrkdwn"`
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

func absDuration(duration time.Duration) time.Duration {
	if duration < 0 {
		return -duration
	}
	return duration
}
