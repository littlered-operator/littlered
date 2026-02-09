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

	// 5. Check cluster health and ensure connectivity
	// We proactively ensure the mesh is connected to heal split-brain scenarios
	if err := r.ensureClusterConnectivity(ctx, littleRed); err != nil {
		log.Error(err, "Failed to ensure cluster connectivity")
		// Continue - partial connectivity might still allow status updates
	}

	// 6. Get cluster state (Ground Truth from all pods)
	currentNodes, err := r.getClusterState(ctx, littleRed)
	if err != nil {
		log.Error(err, "Failed to get cluster state")
		fast, _ := littleRed.GetRequeueIntervals()
		return ctrl.Result{RequeueAfter: fast}, nil
	}

	// 7. Update status
	return r.updateClusterStatus(ctx, littleRed, true, currentNodes)
}

// ensureClusterConnectivity sends CLUSTER MEET commands to ensure full mesh
// This heals partitions by forcing isolated nodes to re-join the cluster.
func (r *LittleRedReconciler) ensureClusterConnectivity(ctx context.Context, littleRed *littleredv1alpha1.LittleRed) error {
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
			return fmt.Errorf("failed to get auth secret: %w", err)
		}
		password = string(secret.Data["password"])
	}

	clusterClient := redisclient.NewClusterClient(password)

	// Get all pod IPs
	podIPs := make([]string, 0)
	for i := 0; i < cluster.GetTotalNodes(); i++ {
		podName := fmt.Sprintf("%s-cluster-%d", littleRed.Name, i)
		pod := &corev1.Pod{}
		if err := r.Get(ctx, types.NamespacedName{
			Name:      podName,
			Namespace: littleRed.Namespace,
		}, pod); err == nil && pod.Status.PodIP != "" {
			podIPs = append(podIPs, pod.Status.PodIP)
		}
	}

	if len(podIPs) < 2 {
		return nil
	}

	// Simple Mesh Healer: Meet everyone to the first reachable node (Seed)
	// We pick the first pod as the seed. If it's down, we try the next.
	seedIP := podIPs[0]
	seedAddr := fmt.Sprintf("%s:%d", seedIP, littleredv1alpha1.RedisPort)

	// Try to meet everyone to the seed
	for i := 1; i < len(podIPs); i++ {
		// MEET <target_ip> <port> executed on the Seed node
		if err := clusterClient.ClusterMeet(ctx, seedAddr, podIPs[i], littleredv1alpha1.RedisPort); err != nil {
			// Log but don't fail, transient errors are expected
		}
	}

	return nil
}

