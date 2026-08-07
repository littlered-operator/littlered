# In-Place Legacy → Per-Shard Cluster Migration — Design & Implementation

Companion mechanics for **ADR-013**. Decision-altitude rationale lives there; this document is the
implementable plan. Scope: shape-preserving, online, in-cluster migration of a pre-0.3 single-STS
cluster (`{name}-cluster`, pods `{name}-cluster-N`) into the 0.3 per-shard layout
(`{name}-shard-K`, pods `{name}-shard-K-M`), on the same running Redis Cluster.

Base branch: `feat/legacy-cluster-migration` off `release/0.3.0`.

---

## 1. Reuse map — what already exists

The migration is ~90% sequencing of existing, identity-based primitives. New code is the driver, the
pure planner, one gather-from-legacy path, and the guard/repair-loop changes.

| Need | Existing surface | File |
| --- | --- | --- |
| Enumerate cluster-member pods (incl. legacy) | pod list by `component=cluster` label selector | `resources.go` `clusterSelectorLabels` |
| Pod/shard identity of the **new** layout | `ClusterPodRefs`, `clusterShardStatefulSetName`, `shardMasterPodName` | `cluster_topology.go` |
| Reverse pod-name → shard | `ShardIndexFromPodName` | `internal/redis/shard_colocation.go` |
| Stand up new empty shard STSs / Service / PDB | `ensureClusterResources`, `buildClusterShardStatefulSet`, `buildClusterHeadlessService`, `buildClusterShardPDB` | `cluster_reconcile.go`, `resources.go` |
| Gather ground truth from a seed | `GatherClusterGroundTruth` (seed addr → `CLUSTER NODES` → full state by IP/ID) | `internal/redis/gather.go`, `cluster_state.go` |
| Deterministic shard slot ranges | `GenerateSlotRanges(shards)`, `ExpandSlotRange`, `FormatSlotRange` | `cluster_client.go` |
| Join a node | `ClusterMeet` | `cluster_client.go` |
| Move a slot range (native) | `ClusterMigrationImport`, `ClusterMigrationInFlight`, `AtomicSlotMigration` probe | `cluster_migrate.go`, `cluster_state.go` |
| Move a slot range (pre-8.4 dance) | `reshardViaDance` + `ClusterSetSlots{Importing,Migrating,Node}`, `ClusterCountKeysInSlots`, `ClusterGetKeysInSlot`, `MigrateKeys`, `SlotsNeedingDrain` | `cluster_reshard.go`, `cluster_migrate.go`, `reshard_plan.go` |
| Attach a replica | `ClusterReplicate`, `NodeKnows` (defer if dest unknown) | `cluster_client.go`, `cluster_state.go` |
| Drop a node | `ClusterForget` (broadcast, skip masters-of-live-replicas) | `cluster_client.go` |
| Detect legacy STS | `detectLegacyClusterStatefulSet`, `clusterStatefulSetName` | `cluster_reconcile.go`, `resources.go` |
| Tunables (batch size, timeouts) | `spec.cluster.reshard{KeyBatchSize,MaxKeysPerReconcile,MigrateTimeoutMillis}` | reused as-is |

**The reshard executor is the workhorse.** `reshardConsolidated` already moves one arbitrary range
from a source addr to a dest addr per reconcile, ASM-or-dance by capability, resumable from on-node
markers. The migration `Draining` phase drives the same executor with different (source, dest)
pairs. Factor the executor's inner "move one range" out of `reshardConsolidated` into a shared
`moveSlotRange(ctx, gt, clusterClient, move ReshardMove) (done bool, res ctrl.Result, err error)` so
both callers share it (behavior-preserving refactor under existing reshard tests — no new red needed
there).

---

## 2. New code

### 2.1 `internal/controller/cluster_migration.go` (the fenced driver)
Entry point called from `reconcileCluster` *before* the current legacy guard:

