# Linear Agent Capabilities

What the Linear adapter (`experimental`) supports today, and which Linear
agent APIs it does not wrap yet.

The adapter implements the full agent activity surface
([ADR 0008](adr/0008-linear-full-adapter.md)), generic issue and comment
participation ([ADR 0013](adr/0013-linear-generic-comments.md)), rate-limit
retry ([ADR 0005](adr/0005-rate-limit-handling.md)), and multi-tenant
installs ([ADR 0006](adr/0006-multi-tenant-install.md)). Linear's agent API
is itself in developer preview and may change. The remaining gaps are
operations a production agent may need that currently go through the
`GraphQL` escape hatch instead of a typed helper.

Linear's agent documentation:
[Getting started](https://linear.app/developers/agents),
[agent interaction](https://linear.app/developers/agent-interaction),
[signals](https://linear.app/developers/agent-signals), and
[best practices](https://linear.app/developers/agent-best-practices).

## Current Support

| Linear capability | Current support | Notes |
| --- | --- | --- |
| App actor auth with client credentials | Supported | Default scopes include `read`, `write`, `app:mentionable`, and `app:assignable`; startup verifies Linear granted all requested scopes. |
| Multi-tenant installs | Supported | Per-install webhook secrets and client credentials or pre-exchanged access tokens through `chat.InstallStore` (ADR 0006). Per-tenant lazy token refresh applies to client-credential installs only; a pre-exchanged `AccessToken` is used as-is until the install store supplies a replacement. |
| Agent session webhooks | Supported | Handles `AgentSessionEvent` `created` and `prompted`, including Linear-created assignment/delegation sessions. |
| Generic issue/comment participation | Supported | Comments that @-mention the app arrive on comment-kind threads (routing to `OnNewMention` while unsubscribed); `Thread.Post` replies as an issue comment (ADR 0013). |
| Inbox notification webhooks | Not normalized | Ignored by the adapter, matching upstream Chat SDK. |
| Mention-created sessions | Supported | Created sessions with `agentSession.comment` route to `OnNewMention` (on unsubscribed threads; normal routing precedence applies — a subscribed thread routes everything to `OnSubscribedMessage`). |
| Delegation-created sessions | Supported | Created sessions without `agentSession.comment` route to `OnNewMention` using `promptContext` and session id fallbacks. |
| Follow-up prompts | Supported | Prompted events route according to runtime subscription state and read `agentActivity.body` with a content-body fallback. |
| Agent activities (all five content types) | Supported | `CreateAgentActivity` sends `thought`, `elicitation`, `action`, `response`, and `error` with `signal`, `signalMetadata`, and `ephemeral` (only `thought` and `action` may be ephemeral). |
| Typed activity helpers | Supported | `PostThought`, `PostAction`, `PostElicitation`, `PostError`; `Thread.Post` creates the `response` activity. |
| Agent-to-human signals | Supported | `auth` and `select` signals with metadata pass through `CreateAgentActivity` / `PostElicitation`. |
| Human-to-agent stop signal | Supported | `RawMessageFrom(ev.Message)` exposes `Signal` / `StopRequested()`; see the routing caveat below. |
| Session updates | Supported | `UpdateSession` replaces `externalUrls` or adjusts them with `AddExternalURLs` / `RemoveExternalURLs`, and replaces the session plan array. |
| GraphQL escape hatch | Supported | `GraphQL` (single-install) and `GraphQLForTenant` (multi-tenant) reuse adapter auth and token refresh, surface GraphQL errors, and never expose tokens. |
| Proactive agent session creation | Supported | `CreateSessionOnIssue` / `CreateSessionOnComment` (plus `ForTenant` variants) wrap `agentSessionCreateOnIssue` / `agentSessionCreateOnComment`; the returned `CreatedAgentSession` carries the adapter's opaque `ThreadID` ([#47](https://github.com/coder/chat/issues/47)). |
| Repository suggestions | Supported | `SuggestRepositories` wraps `issueRepositorySuggestions` with typed candidates and confidence-scored results ([#48](https://github.com/coder/chat/issues/48)). |
| Worked UX examples | Supported | Auth/select elicitation loops, `externalUrls` updates, stop handling, proactive sessions, and repository suggestions are worked through in [`docs/how-to/linear-agent-sessions.md`](how-to/linear-agent-sessions.md), extracted from the tested runnable example ([#49](https://github.com/coder/chat/issues/49)). |
| Rate-limit handling | Supported | Bounded retry on HTTP 429 and GraphQL `RATELIMITED` with a typed `*linear.RateLimited` error (ADR 0005). |
| Message history read-through | Supported | `chat.HistoryReader` reads agent-session activities and issue-comment threads, newest-first with `Before` paging (ADR 0009). |
| Thread reconstruction | Supported | Stored Linear `ThreadID`s (agent-session and comment kinds) reconstruct a `Thread` for later posting. |
| Tenant-correct thread identity | Supported | Opaque Linear thread ids include organization, issue, optional comment, and session ids. |
| Raw payload escape hatch | Supported | `RawMessage` preserves kind, action, session context, signal, signal metadata, source comment, and the full webhook envelope. |

## Missing Capabilities To Track

### 1. Issue Workflow Best Practices

**Status:** Missing typed helpers; possible via `GraphQL`.

Linear's best practices recommend moving delegated issues to a `started`
workflow state when work begins and setting the agent as `Issue.delegate`.
This likely belongs in a higher-level helper package or example workflow, not
the core adapter.

### 2. Stop Handling Versus Thread Serialization

**Status:** Inherent limitation; needs an application-owned pattern.

The `stop` signal arrives as a prompted event on the same thread, so the
thread lock serializes it like any other event and it cannot preempt a
running handler. The
[Linear agent sessions guide](how-to/linear-agent-sessions.md#use-the-full-activity-surface)
explains the limitation and the workable patterns (short handler turns that
check `StopRequested`, or an out-of-band cancellation flag).

### 3. Best-Practice Webhook Categories

**Status:** Partial.

The adapter does not normalize Inbox Notification or Permission Change
webhooks. Assignment/delegation enters the runtime through Linear's
`AgentSessionEvent` `created` webhook.

If mentions create sessions but assignment or delegation does not, see the
fix in the
[example's app setup](../examples/linear-agent-hello-world/README.md#linear-app-setup).

Upstream Vercel Chat SDK precedent, checked on May 13, 2026: its Linear
adapter registers handlers for `OAuthApp` revocation, `Comment`,
`AgentSessionEvent`, and `Reaction`, and has no normalized callbacks for
Inbox Notification or Permission Change payloads. This adapter follows that
model. Reaction webhooks are not normalized here either.

## Planned Work

Future work is planned in [GitHub issues](https://github.com/coder/chat/issues),
not in this document. The former gaps for proactive sessions
([#47](https://github.com/coder/chat/issues/47)), repository suggestions
([#48](https://github.com/coder/chat/issues/48)), and worked UX examples
([#49](https://github.com/coder/chat/issues/49)) have shipped; see the table
above.
