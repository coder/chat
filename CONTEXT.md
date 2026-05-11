# Chat SDK Go

Chat SDK Go provides a Go-native runtime for building chat bots with the same core conversation model as Vercel Chat SDK, without promising TypeScript API compatibility or full feature parity.

## Language

**Go Chat Runtime**:
A Go-native SDK for routing normalized chat events to application handlers and posting replies through platform adapters.
_Avoid_: TypeScript port, Vercel Chat SDK clone

**Runtime Construction**:
The fallible startup step that assembles state, adapters, and validation into a ready **Go Chat Runtime**.
_Avoid_: Infallible constructor, lazy validation

**Runtime Shutdown**:
The idempotent cleanup step that asks adapters and runtime state to release resources.
_Avoid_: Best-effort silent cleanup

**Handler Registration**:
The mutable application step that attaches callbacks for runtime routing hooks such as new mentions and subscribed messages.
_Avoid_: Construction option, required handler

**Routing Hook**:
A single runtime callback slot for a specific routing case such as new mention or subscribed message.
_Avoid_: Middleware chain, handler list

**Semantic Compatibility**:
Compatibility with the upstream Chat SDK's core conversation semantics rather than its exact API shape or full feature set.
_Avoid_: Feature parity, API parity

**Slack-First Slice**:
The first production-shaped vertical slice of the runtime, using Slack to prove the core conversation model before adding another adapter.
_Avoid_: Multi-platform MVP

**Thread**:
The runtime's stable conversation address for routing events, storing subscription state, and posting replies.
_Avoid_: Slack thread_ts, channel, platform thread

**Thread ID**:
An opaque, stable string representation of a **Thread** that is produced and validated by its adapter.
_Avoid_: User-built thread key, raw platform id

**Thread Handle**:
A runtime object reconstructed from a valid **Thread ID** for posting or subscription outside an inbound handler.
_Avoid_: Manually constructed thread

**Subscribed Thread**:
A **Thread** whose future inbound messages should route to existing-conversation message handlers.
_Avoid_: Active thread, listening channel

**New Mention**:
A bot mention in an unsubscribed **Thread** that starts a new conversation with the runtime.
_Avoid_: Mention, trigger

**Pattern Handler**:
A handler that routes messages by matching their content rather than by conversation state.
_Avoid_: Core routing hook

**Command Event**:
A platform command invocation that is distinct from a normal message-created **Event**.
_Avoid_: Message, pattern match

**Middleware**:
A user-defined dispatch wrapper that can alter or short-circuit runtime handler flow.
_Avoid_: Observer, logger

**Runtime Observation**:
Structured visibility into runtime decisions such as ignored events, dedupe, lock conflicts, routing, and handler failures.
_Avoid_: Middleware, metrics framework

**Webhook Handler**:
An `http.Handler` entrypoint exposed by the runtime for one **Platform Adapter**.
_Avoid_: Built-in server, router integration

**Webhook Mount**:
The application step that asks the runtime for a **Webhook Handler** for a named adapter.
_Avoid_: Runtime 404 for configuration typo

**Platform Handshake**:
A platform-specific webhook setup request that an adapter must answer without runtime dispatch.
_Avoid_: Application event, user handler

**Runtime State**:
The required persistent state used by the runtime for thread subscriptions, webhook dedupe, locks, and runtime cache.
_Avoid_: Optional state, default memory

**Thread Application State**:
Product-specific state that an application associates with a **Thread** outside the runtime.
_Avoid_: Runtime state, thread.state

**Message History**:
Past messages in a **Thread** fetched from the platform after their original webhook delivery.
_Avoid_: Current event, application context store

**Memory State**:
An in-process **Runtime State** implementation for tests and local development only.
_Avoid_: Production state

**Redis State**:
A production **Runtime State** implementation for horizontally scaled deployments.
_Avoid_: Optional add-on, cache only

**Webhook Event**:
A single inbound delivery from a platform adapter before runtime dedupe and handler routing.
_Avoid_: Message, request

**Event Identity**:
The stable adapter-scoped identifier used to deduplicate a platform event across deliveries.
_Avoid_: Retry number, request id

**Retry Metadata**:
Platform delivery metadata that explains a repeated webhook attempt without changing the event's identity.
_Avoid_: Dedupe key

**Runtime Options**:
Grouped runtime configuration for coordination behavior such as dedupe and lock timing.
_Avoid_: One-off option sprawl

**Concurrency Strategy**:
The runtime policy for messages that arrive while another handler is already processing the same lock scope.
_Avoid_: Lock implementation detail

**Thread Lock**:
A per-**Thread** coordination guard that prevents concurrent handler execution for the same conversation.
_Avoid_: Event lock, adapter lock, handler lock

**Lock Lease**:
A token-owned **Thread Lock** record that can only be released or extended by its current owner.
_Avoid_: Untokened lock, delete-only lock

