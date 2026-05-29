# Multi-Tenant Install and OAuth

Status: needs-triage

## Problem Statement

Today every adapter is a **Single-Install Adapter**: it holds one credential set (Slack `BotToken`, Linear **App-Actor Client Credentials**) for one **Platform Tenant**, resolved once during **Adapter Initialization**. One running deployment serves exactly one Slack workspace or one Linear org. A product that wants to serve many customer workspaces from one deployment cannot, because there is no per-tenant credential lookup during webhook handling.

This is a known gap, deliberately deferred three times:

- MVP PRD Out of Scope: "Multi-workspace Slack OAuth installation flow".
- MVP PRD Out of Scope: "Account linking, login orchestration, parked auth prompts, or **Application Identity** persistence".
- ADR-0001 excludes "multi-tenant OAuth installation storage".

The reopening is narrow. The runtime already carries **Platform Tenant** context inside every **Thread ID** and every **Actor**; the MVP just never used it to pick a credential. The problem is not "add an OAuth framework". The problem is: when a webhook arrives for tenant `T`, which token does the adapter use to verify and reply, and who owns the record that maps `T` to that token?

The wrong answer is to let the runtime own an install database, encrypted token storage, an OAuth callback web flow, and account linking. Those are application and product concerns. The runtime's only job is to define a contract the adapter calls during webhook handling, and to keep its existing **Platform Tenant** scoping correct.

## Solution

Add a **Multi-Tenant Adapter** mode alongside the existing **Single-Install Adapter**, plus an app-owned **Install Store** capability interface the runtime defines but does not implement.

- The **Single-Install Adapter** stays the default. Existing Slack and Linear setups do not change.
- A **Multi-Tenant Adapter** is constructed with an **Install Store** instead of a baked-in token. During webhook handling, after the adapter has parsed enough of the **Supported Platform Shape** to learn the **Platform Tenant**, it asks the **Install Store** for that tenant's install record (token, optional signing secret, optional bot identity) and uses it to verify and reply.
- The **Install Store** is application-owned. The app persists install records (it ran the OAuth installation flow, it owns the database, it owns encryption). The runtime ships the interface and nothing else.
- **Platform Tenant** scoping is reused verbatim. **Thread ID** and **Actor** already embed tenant context, so threads, dedupe, locks, and `BotActor` self-filtering are already tenant-correct. No new tenant identifier is introduced.
- **Application Identity** and account linking stay app-owned, exactly as in the MVP. Mapping a platform **Actor** to a product user is not an install record and is not the **Install Store**'s job.
- The OAuth installation web flow (authorize redirect, callback, token exchange, "add to Slack" button) is **not** a runtime concern. The runtime defines what an install record must contain to serve a webhook; the app produces those records however it likes.

This is the **Optional Capability** pattern again: a narrow Go interface the adapter consumes, not a string registry or a mandatory runtime subsystem. It does not widen the core **Adapter** interface and it does not add credential storage to **Runtime State**.

### Install Store shape

The runtime defines the contract; the app implements it. Sketch:

```go
// InstallStore is implemented by application code, not the runtime.
// The adapter calls it during webhook handling to resolve per-tenant
// credentials before verification and reply.
type InstallStore interface {
    // Lookup returns the install record for one Platform Tenant.
    // ErrInstallNotFound means the tenant is not installed; the adapter
    // treats the webhook as an Ignored Event (acknowledge, do not dispatch).
    Lookup(ctx context.Context, adapter, tenant string) (Install, error)
}

type Install struct {
    Tenant      string // Platform Tenant this record authorizes
    Credential  any    // adapter-specific token/secret payload
    BotActorID  string // optional pre-discovered bot identity for self-filtering
}

var ErrInstallNotFound = errors.New("chat: install not found")
```

`Credential` is an adapter-specific payload (a **Platform Escape Hatch** for credentials), not a normalized cross-platform token model. The Slack adapter expects a bot token (and, if per-install signing secrets are used, a signing secret); the Linear adapter expects per-tenant **App-Actor Client Credentials** or an installation token. Each adapter documents the concrete type it requires and decodes it like any other **Supported Platform Shape**.

### Adapter construction

A **Multi-Tenant Adapter** is the same `New(ctx, Options)` constructor with an `InstallStore` field instead of a static token. The two modes are mutually exclusive and validated at **Runtime Construction**:

