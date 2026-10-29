# Slack Postgres State Example

## Overview

This example runs a tiny Slack bot with Postgres-backed runtime state. When the
bot is mentioned in a channel, or messaged directly if you enable the DM event,
it subscribes the thread and replies. Later messages in that subscribed thread
are routed through Postgres-backed state.

## Slack App Setup

Set up a Slack app as in the [tutorial](../../docs/tutorials/slack-bot.md):
Steps 2–3 create the app and collect credentials, and
[Use HTTP Instead Of Socket Mode](../../docs/tutorials/slack-bot.md#use-http-instead-of-socket-mode)
exposes your machine and sets the request URL. This example also replies to
follow-up messages in subscribed threads, so add the `channels:history` scope
and `message.channels` event from
[Step 6](../../docs/tutorials/slack-bot.md#step-6-make-it-yours) too.

| Setting | Value |
| --- | --- |
| Bot token scopes | `chat:write`, `app_mentions:read`, `channels:history` (add `im:history` for direct messages) |
| Bot events | `app_mention`, `message.channels` (add `message.im` for direct messages) |
| Request URL | `https://YOUR_PUBLIC_HOST/webhooks/slack` |
| `SLACK_BOT_TOKEN` | **OAuth & Permissions** → Bot User OAuth Token (`xoxb-…`) |
| `SLACK_SIGNING_SECRET` | **Basic Information** → App Credentials → Signing Secret |

Reinstall the app from **OAuth & Permissions** after changing scopes or
events.

## Run Postgres

From the repository root:

```sh
cd examples/slack-postgres-state
docker compose up -d postgres
```

Or let Pitchfork supervise the service:

```sh
cd examples/slack-postgres-state
pitchfork start postgres
```

The database URL for the local Compose service is:

```text
postgres://chat_sdk_go:chat_sdk_go@127.0.0.1:54330/chat_sdk_go?sslmode=disable
```

To stop Postgres:

```sh
docker compose down
```

To delete the Postgres volume:

```sh
docker compose down -v
```

## Run The Bot

From this example directory:

```sh
export SLACK_SIGNING_SECRET="..."
export SLACK_BOT_TOKEN="xoxb-..."
export DATABASE_URL="postgres://chat_sdk_go:chat_sdk_go@127.0.0.1:54330/chat_sdk_go?sslmode=disable"
export PORT=8080

go run .
```

In Slack, mention the bot in a channel where it is present:

```text
@your-bot hello
```

The bot replies in the same thread with portable Markdown and subscribes the
thread:

```text
**hello** _world_ from Postgres state. This thread is now subscribed.
```

Send another message in the same Slack thread. The bot should route it as a
subscribed message and reply:

```text
Postgres remembered this subscribed thread.
```

## Notes

- Runtime state is stored in Postgres, so subscriptions, dedupe data, and locks
  can survive process restarts while Postgres is still running.
- The Postgres state adapter creates its runtime coordination tables on startup.
- Slack URL verification is handled by the Slack adapter at `/webhooks/slack`.
- Slack request signatures are verified with `SLACK_SIGNING_SECRET`.
- The bot token is used for `auth.test` during adapter startup and
  `chat.postMessage` when replying.

## Interface

`package main` in its own module, `github.com/coder/chat/examples/slack-postgres-state`
[go.mod:module]. It is built from [`main.go`](main.go) and takes no CLI flags.
It imports `github.com/coder/chat`, `github.com/coder/chat/adapters/slack`,
`github.com/coder/chat/state/postgres` (as `chatpostgres`), and `github.com/jackc/pgx/v5/pgxpool` [main.go:main].

| Symbol | Role | Source |
| --- | --- | --- |
| `main()` | Connects to Postgres, builds the state, the Slack adapter, and the `chat.Chat` runtime, registers both handlers, and serves the webhook. | [main.go:main] |
| `OnNewMention` handler | Subscribes the thread, then posts `chat.Markdown("**hello** _world_ from Postgres state. This thread is now subscribed.")`. | [main.go:main] |
| `OnSubscribedMessage` handler | Posts `chat.Markdown("Postgres remembered this subscribed thread.")`. | [main.go:main] |
| `/webhooks/slack` | The only HTTP route, on `http.DefaultServeMux`. It serves `bot.Webhook("slack")`, which answers 405 to any method but POST. | [main.go:main] |
| `mustEnv` | Reads a required environment variable. | [main.go:mustEnv] |
| [`compose.yaml`](compose.yaml) | Local `postgres` service and volume. | [compose.yaml:services.postgres] |
| [`pitchfork.toml`](pitchfork.toml) | Pitchfork daemon `postgres` that wraps the Compose service. | [pitchfork.toml:daemons.postgres] |

## Invariants

- `DATABASE_URL`, `SLACK_SIGNING_SECRET`, and `SLACK_BOT_TOKEN` must be
  non-empty. Otherwise `mustEnv` panics with `<NAME> is required`
  [main.go:mustEnv].
- An error from `pgxpool.New`, `chatpostgres.New`, `slack.New`, `chat.New`,
  `bot.Webhook("slack")`, or `http.ListenAndServe` panics [main.go:main].
- The state keys use the `slack-example` namespace: `chatpostgres.New` with `Pool` and `Namespace: "slack-example"`
  [main.go:main].
- The mention handler subscribes the thread before it posts the reply
  [main.go:main].
- `go.mod` replaces `github.com/coder/chat` with `../..` and
  `github.com/coder/chat/state/postgres` with `../../state/postgres`, so the example builds against
  the local tree [go.mod:replace].

## Side Effects

| Environment variable | Default | Use | Source |
| --- | --- | --- | --- |
| `DATABASE_URL` | none (required) | Postgres connection | [main.go:main] |
| `SLACK_SIGNING_SECRET` | none (required) | `slack.Options.SigningSecret` | [main.go:main] |
| `SLACK_BOT_TOKEN` | none (required) | `slack.Options.BotToken` | [main.go:main] |
| `PORT` | `8080` | Listen address `":" + PORT` | [main.go:main] |

- Network: `pgxpool.New(ctx, DATABASE_URL)` opens a connection pool [main.go:main]. `main` does not close it directly: the deferred `bot.Shutdown` calls `State.Shutdown`, which closes it [main.go:main], [`state/postgres` `State.Shutdown`].
- Network: `http.ListenAndServe` on `:$PORT` with the default server, so no
  server timeouts are set [main.go:main].
- Network: `slack.New` builds the Slack adapter, and each handler posts a reply
  with `Thread.Post` [main.go:main].
- State: runtime state goes to Postgres through `chatpostgres.New` [main.go:main].
- Logging: `slog.Default()` goes to the adapter and the runtime. `main` logs
  `listening` and `chat shutdown failed` [main.go:main].
- Lifecycle: a deferred `bot.Shutdown(context.Background())` runs when `main`
  returns or panics. `main` installs no signal handler [main.go:main].
- Compose service `postgres`: image `postgres:17-alpine`, port `127.0.0.1:54330:5432`, volume `postgres-data` at `/var/lib/postgresql/data` [compose.yaml:services.postgres].
- The Compose environment sets `POSTGRES_DB`, `POSTGRES_USER`, and `POSTGRES_PASSWORD` to `chat_sdk_go` [compose.yaml:services.postgres].
- Compose healthcheck: `pg_isready -U chat_sdk_go -d chat_sdk_go` every 2s, timeout 5s, 30 retries [compose.yaml:services.postgres.healthcheck].
- Pitchfork daemon `postgres` in namespace `chat-sdk-go-slack-postgres-state`: runs `docker compose up postgres`, waits on `docker compose exec -T postgres pg_isready -U chat_sdk_go -d chat_sdk_go`, and has `auto = ["start", "stop"]` [pitchfork.toml:daemons.postgres].

## Verification

This example is a separate Go module with no `_test.go` files, so `go test`
only builds it. From the repository root:

- Run: `cd examples/slack-postgres-state && go test ./...`
- Run: `cd examples/slack-postgres-state && go vet ./...`
