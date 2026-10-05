package slackapi

import (
	"encoding/json"
	"errors"
	"fmt"
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

// Event types that MessageEvent decodes.
const (
	// EventTypeAppMention is the type of an event for a message that
	// mentions the app.
	EventTypeAppMention = "app_mention"
	// EventTypeMessage is the type of an event for a message in a
	// conversation that the app is a member of.
	EventTypeMessage = "message"
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
	if head.Type != EventTypeAppMention && head.Type != EventTypeMessage {
		// Other event types can reuse field names with other shapes, for
		// example an object in "channel", so they are not decoded further.
		return nil, false, nil
	}
	var ev MessageEvent
	if err := json.Unmarshal(e.Event, &ev); err != nil {
		return nil, false, fmt.Errorf("slack: invalid %s event: %w", head.Type, err)
	}
	return &ev, true, nil
}
