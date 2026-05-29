# ADR 0004: Interactive Components and Rich Postable Content

## Status

Proposed

## Context

The **Go Chat Runtime** routes inbound **Messages** and posts **Plain Text** + **Portable Markdown** replies. Two linked gaps block real interactive bots.

**Inbound interactions.** A Slack Block Kit button click, menu selection, or modal submission arrives as an x-www-form-urlencoded interactivity payload with a `type`; a Teams card action arrives as a Bot Framework invoke Activity. CONTEXT.md already says "a button click is an **Event**, not a **Message**," and already reserves **Command Event** for slash commands. But the runtime has no **Event** kind or **Routing Hook** for component actions, and these actions carry platform ack contracts: Slack expects a 2xx within 3 seconds (optional `response_url` / `trigger_id` follow-up); Teams expects an invoke response on the turn. Without a normalized path, apps must parse raw payloads and bypass dedupe, **Thread Lock**, and subscription guarantees to handle a button.

**Outbound native content.** Block Kit and Adaptive Cards render the approval prompts, pickers, and buttons that produce those inbound actions. A bot that cannot emit a button cannot receive a button click. The MVP deliberately excluded rich content to prove the portable surface.

This ADR reopens two documented MVP non-goals, narrowly and with justification (per `docs/agents/domain.md`):

- **MVP PRD Out of Scope: "Rich cards, JSX-style cards, modals, files, platform-native payload builders, and cross-platform rich formatting."** Reopened only for native (Block Kit / Adaptive Card) payloads behind an **Optional Capability**; JSX-style cards, files, native payload builders, and cross-platform rich formatting stay out of scope. Source: `.scratch/go-chat-runtime-mvp/PRD.md`.
- **MVP PRD Out of Scope: "Edit, delete, reactions, and other Outbound Mutation operations."** Reopened only for the message-update a platform interaction response needs (replacing a card after a click); general edit/delete/reactions stay deferred. Source: `.scratch/go-chat-runtime-mvp/PRD.md`.

This ADR owns only the two non-goals above (native rich content, narrow **Outbound Mutation**). The **Command Event** / `OnCommand` surface, and the reopening of the MVP non-goal "Slash commands and other Command Events", are owned and justified in ADR 0003 (slash-commands); this ADR adds only the sibling **Interaction Event** / `OnInteraction`. The inbound-interaction reversal of the README "Intentional MVP Gaps" line ("no Slack interactions, buttons, shortcuts, or Block Kit workflow") and the MVP PRD "modals" exclusion is surfaced and justified in the PRD's reopened-non-goals section.

The deferred-dispatch primitive needed for long interaction/command work — **Dispatch Mode**, **Ack-Then-Work**, **Detached Work Context** — is owned by ADR 0002 (async-dispatch) and is referenced, not redefined, here. Teams normalization and auth are ADR 0007 and spike-required. Multi-tenant install is ADR 0006. Observability metric hooks are ADR 0010.

## Decision

Add inbound interactivity and outbound native content as extensions that preserve the four load-bearing patterns: single-slot **Routing Hooks**, opaque adapter-produced **Thread ID**, the small **Adapter** interface, and **Platform Escape Hatch** / **Optional Capability** over core widening.

Inbound:

- Add a proposed **Interaction Event** as a new non-message **Event** kind, sibling to the existing **Command Event**. Both are **Events**, not **Messages**. Only **Interaction Event** is net-new here; **Command Event** and its `OnCommand` hook are owned and justified by ADR 0003 (slash-commands), referenced not redefined.
- Route the **Interaction Event** through its own single-slot **Routing Hook**: proposed `OnInteraction`, atomic-replace exactly like `OnNewMention` / `OnSubscribedMessage` (and like `OnCommand` from ADR 0003). This is the intentional Vercel divergence (single handler, not multi-handler registration); GoDoc must restate it.
- The **Interaction Event** carries the normalized **Thread**, **Actor**, and a **Platform Escape Hatch** `Raw` (which holds `response_url`, `trigger_id`, view state, action values, Teams invoke context), plus a normalized action identity. This mirrors the **Command Event** shape ADR 0003 defines for the sibling hook.
- Reuse the existing **Event** envelope so interactions and commands inherit **Event Identity** dedupe, **Thread Lock** serialization, and **Platform Tenant**-scoped **Thread ID** / **Actor** unchanged. No new tenant identifier.
- The **Platform Adapter** owns the platform-specific immediate ack inside its `Webhook` handler, before/around **Runtime Dispatch**, the same way **Platform Handshake** is adapter-owned (Slack 3s 2xx + optional `response_url` / `trigger_id`; Teams invoke response on the turn). Unbuildable or unsupported shapes are **Ignored Events** after verification; bad signatures and malformed requests are rejected.
- Long command/interaction work uses the ADR 0002 `DispatchDeferred` **Dispatch Mode** + **Ack-Then-Work** + **Detached Work Context**, not a bespoke per-hook mechanism. Under deferred dispatch the **Lock Lease** is acquired before ack and held across detached work, refreshed via `ExtendLock`; **Runtime State** mutations follow the **Detached Work Context**.
- This slice covers `block_actions` (button clicks, menu selections), which acknowledge with an empty `2xx` and follow up via `response_url`, fitting **Ack-Then-Work** cleanly. Opening a modal (`trigger_id` → `views.open`) is an outbound **Optional Capability**. Modal `view_submission` requires a *synchronous* response — field-level validation errors or a view update/push — in the 3-second ack body, which **Ack-Then-Work** cannot provide; its synchronous `response_action` handling is deferred to a separately-designed sub-slice. The **Interaction Event** path here is for asynchronous, `response_url`-style interactions, not the synchronous modal-submission response.
- Like message **Events**, **Interaction Events** acquire the per-**Thread Lock**. Serialization is correct because a click often mutates the same conversation/app state, but it has a cost: under the default `drop` **Concurrency Strategy** a click that lands while a deferred handler holds the lock is dropped, and under `queue` (ADR 0012) it is handled after the in-flight turn — so a time-sensitive action can stall behind a long handler. Interactive bots should select the `queue` strategy; a finer per-interaction lock scope (ADR 0012 `lockScope`) is a possible later refinement if this proves painful.

