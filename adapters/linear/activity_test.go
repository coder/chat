package linear_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/coder/chat"
	"github.com/coder/chat/adapters/linear"
)

// agentSessionThread posts a created agent-session event and returns the
// resulting agent-session Thread ID.
func agentSessionThread(t *testing.T, bot *chat.Chat, api *linearAPIServer, now time.Time) chat.ThreadID {
	t.Helper()
	var threadID chat.ThreadID
	bot.OnNewMention(func(_ context.Context, ev *chat.MessageEvent) error {
		threadID = ev.Thread.ID()
		return nil
	})
	postLinearEvent(t, bot, "whsec", createdPayload(now, "C1", "hello", "U1", "User One", "APP1"))
	if threadID == "" {
		t.Fatal("no agent-session thread captured")
	}
	return threadID
}

func TestCreateAgentActivityContentTypes(t *testing.T) {
	t.Parallel()

	api := newLinearAPIServer(t, 3600)
	now := time.UnixMilli(1_700_000_000_000)
	bot, adapter := newLinearRuntime(t, api, linear.Options{WebhookSecret: "whsec", Now: func() time.Time { return now }})
	threadID := agentSessionThread(t, bot, api, now)

	ctx := context.Background()
	cases := []struct {
		name      string
		content   map[string]any
		ephemeral bool
	}{
		{"thought", map[string]any{"type": "thought", "body": "thinking"}, true},
		{"response", map[string]any{"type": "response", "body": "answer"}, false},
		{"action", map[string]any{"type": "action", "action": "run-tests"}, true},
		{"elicitation", map[string]any{"type": "elicitation", "body": "which one?"}, false},
		{"error", map[string]any{"type": "error", "body": "boom"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sent, err := adapter.CreateAgentActivity(ctx, threadID, linear.AgentActivityInput{Content: tc.content, Ephemeral: tc.ephemeral})
			if err != nil {
				t.Fatalf("create %s: %v", tc.name, err)
			}
			if sent == nil || sent.ID == "" || sent.ThreadID != threadID {
				t.Fatalf("sent = %#v", sent)
			}
			raw := api.lastRawActivity(t)
			content, ok := raw["content"].(map[string]any)
			if !ok || content["type"] != tc.name {
				t.Fatalf("recorded content = %#v", raw["content"])
			}
			if raw["ephemeral"] != tc.ephemeral {
				t.Fatalf("ephemeral = %v, want %v", raw["ephemeral"], tc.ephemeral)
			}
		})
	}
}

