# Microsoft Teams Adapter (Bot Framework) Approach

Status: needs-triage

## Problem Statement

The **Go Chat Runtime** has two adapters: a production-shaped Slack **Single-Install Adapter** and the narrow Linear app-actor slice. Both prove that the small **Adapter** interface, opaque adapter-produced **Thread ID**, and **Event**/**Message**/**Actor** normalization hold against real platforms. The next platform under consideration is Microsoft Teams.

Teams bots do not run on a simple HMAC-signed webhook like Slack or Linear. They run through Azure Bot Service and the Bot Framework Connector: the Connector POSTs a JSON `Activity` to the bot's HTTPS messaging endpoint, the bot must validate a per-request JWT against a JWKS endpoint (not a single shared secret), and the bot's reply is a *separate* authenticated outbound REST call to the Connector rather than a response body. Later proactive posting is not reconstructable from a human-style channel id; it needs a stored `conversationReference` carrying `serviceUrl`. This is a meaningfully different platform boundary than the existing adapters, and the public domain semantics (opaque **Thread ID**, **Postable Message**, **Platform Escape Hatch**) must absorb it without widening the core.

This document is design only. It does not authorize implementation. Several Teams specifics could not be fully verified from documentation and are explicitly marked spike-required below.

### Reopened non-goals

This work reopens documented non-goals; per `docs/agents/domain.md` they are surfaced explicitly with their source:

- **MVP PRD Out of Scope: "Additional adapters beyond Slack"** (`.scratch/go-chat-runtime-mvp/PRD.md`). The Slack-first MVP deliberately excluded other adapters until the core model was proven. It is now proven by Slack plus the Linear app-actor slice, so a third platform is justified to test the **Adapter** seam against a fundamentally different auth/transport model (JWT/JWKS + separate outbound REST, not HMAC + response body).
- **ADR-0001 framed Linear as the next adapter** (`docs/adr/0001-linear-app-actor-slice.md`). That ADR positioned Linear as the second adapter and left "Multiple production adapters" as a README gap. Teams extends the adapter roadmap to a third platform; it does not contradict ADR-0001, it continues the trajectory ADR-0001 opened.

## Solution

Design a Teams **Platform Adapter** that sits behind the existing **Adapter** interface (`Name`, `Init`, `Shutdown`, `Webhook`, `ValidateThreadID`, `PostMessage`, `BotActor`) with adapter name `msteams`. The adapter owns the Bot Framework boundary; the runtime is unchanged.

The adapter will:

- Expose a **Webhook Handler** at the bot messaging endpoint that decodes the inbound Bot Framework `Activity` as a **Supported Platform Shape**, verifies the inbound JWT, normalizes the `Activity` into a runtime **Event**/**Message**/**Actor**, and hands it to **Runtime Dispatch**.
- Own Bot Framework **inbound** JWT validation (Connector -> bot) and **outbound** token minting (bot -> Connector) as adapter-owned platform auth, the same way Slack signature verification and Linear client-credentials exchange are adapter-owned.
- Mint an opaque **Thread ID** by serializing the minimal `conversationReference` needed to post later (`serviceUrl`, `conversation.id`, `tenantId`, `bot.id`, `channelId`), so out-of-webhook and proactive posting survive process restarts. This is heavier than Slack's opaque id but is exactly what the opaque-**Thread ID** contract is for.
- Map `Thread.Post` to a Connector "reply to activity" / "send to conversation" REST call rendering **Plain Text** and **Portable Markdown** through Teams `textFormat`.
- Treat the inbound turn ack and the (separate, authenticated) outbound reply as adapter-owned, within an assumed ~10-15s turn budget pending spike confirmation, and reuse the ADR 0002 **Ack-Then-Work** / **Detached Work Context** primitive for proactive posting that exceeds the turn budget.
- Fire `OnNewMention` from the bot's presence in `Activity.entities[]` Mention objects, not from text matching (Teams embeds `<at>@bot</at>` in `Activity.text`, which must be stripped).

The adapter follows the Slack/Linear direct-HTTP pattern: inject an HTTP client, keep low-level Connector calls private, decode local structs for supported `Activity` shapes, expose only narrow methods through typed **Adapter Access**. `github.com/infracloudio/msbotbuilder-go` is treated as a spike-time reference for its JWT validation code, not adopted wholesale (see Implementation Decisions and the ADR).

