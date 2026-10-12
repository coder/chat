package slackapi

import "encoding/json"

// Message is a Slack message as it appears in events and in
// conversations.history and conversations.replies responses. The JSON tags
// are the Slack field names.
type Message struct {
	// Type is the object type, usually "message".
	Type string `json:"type,omitempty"`
	// Subtype is the message subtype, for example SubtypeBotMessage. It is
	// empty for a plain user message.
	Subtype string `json:"subtype,omitempty"`
	// User is the ID of the user who sent the message.
	User string `json:"user,omitempty"`
	// BotID is the ID of the bot that sent the message.
	BotID string `json:"bot_id,omitempty"`
	// Username is the name that a bot message shows instead of a user name.
	// It is usually set only on a SubtypeBotMessage message.
	Username string `json:"username,omitempty"`
	// BotProfile is the profile of the app that sent the message. It is set
	// only on a message from an app or a bot.
	BotProfile *BotProfile `json:"bot_profile,omitempty"`
	// Text is the message text in Slack mrkdwn.
	Text string `json:"text,omitempty"`
	// TS is the message timestamp, which is also the message ID in its
	// channel.
	TS string `json:"ts,omitempty"`
	// ThreadTS is the timestamp of the thread root. It is empty for a message
	// that is not in a thread.
	ThreadTS string `json:"thread_ts,omitempty"`
	// Team is the ID of the workspace of the sender.
	Team string `json:"team,omitempty"`
	// UserTeam is the ID of the workspace of the user who sent the message.
	// In a channel that is shared with another organization, it differs from
	// the workspace of the app for a user from the other organization.
	UserTeam string `json:"user_team,omitempty"`
	// SourceTeam is the ID of the workspace where the message was sent.
	SourceTeam string `json:"source_team,omitempty"`
	// Files are the files attached to the message.
	Files []File `json:"files,omitempty"`
	// Edited is set when the message was edited.
	Edited *Edited `json:"edited,omitempty"`
	// Raw is the message object exactly as Slack sent it, including the
	// fields that Message does not decode. ConversationHistory and
	// ConversationReplies set it. For an event, Envelope.Event holds the raw
	// JSON instead.
	Raw json.RawMessage `json:"-"`
}

// BotProfile is the profile of the app that sent a Message.
type BotProfile struct {
	// ID is the bot ID, the same as Message.BotID.
	ID string `json:"id,omitempty"`
	// Name is the name of the app.
	Name string `json:"name,omitempty"`
}

// Edited records the last edit of a Message.
type Edited struct {
	// User is the ID of the user who edited the message.
	User string `json:"user,omitempty"`
	// TS is the timestamp of the edit.
	TS string `json:"ts,omitempty"`
}

// File is a Slack file attached to a Message.
type File struct {
	// ID is the file ID.
	ID string `json:"id,omitempty"`
	// Name is the file name.
	Name string `json:"name,omitempty"`
	// Title is the file title.
	Title string `json:"title,omitempty"`
	// MIMEType is the MIME type of the file.
	MIMEType string `json:"mimetype,omitempty"`
	// FileType is the Slack file type, for example "text" or "png".
	FileType string `json:"filetype,omitempty"`
	// Size is the file size in bytes.
	Size int64 `json:"size,omitzero"`
	// URLPrivateDownload is the authenticated download URL of the file.
	URLPrivateDownload string `json:"url_private_download,omitempty"`
}
