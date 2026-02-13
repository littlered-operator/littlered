# Cluster Reconciliation

This document describes the reconciliation mechanisms used by the LittleRed operator to maintain Redis Cluster health, handle node failures, and recover from various failure scenarios.

## Overview

The cluster reconciliation loop runs continuously and performs the following high-level steps:

1. Ensure Kubernetes resources exist (ConfigMap, Services, StatefulSet)
2. Wait for all pods to be ready
3. Gather "ground truth" — the actual state of the cluster as seen by querying each node
4. If the cluster is unhealthy, run the repair loop
5. Update status

The repair loop handles these scenarios in order:

| Step | Condition | Action |
|------|-----------|--------|
| 0 | Quorum loss detected | Force-promote orphaned replicas via `CLUSTER FAILOVER TAKEOVER` |
| 1 | Partitions exist | Heal with `CLUSTER MEET` (but wait if failover in progress) |
| 2 | Ghost nodes exist | Remove with `CLUSTER FORGET` |
| 3 | Orphaned slots (0-replica mode) | Restore slots to empty masters |
| 4 | Empty masters (replica mode) | Assign as replicas to masters that need them |

Each step returns early after taking action, allowing the next reconcile loop to re-evaluate the cluster state.

## Ground Truth

Before any repair action, the operator gathers "ground truth" by querying every pod:

- **Node identity**: Each pod's IP and Redis Cluster NodeID
- **Topology**: What nodes each pod can see (`CLUSTER NODES`)
- **Cluster state**: Whether any node reports `cluster_state:ok`
- **Slot coverage**: Total slots assigned across the cluster
- **Partitions**: Groups of nodes that can see each other (computed via graph traversal)
- **Ghost nodes**: NodeIDs that appear in cluster topology but have no corresponding K8s pod (detected via K8s pod list, not gossip flags)
- **Orphaned replicas**: Replicas whose master NodeID doesn't exist in the live node set

## Failure Scenarios

### Scenario 1: Single Master Failure (No Quorum Loss)

**Situation**: One master dies. Kubernetes replaces the pod. The cluster still has a majority of masters alive.

**What happens**:

1. The old master's NodeID becomes a "ghost" (marked FAIL in gossip, no K8s pod)
2. The dead master's replica becomes "orphaned" (still references the ghost as its master)
3. The new pod starts fresh with a new NodeID, isolated from the cluster (a partition)

**Repair sequence**:

```
Reconcile #1:
  - HasPartitions() = true (new pod isolated)
  - HasOrphanedReplicas() = true (replica points to ghost)
  → WAIT (return early, let gossip handle failover)

[Redis gossip promotes replica to master automatically]

Reconcile #2:
  - HasPartitions() = true
  - HasOrphanedReplicas() = false (replica is now a master)
  → CLUSTER MEET (heal partition)

Reconcile #3:
  - HasPartitions() = false
  - HasGhostNodes() = true
  → CLUSTER FORGET (remove ghost)

Reconcile #4:
  - HasEmptyMasters() = true (new pod is master with no slots)
  → CLUSTER REPLICATE (assign new pod as replica)

Reconcile #5:
  - Cluster healthy
```

