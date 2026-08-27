# How To Run Linear Agent Sessions

The Linear adapter (experimental) turns Linear agent sessions into normal
Chat SDK Go threads: when a user mentions or delegates to your Linear app, a
session event arrives as a `MessageEvent`, and `Thread.Post` sends an agent
activity response. Beyond that portable surface, the full agent activity
vocabulary — thoughts, responses, actions, elicitations, and errors, plus
session updates carrying plans and external URLs — is exposed
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

New and prompted agent sessions route through the normal hooks. Be aware that
`Thread.Post` on an agent session thread creates an agent activity
**response** — a terminal, session-completing "here is my answer" activity —
so only post it when the answer is genuinely final:

```go
bot.OnNewMention(func(ctx context.Context, ev *chat.MessageEvent) error {
	answer, err := solve(ctx, ev.Message.Text) // your actual work
	if err != nil {
		return err
	}
	_, err = ev.Thread.Post(ctx, chat.Markdown(answer))
	return err
})
```

For sessions that need visible progress before the final answer, start with a
thought instead (next sections).

## Mind The Timing Contract

Linear expects a first activity within roughly 10 seconds of a session event
and further activity within roughly 30 minutes. Post a quick **thought**
(`PostThought`) fast — not a response: a `response` activity is a completion
signal that ends the session, so reserve `Thread.Post` for the final answer.
Use [deferred dispatch](deferred-dispatch.md) for the real work — your
handler moves to a detached work context launched at ack time, so the webhook
acknowledgement no longer waits on it.

## Use The Full Activity Surface

Everything beyond a plain response goes through typed adapter access. Each
call can fail (validation, auth, rate limiting) — check every error before
issuing the next activity.

Nonterminal activities keep the session alive and show progress:

```go
la, ok := chat.AdapterAs[*linear.Adapter](bot, "linear")
if !ok {
	return errors.New("linear adapter is not registered")
}

// Ephemeral progress ("thinking...") activity.
if _, err := la.PostThought(ctx, ev.Thread.ID(), "Reading the issue history."); err != nil {
	return err
}

// A tool-call style action with a result.
if _, err := la.PostAction(ctx, ev.Thread.ID(), linear.ActionInput{
	Action:    "ran",
	Parameter: "go test ./...",
	Result:    "ok",
}); err != nil {
	return err
}

// Maintain the session's plan and external links.
if err := la.UpdateSession(ctx, ev.Thread.ID(), linear.AgentSessionUpdateInput{
	Plan: []linear.PlanStep{
		{Title: "Reproduce the bug", Status: "pending"},
		{Title: "Fix and test", Status: "pending"},
	},
	ReplacePlan: true,
}); err != nil {
	return err
}
```

A session ends with exactly **one** completion signal — a response
(`Thread.Post`), an elicitation, or an error. Pick one branch; do not emit
two completions in the same turn:

```go
if needsInput {
	// Ask the user a question (optionally with a select/auth signal).
	_, err := la.PostElicitation(ctx, ev.Thread.ID(), linear.ElicitationInput{
		Body: "Which environment should I deploy to?",
	})
	return err
}
if buildFailed {
	// Terminal failure state.
	_, err := la.PostError(ctx, ev.Thread.ID(), linear.ErrorInput{
		Body: "The build failed; see the attached log.",
	})
	return err
}
_, err := ev.Thread.Post(ctx, chat.Markdown(answer)) // final response
return err
```

Users can press **Stop** on a session. Check for it through the raw message
escape hatch:

```go
if raw, ok := linear.RawMessageFrom(ev.Message); ok && raw.StopRequested() {
	return nil // wind down gracefully
}
```

This check only runs when the stop event reaches your handler, and events on
one thread are serialized by the thread lock — a stop arriving while a
handler is still running cannot preempt it (`ConcurrencyDrop` discards it on
conflict; `ConcurrencyQueue` delivers it only after the in-flight handler
returns). There is no pre-lock hook, so **Linear's Stop control cannot cancel
in-flight work through this adapter today**. What you can do: structure long
sessions as short handler turns (each turn checks `StopRequested` on the
event that started it before doing more work), or receive the stop signal
out-of-band through your own channel (for example, your own Linear webhook
endpoint or admin API that sets a cancellation flag your handlers poll —
the flag must be set by something outside the runtime's serialized dispatch).

## Generic Issue Comments

The adapter also participates in plain Linear issue comments (outside agent
sessions): a comment that @-mentions your app arrives on a comment-backed
thread, and `Thread.Post` replies in that comment thread. Normal routing
precedence applies: the mention routes to `OnNewMention` only while the
thread is unsubscribed — in a thread you have subscribed, every comment
(mention or not) routes to `OnSubscribedMessage`, so do not put
mention-specific handling exclusively in `OnNewMention`.
Agent-activity methods (`PostThought`, `UpdateSession`, ...) are rejected on
comment threads — they only make sense inside agent sessions.

## Known Gaps

The Linear adapter is experimental. The Linear agent API surface it wraps is
itself in developer preview upstream, and some operations (proactive session
creation, repository suggestions, issue workflow automation) currently
require the `GraphQL` escape hatch rather than typed helpers. The tracked
list lives in
[`docs/linear-agent-capabilities.md`](../linear-agent-capabilities.md).
