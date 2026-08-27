# ADR 0006: Multi-Tenant Install and OAuth

## Status

Accepted

## Context

Every adapter today is a **Single-Install Adapter**: one credential set for one **Platform Tenant**, resolved once during **Adapter Initialization**. The Slack adapter holds one `BotToken`; the Linear adapter holds one set of **App-Actor Client Credentials**. One deployment serves one workspace or one org. A product serving many customer workspaces from one deployment cannot, because there is no per-tenant credential lookup during webhook handling.

This was deferred deliberately and repeatedly. This ADR reopens these documented non-goals:

- **MVP PRD Out of Scope: "Multi-workspace Slack OAuth installation flow."** Reopened narrowly: we add per-tenant credential resolution, not the OAuth web flow.
- **MVP PRD Out of Scope: "Account linking, login orchestration, parked auth prompts, or Application Identity persistence."** Reopened only adjacently: install records are reopened; **Application Identity** and account linking stay closed and app-owned.
- **ADR-0001: excludes "multi-tenant OAuth installation storage."** Reopened as a runtime-defined contract; the store implementation stays app-owned. ADR-0001's lazy in-process token refresh is unchanged.

The reopening is justified because the runtime already carries **Platform Tenant** context inside every **Thread ID** and every **Actor** (CONTEXT.md: "**Thread ID** and **Actor** identities include **Platform Tenant** context"; "The **Slack-First Slice** ships as a **Single-Install Adapter** while preserving tenant-correct identifiers"). The MVP built tenant-correct identifiers precisely so multi-tenant support would not require a new identity model. The only missing piece is: when a webhook arrives for tenant `T`, which credential does the adapter use, and who owns the `T`-to-credential record.

The trap is scope. A naive reading pulls in an OAuth installation web flow, encrypted token storage, an install database, install/uninstall lifecycle, and account linking. Those are application and product concerns. CONTEXT.md already draws this line for **Application Identity** ("A **Go Chat Runtime** exposes **Actor** metadata but does not own **Application Identity** linking or login workflows") and for **Thread Application State** (app-owned storage). Multi-tenant credentials sit on the runtime side of one narrow contract and the app side of everything else.

## Decision

Keep the **Single-Install Adapter** as the default and add a **Multi-Tenant Adapter** mode plus an app-owned **Install Store** capability interface that the adapter calls during webhook handling to resolve per-**Platform Tenant** credentials. The runtime defines the contract; it does not own the store, the OAuth flow, or **Application Identity** / account linking.

Specifically:

- **Default unchanged.** The **Single-Install Adapter** with a static token stays the default. This ADR is purely additive; existing Slack and Linear deployments do not change.

- **Mode selection by capability, not flag.** A **Multi-Tenant Adapter** is the same `New(ctx, Options)` constructor given an `InstallStore` instead of a static token. Supplying both, or neither, is a **Runtime Construction** error. Fail-fast construction is preserved; no lazy first-webhook setup.

- **Install Store is an Optional Capability.** The core `chat` package defines a narrow Go interface the adapter consumes and application code implements. It is not a method on the core **Adapter** interface and not a string registry:

  ```go
  type InstallStore interface {
      Lookup(ctx context.Context, adapter, tenant string) (Install, error)
  }

  type Install struct {
      Tenant     string
      Credential any    // adapter-specific Platform Escape Hatch
      BotActorID string // optional pre-discovered bot identity
  }

  var ErrInstallNotFound = errors.New("chat: install not found")
  ```

- **Credentials ride as a Platform Escape Hatch.** `Install.Credential` is an adapter-specific payload, not a normalized cross-platform token model. The Slack adapter requires a bot token (and optionally a per-install signing secret); the Linear adapter requires per-org **App-Actor Client Credentials** or an installation token. Each adapter documents and decodes its concrete type like any other **Supported Platform Shape**.

- **Reuse Platform Tenant scoping; introduce no new identifier.** The tenant string the adapter already bakes into **Thread ID** and **Actor** is the **Install Store** lookup key. Dedupe, **Thread Lock**, subscriptions, and `BotActor` self-filtering are already tenant-correct.

- **Lookup ordering.** During webhook handling the adapter: (1) parses the **Platform Tenant** out of the **Supported Platform Shape**; (2) calls `InstallStore.Lookup`; (3) verifies the signature using install-record material where the platform signs per-install, or the shared app-level signing secret where it does not (Slack); (4) normalizes the tenant-scoped **Event** and hands it to **Runtime Dispatch**. On platforms with a per-install signing secret the tenant must be read from an unverified body for routing only, then re-validated by signature verification before any side effect.

  *Implementation note (as built):* the Slack adapter verifies the shared app-level signature **before** parsing the tenant or calling `Lookup`, so a Slack `InstallStore` only ever sees tenants from verified requests. The unverified-routing-read ordering above applies to per-install-signed platforms (Linear), where the store's tenant argument is untrusted routing input by necessity.

- **Not-installed is an Ignored Event.** `ErrInstallNotFound` is acknowledged to the platform without dispatch, consistent with CONTEXT.md's **Ignored Event** definition. Any other **Install Store** error is a transport failure the platform may retry.

- **Out-of-webhook posting uses the same resolver.** **Thread Handle** reconstruction decodes the **Platform Tenant** from the **Thread ID**, calls `InstallStore.Lookup`, and posts. A stored **Thread ID** stays postable while the app holds a valid install record.

