# Design Note: Per-Shard StatefulSets & Topology-Aware Placement (Cluster Mode)

> **Status:** Direction A **Milestone 1 (structural split) committed** in 0.3.0 — see
> ADR-007 and changelog LR-020. Direction A Milestone 2 (the `placement.shardAntiAffinity`
> API) and Direction B remain future work. This note keeps the full reasoning.
> **Created:** 2026-07-29 (design discussion). This note captures the reasoning so a
> future session can pick up without re-deriving it.
> **Decision owners:** the littlered authors (spare-time OSS); also dogfooded on a
> managed cloud, the intended proving ground.
> **Target release:** Direction A is a breaking change to cluster-mode topology —
> land it in **alpha** (a `0.3.x` bump), *before* any beta promotion of the API.

---

## 1. What we are considering

Two related but separately-shippable changes to **cluster mode**:

- **Direction A — Per-shard StatefulSets.** Replace the single cluster StatefulSet
  (`replicas = shards × (1 + replicasPerShard)`) with **one StatefulSet per shard**, each
  carrying a stable shard-identity label. This makes *per-shard* pod placement expressible
  with native Kubernetes scheduling — in particular "a shard's master and its replica(s)
  never share a failure domain."
- **Direction B — Topology-aware role assignment (optional, later).** Have the operator
  choose *which* pod of a shard is master, and re-choose after a failover, so that masters
  are balanced across failure domains. B is pure Redis-role assignment; it does **no**
  placement.

A must be done first and stands alone. B rides on top of A and is only justified by a
second, distinct goal (§3).

---

## 2. Motivation & the requirement that forces this

The **must-have** driver is **resilience against data loss when a single failure domain
(node/zone) is lost**. Because LittleRed is pure in-memory (no persistence; storage is
EmptyDir — see CLAUDE.md pillar 3.1), a shard's durability comes entirely from having its
master and replica(s) on *different* physical failure domains. If a shard's master and its
only replica sit on the same node and that node dies, the shard's data is gone and the
cluster loses slots.

Today this requirement is **not expressible**, and the reason is structural, not cosmetic.
A `topologySpreadConstraint` only isolates pods it can *select*, and the scheduler evaluates
it at **bind time** and never again (`...IgnoredDuringExecution`) — so per-shard isolation
needs a **stable, schedule-time, per-shard grouping key**. A single StatefulSet cannot carry
one: it has exactly one `spec.template`, so every pod is stamped identically, and the only
per-pod metadata Kubernetes injects on its own (`statefulset.kubernetes.io/pod-name`,
`apps.kubernetes.io/pod-index` = the raw ordinal, `controller-revision-hash`) is
ordinal/revision identity — **never shard-semantic**. Shard is a *function* of the ordinal
(`ordinal mod shards`), which K8s neither computes nor accepts, and `matchLabelKeys` groups by
label *equality* only (raw ordinal → wrong grain). An operator-patched label lands *after*
scheduling (too late); the only single-STS way to a schedule-time shard label is a bespoke
mutating admission webhook. **One template ⇒ no schedule-time shard key ⇒ only a webhook could
fake it** — so a `topologySpreadConstraint` / `podAntiAffinity` on one STS can only say "spread
*all* cluster pods," never "spread the pods *within* a shard." (Confirmed pre-0.3.0 in
`internal/controller/resources.go`: `buildClusterStatefulSet` built one STS at
`replicas = shards × (1+replicasPerShard)` with no shard-index or role label anywhere. The full
mandatory-not-cosmetic argument is ADR-007.)

### 2.1 Why a global spread does not satisfy the requirement

This is the crux, and the reason A is structural rather than a values tweak. A cluster-wide
even spread does **not** imply per-shard isolation. Example — 3 shards × 2 pods = 6 pods,
3 nodes, a single global `maxSkew: 1` on `kubernetes.io/hostname`:

```
N1: A-0  A-1      N2: B-0  B-1      N3: C-0  C-1
```

The global constraint is perfectly satisfied (2 pods/node, skew 0) yet **every shard is
co-located** — the worst possible outcome for Goal 1. Isolation must be scoped *per shard*,
and per-shard scoping requires a stable per-shard label, which requires the shard to be its
own StatefulSet (you cannot assign per-ordinal labels inside a single STS).

---

## 3. Two distinct goals — do not conflate them

| # | Goal | Robust mechanism | Needs operator (B)? |
|---|------|------------------|---------------------|
| **1** | A shard's master and its replica(s) never share a failure domain (single-domain-loss survivability) | **A** — per-shard-scoped topology spread over the shard's StatefulSet | **No.** Survives failover for free. |
| **2** | Masters distributed across domains (no node/zone holds ≥2 masters; smaller blast radius, balanced write load) | **B** — operator role assignment + post-failover rebalance | **Yes.** K8s cannot do this safely (see §5). |

Goal 1 is the must-have and is fully served by A. Goal 2 is a nice-to-have and is the *only*
thing that justifies B.

---

## 4. Direction A — per-shard StatefulSets

### 4.1 The change

- One StatefulSet per shard: `{name}-shard-0`, `{name}-shard-1`, … each with
  `replicas = 1 + replicasPerShard`.