This slice is a single-platform vertical that proves Teams against the existing seam. It is not a full Teams product surface (no Adaptive Card model, no messaging extensions, no Graph-based proactive install). Native rich content, deferred dispatch, multi-tenant install, and interactive components are deliberately cross-referenced to sibling ADRs rather than redefined here.

## User Stories

1. As a Go application developer, I want to register a Teams **Platform Adapter** with adapter name `msteams`, so that my **Go Chat Runtime** can receive Microsoft Teams bot activity behind the same **Adapter** interface as Slack and Linear.
2. As a Go application developer, I want the Teams adapter to preserve the existing runtime model (opaque **Thread ID**, **Event**/**Message**/**Actor**, single-slot **Routing Hooks**), so that my Slack and Linear knowledge transfers.
3. As a Teams bot operator, I want **Adapter Initialization** to validate the Microsoft App ID and credential before serving webhooks, so that missing or invalid Bot Framework configuration fails fast, consistent with fallible **Runtime Construction**.
4. As a security-conscious operator, I want every inbound `Activity` request authenticated by validating its `Authorization: Bearer` JWT against the Bot Connector OpenID/JWKS metadata, so that unauthenticated requests are rejected before **Runtime Dispatch**.
5. As a security-conscious operator, I want all documented inbound JWT checks enforced (Bearer scheme, valid JWT, `iss == https://api.botframework.com`, `aud == bot App ID`, validity window with 5-minute skew, RS256 signature against the keys doc, and the token `serviceUrl` claim matching `Activity.serviceUrl`), with no way to disable validation, so that the adapter matches Microsoft's security guidance.
6. As a security-conscious operator, I want channel-endorsement enforced so that an `msteams` activity whose signing key does not endorse `msteams` is rejected with HTTP 403, so that channel spoofing is not possible. (Endorsement enforcement is spike-required.)
7. As a runtime operator, I want the JWKS keys fetched and cached (>=24h with refresh), so that inbound validation does not hit the metadata endpoint per request and still tolerates key rotation.
8. As a Teams bot operator, I want outbound Connector calls authenticated with a cached `client_credentials` token (scope `https://api.botframework.com/.default`), so that replies and proactive posts are authorized without re-minting a token per call.
9. As a Teams bot operator, I want the outbound token refreshed lazily before expiry in adapter process memory, so that **Runtime State** is not expanded to store adapter credentials (matching the Linear app-actor token-cache decision).
10. As a bot developer, I want an opaque Teams **Thread ID** that serializes the `conversationReference` (`serviceUrl`, `conversation.id`, `tenantId`, `bot.id`, `channelId`), so that I can store a thread reference and post later even after a process restart.
11. As a bot developer, I want `ValidateThreadID` to decode the opaque Teams **Thread ID** back into a `ThreadRef`, so that **Thread Handle** reconstruction works for out-of-webhook posting.
12. As a bot developer, I want the Teams **Thread ID** and **Actor** to carry **Platform Tenant** context from `conversation.tenantId`, so that cross-tenant collisions are impossible, reusing the **Platform Tenant** scoping already baked into **Thread ID** and **Actor**.
13. As a bot developer, I want a Teams channel `Activity` normalized into a `ThreadRef` whose `Channel` is `conversation.id` and `Direct` reflects personal scope (`conversationType == personal` or not `isGroup`), so that channel threads and DMs route through the same model.
14. As a bot developer, I want inbound message `Activity`s normalized into a **Message** whose `Text` has the leading bot `<at>@bot</at>` mention stripped, so that handlers see the user's actual text.
15. As a bot developer, I want `OnNewMention` to fire on the bot's presence in `Activity.entities[]` Mention objects rather than substring text matching, so that mention detection is reliable and not fooled by display-name text.
16. As a bot developer, I want the inbound author normalized into an **Actor** (`Adapter: msteams`, `Tenant`, `ID` from `from.id` or `from.aadObjectId`, `Name` from `from.name`, **Bot Kind** human), so that handlers get a tenant-scoped identity.
17. As a bot developer, I want `BotActor()` derived from `Activity.recipient` with **Bot Kind** bot, so that runtime **Self Message** filtering prevents bot loops.
18. As a bot developer, I want `Thread.Post` to map to the Connector "reply to activity" / "send to conversation" REST call and return a **Sent Message** from the `ResourceResponse.id`, so that thread-scoped replies work like other adapters.
19. As a bot developer, I want **Plain Text** rendered with `textFormat = plain` and **Portable Markdown** rendered with `textFormat = markdown`, so that the portable posting surface is unchanged for Teams. (Markdown subset fidelity is spike-required.)
20. As a bot developer, I want the full `Activity` JSON preserved as **Event.Raw** / **Message.Raw** via the **Platform Escape Hatch**, so that uncommon Teams-specific needs are reachable without widening the core.
21. As a runtime operator, I want `Event.ID` set from `Activity.id` for dedupe, so that Connector redeliveries do not run handlers twice. (Whether `Activity.id` is stable across redelivery is spike-required.)
22. As a Teams bot operator, I want the adapter to ack the inbound turn with an HTTP 2xx within the platform turn budget after sending its reply via the Connector REST call, so that the channel does not report a `504: GatewayTimeout`.
23. As a Teams bot operator, I want long handler work to use the ADR 0002 **Ack-Then-Work** + **Detached Work Context** primitive and deliver results via a proactive Connector post using the stored `conversationReference`, so that work exceeding the ~10-15s turn budget is not abandoned.
24. As a bot developer, I want proactive posting (`continueConversation`-style) reachable through a stored **Thread ID** without an active turn, so that scheduled or async replies can target a known Teams conversation. (Proactive-install prerequisites are spike-required.)
25. As a Teams bot operator, I want the adapter to surface known proactive-posting failures (HTTP 403 `ForbiddenOperationException` when the bot is not installed in the target scope, HTTP 403 `MessageWritesBlocked` when a user blocked/uninstalled the bot) as explicit errors, so that callers can distinguish "not installed" from transient failures.
26. As an adapter author, I want the adapter to default to direct-HTTP Bot Framework calls rather than depending on `github.com/infracloudio/msbotbuilder-go` as a maintained foundation, so that the durable surface is not built on a dormant community SDK, consistent with the Slack/Linear direct-HTTP precedent.
27. As an adapter maintainer, I want the inbound JWT validator, outbound token minter, the `Activity` decoder/normalizer, and the **Thread ID** codec as small adapter-internal deep modules, so that each is testable behind a narrow interface with fake HTTP servers.
28. As a future maintainer, I want Teams native Adaptive Card content explicitly deferred to the ADR 0004 **Native Content** / **Optional Capability** seam, so that the portable **Postable Message** surface is not widened by Teams.
29. As a future maintainer, I want Teams card actions (Adaptive Card `Action.Submit`) explicitly deferred to the ADR 0004 **Interaction Event** / `OnInteraction` design, so that interactive components are not redefined here.
30. As a future maintainer, I want a multi-tenant Teams install explicitly deferred to the ADR 0006 **Multi-Tenant Adapter** / **Install Store** design, so that the first slice ships as a **Single-Install Adapter** like Slack.
31. As a future maintainer, I want every unverified Teams specific (SDK choice, exact ack/turn contract, endorsements, Markdown fidelity, `serviceUrl`/`conversation.id` stability, proactive-install prerequisites, DM identity key) recorded as spike-required, so that implementation does not begin on unconfirmed assumptions.
32. As a reviewer, I want a spike that exercises a real Teams-issued token and a real channel `Activity` before implementation, so that the adapter is built on verified behavior rather than documentation alone.

