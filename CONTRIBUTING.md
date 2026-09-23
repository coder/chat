# Contributing To Chat SDK Go

Bug reports, feature requests, and pull requests are welcome. GitHub
[issues](https://github.com/coder/chat/issues) are the roadmap and the place
to discuss a change before you build it.

## Set Up

1. Install the toolchain pinned in `mise.toml` (Go 1.26.3 and helpers):

   ```sh
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

CI runs these two commands on every pull request, with `GOFLAGS=-race`:

```sh
mise run vet    # go vet in every module
mise run test   # tests in every module
```

Narrower tasks help while you iterate:

| Task | Runs |
| --- | --- |
| `mise run test:root` | `go test ./...` in the root module |
| `mise run test:adapters` | adapter tests and all state-module tests |
| `mise run test:redis`, `test:postgres`, `test:nats` | one state module |
| `mise run test:examples` | the durable-state example modules |

When you bump a state module's dependencies, also run `go mod tidy` in the
matching example module; otherwise its `go.sum` goes stale.

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
