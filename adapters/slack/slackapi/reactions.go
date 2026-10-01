package slackapi

import "context"

// ReactionRequest is the request of reactions.add and reactions.remove.
type ReactionRequest struct {
	// Channel is the ID of the channel of the message.
	Channel string `json:"channel"`
	// Timestamp is the ts of the message.
	Timestamp string `json:"timestamp"`
	// Name is the emoji name without colons, for example "eyes".
	Name string `json:"name"`
}

// AddReaction calls reactions.add. Slack returns the error code
// "already_reacted" when the reaction is already on the message.
func (c *Client) AddReaction(ctx context.Context, req ReactionRequest) error {
	return c.Call(ctx, "reactions.add", req, nil)
}

// RemoveReaction calls reactions.remove. Slack returns the error code
// "no_reaction" when the reaction is not on the message.
func (c *Client) RemoveReaction(ctx context.Context, req ReactionRequest) error {
	return c.Call(ctx, "reactions.remove", req, nil)
}
