# ADR-013: In-Place Legacy → Per-Shard Cluster Migration

## Status
Proposed (targets 0.3.x). Supersedes **ADR-007 §5** ("Upgrading a pre-0.3.0 cluster in place is
not supported; it is a documented clean-slate migration") and the terminal-`Failed` posture of
its `LegacyClusterTopology` guard. Implementation mechanics: `docs/LEGACY_CLUSTER_MIGRATION_DESIGN.md`.

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

The natural mechanism is exactly what Redis Cluster is built for: stand the new empty per-shard
STSs up **alongside** the old single STS, `MEET` them into the same cluster, drain slots old→new
online, attach the new replicas, then `FORGET` + delete the old STS. Data never leaves the cluster;
it migrates node-to-node.

## Decision

### 1. Migration is the **default** resolution (opt-out), not opt-in
On detecting a legacy `{name}-cluster` STS, the 0.3 operator **enters migration mode** instead of
failing. "Unmanaged and terminal" is not an acceptable resting state, and — unlike a clean-slate
rebuild — an in-cluster reshard is **data-safe by construction** (see Decision 4), so it needs no
data-safety opt-in. `LegacyClusterTopology` is repurposed from a terminal `Failed` into the
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

### 4. Data-safe by construction — including the one delete
The slot move is key-preserving (native ASM, or the incremental key-draining dance) — the same
"a non-lossy reshard always exists, so no drop-keys opt-in is built" reasoning as ADR-006/LR-018.
The operator's one departure from ADR-007's "never auto-delete the old STS": it **does** delete the
legacy `{name}-cluster` STS + PDB at decommission — but only *after* every legacy node owns **zero
slots**. In a pure in-memory cluster, cluster data ⟺ slot ownership (the LR-023 invariant), so a
zero-slot node holds no data and deleting it loses nothing. ADR-007 refused to delete because it
could not first drain; once we can drain, deleting an emptied STS is the safe, natural completion.

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
loop assumes the per-shard topology: its slot-alignment check (Step 3) would balk at the transient
split of slots across legacy + new nodes, and its ghost-FORGET (Step 2) would try to evict the
legacy nodes as unknown. Both are bypassed until migration reaches `Complete`; the driver owns all
`MEET`/move/`REPLICATE`/`FORGET` in the interim. Ghost-FORGET, when it does run, is taught to
exempt legacy-named nodes for the migration window.

### 7. A pure decision seam
The migration decision is a pure function `planClusterMigration(ground-truth, spec, legacy-facts)
→ MigrationPlan{Phase, Actions, Reason}`, a sibling of `PlanReshard` / `planClusterWipeRecovery`.
Phase is **re-derived from live cluster state every pass** (which slots sit on legacy vs new nodes,
whether the new STSs exist, whether the legacy STS still exists) — never read back from status
(ADR-006). `status.cluster.migration { phase, shardsMoved, startedAt }` is a monitoring surface only;
nothing load-bearing is persisted. This keeps the whole decision unit-TDD-able (red-first) and the
e2e a thin integration shell.

### Migration phases
Re-derivable, idempotent, resumable from live state:

`Standup` (create the empty `{name}-shard-K` STSs) → `Meet` (`MEET` every new pod into the cluster
via a legacy seed) → `Draining` (one range-for-range move per reconcile, ASM or dance by capability)
→ `ReplicasAttached` (`ClusterReplicate` each `{name}-shard-K-M` to `{name}-shard-K-0`) → `Decommission`
(`FORGET` all legacy nodes, delete the `{name}-cluster` STS + PDB once they own zero slots) →
`Complete` (legacy STS gone; the `LegacyClusterTopology` condition clears and the normal repair loop
resumes; `lrctl verify` colocation passes).

## Consequences
- 0.2→0.3 becomes an **online, in-place, zero-copy-out** migration with no client-visible outage
  beyond ordinary per-slot `-ASK`/`-MOVED` redirection — strictly better than the clean-slate
  rebuild ADR-007 documented.
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
