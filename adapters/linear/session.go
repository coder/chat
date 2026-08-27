package linear

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/coder/chat"
)

// GraphQL is a deliberate low-level escape hatch for calling preview Linear APIs
// that do not yet have typed wrappers (ADR 0008). It reuses the App-Actor Client
// Credentials token-refresh path, API base URL, HTTP client, and bounded
// rate-limit retry. It surfaces GraphQL errors as a returned error and unmarshals
// the response data into dest. It never exposes or returns the access token: the
// token lives only in the Authorization header internally.
//
// Reach it through Adapter Access. It is Linear-specific and not part of the
// portable runtime surface.
func (a *Adapter) GraphQL(ctx context.Context, query string, variables any, dest any) error {
	assertAdapter(a)
	if a.multiTenant() {
		return errors.New("linear: GraphQL requires a tenant; use GraphQLForTenant in multi-tenant mode")
	}
	return a.GraphQLForTenant(ctx, "", query, variables, dest)
}

// GraphQLForTenant is the multi-tenant form of GraphQL: it resolves the per-org
// access token for the given Platform Tenant (the organizationId baked into a
// Thread ID) before issuing the query. Single-install callers use GraphQL.
func (a *Adapter) GraphQLForTenant(ctx context.Context, tenant string, query string, variables any, dest any) error {
	assertAdapter(a)
	if query == "" {
		return errors.New("linear: graphql query is required")
	}
	var envelope struct {
		Data   json.RawMessage `json:"data"`
		Errors []graphQLError  `json:"errors"`
	}
	if err := a.callGraphQL(ctx, tenant, query, variables, &envelope); err != nil {
		return err
	}
	if len(envelope.Errors) > 0 {
		return errors.New("linear: graphql error: " + envelope.Errors[0].Message)
	}
	if dest == nil {
		return nil
	}
	if len(envelope.Data) == 0 {
		return nil
	}
	if err := json.Unmarshal(envelope.Data, dest); err != nil {
		return fmt.Errorf("linear: decode graphql data: %w", err)
	}
	return nil
}

// ExternalURL is a link surfaced on the agent session, e.g. a pull request.
type ExternalURL struct {
	URL   string `json:"url"`
	Label string `json:"label,omitempty"`
}

// PlanStep is one step of the agent session plan. Plan updates replace the whole
// plan array.
type PlanStep struct {
	ID     string `json:"id,omitempty"`
	Title  string `json:"title"`
	Status string `json:"status,omitempty"`
}

// AgentSessionUpdateInput drives the agentSessionUpdate escape hatch (ADR 0008).
// Only the fields the caller sets are sent. Setting ExternalURLs replaces the
// whole list; AddExternalURLs / RemoveExternalURLs adjust it incrementally.
// Plan updates replace the whole plan array; set ReplacePlan to send an empty
// plan rather than leaving it unchanged.
//
// Setting externalUrls can keep a new session from being marked unresponsive
// within the Agent Session Timing Contract window.
type AgentSessionUpdateInput struct {
	ExternalURLs       []ExternalURL
	AddExternalURLs    []ExternalURL
	RemoveExternalURLs []string
	Plan               []PlanStep
	ReplacePlan        bool
}

func (in AgentSessionUpdateInput) empty() bool {
	return in.ExternalURLs == nil &&
		len(in.AddExternalURLs) == 0 &&
		len(in.RemoveExternalURLs) == 0 &&
		in.Plan == nil &&
		!in.ReplacePlan
}

// UpdateSession updates an agent session's external URLs and/or plan (ADR 0008).
// It is agent-session-only and reached through Adapter Access.
func (a *Adapter) UpdateSession(ctx context.Context, id chat.ThreadID, in AgentSessionUpdateInput) error {
	assertAdapter(a)
	if in.empty() {
		return errors.New("linear: session update has no fields set")
	}
	payload, err := a.agentSessionPayload(id)
	if err != nil {
		return err
	}
	input := map[string]any{}
	if in.ExternalURLs != nil {
		input["externalUrls"] = in.ExternalURLs
	}
	if len(in.AddExternalURLs) > 0 {
		input["addExternalUrls"] = in.AddExternalURLs
	}
	if len(in.RemoveExternalURLs) > 0 {
		input["removeExternalUrls"] = in.RemoveExternalURLs
	}
	if in.Plan != nil || in.ReplacePlan {
		plan := in.Plan
		if plan == nil {
			plan = []PlanStep{}
		}
		input["plan"] = plan
	}
	variables := map[string]any{
		"id":    payload.Session,
		"input": input,
	}
	var resp graphQLResponse[agentSessionUpdateData]
	if err := a.callGraphQL(ctx, payload.Organization, `mutation AgentSessionUpdate($id: String!, $input: AgentSessionUpdateInput!) { agentSessionUpdate(id: $id, input: $input) { success } }`, variables, &resp); err != nil {
		return err
	}
	if err := resp.firstError(); err != nil {
		return err
	}
	if !resp.Data.AgentSessionUpdate.Success {
		return errors.New("linear: failed to update agent session")
	}
	return nil
}