## Implementation Decisions

- Build the adapter under `adapters/msteams` with adapter name `msteams` (matching `Activity.channelId`). Follow the Slack/Linear adapter package shape: context-aware constructor, options struct, injectable HTTP client, injectable clock for tests, optional logger.
- The Teams adapter is a **Single-Install Adapter** in this slice. **Thread ID** and **Actor** still carry **Platform Tenant** context (`conversation.tenantId`) so a later **Multi-Tenant Adapter** (ADR 0006) is not blocked. Multi-tenant credential resolution and the **Install Store** are out of scope here.
- Default to **direct-HTTP** Bot Framework integration rather than adopting `github.com/infracloudio/msbotbuilder-go` wholesale. Rationale: it is a community (not Microsoft-official) port, last tagged release v0.2.5 (Oct 2021), most recent commit Nov 2023, ~149 stars, open issues touching real pain (missing auth headers, broken sample), and old transitive deps (`golang-jwt/jwt/v4` v4.1.0, `lestrrat-go/jwx` v1) that are superseded. Treat it as a spike-time reference implementation for its `connector/auth` JWT validation specifically; do not build the durable surface on it without pinning/forking and verifying all inbound checks including endorsements.

### Inbound auth (Connector -> bot), adapter-owned

- Validate the `Authorization: Bearer` JWT on every inbound `Activity` against the static Bot Connector OpenID metadata `https://login.botframework.com/v1/.well-known/openidconfiguration`, resolving signing keys from its `jwks_uri` (`https://login.botframework.com/v1/.well-known/keys`).
- Enforce all documented checks: Bearer scheme; valid JWT; `iss == https://api.botframework.com`; `aud == bot Microsoft App ID`; validity window with 5-minute clock skew; RS256 signature against a key in the keys doc; token `serviceUrl` claim matches `Activity.serviceUrl`.
- Enforce channel endorsement: if `msteams` requires endorsement, the signing key's `endorsements` must include `msteams` or return HTTP 403. (Exact enforcement is spike-required.)
- Cache the JWKS keys (>=24h) and refresh on cache miss / key rotation. Never expose a config flag to disable validation. Return HTTP 403 on any validation failure.
- Use a maintained JWT/JWKS library (current `golang-jwt` / `lestrrat-go/jwx`), not the versions pinned by `msbotbuilder-go`.

