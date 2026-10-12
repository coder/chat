package slackapi

import "context"

// SetAssistantThreadStatusRequest is the request of
// assistant.threads.setStatus.
type SetAssistantThreadStatusRequest struct {
	// ChannelID is the ID of the channel of the thread.
	ChannelID string `json:"channel_id"`
	// ThreadTS is the ts of the parent message of the thread.
	ThreadTS string `json:"thread_ts"`
	// Status is the status text, for example "is thinking...". It is always
	// sent. An empty Status clears the status.
	Status string `json:"status"`
	// LoadingMessages are optional messages that Slack rotates while the
	// status shows. Slack accepts at most 10.
	LoadingMessages []string `json:"loading_messages,omitempty"`
}

// SetAssistantThreadStatus calls assistant.threads.setStatus. Slack clears the
// status when the app posts a message in the thread, and after about 2 minutes
// when the app posts no message.
func (c *Client) SetAssistantThreadStatus(ctx context.Context, req SetAssistantThreadStatusRequest) error {
	return c.Call(ctx, "assistant.threads.setStatus", req, nil)
}
