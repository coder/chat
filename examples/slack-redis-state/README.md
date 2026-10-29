# Slack Redis State Example

## Overview

This example runs a tiny Slack bot with Redis-backed runtime state. When the bot
is mentioned in a channel, or messaged directly if you enable the DM event, it
subscribes the thread and replies. Later messages in that subscribed thread are
routed through Redis-backed state.

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

## Run Redis

From the repository root:

```sh
cd examples/slack-redis-state
docker compose up -d redis
```

Or let Pitchfork supervise the service:

```sh
cd examples/slack-redis-state
pitchfork start redis
```

The Redis URL for the local Compose service is:

```text
redis://127.0.0.1:63800/0
```

To stop Redis:

```sh
docker compose down
```

To delete the Redis volume:

```sh
docker compose down -v
```

## Run The Bot

From this example directory:

```sh
export SLACK_SIGNING_SECRET="..."
export SLACK_BOT_TOKEN="xoxb-..."
export REDIS_URL="redis://127.0.0.1:63800/0"
export PORT=8080

go run .
```

In Slack, mention the bot in a channel where it is present:

```text
@your-bot hello
```

The bot replies in the same thread and subscribes the thread:

```text
hello world from Redis state. This thread is now subscribed.
```

Send another message in the same Slack thread. The bot should route it as a
subscribed message and reply:

```text
Redis remembered this subscribed thread.
```

## Notes

- Runtime state is stored in Redis, so subscriptions, dedupe data, and locks can
  survive process restarts while Redis is still running.
- Slack URL verification is handled by the Slack adapter at `/webhooks/slack`.
- Slack request signatures are verified with `SLACK_SIGNING_SECRET`.
- The bot token is used for `auth.test` during adapter startup and
  `chat.postMessage` when replying.

## Interface

`package main` in its own module, `github.com/coder/chat/examples/slack-redis-state`
[go.mod:module]. It is built from [`main.go`](main.go) and takes no CLI flags.
It imports `github.com/coder/chat`, `github.com/coder/chat/adapters/slack`,
`github.com/coder/chat/state/redis` (as `chatredis`), and `github.com/redis/go-redis/v9` [main.go:main].

| Symbol | Role | Source |
| --- | --- | --- |
| `main()` | Connects to Redis, builds the state, the Slack adapter, and the `chat.Chat` runtime, registers both handlers, and serves the webhook. | [main.go:main] |
| `OnNewMention` handler | Subscribes the thread, then posts `chat.Markdown("**hello** _world_ from Redis state. This thread is now subscribed.")`. | [main.go:main] |
| `OnSubscribedMessage` handler | Posts `chat.Markdown("Redis remembered this subscribed thread.")`. | [main.go:main] |
| `/webhooks/slack` | The only HTTP route, on `http.DefaultServeMux`. It serves `bot.Webhook("slack")`, which answers 405 to any method but POST. | [main.go:main] |
| `mustEnv` | Reads a required environment variable. | [main.go:mustEnv] |
| [`compose.yaml`](compose.yaml) | Local `redis` service and volume. | [compose.yaml:services.redis] |
| [`pitchfork.toml`](pitchfork.toml) | Pitchfork daemon `redis` that wraps the Compose service. | [pitchfork.toml:daemons.redis] |

## Invariants

- `REDIS_URL`, `SLACK_SIGNING_SECRET`, and `SLACK_BOT_TOKEN` must be
  non-empty. Otherwise `mustEnv` panics with `<NAME> is required`
  [main.go:mustEnv].
- An error from `goredis.ParseURL`, `chatredis.New`, `slack.New`, `chat.New`,
  `bot.Webhook("slack")`, or `http.ListenAndServe` panics [main.go:main].
- The state keys use the `slack-example` namespace: `chatredis.New` with `Client` and `Prefix: "slack-example"`
  [main.go:main].
- The mention handler subscribes the thread before it posts the reply
  [main.go:main].
- `go.mod` replaces `github.com/coder/chat` with `../..` and
  `github.com/coder/chat/state/redis` with `../../state/redis`, so the example builds against
  the local tree [go.mod:replace].

## Side Effects

| Environment variable | Default | Use | Source |
| --- | --- | --- | --- |
| `REDIS_URL` | none (required) | Redis connection | [main.go:main] |
| `SLACK_SIGNING_SECRET` | none (required) | `slack.Options.SigningSecret` | [main.go:main] |
| `SLACK_BOT_TOKEN` | none (required) | `slack.Options.BotToken` | [main.go:main] |
| `PORT` | `8080` | Listen address `":" + PORT` | [main.go:main] |

- Network: `goredis.ParseURL(REDIS_URL)` and `goredis.NewClient` build a Redis client [main.go:main]. `main` does not close it directly: the deferred `bot.Shutdown` calls `State.Shutdown`, which closes it [main.go:main], [`state/redis` `State.Shutdown`].
- Network: `http.ListenAndServe` on `:$PORT` with the default server, so no
  server timeouts are set [main.go:main].
- Network: `slack.New` builds the Slack adapter, and each handler posts a reply
  with `Thread.Post` [main.go:main].
- State: runtime state goes to Redis through `chatredis.New` [main.go:main].
- Logging: `slog.Default()` goes to the adapter and the runtime. `main` logs
  `listening` and `chat shutdown failed` [main.go:main].
- Lifecycle: a deferred `bot.Shutdown(context.Background())` runs when `main`
  returns or panics. `main` installs no signal handler [main.go:main].
- Compose service `redis`: image `redis:7.2-alpine`, port `127.0.0.1:63800:6379`, volume `redis-data` at `/data` [compose.yaml:services.redis].
- Compose healthcheck: `redis-cli ping` every 2s, timeout 5s, 30 retries [compose.yaml:services.redis.healthcheck].
- Pitchfork daemon `redis` in namespace `chat-sdk-go-slack-redis-state`: runs `docker compose up redis`, waits on `docker compose exec -T redis redis-cli ping`, and has `auto = ["start", "stop"]` [pitchfork.toml:daemons.redis].

## Verification

This example is a separate Go module with no `_test.go` files, so `go test`
only builds it. From the repository root:

- Run: `cd examples/slack-redis-state && go test ./...`
- Run: `cd examples/slack-redis-state && go vet ./...`
