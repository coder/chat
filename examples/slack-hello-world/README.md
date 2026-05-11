# Slack Hello World Example

This example runs a tiny Slack bot with in-memory runtime state. When the bot is
mentioned in a channel, or messaged directly if you enable the DM event, it
replies with `hello world`.

## Slack App Setup

Create or open a Slack app from the Slack app dashboard:

https://api.slack.com/apps

In **OAuth & Permissions**, add these **Bot Token Scopes**:

| Scope | Why this example needs it |
| --- | --- |
| `chat:write` | Send `hello world` with `chat.postMessage`. |
| `app_mentions:read` | Receive `app_mention` events when the bot is mentioned. |
| `im:history` | Optional for channel-only testing, required if you subscribe to `message.im` so direct messages reach the bot. |

This example does not use ephemeral DM fallback. If you change it to call
`PostEphemeral` with `FallbackToDM: true`, also add `im:write` so the adapter can
open a DM with `conversations.open`.

In **App Home**:

1. Under **Show Tabs**, enable the **Messages Tab**.
2. Make the Messages tab writable. Depending on Slack's current UI, this appears
   as disabling read-only mode or enabling **Allow users to send Slash commands
   and messages from the messages tab**.

If Slack shows **"Sending messages to this app has been turned off"** in the app
DM, this App Home Messages tab setting is still off or read-only.

In **Event Subscriptions**:

1. Enable events.
2. Set the request URL to:

   ```text
   https://YOUR_PUBLIC_HOST/webhooks/slack
   ```

3. Subscribe to these **Bot User Events**:

   ```text
   app_mention
   message.im
   ```

4. Save changes.

After changing scopes, App Home settings, or event subscriptions, reinstall the
app from **OAuth & Permissions** so Slack applies the new configuration to your
workspace.

The app must be reachable from Slack. For local development, expose port `8080`
with a tunnel such as `ngrok`, `cloudflared`, or another HTTPS forwarding tool,
then use that public HTTPS URL as `YOUR_PUBLIC_HOST`.

### Expose Localhost With Tailscale Funnel

Tailscale has two similar commands:

- `tailscale serve` shares a local service only inside your tailnet.
- `tailscale funnel` shares a local service on the public internet.

Slack needs a public HTTPS URL, so use Funnel rather than Serve.

First, make sure Tailscale is installed, running, and signed in. Funnel must be
enabled for your tailnet; the CLI may prompt you to approve this in the Tailscale
admin console.

Start the example on port `8080`:

```sh
export SLACK_SIGNING_SECRET="..."
export SLACK_BOT_TOKEN="xoxb-..."
export PORT=8080

go run ./examples/slack-hello-world
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

Use that URL in Slack's **Event Subscriptions** request URL:

```text
https://your-machine.your-tailnet.ts.net/webhooks/slack
```

When you are done, turn the public endpoint off:

```sh
tailscale funnel reset
```

## Install And Get Credentials

1. Go to **OAuth & Permissions**.
2. Click **Install to Workspace** or **Reinstall to Workspace** after changing
   scopes, App Home settings, or event subscriptions.
3. Copy **Bot User OAuth Token**. It starts with `xoxb-`; use it as
   `SLACK_BOT_TOKEN`.
4. Go to **Basic Information**.
5. In **App Credentials**, copy **Signing Secret**; use it as
   `SLACK_SIGNING_SECRET`.

Treat both values like passwords.

## Run

From the repository root:

```sh
export SLACK_SIGNING_SECRET="..."
export SLACK_BOT_TOKEN="xoxb-..."
export PORT=8080

go run ./examples/slack-hello-world
```

In Slack, mention the bot in a channel where it is present:

```text
@your-bot hello
```

The bot should reply in the same thread:

```text
hello world
```

If you enabled `message.im`, you can also send a direct message to the bot.

## Notes

- State is in memory, so subscriptions and dedupe data are lost when the process
  exits.
- Slack URL verification is handled by the Slack adapter at `/webhooks/slack`.
- Slack request signatures are verified with `SLACK_SIGNING_SECRET`.
- The bot token is used for `auth.test` during adapter startup and
  `chat.postMessage` when replying.

Relevant Slack docs:

- App dashboard: https://api.slack.com/apps
- Events API request URLs: https://docs.slack.dev/apis/events-api/
- `app_mention` event: https://docs.slack.dev/reference/events/app_mention/
- `message.im` event: https://docs.slack.dev/reference/events/message.im
- App Home Messages tab: https://docs.slack.dev/surfaces/app-home
- `chat:write` scope: https://docs.slack.dev/reference/scopes/chat.write
- `conversations.open` scopes: https://docs.slack.dev/reference/methods/conversations.open/
- Bot tokens and `xoxb-` tokens: https://docs.slack.dev/authentication/tokens
