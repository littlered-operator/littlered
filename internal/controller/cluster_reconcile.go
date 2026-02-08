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
	"sigs.k8s.io/controller-runtime/pkg/client"
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
			return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
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

	// 3. If not all pods ready, update status and wait
	if !allPodsReady {
		log.Info("Waiting for all pods to be ready",
			"ready", sts.Status.ReadyReplicas,
			"expected", expectedReplicas)
		return r.updateClusterStatus(ctx, littleRed, false, nil)
	}

	// 4. All pods ready - check if cluster needs initialization
	if littleRed.Status.Cluster == nil || len(littleRed.Status.Cluster.Nodes) == 0 {
		// Before bootstrapping, check if cluster is already formed
		// (handles case where previous bootstrap succeeded but status update failed)
		if r.isClusterAlreadyFormed(ctx, littleRed) {
			log.Info("Cluster already formed, recovering status from actual cluster state")
			return r.recoverStatusFromCluster(ctx, littleRed)
		}
		log.Info("Cluster needs initialization")
		return r.bootstrapCluster(ctx, littleRed)
	}

	// 5. Check cluster health and detect node changes
	currentNodes, err := r.getClusterState(ctx, littleRed)
	if err != nil {
		log.Error(err, "Failed to get cluster state")
		// Cluster might be unhealthy, requeue
		return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
	}

	// 6. Detect pod restarts (node ID changed)
	needsRecovery := r.detectNodeChanges(currentNodes, littleRed.Status.Cluster.Nodes)
	if needsRecovery {
		log.Info("Detected node ID changes, attempting recovery")
		return r.recoverCluster(ctx, littleRed, currentNodes)
	}

	// 7. Update status
	return r.updateClusterStatus(ctx, littleRed, true, currentNodes)
}

// ensureClusterResources creates/updates all resources for cluster mode
func (r *LittleRedReconciler) ensureClusterResources(ctx context.Context, littleRed *littleredv1alpha1.LittleRed) error {
	log := logf.FromContext(ctx)

	// ConfigMap
	if err := r.reconcileClusterConfigMap(ctx, littleRed); err != nil {
		log.Error(err, "Failed to reconcile cluster ConfigMap")
		return err
	}

	// Headless Service (must exist before StatefulSet)
	if err := r.reconcileClusterHeadlessService(ctx, littleRed); err != nil {
		log.Error(err, "Failed to reconcile cluster headless Service")
		return err
	}

	// StatefulSet
	if err := r.reconcileClusterStatefulSet(ctx, littleRed); err != nil {
		log.Error(err, "Failed to reconcile cluster StatefulSet")
		return err
	}

	// Client Service
	if err := r.reconcileClusterClientService(ctx, littleRed); err != nil {
		log.Error(err, "Failed to reconcile cluster client Service")
		return err
	}

	// ServiceMonitor if enabled
	if littleRed.Spec.Metrics.IsEnabled() && littleRed.Spec.Metrics.ServiceMonitor.Enabled {
		if err := r.reconcileServiceMonitor(ctx, littleRed); err != nil {
			log.Error(err, "Failed to reconcile ServiceMonitor")
			// Don't fail - CRD might not be installed
		}
	}

	return nil
}

// reconcileClusterConfigMap ensures the ConfigMap exists
func (r *LittleRedReconciler) reconcileClusterConfigMap(ctx context.Context, littleRed *littleredv1alpha1.LittleRed) error {
	log := logf.FromContext(ctx)
	cm := buildClusterConfigMap(littleRed)

	if err := controllerutil.SetControllerReference(littleRed, cm, r.Scheme); err != nil {
		return err
	}

	existing := &corev1.ConfigMap{}
	err := r.Get(ctx, types.NamespacedName{Name: cm.Name, Namespace: cm.Namespace}, existing)
	if err != nil {
		if apierrors.IsNotFound(err) {
			log.Info("Creating cluster ConfigMap", "name", cm.Name)
			return r.Create(ctx, cm)
		}
		return err
	}

	if existing.Data["redis.conf"] != cm.Data["redis.conf"] {
		log.Info("Updating cluster ConfigMap", "name", cm.Name)
		existing.Data = cm.Data
		return r.Update(ctx, existing)
	}

	return nil
}

// reconcileClusterHeadlessService ensures the headless Service exists
func (r *LittleRedReconciler) reconcileClusterHeadlessService(ctx context.Context, littleRed *littleredv1alpha1.LittleRed) error {
	log := logf.FromContext(ctx)
	svc := buildClusterHeadlessService(littleRed)

	if err := controllerutil.SetControllerReference(littleRed, svc, r.Scheme); err != nil {
		return err
	}

	existing := &corev1.Service{}
	err := r.Get(ctx, types.NamespacedName{Name: svc.Name, Namespace: svc.Namespace}, existing)
	if err != nil {
		if apierrors.IsNotFound(err) {
			log.Info("Creating cluster headless Service", "name", svc.Name)
			return r.Create(ctx, svc)
		}
		return err
	}

	return nil
}

