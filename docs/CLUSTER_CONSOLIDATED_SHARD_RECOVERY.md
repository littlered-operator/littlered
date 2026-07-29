# LR-018 — Cluster Mode: Consolidated-Shard Deadlock & Key-Preserving Reshard Recovery

- **Status:** Design (proposed). Not yet implemented.
- **Mode:** cluster
- **Date:** 2026-07-29
- **Relates to:** LR-012, LR-014 (cluster empty-master handling); ADR-005 / LR-015 / LR-016 (the sentinel-mode analog — "safely recover from a sole surviving data holder"). This is the *cluster-mode* member of the same family, and the replica-rebalance gap LR-014 explicitly deferred.

---

## 1. Field report

A user running **cluster mode** (`shards: 3`, `replicasPerShard: 1`, 6 pods, EmptyDir / pure in-memory, **Redis 8.4.2**) in a managed environment submitted a debug dump. The operator was **permanently stuck**:

- `status.phase: Initializing`, `Ready=False` ("Waiting for cluster to be healthy") — Ready had been False for **~19 hours** at capture (transition `2026-07-19T10:11`).
- All 6 pods `2/2 Running`; Redis itself healthy: `cluster_state:ok`, all 16384 slots served, `cluster_size:2`.
- Every ~2s the reconcile logs the identical no-op triplet and requeues:

  ```
  running repair  partitions:1 ghosts:0 orphanedSlots:false emptyMasters:true masters:2 allNodesView:6
  Detected masters with no slots in replication mode, attempting to assign as replicas
  Not yet Running, requeueing  redis:6/6 clusterHealthy:false
  ```

Redis is serving correctly. The **operator cannot recognise the topology as healthy and cannot heal it** — an infinite `Initializing` loop.

## 2. Observed topology (from gossip)

| Pod | nodeId (short) | Role | Slots | configEpoch |
|-----|----------------|------|-------|-------------|
| cluster-2 | efa6f3a9 | master | **0-5461 AND 10923-16383** (two shard ranges) | 22 |
| cluster-3 | b0fefa4a | master | 5462-10922 | 21 |
| cluster-4 | fd527ae1 | master | *(none)* | **0** |
| cluster-5 | 8b43246d | master | *(none)* | 19 |
| cluster-0 | 033f… | replica → cluster-2 | — | — |
| cluster-1 | da90… | replica → cluster-3 | — | — |

Two masters own all slots (one owns **two-thirds** of the keyspace); two pods are slotless **empty masters** with no replicas. Expected steady state for `shards:3, replicasPerShard:1`: three masters each owning ~⅓ of slots, each with one replica.

## 3. Why the operator is wedged

Tracing `repairCluster` (`internal/controller/cluster_reconcile.go`) against this exact ground truth — **every branch is a no-op**:

0. **Quorum recovery** — `votingMasters = CountMasters() = 2`; fires only when `≤ shards/2 = 1`. `2 ≤ 1` false → skip.
1. **Partitions / 2. ghosts** — `len(Partitions)=1` (one connected component → `HasPartitions()` false); `ghosts:0` → skip.
3. **Missing shards** (`cluster_reconcile.go:332`) — builds `shardOwners[]` by matching each *slot range* to an expected shard. cluster-2's two ranges fill `shardOwners[0]` **and** `shardOwners[2]`; cluster-3 fills `[1]`. All three indices are non-empty → **"no missing shards"**. The check asks *"does every range have an owner?"* — never *"is every shard owned by a **distinct** master?"*. **Double-ownership is invisible to it.**
4. **Empty-master reattach** (`cluster_reconcile.go:440`) — for each empty master it seeks a slot-owning master with `< expectedReplicas` (=1) replicas. cluster-2 and cluster-3 **both already have their one replica** → `targetMaster == nil` for both empties → **nothing happens**. This is the log line that repeats forever.

Meanwhile `IsHealthy(6, 3)` (`internal/redis/cluster_state.go:65`) fails on **two independent clauses** that can never flip back:

- `CountMasters() == 2 != shards(3)`, and
- `HasEmptyMasters() == true`.

To satisfy the first, an empty master must **acquire a shard's slots** — a **reshard**. `grep` over `internal/` confirms the operator has **no** `SETSLOT` / `MIGRATE` / `reshard` / `rebalance` capability whatsoever. There is no code path that can move a range off the double-owner onto an empty master. **This state is a permanent dead end for self-healing.**

