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
| Sync a new node onto the current range owner | `ClusterReplicate`, `NodeKnows` (defer if target unknown), `ClusterNodeState.LinkStatus` (the `up` sync gate) | `cluster_client.go`, `cluster_state.go` |
| Promote a synced new master (atomic handoff) | `ClusterFailover` (coordinated), `ClusterFailoverTakeover` (forced fallback when the legacy owner is unreachable) | `cluster_client.go` |
| Drop a node | `ClusterForget` (broadcast, skip masters-of-live-replicas) | `cluster_client.go` |
| Detect legacy STS | `detectLegacyClusterStatefulSet`, `clusterStatefulSetName` | `cluster_reconcile.go`, `resources.go` |
| Tunables (batch size, timeouts) | `spec.cluster.reshard{KeyBatchSize,MaxKeysPerReconcile,MigrateTimeoutMillis}` | reused as-is |

**Replication + coordinated failover is the workhorse (LR-025).** Migration does **not** move slots.
It stands each new pod up as a slot-less replica of the legacy master owning its range, waits for the
replication link to come `up`, then promotes `{name}-shard-K-0` with a coordinated `CLUSTER FAILOVER`.
All four verbs already exist (`ClusterMeet`, `ClusterReplicate`, `ClusterFailover` / `ClusterFailoverTakeover`,
`ClusterForget`, `cluster_client.go:144–229`), so the new code is purely the *sequencing* — a pure planner
and a driver — with no new I/O primitive and no ASM-vs-dance capability branch in the migration path.

> **Superseded:** an earlier cut drove the LR-018 reshard executor (`reshardConsolidated` / a shared
> `moveSlotRange`, ASM-or-dance by capability) from a `Draining` phase, attaching replicas only afterward.
> That is the LR-025 restart-safety hole (ADR-013 Status/Alternatives): a new master owned slots on EmptyDir
> with no replica for the whole drain, so a restart deadlocked STEP-3 and lost source-deleted keys. The
> reshard executor **stays in the tree for LR-018 consolidated-shard recovery**; migration simply no longer
> calls it. See §10 for the pivot and exactly what from §8/§9 survives.

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

### 2.2 `internal/redis/migration_plan.go` (the pure seam — LR-025 replicate-then-failover)

```go
type MigrationPhase string // Standup, Meet, Replicate, Failover, Decommission, Complete

type MigrationPlan struct {
    Phase        MigrationPhase
    Meets        []string        // new-pod addrs to MEET (via a legacy seed)
    Replicates   []ReplicaAttach // {ReplicaAddr, MasterID}: new pod → replicate the current range owner
    Failovers    []FailoverAction // {name}-shard-K-0 to promote this pass (at most one, for determinism)
    Forgets      []string        // legacy node IDs to FORGET (sorted; only once the shard is fully re-replicated)
    DeleteLegacy bool            // decommission: delete {name}-cluster STS+PDB (all legacy FORGOTTEN)
    ShardsMoved  int             // shards whose new master {name}-shard-K-0 already owns range K
    TotalShards  int
    Reason       string
}

// FailoverAction promotes a synced new master. Coordinated CLUSTER FAILOVER by default;
// Force (=> CLUSTER FAILOVER TAKEOVER) only when the current range owner is unreachable
// and this replica is confirmed synced (the legacy-master-died-mid-migration edge, §7).
type FailoverAction struct {
    Addr  string // {name}-shard-K-0 dial addr (the replica to promote)
    Force bool
}

// PlanClusterMigration derives the phase purely from live ground truth (ADR-013 §7).
func PlanClusterMigration(gt *ClusterGroundTruth, shards, replicasPerShard int, name string,
    legacy LegacyFacts) MigrationPlan
```

Phase derivation is strict-precedence (all from live `gt`, no persisted cursor): the plan reports the
least-advanced phase that still has work, and emits only that phase's action set. This finishes **all**
replication before **any** failover, so every slot has ≥2 live copies before a single handoff.

