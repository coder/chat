package slack

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/coder/chat"
	"github.com/coder/chat/adapters/slack/slackapi"
)

// Compile-time assertions for the Optional Capabilities reached via Adapter Access.
var (
	_ chat.NativeContentPoster = (*Adapter)(nil)
	_ chat.EphemeralPoster     = (*Adapter)(nil)
)

// commandForm is the Supported Platform Shape for a Slack slash command.
type commandForm struct {
	Command     string `json:"command"`
	Text        string `json:"text"`
	TeamID      string `json:"team_id"`
	ChannelID   string `json:"channel_id"`
	ChannelName string `json:"channel_name"`
	UserID      string `json:"user_id"`
	TriggerID   string `json:"trigger_id"`
	ResponseURL string `json:"response_url"`
	APIAppID    string `json:"api_app_id"`
}

// commandBusyMessage is the truthful busy signal a slash command invoker sees
// when the runtime rejects the invocation at admission (ADR 0015). It renders
// as the command's ephemeral response.
const commandBusyMessage = "The bot is at capacity right now. Your command was not run — please try again in a moment."

// commandBusyReply is the ack body of a slash command that the runtime rejects
// at admission (the Admission Bound or Runtime Shutdown). Slack shows its text
// to the invoking user.
type commandBusyReply struct {
	Text string `json:"text"`
}

// normalizeCommand returns the Command Event of a slash command delivery, or a
// nil Event when the workspace has no install. A missing required field or a
// cross-tenant team is an invalid payload, like a malformed event.
func (a *Adapter) normalizeCommand(ctx context.Context, d *slackapi.Delivery) (*chat.Event, error) {
	var cmd commandForm
	if err := json.Unmarshal(d.Payload, &cmd); err != nil {
		return nil, invalidPayload(fmt.Errorf("slack: invalid command payload: %w", err))
	}
	_, found, err := a.resolveInstall(ctx, cmd.TeamID)
	if err != nil || !found {
		return nil, err
	}
	event, err := a.commandEvent(cmd, d)
	if err != nil {
		return nil, invalidPayload(err)
	}
	return event, nil
}

// commandEvent validates a slash command and returns its Command Event.
func (a *Adapter) commandEvent(cmd commandForm, d *slackapi.Delivery) (*chat.Event, error) {
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
		Retry:         retryMetadata(d),
		Raw:           cmd,
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
		// ActionTS is Slack's per-activation timestamp, minted server-side when
		// the user performs the action; a redelivery of the same activation
		// carries the same value.
		ActionTS        string           `json:"action_ts"`
		SelectedOption  selectedOption   `json:"selected_option"`
		SelectedOptions []selectedOption `json:"selected_options"`
	} `json:"actions"`
	TriggerID   string          `json:"trigger_id"`
	ResponseURL string          `json:"response_url"`
	View        json.RawMessage `json:"view"`
	Raw         json.RawMessage `json:"-"`
}

// selectedOption is the value-bearing shape of a menu option in a block_actions
// action (static_select, external_select, overflow, radio_buttons via
// selected_option; multi_static_select, checkboxes via selected_options).
type selectedOption struct {
	Value string `json:"value"`
}

// normalizeInteraction returns the Interaction Event of an interactivity
// delivery, or a nil Event for an Ignored Event: an interaction type that the
// adapter does not support, or a workspace with no install. A missing required
// field is an invalid payload, never an ignore.
func (a *Adapter) normalizeInteraction(ctx context.Context, d *slackapi.Delivery) (*chat.Event, error) {
	var payload interactionPayload
	if err := json.Unmarshal(d.Payload, &payload); err != nil {
		return nil, invalidPayload(fmt.Errorf("slack: invalid interactivity payload: %w", err))
	}
	payload.Raw = d.Payload
	if payload.Type != "block_actions" {
		return nil, nil
	}
	_, found, err := a.resolveInstall(ctx, payload.Team.ID)
	if err != nil || !found {
		return nil, err
	}
	event, err := a.interactionEvent(payload, d)
	if err != nil {
		return nil, invalidPayload(err)
	}
	return event, nil
}

