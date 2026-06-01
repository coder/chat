# 04 - slack-nats-state example

Status: ready-for-agent

## Summary

Add `examples/slack-nats-state`, mirroring `examples/slack-redis-state`.

## Tasks

- `main.go`: connect via `nats.Connect(NATS_URL)`, build `chatnats.New(ctx, Options{Conn: nc, Prefix: "slack-example"})`, register the same mention/subscribed handlers.
- `go.mod`: module `.../examples/slack-nats-state`, require `coder/chat`, `coder/chat/state/nats`, `nats.go`.
- `compose.yaml`: `nats:2.11-alpine` with `-js -sd /data -m 8222`, port `127.0.0.1:42220:4222`, healthz healthcheck, persistent volume.
- `pitchfork.toml`: `nats` daemon mirroring the postgres example.
- `README.md`: adapt the Redis README for `NATS_URL` and the JetStream requirement.

## Acceptance

- `cd examples/slack-nats-state && go build ./...` succeeds.

## Comments

Implemented on branch `nats-state-adapter-impl`.
