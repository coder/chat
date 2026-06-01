# 05 - ADR 0014 and CONTEXT glossary

Status: ready-for-agent

## Summary

Record the decision as ADR 0014 and add the **NATS State** glossary term to `CONTEXT.md`.

## Tasks

- `docs/adr/0014-nats-state-adapter.md` with sections Status / Context / Decision / Consequences / Alternatives Considered. Capture: bucket-TTL strategy, three buckets + `History: 1`, `Create`/revision-CAS/`Delete(LastRevision)` mapping, base64 keys, caller-owned conn + Close, reap-lag consequence, and rejected alternatives (per-key TTL, core NATS, steal-if-expired).
- `CONTEXT.md`: add a `**NATS State**:` entry after `**Postgres State**:`, following the `_Avoid_:` pattern.

## Acceptance

- ADR matches the structure of existing ADRs; glossary entry uses project vocabulary.

## Comments

Implemented on branch `nats-state-adapter-impl`.
