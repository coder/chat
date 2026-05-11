# Go Chat Runtime MVP

Status: ready-for-agent

## Problem Statement

The user has a Go application that cannot be rewritten in TypeScript, but they still want the relaxed, clean conversation model of Vercel Chat SDK: platform adapters, normalized chat events, thread-oriented replies, subscription-based routing, and state-backed coordination.

The problem is not that Go lacks Slack libraries. The problem is that Go lacks a small, idiomatic **Go Chat Runtime** that captures the useful upstream model without copying TypeScript APIs, JSX cards, or every platform-specific feature. The SDK needs to be production-shaped enough for a horizontally scaled Slack bot while staying narrow enough to prove the core abstraction.

## Solution

Build a **Go Chat Runtime** with **Semantic Compatibility** to Vercel Chat SDK's core conversation model. The MVP is a **Slack-First Slice** that proves:

- fail-fast runtime construction and shutdown
- required **Runtime State**
- **Memory State** for tests and local development
- **Redis State** for horizontally scaled deployments
- Slack webhook verification, URL verification, dedupe, retries, and self-message filtering
- normalized **Event**, **Message**, **Message Event**, **Actor**, **Thread**, and **Thread ID**
- explicit subscription routing with `OnNewMention` and `OnSubscribedMessage`
- stable **Thread Handle** reconstruction from validated **Thread ID**
- **Thread** posting with **Plain Text** and **Portable Markdown**
- **Ephemeral Message** delivery with explicit **Ephemeral Fallback**
- structured **Runtime Observation**
- platform-specific escape paths through typed **Adapter Access**

This is not a feature-parity port of Vercel Chat SDK. The project should copy upstream behavior by default when it maps cleanly to Go, and intentionally diverge where Go idioms, MVP scope, or runtime correctness make that better.

## User Stories

