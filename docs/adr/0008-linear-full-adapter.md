# ADR 0008: Linear Agent-Session Completion (Full Activity Surface)

## Status

Accepted

## Context

ADR-0001 built a deliberately narrow **Linear App-Actor Slice**: receive `AgentSessionEvent` `created` and `prompted` webhooks, post a final **Agent Activity Response** through `Thread.Post`, and post an ephemeral **Agent Activity Thought** through a Linear-specific `PostThought` reached via **Adapter Access**. It rejected non-agent-session Linear **Thread IDs** and left the rest of Linear's agent surface for "separately designed slices."

`docs/linear-agent-capabilities.md` tracks what a production Linear agent still needs and the slice does not provide:

- Linear defines five agent activity content types — `thought`, `response`, `action`, `elicitation`, `error` — and the slice only emits two.
- A Linear agent session completes via one of three signals: `response`, `elicitation`, or `error`. The slice can complete only via `response`; a failed session cannot be ended cleanly.
- There is no general agent-activity escape hatch, no preservation of inbound agent-to-human / human-to-agent signals (`auth`, `select`, `stop`), no session-update path for `externalUrls` / plan, and no deliberate `GraphQL` escape hatch for preview APIs.
- Linear's timing expectations are unmet: an agent should emit a first **Agent Activity Thought** within ~10s of session creation or be marked unresponsive, and follow-up work is allowed up to ~30 minutes. The slice dispatches synchronously on the inbound request context (ADR-0001), so honoring the ~30-minute window today means application code hand-rolls detachment.

This ADR expands the Linear adapter to a complete app-actor agent client, behind the existing seams. It reopens these documented non-goals, which this ADR justifies rather than silently overriding (per `docs/agents/domain.md`):

- **ADR-0001 deferred list** — "generic comment mode, multi-tenant OAuth installs, streaming/plans/actions, reactions, history, and Markdown conversion as separately designed slices." This ADR reopens **plans and actions** only (token streaming stays out of scope, deferred — ADR 0011). **Generic comment mode** is split into its own slice (ADR 0013), keeping this ADR faithful to ADR-0001's app-actor-through-agent-sessions identity. Multi-tenant OAuth, reactions, history, and Markdown conversion are not reopened.
- **`docs/linear-agent-capabilities.md` tracked gaps** — this ADR reopens the gaps that block a complete agent session: generic agent activity creation (gap 2), typed activity helpers (gap 3), agent-to-human and human-to-agent signals (gaps 4, 5, 13, 14), session updates including `externalUrls` and plans (gaps 6, 15), the general `GraphQL` escape hatch (gap 1), and structured prompt context preservation (gap 9). Generic issue/comment participation — lifting the agent-session-only **Thread ID** restriction — is split to ADR 0013. Repository suggestions (gap 8), issue-workflow automation (gap 11), and conversation-history-through-activities (gap 10) remain tracked deferrals reachable through the `GraphQL` escape hatch but are not given typed helpers here.

The ~10s / ~30-min timing contract is the cross-cutting reason this cannot stay app-private: it needs the runtime's deferred-dispatch primitive (ADR 0002), not a Linear-only goroutine.

## Decision

Expand `adapters/linear` past the app-actor slice, keeping all new behavior Linear-specific and reached through typed **Adapter Access**, leaving the portable runtime surface unchanged.

1. **Full agent activity coverage.** Add proposed terms **Agent Activity Elicitation**, **Agent Activity Action**, and **Agent Activity Error** alongside the existing **Agent Activity Thought** and **Agent Activity Response**. Expose a generic `CreateAgentActivity(ctx, threadID, AgentActivityInput)` escape hatch carrying content, `signal`, `signalMetadata`, and an `ephemeral` flag, plus thin typed helpers `PostAction` / `PostElicitation` / `PostError` layered over it. `Thread.Post` still creates a **Response**; `PostThought` still creates a thought. Reject `ephemeral` on any type other than `thought` and `action`, matching Linear's rule.

