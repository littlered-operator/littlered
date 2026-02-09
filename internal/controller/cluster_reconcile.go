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

		// Update status to Initializing
		littleRed.Status.Phase = littleredv1alpha1.PhaseInitializing
		littleRed.Status.Redis.Ready = sts.Status.ReadyReplicas
		littleRed.Status.Redis.Total = *sts.Spec.Replicas

		if err := r.Status().Update(ctx, littleRed); err != nil {
			return ctrl.Result{}, err
		}

		fast, _ := littleRed.GetRequeueIntervals()
		return ctrl.Result{RequeueAfter: fast}, nil
	}

	// 4. All pods are ready. Check cluster health.
	// We use the first pod to check the cluster state.
	pod0Name := fmt.Sprintf("%s-cluster-0", littleRed.Name)
	pod0 := &corev1.Pod{}
	if err := r.Get(ctx, types.NamespacedName{Name: pod0Name, Namespace: littleRed.Namespace}, pod0); err != nil {
		return ctrl.Result{}, err
	}

	password := r.getRedisPassword(ctx, littleRed)
	clusterClient := redisclient.NewClusterClient(password)
	addr0 := fmt.Sprintf("%s:%d", pod0.Status.PodIP, littleredv1alpha1.RedisPort)

	info, err := clusterClient.GetClusterInfo(ctx, addr0)

	needsBootstrap := false
	if err != nil {
		// Could be transient or not initialized
		log.Info("Failed to get cluster info, assuming bootstrap needed", "error", err)
		needsBootstrap = true
	} else {
		// If cluster state is not OK or we don't know enough nodes, we might need to bootstrap/heal
		if info.State != "ok" || info.KnownNodes < int(expectedReplicas) {
			log.Info("Cluster state not OK or nodes missing", "state", info.State, "known", info.KnownNodes, "expected", expectedReplicas)
			needsBootstrap = true
		}
	}

	if needsBootstrap {
		return r.bootstrapCluster(ctx, littleRed)
	}

	// 5. Cluster is running and healthy
	return r.updateClusterStatus(ctx, littleRed)
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

	// Check Cluster Info from Pod 0
	pod0Name := fmt.Sprintf("%s-cluster-0", littleRed.Name)
	pod0 := &corev1.Pod{}
	if err := r.Get(ctx, types.NamespacedName{Name: pod0Name, Namespace: littleRed.Namespace}, pod0); err == nil {
		password := r.getRedisPassword(ctx, littleRed)
		clusterClient := redisclient.NewClusterClient(password)
		addr0 := fmt.Sprintf("%s:%d", pod0.Status.PodIP, littleredv1alpha1.RedisPort)

		info, err := clusterClient.GetClusterInfo(ctx, addr0)
		if err == nil {
			if littleRed.Status.Cluster == nil {
				littleRed.Status.Cluster = &littleredv1alpha1.ClusterStatusInfo{}
			}
			littleRed.Status.Cluster.State = info.State

			if info.State == "ok" {
				nodes, err := clusterClient.GetClusterNodes(ctx, addr0)
				if err == nil {
					nodeStates := make([]littleredv1alpha1.ClusterNodeState, 0)
					for _, n := range nodes {
						role := "master"
						if n.IsReplica() {
							role = "replica"
						}
						nodeStates = append(nodeStates, littleredv1alpha1.ClusterNodeState{
							NodeID: n.NodeID,
							Role:   role,
						})
					}
					littleRed.Status.Cluster.Nodes = nodeStates
				}
			}
		}
	}

	// Determine high level phase
	if littleRed.Status.Redis.Ready == littleRed.Status.Redis.Total &&
		littleRed.Status.Cluster != nil &&
		littleRed.Status.Cluster.State == "ok" {

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

	steady, _ := littleRed.GetRequeueIntervals()
	return ctrl.Result{RequeueAfter: steady}, nil
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
