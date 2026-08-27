# Chat SDK Go Documentation

User-facing documentation is organized along [Diátaxis](https://diataxis.fr/):
learning-oriented tutorials, task-oriented how-to guides, information-oriented
reference, and understanding-oriented explanation.

## Tutorials

Start here if you are new to the SDK.

- [Your first Slack bot](tutorials/slack-bot.md) — zero to a running Slack bot
  in under 30 minutes.

## How-To Guides

Task-oriented guides for people already running a bot.

- [Choose a state backend](how-to/choose-a-state-backend.md) — memory, Redis,
  Postgres, or NATS JetStream.
- [Defer long-running work (ack-then-work)](how-to/deferred-dispatch.md) —
  acknowledge webhooks fast and run handlers on a detached context.
- [Handle slash commands](how-to/slash-commands.md) — route Slack slash
  commands through `OnCommand`.
- [Handle interactive components](how-to/interactive-components.md) — buttons,
  menus, Block Kit content, and modals.
- [Install into multiple workspaces (multi-tenant)](how-to/multi-tenant-install.md) —
  resolve per-tenant credentials with an `InstallStore`.
- [Run Linear agent sessions](how-to/linear-agent-sessions.md) — build a Linear
  agent with thoughts, responses, actions, elicitations, and plans.

## Reference

- [API and package reference](reference.md) — pkg.go.dev pointers, module
  layout, and per-adapter capability status.
- [Linear agent capability gaps](linear-agent-capabilities.md) — tracked list
  of Linear agent APIs the adapter does not yet wrap.

## Explanation

- [Architecture and design decisions](explanation.md) — an index over
  [`CONTEXT.md`](../CONTEXT.md) (the ubiquitous language and architecture
  document) and the [ADRs](adr/) that record every significant decision.

## Non-User Documentation

- [`docs/agents/`](agents/) — instructions for coding agents working on this
  repository, not for SDK users.
