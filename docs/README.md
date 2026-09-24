# Chat SDK Go Documentation

The docs follow [Diátaxis](https://diataxis.fr/): a tutorial to learn,
how-to guides to get a task done, reference to look things up, and
explanation to understand the design.

## Tutorials

Start here if you are new to the SDK.

- [Your first Slack bot](tutorials/slack-bot.md) — zero to a running Slack bot
  in under 30 minutes.

## How-To Guides

For people who already have a bot running.

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

- [Reference](reference.md) — modules and packages, how the runtime behaves
  (routing, dispatch, state, concurrency, messages, history), what each
  adapter supports, and the runnable examples. The API itself is in the
  [GoDoc](https://pkg.go.dev/github.com/coder/chat).
- [Linear agent capabilities](linear-agent-capabilities.md) — what the Linear
  adapter supports today and what it does not wrap yet.

## Explanation

- [Architecture and design decisions](explanation.md) — the model in brief,
  design goals, the Vercel Chat SDK concept map, non-goals, and intentional
  gaps, with an index of the [ADRs](adr/) and
  [`CONTEXT.md`](../CONTEXT.md), the project's vocabulary.

## For Contributors

- [`CONTRIBUTING.md`](../CONTRIBUTING.md) — how to build, test, and propose
  changes to this repository.
- [`docs/agents/`](agents/) — instructions for coding agents working on this
  repository, not for SDK users.
