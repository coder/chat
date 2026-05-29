// Package linear provides a Linear app-actor adapter for Chat SDK Go.
//
// The adapter participates as a Linear app-owned actor on App-Actor Client
// Credentials (a Single-Install Adapter). It supports two Linear interaction
// models, both reached through the small chat.Adapter interface and the opaque,
// versioned Thread ID:
//
//   - Agent sessions (ADR 0001, ADR 0008): the bot receives AgentSessionEvent
//     "created" / "prompted" webhooks and posts the five Linear agent activity
//     types — thought, response, action, elicitation, and error.
//   - Generic issue comments (ADR 0013): the bot receives ordinary Linear Comment
//     webhooks and posts ordinary issue comments. This requires enabling the
//     Linear Comment (and as needed Issue) webhook scope in addition to
//     agent-session events; an agent-session-only deployment is unchanged.
//
// The portable runtime surface is unchanged: Thread.Post creates an Agent
// Activity Response on agent-session threads and an ordinary comment on
// issue-comment threads, routing by the thread-kind discriminator carried inside
// the opaque Thread ID. Plain Text and Portable Markdown are the only portable
// bodies; there is no Markdown conversion layer.
//
// # Agent activity surface (ADR 0008)
//
// Beyond Thread.Post (response) and PostThought (ephemeral thought), the adapter
// exposes, all Linear-specific and reached through chat.AdapterAs:
//
//   - CreateAgentActivity: a generic escape hatch carrying a server-validated
//     content map, an optional signal and signal metadata, and the ephemeral
//     flag. Ephemeral is only valid for thought and action activities; any other
//     type with ephemeral set is rejected.
//   - PostAction, PostElicitation, PostError: typed helpers over
//     CreateAgentActivity. A session completes via response, elicitation, or
//     error; PostError ends a failed session cleanly.
//   - UpdateSession: set/replace externalUrls, add/remove URLs, and replace the
//     whole plan array. Setting externalUrls can keep a new session from being
//     marked unresponsive.
//   - GraphQL: a deliberate low-level escape hatch for preview Linear APIs. It
//     reuses the client-credentials token-refresh path, surfaces GraphQL errors,
//     and never exposes or returns the access token.
//
// # Agent Session Timing Contract (Ack-Then-Work)
//
// Linear expects a first Agent Activity Thought within ~10s of session creation
// (or the session may be marked unresponsive) and allows follow-up work for up to
// ~30 minutes. The adapter surfaces the first-thought deadline on the Linear
// Platform Escape Hatch (RawMessageFrom -> RawMessage.Session.FirstThoughtDeadline
// for "created" events); it does not run a watchdog. A handler should post a first
// thought (or set externalUrls via UpdateSession) inside the window, and run
// anything longer under chat.DispatchDeferred (chat.WithRuntimeOptions), where the
// adapter's posting methods are called from the Detached Work Context (ADR 0002)
// with the Thread Lock held and lease-refreshed by the runtime. The adapter
// consumes that primitive; it defines no Linear-private async path.
//
// # Platform Escape Hatch
//
// Inbound signal / signalMetadata (including the human-to-agent "stop" signal)
// and structured session context (promptContext, guidance, previousComments,
// issue, comment) are preserved verbatim on Message.Raw / Event.Raw as the stable
// linear.RawMessage shape, not lifted into the normalized core. Use RawMessageFrom
// to access it and StopRequested to detect a stop signal. The full original
// webhook body is preserved in RawMessage.Envelope.
//
// # Observation and rate limiting
//
// Outbound Linear rate-limit retry/backoff lives in the adapter (RetryPolicy,
// ADR 0005). It is bounded three ways -- an attempt cap (MaxAttempts), a cumulative
// backoff ceiling (MaxElapsed), and the request context deadline -- and honors
// Linear's Retry-After. The load-bearing invariant is that retry never sleeps past
// the request context deadline, so it cannot push the first Agent Activity Thought
// past the first-thought window (ADR 0008). The zero-value RetryPolicy is a
// conservative default (retry on by default); MaxAttempts: 1 disables it. Exhaustion,
// or a backoff that would exceed the deadline, returns a typed *RateLimited error
// carrying the adapter name, last Retry-After, attempt count, and the raw platform
// response as a Platform Escape Hatch, so a caller can defer the work onto the
// ADR 0002 Detached Work Context, drop, or notify. Every attempt emits
// chat.ObsAdapterCall and every observed throttle emits chat.ObsRateLimit through
// the configured Observer (ADR 0010), the same Observation Hook the Slack adapter
// feeds; exhaustion is additionally logged as a structured slog record. Timing,
// signal, ephemeral-rejection, and completion behavior are likewise reported as
// structured slog records on the adapter's logger.
//
// # Multi-tenant installs (ADR 0006)
//
// Multi-tenant is opt-in. By default the adapter is a Single-Install Adapter on
// static App-Actor Client Credentials serving one org, and that path is unchanged.
// Supplying Options.InstallStore instead of a WebhookSecret and ClientCredentials
// selects Multi-Tenant Adapter mode, where one deployment serves many Linear orgs.
// Supplying a static webhook secret or client credentials alongside an install
// store, or supplying none of them, is a construction error.
//
// The InstallStore is application-implemented: the runtime defines the contract
// (chat.InstallStore) and the app brings its own install storage, encryption, and
// OAuth installation flow. The OAuth installation web flow (authorize redirect,
// callback, token exchange, install button) and account linking / Application
// Identity stay app-owned and out of the adapter; the OAuth web flow remains
// deferred here.
//
// Linear signs webhooks per install and authorizes per org, so both the
// verification material and the reply credentials come from the install record.
// During webhook handling the adapter reads organizationId from the unverified
// body for routing only, calls InstallStore.Lookup keyed by that Platform Tenant,
// then re-validates by verifying the Linear-Signature with the install record's
// webhook secret before any side effect. A chat.ErrInstallNotFound from Lookup
// (e.g. an uninstalled org) is an Ignored Event (acknowledged, not dispatched);
// any other Lookup error is a transport failure the platform may retry. Lookup
// runs per webhook; the adapter does not cache install records.
//
// The install credential rides as the adapter-specific linear.LinearInstall
// payload on chat.Install.Credential (a Platform Escape Hatch for credentials):
// the per-install webhook secret plus the per-org App-Actor Client Credentials (or
// a pre-exchanged installation access token), and an optional app actor id for
// tenant-correct self-filtering. Derived access tokens keep ADR-0001's lazy
// in-process refresh, but the cache is keyed by Platform Tenant; a revoked install
// stops resolving once its record is gone. Thread Handle reconstruction
// (out-of-webhook posting) resolves the same way, keyed by the organizationId
// decoded from the opaque Thread ID, and fails cleanly when the install record is
// gone.
//
// # Still out of scope
//
// Multi-tenant OAuth installs (ADR 0006) reuse the per-org credential resolution
// above; the OAuth installation *web flow*, token streaming (ADR 0011), Markdown
// conversion, reactions, edit/delete, files, repository-suggestion ranking, and
// issue-workflow automation remain deferred; the latter are reachable through the
// GraphQL escape hatch. This is an app-owned actor, not a personal-API-key or
// user-OAuth user bot.
package linear
