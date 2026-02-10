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
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	littleredv1alpha1 "github.com/tanne3/littlered-operator/api/v1alpha1"
	redisclient "github.com/tanne3/littlered-operator/internal/redis"
)

// reconcileCluster reconciles cluster mode
func (r *LittleRedReconciler) reconcileCluster(ctx context.Context, littleRed *littleredv1alpha1.LittleRed) (ctrl.Result, error) {
	log := logf.FromContext(ctx)
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
	gt, err := r.gatherGroundTruth(ctx, littleRed)
	if err != nil {
		log.Error(err, "Failed to gather ground truth")
		fast, _ := littleRed.GetRequeueIntervals()
		return ctrl.Result{RequeueAfter: fast}, nil
	}

	// Analyze state
	isHealthy := gt.IsHealthy(expectedReplicas, int32(cluster.Shards))

	// If cluster is not fully healthy or has topology issues, run repair loop
	if !isHealthy || gt.HasPartitions() || gt.HasGhostNodes() || gt.HasOrphanedSlots() || gt.HasEmptyMasters() {
		log.Info("Cluster not healthy or topology issues detected, running repair",
			"partitions", len(gt.Partitions),
			"ghosts", len(gt.GhostNodes),
			"orphanedSlots", gt.HasOrphanedSlots(),
			"emptyMasters", gt.HasEmptyMasters(),
			"masters", gt.CountMasters(),
			"allNodesView", len(gt.AllNodeIDs))
		return r.repairCluster(ctx, littleRed, gt)
	}

	// 5. Cluster is healthy and stable
	return r.updateClusterStatus(ctx, littleRed)
}

