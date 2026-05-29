# Message History Persistence

Status: needs-triage

## Problem Statement

Handlers in the **Go Chat Runtime** receive only the current **Message Event**. Many real bots want more than the triggering message: prior turns in a **Thread** for LLM context, a backfill of what was said before the bot was mentioned, or a durable transcript keyed by **Thread ID** that survives process restarts.

Vercel's full app/template solves this by owning persistence: it stores chat history and user data in Neon Postgres plus Vercel Blob, and feeds that history back into generation. That is an application and LLM concern, not a conversation-runtime concern. The MVP PRD already deferred this explicitly under "Message history APIs" and "Thread application state APIs", and `CONTEXT.md` defines **Message History** as past platform-fetched messages that stay app-owned, with **Thread Application State** living in the application's own storage keyed by **Thread ID**.

The open question is narrow: should the runtime offer *any* seam for retrieving prior platform messages, or does baking history retrieval into the core invite scope creep into the **Runtime State** contract? Today an app that wants history must call the platform API itself through typed **Adapter Access**, which works but is undiscoverable and re-implemented per adapter.

This PRD answers: keep history app-owned, and offer at most one thin, optional read-through capability so the common "fetch recent platform messages for this **Thread**" case is reachable without widening the core.

## Solution

Affirm platform/stored history as application-owned, and add a single proposed **Optional Capability** for read-through platform backfill: the **History Reader**.

- The runtime defines a narrow `HistoryReader` interface that an adapter *may* implement. It reads recent **Messages** for a **Thread ID** straight from the platform API. It does not store anything.
- **History Reader** is reached only through typed **Adapter Access** (`chat.AdapterAs`), the same path used today for Slack ephemeral delivery and Linear `PostThought`. It is never a method on the core runtime, never a **Routing Hook** input, and never auto-invoked during **Runtime Dispatch**.
- The **Runtime State** contract is unchanged. **Runtime State** stays coordination state (subscriptions, dedupe, **Lock Leases**); it does not gain a message-store table or a transcript API. Implementations (`state/memory`, `state/redis`, `state/postgres`) and their shared conformance suite are untouched.
- Stored conversation context (durable transcripts, RAG corpora, LLM context windows, summaries) stays **Thread Application State** in the app's own database keyed by **Thread ID**. The runtime does not own it.
- Absence of the capability returns an explicit unsupported-capability result, matching the existing **Optional Capability** convention; it never silently degrades to an empty slice that looks like "no history".

Read-through history is opt-in, adapter-scoped, and explicit. Portable conversation semantics are unchanged; this adds a side door, not a new core surface.

Illustrative shape (design only):

```go
// HistoryReader is an Optional Capability. An adapter implements it only when
// the platform exposes a conversation read API. Reached via chat.AdapterAs.
type HistoryReader interface {
    // ReadHistory returns recent platform Messages for an opaque Thread ID,
    // newest-or-oldest order defined per adapter and documented in GoDoc.
    // It is a live platform read: no runtime storage, no dedupe, no caching.
    ReadHistory(ctx context.Context, id chat.ThreadID, opts HistoryQuery) ([]chat.Message, error)
}

type HistoryQuery struct {
    Limit  int    // adapter clamps to the platform's max page size
    Before string // optional cursor (a Message.ID); pagination is adapter-owned
}
```

Usage mirrors ADR-0001's adapter-access example:

```go
hr, ok := chat.AdapterAs[interface{ chat.HistoryReader }](bot, "slack")
if ok {
    msgs, err := hr.ReadHistory(ctx, ev.Thread.ID(), chat.HistoryQuery{Limit: 20})
    // app decides what to persist as Thread Application State
}
```

## User Stories

1. As a Go application developer, I want **Message History** to stay app-owned, so that platform backfill and transcript storage do not complicate the core runtime or **Runtime State** contract.
2. As a bot developer, I want an optional **History Reader** capability keyed by **Thread ID**, so that I can fetch recent platform messages without hand-rolling a per-adapter platform call.
3. As a bot developer, I want **History Reader** reached only through typed **Adapter Access**, so that history is an explicit platform-specific side door, not part of the normalized routing surface.
4. As a bot developer, I want absence of **History Reader** to return an explicit unsupported result, so that "this adapter cannot read history" is distinguishable from "this thread has no prior messages".
5. As an LLM-bot developer, I want stored conversation context to live in my own database as **Thread Application State** keyed by **Thread ID**, so that context windows, summaries, and RAG corpora stay under my control.
6. As a runtime operator, I want **History Reader** to never run during **Runtime Dispatch**, so that fetching backfill cannot block a handler against a platform ack deadline or trigger a platform retry storm.
7. As an adapter author, I want **History Reader** to be a narrow optional Go interface, so that adapters whose platforms lack a read API simply do not implement it.
8. As an adapter author, I want **History Reader** to be a live platform read with no runtime storage, so that the capability cannot quietly grow into a hidden message store inside the adapter or **Runtime State**.
9. As a future maintainer, I want history persistence affirmed as a non-goal with one documented thin seam, so that the gap from Vercel's stored-history model is intentional and discoverable.

## Implementation Decisions

