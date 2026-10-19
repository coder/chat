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

// Agent session statuses for SetAgentSessionStatusRequest.Status.
const (
	// AgentSessionActive marks an open session where the agent does not work.
	AgentSessionActive = "active"
	// AgentSessionProcessing shows that the agent works, with a Stop button.
	// A click on Stop sends an agent_session_stopped event.
	AgentSessionProcessing = "processing"
)

// SetAgentSessionStatusRequest is the request of agents.sessions.setStatus.
type SetAgentSessionStatusRequest struct {
	// ChannelID is the ID of the channel of the session.
	ChannelID string `json:"channel_id"`
	// ThreadTS is the ts of the parent message of the thread of the session.
	// Leave it empty only for a session channel, which has no thread.
	ThreadTS string `json:"thread_ts,omitempty"`
	// Status is the new status, for example AgentSessionProcessing. Slack also
	// accepts "suspended" and "closed".
	Status string `json:"status"`
}

// SetAgentSessionStatus calls agents.sessions.setStatus. Slack creates the
// session when the thread has none. The app must be a member of the channel.
// In a workspace without the agent feature, Slack returns the error code
// "feature_disabled".
func (c *Client) SetAgentSessionStatus(ctx context.Context, req SetAgentSessionStatusRequest) error {
	return c.Call(ctx, "agents.sessions.setStatus", req, nil)
}