**Lock Conflict**:
A runtime condition where a **Webhook Event** is accepted but not handled because its **Thread** is already locked.
_Avoid_: Duplicate, handler failure

**Runtime Dispatch**:
The runtime-owned step that routes a normalized **Webhook Event** to the appropriate application handler.
_Avoid_: Adapter dispatch, job processing

**Dispatch Context**:
The `context.Context` used for runtime dispatch and handler execution.
_Avoid_: Background work context

**Accepted Event**:
A verified and normalized inbound event that the runtime has taken responsibility for, regardless of handler success.
_Avoid_: Successful handler, retriable event

**Ignored Event**:
A valid platform event that the adapter acknowledges without handing to runtime dispatch because it is irrelevant, unsupported, or self-authored.
_Avoid_: Rejected webhook, accepted event

**Platform Adapter**:
The platform-specific boundary that verifies inbound webhooks, normalizes events, and renders outbound posts for one chat platform.
_Avoid_: Runtime router, app handler

**Adapter Initialization**:
The startup step where a **Platform Adapter** validates static configuration and establishes required runtime facts such as bot identity.
_Avoid_: Lazy first webhook setup

**Event**:
A normalized inbound platform occurrence that the runtime can dedupe, lock, and route.
_Avoid_: Message

**Message**:
The normalized textual or post-like payload within an **Event**.
_Avoid_: Event, webhook

**Message Event**:
A message-focused handler input that carries the normalized **Event**, **Thread**, and **Message** for message routing hooks.
_Avoid_: Raw event, thread-message tuple

**Platform Escape Hatch**:
Raw or adapter-specific platform data exposed for uncommon cases without becoming the normal application API.
_Avoid_: Raw-first API, parser type contract

**Adapter Access**:
An explicit typed helper path for retrieving a registered **Platform Adapter** when application code needs platform-specific APIs.
_Avoid_: Unchecked type assertion in examples

**Postable Message**:
A normalized outbound message body that adapters can render to their platform's native posting API.
_Avoid_: Card DSL, native payload

**Sent Message**:
A platform-created outbound message record returned by a successful post operation.
_Avoid_: Editable message contract

**Ephemeral Message**:
A private platform-delivered message visible to a specific **Actor** in a platform conversation context.
_Avoid_: Normal thread reply, direct message

**Ephemeral Fallback**:
The explicit caller-selected behavior that sends a direct message when native **Ephemeral Message** delivery is unavailable.
_Avoid_: Public reply fallback, implicit DM fallback

**Outbound Mutation**:
An operation that edits, deletes, or reacts to an existing platform message.
_Avoid_: Core post

**Optional Capability**:
A runtime behavior supported only by adapters that implement its narrow Go interface.
_Avoid_: String capability flag, mandatory adapter method

**Direct Message Thread**:
A private **Thread** between the bot and one or more platform actors.
_Avoid_: Ephemeral message, proactive DM capability

**Plain Text**:
A **Postable Message** body with no formatting intent.
_Avoid_: Unescaped markdown

**Portable Markdown**:
Conservative CommonMark input that adapters may render, translate, or degrade across platforms.
_Avoid_: Slack mrkdwn, GitHub-flavored markdown, native formatted text, fuzzy markdown-like text

**Portable Markdown Subset**:
The supported presentation subset of **Portable Markdown** that callers can expect adapters to render or degrade predictably.
_Avoid_: Full CommonMark compatibility, rich message layout

**Platform Control Syntax**:
Native platform text syntax that changes platform behavior rather than only rendering presentation, such as mentions, channel references, broad notifications, or platform date tokens.
_Avoid_: Markdown formatting, harmless text styling

**Actor**:
A normalized participant identity for a human or bot within an adapter-scoped platform context.
_Avoid_: User, Slack user id

**Bot Kind**:
The normalized classification of whether an **Actor** is human, bot, or unknown.
_Avoid_: Pointer boolean

**Self Message**:
A **Message** authored by the bot identity for the receiving **Platform Adapter**.
_Avoid_: Bot event, echo

**Application Identity**:
The product-specific user or account identity that an application may map from a platform **Actor**.
_Avoid_: Runtime actor, platform user

**Platform Tenant**:
The platform-scoped installation or workspace context that disambiguates actors, threads, and credentials for an adapter.
_Avoid_: Global workspace, account

**Single-Install Adapter**:
A **Platform Adapter** configured for one **Platform Tenant** without runtime OAuth installation or per-tenant credential lookup.
_Avoid_: Multi-workspace adapter

**Supported Platform Shape**:
The subset of a platform webhook payload that an adapter explicitly understands and normalizes.
_Avoid_: Full platform schema, strict external SDK model

## Relationships

