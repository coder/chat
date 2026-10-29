# Chat SDK Go

[![CI](https://github.com/coder/chat/actions/workflows/ci.yaml/badge.svg?branch=main)](https://github.com/coder/chat/actions/workflows/ci.yaml)
[![Go Reference](https://pkg.go.dev/badge/github.com/coder/chat.svg)](https://pkg.go.dev/github.com/coder/chat)
[![Latest release](https://img.shields.io/github/v/release/coder/chat)](https://github.com/coder/chat/releases/latest)

Build Slack and Linear bots in Go. You write handlers for mentions and
thread replies; the runtime receives events over verified webhooks or Slack
Socket Mode, dedupes redeliveries, serializes work per thread, and retries
rate-limited API calls.

- **Plain Go.** `context.Context`, `net/http`, small interfaces, and returned
  errors. It is a library, not a framework.
- **Scales out.** Replicas share state on Redis, Postgres, or NATS JetStream,
  so they dedupe redeliveries and serialize work per thread.
- **Handles slow work.** Opt in to deferred dispatch to acknowledge the
  event first, then run LLM calls and other long handlers on a detached
  context.

## Hello, Slack

A complete bot that replies to every mention:

<!-- build -->
```go
package main

import (
	"context"
	"log"
	"os"
	"os/signal"

	"github.com/coder/chat"
	"github.com/coder/chat/adapters/slack"
	"github.com/coder/chat/state/memory"
)

func main() {
	ctx := context.Background()

	slackAdapter, err := slack.New(ctx, slack.Options{
		AppToken: os.Getenv("SLACK_APP_TOKEN"), // Socket Mode: no public URL needed
		BotToken: os.Getenv("SLACK_BOT_TOKEN"),
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

	// Listen runs until Shutdown stops it.
	go func() {
		if err := bot.Listen(ctx, "slack"); err != nil {
			log.Fatal(err)
		}
	}()

	// On Ctrl-C, Shutdown stops acking new events, and then closes the connection.
	interrupted, stop := signal.NotifyContext(ctx, os.Interrupt)
	defer stop()
	<-interrupted.Done()
	if err := bot.Shutdown(ctx); err != nil {
		log.Fatal(err)
	}
}
```

Turn on Socket Mode in your Slack app and create an app-level token with the
`connections:write` scope. Run the bot, mention it, and it replies in a
thread. The bot opens a websocket to Slack, so it needs no public URL. To
receive events over HTTP instead, set `SigningSecret` and serve
`bot.Webhook("slack")` at your app's Event Subscriptions request URL. The
[tutorial](docs/tutorials/slack-bot.md) walks through the Slack app setup in
under 30 minutes using
[`examples/slack-hello-world`](examples/slack-hello-world/), the same bot
with both transports, server timeouts, graceful shutdown, and environment
checks.

## Install

Chat SDK Go requires Go 1.26.3 or newer. The core module contains the
runtime, the Slack and Linear adapters, and the memory state backend. Its
one dependency is `github.com/coder/websocket`, for Slack Socket Mode:

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
- **Two Slack transports.** Socket Mode needs no public URL; the Events API
  over HTTP suits apps on a hosting service and Slack Marketplace apps. Both
  use one adapter pipeline, so handlers get the same events. `Chat.Listen`
  runs Socket Mode, `Chat.Webhook` serves HTTP, and a bot can use both. See
  [receiving deliveries](docs/reference.md#receiving-deliveries).
- **Ack-then-work dispatch.** Handlers run synchronously by default; opt in
  to `DispatchDeferred` to run them after the event is acknowledged, with
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

## System Overview

This section and the five after it form the repository blueprint: a map of
the code for contributors and coding agents. Each code directory has its own
`README.md` contract with Interface, Invariants, Side Effects, and
Verification sections. Citations use the form `[file:symbol]`.

The root package `chat` is the runtime. It imports no other package from
this repository. A platform adapter in `adapters/` receives a delivery over
`Chat.Webhook` or `Chat.Listen`, normalizes it to a `chat.Event`, and calls
the runtime's `DispatchFunc` [types.go:DispatchFunc]. The runtime then
works through these steps [runtime.go:Chat.dispatch, runtime.go:Chat.prelude]:

1. **Check.** The event must carry an `ID`, an `Adapter`, and a `ThreadID`
   [runtime.go:validateEvent].
2. **Admit.** The admission gate rejects the delivery once `Shutdown` has
   started. Under `DispatchDeferred` it also rejects the delivery when
   `MaxDetached` deliveries, or `MaxDetachedPerTenant` for its tenant, are
   already admitted [admission.go:admissionGate.admit].
3. **Join duplicates.** A concurrent delivery of an event ID that is already
   in the prelude waits for that delivery's outcome
   [runtime.go:Chat.beginEventAcceptance].
4. **Validate.** The adapter validates the Thread ID before any lock is
   taken [runtime.go:Chat.prelude].
5. **Lock.** Under `ConcurrencyDrop` and `ConcurrencyQueue`,
   `State.AcquireLock` takes the Thread Lock here. `ConcurrencyDebounce` and
   `ConcurrencyBurst` take it later, and `ConcurrencyConcurrent` takes none
   [runtime.go:Chat.prelude].
6. **Route.** Commands go to `OnCommand` and interactions to `OnInteraction`.
   A message goes to `OnSubscribedMessage` when its thread is subscribed,
   else to `OnNewMention` when it is a mention or a direct message
   [runtime.go:Chat.route].
7. **Mark.** When the prelude knows the outcome (dropped on a lock conflict,
   ignored, or routed), `State.MarkEvent` records the event ID for
   `DedupeTTL`. A duplicate stops here [runtime.go:Chat.prelude,
   runtime.go:Chat.routedWork, runtime.go:Chat.markAcceptedEvent].
8. **Run.** `DispatchSync` runs the handler before the adapter acknowledges.
   `DispatchDeferred` acknowledges first and runs the handler on a detached
   context bounded by `DetachTimeout` [runtime.go:Chat.startDetachedTail].

Handlers reply through `Thread.Post`, which calls `Adapter.PostMessage`
[thread.go:Thread.Post]. The runtime keeps its coordination data in a
`chat.State` backend from `state/` [state.go:State].

## Local Interface & Invariants

The root directory holds package `chat`. Exported symbols by file:

| File | Symbols |
| --- | --- |
| `runtime.go` | `New`, `Option`, `WithState`, `WithAdapter`, `WithLogger`, `WithRuntimeOptions`, `RuntimeOptions`, `DefaultRuntimeOptions`, `ConcurrencyStrategy` (`ConcurrencyDrop`, `ConcurrencyQueue`, `ConcurrencyDebounce`, `ConcurrencyConcurrent`, `ConcurrencyBurst`), `LockScope` (`LockScopeThread`, `LockScopeChannel`), `DispatchMode` (`DispatchSync`, `DispatchDeferred`), `Chat` (`OnNewMention`, `OnSubscribedMessage`, `OnCommand`, `OnInteraction`, `Webhook`, `Listen`, `Thread`, `Shutdown`), `AdapterAs` |
| `types.go` | `Adapter`, `DispatchFunc`, `Event`, `Message`, `MessageEvent`, `MessageHandler`, `Command`, `CommandEvent`, `CommandHandler`, `Interaction`, `InteractionKind` (`InteractionUnknown`, `InteractionBlockAction`), `InteractionEvent`, `InteractionHandler`, `Actor`, `BotKind` (`BotUnknown`, `BotHuman`, `BotBot`), `ThreadID`, `ThreadRef`, `RetryMetadata`, `PostableMessage`, `MessageFormat` (`MessageFormatText`, `MessageFormatMarkdown`), `Text`, `Markdown`, `SentMessage`, and the Optional Capabilities `Listener`, `HistoryReader` (`HistoryQuery`), `EphemeralPoster` (`EphemeralOptions`), `NativeContentPoster` (`NativeContent`) |
| `state.go` | `State` (`IsThreadSubscribed`, `SubscribeThread`, `UnsubscribeThread`, `MarkEvent`, `AcquireLock`, `ExtendLock`, `ReleaseLock`, `Shutdown`), `LockLease` |
| `thread.go` | `Thread` (`ID`, `Subscribe`, `Unsubscribe`, `Post`, `PostEphemeral`) |
| `install.go` | `InstallStore` (`Lookup`), `Install`, `ErrInstallNotFound` |
| `errors.go` | `ErrUnsupportedCapability`, `ErrPreempted`, `ErrAdmissionRejected` |
| `observer.go` | `Observer`, `DispatchSpan`, `WithObserver`, `ObservationName` and the `Obs*` constants, `DispatchOutcome` and the `Outcome*` constants, `Attr` with the `Attr*` keys and `*Attr` builders |
| `admission.go`, `burst.go` | No exported symbols. They hold the admission gate and burst batching. |
| `doc.go` | Package documentation (`go doc github.com/coder/chat`) |

Invariants:

- `New` fails without a `State`, without an adapter, without a logger, on a
  nil option or adapter, or on an empty or duplicate adapter name. It calls
  `Adapter.Init` for each adapter in registration order. An `Init` error
  fails `New`, which then shuts down every adapter registered so far,
  including the one that failed [runtime.go:New].
- `validateRuntimeOptions` rejects these options
  [runtime.go:validateRuntimeOptions]:
  - a non-positive `DedupeTTL` or `ThreadLockTTL`;
  - under `ConcurrencyDebounce`: a non-positive `DebounceInterval`,
    `DispatchSync`, or a `DetachTimeout` that does not exceed
    `DebounceInterval`;
  - under `ConcurrencyConcurrent`: a non-positive `MaxConcurrent`;
  - under `ConcurrencyBurst`: a non-positive `BurstWindow`, a negative
    `MaxBurstBatch`, or `DispatchSync`;
  - under `DispatchDeferred`: a non-positive `DetachTimeout` or
    `MaxDetached`, or a negative `MaxDetachedPerTenant`;
  - an unknown `ConcurrencyStrategy`, `LockScope`, or `DispatchMode` value.
- The defaults are `DedupeTTL` 24h, `ThreadLockTTL` 2m, `ConcurrencyDrop`,
  `DispatchSync`, `LockScopeThread`, and `MaxDetached` 1024
  [runtime.go:DefaultRuntimeOptions].
- Each hook holds one handler. A later `On*` call replaces it atomically
  under `handlersMu` [runtime.go:Chat.OnNewMention].
- An `Event` must carry an `ID`, an `Adapter`, and a `ThreadID`
  [runtime.go:validateEvent], and it sets at most one of `Message`,
  `Command`, or `Interaction` [types.go:Event].
- A rejected delivery returns `ErrAdmissionRejected` before acknowledgement
  and is never marked, so the platform's retry is not deduped away
  [runtime.go:Chat.dispatch, errors.go:ErrAdmissionRejected].
- A failed prelude also leaves the event unmarked [runtime.go:Chat.prelude].
- An accepted event is acknowledged even when its handler returns an error.
  The runtime logs and observes the error and does not retry it
  [runtime.go:Chat.dispatchSync, doc.go].
- The runtime ignores messages, commands, and interactions from the
  adapter's own bot actor [runtime.go:isSelfActor].
- While a locked handler runs, the runtime renews the lease every
  `ThreadLockTTL/2`. A lost lease cancels the handler with `ErrPreempted` as
  the cause [runtime.go:Chat.startLockRefresh, errors.go:ErrPreempted].
- Lock waiters poll `State.AcquireLock` every `ThreadLockTTL/20`, clamped to
  1ms–1s, because `State` does not signal a release
  [runtime.go:Chat.pollForLock].
- `Shutdown` closes admission, cancels deferred work, drains it, stops
  listeners, shuts down the adapters, and then shuts down `State`. A second
  call waits for the first [runtime.go:Chat.Shutdown].
- `Thread.Post` rejects an empty `Text` and does not subscribe the thread
  [thread.go:Thread.Post, doc.go].
- `Thread.PostEphemeral` rejects an empty actor ID or `Text`. It returns
  `ErrUnsupportedCapability` when the adapter does not implement
  `EphemeralPoster` [thread.go:Thread.PostEphemeral].
- `Chat.Webhook` and `Chat.Listen` return an error for an adapter name that
  is not registered. `Chat.Listen` returns `ErrUnsupportedCapability` for an
  adapter that does not implement `Listener`. After `Shutdown` has started,
  `Chat.Listen` returns nil at once and does not call the adapter
  [runtime.go:Chat.Webhook, runtime.go:Chat.Listen].
- `Observer` calls are best-effort: the runtime recovers a panic and logs it
  at Warn [observer.go:safeEvent, observer.go:recoverObserver]. Default
  attributes never include Thread IDs, message text, or raw actor IDs
  [observer.go:AttrAdapter].
- Under `ConcurrencyBurst`, one runner serves each scope. The window starts
  at the first member's join and seals at `BurstWindow` or `MaxBurstBatch`
  [burst.go:burstScope, burst.go:joinBurstBatch].

Side effects:

- Starts goroutines for each detached tail [runtime.go:Chat.startDetachedTail],
  each lock refresh ticker [runtime.go:Chat.startLockRefresh], each burst
  scope runner [burst.go:joinBurstBatch], and each bounded wait
  [runtime.go:waitBounded].
- Calls `State.MarkEvent`, `AcquireLock`, `ExtendLock`, `ReleaseLock`, and
  `IsThreadSubscribed` [runtime.go]. Lock release uses
  `context.WithoutCancel`, so it still runs after the caller's context ends
  [runtime.go:Chat.releaseLock]. `Thread.Subscribe` and `Thread.Unsubscribe`
  write subscriptions [thread.go:Thread.Subscribe].
- Calls the adapter methods `Init`, `Webhook`, `ValidateThreadID`,
  `PostMessage`, and `Shutdown`, plus `Listener.Listen` for adapters that
  implement it [runtime.go:New, runtime.go:Chat.Listen, thread.go:Thread.Post].
- Logs through `slog` and calls the `Observer` synchronously on the dispatch
  path [observer.go:safeEvent, observer.go:safeDispatch].

## Subdirectories & System Boundaries

- **adapters**: [adapters/README.md](./adapters/README.md) - The Linear and
  Slack platform adapters. Both verify webhook signatures, support
  single-install and multi-tenant (`InstallStore`) modes, implement
  `chat.HistoryReader`, and retry rate-limited calls only within the
  caller's deadline. Slack's low-level Web API and Socket Mode client is
  `adapters/slack/slackapi`, with test fakes in `slackapitest`.
- **state**: [state/README.md](./state/README.md) - The four `chat.State`
  backends: `memory` (root module, in-process) and `nats`, `postgres`, and
  `redis` (each a separate Go module). All four run the
  `internal/statetest` conformance suite.
- **internal**: [internal/README.md](./internal/README.md) - Private helpers:
  `ratelimit` (retry mechanics for the adapters), `statetest` (the `State`
  conformance suite), and `tokens` (random lock lease tokens).
- **examples**: [examples/README.md](./examples/README.md) - Five runnable
  bots: `slack-hello-world` and `linear-agent-hello-world` with in-memory
  state, and three Slack bots on NATS, Postgres, and Redis state, each a
  separate module with `compose.yaml` and `pitchfork.toml`.
- **scripts**: [scripts/README.md](./scripts/README.md) -
  `clawpatch-autofix.sh`, the developer tool behind
  `mise run clawpatch:autofix`.
- **docs**: Prose documentation, not a code contract. See
  [Documentation](#documentation).

## Global Invariants

- Dependencies point inward. `adapters/*` and `state/*` import the root
  `chat` package, and the root package imports neither
  [runtime.go, types.go, state.go].
- `State` holds coordination data only: thread subscriptions, dedupe marks,
  and lock leases. The runtime never creates one silently [doc.go,
  runtime.go:New].
- Every backend must pass `RunStateConformance`. The first `MarkEvent` for
  an ID within its TTL wins. Only the lease token's owner can extend or
  release a lock, and a stale token gets `false` with no error. A cancelled
  context changes no state [internal/statetest/state.go:RunStateConformance].
- A Thread ID starts with the adapter name and a colon
  [runtime.go:adapterNameFromThreadID]. The Slack adapter uses `slack:v1:`
  [adapters/slack/slack.go:decodeThreadID], and the Linear adapter uses
  `linear:v1:` [adapters/linear/linear.go:decodeThreadID].
- An adapter turns `ErrAdmissionRejected` into a retryable response: HTTP
  503 from Linear [adapters/linear/linear.go:Webhook] and `ErrBusy` from
  Slack, which `slackapi.Webhook` serves as 503
  [adapters/slack/slack.go:deliveryHandler,
  adapters/slack/slackapi/webhook.go:Webhook.ServeHTTP]. The exception is a
  Slack slash command: Slack does not redeliver it, so the adapter acks it
  with a busy reply instead [adapters/slack/slack.go:deliveryHandler].
- `InstallStore.Lookup` runs on every inbound webhook, with no cache in the
  adapter. `ErrInstallNotFound` makes the delivery an Ignored Event
  [install.go:InstallStore, install.go:ErrInstallNotFound].
- Adapter rate-limit retry never sleeps past the caller's deadline and ends
  in a typed `RateLimited` error. Both adapters share the mechanics in
  `internal/ratelimit` [internal/ratelimit/ratelimit.go:SleepCtx].
- `go.work` adds six separate modules to the root module: `state/nats`,
  `state/postgres`, `state/redis`, and the three `examples/slack-*-state`
  bots. Its `replace` directives point the root and the three state module
  paths at their local directories [go.work].

## Global Side Effects

- **Outbound network:** the Slack Web API and Socket Mode websockets
  (`adapters/slack/slackapi`), and Linear GraphQL plus its OAuth token
  endpoint (`adapters/linear`).
- **Inbound HTTP:** the application serves the `http.Handler` that
  `Chat.Webhook` returns. Adapters verify each request's signature before
  they dispatch it [adapters/slack/slackapi/webhook.go:Webhook.ServeHTTP,
  adapters/linear/linear.go:Webhook].
- **Persistent state:** NATS JetStream KV buckets `<prefix>_sub`,
  `<prefix>_event`, and `<prefix>_lock`. Postgres tables
  `chat_runtime_subscriptions`, `chat_runtime_events`, and
  `chat_runtime_locks`, which `postgres.New` creates. Redis keys
  `<prefix>:sub:<id>`, `<prefix>:event:<id>`, and `<prefix>:lock:<key>`.
- **Caller-owned clients:** `Chat.Shutdown` calls `State.Shutdown`, which
  closes the NATS connection, pgx pool, or Redis client that the caller
  passed in.
- **Observability:** `slog` logs and the optional `Observer`. Adapters also
  emit `ObsAdapterCall` and `ObsRateLimit`.
- **Developer tooling:** `scripts/clawpatch-autofix.sh` runs Codex with the
  sandbox disabled and creates git commits.

## System Verification

- Run: `mise run test`. It tests the root module, the three backend
  modules, and the three state example modules. The Postgres and Redis
  backend tests use Testcontainers and need Docker.
- Run: `go test ./...` for the root module only.
- Run: `mise run vet` for `go vet` across every module.

## Status

Chat SDK Go is pre-1.0, and the public API may change before 1.0. See the
[releases](https://github.com/coder/chat/releases) for what changed.
Report bugs and request features in
[GitHub issues](https://github.com/coder/chat/issues).

## Contributing

See [CONTRIBUTING.md](CONTRIBUTING.md) for how to build, test, and propose
changes.

## License

Chat SDK Go is released under the [MIT License](LICENSE).
