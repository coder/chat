# Linear Agent-Session Completion (Full Activity Surface)

Status: needs-triage

## Problem Statement

The Linear adapter today is the **Linear App-Actor Slice** from ADR-0001: it receives `AgentSessionEvent` `created` and `prompted` webhooks, posts a final **Agent Activity Response** through `Thread.Post`, and posts an ephemeral **Agent Activity Thought** through a Linear-specific `PostThought` reached via **Adapter Access**. That slice deliberately stopped there.

A production Linear agent needs more of Linear's agent surface than thought-plus-response. `docs/linear-agent-capabilities.md` tracks the gaps: there is no way to emit the other three of Linear's five agent activity types (`elicitation`, `action`, `error`), no way to end a session with an explicit completion signal (`response` / `elicitation` / `error`), no general agent-activity escape hatch, no preservation of inbound agent-to-human/human-to-agent signal metadata (`auth`, `select`, `stop`), and no enforcement of Linear's **Agent Session Timing Contract** (a first **Agent Activity Thought** within ~10s of session creation, follow-up work allowed up to ~30 minutes).

The 30-minute follow-up window is the deeper problem: Linear's timing contract assumes long-running agent work, but the runtime dispatches synchronously on the inbound request context (ADR-0001), so honoring the contract today means application code hand-rolls detachment. This slice must lean on the runtime's new **Dispatch Mode** / **Detached Work Context** primitive from the deferred-dispatch work (ADR 0002) rather than inventing a Linear-private async path.

This is not a request to widen the portable runtime surface. It is a request to make the Linear adapter a complete app-actor agent client behind the existing small **Adapter** interface, the opaque **Thread ID**, and the **Platform Escape Hatch** / **Optional Capability** seams.

## Solution

Expand the Linear **Platform Adapter** into a complete app-actor agent client, all Linear-specific and reached through **Adapter Access**, leaving the portable runtime surface unchanged.

1. **Full agent activity coverage.** Add the three missing Linear agent activity types as proposed terms: **Agent Activity Elicitation**, **Agent Activity Action**, and **Agent Activity Error**, alongside the existing **Agent Activity Thought** and **Agent Activity Response**. Expose them through a single generic `CreateAgentActivity` escape hatch (content + signal + signal metadata + `ephemeral` flag), plus thin typed convenience helpers (`PostAction`, `PostElicitation`, `PostError`) wrapping it. `Thread.Post` keeps creating a **Response**; `PostThought` keeps creating a thought.

2. **Session-completion signals.** Treat `response`, `elicitation`, and `error` as the three ways a Linear agent session completes, and make that explicit in the adapter so an agent can end a failed session with an **Agent Activity Error** instead of leaving it hanging. This is in scope here.

3. **Agent Session Timing Contract.** Make the ~10s first-thought and ~30-min follow-up window first-class adapter behavior: surface the deadline to handlers, recommend the **Ack-Then-Work** pattern from ADR 0002 for any work that cannot finish inside the first-thought window, and document that posting an **Agent Activity Thought** (or setting `externalUrls` via a session update) is what keeps Linear from marking the session unresponsive.

4. **Agent-session threads stay the only thread kind.** Keep ADR-0001's restriction that rejects non-agent-session Linear **Thread IDs**. `Thread.Post` continues to create an **Agent Activity Response** on the one supported thread kind. Generic issue/comment participation — lifting that restriction and adding a thread-kind discriminator — is a distinct interaction model split into ADR 0013.

Inbound signal preservation rides the **Platform Escape Hatch**: inbound `agentActivity.signal` / `signalMetadata` (including the `stop` signal) and structured session context (`promptContext`, `guidance`, `previousComments`, issue, comment) are preserved in a stable Linear raw-message shape on `Message.Raw`, not lifted into the normalized core.

