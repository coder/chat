# Command Events and Slash Command Routing

Status: needs-triage

## Problem Statement

The **Go Chat Runtime** routes message-created **Events** to `OnNewMention` and `OnSubscribedMessage`, but platform commands are not messages. A Slack slash command (`/deploy staging`) and the analogous Microsoft Teams command/invoke are deliberate, parameterized invocations that arrive on a separate webhook path, carry no thread message, and impose a hard acknowledgement deadline. CONTEXT.md already names the **Command Event** as "a platform command invocation that is distinct from a normal message-created **Event**", and the MVP PRD explicitly deferred it. The slot exists; nothing fills it.

Today such a payload would either be dropped by the adapter or, if smuggled into an `Event` with a nil `Message`, be logged by the runtime as an "ignored non-message event" and acknowledged without dispatch. There is no **Routing Hook** for it, no normalized shape, and no contract for the platform's ack window. Slack requires a `2xx` within 3 seconds; Teams requires an invoke/turn response. A command handler that does real work cannot meet that deadline synchronously.

The problem is to model the **Command Event** as a first-class non-message **Event** kind with its own single-slot **Routing Hook**, decode the `x-www-form-urlencoded` command payload as a **Supported Platform Shape**, and define ack semantics that reuse the deferred-dispatch primitive rather than inventing a per-hook async mechanism.

## Solution

Add a **Command Event** as a non-message **Event** kind and route it through a new single-slot **Routing Hook**, `OnCommand`. This is the activation of the **Command Event** term already in CONTEXT.md, not a new concept.

- A **Command Event** is an **Event** with a populated `Command` payload and a nil `Message`. It is explicitly **NOT** a **Message**, consistent with CONTEXT.md's "a button click is an **Event**, not a **Message**".
- It carries the normalized **Thread**, **Actor**, and a **Platform Escape Hatch** `Raw`, exactly like a message **Event**. The command-specific fields (name, raw argument text, parsed token list, platform response handles) are decoded from the platform's `x-www-form-urlencoded` body as a **Supported Platform Shape**.
- `OnCommand` is a single-slot **Routing Hook** that atomically replaces its handler, intentionally unlike Vercel Chat SDK's multiple-handler registration. A missing handler is a no-op and the adapter acknowledges the command without dispatch.
- **Command Events** flow through the same **Runtime Dispatch** spine as messages: dedupe by **Event Identity**, **Thread Lock** acquisition, **Self Message** filtering, **Lock Conflict** handling, and **Runtime Observation**. The runtime never lets a **Platform Adapter** call command handlers on a path that bypasses these guarantees.
- Commands relate to **Threads** but do not change subscription routing on their own. A command does not subscribe or unsubscribe a **Thread**; the handler subscribes explicitly if it wants follow-up messages, exactly as a **New Mention** handler does. A command in an already-**Subscribed Thread** still routes to `OnCommand`, not `OnSubscribedMessage`, because command-ness is an **Event** kind, not a subscription state.

Ack semantics are adapter-owned, mirroring **Platform Handshake**. The adapter sends the platform-specific immediate ack (Slack `2xx` within 3s, optionally seeding the `response_url` / `trigger_id` for follow-up; Teams invoke response) before or around **Runtime Dispatch**. Long command work uses the shared **Ack-Then-Work** contract and **Detached Work Context** from the deferred-dispatch ADR (ADR 0002) selected via a **Dispatch Mode** in **Runtime Options** — not a bespoke per-command timer. Cross-references: deferred dispatch and **Dispatch Mode** are ADR 0002; the sibling **Interaction Event** / `OnInteraction` for Block Kit and card actions is ADR 0004.

Native command responses (Slack ephemeral vs in-channel, Block Kit bodies, `response_url` follow-ups) are not modeled as new core posting surface. **Postable Message** stays **Plain Text** + **Portable Markdown**; richer command replies are reached through typed **Adapter Access** as an **Optional Capability** (ADR 0004), or via the **Platform Escape Hatch** response handles on the **Command Event**.

This is a runtime and Slack-adapter slice. Teams command/invoke is designed against the same shape but is spike-gated (ADR 0007).

## User Stories