```go
// Illustrative shape only; not an implementation.
type Options struct {
    MicrosoftAppID       string
    MicrosoftAppPassword string // client secret; managed-identity is a spike-time alternative
    HTTPClient           *http.Client
    // ... clock, logger
}
```

### Outbound auth (bot -> Connector), adapter-owned

- Mint an outbound token via `client_credentials` (`scope=https://api.botframework.com/.default`) against the Bot Framework token endpoint; cache it in adapter process memory and refresh lazily before expiry (~3600s), matching the Linear app-actor token-cache decision. Do not store outbound tokens in **Runtime State**.
- Single-tenant vs multi-tenant vs managed-identity changes the token URL and inbound `aud`/`iss` expectations; the deployment model is spike-required (see Open Questions).

### Normalization (`Activity` -> Event/Message/Actor)

- `Event.Adapter = msteams`; `Event.Tenant = Activity.conversation.tenantId`; `Event.ID = Activity.id`; `Event.Raw = full Activity JSON` (**Platform Escape Hatch**).
- `Message.ID = Activity.id`; `Message.Text = Activity.text` with the leading bot `<at>@bot</at>` mention stripped; `Message.Author = from Actor`; `Message.Mentioned` derived from `Activity.entities[]` Mention objects; `Message.Raw = Activity`.
- Inbound `Actor{Adapter: msteams, Tenant: tenantId, ID: from.id (or from.aadObjectId), Name: from.name, BotKind: human}`. The canonical `Actor.ID` key (`from.id` vs `from.aadObjectId`) is spike-required.
- `BotActor()` built from `Activity.recipient` with **Bot Kind** bot; runtime **Self Message** filtering uses it.
- `MessageEvent.DirectMessage` true when the conversation is personal scope (`conversationType == personal` or not `isGroup`).
- `OnNewMention` fires only when the bot appears in `Activity.entities[]` Mention objects (`mentioned.id == bot id`), not on text matching. Behavior under resource-specific consent (RSC), where the bot may receive channel messages without a mention, is spike-required.
- Decode only the **Supported Platform Shape** of `Activity` with permissive unknown-field handling; tolerate unrelated Bot Framework fields. Non-`message` activity types (`conversationUpdate`, `messageReaction`, `typing`, etc.) are **Ignored Events** in this slice. `invoke` activities (which expect a synchronous response body) are out of scope here; Teams `invoke` is a transport that carries both command-style invokes (-> **Command Event** / `OnCommand`, ADR 0003) and card-action invokes (-> **Interaction Event** / `OnInteraction`, ADR 0004).

### Thread ID and posting

- `ThreadID` is opaque and adapter-produced: a versioned serialization of the minimal `conversationReference` -- at least `{serviceUrl, conversation.id, tenantId, bot.id, channelId}`. Unlike Slack channel+ts, Teams proactive posting cannot be reconstructed from a human-style id, so `serviceUrl` and `tenantId` must be carried in the **Thread ID**.
- `ValidateThreadID` decodes the opaque string into `ThreadRef{ID, Adapter: msteams, Tenant: tenantId, Channel: conversation.id, Root: channel-thread root where applicable, Direct, Raw: stored conversationReference}`.
- `PostMessage` maps to the Connector "reply to activity" (`POST {serviceUrl}/v3/conversations/{conversationId}/activities/{replyToId}`) or "send to conversation" REST call, using the stored `serviceUrl` as base URI. `SentMessage.ID = ResourceResponse.id`.
- `PostableMessage.Text` renders to `Activity.text` with `textFormat = markdown` for `chat.Markdown()` and `textFormat = plain` for `chat.Text()`. Markdown subset fidelity (tables, code blocks, links) is spike-required; treat unsupported constructs as acceptable degradation per the **Portable Markdown** contract, not as a converter the adapter owns.
- `serviceUrl` can change (Microsoft warns against hardcoding); refresh the stored `serviceUrl` from each inbound `Activity` so the persisted **Thread ID** stays current. Stability of `conversation.id` and `serviceUrl` for long-term persistence is spike-required.

