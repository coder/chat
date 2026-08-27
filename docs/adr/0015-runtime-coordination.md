# ADR 0015: Deferred-Dispatch Admission Bound; Cross-Instance Coalescing Rejected For Now

## Status

Proposed. This decides the deferred-dispatch admission bound (issue #44) and explicitly rejects, for now, the cross-instance coalescing State extension (issue #50) after a full design attempt — see the rejection section for the evidence and the reopening bar. Issue #50 stays open as gated future work. This ADR also gives the staged burst/preemption branch (PR #53) its verdict.

## Context

ADR 0002 made `DispatchDeferred` the opt-in **Ack-Then-Work** mode: the adapter acknowledges after the synchronous prelude and the handler runs on the **Detached Work Context**, bounded by `DetachTimeout`. ADR 0012 expanded the **Concurrency Strategy** set; the reduced implementation on main ships `drop`, `queue`, `debounce`, and `concurrent` plus **Lock Scope** and universal lease-loss cancellation, with `burst` and force/steerability staged on PR #53 pending this design track.

Two costs were deliberately deferred from that reduction:

- **No admission bound (#44).** Every accepted routed event under `DispatchDeferred` retains a detached tail — goroutine, event payload, handler closure — until completion, supersession, or `DetachTimeout`. A unique-event flood grows retention linearly ([r3871060076](https://github.com/coder/chat/pull/38#discussion_r3871060076)). Burst batch members would retain payloads without even holding a goroutine ([r3871214506](https://github.com/coder/chat/pull/38#discussion_r3871214506)).
- **Per-instance coalescing (#50).** Queue supersession and debounce coalescing live in process memory: instances sharing a production **Runtime State** each dispatch their own most-recent event, serialized by the **Thread Lock** but not superseded across instances ([r3871403308](https://github.com/coder/chat/pull/38#discussion_r3871403308)).

Both were drafted here as one design. The admission bound converged under review; the cross-instance protocol did not — this ADR records the resulting split decision. One domain constraint holds throughout: **Runtime State** is coordination state, not **Thread Application State** and not a message store (CONTEXT.md; the ADR 0009 **History Reader** storage rule).

## Decision

### 1. Admission Bound (issue #44)

Add `MaxDetached int` to **Runtime Options**: a per-instance cap on admitted-but-incomplete deferred dispatches. Everything a deferred delivery retains counts against it — running tails, queue/debounce parked waiters, concurrent slot-waiters, and (once burst ships) batch members — from admission until the slot-release point defined next (not until a terminal outcome is *recorded*; recording precedes cleanup). Deliveries that resolve in the prelude (duplicate, dropped conflict, ignored, unrouted, error) release their slot **when the prelude returns** — the runtime's own boundary, before the adapter writes any response; an errored prelude is not acknowledged at all, and the runtime never observes the adapter's write, so prelude return is the only release point that cannot leak permits. Only routed deferred work holds its slot, and it holds it until the detached tail goroutine actually returns — after handler completion *and* lock cleanup — not merely until a terminal outcome is recorded: cleanup can stall (today's release path runs under an uncancellable context after the outcome is recorded), and a slot released before the goroutine, event, and closure are actually gone would let sustained traffic exceed the cap by the amount of work stuck in cleanup.

Semantics at the cap: **reject-with-signal, before ack and before dedupe marking.** Dispatch fails fast with a typed sentinel (`ErrAdmissionRejected`) before the delivery is marked in **Event Identity** dedupe; the webhook layer maps it to a shape-aware response (adapter-owned). Because the delivery was never acknowledged and never marked, no dedupe record blocks a retry — the same honesty contract prelude errors already follow (a failed prelude leaves the event un-marked so a retry is not deduped away). The mapping is shape-aware because platforms do not retry every webhook shape:

- **Platform-retried deliveries** (e.g. Slack Events API callbacks): the adapter returns the retry-inducing response (HTTP 429/503 equivalents) and the platform's own redelivery covers the event.
- **Direct user invocations that the platform does not redeliver** (e.g. Slack slash commands and interactivity — the in-repo Slack normalization records that these carry no retry headers): a bare 429 would turn a click into a silent permanent failure, so the adapter must answer with a *truthful busy response* to the user (a visible "busy, try again" acknowledgement, observed as rejected) — never a silent failure, never a fake success. The user's own retry is honored because no dedupe record exists.

This qualifies ADR 0003 explicitly (flagged per the domain docs' ADR-conflict rule): ADR 0003 defines a verified, normalized command as an **Accepted Event** owed acknowledgement, but the admission gate precedes acceptance — a delivery rejected at admission never becomes an **Accepted Event**, is never marked, and owes only the shape-aware overload response above. Overload rejection is a pre-acceptance outcome, not a broken acknowledgement of an accepted command.

For interaction shapes whose platform contract separates the acknowledgement from the user-visible response (ADR 0004: Slack `block_actions` requires an empty 2xx, with messages via `response_url`), the busy response cannot ride the acknowledgement body. The adapter acknowledges promptly per ADR 0004 and sends the busy message through the follow-up channel as a single, short-deadline, bounded adapter call under a small fixed busy-response budget — not a detached tail, so it does not consume `MaxDetached` capacity, and the budget caps its goroutines under saturation. If even that budget is exhausted, the busy post is skipped: the rejection is still observed (`admission_rejected`), degrading only the user-visible signal, never the acknowledgement — the one narrow, budget-bounded exception to the visible-busy-response rule, stated honestly.

Placement and validation:

- The gate sits at the head of the prelude, before any **Runtime State** interaction, so a rejected delivery leaves no record. A consequence, stated as an explicit qualification of ADR 0002's duplicate contract (per the domain docs' ADR-conflict rule): the required `State` contract has no read-only **Event Identity** check (`MarkEvent` writes on a miss), so at the cap the gate cannot distinguish a redelivery of an already-marked event — under saturation, duplicates receive the overload response like any other delivery and converge to the ordinary duplicate acknowledgement once capacity frees. Probing dedupe with `MarkEvent` is prohibited: marking a first delivery before rejecting it would let the platform's retry be acknowledged as a duplicate and never handled — silent event loss, strictly worse than extra retries. If saturation retry amplification proves material in practice, a read-only dedupe check can be added later as a small optional State capability; it is deliberately not part of this ADR.
- `MaxDetached` must be positive under `DispatchDeferred` (constructor validation, the `DetachTimeout` precedent); `DefaultRuntimeOptions` gains a default (1024). `DispatchSync` ignores it: a synchronous delivery's goroutine and payload belong to the HTTP request before dispatch begins, so a runtime-side cap cannot shed them — and `net/http` imposes no request-concurrency limit by itself. Bounding synchronous serving is therefore an explicit operator contract at the HTTP layer (a request/connection limiter in front of the **Webhook Handler**), stated in the option's GoDoc rather than assumed — see Non-goals.
- Observability: a rejected delivery emits a new observation (`admission_rejected`, carrying the adapter and tenant attributes) and closes its dispatch span with a new terminal outcome (`admission-rejected`). Rejections are never silent.
- Multi-tenant fairness: `MaxDetached` alone is a global ceiling, so on a multi-tenant runtime (ADR 0006) one tenant's sustained valid traffic could starve co-located tenants. An optional `MaxDetachedPerTenant int` (**Runtime Options**, 0 = disabled, negative rejected at **Runtime Construction**) bounds any single installation's share of the admitted work using the same rejection path and observability. Accounting keys on the composite `(adapter, tenant)` — ADR 0006's installation identity — never the bare tenant string, so same-named tenants on different adapters do not share a bucket; an empty tenant is counted as that adapter's single untenanted bucket (it must not bypass the limit), meaning per-installation isolation is only meaningful for adapters that populate `Event.Tenant`. It is a sublimit, not a reservation — fair-share scheduling is out of scope. Deployments serving a single tenant need only the global bound.

Interaction with each strategy (under `DispatchDeferred`):

| Strategy | What retains memory | What the bound caps |
|---|---|---|
| drop | running tails (one per scope) | total tails across scopes |
| queue | running tails + at most one parked waiter per scope | scope-cardinality floods |
| debounce | parked waiters (≤ 1 per scope) + running tails | scope-cardinality floods |
| concurrent | slot-waiters + at most `MaxConcurrent` running | the waiting line behind `MaxConcurrent` |
| burst (staged) | batch members (payload retained, no goroutine) + the running member | total retained members on the instance |

### 2. Burst shaping (issue #44's scope extension)

Burst requires `DispatchDeferred` (constructor validation, the debounce precedent): a synchronous webhook cannot park batch members past the platform acknowledgement deadline, and the burst invariants below are defined only for deferred dispatch. Lock sequencing follows the shipped debounce precedent exactly: burst takes no **Thread Lock** in the prelude — members ack promptly and park; the batch tail acquires the lock once, before running its members. This extends to burst the same qualification of ADR 0002's pre-ack-acquisition rule that the merged debounce implementation already established (main's prelude acquires the lock only under drop/queue; debounce coordinates in the tail), flagged per the domain docs' ADR-conflict rule: ADR 0002's rule governs the immediate-dispatch strategies, and the coalescing strategies acquire at dispatch time in the tail — still exactly one lock owner per scope, still lease-loss-cancelled. When burst ships (see the PR #53 outcome), batch growth is bounded twice:

- **Globally** by `MaxDetached`: each member counts from admission until its member-run ends (the final member until the batch tail goroutine returns — below).
- **Per scope** by a new `MaxBurstBatch int` (**Runtime Options**, required positive under burst): when a scope's open window reaches the cap, the window **seals and rolls** — the sealed batch proceeds to dispatch and the incoming event opens the next window. Nothing is dropped at the batch layer; overflow only ever moves batch boundaries. (#44 demanded an explicit overflow policy; seal-and-roll is it.) Sealed batches for a scope dispatch in seal order through a per-instance FIFO: a rolled batch's tail runs only after its predecessor's tail returns. Batches never enter the pending-waiter supersession path — supersession is newest-wins and would drop accepted members, violating the delivery-preserving guarantee — and FIFO ordering prevents a newer batch from racing an older one to the **Thread Lock**. The window interval is `DebounceInterval`, reused under burst exactly as ADR 0012's `debounceMs` prescribes ("waits `debounceMs` on an idle scope") and required positive under burst like under debounce; its GoDoc's "ignored otherwise" note updates accordingly.

A member's *execution* is budgeted per member, but its *retention* is batch-derived, and this explicitly qualifies ADR 0002's per-tail lifetime rule for burst (flagged per the ADR-conflict rule): a late member of a slow batch waits through its predecessors, so a burst member's admission-to-return lifetime is bounded by the batch bound — the window interval, plus a coordination budget, plus up to `MaxBurstBatch` sequential member budgets (plus any predecessor batches in the FIFO, each themselves so bounded) — not by one `DetachTimeout`. The coordination budget makes the bound real: a batch's pre-execution wait (FIFO turn plus **Thread Lock** acquisition) is bounded by one `DetachTimeout`, and a batch that cannot acquire within it is **abandoned observably** — every member closes with the existing abandonment outcome, slots released at tail return — matching the shipped single-waiter abandonment semantics rather than parking forever behind a stuck scope. Mid-batch lease loss has one disposition: the running member is cancelled (`ErrPreempted`, as on main) and the batch terminates — remaining members close observably with the skipped-on-lease-loss outcome, never run without the **Thread Lock**, and are never silently discarded; there is no reacquisition, preserving the single-acquisition lifecycle. The only unbounded residual is the §1 stalled-cleanup case, which is exactly why slots count until goroutine return rather than outcome recording. Operators size `MaxBurstBatch` and `DetachTimeout` together with the product in mind; ADR 0002's single-`DetachTimeout` lifetime keeps governing every non-burst tail.

Two rules carried from the #38 review history: each batch member runs with its own fresh `DetachTimeout` budget — a batch never runs under a shared deadline inherited from its first member ([r3871403320](https://github.com/coder/chat/pull/38#discussion_r3871403320)) — and the batch's lock lifecycle reuses the same refresh/cancel/outcome machinery as every other deferred holder ([r3872272204](https://github.com/coder/chat/pull/38#discussion_r3872272204)): one lease-lifecycle implementation, no parallel loop. Slot accounting follows §1's release rule at the batch level too: members release as their member-runs end, except the batch's shared tail goroutine is itself covered — the final member's slot is held until that goroutine actually returns after lock cleanup, so a stalled batch cleanup still counts against the cap. Byte-based accounting is refused — see Non-goals.

### 3. Per-instance supersession is the v0.x contract

Queue supersession and debounce coalescing are per runtime instance, by decision and not merely by implementation status: most-recent-wins follows the local dispatch admission sequence, superseded waiters exit promptly, skips are observable, and events delivered to different instances serialize under the **Thread Lock** without cross-instance supersession. This is exactly what main ships and documents on the strategy GoDoc today; this ADR promotes that documented honesty to the decided contract for the v0.x line.

## Cross-instance coalescing (issue #50): rejected for now

A full State-backed design was drafted and reviewed on this very PR: per-scope fencing tokens from a global monotonic sequence, waiter registration with derived TTLs, dispatch-time supersession checks, holder-bound lock takeover with idempotent reconciliation, and fleet capability gating — an optional-capability shape per the ADR 0009 precedent.

**It is rejected for now, on evidence.** Across roughly fifteen adversarial review rounds, every round found new P1-severity protocol holes in the prose specification, clustered entirely in the distributed protocol: waiter barging past parked deliveries, TTL-reset fence reordering, allocation-to-registration stall gaps, divergence between local and global ordering sources, rolling-upgrade capability gaps, acknowledgement-deadline collisions with coordination round-trips, reads returning stale absence after registration expiry, ambiguous takeover commits, and replacement leases expiring mid-reconciliation. Each hole was individually fixable — and each fix added mechanism for the next round to break. That is the signature of specifying a distributed coordination protocol in prose: natural-language review can find holes one at a time, but can never establish their absence. A protocol whose correctness argument cannot be checked mechanically is not a foundation this runtime will make cross-instance ordering promises on.

**The reopening bar.** Issue #50 stays open as future work, gated on all of:

1. A **formally modeled design** — the protocol (fences, registration lifetimes, takeover, degradation) specified and checked in a model checker (e.g. TLA+) against explicit invariants (no stale dispatch after a newer turn, no successor-lease invalidation, no accepted-event loss beyond the documented crash class) — or adoption of a **proven external primitive** that provides the ordering guarantee outright.
2. **Demonstrated user demand**: a real multi-instance deployment for which per-instance supersession plus **Thread Lock** serialization is insufficient in practice, not in principle.

**What stands regardless of the future protocol:**

- **Key-only `ForceReleaseLock(ctx, key)` is rejected finally** (ADR 0012's proposed force shape). The #38 history shows key-only identity cannot distinguish victim from successor ([r3871938987](https://github.com/coder/chat/pull/38#discussion_r3871938987)), and release-then-acquire leaves a gap a third party can enter. Any future force primitive must bind to the holder observed at conflict time, atomically.
- **Local preemption choreography is rejected finally**: the staged `inflightCancels` registry, `preemptLocalIfPending`, and the `victimDone` victim-drain handshake were the single largest P1 source on #38 (e.g. [r3872104899](https://github.com/coder/chat/pull/38#discussion_r3872104899), [r3871040671](https://github.com/coder/chat/pull/38#discussion_r3871040671)).
- **No event payloads in Runtime State**, whatever the protocol: coordination-only is a standing domain rule, so cross-instance waiter takeover (which requires dispatchable payloads in State) is out at any bar.
- The `burst` and force/steerability names remain reserved (ADR 0012); force/steerability is additionally gated on the same formal-design bar, since preemption is the same class of distributed protocol.

## Failure-mode disposition (the #38 anti-examples)

| Class | Disposition in this design |
|---|---|
| Unbounded admission | Precluded: pre-ack `MaxDetached` gate; per-scope `MaxBurstBatch` seal-and-roll |
| Process-local coordination with distributed semantics implied | Precluded by scope honesty: v0.x claims per-instance semantics only (§3); no distributed protocol ships without the formal bar |
| Key-only force release | Rejected finally; no force primitive ships in v0.x |
| Non-atomic ownership handoff; premature handover | Not shipped: no preemption, no local preemption registries in v0.x |
| Lease-lifecycle divergence | Precluded by rule: all lock-holding paths (including future burst) reuse the one refresh/cancel/outcome implementation |
| Temporal/budget skew | Precluded where in scope: per-member execution budgets, no shared batch deadline; local admission-sequence ordering already on main |

## Outcome for PR #53 (staged burst + preemption)

**Verdict: close PR #53.** Judged against per-instance semantics plus the admission bound:

- **Burst: revive with changes, as its own PR, after the admission bound lands.** Burst is per-instance batching and needs no cross-instance machinery. Required changes: members count against `MaxDetached`; `MaxBurstBatch` with seal-and-roll; per-member fresh `DetachTimeout` budgets; the batch lock lifecycle must reuse the shared refresh/cancel machinery (the staged branch's separate refresh loop reproduced the lease-lifecycle failure class twice); `DispatchDeferred` required at construction.
- **Preemption: rejected pending the formal-design bar.** Key-only `ForceReleaseLock` and the local preemption choreography are rejected finally (above); a safe replacement is a distributed protocol of exactly the class this ADR declines to specify in prose. `ErrPreempted`/`OutcomePreempted` (already on main, serving lease-loss cancellation) are unaffected.

## Non-goals

This design explicitly refuses to promise:

- **Cross-instance supersession of any kind in v0.x** — see the rejection section; per-instance is the contract.
- **Admission control for synchronous dispatch.** Under `DispatchSync` the goroutine and payload exist at the HTTP layer before the runtime sees the delivery; a runtime cap cannot shed that load, and `net/http` imposes no request-concurrency limit of its own. Bounding synchronous serving is an explicit serving-layer operator contract.
- **Byte-accounted admission.** The runtime cannot meaningfully measure retained `Raw` platform payloads plus handler closures; the bound is a count, and operators size it against their platform's payload ceiling.
- **Fleet-wide admission control.** `MaxDetached` protects one instance's memory; fleet capacity management belongs to the operator's front door.
- **Fair-share tenant scheduling.** `MaxDetachedPerTenant` is a per-tenant ceiling, not a reservation or weighted scheduler; guaranteed tenant throughput under saturation is not promised.
- **Exactly-once dispatch.** ADR 0002's crash-loss contract for deferred dispatch is inherited unchanged.

## Consequences

- Three new **Runtime Options** (`MaxDetached`, optional `MaxDetachedPerTenant`, `MaxBurstBatch`), one new sentinel (`ErrAdmissionRejected`), and one observation/outcome pair for admission. `MaxDetached` positive is required under `DispatchDeferred`: existing deferred configurations built without `DefaultRuntimeOptions` must set it. Deliberate — no unbounded production default survives this ADR.
- Adapters gain one honesty duty: map `ErrAdmissionRejected` shape-awarely — a retry-inducing response for platform-redelivered shapes, a truthful busy response for direct invocations the platform does not redeliver. Sustained rejection can trip platform webhook-health policies; the alternative (acknowledging discarded work) is worse, and sizing guidance lives with the option's GoDoc.
- The `State` interface does not change, and no optional State capability ships with this ADR.
- Multi-instance deployments keep the documented per-instance coalescing semantics indefinitely, until the reopening bar is met. Operators who need stronger ordering today must route same-scope traffic to one instance (sticky routing) — an operational workaround, stated honestly, not a runtime promise.
- Three ADR 0012 statements are superseded here (flagged per the domain docs' ADR-conflict rule): its proposed force surface (`ForceReleaseLock` by key) falls to the final rejection above; its staging gate that held `burst` on "fenced-coordination design work" is lifted — burst ships per-instance under §2, gated only on the admission bound; and its expectation that `queue`/`debounce`/`burst` "need wait/coalesce coordination" expanding every State implementation is withdrawn for v0.x — per-instance supersession (§3) needs no State expansion, and any future coordination contract goes through the reopening bar. ADR 0003 is qualified as described in §1.
- The CONTEXT.md glossary gains **Admission Bound** when this ADR is accepted and implementation lands.
- Implementation sequencing, each step its own gated PR: (1) admission bound — closes #44; (2) burst revival per the PR #53 outcome. Issue #50 remains open, labeled as gated on the reopening bar.

## Alternatives Considered

### Drop-with-observation at the admission cap

Rejected. Acknowledging then discarding makes the platform 2xx a lie with no retry to correct it — the exact "new silent drop policy" #44 warned against. Observable-but-acked loss is still loss.

### Block the webhook until capacity frees

Rejected. Parking the webhook goroutine busts platform ack deadlines (Slack's 3s), converting overload into platform-side timeouts and retry storms — strictly worse backpressure than an honest retry signal.

### Admission control in front of the webhook only (operator contract)

Rejected as the whole answer. A load balancer cannot see detached-tail occupancy — the exhausted resource is invisible outside the runtime. Front-door rate limiting remains complementary.

### Ship the cross-instance coalescing protocol now (the original scope of this ADR)

Rejected — this is the decision the rejection section records. The drafted protocol (fences, registration TTLs, fenced takeover, reconciliation) accumulated new P1-severity holes in every prose review round without converging; correctness of this protocol class must be established by model checking or a proven primitive, not by iterative prose review.

### Payload-bearing waiter registry in State (global coalescing with takeover)

Rejected on the coordination-only rule at any bar: **Runtime State** would become a message store, with the size limits, retention, and privacy surface the contract deliberately excludes.

Cross-references: ADR 0002 (deferred dispatch, crash-loss contract), ADR 0009 (optional-capability precedent and the storage rule), ADR 0012 (strategy set; force surface superseded here), issues #44 and #50; the PR #38 review history and PR #53 (the staged branch this ADR disposes); the review history of this ADR's own PR (#54) as the rejection evidence.
