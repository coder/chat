# Architecture And Design Decisions

Chat SDK Go's design is documented in two places, and this page is the index
over both. It also states the [design goals](#design-goals), maps the
project against [Vercel Chat SDK](#vercel-chat-sdk-alignment), and lists the
[non-goals](#non-goals) and [intentional gaps](#intentional-gaps).

- [`CONTEXT.md`](../CONTEXT.md) — the ubiquitous language and architecture
  document. It defines every domain term precisely (with the synonyms to
  avoid), states the architectural invariants as explicit relationships, and
  records resolved ambiguities against upstream Vercel Chat SDK behavior.
- [`docs/adr/`](adr/) — Architecture Decision Records. Every significant
  decision has one, including the decisions *not* to build something.

## Reading CONTEXT.md

If you want to understand the system, read `CONTEXT.md` top to bottom; it is
the single most information-dense document in the repository. Its `Language`
section groups the vocabulary by area — runtime lifecycle, the platform
adapter boundary, Linear session lifecycle, threads and routing, the
event/message model, dispatch and concurrency, observability, state and
history, content and formatting, and tenancy and identity. The
`Relationships` section is the closest thing to a formal specification of the
runtime's invariants, and `Flagged ambiguities` explains where and why the
design deliberately diverges from Vercel Chat SDK.

## Decision Records

| ADR | Decision | Status |
| --- | --- | --- |
| [0001](adr/0001-linear-app-actor-slice.md) | Linear app-actor slice before a full Linear adapter | Accepted |
| [0002](adr/0002-async-dispatch.md) | Deferred runtime dispatch (ack-then-work) | Accepted |
| [0003](adr/0003-slash-commands.md) | Command Events and slash command routing | Accepted |
| [0004](adr/0004-interactive-components.md) | Interaction Events and native content instead of a card DSL | Accepted |
| [0005](adr/0005-rate-limit-handling.md) | Rate-limit retry lives in adapters, with typed `RateLimited` errors | Accepted |
| [0006](adr/0006-multi-tenant-install.md) | Multi-tenant installs via app-implemented `InstallStore`; OAuth flows stay app-owned | Accepted |
| [0007](adr/0007-teams-adapter.md) | Microsoft Teams adapter approach (Bot Framework, direct HTTP) | Proposed — gated on a spike |
| [0008](adr/0008-linear-full-adapter.md) | Full Linear agent activity surface (thought/response/action/elicitation/error) plus session updates (plans, external URLs) | Accepted |
| [0009](adr/0009-message-history.md) | Message history stays application-owned; optional storage-free `HistoryReader` | Accepted |
| [0010](adr/0010-observability.md) | Optional `Observer` seam; no OpenTelemetry in core | Accepted |
| [0011](adr/0011-resumable-streaming.md) | Resumable streaming deferred from core, not foreclosed | Proposed |
| [0012](adr/0012-concurrency-strategy.md) | Concurrency strategy expansion (`drop`/`queue`/`debounce`/`concurrent`/`burst` + lock scope implemented; force/steerability names reserved) | Accepted |
| [0013](adr/0013-linear-generic-comments.md) | Linear generic issue/comment participation | Accepted |
| [0014](adr/0014-nats-state-adapter.md) | NATS JetStream state adapter | Accepted |
| [0015](adr/0015-runtime-coordination.md) | Deferred-dispatch admission bound; cross-instance coalescing rejected for now | Accepted |

## The Short Version

For readers who want the model in five paragraphs:

**The runtime coordinates conversations; it does not own your product.**
`chat.Chat` verifies webhooks through adapters, normalizes platform payloads
into events, dedupes them, serializes work per thread with token-owned lock
leases, and routes to your single-slot handlers. Everything your product
stores — transcripts, user records, workflow state — lives in your database,
keyed by the opaque `ThreadID`.

**Adapters own the platform boundary.** Signature verification, payload
normalization, outbound rendering, rate-limit retries, and platform quirks
live inside the adapter. Platform-specific power is reached deliberately via
typed adapter access (`chat.AdapterAs`), never by making raw platform structs
the normal API.

**State is required and small.** Subscriptions, dedupe marks, and locks —
that is all. Memory for development; Redis, Postgres, or NATS for production.

**Events are broader than messages.** A slash command and a button click are
normalized events with their own hooks, not messages. All events ride the
same dispatch spine.

**Semantic compatibility, not feature parity.** Vercel Chat SDK's
conversation model is the precedent; its TypeScript API shapes are not. Where
Go idioms or operational safety argue otherwise, this SDK deliberately
diverges and documents the divergence.

## Design Goals

- Go-native API built around `context.Context`, `net/http`, small
  interfaces, and explicit errors.
- Slack-first vertical slice before claiming multi-platform portability.
- Required runtime state for subscriptions, dedupe, and locks: memory for
  tests and local development; Redis, Postgres, or NATS JetStream for
  horizontally scaled production deployments.
- Thread-oriented application code: handle a message, subscribe the thread,
  reply to the thread.
- Platform escape hatches without making raw platform structs the normal
  API.
- Vercel Chat SDK behavior as the default precedent unless it is
  non-idiomatic in Go or outside the documented scope.

## Vercel Chat SDK Alignment

Chat SDK Go follows Vercel Chat SDK's conversation semantics where they fit
Go, built outward from a production-shaped Slack slice. It is
not a TypeScript API port and does not promise full feature parity. For
readers who know Vercel Chat SDK, this is the concept-by-concept status map:

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
| AI streaming responses | Deferred from core, not foreclosed ([ADR 0011](adr/0011-resumable-streaming.md)); long generation uses ack-then-work |
| Slash commands | Implemented as `OnCommand` Command Events (Slack) |
| Interactive components (buttons, menus) | Implemented as `OnInteraction` `block_actions` (Slack) |
| Native rich content (Block Kit) | Implemented as the `NativeContentPoster` Optional Capability (Slack) |
| Modal open (`views.open`) | Implemented as a Slack adapter Optional Capability |
| Modal `view_submission` synchronous response | Deferred (incompatible with ack-then-work) |
| Cards, JSX-style cards, native payload builders | Not implemented |
| Pattern handlers | Not implemented |
| Observability metrics/tracing | Optional `Observer` seam, no-op default, no OTel dependency in core |
| Message history persistence | App-owned (Thread Application State); thin live read-through via the `HistoryReader` Optional Capability (Slack, Linear) |
| AI-message conversion helpers | Not implemented |
| Multiple production adapters | Slack is the only `supported` adapter; Linear is `experimental` |
| Middleware | Not implemented |

The behavioral differences that matter when porting handler code — single-slot
hooks, fail-fast construction, explicit subscriptions, application-owned
history — are documented on the affected symbols' GoDoc and in the
[reference](reference.md).

## Non-Goals

These are deliberate design boundaries, each recorded in an ADR. Most are
permanent ownership boundaries; streaming is the one explicitly *deferred*
boundary — out of the core runtime today, not foreclosed forever.

- **Streaming token transport in the core runtime** —
  [ADR 0011](adr/0011-resumable-streaming.md) defers token streaming and
  pub/sub transports out of core (without foreclosing a future optional
  capability); long generation is ack-then-work
  ([ADR 0002](adr/0002-async-dispatch.md)) posting one finished message.
- **LLM routing and prompt orchestration** — the runtime coordinates
  conversations; LLM calls, prompt assembly, and generation pipelines are
  application concerns inside handlers
  ([ADR 0011](adr/0011-resumable-streaming.md) classifies generation and
  stream persistence as app/LLM concerns; [`CONTEXT.md`](../CONTEXT.md)
  defines the runtime boundary).
- **A generative-UI card DSL** —
  [ADR 0004](adr/0004-interactive-components.md) rejected a cross-platform
  card model as lossy; platform-native payloads ship opaquely via
  `NativeContentPoster` instead.
- **RAG and embeddings** — [ADR 0009](adr/0009-message-history.md) keeps
  embeddings, summaries, and RAG corpora as Thread Application State in the
  application's own database keyed by Thread ID.
- **Durable transcript persistence in `chat.State`** —
  [ADR 0009](adr/0009-message-history.md) rejected baking a message store
  into runtime state; `chat.State` stays subscriptions, dedupe, and locks.
- **App-user auth orchestration** —
  [ADR 0006](adr/0006-multi-tenant-install.md) scopes the install store to
  platform-tenant credentials; account linking, login prompts, and OAuth web
  flows are Application Identity and stay app-owned.

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
- no Linear streaming, reactions, or Markdown conversion
- no built-in OAuth web flow: authorize/callback/token-exchange routes and
  install storage are application-owned
  ([ADR 0006](adr/0006-multi-tenant-install.md))
- no live Slack end-to-end test in CI
- no dedicated `OnDirectMessage` hook
- no public proactive `OpenDM`, except adapter behavior needed for explicit
  ephemeral fallback
- no pattern handlers
- no middleware
- no history persistence APIs: `HistoryReader` is a storage-free live
  read-through, implemented by the Slack and Linear adapters
- no thread application state APIs
- no JSX cards, files, or typed Block Kit / Adaptive Card payload builders
  (native Block Kit content ships as an opaque payload via
  `NativeContentPoster`)
- no Slack shortcuts or Block Kit workflow steps (`block_actions` buttons and
  menus are routed as Interaction Events)
- no synchronous modal `view_submission` response (modal open via
  `views.open` ships; the synchronous `response_action` is incompatible with
  ack-then-work and is deferred)
- no edit, delete, reaction, or other outbound mutation APIs beyond what a
  native interaction response needs
- no bundled metrics framework, exporters, or scrape endpoint (an optional
  no-op `Observer` seam is provided; OpenTelemetry stays out of the core
  import graph)
- no built-in HTTP server or router integrations
- no adapter marketplace/package conventions
