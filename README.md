# Chat SDK Go

[![CI](https://github.com/coder/chat/actions/workflows/ci.yaml/badge.svg?branch=main)](https://github.com/coder/chat/actions/workflows/ci.yaml)

Chat SDK Go is a Go-native semantic subset of Vercel Chat SDK's
conversation runtime: adapters, normalized events, threads, subscriptions,
state-backed dedupe, and thread-scoped replies.

This is not a TypeScript API port and not a promise of full Vercel Chat SDK
feature parity. The goal is semantic compatibility where the model maps cleanly
to Go, with deliberate Go-shaped differences where that makes the runtime
simpler, safer, or easier to operate.

Status: the core runtime, the Slack adapter, the Linear adapter (agent
sessions and generic issue comments), four state backends (memory, Redis,
Postgres, NATS JetStream), runnable examples, and public contract tests are in
place. The public Go API surface is still early and may change.

## Adapter Maturity

Adapters are tiered honestly:

- **`supported`** — production-grade: hardening test suites, rate-limit
  handling, multi-tenant installs, and documentation. A reasonable default
  choice for production.
- **`experimental`** — implemented and tested, but no promises: the platform
  surface, the adapter API, or both may still change.

| Adapter | Tier | Notes |
| --- | --- | --- |
| Slack (`adapters/slack`) | `supported` | Hardening tests for rate-limit retry ([ADR 0005](docs/adr/0005-rate-limit-handling.md)), multi-tenant installs ([ADR 0006](docs/adr/0006-multi-tenant-install.md)), history read-through ([ADR 0009](docs/adr/0009-message-history.md)), and interactivity. No live end-to-end Slack test runs in CI. |
| Linear (`adapters/linear`) | `experimental` | Fully implemented and hardened (agent sessions, generic comments, rate-limit retry, multi-tenant), but the upstream Linear agent API is itself in developer preview and [capability gaps remain](docs/linear-agent-capabilities.md) (no `HistoryReader`; some operations are GraphQL-escape-hatch only). |
| Microsoft Teams | spike | [ADR 0007](docs/adr/0007-teams-adapter.md) is a proposal gated on a live-tenant spike (draft [PR #4](https://github.com/coder/chat/pull/4), tracked in [#6](https://github.com/coder/chat/issues/6)). Not usable yet. |

## Documentation

Documentation follows [Diátaxis](https://diataxis.fr/). The
[docs index](docs/README.md) maps it all; the short version:

- **Tutorial**: [your first Slack bot](docs/tutorials/slack-bot.md) — zero to
  a running bot in under 30 minutes.
- **How-to guides**: [state backends](docs/how-to/choose-a-state-backend.md),
  [deferred dispatch](docs/how-to/deferred-dispatch.md),
  [slash commands](docs/how-to/slash-commands.md),
  [interactive components](docs/how-to/interactive-components.md),
  [multi-tenant installs](docs/how-to/multi-tenant-install.md), and
  [Linear agent sessions](docs/how-to/linear-agent-sessions.md).
- **Reference**: [package and API reference](docs/reference.md) (pkg.go.dev
  pointers and per-adapter capability status).
- **Explanation**: [architecture and design decisions](docs/explanation.md)
  — an index over [`CONTEXT.md`](CONTEXT.md) and the [ADRs](docs/adr/).

## Vercel Chat SDK Alignment

This project follows Vercel Chat SDK's conversation semantics where they fit
Go, built outward from a production-shaped Slack slice. The table below is the
quick status map for readers familiar with Vercel Chat SDK:

| Vercel Chat SDK concept | Chat SDK Go status |
| --- | --- |
| `Chat` runtime | Implemented as `chat.Chat` |
| Platform adapters | Slack (supported) and Linear (experimental) implemented; Teams is a spike |
| Normalized events and thread-scoped replies | Implemented |
| `onNewMention` | Implemented as `OnNewMention` |
| `onSubscribedMessage` | Implemented as `OnSubscribedMessage` |
| Thread subscriptions | Implemented with explicit `Thread.Subscribe` / `Thread.Unsubscribe` |
| Runtime state adapters | Memory, Redis, Postgres, and NATS JetStream implemented |
| Direct messages | Routed as implicit new mentions, then subscribed messages |
| Ephemeral messages | Slack native ephemeral plus explicit DM fallback |
| Thread handle reconstruction | Implemented with `Chat.Thread` |
| AI streaming responses | Deferred from core, not foreclosed (ADR 0011); long generation uses ack-then-work |
| Slash commands | Implemented as `OnCommand` Command Events (Slack) |
| Interactive components (buttons, menus) | Implemented as `OnInteraction` block_actions (Slack) |
| Native rich content (Block Kit) | Implemented as `NativeContentPoster` Optional Capability (Slack) |
| Modal open (`views.open`) | Implemented as a Slack adapter Optional Capability |
| Modal `view_submission` synchronous response | Deferred (incompatible with ack-then-work) |
| Cards, JSX-style cards, native payload builders | Not yet implemented |
| Pattern handlers | Not yet implemented |
| Observability metrics/tracing | Optional `Observer` seam, no-op default, no OTel dependency in core |
| Message history persistence | App-owned (Thread Application State); thin live read-through via `HistoryReader` Optional Capability (Slack, Linear) |
| AI-message conversion helpers | Not yet implemented |
| Multiple production adapters | Slack is the only `supported` adapter; Linear is `experimental` |
| Middleware | Not yet implemented |

## Design Goals

- Go-native API built around `context.Context`, `net/http`, small interfaces,
  and explicit errors.
- Slack-first vertical slice before claiming multi-platform portability.
- Required runtime state for subscriptions, dedupe, and locks.
- Memory state for tests and local development.
- Redis, Postgres, or NATS JetStream state for horizontally scaled production
  deployments.
- Thread-oriented application code: handle a message, subscribe the thread,
  reply to the thread.
- Platform escape hatches without making raw platform structs the normal API.
- Vercel Chat SDK behavior as the default precedent unless it is non-idiomatic
  in Go or outside the documented scope.

## Install

The core module is:

```sh
go get github.com/coder/chat
```

Redis, Postgres, and NATS state are optional and live in separate modules so
applications that only use core, Slack, or memory state do not pull production
state dependencies:

```sh
go get github.com/coder/chat/state/redis
go get github.com/coder/chat/state/postgres
go get github.com/coder/chat/state/nats
```

Package layout:

```text
github.com/coder/chat
github.com/coder/chat/adapters/slack
github.com/coder/chat/adapters/linear
github.com/coder/chat/state/memory
github.com/coder/chat/state/nats
github.com/coder/chat/state/postgres
github.com/coder/chat/state/redis
```

This repository uses `go.work` for local development across the root module,
state modules, and example modules.

## Examples And Local Services

Which example should you run?

- Start with `examples/slack-hello-world` if you are new to the SDK or want a
  memory-backed bot with no local infrastructure. The
  [tutorial](docs/tutorials/slack-bot.md) walks through it end to end.
- Use `examples/linear-agent-hello-world` if you want to dogfood Linear
  app-actor agent sessions with memory state.
- Use `examples/slack-redis-state` to try durable runtime coordination with
  Redis.
- Use `examples/slack-postgres-state` if Postgres is already your coordination
  store.
- Use `examples/slack-nats-state` if you already run NATS with JetStream.

The memory-backed Slack example runs without local infrastructure:

```sh
go run ./examples/slack-hello-world
```

The memory-backed Linear app-actor example also runs without local
infrastructure, but it requires a Linear OAuth app installed as an app actor and
a public HTTPS webhook URL:

```sh
go run ./examples/linear-agent-hello-world
```

The state-backed Slack examples live in separate example modules so the core
module does not pull Redis, Postgres, or NATS dependencies just to build the
basic example:

- `examples/slack-redis-state`
- `examples/slack-postgres-state`
- `examples/slack-nats-state`

Each state-backed example has its own `compose.yaml`, `pitchfork.toml`, and
README with the backend URL, service startup commands, and Slack setup steps.
For example:

```sh
cd examples/slack-redis-state
docker compose up -d redis
go run .
```

You can also let Pitchfork supervise an example's local service from that
example directory:

```sh
pitchfork start redis
```

## Tiny Slack Example

The core handler for a minimal bot can be tiny:

```go
bot.OnNewMention(func(ctx context.Context, ev *chat.MessageEvent) error {
	_, err := ev.Thread.Post(ctx, chat.Text("hello world"))
	return err
})
```

Replying does not subscribe the thread. Call `ev.Thread.Subscribe(ctx)` when
you want later messages in the same thread to route to `OnSubscribedMessage`.

## Production-Shaped Example

```go
package main

import (
	"context"
	"log/slog"
	"net/http"
	"os"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/coder/chat"
	"github.com/coder/chat/adapters/slack"
	chatredis "github.com/coder/chat/state/redis"
)

func main() {
	ctx := context.Background()

	redisState, err := chatredis.New(ctx, chatredis.Options{
		Client: redis.NewClient(&redis.Options{
			Addr: os.Getenv("REDIS_ADDR"),
		}),
	})
	if err != nil {
		panic(err)
	}

	slackAdapter, err := slack.New(ctx, slack.Options{
		SigningSecret: os.Getenv("SLACK_SIGNING_SECRET"),
		BotToken:      os.Getenv("SLACK_BOT_TOKEN"),
	})
	if err != nil {
		panic(err)
	}

	bot, err := chat.New(ctx,
		chat.WithState(redisState),
		chat.WithAdapter(slackAdapter),
		chat.WithLogger(slog.Default()),
		chat.WithRuntimeOptions(chat.RuntimeOptions{
			DedupeTTL:     24 * time.Hour,
			ThreadLockTTL: 2 * time.Minute,
			Concurrency:   chat.ConcurrencyDrop,
		}),
	)
	if err != nil {
		panic(err)
	}
	defer func() {
		if err := bot.Shutdown(context.Background()); err != nil {
			slog.Error("chat shutdown failed", "error", err)
		}
	}()

	bot.OnNewMention(func(ctx context.Context, ev *chat.MessageEvent) error {
		if !userIsLinked(ev.Message.Author) {
			_, err := ev.Thread.PostEphemeral(ctx, ev.Message.Author, chat.Text(
				"Please link your account before I continue.",
			), chat.EphemeralOptions{
				FallbackToDM: true,
			})
			return err
		}

		if err := ev.Thread.Subscribe(ctx); err != nil {
			return err
		}

		_, err := ev.Thread.Post(ctx, chat.Markdown(
			"I'm listening to this thread now.",
		))
		return err
	})

	bot.OnSubscribedMessage(func(ctx context.Context, ev *chat.MessageEvent) error {
		_, err := ev.Thread.Post(ctx, chat.Text("You said: "+ev.Message.Text))
		return err
	})

	slackWebhook, err := bot.Webhook("slack")
	if err != nil {
		panic(err)
	}

	http.Handle("/webhooks/slack", slackWebhook)
	if err := http.ListenAndServe(":8080", nil); err != nil {
		panic(err)
	}
}

func userIsLinked(chat.Actor) bool {
	return false
}
```

## Core Model

`Chat` is the runtime. It owns adapter registration, runtime state, handler
registration, webhook mounting, dispatch, dedupe, locking, and shutdown.

`Platform Adapter` is a platform boundary. It verifies inbound webhooks,
normalizes platform payloads, renders outbound messages, and exposes
platform-specific APIs through typed adapter access. It does not own application
routing.

`Event` is the normalized inbound envelope. A `Message` is one payload type
inside an event, not the name for every inbound platform occurrence.

`MessageEvent` is the handler input for message routing hooks. It carries the
normalized event, thread, and message together.

`Thread` is the stable conversation address used for routing, subscription,
and replies. In Slack, a root channel message becomes a thread rooted at that
message timestamp, not the entire channel.

`ThreadID` is opaque and adapter-produced. It must include adapter identity and
enough platform tenant/routing context to avoid collisions across workspaces,
channels, and platforms. Application code may store and pass it around, but
must not build it manually.

`Thread Handle` reconstruction is supported for out-of-webhook work:

```go
thread, err := bot.Thread(ctx, threadID)
if err != nil {
	return err
}

_, err = thread.Post(ctx, chat.Text("Reminder"))
```

The runtime decodes the adapter prefix, asks the adapter to validate the
thread ID, and returns an error for unknown adapters or invalid IDs.

## Runtime Construction

Construction is fail-fast:

```go
bot, err := chat.New(ctx,
	chat.WithState(state),
	chat.WithAdapter(slackAdapter),
)
```

`chat.New` validates state, adapter registration, runtime options, and adapter
initialization before webhooks are served. This is an intentional difference
from Vercel Chat SDK, which initializes lazily on first use.

`Shutdown(ctx)` is idempotent. It attempts all adapter cleanup hooks before
state cleanup and returns joined errors if cleanup fails.

## Webhooks

The runtime exposes `net/http` handlers and does not own the HTTP server:

```go
handler, err := bot.Webhook("slack")
if err != nil {
	return err
}

http.Handle("/webhooks/slack", handler)
```

Webhook lookup is fallible. A misspelled adapter name is a startup/configuration
error, not a production 404.

Adapters own platform handshakes. For Slack, URL verification is handled inside
the Slack webhook handler and never reaches application handlers.

## Routing

The runtime has two message routing hooks:

```go
bot.OnNewMention(func(context.Context, *chat.MessageEvent) error)
bot.OnSubscribedMessage(func(context.Context, *chat.MessageEvent) error)
```

Routing order:

1. Ignore self-authored bot messages.
2. Route messages in subscribed threads to `OnSubscribedMessage`.
3. Route mentions in unsubscribed threads to `OnNewMention`.
4. A valid but unsupported or irrelevant platform event is acknowledged and
   ignored.

Direct messages are treated as implicit mentions. An unsubscribed direct message
routes to `OnNewMention`; once subscribed, later direct messages route to
`OnSubscribedMessage`.

Handlers are single-slot per hook. Calling `OnNewMention` or
`OnSubscribedMessage` again atomically replaces the previous handler. Missing
handlers are no-ops. This intentionally differs from Vercel Chat SDK, which
allows multiple handlers per hook.

Subscriptions are explicit:

```go
if err := ev.Thread.Subscribe(ctx); err != nil {
	return err
}
```

Replying successfully to a new mention does not subscribe the thread. A
subscription lasts until explicit unsubscribe.

### Command And Interaction Events

A slash command and a button click are **Events**, not **Messages**. They ride the
same dispatch spine (dedupe by Event Identity, Thread Lock, self-filtering,
lock-conflict acknowledge-and-drop) but route to their own single-slot hooks:

- `OnCommand(func(ctx, *chat.CommandEvent) error)` for Command Events (Slack slash
  commands). Command-ness takes precedence over subscription state: a command in a
  subscribed thread still routes to `OnCommand`, never to `OnSubscribedMessage`. A
  command does not auto-subscribe its thread.
- `OnInteraction(func(ctx, *chat.InteractionEvent) error)` for Interaction Events.
  This slice handles Slack `block_actions` (button clicks, menu selections).

Both hooks are single-slot and no-op-when-unset, like the message hooks; an unset
handler is still acknowledged. The platform ack is adapter-owned: the Slack adapter
returns an empty 2xx and preserves `response_url` / `trigger_id` on the `Raw`
Platform Escape Hatch. Under the default synchronous dispatch the handler runs
before that ack, so long command/interaction work should use the same
`DispatchDeferred` ack-then-work primitive as messages (ADR 0002) to stay inside
Slack's 3-second budget; bots expecting commands or clicks mid-conversation
should select the `queue` Concurrency Strategy.

Native command/interaction responses and Block Kit content are NOT added to
Postable Message, which stays Plain Text + Portable Markdown. They are reached
deliberately through typed Adapter Access:

- `chat.NativeContentPoster.PostNative` posts opaque Block Kit blocks. A
  `NativeContent` whose adapter does not match the target is an error, never a
  silent portable downgrade.
- The Slack adapter's `OpenModal` opens a modal via `views.open` using a preserved
  `trigger_id`. The synchronous modal `view_submission` response is deferred
  because it is incompatible with ack-then-work.
- The Slack adapter's `RespondURL` posts to a preserved `response_url`.

### Observability

Runtime Observation defaults to structured `slog`, unchanged. An optional
`WithObserver(Observer)` seam adds counter-style point events (dedupe hit, lock
conflict, ignored-event-by-reason, handler error, lock-release failure, adapter
call, rate limit) and a per-dispatch span with a terminal outcome
(`handled`, `ignored`, `dropped-lock-conflict`, `duplicate`, `error`). The default
is a no-op Observer, so an unconfigured runtime behaves exactly as before. The core
imports no OpenTelemetry, Prometheus, or statsd; attribute keys are a closed,
low-cardinality set (`adapter`, `route`, `reason`, `outcome`, `tenant`) and never
carry Thread ID, message text, or raw actor IDs. Observer calls are panic-safe: a
broken Observer can never fail an Accepted Event or alter acknowledgement. Under
deferred dispatch the span follows the Detached Work Context so ack-then-work
latency is measured to handler completion.

## Dispatch And Acknowledgement

The default dispatch mode is synchronous (`DispatchSync`): handlers run on the
inbound webhook request context before the platform acknowledgement. For
long-running work, opt in to `DispatchDeferred` (ack-then-work, ADR 0002): the
dedupe/lock prelude runs before the ack, then the handler runs on a detached
work context with automatic lock lease renewal. See the
[deferred dispatch guide](docs/how-to/deferred-dispatch.md).

Once a webhook is verified and normalized into an accepted event, handler errors
are recorded but acknowledged to the platform by default. This avoids platform
retry storms after partial side effects such as posting a message.

Invalid signatures and malformed requests are rejected. Valid but unsupported
platform events are acknowledged and ignored.

## Runtime State

State is required. The runtime must not silently create memory state for
production-facing construction.

Runtime state is coordination state:

- subscribed thread membership
- event dedupe
- thread locks
- runtime cache needed by adapters

Runtime state is not product state. Store application workflow data in your own
database keyed by `ThreadID`.

State implementations:

- `state/memory`: tests and local development, included in the root module
- `state/postgres`: production and horizontally scaled deployments, kept in the
  separate `github.com/coder/chat/state/postgres` module
- `state/redis`: production and horizontally scaled deployments, kept in the
  separate `github.com/coder/chat/state/redis` module
- `state/nats`: production deployments that already run NATS with JetStream,
  kept in the separate `github.com/coder/chat/state/nats` module

The [state backend guide](docs/how-to/choose-a-state-backend.md) compares them.

## Dedupe, Locks, And Concurrency

Event dedupe uses `Event Identity`, not delivery retry metadata. Slack retry
headers are logged as retry metadata but are not part of the dedupe key.

Default runtime options:

```go
chat.RuntimeOptions{
	DedupeTTL:     24 * time.Hour,
	ThreadLockTTL: 2 * time.Minute,
	Concurrency:   chat.ConcurrencyDrop,
}
```

Two concurrency strategies are implemented: `ConcurrencyDrop` (the default)
acknowledges and drops events that hit a locked thread, and `ConcurrencyQueue`
waits for the lock and runs only the most recent superseded follow-up
(ADR 0012). Burst, debounce, force, and concurrent strategies remain proposed
in ADR 0012 and are not implemented.

Thread locks use token-owned lock leases. Release and extend operations must
verify the token so an expired handler cannot release or extend another
handler's newer lock.

Lock conflict behavior defaults to acknowledge-and-drop. A lock conflict is
observed as unhandled runtime contention and should not trigger platform retry.

## Messages

The portable outbound surface is intentionally small:

```go
ev.Thread.Post(ctx, chat.Text("plain text"))
ev.Thread.Post(ctx, chat.Markdown("**portable** formatting intent"))
```

`Text` means no formatting intent. `Markdown` means conservative CommonMark
formatting intent, not Slack `mrkdwn`, GitHub-flavored Markdown, or a
platform-native rich payload. Adapters may render, translate, or degrade it.
The Slack adapter uses Slack's `markdown_text` posting field for Markdown
messages rather than converting CommonMark to `mrkdwn` itself.

Posting returns `SentMessage` identity. Edit, delete, reactions, files, and
typed rich payload builders are outside the portable surface. Platform-native
content and Slack modal opening are reachable deliberately through typed
adapter access (see [Command And Interaction Events](#command-and-interaction-events)).

## Ephemeral Messages

Ephemeral delivery is required for the Slack-first slice:

```go
sent, err := ev.Thread.PostEphemeral(ctx, ev.Message.Author, chat.Text(
	"Please link your account.",
), chat.EphemeralOptions{
	FallbackToDM: true,
})
```

An ephemeral message is not a normal thread reply and must never fall back to a
public reply.

Fallback is explicit:

- If native ephemeral delivery works, the adapter sends native ephemeral output.
- If native ephemeral delivery is unavailable and `FallbackToDM` is true, the
  adapter may deliver through a direct message thread.
- If native ephemeral delivery is unavailable and `FallbackToDM` is false, the
  operation returns no delivered message.
- If fallback is requested but impossible, the operation returns an error.

Ephemeral behavior is modeled as an optional adapter capability through small Go
interfaces, not string capability flags.

## Message History

Message history is application-owned. The runtime owns coordination state
(subscriptions, dedupe, locks), not a message store; durable transcripts, LLM
context windows, summaries, and RAG corpora are Thread Application State kept in
the application's own storage keyed by Thread ID.

For the common "fetch recent platform messages for this thread" case, an adapter
may implement the `HistoryReader` Optional Capability, reached through typed
adapter access like other capabilities:

```go
hr, ok := chat.AdapterAs[interface{ chat.HistoryReader }](bot, "slack")
if ok {
	msgs, err := hr.ReadHistory(ctx, ev.Thread.ID(), chat.HistoryQuery{Limit: 20})
	// The app decides what, if anything, to persist as Thread Application State.
}
```

`HistoryReader` is a thin live read-through, not history persistence:

- `ReadHistory` reads the platform API directly, keyed by the opaque Thread ID.
  It performs no runtime storage: no Runtime State writes, no dedupe, no caching.
- It is reached only through `chat.AdapterAs`; there is no `bot.ReadHistory` and no
  `Thread.History`, and history is never a routing hook input or auto-fetched
  during dispatch.
- Absence of the capability is the explicit unsupported result (`ok == false`),
  never an empty slice that masquerades as "no history".
- Ordering, pagination, and page-size clamping are adapter-owned and documented in
  each adapter's GoDoc. The Slack adapter returns messages newest-first, pages
  toward older messages via a `Before` cursor that is a `Message.ID`, and clamps
  the limit to Slack's maximum. The Linear adapter reads agent-session threads
  from the session's agent activities and issue-comment threads from the root
  comment and its replies, with the same newest-first ordering and `Before`
  cursor semantics, clamped to Linear's maximum page size.
- Long fetches run after ack via the ack-then-work seam; the runtime never fetches
  history on the inbound request path.

This deliberately diverges from Vercel Chat SDK's end-to-end stored-history model:
persistence of conversation content stays an application concern.

## Actors And Identity

`Actor` is scoped by adapter and platform tenant. Raw Slack user IDs are not
global identities.

Bot-ness is explicit:

```go
type BotKind int

const (
	BotUnknown BotKind = iota
	BotHuman
	BotBot
)
```

Self-authored bot messages are ignored before subscription or mention routing.

Application identity is not part of the runtime. Account linking, login prompts,
pending auth flows, and product user records belong to the application.

## Adapter Access

Normalized APIs should cover common flows. Platform-specific APIs are still
reachable through typed adapter access:

```go
slackAdapter, ok := chat.AdapterAs[*slack.Adapter](bot, "slack")
if !ok {
	return errors.New("slack adapter is not registered")
}
```

Examples should prefer this helper over unchecked type assertions.

## Slack Adapter Status

The Slack adapter is the first `supported` adapter. The implementation covers:

- single-install configuration
- multi-tenant installs via an application-implemented `InstallStore` (ADR 0006)
- signing secret verification
- URL verification
- bot identity discovery during adapter initialization
- supported-shape decoding with unknown-field tolerance
- message-created normalization
- direct-message normalization
- root-message thread rooting
- self-message filtering
- retry metadata observation
- thread replies
- plain text and portable markdown posting, using Slack's `markdown_text` field
  for Markdown messages
- native ephemeral messages
- explicit ephemeral DM fallback
- slash commands as Command Events and `block_actions` as Interaction Events
  (ADR 0003, ADR 0004)
- native Block Kit posting, modal open, and `response_url` responses via typed
  adapter access
- Web API rate-limit retry with `Retry-After` handling, bounded backoff, and a
  typed `RateLimited` error (ADR 0005)
- thread history read-through via the `HistoryReader` Optional Capability
  (ADR 0009)

The adapter uses local structs for the Slack payload shapes it supports,
preserves raw payload data as an escape hatch, and validates required fields
for supported event types.

This is still not a complete Slack product surface: see
[Intentional Gaps](#intentional-gaps) for what is deliberately absent.

## Linear Adapter Status

The Linear adapter is `experimental`: the implementation is broad and
hardened, but the upstream Linear agent API is itself in developer preview,
so no production promises are made yet. The implementation covers:

- single-install app-actor client credentials with granted-scope verification
- multi-tenant installs via an application-implemented `InstallStore`, with
  per-install webhook secrets and credentials or pre-exchanged access tokens
  (ADR 0006)
- webhook signing secret verification and timestamp replay checks
- app actor and organization identity discovery during adapter initialization
- Linear `AgentSessionEvent` created and prompted normalization, including
  assignment/delegation-created sessions emitted by Linear
- generic issue/comment participation outside agent sessions, with a
  thread-kind discriminator in the opaque thread ID (ADR 0013)
- source-comment-based event identity for dedupe
- tenant-correct opaque Linear thread IDs
- runtime self-message filtering through the discovered app actor identity
- thread handle reconstruction for stored Linear thread IDs
- the full agent activity surface through typed adapter access: thoughts,
  responses, actions, elicitations, errors, and session updates with plans and
  external URLs (ADR 0008)
- GraphQL rate-limit retry with a typed `RateLimited` error (ADR 0005)
- a `GraphQL` escape hatch and a `RawMessage` escape hatch (including the
  user-initiated stop signal)
- plain text and portable markdown pass-through for Linear activity bodies
- one memory-backed hello-world example with setup and dogfooding instructions

The Linear adapter follows the Slack adapter pattern: supported payload shapes are
modeled locally, low-level HTTP/GraphQL calls stay private, and public
platform-specific behavior is exposed through narrow methods rather than a raw
Linear client.

For the tracked list of Linear agent APIs and best-practice behaviors that are
not yet implemented, see
[`docs/linear-agent-capabilities.md`](docs/linear-agent-capabilities.md).

## Non-Goals

These are deliberate design boundaries, each recorded in an ADR. They are not
"not yet":

- **Streaming token transport in the core runtime** — [ADR 0011](docs/adr/0011-resumable-streaming.md)
  defers token streaming and pub/sub transports out of core (without
  foreclosing a future optional capability); long generation is ack-then-work
  ([ADR 0002](docs/adr/0002-async-dispatch.md)) posting one finished message.
- **LLM routing and prompt orchestration** — the runtime coordinates
  conversations; LLM calls, prompt assembly, and generation pipelines are
  application concerns inside handlers ([ADR 0011](docs/adr/0011-resumable-streaming.md)
  classifies generation and stream persistence as app/LLM concerns;
  [`CONTEXT.md`](CONTEXT.md) defines the runtime boundary).
- **A generative-UI card DSL** — [ADR 0004](docs/adr/0004-interactive-components.md)
  rejected a cross-platform card model as lossy; platform-native payloads ship
  opaquely via `NativeContentPoster` instead.
- **RAG and embeddings** — [ADR 0009](docs/adr/0009-message-history.md) keeps
  embeddings, summaries, and RAG corpora as Thread Application State in the
  application's own database keyed by Thread ID.
- **Durable transcript persistence in `chat.State`** — [ADR 0009](docs/adr/0009-message-history.md)
  rejected baking a message store into runtime state; `chat.State` stays
  subscriptions, dedupe, and locks.
- **App-user auth orchestration** — [ADR 0006](docs/adr/0006-multi-tenant-install.md)
  scopes the install store to platform-tenant credentials; account linking,
  login prompts, and OAuth web flows are Application Identity and stay
  app-owned.

## Intentional Gaps

These are not bugs; they are things the current scope deliberately does not
include:

- no TypeScript API compatibility
- no full Vercel Chat SDK feature parity
- no multiple handlers per routing hook
- no lazy runtime initialization
- no Linear personal API key mode, and no single-install static access token
  (pre-exchanged access tokens are supported through the multi-tenant
  `InstallStore`)
- no Linear streaming, reactions, history read-through, or Markdown conversion
- no built-in OAuth web flow: authorize/callback/token-exchange routes and
  install storage are application-owned (ADR 0006)
- no live Slack end-to-end test in CI
- no dedicated `OnDirectMessage` hook
- no public proactive `OpenDM`, except adapter behavior needed for explicit
  ephemeral fallback
- no pattern handlers
- no middleware
- no history persistence APIs: `HistoryReader` is a storage-free live
  read-through, and only the Slack adapter implements it
- no thread application state APIs
- no JSX cards, files, or typed Block Kit / Adaptive Card payload builders
  (native Block Kit content ships as an opaque payload via `NativeContentPoster`)
- no Slack shortcuts or Block Kit workflow steps (block_actions buttons and menus
  are routed as Interaction Events)
- no synchronous modal `view_submission` response (modal-open via `views.open`
  ships; the synchronous `response_action` is incompatible with ack-then-work and
  is deferred)
- no edit, delete, reaction, or other outbound mutation APIs beyond what a native
  interaction response needs
- no bundled metrics framework, exporters, or scrape endpoint (an optional no-op
  `Observer` seam is provided; OpenTelemetry stays out of the core import graph)
- no burst, debounce, force, or concurrent lock-conflict strategies (drop and
  queue are implemented)
- no built-in HTTP server or router integrations
- no adapter marketplace/package conventions

## Testing Contract

Tests should verify external behavior and public contracts, not private
implementation details.

Required test families:

- runtime construction and shutdown
- handler registration and replacement
- routing order and no-op missing handlers
- explicit subscription and unsubscribe
- direct-message implicit mention routing
- self-message filtering
- accepted, ignored, rejected, duplicate, and lock-conflict events
- state conformance across memory, Redis, Postgres, and NATS
- token-owned lock lease acquire, release, extend, expiry, and stale release
- Slack signature verification and URL verification
- Slack golden payload normalization
- thread ID construction and validation
- thread handle reconstruction
- text, markdown, sent message, ephemeral, and ephemeral fallback posting
- typed adapter access
- README and GoDoc coverage for intentional Vercel differences

Local test commands:

```sh
mise run test
mise run test:root
mise run test:adapters
mise run test:examples
mise run test:nats
mise run test:postgres
mise run test:redis
```

`mise run test` is a composite task that runs the root module tests,
`test:adapters`, and `test:examples`. The adapter-focused task also exercises
the NATS, Redis, and Postgres state modules. The Redis and Postgres state
tests use Testcontainers for real backend coverage and skip when Docker is
unavailable; the NATS tests run against an embedded JetStream server.
