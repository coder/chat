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
package slack
