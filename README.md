# Chat SDK Go

[![CI](https://github.com/coder/chat/actions/workflows/ci.yaml/badge.svg?branch=main)](https://github.com/coder/chat/actions/workflows/ci.yaml)
[![Go Reference](https://pkg.go.dev/badge/github.com/coder/chat.svg)](https://pkg.go.dev/github.com/coder/chat)
[![Latest release](https://img.shields.io/github/v/release/coder/chat)](https://github.com/coder/chat/releases/latest)

Chat SDK Go is a Go runtime for building chat bots and agents on Slack and
Linear. You write handlers against a normalized event model — a mention
arrives, you reply in its thread, you subscribe to keep the conversation
going — and the runtime takes care of the platform plumbing: webhook
verification, event normalization, thread-scoped replies, event dedupe and
per-thread locking in shared state (Redis, Postgres, or NATS JetStream in
production; memory for development) so horizontally scaled replicas dedupe
redeliveries and serialize work per thread, deferred ack-then-work dispatch
with admission bounds for slow handlers such as LLM calls, multi-tenant
installs, and platform rate-limit retries. The API is small, explicit Go —
`context.Context`, `net/http`, small interfaces, returned errors — rather
than a framework.

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

## Features

- **Thread-scoped conversations.** `OnNewMention` and `OnSubscribedMessage`
  route by thread; subscriptions are explicit and survive restarts on a
  durable backend. Start with the [tutorial](docs/tutorials/slack-bot.md).
- **Coordination state you already run.** Subscriptions, dedupe marks, and
  token-owned lock leases on memory, Redis, Postgres, or NATS JetStream,
  behind one contract and one conformance suite —
  [choose a state backend](docs/how-to/choose-a-state-backend.md).
- **Ack-then-work dispatch.** Acknowledge the webhook first, run the handler
  on a detached context with automatic lock renewal, bound in-flight work
  with an admission cap, and pick from five concurrency strategies (drop,
  queue, debounce, concurrent, burst) —
  [defer long-running work](docs/how-to/deferred-dispatch.md).
- **Slash commands and interactive components.** Commands and button clicks
  are first-class events with their own hooks; Block Kit content and modals
  go through typed adapter access —
  [slash commands](docs/how-to/slash-commands.md),
  [interactive components](docs/how-to/interactive-components.md).
- **Multi-tenant installs.** Serve many workspaces or organizations from one
  deployment with an application-implemented `InstallStore`; OAuth flows
  stay yours — [multi-tenant installs](docs/how-to/multi-tenant-install.md).
- **Linear agent sessions.** Thoughts, responses, actions, elicitations,
  plans, and generic issue comments —
  [run Linear agent sessions](docs/how-to/linear-agent-sessions.md).
- **Rate limits handled in the adapter.** Slack and Linear API calls retry
  with `Retry-After` and bounded backoff and surface a typed `RateLimited`
  error when they give up —
  [adapter capability status](docs/reference.md#adapter-capability-status).
- **Observability without a dependency.** Structured `slog` logging plus an
  optional `Observer` seam for counters and per-dispatch spans; no
  OpenTelemetry in the core import graph —
  [observability](docs/reference.md#observability).
- **Message history read-through.** `HistoryReader` fetches recent platform
  messages for a thread on demand; what you persist is up to you —
  [message history](docs/reference.md#message-history).

## Adapters

Adapters are either `supported` — production-grade, with hardening test
suites, rate-limit handling, multi-tenant installs, and documentation — or
`experimental` — implemented and tested, but the platform surface, the
adapter API, or both may still change.

| Adapter | Tier | Notes |
| --- | --- | --- |
| Slack (`adapters/slack`) | `supported` | Hardening tests for rate-limit retry ([ADR 0005](docs/adr/0005-rate-limit-handling.md)), multi-tenant installs ([ADR 0006](docs/adr/0006-multi-tenant-install.md)), history read-through ([ADR 0009](docs/adr/0009-message-history.md)), and interactivity. No live end-to-end Slack test runs in CI. |
| Linear (`adapters/linear`) | `experimental` | Fully implemented and hardened (agent sessions, generic comments, rate-limit retry, multi-tenant, history read-through), but the upstream Linear agent API is itself in developer preview and [capability gaps remain](docs/linear-agent-capabilities.md). |
| Microsoft Teams | spike | [ADR 0007](docs/adr/0007-teams-adapter.md) is a proposal gated on a live-tenant spike (draft [PR #4](https://github.com/coder/chat/pull/4), tracked in [#6](https://github.com/coder/chat/issues/6)). Not usable yet. |

## Documentation

Documentation follows [Diátaxis](https://diataxis.fr/); the
[docs index](docs/README.md) maps it all.

- **Tutorial**: [your first Slack bot](docs/tutorials/slack-bot.md) — zero to
  a running bot in under 30 minutes.
- **How-to guides**: [state backends](docs/how-to/choose-a-state-backend.md),
  [deferred dispatch](docs/how-to/deferred-dispatch.md),
  [slash commands](docs/how-to/slash-commands.md),
  [interactive components](docs/how-to/interactive-components.md),
  [multi-tenant installs](docs/how-to/multi-tenant-install.md), and
  [Linear agent sessions](docs/how-to/linear-agent-sessions.md).
- **Reference**: [runtime semantics and API reference](docs/reference.md) —
  construction, webhooks, routing, dispatch, state, concurrency, messages,
  history, adapter access, per-adapter capability status, and the testing
  contract, plus [pkg.go.dev](https://pkg.go.dev/github.com/coder/chat) for
  the GoDoc.
- **Explanation**: [architecture and design decisions](docs/explanation.md)
  — an index over [`CONTEXT.md`](CONTEXT.md) and the [ADRs](docs/adr/), with
  the design goals, the Vercel Chat SDK comparison, and the non-goals.

## Relationship To Vercel Chat SDK

Chat SDK Go follows [Vercel Chat SDK](https://chat-sdk.dev/)'s conversation
model — adapters, normalized events, threads, subscriptions, thread-scoped
replies — where it maps cleanly to Go. It is not a TypeScript API port:
hooks are single-slot, construction is fail-fast, subscriptions are explicit,
and message history is application-owned. The concept-by-concept status map
is in [docs/explanation.md](docs/explanation.md#vercel-chat-sdk-alignment).

## Non-Goals

Each of these is a recorded decision, not a missing feature. The full list
with the ADR behind each is in
[docs/explanation.md](docs/explanation.md#non-goals); the scope exclusions
are in [intentional gaps](docs/explanation.md#intentional-gaps).

- **Streaming token transport in the core** — deferred, not foreclosed
  ([ADR 0011](docs/adr/0011-resumable-streaming.md)); long generation is
  ack-then-work posting one finished message.
- **LLM orchestration** — prompts, model calls, and generation pipelines live
  in your handlers.
- **A cross-platform card DSL** — platform-native payloads ship opaquely via
  `NativeContentPoster`.
- **Transcript storage, RAG, and embeddings** — message history is
  application-owned; `chat.State` holds subscriptions, dedupe marks, and
  locks only.
- **App-user auth and OAuth web flows** — install storage and account linking
  stay app-owned.

## Status

The current release is [v0.2.0](https://github.com/coder/chat/releases/latest).
The public Go API may change before 1.0; the
[release notes](https://github.com/coder/chat/releases) describe what changed
in each version. Bug reports and feature requests are tracked in
[GitHub issues](https://github.com/coder/chat/issues).
