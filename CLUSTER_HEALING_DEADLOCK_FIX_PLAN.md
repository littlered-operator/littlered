# Plan: Fix Cluster Healing Deadlock + E2E Test False Positives

## Context

E2E test `should reassign replica when replica pod is deleted` fails because:

1. **Operator bug**: `repairCluster()` in `cluster_reconcile.go:173-183` waits indefinitely when `HasOrphanedReplicas()` is true but the replica can't auto-failover. This happens when the master was killed before the replica finished initial PSYNC — Valkey's `cluster-replica-validity-factor` blocks the election permanently.

2. **Test bug**: Multiple cluster-mode tests check `cluster_state:ok` immediately after killing a pod. The check passes on stale gossip (cluster-node-timeout is 15s) before the failure propagates, giving false positives.

Full analysis in `CLUSTER_HEALING_DEADLOCK.md`.

## Fix 1: Operator — Timeout-based force-promotion of stuck orphaned replicas

### Problem

In `repairCluster()`, step 0 (quorum recovery) only promotes orphaned replicas when `votingMasters <= shards/2`. Step 1 (partition healing) blocks on `HasOrphanedReplicas()` indefinitely.

When only 1 of 3 masters is lost, `votingMasters=2 > shards/2=1`, so quorum recovery doesn't trigger. But the orphaned replica can't auto-failover if it never synced with its master. Deadlock.

### Approach: Timeout, not unconditional promotion

**Key insight**: The happy path (master dies after replica has synced) works fine — Valkey's gossip-based failover promotes the replica automatically within ~15-20s. We only need to intervene when the failover is **stuck** (replica never synced, validity-factor blocks it).

**Solution**: Track per-replica orphan detection timestamps in the CR status. Allow natural failover for a grace period (cluster-node-timeout + failoverGracePeriod). If the orphan persists beyond the timeout, force-promote via `CLUSTER FAILOVER TAKEOVER`.

### Configurable grace period in ClusterSpec

Add a `failoverGracePeriod` field to `ClusterSpec` in `api/v1alpha1/littlered_types.go`:

```go
type ClusterSpec struct {
    // ... existing fields ...

    // FailoverGracePeriod is additional time (in seconds) beyond cluster-node-timeout
    // to wait for natural gossip-based failover before the operator force-promotes
    // a stuck orphaned replica. Default: 15 seconds.
    // Total timeout = clusterNodeTimeout + failoverGracePeriod.
    // +kubebuilder:default=15
    // +optional
    FailoverGracePeriod int `json:"failoverGracePeriod,omitempty"`
}
```

The total orphan timeout = `clusterNodeTimeout` (ms) + `failoverGracePeriod` (seconds).

### Data model: Per-replica tracking in CR status

Add to `ClusterStatusInfo` and new type in `api/v1alpha1/littlered_types.go`:

```go
type ClusterStatusInfo struct {
    State            string                `json:"state,omitempty"`
    Nodes            []ClusterNodeState    `json:"nodes,omitempty"`
    LastBootstrap    *metav1.Time          `json:"lastBootstrap,omitempty"`
    OrphanedReplicas []OrphanedReplicaInfo `json:"orphanedReplicas,omitempty"` // NEW
}

type OrphanedReplicaInfo struct {
    PodName      string      `json:"podName"`
    NodeID       string      `json:"nodeId"`
    MasterNodeID string      `json:"masterNodeId"`
    DetectedAt   metav1.Time `json:"detectedAt"`
}
```

This tracks each orphan independently — handles multi-replica failures correctly. State lives in the CR (operator stays stateless), is observable via `kubectl`, and survives operator restarts.

### Changes in `internal/controller/cluster_reconcile.go`

Replace the unconditional `HasOrphanedReplicas()` block at step 1 (lines 173-183):

