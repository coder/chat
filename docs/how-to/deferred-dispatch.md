# How To Defer Long-Running Work (Ack-Then-Work)

Chat platforms expect webhooks to be acknowledged quickly — Slack retries
after 3 seconds, Linear expects agent activity within ~10 seconds. If your
handler calls an LLM or does anything slow, the default synchronous dispatch
mode will run it *before* acknowledging the webhook, and the platform will
retry or time out.

`DispatchDeferred` splits dispatch in two (see
[ADR 0002](../adr/0002-async-dispatch.md)):

1. **Prelude, before ack**: signature verification, normalization, dedupe
   marking, and thread lock acquisition run synchronously on the request
   context.
2. **Detached tail, after ack**: your handler runs on a runtime-managed
   detached work context while the platform already has its 2xx. The runtime
   keeps extending the thread lock lease in the background until the handler
   returns.

## Enable It

```go
bot, err := chat.New(ctx,
	chat.WithState(state),
	chat.WithAdapter(adapter),
	chat.WithRuntimeOptions(chat.RuntimeOptions{
		Dispatch:      chat.DispatchDeferred,
		DetachTimeout: 5 * time.Minute,
		Concurrency:   chat.ConcurrencyQueue,
	}),
)
```

- `Dispatch: chat.DispatchDeferred` turns on ack-then-work. The default is
  `chat.DispatchSync`, which runs the handler before acknowledging.
- `DetachTimeout` bounds how long a detached handler may run after the webhook
  request has ended.
- `Concurrency: chat.ConcurrencyQueue` is the natural companion: while a
  detached handler holds the thread lock, follow-up events on the same thread
  wait instead of being dropped, and only the most recent superseded follow-up
  runs. The default `chat.ConcurrencyDrop` acknowledges and drops conflicting
  events instead.

## Write Handlers For The Detached Context

Your handler code does not change shape — it still receives a
`context.Context` — but under `DispatchDeferred` that context is the detached
work context, not the HTTP request context:

```go
bot.OnNewMention(func(ctx context.Context, ev *chat.MessageEvent) error {
	// The platform already has its ack. Take your time (within
	// DetachTimeout): call the LLM, run tools, then post.
	answer, err := generate(ctx, ev.Message.Text)
	if err != nil {
		return err
	}
	_, err = ev.Thread.Post(ctx, chat.Markdown(answer))
	return err
})
```

Rules that keep this safe:

- Use the `ctx` you are given for every call. It carries the detach timeout
  and is what `Shutdown` uses to drain in-flight work.
- Handler errors after ack are recorded and observed, not retried by the
  platform. If your work must not be lost, make it idempotent and consider
  your own queue.
- `Shutdown(ctx)` waits for detached tails to finish (bounded by the context
  you pass it), so a rolling deploy does not sever half-finished replies.

## When Not To Use It

Stay with `DispatchSync` when handlers are fast (a quick reply, a state
lookup) — synchronous dispatch keeps the failure story simpler because a
handler error still happens before the platform ack. Streaming token
transports are a non-goal of the core runtime; deferred dispatch plus one
finished message is the supported long-generation pattern (see
[ADR 0011](../adr/0011-resumable-streaming.md)).
