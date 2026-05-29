// Package slack provides the Slack adapter for the chat runtime.
//
// # Multi-tenant installs (ADR 0006)
//
// Multi-tenant is opt-in. By default the adapter is a Single-Install Adapter with
// a static Options.BotToken serving one workspace, and that path is unchanged.
// Supplying Options.InstallStore instead of a BotToken selects Multi-Tenant
// Adapter mode, where one deployment serves many workspaces. Supplying both, or
// neither, is a construction error.
//
// The InstallStore is application-implemented: the runtime defines the contract
// (chat.InstallStore) and the app brings its own install storage, encryption, and
// OAuth installation flow. The OAuth installation web flow (the "add to Slack"
// button, authorize redirect, callback, and token exchange) and account linking /
// Application Identity stay app-owned and out of the adapter.
//
// During webhook handling the adapter verifies the signature with the shared
// app-level Options.SigningSecret (Slack signs per app, never per workspace), then
// resolves the per-workspace install via InstallStore.Lookup keyed by the
// team_id Platform Tenant. The install-record bot token is used only for the
// reply. A chat.ErrInstallNotFound from Lookup is an Ignored Event (acknowledged,
// not dispatched); any other Lookup error is a transport failure the platform may
// retry. Lookup runs per webhook; the adapter does not cache install records.
//
// The install credential rides as the adapter-specific slack.SlackInstall payload
// on chat.Install.Credential (a Platform Escape Hatch for credentials): the
// per-workspace bot token plus an optional bot user id used for tenant-correct
// self-filtering. Thread Handle reconstruction (out-of-webhook posting) resolves
// the same way, keyed by the Platform Tenant decoded from the opaque Thread ID.
//
// # Message history (ADR 0009)
//
// The adapter implements the HistoryReader Optional Capability, reached only via
// typed Adapter Access (chat.AdapterAs). ReadHistory is a thin live read-through of
// the Slack conversation read API (conversations.replies for thread-rooted Thread
// IDs, conversations.history for direct messages). It is storage-free: it performs
// no runtime storage, no dedupe, and no caching. Durable transcripts and LLM
// context are Thread Application State, owned by the application. Ordering is
// newest-first, the Before cursor is a Message.ID paging toward older messages, and
// the page-size limit is clamped to Slack's maximum.
//
// # Rate-limit retry (ADR 0005)
//
// Outbound posts are hardened against Slack throttling by default. The adapter
// wraps its Slack Web API call site (chat.postMessage / chat.postEphemeral /
// conversations.open) with bounded retry on a Slack 429 (honoring the Retry-After
// header, in seconds) and the ratelimited API error. Retry is bounded three ways:
// an attempt cap (Options.RetryPolicy.MaxAttempts), a cumulative backoff ceiling
// (MaxElapsed), and the caller's context deadline. The single load-bearing
// invariant is that in-line synchronous retry never sleeps past the caller's
// context deadline, so retry under the default DispatchSync stays inside Slack's
// 3-second ack window and cannot trigger a platform redelivery storm.
//
// The RetryPolicy is per-adapter platform config in Options, never Runtime
// Options. Its zero value is a conservative default that keeps MaxElapsed under
// the ack window, so retry is on out of the box; set MaxAttempts: 1 to disable it
// and get raw single-shot behavior. A throttle whose Retry-After does not fit the
// window, or exhausted retries, surface as a typed *slack.RateLimited error
// carrying the adapter name, last Retry-After, attempt count, and the raw platform
// response as a Platform Escape Hatch. Callers branch on it to defer (onto the
// ADR 0002 Detached Work Context under DispatchDeferred), drop, or notify rather
// than string-matching a generic error. Every attempt emits chat.ObsAdapterCall
// and every throttle emits chat.ObsRateLimit through the configured Observer (ADR
// 0010), with exhaustion additionally logged as a structured slog record; there is
// no global or cross-adapter limiter and no runtime-owned outbound queue.
package slack