- Affirm the MVP PRD non-goals "Message history APIs" and "Thread application state APIs". Both stay non-goals; this PRD reopens them only to add one optional read-through seam, not storage.
- Define `HistoryReader` in the core `chat` package as an **Optional Capability** interface, alongside the existing capability interfaces (e.g. `EphemeralPoster`). It is not added to the required `Adapter` interface.
- **History Reader** is detected and called exclusively through typed **Adapter Access** (`chat.AdapterAs`). The runtime exposes no top-level `bot.ReadHistory` and no `Thread.History` method.
- `ReadHistory` takes an opaque **Thread ID** and returns normalized `[]chat.Message`, preserving raw platform data per message via the **Platform Escape Hatch** (`Message.Raw`), consistent with inbound normalization.
- **History Reader** performs a live platform API read. It does not write to **Runtime State**, does not dedupe via **Event Identity**, and does not cache. Any caching or persistence is the application's job as **Thread Application State**.
- Pagination, ordering, and page-size clamping are adapter-owned and documented in GoDoc, because platform history APIs differ (Slack `conversations.history`/`replies`, Linear comment/activity reads, Teams Bot Framework transcript reads). The runtime does not impose a portable pagination model.
- Absence of the capability returns the existing explicit unsupported-capability result, never an empty `[]chat.Message`.
- The **Runtime State** interface (`IsThreadSubscribed`, `SubscribeThread`, `UnsubscribeThread`, `MarkEvent`, `AcquireLock`, `ExtendLock`, `ReleaseLock`, `Shutdown`) is unchanged. No state implementation gains a transcript store.
- Adapters MAY implement `HistoryReader` independently. The first candidate is Slack (it has a clear read API); Linear and a future Teams adapter (ADR 0007) implement it only if and when the platform read contract is verified.
- Outbound rate-limit/backoff for a history read lives in the **Platform Adapter** (ADR 0005), bounded by the caller-supplied `context.Context`; **History Reader** does not get a bespoke retry path.
- If an app wants history fetched as part of long handler work, it uses the ADR 0002 **Ack-Then-Work** / **Detached Work Context** seam to run the read after ack; the runtime does not fetch history on the inbound request path.
- README and GoDoc document that stored/long-term conversation context is **Thread Application State** (app-owned), and that **History Reader** is a thin live read-through only.

## Testing Decisions

- Capability-detection tests should cover: adapter implements `HistoryReader` (reachable via **Adapter Access**), adapter does not implement it (explicit unsupported result, not empty slice), and wrong adapter name.
- `ReadHistory` tests should mock the platform API at the HTTP boundary and assert normalized `[]chat.Message`, **Thread ID** to platform-read mapping, raw payload preservation via **Platform Escape Hatch**, and adapter-owned limit clamping.
- Tests should assert **History Reader** performs no **Runtime State** writes: no subscription change, no `MarkEvent` dedupe entry, no **Lock Lease** acquisition.
- Context tests should assert a cancelled `context.Context` aborts the platform read and that backoff stays bounded (ADR 0005), so a history read can never outlive its caller's deadline silently.
- Adapter pagination tests should cover cursor/`Before` handling and page-size clamping per adapter, with golden platform-shaped payloads.
- Negative tests should confirm there is no core `bot.ReadHistory` and no `Thread.History`, so history cannot be reached outside **Adapter Access**.
- Documentation review should confirm GoDoc/README state that transcripts and LLM context are **Thread Application State** and that history retrieval is an **Optional Capability**.

## Out of Scope

- Baking message storage into the **Runtime State** contract or any `state/*` implementation.
- A durable runtime-owned transcript, conversation log, or message table keyed by **Thread ID**.
- Automatic history backfill during **Runtime Dispatch** or as a **Routing Hook** input.
- A portable cross-platform pagination, ordering, or transcript model; pagination is adapter-owned.
- Stored conversation context, summarization, context-window assembly, embeddings, or RAG corpora (these are **Thread Application State** and LLM concerns).
- Vercel-style chat-history + user-data persistence (Neon Postgres / Vercel Blob) and generative-UI history replay.
- Resumable streaming and token-stream replay (deferred, not foreclosed — ADR 0011).
- **Outbound Mutation** of historical messages (edit/delete/react), already deferred in the MVP.
- Requiring any adapter to implement `HistoryReader`; it is strictly optional.

## Further Notes

- This deliberately diverges from Vercel's full app, which owns chat-history persistence end to end. The **Go Chat Runtime** is a conversation-coordination runtime: it routes, dedupes, locks, and posts. Persistence of conversation content is an application concern, consistent with `CONTEXT.md`'s **Message History** and **Thread Application State** definitions.
- The four load-bearing patterns are preserved: single-slot **Routing Hooks** are untouched (history is not a hook), **Thread ID** stays opaque and adapter-produced, the small `Adapter` interface does not grow, and history is reached through the **Optional Capability** / **Adapter Access** path rather than by widening the core.
- Cross-references: deferred/detached fetching uses ADR 0002 (**Ack-Then-Work**, **Detached Work Context**); read-side rate-limit backoff follows ADR 0005; per-adapter implementations relate to ADR 0007 (Teams) and the Linear expansion in ADR 0008; observation of history reads, if surfaced, follows ADR 0010 (**Observation Hook**); resumable streaming stays deferred (not foreclosed) per ADR 0011.
- **History Reader** is the minimum viable seam. If real apps later need cursored, normalized, multi-adapter history badly enough to justify a portable contract, that is a separate slice; this PRD intentionally stops at a thin live read-through.
