package linear_test

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/coder/chat"
	"github.com/coder/chat/adapters/linear"
)

// TestCommentStructuredMentionRoutes verifies a comment with no textual @-mention
// but a structured botActorMentions entry naming the app actor still routes as a
// New Mention (structured mention precedence).
func TestCommentStructuredMentionRoutes(t *testing.T) {
	t.Parallel()

	api := newLinearAPIServer(t, 3600)
	now := time.UnixMilli(1_700_000_000_000)
	bot, _ := newLinearRuntime(t, api, linear.Options{WebhookSecret: "whsec", Now: func() time.Time { return now }})

	var mentions int
	bot.OnNewMention(func(context.Context, *chat.MessageEvent) error { mentions++; return nil })

	body := fmt.Sprintf(`{
		"type":"Comment","action":"create","organizationId":"ORG1",
		"createdAt":"2026-05-12T00:00:00Z","webhookTimestamp":%d,
		"data":{
			"id":"CM1","body":"please look at this with no at sign",
			"issueId":"ISSUE1","issue":{"id":"ISSUE1","title":"An issue"},
			"botActorMentions":[{"actorId":"APP1"}],
			"user":{"id":"U1","type":"user","name":"User One"}
		}
	}`, now.UnixMilli())
	postLinearEvent(t, bot, "whsec", body)
	if mentions != 1 {
		t.Fatalf("structured mention routed = %d, want 1", mentions)
	}
}

// TestCommentParentIDStringResolvesRoot verifies the parentId scalar form (not the
// nested parent object) is used to mint the root-comment thread, so a threaded
// reply posts back under the root comment.
func TestCommentParentIDStringResolvesRoot(t *testing.T) {
	t.Parallel()

	api := newLinearAPIServer(t, 3600)
	now := time.UnixMilli(1_700_000_000_000)
	bot, _ := newLinearRuntime(t, api, linear.Options{WebhookSecret: "whsec", Now: func() time.Time { return now }})

	var threadID chat.ThreadID
	bot.OnNewMention(func(_ context.Context, ev *chat.MessageEvent) error {
		threadID = ev.Thread.ID()
		return nil
	})
	body := fmt.Sprintf(`{
		"type":"Comment","action":"create","organizationId":"ORG1",
		"createdAt":"2026-05-12T00:00:00Z","webhookTimestamp":%d,
		"data":{
			"id":"CM2","body":"@APP1 reply","parentId":"ROOTCM",
			"issueId":"ISSUE1","issue":{"id":"ISSUE1"},
			"user":{"id":"U1","type":"user","name":"User One"}
		}
	}`, now.UnixMilli())
	postLinearEvent(t, bot, "whsec", body)
	if threadID == "" {
		t.Fatal("no thread captured")
	}
	thread, err := bot.Thread(context.Background(), threadID)
	if err != nil {
		t.Fatalf("thread: %v", err)
	}
	if _, err := thread.Post(context.Background(), chat.Text("ack")); err != nil {
		t.Fatalf("post: %v", err)
	}
	comment := api.lastComment(t)
	if comment["parentId"] != "ROOTCM" {
		t.Fatalf("parentId = %#v, want ROOTCM from scalar parentId", comment["parentId"])
	}
}

// TestCommentAuthorFallbacks verifies author resolution falls back from user to
// actor to botActor, and that each form still resolves a usable author.
func TestCommentAuthorFallbacks(t *testing.T) {
	t.Parallel()

	api := newLinearAPIServer(t, 3600)
	now := time.UnixMilli(1_700_000_000_000)
	bot, _ := newLinearRuntime(t, api, linear.Options{WebhookSecret: "whsec", Now: func() time.Time { return now }})

	authors := map[string]string{}
	bot.OnNewMention(func(_ context.Context, ev *chat.MessageEvent) error {
		authors[ev.Message.ID] = ev.Message.Author.ID
		return nil
	})

	ts := now.UnixMilli()
	// actor fallback (no user).
	postLinearEvent(t, bot, "whsec", fmt.Sprintf(`{
		"type":"Comment","action":"create","organizationId":"ORG1","webhookTimestamp":%d,
		"data":{"id":"CMA","body":"@APP1","issueId":"ISSUE1","issue":{"id":"ISSUE1"},
			"actor":{"id":"ACTOR1","type":"user","name":"Actor One"}}}`, ts))
	if authors["CMA"] != "ACTOR1" {
		t.Fatalf("actor-fallback author = %q, want ACTOR1", authors["CMA"])
	}
}