1. As a Go application developer, I want to create a **Go Chat Runtime**, so that I can build chat bots without rewriting my app in TypeScript.
2. As a Go application developer, I want **Semantic Compatibility** with Vercel Chat SDK's core model, so that the mental model transfers cleanly.
3. As a Go application developer, I want runtime construction to fail fast, so that invalid Slack or state configuration is found before serving webhooks.
4. As a Go application developer, I want runtime shutdown to attempt all cleanup, so that adapters and state clients release resources predictably.
5. As a Go application developer, I want to mount a **Webhook Handler** into my existing HTTP server, so that the SDK does not own my server lifecycle.
6. As a Go application developer, I want **Webhook Mount** to fail for unknown adapter names, so that typos are found at startup.
7. As a Slack bot developer, I want Slack URL verification handled by the adapter, so that app code does not need Slack-specific handshake boilerplate.
8. As a Slack bot developer, I want Slack signatures verified before dispatch, so that unauthenticated requests are rejected.
9. As a Slack bot developer, I want unsupported valid Slack events acknowledged and ignored, so that Slack does not retry irrelevant events forever.
10. As a Slack bot developer, I want Slack retry headers recorded as **Retry Metadata**, so that I can debug platform delivery behavior.
11. As a runtime operator, I want dedupe keyed by **Event Identity**, so that platform retries do not run handlers twice.
12. As a runtime operator, I want a 24 hour dedupe TTL by default, so that normal retry windows are covered without storing keys forever.
13. As a runtime operator, I want **Thread Lock** coordination, so that two messages in the same conversation do not mutate state concurrently.
14. As a runtime operator, I want **Lock Lease** ownership tokens, so that an expired lock holder cannot release a newer lock.
15. As a runtime operator, I want a drop **Concurrency Strategy** by default, so that lock conflicts do not create platform retry storms.
16. As a runtime operator, I want lock conflicts acknowledged and logged, so that the platform is not asked to retry accepted contention.
17. As a runtime operator, I want structured **Runtime Observation**, so that ignored events, duplicates, lock conflicts, routing, and handler failures are explainable.
18. As a Go application developer, I want required **Runtime State**, so that subscriptions, dedupe, and locks are explicit production concerns.
19. As a Go application developer, I want **Memory State**, so that tests and local examples stay simple.
20. As a production deployer, I want **Redis State**, so that multiple app instances share subscriptions, dedupe, and locks.
21. As an adapter author, I want a clear **Platform Adapter** boundary, so that platform verification and rendering do not own runtime dispatch.
22. As an adapter author, I want adapters to normalize **Webhook Events** into **Events**, so that the runtime can dedupe, lock, and route platform input consistently.
23. As an adapter author, I want to preserve raw platform data as a **Platform Escape Hatch**, so that uncommon platform-specific cases remain possible.
24. As a Slack adapter author, I want to decode only **Supported Platform Shapes**, so that the adapter remains robust when Slack adds unrelated fields.
25. As a bot developer, I want a stable opaque **Thread ID**, so that I can store thread references without depending on raw Slack identifiers.
26. As a bot developer, I want **Thread IDs** to include adapter and **Platform Tenant** context, so that workspace and platform collisions are impossible.
27. As a bot developer, I want to reconstruct a **Thread Handle** from a valid **Thread ID**, so that cron jobs and workflows can post later.
28. As a bot developer, I want `Thread.Post` to reply to the conversation address, so that responses stay in the right thread.
29. As a Slack bot developer, I want a root channel mention to create a thread rooted at that message timestamp, so that future replies stay scoped to that conversation.
30. As a Slack bot developer, I want inbound direct messages to be **Direct Message Threads**, so that DMs use the same routing and posting model.
31. As a Slack bot developer, I want unsubscribed direct-message input to behave as an implicit **New Mention**, so that users can start a conversation in DM without an explicit mention.
32. As a bot developer, I want `OnNewMention`, so that I can handle new conversations.
33. As a bot developer, I want `OnSubscribedMessage`, so that I can handle future messages in subscribed conversations.
34. As a bot developer, I want missing **Routing Hooks** to be no-ops, so that I can implement only the flows I need.
35. As a bot developer, I want **Handler Registration** methods, so that handlers can be attached naturally after runtime construction.
36. As a bot developer, I want handler registration to atomically replace one handler per hook, so that behavior is deterministic and race-safe.
37. As a bot developer, I want **Routing Hook** GoDoc to call out the single-handler difference from Vercel Chat SDK, so that upstream users are not surprised.
38. As a bot developer, I want **Message Event** handler inputs, so that handlers get **Event**, **Thread**, and **Message** context without unpacking raw events.
39. As a bot developer, I want explicit `Thread.Subscribe`, so that replying to a new mention does not accidentally subscribe forever.
40. As a bot developer, I want subscriptions to last until explicit unsubscribe, so that conversations do not silently stop after a TTL.
41. As a bot developer, I want **Self Message** filtering before routing, so that bot replies do not trigger bot loops.
42. As a bot developer, I want **Actor** metadata with **Bot Kind**, so that human, bot, and unknown author states are explicit.
43. As a bot developer, I want **Plain Text** posting, so that simple replies are easy.
44. As a bot developer, I want **Portable Markdown** posting, so that common formatting intent can be translated or degraded by adapters.
45. As a bot developer, I want **Sent Message** identity returned from posts, so that future operations can target messages when supported later.
46. As a Slack bot developer, I want **Ephemeral Message** delivery, so that private prompts and nudges do not leak into public threads.
47. As a Slack bot developer, I want explicit **Ephemeral Fallback**, so that I can choose DM fallback or no delivered message when native ephemeral delivery is unavailable.
48. As a Go application developer, I want typed **Adapter Access**, so that platform-specific APIs are reachable without unchecked type assertions in examples.
49. As a Go application developer, I want **Application Identity** to stay outside the runtime, so that my product auth and account-linking flows remain app-owned.
50. As a Go application developer, I want **Thread Application State** out of the MVP, so that product state remains in my own database keyed by **Thread ID**.
51. As a Go application developer, I want **Message History** out of the MVP, so that platform backfill and app context storage do not complicate the core runtime.
52. As a future adapter author, I want **Optional Capability** support through narrow Go interfaces, so that adapters can grow without a string capability registry.
53. As a future maintainer, I want deferred features documented, so that semantic gaps from Vercel Chat SDK are intentional and discoverable.

## Implementation Decisions

