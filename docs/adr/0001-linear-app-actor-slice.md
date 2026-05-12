# ADR 0001: Linear App-Actor Slice Before Full Linear Adapter

## Status

Accepted

## Context

The Go Chat Runtime follows the upstream Chat SDK's conversation semantics where they fit Go, but it does not promise TypeScript API compatibility or full adapter feature parity. Slack proved the first production-shaped adapter slice. Linear is the next adapter under consideration.

The upstream Chat SDK Linear adapter is broad: it supports normal issue-comment mode, app-actor agent-session mode, multiple authentication modes, multi-tenant OAuth installations, message history, reactions, edit/delete, streaming, plans, actions, errors, and richer Linear Markdown conversion.

The desired Go work is narrower: prove a Linear integration where the bot participates as a Linear app-owned actor through agent sessions, not as a normal user or generic issue-comment bot.

## Decision

Build a **Linear App-Actor Slice** before claiming a full Linear adapter.

The MVP will:

- live under the normal `adapters/linear` package and use adapter name `linear`;
- use nested single-install **App-Actor Client Credentials** as its owned authentication path;
- exclude static access-token auth, personal API key auth, generic comment mode, and multi-tenant OAuth installation storage;
- exchange client credentials during adapter initialization and discover the Linear organization and app user identity before webhooks are served;
- refresh client-credentials tokens lazily before Linear API calls and cache the token in adapter process memory;
- accept Linear agent-session webhooks for app-session creation and prompting;
- normalize buildable agent-session events as mentioned messages and let runtime subscription state decide new-mention vs subscribed-message routing;
- use source comment identity as the logical event identity for dedupe, matching the upstream Chat SDK's Linear message dedupe semantics;
- use opaque Go thread IDs that include organization, issue, optional comment context, and agent session identity;
- reject non-agent-session Linear thread IDs until generic issue/comment posting is separately designed;
- map `Thread.Post` to Linear agent activity responses;
- expose a Linear-specific `PostThought` method through typed adapter access that creates ephemeral Linear agent activity thoughts;
- preserve synchronous runtime dispatch for the MVP;
- use direct HTTP/GraphQL calls and local supported-shape structs, following the Slack adapter pattern rather than introducing a Linear SDK dependency;
- pass plain text and portable markdown bodies through to Linear agent activity bodies without porting the upstream Linear Markdown converter;
- include one memory-backed `examples/linear-agent-hello-world` example with setup instructions.

Webhook handling will reject transport/security/envelope failures such as bad signatures, stale timestamps, and malformed JSON. After successful webhook verification, unsupported or unbuildable Linear agent-session payloads are acknowledged and ignored, matching the upstream Chat SDK's lenient Linear agent-session behavior.

## Consequences

The first Linear adapter will be useful for native Linear app-agent dogfooding, but it will not be a full Linear product surface.

Application authors can use the normal Go runtime flow for final responses:

```go
ev.Thread.Post(ctx, chat.Markdown("..."))
```

They can use typed adapter access for Linear-specific thoughts:

```go
linearAdapter, ok := chat.AdapterAs[*linear.Adapter](bot, "linear")
if ok {
    _, _ = linearAdapter.PostThought(ctx, ev.Thread.ID(), "Thinking...")
}
```

The adapter diverges from the upstream Chat SDK in several deliberate Go-shaped ways:

- no full Linear auth matrix in MVP;
- no generic comments mode in MVP;
- organization identity is included in opaque thread IDs for tenant correctness;
- runtime dispatch remains synchronous;
- no public raw Linear client or GraphQL helper;
- no Linear SDK dependency;
- no rich Linear Markdown conversion layer.

These choices keep the first slice small, testable, and consistent with existing Go runtime and Slack adapter patterns. Future work can add generic comment mode, multi-tenant OAuth installs, streaming/plans/actions, reactions, history, and Markdown conversion as separately designed slices.

## Alternatives Considered

### Port the upstream Linear adapter broadly

Rejected for the MVP. It would pull in too many concerns before the Go runtime has proven the Linear app-actor conversation path.

### Support personal API keys or static access tokens first

Rejected because the desired integration is app-owned actor behavior, not a normal-user bot. Static tokens also blur whether the runtime is acting as a user or an app.

### Add generic runtime typing/streaming APIs now

Rejected as premature. Linear thoughts are exposed through adapter access for now; a cross-platform typing or streaming abstraction can be designed after more adapters need it.

### Use a Linear SDK dependency

Rejected for the MVP. The existing Slack adapter uses direct HTTP calls and local supported-shape structs. The Linear slice only needs token exchange, identity discovery, webhook verification/normalization, and agent activity creation.

### Dispatch Linear webhooks asynchronously

Rejected for the MVP because the current Go runtime defines synchronous dispatch. Long-running Linear agents should post an early thought and enqueue follow-up work in application code until a runtime-level deferred dispatch model is designed.

## Dogfooding and Quality Gates

The Linear hello-world example must prove:

1. Linear app actor is installed and mentionable.
2. A Linear `AgentSessionEvent` session creation reaches `OnNewMention`.
3. The handler explicitly subscribes the thread.
4. The app posts a best-effort ephemeral thought.
5. The app posts a final agent activity response.
6. A follow-up prompt in the same agent session reaches `OnSubscribedMessage`.
7. The app posts a follow-up thought and response.

Dogfooding instructions should include setup steps for:

- creating a Linear OAuth app;
- installing it with `actor=app` and `app:mentionable`;
- configuring client-credentials environment variables;
- enabling agent session webhooks only;
- exposing `/webhooks/linear` through a public HTTPS tunnel;
- running the example with memory state.

A reviewer should be able to verify the live dogfood with screenshots or video showing:

- Linear app settings;
- webhook configuration;
- first app mention / session creation;
- thought and final response;
- follow-up prompt, thought, and response.