### Ack / turn contract

- The inbound webhook ack is adapter-owned, the same way **Platform Handshake** is adapter-owned. The bot sends its reply as a separate authenticated outbound Connector REST call, then returns HTTP 2xx on the inbound request to acknowledge the `Activity`, within an assumed ~10-15s turn budget pending spike confirmation (channel times out with `504: GatewayTimeout` otherwise).
- Unlike Slack/Linear there is no documented HMAC body signature and no "respond in the webhook body" shortcut for `message` activities; the reply is always a separate REST call. Only certain `invoke` activities expect a response body (out of scope here).
- This slice preserves synchronous **Runtime Dispatch** on the inbound request context, consistent with the MVP. Work exceeding the turn budget uses the ADR 0002 **Ack-Then-Work** + **Detached Work Context** primitive and delivers via proactive Connector post; the Teams ~10-15s budget is the relevant ceiling. Deferred dispatch is designed in ADR 0002, not here.

### Capability boundaries (cross-referenced, not redefined here)

- Native Adaptive Card content is deferred to ADR 0004 (**Native Content** as an **Optional Capability** reached via **Adapter Access**, carrying the Adaptive Card payload as a **Platform Escape Hatch**). This slice does not add a card model and does not change **Postable Message**.
- Teams card actions are deferred to ADR 0004 (**Interaction Event** + single-slot `OnInteraction`). Teams `invoke` is a transport split by destination hook: command-style invokes map to **Command Event** / `OnCommand` (ADR 0003) and card-action invokes map to **Interaction Event** / `OnInteraction` (ADR 0004), neither as **Messages**.
- Multi-tenant Teams install is deferred to ADR 0006 (**Multi-Tenant Adapter** + **Install Store**); this slice is **Single-Install**.
- Outbound Connector rate-limit retry/backoff is deferred to ADR 0005 (bounded retry in the **Platform Adapter**, honoring Retry-After, surfaced through **Runtime Observation**).
- Teams **Message History** (channel/chat backfill via Graph) stays application-owned per ADR 0009; at most a thin optional `HistoryReader` seam, not part of **Runtime State**.

## Testing Decisions

- Tests should verify external behavior and public contracts, not private implementation details. Use the Slack and Linear adapter tests as prior art: inject an HTTP client, run fake Bot Framework servers, assert runtime-observable routing/posting.
- Add **Adapter Initialization** tests for missing/invalid Microsoft App ID, missing credential, and successful startup; assert webhooks are not served before init succeeds.
- Add inbound JWT validation tests against a fake JWKS server covering: valid token accepted; missing/malformed Bearer rejected; wrong `iss` rejected; wrong `aud` rejected; expired/not-yet-valid rejected (and accepted within 5-minute skew); RS256 signature mismatch rejected; `serviceUrl`-claim mismatch with `Activity.serviceUrl` rejected; JWKS cache hit avoids refetch; key rotation triggers refresh.
- Add channel-endorsement tests: an `msteams` activity whose signing key endorses `msteams` is accepted; one whose key omits the endorsement returns HTTP 403. (Confirm exact rule during spike before finalizing assertions.)
- Add a test asserting there is no configuration path that disables inbound validation.
- Add outbound token tests against a fake token endpoint: initial `client_credentials` mint with the correct scope, in-memory cache reuse, lazy refresh before expiry, no background refresher.
- Add normalization tests asserting runtime-observable behavior: `Event.ID`/`Event.Tenant`/`Event.Adapter`, bot-mention stripping from `Message.Text`, `Message.Mentioned` derived from `entities[]`, **Actor** and **Bot Kind**, personal-scope `DirectMessage`, and raw `Activity` preserved.
- Add mention-routing tests: `OnNewMention` fires when the bot is in `entities[]`; it does not fire on a bare text containing the bot's display name without a Mention entity.
- Add **Self Message** tests proving an `Activity` authored by the bot's `recipient` identity is ignored.
- Add dedupe tests proving repeated deliveries with the same `Activity.id` run handlers once. (Pending spike confirmation that `Activity.id` is the right dedupe key.)
- Add **Thread ID** tests: round-trip encode/decode, version handling, wrong adapter prefix, missing `serviceUrl`/`conversation.id`/`tenantId`, **Thread Handle** reconstruction, and `serviceUrl` refresh-on-inbound.
- Add outbound posting tests against a fake Connector server: "reply to activity" and "send to conversation" calls carry the bearer token, render `textFormat = markdown` vs `plain`, and return a **Sent Message** from `ResourceResponse.id`.
- Add proactive-post error-mapping tests: HTTP 403 `ForbiddenOperationException` and `MessageWritesBlocked` surface as distinct explicit errors.
- Add Markdown rendering tests for the **Portable Markdown Subset** once Teams Markdown fidelity is confirmed in the spike.
- Run the existing root, adapter, and example test commands. Live dogfooding against a real Azure Bot resource and Teams tenant is required before claiming the integration works end-to-end; the spike (below) is a precondition.

