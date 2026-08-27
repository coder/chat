// Package msteams provides the Microsoft Teams (Bot Framework) adapter for the chat
// runtime, behind the same chat.Adapter interface as Slack and Linear.
//
// # Spike status
//
// This adapter is a SPIKE implementing ADR 0007. Its behavior is exercised end to
// end against fake Bot Framework servers, but it has NOT been validated against a
// real Azure Bot resource and a live Teams tenant. The ADR's Open Questions remain
// the live-validation checklist; the spike encodes the documented behavior and
// marks the unverified assumptions inline. Treat it as ready for a human to wire to
// a real bot and confirm, not as production-proven.
//
// # Boundary shape
//
// Teams does not look like Slack/Linear at the boundary, and the adapter absorbs the
// differences so the runtime and core types stay unchanged:
//
//   - Inbound auth is a per-request JWT validated against the Bot Connector JWKS
//     (fetch + cache + endorsements), not a shared-secret HMAC. Every documented
//     check is enforced and none can be disabled (see auth.go). JWT/JWKS handling is
//     stdlib-only (crypto/rsa over a key rebuilt from the JWK n/e), so the
//     zero-dependency core gains no JWT library -- a deliberate spike finding for
//     Open Question 9 (msbotbuilder-go is not adopted; golang-jwt is not needed).
//   - Outbound replies are separate authenticated Connector REST calls
//     (client_credentials token, cached in process memory, lazily refreshed), not a
//     webhook response body. Runtime State is never expanded to store credentials.
//   - The opaque Thread ID serializes the minimal conversationReference
//     (serviceUrl, conversation id, tenant id, bot id, channel id) so out-of-webhook
//     and proactive posting survive process restarts; serviceUrl is refreshed from
//     each inbound Activity. This is heavier than Slack's id but exactly what the
//     opaque, adapter-produced Thread ID contract permits.
//
// # Inbound turn / dispatch
//
// The Webhook validates the JWT, normalizes a message Activity into an Event, runs
// Runtime Dispatch synchronously on the request context, then acks with HTTP 200.
// The handler's reply goes out as a separate Connector call via Thread.Post during
// dispatch; there is no body-reply shortcut for message activities. Non-message
// activity types and the bot's own messages are Ignored Events. invoke activities
// (command/card-action transport) are out of this slice. Work exceeding the Teams
// turn budget (~10-15s) is the runtime's Ack-Then-Work concern (ADR 0002), delivered
// later as a proactive Connector post through a stored Thread ID.
//
// # Normalization
//
// Event.Adapter is msteams; Event.Tenant is conversation.tenantId; Event.ID is the
// Activity id; Event.Raw and Message.Raw hold the full Activity (Platform Escape
// Hatch). Message.Text has the leading bot mention stripped; Message.Mentioned is
// derived from Activity.entities mention objects (never substring text matching);
// the inbound Actor comes from Activity.from (Bot Kind human) and BotActor() from
// the configured bot identity (Bot Kind bot). DirectMessage reflects personal scope.
//
// # Capability boundaries (deferred, cross-referenced)
//
// Adaptive Card native content (ADR 0004 Native Content), Teams card actions and
// command-style invokes (ADR 0004 Interaction Event / ADR 0003 Command Event),
// multi-tenant install (ADR 0006), message history via Graph (ADR 0009), and an
// EphemeralPoster (no clean Teams equivalent) are intentionally NOT implemented
// here; this slice is a Single-Install Adapter with Plain Text and Portable Markdown
// as the portable posting surface. Outbound rate-limit retry (ADR 0005) IS wired,
// reusing the shared internal/ratelimit mechanics, surfaced through the Observer.
package msteams