- **Complete** — no legacy nodes remain in `gt`. (`presentLegacyNodes == 0`; the driver's step 1 then
  returns `handled=false` and steady state resumes.)
- **Standup** — not all new `{name}-shard-K-M` pods (masters *and* replicas) are cluster members yet, and
  none can be MET (no address / no seed). STS creation is the driver's job (`ensureMigrationResources`);
  the plan just reports the phase.
- **Meet** — some new pod is not yet in `gt.Nodes` but has an address and a legacy seed exists. Emit
  `Meets` for the missing ones (seed = any reachable legacy master).
- **Replicate** — every new pod is MET, but some new pod is not yet a link-`up` replica of the node that
  currently owns its shard's range. The target is derived from `gt`: `ownerOfRange(gt, GenerateSlotRanges(shards)[K])`
  — the legacy master pre-failover, `{name}-shard-K-0` post-failover (so the new *replicas* auto-target the
  new master once it is promoted; in practice Redis reparents them, and this only re-emits if they haven't).
  A new pod that already owns its range (`{name}-shard-K-0` post-failover) is "done", not a replicate target.
  Emit `Replicates` for the rest; **defer** (count, don't emit) any whose target the replica does not yet
  `NodeKnows` (avoids `ERR Unknown node`). `MasterID` = the target owner's `NodeID`.
- **Failover** — every new pod is a link-`up` replica (Replicate is fully satisfied), but some `{name}-shard-K-0`
  does not yet own range K. Emit **one** `FailoverAction` for the lowest such K (coordinated; `Force` only
  on the unreachable-owner edge). `{name}-shard-K-0` is, by the Replicate gate, a synced replica of the
  legacy master owning K, so the coordinated failover is a lossless atomic ownership flip.
- **Decommission** — every `{name}-shard-K-0` owns range K **and** every new replica `{name}-shard-K-M` (M≥1)
  is a link-`up` replica of its own new master `{name}-shard-K-0`. This is the **redundancy gate**, and it is
  enforced *by the strict precedence itself*, not by a separate condition: after all failovers `ownerOfRange(range K)`
  is `{name}-shard-K-0`, so a new replica that is not yet a link-`up` replica of its new master leaves the
  `Replicate` phase unsatisfied (higher precedence) and the plan never reaches Decommission — a legacy node is
  thus only removed once the shard replacing it is fully `(1+rps)`-replicated on new nodes. At Decommission emit
  `Forgets` for every present legacy node ID (all slot-less demoted replicas by now); set
  `DeleteLegacy = !anyLegacyOwnsSlots(gt)` — which, since reaching Decommission requires every `{name}-shard-K-0`
  to own its range, is always true here, so the legacy STS + PDB delete fires. (An earlier draft keyed
  `DeleteLegacy` on "no legacy node remains in `gt`" — a bug: that condition is `Complete`, which has strict
  precedence over Decommission, so it would never hold *at* Decommission and the STS would never be deleted.
  LR-025 as-built.)

Helpers (pure, in `migration_plan.go`): `ownerOfRange(gt, start, end) *ClusterNodeState`;
`isLinkUpReplicaOf(rep, masterNodeID) bool` (`rep.Role == roleReplica && rep.MasterNodeID == masterNodeID
&& rep.LinkStatus == "up"`); `countShardsOnNewMasters(gt, name, ranges)` (reused, = `ShardsMoved`);
`newMasterPodName` / `newReplicaPodName` / `expectedNewPods` / `allNewPodsMet` / `presentLegacyNodes` /
`presentLegacyIDsSorted` (reused as-is). Phase consts are `Migration*`-prefixed.

`LegacyFacts{ SeedAddrs []string; LegacyNodeIDs []string; NewPodAddrs map[string]string }` is unchanged
(assembled by the driver from the legacy pod list + `gt`; the plan stays pure). `NewPodAddrs` is keyed by
pod name (`{name}-shard-K-M` → dial addr) so it uniformly serves MEET and both replica-attach kinds; a
missing entry means the pod isn't up yet (⇒ Standup). Current range ownership is derived from `gt` directly.
The `Move *ReshardMove` field and every `moveSlotRange`/ASM/dance reference are **removed** from the
migration plan (they remain in the LR-018 reshard path, untouched).

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
  controller client, gated on the plan's `DeleteLegacy` (no legacy node remains in `gt`, which the plan
  only reaches once every new master owns its range **and** every new replica is a link-`up` replica of
  its new master — the §2.2 redundancy gate). This is the one operator delete of a workload — justified
  in ADR-013 §4 (a decommissioned legacy node is a slot-less demoted replica; zero-slot ⟺ zero-data).

### Coexistence properties to assert at implementation time (load-bearing)
- Legacy pods carry `component=cluster` (the old builder used the shard-agnostic
  `clusterSelectorLabels`), so the shared headless Service `{name}-cluster` fronts them too and pod
  DNS resolves across old+new. **Confirmed in recon; add an e2e assertion.**
- New empty masters must actually park waiting to be MET (not self-bootstrap a rival cluster). The
  cluster startup script's STEP-3 yield loop (LR-003) should hold them; **verify** a fresh pod under
  the migration path doesn't `ADDSLOTS` itself. If it can, MEET them *before* they'd self-form, or
  ensure the driver's `Standup`→`Meet` ordering wins.
- `cluster-allow-replica-migration no` (already set) keeps Redis from auto-moving replicas across the
  old/new boundary while the new nodes are attached as replicas of the legacy masters (the `Replicate`
  phase) — desirable; no change. (The intended reparent — new replicas following `{name}-shard-K-0` after
  its promotion — is failover-driven, not auto-migration, so it is unaffected.)

---

## 4. Status surface
`api/v1alpha1` — add to `ClusterStatusInfo` (the real type name; monitoring only, re-derived each
pass, ADR-006):

```go
type ClusterMigrationStatus struct {
    Phase       string       `json:"phase,omitempty"`       // Standup, Meet, Replicate, Failover, Decommission, Complete
    ShardsMoved int          `json:"shardsMoved,omitempty"` // shards whose new master {name}-shard-K-0 already owns range K (failed over)
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
- `PlanClusterMigration` phase-derivation table (red-first, from the §2.2 table): for each synthetic `gt`
  assert the exact `MigrationPlan` — nothing MET (⇒ Standup/Meet) / partially MET (⇒ Meet with the right
  addrs) / MET but new pods not yet replicating the range owner (⇒ Replicate, incl. a `NodeKnows`-deferred
  case) / all replicating & link-`up` but a `{name}-shard-K-0` not yet owning its range (⇒ Failover, exactly
  one, lowest K) / all masters own their range but a new replica not yet link-`up` on its new master (⇒ stay
  in Decommission-gate, i.e. `Forgets` empty / not yet) / fully re-replicated with legacy present (⇒
  Decommission, `Forgets` set, `DeleteLegacy` per legacy-presence) / no legacy (⇒ Complete). Include an
  `rps=0` row for the Replicate/Failover/Decommission path (no new replicas; the redundancy gate is
  vacuous, so Decommission is reached as soon as `{name}-shard-K-0` owns its range).
- `ownerOfRange` / `isLinkUpReplicaOf` unit cases (the two new gates).
- `LegacyMigrationReady` — unhealthy legacy variants (missing slots, not-ok, pod not Ready, no quorum) each
  return false; the healthy one true. **Unchanged.**
- `LegacyShapePreserved` — wrong master count, fragmented range, wrong member count → false; exact shape →
  true. **Unchanged.**
- Reshard tests (`moveSlotRange`, `PlanReshard`, ASM/dance) stay green **untouched** — migration no longer
  calls them, so there is no refactor to guard here; they remain the LR-018 path's coverage.

**Tier 1 — bug-class / regression guards:**
- **The LR-025 restart-safety guard is the point of this redesign** — encode it as a unit assertion on the
  invariant the plan enforces: *a `{name}-shard-K-0` is only ever emitted for Failover when it is a link-`up`
  replica of the range owner*, and *no legacy node is emitted for FORGET until its shard is fully
  `(1+rps)`-replicated on new nodes*. A `gt` that violates the precondition must NOT emit the action. This
  is the fast, repeatable teeth; the e2e chaos tier (below) is the environment-specific confirmation.
- Ghost-FORGET exemption: a stray steady-state pass while a legacy STS exists must not evict a legacy node →
  failing repro first if the exemption regresses.

**Tier 3 — e2e (`test/e2e/cluster_migration_test.go`), the long pole:**
- Harness (unchanged): deploy the **pre-split operator image** (build at `85e1a93^`, the last single-STS
  commit) to bootstrap a *real* `{name}-cluster` single-STS cluster; write N keys spanning all shards. Then
  deploy the migration-capable operator image (this branch's git-hash tag) against the same cluster (e2e
  build/deploy loop: commit-first, image tag = git hash, runs on the existing multi-node cluster via kube
  context — never Kind).
- Target assertion first (red against current `release/0.3.0`, which refuses): after upgrade, the operator
  drives migration to `Complete`; assert (a) all keys intact, (b) `cluster_state:ok`, 16384 slots, (c)
  `{name}-cluster` STS gone, N `{name}-shard-K` STSs present, (d) `lrctl verify` colocation passes, (e)
  shared-Service coexistence held during the migration (a client stayed served across the per-shard failovers,
  seeing at most a `-MOVED`).
- **LR-025 restart-during-migration chaos tier (the regression guard for the bug that drove this redesign):**
  while migration is in flight (a new master is a synced replica / just after a failover), kill -9 a
  `{name}-shard-K-0`; assert it does **not** `CrashLoopBackOff`-deadlock and **no keys are lost** — the
  demoted legacy master (or the shard's new replica) fails over / re-serves. Run this on a **pre-8.4 image**
  too (the environment that first exposed the hole — the old dance path; the new mechanism is version-agnostic
  but the pre-8.4 run is where the original red was observed). May be opportunistic in *when* the kill lands
  (like LR-017's tier) but must **always** assert no-loss + no-deadlock.
- `hold` sub-case: with the `hold` annotation, assert the operator does **not** mutate (phase parks, legacy
  STS untouched) until the annotation is removed. **Unchanged.**
- Health-gate sub-case (best-effort, opportunistic like LR-017's REFUSE tier): degrade the legacy cluster
  before upgrade, assert migration does not start until healthy. **Unchanged.**

---

## 6. Effort

| Piece | Est. |
| --- | --- |
> LR-025 re-scopes this: the driver and plan are **reworked, not extended** from the superseded reshard
> build. Net effort is comparable — the plan is *simpler* (no ASM/dance branch, no key-batch draining), the
> driver swaps `moveSlotRange` for `ClusterReplicate`/`ClusterFailover` calls that already exist, and the
> e2e gains the restart-during-migration chaos tier.

| Piece | Est. |
| --- | --- |
| `PlanClusterMigration` rework (replicate/failover phases) + `ownerOfRange`/`isLinkUpReplicaOf` + tier-2 table | ~1 d |
| Driver `cluster_migration.go`: Replicate + Failover execution (reuse `ClusterReplicate`/`ClusterFailover`); drop the `Draining`/`moveSlotRange` call | ~0.75 d |
| Guard repurposing + ghost-FORGET exemption + repair suspension + status/CRD (mostly unchanged from the prior build) | ~0.5 d |
| Decommission redundancy-gate (FORGET + STS/PDB delete once fully re-replicated; RBAC already has STS delete) | ~0.5 d |
| e2e: replicate-then-failover assertions + the LR-025 restart-during-migration chaos tier (pre-8.4 + 8.4) | ~1–1.5 d |
| Docs (ADR-013 done; CLAUDE.md pillar 3.11/§9 as-built; changelog LR-025; USAGE upgrade notes) | ~0.5 d |
| **Total** | **~4 focused days**, e2e the long pole |

## 7. Open items / risks
- **RBAC**: `apps/statefulsets` already carries `delete` (LR-023 as-built §8); no change.
- **New-master self-bootstrap** during `Standup` — resolved SAFE (§8): a fresh cluster pod never
  `ADDSLOTS` itself. Under LR-025 the exposure shrinks further: new nodes become slot-less replicas in the
  `Replicate` phase, well before they could self-form.
- **Coordinated `CLUSTER FAILOVER` edge — legacy master dies mid-migration.** If the range-K owner becomes
  unreachable while `{name}-shard-K-0` is a synced replica, a coordinated failover cannot hand off. Handle in
  the plan: emit `FailoverAction{Force:true}` (⇒ `CLUSTER FAILOVER TAKEOVER`) **only** when the owner is
  unreachable *and* `{name}-shard-K-0` is confirmed link-`up`/synced (it holds the data); otherwise keep
  waiting (the legacy shard's own replica may be failing over — let it, then re-target). Never `Force` a
  replica that isn't confirmed synced. Cover with a tier-2 case.
- **Replicate fan-out pacing.** Emitting all `Replicate` actions in one pass starts up to `shards` concurrent
  full-syncs (one fork per legacy master). This is the ordinary replica-resync load sizing already tolerates
  (ADR-013 Alternatives), but if it proves heavy on a large dataset, bound the emitted set per pass (a natural
  extension — the plan already re-derives every pass). Default: emit all; note the knob, don't build it yet.
- **`FORGET` ordering with the demoted legacy master.** After a shard's failover the demoted legacy
  master reparents-syncs *from* the new master; the Decommission gate (`isLinkUpReplicaOf` new replicas)
  already ensures the shard is fully re-replicated on *new* nodes before FORGET, so removing the legacy copy
  is always safe — assert this in the e2e (no under-replication window).
- **Shard-count / replica-count change** during migration is explicitly out of scope (ADR-013 §5); a
  follow-up reshard handles it post-migration.

## 8. As-built deltas (M3)

> **Scope note (LR-025).** §8 and §9 record the as-built of the **superseded reshard-based** build. Most of
> it **survives** the LR-025 redesign unchanged (see §10 for the itemized survives/superseded list) — only
> the `moveSlotRange`/`Draining` bullet below is retired. Read §8/§9 as history; §10 is authoritative where
> they disagree.

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

## 9. As-built deltas (WS3 — precise trigger)

The migration trigger `detectLegacyClusterStatefulSet` is no longer name-only. It now Gets the
`{name}-cluster` StatefulSet and applies a pure `isLegacyClusterStatefulSet(sts, lr)` that requires ALL
of: name `{name}-cluster`, `component=cluster` label, the per-shard `redis.chuck-chuck-chuck.net/shard`
label **absent** (strongest discriminator vs a 0.3 per-shard STS), `spec.replicas == shards*(1+rps)`
(`GetTotalNodes()`, whole-cluster sizing), and controller-owned by this CR (`IsControlledBy`). Any check
failing ⇒ not a legacy cluster ⇒ no auto-migration. This is **defense-in-depth**, not the ms-smoke fix
(that was operator scoping — ADR-014): the ms-smoke STS was a genuinely legacy-shaped STS, so precise
detection wouldn't have changed it; what this stops is a stray / mis-sized / half-formed `{name}-cluster`
StatefulSet triggering a spurious migration. Edge (kept, fails *safe*): a CR deleted+recreated (new UID)
with an orphaned legacy STS lingering would fail `IsControlledBy` → no migration — acceptable, since
acting on an ambiguously-owned workload is the riskier choice. Red-first: a 10-case table observed
failing under both a false-stub and a true-stub before implementing.

## 10. Restart-safety redesign — replicate-then-failover (LR-025, supersedes the `Draining` reshard path)

**Why.** The prior build (§1–§9) migrated by node-to-node slot **reshard** (native ASM / MIGRATE dance,
reusing the LR-018 executor from a `Draining` phase) and attached the new replicas *afterward*
(`ReplicasAttached`). Live on s1 (dance path, Redis 7.4.0) this **deadlocked and lost data**: a new master
`{name}-shard-0-0` restarted mid-drain, and because it owned slots on EmptyDir **with no replica yet**, the
startup STEP-3 guard (LR-003) found no replica to `TAKEOVER`, parked → `CrashLoopBackOff`; the migrated keys,
already deleted from the source by `MIGRATE`, were gone. The hole is latent in the ASM path (its ~8s atomic
window merely made a restart unlikely). Root cause is a **design gap**, not a bug: draining puts data on a
single, replica-less, in-memory node — a guaranteed single point of failure for the whole drain window.

**The fix (structural, ADR-013 Decision 4 + phases).** Migration no longer moves slots. Each new pod joins
as a **slot-less replica** of the legacy master owning its range, full-syncs, and `{name}-shard-K-0` is then
promoted by a coordinated `CLUSTER FAILOVER` — an atomic ownership flip to an already-synced node whose old
master demotes to a live replica of it. New nodes reach the slot-owning state only *after* a clean, already-
redundant handoff, so the unsafe "owns slots, no synced replica" state never exists. Restart-safe for **all**
`replicasPerShard` (incl. 0 — the demoted legacy master is the transient replica until decommission), and the
**startup script is unchanged**. Phases: `Standup → Meet → Replicate → Failover → Decommission → Complete`
(§2.2). Why this over a minimal "attach a replica before draining" patch, and the full trade-off vs the
reshard mechanism (memory/fork, client churn, primitives, code): ADR-013 Alternatives.

**What survives from §8/§9 (unchanged by LR-025):**
- `restrictToLegacyMesh` (§8) — a fresh un-MET new pod is still its own single-node cluster; the mesh filter
  before planning is still required (now it prevents a premature `Replicate`/`Failover`, not a `Draining`).
- Entry gates run once at intact-legacy entry (§8): `LegacyMigrationReady` / `LegacyShapePreserved` unchanged.
- No new `LittleRedPhase`; `status.status="Migrating"`, phase via `status.cluster.migration` + the repurposed
  `LegacyClusterTopology` Ready=False condition (§8).
- `ensureMigrationResources` still excludes the PDB reconcile; legacy PDB survives until Decommission (§8).
- Condition reasons unchanged (§8): `MigrationHeld` / `MigrationWaitingSeed` / `MigrationWaitingHealthy` /
  `MigrationInProgress` / `MigrationUnsupportedTopology`; `reportLegacyMigrationRefused` for the terminal case.
- Investigations resolved (§8): new-master self-bootstrap SAFE; shared-Service coexistence CONFIRMED.
- RBAC unchanged (§8): `apps/statefulsets` already has `delete`.
- The **precise trigger** `isLegacyClusterStatefulSet` (§9) — unchanged; the mechanism the trigger *starts* is
  what changed.

**What is superseded (retired by LR-025):**
- The `Draining` and `ReplicasAttached` phases → replaced by `Replicate` then `Failover`.
- The migration's use of `moveSlotRange` / `reshardViaDance` / ASM (`ClusterMigrationImport`) and the ASM-vs-
  dance capability probe — the §8 `moveSlotRange`-shared-executor delta no longer applies to migration. **The
  executor and probe stay in the tree for LR-018 consolidated-shard recovery**, which is unaffected; only the
  migration caller is removed. (`reshardViaDance`'s `(done, res)` return can revert to whatever LR-018 alone
  needs, since the migration caller that motivated the `done` flag is gone.)
- `MigrationPlan.Move *ReshardMove` → replaced by `Replicates []ReplicaAttach` (already present) + new
  `Failovers []FailoverAction`.
