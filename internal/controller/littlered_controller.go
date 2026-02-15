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
	"sync"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/apiutil"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/source"

	littleredv1alpha1 "github.com/tanne3/littlered-operator/api/v1alpha1"
	redisclient "github.com/tanne3/littlered-operator/internal/redis"
)

const (
	finalizerName = "chuck-chuck-chuck.net/finalizer"
	fieldManager  = "littlered-operator"
)

// LittleRedReconciler reconciles a LittleRed object
type LittleRedReconciler struct {
	client.Client
	Scheme *runtime.Scheme

	// Sentinel monitoring
	sentinelEvents chan event.GenericEvent
	monitors       map[types.NamespacedName]func()
	monitorsMu     sync.Mutex
}

// +kubebuilder:rbac:groups=chuck-chuck-chuck.net,resources=littlereds,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=chuck-chuck-chuck.net,resources=littlereds/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=chuck-chuck-chuck.net,resources=littlereds/finalizers,verbs=update
// +kubebuilder:rbac:groups=core,resources=configmaps,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=core,resources=services,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=core,resources=secrets,verbs=get;list;watch
// +kubebuilder:rbac:groups=core,resources=pods,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=apps,resources=statefulsets,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=monitoring.coreos.com,resources=servicemonitors,verbs=get;list;watch;create;update;patch;delete

// Reconcile is the main reconciliation loop
func (r *LittleRedReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	// Fetch the LittleRed instance
	littleRed := &littleredv1alpha1.LittleRed{}
	if err := r.Get(ctx, req.NamespacedName, littleRed); err != nil {
		if apierrors.IsNotFound(err) {
			log.Info("LittleRed resource not found, ignoring")
			return ctrl.Result{}, nil
		}
		log.Error(err, "Failed to get LittleRed")
		return ctrl.Result{}, err
	}

	// Apply defaults
	littleRed.SetDefaults()

	// Handle deletion
	if !littleRed.DeletionTimestamp.IsZero() {
		return r.reconcileDelete(ctx, littleRed)
	}

	// Add finalizer if not present
	if !controllerutil.ContainsFinalizer(littleRed, finalizerName) {
		controllerutil.AddFinalizer(littleRed, finalizerName)
		if err := r.Update(ctx, littleRed); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{Requeue: true}, nil
	}

	// Validate spec
	if err := r.validateSpec(ctx, littleRed); err != nil {
		return r.setFailedStatus(ctx, littleRed, "ValidationFailed", err.Error())
	}

	// Set ConfigValid condition
	meta.SetStatusCondition(&littleRed.Status.Conditions, metav1.Condition{
		Type:               littleredv1alpha1.ConditionConfigValid,
		Status:             metav1.ConditionTrue,
		Reason:             "ConfigurationValid",
		Message:            "Configuration validated successfully",
		LastTransitionTime: metav1.Now(),
	})

	// Reconcile based on mode
	if littleRed.Spec.Mode != "sentinel" {
		r.stopSentinelMonitor(req.NamespacedName)
	}

	// Initialize BootstrapRequired for Sentinel mode
	if littleRed.Spec.Mode == "sentinel" && littleRed.Status.Phase == "" && !littleRed.Status.BootstrapRequired {
		log.Info("Initializing new Sentinel cluster: setting bootstrapRequired flag")
		err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
			latest := &littleredv1alpha1.LittleRed{}
			if err := r.Get(ctx, req.NamespacedName, latest); err != nil {
				return err
			}
			if latest.Status.Phase != "" || latest.Status.BootstrapRequired {
				return nil // Already initialized by another pass
			}
			latest.Status.BootstrapRequired = true
			return r.Status().Update(ctx, latest)
		})
		if err != nil {
			return ctrl.Result{}, fmt.Errorf("failed to initialize bootstrap flag: %w", err)
		}
		// Re-fetch to continue with the updated object
		if err := r.Get(ctx, req.NamespacedName, littleRed); err != nil {
			return ctrl.Result{}, err
		}
	}

	switch littleRed.Spec.Mode {
	case "standalone":
		return r.reconcileStandalone(ctx, littleRed)
	case "sentinel":
		return r.reconcileSentinel(ctx, littleRed)
	case "cluster":
		return r.reconcileCluster(ctx, littleRed)
	default:
		return r.setFailedStatus(ctx, littleRed, "InvalidMode", fmt.Sprintf("Unknown mode: %s", littleRed.Spec.Mode))
	}
}

// reconcileDelete handles cleanup when the resource is deleted
func (r *LittleRedReconciler) reconcileDelete(ctx context.Context, littleRed *littleredv1alpha1.LittleRed) (ctrl.Result, error) {
	log := logf.FromContext(ctx)
	log.Info("Reconciling delete")

	// Update phase
	littleRed.Status.Phase = littleredv1alpha1.PhaseTerminating
	if err := r.Status().Update(ctx, littleRed); err != nil {
		if !apierrors.IsConflict(err) {
			return ctrl.Result{}, err
		}
	}

	// Remove finalizer (owned resources will be garbage collected)
	controllerutil.RemoveFinalizer(littleRed, finalizerName)
	if err := r.Update(ctx, littleRed); err != nil {
		return ctrl.Result{}, err
	}

	// Stop sentinel monitor if running
	r.stopSentinelMonitor(types.NamespacedName{
		Name:      littleRed.Name,
		Namespace: littleRed.Namespace,
	})

	return ctrl.Result{}, nil
}

