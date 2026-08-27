# API And Package Reference

The API reference is the GoDoc. Every package carries package-level
documentation (`doc.go`), and the intentional differences from Vercel Chat
SDK are documented directly on the symbols they affect.

## Modules And Packages

| Package | Module | GoDoc |
| --- | --- | --- |
| `github.com/coder/chat` | core | [pkg.go.dev](https://pkg.go.dev/github.com/coder/chat) |
| `github.com/coder/chat/adapters/slack` | core | [pkg.go.dev](https://pkg.go.dev/github.com/coder/chat/adapters/slack) |
| `github.com/coder/chat/adapters/linear` | core | [pkg.go.dev](https://pkg.go.dev/github.com/coder/chat/adapters/linear) |
| `github.com/coder/chat/state/memory` | core | [pkg.go.dev](https://pkg.go.dev/github.com/coder/chat/state/memory) |
| `github.com/coder/chat/state/redis` | separate | [pkg.go.dev](https://pkg.go.dev/github.com/coder/chat/state/redis) |
| `github.com/coder/chat/state/postgres` | separate | [pkg.go.dev](https://pkg.go.dev/github.com/coder/chat/state/postgres) |
| `github.com/coder/chat/state/nats` | separate | [pkg.go.dev](https://pkg.go.dev/github.com/coder/chat/state/nats) |

Redis, Postgres, and NATS state live in separate Go modules so applications
that only use core, Slack, or memory state do not pull their dependencies.
The repository uses `go.work` for local development across all modules.

To browse the reference locally without pkg.go.dev:

```sh
go doc github.com/coder/chat
go doc github.com/coder/chat/adapters/slack
```

## Where To Look For What

- **Runtime construction, hooks, dispatch, runtime options**: package `chat`
  (`chat.New`, `Chat.OnNewMention`, `Chat.OnSubscribedMessage`,
  `Chat.OnCommand`, `Chat.OnInteraction`, `RuntimeOptions`).
- **Event and message model**: package `chat` (`Event`, `Message`,
  `MessageEvent`, `CommandEvent`, `InteractionEvent`, `Thread`, `ThreadID`,
  `Actor`).
- **Optional capabilities**: package `chat` (`NativeContentPoster`,
  `HistoryReader`, `EphemeralPoster` interfaces) plus the adapter packages
  for what each adapter actually implements.
- **Multi-tenant installs**: package `chat` (`InstallStore`, `Install`,
  `ErrInstallNotFound`) plus `slack.SlackInstall` / `linear.LinearInstall`.
- **State contract**: package `chat` (`State`) with implementations in the
  four `state/*` packages.

## Adapter Capability Status

Portable behavior (normalized events, thread routing, `Thread.Post`,
`Thread.Subscribe`) works on every adapter. Optional capabilities and
platform-specific surfaces differ:

| Capability | Slack | Linear |
| --- | --- | --- |
| Message events (mentions, subscribed threads, DMs) | Yes | Yes (agent sessions and issue comments) |
| Ephemeral messages with explicit DM fallback | Yes | Thoughts via `PostThought` (no DM concept) |
| Slash commands (`OnCommand`) | Yes | No (no platform equivalent) |
| Interactive components (`OnInteraction`) | Yes (`block_actions`) | No |
| Native content posting (`NativeContentPoster`) | Yes (Block Kit) | No |
| Modal open / `response_url` | Yes (`OpenModal`, `RespondURL`) | No |
| Message history read-through (`HistoryReader`) | Yes | No (tracked gap) |
| Rate-limit retry with typed `RateLimited` error | Yes | Yes |
| Multi-tenant installs (`InstallStore`) | Yes | Yes |
| Platform escape hatch | Raw payloads on events | `RawMessage`, `GraphQL` |
| Agent activities (thought/action/elicitation/error/plan) | n/a | Yes |

For the tracked list of Linear agent APIs that are not yet wrapped in typed
helpers, see [linear-agent-capabilities.md](linear-agent-capabilities.md).

## Examples

Runnable, documented examples live in [`examples/`](../examples/):

- [`slack-hello-world`](../examples/slack-hello-world/README.md) — memory
  state, no infrastructure (the [tutorial](tutorials/slack-bot.md) target).
- [`slack-redis-state`](../examples/slack-redis-state/README.md),
  [`slack-postgres-state`](../examples/slack-postgres-state/README.md),
  [`slack-nats-state`](../examples/slack-nats-state/README.md) — the same bot
  on each durable state backend, each with a `compose.yaml`.
- [`linear-agent-hello-world`](../examples/linear-agent-hello-world/README.md) —
  Linear agent sessions with memory state.
