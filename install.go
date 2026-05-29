package chat

import (
	"context"
	"errors"
)

// InstallStore is the Optional Capability application code implements and a
// Multi-Tenant Adapter consumes during webhook handling and Thread Handle
// reconstruction to resolve per-Platform-Tenant credentials. It is NOT a method
// on the core Adapter interface and NOT a string registry: the runtime defines
// the contract; the application owns the store, its encryption, and the OAuth
// installation flow.
//
// Lookup is called per inbound webhook; the adapter does not cache install
// records, so an uninstall or credential rotation takes effect as soon as Lookup
// reflects it. An application that wants to avoid a per-event store hit caches
// inside its own Lookup, where it also owns invalidation.
type InstallStore interface {
	// Lookup returns the install record for one Platform Tenant under the named
	// adapter. The adapter parameter lets one application store serve multiple
	// adapters keyed by name. ErrInstallNotFound means the tenant is not installed
	// and the adapter treats the webhook as an Ignored Event (acknowledge, do not
	// dispatch). Any other error is a transport failure the platform may retry.
	Lookup(ctx context.Context, adapter, tenant string) (Install, error)
}

// Install is a durable, app-owned install record mapping one Platform Tenant to
// adapter-specific credentials. It is NOT a normalized cross-platform token model
// and NOT a live access token the runtime refreshes; the adapter performs any
// near-expiry exchange itself, keyed by Platform Tenant, per ADR-0001.
type Install struct {
	// Tenant is the Platform Tenant this record authorizes.
	Tenant string
	// Credential is an adapter-specific payload reached as a Platform Escape Hatch
	// for credentials (e.g. slack.SlackInstall, linear.LinearInstall). It is not a
	// normalized token model; each adapter documents and decodes the concrete type
	// it requires.
	Credential any
	// BotActorID is an optional pre-discovered bot identity used for tenant-correct
	// self-filtering. When empty the adapter falls back to any identity carried in
	// the adapter-specific Credential.
	BotActorID string
}

// ErrInstallNotFound signals that a Platform Tenant is not installed. A
// Multi-Tenant Adapter treats it as an Ignored Event during webhook handling
// (acknowledge, do not dispatch) and as a clean error on the out-of-webhook
// Thread Handle posting path.
var ErrInstallNotFound = errors.New("chat: install not found")
