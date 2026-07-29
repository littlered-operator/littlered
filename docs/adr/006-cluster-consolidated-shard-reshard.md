# ADR-006: Consolidated-Shard Reshard Recovery (Cluster Mode)

## Status
Accepted

## Context
A field report (`debug-0720`) from a cluster-mode instance (`shards: 3`, `replicasPerShard: 1`,
Redis 8.4.2, EmptyDir) sat stuck in `phase: Initializing` for ~19 hours. Redis itself was healthy
— `cluster_state: ok`, all 16384 slots served — but the topology had drifted into a shape the
operator could neither recognise as healthy nor repair:

- One master owned **two** of the three shard ranges (`0-5461` **and** `10923-16383`).
- Two pods were slotless **empty masters** with no replicas.
- So `CountMasters()` (slot-owning masters) was 2, `cluster_size` was 2, against `shards: 3`.

No repair branch could act:

1. **Step 3 (missing shards)** verifies that each expected *range* has *an* owner — never that
   owners are **distinct per shard**. A single master owning two ranges passes as "no missing
   shards."
2. **Step 4 (empty-master reattach)** only makes an empty master a *replica* of an
   *under-replicated* slot-master. Both slot-masters already had their one replica, so the empty
   masters had nowhere to go.
3. `IsHealthy` failed permanently (`CountMasters != shards`, `HasEmptyMasters`), routing every
   ~2s reconcile into a repair loop that was a no-op.

The operator had **no reshard / slot-migration capability at all**. The most probable origin is
the operator's own Step 3: an EmptyDir mass-restart orphaned a shard's range, and Step 3's strict
pod-index→shard assumption (`pod N owns shard N`) re-assigned that range via `ClusterAddSlots` to a
pod that *already* owned a different range — Step 3 created the consolidation it then could not see.

This is the cluster-mode member of the family ADR-005 / LR-015 opened in sentinel mode: a drifted
topology that Redis will not self-heal (it will not spontaneously move committed slots off a healthy
master) and that the operator must repair with external knowledge, **without destroying live data**.
The prior ADRs are all sentinel-mode; this is the first cluster-mode ADR-class recovery rule.

## Decision

### 1. Detect via a pure seam; prevent the cause in Step 3
`PlanReshard(gt, shards)` recognises the consolidated state — a reachable master owning more than
one expected shard range while a reachable **empty** master exists — and emits the key-preserving
move(s) that restore a distinct slot-owning master per shard. It keeps the lowest-index shard on the
over-consolidated master and relocates each surplus range onto a reachable empty master (destination
chosen distinctness-only: lowest PodName, tie-broken by NodeID; restoring the strict pod-index→shard
model is deliberately **not** attempted — least interference). It defers (no move) on a
fragmented/non-aligned range, when no empty master is available, and on a healthy topology.

Simultaneously the cause is closed: `SafeMissingShardTarget` restricts Step 3's missing-shard
assignment to a reachable **empty** master, so recovery can never pile a second range onto a master
that already owns one.

### 2. Preserve keys unconditionally — no drop-keys opt-in
The surplus range on the over-consolidated master holds **live keys currently being served**. The
recovery moves them; it never drops them. A destructive "reassign ownership without moving keys"
(`SETSLOT NODE` alone) path is **not** built, and no opt-in flag guards a lossy variant.

This is the deliberate contrast with ADR-005's `sentinel.allowUnsafeRebootstrapOnDeadlock`. That
flag exists because a leaderless multi-holder sentinel deadlock has *no* non-lossy resolution —
electing one holder necessarily discards the others. Consolidated-shard recovery **always** has a
non-lossy resolution (a key-preserving reshard), so there is no scenario in which dropping keys is
the only way out, and therefore no justification for an unsafe opt-in. (We judge a lossy opt-in
YAGNI and do not build it.)

### 3. Pick the mechanism by a free, gather-time capability probe
`CLUSTER INFO` — already fetched from every reachable node each reconcile — exposes the
`cluster_slot_migration_*` fields iff the engine supports Redis 8.4+ native atomic slot migration
(ASM). `ClusterGroundTruth.AtomicSlotMigration` is set to the **AND over all reachable nodes**, so a
mixed-version cluster mid rolling-upgrade falls back to the baseline. **Nothing is persisted** — not
a status field (a status field is a monitoring surface; an internal engine capability does not belong
there), not an annotation, no external store. The verdict is recomputed each reconcile from live data,
so it always reflects the running engine and needs no cache invalidation. Detect from the gather, not
from a version string: capability, not version, is the true signal (correct for Valkey and any engine
regardless of an `8.x` label), and it costs zero extra round-trips.

