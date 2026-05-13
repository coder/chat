# Linear App-Actor Slice PRD

Status: ready-for-agent

## Problem Statement

The user wants Chat SDK Go to support a native Linear integration where the bot participates as a Linear app-owned actor in Linear agent sessions. They do not want a normal user bot, a personal API key integration, or a generic issue-comment integration as the first Linear slice.

Today, Chat SDK Go has a Slack-first production-shaped slice with normalized **Events**, **Messages**, **Threads**, **Thread IDs**, explicit subscription routing, required **Runtime State**, and thread-scoped posting. There is no Linear **Platform Adapter**, no Linear app-actor webhook handling, and no Linear example that proves real app-agent setup and behavior.

The desired outcome is a minimal, Go-shaped **Linear App-Actor Slice** that follows upstream Chat SDK Linear semantics where they fit, while respecting the existing Go runtime and Slack adapter patterns.

## Solution

Build a Linear **Platform Adapter** that supports app-owned Linear agent sessions as the first Linear vertical slice.

The adapter will use single-install **App-Actor Client Credentials**, verify Linear webhooks, accept Linear **Agent Session Events**, normalize created and prompted agent-session input into runtime **Events**, post final replies as **Agent Activity Responses**, and expose a narrow Linear-specific **Agent Activity Thought** escape hatch through typed **Adapter Access**.

The slice will include one memory-backed hello-world example with setup instructions. The example will prove the full conversation path: app mention creates a Linear agent session, the runtime routes it to `OnNewMention`, the handler explicitly subscribes the **Linear Agent Session Thread**, the adapter posts a best-effort ephemeral thought, the runtime posts a final response, and a follow-up Linear prompt routes to `OnSubscribedMessage`.

This PRD follows the accepted ADR: **Linear App-Actor Slice Before Full Linear Adapter**.

## User Stories