Multi-tenant OAuth installs stay out: that is the **Multi-Tenant Adapter** + **Install Store** work in ADR 0006, and this slice remains a **Single-Install Adapter**. Token streaming stays out of scope (deferred, ADR 0011). Plan/`externalUrls` session updates and a general `GraphQL` escape hatch are in scope as escape hatches because they are how the timing contract and external-work links are honored.

This PRD follows the proposed ADR: **Linear Agent-Session Completion (Full Activity Surface)** (ADR 0008).

## User Stories

1. As a Linear agent developer, I want to post an **Agent Activity Action**, so that native Linear tool-call progress is visible in the agent session UI.
2. As a Linear agent developer, I want to post an **Agent Activity Elicitation**, so that I can ask the user a question and pause for their answer.
3. As a Linear agent developer, I want to post an **Agent Activity Error**, so that a failed session ends with an explicit completion signal instead of going silent.
4. As a Linear agent developer, I want one generic `CreateAgentActivity` escape hatch, so that I can send any server-validated content, signal, signal metadata, and the `ephemeral` flag before typed helpers exist for every shape.
5. As a Linear agent developer, I want typed `PostAction`, `PostElicitation`, and `PostError` helpers, so that the common activities are ergonomic without dropping to raw maps.
6. As a Linear agent developer, I want the adapter to reject `ephemeral` on activity types Linear forbids it on, so that only `thought` and `action` are sent ephemeral and the rest fail fast.
7. As a Linear agent developer, I want to end a session with `response`, `elicitation`, or `error`, so that session completion is explicit and matches Linear's three completion signals.
8. As a Linear agent developer, I want the adapter to surface the **Agent Session Timing Contract** deadline, so that I know I have ~10s to post a first **Agent Activity Thought** before Linear marks the session unresponsive.
9. As a Linear agent developer, I want to run follow-up work under a **Detached Work Context** via **Ack-Then-Work**, so that work spanning up to Linear's ~30-minute follow-up window outlives the inbound webhook request without a Linear-private async hack.
10. As a Linear agent developer, I want an `auth`-signal **Agent Activity Elicitation** helper or example, so that I can prompt the user to link an external account with `signalMetadata.url`.
11. As a Linear agent developer, I want a `select`-signal **Agent Activity Elicitation** helper or example, so that I can offer the user a choice for an ambiguous decision.
12. As a Linear agent developer, I want inbound `signal` and `signalMetadata` preserved on `Message.Raw`, so that I can detect a human-to-agent `stop` signal and halt work cleanly.
13. As a Linear agent developer, I want structured session context (`promptContext`, `guidance`, `previousComments`, issue, comment) preserved on `Message.Raw`, so that I can build an LLM prompt without re-fetching from Linear.
14. As a Linear agent developer, I want to set `externalUrls` and update the session plan through a session-update escape hatch, so that I can publish a pull-request link and keep a new session from being marked unresponsive.
15. As a Linear agent developer, I want a deliberate `GraphQL` escape hatch that reuses adapter auth, token refresh, base URL, and error handling, so that I can call preview Linear agent APIs before typed wrappers exist without ever seeing the access token.
16. As a Go application developer, I want all of this reached through typed **Adapter Access**, so that the core runtime does not grow a Linear-shaped activity, signal, or plan abstraction prematurely.
17. As a runtime operator, I want the adapter to keep using source-comment-based **Event Identity** for dedupe, so that the expanded shapes do not change the existing Linear dedupe contract.
18. As a runtime operator, I want timing, signal, and activity behavior observable through **Runtime Observation**, so that an unresponsive-marked session or a rejected ephemeral activity is explainable in logs.
19. As a future maintainer, I want the reopened ADR-0001 non-goals (plans/actions and the tracked capability gaps) recorded as deliberate, so that the scope expansion is defensible and the still-deferred items (multi-tenant OAuth, token streaming, generic comment mode) stay clearly out.

## Implementation Decisions