**Key insight**: The operator must NOT issue `CLUSTER MEET` while a failover is in progress. See [Why We Wait for Gossip-Based Failover](#why-we-wait-for-gossip-based-failover) below.

### Scenario 2: Quorum Loss Without Shard Loss

**Situation**: Majority of masters are lost, but each lost master has a surviving replica. The cluster cannot self-heal because automatic failover requires a majority vote.

**Example**: 3-shard cluster with 1 replica per shard. 2 masters die simultaneously. Only 1 master remains — not enough for a majority vote.

**What happens**:

1. Multiple ghosts appear (the dead masters)
2. Multiple replicas are orphaned
3. `votingMasters <= shards / 2` — quorum is lost

**Repair sequence**:

```
Reconcile #1:
  - votingMasters (1) <= shards/2 (1) → Quorum loss detected
  - For each orphaned replica:
    → CLUSTER FAILOVER TAKEOVER (force-promote without vote)

Reconcile #2:
  - Quorum restored (promoted replicas are now masters)
  - Normal repair continues (MEET, FORGET, etc.)
```

**Key insight**: `CLUSTER FAILOVER TAKEOVER` bypasses the voting mechanism entirely. The replica forcibly takes over its master's slots. This is safe when we know the master is truly gone (no K8s pod exists).

### Scenario 3: Shard Loss (Data Loss)

**Situation**: A master dies AND its replica(s) also die. The slots owned by that shard are lost.

**Current behavior**: The operator detects orphaned slots (`TotalSlots < 16384`) but cannot recover the data. In 0-replica mode, it can reassign slots to empty masters, but the data is lost.

This scenario requires human intervention or restoration from backup.

### Scenario 4: Master Failure in 0-Replica Mode

**Situation**: A master dies in a cluster with no replicas (`replicasPerShard: 0`). There is no replica to promote — the shard's data is lost and its slots must be reassigned to the replacement pod.

**What happens**:

1. The old master's NodeID becomes a ghost (in the mesh, no K8s pod)
2. Kubernetes replaces the pod with a new instance (new NodeID, new IP)
3. The ghost still owns the slots in the cluster's view

**Repair sequence**:

```
Reconcile #1:
  - HasGhostNodes() = true (old NodeID has no K8s pod)
  → CLUSTER FORGET (remove ghost from all live nodes)

Reconcile #2:
  - HasPartitions() = true (new pod is isolated)
  - HasOrphanedReplicas() = false (0-replica mode, no replicas)
  → CLUSTER MEET (heal partition)

Reconcile #3:
  - Missing shard detected (ghost's slots are now unowned)
  - Intended master (pod N for shard N) is available
  → CLUSTER ADDSLOTS (assign slots to intended master)

Reconcile #4:
  - Cluster healthy
```

**Key insight**: In 0-replica mode, there is no failover to wait for. The critical step is detecting and FORGETting the ghost *before* attempting to MEET the new pod or assign slots. If the ghost isn't forgotten first, ADDSLOTS fails with "Slot already busy" because the ghost still owns those slots at a higher configEpoch.

**Design decisions for 0-replica mode**:

- **Strict shard mapping**: Shard N is always assigned to pod N. The operator never assigns a shard to a different master as a "fallback." If pod N isn't ready, it waits.
- **No replication repair**: Step 4 (assigning empty masters as replicas) is skipped entirely when `replicasPerShard == 0`.

## Ghost Detection: Kubernetes as Source of Truth

The operator uses the Kubernetes pod list — not Redis gossip — to determine which nodes are ghosts. Any NodeID present in the Redis cluster mesh that has no corresponding K8s pod is a ghost, regardless of its gossip status.

This is critical because gossip-based failure detection (`FAIL` flag) can lag behind pod deletions by 15 seconds or more (`cluster-node-timeout`). During this window, a "healthy ghost" problem occurs:

1. Pod is deleted — K8s knows it's gone immediately
2. Redis gossip still considers the old NodeID healthy (no FAIL flag yet)
3. The ghost still "owns" its slots in the cluster's view
4. Any attempt to ADDSLOTS for those ranges fails with "Slot already busy"
5. If a new pod is MEETed into the cluster before the ghost is forgotten, the ghost's higher configEpoch wins slot conflicts, and the new pod can be demoted to a replica of the ghost by Redis's internal conflict resolution

By using K8s as the source of truth, the operator can FORGET ghosts immediately — before gossip catches up, and before the new pod joins the cluster.

## Why We Wait for Gossip-Based Failover

### The Problem

When a master dies and Kubernetes replaces the pod:

1. Old master becomes a ghost (NodeID in cluster, marked FAIL, no pod)
2. Replica becomes orphaned (points to ghost)
3. New pod starts isolated — this is detected as a **partition**

The naive fix for a partition is `CLUSTER MEET`. But if we MEET the new pod while failover is in progress, we break the failover:

- Redis Cluster automatic failover requires a **majority vote from masters**
- The newly-joined pod doesn't know about master-replica relationships
- It cannot participate correctly in the vote
- The failover quorum is disrupted, and the replica cannot be promoted

### The Solution

Before issuing `CLUSTER MEET`, the operator checks for orphaned replicas:

```go
if gt.HasPartitions() {
    if gt.HasOrphanedReplicas() {
        log.Info("Waiting for failover to complete before healing partition")
        return ctrl.Result{RequeueAfter: fast}, nil
    }
    // ... proceed with MEET
}
```

This allows Redis gossip to:

1. Propagate the FAIL status to all nodes
2. Trigger the automatic failover election
3. Have masters vote to promote the replica
4. Complete the promotion

Once the replica is promoted to master, `HasOrphanedReplicas()` returns false, and the operator can safely proceed with `CLUSTER MEET` and `CLUSTER FORGET`.

### Why This Also Delays CLUSTER FORGET

The repair loop structure means that ghost removal is implicitly delayed:

```go
// Step 1: Partition healing (returns early if orphaned replicas)
if gt.HasPartitions() {
    if gt.HasOrphanedReplicas() {
        return // ← Never reaches step 2
    }
    // MEET...
    return
}

// Step 2: Ghost removal (only reached after partitions healed)
if gt.HasGhostNodes() {
    // FORGET...
    return
}
```

This is important because `CLUSTER FORGET` would also interfere with failover — the replica needs the ghost's NodeID to exist in the cluster topology to identify which slots it should take over.

## Replication Topology Repair

After a master failure and failover, the cluster may have incorrect replication topology:

- The new pod (replacing the dead master) starts as an empty master
- It should become a replica of the shard that was just promoted

The operator handles this in step 4:

```go
if !isZeroReplicaMode {
    emptyMasters := gt.GetEmptyMasters()  // Masters with no slots
    for _, em := range emptyMasters {
        // Find a master that needs a replica
        targetMaster := findMasterNeedingReplica()
        if targetMaster != nil {
            clusterClient.ClusterReplicate(ctx, em.Addr, targetMaster.NodeID)
        }
    }
}
```

## Health Checks

The cluster is considered healthy when:

- All expected pods are present and queryable
- All nodes see the same set of NodeIDs (no unknown nodes)
- No partitions (all nodes can reach each other)
- Correct number of masters (equals shard count)
- `cluster_state:ok` reported by nodes
- All 16384 slots are assigned

```go
func (gt *GroundTruth) IsHealthy(expectedNodes, expectedShards int32) bool {
    if len(gt.Nodes) < int(expectedNodes) { return false }
    if len(gt.AllNodeIDs) != int(expectedNodes) { return false }
    if gt.HasPartitions() { return false }
    if gt.CountMasters() != int(expectedShards) { return false }
    return gt.ClusterState == "ok" && gt.TotalSlots == 16384
}
```

## Configuration

### Requeue Intervals

The operator uses two requeue intervals:

- **Fast**: Used during repair operations (default: 2 seconds)
- **Steady**: Used when cluster is healthy (default: 30 seconds)

These can be configured via the LittleRed spec.

### Debug Annotations

- `chuck-chuck-chuck.net/debug-skip-slot-assignment`: Skip slot assignment during bootstrap (for testing)

## Summary

| Failure Type | Detection | Recovery Mechanism |
|--------------|-----------|-------------------|
| Single master failure (with replica) | Orphaned replica, ghost node | Wait for gossip failover, then MEET/FORGET/REPLICATE |
| Master failure (0-replica mode) | Ghost node (K8s truth), missing shard | FORGET ghost, MEET new pod, ADDSLOTS to intended master |
| Quorum loss (replicas survive) | `votingMasters <= shards/2` | Force takeover (`CLUSTER FAILOVER TAKEOVER`) |
| Shard loss (no replica) | Orphaned slots, missing shard | Manual intervention required |
| Network partition | Multiple partition groups | `CLUSTER MEET` after failovers complete |
| Ghost nodes | NodeID with no K8s pod | `CLUSTER FORGET` (K8s-based detection, no gossip dependency) |