// reconcileClusterStatefulSet ensures the StatefulSet exists
func (r *LittleRedReconciler) reconcileClusterStatefulSet(ctx context.Context, littleRed *littleredv1alpha1.LittleRed) error {
	log := logf.FromContext(ctx)
	sts := buildClusterStatefulSet(littleRed)

	if err := controllerutil.SetControllerReference(littleRed, sts, r.Scheme); err != nil {
		return err
	}

	existing := &appsv1.StatefulSet{}
	err := r.Get(ctx, types.NamespacedName{Name: sts.Name, Namespace: sts.Namespace}, existing)
	if err != nil {
		if apierrors.IsNotFound(err) {
			log.Info("Creating cluster StatefulSet", "name", sts.Name)
			return r.Create(ctx, sts)
		}
		return err
	}

	log.Info("Updating cluster StatefulSet", "name", sts.Name)
	existing.Spec = sts.Spec
	return r.Update(ctx, existing)
}

// reconcileClusterClientService ensures the client Service exists
func (r *LittleRedReconciler) reconcileClusterClientService(ctx context.Context, littleRed *littleredv1alpha1.LittleRed) error {
	log := logf.FromContext(ctx)
	svc := buildClusterClientService(littleRed)

	if err := controllerutil.SetControllerReference(littleRed, svc, r.Scheme); err != nil {
		return err
	}

	existing := &corev1.Service{}
	err := r.Get(ctx, types.NamespacedName{Name: svc.Name, Namespace: svc.Namespace}, existing)
	if err != nil {
		if apierrors.IsNotFound(err) {
			log.Info("Creating cluster client Service", "name", svc.Name)
			return r.Create(ctx, svc)
		}
		return err
	}

	// Preserve ClusterIP
	svc.Spec.ClusterIP = existing.Spec.ClusterIP
	svc.Spec.ClusterIPs = existing.Spec.ClusterIPs
	existing.Spec = svc.Spec
	existing.Labels = svc.Labels
	existing.Annotations = svc.Annotations
	return r.Update(ctx, existing)
}

// bootstrapCluster initializes a new Redis Cluster
func (r *LittleRedReconciler) bootstrapCluster(ctx context.Context, littleRed *littleredv1alpha1.LittleRed) (ctrl.Result, error) {
	log := logf.FromContext(ctx)
	log.Info("Bootstrapping new cluster")

	cluster := littleRed.Spec.Cluster
	if cluster == nil {
		cluster = &littleredv1alpha1.ClusterSpec{}
		cluster.SetDefaults()
	}

	// Get password if auth enabled
	password := ""
	if littleRed.Spec.Auth.Enabled {
		secret := &corev1.Secret{}
		if err := r.Get(ctx, types.NamespacedName{
			Name:      littleRed.Spec.Auth.ExistingSecret,
			Namespace: littleRed.Namespace,
		}, secret); err != nil {
			return ctrl.Result{}, fmt.Errorf("failed to get auth secret: %w", err)
		}
		password = string(secret.Data["password"])
	}

	clusterClient := redisclient.NewClusterClient(password)

	// Get all pod IPs (CLUSTER MEET requires IP addresses, not hostnames)
	podIPs := make([]string, cluster.GetTotalNodes())
	podAddrs := make([]string, cluster.GetTotalNodes())
	for i := 0; i < cluster.GetTotalNodes(); i++ {
		podName := fmt.Sprintf("%s-cluster-%d", littleRed.Name, i)
		pod := &corev1.Pod{}
		if err := r.Get(ctx, types.NamespacedName{
			Name:      podName,
			Namespace: littleRed.Namespace,
		}, pod); err != nil {
			log.Error(err, "Failed to get pod", "pod", podName)
			return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
		}
		if pod.Status.PodIP == "" {
			log.Info("Pod has no IP yet", "pod", podName)
			return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
		}
		podIPs[i] = pod.Status.PodIP
		podAddrs[i] = fmt.Sprintf("%s:%d", pod.Status.PodIP, littleredv1alpha1.RedisPort)
	}

	// 1. Get all node IDs
	nodeIDs := make([]string, len(podAddrs))
	for i, addr := range podAddrs {
		nodeID, err := clusterClient.GetMyID(ctx, addr)
		if err != nil {
			log.Error(err, "Failed to get node ID", "addr", addr)
			return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
		}
		nodeIDs[i] = nodeID
		log.Info("Got node ID", "pod", fmt.Sprintf("%s-cluster-%d", littleRed.Name, i), "nodeID", nodeID)
	}

	// 2. CLUSTER MEET all nodes to the first node (using IP addresses)
	firstAddr := podAddrs[0]
	for i := 1; i < len(podAddrs); i++ {
		podName := fmt.Sprintf("%s-cluster-%d", littleRed.Name, i)

		log.Info("Meeting node", "target", firstAddr, "newNodeIP", podIPs[i])
		if err := clusterClient.ClusterMeet(ctx, firstAddr, podIPs[i], littleredv1alpha1.RedisPort); err != nil {
			log.Error(err, "Failed to meet node", "pod", podName, "ip", podIPs[i])
			return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
		}
	}

	// Wait for cluster to stabilize
	time.Sleep(2 * time.Second)

	// 3. Distribute slots across masters
	slotRanges := redisclient.GenerateSlotRanges(cluster.Shards)
	nodeStates := make([]littleredv1alpha1.ClusterNodeState, 0, cluster.GetTotalNodes())

	// Assign slots to master nodes (every (1+replicasPerShard) node is a master)
	for shard := 0; shard < cluster.Shards; shard++ {
		masterIdx := shard * (1 + cluster.ReplicasPerShard)
		masterAddr := podAddrs[masterIdx]
		masterPodName := fmt.Sprintf("%s-cluster-%d", littleRed.Name, masterIdx)

		// Expand slot range and add slots
		slots, err := redisclient.ExpandSlotRange(
			redisclient.FormatSlotRange(slotRanges[shard].Start, slotRanges[shard].End))
		if err != nil {
			return ctrl.Result{}, fmt.Errorf("failed to expand slot range: %w", err)
		}

		log.Info("Adding slots to master",
			"master", masterPodName,
			"slots", redisclient.FormatSlotRange(slotRanges[shard].Start, slotRanges[shard].End))

		if err := clusterClient.ClusterAddSlots(ctx, masterAddr, slots...); err != nil {
			log.Error(err, "Failed to add slots", "master", masterPodName)
			return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
		}

		// Record master state
		nodeStates = append(nodeStates, littleredv1alpha1.ClusterNodeState{
			PodName:    masterPodName,
			NodeID:     nodeIDs[masterIdx],
			Role:       "master",
			SlotRanges: redisclient.FormatSlotRange(slotRanges[shard].Start, slotRanges[shard].End),
		})
	}

	// Wait for slot assignment to propagate
	time.Sleep(2 * time.Second)

	// 4. Assign replicas to masters
	for shard := 0; shard < cluster.Shards; shard++ {
		masterIdx := shard * (1 + cluster.ReplicasPerShard)
		masterNodeID := nodeIDs[masterIdx]

		for replica := 0; replica < cluster.ReplicasPerShard; replica++ {
			replicaIdx := masterIdx + 1 + replica
			replicaAddr := podAddrs[replicaIdx]
			replicaPodName := fmt.Sprintf("%s-cluster-%d", littleRed.Name, replicaIdx)

			log.Info("Assigning replica to master",
				"replica", replicaPodName,
				"masterNodeID", masterNodeID)

			if err := clusterClient.ClusterReplicate(ctx, replicaAddr, masterNodeID); err != nil {
				log.Error(err, "Failed to assign replica", "replica", replicaPodName)
				return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
			}

			// Record replica state
			nodeStates = append(nodeStates, littleredv1alpha1.ClusterNodeState{
				PodName:      replicaPodName,
				NodeID:       nodeIDs[replicaIdx],
				Role:         "replica",
				MasterNodeID: masterNodeID,
			})
		}
	}

	// 5. Update status with cluster state
	now := metav1.Now()
	littleRed.Status.Cluster = &littleredv1alpha1.ClusterStatusInfo{
		State:         "initializing",
		Nodes:         nodeStates,
		LastBootstrap: &now,
	}

	if err := r.Status().Update(ctx, littleRed); err != nil {
		return ctrl.Result{}, err
	}

	log.Info("Cluster bootstrap complete", "nodes", len(nodeStates))
	return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
}