// repairCluster handles healing: partitions, ghost nodes, slot restoration, and replication topology
func (r *LittleRedReconciler) repairCluster(ctx context.Context, littleRed *littleredv1alpha1.LittleRed, gt *GroundTruth) (ctrl.Result, error) {
	log := logf.FromContext(ctx)
	fast, _ := littleRed.GetRequeueIntervals()

	password := r.getRedisPassword(ctx, littleRed)
	clusterClient := redisclient.NewClusterClient(password)

	// 0. Quorum Recovery (High Priority)
	// If we have lost quorum (majority of masters), the cluster cannot heal itself.
	// We must manually promote replicas whose masters are gone.
	shards := littleRed.Spec.Cluster.Shards
	votingMasters := gt.CountMasters()

	// Quorum is lost if available voting masters are <= shards / 2
	if votingMasters <= shards/2 {
		log.Info("Quorum loss detected", "votingMasters", votingMasters, "targetShards", shards)

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

				log.Info("Promoting orphan replica during quorum loss",
					"pod", node.PodName,
					"missingMaster", node.MasterNodeID)

				addr := fmt.Sprintf("%s:%d", node.PodIP, littleredv1alpha1.RedisPort)
				if err := clusterClient.ClusterFailoverTakeover(ctx, addr); err != nil {
					log.Error(err, "Failed to force takeover", "pod", node.PodName)
				} else {
					promotedCount++
				}
			}
		}

		if promotedCount > 0 {
			log.Info("Promoted replicas to restore quorum", "count", promotedCount)
			// Wait for cluster state to settle
			return ctrl.Result{RequeueAfter: fast}, nil
		}
	}

	// 1. Heal Partitions (CLUSTER MEET)
	if gt.HasPartitions() {
		// If any replica is orphaned (no master or points to a non-existent node),
		// wait for failover to complete before issuing CLUSTER MEET.
		// Premature MEET breaks failover quorum because the new node can't vote
		// for replica promotion (it doesn't know the master-replica relationship).
		if gt.HasOrphanedReplicas() {
			log.Info("Waiting for failover to complete before healing partition",
				"reason", "orphaned replica detected")
			return ctrl.Result{RequeueAfter: fast}, nil
		}

		log.Info("Healing partitions", "count", len(gt.Partitions))
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
				log.Info("Meeting node", "seed", seedAddr, "target", targetIP)
				_ = clusterClient.ClusterMeet(ctx, seedAddr, targetIP, littleredv1alpha1.RedisPort)
			}
		}
		return ctrl.Result{RequeueAfter: fast}, nil
	}

	// 2. Forget Ghost Nodes
	if gt.HasGhostNodes() {
		log.Info("Removing ghost nodes", "count", len(gt.GhostNodes))
		for _, ghostID := range gt.GhostNodes {
			log.Info("Forgetting ghost node", "id", ghostID)
			for _, node := range gt.Nodes {
				addr := fmt.Sprintf("%s:%d", node.PodIP, littleredv1alpha1.RedisPort)
				if err := clusterClient.ClusterForget(ctx, addr, ghostID); err != nil {
					log.Info("Failed to forget node (might already be gone)", "node", addr, "ghost", ghostID, "error", err)
				}
			}
		}
		return ctrl.Result{RequeueAfter: fast}, nil
	}

	// 3. Restore Missing Slots (0-Replica Mode Only)
	isZeroReplicaMode := false
	if littleRed.Spec.Cluster.ReplicasPerShard != nil && *littleRed.Spec.Cluster.ReplicasPerShard == 0 {
		isZeroReplicaMode = true
	}

	if isZeroReplicaMode && gt.HasOrphanedSlots() {
		if littleRed.Annotations[AnnotationDebugSkipSlotAssignment] != "true" {
			log.Info("Detected orphaned slots in 0-replica mode, attempting restoration")

			emptyMasters := gt.GetEmptyMasters()
			if len(emptyMasters) == 0 {
				log.Info("No empty masters available to take over slots")
				return r.updateClusterStatus(ctx, littleRed)
			}

			shards := littleRed.Spec.Cluster.Shards
			slotRanges := redisclient.GenerateSlotRanges(shards)

			for i := 0; i < shards; i++ {
				podName := fmt.Sprintf("%s-cluster-%d", littleRed.Name, i)
				node, exists := gt.Nodes[podName]

				if exists && len(node.Slots) == 0 {
					targetRange := slotRanges[i]
					log.Info("Restoring slots to empty master", "pod", podName, "slots", fmt.Sprintf("%d-%d", targetRange.Start, targetRange.End))

					addr := fmt.Sprintf("%s:%d", node.PodIP, littleredv1alpha1.RedisPort)
					slots, _ := redisclient.ExpandSlotRange(redisclient.FormatSlotRange(targetRange.Start, targetRange.End))

					if err := clusterClient.ClusterAddSlots(ctx, addr, slots...); err != nil {
						log.Error(err, "Failed to restore slots", "pod", podName)
					}
				}
			}
			return ctrl.Result{RequeueAfter: fast}, nil
		}
	}

	// 4. Replication Repair (Non-Zero Replica Mode)
	if !isZeroReplicaMode {
		emptyMasters := gt.GetEmptyMasters()
		shardsWithReplicas := gt.GetMastersWithReplicas()

		if len(emptyMasters) > 0 {
			log.Info("Detected masters with no slots in replication mode, attempting to assign as replicas")

			expectedReplicas := 1
			if littleRed.Spec.Cluster.ReplicasPerShard != nil {
				expectedReplicas = *littleRed.Spec.Cluster.ReplicasPerShard
			}

			for _, em := range emptyMasters {
				var targetMaster *NodeState
				for _, m := range gt.Nodes {
					if m.Role == "master" && len(m.Slots) > 0 {
						if len(shardsWithReplicas[m.NodeID]) < expectedReplicas {
							targetMaster = m
							break
						}
					}
				}

				if targetMaster != nil {
					log.Info("Assigning empty master as replica", "pod", em.PodName, "masterNodeID", targetMaster.NodeID)
					addr := fmt.Sprintf("%s:%d", em.PodIP, littleredv1alpha1.RedisPort)
					if err := clusterClient.ClusterReplicate(ctx, addr, targetMaster.NodeID); err != nil {
						log.Error(err, "Failed to replicate", "pod", em.PodName)
					} else {
						return ctrl.Result{RequeueAfter: fast}, nil
					}
				}
			}
		}
	}

	if gt.TotalSlots == 0 {
		return r.bootstrapCluster(ctx, littleRed)
	}

	return r.updateClusterStatus(ctx, littleRed)
}

