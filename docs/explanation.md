# Architecture And Design Decisions

Chat SDK Go's design is documented in two places, and this page is the index
over both:

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
| [0012](adr/0012-concurrency-strategy.md) | Concurrency strategy expansion (`queue` implemented; `burst`/`debounce`/`concurrent` staged) | Accepted (staged) |
| [0013](adr/0013-linear-generic-comments.md) | Linear generic issue/comment participation | Accepted |
| [0014](adr/0014-nats-state-adapter.md) | NATS JetStream state adapter | Accepted |

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
