# ADR 0013: Linear Generic Issue/Comment Participation

## Status

Accepted

## Context

ADR-0001 built the **Linear App-Actor Slice** as a bot that participates "as a Linear app-owned actor through agent sessions, not as a normal user or generic issue-comment bot," and deliberately rejected non-agent-session Linear **Thread IDs**. ADR 0008 completes the *agent-session* surface (five activity types, completion signals, timing contract, plans/actions) but keeps that restriction in place.

Generic issue/comment participation — the bot reacting to ordinary Linear issue comments rather than to app-owned agent sessions — is a genuinely different interaction model:

- different inbound shape and webhook scope: Linear `Comment` events, not `AgentSessionEvent`;
- different mention semantics: a comment that mentions the app actor versus a session created/prompted for it;
- a different posting target: an ordinary Linear issue comment versus an **Agent Activity Response**;
- and an identity nuance: the bot now has a presence in ordinary comment threads, a broader surface than agent sessions.

Because it is a distinct model, it is split out of ADR 0008 (decided during ADR review) rather than reversing ADR-0001's careful app-actor identity in one step.

This ADR reopens documented non-goals, surfaced explicitly per `docs/agents/domain.md`:

- **ADR-0001 deferred "generic comment mode"** and its rejection of non-agent-session Linear **Thread IDs**. Reopened here as its own slice, now that ADR 0008 has completed the agent-session surface it builds on.
- **`docs/linear-agent-capabilities.md`**: the generic issue/comment participation gap (lifting the agent-session-only thread restriction).

## Decision

Add generic Linear issue/comment participation as a second Linear interaction model behind the existing seams, keeping the adapter a **Single-Install Adapter** on **App-Actor Client Credentials**.

- **Thread-kind discriminator.** Extend the opaque, versioned, adapter-produced **Thread ID** with a Linear thread-kind tag (agent-session vs issue-comment). Existing agent-session **Thread IDs** keep decoding (versioned payload); application code never builds or branches on a **Thread ID**, so this is invisible to it.
- **`Thread.Post` routes by kind.** Agent-session threads create an **Agent Activity Response** (ADR 0008); issue-comment threads create an ordinary Linear issue comment. **Plain Text** + **Portable Markdown** stay the only portable bodies, passed through with no Markdown conversion layer.
- **Inbound generic comments.** Accept Linear `Comment` webhooks as a **Supported Platform Shape**, normalize them into **Message Events**, and let **Runtime Dispatch** decide **New Mention** vs **Subscribed Thread** exactly as it does for agent-session prompts. **Event Identity** stays keyed on source comment identity, matching ADR-0001's Linear dedupe semantics.
- **Identity stays app-actor.** The bot participates as its app actor discovered during **Adapter Initialization**; `OnNewMention` fires on an actual mention of the app actor in a comment, and **Self Message** filtering uses `BotActor`. This is not a personal-API-key user bot — that stays out of scope, per ADR-0001.
- **Webhook scope.** Generic participation requires enabling Linear `Comment` (and as needed `Issue`) webhooks in addition to agent-session webhooks; setup docs cover the added scope. Unsupported shapes after verification are **Ignored Events**.
- **Long comment handlers** use the ADR 0002 **Ack-Then-Work** + **Detached Work Context** primitive, identical to the rest of the runtime; no Linear-private async path. Multi-tenant OAuth is ADR 0006; this slice reuses the **Platform Tenant** scoping already in the opaque **Thread ID** and **Actor**.

## Consequences

- The Linear adapter gains a second participation mode, and the opaque **Thread ID** carries the thread-kind discriminator ADR 0008 deferred. Existing agent-session deployments are unaffected (versioned decode).
- The bot now posts ordinary issue comments as its app actor — a broader presence than agent sessions. This is documented as still the *app actor*, not a user bot, so the identity line ADR-0001 drew is preserved, just widened to comments.
- Generic comment participation needs the added `Comment` webhook scope; a deployment that only enables agent-session webhooks is unchanged.
- The portable surface and the small **Adapter** interface are unchanged; the only behavioral widening is which inbound shapes are accepted and what `Thread.Post` creates per thread kind.
- This depends on ADR 0008 (the versioned **Thread ID** and the agent-session surface it tags against) and ADR 0002 (deferred dispatch for long comment handlers).

## Alternatives Considered

### Fold generic comments into ADR 0008 (the "full adapter")

Rejected during ADR review. Generic issue/comment participation is a different interaction model from running agent sessions — different webhook scope, mention semantics, posting target, and a broader identity presence. Bundling it would reverse ADR-0001's deliberate app-actor-through-agent-sessions identity in a single step and make ADR 0008 two designs at once. Splitting keeps each slice faithful to the repo's slice discipline.

### Model issue comments as agent sessions

Rejected. A Linear issue comment is not an agent session; forcing it into the `AgentSessionEvent` shape would misrepresent the platform model and conflate the two completion/timing contracts. Comments normalize into ordinary **Message Events**; sessions keep their **Agent Session Timing Contract**.

### Participate as a normal user bot via a personal API key

Rejected, consistent with ADR-0001. The integration is an app-owned actor; a personal-API-key user bot blurs whether the runtime acts as a user or an app. Generic participation still happens as the app actor.

### Promote a normalized cross-platform "comment vs session" kind into the core

Rejected. Thread kind is a Linear-specific concern carried inside the opaque adapter-produced **Thread ID** and decoded by the adapter. The runtime does not gain a cross-platform thread-kind taxonomy for one platform's distinction.