## 4. How it got here (reconstruction)

A mass restart/reschedule swept the cluster `~2026-07-19 12:24–12:50` (pods are 18h old at capture). Evidence:

- `cluster-4` redis log: `No nodes.conf found. This is a fresh Pod start` → its EmptyDir was wiped; it rejoined empty at **configEpoch 0**.
- `cluster-2` redis log: replica re-sync churn with a changed peer IP (`…197` → `…198`), plus gossip shard-membership changes — the cluster reshuffled under it.

Because storage is EmptyDir (pillar 3.1), a restarted pod loses data and rejoins as an empty node, and roles **drift away from the bootstrap model**. `bootstrapCluster` assigns pod `cluster-i` = master of shard `i` (pods 0..S-1 masters, pods S..N-1 replicas); the live cluster has masters on pods **2 and 3**, replicas on **0 and 1**, empties on **4 and 5** — nothing like the index model.

The most probable trigger for the **double-ownership** is the operator's own Step 3. When shard-2's range (10923-16383) briefly went orphaned (its master **and** replica both EmptyDir-wiped in the sweep — data already gone), Step 3's `intendedMasters[shardIdx]` maps shard 2 → pod `cluster-2`, which was *already* master of shard 0, and `ClusterAddSlots` handed 10923-16383 to it. The pod-index→shard assumption is stale after role drift, so Step 3 **consolidated** two shards onto one master instead of restoring a distinct third master. (It restored *availability* by serving the lost slots empty — the pure-in-memory tradeoff; the shard-2 keys were already gone with the EmptyDir.)

So there are two defects working together: a **detection** defect that lets consolidation happen and hides it, and a **missing recovery capability** to undo it.

> **Note:** the dump also contains an unrelated pod that is not part of the LittleRed instance (different labels, no owner-ref to the CR) and does not interact with operator logic — a red herring, not this bug.

## 5. The gap, precisely

1. **Detection (bug).** Step 3 treats *"every range has an owner"* as *"no missing shards,"* so it is blind to one master owning multiple shard ranges while other masters sit empty. The stale pod-index→shard assumption in Step 3's *assign* path is also what likely **created** the consolidation.
2. **Recovery (missing capability).** Even once detected, the operator cannot move a range to an empty master to restore `CountMasters() == shards`. Combined with Step 4 (reattach only targets *under-replicated* slot-masters), an empty master has **nowhere to go** when the slot-masters are already fully replicated → infinite `Initializing`.

## 6. Design decision — **preserve keys by default; no drop-keys opt-in (YAGNI)**

The recovery must restore three distinct slot-owning masters by migrating the surplus range off the double-owner onto an empty master. Two ways to move a range:

- **(A) Key-preserving reshard** — the standard IMPORTING/MIGRATING/`MIGRATE`/SETSLOT dance (or Redis 8's native atomic slot migration, see §7). Keys in the moved slots are transferred; **no data loss**.
- **(B) Ownership reassignment** — `CLUSTER SETSLOT <slot> NODE <dest>` without moving keys; the source's keys in those slots are **dropped**.

**Decision (owner: Dominik, 2026-07-29):**

> **By default, always preserve keys (A).** A drop-keys path (B) would only ever be introduced behind an explicit opt-in on the cluster CR — and we judge that opt-in **YAGNI** and are **not** building it now. The recovery is unconditionally non-lossy.

Rationale — this mirrors the sentinel-mode stance we converged on in LR-015/LR-016: the operator's job is to **preserve surviving data**, never to destroy a live copy to satisfy a topology invariant. In cluster mode the surplus range on the double-owner holds **live keys currently being served**; a reshard that dropped them would be exactly the LR-016 "liveness probe wiped the survivor" mistake in a new place. The asymmetry that made the sentinel opt-in defensible (a *destructive rebootstrap over multiple holders* is sometimes the only way out of a leaderless deadlock) **does not exist here**: a key-preserving reshard is always available, so there is no scenario where dropping keys is the *only* recovery. Hence no opt-in.

Contrast with `sentinel.allowUnsafeRebootstrapOnDeadlock` (ADR-005): that flag exists because a leaderless multi-holder deadlock has *no* non-lossy resolution (electing one holder necessarily discards the others). Consolidated-shard recovery has one, so it needs no flag.

## 7. Proposed fix

Two parts, matching §5.

### 7.1 Detection — a distinct-owner-per-shard check + a pure planner

Add a **pure decision seam** (unit-TDD-able fast, per CLAUDE.md Test Discipline tier 2), e.g. `planReshard(gt, shards, replicasPerShard) → ReshardPlan`, that recognises the consolidated state and emits an explicit plan:

- Build `shardOwners[]` as today, **but also** flag any shard range whose owner already owns another range (`over-consolidated master`), and enumerate **empty masters** available as reshard destinations.
- When `CountMasters() < shards` **and** an over-consolidated master exists **and** an empty master is available, emit a plan: *move surplus range R from source master S to empty destination master D*, then (Step 4, unchanged) reattach the remaining empty master(s) as replicas.
- Deterministic destination choice (avoid churn/oscillation): prefer the empty master whose **pod index matches the shard's intended index** (restores the bootstrap model where possible); otherwise lowest pod index. Never pick a destination that still holds slots.

Also **harden Step 3's assign path**: never `ClusterAddSlots` a shard range to a master that already owns a *different* range (that is what created this state). If the intended master is unavailable/already-owning, defer (pillar 3.5) rather than consolidate.

`planReshard` returns "no action" whenever the topology is already one-master-per-shard, so it is inert on healthy clusters.

### 7.2 Recovery — key-preserving slot migration primitives

Two implementations, selected by **runtime capability** (see §7.3). None of these primitives exist today.

- **Baseline — classic pre-8.4 dance** (compatibility path; what `redis-cli --cluster reshard` does). **This is the design baseline** because many deployments run pre-8.4 Redis or engines/editions without native ASM (see §7.4). Per slot range moved S→D, per slot:
  1. `CLUSTER SETSLOT <slot> IMPORTING <S-id>` on **D**
  2. `CLUSTER SETSLOT <slot> MIGRATING <D-id>` on **S**
  3. loop `CLUSTER GETKEYSINSLOT <slot> <count>` on **S**, then `MIGRATE <D-host> <D-port> "" 0 <timeout> [AUTH …] KEYS …` until the slot is empty
  4. `CLUSTER SETSLOT <slot> NODE <D-id>` on **S** and **D** (broadcast to all masters for prompt convergence)

  New client methods: `ClusterSetSlotImporting/Migrating/Node`, `ClusterGetKeysInSlot`, `MigrateKeys`. Bounded by `ProbeTimeout`/context deadlines per the LR-012/LR-017 idiom so a dead peer can't stall the loop.

- **Preferred — Redis 8.4+ native atomic slot migration (ASM)** *(verified 2026-07-29 against redis.io docs; `since: 8.4.0`).* One command, run on the **destination** master:

  ```
  CLUSTER MIGRATION IMPORT <start-slot> <end-slot> [<start-slot> <end-slot> ...]   # returns a task-id
  CLUSTER MIGRATION STATUS ID <task-id> | ALL                                       # poll: state=completed/in_progress, last_error, retries
  CLUSTER MIGRATION CANCEL ID <task-id> | ALL
  ```

  ASM replicates the whole slot (snapshot + live delta, like slot-level full-sync) and does a single **atomic ownership handoff** — no key-by-key move, no client-redirect window, correct for multi-key ops. Server-tunable via `cluster-slot-migration-handoff-max-lag-bytes` (default 1MB) and `cluster-slot-migration-write-pause-timeout` (default 10s max write-pause during handoff). This is the `cluster_slot_migration_*` INFO machinery already visible in the dump. New client methods: `ClusterMigrationImport/Status/Cancel`.

  **Caveats worth pinning:** ASM is OSS-only (redis.io marks it *not supported* on Redis Software / Redis Cloud) — a **non-constraint for LittleRed, which only ever manages OSS Redis/Valkey**, but a reason we detect the capability rather than assume it (§7.3): Valkey and version drift are the live reasons to probe, not Enterprise. During import/trim, key-visibility commands (`KEYS`/`SCAN`/`DBSIZE`/`CLUSTER GETKEYSINSLOT`/…) may filter unowned-slot keys — irrelevant to the operator's topology decisions but noted so we don't build a health check on those counts mid-migration.

Both paths are **asynchronous from the operator's view** and must run **incrementally across reconciles** — kick off (dance step or `IMPORT`), then on subsequent fast requeues poll progress (`STATUS`, or slot-ownership in gossip) rather than blocking a reconcile, consistent with the non-blocking idempotent loop (CLAUDE.md rule 1, pillar 3.5). The ASM task-id model fits this natively. Re-entrant safety: if a prior pass left an in-flight task (ASM) or a slot in IMPORTING/MIGRATING (dance), **detect and resume/finish** rather than restart — persist the in-flight migration in `Status.Cluster` so a reconcile restart doesn't relaunch it.

### 7.3 Capability gating (which path to run)

**Detect from the gather, persist nothing.** The `cluster_slot_migration_*` fields are part of `CLUSTER INFO`, which the operator *already fetches from every reachable node on every reconcile* (`gatherTopology` → `GetClusterInfo`). So ASM support is detectable as a **free by-product of the existing gather** — no separate probe, no extra round-trip, and **no persisted state at all**:

- `ParseClusterInfo` flags the presence of a `cluster_slot_migration*` line; `ClusterGroundTruth.AtomicSlotMigration` is set to the **AND over all reachable nodes** (every reachable node must report it). A mixed-version cluster mid rolling-upgrade therefore uses the baseline dance until every node is upgraded — the conservative choice.
- **No status field / no annotation / no cache-invalidation.** `ClusterGroundTruth` is rebuilt each reconcile from live data, so the capability always reflects the *running* version and an image upgrade needs no special handling. This is a deliberate design choice (owner: Dominik, 2026-07-29): a CRD **status field is a monitoring surface** — users build alerts on it — and an internal engine-capability flag does not belong there; and LittleRed will **not** take on any external persistent store (NoSQL etc.) to hold operator bookkeeping. Detecting from the gather sidesteps the question entirely — there is nothing to store.
- **Probe, don't parse a version string.** Presence of the machinery is a truer capability signal than a version number, and correctly handles Valkey / non-Redis engines that may or may not ship ASM regardless of an `8.x` label.
- **Unknown/absent ⇒ baseline (safe default).** If a node is unreachable or does not report the fields, `AtomicSlotMigration` is false and recovery uses the **dance**. The gather-time detection carries *zero* extra network risk under LR-017 conditions: it reads one more field from a response we already fetched, never a new dial.
- **Executor fallback.** As belt-and-suspenders, the executor treats an "unknown command/subcommand" error from `CLUSTER MIGRATION` as "unsupported" and falls back to the dance for that move. The baseline alone is a complete, correct fix; ASM is an opportunistic safety/latency upgrade, never a hard requirement. (Resolves open question §11.1.)

### 7.4 Health verdict

Once §7.1 recognises the state as *actionable-unhealthy* and §7.2 can resolve it, `IsHealthy` needs no new clause: `CountMasters()==shards` becomes reachable again and the existing `HasEmptyMasters()` clause (LR-014) keeps the loop on the fast cadence until the reattach completes. Confirm the reshard-in-progress state stays on the **fast** requeue cadence (like LR-014) so it converges in seconds, not the 30s steady cadence.

## 8. Safety — why it cannot deadlock or lose data

- **No data loss:** the only slot-moving action preserves keys (§6). Reassignment-without-migration is not implemented.
- **Cannot deadlock (LR-014 discipline):** every predicate that reports unhealthy is backed by a repair action. `CountMasters() < shards` with an over-consolidated master and a free empty master ⇒ `planReshard` always emits a concrete migration; when no empty master is free, Step 4's reattach precondition holds instead. The new clause is *maximal-but-actionable*, exactly as LR-014 argued for its empty-master clause.
- **No oscillation:** deterministic destination selection (§7.1) and "never assign a range to a master already owning another range" (Step 3 hardening) prevent the operator from re-creating the consolidation it just undid.
- **Least interference (pillar 3.5):** the operator reshards **only** to break a state Redis cannot self-heal (Redis will not spontaneously move committed slots off a healthy master). It does not attempt to "rebalance" merely-uneven-but-correct clusters.

## 9. Test plan (red-first, per CLAUDE.md Test Discipline)

- **Tier 2 (pure, fast red-green) — the primary guard.** `planReshard` table test constructed from **this dump's exact 6-node topology** (cluster-2 owns 0-5461+10923-16383; cluster-3 owns 5462-10922; cluster-4/5 empty; cluster-0/1 replicas). Assert: RED against current code (no planner exists / no plan emitted → operator stuck), GREEN once the planner emits *move 10923-16383 from cluster-2 → an empty master, then reattach the remaining empty as replica*. Add cases: already-healthy (no action), over-consolidated with **no** free empty master (defer, don't crash), zero-replica mode. Also a `TestIsHealthy`/`CountMasters` case pinning the "2 slot-masters + 2 empty masters" input as unhealthy-and-actionable.
- **Tier 2 — Step 3 hardening:** assert the assign path refuses to `ClusterAddSlots` a range onto a master already owning a different range (the regression that created LR-018).
- **Tier 1 (bug repro):** the pure planner *is* the committed repro; its one-time RED is the observed field stall (the 6-node consolidated topology). Capture the dump-derived fixture in-repo.
- **Tier 3 (e2e):** a cluster-mode chaos scenario that drives a shard into orphaned-then-consolidated state (kill a shard's master+replica together so its slots get absorbed), then asserts the operator reshards a distinct third master back into existence **with the surviving keyspace intact** and reaches `Ready`. Slow loop accepted; keep the *decision* in `planReshard` so the e2e is a thin integration shell.
- Migration-primitive tests: unit-test the baseline IMPORTING/MIGRATING/SETSLOT sequencing and re-entrancy (resume a slot left mid-migration) against a real Redis in envtest/e2e; ASM path tested on an 8.4+ image asserting `CLUSTER MIGRATION STATUS` reaches `state=completed` with keys intact; capability-probe test asserting fallback to the baseline when ASM is absent.

## 9a. Sibling gap — LR-019 (replica rebalance), and whether to do it in this go

LR-014 explicitly deferred a **replica-maldistribution** repair (e.g. shard layouts like 2+1+0 replicas across masters where *all masters have slots*, so the cluster is "healthy" but a shard has no redundancy, and — critically — no empty master exists to trigger Step 4). The user asked whether to fold that fix in here. Analysis:

- **Different axis, orthogonal trigger.** LR-018 is *master/slot* maldistribution (a hard `Initializing` deadlock). LR-019 is *replica* maldistribution among healthy masters (a silent, not-even-reported-unhealthy suboptimal state). Neither subsumes the other.
- **LR-018 alone fully resolves *this* field report.** After LR-018 moves 10923-16383 onto an empty master (say cluster-4), that new master has 0 replicas, so the **existing** Step 4 attaches the remaining empty master (cluster-5) to it → a balanced 3 masters × 1 replica. No LR-019 logic is needed for the reported case; LR-019 covers the *other* drift that this dump does not exhibit.
- **Shared architecture, very different cost/risk.** Both live in `repairCluster` and both want the same pure planner seam (§7.1). But LR-019 needs **no new primitive** — it is a `CLUSTER REPLICATE` from an over-replicated master to a zero-replica master, using the client method that already exists. LR-018 needs the whole slot-migration capability (§7.2) with envtest/e2e surface. Coupling a low-risk change to a high-test-surface one in a single commit is undesirable.
- **LR-019 needs its own no-deadlock argument.** LR-014 deferred it precisely because gating health on per-master replica *counts* risks a deadlock if the rebalance step is buggy (the exact trap LR-014's own fix avoided). That argument (and the health-clause decision: do we make replica-maldistribution *unhealthy*, or repair-silently while staying healthy?) must be made deliberately, not bolted on.

**Recommendation:** **one worktree/PR, two commits + two changelog entries** — land **LR-018** (the deadlock) first, then **LR-019** (replica rebalance) on top, both sharing the `planReshard`/planner seam so we don't build it twice or rebase a second change over the first. This is the "do it now-ish, don't get bitten in 2 months" path without coupling the risk profiles. If schedule forces a cut, LR-018 ships alone and still closes the field report; LR-019 remains a clean, cheap follow-up on the same seam. **Do not** squash them into one commit.

## 10. Relationship to prior work

- **LR-014** already flagged the sibling gap: *"replica **maldistribution** … No repair step rebalances existing replicas … the gap is real and tracked separately (littlered-internal) for an eventual replica-rebalance repair step."* LR-018 is the **master/slot** member of that family (shard-owner maldistribution), worse than the replica case because it is a hard `Initializing` deadlock, not just a suboptimal-but-healthy layout. The replica-rebalance follow-up is scoped here as **LR-019** (§9a).
- **Sentinel analog (ADR-005 / LR-015 / LR-016):** "safely recover from a sole surviving data holder" was the sentinel-mode expression of *operator-owns-topology, never destroy a live copy*. LR-018 applies the same principle to cluster mode — with the sharper conclusion that here a **non-lossy** recovery always exists, so (unlike sentinel's `allowUnsafeRebootstrapOnDeadlock`) **no unsafe opt-in is warranted**.
- **Cross-mode parity (CLAUDE.md §7):** the ADRs to date are sentinel-only; this is the first cluster-mode ADR-class recovery rule. When implemented, add the changelog entry `[LR-018]` and a cluster-mode ADR (next: `docs/adr/006-*`), and cross-link pillar §3.x.

## 11a. Validation — ASM path (s1 lab, 2026-07-29)

The Redis 8.4+ (native ASM) path is verified end-to-end on a live 8.4.2 cluster (operator build `littlered:c749b00`):

- **Setup:** healthy 3×1 cluster, 300 keys (98 in shard 2). Operator paused; shard 2's range moved onto cluster-0 (so cluster-0 owns 0-5461 **and** 10923-16383, 196 keys) and cluster-2 turned into a fresh empty master — the exact consolidated topology (`cluster_size:2`, one empty master).
- **Result:** on operator restart it logged `Consolidated-shard reshard: relocating surplus range (LR-018)` (source cluster-0, dest cluster-2, `atomicSlotMigration:true`) → `Started atomic slot migration` → healed to `cluster_size:3`, phase `Running`. **300/300 keys intact, read-back hits=300 misses=0.** Capability detection (`atomicSlotMigration:true`), `CLUSTER MIGRATION IMPORT`/`STATUS` command surface, and Step 3b all confirmed on real 8.4.2.

**Finding — ASM auto-demotes an emptied source.** When a node loses its *last* slots to a native `CLUSTER MIGRATION IMPORT`, Redis 8.4 turns it into a **replica of the destination**, not a slotless master. Consequences:

1. The operator's reshard is safe: the double-owner (cluster-0) keeps its *other* range (0-5461), so it is never emptied and never demoted — it stays the shard-0 master. Confirmed.
2. Native ASM *consolidation* therefore never produces an empty master; the empty-master trigger comes from EmptyDir restarts / cold-start nodes (the field cause) or the pre-8.4 dance — consistent with the whole premise. A test setup must inject the empty master explicitly (we used `CLUSTER RESET SOFT` + `MEET` to mimic a restarted node), because ASM-based consolidation alone won't.

**Residual:** the `CLUSTER MIGRATION STATUS` non-empty/in-flight parse was not stressed (98-key migrations complete in ~5 ms, so the operator observed completion via the gather before any in-flight poll). redis.io documents the reply as an array of per-task field/value arrays for both RESP2/RESP3, which `parseMigrationTasks` handles; the slow-migration in-flight path gets real exercise in the pre-8.4 dance work (Task #6) and should be re-confirmed there.

## 11. Open questions (decide before/while implementing)

1. ~~**Baseline vs ASM**~~ **RESOLVED (§7.2/§7.3):** build the pre-8.4 dance as the baseline, opportunistically use Redis 8.4+ `CLUSTER MIGRATION IMPORT` when a runtime capability probe confirms it. Remaining sub-item: confirm the probe mechanism on a Valkey target (does Valkey expose the same command / INFO fields?) before relying on it there.
2. **Batch granularity** — one full range per reconcile vs. N keys per pass. Range-at-once is simpler; key-batching bounds per-pass work on large shards. Lean key-batched with a bounded budget, resumable across passes.
3. **Destination tie-break** — restore the strict bootstrap pod-index→shard model, or accept any empty master and just guarantee distinctness? Restoring the index model is tidier for humans/`lrctl` but may cause an extra migration; distinctness-only is cheaper. Leaning distinctness-only (least interference).
4. **`lrctl verify`** — surface "consolidated shard / master owns >1 range" as an explicit finding so this is diagnosable in the field without reading gossip by hand.