- A **Go Chat Runtime** aims for **Semantic Compatibility** with the upstream Chat SDK's core model.
- **Runtime Construction** returns configuration and initialization errors instead of deferring them to the first webhook.
- **Runtime Shutdown** attempts all adapter cleanup hooks before state cleanup and returns joined cleanup errors.
- **Runtime Construction** does not require application handlers; missing **Handler Registration** is a no-op for that route.
- **Handler Registration** may happen through methods on the constructed runtime.
- **Handler Registration** installs or atomically replaces one **Routing Hook** handler; it does not append to a handler chain.
- A missing **Routing Hook** is a no-op.
- **Routing Hook** GoDoc must call out that single-handler replacement differs from Vercel Chat SDK's multiple-handler registration.
- **Semantic Compatibility** does not require TypeScript API compatibility, JSX-style cards, or full adapter feature parity.
- A **Slack-First Slice** should prove the **Go Chat Runtime** before the project generalizes around a second platform.
- A **Thread** is adapter-scoped and may include multiple platform identifiers needed to address the conversation.
- In Slack, a **Thread** is rooted at the parent thread timestamp; for an unthreaded root message, it is rooted at that message's timestamp rather than the whole channel.
- Posting to a **Thread** replies to that conversation address; it does not create a new root/channel message by default.
- A **Direct Message Thread** follows normal **Thread** routing and posting semantics once it exists.
- An inbound **Direct Message Thread** message is treated as an implicit **New Mention** when the thread is not subscribed.
- A **Thread ID** must include the adapter identity and enough platform routing context to avoid collisions across workspaces, channels, and platforms.
- Application code may pass around a **Thread ID**, but only an adapter should construct or decode one.
- Application code may ask the runtime to create a **Thread Handle** from a **Thread ID**, but adapter validation still decides whether the id is valid.
- A **Subscribed Thread** routes inbound message-created **Events** to subscribed-message handlers before mention or pattern handlers are considered.
- A **New Mention** only exists when the mentioned **Thread** is not already subscribed.
- A **Subscribed Thread** remains subscribed until the application explicitly unsubscribes it.
- A **New Mention** handler must explicitly subscribe a **Thread**; successful handler completion does not subscribe automatically.
- A **Pattern Handler** is deferred until the core new-mention and subscribed-message routing semantics are proven.
- **Command Event** support is deferred until after the **Slack-First Slice** proves message conversation routing.
- **Middleware** is deferred from the MVP public API so runtime dispatch invariants remain simple.
- The MVP includes **Runtime Observation** through structured logging, not middleware or a metrics framework.
- The runtime exposes **Webhook Handlers** and does not own HTTP server, router, TLS, or graceful-shutdown concerns.
- A **Webhook Mount** fails immediately when the named adapter is not registered.
- A **Platform Adapter** handles its own **Platform Handshake** requests before runtime dispatch.
- A **Go Chat Runtime** requires **Runtime State**; it must not silently create **Memory State** for production-facing construction.
- **Runtime State** is coordination state, not **Thread Application State**.
- **Thread Application State** is deferred from the MVP and should live in the application's own storage.
- **Message History** APIs are deferred from the MVP.
- **Memory State** is suitable for tests and local development, not horizontally scaled deployment.
- The MVP includes **Redis State** so the **Slack-First Slice** can run across multiple instances.
- The runtime deduplicates **Webhook Events** and serializes handler execution with a **Thread Lock**.
- **Event Identity** is the dedupe key; **Retry Metadata** is recorded for observation only.
- Dedupe TTL and lock TTL belong in **Runtime Options**.
- Default **Runtime Options** use a 24 hour dedupe TTL and a 2 minute **Thread Lock** TTL.
- **Runtime Options** TTL values must be positive.
- **Runtime Options** include a **Concurrency Strategy** that defaults to drop.
- The MVP only implements the drop **Concurrency Strategy**, while keeping names compatible with future queue, debounce, force, or concurrent strategies.
- A **Thread Lock** must not drop distinct **Webhook Events** for the same **Thread**; it only coordinates their processing.
- A **Thread Lock** is represented as a **Lock Lease** with an ownership token.
- Releasing or extending a **Lock Lease** must verify the ownership token so an expired holder cannot affect a newer holder.
- A **Lock Conflict** is acknowledged to the platform by default and recorded as unhandled runtime contention.
- An adapter accepts and normalizes a **Webhook Event**, but **Runtime Dispatch** decides which handler runs.
- The initial **Go Chat Runtime** performs **Runtime Dispatch** synchronously while preserving a boundary for future deferred or queued execution.
- The synchronous **Dispatch Context** comes from the inbound webhook request.
- Runtime locks must be released when **Dispatch Context** is cancelled or handler execution exits.
- **Runtime State** mutations performed during synchronous dispatch respect the caller's **Dispatch Context** and fail when it is cancelled.
- An **Accepted Event** is acknowledged to the platform by default even when its application handler fails.
- Invalid verification or invalid platform payloads are not **Accepted Events**.
- An **Ignored Event** is acknowledged to the platform without runtime handler dispatch.
- Invalid signatures and malformed requests are rejected rather than treated as **Ignored Events**.
- A **Platform Adapter** owns platform webhook verification and message rendering, but **Runtime Dispatch** owns handler routing.
- A **Platform Adapter** should not expose normal app paths that bypass runtime dedupe, locking, or subscription checks.
- **Adapter Initialization** must fail before serving webhooks when required credentials or bot identity are missing.
- Tests may inject **Adapter Initialization** facts that production discovers from the platform.
- A **Webhook Event** is adapter input; an **Event** is the normalized runtime input derived from it.
- A **Message** may be part of an **Event**, but not every **Event** is a **Message**.
- Message **Routing Hooks** receive a **Message Event** so handlers can use both event-level context and message-specific fields.
- A **Platform Escape Hatch** supports platform-specific needs, but common flows should use normalized **Event**, **Message**, and **Thread** fields.
- **Adapter Access** is the sanctioned path for platform-specific APIs beyond the normalized runtime surface.
- Adapter parser structs must not become the stable **Platform Escape Hatch** contract by accident.
- The first **Postable Message** surface is plain text and markdown; rich cards, files, modals, and platform-native payloads are outside the initial compatibility target.
- Posting returns a **Sent Message**, but **Outbound Mutation** is deferred from the MVP.
- The **Slack-First Slice** must support **Ephemeral Message** delivery as a core optional capability.
- An **Ephemeral Message** is not a normal **Thread** reply and must never fall back to a public reply.
- **Ephemeral Fallback** is explicit; when native delivery is unavailable, callers choose whether to fall back to a **Direct Message Thread** or receive no delivered message.
- If **Ephemeral Fallback** is requested but impossible, the operation returns an error.
- An **Optional Capability** is detected through a small Go interface implemented by a **Platform Adapter**.
- Absence of an **Optional Capability** returns an explicit unsupported-capability result.
- **Plain Text** carries no formatting intent; adapters should disable platform markdown parsing for it while preserving benign platform presentation such as automatic URL linking.
- **Portable Markdown** carries conservative CommonMark formatting intent, not platform-native syntax.
- The **Portable Markdown Subset** is paragraphs, line breaks, emphasis, strong emphasis, inline code, fenced code blocks, block quotes, links, unordered lists, and ordered lists. Strikethrough is extension-tolerated and may degrade literally.
- Tables, task lists, images, raw HTML, heading hierarchy, footnotes, definition lists, and embedded media are outside the **Portable Markdown Subset**.
- A **Platform Adapter** should prefer a platform markdown input field for **Portable Markdown** when one exists, instead of converting CommonMark into a platform-native markdown dialect itself.
- If a platform accepts **Portable Markdown** but renders it less richly than intended, that is acceptable degradation. If the platform rejects the post, the adapter returns the platform error rather than retrying through an unowned markdown dialect converter.
- **Portable Markdown** must not be a hidden **Platform Control Syntax** channel; notifications, platform references, and platform date tokens require explicit modeling or a **Platform Escape Hatch**.
- Normalized mentions, channel references, broad notifications, and date tokens are deferred until real app needs justify their cross-platform semantics.
- An **Actor** identity is scoped by adapter and platform tenant, not only by the platform's raw user id.
- **Bot Kind** represents unknown bot status explicitly rather than with a nullable boolean.
- A **Self Message** is ignored before subscription, mention, or pattern routing.
- A **Go Chat Runtime** exposes **Actor** metadata but does not own **Application Identity** linking or login workflows.
- **Thread ID** and **Actor** identities include **Platform Tenant** context.
- The **Slack-First Slice** ships as a **Single-Install Adapter** while preserving tenant-correct identifiers.
- A **Platform Adapter** may decode only **Supported Platform Shapes**, while preserving raw payload data and tolerating unrelated platform fields.
- Supported Slack payloads are decoded with local structs, permissive unknown-field handling, and explicit validation of required fields.