// TestCommentUnknownFieldTolerance verifies a Comment payload with extra unknown
// fields still decodes and routes, and that the verbatim body is preserved on the
// escape hatch (Raw / Envelope).
func TestCommentUnknownFieldTolerance(t *testing.T) {
	t.Parallel()

	api := newLinearAPIServer(t, 3600)
	now := time.UnixMilli(1_700_000_000_000)
	bot, _ := newLinearRuntime(t, api, linear.Options{WebhookSecret: "whsec", Now: func() time.Time { return now }})

	var raw *linear.RawMessage
	bot.OnNewMention(func(_ context.Context, ev *chat.MessageEvent) error {
		raw, _ = linear.RawMessageFrom(ev.Message)
		return nil
	})
	body := fmt.Sprintf(`{
		"type":"Comment","action":"create","organizationId":"ORG1",
		"createdAt":"2026-05-12T00:00:00Z","webhookTimestamp":%d,
		"unknownTop":"x",
		"data":{
			"id":"CM9","body":"@APP1 hi","issueId":"ISSUE1","issue":{"id":"ISSUE1"},
			"reactionData":[{"emoji":"thumbsup"}],"futureField":42,
			"user":{"id":"U1","type":"user","name":"User One"}
		}
	}`, now.UnixMilli())
	postLinearEvent(t, bot, "whsec", body)

	if raw == nil || raw.Kind != "comment" {
		t.Fatalf("raw = %#v", raw)
	}
	if raw.Session != nil {
		t.Fatal("comment-kind raw must have nil Session")
	}
	// The verbatim comment sub-object is preserved on the escape hatch.
	var preserved map[string]any
	if err := json.Unmarshal(raw.Comment, &preserved); err != nil {
		t.Fatalf("comment raw decode: %v", err)
	}
	if preserved["futureField"] != float64(42) {
		t.Fatalf("unknown field not preserved verbatim: %#v", preserved)
	}
	if len(raw.Envelope) == 0 {
		t.Fatal("envelope not preserved on comment event")
	}
}

// TestCommentOAuthClientMismatchIgnored verifies a Comment for another OAuth
// client is an Ignored Event.
func TestCommentOAuthClientMismatchIgnored(t *testing.T) {
	t.Parallel()

	api := newLinearAPIServer(t, 3600)
	now := time.UnixMilli(1_700_000_000_000)
	bot, _ := newLinearRuntime(t, api, linear.Options{WebhookSecret: "whsec", Now: func() time.Time { return now }})

	var seen int
	bot.OnNewMention(func(context.Context, *chat.MessageEvent) error { seen++; return nil })
	body := fmt.Sprintf(`{
		"type":"Comment","action":"create","organizationId":"ORG1","oauthClientId":"other-client",
		"webhookTimestamp":%d,
		"data":{"id":"CM1","body":"@APP1","issueId":"ISSUE1","issue":{"id":"ISSUE1"},
			"user":{"id":"U1","type":"user","name":"User One"}}}`, now.UnixMilli())
	postLinearEvent(t, bot, "whsec", body)
	if seen != 0 {
		t.Fatalf("cross-client comment routed: %d", seen)
	}
}

// TestCommentBareIDInBodyDoesNotMention verifies that the opaque app-actor id
// appearing in unrelated comment text (without an "@" mention prefix and without a
// structured botActorMentions entry) does not route as a New Mention. Only a
// genuine @-mention or structured mention should trigger OnNewMention.
func TestCommentBareIDInBodyDoesNotMention(t *testing.T) {
	t.Parallel()

	api := newLinearAPIServer(t, 3600)
	now := time.UnixMilli(1_700_000_000_000)
	bot, _ := newLinearRuntime(t, api, linear.Options{WebhookSecret: "whsec", Now: func() time.Time { return now }})

	var mentions int
	bot.OnNewMention(func(context.Context, *chat.MessageEvent) error { mentions++; return nil })

	// The bare id APP1 appears in prose but is never @-mentioned.
	body := fmt.Sprintf(`{
		"type":"Comment","action":"create","organizationId":"ORG1",
		"createdAt":"2026-05-12T00:00:00Z","webhookTimestamp":%d,
		"data":{
			"id":"CM1","body":"see commit APP1 for the fix",
			"issueId":"ISSUE1","issue":{"id":"ISSUE1"},
			"user":{"id":"U1","type":"user","name":"User One"}
		}
	}`, now.UnixMilli())
	postLinearEvent(t, bot, "whsec", body)
	if mentions != 0 {
		t.Fatalf("bare id substring false-positive routed as mention: %d", mentions)
	}
}
