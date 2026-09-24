// Package chat is the runtime for Chat SDK Go. It routes normalized chat
// platform events to your handlers and posts replies through platform
// adapters. The Slack adapter ([github.com/coder/chat/adapters/slack]) and the
// Linear adapter ([github.com/coder/chat/adapters/linear]) ship in this
// module.
//
// This page is the API reference. New users should start with the [tutorial];
// the [how-to guides] cover specific tasks, and the [explanation] covers the
// design.
//
// # Core Model
//
// An inbound request passes through four pieces:
//
//   - An [Adapter] is the platform boundary. Its webhook handler verifies the
//     request and normalizes the platform payload into an [Event].
//   - An [Event] is the normalized envelope. It carries at most one payload: a
//     [Message], a [Command], or an [Interaction].
//   - A [Thread] is the conversation the event belongs to, addressed by an
//     opaque, adapter-produced [ThreadID]. Handlers reply with [Thread.Post]
//     and stay in the conversation with [Thread.Subscribe].
//   - A handler is your code. [Chat] routes an event to at most one
//     single-slot hook: [Chat.OnCommand] for commands, [Chat.OnInteraction]
//     for component actions, [Chat.OnSubscribedMessage] for messages in a
//     subscribed thread, and [Chat.OnNewMention] for mentions and direct
//     messages in any other thread. Every other valid event is acknowledged
//     and ignored.
//
// Between the adapter and the handler, the runtime dedupes redeliveries by
// event ID, ignores events from the bot itself, and serializes handlers for
// one thread with a lock lease (unless you opt out with
// [ConcurrencyConcurrent]). The lease lasts [RuntimeOptions].ThreadLockTTL.
// Only deferred dispatch renews it, so under the default synchronous dispatch
// keep handlers well inside that TTL.
//
// # Construction
//
// [New] fails fast: it requires a [State] and at least one [Adapter],
// validates the [RuntimeOptions], and initializes every adapter before it
// returns. The runtime serves webhooks through [net/http] handlers and does
// not own the HTTP server:
//
//	bot, err := chat.New(ctx,
//		chat.WithState(memory.New()), // Redis, Postgres, or NATS in production
//		chat.WithAdapter(slackAdapter),
//	)
//	if err != nil {
//		return err
//	}
//	defer func() {
//		if err := bot.Shutdown(context.Background()); err != nil {
//			slog.Error("chat shutdown failed", "error", err)
//		}
//	}()
//
//	bot.OnNewMention(func(ctx context.Context, ev *chat.MessageEvent) error {
//		_, err := ev.Thread.Post(ctx, chat.Text("hello"))
//		return err
//	})
//
//	webhook, err := bot.Webhook("slack")
//	if err != nil {
//		return err
//	}
//	http.Handle("/webhooks/slack", webhook)
//
// Replying does not subscribe a thread; call [Thread.Subscribe] to route the
// thread's later messages to [Chat.OnSubscribedMessage]. Use [Chat.Thread] to
// rebuild a thread from a stored [ThreadID] for work outside a webhook, such
// as reminders.
//
// # Dispatch Modes
//
// [RuntimeOptions].Dispatch selects when the handler runs relative to the
// platform acknowledgement:
//
//   - [DispatchSync], the default, runs the handler on the webhook request
//     context and acknowledges after it returns. Use it for fast handlers.
//   - [DispatchDeferred] (ack-then-work) runs dedupe before the
//     acknowledgement, then runs the handler on a detached context bounded by
//     [RuntimeOptions].DetachTimeout, which must be positive. Under every
//     strategy except [ConcurrencyConcurrent], which takes no lock, the
//     runtime renews the thread lock while the handler runs; if the lease is
//     lost, it cancels the handler's context with [ErrPreempted] as the cause.
//
// In both modes an accepted event is acknowledged even when its handler
// returns an error; the error is logged and observed, not retried.
//
// Under deferred dispatch, [RuntimeOptions].MaxDetached is the admission
// bound: a per-instance cap on deliveries that were admitted but have not
// finished, including events waiting their turn. It must be positive, and
// [DefaultRuntimeOptions] sets 1024. [RuntimeOptions].MaxDetachedPerTenant
// optionally caps one installation's share. A delivery that arrives at a cap
// is rejected with [ErrAdmissionRejected] before the acknowledgement and
// before dedupe marking, so the platform's retry is not mistaken for a
// duplicate.
//
// [RuntimeOptions].Concurrency decides what happens to an event that arrives
// while a handler holds its lock scope, which is one thread by default or the
// whole channel with [LockScopeChannel]: [ConcurrencyDrop] (the default),
// [ConcurrencyQueue], [ConcurrencyDebounce], [ConcurrencyConcurrent], or
// [ConcurrencyBurst]. Debounce and burst require deferred dispatch.
//
// [WithRuntimeOptions] replaces the whole options struct, so start from
// [DefaultRuntimeOptions] and change only the fields you need.
//
// # State Backends
//
// [State] holds coordination data only: thread subscriptions, dedupe marks,
// and lock leases. It is required; the runtime never creates one silently.
// Store application data, such as transcripts, in your own storage keyed by
// [ThreadID]. Four implementations ship, and all pass one conformance suite:
//
//   - [github.com/coder/chat/state/memory], in this module, for tests and
//     single-process development.
//   - [github.com/coder/chat/state/redis], [github.com/coder/chat/state/postgres],
//     and [github.com/coder/chat/state/nats], each in its own module, for
//     production and for replicas that share state.
//
// # Adapter Capabilities
//
// Features that not every platform supports are optional capabilities:
// interfaces such as [HistoryReader], [EphemeralPoster], and
// [NativeContentPoster] that an adapter may implement in addition to
// [Adapter]. Look one up with [AdapterAs]; ok is false when the adapter does
// not implement it. [AdapterAs] also returns a concrete adapter type for
// platform-specific APIs, such as opening a Slack modal.
//
// # Observability
//
// The runtime logs through [log/slog] ([WithLogger]). [WithObserver] installs
// your [Observer], which receives counter-style observation events and opens a
// [DispatchSpan] for each dispatch that ends with a terminal [DispatchOutcome].
// The default observer records nothing, and the core imports no telemetry
// libraries.
//
// # Further Reading
//
//   - [tutorial]: build and run a Slack bot in under 30 minutes.
//   - [how-to guides]: state backends, deferred dispatch, slash commands,
//     interactive components, multi-tenant installs, and Linear agent
//     sessions.
//   - [reference]: runtime behavior in prose, adapter capability status, and
//     the runnable example programs.
//   - [explanation]: design goals, the Vercel Chat SDK concept map, and
//     non-goals, with an index of the architecture decision records.
//
// [tutorial]: https://github.com/coder/chat/blob/main/docs/tutorials/slack-bot.md
// [how-to guides]: https://github.com/coder/chat/blob/main/docs/README.md#how-to-guides
// [reference]: https://github.com/coder/chat/blob/main/docs/reference.md
// [explanation]: https://github.com/coder/chat/blob/main/docs/explanation.md
package chat
