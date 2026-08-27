# ADR 0015: Deferred-Dispatch Admission Bound; Cross-Instance Coalescing Rejected For Now

## Status

Proposed. This decides the deferred-dispatch admission bound (issue #44) and explicitly rejects, for now, the cross-instance coalescing State extension (issue #50) after a full design attempt — see the rejection section for the evidence and the reopening bar. Issue #50 stays open as gated future work. This ADR also gives the staged burst/preemption branch (PR #53) its verdict.

This is a decision-level document: it fixes decisions, invariants, and non-goals. Implementation mechanics — slot bookkeeping, timer and shutdown lifecycles, counter management — are deliberately not specified here; they are decided in the implementing PRs, where code and hardening tests can actually verify them against the invariants below.

## Context

ADR 0002 made `DispatchDeferred` the opt-in **Ack-Then-Work** mode: the adapter acknowledges after the synchronous prelude and the handler runs on the **Detached Work Context**, bounded by `DetachTimeout`. ADR 0012 expanded the **Concurrency Strategy** set; the reduced implementation on main ships `drop`, `queue`, `debounce`, and `concurrent` plus **Lock Scope** and universal lease-loss cancellation, with `burst` and force/steerability staged on PR #53 pending this design track.

Two costs were deliberately deferred from that reduction:

- **No admission bound (#44).** Every accepted routed event under `DispatchDeferred` retains a detached tail — goroutine, event payload, handler closure — until completion, supersession, or `DetachTimeout`. A unique-event flood grows retention linearly ([r3871060076](https://github.com/coder/chat/pull/38#discussion_r3871060076)). Burst batch members would retain payloads without even holding a goroutine ([r3871214506](https://github.com/coder/chat/pull/38#discussion_r3871214506)).
- **Per-instance coalescing (#50).** Queue supersession and debounce coalescing live in process memory: instances sharing a production **Runtime State** each dispatch their own most-recent event, serialized by the **Thread Lock** but not superseded across instances ([r3871403308](https://github.com/coder/chat/pull/38#discussion_r3871403308)).

Both were drafted here as one design. The admission bound converged under review; the cross-instance protocol did not — this ADR records the resulting split decision. One domain constraint holds throughout: **Runtime State** is coordination state, not **Thread Application State** and not a message store (CONTEXT.md; the ADR 0009 **History Reader** storage rule).

## Decision

### 1. Admission Bound (issue #44)

Add `MaxDetached int` to **Runtime Options**: a per-instance cap on admitted-but-incomplete deferred dispatches. It must be positive under `DispatchDeferred` (constructor validation, the `DetachTimeout` precedent); `DefaultRuntimeOptions` gains a default (1024). Optionally, `MaxDetachedPerTenant int` (0 = disabled) additionally caps any single installation's share, keyed on ADR 0006's installation identity `(adapter, tenant)` — a ceiling through the same rejection path, not a reservation.

Semantics at the cap: **reject-with-signal, before ack and before dedupe marking.** Dispatch fails fast with a typed sentinel (`ErrAdmissionRejected`); the webhook layer maps it to a shape-aware response (adapter-owned):

- **Platform-retried deliveries** (e.g. Slack Events API callbacks): a retry-inducing response (HTTP 429/503 equivalents); the platform's own redelivery covers the event, and because the delivery was never marked, that retry is not deduped away — the same honesty contract prelude errors already follow.
- **Direct user invocations the platform does not redeliver** (e.g. Slack slash commands and interactivity, which carry no retry headers): a truthful, user-visible busy signal, delivered per each shape's acknowledgement contract on a best-effort, bounded basis — never a silent failure, never a fake success. Where the platform's contract makes a visible signal impossible, the rejection is still observable.

This qualifies two accepted ADRs, flagged per the domain docs' ADR-conflict rule: **ADR 0003** — the admission gate precedes acceptance, so a rejected delivery never becomes an **Accepted Event** and owes only the overload response, and **ADR 0002** — the required `State` contract has no read-only dedupe check, so under saturation a redelivered duplicate receives the overload response like any other delivery and converges to the ordinary duplicate acknowledgement once capacity frees. Probing dedupe with `MarkEvent` is prohibited: marking a first delivery before rejecting it would let the platform's retry be deduped away unhandled — silent event loss, strictly worse than extra retries.

**Invariants** — binding on every implementing PR, verified there by code and hardening tests:

1. **Bounded retention.** Everything an admitted deferred delivery retains (goroutine, payload, closure — running tails, parked waiters, slot-waiters, future batch members) counts against the cap, and capacity is never released while that work remains retained.
2. **Honest rejection.** A rejected delivery is never acknowledged as handled, never marked in **Event Identity** dedupe, and always observable: a new observation (`admission_rejected`, carrying adapter and tenant) and a new terminal dispatch outcome (`admission-rejected`).
3. **No silent loss.** Every admitted delivery reaches an observable terminal outcome — including under **Runtime Shutdown**.

Per-strategy invariant under `DispatchDeferred` (one line each):

| Strategy | What the bound caps |
|---|---|
| drop | total running tails across scopes |
| queue | running tails plus parked waiters (≤ 1 per scope) under scope-cardinality floods |
| debounce | parked waiters (≤ 1 per scope) plus running tails under scope-cardinality floods |
| concurrent | the waiting line behind `MaxConcurrent`, plus the running set |
| burst (staged) | all retained batch members — decided at #53 revival under the invariants above |

`DispatchSync` is out of scope by decision: a synchronous delivery's goroutine and payload belong to the HTTP request before dispatch begins, so a runtime cap cannot shed them — bounding synchronous serving is a serving-layer operator contract (see Non-goals).

### 2. Burst admission semantics: deferred to the PR #53 revival

Burst's admission interaction — window and batch caps, member accounting, batch lifecycle, lock sequencing, and any ADR 0002/0004 qualification it requires — is decided when burst revives (see the PR #53 outcome), under this ADR's invariants plus one burst-specific invariant fixed now: **batch shaping is delivery-preserving** — any overflow policy may move batch boundaries but never drops an accepted member. Nothing else about burst is specified here.

### 3. Per-instance supersession is the v0.x contract

Queue supersession and debounce coalescing are per runtime instance, by decision and not merely by implementation status: most-recent-wins follows the local dispatch admission sequence, superseded waiters exit promptly, skips are observable, and events delivered to different instances serialize under the **Thread Lock** without cross-instance supersession. This is exactly what main ships and documents on the strategy GoDoc today; this ADR promotes that documented honesty to the decided contract for the v0.x line.

## Cross-instance coalescing (issue #50): rejected for now

A full State-backed design was drafted and reviewed on this very PR: per-scope fencing tokens from a global monotonic sequence, waiter registration with derived TTLs, dispatch-time supersession checks, holder-bound lock takeover with idempotent reconciliation, and fleet capability gating — an optional-capability shape per the ADR 0009 precedent.

**It is rejected for now, on evidence.** Across roughly fifteen adversarial review rounds, every round found new P1-severity protocol holes in the prose specification, clustered entirely in the distributed protocol: waiter barging past parked deliveries, TTL-reset fence reordering, allocation-to-registration stall gaps, divergence between local and global ordering sources, rolling-upgrade capability gaps, acknowledgement-deadline collisions with coordination round-trips, reads returning stale absence after registration expiry, ambiguous takeover commits, and replacement leases expiring mid-reconciliation. Each hole was individually fixable — and each fix added mechanism for the next round to break. That is the signature of specifying a distributed coordination protocol in prose: natural-language review can find holes one at a time, but can never establish their absence. A protocol whose correctness argument cannot be checked mechanically is not a foundation this runtime will make cross-instance ordering promises on.

**The reopening bar.** Issue #50 stays open as future work, gated on all of:

1. A **formally modeled design** — the protocol (fences, registration lifetimes, takeover, degradation) specified and checked in a model checker (e.g. TLA+) against explicit invariants (no stale dispatch after a newer turn, no successor-lease invalidation, no *undocumented* **Accepted Event** loss — observable supersession and abandonment are the strategies' documented behavior and explicitly allowed; only the documented crash class may lose an event silently) — or adoption of a **proven external primitive** that provides the ordering guarantee outright.
2. **Demonstrated user demand**: a real multi-instance deployment for which per-instance supersession plus **Thread Lock** serialization is insufficient in practice, not in principle.

**What stands regardless of the future protocol:**

- **Key-only `ForceReleaseLock(ctx, key)` is rejected finally** (ADR 0012's proposed force shape). The #38 history shows key-only identity cannot distinguish victim from successor ([r3871938987](https://github.com/coder/chat/pull/38#discussion_r3871938987)), and release-then-acquire leaves a gap a third party can enter. Any future force primitive must bind to the holder observed at conflict time, atomically.
- **Local preemption choreography is rejected finally**: the staged `inflightCancels` registry, `preemptLocalIfPending`, and the `victimDone` victim-drain handshake were the single largest P1 source on #38 (e.g. [r3872104899](https://github.com/coder/chat/pull/38#discussion_r3872104899), [r3871040671](https://github.com/coder/chat/pull/38#discussion_r3871040671)).
- **No event payloads in Runtime State**, whatever the protocol: coordination-only is a standing domain rule, so cross-instance waiter takeover (which requires dispatchable payloads in State) is out at any bar.
- The `burst` and force/steerability names remain reserved (ADR 0012); force/steerability is additionally gated on the same formal-design bar, since preemption is the same class of distributed protocol.

## Failure-mode disposition (the #38 anti-examples)

| Class | Disposition in this design |
|---|---|
| Unbounded admission | Precluded: pre-ack `MaxDetached` gate; the bounded-retention invariant; delivery-preserving batch shaping decided at #53 revival |
| Process-local coordination with distributed semantics implied | Precluded by scope honesty: v0.x claims per-instance semantics only (§3); no distributed protocol ships without the formal bar |
| Key-only force release | Rejected finally; no force primitive ships in v0.x |
| Non-atomic ownership handoff; premature handover | Not shipped: no preemption, no local preemption registries in v0.x |
| Lease-lifecycle divergence; temporal/budget skew | Deferred with the burst revival, bound by this ADR's invariants and decided against code and hardening tests |

## Outcome for PR #53 (staged burst + preemption)

**Verdict: close PR #53.** Judged against per-instance semantics plus the admission bound:

- **Burst: revive with changes, as its own PR, after the admission bound lands.** Burst is per-instance batching and needs no cross-instance machinery. The revival decides burst's admission semantics and lifecycle under this ADR's invariants (bounded retention, honest rejection, no silent loss, delivery-preserving shaping), with code and hardening tests as the verification medium — not ADR prose.
- **Preemption: rejected pending the formal-design bar.** Key-only `ForceReleaseLock` and the local preemption choreography are rejected finally (above); a safe replacement is a distributed protocol of exactly the class this ADR declines to specify in prose. `ErrPreempted`/`OutcomePreempted` (already on main, serving lease-loss cancellation) are unaffected.

## Non-goals

This design explicitly refuses to promise:

- **Cross-instance supersession of any kind in v0.x** — see the rejection section; per-instance is the contract.
- **Admission control for synchronous dispatch.** Under `DispatchSync` the goroutine and payload exist at the HTTP layer before the runtime sees the delivery; a runtime cap cannot shed that load, and `net/http` imposes no request-concurrency limit of its own. Bounding synchronous serving is an explicit serving-layer operator contract.
- **Byte-accounted admission.** The runtime cannot meaningfully measure retained `Raw` platform payloads plus handler closures; the bound is a count, and operators size it against their platform's payload ceiling.
- **Fleet-wide admission control.** `MaxDetached` protects one instance's memory; fleet capacity management belongs to the operator's front door.
- **Fair-share tenant scheduling.** `MaxDetachedPerTenant` is a per-installation ceiling, not a reservation or weighted scheduler; guaranteed tenant throughput under saturation is not promised.
- **Exactly-once dispatch.** ADR 0002's crash-loss contract for deferred dispatch is inherited unchanged.

## Consequences

- New **Runtime Options** (`MaxDetached`, optional `MaxDetachedPerTenant`), one new sentinel (`ErrAdmissionRejected`), and one observation/outcome pair for admission. `MaxDetached` positive is required under `DispatchDeferred`: existing deferred configurations built without `DefaultRuntimeOptions` must set it. Deliberate — no unbounded production default survives this ADR.
- Adapters gain one honesty duty: map `ErrAdmissionRejected` shape-awarely per §1. Sustained rejection can trip platform webhook-health policies; the alternative (acknowledging discarded work) is worse, and sizing guidance lives with the option's GoDoc.
- The `State` interface does not change, and no optional State capability ships with this ADR.
- Multi-instance deployments keep the documented per-instance coalescing semantics indefinitely, until the reopening bar is met. Operators who need stronger ordering today must route same-scope traffic to one instance (sticky routing) — an operational workaround, stated honestly, not a runtime promise.
- Three ADR 0012 statements are superseded here (flagged per the domain docs' ADR-conflict rule): its proposed force surface (`ForceReleaseLock` by key) falls to the final rejection above; its staging gate that held `burst` on "fenced-coordination design work" is lifted — burst is gated only on the admission bound and its own revival decisions; and its expectation that `queue`/`debounce`/`burst` "need wait/coalesce coordination" expanding every State implementation is withdrawn for v0.x — per-instance supersession (§3) needs no State expansion, and any future coordination contract goes through the reopening bar. ADR 0003 and ADR 0002 are qualified as described in §1.
- The CONTEXT.md glossary gains **Admission Bound** when this ADR is accepted and implementation lands.
- Implementation sequencing, each step its own gated PR: (1) admission bound — closes #44; (2) burst revival per the PR #53 outcome, deciding burst admission semantics under this ADR's invariants. Issue #50 remains open, labeled as gated on the reopening bar.

## Alternatives Considered

### Drop-with-observation at the admission cap

Rejected. Acknowledging then discarding makes the platform 2xx a lie with no retry to correct it — the exact "new silent drop policy" #44 warned against. Observable-but-acked loss is still loss.

### Block the webhook until capacity frees

Rejected. Parking the webhook goroutine busts platform ack deadlines (Slack's 3s), converting overload into platform-side timeouts and retry storms — strictly worse backpressure than an honest retry signal.

### Admission control in front of the webhook only (operator contract)

Rejected as the whole answer. A load balancer cannot see detached-tail occupancy — the exhausted resource is invisible outside the runtime. Front-door rate limiting remains complementary.

### Specify the full admission/burst mechanism in this ADR

Rejected, twice over, by this PR's own review history: first the cross-instance protocol and then the burst lifecycle accumulated new findings with every round of prose specification. Mechanism belongs where it can be verified — code and hardening tests in the implementing PRs — while this document fixes only the decisions and invariants those PRs must satisfy.

### Ship the cross-instance coalescing protocol now (the original scope of this ADR)

Rejected — this is the decision the rejection section records. The drafted protocol (fences, registration TTLs, fenced takeover, reconciliation) accumulated new P1-severity holes in every prose review round without converging; correctness of this protocol class must be established by model checking or a proven primitive, not by iterative prose review.

### Payload-bearing waiter registry in State (global coalescing with takeover)

Rejected on the coordination-only rule at any bar: **Runtime State** would become a message store, with the size limits, retention, and privacy surface the contract deliberately excludes.

Cross-references: ADR 0002 (deferred dispatch, crash-loss contract), ADR 0009 (optional-capability precedent and the storage rule), ADR 0012 (strategy set; force surface superseded here), issues #44 and #50; the PR #38 review history and PR #53 (the staged branch this ADR disposes); the review history of this ADR's own PR (#54) as the rejection evidence.