// validateSpec validates the LittleRed spec
func (r *LittleRedReconciler) validateSpec(ctx context.Context, littleRed *littleredv1alpha1.LittleRed) error {
	// Validate auth
	if littleRed.Spec.Auth.Enabled {
		if littleRed.Spec.Auth.ExistingSecret == "" {
			return fmt.Errorf("auth.enabled is true but auth.existingSecret is not set")
		}
		// Verify secret exists
		secret := &corev1.Secret{}
		if err := r.Get(ctx, types.NamespacedName{
			Name:      littleRed.Spec.Auth.ExistingSecret,
			Namespace: littleRed.Namespace,
		}, secret); err != nil {
			if apierrors.IsNotFound(err) {
				return fmt.Errorf("auth secret %q not found", littleRed.Spec.Auth.ExistingSecret)
			}
			return err
		}
		if _, ok := secret.Data["password"]; !ok {
			return fmt.Errorf("auth secret %q does not contain 'password' key", littleRed.Spec.Auth.ExistingSecret)
		}
	}

	// Validate TLS
	if littleRed.Spec.TLS.Enabled {
		if littleRed.Spec.TLS.ExistingSecret == "" {
			return fmt.Errorf("tls.enabled is true but tls.existingSecret is not set")
		}
		// Verify TLS secret exists
		secret := &corev1.Secret{}
		if err := r.Get(ctx, types.NamespacedName{
			Name:      littleRed.Spec.TLS.ExistingSecret,
			Namespace: littleRed.Namespace,
		}, secret); err != nil {
			if apierrors.IsNotFound(err) {
				return fmt.Errorf("TLS secret %q not found", littleRed.Spec.TLS.ExistingSecret)
			}
			return err
		}
		if _, ok := secret.Data["tls.crt"]; !ok {
			return fmt.Errorf("TLS secret %q does not contain 'tls.crt' key", littleRed.Spec.TLS.ExistingSecret)
		}
		if _, ok := secret.Data["tls.key"]; !ok {
			return fmt.Errorf("TLS secret %q does not contain 'tls.key' key", littleRed.Spec.TLS.ExistingSecret)
		}

		// Validate client auth
		if littleRed.Spec.TLS.ClientAuth {
			if littleRed.Spec.TLS.CACertSecret == "" {
				return fmt.Errorf("tls.clientAuth is true but tls.caCertSecret is not set")
			}
			caSecret := &corev1.Secret{}
			if err := r.Get(ctx, types.NamespacedName{
				Name:      littleRed.Spec.TLS.CACertSecret,
				Namespace: littleRed.Namespace,
			}, caSecret); err != nil {
				if apierrors.IsNotFound(err) {
					return fmt.Errorf("CA certificate secret %q not found", littleRed.Spec.TLS.CACertSecret)
				}
				return err
			}
			if _, ok := caSecret.Data["ca.crt"]; !ok {
				return fmt.Errorf("CA certificate secret %q does not contain 'ca.crt' key", littleRed.Spec.TLS.CACertSecret)
			}
		}
	}

	// Validate cluster config
	if littleRed.Spec.Mode == "cluster" {
		if err := r.validateClusterSpec(littleRed); err != nil {
			return err
		}
	}

	return nil
}

// reconcileStandalone reconciles standalone mode
func (r *LittleRedReconciler) reconcileStandalone(ctx context.Context, littleRed *littleredv1alpha1.LittleRed) (ctrl.Result, error) {
	log := logf.FromContext(ctx)
	log.Info("Reconciling standalone mode")

	// Set initial phase
	if littleRed.Status.Phase == "" {
		littleRed.Status.Phase = littleredv1alpha1.PhasePending
	}

	// Reconcile ConfigMap
	if err := r.reconcileConfigMap(ctx, littleRed); err != nil {
		log.Error(err, "Failed to reconcile ConfigMap")
		return ctrl.Result{}, err
	}

	// Reconcile StatefulSet
	if err := r.reconcileStatefulSet(ctx, littleRed); err != nil {
		log.Error(err, "Failed to reconcile StatefulSet")
		return ctrl.Result{}, err
	}

	// Reconcile Service
	if err := r.reconcileService(ctx, littleRed); err != nil {
		log.Error(err, "Failed to reconcile Service")
		return ctrl.Result{}, err
	}

	// Reconcile ServiceMonitor if enabled
	if littleRed.Spec.Metrics.IsEnabled() && littleRed.Spec.Metrics.ServiceMonitor.Enabled {
		if err := r.reconcileServiceMonitor(ctx, littleRed); err != nil {
			log.Error(err, "Failed to reconcile ServiceMonitor")
			// Don't fail reconciliation if ServiceMonitor fails (CRD might not be installed)
		}
	}

	// Update status
	return r.updateStatus(ctx, littleRed)
}

// reconcileConfigMap ensures the ConfigMap exists with the correct content
func (r *LittleRedReconciler) reconcileConfigMap(ctx context.Context, littleRed *littleredv1alpha1.LittleRed) error {
	return r.apply(ctx, littleRed, buildConfigMap(littleRed))
}

// reconcileStatefulSet ensures the StatefulSet exists with the correct spec
func (r *LittleRedReconciler) reconcileStatefulSet(ctx context.Context, littleRed *littleredv1alpha1.LittleRed) error {
	return r.apply(ctx, littleRed, buildStatefulSet(littleRed))
}

// reconcileService ensures the Service exists with the correct spec
func (r *LittleRedReconciler) reconcileService(ctx context.Context, littleRed *littleredv1alpha1.LittleRed) error {
	return r.apply(ctx, littleRed, buildService(littleRed))
}

// reconcileServiceMonitor ensures the ServiceMonitor exists
func (r *LittleRedReconciler) reconcileServiceMonitor(ctx context.Context, littleRed *littleredv1alpha1.LittleRed) error {
	return r.apply(ctx, littleRed, buildServiceMonitor(littleRed))
}