// GroundTruth represents the state of the cluster as seen by the operator
type GroundTruth struct {
	Nodes        map[string]*NodeState // Map PodName -> NodeState
	Partitions   [][]string            // Sets of NodeIDs that see each other
	GhostNodes   []string              // NodeIDs present in cluster but not in K8s
	ClusterState string                // "ok" if ANY node says ok, else "fail" or "unknown"
	TotalSlots   int                   // Max slots assigned reported by any node
	AllNodeIDs   map[string]bool       // Set of all NodeIDs seen in the mesh
}

type NodeState struct {
	PodName      string
	PodIP        string
	NodeID       string
	Slots        []string
	Role         string // "master" or "replica"
	MasterNodeID string // "-" if master
}

func (gt *GroundTruth) IsHealthy(expectedNodes, expectedShards int32) bool {
	if len(gt.Nodes) < int(expectedNodes) {
		return false
	}
	if len(gt.AllNodeIDs) != int(expectedNodes) {
		return false
	}
	if gt.HasPartitions() {
		return false
	}
	if gt.CountMasters() != int(expectedShards) {
		return false
	}
	return gt.ClusterState == "ok" && gt.TotalSlots == 16384
}

func (gt *GroundTruth) HasPartitions() bool {
	return len(gt.Partitions) > 1
}

func (gt *GroundTruth) HasGhostNodes() bool {
	return len(gt.GhostNodes) > 0
}

func (gt *GroundTruth) HasOrphanedSlots() bool {
	return gt.TotalSlots < 16384
}

// HasOrphanedReplicas returns true if any replica has no master or points to a
// node that doesn't exist in our live nodes. This indicates failover is in progress.
func (gt *GroundTruth) HasOrphanedReplicas() bool {
	// Build set of live node IDs
	liveNodeIDs := make(map[string]bool)
	for _, n := range gt.Nodes {
		liveNodeIDs[n.NodeID] = true
	}

	for _, node := range gt.Nodes {
		if node.Role == "replica" {
			// Replica with no master assigned (in failover transition)
			if node.MasterNodeID == "-" || node.MasterNodeID == "" {
				return true
			}
			// Replica pointing to a non-existent master (ghost)
			if !liveNodeIDs[node.MasterNodeID] {
				return true
			}
		}
	}
	return false
}

func (gt *GroundTruth) CountMasters() int {
	count := 0
	for _, n := range gt.Nodes {
		if n.Role == "master" && len(n.Slots) > 0 {
			count++
		}
	}
	return count
}

func (gt *GroundTruth) GetLargestPartitionSeed() *NodeState {
	if len(gt.Partitions) == 0 {
		for _, n := range gt.Nodes {
			return n
		}
		return nil
	}

	maxIdx := 0
	maxLen := 0
	for i, p := range gt.Partitions {
		if len(p) > maxLen {
			maxLen = len(p)
			maxIdx = i
		}
	}

	targetID := gt.Partitions[maxIdx][0]
	for _, n := range gt.Nodes {
		if n.NodeID == targetID {
			return n
		}
	}
	return nil
}

func (gt *GroundTruth) GetEmptyMasters() []*NodeState {
	var empty []*NodeState
	for _, n := range gt.Nodes {
		if n.Role == "master" && len(n.Slots) == 0 {
			empty = append(empty, n)
		}
	}
	return empty
}

// HasEmptyMasters returns true if any node is a master with no slots assigned.
// This indicates a node that should be assigned as a replica.
func (gt *GroundTruth) HasEmptyMasters() bool {
	for _, n := range gt.Nodes {
		if n.Role == "master" && len(n.Slots) == 0 {
			return true
		}
	}
	return false
}

func (gt *GroundTruth) GetMastersWithReplicas() map[string][]string {
	m := make(map[string][]string)
	for _, n := range gt.Nodes {
		if n.Role == "replica" && n.MasterNodeID != "-" {
			m[n.MasterNodeID] = append(m[n.MasterNodeID], n.NodeID)
		}
	}
	return m
}