// getClusterState queries the cluster to get current node states
func (r *LittleRedReconciler) getClusterState(ctx context.Context, littleRed *littleredv1alpha1.LittleRed) ([]littleredv1alpha1.ClusterNodeState, error) {
	cluster := littleRed.Spec.Cluster
	if cluster == nil {
		cluster = &littleredv1alpha1.ClusterSpec{}
		cluster.SetDefaults()
	}

	// Get password if auth enabled
	password := ""
	if littleRed.Spec.Auth.Enabled {
		secret := &corev1.Secret{}
		if err := r.Get(ctx, types.NamespacedName{
			Name:      littleRed.Spec.Auth.ExistingSecret,
			Namespace: littleRed.Namespace,
		}, secret); err != nil {
			return nil, fmt.Errorf("failed to get auth secret: %w", err)
		}
		password = string(secret.Data["password"])
	}

	clusterClient := redisclient.NewClusterClient(password)

	// Query first node for cluster state
	firstAddr := fmt.Sprintf("%s-cluster-0.%s.%s.svc.cluster.local:%d",
		littleRed.Name, clusterHeadlessServiceName(littleRed), littleRed.Namespace, littleredv1alpha1.RedisPort)

	nodes, err := clusterClient.GetClusterNodes(ctx, firstAddr)
	if err != nil {
		return nil, fmt.Errorf("failed to get cluster nodes: %w", err)
	}

	// Get pod IPs for matching (since we use IP-based announce)
	podIPs := make(map[string]string) // podName -> IP
	for i := 0; i < cluster.GetTotalNodes(); i++ {
		podName := fmt.Sprintf("%s-cluster-%d", littleRed.Name, i)
		pod := &corev1.Pod{}
		if err := r.Get(ctx, types.NamespacedName{
			Name:      podName,
			Namespace: littleRed.Namespace,
		}, pod); err != nil {
			return nil, fmt.Errorf("failed to get pod %s: %w", podName, err)
		}
		if pod.Status.PodIP != "" {
			podIPs[podName] = pod.Status.PodIP
		}
	}

	// Convert to ClusterNodeState (match by IP address since we use --cluster-announce-ip with IPs)
	states := make([]littleredv1alpha1.ClusterNodeState, 0, len(nodes))
	for i := 0; i < cluster.GetTotalNodes(); i++ {
		podName := fmt.Sprintf("%s-cluster-%d", littleRed.Name, i)
		podIP, ok := podIPs[podName]
		if !ok {
			continue
		}

		// Find this pod's node in the cluster nodes list by matching IP
		for _, node := range nodes {
			// node.Addr is "IP:port", extract just the IP
			nodeIP := strings.Split(node.Addr, ":")[0]
			if nodeIP == podIP {
				state := littleredv1alpha1.ClusterNodeState{
					PodName: podName,
					NodeID:  node.NodeID,
				}

				if node.IsMaster() {
					state.Role = "master"
					if len(node.Slots) > 0 {
						state.SlotRanges = node.Slots[0]
					}
				} else {
					state.Role = "replica"
					if node.MasterID != "-" {
						state.MasterNodeID = node.MasterID
					}
				}

				states = append(states, state)
				break
			}
		}
	}

	return states, nil
}

