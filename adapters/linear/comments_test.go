package linear_test

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/coder/chat"
	"github.com/coder/chat/adapters/linear"
)

func TestCommentMentionRoutesAndPosts(t *testing.T) {
	t.Parallel()

	api := newLinearAPIServer(t, 3600)
	now := time.UnixMilli(1_700_000_000_000)
	bot, _ := newLinearRuntime(t, api, linear.Options{WebhookSecret: "whsec", Now: func() time.Time { return now }})

	var routes []string
	var threadID chat.ThreadID
	bot.OnNewMention(func(ctx context.Context, ev *chat.MessageEvent) error {
		routes = append(routes, "new:"+ev.Message.ID+":"+ev.Message.Author.Name)
		threadID = ev.Thread.ID()
		raw, ok := linear.RawMessageFrom(ev.Message)
		if !ok || raw.Kind != "comment" {
			t.Fatalf("raw kind = %#v", raw)
		}
		return ev.Thread.Subscribe(ctx)
	})
	bot.OnSubscribedMessage(func(_ context.Context, ev *chat.MessageEvent) error {
		routes = append(routes, "subscribed:"+ev.Message.ID)
		return nil
	})

	postLinearEvent(t, bot, "whsec", commentPayload(now, "CM1", "hey @APP1 help", "U1", "User One", "ISSUE1", ""))
	postLinearEvent(t, bot, "whsec", commentPayload(now, "CM2", "follow up", "U1", "User One", "ISSUE1", "CM1"))

	want := []string{"new:CM1:User One", "subscribed:CM2"}
	if !equalStrings(routes, want) {
		t.Fatalf("routes = %#v, want %#v", routes, want)
	}

	thread, err := bot.Thread(context.Background(), threadID)
	if err != nil {
		t.Fatalf("thread: %v", err)
	}
	sent, err := thread.Post(context.Background(), chat.Text("here you go"))
	if err != nil {
		t.Fatalf("post comment: %v", err)
	}
	if sent.ThreadID != threadID {
		t.Fatalf("sent thread = %q", sent.ThreadID)
	}
	if api.activityCount() != 0 {
		t.Fatalf("comment post should not create an agent activity, got %d", api.activityCount())
	}
	comment := api.lastComment(t)
	if comment["issueId"] != "ISSUE1" || comment["body"] != "here you go" {
		t.Fatalf("comment input = %#v", comment)
	}
	if comment["parentId"] != "CM1" {
		t.Fatalf("comment parentId = %#v, want root CM1", comment["parentId"])
	}
}

func TestCommentWithoutMentionIsUnrouted(t *testing.T) {
	t.Parallel()

	api := newLinearAPIServer(t, 3600)
	now := time.UnixMilli(1_700_000_000_000)
	bot, _ := newLinearRuntime(t, api, linear.Options{WebhookSecret: "whsec", Now: func() time.Time { return now }})

	var seen int
	bot.OnNewMention(func(context.Context, *chat.MessageEvent) error { seen++; return nil })
	postLinearEvent(t, bot, "whsec", commentPayload(now, "CM1", "no mention here", "U1", "User One", "ISSUE1", ""))
	if seen != 0 {
		t.Fatalf("unmentioned comment routed to new mention: %d", seen)
	}
}

func TestSelfCommentFiltered(t *testing.T) {
	t.Parallel()

	api := newLinearAPIServer(t, 3600)
	now := time.UnixMilli(1_700_000_000_000)
	bot, _ := newLinearRuntime(t, api, linear.Options{WebhookSecret: "whsec", Now: func() time.Time { return now }})

	var seen int
	bot.OnNewMention(func(context.Context, *chat.MessageEvent) error { seen++; return nil })
	// Author is the app actor (bot) -> filtered by runtime self-message guard.
	postLinearEvent(t, bot, "whsec", commentPayloadWithActor(now, "CM1", "mention @APP1", "APP1", "Linear Bot", "bot", "ISSUE1", ""))
	if seen != 0 {
		t.Fatalf("self comment routed: %d", seen)
	}
}

func TestCommentTenantScoping(t *testing.T) {
	t.Parallel()

	api := newLinearAPIServer(t, 3600)
	now := time.UnixMilli(1_700_000_000_000)
	bot, _ := newLinearRuntime(t, api, linear.Options{WebhookSecret: "whsec", Now: func() time.Time { return now }})

	var seen int
	bot.OnNewMention(func(context.Context, *chat.MessageEvent) error { seen++; return nil })
	postLinearEvent(t, bot, "whsec", commentPayloadForOrg(now, "ORG2", "CM1", "@APP1", "U1", "User One", "ISSUE1", ""))
	if seen != 0 {
		t.Fatalf("cross-tenant comment routed: %d", seen)
	}
}

