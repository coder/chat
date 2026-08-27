# How To Run Linear Agent Sessions

The Linear adapter (experimental) turns Linear agent sessions into normal
Chat SDK Go threads: when a user mentions or delegates to your Linear app, a
session event arrives as a `MessageEvent`, and `Thread.Post` sends an agent
activity response. Beyond that portable surface, the full agent activity
vocabulary — thoughts, actions, elicitations, errors, and plans — is exposed
through typed adapter access (see [ADR 0001](../adr/0001-linear-app-actor-slice.md),
[ADR 0008](../adr/0008-linear-full-adapter.md), and
[ADR 0013](../adr/0013-linear-generic-comments.md)).

Start from the runnable example:
[`examples/linear-agent-hello-world`](../../examples/linear-agent-hello-world/README.md)
walks through the Linear OAuth app setup (app-actor client credentials,
webhook configuration, public HTTPS URL) and includes dogfooding notes.

## Construct The Adapter

```go
linearAdapter, err := linear.New(ctx, linear.Options{
	WebhookSecret: os.Getenv("LINEAR_WEBHOOK_SECRET"),
	ClientCredentials: linear.ClientCredentials{
		ClientID:     os.Getenv("LINEAR_CLIENT_ID"),
		ClientSecret: os.Getenv("LINEAR_CLIENT_SECRET"),
		// Scopes default to: read, write, app:mentionable, app:assignable.
	},
})
```

On `Init` the adapter exchanges client credentials for an app-actor token,
verifies the granted scopes, and discovers the app's own identity so
self-authored activities are filtered before routing.

## Handle Sessions Like Any Thread

New and prompted agent sessions route through the normal hooks:

```go
bot.OnNewMention(func(ctx context.Context, ev *chat.MessageEvent) error {
	_, err := ev.Thread.Post(ctx, chat.Markdown("On it."))
	return err
})
```

`Thread.Post` on an agent session thread creates an agent activity
**response** — the terminal "here is my answer" activity.

## Mind The Timing Contract

Linear expects a first activity within roughly 10 seconds of a session event
and further activity within roughly 30 minutes. Post a quick thought or
response fast, and use [deferred dispatch](deferred-dispatch.md) for real
work — the Linear webhook is acknowledged first and your handler runs on the
detached context.

## Use The Full Activity Surface

Everything beyond a plain response goes through typed adapter access:

```go
la, ok := chat.AdapterAs[*linear.Adapter](bot, "linear")
if !ok {
	return errors.New("linear adapter is not registered")
}

// Ephemeral progress ("thinking...") activity.
_, err = la.PostThought(ctx, ev.Thread.ID(), "Reading the issue history.")

// A tool-call style action with a result.
_, err = la.PostAction(ctx, ev.Thread.ID(), linear.ActionInput{
	Action:    "ran",
	Parameter: "go test ./...",
	Result:    "ok",
})

// Ask the user a question (optionally with a select/auth signal).
_, err = la.PostElicitation(ctx, ev.Thread.ID(), linear.ElicitationInput{
	Body: "Which environment should I deploy to?",
})

// Terminal failure state.
_, err = la.PostError(ctx, ev.Thread.ID(), linear.ErrorInput{
	Body: "The build failed; see the attached log.",
})

// Maintain the session's plan and external links.
err = la.UpdateSession(ctx, ev.Thread.ID(), linear.AgentSessionUpdateInput{
	Plan: []linear.PlanStep{
		{Title: "Reproduce the bug", Status: "pending"},
		{Title: "Fix and test", Status: "pending"},
	},
	ReplacePlan: true,
})
```

Users can press **Stop** on a session. Check for it through the raw message
escape hatch:

```go
if raw, ok := linear.RawMessageFrom(ev.Message); ok && raw.StopRequested() {
	return nil // wind down gracefully
}
```

## Generic Issue Comments

The adapter also participates in plain Linear issue comments (outside agent
sessions): a comment that @-mentions your app routes to `OnNewMention` on a
comment-backed thread, and `Thread.Post` replies in that comment thread.
Agent-activity methods (`PostThought`, `UpdateSession`, ...) are rejected on
comment threads — they only make sense inside agent sessions.

## Known Gaps

The Linear adapter is experimental. The Linear agent API surface it wraps is
itself in developer preview upstream, and some operations (proactive session
creation, repository suggestions, issue workflow automation) currently
require the `GraphQL` escape hatch rather than typed helpers. The tracked
list lives in
[`docs/linear-agent-capabilities.md`](../linear-agent-capabilities.md).
