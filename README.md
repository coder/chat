# Chat SDK Go

Chat SDK Go is a Go-native runtime for building chat bots with the core
conversation model of Vercel Chat SDK: adapters, normalized events, threads,
subscriptions, state-backed dedupe, and thread-scoped replies.

This is not a TypeScript API port and not a promise of full Vercel Chat SDK
feature parity. The goal is semantic compatibility where the model maps cleanly
to Go, with deliberate Go-shaped differences where that makes the runtime
simpler, safer, or easier to operate.

Status: the MVP runtime is implemented. The public surface is still early, but
the core runtime, Slack adapter, memory state, Redis state module, and public
contract tests are in place.

## Design Goals

- Go-native API built around `context.Context`, `net/http`, small interfaces,
  and explicit errors.
- Slack-first vertical slice before claiming multi-platform portability.
- Required runtime state for subscriptions, dedupe, and locks.
- Memory state for tests and local development.
- Redis state for horizontally scaled production deployments.
- Thread-oriented application code: handle a message, subscribe the thread,
  reply to the thread.
- Platform escape hatches without making raw platform structs the normal API.
- Vercel Chat SDK behavior as the default precedent unless it is non-idiomatic
  in Go or outside the MVP scope.

## Install

The core module is:

```sh
go get github.com/coder/chat
```

Redis state is optional and lives in its own module so applications that only
use core, Slack, or memory state do not pull Redis dependencies:

```sh
go get github.com/coder/chat/state/redis
```

Package layout:

```text
github.com/coder/chat
github.com/coder/chat/adapters/slack
github.com/coder/chat/state/memory
github.com/coder/chat/state/redis
```

This repository uses `go.work` for local development across the root module and
the Redis state module.

## Example

```go
package main

import (
	"context"
	"log/slog"
	"net/http"
	"os"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/coder/chat"
	"github.com/coder/chat/adapters/slack"
	chatredis "github.com/coder/chat/state/redis"
)

func main() {
	ctx := context.Background()

	redisState, err := chatredis.New(ctx, chatredis.Options{
		Client: redis.NewClient(&redis.Options{
			Addr: os.Getenv("REDIS_ADDR"),
		}),
	})
	if err != nil {
		panic(err)
	}

	slackAdapter, err := slack.New(ctx, slack.Options{
		SigningSecret: os.Getenv("SLACK_SIGNING_SECRET"),
		BotToken:      os.Getenv("SLACK_BOT_TOKEN"),
	})
	if err != nil {
		panic(err)
	}

	bot, err := chat.New(ctx,
		chat.WithState(redisState),
		chat.WithAdapter(slackAdapter),
		chat.WithLogger(slog.Default()),
		chat.WithRuntimeOptions(chat.RuntimeOptions{
			DedupeTTL:     24 * time.Hour,
			ThreadLockTTL: 2 * time.Minute,
			Concurrency:   chat.ConcurrencyDrop,
		}),
	)
	if err != nil {
		panic(err)
	}
	defer func() {
		if err := bot.Shutdown(context.Background()); err != nil {
			slog.Error("chat shutdown failed", "error", err)
		}
	}()

	bot.OnNewMention(func(ctx context.Context, ev *chat.MessageEvent) error {
		if !userIsLinked(ev.Message.Author) {
			_, err := ev.Thread.PostEphemeral(ctx, ev.Message.Author, chat.Text(
				"Please link your account before I continue.",
			), chat.EphemeralOptions{
				FallbackToDM: true,
			})
			return err
		}

		if err := ev.Thread.Subscribe(ctx); err != nil {
			return err
		}

		_, err := ev.Thread.Post(ctx, chat.Markdown(
			"I'm listening to this thread now.",
		))
		return err
	})

	bot.OnSubscribedMessage(func(ctx context.Context, ev *chat.MessageEvent) error {
		_, err := ev.Thread.Post(ctx, chat.Text("You said: "+ev.Message.Text))
		return err
	})

	slackWebhook, err := bot.Webhook("slack")
	if err != nil {
		panic(err)
	}

	http.Handle("/webhooks/slack", slackWebhook)
	if err := http.ListenAndServe(":8080", nil); err != nil {
		panic(err)
	}
}

func userIsLinked(chat.Actor) bool {
	return false
}
```

## Core Model

`Chat` is the runtime. It owns adapter registration, runtime state, handler
registration, webhook mounting, dispatch, dedupe, locking, and shutdown.

`Platform Adapter` is a platform boundary. It verifies inbound webhooks,
normalizes platform payloads, renders outbound messages, and exposes
platform-specific APIs through typed adapter access. It does not own application
routing.

