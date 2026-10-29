# Slack Hello World Example

## Overview

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

## Interface

`package main` in the root module, built from [`main.go`](main.go). It takes no
CLI flags. It imports `github.com/coder/chat`,
`github.com/coder/chat/adapters/slack`, and
`github.com/coder/chat/state/memory` [main.go:main].

| Symbol | Role | Source |
| --- | --- | --- |
| `main()` | Reads the environment, builds the bot, and calls `serve`, which returns on `SIGINT`, `SIGTERM`, or the first transport error. `main` panics on a returned error. | [main.go:main] |
| `newBot` | Builds the Slack adapter and a `chat.Chat` with `memory.New()`. Registers an `OnNewMention` handler that posts `chat.Markdown("**hello** _world_")`. | [main.go:newBot] |
| `serve` | Starts Socket Mode (`bot.Listen`) when the app token is set, and the webhook server when the signing secret is set. Then it shuts down both. | [main.go:serve] |
| `shutdown` | Calls a stop function with a 10-second timeout context. | [main.go:shutdown] |
| `serveWebhooks` | Runs `ListenAndServe` and treats `http.ErrServerClosed` as a clean exit. | [main.go:serveWebhooks] |
| `/webhooks/slack` | The only HTTP route. It serves `bot.Webhook("slack")`, which answers 405 to any method but POST. | [main.go:newWebhookServer] |
| `newWebhookServer` | Builds the `*http.Server` with a `ServeMux` and fixed timeouts. | [main.go:newWebhookServer] |
| `mustEnv` | Reads a required environment variable. | [main.go:mustEnv] |
| `mustAllowDemoMemoryState` | Requires `CHAT_DEMO_IN_MEMORY_STATE=1`. | [main.go:mustAllowDemoMemoryState] |

## Invariants

- `CHAT_DEMO_IN_MEMORY_STATE` must equal `1`. Otherwise `main` panics with
  `CHAT_DEMO_IN_MEMORY_STATE=1 is required` [main.go:mustAllowDemoMemoryState].
- `SLACK_BOT_TOKEN` must be non-empty. Otherwise `mustEnv` panics with
  `SLACK_BOT_TOKEN is required` [main.go:mustEnv].
- An error from `newBot` or `serve` panics [main.go:main]. `slack.New` returns
  an error when both `SLACK_APP_TOKEN` and `SLACK_SIGNING_SECRET` are empty
  (check in `github.com/coder/chat/adapters/slack`) [main.go:newBot].
- Socket Mode runs only when `opts.AppToken != ""`. The webhook server runs
  only when `opts.SigningSecret != ""` [main.go:serve].
- `serve` stops on the first transport error or on context cancellation. It
  shuts down the webhook server before the runtime, because the runtime does
  not wait for webhook requests in sync dispatch mode [main.go:serve].
- `bot.Listen` gets `context.Background()`, so only `bot.Shutdown` stops it
  [main.go:serve].
- Each shutdown step gets its own 10-second deadline [main.go:shutdown].
- `serve` joins the first error, both shutdown errors, and the errors of the
  other running transports [main.go:serve].

## Side Effects

| Environment variable | Default | Use | Source |
| --- | --- | --- | --- |
| `CHAT_DEMO_IN_MEMORY_STATE` | none (must be `1`) | Opt-in to in-memory state | [main.go:mustAllowDemoMemoryState] |
| `SLACK_BOT_TOKEN` | none (required) | `slack.Options.BotToken` | [main.go:main] |
| `SLACK_APP_TOKEN` | empty (Socket Mode off) | `slack.Options.AppToken` | [main.go:main] |
| `SLACK_SIGNING_SECRET` | empty (HTTP off) | `slack.Options.SigningSecret` | [main.go:main] |
| `PORT` | `8080` | Listen address `":" + PORT` | [main.go:main] |

- Signals: `signal.NotifyContext` cancels the context on `os.Interrupt` and
  `syscall.SIGTERM` [main.go:main].
- Network: an outbound Socket Mode connection through
  `bot.Listen(context.Background(), "slack")` [main.go:serve].
- Network: `ListenAndServe` on `:$PORT` with `ReadHeaderTimeout` 5s,
  `ReadTimeout` 10s, `WriteTimeout` 10s, and `IdleTimeout` 60s
  [main.go:serveWebhooks], [main.go:newWebhookServer].
- Network: `slack.New` builds the adapter, and each mention posts a reply with
  `Thread.Post` [main.go:newBot].
- Goroutines: one for `bot.Listen` and one for `serveWebhooks`, each only when
  its transport is set [main.go:serve].
- State: `memory.New()`. State is lost on exit [main.go:newBot].
- Logging: `slog.Default()` goes to the adapter and the runtime.
  `serveWebhooks` logs `serving Slack webhooks` [main.go:main],
  [main.go:serveWebhooks].

## Verification

From the repository root:

- Run: `go test ./examples/slack-hello-world/...`
- Run: `go vet ./examples/slack-hello-world/...`
