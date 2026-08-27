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

Users can press **Stop** on a session. It arrives as a prompt carrying the
human-to-agent `stop` signal; Linear expects the agent to halt immediately
and then confirm with one final `response` (or `error`) activity. The worked
example detects and confirms it in one helper:

<!-- source: examples/linear-agent-hello-world/capabilities.go -->
```go
func confirmStop(ctx context.Context, ev *chat.MessageEvent) (bool, error) {
	raw, ok := linear.RawMessageFrom(ev.Message)
	if !ok || !raw.StopRequested() {
		return false, nil
	}
	_, err := ev.Thread.Post(ctx, chat.Text("Stopping as requested — no further changes will be made."))
	return true, err
}
```

Call it first in every message handler and return when it reports stopped.
This check only runs when the stop event reaches your handler, and events on
one thread are serialized by the thread lock — a stop arriving while a
handler is still running cannot preempt it (`ConcurrencyDrop` discards it on
conflict; `ConcurrencyQueue` delivers it only after the in-flight handler
returns). There is no pre-lock hook, so **Linear's Stop control cannot cancel
in-flight work through this adapter today**. What you can do: structure long
sessions as short handler turns (each turn checks `StopRequested` on the
event that started it before doing more work — `confirmStop` above is exactly
that turn-boundary check), or receive the stop signal out-of-band through
your own channel (for example, your own Linear webhook endpoint or admin API
that sets a cancellation flag your handlers poll — the flag must be set by
something outside the runtime's serialized dispatch).

## Worked Capability Loops

The rest of this page walks the full interaction loops. Every code block is
extracted from the buildable, tested example
([`examples/linear-agent-hello-world/capabilities.go`](../../examples/linear-agent-hello-world/capabilities.go));
a documentation test keeps the snippets and the source in sync. The
`linearAgentAccess` parameter is the example's small interface over
`*linear.Adapter` — obtained via `chat.AdapterAs` as shown above — so the
handlers stay testable against a fake.

### Start A Session Proactively

When work starts from an external trigger (a failing build, a cron job) and
the agent was neither mentioned nor delegated, create the session yourself.
`CreateSessionOnIssue` wraps Linear's `agentSessionCreateOnIssue` mutation and
returns a `CreatedAgentSession` whose `ThreadID` behaves exactly like a
webhook-minted one:

<!-- source: examples/linear-agent-hello-world/capabilities.go -->
```go
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
```

`CreateSessionOnComment` roots the session on an existing issue comment
instead. In multi-tenant mode (an `InstallStore` configured), use
`CreateSessionOnIssueForTenant` / `CreateSessionOnCommentForTenant` with the
target organization id — proactive creation has no inbound webhook to resolve
the tenant from.

### Pick A Repository With Suggestions

`SuggestRepositories` wraps Linear's `issueRepositorySuggestions` query: pass
the candidate repositories the agent already has access to, and Linear ranks
them with confidence scores using issue, session, guidance, and internal
signals. Proceed when confident; otherwise pair the shortlist with a `select`
elicitation:

<!-- source: examples/linear-agent-hello-world/capabilities.go -->
```go
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
```

### Offer Choices With A Select Elicitation

A `select`-signal elicitation renders the options natively in Linear. It is a
completion signal: the session waits for the user after you post it.

<!-- source: examples/linear-agent-hello-world/capabilities.go -->
```go
func offerRepositoryChoice(ctx context.Context, la linearAgentAccess, threadID chat.ThreadID, suggestions []linear.RepositorySuggestion) error {
	if len(suggestions) == 0 {
		_, err := la.PostElicitation(ctx, threadID, linear.ElicitationInput{
			Body: "I couldn't match a repository — which one should I work in?",
		})
		return err
	}
	options := make([]linear.SelectOption, 0, len(suggestions))
	for _, s := range suggestions {
		option := repositoryOptionValue(s)
		options = append(options, linear.SelectOption{Value: option, Label: option})
	}
	_, err := la.PostElicitation(ctx, threadID, linear.ElicitationInput{
		Body:           "Which repository should I work in?",
		Signal:         "select",
		SignalMetadata: linear.SelectSignalMetadata{Options: options},
	})
	return err
}
```

Qualify option identities with everything needed to disambiguate them — here
the Git host, so `github.com/acme/backend` and `gitlab.example.com/acme/backend`
stay distinct choices.

The user's answer arrives as a **regular follow-up prompt** (routed to
`OnSubscribedMessage` on a subscribed thread): a picked option delivers the
option's `value` as the message text, but users may instead reply in free
text, which dismisses the elicitation. Match known values and let everything
else fall through to your normal prompt handling:

<!-- source: examples/linear-agent-hello-world/capabilities.go -->
```go
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
```

Since free-text replies are natural language, a production agent should
involve its LLM when interpreting an unmatched answer rather than failing.

Only interpret a follow-up as an answer while a choice is actually pending on
that thread — otherwise a later message that happens to equal an option value
would be misread as a selection. The example keeps a small take-once
per-thread registry (`pendingSelections` in `capabilities.go`), records the
offered values when it posts the elicitation, and consumes the pending state
on the next follow-up (take-once matches Linear dismissing the elicitation on
a free-text reply):

<!-- source: examples/linear-agent-hello-world/main.go -->
```go
		if optionValues, ok := pending.take(ev.Thread.ID()); ok {
			if handled, err := handleSelection(ctx, ev, optionValues); handled {
				return err
			}
		}
```

### Ask The User To Link An Account (Auth Elicitation)

An `auth`-signal elicitation makes Linear render an ephemeral "Link account"
button pointing at your auth flow. `signalMetadata.url` is your account
linking URL; the optional `userId` restricts the prompt to one Linear user:

<!-- source: examples/linear-agent-hello-world/capabilities.go -->
```go
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
```

The follow-up is the part that differs from `select`: **Linear sends no
webhook when the user completes the auth flow.** Your own auth callback is
the trigger. Store the session's `ThreadID` (for example, keyed by the OAuth
`state` parameter) before posting the elicitation, then resume from the
callback — a `thought` both resumes the session and dismisses the ephemeral
auth UI:

<!-- source: examples/linear-agent-hello-world/capabilities.go -->
```go
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
```

If the user replies in the session instead of completing the link, the reply
arrives as a normal follow-up prompt — handle it like any other message.

### Publish Progress Links With externalUrls

Surface a pull request or dashboard on the session as work progresses.
`AddExternalURLs` appends without replacing existing links (`ExternalURLs`
replaces the whole list); Linear also treats the update as session activity:

<!-- source: examples/linear-agent-hello-world/capabilities.go -->
```go
func publishPullRequest(ctx context.Context, la linearAgentAccess, threadID chat.ThreadID, prURL string) error {
	return la.UpdateSession(ctx, threadID, linear.AgentSessionUpdateInput{
		AddExternalURLs: []linear.ExternalURL{{URL: prURL, Label: "Pull request"}},
	})
}
```

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
itself in developer preview upstream, and some operations (for example issue
workflow automation) still require the `GraphQL` escape hatch rather than
typed helpers. The tracked list lives in
[`docs/linear-agent-capabilities.md`](../linear-agent-capabilities.md).
