# ADR 0003: Command Events and Slash Command Routing

## Status

Accepted

## Context

The **Go Chat Runtime** routes message-created **Events** through `OnNewMention` and `OnSubscribedMessage`. Platform commands are not messages: a Slack slash command (`/deploy staging`) and the analogous Microsoft Teams command/invoke are deliberate, parameterized invocations delivered on a separate webhook path, carrying no thread message and imposing a hard acknowledgement deadline.

CONTEXT.md already defines a **Command Event** as "a platform command invocation that is distinct from a normal message-created **Event**" and records that **Command Event** support is "deferred until after the **Slack-First Slice** proves message conversation routing." That slice is implemented. This ADR activates the term.

Reopened non-goals (per `docs/agents/domain.md`, citing the source):

- MVP PRD Out of Scope: **"Slash commands and other Command Events."** Reopened: the message-routing gate that deferred it is satisfied (Slack MVP and Linear app-actor slice are implemented). This ADR designs the activation.
- MVP PRD Out of Scope: **"Pattern handlers."** Surfaced and explicitly NOT reopened. A **Command Event** is a structured platform invocation, not a **Pattern Handler** matching message content. This ADR keeps pattern handlers deferred.
- MVP PRD Out of Scope: **"Middleware."** Surfaced and explicitly NOT reopened. `OnCommand` is a single-slot **Routing Hook** on the existing dispatch spine, not a **Middleware** chain.

Two facts constrain the design. First, the current **Runtime Dispatch** treats an `Event` with a nil `Message` as an "ignored non-message event" and acknowledges it without dispatch; a command smuggled into that shape would silently vanish. Second, Slack requires a `2xx` acknowledgement within 3 seconds and Teams requires an invoke/turn response, so a command handler that does real work cannot run synchronously inside the inbound request.

This ADR depends on and cross-references sibling work: deferred dispatch (**Dispatch Mode**, **Ack-Then-Work**, **Detached Work Context**) is ADR 0002; the sibling **Interaction Event** / `OnInteraction` for Block Kit and card actions is ADR 0004; native rich responses as an **Optional Capability** are ADR 0004; Teams adapter specifics are ADR 0007; multi-tenant credential resolution is ADR 0006.

## Decision

Model a **Command Event** as a non-message **Event** kind routed through a new single-slot **Routing Hook**, `OnCommand`.

- A **Command Event** is an `Event` carrying a `Command` payload and a nil `Message`. It is explicitly NOT a **Message**, consistent with CONTEXT.md's "a button click is an **Event**, not a **Message**." Reuse the existing `Event` envelope; do not add a parallel event type.

```go
type Command struct {
    Name  string   // normalized command name, e.g. "/deploy"
    Text  string   // raw argument string after the command name
    Args  []string // advisory whitespace split of Text, not a parser
    Actor Actor    // who invoked the command
    Raw   any      // Platform Escape Hatch: response_url, trigger_id, Teams invoke value
}

type CommandEvent struct {
    Event   *Event
    Thread  *Thread
    Command *Command
}

type CommandHandler func(context.Context, *CommandEvent) error
```

- Add `OnCommand(CommandHandler)` to `Chat` as a single-slot **Routing Hook**: atomic replace, no-op when unset, intentionally unlike Vercel Chat SDK's multiple-handler registration. GoDoc states the difference.
- Extend **Runtime Dispatch** so an `Event` with a non-nil `Command` routes to `OnCommand` and never to the message hooks. Command-ness takes precedence over subscription state: a command in a **Subscribed Thread** still routes to `OnCommand`. The current nil-`Message` branch narrows to route commands; only events with neither `Message` nor `Command` remain **Ignored Events**.
- **Command Events** ride the existing dispatch spine unchanged: dedupe by **Event Identity** (a stable adapter-scoped command identity, with platform delivery recorded as **Retry Metadata**), **Thread Lock** acquisition, **Self Message** filtering, **Lock Conflict** acknowledge-and-drop, and acceptance semantics. A **Command Event** is an **Accepted Event** once verified and normalized, acknowledged by default even when its handler fails.
- A command carries the normalized **Thread**, **Actor**, and **Platform Escape Hatch** `Raw`, and reuses the opaque adapter-produced **Thread ID** for the conversation it was invoked in, scoped by **Platform Tenant**. A command does not subscribe or unsubscribe a **Thread**; the handler subscribes explicitly if it wants follow-up messages.
- Ack is adapter-owned, mirroring **Platform Handshake**. The Slack adapter sends the immediate `2xx` within the 3-second budget and preserves `response_url` / `trigger_id` on `Command.Raw`; it dispatches the decoded **Command Event** through the same `Webhook(DispatchFunc)` seam it already uses for messages, with no entrypoint that bypasses runtime guarantees. Slack payloads are decoded from `x-www-form-urlencoded` as a **Supported Platform Shape** with local structs, unknown-field tolerance, and required-field validation (`command`, `team_id`, `channel_id`, `user_id`, `trigger_id`).
- Long command work uses the shared **Dispatch Mode** / **Ack-Then-Work** / **Detached Work Context** primitive from ADR 0002, not a command-specific mechanism. Under `DispatchDeferred` the adapter acks within the platform deadline, the handler runs under the **Detached Work Context**, and the **Thread Lock** is acquired before ack and refreshed via `ExtendLock` across the detached work.
- Native command responses (ephemeral vs in-channel, Block Kit bodies, delayed `response_url` posts) are NOT added to **Postable Message**. They are reached through typed **Adapter Access** as an **Optional Capability** (ADR 0004) or via `Command.Raw`. **Plain Text** + **Portable Markdown** stay the portable surface.
- Teams command/invoke normalizes into the same `Command` shape behind the adapter seam, marked spike-required (ADR 0007). It does not block the Slack slice.