// gatherGroundTruth queries all pods to build a view of the cluster
func (r *LittleRedReconciler) gatherGroundTruth(ctx context.Context, littleRed *littleredv1alpha1.LittleRed) (*GroundTruth, error) {
	gt := &GroundTruth{
		Nodes:      make(map[string]*NodeState),
		AllNodeIDs: make(map[string]bool),
	}

	cluster := littleRed.Spec.Cluster
	totalNodes := cluster.GetTotalNodes()
	password := r.getRedisPassword(ctx, littleRed)
	clusterClient := redisclient.NewClusterClient(password)

	nodeIDtoPod := make(map[string]string)

	// 1. Gather Pod Identities (IP + MyID)
	for i := 0; i < totalNodes; i++ {
		podName := fmt.Sprintf("%s-cluster-%d", littleRed.Name, i)
		pod := &corev1.Pod{}
		if err := r.Get(ctx, types.NamespacedName{Name: podName, Namespace: littleRed.Namespace}, pod); err != nil {
			continue
		}
		if pod.Status.PodIP == "" {
			continue
		}

		addr := fmt.Sprintf("%s:%d", pod.Status.PodIP, littleredv1alpha1.RedisPort)
		id, err := clusterClient.GetMyID(ctx, addr)
		if err != nil {
			continue
		}

		gt.Nodes[podName] = &NodeState{
			PodName: podName,
			PodIP:   pod.Status.PodIP,
			NodeID:  id,
		}
		nodeIDtoPod[id] = podName
	}

	// 2. Query Topology from ALL reachable nodes
	adj := make(map[string][]string)

	for _, n := range gt.Nodes {
		addr := fmt.Sprintf("%s:%d", n.PodIP, littleredv1alpha1.RedisPort)

		info, err := clusterClient.GetClusterInfo(ctx, addr)
		if err == nil {
			if info.State == "ok" {
				gt.ClusterState = "ok"
			} else if gt.ClusterState == "" || gt.ClusterState == "unknown" {
				gt.ClusterState = info.State
			}
			if info.SlotsAssigned > gt.TotalSlots {
				gt.TotalSlots = info.SlotsAssigned
			}
		}

		nodes, err := clusterClient.GetClusterNodes(ctx, addr)
		if err != nil {
			continue
		}

		var known []string
		for _, knownNode := range nodes {
			gt.AllNodeIDs[knownNode.NodeID] = true

			isFailed := false
			for _, f := range knownNode.Flags {
				if f == "fail" || f == "noaddr" || f == "handshake" {
					isFailed = true
				}
			}

			if !isFailed {
				known = append(known, knownNode.NodeID)
			} else {
				if _, exists := nodeIDtoPod[knownNode.NodeID]; !exists {
					found := false
					for _, ghost := range gt.GhostNodes {
						if ghost == knownNode.NodeID {
							found = true
							break
						}
					}
					if !found {
						gt.GhostNodes = append(gt.GhostNodes, knownNode.NodeID)
					}
				}
			}

			if knownNode.NodeID == n.NodeID {
				n.Slots = knownNode.Slots
				n.Role = "master"
				if knownNode.IsReplica() {
					n.Role = "replica"
				}
				n.MasterNodeID = knownNode.MasterID
			}
		}
		adj[n.NodeID] = known
	}

	if gt.ClusterState == "" {
		gt.ClusterState = "unknown"
	}

	// 3. Compute Partitions
	visited := make(map[string]bool)
	for id := range nodeIDtoPod {
		if visited[id] {
			continue
		}

		var partition []string
		queue := []string{id}
		visited[id] = true

		for len(queue) > 0 {
			curr := queue[0]
			queue = queue[1:]
			partition = append(partition, curr)

			for _, neighbor := range adj[curr] {
				if _, valid := nodeIDtoPod[neighbor]; valid && !visited[neighbor] {
					visited[neighbor] = true
					queue = append(queue, neighbor)
				}
			}
		}
		gt.Partitions = append(gt.Partitions, partition)
	}

	return gt, nil
}

