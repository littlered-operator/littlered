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

2. **Remove MONITOR healing** (Rule B): The operator no longer issues `SENTINEL MONITOR` to individual sentinels. Bootstrap handles initial MONITOR, and Sentinel's built-in gossip handles the rest.

3. **Remove offset-based promotion** (Step 5 of `DetermineRealMaster`): The operator no longer picks a "best candidate" master when no clear master exists. It waits for Sentinel to reach consensus.

4. **Keep ghost pruning** (Rule D): `SENTINEL RESET` is still issued when ghost nodes (IPs not belonging to any living pod) are detected in the topology, but only when the sentinel's current master is a valid (non-ghost) IP.

5. **Add terminating guard to label updates**: `updateMasterLabel` skips label updates when any pod is terminating, to avoid label churn during failovers.

6. **Simplify phase check**: `updateSentinelStatus` no longer polls Sentinel for replica count. The phase check uses only StatefulSet readiness and master pod presence.

## Consequences
- The operator is now a "setup and observe" controller for Sentinel mode: it creates resources, bootstraps Sentinel, prunes ghosts, and updates labels/status.
- Sentinel is the sole authority for failover decisions and replica reconfiguration.
- Recovery from complex failure scenarios (e.g., all pods restarting simultaneously) may take longer, as the operator no longer force-heals the topology. This is acceptable because Sentinel is designed to handle these scenarios.
- The `GetHealActions()` method on `SentinelClusterState` remains for CLI diagnostic use but the operator does not act on its recommendations.