```go
// Single-install (today, unchanged):
slack.New(ctx, slack.Options{SigningSecret: ..., BotToken: ...})

// Multi-tenant:
slack.New(ctx, slack.Options{InstallStore: appStore /* , shared SigningSecret if Slack signs per-app */})
```

Setting both a static token and an `InstallStore` is a construction error. Setting neither is a construction error. Fail-fast construction is preserved.

### Webhook ordering

The lookup happens **after** tenant extraction and **before** dispatch:

1. Adapter reads the raw body and parses enough of the **Supported Platform Shape** to extract the **Platform Tenant** (Slack `team_id`, Linear `organizationId`/`oauthClientId`).
2. Adapter calls `InstallStore.Lookup(ctx, name, tenant)`.
   - `ErrInstallNotFound` -> **Ignored Event**: acknowledge to the platform, do not dispatch. A webhook for an uninstalled tenant is valid platform input we simply do not serve.
   - Other error -> the adapter returns a transport error; the platform may retry.
3. Adapter verifies the signature. For platforms with a per-install signing secret, verification uses the secret from the install record. For platforms with one app-level signing secret (Slack signs with the app signing secret, not the per-workspace bot token), verification uses the shared secret and the token from the install record is used only for the reply.
4. Adapter normalizes the **Event** (already tenant-scoped) and hands it to **Runtime Dispatch**.

Signature verification cannot move before tenant extraction on platforms that need a per-install secret, so the adapter must parse the tenant out of an unverified body first. That is acceptable: the tenant field is read for routing only and is re-validated by signature verification before any side effect. Adapters document this ordering explicitly.

### Out-of-webhook posting

**Thread Handle** reconstruction (`bot.Thread(ctx, threadID)`) already decodes the **Platform Tenant** from the opaque **Thread ID**. A **Multi-Tenant Adapter** resolves the credential for a reconstructed thread the same way: extract the tenant from the **Thread ID**, call `InstallStore.Lookup`, post. A stored **Thread ID** stays postable as long as the app still has a valid install record for that tenant.

## User Stories

1. As a SaaS operator, I want one deployment to serve many Slack workspaces, so that I do not run a process per customer.
2. As a SaaS operator, I want one deployment to serve many Linear orgs, so that multi-tenant Linear agents are possible without per-org processes.
3. As a Go application developer, I want the **Single-Install Adapter** to remain the default, so that my existing single-workspace bot keeps working unchanged.
4. As a Go application developer, I want to choose multi-tenant mode by supplying an **Install Store**, so that opting in is explicit and visible at construction.
5. As a Go application developer, I want supplying both a static token and an **Install Store** to fail at **Runtime Construction**, so that ambiguous credential configuration is caught before serving webhooks.
6. As an adapter author, I want to look up per-tenant credentials during webhook handling, so that the right token is used to verify and reply for each **Platform Tenant**.
7. As an adapter author, I want the **Install Store** to be an **Optional Capability** interface, so that the core **Adapter** interface and **Runtime State** do not grow credential concerns.
8. As an application developer, I want to own the **Install Store** implementation, so that token encryption, persistence, and the OAuth installation flow stay in my product where they belong.
9. As an application developer, I want a webhook for an uninstalled tenant treated as an **Ignored Event**, so that an unknown workspace does not crash handling or trigger retry storms.
10. As a runtime operator, I want **Platform Tenant** scoping reused from **Thread ID** and **Actor**, so that dedupe, locks, subscriptions, and self-message filtering are already tenant-correct.
11. As a runtime operator, I want no new tenant identifier introduced, so that existing stored **Thread IDs** and **Actor** records stay valid.
12. As a bot developer, I want **Thread Handle** reconstruction to resolve per-tenant credentials, so that cron jobs and out-of-webhook posting work in multi-tenant deployments.
13. As a security-conscious operator, I want per-install signing secrets supported where the platform uses them, so that webhook verification is tenant-correct and not just reply-correct.
14. As a Go application developer, I want **Application Identity** and account linking to stay app-owned, so that mapping a platform **Actor** to a product user is never confused with an install record.
15. As a Go application developer, I want the OAuth installation web flow kept out of the runtime, so that authorize/callback/token-exchange handlers live in my HTTP server like every other product route.
16. As an adapter author, I want the credential payload to be an adapter-specific **Platform Escape Hatch**, so that Slack and Linear can require different credential shapes without a normalized token model.
17. As a future maintainer, I want the reopened non-goals documented as deliberate, so that the boundary between runtime contract and app-owned store stays clear.
18. As a Linear app developer, I want per-tenant **App-Actor Client Credentials** or installation tokens resolved per webhook, so that one deployment can serve agent sessions across many orgs.
19. As an operator, I want install-store lookup failures distinguished from "not installed", so that a transient store outage can retry while an uninstalled tenant is ignored.
20. As a future maintainer, I want token refresh to stay where ADR-0001 put it (adapter process, lazy refresh keyed by **Platform Tenant**), so that the **Install Store** returns durable install records, not live access tokens the runtime must refresh.
21. As an operator, I want `InstallStore.Lookup` called per event and not cached by the adapter, so that an uninstall or credential rotation takes effect as soon as my `Lookup` reflects it.
22. As an application developer, I want to cache inside my own `Lookup` implementation when I want to avoid a per-event store hit, so that I own caching and invalidation rather than the adapter.

