package linear_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/coder/chat"
	"github.com/coder/chat/adapters/linear"
)

// TestCreateSessionOnIssueMintsPostableThread covers the #47 acceptance: the
// typed helper returns a session convertible into the adapter's opaque Thread
// ID, and the created session can be posted to with Thread.Post and PostThought.
func TestCreateSessionOnIssueMintsPostableThread(t *testing.T) {
	t.Parallel()

	api := newLinearAPIServer(t, 3600)
	now := time.UnixMilli(1_700_000_000_000)
	bot, adapter := newLinearRuntime(t, api, linear.Options{WebhookSecret: "whsec", Now: func() time.Time { return now }})

	created, err := adapter.CreateSessionOnIssue(context.Background(), linear.CreateSessionOnIssueInput{
		IssueID:      "ENG-123",
		ExternalURLs: []linear.ExternalURL{{URL: "https://example.com/run/1", Label: "Dashboard"}},
	})
	if err != nil {
		t.Fatalf("create session on issue: %v", err)
	}
	if created.SessionID != "SNEW1" || created.IssueID != "ISSUE7" || created.CommentID != "" {
		t.Fatalf("created = %#v", created)
	}

	input := api.lastSessionCreate(t)
	if input["issueId"] != "ENG-123" {
		t.Fatalf("issueId = %v", input["issueId"])
	}
	urls, ok := input["externalUrls"].([]any)
	if !ok || len(urls) != 1 {
		t.Fatalf("externalUrls = %#v", input["externalUrls"])
	}
	url, _ := urls[0].(map[string]any)
	if url["url"] != "https://example.com/run/1" || url["label"] != "Dashboard" {
		t.Fatalf("externalUrls[0] = %#v", urls[0])
	}

	// The Thread ID is minted from the canonical identifiers Linear returned and
	// validates like any webhook-minted agent-session thread.
	ref, err := adapter.ValidateThreadID(created.ThreadID)
	if err != nil {
		t.Fatalf("validate thread id: %v", err)
	}
	if ref.Tenant != "ORG1" || ref.Channel != "ISSUE7" || ref.Root != "SNEW1" {
		t.Fatalf("thread ref = %#v", ref)
	}

	// Thread Handle reconstruction + Thread.Post creates the response activity on
	// the new session.
	thread, err := bot.Thread(context.Background(), created.ThreadID)
	if err != nil {
		t.Fatalf("thread: %v", err)
	}
	if _, err := thread.Post(context.Background(), chat.Markdown("**update**")); err != nil {
		t.Fatalf("post: %v", err)
	}
	api.assertActivity(t, 0, linearActivity{
		AgentSessionID: "SNEW1",
		Content:        activityContent{Type: "response", Body: "**update**"},
	})

	// PostThought targets the same created session.
	if _, err := adapter.PostThought(context.Background(), created.ThreadID, "Working..."); err != nil {
		t.Fatalf("post thought: %v", err)
	}
	api.assertActivity(t, 1, linearActivity{
		AgentSessionID: "SNEW1",
		Ephemeral:      true,
		Content:        activityContent{Type: "thought", Body: "Working..."},
	})
}

func TestCreateSessionOnCommentMintsPostableThread(t *testing.T) {
	t.Parallel()

	api := newLinearAPIServer(t, 3600)
	now := time.UnixMilli(1_700_000_000_000)
	bot, adapter := newLinearRuntime(t, api, linear.Options{WebhookSecret: "whsec", Now: func() time.Time { return now }})

	created, err := adapter.CreateSessionOnComment(context.Background(), linear.CreateSessionOnCommentInput{CommentID: "CROOT1"})
	if err != nil {
		t.Fatalf("create session on comment: %v", err)
	}
	if created.SessionID != "SNEW2" || created.IssueID != "ISSUE7" || created.CommentID != "CROOT1" {
		t.Fatalf("created = %#v", created)
	}
	if input := api.lastSessionCreate(t); input["commentId"] != "CROOT1" {
		t.Fatalf("commentId = %v", input["commentId"])
	}

	thread, err := bot.Thread(context.Background(), created.ThreadID)
	if err != nil {
		t.Fatalf("thread: %v", err)
	}
	if _, err := thread.Post(context.Background(), chat.Text("hello")); err != nil {
		t.Fatalf("post: %v", err)
	}
	api.assertActivity(t, 0, linearActivity{
		AgentSessionID: "SNEW2",
		Content:        activityContent{Type: "response", Body: "hello"},
	})
}

