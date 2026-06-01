# 03 - Conformance test with embedded server

Status: ready-for-agent

## Summary

Run the shared state conformance suite against the NATS adapter using an in-process embedded JetStream server.

## Tasks

- `state/nats/nats_test.go`: `newEmbeddedConn(t)` starts `nats-server` with `JetStream: true`, `StoreDir: t.TempDir()`, `Port: -1`; cleaned up via `t.Cleanup`. One server per subtest (the factory runs per subtest).
- Call `statetest.RunStateConformance` with `DedupeTTL = ThreadLockTTL = ShortTTL = 1s` and `ExpiryWait = 3s` (covers bucket TTL + age-reaper lag). Leave `AdvanceTime` nil (real clock).
- Do not close the conn in the test; `State.Shutdown` closes it.

## Acceptance

- `go test ./... -race -count=3` passes in `state/nats` with no flakes.

## Comments

Implemented on branch `nats-state-adapter-impl`. Verified: `-race -count=3` green (~10s; `locks`/`dedupe` each wait ~3s for expiry).