1. As a Go application developer, I want a **Command Event** modeled as a non-message **Event** kind, so that slash commands are not forced through message routing.
2. As a Go application developer, I want a single-slot `OnCommand` **Routing Hook**, so that command handling follows the same atomic-replace model as `OnNewMention`.
3. As a Go application developer, I want a missing `OnCommand` handler to be a no-op that the adapter still acknowledges, so that unhandled commands do not trigger platform retries or errors.
4. As a Slack bot developer, I want the adapter to send the Slack `2xx` ack within 3 seconds, so that the user does not see a `dispatch_failed` / timeout error.
5. As a Slack bot developer, I want the slash command `x-www-form-urlencoded` body decoded as a **Supported Platform Shape**, so that I read normalized fields instead of parsing form values myself.
6. As a Slack bot developer, I want the raw command argument string preserved and also offered as a parsed token list, so that I can choose between my own parser and a simple split.
7. As a Slack bot developer, I want the `response_url` and `trigger_id` preserved through the **Platform Escape Hatch**, so that I can post delayed responses or open a modal after ack.
8. As a Teams bot developer, I want a Teams command/invoke normalized into the same **Command Event** shape, so that my command handler is platform-portable for the common case.
9. As a bot developer, I want **Command Events** carrying a normalized **Thread**, **Actor**, and **Platform Escape Hatch** `Raw`, so that I have the same context I get on a message **Event**.
10. As a bot developer, I want a command to NOT auto-subscribe its **Thread**, so that issuing a command does not silently start routing future messages to me.
11. As a bot developer, I want a command in a **Subscribed Thread** to still route to `OnCommand`, so that command-ness is decided by **Event** kind, not subscription state.
12. As a runtime operator, I want **Command Events** deduped by **Event Identity**, so that platform command retries do not run the handler twice.
13. As a runtime operator, I want **Command Events** coordinated by the **Thread Lock**, so that a command and a concurrent message in the same conversation do not race.
14. As a runtime operator, I want a command that hits a **Lock Conflict** acknowledged and recorded, so that the platform is not asked to retry accepted contention.
15. As a runtime operator, I want **Self Message** / self-command filtering to apply, so that a bot-issued command cannot loop.
16. As a runtime operator, I want **Runtime Observation** logs for ignored, accepted, duplicate, lock-conflict, and failed **Command Events**, so that command routing is explainable like message routing.
17. As a bot developer running long command work, I want **Ack-Then-Work** with a **Detached Work Context** (ADR 0002), so that the handler can outlive the 3-second Slack request without me hand-rolling a goroutine and a fresh context.
18. As a bot developer, I want follow-up posting to stay coordinated after ack via the ADR 0002 lock-holding primitive, so that I do not lose the **Thread Lock** during detached command work.
19. As a bot developer, I want delayed and native command responses reached through **Adapter Access** / **Optional Capability** (ADR 0004), so that **Postable Message** stays portable **Plain Text** + **Portable Markdown**.
20. As a bot developer, I want **Command Events** validated for required fields and tenant scoping, so that a malformed or cross-tenant command payload fails or is ignored safely.
21. As a future maintainer, I want **Interaction Events** kept separate (ADR 0004), so that Block Kit / card actions get their own `OnInteraction` hook rather than overloading `OnCommand`.
22. As a future maintainer, I want Teams command specifics marked spike-required (ADR 0007), so that the Slack slice ships without blocking on unverified Teams contract details.

## Implementation Decisions

- Add a `Command` payload type and carry it as a new optional field on the existing `Event` envelope. A **Command Event** is an `Event` with `Command != nil` and `Message == nil`. Do not add a parallel event type; reuse the dedupe/lock/route spine.

```go
type Command struct {
    Name    string   // normalized command name, e.g. "/deploy"
    Text    string   // raw argument string after the command name
    Args    []string // convenience whitespace split of Text; advisory, not a parser
    Actor   Actor    // who invoked the command
    Raw     any      // Platform Escape Hatch: response_url, trigger_id, Teams invoke value
}

type Event struct {
    // ...existing fields...
    Message *Message // nil for a Command Event
    Command *Command // nil for a Message Event; set for a Command Event
}
```

- Add a `CommandEvent` handler input mirroring `MessageEvent`, so command handlers get **Event**, **Thread**, **Actor**, and **Command** without unpacking the raw envelope.