`Event` is the normalized inbound envelope. A `Message` is one payload type
inside an event, not the name for every inbound platform occurrence.

`MessageEvent` is the handler input for message routing hooks. It carries the
normalized event, thread, and message together.

`Thread` is the stable conversation address used for routing, subscription,
and replies. In Slack, a root channel message becomes a thread rooted at that
message timestamp, not the entire channel.

`ThreadID` is opaque and adapter-produced. It must include adapter identity and
enough platform tenant/routing context to avoid collisions across workspaces,
channels, and platforms. Application code may store and pass it around, but
must not build it manually.

`Thread Handle` reconstruction is supported for out-of-webhook work:

```go
thread, err := bot.Thread(ctx, threadID)
if err != nil {
	return err
}

_, err = thread.Post(ctx, chat.Text("Reminder"))
```

The runtime decodes the adapter prefix, asks the adapter to validate the
thread ID, and returns an error for unknown adapters or invalid IDs.

## Runtime Construction

Construction is fail-fast:

```go
bot, err := chat.New(ctx,
	chat.WithState(state),
	chat.WithAdapter(slackAdapter),
)
```

`chat.New` validates state, adapter registration, runtime options, and adapter
initialization before webhooks are served. This is an intentional difference
from Vercel Chat SDK, which initializes lazily on first use.

`Shutdown(ctx)` is idempotent. It attempts all adapter cleanup hooks before
state cleanup and returns joined errors if cleanup fails.

## Webhooks

The runtime exposes `net/http` handlers and does not own the HTTP server:

```go
handler, err := bot.Webhook("slack")
if err != nil {
	return err
}

http.Handle("/webhooks/slack", handler)
```

Webhook lookup is fallible. A misspelled adapter name is a startup/configuration
error, not a production 404.

Adapters own platform handshakes. For Slack, URL verification is handled inside
the Slack webhook handler and never reaches application handlers.

## Routing

The MVP has two message routing hooks:

```go
bot.OnNewMention(func(context.Context, *chat.MessageEvent) error)
bot.OnSubscribedMessage(func(context.Context, *chat.MessageEvent) error)
```

Routing order:

1. Ignore self-authored bot messages.
2. Route messages in subscribed threads to `OnSubscribedMessage`.
3. Route mentions in unsubscribed threads to `OnNewMention`.
4. A valid but unsupported or irrelevant platform event is acknowledged and
   ignored.

Direct messages are treated as implicit mentions. An unsubscribed direct message
routes to `OnNewMention`; once subscribed, later direct messages route to
`OnSubscribedMessage`.

Handlers are single-slot per hook. Calling `OnNewMention` or
`OnSubscribedMessage` again atomically replaces the previous handler. Missing
handlers are no-ops. This intentionally differs from Vercel Chat SDK, which
allows multiple handlers per hook.

Subscriptions are explicit:

```go
if err := ev.Thread.Subscribe(ctx); err != nil {
	return err
}
```

Replying successfully to a new mention does not subscribe the thread. A
subscription lasts until explicit unsubscribe.

## Dispatch And Acknowledgement

MVP dispatch is synchronous and uses the inbound webhook request context.
Long-running work should be explicitly detached or queued by application code.

Once a webhook is verified and normalized into an accepted event, handler errors
are recorded but acknowledged to the platform by default. This avoids platform
retry storms after partial side effects such as posting a message.

Invalid signatures and malformed requests are rejected. Valid but unsupported
platform events are acknowledged and ignored.

## Runtime State

State is required. The runtime must not silently create memory state for
production-facing construction.

Runtime state is coordination state:

- subscribed thread membership
- event dedupe
- thread locks
- runtime cache needed by adapters

Runtime state is not product state. Store application workflow data in your own
database keyed by `ThreadID`.

State implementations:

- `state/memory`: tests and local development, included in the root module
- `state/redis`: production and horizontally scaled deployments, kept in the
  separate `github.com/coder/chat/state/redis` module

## Dedupe, Locks, And Concurrency

Event dedupe uses `Event Identity`, not delivery retry metadata. Slack retry
headers are logged as retry metadata but are not part of the dedupe key.

Default runtime options:

```go
chat.RuntimeOptions{
	DedupeTTL:     24 * time.Hour,
	ThreadLockTTL: 2 * time.Minute,
	Concurrency:   chat.ConcurrencyDrop,
}
```

The MVP implements only `ConcurrencyDrop`. Queue, debounce, force, and
concurrent strategies are future-compatible names, not MVP behavior.

Thread locks use token-owned lock leases. Release and extend operations must
verify the token so an expired handler cannot release or extend another
handler's newer lock.

Lock conflict behavior defaults to acknowledge-and-drop. A lock conflict is
observed as unhandled runtime contention and should not trigger platform retry.