1. As a Go application developer, I want to register a Linear **Platform Adapter**, so that my Go runtime can receive Linear app-agent input.
2. As a Go application developer, I want Linear support to preserve the existing **Go Chat Runtime** model, so that my Slack-oriented runtime knowledge transfers.
3. As a Linear app developer, I want the bot to act as an app-owned actor, so that Linear shows agent activity as coming from the app rather than a human user.
4. As a Linear app developer, I want **App-Actor Client Credentials** to be the owned authentication path, so that setup is explicitly app-actor and not personal-user based.
5. As a Linear app developer, I want client credentials modeled separately from future multi-tenant OAuth credentials, so that the MVP API does not create future naming confusion.
6. As a Linear app developer, I want adapter construction to validate required configuration, so that missing webhook secrets or client credentials fail before serving webhooks.
7. As a Linear app developer, I want adapter initialization to exchange client credentials, so that the adapter can call Linear as the app actor.
8. As a Linear app developer, I want adapter initialization to discover organization identity, so that **Thread IDs** and **Actors** are tenant-correct.
9. As a Linear app developer, I want adapter initialization to discover app user identity, so that runtime **Self Message** filtering can prevent bot loops.
10. As a Linear app developer, I want token refresh to happen lazily before Linear API calls, so that long-running examples do not fail after token expiry.
11. As a runtime operator, I want token refresh to avoid background goroutines, so that adapter lifecycle remains simple in the MVP.
12. As a runtime operator, I want single-install access tokens cached in adapter memory, so that **Runtime State** remains coordination state rather than credential storage.
13. As a security-conscious operator, I want Linear webhook signatures verified, so that unauthenticated requests are rejected.
14. As a security-conscious operator, I want stale Linear webhook timestamps rejected, so that replayed requests are not accepted.
15. As a Linear app developer, I want invalid JSON rejected, so that malformed transport input is surfaced clearly.
16. As a Linear app developer, I want unsupported but valid Linear webhook types acknowledged and ignored, so that Linear does not retry events outside the MVP.
17. As a Linear app developer, I want unbuildable agent-session payloads acknowledged and ignored after verification, so that unexpected Linear shape gaps do not cause retry storms.
18. As a Linear app developer, I want agent-session events for another app actor ignored when provable, so that shared webhook configurations do not trigger the wrong bot.
19. As a Linear app developer, I want `AgentSessionEvent` session creation normalized as a mentioned **Message**, so that `OnNewMention` starts the conversation.
20. As a Linear app developer, I want `AgentSessionEvent` prompting normalized as a mentioned **Message**, so that subscription state determines whether it is a new mention or subscribed follow-up.
21. As a runtime operator, I want Linear **Event Identity** to use source comment identity, so that dedupe follows upstream Chat SDK message semantics.
22. As a runtime operator, I want Linear retries deduped by logical user message, so that webhook delivery details do not run handlers twice.
23. As a bot developer, I want a Linear **Thread ID** that is opaque, so that my app does not depend on raw Linear identifiers.
24. As a bot developer, I want Linear **Thread IDs** to include organization identity, so that cross-tenant collisions are avoided.
25. As a bot developer, I want Linear **Thread IDs** to include issue identity, so that the conversation remains connected to the Linear issue context.
26. As a bot developer, I want Linear **Thread IDs** to include optional comment context, so that comment-scoped agent sessions can be represented later.
27. As a bot developer, I want Linear **Thread IDs** to include agent session identity, so that posts target the correct Linear agent session.
28. As a bot developer, I want malformed Linear **Thread IDs** rejected, so that invalid stored references fail safely.
29. As a bot developer, I want non-agent-session Linear **Thread IDs** rejected in the MVP, so that unsupported generic comment posting is not implied.
30. As a bot developer, I want to reconstruct a **Thread Handle** from a stored Linear **Thread ID**, so that out-of-webhook workflows can post later.
31. As a bot developer, I want `Thread.Post` to create a Linear **Agent Activity Response**, so that final answers appear in the native Linear agent-session UI.
32. As a bot developer, I want `Thread.Post` to accept **Plain Text**, so that simple final responses are easy.
33. As a bot developer, I want `Thread.Post` to accept **Portable Markdown**, so that common formatting intent can be sent to Linear.
34. As a bot developer, I want Linear text and markdown bodies passed through in MVP, so that no premature Markdown conversion layer is required.
35. As a bot developer, I want `Thread.Post` to return a **Sent Message**, so that created Linear activity identity is available when needed.
36. As a Linear app developer, I want a Linear-specific `PostThought` method, so that my app can acknowledge work before a final response.
37. As a Linear app developer, I want `PostThought` to create an ephemeral **Agent Activity Thought**, so that transient thinking output does not become the final answer.
38. As a Linear app developer, I want `PostThought` to return a **Sent Message**, so that thought activity identity can be observed in tests or app logs.
39. As a Go application developer, I want Linear-specific thought posting through **Adapter Access**, so that the core runtime does not grow a premature typing abstraction.
40. As a Go application developer, I want no public raw Linear API client in MVP, so that the adapter exposes deliberate narrow behavior like the Slack adapter.
41. As an adapter maintainer, I want low-level Linear API calls hidden behind deep modules, so that token exchange, GraphQL calls, and agent activity creation are testable without exposing unstable internals.
42. As an adapter maintainer, I want local supported-shape structs, so that the adapter controls which Linear webhook and API fields are part of the MVP contract.
43. As an adapter maintainer, I want no Linear SDK dependency in MVP, so that dependency footprint and public surface stay minimal.
44. As an adapter maintainer, I want an injectable HTTP client, so that tests can use fake Linear API servers like the Slack adapter does.
45. As a Linear app developer, I want the adapter to populate **Actor** names when Linear provides them, so that logs and app handlers can be more readable.
46. As a Linear app developer, I want routing identity to remain based on tenant-scoped actor IDs, so that display-name changes do not affect correctness.
47. As a bot developer, I want `OnNewMention` to explicitly subscribe the Linear thread in the example, so that the example teaches Go runtime subscription semantics.
48. As a bot developer, I want follow-up prompts to route to `OnSubscribedMessage` after subscription, so that continuing Linear agent sessions work like other subscribed threads.
49. As a bot developer, I want self-authored Linear messages filtered by the runtime, so that bot responses do not create loops.
50. As a local dogfooter, I want a memory-backed Linear hello-world example, so that I can prove the adapter without setting up Redis or Postgres.
51. As a local dogfooter, I want setup instructions for creating a Linear OAuth app, so that I can configure the platform correctly.
52. As a local dogfooter, I want setup instructions for installing with `actor=app`, `app:mentionable`, and `app:assignable`, so that the app appears as a mentionable and assignable Linear agent.
53. As a local dogfooter, I want setup instructions for client-credentials environment variables, so that I can run the example locally.
54. As a local dogfooter, I want setup instructions for enabling only agent session webhooks, so that unsupported comments, issues, and reactions do not confuse the MVP.
55. As a local dogfooter, I want setup instructions for exposing the webhook endpoint through HTTPS, so that Linear can deliver webhooks to my local process.
56. As a reviewer, I want dogfooding screenshots and video, so that I can verify the real Linear app-agent behavior without reproducing every setup step.
57. As a future maintainer, I want the MVP non-goals documented, so that missing generic Linear features are understood as deliberate gaps.
58. As a future maintainer, I want the Vercel divergences documented, so that Go-shaped choices are easy to defend and revisit.
59. As a future adapter author, I want this slice to follow existing Slack adapter patterns, so that adding new adapters stays consistent.
60. As a production deployer, I want documentation to say memory state is local-only, so that production users choose Redis or Postgres **Runtime State** separately.

