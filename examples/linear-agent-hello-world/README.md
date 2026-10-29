# Linear Agent Hello World Example

## Overview

A small Linear agent with in-memory state. Mention the installed Linear app
in an issue, or delegate an issue to it, and Linear opens an agent session.
The example then:

1. subscribes the session thread;
2. posts an ephemeral thought, a tool-call action, and an external link;
3. posts a final response, or, if the prompt is exactly `deploy`, asks a
   `select` question instead;
4. answers follow-up prompts through `OnSubscribedMessage`, and stops
   cleanly when the user presses **Stop**.

It runs under deferred dispatch, so work can outlive the inbound webhook
request. It is an app-actor agent
([ADR 0008](../../docs/adr/0008-linear-full-adapter.md)), not a bot that uses
a personal API key.

If you enable Linear's `Comment` webhook category, it also replies to plain
issue comments that mention the app
([ADR 0013](../../docs/adr/0013-linear-generic-comments.md)): the comment
routes to `OnNewMention`, and `Thread.Post` replies with an ordinary issue
comment instead of an agent activity.

[`capabilities.go`](capabilities.go) holds the worked loops behind the
[Linear agent sessions guide](../../docs/how-to/linear-agent-sessions.md):
proactive sessions (`CreateSessionOnIssue`), repository suggestions paired
with a `select` question (`SuggestRepositories`), auth elicitation and the
resume after linking, select-answer handling, `externalUrls` updates, and
stop confirmation. [`capabilities_test.go`](capabilities_test.go) tests each
helper; the stop and select-answer helpers are wired into the running bot.

## Linear App Setup

Create or open a Linear OAuth application from Linear's API settings.

Configure the app for app-actor agent sessions:

1. Enable **Agent session events** for the app webhook configuration. Linear sends
   `AgentSessionEvent` `created` when the app is mentioned or delegated an issue.
2. Set the webhook URL to:

   ```text
   https://YOUR_PUBLIC_HOST/webhooks/linear
   ```

3. Install the app as an app actor with `actor=app`, `app:mentionable`, and
   `app:assignable`. The authorization URL should include scopes like:

   ```text
   read,write,app:mentionable,app:assignable
   ```

4. Copy the webhook signing secret. Use it as `LINEAR_WEBHOOK_SECRET`.
5. Copy the client credentials for the app actor client-credentials flow. Use
   them as `LINEAR_CLIENT_CREDENTIALS_CLIENT_ID` and
   `LINEAR_CLIENT_CREDENTIALS_CLIENT_SECRET`.

Generic issue/comment participation is opt-in (ADR 0013). To exercise it, also
enable the **Comment** (and as needed **Issue**) webhook category so Linear
delivers `Comment` events; the adapter normalizes a comment that mentions the
app actor into a Message Event and `Thread.Post` replies with an ordinary issue
comment. An agent-session-only deployment can leave these disabled and is
unchanged. Other valid webhook types, including Emoji reactions, Inbox
Notifications, and Permission Changes, are acknowledged and ignored.

Treat the webhook secret and client secret like passwords.

If assignment/delegation does not create an agent session but direct mentions do,
reinstall the app actor after confirming `app:assignable` is in the authorization
URL. Linear can keep stale install/app state after scope changes; during
dogfooding we had to delete and recreate the OAuth app before
assignment-created sessions started arriving.

## Expose Localhost