func TestCreateSessionValidationAndErrorPaths(t *testing.T) {
	t.Parallel()

	api := newLinearAPIServer(t, 3600)
	now := time.UnixMilli(1_700_000_000_000)
	_, adapter := newLinearRuntime(t, api, linear.Options{WebhookSecret: "whsec", Now: func() time.Time { return now }})
	ctx := context.Background()

	// Missing identifiers are rejected before any API call.
	if _, err := adapter.CreateSessionOnIssue(ctx, linear.CreateSessionOnIssueInput{}); err == nil {
		t.Fatal("expected missing issue id to fail")
	}
	if _, err := adapter.CreateSessionOnComment(ctx, linear.CreateSessionOnCommentInput{}); err == nil {
		t.Fatal("expected missing comment id to fail")
	}
	// Single-install ForTenant guards the minted Thread ID's organization.
	if _, err := adapter.CreateSessionOnIssueForTenant(ctx, "OTHER_ORG", linear.CreateSessionOnIssueInput{IssueID: "I1"}); err == nil || !strings.Contains(err.Error(), "does not match initialized organization") {
		t.Fatalf("mismatched tenant err = %v", err)
	}
	if _, err := adapter.CreateSessionOnIssueForTenant(ctx, "", linear.CreateSessionOnIssueInput{IssueID: "I1"}); err == nil {
		t.Fatal("expected empty tenant to fail")
	}
	if got := api.sessionCreateCount(); got != 0 {
		t.Fatalf("invalid inputs reached the API: %d", got)
	}
	// The matching tenant is accepted in single-install mode.
	if _, err := adapter.CreateSessionOnIssueForTenant(ctx, "ORG1", linear.CreateSessionOnIssueInput{IssueID: "I1"}); err != nil {
		t.Fatalf("matching tenant: %v", err)
	}

	// success=false and a missing issue in the payload are explicit errors.
	api.mu.Lock()
	api.sessionCreateOverride = map[string]any{"data": map[string]any{"agentSessionCreateOnIssue": map[string]any{"success": false}}}
	api.mu.Unlock()
	if _, err := adapter.CreateSessionOnIssue(ctx, linear.CreateSessionOnIssueInput{IssueID: "I1"}); err == nil || !strings.Contains(err.Error(), "failed to create agent session") {
		t.Fatalf("success=false err = %v", err)
	}
	api.mu.Lock()
	api.sessionCreateOverride = map[string]any{"data": map[string]any{"agentSessionCreateOnIssue": map[string]any{"success": true, "agentSession": map[string]any{"id": "SX", "issue": nil, "comment": nil}}}}
	api.mu.Unlock()
	if _, err := adapter.CreateSessionOnIssue(ctx, linear.CreateSessionOnIssueInput{IssueID: "I1"}); err == nil || !strings.Contains(err.Error(), "did not return an issue") {
		t.Fatalf("missing issue err = %v", err)
	}

	// A GraphQL errors array surfaces as a returned error.
	errAPI := newGraphQLErrorServer(t)
	_, errAdapter := newLinearRuntime(t, errAPI, linear.Options{WebhookSecret: "whsec", Now: func() time.Time { return now }})
	if _, err := errAdapter.CreateSessionOnIssue(ctx, linear.CreateSessionOnIssueInput{IssueID: "I1"}); err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("graphql error = %v", err)
	}
	if _, err := errAdapter.CreateSessionOnComment(ctx, linear.CreateSessionOnCommentInput{CommentID: "C1"}); err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("graphql error = %v", err)
	}
}

// TestCreateSessionRetriesOnRateLimit proves the proactive helpers ride the
// adapter's bounded rate-limit retry (ADR 0005): one 429 is retried and the
// mutation still succeeds.
func TestCreateSessionRetriesOnRateLimit(t *testing.T) {
	t.Parallel()

	api := newLinearAPIServer(t, 3600)
	now := time.UnixMilli(1_700_000_000_000)
	_, adapter := newLinearRuntime(t, api, linear.Options{WebhookSecret: "whsec", Now: func() time.Time { return now }})

	api.mu.Lock()
	api.throttleNext = 1
	api.mu.Unlock()
	created, err := adapter.CreateSessionOnIssue(context.Background(), linear.CreateSessionOnIssueInput{IssueID: "I1"})
	if err != nil {
		t.Fatalf("create after throttle: %v", err)
	}
	if created.SessionID != "SNEW1" {
		t.Fatalf("created = %#v", created)
	}
}