// interactionEvent validates a block_actions payload and returns its
// Interaction Event.
func (a *Adapter) interactionEvent(payload interactionPayload, d *slackapi.Delivery) (*chat.Event, error) {
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

	action := payload.Actions[0]
	// A present-but-empty selected_options (the user cleared every option) must
	// surface as an empty non-nil Values, distinct from the nil of a
	// single-valued component.
	var values []string
	if action.SelectedOptions != nil {
		values = make([]string, 0, len(action.SelectedOptions))
		for _, opt := range action.SelectedOptions {
			values = append(values, opt.Value)
		}
	}

	return &chat.Event{
		ID:            interactionEventID(payload, channelID),
		Adapter:       adapterName,
		Tenant:        payload.Team.ID,
		ThreadID:      threadID,
		DirectMessage: direct,
		Retry:         retryMetadata(d),
		Raw:           payload,
		Interaction: &chat.Interaction{
			Kind:     chat.InteractionBlockAction,
			ActionID: action.ActionID,
			Value:    firstNonEmpty(action.Value, action.SelectedOption.Value),
			Values:   values,
			Actor:    a.humanActor(payload.Team.ID, payload.User.ID),
			Raw:      payload,
		},
	}, nil
}

// interactionEventID derives a stable Event Identity for a block_actions payload
// so a re-delivered activation is deduped without collapsing distinct
// activations. Slack sends no retry headers for interactivity, so dedupe must
// discriminate purely by identity: the anchor is the per-activation action_ts
// (trigger_id fallback), which is minted server-side per activation — a genuine
// redelivery of one activation repeats it, while every new click or selection
// gets a fresh value. The message ts anchors only payloads carrying no
// per-activation value at all, preserving redelivery dedupe there too.
func interactionEventID(payload interactionPayload, channelID string) string {
	anchor := firstNonEmpty(payload.Actions[0].ActionTS, payload.TriggerID, payload.Container.MessageTS, payload.Container.ThreadTS)
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
	// docs/reference.md says PostNative posts opaque Block Kit blocks, and Payload is
	// an any. The typed PostMessage takes []slackapi.Block, so it would need a decode
	// and re-encode of a payload that must go out unchanged.
	payload := nativePostPayload{
		Channel: thread.Channel,
		Blocks:  content.Payload,
	}
	if !thread.Direct {
		payload.ThreadTS = thread.Root
	}

	var resp slackapi.PostMessageResponse
	if err := a.api.WithToken(token).Call(ctx, "chat.postMessage", payload, &resp); err != nil {
		return nil, err
	}
	return &chat.SentMessage{ID: resp.TS, ThreadID: thread.ID, Raw: &resp}, nil
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
	return a.api.WithToken(token).Call(ctx, "views.open", openViewPayload{TriggerID: triggerID, View: view}, &resp)
}

// OpenModalFromRaw opens a modal using the trigger_id preserved on a Command or
// Interaction Event's Raw escape hatch (which must be produced by this adapter),
// matching the RespondURL shape so application code never handles trigger_id
// directly. Multi-tenant deployments use OpenModalForTenantFromRaw.
func (a *Adapter) OpenModalFromRaw(ctx context.Context, raw any, view any) error {
	triggerID := triggerIDFromRaw(raw)
	if triggerID == "" {
		return errors.New("slack: no trigger_id on the escape hatch")
	}
	return a.OpenModal(ctx, triggerID, view)
}

// OpenModalForTenantFromRaw opens a modal using the trigger_id preserved on the
// Raw escape hatch, authorizing with the workspace resolved from the given
// Platform Tenant, for multi-tenant deployments. Single-install callers use
// OpenModalFromRaw.
func (a *Adapter) OpenModalForTenantFromRaw(ctx context.Context, tenant string, raw any, view any) error {
	triggerID := triggerIDFromRaw(raw)
	if triggerID == "" {
		return errors.New("slack: no trigger_id on the escape hatch")
	}
	return a.OpenModalForTenant(ctx, tenant, triggerID, view)
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
	// The response_url is a pre-authorized webhook returning a plain 200/429, so it
	// shares the bounded rate-limit retry seam (ADR 0005): a 429 with Retry-After is
	// retried within the RetryPolicy and surfaces as a typed *RateLimited on
	// exhaustion, with no JSON envelope to decode.
	return a.api.PostResponseURL(ctx, responseURL, responseURLPayload{
		Text:         fields.Text,
		MarkdownText: fields.MarkdownText,
		Mrkdwn:       fields.Mrkdwn,
		ResponseType: "ephemeral",
	})
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

// triggerIDFromRaw extracts the preserved trigger_id from a Command.Raw or
// Interaction.Raw escape hatch.
func triggerIDFromRaw(raw any) string {
	switch v := raw.(type) {
	case commandForm:
		return v.TriggerID
	case *commandForm:
		return v.TriggerID
	case interactionPayload:
		return v.TriggerID
	case *interactionPayload:
		return v.TriggerID
	default:
		return ""
	}
}

type nativePostPayload struct {
	Channel  string `json:"channel"`
	ThreadTS string `json:"thread_ts,omitempty"`
	Blocks   any    `json:"blocks,omitempty"`
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
