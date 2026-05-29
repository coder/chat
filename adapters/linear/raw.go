package linear

import (
	"encoding/json"
	"time"

	"github.com/coder/chat"
)

// firstThoughtBudget is the documented ~10s window in which a Linear agent
// session should receive a first Agent Activity Thought (or an externalUrls
// session update) before Linear may mark it unresponsive (ADR 0008). The adapter
// surfaces the deadline; it does not run a watchdog.
const firstThoughtBudget = 10 * time.Second

// stopSignal is the inbound human-to-agent signal asking the agent to halt.
const stopSignal = "stop"

// RawMessage is the stable Linear Platform Escape Hatch attached to
// Message.Raw and Event.Raw for normalized Linear events (ADR 0008, ADR 0013).
// It preserves inbound signals, signal metadata, and structured session context
// verbatim rather than lifting them into the normalized Message / Event fields.
// The full original webhook body is kept in Envelope so nothing is lost.
type RawMessage struct {
	// Kind is the Linear thread kind this event normalized into:
	// "agent_session" (ADR 0008) or "comment" (ADR 0013).
	Kind string
	// Action is the Linear webhook action, e.g. "created" / "prompted" for agent
	// sessions or "create" / "update" for comments.
	Action         string
	OrganizationID string
	// Session is the agent-session context; nil for comment-kind events.
	Session *RawAgentSession
	// Signal is the inbound agentActivity.signal (including the human-to-agent
	// "stop" signal). Empty when absent.
	Signal string
	// SignalMetadata is the inbound signalMetadata, verbatim.
	SignalMetadata json.RawMessage
	// Comment is the source comment sub-object, verbatim, for comment-kind events.
	Comment json.RawMessage
	// Envelope is the full original webhook body, verbatim, as a final escape
	// hatch.
	Envelope json.RawMessage
}

// RawAgentSession carries the structured agent-session context preserved on the
// escape hatch (ADR 0008): prompt context, guidance, previous comments, and the
// verbatim issue and comment sub-objects. FirstThoughtDeadline is set only for
// the "created" action.
type RawAgentSession struct {
	ID                   string
	PromptContext        string
	Guidance             string
	PreviousComments     json.RawMessage
	Issue                json.RawMessage
	Comment              json.RawMessage
	FirstThoughtDeadline *FirstThoughtDeadline
}

// FirstThoughtDeadline surfaces the Agent Session Timing Contract first-thought
// window (ADR 0008): the session-created timestamp plus the ~10s budget. Post a
// first Agent Activity Thought (or set externalUrls via UpdateSession) before
// Deadline, and run anything longer under DispatchDeferred (Ack-Then-Work).
type FirstThoughtDeadline struct {
	SessionCreatedAt time.Time
	Budget           time.Duration
	Deadline         time.Time
}

// StopRequested reports whether the inbound signal is the human-to-agent "stop"
// signal, so a handler can halt work cleanly.
func (r *RawMessage) StopRequested() bool {
	return r != nil && r.Signal == stopSignal
}

// RawMessageFrom extracts the Linear RawMessage from a normalized chat.Message,
// returning false if the message did not originate from this adapter. It is the
// documented Linear-specific accessor for the Platform Escape Hatch.
func RawMessageFrom(m *chat.Message) (*RawMessage, bool) {
	if m == nil {
		return nil, false
	}
	raw, ok := m.Raw.(*RawMessage)
	return raw, ok
}

// newFirstThoughtDeadline parses the session-created timestamp and computes the
// first-thought deadline. It returns nil when the timestamp is absent or
// unparseable so the surface stays best-effort.
func newFirstThoughtDeadline(createdAt string, now func() time.Time) *FirstThoughtDeadline {
	created, ok := parseLinearTime(createdAt)
	if !ok {
		if now == nil {
			return nil
		}
		created = now()
	}
	return &FirstThoughtDeadline{
		SessionCreatedAt: created,
		Budget:           firstThoughtBudget,
		Deadline:         created.Add(firstThoughtBudget),
	}
}

func parseLinearTime(value string) (time.Time, bool) {
	if value == "" {
		return time.Time{}, false
	}
	if t, err := time.Parse(time.RFC3339Nano, value); err == nil {
		return t, true
	}
	if t, err := time.Parse(time.RFC3339, value); err == nil {
		return t, true
	}
	return time.Time{}, false
}