```go
// migrateLegacyCluster runs one step of the legacy→per-shard migration state machine.
// Returns handled=true if it owned this reconcile (caller returns res immediately).
func (r *LittleRedReconciler) migrateLegacyCluster(
    ctx context.Context, lr *littleredv1alpha1.LittleRed,
) (res ctrl.Result, handled bool, err error)
```

Flow:
1. `detectLegacyClusterStatefulSet` — if absent, `handled=false` (normal 0.3 reconcile; if migration
   just completed this is the transition back to steady state).
2. If annotation `redis.chuck-chuck-chuck.net/migrate-legacy-sts == "hold"` → set the in-progress
   `LegacyClusterTopology` condition (Reason `MigrationHeld`), no mutation, requeue. `handled=true`.
3. Gather from legacy (see §2.3). If gather fails / no reachable legacy seed → report + requeue.
4. Health-gate (§2.4). If not satisfied → report `MigrationWaitingHealthy`, requeue. `handled=true`.
5. Shape-preserving check (§2.5). If violated → terminal `Failed` / `LegacyClusterTopology`
   (Reason `MigrationUnsupportedTopology`) + warning event. `handled=true`.
6. `plan := planClusterMigration(gt, spec, legacyFacts)` (§2.2). Execute `plan.Actions` for the
   current phase (idempotent), update `status.cluster.migration`, requeue. `handled=true`.

### 2.2 `internal/redis/migration_plan.go` (the pure seam)

```go
type MigrationPhase string // Standup, Meet, Draining, ReplicasAttached, Decommission, Complete

type MigrationPlan struct {
    Phase       MigrationPhase
    Meets       []string        // new-pod addrs to MEET (via a legacy seed)
    Move        *ReshardMove    // next range to move this pass (nil if none)
    Replicates  []ReplicaAttach // {ReplicaAddr, MasterID} to attach
    Forgets     []string        // legacy node IDs to FORGET
    DeleteLegacy bool           // decommission: delete {name}-cluster STS+PDB
    ShardsMoved int
    Reason      string
}

// planClusterMigration derives the phase purely from live ground truth.
func planClusterMigration(gt *ClusterGroundTruth, shards, replicasPerShard int, name string,
    legacy LegacyFacts) MigrationPlan
```

Phase derivation (all from live `gt`, no persisted cursor):
- **Standup** — the new `{name}-shard-K` masters are not yet all cluster members. Action: (STS
  creation is done by `ensureClusterResources` in the driver, not the plan); nothing to move yet.
- **Meet** — new STSs exist and their pods are up, but some new pods are not yet in `gt.Nodes`
  (not MET). Emit `Meets` for the missing ones (dest addr = new pod IP; seed = any reachable legacy
  master).
- **Draining** — some shard K's range `GenerateSlotRanges(shards)[K]` is not yet fully owned by
  `{name}-shard-K-0`. Emit one `Move{range, source=current owner, dest={name}-shard-K-0}`. Choose
  the lowest un-migrated K for determinism. `ShardsMoved` = count of ranges already on their new
  master.
- **ReplicasAttached** — all ranges are on new masters, but some `{name}-shard-K-M` (M≥1) is not yet
  replicating `{name}-shard-K-0`. Emit `Replicates` (defer any whose dest the replica doesn't
  `NodeKnows` yet).
- **Decommission** — all new masters own their ranges and all new replicas are attached, and legacy
  nodes still exist in `gt.Nodes`. Emit `Forgets` for every legacy node ID (they own zero slots by
  now) and set `DeleteLegacy=true` once no legacy node remains slot-owning.
- **Complete** — no legacy nodes in `gt`, legacy STS gone. (The driver's step 1 will then return
  `handled=false`.)