// detectNodeChanges checks if any node IDs have changed (pod restarts)
func (r *LittleRedReconciler) detectNodeChanges(current, stored []littleredv1alpha1.ClusterNodeState) bool {
	if len(current) != len(stored) {
		return true
	}

	storedMap := make(map[string]string) // podName -> nodeID
	for _, s := range stored {
		storedMap[s.PodName] = s.NodeID
	}

	for _, c := range current {
		if storedID, ok := storedMap[c.PodName]; ok {
			if storedID != c.NodeID {
				return true
			}
		}
	}

	return false
}

// recoverCluster handles recovery when pod(s) restart with new node IDs
func (r *LittleRedReconciler) recoverCluster(ctx context.Context, littleRed *littleredv1alpha1.LittleRed, currentNodes []littleredv1alpha1.ClusterNodeState) (ctrl.Result, error) {
	log := logf.FromContext(ctx)
	log.Info("Starting cluster recovery")

	storedNodes := littleRed.Status.Cluster.Nodes

	// Check if ALL nodes changed (full restart)
	allChanged := true
	storedMap := make(map[string]string)
	for _, s := range storedNodes {
		storedMap[s.PodName] = s.NodeID
	}
	for _, c := range currentNodes {
		if storedID, ok := storedMap[c.PodName]; ok {
			if storedID == c.NodeID {
				allChanged = false
				break
			}
		}
	}

	if allChanged {
		log.Info("All node IDs changed - performing full recovery (re-bootstrap)")
		return r.fullRecovery(ctx, littleRed)
	}

	// Partial recovery - some nodes restarted
	log.Info("Partial node restart detected - attempting partial recovery")

	// Get password if auth enabled
	password := ""
	if littleRed.Spec.Auth.Enabled {
		secret := &corev1.Secret{}
		if err := r.Get(ctx, types.NamespacedName{
			Name:      littleRed.Spec.Auth.ExistingSecret,
			Namespace: littleRed.Namespace,
		}, secret); err != nil {
			return ctrl.Result{}, fmt.Errorf("failed to get auth secret: %w", err)
		}
		password = string(secret.Data["password"])
	}

	clusterClient := redisclient.NewClusterClient(password)

	// Find a healthy node to use for recovery commands (need IP address)
	var healthyAddr string
	for _, c := range currentNodes {
		if storedID, ok := storedMap[c.PodName]; ok && storedID == c.NodeID {
			// Get pod IP
			pod := &corev1.Pod{}
			if err := r.Get(ctx, types.NamespacedName{
				Name:      c.PodName,
				Namespace: littleRed.Namespace,
			}, pod); err != nil {
				continue
			}
			healthyAddr = fmt.Sprintf("%s:%d", pod.Status.PodIP, littleredv1alpha1.RedisPort)
			break
		}
	}

	if healthyAddr == "" {
		log.Info("No healthy nodes found - falling back to full recovery")
		return r.fullRecovery(ctx, littleRed)
	}

	// Iterate through all stored nodes and recover any that have changed
	for _, stored := range storedNodes {
		// Get pod
		pod := &corev1.Pod{}
		if err := r.Get(ctx, types.NamespacedName{
			Name:      stored.PodName,
			Namespace: littleRed.Namespace,
		}, pod); err != nil {
			log.Error(err, "Failed to get pod for recovery check", "pod", stored.PodName)
			continue
		}

		if pod.Status.PodIP == "" {
			log.Info("Pod has no IP yet, skipping", "pod", stored.PodName)
			continue
		}

		// Get current node ID for this pod
		podAddr := fmt.Sprintf("%s:%d", pod.Status.PodIP, littleredv1alpha1.RedisPort)
		currentNodeID, err := clusterClient.GetMyID(ctx, podAddr)
		if err != nil {
			log.Error(err, "Failed to get current node ID", "pod", stored.PodName)
			continue
		}

		// Check if node ID changed
		if currentNodeID == stored.NodeID {
			continue // No change
		}

		log.Info("Recovering node", "pod", stored.PodName, "oldID", stored.NodeID, "newID", currentNodeID)

		// CLUSTER FORGET old node ID on ALL healthy nodes (not just one)
		// This is critical - FORGET must be run on every node, otherwise other nodes
		// will gossip the old node ID back into the cluster
		for _, otherStored := range storedNodes {
			if otherStored.PodName == stored.PodName {
				continue // Skip the node being recovered
			}
			otherPod := &corev1.Pod{}
			if err := r.Get(ctx, types.NamespacedName{
				Name:      otherStored.PodName,
				Namespace: littleRed.Namespace,
			}, otherPod); err != nil || otherPod.Status.PodIP == "" {
				continue
			}
			otherAddr := fmt.Sprintf("%s:%d", otherPod.Status.PodIP, littleredv1alpha1.RedisPort)
			if err := clusterClient.ClusterForget(ctx, otherAddr, stored.NodeID); err != nil {
				log.Info("Failed to forget old node ID on peer (may already be forgotten)",
					"peer", otherStored.PodName, "oldNodeID", stored.NodeID, "error", err)
				// Continue - node might already be forgotten or peer might be the restarting node
			}
		}

		// CLUSTER MEET new node using IP address
		if err := clusterClient.ClusterMeet(ctx, healthyAddr, pod.Status.PodIP, littleredv1alpha1.RedisPort); err != nil {
			log.Error(err, "Failed to meet new node", "pod", stored.PodName, "ip", pod.Status.PodIP)
			return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
		}

		time.Sleep(1 * time.Second)

		// If master, re-add slots
		if stored.Role == "master" && stored.SlotRanges != "" {
			slots, err := redisclient.ExpandSlotRange(stored.SlotRanges)
			if err != nil {
				log.Error(err, "Failed to expand slot range", "range", stored.SlotRanges)
				continue
			}
			if err := clusterClient.ClusterAddSlots(ctx, podAddr, slots...); err != nil {
				log.Error(err, "Failed to re-add slots", "pod", stored.PodName)
			}
		}

		// If replica, re-assign to master (need to find current master node ID)
		if stored.Role == "replica" && stored.MasterNodeID != "" {
			// Find master's current node ID
			masterNodeID := stored.MasterNodeID
			for _, s2 := range storedNodes {
				if s2.NodeID == stored.MasterNodeID {
					// This is the master, get its current node ID
					masterPod := &corev1.Pod{}
					if err := r.Get(ctx, types.NamespacedName{
						Name:      s2.PodName,
						Namespace: littleRed.Namespace,
					}, masterPod); err == nil && masterPod.Status.PodIP != "" {
						masterAddr := fmt.Sprintf("%s:%d", masterPod.Status.PodIP, littleredv1alpha1.RedisPort)
						if newMasterID, err := clusterClient.GetMyID(ctx, masterAddr); err == nil {
							masterNodeID = newMasterID
						}
					}
					break
				}
			}

			if err := clusterClient.ClusterReplicate(ctx, podAddr, masterNodeID); err != nil {
				log.Error(err, "Failed to re-assign replica", "pod", stored.PodName)
			}
		}
	}

	// Update status with new node IDs by querying each pod directly
	// This ensures we get accurate node IDs even if pods haven't fully rejoined cluster yet
	updatedNodes := make([]littleredv1alpha1.ClusterNodeState, 0, len(storedNodes))
	for _, stored := range storedNodes {
		pod := &corev1.Pod{}
		if err := r.Get(ctx, types.NamespacedName{
			Name:      stored.PodName,
			Namespace: littleRed.Namespace,
		}, pod); err != nil {
			log.Error(err, "Failed to get pod for status update", "pod", stored.PodName)
			// Keep stored state if we can't get pod
			updatedNodes = append(updatedNodes, stored)
			continue
		}

		if pod.Status.PodIP == "" {
			log.Info("Pod has no IP yet, keeping stored state", "pod", stored.PodName)
			updatedNodes = append(updatedNodes, stored)
			continue
		}

		// Get current node ID directly from pod
		podAddr := fmt.Sprintf("%s:%d", pod.Status.PodIP, littleredv1alpha1.RedisPort)
		currentNodeID, err := clusterClient.GetMyID(ctx, podAddr)
		if err != nil {
			log.Error(err, "Failed to get node ID for status update", "pod", stored.PodName)
			// Keep stored state if we can't query
			updatedNodes = append(updatedNodes, stored)
			continue
		}

		// Create updated state with current node ID but preserved role/slots
		updated := littleredv1alpha1.ClusterNodeState{
			PodName:    stored.PodName,
			NodeID:     currentNodeID,
			Role:       stored.Role,
			SlotRanges: stored.SlotRanges,
		}

		// For replicas, update master node ID if master also restarted
		if stored.Role == "replica" && stored.MasterNodeID != "" {
			// Find master pod and get its current node ID
			for _, s2 := range storedNodes {
				if s2.NodeID == stored.MasterNodeID && s2.Role == "master" {
					masterPod := &corev1.Pod{}
					if err := r.Get(ctx, types.NamespacedName{
						Name:      s2.PodName,
						Namespace: littleRed.Namespace,
					}, masterPod); err == nil && masterPod.Status.PodIP != "" {
						masterAddr := fmt.Sprintf("%s:%d", masterPod.Status.PodIP, littleredv1alpha1.RedisPort)
						if newMasterID, err := clusterClient.GetMyID(ctx, masterAddr); err == nil {
							updated.MasterNodeID = newMasterID
						} else {
							updated.MasterNodeID = stored.MasterNodeID
						}
					} else {
						updated.MasterNodeID = stored.MasterNodeID
					}
					break
				}
			}
		}

		updatedNodes = append(updatedNodes, updated)
	}

	littleRed.Status.Cluster.Nodes = updatedNodes
	littleRed.Status.Cluster.State = "recovering"
	if err := r.Status().Update(ctx, littleRed); err != nil {
		return ctrl.Result{}, err
	}

	log.Info("Cluster recovery operations completed, waiting for stabilization")
	return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
}