func TestCreateAgentActivitySignalPassThrough(t *testing.T) {
	t.Parallel()

	api := newLinearAPIServer(t, 3600)
	now := time.UnixMilli(1_700_000_000_000)
	bot, adapter := newLinearRuntime(t, api, linear.Options{WebhookSecret: "whsec", Now: func() time.Time { return now }})
	threadID := agentSessionThread(t, bot, api, now)

	_, err := adapter.CreateAgentActivity(context.Background(), threadID, linear.AgentActivityInput{
		Content:        map[string]any{"type": "elicitation", "body": "link account"},
		Signal:         "auth",
		SignalMetadata: linear.AuthSignalMetadata{URL: "https://example.com/auth"},
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	raw := api.lastRawActivity(t)
	if raw["signal"] != "auth" {
		t.Fatalf("signal = %#v", raw["signal"])
	}
	meta, ok := raw["signalMetadata"].(map[string]any)
	if !ok || meta["url"] != "https://example.com/auth" {
		t.Fatalf("signalMetadata = %#v", raw["signalMetadata"])
	}
}

func TestEphemeralRejectedForNonThoughtAction(t *testing.T) {
	t.Parallel()

	api := newLinearAPIServer(t, 3600)
	now := time.UnixMilli(1_700_000_000_000)
	bot, adapter := newLinearRuntime(t, api, linear.Options{WebhookSecret: "whsec", Now: func() time.Time { return now }})
	threadID := agentSessionThread(t, bot, api, now)

	for _, typ := range []string{"response", "elicitation", "error"} {
		_, err := adapter.CreateAgentActivity(context.Background(), threadID, linear.AgentActivityInput{
			Content:   map[string]any{"type": typ, "body": "x"},
			Ephemeral: true,
		})
		if err == nil || !strings.Contains(err.Error(), "ephemeral is only valid") {
			t.Fatalf("type %s err = %v, want ephemeral rejection", typ, err)
		}
	}
	// thought and action accept ephemeral.
	for _, content := range []map[string]any{
		{"type": "thought", "body": "x"},
		{"type": "action", "action": "x"},
	} {
		if _, err := adapter.CreateAgentActivity(context.Background(), threadID, linear.AgentActivityInput{Content: content, Ephemeral: true}); err != nil {
			t.Fatalf("ephemeral %v err = %v", content["type"], err)
		}
	}
}

func TestTypedHelpersMatchGenericPayloads(t *testing.T) {
	t.Parallel()

	api := newLinearAPIServer(t, 3600)
	now := time.UnixMilli(1_700_000_000_000)
	bot, adapter := newLinearRuntime(t, api, linear.Options{WebhookSecret: "whsec", Now: func() time.Time { return now }})
	threadID := agentSessionThread(t, bot, api, now)
	ctx := context.Background()

	if _, err := adapter.PostAction(ctx, threadID, linear.ActionInput{Action: "run", Parameter: "p", Result: "r", Ephemeral: true}); err != nil {
		t.Fatalf("action: %v", err)
	}
	action := api.lastRawActivity(t)
	content := action["content"].(map[string]any)
	if content["type"] != "action" || content["action"] != "run" || content["parameter"] != "p" || content["result"] != "r" {
		t.Fatalf("action content = %#v", content)
	}
	if action["ephemeral"] != true {
		t.Fatalf("action ephemeral = %v", action["ephemeral"])
	}

	if _, err := adapter.PostElicitation(ctx, threadID, linear.ElicitationInput{Body: "pick", Signal: "select", SignalMetadata: linear.SelectSignalMetadata{Options: []linear.SelectOption{{Value: "a"}}}}); err != nil {
		t.Fatalf("elicitation: %v", err)
	}
	elic := api.lastRawActivity(t)
	if elic["content"].(map[string]any)["type"] != "elicitation" || elic["signal"] != "select" || elic["ephemeral"] != false {
		t.Fatalf("elicitation = %#v", elic)
	}

	if _, err := adapter.PostError(ctx, threadID, linear.ErrorInput{Body: "failed"}); err != nil {
		t.Fatalf("error: %v", err)
	}
	errAct := api.lastRawActivity(t)
	if errAct["content"].(map[string]any)["type"] != "error" || errAct["ephemeral"] != false {
		t.Fatalf("error = %#v", errAct)
	}
}

func TestPostErrorEndsSessionWithoutStrayResponse(t *testing.T) {
	t.Parallel()

	api := newLinearAPIServer(t, 3600)
	now := time.UnixMilli(1_700_000_000_000)
	bot, adapter := newLinearRuntime(t, api, linear.Options{WebhookSecret: "whsec", Now: func() time.Time { return now }})
	threadID := agentSessionThread(t, bot, api, now)

	if _, err := adapter.PostError(context.Background(), threadID, linear.ErrorInput{Body: "fatal"}); err != nil {
		t.Fatalf("post error: %v", err)
	}
	if got := api.activityCount(); got != 1 {
		t.Fatalf("activity count = %d, want exactly one error activity", got)
	}
	only := api.lastRawActivity(t)
	if only["content"].(map[string]any)["type"] != "error" {
		t.Fatalf("only activity = %#v", only)
	}
}

func TestEmptyInputValidation(t *testing.T) {
	t.Parallel()

	api := newLinearAPIServer(t, 3600)
	now := time.UnixMilli(1_700_000_000_000)
	bot, adapter := newLinearRuntime(t, api, linear.Options{WebhookSecret: "whsec", Now: func() time.Time { return now }})
	threadID := agentSessionThread(t, bot, api, now)
	ctx := context.Background()

	if _, err := adapter.CreateAgentActivity(ctx, threadID, linear.AgentActivityInput{}); err == nil {
		t.Fatal("expected empty content to fail")
	}
	if _, err := adapter.PostAction(ctx, threadID, linear.ActionInput{}); err == nil {
		t.Fatal("expected empty action to fail")
	}
	if _, err := adapter.PostElicitation(ctx, threadID, linear.ElicitationInput{}); err == nil {
		t.Fatal("expected empty elicitation to fail")
	}
	if _, err := adapter.PostError(ctx, threadID, linear.ErrorInput{}); err == nil {
		t.Fatal("expected empty error to fail")
	}
}

func TestUpdateSession(t *testing.T) {
	t.Parallel()

	api := newLinearAPIServer(t, 3600)
	now := time.UnixMilli(1_700_000_000_000)
	bot, adapter := newLinearRuntime(t, api, linear.Options{WebhookSecret: "whsec", Now: func() time.Time { return now }})
	threadID := agentSessionThread(t, bot, api, now)
	ctx := context.Background()

	if err := adapter.UpdateSession(ctx, threadID, linear.AgentSessionUpdateInput{
		ExternalURLs: []linear.ExternalURL{{URL: "https://pr", Label: "PR"}},
	}); err != nil {
		t.Fatalf("set external urls: %v", err)
	}
	set := api.lastSessionInput(t)
	urls, ok := set["externalUrls"].([]any)
	if !ok || len(urls) != 1 {
		t.Fatalf("externalUrls = %#v", set["externalUrls"])
	}

	if err := adapter.UpdateSession(ctx, threadID, linear.AgentSessionUpdateInput{
		AddExternalURLs:    []linear.ExternalURL{{URL: "https://add"}},
		RemoveExternalURLs: []string{"https://old"},
	}); err != nil {
		t.Fatalf("add/remove: %v", err)
	}
	addRemove := api.lastSessionInput(t)
	if addRemove["addExternalUrls"] == nil || addRemove["removeExternalUrls"] == nil {
		t.Fatalf("add/remove input = %#v", addRemove)
	}
	if _, present := addRemove["externalUrls"]; present {
		t.Fatalf("externalUrls should be absent when only add/remove set: %#v", addRemove)
	}

	if err := adapter.UpdateSession(ctx, threadID, linear.AgentSessionUpdateInput{
		Plan: []linear.PlanStep{{Title: "step 1", Status: "pending"}},
	}); err != nil {
		t.Fatalf("plan: %v", err)
	}
	plan := api.lastSessionInput(t)
	steps, ok := plan["plan"].([]any)
	if !ok || len(steps) != 1 {
		t.Fatalf("plan = %#v", plan["plan"])
	}

	if err := adapter.UpdateSession(ctx, threadID, linear.AgentSessionUpdateInput{ReplacePlan: true}); err != nil {
		t.Fatalf("replace plan empty: %v", err)
	}
	emptyPlan := api.lastSessionInput(t)
	if steps, ok := emptyPlan["plan"].([]any); !ok || len(steps) != 0 {
		t.Fatalf("replace-empty plan = %#v", emptyPlan["plan"])
	}

	if err := adapter.UpdateSession(ctx, threadID, linear.AgentSessionUpdateInput{}); err == nil {
		t.Fatal("expected empty update to fail")
	}
}

func TestGraphQLEscapeHatch(t *testing.T) {
	t.Parallel()

	api := newLinearAPIServer(t, 1) // expires quickly to exercise refresh
	now := time.UnixMilli(1_700_000_000_000)
	_, adapter := newLinearRuntime(t, api, linear.Options{WebhookSecret: "whsec", Now: func() time.Time { return now }})

	var dest struct {
		Viewer struct {
			ID string `json:"id"`
		} `json:"viewer"`
	}
	if err := adapter.GraphQL(context.Background(), `query ViewerIdentity { viewer { id } }`, nil, &dest); err != nil {
		t.Fatalf("graphql: %v", err)
	}
	if dest.Viewer.ID != "APP1" {
		t.Fatalf("viewer id = %q", dest.Viewer.ID)
	}
	if api.tokenRequests() < 2 {
		t.Fatalf("token requests = %d, want refresh exercised", api.tokenRequests())
	}
}

func TestGraphQLSurfacesErrorsAndHidesToken(t *testing.T) {
	t.Parallel()

	api := newGraphQLErrorServer(t)
	now := time.UnixMilli(1_700_000_000_000)
	_, adapter := newLinearRuntime(t, api, linear.Options{WebhookSecret: "whsec", Now: func() time.Time { return now }})

	var dest map[string]any
	err := adapter.GraphQL(context.Background(), `query Boom { boom }`, nil, &dest)
	if err == nil || !strings.Contains(err.Error(), "graphql error") {
		t.Fatalf("err = %v, want graphql error", err)
	}
	// dest must never receive the bearer token.
	if containsToken(dest) {
		t.Fatalf("dest leaked token: %#v", dest)
	}
}

func TestRateLimitRetryHonorsRetryAfterAndBounds(t *testing.T) {
	t.Parallel()

	api := newLinearAPIServer(t, 3600)
	api.rateLimit = 1 // first activity call is throttled, then succeeds
	now := time.UnixMilli(1_700_000_000_000)
	bot, adapter := newLinearRuntime(t, api, linear.Options{
		WebhookSecret: "whsec",
		Now:           func() time.Time { return now },
		RetryPolicy:   linear.RetryPolicy{MaxAttempts: 3, MaxElapsed: time.Second, BaseDelay: time.Millisecond, MaxDelay: 10 * time.Millisecond},
	})
	threadID := agentSessionThread(t, bot, api, now)

	if _, err := adapter.PostThought(context.Background(), threadID, "thinking"); err != nil {
		t.Fatalf("thought after retry: %v", err)
	}
	if api.activityCount() != 1 {
		t.Fatalf("activity count = %d, want 1 after retry", api.activityCount())
	}
}

func TestRateLimitRetryExhaustionReturnsTypedError(t *testing.T) {
	t.Parallel()

	api := newLinearAPIServer(t, 3600)
	api.rateLimit = 100 // always throttle
	now := time.UnixMilli(1_700_000_000_000)
	bot, adapter := newLinearRuntime(t, api, linear.Options{
		WebhookSecret: "whsec",
		Now:           func() time.Time { return now },
		RetryPolicy:   linear.RetryPolicy{MaxAttempts: 2, MaxElapsed: time.Second, BaseDelay: time.Millisecond, MaxDelay: 5 * time.Millisecond},
	})
	threadID := agentSessionThread(t, bot, api, now)

	_, err := adapter.PostThought(context.Background(), threadID, "thinking")
	var rl *linear.RateLimited
	if !errors.As(err, &rl) {
		t.Fatalf("err = %v, want *linear.RateLimited", err)
	}
	if rl.Attempts < 2 {
		t.Fatalf("attempts = %d", rl.Attempts)
	}
}
