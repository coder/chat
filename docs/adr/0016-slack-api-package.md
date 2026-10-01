# ADR 0016: Low-Level Slack API Package

## Status

Accepted

## Context

The **Go Chat Runtime** reaches Slack only through the Slack **Platform Adapter**. Before this ADR, the adapter kept its Web API calls, its bounded rate-limit retry (ADR 0005), and its request signature check private. It exposes Slack behavior through the portable surface and through **Adapter Access** on a registered adapter. Both paths assume that the application runs the runtime.

Some applications own their runtime. The first is coderd, the Coder server: it runs a Slack bot on its own database and its own chat engine, so it has its own state, dispatch, and routing, and it cannot use `chat.New` or `chat.State`. It still needs the same Slack protocol work that the adapter does: Web API calls with bounded retry, the v0 signature check, Events API parsing, file upload and download, Block Kit, and Markdown split to Slack's block limit. Its bot setup gives the admin a Slack app manifest to create the app from, and while an agent works, the bot shows a status such as "thinking" in the Slack thread (`assistant.threads.setStatus`). It also needs Slack methods that the portable surface leaves out on purpose: ADR 0004 defers portable edit, delete, and reactions, and files are not portable either.

Without a shared package, each such application copies the Slack protocol code out of the adapter, and the copies drift from the adapter and from ADR 0005.

Related decisions, not redefined here: outbound rate-limit retry is ADR 0005; the deferral of general **Outbound Mutation** is ADR 0004; the **Observation Hook** is ADR 0010.

## Decision

1. **A low-level Slack package for applications that own their runtime.** `adapters/slack/slackapi` works on raw Slack IDs (team, channel, and user IDs and message timestamps). It holds Slack protocol and data formats only:
   - a Web API client with typed methods for the calls a bot needs, `Call` as the escape hatch for other methods, `PostResponseURL`, a typed `APIError`, and bounded retry with `RetryPolicy` and `RateLimited` that keeps the ADR 0005 invariants;
   - files: the three-step external upload and a size-limited download, both restricted to the configured file origins;
   - inbound: `VerifyRequest`, `ParseEnvelope`, and `(*Envelope).MessageEvent`;
   - formats: Block Kit types, `SplitMarkdown`, the user mention helpers, and the app `Manifest` types.

   It has no runtime, state, dispatch, dedupe, locks, or policy. The application brings those.

2. **The Slack adapter moves onto `slackapi` for the Web API call, retry, signature check, and history reads.** There is one implementation of each. The adapter's `HistoryReader` calls `ConversationReplies` and `ConversationHistory`. The adapter's methods, options, and error text do not change.

3. **JSON by default, form encoding where Slack documents it.** Methods send a JSON POST with the bearer token. Slack documents `users.info`, `conversations.info`, `conversations.history`, `conversations.replies`, and `files.getUploadURLExternal` as GET or form-only methods, so the client sends them as a form-encoded POST with the bearer token. `Call` picks the encoding from its payload: a `url.Values` payload is sent as a form and any other payload as JSON, so the escape hatch reaches form-only methods too. The POST to an upload URL sends raw bytes and no token, as the official Slack Python SDK does, because the upload URL is pre-authorized.

4. **File URLs stay on Slack file hosts.** The upload and the download accept only URLs whose origin is in `Options.FileOrigins`, and both check every redirect too. A URL from an untrusted payload cannot send the token or file content to another host.

5. **`slackapitest` is exported.** It holds a fake Slack Web API server and a request signer, so downstream applications can test their Slack code against the same fake that this repository uses, without a live workspace.

## Consequences

- Applications that own their runtime can use Slack without the **Go Chat Runtime**, and share the adapter's tested Slack code instead of copying it.
- The Slack adapter now also retries the Slack error code `rate_limited` (the `chat.postMessage` workspace message cap), in addition to HTTP 429 and `ratelimited`. A post that failed at once with `rate_limited` now retries within the **Retry Policy** and returns a typed `RateLimited` when retry stops. This extends the Slack throttling map in ADR 0005.
- The Slack adapter's `HistoryReader` now sends form-encoded requests. Before, it sent JSON, which Slack rejects with `invalid_arguments`, so every `ReadHistory` call failed on real Slack.
- The Slack adapter's `HistoryReader` now returns thread replies newest-first, as its GoDoc says, with the thread root once, on the page that reaches the start of the thread. Before, it returned each `conversations.replies` page as Slack sends it: oldest-first, with the root at the start of every page.
- `slack.RetryPolicy` and `slack.RateLimited` are now type aliases of `slackapi.RetryPolicy` and `slackapi.RateLimited`. Existing code compiles unchanged, and `errors.As` matches either name.
- The portable surface does not grow. The `chat.Adapter` interface and the `Thread` API do not change, and ADR 0004's deferral of portable edit, delete, and reactions stands. Slack's edit, delete, reaction, and file methods exist only in `slackapi`, for Slack-only callers.
- The module keeps zero dependencies.
- Cost: `slackapi` and `slackapitest` are public API. Both start experimental, so their API may change before promotion. `RetryPolicy` and `RateLimited` are the exception, because the supported adapter aliases them. `slackapi` is not a complete Slack SDK: a method with no typed wrapper goes through `Call`.

## Alternatives Considered

### Add the methods to the Slack `Adapter`

Rejected. Adapter methods work on opaque **Thread IDs** and on configuration built for the runtime, so an application with its own runtime would have to build an adapter that it does not route through. Every new method would also widen a **Platform Adapter** past what the runtime needs.

### Export only a thin `Call`

Rejected. Each application would write its own request and response types, form-encoding exceptions, upload steps, and host checks, and get the same Slack details wrong. `Call` stays as the escape hatch for methods with no typed wrapper.

### Keep a Slack client inside each application

Rejected. Slack protocol code would live in two places, with two retry and two signature implementations that drift apart. Slack-specific code belongs in this repository, next to the adapter that already tests it.

### Depend on `slack-go/slack`

Rejected. The module has zero dependencies, and `slack-go/slack` is a large one. It also has its own retry and error types, which do not follow ADR 0005 (bounded retry, the context-deadline invariant, and a typed `RateLimited`).
