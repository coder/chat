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
2. **Detached tail, launched at ack time**: your handler runs on a
   runtime-managed detached work context, concurrently with the webhook
   response — the acknowledgement no longer waits on your handler (though the
   tail may begin before the 2xx is actually written). The runtime renews the
   thread lock lease in the background while the handler runs. If the state
   backend fails to extend the lease (an error or a lost lease), renewal
   stops and is logged/observed, but the handler keeps running **without
   exclusivity** — after the original `ThreadLockTTL` expires, another event
   on the same thread can acquire the lock and run concurrently. Long
   handlers should therefore be idempotent or tolerate overlap under state
   backend failures.

## Enable It

`WithRuntimeOptions` replaces the whole options struct (it does not merge), so
start from `chat.DefaultRuntimeOptions()` to keep the required `DedupeTTL` and
`ThreadLockTTL` defaults:

```go
opts := chat.DefaultRuntimeOptions()
opts.Dispatch = chat.DispatchDeferred
opts.DetachTimeout = 5 * time.Minute
opts.Concurrency = chat.ConcurrencyQueue

bot, err := chat.New(ctx,
	chat.WithState(state),
	chat.WithAdapter(adapter),
	chat.WithRuntimeOptions(opts),
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
  events instead. Two caveats:
  - Coalescing is **per process**: with multiple bot replicas, the shared
    state lock still serializes handlers, but follow-ups that landed on
    different replicas each run in turn. If superseded events must never
    execute twice, route each thread's webhooks to one replica or make
    handlers idempotent.
  - A queued follow-up's `DetachTimeout` clock starts when it is accepted,
    **before** it waits for the lock. Time spent queued behind a long handler
    consumes the follow-up's own budget; if the wait exhausts it, the
    follow-up is cancelled without running (it was already deduped, so it
    will not be redelivered). Size `DetachTimeout` to cover your longest
    handler *plus* the queue wait behind it.

## Write Handlers For The Detached Context

Your handler code does not change shape — it still receives a
`context.Context` — but under `DispatchDeferred` that context is the detached
work context, not the HTTP request context:

```go
bot.OnNewMention(func(ctx context.Context, ev *chat.MessageEvent) error {
	// The acknowledgement is not waiting on you. Take your time (within
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
  and is how the runtime signals cancellation.
- Handler errors after ack are recorded and observed, not retried by the
  platform. If your work must not be lost, make it idempotent and consider
  your own queue.
- `Shutdown(ctx)` cancels the detached work contexts first, then waits
  (bounded by the context you pass it) for handlers to observe cancellation
  and return. In-flight generation is aborted, not completed — deferred
  dispatch is not a durable queue, so work that must survive a rolling deploy
  belongs in application-owned persistence.

## When Not To Use It

Stay with `DispatchSync` when handlers are fast (a quick reply, a state
lookup) — synchronous dispatch keeps the failure story simpler because a
handler error still happens before the platform ack. Streaming token
transports are a non-goal of the core runtime; deferred dispatch plus one
finished message is the supported long-generation pattern (see
[ADR 0011](../adr/0011-resumable-streaming.md)).