`LegacyFacts{ SeedAddrs []string; LegacyNodeIDs []string; NewPodAddrs map[string]string }` is
assembled by the driver from the legacy pod list + `gt`; the plan stays pure. `NewPodAddrs` is keyed
by pod name (`{name}-shard-K-M` → dial addr) so it uniformly serves both MEET and replica-attach; a
missing entry means the pod isn't up yet (⇒ Standup). Current slot-range ownership is derived from
`gt` directly, so no `LegacyMasterRanges`/`SlotRange` field is needed (M1; there is no `SlotRange`
type — `GenerateSlotRanges` returns `[]struct{Start,End int}`). `ReshardMove.Source/Dest` are
`*ClusterNodeState` (not strings), so `MigrationPlan.Move` carries node pointers and the driver
formats `PodIP:RedisPort`. `Draining` is gated on `countShardsOnNewMasters(gt) < shards` (not on a
Move being emittable), so a pass where a range is mid-dance with no clean single owner stays in
`Draining` and resumes next pass rather than falsely advancing. Phase consts are `Migration*`-prefixed.

### 2.3 Gather-from-legacy (the one genuinely new gather path)
Steady-state `gatherGroundTruth` enumerates *expected* pods via `ClusterPodRefs` (new naming), so it
can't see legacy pods before the new STSs exist. Add a thin helper that lists **actual** cluster
member pods by the `component=cluster` label selector (selects legacy + new), picks a reachable seed
IP, and calls the existing `GatherClusterGroundTruth(seed)`. Everything downstream (`CLUSTER NODES`)
is already by IP/ID, so the returned `gt` includes legacy *and* new nodes uniformly.

### 2.4 Health-gate predicate (pure, testable)
`legacyMigrationReady(gt, allLegacyPodsReady bool) bool` ⟺ `gt.ClusterState == "ok"` AND
`gt.TotalSlots == 16384` AND `allLegacyPodsReady` AND a reachable master quorum (`reachable*2 > total`
over slot-owning masters). Pod readiness (kubelet — blackhole-proof, per LR-017/023) is injected as a
bool from the pod list, keeping the function pure.

### 2.5 Shape-preserving check (pure, testable)
`legacyShapePreserved(gt, shards, replicasPerShard) bool` ⟺ the legacy cluster has exactly `shards`
slot-owning masters, each owning exactly one aligned `GenerateSlotRanges(shards)[K]` range, and the
member count matches `shards × (1+replicasPerShard)`. Anything else → refuse (Decision 5).

---

## 3. Reconcile & guard wiring

`reconcileCluster` (current order: multi-site branch → legacy guard → scale-down guard →
`ensureClusterResources` → readiness → wipe recovery → gather → repair):

```
+  res, handled, err := r.migrateLegacyCluster(ctx, lr)
+  if handled || err != nil { return res, err }        // migration owns the reconcile
   // (legacy guard now only reached when NOT migrating — see below)
   ...
```

- **`reportLegacyClusterTopology`**: no longer terminal by default. The driver sets the in-progress
  variant (phase-carrying condition, `Phase` stays `Initializing`/a new `Migrating`). The terminal
  `Failed` path is kept only for the refuse cases (held-with-error, unsupported topology).
- **Ghost-FORGET (Step 2)**: taught to exempt legacy-named node IDs while a legacy STS exists, so a
  stray steady-state pass can never evict a legacy node mid-migration. Belt-and-suspenders on top of
  the repair-loop suspension.
- **Repair loop**: `migrateLegacyCluster` returning `handled=true` short-circuits before `repairCluster`,
  so the steady-state loop simply never runs during migration.
- **Decommission delete**: driver deletes the `{name}-cluster` STS and `{name}-cluster-pdb` via the
  controller client, gated on "no legacy node owns any slot" (from `gt`). This is the one operator
  delete of a workload — justified in ADR-013 §4 (zero-slot ⟺ zero-data).

### Coexistence properties to assert at implementation time (load-bearing)
- Legacy pods carry `component=cluster` (the old builder used the shard-agnostic
  `clusterSelectorLabels`), so the shared headless Service `{name}-cluster` fronts them too and pod
  DNS resolves across old+new. **Confirmed in recon; add an e2e assertion.**
- New empty masters must actually park waiting to be MET (not self-bootstrap a rival cluster). The
  cluster startup script's STEP-3 yield loop (LR-003) should hold them; **verify** a fresh pod under
  the migration path doesn't `ADDSLOTS` itself. If it can, MEET them *before* they'd self-form, or
  ensure the driver's `Standup`→`Meet` ordering wins.