## Out of Scope

- Adopting `github.com/infracloudio/msbotbuilder-go` as a maintained foundation for the durable surface (reference-only during the spike).
- Microsoft Graph integration, including Graph-based proactive app installation, admin-consent flows, and roster/membership queries.
- Adaptive Card and other native rich content models (ADR 0004 **Native Content** seam).
- Teams card actions, messaging-extension flows, task modules, and other interactive components (ADR 0004 **Interaction Event** / `OnInteraction`).
- Teams `invoke` activities and their synchronous response-body contract.
- Multi-tenant Teams install and per-tenant credential resolution (ADR 0006 **Multi-Tenant Adapter** / **Install Store**).
- Ephemeral delivery for arbitrary messages: Teams has no clean equivalent (ephemeral UX is tied to invoke/messaging-extension flows), so the `EphemeralPoster` **Optional Capability** is left unimplemented for Teams initially.
- Outbound mutation (edit/delete via `UpdateActivity`/`DeleteActivity`) beyond returning a **Sent Message**.
- Teams **Message History** / channel backfill baked into **Runtime State** (ADR 0009; at most a thin optional `HistoryReader`).
- Runtime-level deferred/asynchronous dispatch (ADR 0002 owns the **Dispatch Mode** / **Detached Work Context** primitive; this slice consumes it, does not define it).
- Outbound rate-limit retry/backoff policy specifics (ADR 0005).
- Posting to Teams private channels (not supported by the platform).
- Full Bot Framework / Teams product-surface parity.

## Further Notes

The deepest implementation seams should be the inbound JWT/JWKS validator (with endorsement handling), the outbound `client_credentials` token minter/cache, the `Activity` decoder/normalizer, and the Teams **Thread ID** codec that serializes a `conversationReference`. Each encapsulates substantial behavior behind a narrow adapter-internal interface and is testable with fake HTTP servers, mirroring the Linear token/auth, API, verifier, and thread-ID-codec seams.

The load-bearing patterns hold without change: the small **Adapter** interface absorbs Teams as-is; **Thread ID** stays opaque and adapter-produced (it just carries more inside it); rich content and interactions go through the **Platform Escape Hatch** / **Optional Capability** path rather than widening the core; **Routing Hooks** remain single-slot.

The single biggest divergence from the existing adapters is auth shape: inbound is JWT-over-JWKS rather than a shared-secret HMAC, and outbound replies are separate authenticated REST calls rather than a webhook response body. The biggest **Thread ID** divergence is that Teams must persist `serviceUrl` + `tenantId` + `conversation.id` to post later, which is heavier than Slack but exactly what the opaque-**Thread ID** contract was designed to allow.

Implementation must not begin until a spike confirms the unverified specifics enumerated in the ADR's Open Questions: exact `msteams` ack/turn semantics and timeout, whether any body-based reply shortcut exists, endorsement enforcement, the deployment model (single/multi-tenant/managed-identity), Markdown subset fidelity, `serviceUrl`/`conversation.id` persistence stability, proactive-install prerequisites, RSC mention behavior, the canonical `Actor.ID` key, and a hands-on production-readiness evaluation of `msbotbuilder-go`'s `connector/auth` against all inbound checks.