## Example dialogue

> **Dev:** "Should we copy the upstream TypeScript API exactly?"
> **Domain expert:** "No, this is a **Go Chat Runtime**; keep the same clean conversation model, but make the API idiomatic Go."

> **Dev:** "Can runtime construction be infallible if adapters validate credentials and bot identity?"
> **Domain expert:** "No, **Runtime Construction** is fallible and reports invalid configuration before webhooks are served."

> **Dev:** "If one adapter cleanup fails, should shutdown stop immediately?"
> **Domain expert:** "No, **Runtime Shutdown** attempts every cleanup and returns joined errors."

> **Dev:** "Should missing new-mention handlers make startup fail?"
> **Domain expert:** "No, **Handler Registration** is mutable; if no handler is set, that route is a no-op."

> **Dev:** "If an app calls a handler registration method twice, should both handlers run?"
> **Domain expert:** "No, **Handler Registration** atomically replaces the single **Routing Hook** handler."

> **Dev:** "Does single-handler registration match Vercel Chat SDK?"
> **Domain expert:** "No, this is an intentional Go MVP difference that must be documented in **Routing Hook** GoDoc."

> **Dev:** "Should the MVP support Slack, Discord, and Teams?"
> **Domain expert:** "No, ship a **Slack-First Slice** that proves the runtime against one real adapter before claiming portability."

