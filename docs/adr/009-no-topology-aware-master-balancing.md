# ADR-009: No Topology-Aware Master Balancing (Direction B) — Declined, Open to Challenge

## Status
Accepted — the decision is to **not** build this. Deliberately written as a *challenge
surface*: the reasoning below is a set of falsifiable claims. If your workload breaks one of
them, or you see a churn-free way to achieve the benefit, please open an issue — the
"When to revisit" section lists exactly what evidence would reopen it.

## Context
Cluster mode is one StatefulSet per shard with a stable shard identity (ADR-007). That gave us
**Goal 1**: within a shard, the master and its replica(s) never share a failure domain, so a
single domain loss cannot take both — survivability under EmptyDir. The per-shard placement knob
`spec.placement.shardAntiAffinity` (LR-022) makes Goal 1 usable declaratively.

**Goal 2 / "Direction B" — topology-aware master balancing** — is different and orthogonal:
spread the *masters of different shards* across domains, so they don't pile onto one node. Goal 1
says nothing about it — it spreads each shard's two pods, but where `shard-0-0`, `shard-1-0`,
`shard-2-0` (the masters) land relative to *each other* is uncoordinated, so all masters can
still land on one node (each with its replica elsewhere).

The naive case for balancing them: in Redis Cluster the master owns the writes for its slots, so
"where the masters are" looks like "where the load is." Concentrated masters ⇒ one hot node
(amplified by pillar 3.3: each master is a process with its own io-thread budget, so *N* masters
on a node wants *N×* the CPU you sized for) and a larger correlated-failover blast (losing that
node fails over every shard at once).

## Decision
Do **not** implement active topology-aware master balancing — no operator-driven mastership
relocation, no proactive failback, no attempt to keep masters evenly spread over time. Rely on:

- Goal 1 (per-shard survivability) + best-effort scheduler spread via the existing placement knob;
- Redis's own election on failure (promote the surviving data-complete replica);
- and, for read-heavy deployments, client-side read routing (below), which is where load
  balancing actually happens.

## Rationale
Each point is meant to be individually challengeable.

1. **Reads commonly go to replicas, so the load is already spread — regardless of where masters
   sit.** A common client configuration routes reads — typically the dominant share of traffic —
   to replicas rather than to the master (client libraries expose this directly, e.g. Lettuce's
   `ReadFrom`, and equivalent read-preference options elsewhere). Where reads are served by
   replicas, every pod in a shard already takes load, so balancing masters moves only the
   (usually smaller) write share. And littlered **cannot know or control** the client's read
   routing — it is a client-side decision, out of the operator's hands (the same category as
   Valkey's AZ affinity being client read routing). So a master balancer optimizes for the
   master-routed-reads case, does little for the replica-reads case, and can only *cost* in both.

2. **The common topology (1 replica per shard) has zero balancing freedom.** With one master +
   one replica, a master death has exactly one legal recovery: promote the replica. There is no
   choice to optimize, and "the master drifted to another node" has no cheap remedy — the only
   tool is a *controlled failback failover* (churn). Balancing freedom exists only at
   `replicasPerShard ≥ 2`, and even there EmptyDir forces the choice toward the *data-complete*
   replica and Redis already elects by replication offset; overriding that to chase balance means
   fighting Redis's mechanism and risking data (against pillar 3.5).

3. **There is no schedule-time "I am a master" label, so even *initial* master spread is
   structurally hard.** The master is the `-K-0` ordinal, but a shared per-shard StatefulSet
   template cannot label ordinal 0 distinctly from its replicas, and the operator-assigned role
   label lands *after* scheduling. So "spread all masters across domains" is not expressible as a
   `topologySpreadConstraint` the way Goal 1 is — it is not a cheap extension of the LR-022
   machinery. And whatever initial spread we did achieve would **erode on the first failover**
   (point 2), with no cheap way to restore it.

4. **Active balancing buys throughput/blast-radius at the cost of failover churn — and it does
   not improve uptime.** Every rebalance is a deliberate `CLUSTER FAILOVER` = a write pause for
   that shard. Master balancing does not reduce the *number* of failovers (a master dies → a
   failover happens, wherever it was); the "keep it balanced" flavors *add* controlled failovers.
   So if the goal is fewer failovers / higher uptime, this is the wrong lever — arguably the
   opposite one.

Netting the three framings we considered: **throughput** is the only real steady-state case, and
point 1 guts it for the read-heavy deployments where it would matter most; **blast radius** is a
genuine but modest failure-time benefit, mostly obtainable from *initial* spread alone (point 3
makes even that awkward and point 2 erodes it); **uptime** is not a benefit at all (point 4).

## Consequences
- Masters may concentrate on a node, and drift over time as failovers land promotions wherever
  the surviving replica was. We accept this: for read-routed workloads it barely matters, and for
  write-heavy ones the correlated-failover blast is bounded by Goal 1 already guaranteeing
  survival (a concentrated-node loss is a transient multi-shard write pause, not data loss).
- The escape hatches for a genuinely imbalanced, write-heavy, master-routed deployment are
  client-side (read routing) or operational (size nodes for the worst-case master count), not
  operator-driven.

## Alternatives considered
- **Initial master spread at bootstrap (no ongoing balancing).** The least-bad option, but
  structurally hard (point 3) and self-eroding (point 2) for a benefit that is mostly blast-radius
  and partly moot under replica reads. Not worth the mechanism today.
- **Failover-aware placement (influence which replica/domain wins a failover).** Constrained by
  EmptyDir + offset-based election to the data-complete replica; little real freedom, and what
  freedom exists risks data.
- **Proactive failback (relocate masters back after a domain recovers).** Maximum balance, maximum
  churn — rejected outright (point 4, pillar 3.5).
- **Read-only observability instead of balancing.** Surface master-per-domain distribution in
  status / `lrctl verify` so a human can *see* imbalance and act — no intervention, no churn,
  fits "operator surfaces, human decides." Not built here because it needs cluster-wide `nodes`
  RBAC (the operator reads no node topology today); if we ever add that RBAC for the deferred
  under-provisioning condition, this could ride along. Tracked, not committed.

## When to revisit / how to challenge this
Reopen if any of these is shown:
- A measurement on a real multi-node cluster showing **severe, persistent** master concentration
  (e.g. all masters on one node) that meaningfully hurts a workload **which routes reads to
  masters** (so point 1 does not save it).
- A design that spreads (and keeps spread) masters **without** ongoing operator-triggered
  failovers — i.e. that defeats point 3 declaratively or achieves balance as a free side effect
  of a failover that was going to happen anyway.
- A deployment pattern where `replicasPerShard ≥ 2` is the norm *and* the extra balancing freedom
  (point 2) demonstrably pays for its complexity.

## References
- ADR-007 (per-shard StatefulSets; the Goal 1 / Goal 2 framing)
- CLAUDE.md pillars 3.3 (CPU bounded by threads), 3.5 (minimal interference), 3.12 (per-shard STSs)
- `docs/RECONCILIATION_ALGORITHM_CHANGELOG.md` (LR-022 placement knob)
- `docs/PER_SHARD_STATEFULSET_DESIGN.md` (Direction A/B)