func TestAgentSessionThreadStillPostsResponse(t *testing.T) {
	t.Parallel()

	api := newLinearAPIServer(t, 3600)
	now := time.UnixMilli(1_700_000_000_000)
	bot, _ := newLinearRuntime(t, api, linear.Options{WebhookSecret: "whsec", Now: func() time.Time { return now }})
	threadID := agentSessionThread(t, bot, api, now)

	thread, err := bot.Thread(context.Background(), threadID)
	if err != nil {
		t.Fatalf("thread: %v", err)
	}
	if _, err := thread.Post(context.Background(), chat.Text("done")); err != nil {
		t.Fatalf("post: %v", err)
	}
	api.assertActivity(t, 0, linearActivity{AgentSessionID: "S1", Content: activityContent{Type: "response", Body: "done"}})
	if api.commentCount() != 0 {
		t.Fatalf("agent-session post should not create a comment, got %d", api.commentCount())
	}
}

func TestActivityMethodsRejectCommentThreads(t *testing.T) {
	t.Parallel()

	api := newLinearAPIServer(t, 3600)
	now := time.UnixMilli(1_700_000_000_000)
	bot, adapter := newLinearRuntime(t, api, linear.Options{WebhookSecret: "whsec", Now: func() time.Time { return now }})

	var threadID chat.ThreadID
	bot.OnNewMention(func(_ context.Context, ev *chat.MessageEvent) error {
		threadID = ev.Thread.ID()
		return nil
	})
	postLinearEvent(t, bot, "whsec", commentPayload(now, "CM1", "@APP1 hi", "U1", "User One", "ISSUE1", ""))
	if threadID == "" {
		t.Fatal("no comment thread captured")
	}
	ctx := context.Background()

	checks := []struct {
		name string
		err  error
	}{
		{"thought", mustErr(func() error { _, e := adapter.PostThought(ctx, threadID, "x"); return e })},
		{"action", mustErr(func() error { _, e := adapter.PostAction(ctx, threadID, linear.ActionInput{Action: "x"}); return e })},
		{"elicitation", mustErr(func() error {
			_, e := adapter.PostElicitation(ctx, threadID, linear.ElicitationInput{Body: "x"})
			return e
		})},
		{"error", mustErr(func() error { _, e := adapter.PostError(ctx, threadID, linear.ErrorInput{Body: "x"}); return e })},
		{"create", mustErr(func() error {
			_, e := adapter.CreateAgentActivity(ctx, threadID, linear.AgentActivityInput{Content: map[string]any{"type": "response", "body": "x"}})
			return e
		})},
		{"update", adapter.UpdateSession(ctx, threadID, linear.AgentSessionUpdateInput{ExternalURLs: []linear.ExternalURL{{URL: "u"}}})},
	}
	for _, c := range checks {
		if c.err == nil || !strings.Contains(c.err.Error(), "agent-session") {
			t.Fatalf("%s err = %v, want agent-session rejection", c.name, c.err)
		}
	}
	if api.activityCount() != 0 {
		t.Fatalf("no activity should reach a comment thread, got %d", api.activityCount())
	}
}

func mustErr(f func() error) error { return f() }

func commentPayload(now time.Time, commentID, body, userID, userName, issueID, parentID string) string {
	return commentPayloadForOrg(now, "ORG1", commentID, body, userID, userName, issueID, parentID)
}

func commentPayloadForOrg(now time.Time, org, commentID, body, userID, userName, issueID, parentID string) string {
	return commentPayloadWithActorForOrg(now, org, commentID, body, userID, userName, "user", issueID, parentID)
}

func commentPayloadWithActor(now time.Time, commentID, body, userID, userName, actorType, issueID, parentID string) string {
	return commentPayloadWithActorForOrg(now, "ORG1", commentID, body, userID, userName, actorType, issueID, parentID)
}

func commentPayloadWithActorForOrg(now time.Time, org, commentID, body, userID, userName, actorType, issueID, parentID string) string {
	parent := ""
	if parentID != "" {
		parent = fmt.Sprintf(`,"parent":{"id":"%s"}`, parentID)
	}
	return fmt.Sprintf(`{
		"type":"Comment",
		"action":"create",
		"organizationId":"%s",
		"createdAt":"2026-05-12T00:00:00Z",
		"webhookTimestamp":%d,
		"data":{
			"id":"%s",
			"body":"%s",
			"issueId":"%s",
			"issue":{"id":"%s","title":"An issue"}%s,
			"user":{"id":"%s","type":"%s","name":"%s"}
		}
	}`, org, now.UnixMilli(), commentID, body, issueID, issueID, parent, userID, actorType, userName)
}
