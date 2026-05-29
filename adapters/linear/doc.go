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
// Outbound Linear rate-limit retry/backoff lives in the adapter (RetryPolicy). It
// is bounded by attempt count and cumulative backoff, honors Linear's Retry-After,
// and never sleeps past the request context deadline so it cannot violate the
// first-thought window; exhaustion returns a typed *RateLimited error. Timing,
// signal, ephemeral-rejection, completion, and rate-limit behavior are reported as
// structured slog records on the adapter's logger, the Runtime Observation surface
// available today (a dedicated cross-adapter Observation Hook is ADR 0010, out of
// scope here).
//
// # Still out of scope
//
// Multi-tenant OAuth installs (ADR 0006), token streaming (ADR 0011), Markdown
// conversion, reactions, edit/delete, files, repository-suggestion ranking, and
// issue-workflow automation remain deferred; the latter are reachable through the
// GraphQL escape hatch. This is an app-owned actor, not a personal-API-key or
// user-OAuth user bot.
package linear