- `cluster-allow-replica-migration no` (already set) keeps Redis from auto-moving replicas across the
  old/new boundary during `Draining` — desirable; no change.

---

## 4. Status surface
`api/v1alpha1` — add to `ClusterStatusInfo` (the real type name; monitoring only, re-derived each
pass, ADR-006):

```go
type ClusterMigrationStatus struct {
    Phase       string       `json:"phase,omitempty"`       // Standup..Complete
    ShardsMoved int          `json:"shardsMoved,omitempty"`
    TotalShards int          `json:"totalShards,omitempty"`
    StartedAt   *metav1.Time `json:"startedAt,omitempty"`
}
```
Run `make manifests generate`. `lrctl status`/`verify` gain a one-line migration banner when
`status.cluster.migration.phase` is set and not `Complete` (read-only; small addition to the CLI
status renderer).

---

## 5. Test plan (red-first, per project Test Discipline)

**Tier 2 — pure unit (fast red-green), the bulk of the coverage:**
- `planClusterMigration` phase-derivation table: for each synthetic `gt` (nothing MET / partially
  MET / mid-drain / fully drained but replicas unattached / ready to decommission / complete) assert
  the expected `MigrationPlan`. Write assertions from this doc's phase table *first*, watch fail,
  implement.
- `legacyMigrationReady` — unhealthy legacy variants (missing slots, not-ok, pod not Ready, no
  quorum) each return false; the healthy one true.
- `legacyShapePreserved` — wrong master count, fragmented range, wrong member count → false; exact
  shape → true.
- `moveSlotRange` refactor: existing reshard tests stay green (behavior-preserving; no new red).

**Tier 1 — bug-class guards (if any surface during impl):** e.g. a ghost-FORGET that evicts a legacy
node → failing repro first.

