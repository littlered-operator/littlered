# ADR-008: Cluster Total-/Partial-Wipe Re-Bootstrap (Operator-Driven Pod Recycle)

## Status
Accepted

## Context
The cluster-mode analog of the sentinel leaderless bootstrap deadlock (ADR-005 / LR-015),
reproduced end-to-end. When every cluster pod is lost at once, recovery depends on how the
EmptyDir `/data` is affected — which is what the startup script (`buildClusterRedisContainer`)
branches on via the presence of a surviving `nodes.conf`:

- **Pod-delete wipe** (node-pool recycle, mass eviction): EmptyDir is gone, so every pod
  returns fresh, isolated, with a new node ID and no slots. This **self-heals** through the
  normal repair loop — Step 1 MEETs all nodes into one cluster (seed = largest partition, no
  majority required), Step 3 assigns each missing range to its fresh intended `-shard-K-0`
  master (`SafeMissingShardTarget` accepts a reachable empty master), Step 4 reattaches the
  empty replicas shard-aware — never reaching `bootstrapCluster`.

- **Mass container-crash** (kill-9, OOM storm): the pod, its IP, and its EmptyDir survive, so
  `nodes.conf` survives and each restarted master takes the `RESTART_DETECTED=true` branch:
  STEP 3 yields until a peer confirms it no longer owns slots, and at attempt ≥6 forces
  `CLUSTER FAILOVER TAKEOVER` onto its replica (LR-003, the crash-protection that stops an
  empty restarted master from full-sync-wiping a live promoted replica). With the **whole**
  shard down there is no live replica to take over, `TAKEOVER` cannot resolve it, and the
  master parks (`sleep 3600` → liveness-killed → `CrashLoopBackOff`) forever. The pods never
  become Ready, and `reconcileCluster` gathers/repairs only once `allPodsReady`, so the
  operator is structurally blind — exactly the LR-015 shape (an already-initialized instance
  the operator cannot re-bootstrap).

A crucial asymmetry versus sentinel: a cluster total-wipe leaves **zero** data holders (cluster
data lives only in owned slots; all slots gone ⇒ no data anywhere), so there is **no**
≥2-holder / `allowUnsafeRebootstrap` dilemma to model. The only questions are *availability*
(break the deadlock) and *not regressing the LR-003 crash-protection* in a partial wipe where a
live replica still holds data.

## Decision
Break the deadlock in the **operator**, not the startup script, and leave the script's
conservative yield/park unchanged.

The startup script cannot make this call safely: from inside one parked pod it cannot
distinguish a genuine total wipe from a *temporarily-unreachable* live replica (the STEP-3 park
branch is **not** a clean "no live replica" dichotomy — a real data holder can be transiently
unreachable during the yield window). Deciding "abandon and re-bootstrap" requires the global
view. This is the same conclusion LR-016 reached for sentinel: topology decisions belong to the
operator, never a local probe.

Add `recoverClusterWipeDeadlock`, invoked from `reconcileCluster`'s not-all-Ready branch. It
recycles (deletes) exactly the pods matching the wipe signature; their StatefulSets reschedule
them fresh (clean EmptyDir → new node identity), after which the pod-delete self-heal path
re-bootstraps. The decision is the pure, unit-tested `planClusterWipeRecovery`:

- **Recyclable pod** = its redis container is **not-Ready** *and* has **restarted**
  (crash-looping) *and* the last termination was **not** an OOM kill.
- Gated by a **cooldown** (`clusterWipeRecoveryCooldown` = 120s), tracked in
  `status.cluster.wipeDeadlockSince` (mirrors the sentinel `LeaderlessSince`): first
  observation arms it; within the cooldown, wait; once elapsed and the signature still holds,
  recycle every recyclable pod; when the signature clears, clear the marker.
- A **Ready** pod is **never** recycled.

## Rationale
- **Why the kubelet readiness probe is the safety gate.** In a pure in-memory (EmptyDir)
  cluster, data lives only in the RAM of a *serving* redis. The kubelet's readiness probe is a
  local `PING` next to the container — authoritative and, unlike the operator's remote dial,
  impossible for a blackhole to fool (the LR-017 trap: a live data holder momentarily
  unreachable to the operator is *not* dataless). So "redis container not-Ready + crash-looping"
  authoritatively means redis is down ⇒ the pod holds no data ⇒ deleting it loses nothing, by
  construction. A Ready pod may hold data and is left alone.
- **Why delete the pod rather than have the script self-wipe.** Both produce the identical
  Redis-level result (a stranger with a fresh node ID — identity is the `nodes.conf` ID, not
  the IP). The difference is *who decides, when, and with what knowledge*. The operator restricts
  the action to redis-down/no-data pods, waits out a cooldown longer than the script's own
  ~60s yield (so a pod that would self-recover is never preempted), and never touches a Ready
  data holder — none of which a single parked pod can do.
- **Why the cooldown.** It distinguishes a genuine sustained wipe from a transient blip or a
  rolling update, and gives a temporarily-unreachable node time to reappear and be seen before
  anything is recycled.
- **Why exclude OOM kills.** A distinct failure mode; recycling would not fix an OOM, only
  churn. Data-safe either way, but out of scope for the first cut (extend detection later if
  new stuck-modes appear).

## Consequences
- The mass-container-crash deadlock now self-heals in ~cooldown + reschedule + re-bootstrap
  instead of hanging until a human deletes pods or the CR.
- New spec-less status field `status.cluster.wipeDeadlockSince`; new RBAC verb `delete` on
  `pods`.
- The startup script is unchanged — the LR-003 crash-protection (yield/park) still runs; the
  operator only *adds* a recycle action in a state where pods are already stuck not-Ready and
  (by the RAM-only invariant) hold no data.
- A partial wipe that keeps a surviving replica preserves its data: the Ready survivor is never
  recycled, and the existing repair (Step 0/1 orphan promotion) promotes it. Guarded by a
  dedicated e2e tier.

## Alternatives considered
- **Startup-script self-break (park → start fresh).** Rejected: a local actor making a
  topology decision it cannot make safely (temporarily-unreachable live replica), repeating the
  LR-016 anti-pattern.
- **Re-bootstrap via a bootstrap flag (sentinel-style).** N/A: cluster mode already re-bootstraps
  from a fresh state without a latched flag; the blocker is purely that stuck pods never become
  Ready, which recycling fixes directly.
- **Recycle on the operator's remote reachability instead of kubelet readiness.** Rejected: a
  blackholed but live data holder would be wrongly judged dataless (LR-017).

## References
- CLAUDE.md §3.13, §9
- ADR-005 (sentinel analog), ADR-001 (identity / crash-protection), ADR-007 (per-shard STSs)
- `docs/RECONCILIATION_ALGORITHM_CHANGELOG.md` (LR-023; LR-003 for the protected yield/park,
  LR-015/LR-016 for the sentinel precedent, LR-017 for the blackhole gate)