2. **Session-completion signals.** Treat `response`, `elicitation`, and `error` as the three completion signals and make ending a failed session via **Agent Activity Error** a first-class adapter capability. In scope here.

3. **Agent Session Timing Contract.** Add a proposed **Agent Session Timing Contract** (~10s first **Agent Activity Thought**, ~30-min follow-up window). The adapter surfaces the first-thought deadline (session-created timestamp + budget) on the Linear raw-message **Platform Escape Hatch** and documents that posting a first thought — or setting `externalUrls` via a session update — is what keeps Linear from marking the session unresponsive. Work that cannot finish inside the first-thought window uses **Ack-Then-Work** with **Dispatch Mode** = `DispatchDeferred` and the **Detached Work Context** from ADR 0002; the **Thread Lock** / **Lock Lease** is held across detached work and refreshed via `ExtendLock`. The adapter consumes that primitive; it does not define a Linear-private async path and does not run its own watchdog.

4. **Agent-session threads stay the only Linear thread kind here.** This ADR keeps ADR-0001's restriction that non-agent-session Linear **Thread IDs** are rejected; `Thread.Post` continues to create an **Agent Activity Response** on the one supported thread kind. Generic Linear `Comment` webhooks, the **Thread ID** thread-kind discriminator, and ordinary issue-comment posting are a distinct interaction model split into ADR 0013, so this ADR stays a completion of the app-actor agent surface rather than a second participation mode.

Supporting escape hatches, all behind **Adapter Access**:

- A session-update escape hatch (`UpdateSession`) for `agentSessionUpdate`: set/replace `externalUrls`, add/remove URLs, replace the full plan array (plan updates replace the whole array; documented).
- Inbound `signal` / `signalMetadata` (including the human-to-agent `stop` signal) and structured session context (`promptContext`, `guidance`, `previousComments`, issue, comment) preserved on `Message.Raw` as a stable Linear raw-message shape — not lifted into normalized **Message** / **Event** fields.
- A deliberate low-level `GraphQL(ctx, query, variables, dest)` method reusing the **App-Actor Client Credentials** token-refresh path, base URL, HTTP client, and GraphQL error handling; it surfaces GraphQL `errors` and never exposes or returns the access token.

Constraints held constant:

- The adapter stays a **Single-Install Adapter** on **App-Actor Client Credentials**. Multi-tenant OAuth, an **Install Store**, and per-**Platform Tenant** credential lookup are ADR 0006, not here. Reuse the **Platform Tenant** scoping already in the opaque **Thread ID** and **Actor**.
- **Plain Text** and **Portable Markdown** stay the only portable bodies and still pass through unchanged; no Markdown conversion layer is added.
- Direct HTTP/GraphQL and local **Supported Platform Shape** structs; no Linear SDK dependency. Low-level calls stay private behind the slice's existing deep modules; the only widened public surface is the narrow `CreateAgentActivity`, `UpdateSession`, typed activity helpers, and `GraphQL`, all behind **Adapter Access**.
- Outbound Linear rate-limit retry/backoff lives in the adapter, is bounded, honors `Retry-After`, must not violate the first-thought window, and is reported through **Runtime Observation** and the optional **Observation Hook** surface (ADR 0005, ADR 0010).

## Consequences

The Linear adapter becomes a complete app-actor agent client: it can think, respond, act, elicit, and error; it can update session plans and external URLs; and it can detect a `stop` signal and end cleanly. Application authors keep the normal runtime flow for responses and reach the rest through typed **Adapter Access**:

```go
linearAdapter, ok := chat.AdapterAs[*linear.Adapter](bot, "linear")
if ok {
    _, _ = linearAdapter.PostElicitation(ctx, ev.Thread.ID(), elicitation)
}
```

Honoring the ~30-minute follow-up window makes this slice the first real consumer of the ADR 0002 deferred-dispatch primitive, so this ADR depends on **Dispatch Mode** / **Ack-Then-Work** / **Detached Work Context** landing first; until then, follow-up work past the first-thought window still requires app-code detachment as in ADR-0001.