- Each shard STS stamps a stable identity label on its pods, e.g.
  **`redis.chuck-chuck-chuck.net/shard: "<i>"`**. This label is static (set at STS creation),
  present at scheduling time, and never flaps — unlike a role.
- Per-shard placement is then expressible two equivalent ways:
  - **`matchLabelKeys: ["redis.chuck-chuck-chuck.net/shard"]`** on one uniform
    `topologySpreadConstraint`. The scheduler appends each incoming pod's own shard value to
    the selector, so every pod is spread only against its shard-mates. One spec, correct for
    all shards. **Requires K8s ≥ 1.27** (`matchLabelKeys` for topology spread; GA ~1.30).
  - **Per-STS `labelSelector: {…/shard: "<i>"}`** stamped by the operator into each shard's
    pod template. No version floor; the operator builds each STS anyway.

### 4.2 Why A survives failover — but only with operator shard↔STS pinning (NOT for free)

The spread is defined over a **stable pod set** — the shard's StatefulSet pods — and the
argument works *iff a Redis shard's master + replicas actually stay inside that STS*. If they
do, a master death promotes an already-in-another-domain replica within the set, the recreated
pod is forced back into the vacated domain, and the invariant "this shard's pods occupy distinct
domains" holds through failover.

> **Correction (first e2e run).** That "iff" is not free, as an earlier draft of this note
> assumed. OSS Redis/Valkey Cluster has **no failure-domain awareness** (Enterprise-only; Valkey
> AZ = client read routing), and two topology-blind mechanisms re-pair a shard's master/replica
> **across** StatefulSets: the operator's own empty-master reattach (Step 4) and Redis's
> autonomous *replica migration*. The very first e2e run showed Step 4 scrambling the pairing at
> bootstrap — every replica welded to a different shard's master — so per-shard scheduling was
> pinning the wrong pods. A therefore **requires** the operator to hold the invariant:
> **(1) shard-aware reattach** (`chooseReattachTarget` — attach an empty pod to the
> under-replicated master in *its own* shard STS) and **(2) `cluster-allow-replica-migration no`**
> in the cluster config. With both, role flaps stay *within* the set and the §4.2 argument holds.
> This is a thin slice of Direction B (§5) that A turns out to need. See ADR-007 Decision 6 and
> changelog LR-020.

### 4.3 topologySpreadConstraints span StatefulSets — by design

Spread constraints and podAntiAffinity are **pod-scoped, not workload-scoped**: the scheduler
evaluates them against every pod matching the `labelSelector`, with no notion of which
controller owns them. So across N per-shard STSs we compose two independent constraints:

1. **Per-shard isolation** (Goal 1): scoped to same-shard pods via `matchLabelKeys` or a
   per-STS selector (§4.1).
2. **Cross-shard even distribution** (optional): `labelSelector: {instance, component:
   cluster}`, no shard scoping — the scheduler counts all cluster pods of the instance across
   every shard STS and holds skew ≤ maxSkew. This is placement only; it is *not* Goal 2 (which
   is about *master* balance and needs B).

### 4.4 Proposed API surface

Users cannot easily hand-write the per-shard constraint themselves: it depends on the
operator-owned shard label (and, for `matchLabelKeys`, a cluster-version assumption). So the
common case deserves a first-class knob, with the raw `spec.podTemplate` passthrough remaining
the escape hatch for anything exotic:

```yaml
spec:
  mode: cluster
  cluster:
    shards: 3
    replicasPerShard: 1
  placement:
    shardAntiAffinity:
      # Failure domain to isolate a shard's pods across.
      topologyKey: kubernetes.io/hostname        # or topology.kubernetes.io/zone
      # Hard vs soft. Default soft so small/dev clusters still schedule.
      whenUnsatisfiable: ScheduleAnyway          # or DoNotSchedule
```

The operator translates `placement.shardAntiAffinity` into a per-shard
`topologySpreadConstraint` injected into each shard STS, **merged** with any user-supplied
`spec.podTemplate.topologySpreadConstraints` (merge semantics: open question §7).

Default recommendation: **soft (`ScheduleAnyway`) by default.** Goal 1 is a must-have, but a
`DoNotSchedule` default would leave dev/kind/single-node clusters `Pending` out of the box —
bad DX and a footgun. Soft gives best-effort isolation everywhere and never blocks; production
operators opt into `DoNotSchedule` for a hard guarantee. (This preserves CLAUDE.md pillar 3.5,
minimal interference: we enable, we do not force.)

---

## 5. Direction B — topology-aware role assignment (Goal 2 only)

### 5.1 The false-safety failure A does *not* have, and B fixes

Goal 2 ("no node holds two masters") is **role-keyed**, and role is a flapping property while
scheduling is one-shot (`podAntiAffinity` and `topologySpreadConstraints` are only
`…IgnoredDuringExecution`; there is no execution-time re-enforcement short of the optional
descheduler). Concrete failure — 3 shards × 2, 3 nodes, masters initially one-per-node:

```
N1: A-master  B-replica      N2: B-master  C-replica      N3: C-master  A-replica
```