// updateStatus updates the LittleRed status based on current state
func (r *LittleRedReconciler) updateStatus(ctx context.Context, littleRed *littleredv1alpha1.LittleRed) (ctrl.Result, error) {
	log := logf.FromContext(ctx)
	oldStatus := littleRed.Status.DeepCopy()

	// Get StatefulSet status
	sts := &appsv1.StatefulSet{}
	stsName := fmt.Sprintf("%s-redis", littleRed.Name)
	if err := r.Get(ctx, types.NamespacedName{Name: stsName, Namespace: littleRed.Namespace}, sts); err != nil {
		if apierrors.IsNotFound(err) {
			littleRed.Status.Phase = littleredv1alpha1.PhasePending
			littleRed.Status.Redis.Ready = 0
			littleRed.Status.Redis.Total = 1
		} else {
			return ctrl.Result{}, err
		}
	} else {
		littleRed.Status.Redis.Ready = sts.Status.ReadyReplicas
		littleRed.Status.Redis.Total = *sts.Spec.Replicas

		// Determine phase
		if sts.Status.ReadyReplicas == *sts.Spec.Replicas && sts.Status.ReadyReplicas > 0 {
			littleRed.Status.Phase = littleredv1alpha1.PhaseRunning
			meta.SetStatusCondition(&littleRed.Status.Conditions, metav1.Condition{
				Type:               littleredv1alpha1.ConditionReady,
				Status:             metav1.ConditionTrue,
				Reason:             "AllPodsReady",
				Message:            "All pods are ready",
				LastTransitionTime: metav1.Now(),
			})
			meta.SetStatusCondition(&littleRed.Status.Conditions, metav1.Condition{
				Type:               littleredv1alpha1.ConditionInitialized,
				Status:             metav1.ConditionTrue,
				Reason:             "Initialized",
				Message:            "Redis is initialized",
				LastTransitionTime: metav1.Now(),
			})
		} else if sts.Status.ReadyReplicas > 0 {
			littleRed.Status.Phase = littleredv1alpha1.PhaseInitializing
			meta.SetStatusCondition(&littleRed.Status.Conditions, metav1.Condition{
				Type:               littleredv1alpha1.ConditionReady,
				Status:             metav1.ConditionFalse,
				Reason:             "PodsNotReady",
				Message:            fmt.Sprintf("%d/%d pods ready", sts.Status.ReadyReplicas, *sts.Spec.Replicas),
				LastTransitionTime: metav1.Now(),
			})
		} else {
			littleRed.Status.Phase = littleredv1alpha1.PhaseInitializing
			meta.SetStatusCondition(&littleRed.Status.Conditions, metav1.Condition{
				Type:               littleredv1alpha1.ConditionReady,
				Status:             metav1.ConditionFalse,
				Reason:             "PodsNotReady",
				Message:            "Waiting for pods to start",
				LastTransitionTime: metav1.Now(),
			})
		}
	}

	// Update observed generation
	littleRed.Status.ObservedGeneration = littleRed.Generation

	// Update high-level status summary
	if littleRed.Status.Phase == littleredv1alpha1.PhaseRunning {
		littleRed.Status.Status = "Ready"
	} else {
		littleRed.Status.Status = string(littleRed.Status.Phase)
	}

	// Update status if changed
	if !reflect.DeepEqual(oldStatus, littleRed.Status) {
		if err := r.Status().Update(ctx, littleRed); err != nil {
			if apierrors.IsConflict(err) {
				log.Info("Status update conflict, requeueing")
				return ctrl.Result{RequeueAfter: time.Second}, nil
			}
			return ctrl.Result{}, err
		}
	}

	fast, steady := littleRed.GetRequeueIntervals()

	// Requeue if not running to check status
	if littleRed.Status.Phase != littleredv1alpha1.PhaseRunning {
		return ctrl.Result{RequeueAfter: fast}, nil
	}

	return ctrl.Result{RequeueAfter: steady}, nil
}

// reconcileSentinel reconciles sentinel mode
func (r *LittleRedReconciler) reconcileSentinel(ctx context.Context, littleRed *littleredv1alpha1.LittleRed) (ctrl.Result, error) {
	log := logf.FromContext(ctx)
	log.Info("Reconciling sentinel mode")

	// Set initial phase
	if littleRed.Status.Phase == "" {
		littleRed.Status.Phase = littleredv1alpha1.PhasePending
	}

	// Reconcile Redis ConfigMap
	if err := r.reconcileConfigMapSentinel(ctx, littleRed); err != nil {
		log.Error(err, "Failed to reconcile Redis ConfigMap")
		return ctrl.Result{}, err
	}

	// Reconcile Sentinel ConfigMap
	if err := r.reconcileSentinelConfigMap(ctx, littleRed); err != nil {
		log.Error(err, "Failed to reconcile Sentinel ConfigMap")
		return ctrl.Result{}, err
	}

	// Reconcile headless service for Redis (needed before StatefulSet)
	if err := r.reconcileReplicasService(ctx, littleRed); err != nil {
		log.Error(err, "Failed to reconcile replicas Service")
		return ctrl.Result{}, err
	}

	// Reconcile headless service for Sentinel
	if err := r.reconcileSentinelService(ctx, littleRed); err != nil {
		log.Error(err, "Failed to reconcile Sentinel Service")
		return ctrl.Result{}, err
	}

	// Reconcile Redis StatefulSet
	if err := r.reconcileRedisStatefulSetSentinel(ctx, littleRed); err != nil {
		log.Error(err, "Failed to reconcile Redis StatefulSet")
		return ctrl.Result{}, err
	}

	// Reconcile Sentinel StatefulSet
	if err := r.reconcileSentinelStatefulSet(ctx, littleRed); err != nil {
		log.Error(err, "Failed to reconcile Sentinel StatefulSet")
		return ctrl.Result{}, err
	}

	// Reconcile master Service (points to current master)
	if err := r.reconcileMasterService(ctx, littleRed); err != nil {
		log.Error(err, "Failed to reconcile master Service")
		return ctrl.Result{}, err
	}

	// Bootstrap Sentinel if required
	if littleRed.Status.BootstrapRequired {
		if err := r.bootstrapSentinel(ctx, littleRed); err != nil {
			log.Error(err, "Failed to bootstrap Sentinel")
			return ctrl.Result{}, err
		}
	}

	// Update pod labels to reflect current master
	if err := r.updateMasterLabel(ctx, littleRed); err != nil {
		log.Error(err, "Failed to update master labels")
		// Don't fail - this is best effort
	}

	// Clean up ghost nodes from Sentinel topology
	if err := r.reconcileSentinelTopology(ctx, littleRed); err != nil {
		log.Error(err, "Failed to reconcile Sentinel topology")
	}

	// Rescue zombie replicas
	if err := r.reconcileRedisReplicas(ctx, littleRed); err != nil {
		log.Error(err, "Failed to reconcile Redis replicas")
	}

	// Reconcile ServiceMonitor if enabled
	if littleRed.Spec.Metrics.IsEnabled() && littleRed.Spec.Metrics.ServiceMonitor.Enabled {
		if err := r.reconcileServiceMonitor(ctx, littleRed); err != nil {
			log.Error(err, "Failed to reconcile ServiceMonitor")
		}
	}

	// Ensure background sentinel monitoring is running
	r.ensureSentinelMonitor(ctx, littleRed)

	// Update status
	return r.updateSentinelStatus(ctx, littleRed)
}