> **Dev:** "A Slack mention arrived in a channel root message. Is the **Thread** the whole channel?"
> **Domain expert:** "No, the **Thread** is rooted at that message timestamp so subscription follows replies to that conversation."

> **Dev:** "A handler posts to that **Thread**. Should the bot create a new channel root message?"
> **Domain expert:** "No, posting to a **Thread** replies to that conversation."

> **Dev:** "An inbound Slack DM reaches the bot. Is that outside the **Thread** model?"
> **Domain expert:** "No, it is a **Direct Message Thread** and can use normal **Thread** routing and replies."

> **Dev:** "Does an inbound DM need an explicit @mention to start a conversation?"
> **Domain expert:** "No, an unsubscribed **Direct Message Thread** message is an implicit **New Mention**."

> **Dev:** "Can users build a Slack **Thread ID** from channel plus timestamp?"
> **Domain expert:** "No, the Slack adapter produces the **Thread ID** so workspace and versioning details are not lost."

> **Dev:** "Can a cron job post to a known thread later?"
> **Domain expert:** "Yes, use a **Thread Handle** reconstructed from a valid **Thread ID** through the runtime."

> **Dev:** "The bot was mentioned inside a **Subscribed Thread**. Should this call the new-conversation handler?"
> **Domain expert:** "No, the message belongs to the existing conversation; it is not a **New Mention**."

> **Dev:** "A user reacted inside a **Subscribed Thread**. Should the subscribed-message handler fire?"
> **Domain expert:** "No, subscription controls future message routing, not every event type in the conversation."

> **Dev:** "A user replies to a **Subscribed Thread** after the weekend. Should the subscription expire automatically?"
> **Domain expert:** "No, it remains subscribed until the application explicitly unsubscribes it."

> **Dev:** "Does replying successfully to a **New Mention** subscribe the **Thread**?"
> **Domain expert:** "No, the handler must explicitly subscribe when it wants future messages routed as an existing conversation."

> **Dev:** "Should the first slice route arbitrary channel messages by regex?"
> **Domain expert:** "No, defer **Pattern Handler** support until the conversation-state routing model is solid."

> **Dev:** "Should Slack slash commands be routed as normal **Messages**?"
> **Domain expert:** "No, a slash command is a **Command Event** with separate routing semantics, and it is deferred."

> **Dev:** "Should apps wrap dispatch with custom **Middleware** in the first slice?"
> **Domain expert:** "No, keep dispatch invariants explicit; use observer or logger hooks for visibility."

> **Dev:** "How do we know why a message did not reach a handler?"
> **Domain expert:** "Use **Runtime Observation** logs for ignored events, duplicates, lock conflicts, routing decisions, and handler errors."

> **Dev:** "Should the SDK start its own HTTP server?"
> **Domain expert:** "No, expose a **Webhook Handler** so applications can mount it in their existing server or router."

> **Dev:** "If the app asks for the Slack webhook with a misspelled adapter name, should production requests discover that?"
> **Domain expert:** "No, the **Webhook Mount** should fail immediately."

> **Dev:** "Should app code special-case Slack URL verification outside the runtime?"
> **Domain expert:** "No, Slack URL verification is a **Platform Handshake** owned by the Slack adapter."

> **Dev:** "Can the runtime create **Memory State** automatically if none is configured?"
> **Domain expert:** "No, **Runtime State** is required so production code makes subscriptions, dedupe, and locking explicit."

> **Dev:** "Should the runtime store product workflow data on the **Thread**?"
> **Domain expert:** "No, that is **Thread Application State** and belongs in the application's storage for the MVP."

> **Dev:** "Should handlers fetch prior platform messages through the runtime in the first slice?"
> **Domain expert:** "No, **Message History** is deferred; handlers receive the current **Message Event**."

> **Dev:** "Can the first deployable Slack bot rely only on **Memory State**?"
> **Domain expert:** "No, the MVP needs **Redis State** for cross-instance subscriptions, dedupe, and locks."

> **Dev:** "Two different Slack messages arrive in the same **Thread**. Are they duplicates?"
> **Domain expert:** "No, they are separate **Webhook Events**; dedupe keeps both, while the **Thread Lock** serializes their handlers."