- **Runtime State is not expanded.** Install records do not live in **Runtime State**, which stays coordination state. This mirrors ADR-0001's decision that adapter credential caches do not expand **Runtime State**.
- **Lookup is per-event; caching is app-owned.** The adapter calls `InstallStore.Lookup` on each inbound webhook and does not cache install records itself, so an uninstall or credential rotation takes effect as soon as the app's `Lookup` reflects it. An application that wants to avoid a per-event store hit caches inside its own `Lookup`, where it also owns invalidation. Derived access tokens (e.g. the Linear **App-Actor Client Credentials** exchange) keep ADR-0001's lazy in-process refresh, but the cache is keyed by **Platform Tenant**; a revoked install stops resolving once its record is gone.

- **Application Identity and the OAuth flow stay app-owned.** The **Install Store** maps a **Platform Tenant** to platform credentials. It does not map a platform **Actor** to a product user (that is **Application Identity**), and it does not run the authorize/callback/token-exchange web flow (that is an app HTTP route). Token refresh stays lazy and adapter-owned, per ADR-0001; the store returns durable install records, not live access tokens the runtime must refresh.

## Consequences

One deployment can serve many Slack workspaces and many Linear orgs. The application implements one small interface and brings its own install storage, encryption, and OAuth installation flow.

Single-install users are unaffected: the default path, construction, and credentials are unchanged.

The runtime stays small. Multi-tenant adds one **Optional Capability** interface and a credential-resolution step inside adapters, not a new core **Adapter** method, not a credential subsystem in **Runtime State**, and not an HTTP installation framework. The four load-bearing patterns hold: single-slot **Routing Hooks**, opaque adapter-produced **Thread ID**, the small **Adapter** interface, and **Platform Escape Hatch** / **Optional Capability** over core widening.

Deliberate divergences from the upstream Chat SDK and trade-offs:

- The runtime owns the credential-lookup *contract* but never the store, unlike full marketplace SDKs that ship installation persistence.
- The OAuth web flow is explicitly out: the app owns authorize/callback routes, matching how the runtime exposes only **Webhook Handlers** and owns no HTTP server.
- On per-install-signed platforms (Linear) the tenant is parsed from an unverified body before verification. This is a routing read only, re-validated by signature verification before side effects; adapters document the ordering. (On per-app-signed platforms — Slack — the implementation verifies the shared signature first, so lookup happens post-verification.)
- The credential payload is adapter-specific (`any`), trading a normalized token model for honesty about how differently Slack and Linear authorize.

Costs: the app must build and secure the **Install Store**; a misimplemented store (returning a stale or wrong-tenant credential) can post to the wrong workspace, so tenant-correctness tests are required. Adapters carry two construction modes to keep tested.

This composes cleanly with sibling ADRs: deferred dispatch (ADR 0002), command/interaction events (ADR 0003), native content (ADR 0004), and rate-limit handling (ADR 0005) are orthogonal. Linear's full-adapter expansion (ADR 0008) defers its multi-tenant OAuth to this ADR and relies on the per-org resolution defined here. The Teams adapter (ADR 0007) can adopt the same **Install Store** seam for multi-tenant Bot Framework installs.

## Alternatives Considered

### Bake an install database into the runtime

Rejected. It would put credential storage, encryption, and an install schema inside the runtime, contradicting CONTEXT.md's app-owned line for **Thread Application State** and **Application Identity**, and inflating the small core. The runtime defines the contract; the app owns the store.

### Add credential storage to Runtime State

Rejected. **Runtime State** is coordination state (subscriptions, dedupe, locks), and ADR-0001 already decided adapter credential caches do not expand it. Overloading it with per-tenant tokens blurs the coordination/credential boundary and forces every state backend to become a secrets store.

### Run the OAuth installation web flow in the runtime

Rejected. The runtime exposes **Webhook Handlers** and owns no HTTP server, router, or TLS. Authorize redirects, callbacks, and token exchange are ordinary application routes. Owning them would make the runtime a web framework and duplicate per-platform install UX that apps already build.

### Introduce a new tenant identifier for installs

Rejected. **Thread ID** and **Actor** already embed **Platform Tenant** context, specifically so multi-tenant would not need a new identity. A second identifier would risk drift between the install key and the tenant baked into stored thread IDs, invalidating existing records.

### Normalize a cross-platform credential/token model

Rejected. Slack authorizes with a per-workspace bot token under one app signing secret; Linear authorizes per org with client credentials or installation tokens. Forcing a single token struct hides these differences and leaks platform specifics into the core. The credential rides as an adapter-specific **Platform Escape Hatch** instead.

### Fold account linking into the Install Store

Rejected. Mapping a platform **Actor** to a product user is **Application Identity**, which CONTEXT.md keeps app-owned. An **Install Store** keyed by **Platform Tenant** answers "which credential serves this workspace", not "who is this user"; merging them would reopen a non-goal this ADR deliberately leaves closed.

### Make every adapter multi-tenant by default

Rejected. Most deployments are single-workspace, and the **Single-Install Adapter** is simpler and safer for them. Multi-tenant is opt-in via the **Install Store** so the common case keeps a baked-in token and fail-fast construction with no store dependency.