// reconcileConfigMapSentinel ensures the Redis ConfigMap exists for sentinel mode
func (r *LittleRedReconciler) reconcileConfigMapSentinel(ctx context.Context, littleRed *littleredv1alpha1.LittleRed) error {
	return r.apply(ctx, littleRed, buildConfigMapSentinelMode(littleRed))
}

// reconcileSentinelConfigMap ensures the Sentinel ConfigMap exists
func (r *LittleRedReconciler) reconcileSentinelConfigMap(ctx context.Context, littleRed *littleredv1alpha1.LittleRed) error {
	return r.apply(ctx, littleRed, buildSentinelConfigMap(littleRed))
}

// reconcileReplicasService ensures the headless service for Redis pods exists
func (r *LittleRedReconciler) reconcileReplicasService(ctx context.Context, littleRed *littleredv1alpha1.LittleRed) error {
	return r.apply(ctx, littleRed, buildReplicasHeadlessService(littleRed))
}

// reconcileSentinelService ensures the headless service for Sentinel pods exists
func (r *LittleRedReconciler) reconcileSentinelService(ctx context.Context, littleRed *littleredv1alpha1.LittleRed) error {
	return r.apply(ctx, littleRed, buildSentinelHeadlessService(littleRed))
}

// reconcileRedisStatefulSetSentinel ensures the Redis StatefulSet exists for sentinel mode
func (r *LittleRedReconciler) reconcileRedisStatefulSetSentinel(ctx context.Context, littleRed *littleredv1alpha1.LittleRed) error {
	return r.apply(ctx, littleRed, buildRedisStatefulSetSentinel(littleRed))
}

// reconcileSentinelStatefulSet ensures the Sentinel StatefulSet exists
func (r *LittleRedReconciler) reconcileSentinelStatefulSet(ctx context.Context, littleRed *littleredv1alpha1.LittleRed) error {
	return r.apply(ctx, littleRed, buildSentinelStatefulSet(littleRed))
}

// reconcileMasterService ensures the master Service exists
func (r *LittleRedReconciler) reconcileMasterService(ctx context.Context, littleRed *littleredv1alpha1.LittleRed) error {
	return r.apply(ctx, littleRed, buildMasterService(littleRed))
}

// reconcileSentinelTopology ensures Sentinel's internal view matches K8s Pod list.
// In strict IP-identity mode, old pod IPs are "ghosts" that will never return.
func (r *LittleRedReconciler) reconcileSentinelTopology(ctx context.Context, littleRed *littleredv1alpha1.LittleRed) error {
	log := logf.FromContext(ctx)

	// Skip if we haven't bootstrapped yet
	if littleRed.Status.BootstrapRequired {
		return nil
	}

	podList := &corev1.PodList{}
	listOpts := []client.ListOption{
		client.InNamespace(littleRed.Namespace),
		client.MatchingLabels(redisSelectorLabels(littleRed)),
	}
	if err := r.List(ctx, podList, listOpts...); err != nil {
		return err
	}

	// Build map of current valid Pod IPs
	validIPs := make(map[string]bool)
	for _, pod := range podList.Items {
		if pod.Status.PodIP != "" {
			validIPs[pod.Status.PodIP] = true
		}
	}

	addresses := r.getSentinelAddresses(ctx, littleRed)
	password := ""
	if littleRed.Spec.Auth.Enabled {
		password = r.getRedisPassword(ctx, littleRed)
	}

	sc := redisclient.NewSentinelClient(addresses, password)

	// 1. Ensure all Sentinels are monitoring the master
	// When a Sentinel restarts, it has a new IP and starts idle. We must re-introduce it.
	masterInfo, err := sc.GetMaster(ctx)
	if err != nil {
		return fmt.Errorf("failed to get master for topology sync: %w", err)
	}

	// Iterate all sentinel pods and check if they are monitoring
	for _, addr := range addresses {
		monitoring, err := sc.IsMonitoring(ctx, addr, redisclient.SentinelMasterName)
		if err != nil {
			log.V(1).Info("Failed to check if sentinel is monitoring", "addr", addr, "error", err)
			continue
		}
		if !monitoring && validIPs[masterInfo.IP] {
			log.Info("Sentinel is idle, introducing master", "sentinel", addr, "master", masterInfo.IP)
			// Use the common Monitor/Set logic
			if err := sc.Monitor(ctx, redisclient.SentinelMasterName, masterInfo.IP, littleredv1alpha1.RedisPort, 2); err != nil {
				log.Error(err, "Failed to introduce master to idle sentinel", "sentinel", addr)
				continue
			}
			// Re-apply standard settings
			sentinel := littleRed.Spec.Sentinel
			if sentinel == nil {
				sentinel = &littleredv1alpha1.SentinelSpec{}
			}
			sc.Set(ctx, redisclient.SentinelMasterName, "down-after-milliseconds", fmt.Sprintf("%d", sentinel.DownAfterMilliseconds))
			sc.Set(ctx, redisclient.SentinelMasterName, "failover-timeout", fmt.Sprintf("%d", sentinel.FailoverTimeout))
			sc.Set(ctx, redisclient.SentinelMasterName, "parallel-syncs", "1")
		}
	}

	// 2. Check if Master is valid (not a ghost)
	if !validIPs[masterInfo.IP] {
		log.Info("Sentinel master is a ghost node, skipping RESET until failover completes", "ip", masterInfo.IP)
		return nil
	}

	// 3. Get replicas and check for ghosts
	replicas, err := sc.GetReplicas(ctx, redisclient.SentinelMasterName)
	if err != nil {
		return fmt.Errorf("failed to get replicas for topology sync: %w", err)
	}

	ghostFound := false
	validReplicaCount := 0
	for _, replica := range replicas {
		if !validIPs[replica.IP] {
			// Only consider ghosts that are already marked as DOWN
			if strings.Contains(replica.Flags, "s_down") || strings.Contains(replica.Flags, "o_down") {
				log.Info("Ghost node detected in Sentinel topology", "ip", replica.IP, "flags", replica.Flags)
				ghostFound = true
			}
		} else {
			validReplicaCount++
		}
	}

	// 3. Issue RESET only if it's safe:
	// We found a ghost, AND the master is valid.
	// We don't require validReplicaCount == 2 because a stale topology might be
	// preventing discovery of the living replicas. RESET will force re-discovery
	// from the (verified living) master.
	if ghostFound {
		log.Info("Issuing SENTINEL RESET to clear ghost nodes", "master", redisclient.SentinelMasterName)
		if err := sc.Reset(ctx, redisclient.SentinelMasterName); err != nil {
			return fmt.Errorf("failed to reset sentinel topology: %w", err)
		}
	}

	return nil
}