// getClusterState queries ALL pods to build a robust view of the cluster
func (r *LittleRedReconciler) getClusterState(ctx context.Context, littleRed *littleredv1alpha1.LittleRed) ([]littleredv1alpha1.ClusterNodeState, error) {
	log := logf.FromContext(ctx)
	cluster := littleRed.Spec.Cluster
	if cluster == nil {
		cluster = &littleredv1alpha1.ClusterSpec{}
		cluster.SetDefaults()
	}

	// Get password
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

	// Phase 1: Ground Truth - Query CLUSTER MYID from every pod
	podIDMap := make(map[string]string)
	var bestNodeAddr string
	maxKnownNodes := -1

	for i := 0; i < cluster.GetTotalNodes(); i++ {
		podName := fmt.Sprintf("%s-cluster-%d", littleRed.Name, i)
		pod := &corev1.Pod{}
		if err := r.Get(ctx, types.NamespacedName{
			Name:      podName,
			Namespace: littleRed.Namespace,
		}, pod); err != nil || pod.Status.PodIP == "" {
			continue
		}

		addr := fmt.Sprintf("%s:%d", pod.Status.PodIP, littleredv1alpha1.RedisPort)

		// 1. Get ID
		nodeID, err := clusterClient.GetMyID(ctx, addr)
		if err != nil {
			continue // Pod unreachable
		}
		podIDMap[podName] = nodeID

		// 2. Check view quality (how many nodes does it see?)
		info, err := clusterClient.GetClusterInfo(ctx, addr)
		if err == nil {
			if info.KnownNodes > maxKnownNodes {
				maxKnownNodes = info.KnownNodes
				bestNodeAddr = addr
			}
		}
	}

	if len(podIDMap) == 0 {
		return nil, fmt.Errorf("no reachable nodes found")
	}

	// Phase 2: Get Topology from the "Best" node (most connected)
	var nodes []redisclient.ClusterNodeInfo
	if bestNodeAddr != "" {
		var err error
		nodes, err = clusterClient.GetClusterNodes(ctx, bestNodeAddr)
		if err != nil {
			log.Info("Failed to get topology from best node, falling back to basic status", "node", bestNodeAddr)
		}
	}

	// Phase 3: Build State
	states := make([]littleredv1alpha1.ClusterNodeState, 0, len(podIDMap))
	for podName, nodeID := range podIDMap {
		state := littleredv1alpha1.ClusterNodeState{
			PodName: podName,
			NodeID:  nodeID,
			Role:    "unknown",
		}

		// Fill in details from topology if available
		for _, node := range nodes {
			if node.NodeID == nodeID {
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
				break
			}
		}
		states = append(states, state)
	}

	return states, nil
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
			fast, _ := littleRed.GetRequeueIntervals()
			return ctrl.Result{RequeueAfter: fast}, nil
		}
		if pod.Status.PodIP == "" {
			log.Info("Pod has no IP yet", "pod", podName)
			fast, _ := littleRed.GetRequeueIntervals()
			return ctrl.Result{RequeueAfter: fast}, nil
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
			fast, _ := littleRed.GetRequeueIntervals()
			return ctrl.Result{RequeueAfter: fast}, nil
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
			fast, _ := littleRed.GetRequeueIntervals()
			return ctrl.Result{RequeueAfter: fast}, nil
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
			// If slots are busy, verify if this node already owns them
			if strings.Contains(err.Error(), "busy") {
				// We need to check if the node actually has slots assigned
				nodes, nodeErr := clusterClient.GetClusterNodes(ctx, masterAddr)
				if nodeErr != nil {
					log.Error(nodeErr, "Failed to verify slot assignment after busy error", "master", masterPodName)
					fast, _ := littleRed.GetRequeueIntervals()
					return ctrl.Result{RequeueAfter: fast}, nil
				}

				// Find myself
				myID := nodeIDs[masterIdx]
				hasSlots := false
				for _, n := range nodes {
					if n.NodeID == myID && len(n.Slots) > 0 {
						hasSlots = true
						break
					}
				}

				if hasSlots {
					log.Info("Slots already assigned (verified)", "master", masterPodName)
				} else {
					// Slots are busy but not owned by us -> Conflict!
					log.Error(err, "Slots busy but not owned by target master (possible conflict)", "master", masterPodName)
					fast, _ := littleRed.GetRequeueIntervals()
					return ctrl.Result{RequeueAfter: fast}, nil
				}
			} else {
				log.Error(err, "Failed to add slots", "master", masterPodName)
				fast, _ := littleRed.GetRequeueIntervals()
				return ctrl.Result{RequeueAfter: fast}, nil
			}
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
				// Verify if already replicating
				nodes, nodeErr := clusterClient.GetClusterNodes(ctx, replicaAddr)
				if nodeErr != nil {
					log.Error(nodeErr, "Failed to verify replica status after error", "replica", replicaPodName)
					fast, _ := littleRed.GetRequeueIntervals()
					return ctrl.Result{RequeueAfter: fast}, nil
				}

				// Find myself and check master ID
				myID := nodeIDs[replicaIdx]
				isCorrect := false
				for _, n := range nodes {
					if n.NodeID == myID && n.MasterID == masterNodeID {
						isCorrect = true
						break
					}
				}

				if isCorrect {
					log.Info("Replica already assigned (verified)", "replica", replicaPodName)
				} else {
					log.Error(err, "Failed to assign replica", "replica", replicaPodName)
					fast, _ := littleRed.GetRequeueIntervals()
					return ctrl.Result{RequeueAfter: fast}, nil
				}
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
	fast, _ := littleRed.GetRequeueIntervals()
	return ctrl.Result{RequeueAfter: fast}, nil
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
			// Update healthy flag based on actual state
			if actualClusterState != "ok" {
				healthy = false
			}
		} else {
			log.Error(err, "Failed to get cluster info from Redis")
			healthy = false
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
		masterIndices := []string{}
		// Sort nodes by pod name to get consistent ordering
		for i := 0; i < littleRed.Spec.Cluster.GetTotalNodes(); i++ {
			podName := fmt.Sprintf("%s-cluster-%d", littleRed.Name, i)
			for _, node := range littleRed.Status.Cluster.Nodes {
				if node.PodName == podName && node.Role == "master" {
					masterIndices = append(masterIndices, fmt.Sprintf("%d", i))
					break
				}
			}
		}

		masterInfo := ""
		if len(masterIndices) > 0 {
			masterInfo = fmt.Sprintf(" (M: %s)", strings.Join(masterIndices, ","))
		}

		littleRed.Status.Status = fmt.Sprintf("%s%s", littleRed.Status.Cluster.State, masterInfo)
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
		fast, _ := littleRed.GetRequeueIntervals()
		return ctrl.Result{RequeueAfter: fast}, nil
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
		fast, _ := littleRed.GetRequeueIntervals()
		return ctrl.Result{RequeueAfter: fast}, nil
	}

	addr := fmt.Sprintf("%s:%d", pod.Status.PodIP, littleredv1alpha1.RedisPort)
	nodes, err := clusterClient.GetClusterNodes(ctx, addr)
	if err != nil {
		log.Error(err, "Failed to get cluster nodes")
		fast, _ := littleRed.GetRequeueIntervals()
		return ctrl.Result{RequeueAfter: fast}, nil
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
	fast, _ := littleRed.GetRequeueIntervals()
	return ctrl.Result{RequeueAfter: fast}, nil
}
