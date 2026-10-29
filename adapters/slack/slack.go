package slack

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/coder/chat"
	"github.com/coder/chat/adapters/slack/slackapi"
)

const adapterName = "slack"

type Options struct {
	// SigningSecret is the signing secret of the Slack app. It enables
	// Webhook, which checks the signature of each request with it. At least
	// one of SigningSecret and AppToken is required, and both are allowed.
	SigningSecret string
	// AppToken is the app-level token (xapp-) of the Slack app, with the
	// connections:write scope. It enables Listen, which receives deliveries
	// over Socket Mode.
	AppToken  string
	BotToken  string
	TeamID    string
	BotUserID string
	BotID     string
	// InstallStore selects Multi-Tenant Adapter mode: per-workspace bot tokens are
	// resolved per delivery (and per Thread Handle reconstruction) from the store
	// instead of a static BotToken. Supplying both BotToken and InstallStore, or
	// neither, is a Runtime Construction error. SigningSecret and AppToken work
	// the same in both modes: Slack has one of each for each app, never one for
	// each workspace.
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
	// RetryPolicy bounds outbound Slack rate-limit retry/backoff (ADR 0005). The
	// zero value applies a conservative default that caps the total backoff sleep
	// at 2 seconds but does not bound the HTTP attempts; MaxAttempts: 1 disables
	// retry. It is per-adapter platform config, never Runtime Options.
	RetryPolicy RetryPolicy
}

// SlackInstall is the adapter-specific Install.Credential payload for Slack, a
// Platform Escape Hatch for credentials. Slack has one signing secret and one
// app-level token for each app (kept in Options.SigningSecret and
// Options.AppToken), so only the per-workspace bot token rides here; it is used
// for replies, never to receive deliveries.
// BotUserID is optional; when set it makes self-filtering tenant-correct for the
// workspace, and takes second place behind Install.BotActorID.
type SlackInstall struct {
	BotToken  string
	BotUserID string
}

type Adapter struct {
	signingSecret          string
	appToken               string
	botToken               string
	teamID                 string
	botUserID              string
	botID                  string
	installStore           chat.InstallStore
	api                    *slackapi.Client
	now                    func() time.Time
	signatureTolerance     time.Duration
	disableNativeEphemeral bool
}