## Messages

The MVP outbound surface is intentionally small:

```go
ev.Thread.Post(ctx, chat.Text("plain text"))
ev.Thread.Post(ctx, chat.Markdown("**portable** formatting intent"))
```

`Text` means no formatting intent. `Markdown` means conservative CommonMark
formatting intent, not Slack `mrkdwn`, GitHub-flavored Markdown, or a
platform-native rich payload. Adapters may render, translate, or degrade it.
The Slack adapter uses Slack's `markdown_text` posting field for Markdown
messages rather than converting CommonMark to `mrkdwn` itself.

Posting returns `SentMessage` identity. Edit, delete, reactions, files, cards,
modals, and native rich payload builders are outside the MVP.

## Ephemeral Messages

Ephemeral delivery is required for the Slack-first slice:

```go
sent, err := ev.Thread.PostEphemeral(ctx, ev.Message.Author, chat.Text(
	"Please link your account.",
), chat.EphemeralOptions{
	FallbackToDM: true,
})
```

An ephemeral message is not a normal thread reply and must never fall back to a
public reply.

Fallback is explicit:

- If native ephemeral delivery works, the adapter sends native ephemeral output.
- If native ephemeral delivery is unavailable and `FallbackToDM` is true, the
  adapter may deliver through a direct message thread.
- If native ephemeral delivery is unavailable and `FallbackToDM` is false, the
  operation returns no delivered message.
- If fallback is requested but impossible, the operation returns an error.

Ephemeral behavior is modeled as an optional adapter capability through small Go
interfaces, not string capability flags.

## Actors And Identity

`Actor` is scoped by adapter and platform tenant. Raw Slack user IDs are not
global identities.

Bot-ness is explicit:

```go
type BotKind int

const (
	BotUnknown BotKind = iota
	BotHuman
	BotBot
)
```

Self-authored bot messages are ignored before subscription or mention routing.

Application identity is not part of the runtime. Account linking, login prompts,
pending auth flows, and product user records belong to the application.

## Adapter Access

Normalized APIs should cover common flows. Platform-specific APIs are still
reachable through typed adapter access:

```go
slackAdapter, ok := chat.AdapterAs[*slack.Adapter](bot, "slack")
if !ok {
	return errors.New("slack adapter is not registered")
}
```

Examples should prefer this helper over unchecked type assertions.

## Slack MVP

The Slack adapter is the first production-shaped adapter. It must support:

- single-install configuration
- signing secret verification
- URL verification
- bot identity discovery during adapter initialization
- supported-shape decoding with unknown-field tolerance
- message-created normalization
- direct-message normalization
- root-message thread rooting
- self-message filtering
- retry metadata observation
- thread replies
- native ephemeral messages
- explicit ephemeral DM fallback

The adapter should use local structs for the Slack payload shapes it supports,
preserve raw payload data as an escape hatch, and validate required fields for
supported event types.

## Intentional MVP Gaps

These are not bugs in the MVP:

- no TypeScript API compatibility
- no full Vercel Chat SDK feature parity
- no multiple handlers per routing hook
- no lazy runtime initialization
- no multi-platform MVP
- no multi-workspace Slack OAuth installation flow
- no dedicated `OnDirectMessage` hook
- no public proactive `OpenDM`, except adapter behavior needed for explicit
  ephemeral fallback
- no pattern handlers
- no slash commands
- no middleware
- no message history APIs
- no thread application state APIs
- no rich cards, JSX cards, modals, files, or native rich payload builders
- no edit, delete, reaction, or other outbound mutation APIs
- no queue, debounce, force, or concurrent lock-conflict strategies
- no built-in HTTP server or router integrations
- no adapter marketplace/package conventions

## Testing Contract

Tests should verify external behavior and public contracts, not private
implementation details.

Required test families:

- runtime construction and shutdown
- handler registration and replacement
- routing order and no-op missing handlers
- explicit subscription and unsubscribe
- direct-message implicit mention routing
- self-message filtering
- accepted, ignored, rejected, duplicate, and lock-conflict events
- state conformance across memory and Redis
- token-owned lock lease acquire, release, extend, expiry, and stale release
- Slack signature verification and URL verification
- Slack golden payload normalization
- thread ID construction and validation
- thread handle reconstruction
- text, markdown, sent message, ephemeral, and ephemeral fallback posting
- typed adapter access
- README and GoDoc coverage for intentional Vercel differences

Local test commands:

```sh
mise run test
mise run test:root
mise run test:adapters
```

`mise run test` is a composite task that runs the root module tests and
`test:adapters`; the adapter-focused task also exercises the Redis state module.