// fullRecovery performs a full cluster re-bootstrap when all nodes lost identity
func (r *LittleRedReconciler) fullRecovery(ctx context.Context, littleRed *littleredv1alpha1.LittleRed) (ctrl.Result, error) {
	log := logf.FromContext(ctx)
	log.Info("Performing full cluster recovery (all nodes lost identity)")

	cluster := littleRed.Spec.Cluster
	if cluster == nil {
		cluster = &littleredv1alpha1.ClusterSpec{}
		cluster.SetDefaults()
	}

	// Get password if auth enabled
	password := ""
	if littleRed.Spec.Auth.Enabled {
		secret := &corev1.Secret{}
		if err := r.Get(ctx, types.NamespacedName{
			Name:      littleRed.Spec.Auth.ExistingSecret,
			Namespace: littleRed.Namespace,
		}, secret); err != nil {
			return ctrl.Result{}, fmt.Errorf("failed to get auth secret: %w", err)
		}
		password = string(secret.Data["password"])
	}

	clusterClient := redisclient.NewClusterClient(password)

	// Reset all nodes
	for i := 0; i < cluster.GetTotalNodes(); i++ {
		podName := fmt.Sprintf("%s-cluster-%d", littleRed.Name, i)
		addr := fmt.Sprintf("%s.%s.%s.svc.cluster.local:%d",
			podName, clusterHeadlessServiceName(littleRed), littleRed.Namespace, littleredv1alpha1.RedisPort)

		log.Info("Resetting node", "pod", podName)
		if err := clusterClient.ClusterResetSoft(ctx, addr); err != nil {
			log.Error(err, "Failed to reset node", "pod", podName)
			// Continue - some nodes might fail
		}
	}

	// Clear stored cluster state to trigger fresh bootstrap
	littleRed.Status.Cluster = nil
	if err := r.Status().Update(ctx, littleRed); err != nil {
		return ctrl.Result{}, err
	}

	log.Info("Cluster reset complete, will re-bootstrap on next reconcile")
	return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
}

