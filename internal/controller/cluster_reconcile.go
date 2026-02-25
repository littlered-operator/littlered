/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package controller

import (
	"context"
	"fmt"
	"reflect"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"

	littleredv1alpha1 "github.com/littlered-operator/littlered-operator/api/v1alpha1"
	redisclient "github.com/littlered-operator/littlered-operator/internal/redis"
)

// reconcileCluster reconciles cluster mode
func (r *LittleRedReconciler) reconcileCluster(ctx context.Context, littleRed *littleredv1alpha1.LittleRed) (ctrl.Result, error) {
	log := r.getLogger(ctx, littleRed, LogCategoryRecon)
	log.Info("Reconciling cluster mode")

	// Set initial phase
	if littleRed.Status.Phase == "" {
		littleRed.Status.Phase = littleredv1alpha1.PhasePending
	}

	// 1. Ensure resources (ConfigMap, Services, StatefulSet)
	if err := r.ensureClusterResources(ctx, littleRed); err != nil {
		log.Error(err, "Failed to ensure cluster resources")
		return ctrl.Result{}, err
	}

	// 2. Get StatefulSet and check if all pods are ready
	sts := &appsv1.StatefulSet{}
	if err := r.Get(ctx, types.NamespacedName{
		Name:      clusterStatefulSetName(littleRed),
		Namespace: littleRed.Namespace,
	}, sts); err != nil {
		if apierrors.IsNotFound(err) {
			log.Info("StatefulSet not yet created, requeueing")
			fast, _ := littleRed.GetRequeueIntervals()
			return ctrl.Result{RequeueAfter: fast}, nil
		}
		return ctrl.Result{}, err
	}

	cluster := littleRed.Spec.Cluster
	if cluster == nil {
		cluster = &littleredv1alpha1.ClusterSpec{}
		cluster.SetDefaults()
	}

	expectedReplicas := int32(cluster.GetTotalNodes())
	allPodsReady := sts.Status.ReadyReplicas == expectedReplicas

	// 3. If not all pods ready, wait (update status to Initializing)
	if !allPodsReady {
		log.Info("Waiting for all pods to be ready",
			"ready", sts.Status.ReadyReplicas,
			"expected", expectedReplicas)

		littleRed.Status.Phase = littleredv1alpha1.PhaseInitializing
		littleRed.Status.Redis.Ready = sts.Status.ReadyReplicas
		littleRed.Status.Redis.Total = *sts.Spec.Replicas

		if err := r.Status().Update(ctx, littleRed); err != nil {
			return ctrl.Result{}, err
		}

		fast, _ := littleRed.GetRequeueIntervals()
		return ctrl.Result{RequeueAfter: fast}, nil
	}

	// 4. All pods are ready. Gather Ground Truth.
	gt := r.gatherGroundTruth(ctx, littleRed)

	// Analyze state
	isHealthy := gt.IsHealthy(expectedReplicas, int32(cluster.Shards))

	// If cluster is not fully healthy or has topology issues, run repair loop
	if !isHealthy || gt.HasPartitions() || gt.HasGhostNodes() || gt.HasOrphanedSlots() || gt.HasEmptyMasters() {
		r.getLogger(ctx, littleRed, LogCategoryState).Info("Cluster not healthy or topology issues detected, running repair",
			"partitions", len(gt.Partitions),
			"ghosts", len(gt.GhostNodes),
			"orphanedSlots", gt.HasOrphanedSlots(),
			"emptyMasters", gt.HasEmptyMasters(),
			"masters", gt.CountMasters(),
			"allNodesView", len(gt.AllNodeIDs))
		return r.repairCluster(ctx, littleRed, gt)
	}

	// 5. Cluster is healthy and stable — pass gt through to avoid a second gather
	return r.updateClusterStatus(ctx, littleRed, gt)
}