## Implementation Decisions

- Keep the **Single-Install Adapter** as the default mode. The existing static-token construction path is unchanged; this is purely additive.
- Add a **Multi-Tenant Adapter** mode selected by supplying an **Install Store** in the adapter `Options` instead of a static token. The modes are mutually exclusive and validated at **Runtime Construction**.
- Define the **Install Store** as a narrow Go interface in the core `chat` package, consumed by adapters, implemented by application code. It is an **Optional Capability**, not a method on the core **Adapter** interface.
- Do not add credential storage to **Runtime State**. **Runtime State** stays coordination state (subscriptions, dedupe, locks). Install records live in the app's own storage, mirroring how **Thread Application State** stays app-owned.
- Reuse **Platform Tenant** scoping already baked into **Thread ID** and **Actor**. Do not introduce a new tenant identifier. The tenant string the adapter already produces is the **Install Store** lookup key.
- Lookup happens during webhook handling, after the adapter parses the **Platform Tenant** out of the **Supported Platform Shape** and before **Runtime Dispatch**.
- Carry the credential as an adapter-specific payload (`Install.Credential any`), a **Platform Escape Hatch** for credentials. Do not normalize a cross-platform token model. Each adapter documents and decodes the concrete type it requires.
- Resolve per-install signing secrets from the install record where the platform signs per-install. Where the platform signs with one app-level secret (Slack), keep the shared signing secret in adapter `Options` and use the install-record token only for replies. Document the verification ordering per adapter.
- Treat `ErrInstallNotFound` as an **Ignored Event**: acknowledge the platform, do not dispatch. Treat any other **Install Store** error as a transport failure the platform may retry.
- Resolve credentials the same way for **Thread Handle** reconstruction: decode the **Platform Tenant** from the **Thread ID**, call `InstallStore.Lookup`, then post.
- Keep **Application Identity** and account linking app-owned. The **Install Store** maps a **Platform Tenant** to platform credentials; it does not map a platform **Actor** to a product user.
- Keep the OAuth installation web flow (authorize redirect, callback, token exchange, install/uninstall lifecycle, encryption) entirely in application code. The runtime defines what an install record must contain to serve a webhook and nothing about how it is obtained.
- Call `InstallStore.Lookup` per inbound webhook; the adapter does not cache install records itself. An uninstall or credential rotation takes effect as soon as the app's `Lookup` reflects it. An app that wants to avoid a per-event store hit caches inside its own `Lookup` implementation, where it also owns invalidation.
- Keep token refresh where ADR-0001 placed it: the adapter refreshes lazily in process. The **Install Store** returns durable install records (e.g. client credentials or refresh material), and the adapter performs any near-expiry exchange before calling the platform, per tenant. Derived access tokens (e.g. the Linear **App-Actor Client Credentials** exchange) keep this lazy in-process refresh, but the cache is keyed by **Platform Tenant**; a revoked install stops resolving once its record is gone.
- Preserve the load-bearing patterns: single-slot **Routing Hooks**, opaque adapter-produced **Thread ID**, the small **Adapter** interface, and **Platform Escape Hatch** / **Optional Capability** over core widening. Multi-tenant adds an **Optional Capability**, not a new core method.
- Multi-tenant install is independent of deferred dispatch (ADR 0002), command/interaction events (ADR 0003), native content (ADR 0004), rate-limit handling (ADR 0005), and the Teams adapter (ADR 0007). This PRD does not redefine those decisions.

