# Linear Agent Capabilities

Status: tracking document for the Linear app-actor adapter.

This document compares the Linear app-actor adapter against Linear's current agent documentation:

- Developing the Agent Interaction: https://linear.app/developers/agent-interaction
- Signals: https://linear.app/developers/agent-signals
- Interaction Best Practices: https://linear.app/developers/agent-best-practices
- Getting Started: https://linear.app/developers/agents

The adapter currently implements the minimum runtime slice needed to receive Linear `AgentSessionEvent` webhooks and respond in agent sessions. A production-quality Linear agent integration needs more of Linear's agent APIs, either through first-class typed helpers or through a deliberate GraphQL escape hatch.

## Current Support

| Linear capability | Current support | Notes |
| --- | --- | --- |
| App actor auth with client credentials | Supported | Default scopes include `read`, `write`, `comments:create`, `issues:create`, `app:mentionable`, and `app:assignable`. |
| Agent session webhooks | Partially supported | Handles `AgentSessionEvent` `created` and `prompted`, including Linear-created assignment/delegation sessions. |
| Inbox notification webhooks | Not normalized | Ignored by the adapter, matching upstream Chat SDK. |
| Mention-created sessions | Supported | Created sessions with `agentSession.comment` route to `OnNewMention`. |
| Delegation-created sessions | Supported | Created sessions without `agentSession.comment` route to `OnNewMention` using `promptContext` and session id fallbacks. |
| Follow-up prompts | Supported | Prompted events route according to runtime subscription state and read `agentActivity.body` with a content-body fallback. |
| First thought / acknowledgement | Supported narrowly | `Adapter.PostThought` creates an ephemeral `thought` activity. |
| Final response | Supported narrowly | `Thread.Post` creates a `response` activity. |
| Thread reconstruction | Supported | Stored Linear agent-session `ThreadID`s can reconstruct a `Thread` for later posting. |
| Tenant-correct thread identity | Supported | Opaque Linear thread ids include organization, issue, optional comment, and session ids. |

## Missing Capabilities To Track

### 1. General GraphQL Escape Hatch

**Status:** Missing.

The adapter should expose a deliberate low-level GraphQL method that reuses adapter authentication, token refresh, API base URL, HTTP client, and GraphQL error handling without exposing raw tokens.

Candidate shape:

```go
func (a *Adapter) GraphQL(ctx context.Context, query string, variables any, dest any) error
```

Why this matters:

- Linear's Agent APIs are in Developer Preview and may change.
- The agent docs explicitly point developers to the GraphQL schema explorer and raw SDL for webhook/API types.
- A serious agent needs to call APIs before the Go adapter has typed wrappers for every agent operation.

Acceptance notes:

- Must use the same client-credentials token refresh path as internal calls.
- Must surface GraphQL `errors` clearly.
- Must not expose or return the access token.
- Should be documented as a Linear-specific escape hatch, not a cross-platform API.

### 2. Generic Agent Activity Creation

**Status:** Missing, except for `PostThought` and `Thread.Post` response.

Linear allows five agent-emitted activity content types:

- `thought`
- `elicitation`
- `action`
- `response`
- `error`

The adapter should expose a generic `CreateAgentActivity` / `PostActivity` escape hatch that can send all server-validated content shapes, signals, signal metadata, and the `ephemeral` flag.

Candidate shape:

```go
type AgentActivityInput struct {
    Content        map[string]any
    Signal         string
    SignalMetadata any
    Ephemeral      bool
}

func (a *Adapter) CreateAgentActivity(ctx context.Context, threadID chat.ThreadID, input AgentActivityInput) (*chat.SentMessage, error)
```

Why this matters:

- `action` activities are needed for native Linear tool-call progress.
- `elicitation` activities are needed to ask users questions.
- `error` activities are needed to end failed sessions properly.
- Only `thought` and `action` may be ephemeral, so validation or docs should make that clear.

### 3. Typed Activity Convenience Helpers

**Status:** Mostly missing.

Convenience helpers can wrap generic activity creation after the escape hatch exists:

- `PostThought` (already present)
- `PostAction`
- `PostElicitation`
- `PostError`
- possibly `PostResponse` for callers that want to bypass `Thread.Post`

These should stay Linear-specific through `Adapter Access` until there is evidence that a cross-platform abstraction is useful.

### 4. Agent-to-Human Signals

**Status:** Missing.

Linear supports signals on agent-created activities. The docs currently call out:

- `auth` signal on `elicitation`, with `signalMetadata.url`, optional `userId`, and optional `providerName`.
- `select` signal on `elicitation`, with selectable options.

Needed support:

- Generic `Signal` and `SignalMetadata` fields in the activity escape hatch.
- Examples for auth/account linking.
- Examples for select/choice elicitation.

### 5. Human-to-Agent Stop Signal

**Status:** Missing.

Linear can send a `stop` signal on user-generated `prompt` activities. The adapter currently normalizes prompted events to a text message but does not expose signal metadata.

Needed support:

- Preserve inbound `agentActivity.signal` and `signalMetadata` in `Message.Raw` or a typed Linear raw-message struct.
- Document how application code should detect a stop request.
- Recommend halting work and emitting a final `response` or `error` activity.

### 6. Agent Session Updates

**Status:** Missing.

Linear uses `agentSessionUpdate` for session-level metadata.

Needed operations:

- Set or replace `externalUrls`.
- Add/remove external URLs.
- Publish pull request or dashboard links through `externalUrls`.
- Update the full session plan array.

Candidate helpers:

```go
func (a *Adapter) UpdateSession(ctx context.Context, threadID chat.ThreadID, input AgentSessionUpdateInput) error
func (a *Adapter) SetExternalURLs(ctx context.Context, threadID chat.ThreadID, urls []ExternalURL) error
func (a *Adapter) UpdatePlan(ctx context.Context, threadID chat.ThreadID, plan []PlanStep) error
```

