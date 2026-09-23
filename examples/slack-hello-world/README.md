# Slack Hello World Example

This example runs a tiny Slack bot with in-memory runtime state. When the bot is
mentioned in a channel, or messaged directly if you enable the DM event, it
replies with portable Markdown: `**hello** _world_`.

This example requires `CHAT_DEMO_IN_MEMORY_STATE=1` because state is lost on
restart. Use `examples/slack-redis-state`, `examples/slack-postgres-state`, or
`examples/slack-nats-state` for durable Slack apps.

## Slack App Setup

Set up a Slack app as in the [tutorial](../../docs/tutorials/slack-bot.md):
Steps 2–3 create the app and collect credentials, Step 5 exposes your
machine, and Step 6 subscribes to events.

| Setting | Value |
| --- | --- |
| Bot token scopes | `chat:write`, `app_mentions:read` (add `im:history` for direct messages) |
| Bot events | `app_mention` (add `message.im` for direct messages) |
| Request URL | `https://YOUR_PUBLIC_HOST/webhooks/slack` |
| `SLACK_BOT_TOKEN` | **OAuth & Permissions** → Bot User OAuth Token (`xoxb-…`) |
| `SLACK_SIGNING_SECRET` | **Basic Information** → App Credentials → Signing Secret |

Reinstall the app from **OAuth & Permissions** after changing scopes or
events.

## Run

From the repository root:

```sh
export SLACK_SIGNING_SECRET="..."
export SLACK_BOT_TOKEN="xoxb-..."
export CHAT_DEMO_IN_MEMORY_STATE=1
export PORT=8080

go run ./examples/slack-hello-world
```

In Slack, mention the bot in a channel where it is present:

```text
@your-bot hello
```

The bot should reply in the same thread with portable Markdown:

```text
**hello** _world_
```

If you enabled `message.im`, you can also send a direct message to the bot.

## Notes

- State is in memory, so subscriptions and dedupe data are lost when the process
  exits. This example is intended for local demos.
- Slack URL verification is handled by the Slack adapter at `/webhooks/slack`.
- Slack request signatures are verified with `SLACK_SIGNING_SECRET`.
- The bot token is used for `auth.test` during adapter startup and
  `chat.postMessage` when replying.
