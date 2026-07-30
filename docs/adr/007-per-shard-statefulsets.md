# ADR-007: Per-Shard StatefulSets & Stable Shard Identity (Cluster Mode)

## Status
Accepted (0.3.0, Milestone 1 — the structural split). The first-class placement API
(`spec.placement.shardAntiAffinity`) is Milestone 2 and is sketched under "Deferred" below.

## Context

Before 0.3.0, cluster mode was a **single** StatefulSet `{name}-cluster` sized
`replicas = shards × (1+replicasPerShard)`, with a **striped pod-index→shard model**: pods
`0..shards-1` were the shard masters (pod N = shard N), pods `shards..total-1` were replicas
mapped back to master `(i-shards) % shards`. Every pod carried identical labels
(`component=cluster`); there was no per-shard or per-role label anywhere in cluster mode.

The **must-have driver** is resilience against data loss when a single failure domain
(node/zone) is lost. LittleRed is pure in-memory (EmptyDir, pillar 3.1), so a shard's
durability *is* master/replica **domain diversity**: if a shard's master and its only replica
sit on the same node and that node dies, the shard's data and its slots are gone. That
requirement was **not expressible** — you cannot scope a `topologySpreadConstraint` to
"a shard's pods" without a stable per-shard label, and a single StatefulSet cannot carry one.

### Why this is *mandatory*, not merely cleaner

The requirement forces a **stable, schedule-time, per-shard grouping key**: a
`topologySpreadConstraint` only isolates pods it can *select*, and the scheduler evaluates it
at **bind time** and never again (`...IgnoredDuringExecution`). So the shard key must be
per-pod, present *before* scheduling, and stable. A single StatefulSet cannot provide one:

- **One template ⇒ no native shard key.** A StatefulSet has exactly one `spec.template`; every
  pod is stamped from it identically. The only per-pod metadata Kubernetes injects on its own —
  `statefulset.kubernetes.io/pod-name`, `apps.kubernetes.io/pod-index` (the raw ordinal), and
  `controller-revision-hash` — is ordinal/revision identity, **none shard-semantic**. Shard is
  a *function* of the ordinal (`ordinal mod shards`), which K8s neither computes nor accepts;
  `matchLabelKeys` groups by label *equality* only (raw ordinal → wrong grain, not same-shard).
- **Operator-patched labels are too late.** The operator can label a pod only *after* it exists
  — after scheduling. `IgnoredDuringExecution` means the constraint never re-runs, so such a
  label cannot influence placement.
- **The only functioning single-STS variant is a mutating admission webhook** that stamps the
  shard label at admission (before scheduling) by parsing the ordinal — an entire extra
  control-plane component (TLS certs, failure modes, can block all pod creation) whose sole job
  is to reconstruct what per-shard StatefulSets carry declaratively for free.
- **Every no-webhook single-STS alternative fails the requirement, not just does it worse:**
  role-based anti-affinity (role unknown at schedule time + flaps, one-shot), a global even
  spread (a cluster-wide `maxSkew:1` is satisfied by 2 pods/node even when *every* shard is
  co-located), per-pod anti-affinity (forces `#nodes ≥ #pods`, kills bin-packing, still
  one-shot).

So the honest framing is **mandatory**: single-domain-loss survivability *via native
Kubernetes scheduling* is not expressible with one StatefulSet without inventing a bespoke
webhook; the clean alternatives don't achieve it at all.

The choice is **over-determined** by two independent structural wins that don't rest on the
label argument: (1) **shard = the workload unit** — per-shard rolling updates/partition,
per-shard PDB, per-shard scaling, none expressible under one shared StatefulSet; (2) it
**deletes the fragile pod-index→shard decode** — the striped `(i-shards)%shards` assumption is
exactly what produced the LR-018 consolidated-shard bug.

## Decision

### 1. One StatefulSet per shard, stable shard label
Cluster mode builds **N StatefulSets**, `{name}-shard-K` for K in `0..shards-1`, each sized
`1 + replicasPerShard`. Each stamps a static per-shard identity label
`redis.chuck-chuck-chuck.net/shard: "<K>"` on the STS selector, the pod template, and the STS
metadata. The label is set at STS creation, present at scheduling time, and never flaps.

### 2. Master identity is positional-within-shard, not striped
Shard K's intended master is `{name}-shard-K-0` (ordinal 0 within its shard STS); `-1..R` are
its replicas. The pure helper `ClusterPodRefs(name, shards, replicasPerShard)` is the single
source of truth for pod enumeration and master identity, replacing five ad-hoc
`{name}-cluster-N` ordinal loops and the `(i-shards)%shards` formula.

### 3. Shared Services stay shard-agnostic
The one headless Service `{name}-cluster` (selector `component=cluster`) and the client Service
`{name}` are **unchanged**: every shard StatefulSet uses `{name}-cluster` as its governing
`serviceName`, so peer discovery (`getent hosts {name}-cluster`) and pod DNS
`{pod}.{name}-cluster.ns.svc` keep resolving across all shards. Only the shard STS **selectors**
carry the shard label; the Services must not.

### 4. One PodDisruptionBudget per shard
`{name}-shard-K-pdb`, scoped to the shard's pods, created only when `replicasPerShard > 0`
(unchanged redundancy rule). This binds the disruption budget to the shard's failure domain: a
voluntary drain can take at most `maxUnavailable` (default 1) pod of any one shard.

### 5. Never delete data on migration (breaking change)
Renaming one StatefulSet into N rebuilds the workloads; with EmptyDir that is a clean slate.
The operator **does not** auto-delete the old `{name}-cluster` StatefulSet — LittleRed never
deletes data by default. If a pre-0.3.0 single-STS is present, the operator refuses to stand up
the per-shard StatefulSets beside it, surfaces a `LegacyClusterTopology` condition + event, and
waits for an operator to migrate/remove it. Reducing `shards` is likewise refused
(`ShardScaleDownRefused`) — it would orphan shard StatefulSets and drop their slots, and there
is no reshard-away path.

## Consequences
- Single-domain-loss survivability for a shard becomes expressible via a per-shard-scoped
  `topologySpreadConstraint` over the shard's StatefulSet — and survives failover for free,
  because the spread is defined over a stable pod set and role flaps happen *within* it.
- Cluster mode is now N StatefulSets. Status still reports a flat `Nodes` list, now keyed by the
  `{name}-shard-K-N` pod names.
- The reshard/state layers (`PlanReshard`, gatherer, `cluster_state`) are unchanged — they are
  keyed by IP/NodeID/role, never by pod ordinal or STS name.
- Upgrading a pre-0.3.0 cluster in place is not supported; it is a documented clean-slate
  migration (see changelog LR-020 and USAGE upgrade notes).

## Alternatives considered
- **Single STS + mutating webhook to stamp the shard label** — works, but adds a whole
  control-plane component to emulate what per-shard STSs express declaratively. Rejected.
- **Single STS + operator-patched labels / role anti-affinity / global spread** — each fails to
  achieve single-domain-loss survivability (see Context). Rejected.

## Deferred — Direction B (topology-aware role assignment)
Balancing *masters* across domains ("no node holds ≥2 masters") is role-keyed and one-shot
scheduling cannot re-enforce it after a failover; only the operator can (re-choose which
already-placed pod is master). B rides on top of A and is justified only if master
blast-radius distribution becomes a requirement. Revisit after A is dogfooded. See
`docs/PER_SHARD_STATEFULSET_DESIGN.md` §5.
