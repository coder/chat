# How To Choose A State Backend

Runtime state is required: the runtime stores subscribed-thread membership,
event dedupe marks, and thread lock leases in a `chat.State`. It is
coordination state, not product state — keep your application's own data in
your own database keyed by `ThreadID`.

Four implementations ship today:

| Backend | Module | Use for |
| --- | --- | --- |
| Memory | `github.com/coder/chat/state/memory` (in the core module) | Tests and local demos. Lost on restart. |
| Redis | `github.com/coder/chat/state/redis` | Production, horizontally scaled deployments. |
| Postgres | `github.com/coder/chat/state/postgres` | Production, when Postgres is already your coordination store. |
| NATS JetStream | `github.com/coder/chat/state/nats` | Production, when you already run NATS with JetStream. |

Redis, Postgres, and NATS live in separate Go modules so applications that only
need core, Slack, or memory state do not pull their dependencies.

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

redisState, err := chatredis.New(ctx, chatredis.Options{
	Client: redis.NewClient(&redis.Options{Addr: os.Getenv("REDIS_ADDR")}),
	// Prefix defaults to "chat".
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
	Pool: pool,
	// Namespace defaults to "chat".
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
	Conn: conn,
	// Prefix defaults to "chat"; DedupeTTL and ThreadLockTTL default to the
	// runtime defaults (24h and 2m) and must match your RuntimeOptions.
})
```

NATS state stores subscriptions, dedupe marks, and locks in three JetStream
Key-Value buckets with bucket-level TTLs (see
[ADR 0014](../adr/0014-nats-state-adapter.md)). Because JetStream TTLs are
per-bucket, the dedupe and lock TTLs are fixed at construction time. The
runnable example is
[`examples/slack-nats-state`](../../examples/slack-nats-state/README.md).

## How To Decide

- Writing tests or following the tutorial: use memory.
- Already running Redis: use Redis. Same for Postgres and NATS — the backends
  are contract-equivalent, so pick the one you already operate.
- Running more than one bot replica: any of the durable backends works; all
  three implement the same token-owned lock lease and dedupe contract, which is
  what makes horizontal scaling safe.

All backends are exercised by the same conformance suite; Redis and Postgres
integration tests run against real backends via Testcontainers, and NATS tests
run against an embedded JetStream server.