### 4. Execute incrementally (Step 3b `reshardConsolidated`)
One move per reconcile; the next gather observes progress and re-plans.

- **Redis 8.4+ (preferred):** `CLUSTER MIGRATION IMPORT` on the destination — one atomic,
  server-managed task. Re-entrant via `CLUSTER MIGRATION STATUS` (do not relaunch `IMPORT` while a
  task runs); completion observed when the destination owns the range in the gather.
- **Pre-8.4 (baseline, always available):** `reshardViaDance` — idempotently re-mark the range
  IMPORTING (dest) / MIGRATING (source), drain up to `ReshardMaxKeysPerReconcile` keys in
  `ReshardKeyBatchSize`-key `MIGRATE` batches, and flip `SETSLOT NODE` (broadcast to all reachable
  masters) **only once the whole range is drained**. Because ownership flips only at the end, the
  source keeps owning the range in gossip throughout; `PlanReshard` re-emits the same move and the
  executor **resumes from the cluster's own IMPORTING/MIGRATING markers — no persisted operator
  state, no cursor**. The per-reconcile bound is a key *count* (deterministic, unit-testable, keeps
  each pass short for the single reconcile worker), not wall-clock; the real anti-hang bound is the
  per-`MIGRATE` `ReshardMigrateTimeoutMillis`. A single key too large to move within that timeout
  stalls the reshard and is logged **loudly** (never a silent no-op).

`ReshardKeyBatchSize` (128), `ReshardMaxKeysPerReconcile` (2000) and `ReshardMigrateTimeoutMillis`
(5000) are advanced `spec.cluster.reshard*` fields; ignored on Redis 8.4+ (ASM path).

## Consequences
- **Cannot deadlock (LR-014 discipline):** every unhealthy verdict is backed by a repair action.
  Over-consolidation + a free empty master ⇒ `PlanReshard` always emits a concrete move; no free
  empty master ⇒ Step 4's precondition holds instead. The clause is maximal-but-actionable.
- **No oscillation:** deterministic destination selection plus the Step 3 hardening (never assign a
  range to a master already owning another) stop the operator from re-creating what it just undid.
- **Least interference (pillar 3.5):** the operator reshards *only* to break a state Redis cannot
  self-heal; it does not rebalance a merely-uneven-but-correct cluster.
- **Parsing correctness:** the reshard dance is the first path that puts slots into
  IMPORTING/MIGRATING state, which exposed a latent `ParseClusterNodes` bug — the per-slot
  `[slot->-id]`/`[slot-<-id]` notations were being counted as owned slots (see LR-018, commit
  `1a9733d`). Now excluded. Relevant to any mid-migration observation, not just this rule.
- **Storage-model corollary:** this rule, like all of LittleRed's cluster healing, assumes the
  pure-in-memory / EmptyDir model (pillar 3.1). Slots whose data was already lost to a restart are
  served empty; the reshard preserves whatever data is *present* on the over-consolidated master.

## Alternatives considered
- **Drop-keys reassignment (`SETSLOT NODE` without migrating keys), possibly behind an opt-in.**
  Rejected: a non-lossy path always exists here (§2), so a lossy one is never necessary; an opt-in
  for it is YAGNI.
- **Blocking, whole-range dance in one reconcile.** Rejected: the operator runs a single reconcile
  worker, so a multi-second/minute blocking pass starves every other instance (the LR-017 lesson).
  The incremental, key-count-bounded design keeps each pass short.
- **Persisting the ASM capability (status field / annotation).** Rejected: detectable for free from
  the `CLUSTER INFO` already gathered; persisting it adds a monitoring surface and cache-invalidation
  for no benefit (§3).
- **Restoring the strict pod-index→shard model on reshard.** Deferred: tidier for humans/`lrctl` but
  may force an extra migration; distinctness-only is cheaper and sufficient (LR-018 §11.3).

## Related
- ADR-005 (sentinel leaderless recovery) — the sentinel-mode member of the same "operator repairs a
  drifted topology without destroying live data" family; the deliberate opt-in contrast (§2).
- LR-018 (changelog) — the incident, fix, and e2e validation record.
- LR-014 — flagged the deferred *replica*-rebalance sibling (tracked as LR-019).
- Pillar 3.11 (CLAUDE.md).