> **Dev:** "Should Slack retry number be part of the dedupe key?"
> **Domain expert:** "No, use **Event Identity** for dedupe and record retry headers as **Retry Metadata**."

> **Dev:** "Should dedupe and lock timing each get separate top-level options?"
> **Domain expert:** "No, group coordination timing under **Runtime Options**."

> **Dev:** "How long should runtime dedupe and thread locks last by default?"
> **Domain expert:** "Use a 24 hour dedupe TTL and a 2 minute **Thread Lock** TTL, with explicit release when handlers finish."

> **Dev:** "Should the MVP implement every upstream concurrency strategy?"
> **Domain expert:** "No, start with the drop **Concurrency Strategy** and keep the option shape compatible with future strategies."

> **Dev:** "A Slack event hits a **Lock Conflict**. Should Slack retry it?"
> **Domain expert:** "No, the runtime should acknowledge the event by default and record that it was not handled because the **Thread** was busy."

> **Dev:** "If an old handler releases a lock after it expired and another handler acquired it, should the release delete the new lock?"
> **Domain expert:** "No, a **Lock Lease** is token-owned; release and extension only affect the current owner's token."

> **Dev:** "Should the Slack adapter call user handlers directly?"
> **Domain expert:** "No, it should normalize the **Webhook Event** and hand it to **Runtime Dispatch**."

> **Dev:** "Should long-running work keep using the webhook request's **Dispatch Context** after the platform times out?"
> **Domain expert:** "No, long-running work needs an explicit app-owned detach or queue path; synchronous dispatch uses the request context."

> **Dev:** "If the **Dispatch Context** is cancelled, should `Subscribe` secretly continue in the background?"
> **Domain expert:** "No, runtime state mutations respect the caller's context and fail when it is cancelled."

> **Dev:** "A handler posts once and then returns an error. Should the platform retry the **Accepted Event** by default?"
> **Domain expert:** "No, handler failure is recorded, but the **Accepted Event** remains acknowledged by default."

> **Dev:** "Slack sends a valid event type the **Slack-First Slice** does not support yet. Should the platform retry it?"
> **Domain expert:** "No, treat it as an **Ignored Event** and acknowledge it."

> **Dev:** "Can app code parse raw Slack payloads with the adapter and dispatch handlers itself?"
> **Domain expert:** "No, that bypasses runtime guarantees; the **Platform Adapter** should enter through the **Go Chat Runtime**."

> **Dev:** "Can the Slack adapter discover bot identity after the first self-message arrives?"
> **Domain expert:** "No, **Adapter Initialization** must establish bot identity before webhook handling starts."

> **Dev:** "Should a Slack button click be represented as a **Message**?"
> **Domain expert:** "No, it should be an **Event** with its own payload shape, even if it belongs to a **Thread**."

> **Dev:** "Should message handlers receive only a **Thread** and **Message**?"
> **Domain expert:** "No, use a **Message Event** so event metadata is still available without making handlers unpack raw events."

> **Dev:** "Should app code read Slack team id by type-asserting the adapter's internal parser struct?"
> **Domain expert:** "No, promote stable metadata or provide a **Platform Escape Hatch** that does not expose parser internals as the contract."

> **Dev:** "How should app code call a Slack-specific API?"
> **Domain expert:** "Use **Adapter Access** to retrieve the registered Slack adapter through a typed helper."

> **Dev:** "Should the first **Postable Message** model include Slack Block Kit and Teams Adaptive Cards?"
> **Domain expert:** "No, start with text and markdown so adapters can prove the common posting path before rich content is modeled."

> **Dev:** "Should MVP adapters support editing, deleting, and reacting to messages?"
> **Domain expert:** "No, return a **Sent Message** from posts, but defer **Outbound Mutation**."

> **Dev:** "Can login prompts or private nudges always use normal **Thread** replies?"
> **Domain expert:** "No, the **Slack-First Slice** needs **Ephemeral Message** delivery for private platform-visible prompts."

> **Dev:** "If an adapter cannot send an **Ephemeral Message**, should the runtime fall back to a public reply?"
> **Domain expert:** "No, it should either use explicit **Ephemeral Fallback** to a DM or return no delivered message."

> **Dev:** "Can an **Ephemeral Message** fall back to a DM?"
> **Domain expert:** "Yes, but only through explicit **Ephemeral Fallback** selected by the caller."

> **Dev:** "Should adapters declare **Optional Capabilities** through string flags?"
> **Domain expert:** "No, use narrow Go interfaces so support is part of the adapter's type contract."

> **Dev:** "Does **Portable Markdown** mean Slack mrkdwn?"
> **Domain expert:** "No, **Portable Markdown** is conservative CommonMark input that adapters may translate or degrade."

> **Dev:** "Should the Slack adapter convert **Portable Markdown** into Slack mrkdwn?"
> **Domain expert:** "No, use Slack's markdown-native posting field for **Portable Markdown** where the target method supports it. Do not own a CommonMark-to-mrkdwn converter unless a fallback becomes necessary."

