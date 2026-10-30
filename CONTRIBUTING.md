# Contributing To Chat SDK Go

Bug reports, feature requests, and pull requests are welcome. GitHub
[issues](https://github.com/coder/chat/issues) are the roadmap and the place
to discuss a change before you build it.

## Set Up

1. Trust the repository's `mise.toml`, then install the toolchain it pins
   (Go 1.27.1 and helpers):

   ```sh
   mise trust
   mise install
   ```

   Without mise, install Go 1.26.3 or newer yourself.

2. Install Docker if you want to run the Redis and Postgres state tests. They
   use Testcontainers and are skipped when no container runtime is healthy.
   The NATS tests use an embedded server and need nothing extra.

## Repository Layout

- The root module holds the runtime, the Slack and Linear adapters, and the
  memory state backend.
- `state/redis`, `state/postgres`, and `state/nats` are separate Go modules,
  so the core does not pull their dependencies.
- `examples/` holds runnable bots. The three durable-state examples are
  separate modules too.
- `go.work` ties all modules together for local development.

## Build And Test

CI runs these two commands on every pull request:

```sh
mise run vet                    # go vet in every module
GOFLAGS=-race mise run test     # tests in every module, with the race detector
```

CI runs them twice: once with the Go that `mise.toml` pins, and once with the
minimum Go from `go.mod` (`GOTOOLCHAIN=go1.26.3`).

Narrower tasks help while you iterate:

| Task | Runs |
| --- | --- |
| `mise run test:root` | `go test ./...` in the root module |
| `mise run test:adapters` | adapter tests and all state-module tests |
| `mise run test:redis`, `test:postgres`, `test:nats` | one state module |
| `mise run test:examples` | the durable-state example modules |

When you bump a state module's dependencies, also run `go mod tidy` in the
matching example module; otherwise its `go.sum` goes stale.

### What Tests Must Cover

Tests check external behavior and public contracts, not private
implementation details. Keep these families covered:

- runtime construction and shutdown
- handler registration and replacement
- routing order and no-op missing handlers
- explicit subscription and unsubscribe
- direct-message implicit mention routing
- self-message filtering
- accepted, ignored, rejected, duplicate, and lock-conflict events
- state conformance across memory, Redis, Postgres, and NATS
- token-owned lock lease acquire, release, extend, expiry, and stale release
- Slack signature verification and URL verification
- Slack golden payload normalization
- thread ID construction and validation
- thread handle reconstruction
- text, Markdown, sent message, ephemeral, and ephemeral fallback posting
- typed adapter access
- documentation coverage of intentional Vercel Chat SDK differences in the
  README, reference, explanation, and GoDoc (see
  [Documentation](#documentation))

## Documentation

Docs follow [Diátaxis](https://diataxis.fr/); the
[docs index](docs/README.md) shows where each kind of page lives. Update the
docs in the same pull request as the behavior they describe.

Some docs are checked by `documentation_test.go`:

- Go blocks in `README.md` marked `<!-- build -->` must compile.
- Go blocks in `docs/how-to/linear-agent-sessions.md` marked
  `<!-- source: path -->` must appear verbatim in that source file.
- A few key phrases must stay in the README, reference, explanation, and
  package docs. If you reword one, update the test in the same change.

## Emulator Tests

An opt-in tier runs the Slack adapter end to end against
[emulate.dev](https://emulate.dev) (`vercel-labs/emulate`, pinned to 0.12.3),
a local, stateful emulator of the Slack and Linear APIs. It covers flows that
the fakes do not cover end to end:

- a signed Events API message event, through the adapter's HTTP handler and
  the mention handler, to a thread reply that `conversations.replies` shows;
- a button click from emulate's block-action simulator, through the adapter's
  HTTP handler, to the interaction handler.

You need Node.js 22 or later for `npx`. Run from the repository root:

```sh
mise run emulate        # terminal 1: Slack on :4710, Linear on :4711
mise run test:emulate   # terminal 2: sets CHAT_EMULATE_SLACK_URL, runs TestEmulate*
```

The seed is `testdata/emulate/seed.yaml`. The block-action test listens on
`127.0.0.1:4799`, the seeded interactivity URL. Without
`CHAT_EMULATE_SLACK_URL`, the tests skip and `go test ./...` is unchanged. The
`Emulate` workflow runs the tier on each pull request, separately from `CI`.

emulate is not the source of truth for Slack's rules. The fakes
(`adapters/slack/slackapi/slackapitest` and `linearAPIServer` in
`adapters/linear/linear_test.go`) and the live tests (`TestLive*` in
`adapters/slack/slackapi/live_test.go`) are. Known gaps in emulate 0.12.3:

- No Socket Mode: `apps.connections.open` returns 404.
- No slash commands.
- No Events API request URL in the CLI or the seed. The event test posts the
  user message to emulate, then signs and sends its `event_callback` itself.
- emulate accepts a JSON body for `conversations.history` and
  `conversations.replies`. Real Slack returns `invalid_arguments`, so this tier
  would have missed #98.
- Scope checks are relaxed unless the seed sets `strict_scopes: true`. This
  seed leaves them relaxed.
- No Linear tests. emulate's Linear schema differs from Linear's, so the
  adapter cannot reply: `User` has no `organization` (single-install `Init`
  fails), `CommentCreateInput` has no `parentId`, `AgentActivityCreateInput` is
  `{sessionId, type, body}` instead of `{agentSessionId, content}`, Comment
  webhooks have no `data.user`, and `AgentSessionEvent` webhooks carry the
  session in `data` instead of `agentSession`. The seed keeps a Linear team for
  manual checks. Issue #148 tracks Linear tests.

## Validating Adapter Changes Live

CI has no live Slack or Linear tests. Before you claim that a change works
against a real workspace, capture screenshots or a video of each step. For
the Linear example, that means:

- the Linear app actor settings;
- the webhook configuration with agent session events enabled;
- the first app mention and the agent session it creates;
- the ephemeral thought and the final response;
- a follow-up prompt and its thought and response.

## Design Changes

[`CONTEXT.md`](CONTEXT.md) defines the project's vocabulary; use its terms in
code, docs, and reviews. Significant decisions, including decisions not to
build something, get an Architecture Decision Record in [`docs/adr/`](docs/adr/).
Keep ADRs to the decision, its invariants, and its non-goals; implementation
detail belongs in the pull request and its tests.