```go
type CommandEvent struct {
    Event   *Event
    Thread  *Thread
    Command *Command
}

type CommandHandler func(context.Context, *CommandEvent) error
```

- Add a single-slot `OnCommand(CommandHandler)` **Routing Hook** on `Chat`, with atomic replace and no-op-when-unset semantics identical to `OnNewMention`. GoDoc must call out the single-handler difference from Vercel Chat SDK.
- Extend **Runtime Dispatch** routing so an `Event` with a non-nil `Command` routes to `OnCommand` and never to the message hooks. The existing nil-`Message` branch that currently logs "ignored non-message event" becomes: route command events to `OnCommand`; only events with neither `Message` nor `Command` remain **Ignored Events**.
- Command-ness takes precedence over subscription state for routing: a `Command` event routes to `OnCommand` even in a **Subscribed Thread**. Subscription continues to govern message routing only.
- Keep dedupe, **Thread Lock**, **Self Message** filtering, **Lock Conflict** acknowledge-and-drop, and acceptance semantics unchanged. A **Command Event** is an **Accepted Event** once verified and normalized, acknowledged to the platform by default even when its handler fails.
- Like messages, **Command Events** acquire the per-**Thread Lock**. Under the default `drop` **Concurrency Strategy** a command issued while a deferred handler holds the lock is dropped via **Lock Conflict** acknowledge-and-drop; under `queue` (ADR 0012) it runs after the in-flight turn. Bots expecting commands mid-conversation should select `queue`.
- **Event Identity** for a Slack slash command derives from a stable adapter-scoped command identity (e.g. trigger id plus command plus invoking user and timestamp), not from the HTTP delivery. Record platform delivery details as **Retry Metadata**.
- The **Thread ID** on a **Command Event** is the adapter-produced opaque id for the conversation the command was invoked in (Slack channel / DM, scoped by **Platform Tenant**), reusing the existing **Thread ID** codec. Commands invoked outside a thread context still address a conversation; the adapter roots the **Thread** the same way it does for a channel message.
- Ack is adapter-owned, like **Platform Handshake**. The Slack adapter sends the immediate `2xx` (empty body, or an early ephemeral acknowledgement) and preserves `response_url` / `trigger_id` on `Command.Raw` for follow-up. The adapter, not the runtime, owns the 3-second budget.
- Long command work uses the shared **Dispatch Mode** / **Ack-Then-Work** / **Detached Work Context** primitive defined in ADR 0002. Under `DispatchDeferred`, the adapter acks within the platform deadline and the command handler runs under the **Detached Work Context**; the **Thread Lock** is acquired before ack and refreshed via `ExtendLock` across the detached work. Do not add a command-specific async mechanism.
- Native command responses (ephemeral vs in-channel, Block Kit, delayed `response_url` posts) are NOT added to **Postable Message**. They are reached through typed **Adapter Access** as an **Optional Capability** (ADR 0004) or via `Command.Raw`. `Thread.Post` from a command handler posts a normal **Plain Text** / **Portable Markdown** reply to the command's conversation.
- The Slack adapter mounts command handling on its existing **Webhook Handler** (Slack delivers commands and events to configured request URLs); the adapter dispatches the decoded **Command Event** through the same `DispatchFunc` it already uses for messages. No new adapter-facing dispatch entrypoint that bypasses runtime guarantees.
- Slack command payloads are decoded with local structs from the `x-www-form-urlencoded` body, permissive unknown-field handling, and explicit validation of required fields (`command`, `team_id`, `channel_id`, `user_id`, `trigger_id`).
- Teams command/invoke normalization targets the same `Command` shape but is designed behind the adapter seam and marked spike-required (ADR 0007); it does not block the Slack slice.
- README / GoDoc document the new hook, the **Command Event** / **Message** distinction, the adapter-owned ack, and the **Ack-Then-Work** relationship to ADR 0002.

## Testing Decisions