// reconcileRedisReplicas ensures that all living Redis pods are actually replicas of the current master.
// This rescues "zombie replicas" that started up pointing to a ghost master IP.
func (r *LittleRedReconciler) reconcileRedisReplicas(ctx context.Context, littleRed *littleredv1alpha1.LittleRed) error {
	log := logf.FromContext(ctx)

	// 1. Get current master IP from Sentinel
	addresses := r.getSentinelAddresses(ctx, littleRed)
	password := ""
	if littleRed.Spec.Auth.Enabled {
		password = r.getRedisPassword(ctx, littleRed)
	}
	sc := redisclient.NewSentinelClient(addresses, password)
	masterInfo, err := sc.GetMaster(ctx)
	if err != nil {
		return nil // Can't reconcile without a known master
	}

	// 2. List all Redis pods
	podList := &corev1.PodList{}
	listOpts := []client.ListOption{
		client.InNamespace(littleRed.Namespace),
		client.MatchingLabels(redisSelectorLabels(littleRed)),
	}
	if err := r.List(ctx, podList, listOpts...); err != nil {
		return err
	}

	// 3. Verify that the master reported by Sentinel is actually a living pod.
	// If the master is a ghost, we MUST NOT rescue replicas towards it, as that
	// would sabotage Sentinel's attempt to perform a failover.
	masterIsGhost := true
	for _, pod := range podList.Items {
		if pod.Status.PodIP == masterInfo.IP {
			masterIsGhost = false
			break
		}
	}

	if masterIsGhost {
		log.Info("Sentinel reported master is a ghost node, skipping replica rescue until failover completes", "ip", masterInfo.IP)
		return nil
	}

	// 4. Verify and fix replicas
	for _, pod := range podList.Items {
		if pod.Status.PodIP == "" || pod.Status.PodIP == masterInfo.IP {
			continue // Skip pod without IP or the master itself
		}

		// Check what this pod thinks its master is
		addr := fmt.Sprintf("%s:%d", pod.Status.PodIP, littleredv1alpha1.RedisPort)
		role, currentMasterHost, _, err := redisclient.GetReplicationInfo(ctx, addr, password)
		if err != nil {
			log.V(1).Info("Failed to get replication info", "pod", pod.Name, "error", err)
			continue
		}

		if role == "master" || currentMasterHost != masterInfo.IP {
			log.Info("Rescuing zombie replica: reconfiguring pod to follow real master",
				"pod", pod.Name, "wrong_master", currentMasterHost, "real_master", masterInfo.IP)
			if err := redisclient.SlaveOf(ctx, addr, password, masterInfo.IP, fmt.Sprintf("%d", littleredv1alpha1.RedisPort)); err != nil {
				log.Error(err, "Failed to issue SLAVEOF command to zombie replica", "pod", pod.Name)
			}
		}
	}

	return nil
}

// getSentinelAddresses returns a list of Sentinel addresses to try (Service FQDN and pod IPs)
func (r *LittleRedReconciler) getSentinelAddresses(ctx context.Context, littleRed *littleredv1alpha1.LittleRed) []string {
	addresses := []string{
		fmt.Sprintf("%s-sentinel.%s.svc:%d",
			littleRed.Name, littleRed.Namespace, littleredv1alpha1.SentinelPort),
	}

	// Also add pod IPs for resilience
	podList := &corev1.PodList{}
	listOpts := []client.ListOption{
		client.InNamespace(littleRed.Namespace),
		client.MatchingLabels(sentinelSelectorLabels(littleRed)),
	}
	if err := r.List(ctx, podList, listOpts...); err == nil {
		for _, pod := range podList.Items {
			if pod.Status.PodIP != "" {
				addresses = append(addresses, fmt.Sprintf("%s:%d", pod.Status.PodIP, littleredv1alpha1.SentinelPort))
			}
		}
	}

	return addresses
}

// getMasterPodName queries Sentinel to find the current master pod name.
// Returns an error if Sentinel query fails.
func (r *LittleRedReconciler) getMasterPodName(ctx context.Context, littleRed *littleredv1alpha1.LittleRed, podList *corev1.PodList) (string, error) {
	// Try to get real master from Sentinel
	addresses := r.getSentinelAddresses(ctx, littleRed)

	password := ""
	if littleRed.Spec.Auth.Enabled {
		password = r.getRedisPassword(ctx, littleRed)
	}

	// Use a short timeout for the check
	checkCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()

	sc := redisclient.NewSentinelClient(addresses, password)
	masterInfo, err := sc.GetMaster(checkCtx)
	if err != nil {
		// If Sentinel explicitly says "no master", it's not a failure we should error on
		if strings.Contains(err.Error(), "redis: nil") {
			return "", nil
		}
		return "", fmt.Errorf("failed to get master from Sentinel: %w", err)
	}

	// masterInfo.IP MUST be an IP address in our strict identity model.
	reportedIdentity := masterInfo.IP

	// Find pod with matching IP
	for _, pod := range podList.Items {
		if pod.Status.PodIP == reportedIdentity {
			return pod.Name, nil
		}
	}

	return "", fmt.Errorf("sentinel reported master IP %q not found in pod list", reportedIdentity)
}

