# Interactive Components and Rich Postable Content

Status: needs-triage

## Problem Statement

The MVP **Go Chat Runtime** routes inbound **Messages** and posts **Plain Text** + **Portable Markdown** replies. It cannot represent two things real bots need:

1. **Inbound interactive-component actions.** A Slack Block Kit button click, overflow menu selection, or modal submission is delivered as an x-www-form-urlencoded interactivity payload with a `type`, and a Teams card action arrives as a Bot Framework invoke Activity. These are not **Messages** ("a button click is an **Event**, not a **Message**"), and they carry their own ack contract: Slack expects a 2xx within 3 seconds (optionally a `response_url` / `trigger_id` follow-up); Teams expects an invoke response on the turn. The runtime has no normalized envelope or **Routing Hook** for them, so apps would have to parse raw payloads and bypass dedupe, locking, and subscription guarantees to handle a button.

2. **Outbound native rich content.** Block Kit and Adaptive Cards are how production bots render approval prompts, pickers, and the very buttons that produce the inbound actions above. The MVP **Out of Scope** list explicitly excludes "rich cards, JSX-style cards, modals, files, platform-native payload builders, and cross-platform rich formatting." That exclusion was correct for proving the portable surface, but a bot that cannot emit a button cannot receive a button click. We need native rich content **without** changing what **Postable Message** means and **without** inventing a cross-platform card model the runtime would have to own forever.

The hard part is keeping portable content portable. **Plain Text** + **Portable Markdown** must stay the default and must never silently become a platform-native payload. Native content must be opt-in and explicit, reached the same deliberate way Slack-specific APIs already are.

This PRD also depends on the deferred-dispatch primitive: long interaction/command work cannot run on the dead inbound request context. That primitive (**Dispatch Mode**, **Ack-Then-Work**, **Detached Work Context**) is owned by the async-dispatch work item (ADR 0002) and is referenced here, not redefined.

## Solution

Add inbound interactivity and outbound native content as two linked extensions that preserve the four load-bearing patterns: single-slot **Routing Hooks**, opaque adapter-produced **Thread ID**, the small **Adapter** interface, and **Platform Escape Hatch** / **Optional Capability** over core widening.

Inbound:

