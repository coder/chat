# Linear Agent Capabilities

Status: tracking document for the Linear adapter (`experimental`).

This document compares the Linear adapter against Linear's agent
documentation:

- Developing the Agent Interaction: https://linear.app/developers/agent-interaction
- Signals: https://linear.app/developers/agent-signals
- Interaction Best Practices: https://linear.app/developers/agent-best-practices
- Getting Started: https://linear.app/developers/agents

The adapter implements the full agent activity surface (ADR 0008), generic
issue/comment participation (ADR 0013), rate-limit retry (ADR 0005), and
multi-tenant installs (ADR 0006). Linear's Agent API is itself in Developer
Preview upstream and may change. The remaining gaps below are operations a
production-quality agent may need that currently require the `GraphQL` escape
hatch rather than typed helpers.

## Current Support

| Linear capability | Current support | Notes |
| --- | --- | --- |
| App actor auth with client credentials | Supported | Default scopes include `read`, `write`, `app:mentionable`, and `app:assignable`; startup verifies Linear granted all requested scopes. |
| Multi-tenant installs | Supported | Per-install webhook secrets and client credentials or pre-exchanged access tokens through `chat.InstallStore` (ADR 0006), with per-tenant lazy token refresh. |
| Agent session webhooks | Supported | Handles `AgentSessionEvent` `created` and `prompted`, including Linear-created assignment/delegation sessions. |
| Generic issue/comment participation | Supported | Comments that @-mention the app route to `OnNewMention` on comment-kind threads; `Thread.Post` replies as an issue comment (ADR 0013). |
| Inbox notification webhooks | Not normalized | Ignored by the adapter, matching upstream Chat SDK. |
| Mention-created sessions | Supported | Created sessions with `agentSession.comment` route to `OnNewMention`. |
| Delegation-created sessions | Supported | Created sessions without `agentSession.comment` route to `OnNewMention` using `promptContext` and session id fallbacks. |
| Follow-up prompts | Supported | Prompted events route according to runtime subscription state and read `agentActivity.body` with a content-body fallback. |
| Agent activities (all five content types) | Supported | `CreateAgentActivity` sends `thought`, `elicitation`, `action`, `response`, and `error` with `signal`, `signalMetadata`, and `ephemeral` (only `thought` and `action` may be ephemeral). |
| Typed activity helpers | Supported | `PostThought`, `PostAction`, `PostElicitation`, `PostError`; `Thread.Post` creates the `response` activity. |
| Agent-to-human signals | Supported | `auth` and `select` signals with metadata pass through `CreateAgentActivity` / `PostElicitation`. |
| Human-to-agent stop signal | Supported | `RawMessageFrom(ev.Message)` exposes `Signal` / `StopRequested()`; see the routing caveat below. |
| Session updates | Supported | `UpdateSession` sets `externalUrls` and replaces the session plan array. |
| GraphQL escape hatch | Supported | `GraphQL` (single-install) and `GraphQLForTenant` (multi-tenant) reuse adapter auth and token refresh, surface GraphQL errors, and never expose tokens. |
| Rate-limit handling | Supported | Bounded retry on HTTP 429 and GraphQL `RATELIMITED` with a typed `*linear.RateLimited` error (ADR 0005). |
| Thread reconstruction | Supported | Stored Linear `ThreadID`s (agent-session and comment kinds) reconstruct a `Thread` for later posting. |
| Tenant-correct thread identity | Supported | Opaque Linear thread ids include organization, issue, optional comment, and session ids. |
| Raw payload escape hatch | Supported | `RawMessage` preserves kind, action, session context, signal, signal metadata, source comment, and the full webhook envelope. |

## Missing Capabilities To Track

### 1. Proactive Agent Session Creation

**Status:** Missing typed helpers; possible via `GraphQL`.

Linear supports creating sessions when the agent was not mentioned or
delegated (`agentSessionCreateOnIssue`, `agentSessionCreateOnComment`).
A typed helper should return a session convertible into this adapter's opaque
`ThreadID`, with tests proving the created session can be posted to with
`Thread.Post` and `PostThought`.

### 2. Repository Suggestions

**Status:** Missing typed helpers; possible via `GraphQL`.

Linear exposes `issueRepositorySuggestions` for ranking candidate
repositories. A helper should cover the candidate input shape, returned
suggestions (hostname, repository full name, confidence), and pairing low
confidence with a `select` elicitation.

### 3. Conversation History Through Agent Activities

**Status:** Missing.

Linear recommends using Agent Activities for session conversation history.
There is no typed way to query/list activities for a session. Relatedly, the
Linear adapter does not implement the cross-platform `chat.HistoryReader`
Optional Capability (the Slack adapter does).

### 4. Issue Workflow Best Practices

**Status:** Missing typed helpers; possible via `GraphQL`.

Linear's best practices recommend moving delegated issues to a `started`
workflow state when work begins and setting the agent as `Issue.delegate`.
This likely belongs in a higher-level helper package or example workflow, not
the core adapter.

### 5. Stop Handling Versus Thread Serialization

**Status:** Inherent limitation; needs an application-owned pattern.

The `stop` signal arrives as a prompted event on the same thread, so it is
serialized behind the thread lock like any other event: it cannot preempt a
handler that is already running (`ConcurrencyDrop` discards it during a
conflict; `ConcurrencyQueue` delivers it only after the in-flight handler
returns). Active cancellation needs an application-owned signal — for
example, a per-thread cancellation flag in your own store that long-running
handlers poll. A documented example is still to be written.

### 6. Best-Practice Webhook Categories

**Status:** Partial.

The adapter does not normalize Inbox Notification or Permission Change
webhooks. Assignment/delegation enters the runtime through Linear's
`AgentSessionEvent` `created` webhook.

Setup footgun: if direct mentions create sessions but assignment/delegation
does not, reinstall the app actor after confirming `app:assignable` is in the
authorization URL. Linear can keep stale install/app state after scope
changes; during dogfooding we had to delete and recreate the OAuth app before
assignment-created sessions started arriving.

Upstream Vercel Chat SDK precedent, checked on May 13, 2026: its Linear
adapter registers handlers for `OAuthApp` revocation, `Comment`,
`AgentSessionEvent`, and `Reaction`, and has no normalized callbacks for
Inbox Notification or Permission Change payloads. This adapter follows that
model. Reaction webhooks are not normalized here either.

### 7. UX Example Coverage

**Status:** Docs gap.

The mechanics for auth elicitation (`signalMetadata.url`, optional `userId`),
select elicitation, and PR/dashboard links via `externalUrls` are all
implemented, but worked examples (including the follow-up behavior after a
user completes auth or makes a selection) are still to be written.

## Proposed Next Implementation Slice

1. Typed proactive session creation returning an opaque `ThreadID`.
2. `chat.HistoryReader` parity (or a Linear-specific activity-history query).
3. Repository suggestions helper.
4. Worked examples for auth/select elicitations, external URLs, and
   application-owned stop handling.
