# 02 - Adapter implementation (nats.go)

Status: ready-for-agent

## Summary

Implement `chat.State` in `state/nats/nats.go` using JetStream KV with bucket-level TTL (Strategy A).

## Tasks

- `Options{Conn *nats.Conn, Prefix string, DedupeTTL, ThreadLockTTL time.Duration}`; defaults Prefix=`chat`, DedupeTTL=24h, ThreadLockTTL=2m.
- `New`: validate `Conn`, validate prefix charset, verify `CONNECTED` + `AccountInfo`, create three buckets (`_sub` TTL 0, `_event` TTL DedupeTTL, `_lock` TTL ThreadLockTTL) with `History: 1` via `CreateOrUpdateKeyValue`.
- Methods: subscribe/unsubscribe/check via `Put`/`Delete`/`Get`; `MarkEvent` via `Create`; lock acquire via `Create` + `internal/tokens`; extend via `Get`+revision `Update`; release via `Get`+`Delete(LastRevision)`.
- `encodeKey` (base64 RawURL) at the NATS boundary; `isWrongLastSequence` helper (APIError 10071) → treat as `false`.
- Explicit `ctx.Err()` guard at the top of every mutating method.
- `Shutdown` closes the conn once (`sync.Once`); `var _ chat.State = (*State)(nil)`.

## Acceptance

- `go vet ./...` and `go build ./...` clean in `state/nats`.
- All errors prefixed `nats state:`.

## Comments

Implemented on branch `nats-state-adapter-impl`.
