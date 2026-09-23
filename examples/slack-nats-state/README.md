# Slack NATS State Example

This example runs a tiny Slack bot with NATS JetStream-backed runtime state. When
the bot is mentioned in a channel, or messaged directly if you enable the DM
event, it subscribes the thread and replies. Later messages in that subscribed
thread are routed through JetStream-backed state.

This adapter requires a NATS server with **JetStream enabled** (NATS Server 2.9
or newer).

## Slack App Setup

Set up a Slack app as in the [tutorial](../../docs/tutorials/slack-bot.md):
Steps 2–3 create the app and collect credentials, Step 5 exposes your
machine, and Step 6 subscribes to events. This example also replies to follow-up messages in
subscribed threads, so add Step 8's `channels:history` scope and
`message.channels` event too.

| Setting | Value |
| --- | --- |
| Bot token scopes | `chat:write`, `app_mentions:read`, `channels:history` (add `im:history` for direct messages) |
| Bot events | `app_mention`, `message.channels` (add `message.im` for direct messages) |
| Request URL | `https://YOUR_PUBLIC_HOST/webhooks/slack` |
| `SLACK_BOT_TOKEN` | **OAuth & Permissions** → Bot User OAuth Token (`xoxb-…`) |
| `SLACK_SIGNING_SECRET` | **Basic Information** → App Credentials → Signing Secret |

Reinstall the app from **OAuth & Permissions** after changing scopes or
events.

## Run NATS

From the repository root:

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