func TestSuggestRepositories(t *testing.T) {
	t.Parallel()

	api := newLinearAPIServer(t, 3600)
	now := time.UnixMilli(1_700_000_000_000)
	_, adapter := newLinearRuntime(t, api, linear.Options{WebhookSecret: "whsec", Now: func() time.Time { return now }})
	ctx := context.Background()
	candidates := []linear.CandidateRepository{
		{Hostname: "github.com", RepositoryFullName: "acme/backend"},
		{Hostname: "github.com", RepositoryFullName: "acme/frontend"},
	}

	// Agent-session thread: the session id sharpens the ranking.
	sessionThread := linear.EncodeAgentSessionThreadIDForTest("ORG1", "ISSUE1", "S1")
	suggestions, err := adapter.SuggestRepositories(ctx, sessionThread, candidates)
	if err != nil {
		t.Fatalf("suggest repositories: %v", err)
	}
	want := []linear.RepositorySuggestion{
		{Hostname: "github.com", RepositoryFullName: "acme/backend", Confidence: 0.92},
		{Hostname: "github.com", RepositoryFullName: "acme/frontend", Confidence: 0.35},
	}
	if len(suggestions) != len(want) {
		t.Fatalf("suggestions = %#v", suggestions)
	}
	for i := range want {
		if suggestions[i] != want[i] {
			t.Fatalf("suggestions[%d] = %#v, want %#v", i, suggestions[i], want[i])
		}
	}
	vars := api.lastSuggestionVars(t)
	if vars["issueId"] != "ISSUE1" || vars["agentSessionId"] != "S1" {
		t.Fatalf("variables = %#v", vars)
	}
	sent, ok := vars["candidateRepositories"].([]any)
	if !ok || len(sent) != 2 {
		t.Fatalf("candidateRepositories = %#v", vars["candidateRepositories"])
	}
	first, _ := sent[0].(map[string]any)
	if first["hostname"] != "github.com" || first["repositoryFullName"] != "acme/backend" {
		t.Fatalf("candidateRepositories[0] = %#v", sent[0])
	}

	// Issue-comment thread: no session, so agentSessionId is omitted.
	commentThread := linear.EncodeCommentThreadIDForTest("ORG1", "ISSUE1", "C9")
	if _, err := adapter.SuggestRepositories(ctx, commentThread, candidates); err != nil {
		t.Fatalf("suggest on comment thread: %v", err)
	}
	vars = api.lastSuggestionVars(t)
	if _, present := vars["agentSessionId"]; present {
		t.Fatalf("agentSessionId sent for a comment thread: %#v", vars)
	}

	// Validation happens before any API call.
	before := api.suggestionCount()
	if _, err := adapter.SuggestRepositories(ctx, sessionThread, nil); err == nil {
		t.Fatal("expected empty candidates to fail")
	}
	if _, err := adapter.SuggestRepositories(ctx, sessionThread, []linear.CandidateRepository{{Hostname: "github.com"}}); err == nil {
		t.Fatal("expected candidate without repository full name to fail")
	}
	if _, err := adapter.SuggestRepositories(ctx, sessionThread, []linear.CandidateRepository{{RepositoryFullName: "acme/backend"}}); err == nil {
		t.Fatal("expected candidate without hostname to fail")
	}
	if _, err := adapter.SuggestRepositories(ctx, chat.ThreadID("linear:v1:garbage"), candidates); err == nil {
		t.Fatal("expected malformed thread id to fail")
	}
	if got := api.suggestionCount(); got != before {
		t.Fatalf("invalid inputs reached the API: %d calls", got-before)
	}

	// A throttled query is retried within the bounded RetryPolicy (ADR 0005).
	api.mu.Lock()
	api.throttleNext = 1
	api.mu.Unlock()
	if _, err := adapter.SuggestRepositories(ctx, sessionThread, candidates); err != nil {
		t.Fatalf("suggest after throttle: %v", err)
	}

	// A GraphQL errors array surfaces as a returned error.
	errAPI := newGraphQLErrorServer(t)
	_, errAdapter := newLinearRuntime(t, errAPI, linear.Options{WebhookSecret: "whsec", Now: func() time.Time { return now }})
	if _, err := errAdapter.SuggestRepositories(ctx, sessionThread, candidates); err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("graphql error = %v", err)
	}
}