- Publish the module as `github.com/coder/chat`.
- Put the core `chat` package at the repository root. It owns the **Go Chat Runtime**, core types, errors, **Runtime Options**, **Runtime Dispatch**, **Handler Registration**, **Thread Handle**, and **Webhook Mount** APIs.
- Keep platform adapters and state implementations as subpackages under the root module, starting with `adapters/slack`, `state/memory`, and `state/redis`.
- Build a Slack **Platform Adapter** as the first production-shaped adapter. It owns Slack signature verification, Slack URL verification, local supported-shape decoding, Slack event normalization, Slack posting, Slack ephemeral delivery, bot identity discovery, and Slack-specific rendering.
- Build `state/memory` for tests, examples, and local development. It must implement the same **Runtime State** contract as production state.
- Build `state/redis` as the first production **Runtime State** implementation. It must support durable subscriptions, event dedupe, token-owned **Lock Leases**, lock extension, lock release, and shutdown.
- Add a shared state conformance test suite. **Memory State** and **Redis State** must pass the same externally visible behavior tests.
- Runtime construction is fallible. It validates required state, unique adapter names, positive **Runtime Options** TTLs, and adapter initialization facts before webhooks are served.
- Runtime shutdown is idempotent. It attempts all adapter shutdown hooks before state shutdown and returns joined cleanup errors.
- Handler registration is mutable and method-based. `OnNewMention` and `OnSubscribedMessage` install or atomically replace one handler per **Routing Hook**. Missing handlers are no-ops.
- GoDoc for handler registration must explicitly document that this intentionally differs from Vercel Chat SDK's multiple-handler registration.
- The runtime exposes fallible **Webhook Mount** lookup. Unknown adapter names fail during mount rather than producing runtime 404 behavior.
- **Webhook Handlers** integrate through `net/http`. The SDK does not own servers, routers, TLS, graceful shutdown, or framework adapters.
- **Runtime Dispatch** is synchronous in the MVP and uses the inbound request's **Dispatch Context**. Long-running work must be explicitly detached or queued by application code.
- Runtime locks are released when handler execution exits or context cancellation unwinds the handler path.
- Runtime state mutations respect the caller's context. The SDK does not secretly switch to a background context for subscription or lock operations.
- Accepted handler errors are observed but acknowledged to the platform by default. Verification failures and malformed requests are rejected.
- Valid but irrelevant, unsupported, or self-authored platform events become **Ignored Events** and are acknowledged without runtime dispatch.
- **Event Identity** is the dedupe key. Slack retry headers are **Retry Metadata**, not part of dedupe identity.
- Default **Runtime Options** use a 24 hour dedupe TTL, a 2 minute **Thread Lock** TTL, and drop **Concurrency Strategy**.
- The MVP only implements the drop **Concurrency Strategy**, while preserving option naming that can later grow toward queue, debounce, force, or concurrent strategies.
- **Lock Leases** include ownership tokens. Redis release and extension must compare token ownership before mutating a lock.
- **Thread IDs** are opaque stable strings produced by adapters. They include adapter identity and enough platform routing context to avoid collisions across platform tenants, channels, and platforms.
- Application code may pass around **Thread IDs**, but only adapters construct and decode them.
- The runtime provides a helper to reconstruct a **Thread Handle** from a validated **Thread ID** for out-of-webhook posting or subscription.
- A Slack root message without a platform thread timestamp normalizes to a **Thread** rooted at that message timestamp, not the whole channel.
- Posting to a **Thread** replies to that conversation address and does not create a new root channel message by default.
- A **Subscribed Thread** routes message-created **Events** to subscribed-message handlers before mention routing.
- **New Mention** only exists when the mentioned **Thread** is not subscribed.
- A **New Mention** handler must explicitly subscribe the **Thread**. Successful handler completion never auto-subscribes.
- Subscriptions remain active until explicit unsubscribe.
- Inbound **Direct Message Thread** messages route as implicit **New Mentions** when unsubscribed, and as subscribed messages after subscription.
- The MVP does not include a dedicated direct-message routing hook.
- The MVP does not include proactive public `OpenDM`, except as an adapter capability needed to implement explicit **Ephemeral Fallback**.
- Normalized inbound values use an **Event** envelope. **Message** is one event payload, not the name for every platform occurrence.
- Message routing hooks receive a **Message Event** that carries the normalized **Event**, **Thread**, and **Message**.
- **Actor** identity is scoped by adapter and **Platform Tenant**. Raw platform user IDs are not globally stable identities.
- **Bot Kind** uses an explicit unknown/human/bot classification rather than a nullable boolean.
- **Self Messages** are ignored before subscription, mention, or pattern routing.
- **Application Identity** linking, login prompts, pending auth flows, and account-link persistence are application-owned.
- **Platform Escape Hatch** support preserves raw or adapter-specific data for uncommon cases. Internal parser structs must not become the stable app contract by accident.
- **Adapter Access** is exposed through a typed helper so application code can reach platform-specific APIs deliberately.
- **Postable Message** starts with **Plain Text** and **Portable Markdown**. Slack native `mrkdwn`, GitHub-flavored markdown, rich cards, files, and platform-native payloads are not the normalized MVP surface.
- **Portable Markdown** is formatting intent that adapters may translate or degrade.
- Posting returns **Sent Message** identity, but edit, delete, and reaction operations are deferred.
- **Ephemeral Message** is a core optional capability implemented through narrow Go interfaces.
- **Ephemeral Fallback** is explicit. When native ephemeral delivery is unavailable, callers choose whether to fall back to a **Direct Message Thread** or receive no delivered message. There is never an implicit public reply fallback.
- Optional adapter behavior is modeled with small Go interfaces, not string capability flags.
- The Slack adapter is a **Single-Install Adapter** in the MVP. **Thread ID** and **Actor** identity still include **Platform Tenant** context so multi-workspace support is not blocked later.
- Slack payload parsing uses local structs for **Supported Platform Shapes**, permissive unknown-field handling, and explicit validation of required fields for supported event types.
- README and GoDoc must document intentional upstream differences: single-handler hooks, fail-fast construction, deferred dedicated direct-message hook, deferred proactive `OpenDM`, deferred thread application state, and deferred feature families.

