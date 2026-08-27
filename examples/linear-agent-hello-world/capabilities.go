package main

// Worked examples for the Linear capability loops: proactive session creation,
// repository suggestions, auth/select elicitations, externalUrls updates, and
// stop handling. The code snippets in docs/how-to/linear-agent-sessions.md are
// extracted from this file; documentation_test.go keeps them in sync.

import (
	"context"
	"strings"

	"github.com/coder/chat"
	"github.com/coder/chat/adapters/linear"
)

// startProactiveSession creates an agent session on an issue the agent was
// neither mentioned on nor delegated (agentSessionCreateOnIssue) — for example
// when work starts from an external trigger such as a failing build — and posts
// a first thought inside the ~10s first-thought window. The returned Thread ID
// works everywhere a webhook-minted one does. CreateSessionOnComment roots the
// session on an existing comment instead; in multi-tenant mode use the
// ForTenant variants with the target organization id.
func startProactiveSession(ctx context.Context, la linearAgentAccess, issueID, dashboardURL string) (chat.ThreadID, error) {
	created, err := la.CreateSessionOnIssue(ctx, linear.CreateSessionOnIssueInput{
		IssueID: issueID, // a UUID or an identifier such as "ENG-123"
		// Seeding externalUrls also keeps the fresh session from being marked
		// unresponsive before the first activity arrives.
		ExternalURLs: []linear.ExternalURL{{URL: dashboardURL, Label: "Run dashboard"}},
	})
	if err != nil {
		return "", err
	}
	if _, err := la.PostThought(ctx, created.ThreadID, "Investigating this issue."); err != nil {
		return "", err
	}
	return created.ThreadID, nil
}

// chooseRepository asks Linear to rank candidate repositories the agent already
// has access to (issueRepositorySuggestions), then either proceeds confidently
// or pairs the low-confidence shortlist with a select elicitation.
func chooseRepository(ctx context.Context, la linearAgentAccess, ev *chat.MessageEvent, candidates []linear.CandidateRepository) error {
	suggestions, err := la.SuggestRepositories(ctx, ev.Thread.ID(), candidates)
	if err != nil {
		return err
	}
	if best := bestSuggestion(suggestions); best != nil && best.Confidence >= 0.8 {
		_, err := la.PostThought(ctx, ev.Thread.ID(), "Working in "+best.RepositoryFullName+".")
		return err
	}
	return offerRepositoryChoice(ctx, la, ev.Thread.ID(), suggestions)
}

// bestSuggestion picks the highest-confidence suggestion, or nil when Linear
// returned none.
func bestSuggestion(suggestions []linear.RepositorySuggestion) *linear.RepositorySuggestion {
	var best *linear.RepositorySuggestion
	for i := range suggestions {
		if best == nil || suggestions[i].Confidence > best.Confidence {
			best = &suggestions[i]
		}
	}
	return best
}

// offerRepositoryChoice turns a suggestion shortlist into a select elicitation.
// The elicitation is a completion signal: the session waits for the user, and
// their choice comes back as a follow-up prompt (see handleSelection).
func offerRepositoryChoice(ctx context.Context, la linearAgentAccess, threadID chat.ThreadID, suggestions []linear.RepositorySuggestion) error {
	if len(suggestions) == 0 {
		_, err := la.PostElicitation(ctx, threadID, linear.ElicitationInput{
			Body: "I couldn't match a repository — which one should I work in?",
		})
		return err
	}
	options := make([]linear.SelectOption, 0, len(suggestions))
	for _, s := range suggestions {
		options = append(options, linear.SelectOption{Value: s.RepositoryFullName, Label: s.RepositoryFullName})
	}
	_, err := la.PostElicitation(ctx, threadID, linear.ElicitationInput{
		Body:           "Which repository should I work in?",
		Signal:         "select",
		SignalMetadata: linear.SelectSignalMetadata{Options: options},
	})
	return err
}

// handleSelection handles the follow-up prompt that answers a select
// elicitation: a chosen option arrives as a regular prompt whose text is the
// option's value. Users may instead reply in free text, so unmatched answers
// return handled=false and fall through to normal prompt handling (ideally an
// LLM interpreting the reply).
func handleSelection(ctx context.Context, ev *chat.MessageEvent, optionValues []string) (bool, error) {
	answer := strings.TrimSpace(ev.Message.Text)
	for _, value := range optionValues {
		if answer == value {
			_, err := ev.Thread.Post(ctx, chat.Markdown("Deploying to **"+value+"** — I'll report back here."))
			return true, err
		}
	}
	return false, nil
}

// requireAccountLink asks the user to link an external account before the agent
// continues. Linear renders an ephemeral "Link account" button from the auth
// signal; the elicitation completes the session pending the user's action.
func requireAccountLink(ctx context.Context, la linearAgentAccess, threadID chat.ThreadID, authURL, linearUserID string) error {
	_, err := la.PostElicitation(ctx, threadID, linear.ElicitationInput{
		Body:   "Please link your account to continue.",
		Signal: "auth",
		SignalMetadata: linear.AuthSignalMetadata{
			URL:          authURL,
			UserID:       linearUserID, // optional: restricts the prompt to one user
			ProviderName: "Example CI",
		},
	})
	return err
}

// resumeAfterAccountLink runs from the application's own auth callback once the
// user finishes linking: Linear sends no webhook for auth completion, so the
// application's stored Thread ID reconstructs the session and a thought resumes
// it (which also dismisses the ephemeral auth UI).
func resumeAfterAccountLink(ctx context.Context, bot *chat.Chat, la linearAgentAccess, threadID chat.ThreadID) error {
	if _, err := la.PostThought(ctx, threadID, "Account linked — resuming."); err != nil {
		return err
	}
	thread, err := bot.Thread(ctx, threadID)
	if err != nil {
		return err
	}
	_, err = thread.Post(ctx, chat.Markdown("All set — the account is linked and the task is done."))
	return err
}

// publishPullRequest surfaces a pull request on the session without replacing
// links that are already there. Linear also treats externalUrls updates as
// session activity, so setting one keeps a fresh session responsive.
func publishPullRequest(ctx context.Context, la linearAgentAccess, threadID chat.ThreadID, prURL string) error {
	return la.UpdateSession(ctx, threadID, linear.AgentSessionUpdateInput{
		AddExternalURLs: []linear.ExternalURL{{URL: prURL, Label: "Pull request"}},
	})
}

// confirmStop detects the human-to-agent stop signal and confirms the halt:
// after disengaging, Linear expects one final response (or error) activity
// confirming the agent's state.
func confirmStop(ctx context.Context, ev *chat.MessageEvent) (bool, error) {
	raw, ok := linear.RawMessageFrom(ev.Message)
	if !ok || !raw.StopRequested() {
		return false, nil
	}
	_, err := ev.Thread.Post(ctx, chat.Text("Stopping as requested — no further changes will be made."))
	return true, err
}
