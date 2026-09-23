# How To Defer Long-Running Work (Ack-Then-Work)

Slack retries a webhook that is not acknowledged within 3 seconds. Linear
expects a first agent activity within about 10 seconds. By default the runtime
runs your handler *before* it acknowledges the webhook, so a handler that
calls an LLM can easily miss those deadlines.

Deferred dispatch fixes this. The acknowledgement no longer waits for your
handler, which runs on a detached context
([ADR 0002](../adr/0002-async-dispatch.md)).

## Enable It

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

`WithRuntimeOptions` replaces the whole options struct; it does not merge.
Start from `chat.DefaultRuntimeOptions()` so you keep the required
`DedupeTTL` and `ThreadLockTTL` defaults.

| Option | Default | What it does |
| --- | --- | --- |
| `Dispatch` | `DispatchSync` | `DispatchDeferred` turns on ack-then-work. |
| `DetachTimeout` | `0` | How long a handler may run after the webhook request ends. Required under deferred dispatch: `chat.New` fails while it is zero. |
| `Concurrency` | `ConcurrencyDrop` | What happens to an event that arrives while a handler holds the thread lock. See [Pick a concurrency strategy](#pick-a-concurrency-strategy). |
| `MaxDetached` | `1024` | The cap on deferred work in flight. See [Handle overload](#handle-overload). |

## How It Works

Dispatch runs in two parts:

1. **Before the acknowledgement**, on the request context: signature
   verification, normalization, dedupe marking, and thread lock acquisition.
2. **Launched at acknowledgement time**, on a runtime-managed detached
   context: your handler. It runs concurrently with the webhook response and
   may start just before the 2xx is written. While it runs, the runtime renews the
   thread lock lease in the background.

If the lease is lost — the state backend fails to extend it, it expires, or
another runtime instance releases it — the runtime cancels the handler's
context with `chat.ErrPreempted` as the cause (`context.Cause(ctx)`). The
handler no longer has the thread to itself, so treat `ErrPreempted` as "stop,
someone else may own this thread now". Cancellation is cooperative: a handler
that ignores its context keeps running.

## Write Handlers For The Detached Context

Handlers keep the same signature. Under deferred dispatch the `ctx` they
receive is the detached context, not the HTTP request context:

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

Follow three rules:

- **Use the `ctx` you are given for every call.** It carries the
  `DetachTimeout` deadline and the runtime's cancellation signal.
- **Expect no platform retry.** An error returned after the acknowledgement
  is logged and observed, but the platform never redelivers the event. If the
  work must not be lost, make it idempotent and put it on your own queue.
- **Expect shutdown to abort work.** `Shutdown(ctx)` cancels the detached
  contexts first, then waits (bounded by the context you pass it) for
  handlers to return. Deferred dispatch is not a durable queue; work that
  must survive a rolling deploy belongs in your own persistence.

## Pick A Concurrency Strategy

Deferred handlers hold the thread lock longer, so events that overlap on one
thread become common. The concurrency strategy decides what happens to them
([ADR 0012](../adr/0012-concurrency-strategy.md)):

| Strategy | What happens to an overlapping event |
| --- | --- |
| `ConcurrencyDrop` (default) | It is acknowledged and dropped. |
| `ConcurrencyQueue` | It waits for the running handler. Only the newest waiting event runs; older waiting events are superseded, and supersession is observable. |
| `ConcurrencyDebounce` | Each new event replaces the waiting one; only the last event of a `DebounceInterval` quiet period runs. Requires a `DetachTimeout` longer than `DebounceInterval`. |
| `ConcurrencyConcurrent` | There is no thread lock. Every event runs, up to `MaxConcurrent` at once. |
| `ConcurrencyBurst` | Events collect for a fixed `BurstWindow`, then run as one batch, in join order, under a single lock hold. Nothing accepted is dropped, and each member gets its own `DetachTimeout`. `MaxBurstBatch` optionally closes a full window early; batches run in the order they close. |

Debounce and burst require deferred dispatch. `DebounceInterval`,
`MaxConcurrent`, and `BurstWindow` must be positive under their strategy, or
`chat.New` fails. The `chat.ConcurrencyBurst` GoDoc has the full burst
lifecycle.

`ConcurrencyQueue` suits most conversational bots: a follow-up sent while
the bot is still working waits instead of disappearing. Two caveats:

- **Coalescing happens inside one process.** With several replicas, the
  shared state lock still serializes handlers, but follow-ups that landed on
  different replicas each run in turn. The same holds for debounce and
  burst batching. If a superseded event must never run, route each thread's
  webhooks to one replica or make handlers idempotent.
- **Queue time counts against `DetachTimeout`.** A queued event's clock
  starts when it is accepted, before it waits for the lock. If the wait uses
  up its budget, the event is cancelled without running, and because it was
  already deduped, the platform will not redeliver it. Size `DetachTimeout`
  for your longest handler *plus* the queue wait behind it.

## Handle Overload

`MaxDetached` caps admitted-but-incomplete deferred deliveries: running
handlers plus events waiting under the queue, debounce, concurrent, and
burst strategies ([ADR 0015](../adr/0015-runtime-coordination.md)). It stops
an event flood from growing goroutines and retained payloads without limit.
Sizing guidance lives on the `MaxDetached` GoDoc.

A delivery that arrives at the cap is rejected with
`chat.ErrAdmissionRejected` before the acknowledgement and before dedupe
marking, so a later redelivery is not mistaken for a duplicate. The adapter
answers the platform:

| Delivery | Response |
| --- | --- |
| Slack Events API callback, Linear webhook | 503, so the platform retries. |
| Slack slash command | 200 with a visible "at capacity" message. |
| Slack `block_actions` click | 503, which Slack shows as a warning on the component. |

Slack does not redeliver slash commands or clicks, so those get an honest
busy signal instead of a retry.

`MaxDetachedPerTenant` optionally caps one installation's share through the
same rejection path.

## When Not To Use It

Keep `DispatchSync` when handlers are fast, such as a quick reply or a state
lookup. The failure story is simpler: a handler error happens before the
platform acknowledgement.

Token streaming is not part of the core runtime. For long generation, use
deferred dispatch and post one finished message
([ADR 0011](../adr/0011-resumable-streaming.md)).