// updateMasterLabel updates the role labels on Redis pods based on current master
func (r *LittleRedReconciler) updateMasterLabel(ctx context.Context, littleRed *littleredv1alpha1.LittleRed) error {
	log := logf.FromContext(ctx)

	// List all Redis pods
	podList := &corev1.PodList{}
	listOpts := []client.ListOption{
		client.InNamespace(littleRed.Namespace),
		client.MatchingLabels(redisSelectorLabels(littleRed)),
	}
	if err := r.List(ctx, podList, listOpts...); err != nil {
		return err
	}

	if len(podList.Items) == 0 {
		return nil // No pods yet
	}

	masterPodName, err := r.getMasterPodName(ctx, littleRed, podList)
	if err != nil {
		// If we can't get the master from Sentinel, keep current labels to avoid churn
		log.Info("Sentinel unreachable or master unknown, skipping label update", "error", err)
		return nil
	}

	// Safety: if masterPodName is empty, something is wrong with logic, but we must NOT
	// proceed to relabel everything as replicas.
	if masterPodName == "" {
		log.Info("WARNING: Sentinel reported no master, but GetMaster returned success with empty name. Skipping label update to maintain safety.")
		return nil
	}

	// Log master change if detected. Use Status as the "last known state"
	// since the physical pod for the old master might have been deleted.
	lastKnownMaster := ""
	if littleRed.Status.Master != nil {
		lastKnownMaster = littleRed.Status.Master.PodName
	}

	if lastKnownMaster != "" && lastKnownMaster != masterPodName {
		log.Info("Master switch detected", "oldMaster", lastKnownMaster, "newMaster", masterPodName)
	}

	for i := range podList.Items {
		pod := &podList.Items[i]
		currentRole := pod.Labels[LabelRole]
		expectedRole := RoleReplica

		if pod.Name == masterPodName {
			expectedRole = RoleMaster
		}

		if currentRole != expectedRole {
			log.Info("Updating pod role label", "pod", pod.Name, "role", expectedRole)
			if pod.Labels == nil {
				pod.Labels = make(map[string]string)
			}
			pod.Labels[LabelRole] = expectedRole
			if err := r.Update(ctx, pod); err != nil {
				return err
			}
		}
	}

	return nil
}

