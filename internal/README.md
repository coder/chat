# internal

## Overview

`internal` groups private helper packages of the `github.com/coder/chat` module.
It has no Go source files of its own. Go allows only code in the tree rooted at
`github.com/coder/chat` to import these packages. The importers are the
adapters (`github.com/coder/chat/adapters/linear`,
`github.com/coder/chat/adapters/slack/slackapi`) and the state backends
(`github.com/coder/chat/state/memory`, `github.com/coder/chat/state/nats`,
`github.com/coder/chat/state/postgres`, `github.com/coder/chat/state/redis`).
The `state/nats`, `state/postgres`, and `state/redis` backends are separate
modules in `go.work`. Their import paths are inside that tree, so the rule
allows them.

Run all three packages with `go test ./internal/...`. None of them has its own
tests, so the command reports `[no test files]`. Each child README names the
importer tests that exercise it.

## Subdirectories & Boundaries

- ratelimit: [./ratelimit/README.md](./ratelimit/README.md) - Shared retry mechanics for the adapter rate-limit loops: `BufferRequestBody`, `CloneRequest`, `BackoffDelay` (1-based attempt, clamped to `maxDelay`), and `SleepCtx` (never outlasts the caller's context). Throttling detection stays in each adapter.
- statetest: [./statetest/README.md](./statetest/README.md) - Test-only `chat.State` conformance suite: `Harness` and `RunStateConformance`, which check subscriptions, event dedupe, lock ownership and expiry, and that a cancelled context changes no state. Every state backend's tests call it.
- tokens: [./tokens/README.md](./tokens/README.md) - `New` returns a 32-character hex token from 16 `crypto/rand` bytes. The memory, nats, and postgres backends use it for lock lease tokens.
