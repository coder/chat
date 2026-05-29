# Vercel Chat SDK Gap Analysis — Roadmap / Index

Status: needs-triage — every PRD and ADR below is **Proposed** and awaiting human review/grilling per `docs/agents/grill-with-docs.md` and `docs/agents/domain.md`. No Go code was written in this pass. These are design docs only.

## Two buckets

The Vercel Chat SDK is a full TypeScript chat *application* template. The **Go Chat Runtime** (`github.com/coder/chat`) is a conversation *runtime*: it dedupes, locks, and routes normalized chat **Events** to single-slot **Routing Hooks**, and posts replies through small **Platform Adapters**. Most of what makes the Vercel SDK look big is application surface that a runtime should not own. Split the gap analysis accordingly.

### Bucket 1 — Intentional non-goals (the runtime does not grow into these)

These are app/LLM concerns, not runtime concerns. They stay out of the core; an app composes them on top.

- **Multi-provider LLM routing / model selection** — the runtime posts finished **Postable Messages**; which model produced them is an app concern.
- **Generative UI / JSX-style cards / artifacts** — not a cross-platform card model. Native rich content is reachable only as an **Optional Capability** + **Platform Escape Hatch** (ADR 0004), never baked into **Postable Message**.
- **RAG / retrieval / embeddings** — app/LLM concern; no runtime seam.
- **Resumable streaming** (client resume + server pub/sub + Redis + stream-to-chat persistence) — **deferred, not foreclosed** (ADR 0011). The runtime is discrete-reply today (it posts finished messages, not token streams) and long work uses the ADR 0002 seam; streaming stays revisitable as a future Optional Capability if a real need lands.
- **App-level auth / login orchestration / account linking** — **Application Identity** stays app-owned even under multi-tenant install (ADR 0006).
- **Message-history storage / transcript persistence** — affirmed app-owned (**Thread Application State**); at most a thin optional read-through seam, never storage in the core **Runtime State** contract (ADR 0009).

Making these scope calls *explicitly* is the point — ADR 0009 keeps history app-owned (with a thin optional reader), and ADR 0011 records resumable streaming as a deliberate, revisitable deferral (not foreclosed) rather than an accident of what got built first.

### Bucket 2 — Genuine, priority-aligned work

The real gaps for a Go conversation runtime are production-hardening of the adapter/dispatch seam and one new adapter spike. These are the work items below.

## Sequencing

The order is dependency-driven, not feature-popularity-driven.

1. **Deferred dispatch (ack-then-work)** — ADR 0002, together with the **Concurrency Strategy expansion** it depends on (`queue`/`burst`/`debounce`/`concurrent` + `lockScope` + force/steerability) — ADR 0012. *Do these first.*
2. **Slack hardening** — command events (ADR 0003), interactive components / rich content (ADR 0004), rate-limit handling (ADR 0005), multi-tenant install / OAuth (ADR 0006).
3. **Microsoft Teams adapter spike (design only)** — ADR 0007.
4. **Linear expansion** — agent-session completion (ADR 0008), then generic issue/comment participation (ADR 0013).
5. **Strategic decisions** — message-history persistence (ADR 0009), observability (ADR 0010), resumable-streaming scope decision (ADR 0011).

### Why deferred dispatch is the keystone

Every adapter has a platform ack deadline: Slack 3s, Linear ~10s first thought, Teams turn. Today **Runtime Dispatch** is synchronous, so any long handler work (LLM calls, tool use) races that deadline and the app must detach by hand — which cannot hold or refresh the **Thread Lock**.

ADR 0002 introduces a proposed **Dispatch Mode** in **Runtime Options** (`DispatchSync` default, `DispatchDeferred`), orthogonal to the existing **Concurrency Strategy**, plus a **Detached Work Context** that outlives the dead request context. **Ack-Then-Work** is the shared contract: the adapter acks within the platform deadline, then the handler runs detached with the **Lock Lease** held and refreshed via `ExtendLock`.

This is the primitive behind almost everything else:

- The default `drop` strategy drops same-Thread follow-ups during long deferred work; the `queue` Concurrency Strategy (ADR 0012) is its companion and a functional prerequisite for chatty agents.
- Slash commands and interactions need it for long command/interaction work (ADR 0003, 0004 depend on it).
- Rate-limit backoff that exceeds the ack budget moves work into the **Detached Work Context** instead of sleeping past the deadline (ADR 0005 depends on it).
- Teams proactive posting and Linear's ~30-min follow-up window both ride it (ADR 0007, 0008 depend on it).
- It is the named seam an app uses *instead of* a runtime stream layer (ADR 0011 depends on it).

`DispatchSync` stays the default, so nothing prior is contradicted — deferred dispatch is added beside it.

## Work-item index

Paths are relative to repo root: PRDs at `.scratch/<slug>/PRD.md`, ADRs at `docs/adr/<adr>-<slug>.md`.

