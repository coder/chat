# How To Choose A State Backend

Every bot needs a `chat.State`. The runtime keeps three things there:
which threads are subscribed, which events it has already handled (dedupe
marks), and who holds each thread's lock. That is coordination state, not
product state: keep your application's data in your own database, keyed by
`ThreadID`.

## Pick One

| Backend | Module | Use it when |
| --- | --- | --- |
| Memory | `github.com/coder/chat/state/memory` (core module) | You are writing tests or following the tutorial. State is lost on restart. |
| Redis | `github.com/coder/chat/state/redis` | You already run Redis. |
| Postgres | `github.com/coder/chat/state/postgres` | You already run Postgres. |
| NATS JetStream | `github.com/coder/chat/state/nats` | You already run NATS with JetStream. |

The three durable backends are interchangeable: they implement the same
token-owned lock lease and dedupe contract and pass the same conformance
suite, so any of them lets you run several bot replicas safely. Pick the one
you already operate. Redis and Postgres are tested against real servers via
Testcontainers; NATS is tested against an embedded JetStream server.

Redis, Postgres, and NATS are separate Go modules, so an application that
uses only the core module does not pull their dependencies.

Whichever you pick, give each bot application its own namespace; see
[One namespace per bot application](#one-namespace-per-bot-application).

## Memory

```go
import "github.com/coder/chat/state/memory"

bot, err := chat.New(ctx,
	chat.WithState(memory.New()),
	chat.WithAdapter(adapter),
)
```

Memory state is for tests and local development only. Subscriptions and dedupe
data vanish when the process exits, so a restarted bot forgets which threads it
was in and may re-handle redelivered events.

## Redis

```sh
go get github.com/coder/chat/state/redis
```

```go
import (
	"github.com/redis/go-redis/v9"

	chatredis "github.com/coder/chat/state/redis"
)

redisOptions, err := redis.ParseURL(os.Getenv("REDIS_URL")) // e.g. redis://127.0.0.1:6379/0
if err != nil {
	return err
}
redisState, err := chatredis.New(ctx, chatredis.Options{
	Client: redis.NewClient(redisOptions),
	Prefix: "mybot", // see "One namespace per bot application" below
})
```

The runnable example, including a `compose.yaml` for a local Redis, is
[`examples/slack-redis-state`](../../examples/slack-redis-state/README.md).

## Postgres

```sh
go get github.com/coder/chat/state/postgres
```

```go
import (
	"github.com/jackc/pgx/v5/pgxpool"

	chatpostgres "github.com/coder/chat/state/postgres"
)

pool, err := pgxpool.New(ctx, os.Getenv("DATABASE_URL"))
if err != nil {
	return err
}

pgState, err := chatpostgres.New(ctx, chatpostgres.Options{
	Pool:      pool,
	Namespace: "mybot", // see "One namespace per bot application" below
})
```

The Postgres state initializes its own schema (subscription, event, and lock
tables) on startup. The runnable example is
[`examples/slack-postgres-state`](../../examples/slack-postgres-state/README.md).

## NATS JetStream

```sh
go get github.com/coder/chat/state/nats
```

```go
import (
	natsgo "github.com/nats-io/nats.go"

	chatnats "github.com/coder/chat/state/nats"
)

conn, err := natsgo.Connect(os.Getenv("NATS_URL"))
if err != nil {
	return err
}

natsState, err := chatnats.New(ctx, chatnats.Options{
	Conn:   conn,
	Prefix: "mybot", // see "One namespace per bot application" below
	// DedupeTTL and ThreadLockTTL default to the runtime defaults (24h and
	// 2m) and must match your RuntimeOptions.
})
```

NATS state stores subscriptions, dedupe marks, and locks in three JetStream
Key-Value buckets with bucket-level TTLs (see
[ADR 0014](../adr/0014-nats-state-adapter.md)). Because JetStream TTLs are
per-bucket, the dedupe and lock TTLs are fixed at construction time. The
runnable example is
[`examples/slack-nats-state`](../../examples/slack-nats-state/README.md).

## One Namespace Per Bot Application

The Redis `Prefix`, Postgres `Namespace`, and NATS `Prefix` options default to
`chat`. Set them.

- **Replicas of one bot share a namespace.** That is what lets them dedupe
  and lock across each other.
- **Independent bots need different namespaces.** Thread IDs identify the
  platform tenant and channel but not your application. If two bots share a
  backend and a namespace, bot A subscribing a thread can route that
  thread's follow-ups into bot B's `OnSubscribedMessage`, and one bot's locks
  can suppress the other's events.