The opaque **Thread ID** is unchanged in this ADR: agent-session threads remain the only kind. The thread-kind discriminator that generic comment participation needs is deferred with it to ADR 0013.

The public Linear surface grows by four narrow methods plus typed helpers. This is more platform-specific surface than the slice, but it stays Linear-specific behind **Adapter Access**; the **Go Chat Runtime** does not gain a typing/streaming/plan/elicitation abstraction, and **Postable Message** is untouched. Several tracked gaps (repository suggestions, issue-workflow automation, activity history) remain unbuilt but are now reachable through the `GraphQL` escape hatch.

Divergences from the upstream Chat SDK and from ADR-0001 are deliberate:

- generic comment mode stays deferred (split to ADR 0013), preserving ADR-0001's app-actor-through-agent-sessions identity;
- plans, actions, elicitation, and error activities are now in scope (ADR-0001 deferred them);
- token streaming stays out of scope (deferred, not foreclosed — ADR 0011), unlike the upstream's richer streaming;
- Markdown conversion, reactions, and history stay deferred;
- the timing contract is honored through the shared deferred-dispatch primitive, not a Linear-private async path.

## Alternatives Considered

### Keep the app-actor slice and push everything else into application code via a single GraphQL escape hatch

Rejected. A bare `GraphQL` method does make every Linear operation reachable, and it is part of this decision — but leaving session completion (`error`), the activity-type/ephemeral validation rules, and the timing contract entirely to application code would push Linear's correctness rules (only `thought`/`action` may be ephemeral; a session completes via `response`/`elicitation`/`error`; a first thought is due in ~10s) into every app. The adapter owns the platform contract; those rules belong in it.

### Add a cross-platform typing / streaming / elicitation / plan abstraction to the runtime now

Rejected as premature, consistent with ADR-0001's rejection of generic runtime typing/streaming APIs. Only Linear needs these shapes today. Elicitation `select` / `auth` and plans are modeled as native Linear activity signals behind **Adapter Access**; a portable abstraction can be designed if a second adapter needs the same shape. Portable interactive components are the separate **Interaction Event** / **Native Content** work (ADR 0003, ADR 0004), not this.

### Solve the ~30-minute follow-up window with a Linear-private goroutine and background context

Rejected. Spawning detached work on `context.Background()` inside the adapter would duplicate — and diverge from — the **Detached Work Context** and **Lock Lease** semantics being designed in ADR 0002, and would re-create the exact app-code hack ADR-0001 told authors to write. The timing contract must ride the shared **Dispatch Mode** / **Ack-Then-Work** primitive so lock holding, lease extension, and state mutation under the detached context behave identically across adapters.

### Reopen multi-tenant OAuth installs here too, since this is "the full Linear adapter"

Rejected. Multi-tenant install is a distinct concern with its own **Multi-Tenant Adapter** mode and app-owned **Install Store** contract (ADR 0006), and **Application Identity** / account linking stays app-owned regardless. Bundling it would couple two large designs; this slice stays a **Single-Install Adapter** and reuses the **Platform Tenant** scoping already baked into **Thread ID** and **Actor**.

### Add rich Linear Markdown conversion as part of "full adapter"

Rejected for this slice. The portable surface is still **Plain Text** + **Portable Markdown**, and there is no richer formatted content in the runtime that needs a CommonMark-to-Linear converter yet. It stays a tracked deferral, consistent with ADR-0001's reasoning, so the expansion does not silently widen the posting contract.

### Promote inbound signals and structured session context into normalized Message / Event fields

Rejected. `stop` signals, `auth` / `select` metadata, `promptContext`, `guidance`, and `previousComments` are Linear-specific and have no cross-platform meaning yet. Promoting them would widen the normalized core for one platform; they belong on `Message.Raw` as a stable **Platform Escape Hatch** with a documented Linear-specific accessor, matching the runtime's escape-hatch-over-core-widening pattern.
