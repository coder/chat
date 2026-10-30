# linear

## Overview

Package `linear` (`github.com/coder/chat/adapters/linear`) is the Linear app-actor adapter for the `chat` runtime (root package `github.com/coder/chat`). The bot acts as a Linear app-owned actor on App-Actor Client Credentials. [doc.go:linear]

It supports two Linear interaction models, both reached through `chat.Adapter` and an opaque, versioned Thread ID: [doc.go:linear]

1. Agent sessions. The bot receives `AgentSessionEvent` `created` and `prompted` webhooks. It posts the five Linear agent activity types: thought, response, action, elicitation, and error.
2. Issue comments. The bot receives Linear `Comment` webhooks and posts ordinary issue comments.

The adapter runs in one of two modes. Single-install mode uses a static `Options.WebhookSecret` and `Options.ClientCredentials` for one organization. Multi-tenant mode uses `Options.InstallStore` (`chat.InstallStore`) and reads the webhook secret and credentials from the install record of each organization. [doc.go:linear], [linear.go:New]

The adapter owns its own rate-limit retry (`RetryPolicy`) and Linear-specific extras (agent activities, session updates, proactive sessions, repository suggestions, and a raw GraphQL escape hatch). Callers reach the extras through `chat.AdapterAs`. [doc.go:linear]

## Interface

### Construction and types

- `New` validates `Options`, sets defaults, and returns an `Adapter`. [linear.go:New]
- `Options` holds `WebhookSecret`, `ClientCredentials`, `InstallStore`, `APIBaseURL`, `Client`, `Now`, `SignatureTolerance`, `Logger`, `RetryPolicy`, and `Observer`. [linear.go:Options]
- `ClientCredentials` holds `ClientID`, `ClientSecret`, and `Scopes`. [linear.go:ClientCredentials]
- `LinearInstall` is the install credential (`WebhookSecret`, `ClientCredentials`, `AccessToken`, `BotUserID`) carried on `chat.Install.Credential`. [linear.go:LinearInstall]
- `Adapter` is the Linear adapter. It implements `chat.Adapter` and `chat.HistoryReader`. [linear.go:Adapter], [history.go:HistoryReader assertion]
- `RetryPolicy` sets `MaxAttempts`, `MaxElapsed`, `BaseDelay`, and `MaxDelay`. [retry.go:RetryPolicy]
- `RateLimited` is the error returned when throttling retry stops (`Adapter`, `RetryAfter`, `Attempts`, `Raw`, `Err`). [retry.go:RateLimited]

### chat.Adapter methods

- `Adapter.Name` returns `"linear"`. [linear.go:Name]
- `Adapter.Init` exchanges a token and fetches the viewer identity in single-install mode. It does nothing in multi-tenant mode. [linear.go:Init]
- `Adapter.Shutdown` returns nil. [linear.go:Shutdown]
- `Adapter.Webhook` returns the `http.Handler` for inbound Linear webhooks, for a `chat.DispatchFunc`. [linear.go:Webhook]
- `Adapter.ValidateThreadID` decodes a Thread ID into a `chat.ThreadRef`. [linear.go:ValidateThreadID]
- `Adapter.PostMessage` posts an issue comment on a comment thread, or a response activity on an agent-session thread. [linear.go:PostMessage]
- `Adapter.BotActor` returns the bot as a `chat.Actor`. [linear.go:BotActor]
- `Adapter.ReadHistory` reads agent activities or comment replies, newest first. [history.go:ReadHistory]

### Agent activities

- `Adapter.PostThought` posts an ephemeral thought activity. [linear.go:PostThought]
- `Adapter.CreateAgentActivity` posts any agent activity from an `AgentActivityInput`. [activity.go:CreateAgentActivity,AgentActivityInput]
- `Adapter.PostAction` posts an action activity from an `ActionInput`. [activity.go:PostAction,ActionInput]
- `Adapter.PostElicitation` posts an elicitation activity from an `ElicitationInput`. [activity.go:PostElicitation,ElicitationInput]
- `Adapter.PostError` posts an error activity from an `ErrorInput`. [activity.go:PostError,ErrorInput]
- `AuthSignalMetadata`, `SelectSignalMetadata`, and `SelectOption` are elicitation signal metadata types. [activity.go:AuthSignalMetadata,SelectSignalMetadata,SelectOption]