N2 dies → shard B fails over → **B-replica on N1 is promoted** → N1 now holds A-master **and**
B-master. A "no two masters per node" `podAntiAffinity` is now *violated*, but K8s does not
evict or reschedule the running pods, so you believed masters were spread and one failover
silently undid it. **That** is the false sense of safety — and note it is Goal 2, not Goal 1
(shard A's master/replica are still in distinct domains throughout).

### 5.2 What B is (and is not)

- **B does no placement.** Kubernetes still owns where every pod lives (via A). B only *reads*
  pod→node→zone from the K8s API (CLAUDE.md pillar 3.4, K8s as source of truth) and chooses,
  among the **already-placed** pods of each shard, which is master — via the existing
  `CLUSTER REPLICATE` / manual `CLUSTER FAILOVER` levers.
- **B is bounded by A's placement.** If A placed shard-i's two pods in zoneX and zoneY, B can
  make either the master but can never put shard-i's master in zoneZ. Clean layering: **A owns
  "which domains a shard occupies"; B owns "which occupant is master."**
- **B re-establishes Goal 2 after a failover** rebalances masters (issue a manual failover to
  move mastership to an under-loaded domain). This is the one thing pure scheduling cannot do
  and the entire reason B exists.
- **B is not re-implementing the scheduler.** Redis master/replica is a concept K8s has no
  notion of; assigning it is squarely operator territory (it is already what cluster bootstrap
  does). B only *consumes* topology metadata to inform a role choice — it never evaluates
  placement.

---

## 6. Non-goals / scope boundaries

- **Sentinel mode needs neither A nor B.** Its three data pods are a single StatefulSet;
  spreading all three across domains already guarantees master/replica domain diversity and
  survives failover (pods don't move on failover, only roles do). The existing
  `spec.podTemplate` passthrough covers it — this note is cluster-mode-only.
- **Standalone** is out of scope (single pod, nothing to spread).
- **No persistence changes.** EmptyDir stays; durability remains replication-across-domains,
  which is exactly what A guarantees.

---

## 7. Open questions

1. **Merge semantics** for operator-injected `placement.shardAntiAffinity` vs a user's raw
   `spec.podTemplate.topologySpreadConstraints`: append both, or let a user constraint on the
   same `topologyKey` override? Proposal: append, and document that the operator's per-shard
   constraint always wins for the shard label (users layer *additional* constraints).
2. **matchLabelKeys vs per-STS selector.** `matchLabelKeys` is cleaner but sets a K8s ≥1.27
   floor. Per-STS templated selectors have no floor. Decide whether to require 1.27 or emit
   per-STS selectors (leaning: per-STS selectors, no version floor, since the operator templates
   each STS anyway).
3. **Under-provisioning UX.** With `DoNotSchedule`, too few domains ⇒ `Pending` pods. The
   operator must surface this as a status condition ("wanted 3 domains for shard-1, saw 2"),
   not sit silently Pending. Required for A regardless of B.
4. **Rolling updates under a hard constraint.** `DoNotSchedule` + exactly
   `1+replicasPerShard` domains can wedge a rolling update (a recreated pod has nowhere legal to
   land while the old one drains). Need `maxUnavailable`/partition semantics that respect the
   spread, or accept that hard isolation requires strictly more domains than shard members.
5. **Whether to ship B at all.** Only if Goal 2 (master blast-radius distribution) is an actual
   requirement. Revisit after A lands and is dogfooded.

---

## 8. Relationship to other work

- **LR-018 (cluster consolidated-shard reshard).** A introduces a **stable per-shard identity**
  (the shard label + per-shard STS), which the reshard/scaling work also wants for reasoning
  about shards deterministically. Sequence intentionally: land the reshard work already in
  flight, then do A on top of a stable shard identity rather than inventing shard identity twice.
- **Cross-mode parity (CLAUDE.md §7).** A is cluster-only by nature (sentinel already has its
  single-STS spread), so there is no sibling to fix in lockstep here — but the
  `spec.podTemplate` scheduling passthrough must stay wired on *every* StatefulSet builder
  (a recent gap: the sentinel *monitor* STS silently dropped `TopologySpreadConstraints` +
  `PriorityClassName`; now fixed — keep new builders honest).

---

## 9. Rollout / migration

- **Breaking change.** Restructuring one StatefulSet into N renames the workloads and pods, so
  existing cluster instances are rebuilt on upgrade. Combined with no-persistence (EmptyDir),
  that means **data loss on migration** — reconstructed via re-bootstrap. Acceptable in alpha;
  it is precisely why this belongs in a `0.3.x` alpha bump *before* beta.
- **Call it out loudly** in the changelog and upgrade notes; provide the "drain/repopulate or
  accept clean slate" guidance rather than implying a seamless in-place upgrade.
- When A is committed, record it as an ADR (topology/identity decision) and a
  `RECONCILIATION_ALGORITHM_CHANGELOG.md` entry, and update `API_SPEC.md`, `ARCHITECTURE.md`,
  `USAGE.md`, and CLAUDE.md §4 (cluster mode now = N StatefulSets).