Linear requires a public HTTPS endpoint. For local development, expose port
`8080` with a tunnel such as Tailscale Funnel, `ngrok`, or `cloudflared` (see
[Use HTTP Instead Of Socket Mode in the Slack tutorial](../../docs/tutorials/slack-bot.md#use-http-instead-of-socket-mode)
for Funnel commands), then use that public HTTPS URL as `YOUR_PUBLIC_HOST`.

## Run

From the repository root:

```sh
export LINEAR_WEBHOOK_SECRET="..."
export LINEAR_CLIENT_CREDENTIALS_CLIENT_ID="..."
export LINEAR_CLIENT_CREDENTIALS_CLIENT_SECRET="..."
export PORT=8080

go run ./examples/linear-agent-hello-world
```

In Linear, mention the installed app actor in an issue or delegate the issue to
the app actor:

```text
@your-agent hello
```

Expected behavior:

1. Linear creates an agent session from the mention or delegation.
2. The example receives an `AgentSessionEvent` and routes it to `OnNewMention`.
3. The example subscribes the Linear agent session thread.
4. The app posts an ephemeral thought:

   ```text
   Thinking...
   ```

5. The app posts a `search-codebase` action and adds a **Draft PR** external
   link to the session.
6. The app posts the final response, a bold `hello from Linear app actor`
   line followed by a note inviting a follow-up prompt.

   If the prompt text is exactly `deploy`, the app instead asks a `select`
   question ("Which environment should I target?", `staging` or `prod`), and
   your next follow-up is read as the answer.

7. Send a follow-up prompt in the same Linear agent session.
8. The example routes it to `OnSubscribedMessage`, posts the ephemeral thought
   `Reading your follow-up...`, and replies with:

   ```text
   Follow-up received: YOUR_MESSAGE
   ```

   A follow-up carrying Linear's stop signal gets a stop confirmation instead.

## Notes

- State is in memory, so subscriptions and dedupe data are lost when the process
  exits.
- Use Redis, Postgres, or NATS JetStream runtime state for production
  deployments.
- Linear request signatures are verified with `LINEAR_WEBHOOK_SECRET`.
- Client credentials are exchanged during adapter startup and refreshed lazily
  before Linear API calls.
- Linear thoughts, actions, elicitations, errors, session updates, and the raw
  GraphQL escape hatch are exposed through typed adapter access
  (`chat.AdapterAs[*linear.Adapter]`) rather than a generic runtime API.
- Inbound signals (including `stop`) and structured session context are preserved
  on `Message.Raw`; read them with `linear.RawMessageFrom`.
- This example runs under `chat.DispatchDeferred`, so the webhook
  acknowledgement does not wait for follow-up work, and the runtime holds and
  renews the thread lock while that work runs.

## Interface

`package main` in the root module, built from [`main.go`](main.go) and
[`capabilities.go`](capabilities.go). It takes no CLI flags. It imports
`github.com/coder/chat`, `github.com/coder/chat/adapters/linear`, and
`github.com/coder/chat/state/memory` [main.go:main].

| Symbol | Role | Source |
| --- | --- | --- |
| `main()` | Builds the Linear adapter and the `chat.Chat` runtime, registers both handlers, and serves the webhook. | [main.go:main] |
| `/webhooks/linear` | The only HTTP route. It serves `bot.Webhook("linear")`, which answers 405 to any method but POST. | [main.go:newWebhookServer] |
| `linearAgentAccess` | Interface over `PostThought`, `PostAction`, `PostElicitation`, `PostError`, `UpdateSession`, `CreateSessionOnIssue`, and `SuggestRepositories`. `*linear.Adapter` and a test fake satisfy it. | [main.go:linearAgentAccess] |
| `newMentionHandler` | `OnNewMention` handler: comment reply, or subscribe, thought, action, external URL, then a final response or a `select` elicitation. | [main.go:newMentionHandler] |
| `newFollowUpHandler` | `OnSubscribedMessage` handler: stop confirmation, then a pending select answer, then a normal follow-up reply. | [main.go:newFollowUpHandler] |
| `shouldAsk` | Returns true when the prompt text is `deploy`. | [main.go:shouldAsk] |
| `newWebhookServer` | Builds the `*http.Server` with a `ServeMux` and fixed timeouts. | [main.go:newWebhookServer] |
| `mustEnv` | Reads a required environment variable. | [main.go:mustEnv] |
| `selectionKindDeploy`, `selectionKindRepository` | Kinds of pending select elicitation (`"deploy"`, `"repository"`). | [capabilities.go:selectionKindDeploy], [capabilities.go:selectionKindRepository] |
| `pendingSelection`, `pendingSelections`, `newPendingSelections` | Per-thread record of the offered select values, with `set` and `take`. | [capabilities.go:pendingSelections] |
| `startProactiveSession` | Creates a session on an issue with `CreateSessionOnIssue`, seeds a `Run dashboard` URL, and posts a first thought. | [capabilities.go:startProactiveSession] |
| `chooseRepository`, `bestSuggestion`, `offerRepositoryChoice`, `repositoryOptionValue` | Repository suggestion loop with `SuggestRepositories` and a `select` fallback. | [capabilities.go:chooseRepository] |
| `handleSelection` | Matches a follow-up to an offered value and posts an acknowledgement for the selection kind. | [capabilities.go:handleSelection] |
| `requireAccountLink`, `resumeAfterAccountLink` | `auth` elicitation, and the resume that the application's own auth callback calls. | [capabilities.go:requireAccountLink], [capabilities.go:resumeAfterAccountLink] |
| `publishPullRequest` | Adds a `Pull request` external URL with `AddExternalURLs`. | [capabilities.go:publishPullRequest] |
| `confirmStop` | Detects `linear.RawMessageFrom(...).StopRequested()` and posts a stop confirmation. | [capabilities.go:confirmStop] |

The running bot calls `confirmStop` and `handleSelection` from
`newFollowUpHandler` [main.go:newFollowUpHandler]. The other
`capabilities.go` helpers are not called from `main.go`.

## Invariants

- `LINEAR_WEBHOOK_SECRET`, `LINEAR_CLIENT_CREDENTIALS_CLIENT_ID`, and
  `LINEAR_CLIENT_CREDENTIALS_CLIENT_SECRET` must be non-empty. Otherwise
  `mustEnv` panics with `<NAME> is required` [main.go:mustEnv].
- An error from `linear.New`, `chat.New`, `bot.Webhook("linear")`, or
  `ListenAndServe` panics. A missing adapter panics with
  `linear adapter is not registered` [main.go:main].
- The runtime uses `chat.DispatchDeferred` with `DedupeTTL` 24h,
  `ThreadLockTTL` 2m, `DetachTimeout` 5m, and `MaxDetached` 1024. The code
  comment says deferred dispatch requires this admission bound [main.go:main].
- A mention on a `comment`-kind thread gets one plain reply and returns before
  `Thread.Subscribe` [main.go:newMentionHandler].
- On an agent session, the handler subscribes the thread before it posts any
  activity [main.go:newMentionHandler].
- If the final response fails to post, the handler posts an error activity and
  unsubscribes with `context.WithoutCancel(ctx)` [main.go:newMentionHandler].
- A stop request is handled first and drops the pending selection for the
  thread [main.go:newFollowUpHandler].
- Each follow-up consumes the pending selection. The handler registers it again
  only when a matched choice fails to post, because deferred dispatch does not
  redeliver a handler error [main.go:newFollowUpHandler].
- `handleSelection` matches only `strings.TrimSpace(text)` equal to an offered
  value. Other text returns `handled=false` [capabilities.go:handleSelection].
- `chooseRepository` skips the question only when the best suggestion has
  `Confidence >= 0.8` [capabilities.go:chooseRepository].
- `offerRepositoryChoice` records a pending selection only for a non-empty
  shortlist whose elicitation posted without error
  [capabilities.go:offerRepositoryChoice].
- With an empty shortlist, `offerRepositoryChoice` posts a free-text
  elicitation with no `select` signal and records no pending selection
  [capabilities.go:offerRepositoryChoice].
- `startProactiveSession` returns the created thread ID with the error when the
  first thought fails, so a caller does not create a duplicate session
  [capabilities.go:startProactiveSession].
- `pendingSelections` guards its map with a `sync.Mutex`
  [capabilities.go:pendingSelections].

## Side Effects

| Environment variable | Default | Use | Source |
| --- | --- | --- | --- |
| `LINEAR_WEBHOOK_SECRET` | none (required) | `linear.Options.WebhookSecret` | [main.go:main] |
| `LINEAR_CLIENT_CREDENTIALS_CLIENT_ID` | none (required) | `linear.ClientCredentials.ClientID` | [main.go:main] |
| `LINEAR_CLIENT_CREDENTIALS_CLIENT_SECRET` | none (required) | `linear.ClientCredentials.ClientSecret` | [main.go:main] |
| `PORT` | `8080` | Listen address `":" + PORT` | [main.go:main] |

- Network: `http.Server.ListenAndServe` on `:$PORT`, with
  `ReadHeaderTimeout` 5s, `ReadTimeout` 10s, `WriteTimeout` 10s, and
  `IdleTimeout` 60s [main.go:newWebhookServer].
- Network: Linear API calls through `linearAgentAccess` (thoughts, actions,
  elicitations, errors, session updates) and `Thread.Post`
  [main.go:newMentionHandler], [main.go:newFollowUpHandler].
- External URL: each agent-session mention sets the `Draft PR` link
  `https://example.com/pr/1` on the session [main.go:newMentionHandler].
- State: runtime state lives in `memory.New()`, and pending selections live in
  a process map. Both are lost on exit [main.go:main],
  [capabilities.go:pendingSelections].
- Logging: `slog.Default()` goes to the adapter and the runtime. `main` logs
  `listening` and `chat shutdown failed` [main.go:main].
- Lifecycle: a deferred `bot.Shutdown(context.Background())` runs when `main`
  returns or panics. `main` installs no signal handler [main.go:main].

## Verification

From the repository root:

- Run: `go test ./examples/linear-agent-hello-world/...`
- Run: `go vet ./examples/linear-agent-hello-world/...`