### Sessions, suggestions, and GraphQL

- `Adapter.UpdateSession` sets external URLs and the plan from an `AgentSessionUpdateInput`. [session.go:UpdateSession,AgentSessionUpdateInput]
- `ExternalURL` and `PlanStep` are session update types. [session.go:ExternalURL,PlanStep]
- `Adapter.CreateSessionOnIssue` and `Adapter.CreateSessionOnIssueForTenant` create a proactive session on an issue. [session.go:CreateSessionOnIssue,CreateSessionOnIssueForTenant]
- `Adapter.CreateSessionOnComment` and `Adapter.CreateSessionOnCommentForTenant` create a proactive session on a comment. [session.go:CreateSessionOnComment,CreateSessionOnCommentForTenant]
- `CreateSessionOnIssueInput`, `CreateSessionOnCommentInput`, and `CreatedAgentSession` are the proactive session types. [session.go:CreateSessionOnIssueInput,CreateSessionOnCommentInput,CreatedAgentSession]
- `Adapter.SuggestRepositories` ranks `CandidateRepository` values into `RepositorySuggestion` values for a thread's issue. [suggestions.go:SuggestRepositories,CandidateRepository,RepositorySuggestion]
- `Adapter.GraphQL` and `Adapter.GraphQLForTenant` send a raw GraphQL request. [session.go:GraphQL,GraphQLForTenant]

### Platform escape hatch

- `RawMessage` is the Linear payload on `chat.Message.Raw`. [raw.go:RawMessage]
- `RawMessageFrom` extracts a `*RawMessage` from a `chat.Message`. [raw.go:RawMessageFrom]
- `RawMessage.StopRequested` reports the human-to-agent `stop` signal. It is nil-safe. [raw.go:RawMessage.StopRequested]
- `RawAgentSession` holds session context. `FirstThoughtDeadline` holds the first-thought timing. [raw.go:RawAgentSession,FirstThoughtDeadline]

## Invariants

### Construction

- Multi-tenant mode (`InstallStore` set) rejects a static `WebhookSecret` or `ClientCredentials`. [linear.go:New]
- Single-install mode requires `WebhookSecret`, `ClientCredentials.ClientID`, and `ClientCredentials.ClientSecret`. [linear.go:New]
- `New` rejects a negative `SignatureTolerance`. Zero uses `defaultSignatureTolerance` (1 minute). [linear.go:New,defaultSignatureTolerance]
- Empty scopes default to `read`, `write`, `app:mentionable`, and `app:assignable`. [linear.go:New,defaultClientCredentialScopes]
- `RetryPolicy` defaults for zero fields: `MaxAttempts` 3, `MaxElapsed` 3s, `BaseDelay` 200ms, `MaxDelay` 1s. `MaxAttempts` 1 turns retry off. [retry.go:RetryPolicy.withDefaults], [doc.go:linear]
- Single-install `Init` requires a non-empty organization ID and app user ID from the viewer query. [linear.go:Init]
- `Init`, `Webhook`, `PostThought`, `BotActor`, `ReadHistory`, and the activity, session, suggestion, and GraphQL methods panic on a nil `*Adapter` through `assertAdapter`. [linear.go:assertAdapter,Init,Webhook,PostThought,BotActor], [history.go:ReadHistory], [activity.go:CreateAgentActivity], [session.go:GraphQL], [suggestions.go:SuggestRepositories]

### Webhooks