- Keep all new behavior in the existing `adapters/linear` package under adapter name `linear`. No new adapter, no new top-level runtime API.
- Add a generic agent-activity escape hatch reached through **Adapter Access**. Illustrative shape:

  ```go
  type AgentActivityInput struct {
      Content        map[string]any // server-validated content shape: thought/response/action/elicitation/error
      Signal         string         // e.g. "auth", "select" on elicitation
      SignalMetadata any
      Ephemeral      bool           // only valid for thought and action
  }

  func (a *Adapter) CreateAgentActivity(ctx context.Context, threadID chat.ThreadID, in AgentActivityInput) (*chat.SentMessage, error)
  ```

- Layer typed convenience helpers over `CreateAgentActivity` once it exists: `PostAction`, `PostElicitation`, `PostError`. Keep `PostThought` and `Thread.Post`-to-**Agent Activity Response** as the existing entry points. All stay Linear-specific behind **Adapter Access**; do not add a cross-platform typing/streaming/elicitation API to the **Go Chat Runtime**.
- Reject `Ephemeral: true` for any content type other than `thought` and `action`, with an explicit error. Linear only allows those two to be ephemeral.
- Model the three new activity types as proposed domain terms — **Agent Activity Elicitation**, **Agent Activity Action**, **Agent Activity Error** — siblings of the existing **Agent Activity Thought** / **Agent Activity Response**. They are Linear content shapes, not new core **Event** or **Message** kinds.
- Model the **Agent Session Timing Contract** as adapter behavior, not a runtime timer. Surface the first-thought deadline to handlers on the Linear raw-message escape hatch (e.g. session-created timestamp + ~10s budget). The adapter does not itself schedule a watchdog; it documents that the handler must post a first **Agent Activity Thought** (or set `externalUrls`) inside the window, and that anything longer uses **Ack-Then-Work**.
- For follow-up work that exceeds the first-thought window, use the runtime **Dispatch Mode** = `DispatchDeferred` and the **Detached Work Context** from ADR 0002. Under deferred dispatch the **Thread Lock** / **Lock Lease** is held across the detached work and refreshed via `ExtendLock`; the 2m `ThreadLockTTL` is a lease, not a hard deadline. This adapter does not define those primitives; it consumes them. Do not reintroduce a Linear-private goroutine path.
- Preserve inbound `agentActivity.signal`, `signalMetadata` (including human-to-agent `stop`), and structured session context (`promptContext`, `guidance`, `previousComments`, `agentSession.issue`, `agentSession.comment`) in a stable Linear raw-message struct exposed through `Message.Raw` as a **Platform Escape Hatch**. Do not lift these into the normalized **Message** / **Event** fields; document the accessor as Linear-specific.
- Add a session-update escape hatch for `agentSessionUpdate`: set/replace `externalUrls`, add/remove external URLs, and replace the full plan array. Document that plan updates replace the whole array and that setting `externalUrls` can keep a new session from being marked unresponsive. Illustrative shape:

  ```go
  func (a *Adapter) UpdateSession(ctx context.Context, threadID chat.ThreadID, in AgentSessionUpdateInput) error
  ```

- Add a deliberate low-level `GraphQL` escape hatch that reuses the adapter's **App-Actor Client Credentials** token refresh path, API base URL, HTTP client, and GraphQL error handling. It must surface GraphQL `errors` clearly and must never expose or return the access token. Document it as a Linear-specific escape hatch, not a cross-platform API. Illustrative shape:

  ```go
  func (a *Adapter) GraphQL(ctx context.Context, query string, variables any, dest any) error
  ```