- Tests assert external behavior and public contracts, not private parsing internals.
- Routing tests: a **Command Event** routes to `OnCommand`; a missing `OnCommand` handler is a no-op that the adapter still acknowledges; a command never routes to `OnNewMention` or `OnSubscribedMessage`; a command in a **Subscribed Thread** still routes to `OnCommand`; an event with neither `Message` nor `Command` remains an **Ignored Event**.
- Handler registration tests: unset no-op, install, atomic replace, and race-safe observation under concurrent dispatch, matching the message-hook tests.
- Dispatch tests: command **Accepted Event** acknowledged on handler error, dedupe by **Event Identity**, **Thread Lock** acquisition, **Lock Conflict** acknowledge-and-drop, self-command filtering, and context cancellation.
- Slack adapter tests: golden `x-www-form-urlencoded` slash-command payloads decoded to the **Supported Platform Shape**; required-field validation; `response_url` / `trigger_id` preserved on `Command.Raw`; **Thread ID** construction and **Platform Tenant** scoping for the command's conversation; immediate `2xx` ack within the 3-second budget asserted at the HTTP boundary.
- **Ack-Then-Work** tests (shared with ADR 0002): under `DispatchDeferred` a command exercises the ADR 0002 lock-holding primitive across detached work — asserting the behavior is owned by ADR 0002; this PRD verifies a **Command Event** routes through it.
- **Adapter Access** tests: typed retrieval of the Slack adapter for native command response capability; absent-capability returns explicit unsupported result.
- **Runtime Observation** tests: ignored, accepted, duplicate, lock-conflict, and failed **Command Events** are logged with adapter, event id, and route.
- Documentation tests/review: GoDoc and README state the single-handler `OnCommand` difference from Vercel Chat SDK and the **Command Event** vs **Message** distinction.

## Out of Scope

- **Interaction Events** and the `OnInteraction` hook for Block Kit buttons/menus/modals and Teams card actions — ADR 0004.
- Deferred dispatch itself: **Dispatch Mode**, **Ack-Then-Work**, and the **Detached Work Context** are defined in ADR 0002; this PRD consumes them and does not redefine them.
- A cross-platform card / rich command-response model. Native command responses are an **Optional Capability** + **Platform Escape Hatch** (ADR 0004); **Postable Message** stays **Plain Text** + **Portable Markdown**.
- **Pattern Handlers** that route messages by content matching. Commands are explicit platform invocations, not content patterns; pattern routing remains deferred.
- **Middleware** / user-defined dispatch wrappers. `OnCommand` is a single-slot hook on the same spine, not a middleware chain.
- A command argument grammar, flag parser, or subcommand router. The runtime offers raw `Text` and an advisory `Args` split; richer parsing is application-owned.
- Slack command autocomplete, command scopes/permissions management, and app-directory command registration. Those are platform configuration concerns, not runtime routing.
- Teams command/invoke beyond the shared shape: the exact invoke/turn contract and SDK choice are spike-required (ADR 0007).
- Multi-tenant credential resolution for commands. Commands reuse the **Platform Tenant** scoping already in **Thread ID** / **Actor**; per-tenant install lookup is ADR 0006.

## Further Notes

This PRD reopens three MVP PRD non-goals and justifies each reopening per `docs/agents/domain.md`:

- **"Slash commands and other Command Events"** (MVP PRD Out of Scope). The MVP deferred commands until message routing was proven; CONTEXT.md says **Command Event** support is "deferred until after the **Slack-First Slice** proves message conversation routing." That slice is implemented (README: Slack MVP and Linear app-actor slice are in place), so the gate is satisfied and the **Command Event** term can be activated as designed.
- **"Pattern handlers"** (MVP PRD Out of Scope). Surfaced only to distinguish it: a **Command Event** is a structured platform invocation, not a **Pattern Handler** matching message content. This work does not reopen pattern handlers; they stay deferred and explicitly out of scope.
- **"Middleware"** (MVP PRD Out of Scope). Surfaced to defend that `OnCommand` does not require **Middleware**: it is a single-slot **Routing Hook** on the existing dispatch spine. **Middleware** stays deferred; reopening it is rejected.

The load-bearing patterns are preserved: single-slot **Routing Hook** (`OnCommand`), opaque adapter-produced **Thread ID** reused unchanged, the small **Adapter** interface (commands ride the existing `Webhook(DispatchFunc)` seam), and **Platform Escape Hatch** / **Optional Capability** for native command responses instead of widening the core.

The deepest seams are the Slack `x-www-form-urlencoded` command decoder/validator, the adapter-owned ack within the 3-second budget, and the **Command Event** routing branch in **Runtime Dispatch**. Each is testable independently with golden payloads and HTTP-boundary assertions.
