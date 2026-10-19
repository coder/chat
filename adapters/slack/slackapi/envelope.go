package slackapi

import (
	"encoding/json"
	"errors"
	"fmt"
	"slices"
)

// Envelope types of a Slack Events API request.
const (
	// EnvelopeURLVerification is the envelope type of the request that Slack
	// sends to verify a new request URL. Answer it with Envelope.Challenge.
	EnvelopeURLVerification = "url_verification"
	// EnvelopeEventCallback is the envelope type of a request that carries an
	// event in Envelope.Event.
	EnvelopeEventCallback = "event_callback"
)

// Event types that the event methods of Envelope decode.
const (
	// EventTypeAppMention is the type of an event for a message that
	// mentions the app.
	EventTypeAppMention = "app_mention"
	// EventTypeMessage is the type of an event for a message in a
	// conversation that the app is a member of.
	EventTypeMessage = "message"
	// EventTypeAgentSessionStopped is the type of an event for a click on the
	// Stop button of an agent session.
	EventTypeAgentSessionStopped = "agent_session_stopped"
	// EventTypeChannelShared is the type of an event for a channel that is
	// shared with another workspace or organization.
	EventTypeChannelShared = "channel_shared"
)

// Message subtypes of Message.Subtype.
const (
	// SubtypeMessageChanged marks an edit of a message. The new message is in
	// MessageEvent.NewMessage and the old one in MessageEvent.PreviousMessage.
	SubtypeMessageChanged = "message_changed"
	// SubtypeMessageDeleted marks a deleted message. The deleted message is
	// in MessageEvent.PreviousMessage.
	SubtypeMessageDeleted = "message_deleted"
	// SubtypeBotMessage marks a message from a bot integration.
	SubtypeBotMessage = "bot_message"
	// SubtypeFileShare marks a message with files attached.
	SubtypeFileShare = "file_share"
	// SubtypeThreadBroadcast marks a thread reply that is also sent to the
	// channel.
	SubtypeThreadBroadcast = "thread_broadcast"
)

// Envelope is the outer object of a Slack Events API request body.
type Envelope struct {
	// Type is the envelope type, for example EnvelopeEventCallback.
	Type string `json:"type,omitempty"`
	// Challenge is the value to return for an EnvelopeURLVerification
	// request.
	Challenge string `json:"challenge,omitempty"`
	// TeamID is the ID of the workspace that the event comes from.
	TeamID string `json:"team_id,omitempty"`
	// APIAppID is the ID of the app that the event is for.
	APIAppID string `json:"api_app_id,omitempty"`
	// EventID is the unique ID of the event. Slack sends the same ID again
	// when it retries a delivery.
	EventID string `json:"event_id,omitempty"`
	// EventTime is the time of the event in Unix seconds.
	EventTime int64 `json:"event_time,omitzero"`
	// IsExtSharedChannel is true when the event comes from a channel that is
	// shared with another organization.
	IsExtSharedChannel bool `json:"is_ext_shared_channel,omitzero"`
	// Event is the raw inner event of an EnvelopeEventCallback request.
	Event json.RawMessage `json:"event,omitempty"`
}

// ParseEnvelope decodes a Slack Events API request body. It does not verify
// the request signature. It returns an error if the body is not valid JSON or
// has no type.
func ParseEnvelope(body []byte) (*Envelope, error) {
	var env Envelope
	if err := json.Unmarshal(body, &env); err != nil {
		return nil, fmt.Errorf("slack: invalid envelope: %w", err)
	}
	if env.Type == "" {
		return nil, errors.New("slack: envelope type is required")
	}
	return &env, nil
}

