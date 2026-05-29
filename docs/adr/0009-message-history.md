# ADR 0009: Message History Persistence

## Status

Proposed

## Context

The **Go Chat Runtime** hands handlers only the current **Message Event**. It owns no record of prior turns in a **Thread**, and `CONTEXT.md` defines **Message History** (past platform-fetched messages) as deferred and app-owned, with **Thread Application State** living in the application's own storage keyed by **Thread ID**.

Vercel's full app/template, by contrast, owns persistence: it stores chat history and user data in Neon Postgres plus Vercel Blob and replays history into generation. That is an app/LLM concern. This runtime is a conversation-coordination runtime; **Runtime State** is coordination state (subscriptions, dedupe, **Lock Leases**), not a message store.

The gap is real but narrow. An app that wants recent platform messages today must call the platform API itself through typed **Adapter Access**. That works but is undiscoverable and re-implemented per adapter. The question for this ADR is whether the runtime should offer any history seam, and if so, how thin it can be without widening the core or the **Runtime State** contract.

This ADR reopens two MVP PRD non-goals to evaluate them deliberately (source: `.scratch/go-chat-runtime-mvp/PRD.md`, Out of Scope):

- **"Message history APIs"** — reopened to decide whether a read-through retrieval seam is warranted. Resolved below: affirmed as a non-goal for *storage*; allowed only as one optional live read-through capability.
- **"Thread application state APIs"** — reopened because stored conversation context (transcripts, LLM context windows, summaries, RAG) is the obvious adjacent ask. Resolved below: reaffirmed as app-owned **Thread Application State**; the runtime adds nothing here.

Neither reopening changes `CONTEXT.md`'s **Message History** or **Thread Application State** definitions; this ADR keeps both app-owned.

## Decision

Affirm platform/stored history as application-owned, and add at most one proposed **Optional Capability**: the **History Reader**.

- Define a narrow `HistoryReader` interface in the core `chat` package, alongside existing capability interfaces (e.g. `EphemeralPoster`). It is NOT added to the required `Adapter` interface.
- `HistoryReader.ReadHistory(ctx, ThreadID, HistoryQuery) ([]Message, error)` is a live platform read keyed by the opaque **Thread ID**. It returns normalized **Messages** with raw platform data preserved via the **Platform Escape Hatch** (`Message.Raw`).
- It is reached only through typed **Adapter Access** (`chat.AdapterAs`), the path used today for Slack ephemeral delivery and Linear `PostThought`. There is no top-level `bot.ReadHistory` and no `Thread.History`; history is never a **Routing Hook** input and is never auto-invoked during **Runtime Dispatch**.
- **History Reader** performs NO runtime storage: it does not write **Runtime State**, does not dedupe via **Event Identity**, and does not cache. The **Runtime State** interface and all `state/*` implementations (and the `internal/statetest` conformance suite) are unchanged.
- Stored conversation context — durable transcripts, LLM context windows, summaries, embeddings, RAG corpora — stays **Thread Application State** in the app's own database keyed by **Thread ID**. The runtime owns none of it.
- Absence of the capability returns the existing explicit unsupported-capability result, never an empty `[]Message` that masquerades as "no history".
- Pagination, ordering, and page-size clamping are adapter-owned and documented in GoDoc, because platform read APIs differ (Slack `conversations.history`/`replies`, Linear comment/activity reads, Teams Bot Framework transcripts). No portable pagination model is imposed.
- Read-side rate-limit/backoff lives in the **Platform Adapter** and is bounded by the caller's `context.Context` (ADR 0005). When history must be fetched during long work, the app uses the **Ack-Then-Work** / **Detached Work Context** seam (ADR 0002) after ack; the runtime never fetches history on the inbound request path.

Read-through history is opt-in, adapter-scoped, explicit, and storage-free. Portable conversation semantics and the **Runtime State** contract are untouched.

## Consequences

- The common "fetch recent platform messages for this **Thread**" case becomes discoverable and uniform across adapters that can support it, without growing the core surface or the `Adapter` interface.
- Apps still own persistence. Anything durable — transcripts, context windows, RAG — is **Thread Application State** in the app's database keyed by **Thread ID**. The runtime stays a coordination runtime.
- The four load-bearing patterns hold: single-slot **Routing Hooks** unchanged, **Thread ID** stays opaque and adapter-produced, the small `Adapter` interface does not grow, and history rides the **Optional Capability** / **Adapter Access** path instead of widening the core.
- Adapters opt in independently. Slack is the first candidate (clear read API). Linear (ADR 0008) and a future Teams adapter (ADR 0007) implement `HistoryReader` only once their platform read contract is verified; until then, callers get the explicit unsupported result.
- A deliberate divergence from Vercel's stored-history model is documented: this runtime offers a thin live read-through, not end-to-end history persistence or generative-UI replay.
- Risk accepted: a `HistoryReader` that grew runtime caching would quietly become a hidden message store. The decision forbids any runtime storage in `ReadHistory` and tests assert no **Runtime State** writes, keeping the seam thin by contract.

## Alternatives Considered

### Bake message history into the Runtime State contract

Add a transcript/message store to **Runtime State** so handlers can read prior turns from the runtime. Rejected: **Runtime State** is coordination state by definition in `CONTEXT.md`; adding a message store would force every `state/*` implementation and the conformance suite to own a product database schema, exactly the **Thread Application State** the MVP deferred. It also blurs the line between coordination and app data.

### Auto-backfill history into the Message Event during dispatch

Fetch recent **Messages** and attach them to every **Message Event** before calling handlers. Rejected: it puts a platform API read on the inbound **Runtime Dispatch** path, risking the platform ack deadline (Slack 3s, Linear 10s, Teams turn) and retry storms, and forces a cost on handlers that do not want history. Deferred fetching belongs to ADR 0002's **Ack-Then-Work** seam, app-invoked.

### A portable cross-platform history/transcript model

Define a normalized, cursored, multi-adapter transcript API in the core. Rejected as premature: platform read APIs differ enough (threaded vs flat, activity vs comment, cursor styles) that a portable contract is a large, speculative surface. The MVP proves capabilities through narrow interfaces first; a portable model can be a separate slice if real multi-adapter demand appears.

### Leave history fully app-owned with no runtime seam at all

Keep the status quo: apps call the platform API directly through **Adapter Access** with no shared interface. Rejected as the chosen middle ground's weaker sibling: it is undiscoverable and re-implemented per adapter, with no uniform unsupported-capability signal. One narrow `HistoryReader` interface costs little and standardizes the common read without any storage commitment.

### Runtime-owned caching of fetched history

Let the runtime cache `ReadHistory` results keyed by **Thread ID** to cut platform calls. Rejected: a cache is storage, and storage is **Thread Application State**. Runtime caching would re-introduce the very message store this ADR refuses, plus invalidation and tenancy concerns. Apps that want caching own it.

Cross-references: deferred/detached fetching uses ADR 0002 (**Ack-Then-Work**, **Detached Work Context**); read-side rate-limit backoff follows ADR 0005; per-adapter `HistoryReader` implementations relate to ADR 0007 (Teams) and ADR 0008 (Linear expansion); observation of history reads, if surfaced, follows ADR 0010 (**Observation Hook**); resumable streaming stays deferred (not foreclosed) in ADR 0011.