// updateSentinelStatus updates the LittleRed status for sentinel mode
func (r *LittleRedReconciler) updateSentinelStatus(ctx context.Context, lr *littleredv1alpha1.LittleRed) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		latest := &littleredv1alpha1.LittleRed{}
		if err := r.Get(ctx, types.NamespacedName{Name: lr.Name, Namespace: lr.Namespace}, latest); err != nil {
			return err
		}
		oldStatus := latest.Status.DeepCopy()

		// Get Redis StatefulSet status
		redisSts := &appsv1.StatefulSet{}
		if err := r.Get(ctx, types.NamespacedName{
			Name:      fmt.Sprintf("%s-redis", latest.Name),
			Namespace: latest.Namespace,
		}, redisSts); err != nil {
			if !apierrors.IsNotFound(err) {
				return err
			}
			latest.Status.Redis.Ready = 0
			latest.Status.Redis.Total = 3
		} else {
			latest.Status.Redis.Ready = redisSts.Status.ReadyReplicas
			latest.Status.Redis.Total = *redisSts.Spec.Replicas
		}

		// Get Sentinel StatefulSet status
		sentinelSts := &appsv1.StatefulSet{}
		if err := r.Get(ctx, types.NamespacedName{
			Name:      fmt.Sprintf("%s-sentinel", latest.Name),
			Namespace: latest.Namespace,
		}, sentinelSts); err != nil {
			if !apierrors.IsNotFound(err) {
				return err
			}
			if latest.Status.Sentinels == nil {
				latest.Status.Sentinels = &littleredv1alpha1.SentinelStatus{}
			}
			latest.Status.Sentinels.Ready = 0
			latest.Status.Sentinels.Total = 3
		} else {
			if latest.Status.Sentinels == nil {
				latest.Status.Sentinels = &littleredv1alpha1.SentinelStatus{}
			}
			latest.Status.Sentinels.Ready = sentinelSts.Status.ReadyReplicas
			latest.Status.Sentinels.Total = *sentinelSts.Spec.Replicas
		}

		// Set replicas status (Redis pods - 1 master = replicas)
		if latest.Status.Replicas == nil {
			latest.Status.Replicas = &littleredv1alpha1.ReplicaStatus{}
		}
		if latest.Status.Redis.Ready > 0 {
			latest.Status.Replicas.Ready = latest.Status.Redis.Ready - 1
		} else {
			latest.Status.Replicas.Ready = 0
		}
		latest.Status.Replicas.Total = latest.Status.Redis.Total - 1

		// Set master info
		if latest.Status.Master == nil {
			latest.Status.Master = &littleredv1alpha1.MasterStatus{}
		}

		// List Redis pods to find the master IP
		podList := &corev1.PodList{}
		listOpts := []client.ListOption{
			client.InNamespace(latest.Namespace),
			client.MatchingLabels(redisSelectorLabels(latest)),
		}
		_ = r.List(ctx, podList, listOpts...)

		masterPodName, err := r.getMasterPodName(ctx, latest, podList)
		if err != nil {
			log.Info("Sentinel unreachable or master unknown, reporting no master in status", "error", err)
			masterPodName = ""
		}
		latest.Status.Master.PodName = masterPodName

		// Try to get master pod IP
		if masterPodName != "" {
			masterPod := &corev1.Pod{}
			if err := r.Get(ctx, types.NamespacedName{
				Name:      masterPodName,
				Namespace: latest.Namespace,
			}, masterPod); err == nil {
				latest.Status.Master.IP = masterPod.Status.PodIP
			}
		} else {
			latest.Status.Master.IP = ""
		}
		// Determine phase
		allReady := latest.Status.Redis.Ready == latest.Status.Redis.Total &&
			latest.Status.Sentinels.Ready == latest.Status.Sentinels.Total &&
			latest.Status.Redis.Ready > 0 &&
			masterPodName != ""

		if allReady {
			latest.Status.Phase = littleredv1alpha1.PhaseRunning
			// If we reach Running phase, initial bootstrap is definitely complete
			latest.Status.BootstrapRequired = false

			meta.SetStatusCondition(&latest.Status.Conditions, metav1.Condition{
				Type:               littleredv1alpha1.ConditionReady,
				Status:             metav1.ConditionTrue,
				Reason:             "AllPodsReady",
				Message:            "All Redis and Sentinel pods are ready",
				LastTransitionTime: metav1.Now(),
			})
			meta.SetStatusCondition(&latest.Status.Conditions, metav1.Condition{
				Type:               littleredv1alpha1.ConditionSentinelReady,
				Status:             metav1.ConditionTrue,
				Reason:             "QuorumEstablished",
				Message:            "Sentinel quorum is established",
				LastTransitionTime: metav1.Now(),
			})
			meta.SetStatusCondition(&latest.Status.Conditions, metav1.Condition{
				Type:               littleredv1alpha1.ConditionInitialized,
				Status:             metav1.ConditionTrue,
				Reason:             "Initialized",
				Message:            "Redis sentinel cluster is initialized",
				LastTransitionTime: metav1.Now(),
			})
		} else {
			latest.Status.Phase = littleredv1alpha1.PhaseInitializing
			meta.SetStatusCondition(&latest.Status.Conditions, metav1.Condition{
				Type:               littleredv1alpha1.ConditionReady,
				Status:             metav1.ConditionFalse,
				Reason:             "PodsNotReady",
				Message:            fmt.Sprintf("Redis: %d/%d, Sentinels: %d/%d", latest.Status.Redis.Ready, latest.Status.Redis.Total, latest.Status.Sentinels.Ready, latest.Status.Sentinels.Total),
				LastTransitionTime: metav1.Now(),
			})
		}

		// Update observed generation
		latest.Status.ObservedGeneration = latest.Generation

		// Update high-level status summary
		if latest.Status.Phase == littleredv1alpha1.PhaseRunning {
			latest.Status.Status = latest.Status.Master.PodName
		} else {
			latest.Status.Status = string(latest.Status.Phase)
		}

		// Update status if changed
		if !reflect.DeepEqual(oldStatus, &latest.Status) {
			return r.Status().Update(ctx, latest)
		}
		return nil
	})

	if err != nil {
		return ctrl.Result{}, err
	}

	// Re-fetch to get current phase/annotations for requeue logic
	latest := &littleredv1alpha1.LittleRed{}
	if err := r.Get(ctx, types.NamespacedName{Name: lr.Name, Namespace: lr.Namespace}, latest); err != nil {
		return ctrl.Result{}, err
	}

	fast, steady := latest.GetRequeueIntervals()

	// Requeue if not running
	if latest.Status.Phase != littleredv1alpha1.PhaseRunning {
		return ctrl.Result{RequeueAfter: fast}, nil
	}

	// Periodically requeue to update master info, unless disabled via annotation
	if latest.Annotations[AnnotationDisablePolling] == "true" {
		log.Info("Sentinel polling disabled via annotation")
		return ctrl.Result{}, nil
	}

	return ctrl.Result{RequeueAfter: steady}, nil
}

// setFailedStatus sets the LittleRed status to Failed
func (r *LittleRedReconciler) setFailedStatus(ctx context.Context, lr *littleredv1alpha1.LittleRed, reason, message string) (ctrl.Result, error) {
	log := logf.FromContext(ctx)
	log.Error(fmt.Errorf("%s", message), "Validation failed", "reason", reason)

	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		latest := &littleredv1alpha1.LittleRed{}
		if err := r.Get(ctx, types.NamespacedName{Name: lr.Name, Namespace: lr.Namespace}, latest); err != nil {
			return err
		}

		latest.Status.Phase = littleredv1alpha1.PhaseFailed
		latest.Status.ObservedGeneration = latest.Generation
		meta.SetStatusCondition(&latest.Status.Conditions, metav1.Condition{
			Type:               littleredv1alpha1.ConditionConfigValid,
			Status:             metav1.ConditionFalse,
			Reason:             reason,
			Message:            message,
			LastTransitionTime: metav1.Now(),
		})
		meta.SetStatusCondition(&latest.Status.Conditions, metav1.Condition{
			Type:               littleredv1alpha1.ConditionReady,
			Status:             metav1.ConditionFalse,
			Reason:             reason,
			Message:            message,
			LastTransitionTime: metav1.Now(),
		})

		return r.Status().Update(ctx, latest)
	})

	if err != nil {
		return ctrl.Result{}, err
	}

	// Don't requeue - wait for spec change
	return ctrl.Result{}, nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *LittleRedReconciler) SetupWithManager(mgr ctrl.Manager) error {
	r.sentinelEvents = make(chan event.GenericEvent)
	r.monitors = make(map[types.NamespacedName]func())

	return ctrl.NewControllerManagedBy(mgr).
		For(&littleredv1alpha1.LittleRed{}).
		Owns(&corev1.ConfigMap{}).
		Owns(&corev1.Service{}).
		Owns(&appsv1.StatefulSet{}).
		WatchesRawSource(source.Channel(r.sentinelEvents, &handler.EnqueueRequestForObject{})).
		Named("littlered").
		Complete(r)
}

