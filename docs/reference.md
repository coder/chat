# Reference

The API reference is the GoDoc: every package carries package-level
documentation (`doc.go`), and the intentional differences from Vercel Chat
SDK are documented on the symbols they affect. This page covers what GoDoc
does not: the module layout, the runtime's semantics organized by concept,
per-adapter capability status, the runnable examples, and the testing
contract.

## Modules And Packages

| Package | Module | GoDoc |
| --- | --- | --- |
| `github.com/coder/chat` | core | [pkg.go.dev](https://pkg.go.dev/github.com/coder/chat) |
| `github.com/coder/chat/adapters/slack` | core | [pkg.go.dev](https://pkg.go.dev/github.com/coder/chat/adapters/slack) |
| `github.com/coder/chat/adapters/linear` | core | [pkg.go.dev](https://pkg.go.dev/github.com/coder/chat/adapters/linear) |
| `github.com/coder/chat/state/memory` | core | [pkg.go.dev](https://pkg.go.dev/github.com/coder/chat/state/memory) |
| `github.com/coder/chat/state/redis` | separate | [pkg.go.dev](https://pkg.go.dev/github.com/coder/chat/state/redis) |
| `github.com/coder/chat/state/postgres` | separate | [pkg.go.dev](https://pkg.go.dev/github.com/coder/chat/state/postgres) |
| `github.com/coder/chat/state/nats` | separate | [pkg.go.dev](https://pkg.go.dev/github.com/coder/chat/state/nats) |

Redis, Postgres, and NATS state live in separate Go modules so applications
that only use core, Slack, or memory state do not pull their dependencies.
The repository uses `go.work` for local development across the root module,
the state modules, and the example modules.

To browse the reference locally without pkg.go.dev:

```sh
go doc github.com/coder/chat
go doc github.com/coder/chat/adapters/slack
```

### Where To Look For What

- **Runtime construction, hooks, dispatch, runtime options**: package `chat`
  (`chat.New`, `Chat.OnNewMention`, `Chat.OnSubscribedMessage`,
  `Chat.OnCommand`, `Chat.OnInteraction`, `RuntimeOptions`).
- **Event and message model**: package `chat` (`Event`, `Message`,
  `MessageEvent`, `CommandEvent`, `InteractionEvent`, `Thread`, `ThreadID`,
  `Actor`).
- **Optional capabilities**: package `chat` (`NativeContentPoster`,
  `HistoryReader`, `EphemeralPoster` interfaces) plus the adapter packages
  for what each adapter actually implements.
- **Multi-tenant installs**: package `chat` (`InstallStore`, `Install`,
  `ErrInstallNotFound`) plus `slack.SlackInstall` / `linear.LinearInstall`.
- **State contract**: package `chat` (`State`) with implementations in the
  four `state/*` packages.

## Core Model

`Chat` is the runtime. It owns adapter registration, runtime state, handler
registration, webhook mounting, dispatch, dedupe, locking, and shutdown.

A `Platform Adapter` is a platform boundary. It verifies inbound webhooks,
normalizes platform payloads, renders outbound messages, and exposes
platform-specific APIs through typed adapter access. It does not own
application routing.

`Event` is the normalized inbound envelope. A `Message` is one payload type
inside an event, not the name for every inbound platform occurrence: a slash
command and a button click are events too (see
[Command And Interaction Events](#command-and-interaction-events)).

`MessageEvent` is the handler input for the message routing hooks. It carries
the normalized event, thread, and message together.

`Thread` is the stable conversation address used for routing, subscription,
and replies. In Slack, a root channel message becomes a thread rooted at that
message timestamp, not the entire channel.

`ThreadID` is opaque and adapter-produced. It includes adapter identity and
enough platform tenant and routing context to avoid collisions across
workspaces, channels, and platforms. Application code may store and pass it
around, but must not build it manually.

Thread handles can be reconstructed from a stored `ThreadID` for
out-of-webhook work such as reminders or proactive follow-ups:

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
	chat.WithLogger(slog.Default()),
	chat.WithRuntimeOptions(chat.DefaultRuntimeOptions()),
)
if err != nil {
	return err
}
defer func() {
	if err := bot.Shutdown(context.Background()); err != nil {
		slog.Error("chat shutdown failed", "error", err)
	}
}()
```

`chat.New` requires a `State` and at least one adapter, validates the runtime
options, and initializes every adapter (which is where the Slack adapter
discovers its own bot identity) before any webhook is served. A failing
adapter initialization shuts down the adapters registered so far and returns
the joined error. This is an intentional difference from Vercel Chat
SDK, which initializes lazily on first use.

Options:

- `WithState(State)` — required. The runtime never silently creates memory
  state; see [Runtime State](#runtime-state).
- `WithAdapter(Adapter)` — at least one; adapter names must be unique.
- `WithLogger(*slog.Logger)` — defaults to `slog.Default()`.
- `WithObserver(Observer)` — optional; see [Observability](#observability).
- `WithRuntimeOptions(RuntimeOptions)` — replaces the whole options struct
  (it does not merge), so start from `chat.DefaultRuntimeOptions()`. See
  [Dedupe, Locks, And Concurrency](#dedupe-locks-and-concurrency).

`Shutdown(ctx)` is idempotent. It cancels detached work, runs every
adapter's cleanup hook before state cleanup, and returns joined errors if
cleanup fails.

## Webhooks

The runtime exposes `net/http` handlers and does not own the HTTP server:

```go
handler, err := bot.Webhook("slack")
if err != nil {
	return err
}

http.Handle("/webhooks/slack", handler)
```

Webhook lookup is fallible: a misspelled adapter name is a startup error, not
a production 404.

Adapters own platform handshakes. For Slack, the `url_verification`
challenge is answered inside the Slack webhook handler and never reaches
application handlers.

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
4. Acknowledge and ignore any other valid platform event.

Direct messages are implicit mentions: an unsubscribed direct message routes
to `OnNewMention`; once the thread is subscribed, later direct messages route
to `OnSubscribedMessage`. There is no dedicated direct-message hook.

Handlers are single-slot per hook. Calling `OnNewMention` or
`OnSubscribedMessage` again atomically replaces the previous handler. A
missing handler is a no-op that still acknowledges the platform. This
intentionally differs from Vercel Chat SDK, which allows multiple handlers
per hook.

Subscriptions are explicit:

```go
if err := ev.Thread.Subscribe(ctx); err != nil {
	return err
}
```

Replying to a new mention does not subscribe the thread. A subscription lasts
until `Thread.Unsubscribe`.

### Command And Interaction Events

A slash command and a button click are events, not messages. They ride the
same dispatch spine (dedupe by event identity, thread lock, self-filtering,
lock-conflict acknowledge-and-drop) but route to their own single-slot hooks:

- `OnCommand(func(ctx, *chat.CommandEvent) error)` for Command Events (Slack
  slash commands). Command-ness takes precedence over subscription state: a
  command in a subscribed thread routes to `OnCommand`, never to
  `OnSubscribedMessage`. A command does not auto-subscribe its thread.
- `OnInteraction(func(ctx, *chat.InteractionEvent) error)` for Interaction
  Events: Slack `block_actions` on messages (button clicks, menu selections).
  `block_actions` raised inside a modal view are not normalized and are
  rejected before routing.

Both hooks are single-slot and no-op when unset, like the message hooks; an
unset handler is still acknowledged. The platform acknowledgement is
adapter-owned: the Slack adapter returns an empty 2xx and preserves
`response_url` and `trigger_id` on the `Raw` platform escape hatch. Under the
default synchronous dispatch the handler runs before that acknowledgement, so
long command or interaction work needs
[deferred dispatch](how-to/deferred-dispatch.md) to stay inside Slack's
3-second budget, and bots expecting commands or clicks mid-conversation
should select the `ConcurrencyQueue` strategy.

Native command and interaction responses and Block Kit content are not part of
the portable `PostableMessage` surface (plain text and portable Markdown).
They are reached deliberately through typed adapter access:

- `chat.NativeContentPoster.PostNative` posts opaque Block Kit blocks. A
  `NativeContent` whose `Adapter` does not match the target adapter is an
  error, never a silent portable downgrade.
- The Slack adapter's `OpenModalFromRaw` (and `OpenModal` for callers holding
  a `trigger_id`) opens a modal via `views.open`. The synchronous modal
  `view_submission` response is deferred because it is incompatible with
  ack-then-work; the adapter acknowledges and drops `view_submission`
  payloads.
- The Slack adapter's `RespondURL` posts to a preserved `response_url`.

The [slash commands](how-to/slash-commands.md) and
[interactive components](how-to/interactive-components.md) guides show the
Slack configuration and handler patterns.

## Dispatch And Acknowledgement

The default dispatch mode is synchronous (`DispatchSync`): handlers run on the
inbound webhook request context before the platform acknowledgement. For
long-running work, opt in to `DispatchDeferred` (ack-then-work,
[ADR 0002](adr/0002-async-dispatch.md)): the dedupe and lock prelude runs
before the acknowledgement, then the handler runs on a detached work context
with automatic lock lease renewal, bounded by `DetachTimeout`. Under deferred
dispatch `MaxDetached` bounds admitted-but-incomplete deliveries; a delivery
arriving at the bound is rejected with `chat.ErrAdmissionRejected` before
acknowledgement and before dedupe marking ([ADR 0015](adr/0015-runtime-coordination.md)).
The [deferred dispatch guide](how-to/deferred-dispatch.md) covers enabling
it and writing handlers for the detached context.

Once a webhook is verified and normalized into an accepted event, handler
errors are logged and observed but the event is still acknowledged to the
platform. This avoids platform retry storms after partial side effects such as
posting a message.

Invalid signatures and malformed requests are rejected. Valid but unsupported
platform events are acknowledged and ignored.

### Observability

Logging is structured `slog` through `WithLogger`. The optional
`WithObserver(Observer)` seam ([ADR 0010](adr/0010-observability.md)) adds
counter-style point events (dedupe hit, lock conflict, ignored event by
reason, handler error, lock-release failure, admission rejection, adapter
call, rate limit) and a per-dispatch span with a terminal outcome
(`handled`, `ignored`, `dropped-lock-conflict`, `duplicate`, `error`,
`preempted`, `admission-rejected`). The default is a no-op observer, so an
unconfigured runtime behaves exactly as without the seam.

The core imports no OpenTelemetry, Prometheus, or statsd. Attribute keys are
a closed, low-cardinality set (`adapter`, `route`, `reason`, `outcome`,
`tenant`) and never carry thread IDs, message text, or raw actor IDs.
Observer calls are panic-safe: a broken observer can never fail an accepted
event or alter acknowledgement. Under deferred dispatch the span follows the
detached work context, so ack-then-work latency is measured to handler
completion.

## Runtime State

State is required. The runtime never silently creates memory state.

Runtime state is coordination state:

- subscribed thread membership
- event dedupe marks
- thread lock leases

Runtime state is not product state. Store application workflow data in your
own database keyed by `ThreadID`.

The `chat.State` contract is small — subscription membership, `MarkEvent`
for dedupe, and token-owned `AcquireLock` / `ExtendLock` / `ReleaseLock` —
and four implementations ship:

- `state/memory`: tests and local development, in the root module.
- `state/redis`, `state/postgres`, `state/nats`: production and horizontally
  scaled deployments, each in its own module.

All backends pass the same conformance suite. The
[state backend guide](how-to/choose-a-state-backend.md) compares them and
covers namespacing when several bots share one backend.

## Dedupe, Locks, And Concurrency

Event dedupe uses event identity, not delivery retry metadata. Slack retry
headers are logged as retry metadata but are not part of the dedupe key.

Default runtime options (`chat.DefaultRuntimeOptions()`):

```go
chat.RuntimeOptions{
	DedupeTTL:     24 * time.Hour,
	ThreadLockTTL: 2 * time.Minute,
	Concurrency:   chat.ConcurrencyDrop,
	Dispatch:      chat.DispatchSync,
	LockScope:     chat.LockScopeThread,
	MaxDetached:   1024,
}
```

The runtime implements all five upstream-aligned concurrency strategies
([ADR 0012](adr/0012-concurrency-strategy.md)):

- `ConcurrencyDrop` (default): a lock conflict is acknowledged and dropped.
- `ConcurrencyQueue`: the newest follow-up waits for the in-flight handler;
  superseded follow-ups are observable, never silent.
- `ConcurrencyDebounce`: each new routed event supersedes the previous
  waiter; only the final event in a `DebounceInterval` quiet period
  dispatches, and superseded events are observable. Requires deferred
  dispatch.
- `ConcurrencyConcurrent`: no thread lock at all; every event dispatches in
  its own execution, bounded by `MaxConcurrent`.
- `ConcurrencyBurst`: routed events for a scope collect for a `BurstWindow`,
  then dispatch as one batch under a single lock hold, each member in join
  order with its own `DetachTimeout` budget; `MaxBurstBatch` optionally seals
  a full window early. Requires deferred dispatch.

Queue supersession, debounce coalescing, and burst batching are per runtime
instance. Instances sharing a state are serialized by the thread lock, not
coalesced ([ADR 0015](adr/0015-runtime-coordination.md)). The
force/steerability (`onLockConflict`) preemption hook from ADR 0012 is
rejected for v0.x by ADR 0015; the names stay reserved behind that ADR's
formal-design bar.

`LockScope` chooses what the lock guards: per thread (`LockScopeThread`, the
default) or per channel (`LockScopeChannel`) for platforms whose model needs
channel-wide ordering.

Thread locks are token-owned lock leases. Release and extend operations
verify the token, so an expired handler cannot release or extend another
handler's newer lock. A deferred handler whose lock lease is lost mid-run
(released elsewhere, expired, or no longer refreshable) is cancelled with
`chat.ErrPreempted` as its context cause rather than running on
unserialized.

Lock conflict behavior defaults to acknowledge-and-drop. A lock conflict is
observed as runtime contention and does not trigger a platform retry.

## Messages

The portable outbound surface is intentionally small:

```go
ev.Thread.Post(ctx, chat.Text("plain text"))
ev.Thread.Post(ctx, chat.Markdown("**portable** formatting intent"))
```

`Text` means no formatting intent. `Markdown` means conservative CommonMark
formatting intent, not Slack `mrkdwn`, GitHub-flavored Markdown, or a
platform-native rich payload. Adapters may render, translate, or degrade it.
The Slack adapter posts Markdown messages through Slack's `markdown_text`
field rather than converting CommonMark to `mrkdwn` itself.

Posting returns the `SentMessage` identity. Edit, delete, reactions, files,
and typed rich payload builders are outside the portable surface.
Platform-native content and Slack modal opening are reachable deliberately
through typed adapter access (see
[Command And Interaction Events](#command-and-interaction-events)).

### Ephemeral Messages

```go
sent, err := ev.Thread.PostEphemeral(ctx, ev.Message.Author, chat.Text(
	"Please link your account.",
), chat.EphemeralOptions{
	FallbackToDM: true,
})
```

An ephemeral message is not a normal thread reply and never falls back to a
public reply. Fallback is explicit:

- If native ephemeral delivery works, the adapter sends native ephemeral
  output.
- If native ephemeral delivery is unavailable and `FallbackToDM` is true, the
  adapter may deliver through a direct message thread.
- If native ephemeral delivery is unavailable and `FallbackToDM` is false, the
  operation returns no delivered message.
- If fallback is requested but impossible, the operation returns an error.

Ephemeral delivery is an optional adapter capability expressed through a
small Go interface (`EphemeralPoster`), not a string capability flag. The
Slack adapter implements it; on adapters that do not, `PostEphemeral` returns
`chat.ErrUnsupportedCapability`.

## Message History

Message history is application-owned. The runtime owns coordination state
(subscriptions, dedupe, locks), not a message store; durable transcripts, LLM
context windows, summaries, and RAG corpora are Thread Application State kept
in the application's own storage keyed by Thread ID
([ADR 0009](adr/0009-message-history.md)).

For the common "fetch recent platform messages for this thread" case, an
adapter may implement the `HistoryReader` Optional Capability, reached through
typed adapter access like other capabilities:

```go
hr, ok := chat.AdapterAs[chat.HistoryReader](bot, "slack")
if ok {
	msgs, err := hr.ReadHistory(ctx, ev.Thread.ID(), chat.HistoryQuery{Limit: 20})
	// The app decides what, if anything, to persist as Thread Application State.
}
```

`HistoryReader` is a thin live read-through, not history persistence:

- `ReadHistory` reads the platform API directly, keyed by the opaque Thread
  ID. It performs no runtime storage: no runtime state writes, no dedupe, no
  caching.
- It is reached only through `chat.AdapterAs`; there is no `bot.ReadHistory`
  and no `Thread.History`, and history is never a routing hook input or
  auto-fetched during dispatch.
- Absence of the capability is the explicit unsupported result
  (`ok == false`), never an empty slice that masquerades as "no history".
- Ordering, pagination, and page-size clamping are adapter-owned and
  documented in each adapter's GoDoc. The Slack adapter returns messages
  newest-first, pages toward older messages via a `Before` cursor that is a
  `Message.ID`, and clamps the limit to Slack's maximum. The Linear adapter
  reads agent-session threads from the session's agent activities and
  issue-comment threads from the root comment and its replies, with the same
  newest-first ordering and `Before` cursor semantics, clamped to Linear's
  maximum page size.
- Long fetches belong after the acknowledgement, under deferred dispatch; the
  runtime never fetches history on the inbound request path.

This deliberately diverges from Vercel Chat SDK's end-to-end stored-history
model: persistence of conversation content stays an application concern.

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

Self-authored bot messages are ignored before subscription or mention
routing.

Application identity is not part of the runtime. Account linking, login
prompts, pending auth flows, and product user records belong to the
application.

## Adapter Access

Normalized APIs cover the common flows. Platform-specific APIs remain
reachable through typed adapter access:

```go
slackAdapter, ok := chat.AdapterAs[*slack.Adapter](bot, "slack")
if !ok {
	return errors.New("slack adapter is not registered")
}
```

`chat.AdapterAs[T](bot, name)` returns `(T, bool)`; prefer it over unchecked
type assertions. Optional capabilities (`NativeContentPoster`,
`HistoryReader`, `EphemeralPoster`) are reached the same way.

## Adapter Capability Status

Portable behavior (normalized events, thread routing, `Thread.Post`,
`Thread.Subscribe`) works on every adapter. Optional capabilities and
platform-specific surfaces differ:

| Capability | Slack | Linear |
| --- | --- | --- |
| Message events (mentions, subscribed threads, DMs) | Yes | Yes (agent sessions and issue comments) |
| Ephemeral messages with explicit DM fallback (`Thread.PostEphemeral`) | Yes | No (unsupported capability); Linear-specific ephemeral *thoughts* via `PostThought` on agent-session threads |
| Slash commands (`OnCommand`) | Yes | No (no platform equivalent) |
| Interactive components (`OnInteraction`) | Yes (message `block_actions`; modal-view actions are not normalized) | No |
| Native content posting (`NativeContentPoster`) | Yes (Block Kit) | No |
| Modal open / `response_url` | Yes (`OpenModalFromRaw`, `OpenModal`, `RespondURL`) | No |
| Message history read-through (`HistoryReader`) | Yes | Yes (agent-session activities and issue-comment threads) |
| Rate-limit retry with typed `RateLimited` error | Yes | Yes |
| Multi-tenant installs (`InstallStore`) | Yes | Yes |
| Platform escape hatch | Raw payloads on events | `RawMessage`, `GraphQL` |
| Agent activities (thought/response/action/elicitation/error) | n/a | Yes |
| Session updates (plan, external URLs) | n/a | Yes (`UpdateSession`) |

### Slack Adapter

The Slack adapter (`adapters/slack`) is the `supported` adapter. It covers:

- single-install configuration, and multi-tenant installs via an
  application-implemented `InstallStore`
  ([ADR 0006](adr/0006-multi-tenant-install.md))
- signing secret verification and URL verification
- bot identity discovery during adapter initialization
- supported-shape decoding with unknown-field tolerance
- message-created and direct-message normalization, root-message thread
  rooting, and self-message filtering
- retry metadata observation
- thread replies in plain text and portable Markdown (via Slack's
  `markdown_text` field)
- native ephemeral messages with explicit DM fallback
- slash commands as Command Events and `block_actions` as Interaction Events
  ([ADR 0003](adr/0003-slash-commands.md),
  [ADR 0004](adr/0004-interactive-components.md))
- native Block Kit posting, modal open, and `response_url` responses via
  typed adapter access
- Web API rate-limit retry with `Retry-After` handling, bounded backoff, and
  a typed `RateLimited` error ([ADR 0005](adr/0005-rate-limit-handling.md))
- thread history read-through via the `HistoryReader` Optional Capability
  ([ADR 0009](adr/0009-message-history.md))

The adapter uses local structs for the Slack payload shapes it supports,
preserves raw payload data as an escape hatch, and validates required fields
for supported event types. It is not a complete Slack product surface; see
[Intentional Gaps](explanation.md#intentional-gaps).

### Linear Adapter

The Linear adapter (`adapters/linear`) is `experimental`: the implementation
is broad and hardened, but the upstream Linear agent API is itself in
developer preview, so no production promises are made yet. It covers:

- single-install app-actor client credentials with granted-scope
  verification, and multi-tenant installs via an application-implemented
  `InstallStore` with per-install webhook secrets and credentials or
  pre-exchanged access tokens ([ADR 0006](adr/0006-multi-tenant-install.md))
- webhook signing secret verification and timestamp replay checks
- app actor and organization identity discovery during adapter
  initialization
- Linear `AgentSessionEvent` created and prompted normalization, including
  assignment/delegation-created sessions emitted by Linear
- generic issue/comment participation outside agent sessions, with a
  thread-kind discriminator in the opaque thread ID
  ([ADR 0013](adr/0013-linear-generic-comments.md))
- source-comment-based event identity for dedupe, tenant-correct opaque
  thread IDs, and thread handle reconstruction for stored thread IDs
- self-message filtering through the discovered app actor identity
- the full agent activity surface through typed adapter access: thoughts,
  responses, actions, elicitations, errors, and session updates with plans
  and external URLs ([ADR 0008](adr/0008-linear-full-adapter.md))
- thread history read-through via the `HistoryReader` Optional Capability,
  reading agent-session activities and issue-comment threads
  ([ADR 0009](adr/0009-message-history.md))
- GraphQL rate-limit retry with a typed `RateLimited` error
  ([ADR 0005](adr/0005-rate-limit-handling.md))
- a `GraphQL` escape hatch and a `RawMessage` escape hatch (including the
  user-initiated stop signal)
- plain text and portable Markdown pass-through for Linear activity bodies

The Linear adapter follows the Slack adapter pattern: supported payload
shapes are modeled locally, low-level HTTP and GraphQL calls stay private,
and platform-specific behavior is exposed through narrow methods rather than
a raw Linear client. The [Linear agent sessions guide](how-to/linear-agent-sessions.md)
walks through building an agent; the tracked list of Linear agent APIs not
yet wrapped is in [linear-agent-capabilities.md](linear-agent-capabilities.md).

## Examples

Runnable, documented examples live in [`examples/`](../examples/). Pick one:

- [`slack-hello-world`](../examples/slack-hello-world/README.md) — start here.
  Memory state, no infrastructure; the [tutorial](tutorials/slack-bot.md)
  walks through it end to end.
- [`slack-redis-state`](../examples/slack-redis-state/README.md),
  [`slack-postgres-state`](../examples/slack-postgres-state/README.md),
  [`slack-nats-state`](../examples/slack-nats-state/README.md) — the same bot
  on each durable state backend. Each lives in its own module (so the core
  module does not pull backend dependencies) and ships a `compose.yaml`, a
  `pitchfork.toml`, and a README with the backend URL, service startup, and
  Slack setup steps.
- [`linear-agent-hello-world`](../examples/linear-agent-hello-world/README.md) —
  Linear agent sessions with memory state. Requires a Linear OAuth app
  installed as an app actor and a public HTTPS webhook URL.

The memory-backed examples run without local infrastructure:

```sh
go run ./examples/slack-hello-world
go run ./examples/linear-agent-hello-world
```

The state-backed examples start their backend with Docker Compose (or let
Pitchfork supervise it from the example's `pitchfork.toml`):

```sh
cd examples/slack-redis-state
docker compose up -d redis   # or: pitchfork start redis
go run .
```

## Testing Contract

Tests verify external behavior and public contracts, not private
implementation details. Required test families:

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
- documentation coverage for intentional Vercel differences (README, this
  reference, the explanation page, and GoDoc)

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
