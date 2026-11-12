package slackapi

import (
	"encoding/json"
	"fmt"
)

// InteractionTypeBlockActions is the type of the interaction payload that
// Slack sends when someone uses an interactive element, such as a button.
const InteractionTypeBlockActions = "block_actions"

// BlockActionsPayload is a block_actions interaction payload.
type BlockActionsPayload struct {
	// Type is InteractionTypeBlockActions.
	Type string `json:"type,omitempty"`
	// Team is the workspace of the user. It is zero when Slack sends null,
	// for an org-wide install on Enterprise Grid.
	Team InteractionTeam `json:"team,omitzero"`
	// User is the user who used the element.
	User InteractionUser `json:"user,omitzero"`
	// APIAppID is the ID of the app.
	APIAppID string `json:"api_app_id,omitempty"`
	// Container is where the element is.
	Container InteractionContainer `json:"container,omitzero"`
	// TriggerID opens a modal within 3 seconds of the interaction.
	TriggerID string `json:"trigger_id,omitempty"`
	// Channel is the conversation of the element.
	Channel InteractionChannel `json:"channel,omitzero"`
	// Message is the message that holds the element. It is nil when the
	// element is not in a message. Message.ThreadTS is its thread.
	Message *Message `json:"message,omitempty"`
	// ResponseURL is the URL that PostResponseURL can post a response to.
	ResponseURL string `json:"response_url,omitempty"`
	// Actions are the actions of the interaction, usually one.
	Actions []BlockAction `json:"actions,omitempty"`
}

// InteractionTeam is the workspace in an interaction payload.
type InteractionTeam struct {
	// ID is the workspace ID (T).
	ID string `json:"id,omitempty"`
	// Domain is the subdomain of the workspace.
	Domain string `json:"domain,omitempty"`
}

// InteractionUser is the user in an interaction payload.
type InteractionUser struct {
	// ID is the user ID.
	ID string `json:"id,omitempty"`
	// Username is the user name.
	Username string `json:"username,omitempty"`
	// Name is the name of the user.
	Name string `json:"name,omitempty"`
	// TeamID is the ID of the workspace of the user.
	TeamID string `json:"team_id,omitempty"`
}

// InteractionContainer is the container of the element in an interaction
// payload.
type InteractionContainer struct {
	// Type is the container type, for example "message".
	Type string `json:"type,omitempty"`
	// MessageTS is the ts of the message that holds the element.
	MessageTS string `json:"message_ts,omitempty"`
	// ChannelID is the ID of the conversation of the message.
	ChannelID string `json:"channel_id,omitempty"`
	// IsEphemeral is true when the message is an ephemeral message.
	IsEphemeral bool `json:"is_ephemeral,omitzero"`
}

// InteractionChannel is the conversation in an interaction payload.
type InteractionChannel struct {
	// ID is the conversation ID.
	ID string `json:"id,omitempty"`
	// Name is the name of the conversation.
	Name string `json:"name,omitempty"`
}

// BlockAction is one action in a BlockActionsPayload.
type BlockAction struct {
	// Type is the element type, for example "button".
	Type string `json:"type,omitempty"`
	// ActionID is the action_id of the element.
	ActionID string `json:"action_id,omitempty"`
	// BlockID is the block_id of the block that holds the element.
	BlockID string `json:"block_id,omitempty"`
	// Value is the value of the element, for example ButtonElement.Value.
	Value string `json:"value,omitempty"`
	// ActionTS is the timestamp of the action.
	ActionTS string `json:"action_ts,omitempty"`
}

// ParseBlockActions decodes the payload of a DeliveryInteractive delivery
// when its type is InteractionTypeBlockActions. It returns false for an
// interaction of another type, for example view_submission, and an error if
// the payload does not decode.
func ParseBlockActions(payload []byte) (*BlockActionsPayload, bool, error) {
	var head struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(payload, &head); err != nil {
		return nil, false, fmt.Errorf("slack: invalid interaction: %w", err)
	}
	if head.Type != InteractionTypeBlockActions {
		return nil, false, nil
	}
	var p BlockActionsPayload
	if err := json.Unmarshal(payload, &p); err != nil {
		return nil, false, fmt.Errorf("slack: invalid block_actions interaction: %w", err)
	}
	return &p, true, nil
}