// apply uses Server-Side Apply to create or update a resource. It sets the
// controller reference and resolves the GVK from the scheme before patching.
// SSA only manages fields the operator explicitly sets, preserving external
// labels, annotations (e.g. kubectl rollout restart), and server-defaulted
// fields like ClusterIP.
func (r *LittleRedReconciler) apply(ctx context.Context, owner *littleredv1alpha1.LittleRed, obj client.Object) error {
	if err := controllerutil.SetControllerReference(owner, obj, r.Scheme); err != nil {
		return err
	}
	gvk, err := apiutil.GVKForObject(obj, r.Scheme)
	if err != nil {
		return err
	}
	obj.GetObjectKind().SetGroupVersionKind(gvk)
	return r.Patch(ctx, obj, client.Apply, client.FieldOwner(fieldManager), client.ForceOwnership)
}

// getRedisPassword retrieves the Redis password from the secret if auth is enabled
func (r *LittleRedReconciler) getRedisPassword(ctx context.Context, littleRed *littleredv1alpha1.LittleRed) string {
	if !littleRed.Spec.Auth.Enabled {
		return ""
	}
	secret := &corev1.Secret{}
	if err := r.Get(ctx, types.NamespacedName{
		Name:      littleRed.Spec.Auth.ExistingSecret,
		Namespace: littleRed.Namespace,
	}, secret); err != nil {
		return ""
	}
	return string(secret.Data["password"])
}

// validateClusterSpec validates cluster-specific configuration
func (r *LittleRedReconciler) validateClusterSpec(littleRed *littleredv1alpha1.LittleRed) error {
	cluster := littleRed.Spec.Cluster
	if cluster == nil {
		return nil
	}

	if cluster.Shards < 3 {
		return fmt.Errorf("cluster.shards must be at least 3, got %d", cluster.Shards)
	}

	if cluster.ReplicasPerShard != nil && *cluster.ReplicasPerShard < 0 {
		return fmt.Errorf("cluster.replicasPerShard cannot be negative, got %d", *cluster.ReplicasPerShard)
	}

	return nil
}

// bootstrapSentinel configures Sentinels to monitor the initial master
func (r *LittleRedReconciler) bootstrapSentinel(ctx context.Context, lr *littleredv1alpha1.LittleRed) error {
	log := logf.FromContext(ctx)

	// 1. Just-in-Time API Check: Re-fetch the object to ensure another worker hasn't already bootstrapped
	latest := &littleredv1alpha1.LittleRed{}
	if err := r.Get(ctx, types.NamespacedName{Name: lr.Name, Namespace: lr.Namespace}, latest); err != nil {
		return err
	}
	if !latest.Status.BootstrapRequired {
		log.Info("Bootstrap: flag already cleared in latest API version, skipping")
		*lr = *latest // Update local copy
		return nil
	}

	// 2. Ensure redis-0 has an IP (required for bootstrap)
	pod0 := &corev1.Pod{}
	pod0Name := fmt.Sprintf("%s-redis-0", lr.Name)
	if err := r.Get(ctx, types.NamespacedName{Name: pod0Name, Namespace: lr.Namespace}, pod0); err != nil {
		return err
	}

	if pod0.Status.PodIP == "" {
		log.Info("Bootstrap: waiting for redis-0 to have an IP before configuring Sentinel")
		return nil
	}

	// 3. Configure Sentinel Client
	addresses := r.getSentinelAddresses(ctx, lr)
	password := r.getRedisPassword(ctx, lr)
	sc := redisclient.NewSentinelClient(addresses, password)

	// 4. Just-in-Time Sentinel Check: Ask Sentinel if mymaster is already registered
	// This handles cases where the operator crashed after MONITOR but before status update.
	if info, err := sc.GetMaster(ctx); err == nil && info != nil {
		log.Info("Bootstrap: Sentinel already has a master registered, proceeding to clear flag", "master", info.IP)
	} else {
		// Only if Sentinel is fresh do we issue the MONITOR command
		quorum := 2
		if lr.Spec.Sentinel != nil && lr.Spec.Sentinel.Quorum > 0 {
			quorum = lr.Spec.Sentinel.Quorum
		}

		// Use Pod IP for initial bootstrap to avoid DNS races.
		masterAddr := pod0.Status.PodIP
		log.Info("Bootstrap: configuring Sentinel to monitor initial master", "master", masterAddr)

		if err := sc.Monitor(ctx, "mymaster", masterAddr, littleredv1alpha1.RedisPort, quorum); err != nil {
			return fmt.Errorf("failed to issue SENTINEL MONITOR: %w", err)
		}

		// Apply settings
		if lr.Spec.Sentinel != nil {
			s := lr.Spec.Sentinel
			if s.DownAfterMilliseconds > 0 {
				_ = sc.Set(ctx, "mymaster", "down-after-milliseconds", fmt.Sprintf("%d", s.DownAfterMilliseconds))
			}
			if s.FailoverTimeout > 0 {
				_ = sc.Set(ctx, "mymaster", "failover-timeout", fmt.Sprintf("%d", s.FailoverTimeout))
			}
			if s.ParallelSyncs > 0 {
				_ = sc.Set(ctx, "mymaster", "parallel-syncs", fmt.Sprintf("%d", s.ParallelSyncs))
			}
		}
	}

	// 5. Clear bootstrap flag with retry on conflict
	log.Info("Bootstrap: initial master registered, clearing bootstrapRequired flag")
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		latestUpdate := &littleredv1alpha1.LittleRed{}
		if err := r.Get(ctx, types.NamespacedName{Name: lr.Name, Namespace: lr.Namespace}, latestUpdate); err != nil {
			return err
		}
		if !latestUpdate.Status.BootstrapRequired {
			return nil // Already done
		}
		latestUpdate.Status.BootstrapRequired = false
		return r.Status().Update(ctx, latestUpdate)
	})
	if err != nil {
		return fmt.Errorf("failed to clear bootstrap flag: %w", err)
	}

	// Update the local object version to avoid subsequent conflicts in the same reconcile pass
	return r.Get(ctx, types.NamespacedName{Name: lr.Name, Namespace: lr.Namespace}, lr)
}
