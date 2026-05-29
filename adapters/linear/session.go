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