## Implementation Decisions

- Build a Linear **Platform Adapter** as a second platform slice after the Slack-first runtime has proven the core model.
- Keep the adapter under the normal Linear platform identity and adapter name, even though the first supported shape is app-actor agent sessions only.
- The public adapter constructor follows the Slack adapter style: context-aware construction, an options struct, injectable HTTP client, injectable clock for tests, and optional logger.
- Model **App-Actor Client Credentials** as a nested option with client ID, client secret, and optional scopes. This preserves top-level OAuth app credentials for a future multi-tenant installation design.
- Default client-credentials scopes include read, write, comment creation, issue creation, app mentionability, and app assignability.
- Do not expose static access-token auth, personal API key auth, or multi-tenant OAuth install storage in the MVP.
- During **Adapter Initialization**, exchange client credentials for an app-actor token and query Linear for organization and app user identity.
- Cache the client-credentials access token in adapter process memory. Refresh it lazily before Linear API calls when it is near expiry. Do not add token methods to **Runtime State**.
- Implement a small token/auth deep module that owns OAuth token exchange, expiry tracking, and refresh decisions behind a narrow adapter-internal interface.
- Implement a small Linear API deep module that owns direct HTTP/GraphQL calls for identity discovery and agent activity creation behind a narrow adapter-internal interface.
- Do not introduce a Linear SDK dependency in MVP. Follow the Slack adapter pattern of direct HTTP calls and local supported-shape structs.
- Do not expose a raw Linear client or generic GraphQL helper publicly. Platform-specific public behavior should be narrow and reachable through typed **Adapter Access**.
- Implement Linear webhook verification as an adapter-owned boundary. Verify method, signature, timestamp freshness, and JSON envelope before considering runtime dispatch.
- Reject transport/security/envelope failures. After successful verification, acknowledge and ignore unsupported or unbuildable Linear agent-session payloads.
- Accept Linear **Agent Session Event** actions that represent session creation and user prompting. Other Linear webhook types and actions are ignored in MVP.
- Normalize buildable session-creation events as mentioned **Messages**. The source comment ID is both the runtime message ID and the basis for **Event Identity**.
- Normalize buildable prompted events as mentioned **Messages**. The agent activity source comment ID is both the runtime message ID and the basis for **Event Identity**.
- Use source comment identity rather than webhook delivery identity for Linear dedupe, matching upstream Chat SDK Linear message semantics.
- Let **Runtime Dispatch** decide new-mention vs subscribed-message routing from subscription state. The adapter marks agent-session input as mentioned; it does not bypass runtime routing.
- Preserve synchronous runtime dispatch in MVP. Long-running Linear agents should post an early thought and enqueue follow-up work in application code until deferred dispatch is designed at the runtime level.
- Encode Linear **Thread IDs** as opaque adapter-produced values with a versioned payload containing organization, issue, optional comment, and agent session identity.
- Require organization identity in Go Linear **Thread IDs** as a tenant-correctness difference from upstream Chat SDK's shorter Linear thread strings.
- Validate Linear **Thread IDs** by decoding the opaque payload and requiring agent session identity. Reject malformed IDs and non-agent-session IDs.
- Support **Thread Handle** reconstruction from stored Linear agent-session **Thread IDs** so out-of-webhook posting works when adapter credentials are still valid.
- Map `Thread.Post` on a **Linear Agent Session Thread** to Linear **Agent Activity Response** creation.
- Pass **Plain Text** and **Portable Markdown** bodies through unchanged to Linear agent activity bodies in MVP.
- Do not port upstream Chat SDK's richer Linear Markdown conversion until the Go runtime has richer formatted content that needs it.
- Add a Linear-specific `PostThought` method on the adapter. It creates an ephemeral **Agent Activity Thought** for an agent session and returns a **Sent Message** for the created Linear activity.
- Treat `PostThought` as an adapter-specific escape hatch. Do not add a generic `Thread.StartTyping` or runtime-level thinking API in MVP.
- Populate **Actor** names for the app actor and inbound actors when Linear provides names. Keep routing and self-message identity based on adapter, tenant, and actor ID.
- Use the runtime's existing **Self Message** filtering by returning a tenant-scoped `BotActor` for the discovered Linear app user.
- Build one memory-backed Linear app-agent hello-world example. Do not add Redis or Postgres Linear examples in this slice.
- The example proves new mention routing, explicit subscription, best-effort thought posting, final response posting, and subscribed follow-up routing.
- The example setup instructions ask users to enable Linear agent session webhooks only. Comments, issues, and reactions remain outside the MVP example.
- The README documents that memory state is local-only and production deployments should choose Redis or Postgres **Runtime State** separately.
- Respect the accepted ADR for this slice and update public docs/GoDoc to make intentional gaps discoverable.