type agentSessionUpdateData struct {
	AgentSessionUpdate struct {
		Success bool `json:"success"`
	} `json:"agentSessionUpdate"`
}

// CreateSessionOnIssueInput drives the agentSessionCreateOnIssue mutation:
// proactively creating an agent session on an issue the agent was not mentioned
// on or delegated. Linear's Agent API is in developer preview upstream and this
// shape may change with it.
type CreateSessionOnIssueInput struct {
	// IssueID is the target issue: a UUID or an issue identifier such as
	// "ENG-123".
	IssueID string
	// ExternalURLs optionally seeds the session's external links at creation,
	// which also keeps the new session from being marked unresponsive within the
	// Agent Session Timing Contract window (ADR 0008).
	ExternalURLs []ExternalURL
}

// CreateSessionOnCommentInput drives the agentSessionCreateOnComment mutation:
// proactively creating an agent session rooted on an existing issue comment.
type CreateSessionOnCommentInput struct {
	// CommentID is the root comment the session will be associated with.
	CommentID string
	// ExternalURLs optionally seeds the session's external links at creation.
	ExternalURLs []ExternalURL
}

// CreatedAgentSession identifies a proactively created Linear agent session.
type CreatedAgentSession struct {
	// ThreadID is the adapter's opaque agent-session Thread ID for the new
	// session. It works everywhere a webhook-minted Thread ID does: Thread Handle
	// reconstruction via chat.Chat.Thread, Thread.Post, PostThought, the other
	// activity helpers, and UpdateSession.
	ThreadID chat.ThreadID
	// SessionID, IssueID, and CommentID are the raw Linear identifiers of the
	// created session, its issue, and its root comment (empty when the session
	// was created on an issue without a root comment).
	SessionID string
	IssueID   string
	CommentID string
}

// CreateSessionOnIssue proactively creates an agent session on an issue when the
// agent was not mentioned or delegated. It is reached through Adapter Access and
// requires the single-install identity discovered at Init; multi-tenant callers
// use CreateSessionOnIssueForTenant.
func (a *Adapter) CreateSessionOnIssue(ctx context.Context, in CreateSessionOnIssueInput) (*CreatedAgentSession, error) {
	assertAdapter(a)
	if a.multiTenant() {
		return nil, errors.New("linear: CreateSessionOnIssue requires a tenant; use CreateSessionOnIssueForTenant in multi-tenant mode")
	}
	return a.createSessionOnIssue(ctx, a.BotActor().Tenant, in)
}

// CreateSessionOnIssueForTenant is the multi-tenant form of CreateSessionOnIssue:
// it resolves the per-org access token for the given Platform Tenant (the Linear
// organizationId) and mints the returned Thread ID for that tenant.
func (a *Adapter) CreateSessionOnIssueForTenant(ctx context.Context, tenant string, in CreateSessionOnIssueInput) (*CreatedAgentSession, error) {
	assertAdapter(a)
	if err := a.validateProactiveTenant(tenant); err != nil {
		return nil, err
	}
	return a.createSessionOnIssue(ctx, tenant, in)
}

// CreateSessionOnComment proactively creates an agent session rooted on an
// existing issue comment. It is reached through Adapter Access and requires the
// single-install identity discovered at Init; multi-tenant callers use
// CreateSessionOnCommentForTenant.
func (a *Adapter) CreateSessionOnComment(ctx context.Context, in CreateSessionOnCommentInput) (*CreatedAgentSession, error) {
	assertAdapter(a)
	if a.multiTenant() {
		return nil, errors.New("linear: CreateSessionOnComment requires a tenant; use CreateSessionOnCommentForTenant in multi-tenant mode")
	}
	return a.createSessionOnComment(ctx, a.BotActor().Tenant, in)
}

// CreateSessionOnCommentForTenant is the multi-tenant form of
// CreateSessionOnComment.
func (a *Adapter) CreateSessionOnCommentForTenant(ctx context.Context, tenant string, in CreateSessionOnCommentInput) (*CreatedAgentSession, error) {
	assertAdapter(a)
	if err := a.validateProactiveTenant(tenant); err != nil {
		return nil, err
	}
	return a.createSessionOnComment(ctx, tenant, in)
}

