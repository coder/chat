package slack

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/coder/chat"
)

// noopObserver is the adapter's default Observer when none is configured.
type noopObserver struct{}

func (noopObserver) Event(context.Context, chat.ObservationName, ...chat.Attr) {}

func (noopObserver) Dispatch(ctx context.Context, _ ...chat.Attr) (context.Context, chat.DispatchSpan) {
	return ctx, noopSpan{}
}

type noopSpan struct{}

func (noopSpan) End(chat.DispatchOutcome, ...chat.Attr) {}

// Compile-time assertions for the Optional Capabilities reached via Adapter Access.
var (
	_ chat.NativeContentPoster = (*Adapter)(nil)
	_ chat.EphemeralPoster     = (*Adapter)(nil)
)

// commandForm is the Supported Platform Shape for a Slack slash command.
type commandForm struct {
	Command     string
	Text        string
	TeamID      string
	ChannelID   string
	ChannelName string
	UserID      string
	TriggerID   string
	ResponseURL string
	APIAppID    string
}

// handleCommandForm resolves the per-tenant install for a slash command, then
// normalizes and dispatches it. A not-installed workspace is an Ignored Event
// (ack 200, no dispatch); a store transport error is a 5xx the platform may retry.
func (a *Adapter) handleCommandForm(w http.ResponseWriter, r *http.Request, dispatch chat.DispatchFunc, form url.Values) {
	_, found, err := a.resolveInstall(r.Context(), form.Get("team_id"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if !found {
		w.WriteHeader(http.StatusOK)
		return
	}
	event, err := a.normalizeCommand(r, form)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if err := dispatch(r.Context(), event); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusOK)
}

// normalizeCommand decodes and validates a slash command into a Command Event. A
// missing required field or a cross-tenant team is a 400, like a malformed event.
func (a *Adapter) normalizeCommand(r *http.Request, form url.Values) (*chat.Event, error) {
	cmd := commandForm{
		Command:     form.Get("command"),
		Text:        form.Get("text"),
		TeamID:      form.Get("team_id"),
		ChannelID:   form.Get("channel_id"),
		ChannelName: form.Get("channel_name"),
		UserID:      form.Get("user_id"),
		TriggerID:   form.Get("trigger_id"),
		ResponseURL: form.Get("response_url"),
		APIAppID:    form.Get("api_app_id"),
	}
	if cmd.Command == "" {
		return nil, errors.New("slack: command is required")
	}
	if cmd.TeamID == "" {
		return nil, errors.New("slack: command team_id is required")
	}
	if cmd.ChannelID == "" {
		return nil, errors.New("slack: command channel_id is required")
	}
	if cmd.UserID == "" {
		return nil, errors.New("slack: command user_id is required")
	}
	if cmd.TriggerID == "" {
		return nil, errors.New("slack: command trigger_id is required")
	}
	if !a.multiTenant() && a.teamID != "" && cmd.TeamID != a.teamID {
		return nil, fmt.Errorf("slack: team_id %q does not match configured team", cmd.TeamID)
	}

	direct := isDirectChannel(cmd.ChannelID, cmd.ChannelName)
	root := ""
	if !direct {
		// A slash command carries no message ts, so root the Thread at the channel.
		root = cmd.ChannelID
	}
	threadID, err := encodeThreadID(threadPayload{
		Team:    cmd.TeamID,
		Channel: cmd.ChannelID,
		Root:    root,
		Direct:  direct,
	})
	if err != nil {
		return nil, err
	}

	return &chat.Event{
		ID:            commandEventID(cmd),
		Adapter:       adapterName,
		Tenant:        cmd.TeamID,
		ThreadID:      threadID,
		DirectMessage: direct,
		Retry: chat.RetryMetadata{
			Num:    r.Header.Get("X-Slack-Retry-Num"),
			Reason: r.Header.Get("X-Slack-Retry-Reason"),
		},
		Raw: cmd,
		Command: &chat.Command{
			Name:  cmd.Command,
			Text:  cmd.Text,
			Args:  strings.Fields(cmd.Text),
			Actor: a.humanActor(cmd.TeamID, cmd.UserID),
			Raw:   cmd,
		},
	}, nil
}

// commandEventID derives a stable, adapter-scoped Event Identity for a slash
// command. Slack sends no retry header for commands, so the deterministic id keeps
// dedupe meaningful only when Slack re-delivers the same trigger_id.
func commandEventID(cmd commandForm) string {
	return strings.Join([]string{
		adapterName, "command",
		cmd.TeamID, cmd.ChannelID, cmd.UserID, cmd.Command, cmd.TriggerID,
	}, ":")
}

// interactionPayload is the Supported Platform Shape for Slack interactivity.
type interactionPayload struct {
	Type string `json:"type"`
	Team struct {
		ID string `json:"id"`
	} `json:"team"`
	User struct {
		ID string `json:"id"`
	} `json:"user"`
	Channel struct {
		ID string `json:"id"`
	} `json:"channel"`
	Container struct {
		ChannelID string `json:"channel_id"`
		ThreadTS  string `json:"thread_ts"`
		MessageTS string `json:"message_ts"`
	} `json:"container"`
	Actions []struct {
		ActionID string `json:"action_id"`
		BlockID  string `json:"block_id"`
		Value    string `json:"value"`
		Type     string `json:"type"`
	} `json:"actions"`
	TriggerID   string          `json:"trigger_id"`
	ResponseURL string          `json:"response_url"`
	View        json.RawMessage `json:"view"`
	Raw         json.RawMessage `json:"-"`
}

// handleInteractionForm resolves the per-tenant install for an interactivity
// payload, then normalizes and dispatches it. An unsupported interaction type and
// a not-installed workspace are Ignored Events (ack 200, no dispatch); a store
// transport error is a 5xx the platform may retry; a missing required field is a
// 400.
func (a *Adapter) handleInteractionForm(w http.ResponseWriter, r *http.Request, dispatch chat.DispatchFunc, raw string) {
	var payload interactionPayload
	if err := json.Unmarshal([]byte(raw), &payload); err != nil {
		http.Error(w, "slack: invalid interactivity payload", http.StatusBadRequest)
		return
	}
	payload.Raw = json.RawMessage(raw)

	if payload.Type != "block_actions" {
		w.WriteHeader(http.StatusOK)
		return
	}

	_, found, err := a.resolveInstall(r.Context(), payload.Team.ID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if !found {
		w.WriteHeader(http.StatusOK)
		return
	}

	event, err := a.normalizeInteraction(r, payload)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if err := dispatch(r.Context(), event); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusOK)
}

// normalizeInteraction decodes a verified Slack block_actions payload into an
// Interaction Event. A missing required field is a 400, never an ignore.
func (a *Adapter) normalizeInteraction(r *http.Request, payload interactionPayload) (*chat.Event, error) {
	if payload.Team.ID == "" {
		return nil, errors.New("slack: interaction team id is required")
	}
	if !a.multiTenant() && a.teamID != "" && payload.Team.ID != a.teamID {
		return nil, fmt.Errorf("slack: team_id %q does not match configured team", payload.Team.ID)
	}
	if payload.User.ID == "" {
		return nil, errors.New("slack: interaction user id is required")
	}
	channelID := firstNonEmpty(payload.Channel.ID, payload.Container.ChannelID)
	if channelID == "" {
		return nil, errors.New("slack: interaction channel id is required")
	}
	if len(payload.Actions) == 0 || payload.Actions[0].ActionID == "" {
		return nil, errors.New("slack: interaction action_id is required")
	}

	direct := isDirectChannel(channelID, "")
	root := ""
	if !direct {
		// Anchor to the message bearing the block: thread_ts if threaded, else ts.
		root = firstNonEmpty(payload.Container.ThreadTS, payload.Container.MessageTS)
		if root == "" {
			return nil, errors.New("slack: interaction message ts is required")
		}
	}
	threadID, err := encodeThreadID(threadPayload{
		Team:    payload.Team.ID,
		Channel: channelID,
		Root:    root,
		Direct:  direct,
	})
	if err != nil {
		return nil, err
	}

	return &chat.Event{
		ID:            interactionEventID(payload, channelID),
		Adapter:       adapterName,
		Tenant:        payload.Team.ID,
		ThreadID:      threadID,
		DirectMessage: direct,
		Retry: chat.RetryMetadata{
			Num:    r.Header.Get("X-Slack-Retry-Num"),
			Reason: r.Header.Get("X-Slack-Retry-Reason"),
		},
		Raw: payload,
		Interaction: &chat.Interaction{
			Kind:     chat.InteractionBlockAction,
			ActionID: payload.Actions[0].ActionID,
			Actor:    a.humanActor(payload.Team.ID, payload.User.ID),
			Raw:      payload,
		},
	}, nil
}

// interactionEventID derives a stable Event Identity for a block_actions payload so
// a re-delivered click is deduped.
func interactionEventID(payload interactionPayload, channelID string) string {
	anchor := firstNonEmpty(payload.Container.MessageTS, payload.Container.ThreadTS, payload.TriggerID)
	return strings.Join([]string{
		adapterName, "interaction",
		payload.Team.ID, channelID, payload.User.ID, payload.Actions[0].ActionID, anchor,
	}, ":")
}

func (a *Adapter) humanActor(teamID, userID string) chat.Actor {
	return chat.Actor{
		Adapter: adapterName,
		Tenant:  teamID,
		ID:      userID,
		BotKind: chat.BotHuman,
	}
}

func isDirectChannel(channelID, channelName string) bool {
	return strings.HasPrefix(channelID, "D") || channelName == "directmessage"
}

// PostNative implements the NativeContentPoster Optional Capability, posting Block
// Kit blocks to a thread. A content adapter that does not match "slack" is an
// error, never a silent portable downgrade.
func (a *Adapter) PostNative(ctx context.Context, thread chat.ThreadRef, content chat.NativeContent) (*chat.SentMessage, error) {
	if content.Adapter != adapterName {
		return nil, fmt.Errorf("slack: native content adapter %q does not match %q", content.Adapter, adapterName)
	}
	if content.Payload == nil {
		return nil, errors.New("slack: native content payload is required")
	}
	token, err := a.postToken(ctx, thread.Tenant)
	if err != nil {
		return nil, err
	}
	payload := postMessagePayload{
		Channel: thread.Channel,
		Blocks:  content.Payload,
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

// OpenModal opens a modal via views.open using a trigger_id reachable from a
// Command or Interaction Event's Raw escape hatch. The opaque view is an outbound
// Optional Capability; the synchronous view_submission response is a deferred slice.
//
// In multi-tenant mode the workspace token is resolved from the Thread ID's tenant
// via the same install resolver; the caller supplies the thread the trigger came
// from so the correct workspace token is used.
func (a *Adapter) OpenModal(ctx context.Context, triggerID string, view any) error {
	if triggerID == "" {
		return errors.New("slack: trigger_id is required to open a modal")
	}
	if view == nil {
		return errors.New("slack: modal view is required")
	}
	if a.multiTenant() {
		return errors.New("slack: OpenModal requires a workspace token; use OpenModalForTenant in multi-tenant mode")
	}
	return a.openModalWithToken(ctx, a.botToken, triggerID, view)
}

// OpenModalForTenant opens a modal authorizing with the workspace resolved from
// the given Platform Tenant, for multi-tenant deployments. Single-install callers
// use OpenModal.
func (a *Adapter) OpenModalForTenant(ctx context.Context, tenant string, triggerID string, view any) error {
	if triggerID == "" {
		return errors.New("slack: trigger_id is required to open a modal")
	}
	if view == nil {
		return errors.New("slack: modal view is required")
	}
	token, err := a.postToken(ctx, tenant)
	if err != nil {
		return err
	}
	return a.openModalWithToken(ctx, token, triggerID, view)
}

func (a *Adapter) openModalWithToken(ctx context.Context, token string, triggerID string, view any) error {
	var resp openViewResponse
	if err := a.callWithToken(ctx, token, "views.open", openViewPayload{TriggerID: triggerID, View: view}, &resp); err != nil {
		return err
	}
	if !resp.OK {
		return fmt.Errorf("slack: views.open failed: %s", resp.Error)
	}
	return nil
}

// RespondURL posts a portable reply to the response_url carried on a Command or
// Interaction Event's Raw escape hatch (which must be produced by this adapter).
// It keeps the native response path adapter-owned without widening Postable Message.
// It needs no bot token: the response_url is a pre-authorized webhook, so it works
// identically in single-install and multi-tenant modes.
func (a *Adapter) RespondURL(ctx context.Context, raw any, msg chat.PostableMessage) error {
	responseURL := responseURLFromRaw(raw)
	if responseURL == "" {
		return errors.New("slack: no response_url on the escape hatch")
	}
	fields, err := slackMessageFields(msg)
	if err != nil {
		return err
	}
	body, err := json.Marshal(responseURLPayload{
		Text:         fields.Text,
		MarkdownText: fields.MarkdownText,
		Mrkdwn:       fields.Mrkdwn,
		ResponseType: "ephemeral",
	})
	if err != nil {
		return fmt.Errorf("slack: encode response_url body: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, responseURL, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	// The response_url is a pre-authorized webhook returning a plain 200/429, so it
	// shares the bounded rate-limit retry seam (ADR 0005) with a nil dest: a 429 with
	// Retry-After is retried within the RetryPolicy and surfaces as a typed
	// *RateLimited on exhaustion, with no JSON envelope to decode.
	return a.doWithRetry(ctx, "response_url", req, nil)
}

// responseURLFromRaw extracts the preserved response_url from a Command.Raw or
// Interaction.Raw escape hatch.
func responseURLFromRaw(raw any) string {
	switch v := raw.(type) {
	case commandForm:
		return v.ResponseURL
	case *commandForm:
		return v.ResponseURL
	case interactionPayload:
		return v.ResponseURL
	case *interactionPayload:
		return v.ResponseURL
	default:
		return ""
	}
}

type openViewPayload struct {
	TriggerID string `json:"trigger_id"`
	View      any    `json:"view"`
}

type openViewResponse struct {
	OK    bool   `json:"ok"`
	Error string `json:"error"`
}

type responseURLPayload struct {
	Text         string `json:"text,omitempty"`
	MarkdownText string `json:"markdown_text,omitempty"`
	Mrkdwn       *bool  `json:"mrkdwn,omitempty"`
	ResponseType string `json:"response_type,omitempty"`
}