- Keep ADR-0001's restriction that rejects non-agent-session Linear **Thread IDs**; agent-session threads stay the only Linear thread kind here. `Thread.Post` continues to create an **Agent Activity Response** on that one supported kind. Do not add a thread-kind discriminator to the opaque **Thread ID** or accept generic Linear `Comment` webhooks; that interaction model is split to ADR 0013. **Plain Text** and **Portable Markdown** still pass through unchanged; do not add a Markdown conversion layer (that remains a separately tracked gap).
- Keep the adapter a **Single-Install Adapter** using **App-Actor Client Credentials**. Multi-tenant OAuth installs, an **Install Store**, and per-**Platform Tenant** credential lookup are ADR 0006, not here. Reuse the **Platform Tenant** context already in the opaque **Thread ID** and **Actor**.
- Keep direct HTTP/GraphQL calls and local **Supported Platform Shape** structs; do not introduce a Linear SDK dependency. Keep low-level calls private behind the adapter's existing deep modules (token/auth, Linear API, webhook verify/normalize, thread-ID codec); the new `GraphQL` and `CreateAgentActivity` methods are the only widened public surface, both narrow and both behind **Adapter Access**.
- Surface timing, signal, ephemeral-rejection, completion-signal, and rate-limited Linear API behavior through **Runtime Observation**, and through the optional **Observation Hook** surface (ADR 0010) where dispatch latency and adapter API calls are already reported. Outbound Linear rate-limit retry/backoff lives in the adapter and is bounded (ADR 0005).
- Update README and GoDoc to record the expanded Linear surface and to mark which ADR-0001 deferrals are now closed vs still deferred.

## Testing Decisions

- Verify external behavior and public contracts, not private implementation details, consistent with the existing Linear and Slack adapter tests.
- Use a fake Linear GraphQL endpoint (as the slice does) to verify `CreateAgentActivity` sends each of the five content types with the expected payload, that signal and signal metadata pass through, and that the returned **Sent Message** carries the created activity identity.
- Assert `Ephemeral: true` is accepted for `thought` and `action` and rejected with an explicit error for `response`, `elicitation`, and `error`.
- Verify typed `PostAction`, `PostElicitation`, and `PostError` produce the same payloads as the equivalent `CreateAgentActivity` calls.
- Verify session completion: an **Agent Activity Error** ends a session, and the adapter does not also post a stray **Agent Activity Response**.
- Verify inbound signal preservation: a `prompted` event carrying a `stop` signal exposes that signal and metadata on `Message.Raw`, and structured session context fields round-trip through the raw escape hatch.
- Verify the timing contract surface: the first-thought deadline (session-created timestamp + budget) is present on the Linear raw-message shape, and the documented **Ack-Then-Work** path posts a first thought before doing detached work. Do not assert wall-clock timing against Linear; assert the surfaced budget and the ordering (thought before long work).
- Verify deferred-dispatch interaction at the seam owned by ADR 0002: under `DispatchDeferred` the **Thread Lock** is held across detached follow-up work and the dead request context is not used for the later **Agent Activity Response**. Keep this test focused on the Linear adapter's use of the primitive, not on re-testing the primitive.
- Verify `UpdateSession` sets/replaces `externalUrls`, add/remove behavior, and full-plan replacement against the fake GraphQL endpoint.
- Verify the `GraphQL` escape hatch reuses the client-credentials token (refresh path exercised), surfaces GraphQL `errors`, and never returns the access token.
- Verify the **Thread ID** still rejects non-agent-session Linear thread IDs: an agent-session **Thread ID** posts an **Agent Activity Response**; agent-session identity round-trips through encode/decode; malformed and wrong-adapter IDs are rejected; **Thread Handle** reconstruction works.
- Verify **Plain Text** and **Portable Markdown** still pass through unchanged for agent-activity responses.
- Verify bounded outbound rate-limit retry honors Linear `Retry-After` and surfaces attempts/exhaustion through **Runtime Observation** without violating the first-thought window.
- Extend the memory-backed `examples/linear-agent-hello-world` (or add a sibling example) to demonstrate an action, an elicitation with a `select` signal, an error completion, an `externalUrls` session update, and **Ack-Then-Work** follow-up. Keep examples toy-sized.
- Run the existing root, adapter, and example test commands. If Docker-dependent Redis/Postgres tests are unavailable, report that as a validation limitation rather than claiming full validation.
- Live dogfooding against Linear is required before claiming end-to-end behavior: evidence should show a first thought inside the timing window, an action, an elicitation, an error completion, an `externalUrls` link, and a follow-up prompt continuing under deferred dispatch.