## Testing Decisions

- Tests should verify external behavior and public contracts, not private implementation details.
- Use the Slack adapter tests as prior art for webhook verification, local fake API servers, event normalization, routing through the runtime, posting behavior, and thread ID validation.
- Add adapter construction tests for missing webhook secret, missing client credentials, invalid signature tolerance if configurable, default option behavior, and adapter name.
- Add token/auth tests using a fake Linear OAuth endpoint to verify initial token exchange, default scopes, expiry tracking, lazy refresh, and no background refresh requirement.
- Add identity discovery tests using a fake Linear GraphQL endpoint to verify organization ID, app user ID, app display name, and `BotActor` output.
- Add webhook verification tests for valid signatures, invalid signatures, stale timestamps, malformed timestamps, non-POST requests, body read failures where feasible, and malformed JSON.
- Add webhook ignored-event tests for unsupported Linear webhook types, unsupported agent-session actions, unbuildable session payloads, and events for another app actor when payload identity proves the mismatch.
- Add session-creation normalization tests that assert runtime-observable behavior: `OnNewMention` fires, message ID and event identity derive from source comment ID, thread ID is present, author is normalized, and message is marked mentioned.
- Add prompted normalization tests that assert subscription-sensitive behavior: before subscription it can route as a new mention, and after explicit subscription it routes to `OnSubscribedMessage`.
- Add self-message tests proving a Linear message authored by the discovered app actor is ignored by runtime self-message filtering.
- Add dedupe tests proving repeated Linear webhook deliveries with the same source comment identity run handlers once.
- Add thread ID tests for round-trip encoding/decoding, malformed IDs, wrong adapter prefix, missing organization, missing issue, missing agent session, optional comment context, and thread handle reconstruction.
- Add outbound response tests using a fake Linear GraphQL endpoint to verify `Thread.Post` creates a response activity with the expected session ID, body, and returned **Sent Message**.
- Add thought tests using a fake Linear GraphQL endpoint to verify `PostThought` creates an ephemeral thought activity, rejects empty thought text, rejects non-agent-session thread IDs, and returns a **Sent Message**.
- Add markdown/text posting tests proving the MVP passes bodies through unchanged for both **Plain Text** and **Portable Markdown**.
- Add actor-name tests proving names are populated when present and omitted safely when absent.
- Add example compile tests or example-focused validation consistent with existing example validation commands.
- Add README/docs coverage tests if the repository's documentation tests require public examples to remain synchronized.
- Run the existing root, adapter, and example test commands. If Docker-dependent Redis/Postgres tests are unavailable, report that as a validation limitation rather than claiming full validation.
- Live dogfooding is required before claiming the integration works end-to-end against Linear. Dogfooding evidence should include screenshots or video of app settings, webhook configuration, first mention, thought/response, follow-up prompt, and follow-up thought/response.

## Out of Scope

- Personal API key Linear bots.
- Pre-obtained static access-token auth.
- Generic Linear issue-comment mode.
- Multi-tenant OAuth installation flow and installation token storage.
- Encrypted Linear installation token storage.
- Generic Linear issue/comment posting outside agent sessions.
- Linear issue creation, assignment, state changes, or project operations.
- Linear message history APIs.
- Linear reactions.
- Edit/delete/outbound mutation APIs.
- Files or attachments.
- Cards, buttons, modals, actions, elicitation, or native rich UI.
- Streaming responses, plan updates, action logs, and error activities beyond the narrow ephemeral thought and final response operations.
- A generic runtime typing/thinking API.
- A raw public Linear API client or public GraphQL helper.
- A Linear SDK dependency.
- Redis or Postgres Linear examples.
- Runtime-level asynchronous/deferred dispatch.
- Full upstream Chat SDK Linear feature parity.

## Further Notes

The accepted ADR records why this feature is intentionally narrower than the upstream Chat SDK Linear adapter. Future work can add generic comment mode, multi-tenant OAuth installs, streaming/plans/actions, reactions, history, and Markdown conversion as separately designed slices.

The deepest implementation seams should be the token/auth module, the Linear API/GraphQL module, the webhook verifier/normalizer, and the thread ID codec. Each encapsulates a lot of behavior behind a small interface and can be tested independently with fake HTTP servers or pure unit tests.

The example should remain toy-sized. Its job is to prove the Linear app-actor integration path and teach explicit subscription, not to pretend to be a full AI agent.
