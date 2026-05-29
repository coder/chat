package linear

import (
	"context"
	"errors"
	"strings"

	"github.com/coder/chat"
)

// AgentActivityInput is the generic agent-activity escape hatch (ADR 0008). It
// carries a server-validated content shape, an optional signal and signal
// metadata, and the ephemeral flag. Reach it through Adapter Access; it is
// Linear-specific and not part of the portable runtime surface.
//
// Content must include a "type" key set to one of "thought", "response",
// "action", "elicitation", or "error", plus the body fields Linear expects for
// that type. Ephemeral is only valid for "thought" and "action"; any other type
// with Ephemeral set is rejected.
type AgentActivityInput struct {
	Content        map[string]any
	Signal         string
	SignalMetadata any
	Ephemeral      bool
}

// CreateAgentActivity posts an arbitrary agent activity on an agent-session
// thread. It is the generic escape hatch the typed helpers (PostAction,
// PostElicitation, PostError), PostThought, and the Thread.Post response path
// all funnel through. It rejects non-agent-session threads and enforces
// Linear's ephemeral-only-on-thought/action rule.
func (a *Adapter) CreateAgentActivity(ctx context.Context, id chat.ThreadID, in AgentActivityInput) (*chat.SentMessage, error) {
	assertAdapter(a)
	if len(in.Content) == 0 {
		return nil, errors.New("linear: agent activity content is required")
	}
	payload, err := a.agentSessionPayload(id)
	if err != nil {
		return nil, err
	}
	return a.createAgentActivity(ctx, payload, activityRequest{
		content:        in.Content,
		signal:         in.Signal,
		signalMetadata: in.SignalMetadata,
		ephemeral:      in.Ephemeral,
	})
}

// ActionInput describes a native Linear tool-call action activity. Actions may
// be ephemeral.
type ActionInput struct {
	Action    string
	Parameter string
	Result    string
	Ephemeral bool
}

// PostAction posts an Agent Activity Action describing native tool-call progress
// in the agent session UI.
func (a *Adapter) PostAction(ctx context.Context, id chat.ThreadID, in ActionInput) (*chat.SentMessage, error) {
	assertAdapter(a)
	if strings.TrimSpace(in.Action) == "" {
		return nil, errors.New("linear: action is required")
	}
	content := map[string]any{"type": activityTypeAction, "action": in.Action}
	if in.Parameter != "" {
		content["parameter"] = in.Parameter
	}
	if in.Result != "" {
		content["result"] = in.Result
	}
	payload, err := a.agentSessionPayload(id)
	if err != nil {
		return nil, err
	}
	return a.createAgentActivity(ctx, payload, activityRequest{content: content, ephemeral: in.Ephemeral})
}

// ElicitationInput describes a question the agent asks the user. An elicitation
// is a session-completion signal and is never ephemeral. Signal selects an
// optional native Linear elicitation flow ("auth", "select"); SignalMetadata
// carries its shape, e.g. AuthSignalMetadata or SelectSignalMetadata.
type ElicitationInput struct {
	Body           string
	Signal         string
	SignalMetadata any
}

// PostElicitation posts an Agent Activity Elicitation, asking the user a
// question and completing the session pending their answer. Elicitations are
// never ephemeral.
func (a *Adapter) PostElicitation(ctx context.Context, id chat.ThreadID, in ElicitationInput) (*chat.SentMessage, error) {
	assertAdapter(a)
	if strings.TrimSpace(in.Body) == "" {
		return nil, errors.New("linear: elicitation body is required")
	}
	payload, err := a.agentSessionPayload(id)
	if err != nil {
		return nil, err
	}
	return a.createAgentActivity(ctx, payload, activityRequest{
		content:        map[string]any{"type": activityTypeElicitation, "body": in.Body},
		signal:         in.Signal,
		signalMetadata: in.SignalMetadata,
		ephemeral:      false,
	})
}

// ErrorInput describes a failure that ends the session cleanly.
type ErrorInput struct {
	Body string
}

// PostError posts an Agent Activity Error, ending a failed session with an
// explicit completion signal instead of leaving it hanging. Errors are never
// ephemeral.
func (a *Adapter) PostError(ctx context.Context, id chat.ThreadID, in ErrorInput) (*chat.SentMessage, error) {
	assertAdapter(a)
	if strings.TrimSpace(in.Body) == "" {
		return nil, errors.New("linear: error body is required")
	}
	payload, err := a.agentSessionPayload(id)
	if err != nil {
		return nil, err
	}
	return a.createAgentActivity(ctx, payload, activityRequest{
		content:   map[string]any{"type": activityTypeError, "body": in.Body},
		ephemeral: false,
	})
}

// AuthSignalMetadata is the metadata shape for an "auth"-signal elicitation,
// prompting the user to link an external account. It is serialized as
// signalMetadata. The shape follows Linear's preview agent API and is documented
// as escape-hatch-adjacent.
type AuthSignalMetadata struct {
	URL          string `json:"url"`
	UserID       string `json:"userId,omitempty"`
	ProviderName string `json:"providerName,omitempty"`
}

// SelectSignalMetadata is the metadata shape for a "select"-signal elicitation,
// offering the user a choice. It is serialized as signalMetadata.
type SelectSignalMetadata struct {
	Options []SelectOption `json:"options"`
}

// SelectOption is one choice in a SelectSignalMetadata.
type SelectOption struct {
	Value string `json:"value"`
	Label string `json:"label,omitempty"`
}
