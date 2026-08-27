# ADR 0007: Microsoft Teams Adapter (Bot Framework) Approach

## Status

Proposed

## Context

The **Go Chat Runtime** has a production-shaped Slack **Single-Install Adapter** and the narrow Linear app-actor slice (ADR 0001). Both validate the small **Adapter** interface (`Name`, `Init`, `Shutdown`, `Webhook`, `ValidateThreadID`, `PostMessage`, `BotActor`), opaque adapter-produced **Thread ID**, **Event**/**Message**/**Actor** normalization, and the **Platform Escape Hatch**. Microsoft Teams is the next platform under consideration.

Teams does not look like Slack or Linear at the boundary. Bots run through Azure Bot Service and the Bot Framework Connector:

- The Connector POSTs a JSON `Activity` (the single inbound payload type) to the bot's HTTPS messaging endpoint. `Activity` carries `type`, `id`, `channelId` (`msteams`), `serviceUrl`, `from`/`recipient` (`ChannelAccount`), `conversation` (`ConversationAccount`, including `tenantId`/`conversationType`/`isGroup`), `text`/`textFormat`, `replyToId`, `entities[]` (Mentions), and `channelData`.
- Inbound auth is a per-request JWT validated against a JWKS endpoint, not a single shared HMAC secret. The bot must enforce: Bearer scheme; valid JWT; `iss == https://api.botframework.com`; `aud == bot App ID`; validity window (5-minute skew); RS256 signature against `https://login.botframework.com/v1/.well-known/keys` (cache >=24h, refresh on rotation); and the token `serviceUrl` claim matching `Activity.serviceUrl`. Channels may require endorsement: the signing key's `endorsements` must include `msteams`. Microsoft is explicit that all checks are mandatory and validation must never be disableable.
- Outbound auth is a separate `client_credentials` OAuth2 flow (`scope=https://api.botframework.com/.default`) producing a bearer token (~3600s, cache/refresh).
- The bot's reply is a *separate* authenticated outbound Connector REST call ("reply to activity" / "send to conversation") using the inbound `serviceUrl` as base URI, not a webhook response body. The bot then returns HTTP 2xx to ack the `Activity`. The turn budget is ~10-15s depending on channel; exceeding it yields a `504: GatewayTimeout`.
- Later proactive posting needs a stored `conversationReference` (`serviceUrl`, `conversation`, `tenantId`, `bot`/`user`); it cannot be reconstructed from a human-style channel id. In Teams the bot only sees channel/group messages where it is @mentioned (absent RSC), the bot mention is embedded in `Activity.text` as `<at>@bot</at>`, and bot identity must come from `entities[]`, not text.

This ADR reopens documented non-goals; per `docs/agents/domain.md` they are surfaced explicitly with their source:

- **MVP PRD Out of Scope: "Additional adapters beyond Slack"** (`.scratch/go-chat-runtime-mvp/PRD.md`). The Slack-first MVP deferred other adapters until the core model was proven. Slack plus the Linear app-actor slice have proven it, so a third platform is justified -- specifically to test the **Adapter** seam against a fundamentally different auth/transport model.
- **ADR-0001 framed Linear as the next adapter** (`docs/adr/0001-linear-app-actor-slice.md`), which positioned Linear as the second adapter and left "Multiple production adapters" as a README gap. Teams continues that roadmap to a third platform without contradicting ADR 0001.

This ADR is design only. Several Teams specifics could not be verified from documentation and are marked spike-required.

## Decision

Build a Teams **Platform Adapter** under `adapters/msteams` (adapter name `msteams`, matching `Activity.channelId`) behind the existing **Adapter** interface, ships as a **Single-Install Adapter**, with **direct-HTTP** Bot Framework integration. The runtime is unchanged.

The adapter will:

- Expose a **Webhook Handler** at the bot messaging endpoint that decodes the inbound `Activity` as a **Supported Platform Shape** (permissive unknown-field handling), validates the inbound JWT, normalizes a `message` `Activity` into a runtime **Event**/**Message**/**Actor**, and hands it to **Runtime Dispatch**. Non-`message` activity types are **Ignored Events** in this slice; `invoke` activities are out of scope here -- Teams `invoke` is a transport that carries both command-style invokes (-> **Command Event**, ADR 0003) and card-action invokes (-> **Interaction Event**, ADR 0004).
- Own **inbound** JWT/JWKS validation enforcing all documented checks (Bearer, valid JWT, `iss`, `aud == App ID`, validity window with 5-minute skew, RS256 against the keys doc, `serviceUrl`-claim match), plus channel endorsement (return HTTP 403 when an `msteams` activity's signing key omits the `msteams` endorsement). JWKS keys are cached (>=24h) and refreshed on rotation. There is no flag to disable validation. Validation failures return HTTP 403. JWT/JWKS validation is implemented with the standard library only (`crypto/rsa`; see Spike Findings) — no JWT library is added.
- Own **outbound** `client_credentials` token minting (`scope=https://api.botframework.com/.default`), cached in adapter process memory and refreshed lazily before expiry. **Runtime State** is not expanded to store adapter credentials, matching the Linear app-actor token-cache decision (ADR 0001).
- Mint an opaque **Thread ID** as a versioned serialization of the minimal `conversationReference` -- at least `{serviceUrl, conversation.id, tenantId, bot.id, channelId}` -- so out-of-webhook and proactive posting survive process restarts. `serviceUrl` is refreshed from each inbound `Activity` because Microsoft warns it can change. `ValidateThreadID` decodes this into a `ThreadRef` (`Adapter: msteams`, `Tenant: tenantId`, `Channel: conversation.id`, `Direct` from personal scope, `Raw` = stored `conversationReference`), so **Thread Handle** reconstruction works.
- Normalize: `Event.Adapter = msteams`; `Event.Tenant = conversation.tenantId`; `Event.ID = Activity.id`; `Event.Raw = Activity` (**Platform Escape Hatch**). `Message.Text = Activity.text` with the leading bot `<at>@bot</at>` stripped; `Message.Mentioned` from `entities[]`; inbound **Actor** from `from` (**Bot Kind** human); `BotActor()` from `recipient` (**Bot Kind** bot). `OnNewMention` fires from the bot's presence in `entities[]` Mention objects, never from text matching.
- Map `Thread.Post` to the Connector "reply to activity" / "send to conversation" REST call, render **Plain Text** with `textFormat = plain` and **Portable Markdown** with `textFormat = markdown`, and return a **Sent Message** from `ResourceResponse.id`. Unsupported Markdown constructs are acceptable degradation per the **Portable Markdown** contract, not a converter the adapter owns.
- Treat the inbound turn ack as adapter-owned (the same way **Platform Handshake** is adapter-owned): send the reply via the separate authenticated Connector REST call, then return HTTP 2xx within the ~10-15s turn budget. Preserve synchronous **Runtime Dispatch** on the inbound request context. Work exceeding the turn budget uses the ADR 0002 **Ack-Then-Work** + **Detached Work Context** primitive and delivers via a proactive Connector post using the stored `conversationReference`. Map proactive-post failures HTTP 403 `ForbiddenOperationException` (not installed) and `MessageWritesBlocked` (blocked/uninstalled) to distinct explicit errors.

Follow the Slack/Linear direct-HTTP pattern: inject an HTTP client, keep low-level Connector calls private, decode local supported-shape structs, expose only narrow methods through typed **Adapter Access**.

Capability boundaries are cross-referenced, not redefined here:

- Native Adaptive Card content -> ADR 0004 (**Native Content** as an **Optional Capability**; **Postable Message** stays portable and unchanged).
- Teams card actions -> ADR 0004 (**Interaction Event** + single-slot `OnInteraction`); command-style `invoke` -> ADR 0003 (**Command Event** + single-slot `OnCommand`). Teams `invoke` is one transport split by destination hook. These are Event kinds, not **Messages**.
- Multi-tenant Teams install -> ADR 0006 (**Multi-Tenant Adapter** + app-owned **Install Store**). This slice is **Single-Install**; **Thread ID** and **Actor** still carry **Platform Tenant** context so it is not blocked.
- Outbound Connector rate-limit retry/backoff -> ADR 0005 (bounded retry in the **Platform Adapter**, honoring Retry-After, surfaced via **Runtime Observation**).
- Teams **Message History** -> ADR 0009 (application-owned; at most a thin optional `HistoryReader`, never in **Runtime State**).
- Observability of dispatch/JWKS/Connector calls -> ADR 0010 (**Runtime Observation** default, optional **Observation Hook** surface).

Implementation must not begin until a spike confirms the Open Questions below.

### Open Questions (spike-required before implementation)

1. Exact inbound ack semantics for the `msteams` channel: confirm a bare HTTP 200 with empty body acks a `message` activity, and confirm the real timeout within the documented ~10-15s range.
2. Whether any body-based reply shortcut exists for `message` activities, or whether every reply must be a separate authenticated outbound Connector REST call (current evidence: separate call required; only `invoke` activities expect a response body).
3. Endorsement enforcement: confirm whether `msteams` requires an endorsement on the signing key and the exact rule for returning HTTP 403 when absent.
4. Confirm the **single-tenant** Azure Bot resource specifics for the chosen single-install model (outbound token URL, inbound `aud`/`iss` expectations). The deployment model is decided — single-install, mirroring Slack's MVP; multi-tenant and managed-identity are out of this slice and deferred to ADR 0006. The spike only verifies the single-tenant resource details, not which model to use.
5. Teams Markdown subset fidelity under `textFormat = markdown` (tables, code blocks, links) so **Portable Markdown** rendering is predictable.
6. **Thread ID** encoding stability: confirm `conversation.id` and `serviceUrl` are stable enough to persist for later proactive posting, and that refresh-on-inbound covers `serviceUrl` changes.
7. Proactive-posting prerequisites: whether the bot always receives an inbound `Activity` first (so a stored `conversationReference` exists) or whether Graph-based proactive install (a much larger surface) is needed.
8. Mention behavior: confirm `OnNewMention` should fire only on an explicit bot Mention entity (`mentioned.id == bot id`), and how to behave under RSC where the bot may receive channel messages without a mention.
9. `msbotbuilder-go` production-readiness: build the echobot, exercise `ParseRequest` against a real Teams-issued token, and confirm its `connector/auth` implements all inbound checks (including endorsements and the `serviceUrl`-claim check) and the correct outbound scope; evaluate replacing its old `jwt/v4` + `jwx v1` deps.
10. DM/personal-scope identity: confirm whether `from.id` alone is a stable cross-conversation user key or whether `from.aadObjectId` should be the canonical `Actor.ID` for Teams (`aadObjectId` is tenant-stable; `from.id` can be bot-scoped).

## Consequences

Coder gets a path to a Teams bot behind the same runtime model as Slack and Linear, with `Thread.Post` and **Thread Handle** reconstruction working as elsewhere:

```go
ev.Thread.Post(ctx, chat.Markdown("..."))
```

```go
teamsAdapter, ok := chat.AdapterAs[*msteams.Adapter](bot, "msteams")
if ok {
    // narrow Teams-specific methods reachable deliberately, never raw internals
}
```

The adapter diverges from the existing adapters in deliberate ways:

- inbound auth is JWT-over-JWKS (fetch + cache + endorsements), not a shared-secret HMAC;
- outbound replies are separate authenticated Connector REST calls, not a webhook response body;
- the opaque **Thread ID** is heavier because it must serialize a `conversationReference` (`serviceUrl` + `tenantId` + `conversation.id`) to survive restarts and enable proactive posting;
- the turn budget is ~10-15s (vs Slack 3s, and Linear's two-part **Agent Session Timing Contract**: first thought ~10s, follow-up window ~30min -- ADR 0008), and long work uses the ADR 0002 detached primitive plus proactive posting.

The JWKS fetch/cache, endorsement handling, and `conversationReference` persistence are net-new complexity not present in the Slack/Linear adapters, concentrated in adapter-internal deep modules so the runtime and core types stay unchanged.

Because key behaviors are documentation-only until a spike runs, this ADR commits to the *shape* (small **Adapter**, opaque **Thread ID**, **Platform Escape Hatch**, direct-HTTP) but not to the exact ack/auth/endorsement details. Implementing before the spike risks building on unconfirmed assumptions about the `msteams` turn contract, endorsements, and proactive prerequisites.

Future work, separately designed: Adaptive Card **Native Content** (ADR 0004), Teams card-action **Interaction Events** (ADR 0004), command-style **Command Events** (ADR 0003), **Multi-Tenant Adapter** install (ADR 0006), `invoke`/messaging-extension flows, outbound mutation, and a Graph-based proactive-install path.

## Alternatives Considered

### Adopt `github.com/infracloudio/msbotbuilder-go` as the foundation

Rejected as the durable foundation. It is a community (not Microsoft-official) Go port whose `core.Adapter` does offer `ParseRequest` (structure + auth), `ProcessActivity`, and `ProactiveMessage`, which on paper shortcut the hardest parts (inbound JWKS validation, outbound token minting). But it is effectively dormant: last tagged release v0.2.5 (Oct 2021), most recent commit Nov 2023, ~149 stars, open issues touching real pain (missing auth headers, broken sample, module resolution), and superseded transitive deps (`golang-jwt/jwt/v4` v4.1.0, `lestrrat-go/jwx` v1). Its endorsement and `serviceUrl`-claim handling are unverified. Building the durable surface on a four-plus-year-dormant dependency contradicts the Slack/Linear direct-HTTP precedent and the no-SDK-dependency stance from ADR 0001. It is retained as a spike-time reference implementation for its `connector/auth` code specifically; adopting it would require pinning/forking and verifying every inbound check.

### Mirror Slack's HMAC + response-body model for Teams

Rejected because it does not match the platform. Teams has no documented HMAC body signature and no "reply in the webhook body" shortcut for `message` activities; inbound auth is JWT-over-JWKS and the reply is a separate authenticated Connector REST call. Forcing the Slack model would either skip required JWT checks (a security failure Microsoft explicitly forbids) or invent a reply path the platform does not support.

### Reconstruct the Teams Thread ID from a human-style id like Slack channel+ts

Rejected because Teams proactive posting requires `serviceUrl` + `tenantId` + `conversation.id`, none of which are reconstructable from a short human-style id. The opaque **Thread ID** must serialize the minimal `conversationReference`. This is heavier than Slack's id but is exactly what the opaque, adapter-produced **Thread ID** contract permits; it keeps the core unchanged.

### Make Teams dispatch asynchronous at the runtime level

Rejected here. Synchronous **Runtime Dispatch** on the inbound request context is the MVP stance, and deferred dispatch is owned by ADR 0002 (**Dispatch Mode** + **Detached Work Context**), kept orthogonal to **Concurrency Strategy**. The Teams ~10-15s turn budget is handled by consuming that primitive (**Ack-Then-Work**, then proactive post), not by defining a bespoke async path in this adapter.

### Add an Adaptive Card / cross-platform card model to support rich Teams content now

Rejected. Adding a card model would change the meaning of **Postable Message** and widen the portable surface. Native rich content is an ADR 0004 concern: a **Native Content** **Optional Capability** reached via **Adapter Access**, carrying the Adaptive Card payload as a **Platform Escape Hatch**. **Plain Text** + **Portable Markdown** stay portable and default.

### Ship Teams as a Multi-Tenant Adapter from the start

Rejected for this slice. The Slack precedent is a **Single-Install Adapter**, and multi-tenant credential resolution plus the app-owned **Install Store** are ADR 0006 concerns. **Thread ID** and **Actor** already carry **Platform Tenant** context (`conversation.tenantId`), so shipping single-install does not block a later **Multi-Tenant Adapter**. Deciding the Azure Bot deployment model (single/multi-tenant/managed-identity) is a spike Open Question regardless.

### Implement now and verify behavior during implementation

Rejected. The inbound ack/turn contract, endorsement enforcement, Markdown fidelity, `serviceUrl`/`conversation.id` persistence stability, proactive-install prerequisites, and the `msbotbuilder-go` auth implementation are documentation-only or unverified. Committing code before a spike confirms them risks building on wrong assumptions about the `msteams` channel. The decision is to design now and gate implementation on the spike.

## Spike Findings

A code spike of the adapter exists on the `spike/msteams-adapter` branch
([PR #4](https://github.com/coder/chat/pull/4)). It resolved the SDK-adoption decision in
**Open Question 9**:

- **`msbotbuilder-go` is not adopted.** It is rejected as unmaintained (dormant for years,
  superseded transitive deps), confirming the assessment under Alternatives Considered.
- **Inbound JWT/JWKS validation is standard library only.** The spike implements every
  mandatory inbound check with `crypto/rsa` over a public key rebuilt from the JWK
  `n`/`e`, so the otherwise zero-dependency module gains no JWT library at all —
  `golang-jwt`/`jwx` proved unnecessary. The Decision above records this stdlib-only
  choice, which preserves the repo's zero-dependency, stdlib-direct stance (Slack/Linear
  precedent).

Q9's remaining verification steps are superseded rather than resolved: with
`msbotbuilder-go` rejected, exercising *its* auth against a real Teams-issued token is
moot, and validating the replacement stdlib validator against real tokens carries over
into the live-validation checklist below.

The remaining Open Questions still require live validation against a real Azure Bot
resource and Teams tenant before this ADR moves to Accepted; that validation is tracked
in `.scratch/teams-adapter/issues/01-live-tenant-validation.md` (public tracking:
[issue #6](https://github.com/coder/chat/issues/6)).
