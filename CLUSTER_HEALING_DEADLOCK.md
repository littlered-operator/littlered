# Cluster Healing Deadlock: Analysis of E2E Test Failure

## Failed Test

```
Cluster Mode Functional Testing
  Failover Recovery (3 Masters, 1 Replica/Shard)
    should reassign replica when replica pod is deleted
```

**File**: `test/e2e/cluster_functional_test.go:327`

## Symptom

After deleting replica pod-3, the test waits for `cluster_state:ok` on pod-0, but pod-0 reports:

```
cluster_state:fail
cluster_slots_assigned:0
cluster_known_nodes:1
cluster_size:0
```

Pod-0 is completely isolated — it knows only itself and has zero slots. The operator logs show it stuck in an infinite loop:

```
Waiting for failover to complete before healing partition  reason: orphaned replica detected
```

This message repeats every 2 seconds for the entire 4-minute test timeout without resolution.

## Timeline (all times UTC)

| Time | Event |
|------|-------|
| 22:22:22 | CR `func-cluster-failover` created (3 masters, 1 replica/shard = 6 pods) |
| 22:22:23 | All 6 pods start, cluster formation begins |
| 22:22:30 | Operator assigns slots to masters (pods 0,1,2), begins replica assignment |
| 22:22:34 | Pod-3 assigned as replica of pod-2 (master, slots 10923-16383) |
| 22:22:36 | Pod-4 assigned as replica of pod-1 (master, slots 5462-10922) |
| 22:22:38 | Pod-5 assigned as replica of pod-0 (master, slots 0-5461). Cluster reaches `cluster_state:ok`. Pod-5 starts initial PSYNC but it **never completes** — the sync fails after 5 seconds. |
| 22:22:40 | Operator confirms cluster healthy, status set to Running |
| ~22:22:42 | **Test 1** (`should promote replica when master pod is deleted`) starts, runs `CLUSTER NODES` on pod-0 |
| 22:22:43 | Test 1 force-deletes pod-0. Test 1's `Eventually` immediately checks `cluster_state:ok` on pod-1 — **passes on first poll** because gossip hasn't propagated the failure yet (cluster-node-timeout = 15s). Test 1 also verifies topology (6 nodes, 3 masters) — also passes from stale cached state. |
| 22:22:44.075 | **Test 2** (`should reassign replica when replica pod is deleted`) starts, force-deletes pod-3 |
| 22:22:43-44 | StatefulSet controller recreates pod-0 and pod-3. Both start **fresh with no cluster config** (new node IDs, empty `nodes.conf`). |
| 22:22:51 | Operator detects broken cluster: `partitions: 3, ghosts: 2, orphanedSlots: false, emptyMasters: true, masters: 2, allNodesView: 8` |
| 22:22:51 | Operator sees orphaned replica (pod-5 still points to pod-0's old node ID which no longer exists). Enters "Waiting for failover" loop. |
| 22:22:51–22:26:44 | Operator stuck in loop. Pod-5 continuously reports: `Currently unable to failover: Disconnected from primary for longer than allowed. Please check the 'cluster-replica-validity-factor' configuration option.` |
| 22:26:44 | Test 2 times out after 4 minutes |

## Root Cause 1: Test 1 Passes Vacuously (Stale State)

Test 1 deletes pod-0 and immediately checks `cluster_state:ok` on pod-1. It passes on the **first poll** because Redis Cluster gossip needs ~15 seconds (`cluster-node-timeout`) to detect and propagate the failure. The test verified the pre-failure state, not actual recovery.

**Evidence**: Test 2 starts at 22:22:44.075 — only ~1 second after test 1 deleted pod-0 at 22:22:43. Test 1's `Eventually` (4 min timeout, 5s interval) would need at least 5 seconds if the first check failed. It passed instantly.

This means the cluster **never actually recovered** from test 1's pod-0 deletion before test 2 started.

## Root Cause 2: Operator Healing Deadlock (No Timeout)

In `internal/controller/cluster_reconcile.go`, the `repairCluster()` function has a guard that blocks partition healing when orphaned replicas exist:

```go
// cluster_reconcile.go:173-183
if gt.HasPartitions() {
    if gt.HasOrphanedReplicas() {
        log.Info("Waiting for failover to complete before healing partition",
            "reason", "orphaned replica detected")
        return ctrl.Result{RequeueAfter: fast}, nil  // ← infinite loop
    }
    // ... CLUSTER MEET to heal partitions (never reached)
}
```

The `HasOrphanedReplicas()` function (`cluster_reconcile.go:443-465`) returns true when a replica points to a master node ID that doesn't exist in the live node set:

```go
func (gt *GroundTruth) HasOrphanedReplicas() bool {
    liveNodeIDs := make(map[string]bool)
    for _, n := range gt.Nodes {
        liveNodeIDs[n.NodeID] = true
    }
    for _, node := range gt.Nodes {
        if node.Role == "replica" {
            if node.MasterNodeID == "-" || node.MasterNodeID == "" {
                return true
            }
            if !liveNodeIDs[node.MasterNodeID] {
                return true  // ← pod-5 hits this: its master (old pod-0) is gone
            }
        }
    }
    return false
}
```

### Why pod-5 can never failover

Pod-5 was assigned as replica of pod-0 at 22:22:38. Its initial PSYNC started but **never completed** — it failed after 5 seconds with "Failed to read response from the server" (pod-0 was about to be killed). Since pod-5 never successfully synced with its master, Valkey's `cluster-replica-validity-factor` check prevents it from performing a failover:

```
Currently unable to failover: Disconnected from primary for longer than allowed.
Please check the 'cluster-replica-validity-factor' configuration option.
```

This is Valkey's safety mechanism: a replica that has been disconnected for too long (or never connected) might have stale data and shouldn't promote itself.

### Why quorum recovery didn't help

The operator's quorum recovery (step 0 in `repairCluster()`) only triggers when `votingMasters <= shards/2`. With 2 of 3 masters still alive (`votingMasters=2, shards/2=1`), quorum was technically maintained. The operator correctly identified this wasn't a quorum loss — but it had no mechanism to handle the case where a replica *can't* failover.

## Cluster State at Failure

| Pod | Status | Cluster Role | Notes |
|-----|--------|-------------|-------|
| pod-0 | Running (fresh, 4m4s) | Isolated master, 0 slots | New node ID, no cluster config, knows only itself |
| pod-1 | Running (original, 4m25s) | Master, slots 5462-10922 | Part of original cluster, sees pod-0's old node as FAIL |
| pod-2 | Running (original, 4m25s) | Master, slots 10923-16383 | Part of original cluster |
| pod-3 | Running (fresh, 4m3s) | Isolated master, 0 slots | New node ID, no cluster config, knows only itself |
| pod-4 | Running (original, 4m25s) | Replica of pod-1 | Healthy |
| pod-5 | Running (original, 4m25s) | Replica of dead pod-0 | Stuck trying to failover, can't due to validity-factor |

The cluster has 3 partitions: {pod-0}, {pod-3}, {pod-1, pod-2, pod-4, pod-5}. The operator detects this but refuses to heal (CLUSTER MEET) because pod-5 is an orphaned replica.

## StatefulSet Observations

- StatefulSet `generation: 1` — the spec was never modified after creation (SSA did **not** cause a rolling update)
- `currentRevision == updateRevision` — no rolling update was in progress
- Pod-0 and pod-3 were killed/recreated by: test 1 (pod-0) and test 2 (pod-3) force-deleting them
- Both pods lost their cluster state because `nodes.conf` is stored in an `emptyDir` volume, not a PVC

## Fixes Needed

### Fix 1: Test — Verify actual recovery, not stale state

Test 1 must not pass on stale gossip state. After deleting a master, the test should:

1. First wait for `cluster_state:fail` to appear (confirming failure propagated through gossip)
2. Then wait for `cluster_state:ok` (confirming actual recovery)
3. Verify that a **new** master exists for the lost shard's slots (not just that 3 masters exist)

### Fix 2: Operator — Add timeout to orphaned replica wait

The operator's "wait for failover" guard needs a timeout. When an orphaned replica can't failover within a reasonable time, the operator should proceed to force-heal:

Possible approaches:
- Track time since first detection of orphaned replica; after N seconds, proceed to heal
- Detect that the replica's master doesn't exist in **any** partition (not just our local view) and skip the wait
- Issue `CLUSTER FAILOVER TAKEOVER` on the orphaned replica after a timeout
- Forget the ghost master node and reassign the orphaned replica to a live master
- Reset the stuck replica's cluster state so it can be re-integrated

The original rationale for the guard is sound: premature `CLUSTER MEET` can break failover quorum because the new node can't vote for replica promotion. But when the failover will **never** complete, the guard becomes a deadlock.

## Debug Artifacts

All artifacts are in:
```
./debug-artifacts-20260212-232644-should-reassign-replica-when-replica-pod-is-deleted/
```

Key files:
- `operator-logs.txt` — Shows the infinite "Waiting for failover" loop from 22:22:51 onward
- `pod-func-cluster-failover-cluster-0-redis.log` — Shows pod-0 starting fresh with "No cluster configuration found"
- `pod-func-cluster-failover-cluster-5-redis.log` — Shows pod-5 stuck on "Currently unable to failover"
- `events.txt` — Shows pod-0 and pod-3 being killed and recreated by StatefulSet controller
- `littlered-crs.yaml` — Shows CR status with stale topology (pre-failure node IDs)
- `statefulsets.yaml` — Confirms generation:1 (no SSA-triggered rolling update)