// repairCluster handles healing: partitions, ghost nodes, slot restoration, and replication topology
func (r *LittleRedReconciler) repairCluster(ctx context.Context, littleRed *littleredv1alpha1.LittleRed, gt *redisclient.ClusterGroundTruth) (ctrl.Result, error) {
	log := r.getLogger(ctx, littleRed, LogCategoryRecon)
	fast, _ := littleRed.GetRequeueIntervals()

	password := r.getRedisPassword(ctx, littleRed)
	clusterClient := redisclient.NewClusterClient(password, littleRed.Spec.TLS.Enabled)

	stateLog := r.getLogger(ctx, littleRed, LogCategoryState)
	auditLog := r.getLogger(ctx, littleRed, LogCategoryAudit)

	// 0. Quorum Recovery (High Priority)
	// If we have lost quorum (majority of masters), the cluster cannot heal itself.
	// We must manually promote replicas whose masters are gone.
	shards := littleRed.Spec.Cluster.Shards
	votingMasters := gt.CountMasters()

	// Quorum is lost if available voting masters are <= shards / 2
	if votingMasters <= shards/2 {
		stateLog.Info("Quorum loss detected", "votingMasters", votingMasters, "targetShards", shards)

		// Build set of live node IDs for fast lookup
		liveNodes := make(map[string]bool)
		for _, n := range gt.Nodes {
			liveNodes[n.NodeID] = true
		}

		promotedCount := 0
		for _, node := range gt.Nodes {
			if node.Role == "replica" {
				// Skip if master is known/live or if master ID is invalid
				if node.MasterNodeID == "" || node.MasterNodeID == "-" || liveNodes[node.MasterNodeID] {
					continue
				}

				auditLog.Info("Promoting orphan replica during quorum loss",
					"pod", node.PodName,
					"missingMaster", node.MasterNodeID)

				addr := fmt.Sprintf("%s:%d", node.PodIP, littleredv1alpha1.RedisPort)
				if err := clusterClient.ClusterFailoverTakeover(ctx, addr); err != nil {
					auditLog.Error(err, "Failed to force takeover", "pod", node.PodName)
				} else {
					promotedCount++
				}
			}
		}

		if promotedCount > 0 {
			auditLog.Info("Promoted replicas to restore quorum", "count", promotedCount)
			// Wait for cluster state to settle
			return ctrl.Result{RequeueAfter: fast}, nil
		}
	}

	// 1. Heal Partitions (CLUSTER MEET)
	if gt.HasPartitions() {
		// Check for orphaned replicas whose master is a ghost.
		// Allow natural failover for a grace period, then force-promote if stuck.
		gracePeriod := 15 // default
		if littleRed.Spec.Cluster != nil && littleRed.Spec.Cluster.FailoverGracePeriod > 0 {
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
				auditLog.Info("Force-promoting stuck orphan replica",
					"pod", node.PodName, "orphanAge", age, "timeout", orphanTimeout)
				addr := fmt.Sprintf("%s:%d", node.PodIP, littleredv1alpha1.RedisPort)
				if err := clusterClient.ClusterFailoverTakeover(ctx, addr); err != nil {
					auditLog.Error(err, "Failed to force takeover", "pod", node.PodName)
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

		// Only update if changes occurred to avoid unnecessary status updates
		if len(littleRed.Status.Cluster.OrphanedReplicas) != len(currentOrphans) || promotedCount > 0 {
			littleRed.Status.Cluster.OrphanedReplicas = currentOrphans
			if err := r.Status().Update(ctx, littleRed); err != nil {
				if !apierrors.IsConflict(err) {
					return ctrl.Result{}, err
				}
				return ctrl.Result{Requeue: true}, nil
			}
		}

		if promotedCount > 0 || hasBlockingOrphans {
			return ctrl.Result{RequeueAfter: fast}, nil
		}

		auditLog.Info("Healing partitions", "count", len(gt.Partitions))
		seedNode := gt.GetLargestPartitionSeed()
		if seedNode != nil {
			seedAddr := fmt.Sprintf("%s:%d", seedNode.PodIP, littleredv1alpha1.RedisPort)
			for _, node := range gt.Nodes {
				if node.NodeID == seedNode.NodeID {
					continue
				}
				targetIP := node.PodIP
				if targetIP == "" {
					continue
				}
				auditLog.Info("Meeting node", "seed", seedAddr, "target", targetIP)
				_ = clusterClient.ClusterMeet(ctx, seedAddr, targetIP, littleredv1alpha1.RedisPort)
			}
		}
		return ctrl.Result{RequeueAfter: fast}, nil
	}

	// 2. Forget Ghost Nodes (With Safety Check)
	if gt.HasGhostNodes() {
		// Safety: Don't forget a ghost if it is the master of a live replica.
		// We should wait for the replica to be promoted (Step 0) instead.
		protectedMasters := make(map[string]bool)
		for _, n := range gt.Nodes {
			if n.Role == "replica" && n.MasterNodeID != "" && n.MasterNodeID != "-" {
				protectedMasters[n.MasterNodeID] = true
			}
		}

		ghostsToRemove := make([]string, 0)
		for _, ghostID := range gt.GhostNodes {
			if protectedMasters[ghostID] {
				stateLog.Info("Skipping removal of ghost node because it is still a master of a live replica", "ghost", ghostID)
				continue
			}
			ghostsToRemove = append(ghostsToRemove, ghostID)
		}

		if len(ghostsToRemove) > 0 {
			stateLog.Info("Removing ghost nodes", "count", len(ghostsToRemove))
			for _, ghostID := range ghostsToRemove {
				auditLog.Info("Forgetting ghost node", "id", ghostID)
				for _, node := range gt.Nodes {
					addr := fmt.Sprintf("%s:%d", node.PodIP, littleredv1alpha1.RedisPort)
					if err := clusterClient.ClusterForget(ctx, addr, ghostID); err != nil {
						log.Info("Failed to forget node (might already be gone)", "node", addr, "ghost", ghostID, "error", err)
					}
				}
			}
			return ctrl.Result{RequeueAfter: fast}, nil
		}
	}

	// 3. Recover Missing Shards (Strict Shard Validation)
	// We assume we never fragment shards. Verify assignments and restore missing shards.
	expectedRanges := redisclient.GenerateSlotRanges(shards)

	// Map shard index to the NodeID that holds it
	shardOwners := make([]string, shards)

	// Validate current assignments
	for _, node := range gt.Nodes {
		for _, slotStr := range node.Slots {
			start, end, err := redisclient.ParseSlotRange(slotStr)
			if err != nil {
				stateLog.Error(err, "Failed to parse slot range", "node", node.PodName, "range", slotStr)
				continue
			}

			// Check if this range matches exactly one of our expected shards
			matchedShardIdx := -1
			for i, r := range expectedRanges {
				if r.Start == start && r.End == end {
					matchedShardIdx = i
					break
				}
			}

			if matchedShardIdx == -1 {
				// Mismatch! Found a slot range that doesn't align with our shard definition.
				// This implies fragmentation or external manipulation.
				stateLog.Error(nil, "Cluster slot topology mismatch detected. Found fragmented or non-aligned slot range. Refusing to reconcile to avoid data loss.",
					"node", node.PodName,
					"foundRange", fmt.Sprintf("%d-%d", start, end),
					"expectedShards", shards)
				return ctrl.Result{RequeueAfter: fast}, nil // Retry later, maybe transient? Or stuck.
			}

			// Valid range found
			shardOwners[matchedShardIdx] = node.NodeID
		}
	}

	// Check for missing shards
	var missingShardIndices []int
	for i, owner := range shardOwners {
		if owner == "" {
			missingShardIndices = append(missingShardIndices, i)
		}
	}

	if len(missingShardIndices) > 0 {
		stateLog.Info("Detected missing shards", "count", len(missingShardIndices), "indices", missingShardIndices)

		// Find the intended master for each missing shard (strict: pod N owns shard N).
		// Never assign a shard to a different master — that causes split-ownership
		// and "Slot already busy" errors. If the intended master isn't available, wait.
		intendedMasters := make(map[int]*redisclient.ClusterNodeState) // shardIdx -> Node
		for i := 0; i < shards; i++ {
			podName := fmt.Sprintf("%s-cluster-%d", littleRed.Name, i)
			if node, ok := gt.Nodes[podName]; ok && node.Role == "master" {
				intendedMasters[i] = node
			}
		}

		ops := 0
		for _, shardIdx := range missingShardIndices {
			targetNode := intendedMasters[shardIdx]
			if targetNode == nil {
				log.Info("Intended master for shard not available, waiting",
					"shardIdx", shardIdx,
					"expectedPod", fmt.Sprintf("%s-cluster-%d", littleRed.Name, shardIdx))
				continue
			}

			targetRange := expectedRanges[shardIdx]
			addr := fmt.Sprintf("%s:%d", targetNode.PodIP, littleredv1alpha1.RedisPort)

			auditLog.Info("Assigning missing shard to master",
				"shardIdx", shardIdx,
				"range", fmt.Sprintf("%d-%d", targetRange.Start, targetRange.End),
				"target", targetNode.PodName)

			slots, _ := redisclient.ExpandSlotRange(redisclient.FormatSlotRange(targetRange.Start, targetRange.End))
			if err := clusterClient.ClusterAddSlots(ctx, addr, slots...); err != nil {
				auditLog.Error(err, "Failed to assign shard", "shardIdx", shardIdx)
			} else {
				ops++
			}
		}

		if ops > 0 {
			return ctrl.Result{RequeueAfter: fast}, nil
		}
	}

	// 4. Replication Repair (Non-Zero Replica Mode)
	isZeroReplicaMode := false
	if littleRed.Spec.Cluster.ReplicasPerShard != nil && *littleRed.Spec.Cluster.ReplicasPerShard == 0 {
		isZeroReplicaMode = true
	}

	if !isZeroReplicaMode {
		emptyMasters := gt.GetEmptyMasters()
		shardsWithReplicas := gt.GetMastersWithReplicas()

		if len(emptyMasters) > 0 {
			stateLog.Info("Detected masters with no slots in replication mode, attempting to assign as replicas")

			expectedReplicas := 1
			if littleRed.Spec.Cluster.ReplicasPerShard != nil {
				expectedReplicas = *littleRed.Spec.Cluster.ReplicasPerShard
			}

			for _, em := range emptyMasters {
				var targetMaster *redisclient.ClusterNodeState
				for _, m := range gt.Nodes {
					if m.Role == "master" && len(m.Slots) > 0 {
						if len(shardsWithReplicas[m.NodeID]) < expectedReplicas {
							targetMaster = m
							break
						}
					}
				}

				if targetMaster != nil {
					auditLog.Info("Assigning empty master as replica", "pod", em.PodName, "masterNodeID", targetMaster.NodeID)
					addr := fmt.Sprintf("%s:%d", em.PodIP, littleredv1alpha1.RedisPort)
					if err := clusterClient.ClusterReplicate(ctx, addr, targetMaster.NodeID); err != nil {
						auditLog.Error(err, "Failed to replicate", "pod", em.PodName)
					} else {
						return ctrl.Result{RequeueAfter: fast}, nil
					}
				}
			}
		}
	}

	if gt.TotalSlots == 0 {
		// Safety Guard: Only bootstrap if the cluster is truly empty (no slots AND no replicas).
		// If we have replicas, it implies a previous state existed, and we shouldn't overwrite it.
		hasReplicas := false
		for _, n := range gt.Nodes {
			if n.Role == "replica" {
				hasReplicas = true
				break
			}
		}

		if !hasReplicas {
			return r.bootstrapCluster(ctx, littleRed)
		}

		stateLog.Info("Cluster has 0 slots but contains replicas. Refusing to bootstrap to avoid data loss.", "replicas_detected", true)
		// Fall through to update status (will likely show as unhealthy/initializing)
	}

	return r.updateClusterStatus(ctx, littleRed, nil)
}

// gatherGroundTruth queries all pods to build a view of the cluster
func (r *LittleRedReconciler) gatherGroundTruth(ctx context.Context, littleRed *littleredv1alpha1.LittleRed) *redisclient.ClusterGroundTruth {
	cluster := littleRed.Spec.Cluster
	totalNodes := cluster.GetTotalNodes()
	password := r.getRedisPassword(ctx, littleRed)

	clusterPods := make(map[string]string)
	for i := 0; i < totalNodes; i++ {
		podName := fmt.Sprintf("%s-cluster-%d", littleRed.Name, i)
		pod := &corev1.Pod{}
		if err := r.Get(ctx, types.NamespacedName{Name: podName, Namespace: littleRed.Namespace}, pod); err == nil && pod.Status.PodIP != "" {
			clusterPods[pod.Status.PodIP] = podName
		}
	}

	g := &operatorGatherer{password: password, tlsEnabled: littleRed.Spec.TLS.Enabled}
	return redisclient.GatherClusterGroundTruth(ctx, g, clusterPods)
}

// bootstrapCluster initializes a new Redis Cluster
func (r *LittleRedReconciler) bootstrapCluster(ctx context.Context, littleRed *littleredv1alpha1.LittleRed) (ctrl.Result, error) {
	log := r.getLogger(ctx, littleRed, LogCategoryRecon)
	auditLog := r.getLogger(ctx, littleRed, LogCategoryAudit)
	auditLog.Info("Bootstrapping/Healing cluster")

	cluster := littleRed.Spec.Cluster
	if cluster == nil {
		cluster = &littleredv1alpha1.ClusterSpec{}
		cluster.SetDefaults()
	}

	password := r.getRedisPassword(ctx, littleRed)
	clusterClient := redisclient.NewClusterClient(password, littleRed.Spec.TLS.Enabled)
	totalNodes := cluster.GetTotalNodes()

	// Gather all Pod IPs and Node IDs
	podIPs := make([]string, totalNodes)
	nodeIDs := make([]string, totalNodes)

	for i := 0; i < totalNodes; i++ {
		podName := fmt.Sprintf("%s-cluster-%d", littleRed.Name, i)
		pod := &corev1.Pod{}
		if err := r.Get(ctx, types.NamespacedName{Name: podName, Namespace: littleRed.Namespace}, pod); err != nil {
			return ctrl.Result{}, err
		}
		if pod.Status.PodIP == "" {
			return ctrl.Result{RequeueAfter: time.Second}, nil
		}
		podIPs[i] = pod.Status.PodIP

		addr := fmt.Sprintf("%s:%d", pod.Status.PodIP, littleredv1alpha1.RedisPort)
		id, err := clusterClient.GetMyID(ctx, addr)
		if err != nil {
			auditLog.Error(err, "Failed to get Node ID", "pod", podName)
			return ctrl.Result{RequeueAfter: time.Second}, nil
		}
		nodeIDs[i] = id
	}

	// 1. CLUSTER MEET: Everyone meets Node 0
	seedAddr := fmt.Sprintf("%s:%d", podIPs[0], littleredv1alpha1.RedisPort)
	for i := 1; i < totalNodes; i++ {
		auditLog.Info("Meeting node", "node", i, "target", 0)
		if err := clusterClient.ClusterMeet(ctx, seedAddr, podIPs[i], littleredv1alpha1.RedisPort); err != nil {
			auditLog.Error(err, "Failed to meet node", "node", i)
		}
	}

	// Wait for gossip to propagate slightly
	time.Sleep(2 * time.Second)

	// 2. Assign Slots to Masters
	if littleRed.Annotations[AnnotationDebugSkipSlotAssignment] == "true" {
		auditLog.Info("DEBUG: Skipping slot assignment due to annotation")
	} else {
		slotRanges := redisclient.GenerateSlotRanges(cluster.Shards)

		for i := 0; i < cluster.Shards; i++ {
			masterAddr := fmt.Sprintf("%s:%d", podIPs[i], littleredv1alpha1.RedisPort)

			nodes, err := clusterClient.GetClusterNodes(ctx, masterAddr)
			if err == nil {
				hasSlots := false
				for _, n := range nodes {
					if n.NodeID == nodeIDs[i] && len(n.Slots) > 0 {
						hasSlots = true
						break
					}
				}
				if hasSlots {
					log.Info("Node already has slots, skipping assignment", "node", i)
					continue
				}
			}

			auditLog.Info("Assigning slots to master", "node", i, "slots", fmt.Sprintf("%d-%d", slotRanges[i].Start, slotRanges[i].End))
			slots, _ := redisclient.ExpandSlotRange(redisclient.FormatSlotRange(slotRanges[i].Start, slotRanges[i].End))
			if err := clusterClient.ClusterAddSlots(ctx, masterAddr, slots...); err != nil {
				auditLog.Error(err, "Failed to add slots", "node", i)
			}
		}
	}

	// 3. Assign Replicas
	for i := cluster.Shards; i < totalNodes; i++ {
		masterIndex := (i - cluster.Shards) % cluster.Shards
		masterID := nodeIDs[masterIndex]

		replicaAddr := fmt.Sprintf("%s:%d", podIPs[i], littleredv1alpha1.RedisPort)

		nodes, err := clusterClient.GetClusterNodes(ctx, replicaAddr)
		alreadyCorrect := false
		if err == nil {
			for _, n := range nodes {
				if n.NodeID == nodeIDs[i] && n.MasterID == masterID {
					alreadyCorrect = true
					break
				}
			}
		}

		if !alreadyCorrect {
			auditLog.Info("Assigning replica to master", "replicaNode", i, "masterNode", masterIndex)
			if err := clusterClient.ClusterReplicate(ctx, replicaAddr, masterID); err != nil {
				auditLog.Error(err, "Failed to replicate", "replica", i, "master", masterIndex)
			}
		}
	}

	return r.updateClusterStatus(ctx, littleRed, nil)
}

// updateClusterStatus updates the LittleRed status for cluster mode.
// gt may be passed in from the caller to avoid a redundant gather; pass nil to gather fresh.
func (r *LittleRedReconciler) updateClusterStatus(ctx context.Context, littleRed *littleredv1alpha1.LittleRed, gt *redisclient.ClusterGroundTruth) (ctrl.Result, error) {
	log := r.getLogger(ctx, littleRed, LogCategoryRecon)
	oldStatus := littleRed.Status.DeepCopy()
	// Get StatefulSet status
	sts := &appsv1.StatefulSet{}
	if err := r.Get(ctx, types.NamespacedName{
		Name:      clusterStatefulSetName(littleRed),
		Namespace: littleRed.Namespace,
	}, sts); err != nil {
		return ctrl.Result{}, err
	}

	littleRed.Status.Redis.Ready = sts.Status.ReadyReplicas
	littleRed.Status.Redis.Total = *sts.Spec.Replicas

	clusterShards := int32(littleredv1alpha1.DefaultClusterShards)
	if littleRed.Spec.Cluster != nil {
		clusterShards = int32(littleRed.Spec.Cluster.Shards)
	}

	// Gather ground truth to get node details for status.
	// Reuse the caller's gt if provided (happy path); gather fresh otherwise (post-repair).
	if gt == nil {
		gt = r.gatherGroundTruth(ctx, littleRed)
	}
	clusterOK := false
	if gt != nil {
		if littleRed.Status.Cluster == nil {
			littleRed.Status.Cluster = &littleredv1alpha1.ClusterStatusInfo{}
		}
		littleRed.Status.Cluster.State = gt.ClusterState
		clusterOK = gt.IsHealthy(littleRed.Status.Redis.Total, clusterShards)

		// Populate node details
		nodeStates := make([]littleredv1alpha1.ClusterNodeState, 0)
		for i := 0; i < int(*sts.Spec.Replicas); i++ {
			podName := fmt.Sprintf("%s-cluster-%d", littleRed.Name, i)
			if node, ok := gt.Nodes[podName]; ok {
				nodeStates = append(nodeStates, littleredv1alpha1.ClusterNodeState{
					PodName:      podName,
					NodeID:       node.NodeID,
					Role:         node.Role,
					MasterNodeID: node.MasterNodeID,
					SlotRanges:   strings.Join(node.Slots, ","),
				})
			}
		}
		littleRed.Status.Cluster.Nodes = nodeStates
	}

	// Determine high level phase
	if littleRed.Status.Redis.Ready == littleRed.Status.Redis.Total && clusterOK {
		littleRed.Status.Phase = littleredv1alpha1.PhaseRunning
		littleRed.Status.Status = "Ready"

		meta.SetStatusCondition(&littleRed.Status.Conditions, metav1.Condition{
			Type:               littleredv1alpha1.ConditionReady,
			Status:             metav1.ConditionTrue,
			Reason:             "ClusterHealthy",
			Message:            "All pods ready and cluster state is ok",
			LastTransitionTime: metav1.Now(),
		})
	} else {
		if littleRed.Status.Phase != littleredv1alpha1.PhasePending {
			littleRed.Status.Phase = littleredv1alpha1.PhaseInitializing
		}
		littleRed.Status.Status = "Initializing"

		log.Info("Not yet Running, requeueing",
			"redis", fmt.Sprintf("%d/%d", littleRed.Status.Redis.Ready, littleRed.Status.Redis.Total),
			"clusterHealthy", clusterOK)

		meta.SetStatusCondition(&littleRed.Status.Conditions, metav1.Condition{
			Type:               littleredv1alpha1.ConditionReady,
			Status:             metav1.ConditionFalse,
			Reason:             "ClusterNotReady",
			Message:            "Waiting for cluster to be healthy",
			LastTransitionTime: metav1.Now(),
		})
	}

	// Update status if changed
	if !reflect.DeepEqual(oldStatus, &littleRed.Status) {
		if err := r.Status().Update(ctx, littleRed); err != nil {
			if apierrors.IsConflict(err) {
				return ctrl.Result{Requeue: true}, nil
			}
			return ctrl.Result{}, err
		}
	}

	fast, steady := littleRed.GetRequeueIntervals()
	if littleRed.Status.Phase == littleredv1alpha1.PhaseRunning {
		return ctrl.Result{RequeueAfter: steady}, nil
	}
	return ctrl.Result{RequeueAfter: fast}, nil
}

// ensureClusterResources creates/updates all resources for cluster mode
func (r *LittleRedReconciler) ensureClusterResources(ctx context.Context, littleRed *littleredv1alpha1.LittleRed) error {
	if err := r.reconcileClusterConfigMap(ctx, littleRed); err != nil {
		return err
	}
	if err := r.reconcileClusterHeadlessService(ctx, littleRed); err != nil {
		return err
	}
	if err := r.reconcileClusterStatefulSet(ctx, littleRed); err != nil {
		return err
	}
	if err := r.reconcileClusterClientService(ctx, littleRed); err != nil {
		return err
	}

	return nil
}

// reconcileClusterConfigMap ensures the ConfigMap exists
func (r *LittleRedReconciler) reconcileClusterConfigMap(ctx context.Context, littleRed *littleredv1alpha1.LittleRed) error {
	return r.apply(ctx, littleRed, buildClusterConfigMap(littleRed))
}

// reconcileClusterHeadlessService ensures the headless Service exists
func (r *LittleRedReconciler) reconcileClusterHeadlessService(ctx context.Context, littleRed *littleredv1alpha1.LittleRed) error {
	return r.apply(ctx, littleRed, buildClusterHeadlessService(littleRed))
}

// reconcileClusterStatefulSet ensures the StatefulSet exists
func (r *LittleRedReconciler) reconcileClusterStatefulSet(ctx context.Context, littleRed *littleredv1alpha1.LittleRed) error {
	return r.apply(ctx, littleRed, buildClusterStatefulSet(littleRed))
}

// reconcileClusterClientService ensures the client Service exists
func (r *LittleRedReconciler) reconcileClusterClientService(ctx context.Context, littleRed *littleredv1alpha1.LittleRed) error {
	return r.apply(ctx, littleRed, buildClusterClientService(littleRed))
}