// updateClusterStatus updates the LittleRed status for cluster mode
func (r *LittleRedReconciler) updateClusterStatus(ctx context.Context, littleRed *littleredv1alpha1.LittleRed, healthy bool, currentNodes []littleredv1alpha1.ClusterNodeState) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	// Get StatefulSet status
	sts := &appsv1.StatefulSet{}
	if err := r.Get(ctx, types.NamespacedName{
		Name:      clusterStatefulSetName(littleRed),
		Namespace: littleRed.Namespace,
	}, sts); err != nil {
		if !apierrors.IsNotFound(err) {
			return ctrl.Result{}, err
		}
		littleRed.Status.Redis.Ready = 0
		littleRed.Status.Redis.Total = 0
	} else {
		littleRed.Status.Redis.Ready = sts.Status.ReadyReplicas
		littleRed.Status.Redis.Total = *sts.Spec.Replicas
	}

	// Get actual cluster state from Redis CLUSTER INFO
	actualClusterState := "unknown"
	if healthy {
		password := ""
		if littleRed.Spec.Auth.Enabled {
			secret := &corev1.Secret{}
			if err := r.Get(ctx, types.NamespacedName{
				Name:      littleRed.Spec.Auth.ExistingSecret,
				Namespace: littleRed.Namespace,
			}, secret); err == nil {
				password = string(secret.Data["password"])
			}
		}

		clusterClient := redisclient.NewClusterClient(password)
		firstAddr := fmt.Sprintf("%s-cluster-0.%s.%s.svc.cluster.local:%d",
			littleRed.Name, clusterHeadlessServiceName(littleRed), littleRed.Namespace, littleredv1alpha1.RedisPort)

		if clusterInfo, err := clusterClient.GetClusterInfo(ctx, firstAddr); err == nil {
			actualClusterState = clusterInfo.State
		} else {
			log.Error(err, "Failed to get cluster info from Redis")
		}
	}

	// Update cluster state
	if littleRed.Status.Cluster != nil {
		if actualClusterState == "ok" {
			littleRed.Status.Cluster.State = "ok"
		} else if actualClusterState != "unknown" {
			littleRed.Status.Cluster.State = actualClusterState
		} else if littleRed.Status.Cluster.State == "" {
			littleRed.Status.Cluster.State = "initializing"
		}

		// Update node states if we have current info
		if len(currentNodes) > 0 {
			// Merge current node IDs with stored slot assignments
			nodeMap := make(map[string]littleredv1alpha1.ClusterNodeState)
			for _, n := range littleRed.Status.Cluster.Nodes {
				nodeMap[n.PodName] = n
			}
			for _, c := range currentNodes {
				if stored, ok := nodeMap[c.PodName]; ok {
					c.SlotRanges = stored.SlotRanges
					if c.Role == "" {
						c.Role = stored.Role
					}
					if c.MasterNodeID == "" {
						c.MasterNodeID = stored.MasterNodeID
					}
				}
				nodeMap[c.PodName] = c
			}

			newNodes := make([]littleredv1alpha1.ClusterNodeState, 0, len(nodeMap))
			for _, n := range nodeMap {
				newNodes = append(newNodes, n)
			}
			littleRed.Status.Cluster.Nodes = newNodes
		}
	}

	// Determine phase and conditions
	allReady := littleRed.Status.Redis.Ready == littleRed.Status.Redis.Total && littleRed.Status.Redis.Ready > 0

	if allReady && healthy {
		littleRed.Status.Phase = littleredv1alpha1.PhaseRunning
		meta.SetStatusCondition(&littleRed.Status.Conditions, metav1.Condition{
			Type:               littleredv1alpha1.ConditionReady,
			Status:             metav1.ConditionTrue,
			Reason:             "AllPodsReady",
			Message:            "All cluster pods are ready",
			LastTransitionTime: metav1.Now(),
		})
		meta.SetStatusCondition(&littleRed.Status.Conditions, metav1.Condition{
			Type:               littleredv1alpha1.ConditionClusterReady,
			Status:             metav1.ConditionTrue,
			Reason:             "ClusterHealthy",
			Message:            "Redis Cluster is healthy",
			LastTransitionTime: metav1.Now(),
		})
		meta.SetStatusCondition(&littleRed.Status.Conditions, metav1.Condition{
			Type:               littleredv1alpha1.ConditionInitialized,
			Status:             metav1.ConditionTrue,
			Reason:             "Initialized",
			Message:            "Redis Cluster is initialized",
			LastTransitionTime: metav1.Now(),
		})
	} else {
		littleRed.Status.Phase = littleredv1alpha1.PhaseInitializing
		meta.SetStatusCondition(&littleRed.Status.Conditions, metav1.Condition{
			Type:               littleredv1alpha1.ConditionReady,
			Status:             metav1.ConditionFalse,
			Reason:             "PodsNotReady",
			Message:            fmt.Sprintf("Cluster pods: %d/%d ready", littleRed.Status.Redis.Ready, littleRed.Status.Redis.Total),
			LastTransitionTime: metav1.Now(),
		})
		if !healthy && littleRed.Status.Cluster != nil {
			meta.SetStatusCondition(&littleRed.Status.Conditions, metav1.Condition{
				Type:               littleredv1alpha1.ConditionClusterReady,
				Status:             metav1.ConditionFalse,
				Reason:             "ClusterUnhealthy",
				Message:            "Redis Cluster is not healthy",
				LastTransitionTime: metav1.Now(),
			})
		}
	}

	// Update observed generation
	littleRed.Status.ObservedGeneration = littleRed.Generation

	// Update high-level status summary
	if littleRed.Status.Cluster != nil {
		littleRed.Status.Status = fmt.Sprintf("%s (%d shards)", littleRed.Status.Cluster.State, littleRed.Spec.Cluster.Shards)
	} else {
		littleRed.Status.Status = string(littleRed.Status.Phase)
	}

	// Update status
	if err := r.Status().Update(ctx, littleRed); err != nil {
		if apierrors.IsConflict(err) {
			log.Info("Status update conflict, requeueing")
			return ctrl.Result{RequeueAfter: time.Second}, nil
		}
		return ctrl.Result{}, err
	}

	// Requeue if not running
	if littleRed.Status.Phase != littleredv1alpha1.PhaseRunning {
		return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
	}

	// Periodically requeue to check cluster health
	return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
}

