# Chat SDK Go

[![CI](https://github.com/coder/chat/actions/workflows/ci.yaml/badge.svg?branch=main)](https://github.com/coder/chat/actions/workflows/ci.yaml)
[![Go Reference](https://pkg.go.dev/badge/github.com/coder/chat.svg)](https://pkg.go.dev/github.com/coder/chat)
[![Latest release](https://img.shields.io/github/v/release/coder/chat)](https://github.com/coder/chat/releases/latest)

Build Slack and Linear bots in Go. You write handlers for mentions and
thread replies; the runtime verifies webhooks, dedupes redeliveries,
serializes work per thread, and retries rate-limited API calls.

- **Plain Go.** `context.Context`, `net/http`, small interfaces, and returned
  errors. It is a library, not a framework.
- **Scales out.** Replicas share state on Redis, Postgres, or NATS JetStream,
  so they dedupe redeliveries and serialize work per thread.
- **Handles slow work.** Opt in to deferred dispatch to acknowledge the
  webhook first, then run LLM calls and other long handlers on a detached
  context.

## Hello, Slack

A complete bot that replies to every mention:

<!-- build -->
```go
package main

import (
	"context"
	"log"
	"net/http"
	"os"

	"github.com/coder/chat"
	"github.com/coder/chat/adapters/slack"
	"github.com/coder/chat/state/memory"
)

func main() {
	ctx := context.Background()

	slackAdapter, err := slack.New(ctx, slack.Options{
		SigningSecret: os.Getenv("SLACK_SIGNING_SECRET"),
		BotToken:      os.Getenv("SLACK_BOT_TOKEN"),
	})
	if err != nil {
		log.Fatal(err)
	}

	bot, err := chat.New(ctx,
		chat.WithState(memory.New()), // swap for Redis, Postgres, or NATS in production
		chat.WithAdapter(slackAdapter),
	)
	if err != nil {
		log.Fatal(err)
	}

	bot.OnNewMention(func(ctx context.Context, ev *chat.MessageEvent) error {
		_, err := ev.Thread.Post(ctx, chat.Markdown("**hello** _world_"))
		return err
	})

	webhook, err := bot.Webhook("slack")
	if err != nil {
		log.Fatal(err)
	}
	http.Handle("/webhooks/slack", webhook)
	log.Fatal(http.ListenAndServe(":8080", nil))
}
```

Point your Slack app's Event Subscriptions request URL at
`https://YOUR_HOST/webhooks/slack`, mention the bot, and it replies in a
thread. The [tutorial](docs/tutorials/slack-bot.md) walks through the Slack
app setup in under 30 minutes using
[`examples/slack-hello-world`](examples/slack-hello-world/), the same bot
with server timeouts and environment checks.

## Install

Chat SDK Go requires Go 1.26.3 or newer. The core module contains the
runtime, the Slack and Linear adapters, and the memory state backend:

```sh
go get github.com/coder/chat
```

The durable state backends are separate modules, so applications that only
use the core do not pull their dependencies:

```sh
go get github.com/coder/chat/state/redis
go get github.com/coder/chat/state/postgres
go get github.com/coder/chat/state/nats
```

## What You Get

- **Thread-scoped conversations.** `OnNewMention` starts a conversation;
  `Thread.Subscribe` keeps the bot in it, and `OnSubscribedMessage` receives
  the follow-ups — [tutorial](docs/tutorials/slack-bot.md).
- **Shared state you already run.** Memory for development; Redis, Postgres,
  or NATS JetStream in production, all tested by one conformance suite —
  [choose a state backend](docs/how-to/choose-a-state-backend.md).
- **Ack-then-work dispatch.** Handlers run synchronously by default; opt in
  to `DispatchDeferred` to run them after the webhook is acknowledged, with
  lock renewal and an admission cap. Five concurrency strategies (drop,
  queue, debounce, concurrent, burst) decide what happens when events
  overlap on one thread; debounce and burst require deferred dispatch —
  [defer long-running work](docs/how-to/deferred-dispatch.md).
- **Slash commands and interactive components.** Commands and button clicks
  have their own hooks; Block Kit and modals go through typed adapter access —
  [slash commands](docs/how-to/slash-commands.md),
  [interactive components](docs/how-to/interactive-components.md).
- **Multi-tenant installs.** Serve many Slack workspaces or Linear
  organizations from one deployment; you own the OAuth flow —
  [multi-tenant installs](docs/how-to/multi-tenant-install.md).
- **Linear agent sessions.** Thoughts, actions, elicitations, plans,
  responses, and plain issue comments —
  [Linear agent sessions](docs/how-to/linear-agent-sessions.md).
- **Rate-limit retries.** Adapters honor `Retry-After` with bounded backoff
  and return a typed `RateLimited` error when they give up —
  [capability status](docs/reference.md#adapter-capability-status).
- **Observability without extra dependencies.** `slog` logging plus an
  optional `Observer` for metrics and spans —
  [observability](docs/reference.md#observability).
- **Message history on demand.** `HistoryReader` fetches recent platform
  messages for a thread; what you store is up to you —
  [message history](docs/reference.md#message-history).

## Adapters

`supported` adapters are production-grade: hardening tests, rate-limit
handling, multi-tenant installs, and docs. `experimental` adapters work and
are tested, but their API may still change.

| Adapter | Tier | Notes |
| --- | --- | --- |
| Slack (`adapters/slack`) | `supported` | Hardening tests cover rate-limit retry, multi-tenant installs, history read-through, and interactivity. No live end-to-end Slack test runs in CI. |
| Linear (`adapters/linear`) | `experimental` | Fully implemented and hardened, but Linear's agent API is itself in developer preview; see the [capability gaps](docs/linear-agent-capabilities.md). |
| Microsoft Teams | spike | Not usable yet. [ADR 0007](docs/adr/0007-teams-adapter.md) is waiting on a live-tenant spike ([#6](https://github.com/coder/chat/issues/6)). |

## Documentation

The [docs index](docs/README.md) lists everything. Good starting points:

- **New here?** [Your first Slack bot](docs/tutorials/slack-bot.md), zero to
  running in under 30 minutes.
- **Doing a task?** The [how-to guides](docs/README.md#how-to-guides) cover
  state backends, deferred dispatch, commands, interactivity, multi-tenant
  installs, and Linear.
- **Looking something up?** The [reference](docs/reference.md) and the GoDoc
  (`go doc github.com/coder/chat`).
- **Want the why?** [Architecture and design decisions](docs/explanation.md).

## Scope

Chat SDK Go follows [Vercel Chat SDK](https://chat-sdk.dev/)'s conversation
model — adapters, normalized events, threads, subscriptions — where it fits
Go. It is not a TypeScript API port: hooks hold one handler each,
construction fails fast, subscriptions are explicit, and message history
belongs to your application. See the
[concept map](docs/explanation.md#vercel-chat-sdk-alignment).

These are left out on purpose, each by a recorded decision
([full list](docs/explanation.md#non-goals)):

- **Token streaming in the core.** Deferred, not ruled out
  ([ADR 0011](docs/adr/0011-resumable-streaming.md)); long generation posts
  one finished message.
- **LLM orchestration.** Prompts and model calls live in your handlers.
- **A cross-platform card DSL.** Native payloads go through
  `NativeContentPoster`.
- **Transcript storage, RAG, and embeddings.** Runtime state holds only
  subscriptions, dedupe marks, and locks.
- **App-user auth and OAuth web flows.** Your application owns these.

## Status

Chat SDK Go is pre-1.0, and the public API may change before 1.0. See the
[releases](https://github.com/coder/chat/releases) for what changed.
Report bugs and request features in
[GitHub issues](https://github.com/coder/chat/issues).

## Contributing

See [CONTRIBUTING.md](CONTRIBUTING.md) for how to build, test, and propose
changes.
