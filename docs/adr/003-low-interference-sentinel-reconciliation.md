# ADR-003: Low-Interference Sentinel Reconciliation

## Status
Accepted

## Context
Starting with commit 89ea364, the sentinel reconciliation loop (`reconcileSentinelCluster`) was progressively expanded to actively heal the cluster by issuing SLAVEOF and SENTINEL MONITOR commands to individual Redis and Sentinel nodes. While well-intentioned, this violated the "Minimal Interference" principle (Section 2.5 of LLM_STARTUP.md) and introduced regressions where the operator fought Sentinel's built-in failover mechanisms.

Specific problems observed:
- **SLAVEOF interference**: The operator issued `SLAVEOF` commands to nodes it believed were misconfigured, sometimes racing with Sentinel's own reconfiguration during failovers.
- **MONITOR interference**: The operator re-issued `SENTINEL MONITOR` to sentinels it believed were idle, potentially disrupting ongoing failover state.
- **Offset-based promotion**: `DetermineRealMaster` included a "last resort" step that picked the node with the highest replication offset and treated it as the master. This caused the operator to act on speculative data rather than waiting for Sentinel consensus.

## Decision
Revert the reconciliation behavior to a low-interference approach:

1. **Remove SLAVEOF healing** (Rule C): The operator no longer issues `SLAVEOF` commands to Redis nodes. Sentinel handles all replica reconfiguration.

2. **Narrow MONITOR healing** (Rule B → Rule 0): The operator no longer re-issues `SENTINEL MONITOR` to sentinels that already have a master configured — that was the race-inducing behavior. The sole exception is a sentinel that is reachable but has **no master configured at all** (`Reachable && !Monitoring`). Such a sentinel cannot self-heal via gossip: gossip requires an existing master config to subscribe to the pubsub channel, creating a circular dependency. Issuing `SENTINEL MONITOR` to a blank sentinel is non-disruptive to the rest of the cluster and is the only way to bring it back into the quorum. This targeted form is called **Rule 0** in the code.

3. **Remove offset-based promotion** (Step 5 of `DetermineRealMaster`): The operator no longer picks a "best candidate" master when no clear master exists. It waits for Sentinel to reach consensus.

4. **Keep ghost pruning** (Rule D): `SENTINEL RESET` is still issued when ghost nodes (IPs not belonging to any living pod) are detected in the topology, but only when the sentinel's current master is a valid (non-ghost) IP.

5. **Add terminating guard to label updates**: `updateMasterLabel` skips label updates when any pod is terminating, to avoid label churn during failovers.

6. **Simplify phase check**: `updateSentinelStatus` no longer polls Sentinel for replica count. The phase check uses only StatefulSet readiness and master pod presence.

## Amendment (2026-02-19): zombie-replica self-healing

### Zombie replica problem

A race condition was observed: a new Redis pod starts during a Sentinel failover transition, queries Sentinel and receives a ghost master IP (a pod being force-deleted whose network stack is still partially alive), successfully PINGs that IP in the brief window before the kernel tears down the connection, starts as `REPLICAOF <ghost>`, and then the ghost vanishes. Sentinel cannot self-heal this situation because:
- The zombie never syncs to the real master, so the real master doesn't list it as a replica.
- Sentinel only sends `SLAVEOF` to replicas it knows about (from its topology state), not to zombies pointing at ghost IPs.

Adding a `SLAVEOF` fix in the operator reconciler was evaluated and explicitly rejected: it would violate this ADR by having the operator race with Sentinel's reconfiguration logic.

### Self-healing via liveness and readiness probes

The zombie problem is instead solved at the pod level, without operator involvement:

**Liveness probe** (`buildSentinelLivenessProbe`): The probe inspects `INFO replication` and applies this logic:
1. Bootstrap guard (`/data/bootstrap-in-progress` present) → pass immediately.
2. `role:master` → pass.
3. `master_link_status:up` → pass (replica is synced).
4. `master_link_status:down` but the configured master IP is still reachable via `redis-cli PING` → pass (legitimate failover in progress; Sentinel will redirect the replica).
5. `master_link_status:down` and master IP is unreachable → **fail** (zombie: following a ghost).

The `failureThreshold` is computed from `downAfterMilliseconds + failoverTimeout + 15 s buffer` so that a healthy replica is never killed mid-failover. After the threshold is exceeded, Kubernetes restarts the pod, which runs the startup script fresh and joins the actual master.

**Readiness probe** (`buildSentinelReadinessProbe`): Immediately removes a zombie from service endpoints by failing if `master_link_status` is not `up` (and the node is not master). This is faster than the liveness threshold and ensures clients are not routed to a zombie replica.

## Consequences
- The operator is now a "setup and observe" controller for Sentinel mode: it creates resources, bootstraps Sentinel, prunes ghosts, and updates labels/status.
- Sentinel is the sole authority for failover decisions and replica reconfiguration.
- Recovery from complex failure scenarios (e.g., all pods restarting simultaneously) may take longer, as the operator no longer force-heals the topology. This is acceptable because Sentinel is designed to handle these scenarios.
- Zombie replicas are self-healing: Kubernetes restarts them without operator involvement, within `downAfterMilliseconds + failoverTimeout + buffer` seconds of the ghost master disappearing.
- The `GetHealActions()` method on `SentinelClusterState` remains for CLI diagnostic use but the operator does not act on its recommendations.
