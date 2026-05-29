# ADR 0011: Resumable Streaming Scope (Deferred, Not Foreclosed)

## Status

Proposed

## Context

The **Go Chat Runtime** posts finished messages. A handler computes a reply and calls `Thread.Post(ctx, chat.Text(...))` or `chat.Markdown(...)`, which an adapter renders to one platform post. There is no token stream, no partial-message surface, and no client-resume protocol. **Runtime State** is coordination state (subscriptions, dedupe, **Lock Leases**, runtime cache), not a transcript or a stream buffer.

The Vercel Chat SDK app/template ships resumable streaming, and a reader coming from it may expect the Go runtime to match. Vercel's resumable streaming is two-sided:

- client `useChat` resumes an in-progress stream on mount/reconnect;
- a server `resumable-stream` pub/sub layer fans tokens to subscribers;
- Redis backs the live stream;
- the stream is persisted into chat history so a late or reconnecting client replays it.

That is an AI-output transport bound to a browser chat UI and an LLM token source. This runtime has neither. It normalizes inbound platform **Events**, routes them to a single-slot **Routing Hook**, and renders one outbound **Postable Message** per reply onto Slack, Linear, or (future) Teams. None of those platforms is a token-stream sink the way a React `useChat` hook is; they are message APIs that take a finished post. The runtime is a discrete-reply chat/agent runtime today.

This ADR records the scope posture deliberately, because "AI streaming responses: Not yet implemented" in the README is ambiguous between "coming soon," "never," and "not now." The resolution below is the middle one: **deferred and not yet implemented, with the seam for long work named — and explicitly not foreclosed.**

This ADR touches one documented non-goal, surfaced explicitly per `docs/agents/domain.md`:

- **ADR-0001 alternative "Add generic runtime typing/streaming APIs now"** (Rejected as premature): rejected because "a cross-platform typing or streaming abstraction can be designed after more adapters need it." That deliberately left the door open. Slack, Linear, and the planned Teams adapter (ADR 0007) now give a cross-platform vantage, and across them there is still no shared token-stream sink to abstract — so this ADR continues to defer rather than build, **without closing the door**. (Linear's **Agent Activity Thought** working-signal stays exactly as ADR-0001 shipped it: an ephemeral, adapter-specific status post reached through **Adapter Access**, not a token stream.)

This does not contradict ADR-0001. ADR-0001 deferred *designing* a streaming abstraction; this ADR keeps that deferral explicit and names the seam apps use for long work in the meantime.

Related ADRs (decisions not redefined here): deferred dispatch is **ADR 0002** (proposed **Dispatch Mode** / **Ack-Then-Work** / **Detached Work Context**); this ADR points at it as the seam for long generation and does not restate its contract. ADR 0002's "Add a runtime AI-stream layer for long generation" alternative defers to this ADR; the two are reciprocal. Linear's **Agent Session Timing Contract** (ADR 0008) and **Agent Activity Thought** are progress signals, not streaming, and are unaffected. The **Observation Hook** surface (ADR 0010) is for runtime metrics, not stream transport.

## Decision

Defer AI-side resumable streaming: the **Go Chat Runtime** does not implement it now, and this ADR does **not** foreclose it as a permanent non-goal. The runtime posts finished **Postable Messages**; client resume, server pub/sub, Redis stream persistence, and stream-to-chat persistence are app/LLM concerns today.

Concretely, the runtime does NOT add **in the current scope**:

- a streaming or partial-message variant of `Thread.Post` (no `Thread.Stream`, no chunk writer, no `io.Writer` reply surface);
- a server-side token pub/sub layer or a stream buffer in **Runtime State** (it stays coordination state per the MVP PRD; a stream transcript is **Thread Application State**, app-owned);
- a client-facing resume protocol or a runtime-owned HTTP endpoint clients poll/reconnect to (the runtime exposes adapter **Webhook Handlers** only, per the MVP PRD; it owns no client-facing read API);
- a cross-platform "typing"/streaming abstraction (the door ADR-0001 left open stays open, not walked through yet).

The seam an app uses instead, for long generation, is **ADR 0002**. An app needing minutes of LLM/tool work selects `DispatchDeferred` and runs the handler under the **Detached Work Context** (**Ack-Then-Work**): the adapter acknowledges within the platform deadline (Slack 3s, Linear ~10s, Teams turn), then the handler does the long work and posts one finished **Postable Message** when done. The **Thread Lock** is held and `ExtendLock`-refreshed across that work, so concurrency stays correct. This is discrete-reply, not streaming: the user sees one final post, optionally preceded by a platform-native progress signal the adapter already owns (Linear's **Agent Activity Thought**; a Slack interim post is just another **Postable Message**).

**The door stays open, by design.** If a real need appears — a platform that is genuinely a token-stream sink, or a confirmed cross-platform streaming demand — streaming would be added as a future **Optional Capability** on adapters that natively stream (the native stream payload carried as a **Platform Escape Hatch**), plus app-owned transport. It would **not** change `Thread.Post`, **Runtime State**, or the single-slot **Routing Hook** shape. Because the reversal cost is contained to a new optional interface, deferring now paints the core into no corner, and this decision is intentionally revisitable rather than final.

The load-bearing patterns are the reason it stays out *for now*: single-slot **Routing Hooks** (one reply per route), the opaque adapter-produced **Thread ID**, the small **Adapter** interface (which renders a finished post), and the **Platform Escape Hatch** / **Optional Capability** path for anything platform-specific. A stream layer would widen all four, so it is not added speculatively — but it is not ruled out either.

## Consequences

- The posting contract stays narrow today: one finished **Postable Message** per reply, **Plain Text** + **Portable Markdown**, rendered by the small **Adapter** interface. No partial-post state machine, chunk ordering, or resume cursor enters the core now.
- **Runtime State** stays coordination state. No stream buffer, transcript, or Redis stream key joins the contract that **Memory State**, **Redis State**, and **Postgres State** must satisfy, so the conformance suite is unchanged.
- Long generation has a sanctioned home: **ADR 0002** `DispatchDeferred`. Apps do not hand-roll a goroutine that blows the platform ack deadline, and the runtime does not grow a parallel stream path to solve a problem deferred dispatch already solves.
- The runtime is not, today, a client-facing read service. It ingests adapter **Webhook Handlers** and posts replies; it owns no endpoint a browser reconnects to, so it stays mountable in any `net/http` server.
- The Vercel gap is documented honestly: "AI streaming responses" is **deferred / not yet implemented**, with the ADR 0002 seam for long work — explicitly *not* a closed non-goal. A Vercel reader is told it is not here now and not currently planned, but the design is revisitable if a real need lands.
- Reversal cost is low and contained: streaming, if wanted, is a new **Optional Capability** on natively-streaming adapters plus an app-owned transport, not a change to `Thread.Post`, **Runtime State**, or the **Routing Hook** shape. This is exactly why the door is kept open rather than nailed shut.

## Alternatives Considered

### Affirm resumable streaming as a permanent, hard non-goal

Rejected. Declaring streaming a permanent non-goal would foreclose a design ADR-0001 deliberately left open "after more adapters need it." The current multi-adapter vantage shows no shared token-stream sink *yet*, but absence of a need today is not evidence it will never arise, and the reversal cost is low (a future **Optional Capability**). Recording it as a deferral keeps the decision honest and revisitable rather than overclaiming finality.

### Add a streaming `Thread.Post` variant (chunk writer / `io.Writer` reply) now

Rejected for now. It turns the single finished-message posting contract into a partial-post state machine (chunk ordering, flush cadence, mid-stream failure, edit-vs-append) for no platform that needs it today: Slack, Linear, and Teams take finished posts, and Linear's working signal is already the ephemeral **Agent Activity Thought**. Long generation is a *dispatch-timing* problem solved by ADR 0002, not an output-shape problem. Revisit only if a platform becomes a real stream sink.

### Build a server-side stream pub/sub backed by Redis, Vercel-style

Rejected for now. That is an AI-output transport for a browser `useChat` client this runtime does not have. It would put a token stream and a stream buffer into **Runtime State**, which is coordination state by decision (MVP PRD), and make the runtime own a client-facing read/reconnect endpoint, which it does not. A stream transcript is **Thread Application State**, app-owned.

### Persist a live stream to chat history for late/reconnecting clients

Rejected for now. **Message History** is app-owned (ADR 0009 offers at most a thin optional **History Reader** read-through, never a runtime-owned store). Stream-to-history persistence would bake exactly the storage that decision keeps out of the runtime. An app that wants a replayable transcript owns it as **Thread Application State**.

### Design the cross-platform typing/streaming abstraction now

Rejected for now, with the multi-adapter vantage ADR-0001 said to wait for. Across Slack, Linear, and Teams there is no shared token-stream sink to abstract; the only shared "progress" concept is a platform-native interim post, which each adapter already expresses as an ordinary **Postable Message** (or, for Linear, the existing **Agent Activity Thought** escape hatch). There is nothing portable to generalize yet — so the abstraction is deferred, not declared impossible.
