# ADR 0015: Runtime Coordination — Admission Bound and Fenced Cross-Instance Coalescing

## Status

Proposed. This is the design for the deferred-dispatch admission bound (issue #44) and cross-instance coalescing (issue #50), decided together as one runtime-coordination track. It reshapes the force/steerability surface ADR 0012 proposed (`ForceReleaseLock` by key) and gives the staged burst/preemption branch (PR #53) its verdict.

## Context

ADR 0002 made `DispatchDeferred` the opt-in **Ack-Then-Work** mode: the adapter acknowledges after the synchronous prelude and the handler runs on the **Detached Work Context**, bounded by `DetachTimeout`. ADR 0012 expanded the **Concurrency Strategy** set; the reduced implementation on main ships `drop`, `queue`, `debounce`, and `concurrent` plus **Lock Scope** and universal lease-loss cancellation (a deferred holder whose **Lock Lease** is lost mid-run is cancelled within one refresh interval, `ThreadLockTTL/2`). Two costs were deliberately deferred:

- **No admission bound (#44).** Every accepted routed event under `DispatchDeferred` retains a detached tail — goroutine, event payload, handler closure — until completion, supersession, or `DetachTimeout`. A unique-event flood grows retention linearly. Burst batch members would retain payloads without even holding a goroutine.
- **Per-instance coalescing (#50).** Queue supersession and debounce coalescing live in process memory (the pending-waiter registry). Instances sharing a production **Runtime State** each dispatch their own most-recent event; the **Thread Lock** serializes them, but nothing supersedes across instances, while the upstream semantic is global newest-wins.

The burst strategy and the `OnLockConflict`/`ForceReleaseLock` preemption path were staged on PR #53 rather than merged, because nine review rounds on PR #38 kept finding P1s in exactly that machinery. Those findings were not random; they reduce to recurring failure classes, and this design's job is to preclude the classes structurally rather than patch instances of them:

1. **Unbounded admission** — goroutines/payloads retained before any concurrency gate ([r3871060076](https://github.com/coder/chat/pull/38#discussion_r3871060076), [r3871214506](https://github.com/coder/chat/pull/38#discussion_r3871214506)).
2. **Process-local coordination with distributed semantics implied** — per-instance registries (waiters, cancels) that multi-instance deployments silently defeat ([r3871403308](https://github.com/coder/chat/pull/38#discussion_r3871403308), [r3871919968](https://github.com/coder/chat/pull/38#discussion_r3871919968)).
3. **Key-only force release** — deleting a lock by key alone races holder turnover and kills innocent successor leases ([r3871938987](https://github.com/coder/chat/pull/38#discussion_r3871938987)).
4. **Non-atomic ownership handoff** — choreography between lock acquisition, local registration maps, and supersession rechecks, each gap a time-of-check race ([r3872104899](https://github.com/coder/chat/pull/38#discussion_r3872104899), [r3872390326](https://github.com/coder/chat/pull/38#discussion_r3872390326)).
5. **Premature handover** — a preemptor proceeding while its victim still runs ([r3871040671](https://github.com/coder/chat/pull/38#discussion_r3871040671)).
6. **Lease-lifecycle divergence** — parallel refresh/cleanup loops re-deriving hardening the shared path already has ([r3872272204](https://github.com/coder/chat/pull/38#discussion_r3872272204), [r3872390336](https://github.com/coder/chat/pull/38#discussion_r3872390336)).
7. **Temporal and budget skew** — shared batch deadlines starving late members; prelude stalls displacing newer waiters ([r3871403320](https://github.com/coder/chat/pull/38#discussion_r3871403320), [r3872622515](https://github.com/coder/chat/pull/38#discussion_r3872622515)).

One domain constraint is honored throughout: **Runtime State** is coordination state, not **Thread Application State** and not a message store (CONTEXT.md; the ADR 0009 **History Reader** storage rule). No design below places event payloads in State.

## Decision

Four pieces, decided together. Two are runtime-local (admission bound, burst shaping); two are optional **Runtime State** capabilities discovered by type assertion, per the ADR 0009 `HistoryReader` precedent — the required `State` interface does not change.

### 1. Admission Bound (issue #44)

Add `MaxDetached int` to **Runtime Options**: a per-instance cap on admitted-but-incomplete deferred dispatches. Everything a deferred delivery retains counts against it — running tails, queue/debounce parked waiters, concurrent slot-waiters, and (once burst ships) batch members — from admission until the dispatch reaches a terminal outcome. Deliveries that resolve in the prelude (duplicate, dropped conflict, ignored, unrouted, error) release their slot at acknowledgement; only routed deferred work holds it to the tail's end.

Semantics at the cap: **reject-with-signal, before ack and before dedupe marking.** Dispatch fails fast with a typed sentinel (`ErrAdmissionRejected`) before the delivery is marked in **Event Identity** dedupe; the webhook layer maps it to the platform's retry-inducing response (HTTP 429/503 equivalents; the mapping is adapter-owned). Because the delivery was never acknowledged and never marked, the platform's own retry redelivers it and is not deduped away — the same honesty contract prelude errors already follow (a failed prelude leaves the event un-marked so a retry is not deduped away).

Placement and validation:

- The gate sits at the head of the prelude, before any **Runtime State** write, so a rejected delivery leaves no record.
- `MaxDetached` must be positive under `DispatchDeferred` (constructor validation, the `DetachTimeout` precedent); `DefaultRuntimeOptions` gains a default (1024). `DispatchSync` ignores it: a synchronous delivery's goroutine and payload belong to the HTTP request before dispatch begins, so a runtime-side cap cannot shed them — and `net/http` imposes no request-concurrency limit by itself. Bounding synchronous serving is therefore an explicit operator contract at the HTTP layer (a request/connection limiter in front of the **Webhook Handler**), stated in the option's GoDoc rather than assumed — see Non-goals.
- Observability: a rejected delivery emits a new observation (`admission_rejected`) and closes its dispatch span with a new terminal outcome (`admission-rejected`). Rejections are never silent.

Interaction with each strategy (under `DispatchDeferred`):

| Strategy | What retains memory | What the bound caps |
|---|---|---|
| drop | running tails (one per scope) | total tails across scopes |
| queue | running tails + at most one parked waiter per scope | scope-cardinality floods |
| debounce | parked waiters (≤ 1 per scope) + running tails | scope-cardinality floods |
| concurrent | slot-waiters + at most `MaxConcurrent` running | the waiting line behind `MaxConcurrent` |
| burst (staged) | batch members (payload retained, no goroutine) + the running member | total retained members on the instance |

### 2. Burst shaping (issue #44's scope extension)

Burst requires `DispatchDeferred` (constructor validation, the debounce precedent): a synchronous webhook cannot park batch members past the platform acknowledgement deadline, and the burst invariants below — members counted by `MaxDetached`, fresh per-member budgets — are defined only for deferred dispatch. When burst ships (see the PR #53 outcome), batch growth is bounded twice:

- **Globally** by `MaxDetached`: each member counts from admission to its member-run's terminal outcome.
- **Per scope** by a new `MaxBurstBatch int` (**Runtime Options**, required positive under burst): when a scope's open window reaches the cap, the window **seals and rolls** — the sealed batch proceeds to dispatch and the incoming event opens the next window. Nothing is dropped at the batch layer; overflow only ever moves batch boundaries. (#44 demanded an explicit overflow policy; seal-and-roll is it.)

Two rules carried from the failure classes: each batch member runs with its own fresh `DetachTimeout` budget — a batch never runs under a shared deadline inherited from its first member (class 7) — and the batch's lock lifecycle reuses the same refresh/cancel/outcome machinery as every other deferred holder (class 6): one lease-lifecycle implementation, no parallel loop. Byte-based accounting is refused — see Non-goals.

### 3. Cross-instance coalescing (issue #50): Waiter Fences on an optional `Coalescer` capability

Queue supersession and debounce coalescing extend across instances through a State-issued monotonic **Waiter Fence** — a fencing token that totally orders coalescing participants. New optional capability:

```go
// Coalescer is an optional State capability providing fenced cross-instance
// waiter supersession.
type Coalescer interface {
	// NextFence allocates the next fence from one non-resetting monotonic
	// sequence shared by all scopes. Allocator state is O(1) and permanent;
	// fences are never reissued.
	NextFence(ctx context.Context) (uint64, error)
	// RegisterWaiter records fence as the scope's newest waiter iff it is newer
	// than the currently registered fence; registered=false means the caller is
	// already superseded.
	RegisterWaiter(ctx context.Context, scope string, fence uint64, ttl time.Duration) (bool, error)
	// NewestWaiter reads the scope's currently registered fence, if any.
	NewestWaiter(ctx context.Context, scope string) (uint64, bool, error)
}
```

Fences come from **one global, non-resetting sequence**, not per-scope counters: a per-scope counter would need a TTL to avoid unbounded per-scope State growth, and an expired-then-reset counter would let an old delivery's high fence outrank a fresh low one, reversing newest-wins. A global sequence keeps allocator state O(1) and permanent while per-scope *register* entries expire freely — an expired register only ever degrades to per-instance semantics, never reorders.

Protocol — for `queue` and `debounce` under `DispatchDeferred` only. Burst is delivery-preserving and drop is first-wins, so neither coalesces by fence. Synchronous queue waits are excluded by design: a sync park is bounded only by the caller's request context, which need not carry a deadline, so no finite park horizon exists from which to derive a sound registration TTL (see Non-goals). Under deferred dispatch every park is bounded by `DetachTimeout`.

- **Allocate early; register within a bounded window.** A routed queue/debounce delivery allocates its fence before the reorderable prelude work (the dedupe mark and lock acquisition — the State round-trips where deliveries actually stall). This is the cross-instance analog of the local admission sequence: a delivery delayed in its prelude can never displace a waiter fenced after it (class 7). A fence is registrable only within a bounded registration window from its allocation — one `ThreadLockTTL`, enforced by the runtime on a local monotonic clock — and the runtime likewise requires the dispatch-time check to begin within one `ThreadLockTTL` of registration (both bounds are runtime-enforced, not assumed). Past either bound, a marked delivery proceeds per-instance (unfenced or degraded, observed) and an unmarked one fails the prelude, so the platform retry redelivers with a fresh fence. Without this bound a delivery stalled between allocation and registration could outlive the register entries of newer, already-completed deliveries and register its stale fence into an empty register — dispatching an older event after a newer one ran.
- **Every routed delivery registers.** Whether it parks or acquires the **Thread Lock** immediately, a routed queue/debounce delivery registers its fence; only duplicates, unrouted events, and prelude failures never register (so they can never displace a live waiter). Registering the immediate acquirer is what closes the barging hole: a newer delivery that wins the lock race against a parked older waiter publishes its fence, so the older waiter observes it at dispatch time and skips — an older event can never dispatch *after* a newer one ran. `RegisterWaiter` returning false means a newer waiter is already registered: the delivery is superseded and skips immediately (releasing any lease it holds), exactly like local supersession.
- **Check on dispatch — the turn-claim point.** Every registered delivery (including a preemptor — §4) re-reads `NewestWaiter` once, after acquiring the **Thread Lock** and immediately before running the handler. If a newer fence is registered, it skips: release the lock, emit the observable skip. Once a handler starts it is never retroactively superseded — a follow-up arriving after the turn is claimed queues behind it, matching local queue semantics.

The guarantee split, stated honestly:

- **Per-instance (unchanged, always):** most-recent-wins by admission order; superseded waiters exit promptly; skips are observable.
- **Cross-instance (capability present, deferred dispatch, uniform fleet):** among registered deliveries that have not yet claimed their turn, at most the newest dispatches. "Newest" is defined by fence-allocation order at the shared State, which approximates arrival order — not platform send order — under delivery skew. The guarantee assumes every instance sharing the State participates: during a rolling upgrade, deliveries handled by non-participating instances (older runtimes, or runtimes whose State lacks the capability) dispatch unfenced and are invisible to fenced ordering — cross-instance newest-wins is suspended for exactly those deliveries and resumes once the fleet is uniform; fenced deliveries still order among themselves. A capability/version gate that could enforce fleet uniformity from within the runtime is refused (it would need fleet membership in State); uniformity is an operator rollout contract, documented, like the preemption mixed-fleet rule in §4.
- **Capability absent (or sync dispatch):** exactly today's documented per-instance behavior — correct per instance, weaker globally, logged once at startup. Never a constructor error: per-instance coalescing is valid semantics, not a broken configuration.

Failure modes accepted and documented, not hidden:

- **Register-then-never-run loses the coalesced turn.** If the instance holding the newest fence crashes, shuts down, or abandons its delivery (`DetachTimeout`) after older waiters skipped, no instance dispatches that group's turn — where per-instance coalescing would have dispatched a stale event. This is the coordination-only price: takeover would require the event payload in State (a message store — refused). Register entries carry a runtime-derived TTL: the registration window plus the tail-start bound plus `DetachTimeout` — that is, 2×`ThreadLockTTL` + `DetachTimeout` — with a safety margin; `DetachTimeout` alone is insufficient because the detach clock starts only when the tail does, and both leading intervals are the runtime-enforced bounds above, so implementations and operators can compute the TTL from configuration. This arithmetic makes the ordering sound: while any older fence remains registrable, every newer registration that could supersede it is still visible, and a parked waiter always finds a newer turn-claimer's registration alive at its dispatch-time check. Expiry beyond that horizon degrades to per-instance semantics rather than blocking dispatch. The loss window is the same class ADR 0002 already accepts for deferred dispatch (a crash after ack loses the work).
- **Coalescer failures degrade; they never lose an accepted event.** The dedupe mark is the commit point of the prelude. Any `Coalescer` failure during the prelude — allocation or registration, whether a backend error or the request context ending — degrades that delivery to per-instance semantics (it proceeds unfenced or unregistered, observed); it never fails the prelude and never abandons the delivery. A marked-but-unacknowledged delivery is therefore never silently dropped by a coordination call, preserving ADR 0002's retry contract (a platform retry of an un-marked failure redelivers; a marked delivery always launches). The post-ack dispatch-time check splits differently: a backend error degrades to per-instance semantics for that dispatch, while cancellation or deadline expiry of the delivery's own context is the existing abandonment path (exit without running) — a delivery whose execution budget ended can never "fall back" into running the handler past its budget. Events are never lost to a register outage — they serialize under the **Thread Lock** as today.

Backend fit — all four in-repo States implement it, and the conformance suite grows a capability section: **Memory State** (atomic counter + per-scope register map with lazy expiry), **Redis State** (one persistent `INCR` key + per-scope compare-greater script with TTL), **Postgres State** (a sequence + per-scope register row with expiry), **NATS State** (the revision of a single allocation key for fences; a register bucket whose bucket-level TTL follows ADR 0014's uniform-TTL mechanics — the adapter validates and does not honor the per-call `ttl`, so its operator-configured register-bucket TTL must cover the fleet maximum of the derived register TTL (2×`ThreadLockTTL` + `DetachTimeout` + margin); mixed-configuration fleets size it to the maximum). Third-party States keep compiling and keep today's semantics.

### 4. Preemption on a fenced Lock Takeover (`LockForcer` reshaped; issue #50's scope extension)

ADR 0012 proposed `ForceReleaseLock` by key. The #38 history shows why that shape cannot be made safe (classes 3–5): key-only identity cannot distinguish victim from successor, release-then-acquire leaves a gap a third party can enter, and the compensating local choreography (`inflightCancels`, `victimDone`) was the single largest P1 source. The reshaped surface:

```go
// LockHolder identifies a Thread Lock holder for compare-and-take. Identity is
// an opaque per-lease identifier, stable across ExtendLock refreshes; it need
// not be — and should not be — a release-capable token.
type LockHolder struct {
	Key      string
	Identity string
}

// LockForcer is an optional State capability enabling preemption.
type LockForcer interface {
	// ObserveLock reports the current holder of key, if held.
	ObserveLock(ctx context.Context, key string) (LockHolder, bool, error)
	// TakeLock atomically replaces the lock iff its current holder is still
	// observed, returning a fresh Lock Lease for the caller. taken=false means
	// the holder changed or the lock is free — never an error.
	TakeLock(ctx context.Context, observed LockHolder, ttl time.Duration) (LockLease, bool, error)
}
```

- **Takeover, not release-then-acquire.** `TakeLock` is a single compare-and-swap on the lock key: there is no window in which a third party can acquire between "release" and "acquire" — the class-3 successor race is precluded by construction, not by runtime choreography. A holder's ordinary `ExtendLock` churn (e.g. NATS revision bumps) must not defeat a takeover whose observed `Identity` still matches; a genuine holder change must. On `taken=false` the preemptor falls back to the configured strategy path, observed — no retry loop. `ObserveLock`/`TakeLock` *failures* follow §3's commit-point rule: after the dedupe mark, a backend error or the request context ending falls back observably to the configured strategy path — it never abandons the delivery and never fails the prelude post-mark, so a platform retry can never be deduped away from an event that was never launched.
- **No local preemption registry.** The staged `inflightCancels` map, `preemptLocalIfPending`, and the `victimDone` handshake are rejected wholesale (classes 4 and 5). Preemption takes exactly one path — through the State — whether the victim is local or remote. The victim is cancelled via the universal lease-loss cancellation already on main: its next `ExtendLock` fails and its **Detached Work Context** is cancelled with `ErrPreempted`. Preemption additionally requires each lease-refresh call to be independently time-bounded, with a refresh that cannot complete within its bound counting as lease loss: today's loop calls `ExtendLock` under `context.WithoutCancel` with no RPC deadline, so a backend that stalls after a takeover would otherwise delay the victim's cancellation signal indefinitely. With that bound, the signal latency is at most one refresh interval plus the per-call bound.
- **What preemption means, honestly:** preemption invalidates the victim's lease; it does not stop the victim. The victim's context is cancelled within one refresh interval plus the per-call bound above (`ThreadLockTTL/2`; ~60s at defaults), but termination is cooperative — Go cannot forcibly stop a handler — so victim and preemptor overlap for the cancellation-signal latency plus however long the victim takes to honor its context, and a handler that ignores cancellation overlaps unboundedly. This is the same cooperative-cancellation contract every runtime stop already carries (`DetachTimeout`, shutdown drain, lease loss); preemption adds no stronger guarantee. `ThreadLockTTL` tunes the signal latency: shorter TTL, faster cancellation, more State load. A future local-victim nudge is permitted as a best-effort latency optimization only — it must never carry correctness (no registry the semantics depend on).
- **Hook and gating.** `RuntimeOptions.OnLockConflict LockConflictHook` (`func(context.Context, *Event) bool`) is consulted on a **Lock Conflict**; true → observe + take, false → the configured strategy path. Constructor validation requires `DispatchDeferred` (only deferred holders participate in lease-loss cancellation; a sync holder cannot be stopped — the class-2 mixed-fleet finding), requires the State to implement `LockForcer` (a preemption hook on a State that cannot preempt is a misconfiguration, failed fast, not degraded silently), and rejects `ConcurrencyConcurrent` (that strategy takes no **Thread Lock**, so no **Lock Conflict** can ever invoke the hook; accepting the combination would silently disable a configured preemption policy).
- **Stale preemptors cannot dispatch stale events.** A preemptor is a conflicted delivery: under queue/debounce it holds a fence like any routed delivery and performs the §3 dispatch-time check after `TakeLock`. If a newer waiter registered meanwhile, the preemptor skips and releases. Residual, documented: a preemptor superseded between its conflict decision and its takeover may still invalidate a victim whose successor would have queued — a bounded spurious preemption, observable, but never a stale dispatch and never an innocent successor's lease deleted.
- **Mixed fleets:** preemption assumes instances sharing a State configure it uniformly. A `DispatchSync` holder whose lease is taken keeps running until its handler returns (it has no refresh loop) — the same overlap risk sync dispatch already carries for TTL expiry today. Documented, not solved here.

### 5. Composition

`Coalescer` and `LockForcer` are independent capabilities, and each alone is honest: coalescing without preemption gives global newest-wins with strategy-path conflict handling; preemption without coalescing gives fenced takeover with per-instance-only supersession (a stale preemptor may then dispatch its event — today's per-instance semantics, serialized as ever by the **Thread Lock**). Implementing both gives the full contract. The conformance suite tests each capability separately plus the composition.

## Failure-mode disposition (the #38 anti-examples)

| # | Class | Disposition in this design |
|---|---|---|
| 1 | Unbounded admission | Precluded: pre-ack `MaxDetached` gate; per-scope `MaxBurstBatch` seal-and-roll |
| 2 | Process-local coordination, distributed semantics | Precluded: cross-instance semantics are only claimed where a State capability backs them; absence degrades to documented per-instance behavior; preemption is single-path via State |
| 3 | Key-only force release | Precluded: `TakeLock` compare-and-swap on observed holder identity; no release→acquire gap |
| 4 | Non-atomic handoff/registration races | Precluded: no local preemption registries to race with State updates; cross-instance ordering is enforced by State atomics (fence CAS, lock CAS) |
| 5 | Premature handover | Transformed and documented: no victim-drain handshake; the victim's context is cancelled within one refresh interval plus the bounded refresh call, termination is cooperative — overlap lasts the signal latency plus the victim's cancellation latency (unbounded only for handlers that ignore their context) |
| 6 | Lease-lifecycle divergence | Precluded by rule: all lock-holding paths (including burst) reuse the one refresh/cancel/outcome implementation |
| 7 | Temporal/budget skew | Precluded: early fence allocation pins cross-instance admission order; per-member execution budgets; no shared batch deadline |

Accepted residuals, all observable: coalesced-turn loss on register-then-die (§3); preemption overlap of one refresh interval plus the victim's cooperative cancellation latency (§4); bounded spurious preemption by a superseded preemptor (§4); fence order approximating State-arrival order, not platform send order (§3); per-instance degradation for deliveries whose prelude stalls past the registration window (§3).

## Outcome for PR #53 (staged burst + preemption)

**Verdict: close PR #53.** Neither half should merge in its current shape, and the halves should not travel together again.

- **Burst: rewrite on this design, salvaging the concept and its tests.** `ConcurrencyBurst` and per-member fresh budgets survive. Required changes: members count against `MaxDetached`; `MaxBurstBatch` with seal-and-roll; the batch lock lifecycle must reuse the shared refresh/cancel machinery (the staged branch's separate refresh loop reproduced class 6 twice). Ship as its own PR after the admission bound lands.
- **Preemption: rewrite from scratch.** Rejected outright: key-only `ForceReleaseLock(ctx, key)` and all four backend implementations of it; the `inflightCancels` registry; `preemptLocalIfPending`/`victimDone`. Surviving with changed shape: `OnLockConflict` (same signature, now deferred-only and `LockForcer`-required at construction), `ErrPreempted`/`OutcomePreempted` (already on main), and the `LockForcer` capability name — carrying the `ObserveLock`/`TakeLock` contract above. Ship last, after the `Coalescer` fence exists, so the stale-preemptor check has something to read.

## Non-goals

This design explicitly refuses to promise:

- **Exactly-once (or at-least-once) cross-instance dispatch.** Coalescing is at-most-newest per group; deferred dispatch's crash-loss contract (ADR 0002) is inherited, and supersede-then-crash loses the group's turn.
- **Cross-instance waiter takeover.** It would require event payloads in **Runtime State** — a message store, refused on the coordination-only rule.
- **Cross-instance burst batch merging.** Same refusal; per-instance batches serialized by the **Thread Lock** are the contract.
- **Byte-accounted admission.** The runtime cannot meaningfully measure retained `Raw` platform payloads plus handler closures; the bound is a count, and operators size it against their platform's payload ceiling.
- **Fleet-wide admission control.** `MaxDetached` protects one instance's memory; fleet capacity management belongs to the operator's front door.
- **Zero-overlap — or even hard-bounded — preemption.** A strict victim-drain handoff is exactly the class-4/5 churn machine, and Go cannot forcibly stop a handler regardless: the contract is prompt cancellation and cooperative termination, nothing stronger.
- **Admission control for synchronous dispatch.** Under `DispatchSync` the goroutine and payload exist at the HTTP layer before the runtime sees the delivery; a runtime cap cannot shed that load, and `net/http` imposes no request-concurrency limit of its own. Bounding synchronous serving is an explicit serving-layer operator contract (request limiting in front of the **Webhook Handler**), not a runtime promise.
- **Cross-instance coalescing for synchronous dispatch.** A synchronous queue park is bounded only by the caller's request context, which need not carry a deadline, so no sound registration TTL exists for it. Sync queue keeps per-instance semantics; cross-instance coalescing pairs with `DispatchDeferred`, whose parks `DetachTimeout` bounds.
- **State watch/notify primitives.** Coalescing is check-at-dispatch; no backend is required to push notifications.

## Consequences

- Two new **Runtime Options** (`MaxDetached`, `MaxBurstBatch`), one new sentinel (`ErrAdmissionRejected`), and one observation/outcome pair for admission. `MaxDetached` positive is required under `DispatchDeferred`: existing deferred configurations built without `DefaultRuntimeOptions` must set it. Deliberate — no unbounded production default survives this ADR.
- Adapters gain one honesty duty: map `ErrAdmissionRejected` to the platform's retry-inducing response. Sustained rejection can trip platform webhook-health policies; the alternative (acknowledging discarded work) is worse, and sizing guidance lives with the option's GoDoc.
- The `State` interface itself does not change; both extensions are optional capabilities (ADR 0009 precedent). All four in-repo States implement both; the conformance suite grows capability sections (fence monotonicity, register-only-newer, TTL expiry, observe/take atomicity under contention, extend-churn non-defeat).
- Queue/debounce dispatch under `DispatchDeferred` gains up to three State round-trips (allocate, register, check) when `Coalescer` is present — the cost of global newest-wins. Absent the capability, or under sync dispatch, nothing new is paid.
- ADR 0012's proposed force surface is superseded by §4 (flagged per the domain docs' ADR-conflict rule). ADR 0012's status note and the CONTEXT.md glossary (**Admission Bound**, **Waiter Fence**, **Lock Takeover**) update when this ADR is accepted and implementation lands.
- Implementation sequencing, each step its own gated PR: (1) admission bound — closes #44; (2) `Coalescer` + backends + conformance; (3) queue/debounce fence integration — closes #50; (4) burst revival; (5) fenced preemption, including the independently time-bounded lease-refresh calls.

## Alternatives Considered

### Drop-with-observation at the admission cap

Rejected. Acknowledging then discarding makes the platform 2xx a lie with no retry to correct it — the exact "new silent drop policy" #44 warned against. Observable-but-acked loss is still loss.

### Block the webhook until capacity frees

Rejected. Parking the webhook goroutine busts platform ack deadlines (Slack's 3s), converting overload into platform-side timeouts and retry storms — strictly worse backpressure than an honest retry signal.

### Admission control in front of the webhook only (operator contract)

Rejected as the whole answer. A load balancer cannot see detached-tail occupancy — the exhausted resource is invisible outside the runtime. Front-door rate limiting remains complementary.

### Required `State` methods instead of optional capabilities

Rejected. Growing the required interface breaks every third-party State for features many deployments never enable; the `HistoryReader` precedent (ADR 0009) established capability discovery with honest degradation.

### Payload-bearing waiter registry in State (global coalescing with takeover)

Rejected on the coordination-only rule: **Runtime State** would become a message store, with the size limits, retention, and privacy surface the contract deliberately excludes. The fence carries ordering, never content.

### Key-only `ForceReleaseLock` (ADR 0012's proposed shape, PR #53's implementation)

Rejected. Key-only identity cannot distinguish victim from successor (class 3), and release-then-acquire reopens the race even with identity. Compare-and-take on a single key moves the atomicity into a State primitive every backend can actually provide.

### Local preemption fast-path registry (`inflightCancels` + `victimDone`)

Rejected as correctness machinery. Two coordination systems — process maps and the State — must agree at every interleaving, and the #38 history is the catalog of ways they didn't. One path, through the State; local nudges may only ever be best-effort latency sugar.

### Cross-instance debounce timer coordination (a global quiet period)

Rejected. A globally-reset quiet period needs watch/notify primitives or polling loops in every parked waiter. Check-at-dispatch delivers the observable contract — only the newest dispatches — without new State primitives; the quiet period stays a per-instance approximation.

### Single-phase fence allocation (allocate at registration time)

Rejected. Registration order diverges from arrival order when preludes stall on State round-trips: a stale delivery could fence out a newer waiter (class 7, cross-instance). Allocating before the reorderable work pins the order; registering only routed deliveries keeps duplicates and failures from superseding anyone.

### Per-scope fence counters with TTL

Rejected. A per-scope counter must either live forever — unique-scope traffic then grows Memory/Redis/Postgres coordination state permanently — or expire, and an expired-then-reset counter lets an old delivery's high fence outrank a fresh low one, reversing newest-wins exactly when a scope goes quiet. One global non-resetting sequence costs O(1) permanent state, preserves strict ordering across expiry of the per-scope register entries, and its per-scope subsequence is still strictly increasing.

Cross-references: ADR 0002 (deferred dispatch, crash-loss contract), ADR 0009 (optional-capability precedent), ADR 0012 (strategy set; force surface reshaped here), ADR 0014 (NATS State mechanics reused for fences and takeover); issues #44 and #50; the PR #38 review history and PR #53 (the staged branch this ADR disposes).