## Out of Scope

- Multi-tenant OAuth installation flow, **Install Store**, and per-**Platform Tenant** credential lookup. That is the **Multi-Tenant Adapter** in ADR 0006. This slice stays a **Single-Install Adapter** on **App-Actor Client Credentials**.
- Generic issue/comment participation: accepting generic Linear `Comment` webhooks, a thread-kind discriminator on the opaque **Thread ID**, and `Thread.Post` creating ordinary issue comments. That is a distinct interaction model split to ADR 0013; this slice keeps agent-session threads as the only Linear thread kind.
- AI-side resumable streaming and token-stream output. The runtime posts finished messages, not streams; token streaming stays out of scope (deferred, ADR 0011). Long Linear generation uses **Ack-Then-Work** + **Detached Work Context**, not a stream layer.
- Defining the **Dispatch Mode**, **Detached Work Context**, **Ack-Then-Work**, or **Lock Lease** extension semantics. Those are owned by ADR 0002; this slice only consumes them.
- A cross-platform card / interactive-component model. Linear `select` / `auth` elicitations are sent as native Linear activity signals through the escape hatch, not as a portable card; portable Block Kit/Adaptive Card interaction is the separate **Interaction Event** / **Native Content** work (ADR 0003, ADR 0004).
- A cross-platform typing, streaming, plan, or elicitation abstraction in the core runtime. These stay Linear-specific behind **Adapter Access** until another adapter needs the same shape.
- Rich Linear Markdown conversion. **Plain Text** and **Portable Markdown** still pass through unchanged; the upstream Linear Markdown converter remains a separately tracked gap, not part of this slice.
- Linear reactions, edit/delete and other **Outbound Mutation**, files/attachments, repository-suggestion ranking, and issue-workflow state automation (these stay tracked in `docs/linear-agent-capabilities.md`; the `GraphQL` escape hatch makes them possible without first-class typed helpers).
- A raw public Linear API client. The widened public surface is limited to the narrow `CreateAgentActivity`, `UpdateSession`, typed activity helpers, and the deliberate `GraphQL` escape hatch.
- A Linear SDK dependency.
- Changing the portable **Postable Message** surface. **Plain Text** and **Portable Markdown** stay the only portable bodies.

## Further Notes

This slice reopens the ADR-0001 deferral of **plans and actions** from the "separately designed slices" list, plus the tracked gaps in `docs/linear-agent-capabilities.md` that block a complete agent session (generic agent activity creation, signals, session updates, GraphQL escape hatch, timing). The justification is recorded in ADR 0008. It explicitly does not reopen multi-tenant OAuth (ADR 0006) or token streaming (ADR 0011), and it does not reopen reactions, history, or Markdown conversion, which stay tracked deferrals. Generic issue/comment participation — lifting the agent-session-only **Thread ID** restriction — is split to its own slice, ADR 0013, keeping this slice faithful to ADR-0001's app-actor-through-agent-sessions identity.

The load-bearing patterns are preserved: single-slot **Routing Hooks** (`OnNewMention` / `OnSubscribedMessage`) still route Linear input by subscription state; the opaque adapter-produced **Thread ID** is unchanged and still addresses only agent-session threads; the small **Adapter** interface is unchanged; and every new Linear behavior arrives through the **Platform Escape Hatch** / **Optional Capability** path via **Adapter Access**, not by widening the core.

The deepest seams to build are the generic agent-activity codec (the five content shapes plus signal/ephemeral validation), the inbound signal/context raw-message struct, and the `GraphQL` escape hatch. Each encapsulates real behavior behind a narrow surface and is testable with fake HTTP/GraphQL servers, matching the slice's existing deep modules. Typed convenience helpers should land only after the generic escape hatch is dogfooded, per the implementation order tracked in `docs/linear-agent-capabilities.md`.