Important Linear behavior:

- Setting `externalUrls` can prevent a new session from being marked unresponsive.
- Plan updates replace the full plan array; they do not patch one plan item.

### 7. Proactive Agent Session Creation

**Status:** Missing.

Linear supports creating sessions when the agent was not mentioned or delegated:

- `agentSessionCreateOnIssue`
- `agentSessionCreateOnComment`

Needed support:

- Typed helpers or documented GraphQL examples.
- Returned session should be convertible into this adapter's opaque Linear `ThreadID`.
- Tests should prove a proactively-created session can be posted to with `Thread.Post` and `PostThought`.

### 8. Repository Suggestions

**Status:** Missing.

Linear exposes `issueRepositorySuggestions` for ranking candidate repositories using issue, session, guidance, and Linear signals.

Needed support:

- Typed helper or GraphQL example.
- Candidate repository input shape.
- Returned suggestions with hostname, repository full name, and confidence.
- Example of using suggestions with a `select` elicitation when confidence is low.

### 9. Prompt Context and Structured Session Context

**Status:** Partial.

The adapter now uses `promptContext` as fallback text for delegation-created sessions. It does not expose structured fields beyond raw webhook data.

Needed support:

- Preserve `promptContext`, `guidance`, `previousComments`, `agentSession.issue`, and `agentSession.comment` in a stable Linear raw-message escape hatch.
- Document how application code should build an LLM prompt from these fields.
- Consider a typed accessor for Linear agent-session event metadata.

### 10. Conversation History Through Agent Activities

**Status:** Missing.

Linear recommends using Agent Activities for session conversation history rather than relying on editable comments alone.

Needed support:

- Query/list activities for a session.
- Convert activity history into an application-friendly representation.
- Preserve prompt, thought, action, elicitation, response, error, signal, and metadata fields.

### 11. Issue Workflow Best Practices

**Status:** Missing.

Linear's best practices recommend workflow updates when an agent starts work.

Needed operations:

- Query the issue's team workflow states filtered to `started` statuses.
- Move delegated issues to the first `started` status when work begins if not already started/completed/canceled.
- If the agent is working on implementation and no `Issue.delegate` is set, set itself as delegate.

This likely belongs in a higher-level Linear agent helper package or example workflow, not the core adapter, but the GraphQL escape hatch must make it possible.

### 12. Best-Practice Webhook Categories

**Status:** Partial.

The adapter does not normalize Inbox Notification or Permission Change webhooks. Assignment/delegation should enter the runtime through Linear's `AgentSessionEvent` `created` webhook: Linear creates the agent session automatically when the app actor is delegated an issue, and follow-up chat arrives as `AgentSessionEvent` `prompted`.

Upstream Vercel Chat SDK precedent, checked on May 13, 2026:

- `@chat-adapter/linear` documents app-actor mode as driven by `AgentSessionEvent` and asks webhook setup to enable Comments, Agent session events, Issues, and optional Emoji reactions.
- Its adapter imports Linear webhook types for `AgentSessionEvent`, `Comment`, and `Reaction` and registers handlers for `OAuthApp` revocation, `Comment`, `AgentSessionEvent`, and `Reaction`.
- It does not register `AppUserNotification` or `PermissionChange` handlers, and it has no normalized callbacks for Inbox Notification or Permission Change payloads.
- Its `AgentSessionEvent` parser handles `prompted` and `created`; this Go adapter follows that routing model while tolerating created assignment/delegation payloads that omit `agentSession.comment` by using `promptContext` and session ID fallbacks.

Needed support:

- Keep Inbox Notification actions ignored until the adapter has a raw Linear webhook callback or a typed event with a clear runtime semantic.
- Keep Permission Change webhooks ignored until the adapter has a Linear-specific callback for installation/team-access changes; they do not map cleanly to normalized chat messages.
- Preserve enough raw payload data for advanced agents to react to permission and notification changes through future adapter extensions.

### 13. Account Linking / Auth Flow UX

**Status:** Missing.

A first-class Linear agent often needs to prompt the user to link an external account.

Needed support:

- `elicitation` + `auth` signal helper or example.
- `signalMetadata.url` support.
- Optional target `userId` support.
- Follow-up behavior after the user completes auth, usually emitting a new `thought` and continuing work.

### 14. Select / Choice UX

**Status:** Missing.

A first-class agent needs user choices for ambiguous decisions, such as selecting repositories or confirming an action.

Needed support:

- `elicitation` + `select` signal helper or example.
- Option metadata shape once confirmed against the GraphQL schema.
- Handling the prompted follow-up generated by the user's selection.

### 15. PR / External Work Links

**Status:** Missing.

Agents should link users to external work surfaces, especially pull requests or dashboards.

Needed support:

- `externalUrls` session update helper or documented GraphQL example.
- Example for publishing a pull request URL.
- Guidance that the URL list is replaced as a whole unless using add/remove fields.

## Proposed Next Implementation Slice

The next slice should focus on escape hatches before building many typed wrappers:

1. Public `GraphQL` method on the Linear adapter.
2. Public generic `CreateAgentActivity` method with `content`, `signal`, `signalMetadata`, and `ephemeral` support.
3. Preserve inbound `signal`, `signalMetadata`, and `promptContext` in a stable Linear raw-message shape.
4. Add README examples for:
   - `agentSessionUpdate` external URLs.
   - plan updates.
   - auth elicitation.
   - select elicitation.
   - repository suggestions.
   - proactive session creation.
5. Add tests for GraphQL auth reuse, generic activity payload pass-through, signal preservation, and docs examples.

After that escape-hatch slice is dogfooded, add typed convenience helpers for the most common operations.
