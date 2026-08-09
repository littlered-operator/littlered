# ADR-013: In-Place Legacy → Per-Shard Cluster Migration

## Status
Proposed (targets 0.3.x). Supersedes **ADR-007 §5** ("Upgrading a pre-0.3.0 cluster in place is
not supported; it is a documented clean-slate migration") and the terminal-`Failed` posture of
its `LegacyClusterTopology` guard. Implementation mechanics: `docs/LEGACY_CLUSTER_MIGRATION_DESIGN.md`.

> **Amended 2026-08-09 (restart-safety redesign, LR-025).** The migration *mechanism* changed from
> node-to-node slot **reshard** (native ASM, or the pre-8.4 MIGRATE "dance") to **replicate-then-failover**.
> A live s1 run (dance path, Redis 7.4.0) showed the reshard mechanism both **deadlocks** and **loses data**
> when a new per-shard master restarts mid-drain: the drain leaves that master owning slots on EmptyDir
> **with no replica yet** (replicas attached only afterward), so a restart trips the startup STEP-3 guard
> (LR-003 — no replica to `TAKEOVER` → `CrashLoopBackOff`) and the just-migrated keys, already deleted from
> the source, are lost. The hole is latent in the ASM path too (Run A passed only because ASM's window is
> ~8s). The fix is structural, not a patch (see Decision 4, the phases in Decision 7, and Alternatives):
> keep the new nodes **slot-less** until an atomic, already-redundant handoff. Design mechanics for the
> new mechanism: `docs/LEGACY_CLUSTER_MIGRATION_DESIGN.md` §10.

> ADR number: 013, not 010–012. Those are claimed on sibling branches (010 ghost-replica prune,
> 011 failover, 012 multi-site); 013 avoids an integration collision.

## Context

0.3.0 (ADR-007) split cluster mode from a **single** StatefulSet `{name}-cluster` (striped
pod-index→shard model, pods `{name}-cluster-N`) into **one StatefulSet per shard**
`{name}-shard-K` (pods `{name}-shard-K-M`). The old single-STS operating code was removed
outright — the 0.3 operator has no code that can *manage* a pre-0.3 cluster.

ADR-007 §5 handled this by **refusing**: on detecting a lingering `{name}-cluster` STS the
operator sets `Phase: Failed` / `Status: LegacyClusterTopology`, emits a warning event, and
requeues forever. It never deletes the old STS (data safety) — but it also never heals, never
reconciles, never progresses. The resting state of a 0.2 cluster under a 0.3 operator is
**unmanaged and terminal**. Since 0.2→0.3 is *already* a wall today, that "refuse" is not a
working path anyone is relying on; it is a dead end waiting for a human.

Three facts, established by reading the split commit (`85e1a93`, LR-020), make an in-place
online migration far cheaper than "re-add the old cluster code":

1. **The CRD is unchanged.** The split touched only doc-comments in `littlered_types.go` and the
   generated CRD YAML. `spec.cluster.shards` and `spec.cluster.replicasPerShard` are byte-identical
   pre-0.3 vs 0.3. A legacy CR parses cleanly on the 0.3 operator. **The migration problem lives
   entirely at the workload/pod layer, never at the API layer.**
2. **The data-plane is identity-based, not STS-name-based.** The gatherer, `PlanReshard`, the
   reshard executors (native ASM `ClusterMigrationImport` and the pre-8.4 `reshardViaDance`), and
   the primitives (`ClusterMeet`/`ClusterForget`/`ClusterReplicate`/`ClusterAddSlots`/`SETSLOT`)
   all key off NodeID / IP / owned-slots — never off pod ordinal or STS name. Only three things in
   cluster mode are STS-name-aware: expected-pod enumeration (`ClusterPodRefs`), the two guards
   (`LegacyClusterTopology` / `ShardScaleDownRefused`), and ghost-node detection.
3. **The move is 1:1, range-for-range.** Pre-0.3 `bootstrapCluster` assigned
   `GenerateSlotRanges(shards)` to its masters — the *exact same deterministic ranges* the 0.3
   shards expect. So for a **shape-preserving** migration (same `shards`, same `replicasPerShard`)
   shard K's slot range is identical old vs new; migration is "move range
   `GenerateSlotRanges(shards)[K]` from whatever node owns it (a legacy pod) to `{name}-shard-K-0`."
   That maps directly onto the existing `ReshardMove{Start,End,Source,Dest}` and its executor.

**Consequence: the removed striped `(i-shards)%shards` decode does not need to be resurrected.**
Legacy masters are identified by *which slots they own* (from `CLUSTER NODES`), not by pod index.
The only legacy-naming awareness required is a prefix check (`{name}-cluster` STS, `{name}-cluster-N`
pods) to exempt legacy nodes from ghost-FORGET and to delete the old STS at the end — a handful of
lines, not the ~350-line reconcile rewrite the split removed.

The natural mechanism is exactly what Redis Cluster is built for. Because the move is 1:1 range-for-range
(fact 3), migration is a **node-placement change, not a slot change** — range K is already range K, and all
we do is replace *which node* owns it. Redis's canonical way to replace a node is replication + failover, so:
stand the new empty per-shard STSs up **alongside** the old single STS, `MEET` them into the same cluster,
make each new pod a **slot-less replica** of the legacy master that owns its range and let it full-sync,
then promote `{name}-shard-K-0` with a coordinated `CLUSTER FAILOVER` (an atomic ownership flip to a node
that is already caught up; the legacy master demotes to a live replica of it), and finally `FORGET` +
delete the old STS. Data never leaves the cluster; the slots never logically move — only mastership does.

> The **original** mechanism drained slots old→new node-to-node (native ASM / MIGRATE dance) and attached
> the new replicas *afterward*. That is the LR-025 restart-safety hole (Status amendment above, and
> Alternatives). Replicate-then-failover keeps every new node slot-less until the atomic handoff, so it is
> restart-safe by construction — the reasoning below (identity-based data-plane, no striped decode) is
> unchanged and, if anything, cleaner: with no slot move at all, even the transient split-ownership state
> the repair loop had to be suspended for no longer arises during the transfer.

## Decision

### 1. Migration is the **default** resolution (opt-out), not opt-in
On detecting a legacy `{name}-cluster` STS, the 0.3 operator **enters migration mode** instead of
failing. "Unmanaged and terminal" is not an acceptable resting state, and — unlike a clean-slate
rebuild — an in-cluster replicate-then-failover migration is **data-safe by construction** (see
Decision 4), so it needs no data-safety opt-in. `LegacyClusterTopology` is repurposed from a terminal `Failed` into the
**in-progress** condition, carrying the migration phase; it becomes terminal-`Failed` again only if
migration cannot safely proceed (e.g. a non-shape-preserving legacy topology, Decision 5).

### 2. Health-gated start
Migration begins only when the legacy cluster is currently safe to rewrite: **all** legacy pods
Ready, `cluster_state:ok`, all 16384 slots assigned, and a reachable master quorum. If the legacy
cluster is mid-incident, the operator reports and waits rather than piling a topology rewrite onto
an unstable cluster (the Rule A / wipe-cooldown spirit, pillar 3.5). This is the analog of every
other "never act into instability" gate in the operator.

### 3. `hold` escape hatch
The annotation `redis.chuck-chuck-chuck.net/migrate-legacy-sts: "hold"` pins a **non-mutating**
holding state for a maintenance window or change-control sign-off. Absence of the annotation ⇒
proceed. The annotation is transient and self-documents as temporary — chosen over a spec field so
nothing is added to the CRD and removing the feature later is deleting a file plus re-tightening one
guard. No other annotation value has meaning; `hold` is the only recognized value.

### 4. Data-safe by construction — new nodes own slots only after an atomic, already-redundant handoff
Migration replaces each shard's node placement by **replication + failover**, never by moving slots
onto a bare new node (LR-025). For shard K the new pods first join as **slot-less replicas** of the
node that currently owns range K (the legacy master), full-sync its data, and only then is
`{name}-shard-K-0` promoted by a coordinated `CLUSTER FAILOVER` — an atomic ownership flip to a node
that is already caught up, and whose old master demotes to a live replica of it. Consequences:

- **A new node holds no slots for the entire data transfer**, so it never enters the one state that is
  unsafe under EmptyDir + no-persistence (pillar 3.1): *owning slots with no synced replica*. That is
  precisely the state that both deadlocks the startup STEP-3 guard (LR-003 — a restarted slot-owner with
  no replica to `TAKEOVER` parks → `CrashLoopBackOff`) and loses the just-migrated, already-source-deleted
  keys. It is the failure the reshard mechanism hit live on s1 (LR-025); replicate-then-failover removes
  the state itself, so no startup-script change is needed (contrast the LR-023 recycle).
- **Every slot has ≥2 live copies at every instant**: legacy master + new node(s) during sync; new master
  + demoted-legacy master (+ new replicas) after failover. This holds independent of `replicasPerShard`,
  **including `0`** — the demoted legacy master is the transient replica until decommission, so even a
  no-replica cluster is restart-safe *throughout* the migration and returns to its single-copy contract
  only at the final `FORGET`. (This is why LR-025 needs no rps=0 special case.)
- **The one operator delete** (legacy `{name}-cluster` STS + PDB) still departs from ADR-007's "never
  auto-delete", and is still gated on data-safety: a legacy node is `FORGET`-then-deleted only once the
  shard replacing it is fully `(1+replicasPerShard)`-replicated on new nodes — so removing the legacy
  copies never drops a shard below its target redundancy. In a pure in-memory cluster cluster data ⟺ slot
  ownership (the LR-023 invariant); by decommission every legacy node is a slot-less demoted replica, so
  deleting it loses nothing.

### 5. Shape-preserving only
The first cut migrates `shards` masters + `replicasPerShard` replicas into the identically-shaped
per-shard layout — only the workload/pod arrangement changes, which is what enables the trivial
1:1 range mapping (Context 3). A legacy topology that is **not** shape-preserving (its live
`shards` / `replicasPerShard` disagree with the CR, or its slot ranges are not the aligned
`GenerateSlotRanges(shards)`) is **refused** (terminal `LegacyClusterTopology`), not guessed at.
Changing shard/replica counts stays a separate, already-supported reshard **after** migration
completes.

### 6. The normal repair loop is suspended during migration
Only the migration driver mutates topology while migration is in flight. The steady-state repair
loop assumes the finished per-shard topology: during migration the cluster transiently carries the
legacy nodes *plus* the new nodes as extra cross-STS replicas of the legacy masters, which its
ghost-FORGET (Step 2) would try to evict as unknown and its shard-aware reattach / colocation checks
(Step 4, LR-020) would try to "correct." Both are bypassed until migration reaches `Complete`; the
driver owns all `MEET`/`REPLICATE`/`FAILOVER`/`FORGET` in the interim. Ghost-FORGET, when it does
run, is taught to exempt legacy-named nodes for the migration window.

### 7. A pure decision seam
The migration decision is a pure function `planClusterMigration(ground-truth, spec, legacy-facts)
→ MigrationPlan{Phase, Actions, Reason}`, a sibling of `PlanReshard` / `planClusterWipeRecovery`.
Phase is **re-derived from live cluster state every pass** (which slots sit on legacy vs new nodes,
whether the new STSs exist, whether the legacy STS still exists) — never read back from status
(ADR-006). `status.cluster.migration { phase, shardsMoved, startedAt }` is a monitoring surface only;
nothing load-bearing is persisted. This keeps the whole decision unit-TDD-able (red-first) and the
e2e a thin integration shell.

### Migration phases (replicate-then-failover, LR-025)
Re-derivable, idempotent, resumable from live state:

`Standup` (create the empty `{name}-shard-K` STSs) → `Meet` (`MEET` every new pod into the cluster via a
legacy seed) → `Replicate` (`CLUSTER REPLICATE` **every** new pod — master-to-be *and* its replicas — onto
the node that currently owns its shard's range, i.e. the legacy master for range K, and wait for each
replication link to come `up`; the new nodes full-sync as slot-less replicas) → `Failover` (one coordinated
`CLUSTER FAILOVER` per pass, issued on a synced `{name}-shard-K-0`, promoting it to own range K; the legacy
master demotes to a replica of it and the shard's other new replicas reparent to the new master) →
`Decommission` (`FORGET` all legacy nodes and delete the `{name}-cluster` STS + PDB, once every new master
owns its range **and** every new replica is a link-`up` replica of its new master — so no shard is left
below its `(1+replicasPerShard)` redundancy) → `Complete` (legacy STS gone; the `LegacyClusterTopology`
condition clears and the normal repair loop resumes; `lrctl verify` colocation passes).

The mechanism uses only Redis's most battle-tested primitives — `MEET`, `REPLICATE`, coordinated
`FAILOVER`, `FORGET` — and **no slot move**: no native ASM, no MIGRATE dance, no `-ASK`/`-MOVED` churn
during the transfer (new nodes are invisible replicas until their shard's single atomic failover). The
reshard executor (`moveSlotRange`, ASM/dance capability probe) stays in the tree for LR-018 consolidated-
shard recovery, but migration no longer calls it.

## Consequences
- 0.2→0.3 becomes an **online, in-place, zero-copy-out** migration with no client-visible outage
  beyond one atomic `-MOVED` ownership flip per shard at its failover (the new nodes sync as invisible
  replicas beforehand, so there is no per-slot redirection churn during the transfer) — strictly better
  than the clean-slate rebuild ADR-007 documented.
- **Restart-safe for all `replicasPerShard` (incl. 0)** and the **startup script is unchanged** (LR-025):
  a new node reaches the slot-owning state only after a clean failover, at which point it always has a
  synced replica (the demoted legacy master, then its own new replicas), so the existing STEP-3 `TAKEOVER`
  breaker (LR-003) suffices — migration adds no startup-script special case (the LR-023 precedent). The
  reshard mechanism it replaces was not restart-safe (Status amendment, Alternatives).
- Upgrading the 0.3 operator over a legacy cluster **is** the migration trigger. This is an
  irreversible, production-affecting event (a one-way door on operator version: once slots live on
  per-shard nodes, only 0.3 understands them). Made **observable** (status phases) and **pausable**
  (`hold`), and documented loudly in USAGE — not silent. The irreversibility is inherent to the
  topology change regardless of trigger; the gate controls *when*, never *whether*.
- The migration code is fenced (`cluster_migration.go`, `planClusterMigration`, the annotation
  handling, the one gather-from-legacy path) and removable as a unit once the migration window
  closes: delete the file and re-tighten the `LegacyClusterTopology` guard to ADR-007's refuse.
- No pre-0.3 striped operating code is resurrected (Context, above).
- ADR-007 §5 and the `LegacyClusterTopology` / clean-slate USAGE notes are amended accordingly.

## Alternatives considered
- **Node-to-node slot reshard (native ASM, or the pre-8.4 MIGRATE "dance") to move each range onto a
  bare new master.** This was the *original* mechanism — reused wholesale from the LR-018 reshard executor,
  which is what made ADR-013 cheap to propose — and is now **superseded** (restart-safety redesign, LR-025).
  It leaves each new master owning slots on EmptyDir **with no replica** for the whole drain window (replicas
  attached only in a later phase), so any restart of that master both deadlocks the startup STEP-3 guard (no
  replica to `TAKEOVER` → `CrashLoopBackOff`) and loses the just-migrated, already-source-deleted keys — found
  live on s1 (dance path; latent in ASM, whose ~8s atomic window merely made a restart unlikely). A minimal
  patch — "attach a synced replica to each new master *before* draining into it" — would have closed the
  window only for `replicasPerShard ≥ 1` and only down to the async-replication lag, and would still transit
  the dangerous slot-owning state. Replicate-then-failover instead removes the unsafe state entirely: new
  nodes stay slot-less until an atomic, already-redundant handoff, restart-safe for all `replicasPerShard`,
  built from Redis's most-trusted primitives, and *simpler* in the migration path (no ASM-vs-dance capability
  branch, no incremental key-batch draining, no `-ASK`/`-MOVED` churn). Its one cost — each new master
  full-syncs from a live legacy master (a fork/CoW event) — is the ordinary replica-resync load the cluster's
  sizing already tolerates in steady state (every EmptyDir replica restart triggers it), and it is paced one
  shard per reconcile.
- **Opt-in (annotation to start; refuse otherwise).** Rejected: leaves the default resting state
  unmanaged and terminal — the exact dead end this ADR removes. The annotation survives only as the
  `hold` opt-*out*.
- **External `lrctl` migration tool.** Rejected: `lrctl` is deliberately read-only (status/verify/
  inspect/debug-dump). A mutating migration verb would duplicate the operator's reshard/MEET/FORGET
  orchestration and reintroduce a *second topology authority* fighting the operator — the "two cooks"
  anti-pattern ADR-011 spent effort eliminating.
- **Re-add a full legacy adapter (operate the old cluster in the old way, then migrate).** Rejected:
  ~60–100 lines of striped decode + wiring through gather/status/repair, for a capability migration
  never needs — you only need to *drain* the legacy cluster, not *manage* it. Heaviest option, and
  it contradicts "clearly identified and easy to remove."
- **Unconditional / non-health-gated auto-migrate.** Rejected: would begin rewriting topology on an
  already-degraded legacy cluster and gives admins no window control. Decisions 2+3 keep the
  opt-out default while preserving both.
