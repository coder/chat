# Slack Hello World Example

This example runs a tiny Slack bot with in-memory runtime state. When the bot is
mentioned in a channel, or messaged directly if you enable the DM event, it
replies with portable Markdown: `**hello** _world_`.

This example requires `CHAT_DEMO_IN_MEMORY_STATE=1` because state is lost on
restart. Use `examples/slack-redis-state`, `examples/slack-postgres-state`, or
`examples/slack-nats-state` for durable Slack apps.

## Transports

The bot receives Slack events over Socket Mode, over HTTP, or over both. Set
the environment variable of each transport that you want:

| Variable | Transport |
| --- | --- |
| `SLACK_APP_TOKEN` | Socket Mode. The bot opens a websocket to Slack with `Chat.Listen`, so it needs no public URL. |
| `SLACK_SIGNING_SECRET` | HTTP. The bot serves the Slack webhook on `http://localhost:$PORT/webhooks/slack`, and Slack must reach it over public HTTPS. |

Set at least one of them. With both, the bot runs both transports, and it
stops when one of them fails.

## Slack App Setup

Set up a Slack app as in the [tutorial](../../docs/tutorials/slack-bot.md).
Steps 2 and 3 create the app from a manifest with Socket Mode on, and collect
the credentials. The section
[Use HTTP Instead Of Socket Mode](../../docs/tutorials/slack-bot.md#use-http-instead-of-socket-mode)
sets up the HTTP transport.

| Setting | Value |
| --- | --- |
| Bot token scopes | `chat:write`, `app_mentions:read` (add `im:history` for direct messages) |
| Bot events | `app_mention` (add `message.im` for direct messages) |
| `SLACK_BOT_TOKEN` | **OAuth & Permissions** → Bot User OAuth Token (`xoxb-…`) |
| `SLACK_APP_TOKEN` | **Basic Information** → App-Level Tokens, a token with the `connections:write` scope (`xapp-…`). Socket Mode must be on in **Socket Mode**. |
| `SLACK_SIGNING_SECRET` | **Basic Information** → App Credentials → Signing Secret. Only for HTTP. |
| Request URL | `https://YOUR_PUBLIC_HOST/webhooks/slack`. Only for HTTP. |

Reinstall the app from **OAuth & Permissions** after changing scopes or
events.

## Run

From the repository root, with Socket Mode:

```sh
export SLACK_APP_TOKEN="xapp-..."
export SLACK_BOT_TOKEN="xoxb-..."
export CHAT_DEMO_IN_MEMORY_STATE=1

go run ./examples/slack-hello-world
```

Or with HTTP:

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

Press `Ctrl-C` to stop the bot. It shuts down its HTTP server first, and
waits for the requests that run. Then it shuts down the runtime with
`Chat.Shutdown`. The runtime stops acking new Socket Mode events, and then
closes the Socket Mode connection.

## Notes

- State is in memory, so subscriptions and dedupe data are lost when the process
  exits. This example is intended for local demos.
- Both transports give each Slack payload to the same adapter pipeline, so the
  bot behaves the same over each of them.
- Over HTTP, the Slack adapter answers Slack URL verification at
  `/webhooks/slack`, and verifies request signatures with
  `SLACK_SIGNING_SECRET`.
- The bot token is used for `auth.test` during adapter startup and
  `chat.postMessage` when replying.