// bootstrapCluster initializes a new Redis Cluster
func (r *LittleRedReconciler) bootstrapCluster(ctx context.Context, littleRed *littleredv1alpha1.LittleRed) (ctrl.Result, error) {
	log := logf.FromContext(ctx)
	log.Info("Bootstrapping/Healing cluster")

	cluster := littleRed.Spec.Cluster
	if cluster == nil {
		cluster = &littleredv1alpha1.ClusterSpec{}
		cluster.SetDefaults()
	}

	password := r.getRedisPassword(ctx, littleRed)
	clusterClient := redisclient.NewClusterClient(password)
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
			log.Error(err, "Failed to get Node ID", "pod", podName)
			return ctrl.Result{RequeueAfter: time.Second}, nil
		}
		nodeIDs[i] = id
	}

	// 1. CLUSTER MEET: Everyone meets Node 0
	seedAddr := fmt.Sprintf("%s:%d", podIPs[0], littleredv1alpha1.RedisPort)
	for i := 1; i < totalNodes; i++ {
		log.Info("Meeting node", "node", i, "target", 0)
		if err := clusterClient.ClusterMeet(ctx, seedAddr, podIPs[i], littleredv1alpha1.RedisPort); err != nil {
			log.Error(err, "Failed to meet node", "node", i)
		}
	}

	// Wait for gossip to propagate slightly
	time.Sleep(2 * time.Second)

	// 2. Assign Slots to Masters
	if littleRed.Annotations[AnnotationDebugSkipSlotAssignment] == "true" {
		log.Info("DEBUG: Skipping slot assignment due to annotation")
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

			log.Info("Assigning slots to master", "node", i, "slots", fmt.Sprintf("%d-%d", slotRanges[i].Start, slotRanges[i].End))
			slots, _ := redisclient.ExpandSlotRange(redisclient.FormatSlotRange(slotRanges[i].Start, slotRanges[i].End))
			if err := clusterClient.ClusterAddSlots(ctx, masterAddr, slots...); err != nil {
				log.Error(err, "Failed to add slots", "node", i)
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
			log.Info("Assigning replica to master", "replicaNode", i, "masterNode", masterIndex)
			if err := clusterClient.ClusterReplicate(ctx, replicaAddr, masterID); err != nil {
				log.Error(err, "Failed to replicate", "replica", i, "master", masterIndex)
			}
		}
	}

	return r.updateClusterStatus(ctx, littleRed)
}

// updateClusterStatus updates the LittleRed status for cluster mode
func (r *LittleRedReconciler) updateClusterStatus(ctx context.Context, littleRed *littleredv1alpha1.LittleRed) (ctrl.Result, error) {
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

	clusterShards := int32(3)
	if littleRed.Spec.Cluster != nil {
		clusterShards = int32(littleRed.Spec.Cluster.Shards)
	}

	// Gather ground truth to get node details for status
	gt, err := r.gatherGroundTruth(ctx, littleRed)
	clusterOK := false
	if err == nil {
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

		meta.SetStatusCondition(&littleRed.Status.Conditions, metav1.Condition{
			Type:               littleredv1alpha1.ConditionReady,
			Status:             metav1.ConditionFalse,
			Reason:             "ClusterNotReady",
			Message:            "Waiting for cluster to be healthy",
			LastTransitionTime: metav1.Now(),
		})
	}

	if err := r.Status().Update(ctx, littleRed); err != nil {
		if apierrors.IsConflict(err) {
			return ctrl.Result{Requeue: true}, nil
		}
		return ctrl.Result{}, err
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
	cm := buildClusterConfigMap(littleRed)
	if err := controllerutil.SetControllerReference(littleRed, cm, r.Scheme); err != nil {
		return err
	}
	return r.createOrUpdate(ctx, cm)
}

// reconcileClusterHeadlessService ensures the headless Service exists
func (r *LittleRedReconciler) reconcileClusterHeadlessService(ctx context.Context, littleRed *littleredv1alpha1.LittleRed) error {
	svc := buildClusterHeadlessService(littleRed)
	if err := controllerutil.SetControllerReference(littleRed, svc, r.Scheme); err != nil {
		return err
	}
	return r.createOrUpdate(ctx, svc)
}

// reconcileClusterStatefulSet ensures the StatefulSet exists
func (r *LittleRedReconciler) reconcileClusterStatefulSet(ctx context.Context, littleRed *littleredv1alpha1.LittleRed) error {
	sts := buildClusterStatefulSet(littleRed)
	if err := controllerutil.SetControllerReference(littleRed, sts, r.Scheme); err != nil {
		return err
	}
	return r.createOrUpdate(ctx, sts)
}

// reconcileClusterClientService ensures the client Service exists

func (r *LittleRedReconciler) reconcileClusterClientService(ctx context.Context, littleRed *littleredv1alpha1.LittleRed) error {

	svc := buildClusterClientService(littleRed)

	if err := controllerutil.SetControllerReference(littleRed, svc, r.Scheme); err != nil {

		return err

	}

	return r.createOrUpdate(ctx, svc)

}