## Testing Decisions

- Tests assert external behavior and public contracts, not private credential plumbing.
- Construction tests cover: single-install (static token) still valid, multi-tenant (install store) valid, both supplied is a construction error, neither supplied is a construction error.
- Install-store lookup tests use a fake **Install Store** to cover: hit returns the right credential for the tenant, `ErrInstallNotFound` produces an **Ignored Event** (acknowledged, not dispatched), and a non-`ErrInstallNotFound` error surfaces as a retriable transport failure.
- Tenant-correctness tests prove the **Platform Tenant** extracted for lookup matches the tenant baked into the resulting **Thread ID** and **Actor**, and that two tenants delivering concurrent webhooks resolve distinct credentials and do not cross dedupe or lock scopes.
- Verification tests cover both signing models: per-install signing secret resolved from the install record, and shared app-level signing secret with the install-record token used only for the reply.
- Slack multi-tenant tests use golden payloads with distinct `team_id` values and a fake **Install Store** to prove per-workspace token selection on verify and post.
- Linear multi-tenant tests prove per-org credential resolution keyed by `organizationId`/`oauthClientId`, including an event for an uninstalled org becoming an **Ignored Event**.
- **Thread Handle** tests prove out-of-webhook posting resolves credentials through the **Install Store** for the tenant decoded from a stored **Thread ID**, and fails cleanly when the install record is gone.
- Self-message tests prove `BotActor` self-filtering is tenant-correct when the bot identity comes from the per-install record.
- Documentation tests or review checks confirm GoDoc/README state that multi-tenant is opt-in, that account linking and the OAuth installation flow stay app-owned, and that the **Install Store** is application-implemented.

## Out of Scope

- The OAuth installation web flow itself: authorize redirect, callback handler, `code` exchange, "add to Slack" / Linear install button, and uninstall webhooks. The app owns these HTTP routes and the runtime does not.
- A concrete **Install Store** implementation. The runtime ships the interface only; apps bring Postgres, Redis, KMS-backed, or any storage.
- Encrypted token storage and key management. App-owned, as before.
- **Application Identity** and account linking. Mapping a platform **Actor** to a product user stays app-owned, reaffirming the MVP non-goal even though multi-tenant install reopens the adjacent OAuth non-goal.
- Parked auth prompts, login orchestration, and pending-auth resume. App-owned.
- A normalized cross-platform credential/token model. Credentials ride as an adapter-specific **Platform Escape Hatch**.
- Credential storage in **Runtime State**. **Runtime State** stays coordination state.
- Background token-refresh schedulers. Refresh stays lazy and adapter-owned per ADR-0001.
- Install/uninstall lifecycle events, billing, seat counting, and tenant provisioning.
- A new tenant identifier. **Platform Tenant** in **Thread ID** and **Actor** is reused.
- Multiple **Install Stores** per adapter or dynamic adapter (re)registration at runtime. One store per **Multi-Tenant Adapter**, configured at construction.

## Further Notes

- This reopens three deliberately deferred non-goals (two from the MVP PRD Out of Scope, one from ADR-0001). The justification is that the runtime already carries **Platform Tenant** context everywhere; the missing piece is a per-tenant credential lookup, not a new identity model. Reopening is scoped to the runtime contract (the **Install Store** interface and the **Multi-Tenant Adapter** mode), explicitly not the OAuth web flow, token storage, or account linking, which stay app-owned. See ADR 0006.
- The split is deliberate: the runtime owns "given a tenant, the adapter can find the credential" (the contract); the application owns "how a tenant became installed and where its tokens live" (the store and the flow). This is the same line CONTEXT.md draws for **Application Identity** and **Thread Application State**.
- Slack's per-app signing secret means verification uses the shared secret while replies use the per-workspace bot token; Linear's per-org credentials mean both verification material and reply credentials can come from the install record. The adapter, not the runtime, knows which model applies, so the credential payload stays an adapter-specific **Platform Escape Hatch**.
- Linear's full-adapter expansion (ADR 0008) explicitly defers multi-tenant OAuth to this work item; the per-org credential resolution described here is the seam that expansion relies on.
- The strongest deep modules here are the **Install Store** contract, the adapter-internal tenant-extraction-before-verification step, and the per-tenant credential resolver shared between webhook handling and **Thread Handle** reconstruction.
