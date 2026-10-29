# adapters

## Overview

The `adapters` directory groups the platform adapters of Chat SDK Go (`github.com/coder/chat`). It has no Go source files of its own. Each child package connects the portable `chat` runtime (root package `github.com/coder/chat`) to one chat platform: it receives platform webhooks or connections, normalizes them into `chat.Event` values, and posts replies through the platform API.

Both adapters share these traits, from their own contracts:

1. Each adapter runs in single-install mode (static credentials) or multi-tenant mode (`chat.InstallStore`, looked up for each request routed to an install, with no caching).
2. Each adapter verifies the signature of inbound webhooks before dispatch.
3. Each adapter retries platform rate limits within a `RetryPolicy`, never sleeps past the caller's context deadline, and returns a typed `*RateLimited` error when retry stops.
4. Each adapter emits `chat.ObsAdapterCall` and `chat.ObsRateLimit` events to the configured observer.
5. Each adapter implements `chat.HistoryReader` as a live read with no storage or caching.

## Subdirectories & Boundaries

- linear: [./linear/README.md](./linear/README.md) - Linear app-actor adapter. It handles agent-session and issue-comment webhooks, posts agent activities and comments through the Linear GraphQL API, and owns its own rate-limit retry.
- slack: [./slack/README.md](./slack/README.md) - Slack adapter. It receives deliveries over HTTP webhooks and Socket Mode, and posts messages, ephemeral messages, Block Kit content, and modals. It contains the low-level `slackapi` package ([./slack/slackapi/README.md](./slack/slackapi/README.md)), which holds the Slack Web API client, transports, and rate-limit retry, and its `slackapitest` test fakes ([./slack/slackapi/slackapitest/README.md](./slack/slackapi/slackapitest/README.md)).