// validateProactiveTenant guards proactive session creation, which mints Thread
// IDs for a tenant instead of validating an existing one: the tenant is required
// (it becomes the Thread ID's organization), and in single-install mode it must
// match the initialized organization so the minted Thread ID stays usable.
func (a *Adapter) validateProactiveTenant(tenant string) error {
	if tenant == "" {
		return errors.New("linear: tenant is required")
	}
	if !a.multiTenant() {
		bot := a.BotActor()
		if bot.Tenant != "" && tenant != bot.Tenant {
			return fmt.Errorf("linear: tenant %q does not match initialized organization", tenant)
		}
	}
	return nil
}

func (a *Adapter) createSessionOnIssue(ctx context.Context, tenant string, in CreateSessionOnIssueInput) (*CreatedAgentSession, error) {
	if in.IssueID == "" {
		return nil, errors.New("linear: issue id is required")
	}
	input := map[string]any{"issueId": in.IssueID}
	if len(in.ExternalURLs) > 0 {
		input["externalUrls"] = in.ExternalURLs
	}
	var resp graphQLResponse[agentSessionCreateOnIssueData]
	if err := a.callGraphQL(ctx, tenant, `mutation AgentSessionCreateOnIssue($input: AgentSessionCreateOnIssue!) { agentSessionCreateOnIssue(input: $input) { success agentSession { id issue { id } comment { id } } } }`, map[string]any{"input": input}, &resp); err != nil {
		return nil, err
	}
	if err := resp.firstError(); err != nil {
		return nil, err
	}
	return createdSessionFromPayload(tenant, resp.Data.AgentSessionCreateOnIssue)
}

func (a *Adapter) createSessionOnComment(ctx context.Context, tenant string, in CreateSessionOnCommentInput) (*CreatedAgentSession, error) {
	if in.CommentID == "" {
		return nil, errors.New("linear: comment id is required")
	}
	input := map[string]any{"commentId": in.CommentID}
	if len(in.ExternalURLs) > 0 {
		input["externalUrls"] = in.ExternalURLs
	}
	var resp graphQLResponse[agentSessionCreateOnCommentData]
	if err := a.callGraphQL(ctx, tenant, `mutation AgentSessionCreateOnComment($input: AgentSessionCreateOnComment!) { agentSessionCreateOnComment(input: $input) { success agentSession { id issue { id } comment { id } } } }`, map[string]any{"input": input}, &resp); err != nil {
		return nil, err
	}
	if err := resp.firstError(); err != nil {
		return nil, err
	}
	return createdSessionFromPayload(tenant, resp.Data.AgentSessionCreateOnComment)
}

// createdSessionFromPayload converts an agentSessionCreateOn* payload into a
// CreatedAgentSession, minting the opaque agent-session Thread ID from the
// canonical identifiers Linear returned (not the caller's input, which may be an
// issue identifier alias such as "ENG-123").
func createdSessionFromPayload(tenant string, payload createdSessionPayload) (*CreatedAgentSession, error) {
	session := payload.AgentSession
	if !payload.Success || session.ID == "" {
		return nil, errors.New("linear: failed to create agent session")
	}
	if session.Issue.ID == "" {
		return nil, errors.New("linear: created agent session did not return an issue")
	}
	threadID, err := encodeThreadID(threadPayload{
		Organization: tenant,
		Issue:        session.Issue.ID,
		Comment:      session.Comment.ID,
		Session:      session.ID,
		Kind:         threadKindAgentSession,
	})
	if err != nil {
		return nil, err
	}
	return &CreatedAgentSession{
		ThreadID:  threadID,
		SessionID: session.ID,
		IssueID:   session.Issue.ID,
		CommentID: session.Comment.ID,
	}, nil
}

type agentSessionCreateOnIssueData struct {
	AgentSessionCreateOnIssue createdSessionPayload `json:"agentSessionCreateOnIssue"`
}

type agentSessionCreateOnCommentData struct {
	AgentSessionCreateOnComment createdSessionPayload `json:"agentSessionCreateOnComment"`
}

type createdSessionPayload struct {
	Success      bool `json:"success"`
	AgentSession struct {
		ID      string  `json:"id"`
		Issue   nodeRef `json:"issue"`
		Comment nodeRef `json:"comment"`
	} `json:"agentSession"`
}

// nodeRef is a minimal Supported Platform Shape for a referenced Linear node: a
// nullable object read only for its id (JSON null decodes to the zero value).
type nodeRef struct {
	ID string `json:"id"`
}