- `Webhook` accepts only POST (else 405) and bodies up to 1 MiB (else 413). [linear.go:Webhook,maxWebhookBodyBytes]
- An unreadable body returns 400 `read body`. After the signature check, a body that does not decode into the webhook envelope returns 400 `invalid linear payload`. [linear.go:Webhook]
- The `Linear-Signature` header must be the hex HMAC-SHA256 of the raw body. The adapter checks it before it decodes the event envelope or dispatches (else 401). In multi-tenant mode, the unverified routing read in the next rule comes first. [linear.go:verifySignatureWithSecret,Webhook,resolveWebhookInstall]
- `webhookTimestamp` must be non-zero and within the signature tolerance (else 401). [linear.go:verifyTimestamp]
- Multi-tenant mode reads `organizationId` from the unverified body for routing only. It then verifies the signature with the install record's webhook secret. [doc.go:linear], [linear.go:resolveWebhookInstall]
- In multi-tenant mode, a body with no organization or a `chat.ErrInstallNotFound` lookup is an Ignored Event (200, not dispatched). Other lookup errors return 500. [linear.go:resolveWebhookInstall]
- `chat.ErrAdmissionRejected` from dispatch returns 503, so Linear redelivers. Other dispatch errors return 500. [linear.go:Webhook]
- An agent-session event needs an organization, a session ID, and an issue ID. A `prompted` event needs `agentActivity`. [linear.go:normalizeAgentSessionEvent]
- The adapter drops agent-session and comment events whose organization or OAuth client ID conflicts with the resolved install. [linear.go:normalizeAgentSessionEvent], [comments.go:normalizeCommentEvent]
- A comment event needs a comment ID, an organization, an issue ID, and an author. [comments.go:normalizeCommentEvent]
- Multi-tenant mode drops events that the bot authored. [linear.go:normalizeAgentSessionEvent], [comments.go:normalizeCommentEvent]
- Event IDs have the form `linear:<org>:message:<sourceID>`. [linear.go:normalizeAgentSessionEvent], [comments.go:normalizeCommentEvent]

### Tokens and tenants

- The adapter refreshes a token when it is empty or within 1 hour of expiry. [linear.go:ensureToken,tokenRefreshBuffer]
- A token response without a positive `expires_in` gets Linear's documented 30-day lifetime, so the adapter refreshes it 1 hour before 30 days pass. This holds for the single-install token and for cached per-tenant tokens. The adapter does not refresh a token after a 401. [linear.go:exchangeClientCredentials,defaultTokenLifetime,ensureToken,tenantToken]
- A client-credentials response must contain `access_token` and grant every requested scope. [linear.go:exchangeClientCredentials,verifyGrantedScopes]
- An install credential must be a `LinearInstall` or a non-nil `*LinearInstall`. A multi-tenant install needs a webhook secret. A pre-supplied `AccessToken` is used as is. [linear.go:linearCredential,resolveWebhookInstall,tenantToken]
- `GraphQL`, `CreateSessionOnIssue`, and `CreateSessionOnComment` fail in multi-tenant mode. Use the `ForTenant` forms. [session.go:GraphQL,CreateSessionOnIssue,CreateSessionOnComment]
- `GraphQLForTenant` requires a non-empty query and never returns the access token. [session.go:GraphQLForTenant]

### Thread IDs and posting

- A Thread ID must start with `linear:v1:`. Its payload needs `org` and `issue`, plus `session` for agent-session threads or `comment` for comment threads. An empty kind means agent session. [linear.go:decodeThreadID,threadPayload.validate,threadPayload.kind]
- Single-install mode rejects a Thread ID from another organization. [linear.go:validateThreadPayload]
- Agent activity operations fail on a non-agent-session thread. [linear.go:agentSessionPayload]
- `PostMessage` requires non-empty text and `chat.MessageFormatText` or `chat.MessageFormatMarkdown`. `PostThought` requires non-blank text. [linear.go:validatePostableMessage,PostThought]
- `Ephemeral` is valid only for thought and action activities. [linear.go:createAgentActivity]
- An activity or comment mutation must report `success` and a non-empty ID, else the call fails. [linear.go:createAgentActivity], [comments.go:createIssueComment]
- `CreateAgentActivity` rejects empty `Content`. `PostAction` rejects a blank `Action`. `PostElicitation` and `PostError` reject a blank `Body`. [activity.go:CreateAgentActivity,PostAction,PostElicitation,PostError]
- `UpdateSession` rejects an empty update. [session.go:UpdateSession]
- `SuggestRepositories` requires at least one candidate, and each candidate needs `Hostname` and `RepositoryFullName`. [suggestions.go:SuggestRepositories]

