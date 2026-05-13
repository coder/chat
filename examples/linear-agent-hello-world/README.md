# Linear Agent Hello World Example

This example runs a tiny Linear app-actor bot with in-memory runtime state. When
someone mentions the installed Linear app in an issue, or delegates an issue to
the app, Linear creates an agent session and sends an `AgentSessionEvent`
webhook. The example subscribes the session thread, posts an ephemeral thought,
posts a final response, and then routes follow-up prompts to
`OnSubscribedMessage`.

This is a Linear app-actor example, not a personal API key or generic issue
comment bot.

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

Do not enable Comments, Issues, or Emoji reaction webhook categories for this
example unless you are experimenting. The MVP adapter handles agent session
webhooks; other valid webhook types, including Inbox Notifications and
Permission Changes, are acknowledged and ignored.

Treat the webhook secret and client secret like passwords.

If assignment/delegation does not create an agent session but direct mentions do,
reinstall the app actor after confirming `app:assignable` is in the authorization
URL. Linear can keep stale install/app state after scope changes; during
dogfooding we had to delete and recreate the OAuth app before
assignment-created sessions started arriving.

## Expose Localhost

Linear requires a public HTTPS endpoint. For local development, expose port
`8080` with a tunnel such as `ngrok`, `cloudflared`, Tailscale Funnel, or another
HTTPS forwarding tool, then use that public HTTPS URL as `YOUR_PUBLIC_HOST`.

### Expose Localhost With Tailscale Funnel

Tailscale has two similar commands:

- `tailscale serve` shares a local service only inside your tailnet.
- `tailscale funnel` shares a local service on the public internet.

Linear needs a public HTTPS URL, so use Funnel rather than Serve.

Start the example on port `8080`:

```sh
export LINEAR_WEBHOOK_SECRET="..."
export LINEAR_CLIENT_CREDENTIALS_CLIENT_ID="..."
export LINEAR_CLIENT_CREDENTIALS_CLIENT_SECRET="..."
export PORT=8080

go run ./examples/linear-agent-hello-world
```

In another terminal, expose it with Funnel:

```sh
tailscale funnel --bg --https=443 localhost:8080
tailscale funnel status
```

The status output should show a public HTTPS URL similar to:

```text
https://your-machine.your-tailnet.ts.net
```

Use that URL in Linear's webhook settings:

```text
https://your-machine.your-tailnet.ts.net/webhooks/linear
```

When you are done, turn the public endpoint off:

```sh
tailscale funnel reset
```

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

5. The app posts the final response:

   ```text
   hello from Linear app actor
   ```

6. Send a follow-up prompt in the same Linear agent session.
7. The example routes it to `OnSubscribedMessage`, posts another ephemeral
   thought, and replies with:

   ```text
   Follow-up received: YOUR_MESSAGE
   ```

## Dogfooding Evidence

Before claiming a live Linear dogfood passed, capture screenshots or video of:

- the Linear app actor settings;
- the webhook configuration with agent session events enabled;
- the first app mention and created agent session;
- the ephemeral thought and final response;
- a follow-up prompt and follow-up thought/response.

## Notes

- State is in memory, so subscriptions and dedupe data are lost when the process
  exits.
- Use Redis or Postgres runtime state for production deployments.
- Linear request signatures are verified with `LINEAR_WEBHOOK_SECRET`.
- Client credentials are exchanged during adapter startup and refreshed lazily
  before Linear API calls.
- Linear thoughts are exposed through typed adapter access rather than a generic
  runtime typing API.
