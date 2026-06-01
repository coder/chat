# Slack NATS State Example

This example runs a tiny Slack bot with NATS JetStream-backed runtime state. When
the bot is mentioned in a channel, or messaged directly if you enable the DM
event, it subscribes the thread and replies. Later messages in that subscribed
thread are routed through JetStream-backed state.

This adapter requires a NATS server with **JetStream enabled** (NATS Server 2.9
or newer).

## Slack App Setup

Create or open a Slack app from the Slack app dashboard:

https://api.slack.com/apps

In **OAuth & Permissions**, add these **Bot Token Scopes**:

| Scope | Why this example needs it |
| --- | --- |
| `chat:write` | Send replies with `chat.postMessage`. |
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
with a tunnel such as `ngrok`, `cloudflared`, or Tailscale Funnel, then use that
public HTTPS URL as `YOUR_PUBLIC_HOST`.

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

## Run NATS

From this example directory:

```sh
cd examples/slack-nats-state
docker compose up -d nats
```

Or let Pitchfork supervise the service:

```sh
cd examples/slack-nats-state
pitchfork start nats
```

The NATS URL for the local Compose service is:

```text
nats://127.0.0.1:42220
```

To stop NATS:

```sh
docker compose down
```

To delete the JetStream volume:

```sh
docker compose down -v
```

## Run The Bot

From this example directory:

```sh
export SLACK_SIGNING_SECRET="..."
export SLACK_BOT_TOKEN="xoxb-..."
export NATS_URL="nats://127.0.0.1:42220"
export PORT=8080

go run .
```

In Slack, mention the bot in a channel where it is present:

```text
@your-bot hello
```

The bot replies in the same thread and subscribes the thread:

```text
hello world from NATS state. This thread is now subscribed.
```

Send another message in the same Slack thread. The bot should route it as a
subscribed message and reply:

```text
NATS remembered this subscribed thread.
```

## Notes

- Runtime state is stored in NATS JetStream Key-Value buckets, so subscriptions,
  dedupe data, and locks can survive process restarts while JetStream storage is
  retained.
- The adapter creates three buckets: `slack-example_sub` (never expires),
  `slack-example_event` (dedupe TTL), and `slack-example_lock` (lock TTL).
- Slack URL verification is handled by the Slack adapter at `/webhooks/slack`.
- Slack request signatures are verified with `SLACK_SIGNING_SECRET`.
- The bot token is used for `auth.test` during adapter startup and
  `chat.postMessage` when replying.

Relevant docs:

- NATS JetStream Key-Value store: https://docs.nats.io/nats-concepts/jetstream/key-value-store
- Slack app dashboard: https://api.slack.com/apps
- Events API request URLs: https://docs.slack.dev/apis/events-api/
- `app_mention` event: https://docs.slack.dev/reference/events/app_mention/
- `message.im` event: https://docs.slack.dev/reference/events/message.im
- App Home Messages tab: https://docs.slack.dev/surfaces/app-home
- `chat:write` scope: https://docs.slack.dev/reference/scopes/chat.write
- Bot tokens and `xoxb-` tokens: https://docs.slack.dev/authentication/tokens
