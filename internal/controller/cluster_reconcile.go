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

	// Get all pod addresses
	podAddrs := make([]string, cluster.GetTotalNodes())
	for i := 0; i < cluster.GetTotalNodes(); i++ {
		podName := fmt.Sprintf("%s-cluster-%d", littleRed.Name, i)
		podAddrs[i] = fmt.Sprintf("%s.%s.%s.svc.cluster.local:%d",
			podName, clusterHeadlessServiceName(littleRed), littleRed.Namespace, littleredv1alpha1.RedisPort)
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

	// 2. CLUSTER MEET all nodes to the first node
	firstAddr := podAddrs[0]
	for i := 1; i < len(podAddrs); i++ {
		podName := fmt.Sprintf("%s-cluster-%d", littleRed.Name, i)
		host := fmt.Sprintf("%s.%s.%s.svc.cluster.local",
			podName, clusterHeadlessServiceName(littleRed), littleRed.Namespace)

		log.Info("Meeting node", "target", firstAddr, "newNode", host)
		if err := clusterClient.ClusterMeet(ctx, firstAddr, host, littleredv1alpha1.RedisPort); err != nil {
			log.Error(err, "Failed to meet node", "host", host)
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

	// Convert to ClusterNodeState
	states := make([]littleredv1alpha1.ClusterNodeState, 0, len(nodes))
	for i := 0; i < cluster.GetTotalNodes(); i++ {
		podName := fmt.Sprintf("%s-cluster-%d", littleRed.Name, i)
		podHostname := fmt.Sprintf("%s.%s.%s.svc.cluster.local",
			podName, clusterHeadlessServiceName(littleRed), littleRed.Namespace)

		// Find this pod's node in the cluster nodes list
		for _, node := range nodes {
			if node.Hostname == podHostname {
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

	// Find a healthy node to use for recovery commands
	var healthyAddr string
	for _, c := range currentNodes {
		if storedID, ok := storedMap[c.PodName]; ok && storedID == c.NodeID {
			healthyAddr = fmt.Sprintf("%s.%s.%s.svc.cluster.local:%d",
				c.PodName, clusterHeadlessServiceName(littleRed), littleRed.Namespace, littleredv1alpha1.RedisPort)
			break
		}
	}

	if healthyAddr == "" {
		log.Info("No healthy nodes found - falling back to full recovery")
		return r.fullRecovery(ctx, littleRed)
	}

	// For each changed node, forget old ID and meet new node
	for _, c := range currentNodes {
		storedID, ok := storedMap[c.PodName]
		if !ok || storedID == c.NodeID {
			continue
		}

		log.Info("Recovering node", "pod", c.PodName, "oldID", storedID, "newID", c.NodeID)

		// CLUSTER FORGET old node ID on healthy node
		if err := clusterClient.ClusterForget(ctx, healthyAddr, storedID); err != nil {
			log.Error(err, "Failed to forget old node ID", "nodeID", storedID)
			// Continue - node might already be forgotten
		}

		// CLUSTER MEET new node
		newHost := fmt.Sprintf("%s.%s.%s.svc.cluster.local",
			c.PodName, clusterHeadlessServiceName(littleRed), littleRed.Namespace)
		if err := clusterClient.ClusterMeet(ctx, healthyAddr, newHost, littleredv1alpha1.RedisPort); err != nil {
			log.Error(err, "Failed to meet new node", "host", newHost)
			return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
		}

		time.Sleep(1 * time.Second)

		// Find stored state for this pod
		var storedState *littleredv1alpha1.ClusterNodeState
		for i := range storedNodes {
			if storedNodes[i].PodName == c.PodName {
				storedState = &storedNodes[i]
				break
			}
		}

		if storedState == nil {
			continue
		}

		newAddr := fmt.Sprintf("%s:%d", newHost, littleredv1alpha1.RedisPort)

		// If master, re-add slots
		if storedState.Role == "master" && storedState.SlotRanges != "" {
			slots, err := redisclient.ExpandSlotRange(storedState.SlotRanges)
			if err != nil {
				log.Error(err, "Failed to expand slot range", "range", storedState.SlotRanges)
				continue
			}
			if err := clusterClient.ClusterAddSlots(ctx, newAddr, slots...); err != nil {
				log.Error(err, "Failed to re-add slots", "pod", c.PodName)
			}
		}

		// If replica, re-assign to master
		if storedState.Role == "replica" && storedState.MasterNodeID != "" {
			if err := clusterClient.ClusterReplicate(ctx, newAddr, storedState.MasterNodeID); err != nil {
				log.Error(err, "Failed to re-assign replica", "pod", c.PodName)
			}
		}
	}

	// Update status with new node IDs
	littleRed.Status.Cluster.State = "recovering"
	if err := r.Status().Update(ctx, littleRed); err != nil {
		return ctrl.Result{}, err
	}

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

	// Update cluster state
	if littleRed.Status.Cluster != nil {
		if healthy {
			littleRed.Status.Cluster.State = "ok"
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