// validateClusterSpec validates cluster-specific configuration
func (r *LittleRedReconciler) validateClusterSpec(littleRed *littleredv1alpha1.LittleRed) error {
	cluster := littleRed.Spec.Cluster
	if cluster == nil {
		return nil // Will use defaults
	}

	if cluster.Shards < 3 {
		return fmt.Errorf("cluster.shards must be at least 3, got %d", cluster.Shards)
	}

	if cluster.ReplicasPerShard < 0 {
		return fmt.Errorf("cluster.replicasPerShard cannot be negative, got %d", cluster.ReplicasPerShard)
	}

	return nil
}

// getClusterPods returns all pods for the cluster
func (r *LittleRedReconciler) getClusterPods(ctx context.Context, littleRed *littleredv1alpha1.LittleRed) (*corev1.PodList, error) {
	podList := &corev1.PodList{}
	listOpts := []client.ListOption{
		client.InNamespace(littleRed.Namespace),
		client.MatchingLabels(clusterSelectorLabels(littleRed)),
	}
	if err := r.List(ctx, podList, listOpts...); err != nil {
		return nil, err
	}
	return podList, nil
}

// isClusterAlreadyFormed checks if a Redis cluster is already formed
// (handles case where bootstrap succeeded but status update failed)
func (r *LittleRedReconciler) isClusterAlreadyFormed(ctx context.Context, littleRed *littleredv1alpha1.LittleRed) bool {
	log := logf.FromContext(ctx)

	// Get password if auth enabled
	password := ""
	if littleRed.Spec.Auth.Enabled {
		secret := &corev1.Secret{}
		if err := r.Get(ctx, types.NamespacedName{
			Name:      littleRed.Spec.Auth.ExistingSecret,
			Namespace: littleRed.Namespace,
		}, secret); err == nil {
			password = string(secret.Data["password"])
		}
	}

	clusterClient := redisclient.NewClusterClient(password)

	// Try to get cluster info from first pod
	pod := &corev1.Pod{}
	podName := fmt.Sprintf("%s-cluster-0", littleRed.Name)
	if err := r.Get(ctx, types.NamespacedName{
		Name:      podName,
		Namespace: littleRed.Namespace,
	}, pod); err != nil || pod.Status.PodIP == "" {
		return false
	}

	addr := fmt.Sprintf("%s:%d", pod.Status.PodIP, littleredv1alpha1.RedisPort)
	clusterInfo, err := clusterClient.GetClusterInfo(ctx, addr)
	if err != nil {
		log.Info("Could not get cluster info, assuming not formed", "error", err)
		return false
	}

	// If cluster state is ok and slots are assigned, cluster is already formed
	if clusterInfo.State == "ok" && clusterInfo.SlotsAssigned == 16384 {
		log.Info("Cluster already formed", "state", clusterInfo.State, "slotsAssigned", clusterInfo.SlotsAssigned)
		return true
	}

	return false
}