- Add a proposed **Interaction Event** as a new non-message **Event** kind, sibling to the existing **Command Event** (CONTEXT.md already reserves **Command Event**; only **Interaction Event** is net-new). Both are **Events**, not **Messages**.
- Route the **Interaction Event** through its own single-slot **Routing Hook**: a proposed `OnInteraction`, atomic-replace exactly like `OnNewMention` / `OnSubscribedMessage` (intentionally unlike Vercel's multi-handler registration) and like the sibling `OnCommand` that ADR 0003 adds for **Command Events**.
- Both carry the normalized **Thread**, **Actor**, and a **Platform Escape Hatch** `Raw`. **Command Event** payloads are decoded as a **Supported Platform Shape** from x-www-form-urlencoded; **Interaction Event** carries the normalized action identity plus `Raw`.
- The **Platform Adapter** owns the platform-specific immediate ack (Slack 3s 2xx + optional `response_url` / `trigger_id`; Teams invoke response) as part of webhook handling, the same way **Platform Handshake** is adapter-owned. Long work uses the ADR 0002 **Ack-Then-Work** + **Detached Work Context** primitive, not a bespoke per-hook mechanism.
- This slice covers `block_actions` (button clicks, menu selections) inbound: they acknowledge with an empty `2xx` and follow up via `response_url`, fitting **Ack-Then-Work** cleanly. Opening a modal (`trigger_id` -> `views.open`) is an outbound **Optional Capability**. Modal `view_submission` requires a *synchronous* `response_action` in the 3-second ack body (field-level validation errors or a view update/push), which **Ack-Then-Work** cannot provide; its synchronous handling is deferred to a separately-designed sub-slice (see Out of Scope). The **Interaction Event** path here is for asynchronous, `response_url`-style interactions, not the synchronous modal-submission response.
- Like message **Events**, **Interaction Events** acquire the per-**Thread Lock**. Serialization is correct because a click often mutates the same conversation/app state, but it has a cost: under the default `drop` **Concurrency Strategy** a click that lands while a deferred handler holds the lock is dropped, and under `queue` (ADR 0012) it is handled after the in-flight turn — so a time-sensitive action can stall behind a long handler. Interactive bots should select the `queue` strategy; a finer per-interaction lock scope (ADR 0012 `lockScope`) is a possible later refinement.

Outbound:

- Do **not** add a cross-platform card model and do **not** change the meaning of **Postable Message**. **Plain Text** and **Portable Markdown** stay portable.
- Model native rich content as a proposed **Native Content** payload sent through a proposed **Optional Capability** interface (e.g. `NativeContentPoster`) reached via typed **Adapter Access**. The payload is an explicitly platform-native body (Block Kit / Adaptive Card) carried as a **Platform Escape Hatch**.
- Native content is opt-in and explicit; portable content remains the default and never silently becomes native.

This is a **Semantic Compatibility** extension, not a feature-parity port. It reopens two MVP non-goals deliberately and narrowly (see Out of Scope).

## User Stories

1. As a Slack bot developer, I want a Block Kit button click delivered as a normalized **Interaction Event**, so that I can handle it without parsing raw interactivity payloads.
2. As a Slack bot developer, I want a slash command delivered as a **Command Event**, so that commands route separately from **Messages**.
3. As a bot developer, I want `OnInteraction` and `OnCommand` as single-slot **Routing Hooks**, so that interaction and command routing match the atomic-replace shape of the existing hooks.
4. As a bot developer, I want missing `OnInteraction` / `OnCommand` handlers to be no-ops, so that I only implement the flows I need.
5. As a bot developer, I want **Interaction Events** and **Command Events** to carry **Thread** and **Actor**, so that I can post back to the right conversation as the right participant.
6. As a bot developer, I want the original platform payload preserved as a **Platform Escape Hatch** `Raw`, so that uncommon fields (action values, view state, `trigger_id`) remain reachable.
7. As a Slack adapter author, I want the adapter to ack the interaction within 3 seconds before handing off to **Runtime Dispatch**, so that Slack does not mark the action failed.
8. As a Slack adapter author, I want `response_url` / `trigger_id` exposed through the **Platform Escape Hatch**, so that handlers can update a message or open a modal as the explicit interaction response.
9. As a Teams adapter author, I want a card action invoke normalized to an **Interaction Event** with an adapter-owned invoke ack, so that Teams' turn contract is honored without leaking into the runtime.
10. As a bot developer running long interaction work, I want to use the ADR 0002 **Ack-Then-Work** + **Detached Work Context** primitive, so that I do not run handler work on the dead inbound request context.
11. As a runtime operator, I want **Interaction Events** and **Command Events** deduped by **Event Identity** and serialized by **Thread Lock** like other **Events**, so that retried button clicks do not double-fire.
12. As a bot developer, I want to post a **Native Content** payload through an **Optional Capability** reached via typed **Adapter Access**, so that I can render Block Kit / Adaptive Cards explicitly.
13. As a bot developer, I want **Plain Text** and **Portable Markdown** posting to stay exactly as the MVP defined them, so that portable replies never silently become a native payload.
14. As a bot developer, I want native content to be opt-in, so that I never accidentally ship a platform-locked payload through the portable path.
15. As a bot developer on an adapter without native posting, I want an explicit unsupported-capability result, so that absence of the capability is a typed contract, not a panic.
16. As a Slack bot developer, I want to post Block Kit blocks that contain buttons whose clicks return as **Interaction Events**, so that the inbound and outbound halves close the loop.
17. As a runtime operator, I want **Interaction Event** and **Command Event** routing visible through **Runtime Observation**, so that ignored, deduped, and lock-conflicted actions are explainable.
18. As a future maintainer, I want the reopened MVP non-goals documented with justification, so that the scope change from the MVP PRD is intentional and discoverable.

## Implementation Decisions

- Add **Interaction Event** as a new non-message **Event** kind alongside the existing **Command Event**, both distinct from **Message**. A button click and a slash command are **Events**, never **Messages**.
- Carry interaction/command payloads on the existing **Event** envelope so they reuse **Event Identity** dedupe, **Thread Lock** serialization, and **Platform Tenant**-scoped **Thread ID** / **Actor** unchanged. No new tenant identifier is introduced.
- Add the single-slot **Routing Hook** `OnInteraction` (the sibling `OnCommand` is added by ADR 0003), registered with the same atomic-replace **Handler Registration** semantics as `OnNewMention` / `OnSubscribedMessage`. GoDoc must restate the intentional single-handler divergence from Vercel Chat SDK.
- Illustrative inbound shape (design only, not final). `CommandEvent` and `OnCommand` are defined by ADR 0003 (slash-commands) and only referenced here; the net-new shape is **Interaction Event**:

  ```go
  // Interaction Event: normalized action identity + Platform Escape Hatch.
  type InteractionEvent struct {
      Event   *Event
      Thread  *Thread
      Actor   Actor
      Kind    InteractionKind // button | menu | ... (block_actions this slice)
      ActionID string         // adapter-normalized action identifier
      // response_url, trigger_id, action values live in Event.Raw
  }

  // OnCommand is defined by ADR 0003; shown only for context.
  func (c *Chat) OnInteraction(func(context.Context, *InteractionEvent) error)
  ```

- The **Platform Adapter** owns the platform-specific immediate ack inside its `Webhook` handler, before/around **Runtime Dispatch**, exactly like **Platform Handshake**: Slack returns a 3s 2xx (empty body, or an inline response-message body when the handler is synchronous and fast); Teams returns the invoke response on the turn. The runtime does not own platform ack timing.
- Long command/interaction work uses the ADR 0002 **Dispatch Mode** (`DispatchDeferred`) + **Ack-Then-Work** + **Detached Work Context** primitive. That ADR owns the lock/state behavior across detached work; this PRD does not redefine it.
- **Interaction Event** payloads are decoded as a **Supported Platform Shape** from x-www-form-urlencoded (Slack interactivity) with permissive unknown-field handling and explicit required-field validation, following the Slack adapter pattern (**Command Event** decoding is ADR 0003's). Unbuildable or unsupported interaction shapes are **Ignored Events** after webhook verification succeeds; bad signatures and malformed requests are rejected, not ignored.
- The adapter must not expose a normal app path that dispatches interactions/commands outside runtime dedupe, locking, and subscription checks. `response_url` / `trigger_id` and Teams invoke context are reached through the **Platform Escape Hatch**, not promoted into the core surface.
- Do **not** change **Postable Message**. **Plain Text** + **Portable Markdown** keep their MVP meaning and stay the default outbound path.
- Add **Native Content** as a proposed **Optional Capability**, detected through a narrow Go interface and reached via typed **Adapter Access** (`chat.AdapterAs`), not through string flags or a widened core post method. Illustrative shape (design only):

  ```go
  // NativeContent carries an explicitly platform-native body as a
  // Platform Escape Hatch. It is NOT a cross-platform card model.
  type NativeContent struct {
      Adapter string // must match the target adapter; mismatch is an error
      Payload any    // Slack Block Kit blocks / Teams Adaptive Card
  }

  // Optional Capability: only adapters that implement it support native posts.
  type NativeContentPoster interface {
      PostNative(ctx context.Context, ref ThreadRef, c NativeContent) (*SentMessage, error)
  }
  ```

- Absence of `NativeContentPoster` returns an explicit unsupported-capability result, matching the existing **Optional Capability** rule. A `NativeContent` whose `Adapter` does not match the target adapter is an error, never a silent portable downgrade.
- Native posting returns a **Sent Message** record like portable posting. Editing a previously posted message (an interaction response that updates a card) is an **Outbound Mutation** and is reopened only narrowly (see Out of Scope); the broad edit/delete/reaction surface stays deferred.
- Surface **Interaction Event** / **Command Event** routing, dedupe hits, **Lock Conflicts**, and ack outcomes through structured **Runtime Observation**; richer metric/trace hooks are the observability work item (ADR 0010), not here.
- Teams normalization (Bot Framework Activity -> **Event**, conversationReference -> opaque **Thread ID**, JWT auth, turn/invoke ack) is the Teams work item (ADR 0007) and is spike-required; this PRD only fixes the cross-platform **Interaction Event** + ack contract Teams must satisfy.

## Testing Decisions

- Tests assert external behavior and public contracts, not adapter parser internals.
- Routing tests cover `OnInteraction` / `OnCommand` unset no-op, install, atomic replace under concurrent dispatch, and that **Interaction Events** / **Command Events** never reach message hooks (they are not **Messages**).
- Dedupe/lock tests cover **Interaction Event** / **Command Event** **Event Identity** dedupe (retried button click runs once), **Thread Lock** serialization with concurrent message events, and **Lock Conflict** acknowledgement.
- Slack adapter tests use golden interactivity and slash-command payloads (x-www-form-urlencoded) covering `block_actions` button, static/overflow menu, slash command, unsupported interaction type (including a `view_submission` payload, ignored in this slice), malformed body, and invalid signature.
- Ack tests assert the Slack 3s 2xx is returned before detached work begins, that `response_url` / `trigger_id` are reachable through the **Platform Escape Hatch**, and that deferred work runs under a **Detached Work Context**, not the request context (cross-referencing ADR 0002 ack tests).
- Teams adapter tests are spike-gated (ADR 0007) but must cover invoke-Activity normalization to **Interaction Event** and adapter-owned invoke ack once the SDK/auth contract is confirmed.
- Native content tests cover present-capability `PostNative` success and **Sent Message** return, absent-capability explicit unsupported result, adapter-mismatch error, and that the portable `Plain Text` / `Portable Markdown` path is unchanged and never emits a native payload.
- **Adapter Access** tests cover retrieving `NativeContentPoster` by type, wrong type, missing adapter, and no panic in normal examples.
- Documentation tests/review confirm GoDoc/README document the single-handler divergence for the new hooks and the two reopened non-goals.

## Out of Scope

- A cross-platform card / **Postable Message** rich model. Native content stays a **Platform Escape Hatch** behind an **Optional Capability**; the runtime never owns a card DSL.
- Any change to the meaning of **Postable Message**, **Plain Text**, or **Portable Markdown**.
- Platform-native payload **builders** (typed Block Kit / Adaptive Card constructors). The runtime carries an opaque native payload; building it is app-owned.
- Modal `view_submission` synchronous responses (field-level validation errors, view update/push via `response_action`). They require a synchronous in-ack-body response within 3s, which is incompatible with **Ack-Then-Work**; this slice covers only `block_actions` inbound and modal-open outbound. Synchronous modal-submission handling is deferred to a separately-designed sub-slice. Modal-open (`trigger_id` -> `views.open`) ships here as an outbound **Optional Capability**.
- The broad **Outbound Mutation** surface (general edit, delete, reactions). Only the narrow message-update needed for an interaction response is reopened; everything else stays deferred to a separate slice.
- Files, uploads, and embedded media.
- The deferred-dispatch primitive itself (**Dispatch Mode**, **Ack-Then-Work**, **Detached Work Context**) — owned by ADR 0002, referenced here.
- Multi-tenant install / **Install Store** (ADR 0006), the Teams adapter internals (ADR 0007), and observability metric hooks (ADR 0010).
- Normalized cross-platform mentions, channel references, broad notifications, and date tokens (**Platform Control Syntax** stays deferred).
- Pattern handlers and **Middleware**.

### Reopened MVP non-goals (justified)

- **Inbound interactive-component handling — MVP PRD Out of Scope: "modals" and README "Intentional MVP Gaps": "no Slack interactions, buttons, shortcuts, or Block Kit workflow."** Reopened narrowly. The MVP excluded inbound interactions to prove the portable message surface first. A bot that emits buttons must receive their clicks, so handling Block Kit `block_actions` (button clicks, overflow/static menus) as **Interaction Events** is required to close the loop. We reopen only normalized inbound `block_actions` routing via the **Interaction Event** + `OnInteraction` **Routing Hook**; shortcuts and Block Kit *workflow* (multi-step workflow steps) stay out of scope. Sources: `.scratch/go-chat-runtime-mvp/PRD.md` Out of Scope ("modals"); `README.md` "Intentional MVP Gaps".
- **MVP PRD Out of Scope: "Rich cards, JSX-style cards, modals, files, platform-native payload builders, and cross-platform rich formatting."** Reopened narrowly. A bot that cannot emit a button cannot receive a button click, so native rich content is required to close the interaction loop. We reopen only native (Block Kit / Adaptive Card) payloads via **Optional Capability** + **Platform Escape Hatch**; JSX-style cards, files, native payload **builders**, and cross-platform rich formatting stay out of scope. **Postable Message** is unchanged. Source: `.scratch/go-chat-runtime-mvp/PRD.md` Out of Scope.
- **MVP PRD Out of Scope: "Edit, delete, reactions, and other Outbound Mutation operations."** Reopened minimally. A platform interaction response often updates the originating message (replace a card after a button click). We reopen only the message-update an interaction response needs; general edit, delete, and reactions remain deferred. Source: `.scratch/go-chat-runtime-mvp/PRD.md` Out of Scope.

## Further Notes

- This is the inbound/outbound pair of ADR 0004; it depends on the deferred-dispatch primitive (async-dispatch / ADR 0002) and relates to Teams (ADR 0007), multi-tenant install (ADR 0006), and observability (ADR 0010). Cross-reference those by number; do not redefine their decisions.
- Vercel divergence to keep explicit: the runtime stays a discrete-reply runtime with single-slot hooks and no JSX/generative-UI card model. Block Kit / Adaptive Cards are reached deliberately through **Adapter Access**, not registered as cross-platform components.
- Keep the **Command Event** vocabulary already in CONTEXT.md; `OnCommand` is owned by ADR 0003, not this work item. Mark **Interaction Event**, `OnInteraction`, and **Native Content** as proposed and add them to CONTEXT.md vocabulary only when ADR 0004 is accepted.
- Platform timing facts that must not drift: Slack interactivity/commands ack with a 2xx within 3 seconds (then work async); Teams card actions ack on the turn via an invoke response. Both are adapter-owned.
- Like message **Events**, **Interaction Events** take the per-**Thread Lock**, so a click can be dropped (default `drop` **Concurrency Strategy**) or stalled behind an in-flight turn (`queue`, ADR 0012) while a deferred handler holds the lock. Recommend the `queue` strategy for interactive bots; a finer per-interaction lock scope (ADR 0012 `lockScope`) is a possible later refinement.
- Modal `view_submission`'s synchronous `response_action` (validation errors / view update or push in the 3s ack body) is incompatible with **Ack-Then-Work** and is not handled by the generic **Interaction Event** path in this slice; it is deferred to a separately-designed sub-slice (see Out of Scope). Modal-open via `trigger_id` -> `views.open` ships here as an outbound **Optional Capability**.