// TestProactiveCapabilitiesMultiTenant proves per-tenant credential resolution
// (ADR 0006) for the new helpers: each org's call carries that org's derived
// token, the single-install entry points are rejected in multi-tenant mode, and
// an uninstalled org fails cleanly at install lookup.
func TestProactiveCapabilitiesMultiTenant(t *testing.T) {
	t.Parallel()

	api := newLinearMTServer(t)
	now := time.UnixMilli(1_700_000_000_000)
	api.setOrgToken("clientA", "token-A")
	api.setOrgToken("clientB", "token-B")
	store := newFakeInstallStore()
	store.set("ORG_A", chat.Install{
		Tenant:     "ORG_A",
		BotActorID: "APP_A",
		Credential: linear.LinearInstall{WebhookSecret: "secretA", ClientCredentials: linear.ClientCredentials{ClientID: "clientA", ClientSecret: "csA"}},
	})
	store.set("ORG_B", chat.Install{
		Tenant:     "ORG_B",
		BotActorID: "APP_B",
		Credential: linear.LinearInstall{WebhookSecret: "secretB", ClientCredentials: linear.ClientCredentials{ClientID: "clientB", ClientSecret: "csB"}},
	})
	_, adapter := newMultiTenantLinearRuntime(t, api, store, now)
	ctx := context.Background()

	// Single-install entry points require the ForTenant variants here.
	if _, err := adapter.CreateSessionOnIssue(ctx, linear.CreateSessionOnIssueInput{IssueID: "I1"}); err == nil || !strings.Contains(err.Error(), "CreateSessionOnIssueForTenant") {
		t.Fatalf("single-install entry in multi-tenant mode err = %v", err)
	}
	if _, err := adapter.CreateSessionOnComment(ctx, linear.CreateSessionOnCommentInput{CommentID: "C1"}); err == nil || !strings.Contains(err.Error(), "CreateSessionOnCommentForTenant") {
		t.Fatalf("single-install entry in multi-tenant mode err = %v", err)
	}
	if _, err := adapter.CreateSessionOnIssueForTenant(ctx, "", linear.CreateSessionOnIssueInput{IssueID: "I1"}); err == nil {
		t.Fatal("expected empty tenant to fail")
	}

	created, err := adapter.CreateSessionOnIssueForTenant(ctx, "ORG_A", linear.CreateSessionOnIssueInput{IssueID: "ENG-1"})
	if err != nil {
		t.Fatalf("create for ORG_A: %v", err)
	}
	ref, err := adapter.ValidateThreadID(created.ThreadID)
	if err != nil {
		t.Fatalf("validate created thread: %v", err)
	}
	if ref.Tenant != "ORG_A" || ref.Root != "SMT1" {
		t.Fatalf("thread ref = %#v", ref)
	}

	suggestions, err := adapter.SuggestRepositories(ctx, linear.EncodeAgentSessionThreadIDForTest("ORG_B", "ISSUE_B", "SB"), []linear.CandidateRepository{{Hostname: "github.com", RepositoryFullName: "acme/backend"}})
	if err != nil {
		t.Fatalf("suggest for ORG_B: %v", err)
	}
	if len(suggestions) != 1 || suggestions[0].RepositoryFullName != "acme/backend" {
		t.Fatalf("suggestions = %#v", suggestions)
	}

	api.mu.Lock()
	sessionAuth := append([]string(nil), api.sessionCreateAuth...)
	suggestAuth := append([]string(nil), api.suggestAuth...)
	api.mu.Unlock()
	if len(sessionAuth) != 1 || sessionAuth[0] != "Bearer token-A" {
		t.Fatalf("session create auth = %v, want token-A", sessionAuth)
	}
	if len(suggestAuth) != 1 || suggestAuth[0] != "Bearer token-B" {
		t.Fatalf("suggest auth = %v, want token-B", suggestAuth)
	}

	// An uninstalled org fails cleanly at install lookup.
	if _, err := adapter.CreateSessionOnIssueForTenant(ctx, "ORG_X", linear.CreateSessionOnIssueInput{IssueID: "I1"}); err == nil || !strings.Contains(err.Error(), "install lookup") {
		t.Fatalf("uninstalled org err = %v", err)
	}
	if _, err := adapter.SuggestRepositories(ctx, linear.EncodeAgentSessionThreadIDForTest("ORG_X", "I1", "SX"), []linear.CandidateRepository{{Hostname: "github.com", RepositoryFullName: "a/b"}}); err == nil || !strings.Contains(err.Error(), "install lookup") {
		t.Fatalf("uninstalled org err = %v", err)
	}
}