Outbound:

- Do **not** add a cross-platform card model and do **not** change the meaning of **Postable Message**. **Plain Text** + **Portable Markdown** stay the portable default.
- Model native rich content as proposed **Native Content**: an explicitly platform-native body (Block Kit / Adaptive Card) carried as a **Platform Escape Hatch**, sent through a proposed **Optional Capability** interface `NativeContentPoster` reached via typed **Adapter Access** (`chat.AdapterAs`).
- Native content is opt-in and explicit. Absence of the capability returns an explicit unsupported-capability result; a `NativeContent` whose adapter does not match the target is an error, never a silent portable downgrade. Native posting returns a **Sent Message** like portable posting.

Illustrative shapes (design only, not final API):

```go
// OnCommand is defined by ADR 0003; shown only for context.
func (c *Chat) OnInteraction(func(context.Context, *InteractionEvent) error)

// Optional Capability for explicit platform-native posts.
type NativeContentPoster interface {
    PostNative(ctx context.Context, ref ThreadRef, c NativeContent) (*SentMessage, error)
}
```

## Consequences

- A button click and a slash command are first-class normalized **Events** with dedupe, locking, and tenant-scoped **Thread** / **Actor**, but never reach message hooks. Apps stop parsing raw interactivity payloads outside runtime guarantees.
- The inbound and outbound halves close the loop: a bot posts Block Kit via `NativeContentPoster`, and the resulting clicks return as **Interaction Events**.
- Ack timing stays adapter-owned and correct (Slack 3s, Teams turn). Long work is deferred through the shared ADR 0002 primitive, so this ADR adds no new async lifecycle.
- The portable surface is untouched: **Postable Message**, **Plain Text**, **Portable Markdown** keep their MVP meaning, and native payloads can only be sent deliberately through **Adapter Access**, never by accident.
- Two MVP non-goals are reopened narrowly. The broad **Outbound Mutation** surface and platform-native payload builders remain deferred, so scope creep is bounded and documented.
- This slice ships `block_actions` (buttons, menus) inbound and **Native Content** + modal-open outbound. Modal `view_submission`'s synchronous response (validation errors / view update) is incompatible with **Ack-Then-Work** and is deferred to a separate sub-slice, keeping the slice bounded.
- New proposed terms (**Interaction Event**, `OnInteraction`, **Native Content**) are added to CONTEXT.md vocabulary only when this ADR is accepted. `OnCommand` / **Command Event** are owned by ADR 0003, not this ADR.
- Teams support is gated on the ADR 0007 spike (SDK choice, JWT auth, turn/invoke contract). This ADR fixes the cross-platform **Interaction Event** + ack contract Teams must satisfy but does not implement it.

## Alternatives Considered

### Add a cross-platform card model into Postable Message

Rejected. It would force the runtime to own a card DSL that maps lossily onto every platform, contradicting the **Postable Message** "no card DSL / no native payload" rule and the **Semantic Compatibility** (not feature parity) stance. Native content as a **Platform Escape Hatch** behind an **Optional Capability** keeps the core narrow.

### Deliver button clicks as Messages

Rejected. CONTEXT.md is explicit that a button click is an **Event**, not a **Message**. Overloading **Message** would pollute message routing and force handlers to discriminate payload kinds the runtime should normalize.

### Multi-handler registration for interactions (Vercel shape)

Rejected. The runtime's load-bearing pattern is single-slot atomic-replace **Routing Hooks**; `OnCommand` / `OnInteraction` follow `OnNewMention` / `OnSubscribedMessage` for determinism and race-safety, an intentional divergence from Vercel's multi-handler registration.

### A bespoke per-hook async mechanism for long interaction work

Rejected. Deferred dispatch is a shared primitive owned by ADR 0002 (**Dispatch Mode** / **Ack-Then-Work** / **Detached Work Context**). Inventing a separate async path here would duplicate lock-extension and detached-context semantics and risk drift.

### Runtime-owned interaction ack and Retry-After backoff

Rejected. Platform ack timing (Slack 3s, Teams turn) is platform-specific and belongs with **Platform Handshake**-style adapter ownership; outbound rate-limit backoff is ADR 0005's adapter concern. The runtime owning ack would leak platform deadlines into core dispatch.

### Typed Block Kit / Adaptive Card builders in the runtime

Rejected for this slice. The runtime carries an opaque native payload; typed builders are app-owned (or a later, separately justified slice). Baking builders in would re-import the cross-platform-rich-formatting scope the MVP deferred.

### Reopen the full Outbound Mutation surface

Rejected. Only the message-update an interaction response needs is reopened. General edit, delete, and reactions stay deferred to a separate slice so this ADR's scope stays bounded.
