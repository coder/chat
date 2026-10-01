package slackapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
)

// Conversation is a Slack channel, private channel, DM, or group DM as
// conversations.info returns it. The JSON tags are the Slack field names.
type Conversation struct {
	// ID is the conversation ID.
	ID string `json:"id,omitempty"`
	// Name is the channel name without the leading "#". It is empty for a DM.
	Name string `json:"name,omitempty"`
	// IsChannel is true for a public channel.
	IsChannel bool `json:"is_channel,omitzero"`
	// IsGroup is true for a private channel.
	IsGroup bool `json:"is_group,omitzero"`
	// IsIM is true for a DM.
	IsIM bool `json:"is_im,omitzero"`
	// IsMPIM is true for a group DM.
	IsMPIM bool `json:"is_mpim,omitzero"`
	// IsPrivate is true for a private channel or a group DM.
	IsPrivate bool `json:"is_private,omitzero"`
	// IsArchived is true when the conversation is archived.
	IsArchived bool `json:"is_archived,omitzero"`
	// IsShared is true when the conversation is shared with another workspace.
	IsShared bool `json:"is_shared,omitzero"`
	// IsExtShared is true when the conversation is shared with another
	// organization.
	IsExtShared bool `json:"is_ext_shared,omitzero"`
	// IsOrgShared is true when the conversation is shared with other
	// workspaces of the same Enterprise Grid organization.
	IsOrgShared bool `json:"is_org_shared,omitzero"`
	// IsPendingExtShared is true when an external share of the conversation
	// waits for approval.
	IsPendingExtShared bool `json:"is_pending_ext_shared,omitzero"`
	// IsMember is true when the user of the token is a member of the
	// conversation.
	IsMember bool `json:"is_member,omitzero"`
}

// ConversationInfoRequest is the request of conversations.info.
type ConversationInfoRequest struct {
	// Channel is the conversation ID.
	Channel string `json:"channel"`
}

// ConversationInfo calls conversations.info and returns the conversation. It
// sends the request form encoded. Slack returns the error code
// "channel_not_found" for an unknown conversation or one that the token
// cannot see.
func (c *Client) ConversationInfo(ctx context.Context, req ConversationInfoRequest) (*Conversation, error) {
	values := url.Values{}
	setFormString(values, "channel", req.Channel)
	var resp struct {
		Channel Conversation `json:"channel"`
	}
	if err := c.Call(ctx, "conversations.info", values, &resp); err != nil {
		return nil, err
	}
	return &resp.Channel, nil
}

// ConversationHistoryRequest is the request of conversations.history.
type ConversationHistoryRequest struct {
	// Channel is the conversation ID.
	Channel string `json:"channel"`
	// Oldest is the ts of the start of the time range. Empty means no start.
	Oldest string `json:"oldest,omitempty"`
	// Latest is the ts of the end of the time range. Empty means now.
	Latest string `json:"latest,omitempty"`
	// Inclusive includes the messages at Oldest and Latest in the page.
	Inclusive bool `json:"inclusive,omitzero"`
	// Limit is the maximum number of messages in the page. Zero uses the
	// Slack default.
	Limit int `json:"limit,omitzero"`
	// Cursor is the NextCursor of the previous page. Empty requests the first
	// page.
	Cursor string `json:"cursor,omitempty"`
}

// ConversationRepliesRequest is the request of conversations.replies.
type ConversationRepliesRequest struct {
	// Channel is the conversation ID.
	Channel string `json:"channel"`
	// TS is the ts of the parent message of the thread.
	TS string `json:"ts"`
	// Oldest is the ts of the start of the time range. Empty means no start.
	Oldest string `json:"oldest,omitempty"`
	// Latest is the ts of the end of the time range. Empty means now.
	Latest string `json:"latest,omitempty"`
	// Inclusive includes the messages at Oldest and Latest in the page.
	Inclusive bool `json:"inclusive,omitzero"`
	// Limit is the maximum number of messages in the page. Zero uses the
	// Slack default.
	Limit int `json:"limit,omitzero"`
	// Cursor is the NextCursor of the previous page. Empty requests the first
	// page.
	Cursor string `json:"cursor,omitempty"`
}

// MessagePage is one page of messages from conversations.history or
// conversations.replies.
type MessagePage struct {
	// Messages are the messages of the page.
	Messages []Message
	// HasMore is true when Slack has more messages after this page.
	HasMore bool
	// NextCursor is the cursor of the next page. It is empty on the last page.
	NextCursor string
}

// ConversationHistory calls conversations.history and returns one page of the
// messages in the conversation, newest first. To read the next page, send the
// request again with Cursor set to NextCursor. It sends the request form
// encoded.
func (c *Client) ConversationHistory(ctx context.Context, req ConversationHistoryRequest) (*MessagePage, error) {
	values := url.Values{}
	setFormString(values, "channel", req.Channel)
	setFormPage(values, req.Oldest, req.Latest, req.Inclusive, req.Limit, req.Cursor)
	return c.messagePage(ctx, "conversations.history", values)
}

// ConversationReplies calls conversations.replies and returns one page of the
// messages in the thread, oldest first. To read the next page, send the
// request again with Cursor set to NextCursor. It sends the request form
// encoded. Slack returns the error code "thread_not_found" when TS is not a
// message in the conversation.
func (c *Client) ConversationReplies(ctx context.Context, req ConversationRepliesRequest) (*MessagePage, error) {
	values := url.Values{}
	setFormString(values, "channel", req.Channel)
	setFormString(values, "ts", req.TS)
	setFormPage(values, req.Oldest, req.Latest, req.Inclusive, req.Limit, req.Cursor)
	return c.messagePage(ctx, "conversations.replies", values)
}

// setFormPage sets the time range and pagination fields that
// conversations.history and conversations.replies share.
func setFormPage(values url.Values, oldest, latest string, inclusive bool, limit int, cursor string) {
	setFormString(values, "oldest", oldest)
	setFormString(values, "latest", latest)
	setFormBool(values, "inclusive", inclusive)
	setFormInt(values, "limit", limit)
	setFormString(values, "cursor", cursor)
}

// messagePage calls a paginated message method and returns the page, with
// Message.Raw set on each message.
func (c *Client) messagePage(ctx context.Context, method string, values url.Values) (*MessagePage, error) {
	var resp struct {
		Messages         []json.RawMessage `json:"messages"`
		HasMore          bool              `json:"has_more"`
		ResponseMetadata struct {
			NextCursor string `json:"next_cursor"`
		} `json:"response_metadata"`
	}
	if err := c.Call(ctx, method, values, &resp); err != nil {
		return nil, err
	}
	messages := make([]Message, len(resp.Messages))
	for i, raw := range resp.Messages {
		if err := json.Unmarshal(raw, &messages[i]); err != nil {
			return nil, fmt.Errorf("slack: decode %s response: %w", method, err)
		}
		messages[i].Raw = raw
	}
	return &MessagePage{
		Messages:   messages,
		HasMore:    resp.HasMore,
		NextCursor: resp.ResponseMetadata.NextCursor,
	}, nil
}
