# Linear Generic Issue/Comment Participation

Status: needs-triage

## Problem Statement

The **Linear App-Actor Slice** (ADR-0001) and its completion (ADR 0008) let the bot participate only through Linear **agent sessions**: it receives `AgentSessionEvent` webhooks and posts **Agent Activity** content. Non-agent-session Linear **Thread IDs** are rejected. A bot that should react to ordinary Linear issue comments — answer a question left as a comment, act on a mention in a thread that is not an agent session — cannot, because the adapter accepts no generic `Comment` webhooks and `Thread.Post` only creates **Agent Activity Responses**.

Generic issue/comment participation is a distinct interaction model from running agent sessions, so it is its own slice rather than part of ADR 0008.

## Solution

Add generic Linear issue/comment participation behind the existing seams, keeping the adapter a **Single-Install Adapter** on **App-Actor Client Credentials**:

- A Linear thread-kind discriminator in the opaque, versioned **Thread ID** (agent-session vs issue-comment); existing agent-session **Thread IDs** keep decoding.
- Accept Linear `Comment` webhooks, normalize them into **Message Events**, and let **Runtime Dispatch** decide **New Mention** vs **Subscribed Thread** as it does for agent-session prompts.
- `Thread.Post` routes by kind: agent-session → **Agent Activity Response** (ADR 0008); issue-comment → ordinary Linear issue comment.
- The bot participates as its app actor (not a personal-API-key user bot); mentions and **Self Message** filtering use the discovered `BotActor`.

This widens which inbound shapes the Linear adapter accepts and what `Thread.Post` creates, but leaves the portable surface (**Plain Text** + **Portable Markdown**) and the small **Adapter** interface unchanged.

## User Stories

1. As a Linear bot developer, I want the bot to receive ordinary issue-comment mentions as **Message Events**, so that it can respond outside an agent session.
2. As a Linear bot developer, I want `Thread.Post` on an issue-comment **Thread** to create an ordinary Linear comment, so that replies land in the issue thread.
3. As a Linear bot developer, I want issue-comment threads and agent-session threads to use the same routing (`OnNewMention` / `OnSubscribedMessage`) and subscription model, so that I write one conversation flow.
4. As a runtime operator, I want existing agent-session **Thread IDs** to keep decoding after the thread-kind discriminator is added, so that stored references stay valid.
5. As a Linear bot developer, I want the bot to participate as the app actor (not a user bot), so that the app-owned identity from ADR-0001 is preserved.
6. As a deployer, I want clear setup steps for the added `Comment` webhook scope, so that generic participation is opt-in and explicit.
7. As a Linear bot developer, I want long comment handlers to use the same **Ack-Then-Work** primitive as the rest of the runtime, so that there is no Linear-private async path.

## Implementation Decisions

- The opaque **Thread ID** gains a versioned Linear thread-kind tag; only the adapter constructs or decodes it.
- `Thread.Post` branches on thread kind; agent-session posting is unchanged from ADR 0008.
- Generic `Comment` webhooks are decoded as a **Supported Platform Shape** with unknown-field tolerance; unsupported shapes after verification are **Ignored Events**.
- **Event Identity** for comments stays keyed on source comment identity, matching ADR-0001's Linear dedupe.
- Identity stays app-actor; `OnNewMention` fires on an explicit mention of the app actor; personal-API-key user bots stay out of scope.
- Direct HTTP/GraphQL and local structs; no Linear SDK dependency; new behavior reached through existing deep modules and **Adapter Access** where Linear-specific.
- Single-install only; multi-tenant OAuth is ADR 0006. Long handlers use ADR 0002 deferred dispatch.

## Testing Decisions

- Thread-ID tests cover the versioned thread-kind discriminator and backward-compatible decode of existing agent-session **Thread IDs**.
- Routing tests cover issue-comment mention → **New Mention**, subscribed issue-comment → **Subscribed Thread**, and **Self Message** filtering of the app actor's own comments.
- Posting tests cover `Thread.Post` creating an issue comment on issue-comment threads and an **Agent Activity Response** on agent-session threads.
- Normalization tests cover `Comment` webhook decode, **Event Identity** by source comment, and **Platform Tenant** scoping.
- Regression: agent-session behavior from ADR 0008 / ADR-0001 is unchanged.

## Out of Scope

- Personal-API-key or user-OAuth user bots — participation stays app-actor (ADR-0001).
- Multi-tenant OAuth installs — ADR 0006.
- Reactions, edit/delete, and other **Outbound Mutation** on comments.
- Rich Linear Markdown conversion — the portable surface is unchanged.
- Issue-workflow automation and repository suggestions — reachable through the ADR 0008 `GraphQL` escape hatch, not given typed helpers here.

## Further Notes

- Split from ADR 0008 during ADR review because generic comment participation is a different interaction model and bundling it would reverse ADR-0001's app-actor-through-agent-sessions identity in one step.
- Depends on ADR 0008 (versioned **Thread ID**, agent-session surface) and ADR 0002 (deferred dispatch).