**Tier 3 — e2e (`test/e2e/`), the long pole:**
- Harness: deploy the **pre-split operator image** (build at `85e1a93^`, the last single-STS commit)
  to bootstrap a *real* `{name}-cluster` single-STS cluster; write N keys spanning all shards.
  Then deploy the migration-capable operator image (this branch's git-hash tag) against the same
  cluster (per the e2e build/deploy loop: commit-first, image tag = git hash, runs on the existing
  multi-node cluster via kube context — never Kind).
- Target assertion first (red against current `release/0.3.0`, which refuses): after upgrade, the
  operator drives migration to `Complete`; assert (a) all keys intact, (b) `cluster_state:ok`,
  16384 slots, (c) `{name}-cluster` STS gone, N `{name}-shard-K` STSs present, (d) `lrctl verify`
  colocation passes, (e) shared-Service coexistence held during `Draining` (a client stayed served).
- `hold` sub-case: with the `hold` annotation, assert the operator does **not** mutate (phase parks,
  legacy STS untouched) until the annotation is removed.
- Health-gate sub-case (best-effort, may be opportunistic like LR-017's REFUSE tier): degrade the
  legacy cluster before upgrade, assert migration does not start until healthy.

---

## 6. Effort

| Piece | Est. |
| --- | --- |
| `planClusterMigration` + health/shape predicates + tier-2 tests | ~1 d |
| Driver `cluster_migration.go` + gather-from-legacy + `moveSlotRange` refactor | ~1–1.5 d |
| Guard repurposing + ghost-FORGET exemption + repair suspension + status/CRD | ~0.75 d |
| Decommission (FORGET + STS/PDB delete, RBAC already has pod delete; add STS delete if missing) | ~0.5 d |
| e2e harness (pre-split bootstrap → upgrade → assert) | ~1–1.5 d |
| Docs (finalize ADR-013, amend ADR-007 §5, USAGE upgrade notes) | ~0.5 d |
| **Total** | **~4–5 focused days**, e2e the long pole |

## 7. Open items / risks
- **RBAC**: confirm the operator can `delete` StatefulSets (it has `delete` on pods from LR-023).
  Add `statefulsets` delete if absent.
- **New-master self-bootstrap** during `Standup` (see §3 coexistence) — the one behavior to verify
  early, since it's the only place a fresh pod could form a rival cluster.
- **Native ASM vs dance** parity across the migration path is inherited from the reshard executor;
  the e2e should run at least once on a pre-8.4 image to exercise the dance (as LR-018 did).
- **Shard-count / replica-count change** during migration is explicitly out of scope (ADR-013 §5);
  a follow-up reshard handles it post-migration.

## 8. As-built deltas (M3)

Accepted deviations from §1–§7, folded into the record after the M3 driver landed:

- **`restrictToLegacyMesh` (new helper, refines §2.3).** `GatherClusterGroundTruth` includes *every*
  reachable probed pod in `gt.Nodes` — but a fresh, un-MET `{name}-shard-K-0` is its own single-node
  cluster, so it would appear in `gt.Nodes` prematurely and M1's plan (which keys Meet-detection on
  `gt.Nodes` *absence*) would skip Meet straight to a doomed Draining. The driver therefore filters
  `gt.Nodes` down to the legacy-rooted partition (via `gt.Partitions`) before planning, reconstructing
  M1's mesh-membership contract. Safe default: if no legacy-containing partition is identifiable, it
  does not filter.
- **Entry gates run once, at intact-legacy entry (refines §2.4/§5).** `LegacyMigrationReady` and
  `LegacyShapePreserved` are enforced only while no new-shard pod exists yet. Once migration is
  underway a drained legacy master owns zero slots and would false-fail the shape check, so the gates
  are not re-run; the migration is idempotent and resumable and cannot be un-started.
- **`moveSlotRange` shared executor (refines §1).** Extracted from `reshardConsolidated`; both the
  LR-018 consolidated-shard reshard and the migration Draining phase call it. `reshardViaDance`'s
  return changed to `(done, res)` — `done` is true only on the ownership-flip pass; inert for the
  reshard caller (which ignores it), advisory logging for the migration caller. Behavior-preserving
  (existing reshard tests + envtest are the guard).
- **No new `LittleRedPhase` (refines §3).** During migration `status.phase` stays `Initializing` and
  `status.status` = `"Migrating"`; the migration phase is surfaced via `status.cluster.migration.phase`
  and the repurposed `LegacyClusterTopology` (Ready=False) condition. Avoids CRD enum churn / lrctl drift.
- **`ensureMigrationResources` excludes the PDB reconcile (refines §3).** It creates ConfigMap +
  shared Services + per-shard STSs but deliberately does NOT run `reconcileClusterPDB`, which would
  delete the legacy `{name}-cluster-pdb` early. The legacy PDB survives until Decommission so legacy
  pods keep disruption protection while they still hold slots; per-shard PDBs are created by the
  normal reconcile once migration completes.
- **Condition reasons:** `MigrationHeld` (hold), `MigrationWaitingSeed` (no reachable legacy seed),
  `MigrationWaitingHealthy` (health gate), `MigrationInProgress` (per-phase), `MigrationUnsupportedTopology`
  (terminal refuse). `reportLegacyClusterTopology` was renamed `reportLegacyMigrationRefused` and now
  serves only the terminal-refuse case.
- **RBAC unchanged.** `apps/statefulsets` already carried `delete` (LR-023); no marker/role change needed.
- **Investigations resolved.** (1) New-master self-bootstrap: SAFE — a fresh cluster pod (no `nodes.conf`)
  execs `redis-server` without ever running `CLUSTER ADDSLOTS`; slot assignment is operator-only
  (`bootstrapCluster` inside `repairCluster`, which is suspended during migration), so a fresh new pod is
  an empty single-node cluster waiting to be MET, never a rival cluster with slots. (2) Shared-Service
  coexistence: CONFIRMED — pre-split pods carry `component=cluster` (`git show 85e1a93^`), which the shared
  headless Service `{name}-cluster` selects, so it fronts legacy + new pods. M5 adds the live assertion.