```go
// In the partition-healing section (step 1), replace the HasOrphanedReplicas guard:

if gt.HasPartitions() {
    // Check for orphaned replicas whose master is a ghost.
    // Allow natural failover for a grace period, then force-promote if stuck.
    gracePeriod := 15 // default
    if littleRed.Spec.Cluster.FailoverGracePeriod > 0 {
        gracePeriod = littleRed.Spec.Cluster.FailoverGracePeriod
    }
    orphanTimeout := time.Duration(littleRed.Spec.Cluster.ClusterNodeTimeout)*time.Millisecond +
        time.Duration(gracePeriod)*time.Second

    // Build lookup sets
    liveNodes := make(map[string]bool)
    for _, n := range gt.Nodes {
        liveNodes[n.NodeID] = true
    }
    ghostSet := make(map[string]bool)
    for _, g := range gt.GhostNodes {
        ghostSet[g] = true
    }

    // Reconcile orphan tracking: detect new orphans, check timeouts on existing ones
    now := metav1.Now()
    existingOrphans := make(map[string]*littleredv1alpha1.OrphanedReplicaInfo)
    if littleRed.Status.Cluster != nil {
        for i := range littleRed.Status.Cluster.OrphanedReplicas {
            o := &littleRed.Status.Cluster.OrphanedReplicas[i]
            existingOrphans[o.PodName] = o
        }
    }

    var currentOrphans []littleredv1alpha1.OrphanedReplicaInfo
    hasBlockingOrphans := false
    promotedCount := 0

    for _, node := range gt.Nodes {
        if node.Role != "replica" {
            continue
        }
        if node.MasterNodeID == "" || node.MasterNodeID == "-" || liveNodes[node.MasterNodeID] {
            continue
        }
        if !ghostSet[node.MasterNodeID] {
            continue // Master unknown — might be in transition
        }

        // This is an orphaned replica whose master is a ghost
        orphanInfo, tracked := existingOrphans[node.PodName]
        if !tracked {
            // New orphan — start tracking
            orphanInfo = &littleredv1alpha1.OrphanedReplicaInfo{
                PodName:      node.PodName,
                NodeID:       node.NodeID,
                MasterNodeID: node.MasterNodeID,
                DetectedAt:   now,
            }
        }

        age := now.Time.Sub(orphanInfo.DetectedAt.Time)
        if age >= orphanTimeout {
            // Timeout exceeded — force-promote
            log.Info("Force-promoting stuck orphan replica",
                "pod", node.PodName, "orphanAge", age, "timeout", orphanTimeout)
            addr := fmt.Sprintf("%s:%d", node.PodIP, littleredv1alpha1.RedisPort)
            if err := clusterClient.ClusterFailoverTakeover(ctx, addr); err != nil {
                log.Error(err, "Failed to force takeover", "pod", node.PodName)
            } else {
                promotedCount++
            }
        } else {
            // Still within grace period — track and wait
            log.Info("Waiting for natural failover",
                "pod", node.PodName, "orphanAge", age, "timeout", orphanTimeout)
            currentOrphans = append(currentOrphans, *orphanInfo)
            hasBlockingOrphans = true
        }
    }

    // Persist orphan tracking (removes resolved orphans, adds new ones)
    if littleRed.Status.Cluster == nil {
        littleRed.Status.Cluster = &littleredv1alpha1.ClusterStatusInfo{}
    }
    littleRed.Status.Cluster.OrphanedReplicas = currentOrphans
    if err := r.Status().Update(ctx, littleRed); err != nil {
        if !apierrors.IsConflict(err) {
            return ctrl.Result{}, err
        }
    }

    if promotedCount > 0 {
        return ctrl.Result{RequeueAfter: fast}, nil
    }
    if hasBlockingOrphans {
        return ctrl.Result{RequeueAfter: fast}, nil
    }

    // No orphans blocking — proceed to heal partitions
    // ... existing CLUSTER MEET code ...
}
```

Keep the existing quorum-loss step 0 (lines 129-171) as-is — it handles the catastrophic case where majority of masters are lost and immediate TAKEOVER is warranted regardless of timeout.

### Files to modify

- `api/v1alpha1/littlered_types.go` — Add `FailoverGracePeriod` to `ClusterSpec`, add `OrphanedReplicaInfo` type and field to `ClusterStatusInfo`
- `api/v1alpha1/littlered_defaults.go` — Add default for `FailoverGracePeriod` in `SetDefaults()`
- `internal/controller/cluster_reconcile.go` — Replace `HasOrphanedReplicas()` guard with timeout-based logic
- Run `make generate` and `make manifests` — Regenerate deepcopy and CRD manifests

## Fix 2: E2E Tests — Wait for failure before checking recovery

### Problem

Tests check `cluster_state:ok` immediately after pod kill. With 15s gossip propagation delay, the first poll sees stale pre-failure state.

### Affected tests

| Test | File:Line | Risk |
|------|-----------|------|
| 0-replica healing | cluster_functional_test.go:220 | Critical |
| promote replica | cluster_functional_test.go:292 | Critical |
| reassign replica | cluster_functional_test.go:335 | Critical |
| multi-pod random chaos | cluster_chaos_test.go:423 | Critical |

### Approach

Add a helper function that waits for the cluster to detect the failure first:

```go
// waitForClusterFailureDetected waits for at least one node to report cluster_state:fail,
// confirming that gossip has propagated the failure. This prevents false-positive recovery
// checks against stale pre-failure state.
func waitForClusterFailureDetected(namespace, crName string, queryPod string) {
    Eventually(func(g Gomega) {
        cmd := exec.Command("kubectl", "exec", queryPod,
            "-n", namespace, "-c", "redis", "--",
            "valkey-cli", "CLUSTER", "INFO")
        output, err := utils.Run(cmd)
        g.Expect(err).NotTo(HaveOccurred())
        g.Expect(output).To(ContainSubstring("cluster_state:fail"))
    }, 30*time.Second, 2*time.Second).Should(Succeed(),
        "Cluster should detect failure within 30s (cluster-node-timeout=15s)")
}
```

Then in each affected test, call this between the pod kill and the recovery check.

**Note**: For the 0-replica mode test, `cluster_state:fail` might not always appear if the remaining masters don't cover all slots. In that case, check `cluster_known_nodes` decreased instead, or use `time.Sleep(20*time.Second)` to wait past the gossip window.

### Files to modify

- `test/e2e/cluster_functional_test.go` — Add failure detection to 3 tests
- `test/e2e/cluster_chaos_test.go` — Add failure detection to multi-pod chaos test
- `test/e2e/cluster_utils_test.go` — Add `waitForClusterFailureDetected` helper

## Verification

1. `go build ./...` — compiles
2. `go vet ./...` — no issues
3. `make generate && make manifests` — CRD and deepcopy regenerated
4. `go test ./internal/controller/ -count=1` — unit tests pass
5. Run the failing test: `SKIP_OPERATOR_DEPLOY=true TEST_NAMESPACE=e2e DEBUG_ON_FAILURE=true make test-e2e FOCUS="should reassign replica"`
6. Run full cluster functional tests: `SKIP_OPERATOR_DEPLOY=true TEST_NAMESPACE=e2e make test-e2e FOCUS="Cluster Mode Functional"`