> **Dev:** "Should **Plain Text** disable Slack URL linking?"
> **Domain expert:** "No. **Plain Text** means no formatting intent, so disable Slack markdown parsing, but keep benign URL linking unless it proves surprising."

> **Dev:** "Can **Portable Markdown** trigger Slack-specific entities like `@here`, `<@U123>`, `<#C123>`, or Slack date tokens?"
> **Domain expert:** "No. **Portable Markdown** is presentation, not **Platform Control Syntax**. Mentions, channel references, broad notifications, and platform date tokens must be explicit constructs or platform escape hatches."

> **Dev:** "Should mentions, channel references, broad notifications, or date tokens be normalized now?"
> **Domain expert:** "No. Keep them out of the normalized MVP until real app needs justify their cross-platform semantics; use **Platform Escape Hatch** for platform-native control syntax in the meantime."

> **Dev:** "What CommonMark features does **Portable Markdown** promise in the MVP?"
> **Domain expert:** "Only the **Portable Markdown Subset**: paragraphs, line breaks, emphasis, strong emphasis, inline code, fenced code blocks, block quotes, links, unordered lists, and ordered lists. Strikethrough may be rendered when supported, but can degrade literally. Rich layout and extended Markdown features stay out of scope."

> **Dev:** "If Slack `markdown_text` rejects or poorly renders **Portable Markdown**, should the adapter retry through classic mrkdwn conversion?"
> **Domain expert:** "No. Accepted-but-less-rich rendering is normal degradation. Rejected posts return the platform error. Do not add automatic CommonMark-to-mrkdwn retry unless we explicitly decide to own that converter later."

> **Dev:** "The Slack bot's own reply arrived as an inbound **Message**. Should it route to subscribed-message handlers?"
> **Domain expert:** "No, it is a **Self Message** and must be ignored before handler routing."

> **Dev:** "How should an unknown bot-vs-human author be represented?"
> **Domain expert:** "Use **Bot Kind** so unknown, human, and bot are explicit states."

> **Dev:** "A Slack **Actor** has not linked their product account. Should the runtime park and resume the request?"
> **Domain expert:** "No, that belongs to **Application Identity**; the app handler may post a login prompt and return."

> **Dev:** "Can the first Slack adapter ignore workspace id because it uses one bot token?"
> **Domain expert:** "No, it can be a **Single-Install Adapter**, but **Thread ID** and **Actor** identities still need **Platform Tenant** context."

> **Dev:** "Should the Slack adapter reject a valid event because Slack added an unknown field?"
> **Domain expert:** "No, it should decode the **Supported Platform Shape** it needs and tolerate unrelated fields."

## Flagged ambiguities