| # | Work item | PRD | ADR | Scope (one line) | Status |
|---|-----------|-----|-----|------------------|--------|
| 0002 | Deferred Runtime Dispatch (ack-then-work) | `.scratch/async-dispatch/PRD.md` | `docs/adr/0002-async-dispatch.md` | Proposed **Dispatch Mode** (`DispatchSync`/`DispatchDeferred`) + **Detached Work Context** so handlers ack fast then do long work with the **Lock Lease** held; the shared primitive behind every ack deadline. | Proposed |
| 0003 | Command Events and Slash Command Routing | `.scratch/slash-commands/PRD.md` | `docs/adr/0003-slash-commands.md` | Activate **Command Event** as a non-message **Event** with a single-slot **OnCommand** hook, adapter-owned 3s ack, x-www-form-urlencoded payload decode; long work via Ack-Then-Work. | Proposed |
| 0004 | Interactive Components and Rich Postable Content | `.scratch/interactive-components/PRD.md` | `docs/adr/0004-interactive-components.md` | Proposed **Interaction Event** + **OnInteraction** for Block Kit/card actions, and **Native Content** as an **Optional Capability** beside (not inside) **Postable Message**. | Proposed |
| 0005 | Platform API Rate-Limit Handling | `.scratch/rate-limit-handling/PRD.md` | `docs/adr/0005-rate-limit-handling.md` | Bounded retry/backoff honoring Retry-After lives in the **Platform Adapter**, context-bounded so it never violates ack deadlines; surfaces via **Runtime Observation**. | Proposed |
| 0006 | Multi-Tenant Install and OAuth | `.scratch/multi-tenant-install/PRD.md` | `docs/adr/0006-multi-tenant-install.md` | Proposed **Multi-Tenant Adapter** mode + app-owned **Install Store** contract resolving per-**Platform Tenant** credentials; account linking stays **Application Identity** (app-owned). | Proposed |
| 0007 | Microsoft Teams Adapter (Bot Framework) Approach | `.scratch/teams-adapter/PRD.md` | `docs/adr/0007-teams-adapter.md` | DESIGN ONLY: Bot Framework Activity -> **Event**/**Message**/**Actor**, conversationReference -> opaque **Thread ID**, JWT auth; SDK choice and turn/invoke contract are spike-required. | Proposed |
| 0008 | Linear Agent-Session Completion | `.scratch/linear-full-adapter/PRD.md` | `docs/adr/0008-linear-full-adapter.md` | Complete the app-actor agent surface: full agent activity coverage (**Elicitation**/**Action**/**Error**), response/elicitation/error completion, the **Agent Session Timing Contract**, plans/actions, and a `GraphQL` escape hatch. | Proposed |
| 0009 | Message History Persistence | `.scratch/message-history/PRD.md` | `docs/adr/0009-message-history.md` | Affirm history as app-owned; offer at most a narrow optional **History Reader** read-through capability keyed by **Thread ID**, never in **Runtime State**. | Proposed |
| 0010 | Runtime Observability (Metrics and Tracing) | `.scratch/observability/PRD.md` | `docs/adr/0010-observability.md` | Keep slog **Runtime Observation** as default; add a narrow optional **Observation Hook** (OpenTelemetry-style) for latency, dedupe hits, **Lock Conflict** counts, adapter calls — no hard OTel dependency. | Proposed |
| 0011 | Resumable Streaming Scope (Deferred) | _(none — ADR-only)_ | `docs/adr/0011-resumable-streaming.md` | ADR ONLY: AI-side resumable streaming is **deferred / not yet implemented, not foreclosed**; long generation uses the ADR 0002 **Ack-Then-Work** seam. Revisitable as a future Optional Capability. | Proposed |
| 0012 | Concurrency Strategy Expansion | `.scratch/concurrency-strategy/PRD.md` | `docs/adr/0012-concurrency-strategy.md` | Expand **Concurrency Strategy** to upstream's `queue`/`burst`/`debounce`/`concurrent` + **Lock Scope** + force/steerability; `queue` is the **DispatchDeferred** companion that stops same-Thread follow-ups being dropped. Touches the **Runtime State** conformance suite. | Proposed |
| 0013 | Linear Generic Issue/Comment Participation | `.scratch/linear-generic-comments/PRD.md` | `docs/adr/0013-linear-generic-comments.md` | Add ordinary Linear issue/comment participation as a second interaction model: thread-kind discriminator in the opaque **Thread ID**, `Comment` webhook normalization into **Message Events**, `Thread.Post` routes by kind. Split from 0008. | Proposed |

## Dependency notes

- **0002 unblocks 0003, 0004, 0005, 0007, 0008, 0011** — the ack-then-work primitive is the shared dependency.
- **0002 depends on 0012** — `DispatchDeferred` needs the `queue` Concurrency Strategy so long deferred work does not drop same-Thread follow-ups under the default `drop`. 0012 also extends the **Runtime State** contract, so it ripples through the memory/redis/postgres conformance suite.
- **0003 and 0004 are linked** — commands and interactions are sibling non-message **Event** kinds with sibling single-slot hooks; 0003 depends on 0004's native-response path for command replies.
- **0006 has no dispatch dependency** but 0007 and 0008 depend on it for per-tenant credential resolution.
- **0013 depends on 0008** — generic comment participation builds on the versioned **Thread ID** and agent-session surface 0008 defines.
- **0009, 0010 stand alone**; 0010's **Observation Hook** is where 0005's rate-limit attempts/exhaustion surface.

## Review posture

All of the above is **Proposed** and awaiting human review/grilling. Per `docs/agents/grill-with-docs.md` these docs are written to be challenged against the existing domain model and sharpened before anything is **Accepted**; per `docs/agents/domain.md` every reopened MVP non-goal is surfaced with its source and justified rather than silently overturned. New terms (e.g. **Dispatch Mode**, **Detached Work Context**, **Interaction Event**, **Multi-Tenant Adapter**, **Install Store**, **Observation Hook**, **History Reader**) are marked proposed and are added to `CONTEXT.md` vocabulary only when their ADR is accepted. No Go code was written in this pass.