// slackInstall is the resolved per-event credential the delivery and posting paths
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
	if opts.SigningSecret == "" && opts.AppToken == "" {
		return nil, errors.New("slack: signing secret or app token is required")
	}
	// Mode selection by capability, validated fail-fast: exactly one of a static bot
	// token or an install store. Slack's app-level signing secret and app token stay
	// in Options for both modes.
	if opts.BotToken != "" && opts.InstallStore != nil {
		return nil, errors.New("slack: set either bot token or install store, not both")
	}
	if opts.BotToken == "" && opts.InstallStore == nil {
		return nil, errors.New("slack: bot token or install store is required")
	}
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	tolerance := opts.SignatureTolerance
	if tolerance == 0 {
		tolerance = slackapi.DefaultSignatureTolerance
	}
	if tolerance < 0 {
		return nil, errors.New("slack: signature tolerance must be non-negative")
	}
	api := slackapi.New(slackapi.Options{
		Token:       opts.BotToken,
		BaseURL:     opts.APIBaseURL,
		HTTPClient:  opts.Client,
		RetryPolicy: opts.RetryPolicy,
		Observer:    opts.Observer,
		Logger:      opts.Logger,
	})
	return &Adapter{
		signingSecret:          opts.SigningSecret,
		appToken:               opts.AppToken,
		botToken:               opts.BotToken,
		teamID:                 opts.TeamID,
		botUserID:              opts.BotUserID,
		botID:                  opts.BotID,
		installStore:           opts.InstallStore,
		api:                    api,
		now:                    now,
		signatureTolerance:     tolerance,
		disableNativeEphemeral: opts.DisableNativeEphemeral,
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
	// Multi-tenant identity is per-install, resolved per delivery / from each install
	// record, so there is no single auth.test identity to discover at construction.
	if a.multiTenant() {
		return nil
	}
	if a.teamID != "" && a.botUserID != "" && a.botID != "" {
		return nil
	}

	resp, err := a.api.AuthTest(ctx)
	if err != nil {
		return err
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

// Webhook returns the HTTP transport for the request URLs of the Slack app: the
// Events API, slash command, and interactivity URLs. It uses the same delivery
// handler as Listen. With no Options.SigningSecret, it returns a handler that
// answers 404 to each request.
func (a *Adapter) Webhook(dispatch chat.DispatchFunc) http.Handler {
	if a.signingSecret == "" {
		return http.NotFoundHandler()
	}
	return &slackapi.Webhook{
		SigningSecret:      a.signingSecret,
		Handler:            a.deliveryHandler(dispatch),
		Now:                a.now,
		SignatureTolerance: a.signatureTolerance,
	}
}

var _ chat.Listener = (*Adapter)(nil)

// Listen is the Socket Mode transport. It implements the Listener Optional
// Capability, which Chat.Listen runs. It connects to Slack with
// Options.AppToken, and gives each delivery to the same delivery handler as
// Webhook. It blocks until ctx is done. Then it closes its connections and
// returns nil.
//
// Listen returns an error at once when the adapter has no Options.AppToken. It
// also returns an error when Slack rejects the app token or turns Socket Mode
// off (see slackapi.Client.Listen). It logs the errors that it recovers from
// to Options.Logger.
func (a *Adapter) Listen(ctx context.Context, dispatch chat.DispatchFunc) error {
	if a.appToken == "" {
		return errors.New("slack: app token is required to listen")
	}
	return a.api.WithToken(a.appToken).Listen(ctx, a.deliveryHandler(dispatch), slackapi.ListenOptions{})
}

// deliveryHandler returns the one inbound pipeline of the adapter, for both
// transports. It normalizes a delivery to an Event and dispatches it. An Ignored
// Event is acknowledged without dispatch.
func (a *Adapter) deliveryHandler(dispatch chat.DispatchFunc) slackapi.Handler {
	return func(ctx context.Context, d *slackapi.Delivery) (any, error) {
		event, err := a.normalizeDelivery(ctx, d)
		if err != nil || event == nil {
			return nil, err
		}
		err = dispatch(ctx, event)
		if !errors.Is(err, chat.ErrAdmissionRejected) {
			return nil, err
		}
		if event.Command != nil {
			// Slack does not redeliver slash commands, so a missing ack would
			// lose the invocation with only a generic Slack error shown. Slack
			// shows the ack body to the invoking user as an ephemeral message, so
			// the ack carries a truthful busy signal instead: never a silent
			// failure, never a fake success.
			return commandBusyReply{Text: commandBusyMessage}, nil
		}
		// Slack redelivers an Events API payload that gets no ack, and the
		// delivery was never dedupe-marked, so the redelivery is not deduped
		// away. Slack does not redeliver block_actions, but a missing ack is the
		// truthful signal: Slack shows a warning on the activated component, and
		// the rejection stays observable through Runtime Observation.
		return nil, fmt.Errorf("%w: %w", slackapi.ErrBusy, err)
	}
}

// normalizeDelivery returns the Event of a delivery, or a nil Event for an
// Ignored Event: a delivery type, envelope type, or interaction type that the
// adapter does not support, or a workspace with no install. A malformed or
// cross-tenant payload is an error that wraps slackapi.ErrInvalidPayload. An
// install store failure is another error.
func (a *Adapter) normalizeDelivery(ctx context.Context, d *slackapi.Delivery) (*chat.Event, error) {
	switch d.Type {
	case slackapi.DeliveryEventsAPI:
		return a.normalizeEvent(ctx, d)
	case slackapi.DeliverySlashCommands:
		return a.normalizeCommand(ctx, d)
	case slackapi.DeliveryInteractive:
		return a.normalizeInteraction(ctx, d)
	default:
		return nil, nil
	}
}

// invalidPayload marks err as a malformed or rejected payload, so the
// transport answers it with slackapi.ErrInvalidPayload.
func invalidPayload(err error) error {
	return invalidPayloadError{err}
}

type invalidPayloadError struct{ error }

func (e invalidPayloadError) Unwrap() []error {
	return []error{e.error, slackapi.ErrInvalidPayload}
}

// retryMetadata returns the Retry Metadata of a delivery.
func retryMetadata(d *slackapi.Delivery) chat.RetryMetadata {
	retry := chat.RetryMetadata{Reason: d.RetryReason}
	if d.RetryAttempt > 0 {
		retry.Num = strconv.Itoa(d.RetryAttempt)
	}
	return retry
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
	payload := slackapi.PostMessageRequest{
		Channel:      thread.Channel,
		Text:         messageFields.Text,
		MarkdownText: messageFields.MarkdownText,
		Mrkdwn:       messageFields.Mrkdwn,
	}
	if !thread.Direct {
		payload.ThreadTS = thread.Root
	}

	resp, err := a.api.WithToken(token).PostMessage(ctx, payload)
	if err != nil {
		return nil, err
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
		payload := slackapi.PostEphemeralRequest{
			Channel:      thread.Channel,
			User:         actor.ID,
			Text:         messageFields.Text,
			MarkdownText: messageFields.MarkdownText,
			Mrkdwn:       messageFields.Mrkdwn,
		}
		if !thread.Direct {
			payload.ThreadTS = thread.Root
		}

		resp, err := a.api.WithToken(token).PostEphemeral(ctx, payload)
		if err == nil {
			return &chat.SentMessage{ID: resp.MessageTS, ThreadID: thread.ID, Raw: resp}, nil
		}
		// Only an ok:false rejection falls back to a DM; a transport, HTTP status,
		// or rate-limit failure returns as is.
		if _, rejected := apiRejection(err); !rejected || !opts.FallbackToDM {
			return nil, err
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

// resolveInstall returns the per-tenant credential for an inbound delivery. In
// single-install mode it returns the static token and discovered bot identity. In
// multi-tenant mode it calls the InstallStore: ErrInstallNotFound (or an empty
// tenant) signals an Ignored Event (found=false, nil error, caller acks without
// dispatch); any other store error is a transport failure (found=false, non-nil
// error, caller does not ack, so Slack can redeliver). The same resolver serves Thread Handle posting via
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

// postToken resolves the per-tenant bot token for an out-of-delivery post (Thread
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

// normalizeEvent returns the Message Event of an Events API delivery, or a nil
// Event for an Ignored Event.
func (a *Adapter) normalizeEvent(ctx context.Context, d *slackapi.Delivery) (*chat.Event, error) {
	var envelope slackapi.Envelope
	if err := json.Unmarshal(d.Payload, &envelope); err != nil {
		return nil, invalidPayload(fmt.Errorf("slack: invalid event payload: %w", err))
	}
	if envelope.Type != slackapi.EnvelopeEventCallback {
		return nil, nil
	}
	install, found, err := a.resolveInstall(ctx, envelope.TeamID)
	if err != nil || !found {
		return nil, err
	}
	event, err := a.messageEvent(&envelope, d, install.botUserID)
	if err != nil {
		return nil, invalidPayload(err)
	}
	return event, nil
}

// messageEvent validates an event_callback envelope and returns its Message
// Event, or a nil Event for an event type that the adapter does not support or
// for the bot's own message in multi-tenant mode.
func (a *Adapter) messageEvent(envelope *slackapi.Envelope, d *slackapi.Delivery, botUserID string) (*chat.Event, error) {
	if envelope.TeamID == "" {
		return nil, errors.New("slack: team_id is required")
	}
	// The cross-tenant guard only applies to a single-install adapter pinned to one
	// team; in multi-tenant mode every workspace the store serves is valid.
	if !a.multiTenant() && a.teamID != "" && envelope.TeamID != a.teamID {
		return nil, fmt.Errorf("slack: team_id %q does not match configured team", envelope.TeamID)
	}
	if envelope.EventID == "" {
		return nil, errors.New("slack: event_id is required")
	}

	var ev slackEvent
	if err := json.Unmarshal(envelope.Event, &ev); err != nil {
		return nil, fmt.Errorf("slack: invalid event: %w", err)
	}
	if !supportedMessageEvent(ev) {
		return nil, nil
	}
	if ev.Channel == "" {
		return nil, errors.New("slack: event channel is required")
	}
	if ev.TS == "" {
		return nil, errors.New("slack: event ts is required")
	}

	threadID, direct, err := a.threadIDForEvent(envelope.TeamID, ev)
	if err != nil {
		return nil, err
	}
	author := a.actorForEvent(envelope.TeamID, ev, botUserID)
	if author.ID == "" {
		return nil, errors.New("slack: event author is required")
	}
	// Multi-tenant self-filtering is tenant-correct from the per-install bot id: the
	// runtime's BotActor() filter cannot match because there is no single bot
	// identity, so drop the bot's own messages here as an Ignored Event.
	if a.multiTenant() && author.BotKind == chat.BotBot && botUserID != "" && author.ID == botUserID {
		return nil, nil
	}

	return &chat.Event{
		ID:            envelope.EventID,
		Adapter:       adapterName,
		Tenant:        envelope.TeamID,
		ThreadID:      threadID,
		DirectMessage: direct,
		Retry:         retryMetadata(d),
		Raw:           d.Payload,
		Message: &chat.Message{
			ID:        ev.TS,
			Text:      ev.Text,
			Author:    author,
			Mentioned: direct || ev.Type == "app_mention" || strings.Contains(ev.Text, "<@"+botUserID+">"),
			Raw:       ev.Raw,
		},
	}, nil
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

func (a *Adapter) postEphemeralFallback(ctx context.Context, token string, tenant string, actor chat.Actor, msg chat.PostableMessage) (*chat.SentMessage, error) {
	messageFields, err := slackMessageFields(msg)
	if err != nil {
		return nil, err
	}
	api := a.api.WithToken(token)
	conv, err := api.OpenConversation(ctx, slackapi.OpenConversationRequest{Users: actor.ID})
	if err != nil {
		return nil, err
	}
	if conv.ID == "" {
		return nil, errors.New("slack: conversations.open did not return channel id")
	}

	threadID, err := encodeThreadID(threadPayload{Team: tenant, Channel: conv.ID, Direct: true})
	if err != nil {
		return nil, err
	}
	postResp, err := api.PostMessage(ctx, slackapi.PostMessageRequest{
		Channel:      conv.ID,
		Text:         messageFields.Text,
		MarkdownText: messageFields.MarkdownText,
		Mrkdwn:       messageFields.Mrkdwn,
	})
	if err != nil {
		if apiErr, rejected := apiRejection(err); rejected {
			return nil, fmt.Errorf("slack: fallback chat.postMessage failed: %s", apiErr.Code)
		}
		return nil, err
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

// apiRejection reports whether err is an ok:false Web API response, as opposed to
// a transport, HTTP status, or rate-limit failure.
func apiRejection(err error) (*slackapi.APIError, bool) {
	apiErr, ok := errors.AsType[*slackapi.APIError](err)
	if !ok || apiErr.StatusCode < 200 || apiErr.StatusCode > 299 {
		return nil, false
	}
	return apiErr, true
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
	Direct  bool   `json:"direct,omitzero"`
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
	rest, ok := strings.CutPrefix(string(id), prefix)
	if !ok {
		return threadPayload{}, fmt.Errorf("slack: malformed thread id %q", id)
	}
	body, err := base64.RawURLEncoding.DecodeString(rest)
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