### History and retry

- `ReadHistory` uses 50 when `Limit <= 0` and caps `Limit` at 250. [history.go:ReadHistory,linearDefaultHistoryLimit,linearMaxHistoryLimit]
- `ReadHistory` does no storage, dedupe, or caching. [history.go:ReadHistory], [doc.go:linear]
- A `Before` cursor resolves to a `createdAt` filter. An unresolved cursor is an error. [history.go:historyCursorFilter]
- Retry runs on HTTP 429 or a throttling GraphQL error. It never sleeps past the request context deadline, and returns `*RateLimited` instead. [retry.go:doJSONWithRetry,rateLimitedResponse]
- Retry also returns `*RateLimited` after `MaxAttempts`, or when the next delay would push the total past `MaxElapsed`. A context cancelled during a backoff sleep returns the context error instead. Each delay is the exponential backoff, raised to the platform's Retry-After, then capped at `MaxDelay`. [retry.go:doJSONWithRetry], [internal/ratelimit/ratelimit.go:BackoffDelay]
- The first-thought budget is 10 seconds from session creation. The adapter reports the deadline and runs no watchdog. [raw.go:firstThoughtBudget], [doc.go:linear]
- When the session timestamp does not parse, `newFirstThoughtDeadline` uses `now()` as the creation time. Its doc comment says it returns nil in that case, so the doc comment and the code disagree. [raw.go:newFirstThoughtDeadline]

## Side Effects

- The adapter POSTs to `{APIBaseURL}/oauth/token` (default `https://api.linear.app`) for the client-credentials grant. [linear.go:exchangeClientCredentials,defaultAPIBaseURL]
- All GraphQL calls POST to `{APIBaseURL}/graphql` with a bearer token through `Options.Client` (default `http.DefaultClient`). [linear.go:callGraphQLWithToken,New]
- GraphQL operations: `ViewerIdentity` at `Init`, `AgentActivityCreate` for activities, `CommentCreate` for comments, `AgentSessionUpdate`, `AgentSessionCreateOnIssue`, `AgentSessionCreateOnComment`, `IssueRepositorySuggestions`, and history queries. [linear.go:fetchIdentity,createAgentActivity], [comments.go:createIssueComment], [session.go:UpdateSession,createSessionOnIssue,createSessionOnComment], [suggestions.go:SuggestRepositories], [history.go:readAgentSessionHistory,readCommentThreadHistory]
- Multi-tenant mode calls `InstallStore.Lookup` on every webhook whose body names an organization, and on every outbound token resolution. A body with no organization is ignored without a lookup. It does not cache install records. [linear.go:resolveWebhookInstall,resolveToken]
- A mutex (`a.mu`) guards the access token, expiry, organization, and bot identity. A second mutex guards the per-tenant token cache. [linear.go:Adapter,tenantToken]
- `Webhook` calls the runtime `chat.DispatchFunc` for each normalized event. [linear.go:Webhook]
- Retry sleeps with `ratelimit.SleepCtx` from `internal/ratelimit`, and resends a buffered copy of the request body. [retry.go:doJSONWithRetry]
- The adapter emits `chat.ObsAdapterCall` per attempt and `chat.ObsRateLimit` per throttle to `Options.Observer`. [retry.go:doJSONWithRetry]
- The adapter logs a `linear rate limited` warning on throttles, warnings for dropped events, and debug records for ignored webhooks. [retry.go:logRetry], [linear.go:Webhook,normalizeAgentSessionEvent], [comments.go:normalizeCommentEvent]

## Verification

- Run: `go test ./adapters/linear/...`
- The emulate.dev tier has no Linear tests yet, because emulate's Linear schema differs from Linear's. See "Emulator Tests" in `CONTRIBUTING.md` and issue #148.
