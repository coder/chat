package slackapi

import "context"

// PostMessageRequest is the request of chat.postMessage.
type PostMessageRequest struct {
	// Channel is the ID of the channel, DM, or group DM.
	Channel string `json:"channel"`
	// ThreadTS posts the message as a reply in a thread. It must be the ts of
	// the parent message, not the ts of a reply.
	ThreadTS string `json:"thread_ts,omitempty"`
	// Text is the message text in Slack mrkdwn. With Blocks, Slack uses it as
	// the fallback text for notifications.
	Text string `json:"text,omitempty"`
	// Blocks are the Block Kit blocks of the message.
	Blocks []Block `json:"blocks,omitempty"`
	// MarkdownText is the message text in standard Markdown. It cannot be
	// combined with Text or Blocks and holds at most 12,000 characters.
	MarkdownText string `json:"markdown_text,omitempty"`
	// Mrkdwn set to false turns off Slack mrkdwn parsing of Text. Nil uses
	// the Slack default, which parses it.
	Mrkdwn *bool `json:"mrkdwn,omitempty"`
	// UnfurlLinks turns the unfurl of text links on or off. Nil uses the
	// Slack default.
	UnfurlLinks *bool `json:"unfurl_links,omitempty"`
	// UnfurlMedia turns the unfurl of media links on or off. Nil uses the
	// Slack default.
	UnfurlMedia *bool `json:"unfurl_media,omitempty"`
}

// PostMessageResponse is the response of chat.postMessage.
type PostMessageResponse struct {
	// Channel is the ID of the channel of the message.
	Channel string `json:"channel"`
	// TS is the ts of the new message.
	TS string `json:"ts"`
	// Message is the new message as Slack stored it.
	Message *Message `json:"message"`
}

// PostMessage calls chat.postMessage. Set Text, Blocks, or MarkdownText. Slack
// rejects MarkdownText together with Text or Blocks with the error code
// "markdown_text_conflict".
func (c *Client) PostMessage(ctx context.Context, req PostMessageRequest) (*PostMessageResponse, error) {
	var resp PostMessageResponse
	if err := c.Call(ctx, "chat.postMessage", req, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

// PostEphemeralRequest is the request of chat.postEphemeral.
type PostEphemeralRequest struct {
	// Channel is the ID of the channel, DM, or group DM.
	Channel string `json:"channel"`
	// User is the ID of the only user who sees the message. The user must be
	// a member of Channel.
	User string `json:"user"`
	// ThreadTS posts the message in a thread. It must be the ts of the parent
	// message, not the ts of a reply.
	ThreadTS string `json:"thread_ts,omitempty"`
	// Text is the message text in Slack mrkdwn. With Blocks, Slack uses it as
	// the fallback text for notifications.
	Text string `json:"text,omitempty"`
	// Blocks are the Block Kit blocks of the message.
	Blocks []Block `json:"blocks,omitempty"`
	// MarkdownText is the message text in standard Markdown. It cannot be
	// combined with Text or Blocks and holds at most 12,000 characters.
	MarkdownText string `json:"markdown_text,omitempty"`
}

// PostEphemeralResponse is the response of chat.postEphemeral.
type PostEphemeralResponse struct {
	// MessageTS is the ts of the message. Slack does not store an ephemeral
	// message, so the ts cannot update or delete it.
	MessageTS string `json:"message_ts"`
}

// PostEphemeral calls chat.postEphemeral to post a message that only one user
// sees. Slack returns the error code "user_not_in_channel" when the user is
// not a member of the channel. Set Text, Blocks, or MarkdownText, as for
// PostMessage.
func (c *Client) PostEphemeral(ctx context.Context, req PostEphemeralRequest) (*PostEphemeralResponse, error) {
	var resp PostEphemeralResponse
	if err := c.Call(ctx, "chat.postEphemeral", req, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

// UpdateMessageRequest is the request of chat.update.
type UpdateMessageRequest struct {
	// Channel is the ID of the channel of the message.
	Channel string `json:"channel"`
	// TS is the ts of the message to update.
	TS string `json:"ts"`
	// Text is the new message text in Slack mrkdwn.
	Text string `json:"text,omitempty"`
	// Blocks are the new Block Kit blocks of the message.
	Blocks []Block `json:"blocks,omitempty"`
	// MarkdownText is the new message text in standard Markdown. It cannot be
	// combined with Text or Blocks and holds at most 12,000 characters.
	MarkdownText string `json:"markdown_text,omitempty"`
}

// UpdateMessageResponse is the response of chat.update.
type UpdateMessageResponse struct {
	// Channel is the ID of the channel of the message.
	Channel string `json:"channel"`
	// TS is the ts of the updated message.
	TS string `json:"ts"`
	// Text is the new message text.
	Text string `json:"text"`
}

// UpdateMessage calls chat.update. An update with Text and no Blocks removes
// the existing blocks of the message. A bot token can update only the messages
// of the bot.
func (c *Client) UpdateMessage(ctx context.Context, req UpdateMessageRequest) (*UpdateMessageResponse, error) {
	var resp UpdateMessageResponse
	if err := c.Call(ctx, "chat.update", req, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

// DeleteMessageRequest is the request of chat.delete.
type DeleteMessageRequest struct {
	// Channel is the ID of the channel of the message.
	Channel string `json:"channel"`
	// TS is the ts of the message to delete.
	TS string `json:"ts"`
}

// DeleteMessage calls chat.delete. A bot token can delete only the messages of
// the bot.
func (c *Client) DeleteMessage(ctx context.Context, req DeleteMessageRequest) error {
	return c.Call(ctx, "chat.delete", req, nil)
}