// MessageEvent is an app_mention or message event. For a
// SubtypeMessageChanged or SubtypeMessageDeleted event, the embedded Message
// describes the event itself, and NewMessage and PreviousMessage describe the
// changed message.
type MessageEvent struct {
	Message
	// Channel is the ID of the conversation of the message.
	Channel string `json:"channel,omitempty"`
	// ChannelType is the conversation type: "channel", "group", "im", or
	// "mpim".
	ChannelType string `json:"channel_type,omitempty"`
	// EventTS is the timestamp of the event.
	EventTS string `json:"event_ts,omitempty"`
	// NewMessage is the message after the edit of a SubtypeMessageChanged
	// event.
	NewMessage *Message `json:"message,omitempty"`
	// PreviousMessage is the message before the edit of a
	// SubtypeMessageChanged event, or the deleted message of a
	// SubtypeMessageDeleted event.
	PreviousMessage *Message `json:"previous_message,omitempty"`
}

// MessageEvent decodes the inner event of an EnvelopeEventCallback envelope
// when its type is EventTypeAppMention or EventTypeMessage, with any subtype.
// It returns false for an envelope of another type and for an event of
// another type. It returns an error if the event is missing or is not valid
// JSON for its type.
func (e *Envelope) MessageEvent() (*MessageEvent, bool, error) {
	return decodeEvent[MessageEvent](e, EventTypeAppMention, EventTypeMessage)
}

// AgentSessionStoppedEvent is an agent_session_stopped event. Slack sends it
// when a user clicks Stop on an agent session in the processing status. Slack
// does not change the status of the session.
type AgentSessionStoppedEvent struct {
	// Channel is the ID of the channel of the session.
	Channel string `json:"channel,omitempty"`
	// ThreadTS is the ts of the parent message of the thread of the session.
	ThreadTS string `json:"thread_ts,omitempty"`
	// User is the ID of the user who clicked Stop.
	User string `json:"user,omitempty"`
	// EventTS is the timestamp of the event.
	EventTS string `json:"event_ts,omitempty"`
}

// AgentSessionStoppedEvent decodes the inner event of an EnvelopeEventCallback
// envelope when its type is EventTypeAgentSessionStopped. It returns false and
// errors as MessageEvent does.
func (e *Envelope) AgentSessionStoppedEvent() (*AgentSessionStoppedEvent, bool, error) {
	return decodeEvent[AgentSessionStoppedEvent](e, EventTypeAgentSessionStopped)
}

// ChannelSharedEvent is a channel_shared event. Slack sends it when a channel
// becomes shared, and when another workspace or organization joins a shared
// channel. Slack also sends it for a share between the workspaces of one
// Enterprise Grid organization.
type ChannelSharedEvent struct {
	// Channel is the ID of the channel.
	Channel string `json:"channel,omitempty"`
	// ConnectedTeamID is the ID of the workspace (T) or the organization (E)
	// that the channel is now shared with.
	ConnectedTeamID string `json:"connected_team_id,omitempty"`
	// EventTS is the timestamp of the event.
	EventTS string `json:"event_ts,omitempty"`
}

// ChannelSharedEvent decodes the inner event of an EnvelopeEventCallback
// envelope when its type is EventTypeChannelShared. It returns false and
// errors as MessageEvent does.
func (e *Envelope) ChannelSharedEvent() (*ChannelSharedEvent, bool, error) {
	return decodeEvent[ChannelSharedEvent](e, EventTypeChannelShared)
}

// decodeEvent decodes the inner event of an EnvelopeEventCallback envelope
// into T when the event has one of types. It returns false for an envelope of
// another type and for an event of another type, and an error if the event is
// missing or is not valid JSON for T.
func decodeEvent[T any](e *Envelope, types ...string) (*T, bool, error) {
	if e.Type != EnvelopeEventCallback {
		return nil, false, nil
	}
	if len(e.Event) == 0 {
		return nil, false, errors.New("slack: event is required")
	}
	var head struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(e.Event, &head); err != nil {
		return nil, false, fmt.Errorf("slack: invalid event: %w", err)
	}
	if !slices.Contains(types, head.Type) {
		// Other event types can reuse field names with other shapes, for
		// example an object in "channel", so they are not decoded further.
		return nil, false, nil
	}
	var ev T
	if err := json.Unmarshal(e.Event, &ev); err != nil {
		return nil, false, fmt.Errorf("slack: invalid %s event: %w", head.Type, err)
	}
	return &ev, true, nil
}
