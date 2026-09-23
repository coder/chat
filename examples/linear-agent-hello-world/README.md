# Linear Agent Hello World Example

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
[Step 5 of the Slack tutorial](../../docs/tutorials/slack-bot.md#step-5-expose-the-bot-to-slack)
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
