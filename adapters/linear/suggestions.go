package linear

import (
	"context"
	"errors"

	"github.com/coder/chat"
)

// CandidateRepository is one repository the agent already has access to, offered
// to Linear's issueRepositorySuggestions ranking. Hostname is the Git service
// host (e.g. "github.com"); RepositoryFullName is the owner/name form (e.g.
// "acme/backend").
type CandidateRepository struct {
	Hostname           string `json:"hostname"`
	RepositoryFullName string `json:"repositoryFullName"`
}

// RepositorySuggestion is one ranked repository returned by Linear. Confidence
// is Linear's score from 0.0 to 1.0; Hostname may be empty when Linear does not
// resolve one.
type RepositorySuggestion struct {
	Hostname           string  `json:"hostname"`
	RepositoryFullName string  `json:"repositoryFullName"`
	Confidence         float64 `json:"confidence"`
}

// SuggestRepositories asks Linear to rank the candidate repositories most likely
// to be relevant for the issue behind the given thread, using issue, session,
// guidance, and Linear-internal signals (issueRepositorySuggestions). It accepts
// both Linear thread kinds: on an agent-session thread the session id is passed
// along to sharpen the ranking; on an issue-comment thread only the issue is
// used. A low-confidence result pairs naturally with a "select"-signal
// elicitation offering the user the shortlist.
//
// It is reached through Adapter Access, inherits the adapter's bounded
// rate-limit retry (ADR 0005) and per-tenant token resolution (ADR 0006), and —
// like the rest of the agent surface — wraps a preview Linear API that may
// change upstream.
func (a *Adapter) SuggestRepositories(ctx context.Context, id chat.ThreadID, candidates []CandidateRepository) ([]RepositorySuggestion, error) {
	assertAdapter(a)
	if len(candidates) == 0 {
		return nil, errors.New("linear: at least one candidate repository is required")
	}
	for _, candidate := range candidates {
		if candidate.Hostname == "" || candidate.RepositoryFullName == "" {
			return nil, errors.New("linear: candidate repository hostname and repository full name are required")
		}
	}
	payload, err := a.validateThreadPayload(id)
	if err != nil {
		return nil, err
	}
	variables := map[string]any{
		"issueId":               payload.Issue,
		"candidateRepositories": candidates,
	}
	// agentSessionId is a nullable variable: sent for agent-session threads,
	// omitted for issue-comment threads.
	if payload.Session != "" {
		variables["agentSessionId"] = payload.Session
	}
	var resp graphQLResponse[repositorySuggestionsData]
	if err := a.callGraphQL(ctx, payload.Organization, `query IssueRepositorySuggestions($issueId: String!, $agentSessionId: String, $candidateRepositories: [CandidateRepository!]!) { issueRepositorySuggestions(issueId: $issueId, agentSessionId: $agentSessionId, candidateRepositories: $candidateRepositories) { suggestions { hostname repositoryFullName confidence } } }`, variables, &resp); err != nil {
		return nil, err
	}
	if err := resp.firstError(); err != nil {
		return nil, err
	}
	return resp.Data.IssueRepositorySuggestions.Suggestions, nil
}

type repositorySuggestionsData struct {
	IssueRepositorySuggestions struct {
		Suggestions []RepositorySuggestion `json:"suggestions"`
	} `json:"issueRepositorySuggestions"`
}
