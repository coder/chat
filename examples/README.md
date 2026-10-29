# examples

## Overview

Runnable example bots for the `github.com/coder/chat` SDK. Each subdirectory
is one `package main` program. Two examples build in the root module and use
in-memory state (`github.com/coder/chat/state/memory`). Three examples are
separate Go modules that pair the Slack adapter with a durable state backend
and ship a `compose.yaml` and a `pitchfork.toml` for the local service. Each
of those `go.mod` files replaces `github.com/coder/chat` with the local tree
(`../..`).

## Subdirectories & Boundaries

- linear-agent-hello-world: [./linear-agent-hello-world/README.md](./linear-agent-hello-world/README.md) - Linear app-actor agent on `github.com/coder/chat/adapters/linear` with in-memory state and `chat.DispatchDeferred`. Serves `/webhooks/linear` and needs `LINEAR_WEBHOOK_SECRET`, `LINEAR_CLIENT_CREDENTIALS_CLIENT_ID`, and `LINEAR_CLIENT_CREDENTIALS_CLIENT_SECRET`. `capabilities.go` holds the agent-session helper loops. Root module.
- slack-hello-world: [./slack-hello-world/README.md](./slack-hello-world/README.md) - Slack bot with in-memory state that replies `**hello** _world_` to mentions. Runs Socket Mode (`SLACK_APP_TOKEN`), HTTP on `/webhooks/slack` (`SLACK_SIGNING_SECRET`), or both. Requires `CHAT_DEMO_IN_MEMORY_STATE=1`. Root module.
- slack-nats-state: [./slack-nats-state/README.md](./slack-nats-state/README.md) - Slack webhook bot with NATS JetStream state (`github.com/coder/chat/state/nats`). Needs `NATS_URL`. Compose service `nats` on `127.0.0.1:42220`. Separate module.
- slack-postgres-state: [./slack-postgres-state/README.md](./slack-postgres-state/README.md) - Slack webhook bot with Postgres state (`github.com/coder/chat/state/postgres`). Needs `DATABASE_URL`. Compose service `postgres` on `127.0.0.1:54330`. Separate module.
- slack-redis-state: [./slack-redis-state/README.md](./slack-redis-state/README.md) - Slack webhook bot with Redis state (`github.com/coder/chat/state/redis`). Needs `REDIS_URL`. Compose service `redis` on `127.0.0.1:63800`. Separate module.

Every HTTP listener in these examples uses `:$PORT` (default `:8080`).
`slack-hello-world` opens one only when `SLACK_SIGNING_SECRET` is set. With
Socket Mode alone it listens on no port. Root-module examples
run with `go test ./examples/<name>/...` from the repository root. The
separate modules run with `cd examples/<name> && go test ./...`.