// recoverStatusFromCluster recovers CR status from actual cluster state
// Used when cluster is formed but status was lost
func (r *LittleRedReconciler) recoverStatusFromCluster(ctx context.Context, littleRed *littleredv1alpha1.LittleRed) (ctrl.Result, error) {
	log := logf.FromContext(ctx)
	log.Info("Recovering cluster status from actual cluster state")

	cluster := littleRed.Spec.Cluster
	if cluster == nil {
		cluster = &littleredv1alpha1.ClusterSpec{}
		cluster.SetDefaults()
	}

	// Get password if auth enabled
	password := ""
	if littleRed.Spec.Auth.Enabled {
		secret := &corev1.Secret{}
		if err := r.Get(ctx, types.NamespacedName{
			Name:      littleRed.Spec.Auth.ExistingSecret,
			Namespace: littleRed.Namespace,
		}, secret); err != nil {
			return ctrl.Result{}, fmt.Errorf("failed to get auth secret: %w", err)
		}
		password = string(secret.Data["password"])
	}

	clusterClient := redisclient.NewClusterClient(password)

	// Get cluster nodes from first pod
	pod := &corev1.Pod{}
	podName := fmt.Sprintf("%s-cluster-0", littleRed.Name)
	if err := r.Get(ctx, types.NamespacedName{
		Name:      podName,
		Namespace: littleRed.Namespace,
	}, pod); err != nil {
		return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
	}

	addr := fmt.Sprintf("%s:%d", pod.Status.PodIP, littleredv1alpha1.RedisPort)
	nodes, err := clusterClient.GetClusterNodes(ctx, addr)
	if err != nil {
		log.Error(err, "Failed to get cluster nodes")
		return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
	}

	// Build pod IP to pod name mapping
	podIPs := make(map[string]string)
	for i := 0; i < cluster.GetTotalNodes(); i++ {
		pName := fmt.Sprintf("%s-cluster-%d", littleRed.Name, i)
		p := &corev1.Pod{}
		if err := r.Get(ctx, types.NamespacedName{
			Name:      pName,
			Namespace: littleRed.Namespace,
		}, p); err == nil && p.Status.PodIP != "" {
			podIPs[p.Status.PodIP] = pName
		}
	}

	// Convert cluster nodes to status
	nodeStates := make([]littleredv1alpha1.ClusterNodeState, 0, len(nodes))
	for _, node := range nodes {
		// Extract IP from addr (format: "IP:port")
		nodeIP := strings.Split(node.Addr, ":")[0]
		podName, ok := podIPs[nodeIP]
		if !ok {
			continue
		}

		state := littleredv1alpha1.ClusterNodeState{
			PodName: podName,
			NodeID:  node.NodeID,
		}

		if node.IsMaster() {
			state.Role = "master"
			if len(node.Slots) > 0 {
				state.SlotRanges = strings.Join(node.Slots, ",")
			}
		} else {
			state.Role = "replica"
			if node.MasterID != "-" {
				state.MasterNodeID = node.MasterID
			}
		}

		nodeStates = append(nodeStates, state)
	}

	// Update status
	now := metav1.Now()
	littleRed.Status.Cluster = &littleredv1alpha1.ClusterStatusInfo{
		State:         "ok",
		Nodes:         nodeStates,
		LastBootstrap: &now,
	}

	if err := r.Status().Update(ctx, littleRed); err != nil {
		return ctrl.Result{}, err
	}

	log.Info("Recovered cluster status", "nodes", len(nodeStates))
	return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
}