## Testing Decisions

- Tests should assert external behavior and public contracts, not private implementation details.
- Runtime construction tests should cover missing state, duplicate adapter names, invalid **Runtime Options**, adapter initialization failures, and successful startup.
- Runtime shutdown tests should cover idempotency, attempt-all cleanup, adapter errors, state errors, and joined errors.
- Handler registration tests should cover unset no-op behavior, install behavior, replace-on-register behavior, and race-safe observation under concurrent dispatch.
- Routing tests should cover subscribed-message priority over new-mention routing, explicit subscription, unsubscribe, missing handlers, direct-message implicit mention routing, self-message filtering, and ignored event behavior.
- Dispatch tests should cover accepted handler errors, acknowledgement semantics, context cancellation, lock release on exit, and state mutation cancellation.
- Dedupe tests should cover duplicate **Event Identity** handling, retry metadata exclusion from dedupe keys, dedupe TTL, and accepted duplicate acknowledgement.
- Lock tests should cover acquire success, conflict/drop behavior, release, extend, TTL expiry, ownership-token compare-and-release, and stale holder release safety.
- State conformance tests should run against both **Memory State** and **Redis State**.
- Redis state tests should include real Redis behavior or a test double that supports the atomicity and Lua/compare-and-delete semantics required by token-owned **Lock Leases**.
- Slack adapter tests should use golden Slack-shaped payloads for URL verification, message-created events, mentions, direct messages, retries, unsupported valid events, self messages, malformed requests, and invalid signatures.
- Slack signature tests should cover timestamp handling, body signing, invalid signatures, and replay-window behavior if implemented.
- Slack normalization tests should cover **Thread ID** construction, **Platform Tenant** scoping, root-message thread rooting, direct-message thread normalization, **Actor** and **Bot Kind**, raw payload preservation, and required-field validation.
- Slack posting tests should mock platform API calls at the HTTP boundary and assert thread replies, **Plain Text**, **Portable Markdown**, **Sent Message**, **Ephemeral Message**, and **Ephemeral Fallback** behavior.
- Optional capability tests should cover present interface behavior, absent interface behavior, explicit unsupported behavior, and no public reply fallback for ephemeral messages.
- **Adapter Access** tests should cover registered adapter retrieval, wrong type, missing adapter, and no panic in normal helper examples.
- **Thread Handle** tests should cover valid reconstruction, unknown adapter, malformed **Thread ID**, and adapter validation failure.
- Documentation tests or review checks should confirm GoDoc/README mention intentional Vercel Chat SDK differences that affect user expectations.

## Out of Scope

- Full Vercel Chat SDK feature parity.
- TypeScript API compatibility.
- Multi-platform MVP.
- Multi-workspace Slack OAuth installation flow.
- Account linking, login orchestration, parked auth prompts, or **Application Identity** persistence.
- Dedicated `OnDirectMessage` hook.
- Public proactive `OpenDM`, except whatever adapter-internal behavior is required for explicit **Ephemeral Fallback**.
- Pattern handlers.
- Slash commands and other **Command Events**.
- Middleware.
- Message history APIs.
- Thread application state APIs.
- Rich cards, JSX-style cards, modals, files, platform-native payload builders, and cross-platform rich formatting.
- Edit, delete, reactions, and other **Outbound Mutation** operations.
- Queue, debounce, force, or concurrent **Concurrency Strategy** implementations.
- Built-in HTTP server, router integrations, TLS, or graceful-shutdown orchestration.
- Adapter marketplace/package conventions.
- Additional adapters beyond Slack.

## Further Notes

- Use Vercel Chat SDK as behavioral precedent by default. Diverge only when the upstream shape is non-idiomatic in Go, conflicts with MVP scope, or weakens runtime correctness.
- The strongest deep modules are the core **Go Chat Runtime**, the **Runtime State** contract with conformance tests, the Redis lock/dedupe implementation, the Slack **Platform Adapter**, the **Thread ID** codec/validator, and the **Postable Message** renderer.
- `CONTEXT.md` is the source of domain vocabulary. Implementation issues should use these terms rather than drifting to avoided synonyms.
- The README already contains compatibility notes and should remain the user-facing summary of intentional upstream gaps.