- "Reimplement Vercel's Chat SDK in Go" could mean API parity, feature parity, or a Go-native runtime; resolved: the goal is a **Go Chat Runtime** with **Semantic Compatibility**.
- "Constructor" could mean allocation only or startup validation; resolved: **Runtime Construction** is fallible validation and assembly.
- "Shutdown" could mean best-effort logging or explicit cleanup result; resolved: **Runtime Shutdown** attempts all cleanup and returns joined errors.
- "Handler registration" could mean required construction-time configuration or mutable callbacks; resolved: **Handler Registration** uses runtime methods and missing handlers are no-ops.
- "Handler hook" could mean a chain or multiple callbacks; resolved: a **Routing Hook** has one atomically replaceable handler, intentionally differing from upstream.
- "Thread" could mean a platform-native thread id, a channel, or a normalized conversation address; resolved: **Thread** means the runtime's adapter-scoped conversation address.
- "Direct message" could mean an existing private conversation, a separate direct-message handler, or proactive DM opening; resolved: inbound **Direct Message Thread** messages route as implicit **New Mentions**, while proactive opening and a dedicated direct-message hook are deferred.
- "Thread ID" could mean a raw platform identifier or a runtime identifier; resolved: **Thread ID** is opaque, adapter-produced, and stable for storage.
- "Thread handle" could mean user construction or runtime reconstruction; resolved: **Thread Handle** is reconstructed by the runtime from a validated **Thread ID**.
- "Mention" could mean any platform mention or a new-conversation entrypoint; resolved: **New Mention** means a mention in an unsubscribed **Thread**.
- "Subscribe" could mean an automatic effect of handling a mention or an explicit application choice; resolved: subscribing is explicit.
- "Pattern handler" could mean a primary routing primitive or a later routing extension; resolved: **Pattern Handler** is deferred from the first slice.
- "Slash command" could mean a message, pattern match, or command invocation; resolved: slash commands are deferred **Command Events**.
- "Middleware" could mean observability or user-controlled dispatch mutation; resolved: **Middleware** is dispatch mutation and is deferred.
- "Observability" could mean middleware, metrics, or logs; resolved: **Runtime Observation** starts as structured logging.
- "HTTP integration" could mean a built-in server or a mountable handler; resolved: the runtime exposes **Webhook Handlers** only.
- "Webhook lookup" could mean dynamic runtime routing or startup mount validation; resolved: **Webhook Mount** is fallible.
- "Platform handshake" could mean a user event or adapter setup request; resolved: **Platform Handshake** is adapter-owned and does not dispatch.
- "State" could mean optional app convenience storage or required runtime coordination; resolved: **Runtime State** is required, while **Memory State** is explicitly dev/test only.
- "Thread state" could mean runtime coordination or product data; resolved: **Thread Application State** is outside the MVP runtime state.
- "Message history" could mean current webhook context or platform backfill; resolved: **Message History** is deferred from the MVP.
- "Redis" could mean an optional cache layer; resolved: **Redis State** is the first production runtime state implementation.
- "Event identity" could mean delivery attempt or platform event; resolved: **Event Identity** excludes **Retry Metadata**.
- "Runtime options" could mean many unrelated one-off settings; resolved: **Runtime Options** groups coordination timing such as dedupe and lock TTLs.
- "Concurrency" could mean a full queueing system or a lock-conflict policy; resolved: **Concurrency Strategy** starts with drop only.
- "Locking" could mean event dedupe, adapter-wide exclusion, or handler coordination; resolved: dedupe applies to **Webhook Events**, while **Thread Lock** applies per **Thread**.
- "Lock release" could mean deleting a key by thread id or releasing an owned lease; resolved: **Lock Lease** release and extension are token-checked.
- "Lock conflict" could mean a duplicate event or handler failure; resolved: **Lock Conflict** is accepted-but-unhandled thread contention.
- "Dispatch" could mean adapter-specific webhook handling or runtime handler routing; resolved: **Runtime Dispatch** is owned by the **Go Chat Runtime**.
- "Context" could mean webhook request lifetime or background job lifetime; resolved: **Dispatch Context** is the synchronous request context.
- "Accepted event" could mean successful handler execution; resolved: **Accepted Event** means verified and normalized event ownership by the runtime.
- "Ignored event" could mean invalid input or irrelevant valid input; resolved: **Ignored Event** is valid platform input acknowledged without dispatch.
- "Adapter" could mean a full mini-framework or a platform boundary; resolved: **Platform Adapter** does platform conversion and transport, not application routing.
- "Adapter initialization" could mean lazy setup or startup validation; resolved: **Adapter Initialization** is fail-fast startup validation plus required runtime fact discovery.
- "Message" could mean any inbound platform occurrence; resolved: **Event** is the routing envelope and **Message** is only one payload type.
- "Message handler input" could mean raw **Event** or a tuple; resolved: message hooks receive a **Message Event**.
- "Raw payload" could mean normal app API or rare escape hatch; resolved: platform-specific data is a **Platform Escape Hatch**, while common flows use normalized fields.
- "Adapter access" could mean unchecked assertions or a supported escape path; resolved: **Adapter Access** uses a typed helper.
- "Post message" could mean a normalized body, a card DSL, or native platform payload; resolved: **Postable Message** starts as normalized text and markdown only.
- "Sent message" could imply edit/delete/reaction support; resolved: **Sent Message** only records successful post identity in the MVP.
- "Outbound mutation" could be part of core posting or optional operations; resolved: **Outbound Mutation** is deferred.
- "Ephemeral message" could mean a normal reply with visibility metadata or an adapter escape hatch; resolved: **Ephemeral Message** is a core optional capability with explicit **Ephemeral Fallback** behavior.
- "Capability" could mean a string registry or an interface contract; resolved: **Optional Capability** is interface-based.
- "Markdown" could mean CommonMark input, fuzzy formatting intent, Slack mrkdwn, platform control syntax, or GitHub-flavored markdown; resolved: **Portable Markdown** is conservative CommonMark input, not native syntax. When a platform provides a markdown-native posting field, prefer that over translating into the platform's markdown dialect. Do not use **Portable Markdown** as a hidden **Platform Control Syntax** channel.
- "User" could mean a platform-local id, global identity, or bot identity; resolved: **Actor** is scoped by adapter and tenant, **Bot Kind** models human/bot/unknown, and **Self Message** covers bot-authored inbound messages.
- "Account linking" could mean platform actor metadata or product authentication; resolved: **Application Identity** stays outside the core runtime.
- "Workspace" or "tenant" could mean product account, Slack workspace, or adapter install; resolved: **Platform Tenant** means the adapter's platform-scoped installation context.
- "Slack payload parsing" could mean full schema ownership or supported-shape decoding; resolved: adapters decode **Supported Platform Shapes** and preserve escape hatches.