## Consequences

A bot can register one command handler and receive normalized, tenant-scoped **Command Events** with the same context and runtime guarantees as message **Events**:

```go
bot.OnCommand(func(ctx context.Context, ev *chat.CommandEvent) error {
    _, err := ev.Thread.Post(ctx, chat.Text("running "+ev.Command.Name))
    return err
})
```

For native command responses, the handler reaches the adapter deliberately through **Adapter Access** rather than widening the core:

```go
slackAdapter, ok := chat.AdapterAs[*slack.Adapter](bot, "slack")
if ok {
    // RespondURL posts to the preserved response_url (ephemeral by default).
    _ = slackAdapter.RespondURL(ctx, ev.Command.Raw, chat.Text("queued"))
}
```

The runtime gains a third single-slot **Routing Hook** but no new dispatch spine, no new lock or dedupe path, and no new posting surface. The "ignored non-message event" branch tightens, so a future event kind without a handler still degrades to an **Ignored Event** rather than an error.

Deliberate divergences from the upstream Chat SDK and intentional limits:

- Single-slot `OnCommand`, not multiple handlers or a command registry/router keyed by command name. Command-name dispatch is application-owned.
- No command argument grammar, flag parser, or subcommand router; the runtime offers raw `Text` and an advisory `Args` split.
- Native rich command responses stay an **Optional Capability** + **Platform Escape Hatch**, never core **Postable Message**.
- Synchronous dispatch remains the default; long command work depends on ADR 0002 being accepted, not on a per-command timer.
- Like messages, **Command Events** acquire the per-**Thread Lock**; under the default `drop` **Concurrency Strategy** a command issued while a deferred handler holds the lock is dropped, and under `queue` (ADR 0012) it runs after the in-flight turn. Bots expecting commands mid-conversation should select `queue`.
- Teams remains spike-gated (ADR 0007); the Slack slice ships without it.

These keep the **Command Event** slice small, consistent with the existing **Adapter** / **State** design, and faithful to the four load-bearing patterns: single-slot **Routing Hooks**, opaque adapter-produced **Thread ID**, the small **Adapter** interface, and **Platform Escape Hatch** / **Optional Capability** over core widening.

## Alternatives Considered

### Route slash commands as **Messages**

Rejected. CONTEXT.md already resolves that a slash command is a **Command Event** with separate routing semantics, and that not every **Event** is a **Message**. Forcing commands through `OnNewMention` / `OnSubscribedMessage` would conflate structured invocations with conversation, drag subscription state into command routing, and lose the command name and platform response handles. Rejected for breaking the **Event** vs **Message** distinction.

### Add a command-name router or pattern-matching dispatch

Rejected. A registry keyed by command name, or routing by content match, reintroduces **Pattern Handler** semantics that CONTEXT.md keeps deferred and multiplies the single-slot hook into a table. The runtime exposes one `OnCommand` hook with the normalized `Command.Name`; the application switches on it. Rejected as premature routing machinery and as scope creep into pattern handlers.

### Give commands a bespoke per-hook async/ack mechanism

Rejected. A command-specific background-work timer would duplicate the deferred-dispatch primitive and create a second, inconsistent async model. The shared **Dispatch Mode** / **Ack-Then-Work** / **Detached Work Context** from ADR 0002 already covers Slack's 3-second deadline, Linear's 10s, and Teams's turn. Rejected to avoid two divergent async contracts; commands consume ADR 0002 instead.

### Let the adapter dispatch command handlers directly, outside **Runtime Dispatch**

Rejected. A **Platform Adapter** must not expose app paths that bypass runtime dedupe, locking, or self-filtering. Commands ride the existing `Webhook(DispatchFunc)` seam so they inherit **Event Identity** dedupe, the **Thread Lock**, **Lock Conflict** handling, and **Runtime Observation**. Rejected for breaking the adapter/runtime boundary.

### Model native command responses as a new cross-platform card / response model in **Postable Message**

Rejected. Ephemeral-vs-in-channel, Block Kit, and `response_url` semantics are platform-specific and would widen the portable surface that CONTEXT.md fixes at **Plain Text** + **Portable Markdown**. Native responses are an **Optional Capability** reached via **Adapter Access**, or carried on `Command.Raw` (ADR 0004). Rejected to keep the portable surface stable.

### Subscribe the **Thread** automatically on command invocation

Rejected. Auto-subscription would make issuing a command silently start routing future messages, contradicting the explicit-subscription rule that a **New Mention** handler must opt in. Command handlers subscribe explicitly when they want follow-up. Rejected for hidden subscription side effects.

### Fold commands into the sibling **Interaction Event**

Rejected. Slash commands and Block Kit / card actions arrive on different platform contracts and deserve distinct single-slot hooks (`OnCommand` vs `OnInteraction`, ADR 0004). Overloading one hook would force callers to branch on a kind discriminator inside a handler. Rejected to keep each non-message **Event** kind on its own hook.
