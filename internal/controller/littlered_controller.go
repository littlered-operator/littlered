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
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"time"

	"github.com/go-logr/logr"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/apiutil"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/source"

	littleredv1alpha1 "github.com/littlered-operator/littlered-operator/api/v1alpha1"
	redisclient "github.com/littlered-operator/littlered-operator/internal/redis"
)

const (
	finalizerName = "chuck-chuck-chuck.net/finalizer"
	fieldManager  = "littlered-operator"
)

// Logging categories
const (
	LogCategoryRecon = "recon" // Steady-state reconciliation
	LogCategoryState = "state" // Observations about cluster state
	LogCategoryAudit = "audit" // Cluster interference actions
)

type SentinelErrorCode int

const (
	SentinelUnreachable SentinelErrorCode = iota
	SentinelNoMaster
	SentinelGhostMaster
)

type SentinelError struct {
	Code    SentinelErrorCode
	Message string
	IP      string
	Err     error
}

func (e *SentinelError) Error() string {
	if e.Err != nil {
		return fmt.Sprintf("%s: %v", e.Message, e.Err)
	}
	return e.Message
}

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
	// Fetch the LittleRed instance
	littleRed := &littleredv1alpha1.LittleRed{}
	if err := r.Get(ctx, req.NamespacedName, littleRed); err != nil {
		if apierrors.IsNotFound(err) {
			logf.FromContext(ctx).Info("LittleRed resource not found, ignoring", "category", LogCategoryRecon)
			return ctrl.Result{}, nil
		}
		logf.FromContext(ctx).Error(err, "Failed to get LittleRed", "category", LogCategoryRecon)
		return ctrl.Result{}, err
	}

	log := r.getLogger(ctx, littleRed, LogCategoryRecon)

	// Apply defaults
	littleRed.SetDefaults()

	// Validate supported constraints for initial release
	if err := littleRed.Validate(); err != nil {
		return r.setFailedStatus(ctx, littleRed, "UnsupportedConfiguration", err.Error())
	}

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
	if littleRed.Spec.Mode != ComponentSentinel {
		r.stopSentinelMonitor(req.NamespacedName)
	}

	// Initialize BootstrapRequired for Sentinel mode
	if littleRed.Spec.Mode == ComponentSentinel && littleRed.Status.Phase == "" && !littleRed.Status.BootstrapRequired {
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
	case ComponentSentinel:
		return r.reconcileSentinel(ctx, littleRed)
	case ComponentCluster:
		return r.reconcileCluster(ctx, littleRed)
	default:
		return r.setFailedStatus(ctx, littleRed, "InvalidMode", fmt.Sprintf("Unknown mode: %s", littleRed.Spec.Mode))
	}
}

// getLogger returns a logger with standard fields and stripped redundancies
func (r *LittleRedReconciler) getLogger(ctx context.Context, lr *littleredv1alpha1.LittleRed, category string) logr.Logger {
	log := logf.FromContext(ctx).
		WithValues(
			"category", category,
		)

	// Note: name and namespace are already included in context by controller-runtime
	// as top-level fields. We add 'category' to enable filtering.

	return log
}

// reconcileDelete handles cleanup when the resource is deleted
func (r *LittleRedReconciler) reconcileDelete(ctx context.Context, littleRed *littleredv1alpha1.LittleRed) (ctrl.Result, error) {
	log := r.getLogger(ctx, littleRed, LogCategoryRecon)
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
	if littleRed.Spec.Mode == ComponentCluster {
		if err := r.validateClusterSpec(littleRed); err != nil {
			return err
		}
	}

	return nil
}

// reconcileStandalone reconciles standalone mode
func (r *LittleRedReconciler) reconcileStandalone(ctx context.Context, littleRed *littleredv1alpha1.LittleRed) (ctrl.Result, error) {
	log := r.getLogger(ctx, littleRed, LogCategoryRecon)
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
	log := r.getLogger(ctx, littleRed, LogCategoryRecon)
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
		log.Info("Not yet Running, requeueing",
			"phase", littleRed.Status.Phase,
			"redis", fmt.Sprintf("%d/%d", littleRed.Status.Redis.Ready, littleRed.Status.Redis.Total))
		return ctrl.Result{RequeueAfter: fast}, nil
	}

	return ctrl.Result{RequeueAfter: steady}, nil
}

// reconcileSentinel reconciles sentinel mode
func (r *LittleRedReconciler) reconcileSentinel(ctx context.Context, littleRed *littleredv1alpha1.LittleRed) (ctrl.Result, error) {
	log := r.getLogger(ctx, littleRed, LogCategoryRecon)
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

	// Unified Sentinel cluster reconciliation
	if err := r.reconcileSentinelCluster(ctx, littleRed); err != nil {
		log.Error(err, "Failed to reconcile Sentinel cluster")
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

// reconcileSentinelCluster gathers ground truth from all pods (Redis and Sentinel)
// and performs atomic healing of the entire cluster state.
//
//nolint:gocyclo
func (r *LittleRedReconciler) reconcileSentinelCluster(ctx context.Context, littleRed *littleredv1alpha1.LittleRed) error {
	log := r.getLogger(ctx, littleRed, LogCategoryRecon)

	// Skip if we haven't bootstrapped yet
	if littleRed.Status.BootstrapRequired {
		return nil
	}

	password := r.getRedisPassword(ctx, littleRed)

	// 1. Gather all living pods
	redisPods := &corev1.PodList{}
	if err := r.List(ctx, redisPods, client.InNamespace(littleRed.Namespace), client.MatchingLabels(redisSelectorLabels(littleRed))); err != nil {
		return err
	}

	sentinelPods := &corev1.PodList{}
	if err := r.List(ctx, sentinelPods, client.InNamespace(littleRed.Namespace), client.MatchingLabels(sentinelSelectorLabels(littleRed))); err != nil {
		return err
	}

	redisMap := make(map[string]string)
	for _, p := range redisPods.Items {
		if p.Status.PodIP != "" && p.DeletionTimestamp.IsZero() {
			redisMap[p.Status.PodIP] = p.Name
		}
	}

	sentinelMap := make(map[string]string)
	for _, p := range sentinelPods.Items {
		if p.Status.PodIP != "" && p.DeletionTimestamp.IsZero() {
			sentinelMap[p.Status.PodIP] = p.Name
		}
	}

	// Any pod terminating? (Important for guardrails)
	anyTerminating := false
	for _, p := range append(redisPods.Items, sentinelPods.Items...) {
		if !p.DeletionTimestamp.IsZero() {
			anyTerminating = true
			break
		}
	}

	// 2. Gather Cluster State (Ground Truth)
	g := &operatorGatherer{password: password, tlsEnabled: littleRed.Spec.TLS.Enabled}
	state := redisclient.GatherClusterState(ctx, g, redisMap, sentinelMap)

	// 3. Healing

	// Rule 0: Re-register sentinel pods that started without a master configured.
	// This happens when a sentinel pod restarts with a new IP: bootstrapRequired is
	// already false so bootstrapSentinel() won't run, yet the new pod starts bare.
	// Sentinel gossip cannot self-heal this — a sentinel with no master configured
	// cannot discover the pubsub channel and therefore never joins the cluster.
	// We detect the condition via Reachable && !Monitoring and issue SENTINEL MONITOR
	// directly to the individual pod IP (not via the headless Service, for the same
	// reason as bootstrap: the Service load-balances to a single backend).
	// This action is safe during any transition because adding a monitor to an
	// unconfigured sentinel is non-disruptive to the running cluster.
	auditLog := r.getLogger(ctx, littleRed, LogCategoryAudit)
	if state.RealMasterIP != "" {
		quorum := 2
		if littleRed.Spec.Sentinel != nil && littleRed.Spec.Sentinel.Quorum > 0 {
			quorum = littleRed.Spec.Sentinel.Quorum
		}
		for ip, sn := range state.SentinelNodes {
			if !sn.Reachable || sn.Monitoring {
				continue
			}
			auditLog.Info("Sentinel pod has no master configured, re-registering",
				"pod", sn.PodName, "ip", ip, "master", state.RealMasterIP)
			podAddr := fmt.Sprintf("%s:%d", ip, littleredv1alpha1.SentinelPort)
			podSC := redisclient.NewSentinelClient([]string{podAddr}, password, littleRed.Spec.TLS.Enabled)
			if err := podSC.Monitor(ctx, redisclient.SentinelMasterName, state.RealMasterIP, littleredv1alpha1.RedisPort, quorum); err != nil {
				auditLog.Error(err, "Failed to re-register sentinel pod", "pod", sn.PodName)
				continue
			}
			if password != "" {
				_ = podSC.Set(ctx, redisclient.SentinelMasterName, "auth-pass", password)
			}
			applySentinelSettings(ctx, podSC, littleRed.Spec.Sentinel)
		}
	}

	// Rule A: Guardrails
	// We skip ALL healing if:
	// 1. Any pod is terminating (K8s is already working).
	// 2. Sentinel reports an active failover (Sentinel is already working).
	if anyTerminating || state.FailoverActive {
		log.Info("Cluster transition in progress. Skipping healing.",
			"anyTerminating", anyTerminating,
			"failoverActive", state.FailoverActive)
		return nil
	}

	// Note: state.RealMasterIP == "" (leaderless) used to be a hard blocker here.
	// We now allow reconciliation to proceed so that Rule A+ (ghost pruning)
	// can clear stuck sentinels even during leaderless periods.

	// Ghost pruning: only safe if the master Sentinel reports is a living pod
	ghostMasterFound := false
	ghostFound := false
	stateLog := r.getLogger(ctx, littleRed, LogCategoryState)

	quorum := 2
	if littleRed.Spec.Sentinel != nil && littleRed.Spec.Sentinel.Quorum > 0 {
		quorum = littleRed.Spec.Sentinel.Quorum
	}

	for ip, sn := range state.SentinelNodes {
		if !sn.Reachable || !sn.Monitoring {
			continue
		}
		// A sentinel still pointing at a ghost master means it lost its failover
		// notification (e.g. two sentinels raced to lead the failover and the
		// "winner" superseded the elected leader before it could record the
		// switch).
		//
		// We REMOVE and re-MONITOR this individual sentinel so it points to the correct
		// master IP.
		// Safety: We ONLY do this if a living master consensus exists. If the cluster
		// is currently leaderless (RealMasterIP == ""), we MUST NOT intervene,
		// because sentinels might be correctly timing out a recently-deceased master.
		if state.RealMasterIP != "" && state.IsGhost(sn.MasterIP) && !state.IsGhost(state.RealMasterIP) && state.RedisNodes[state.RealMasterIP] != nil && state.RedisNodes[state.RealMasterIP].Reachable {
			auditLog.Info("Sentinel monitoring ghost master; re-registering correct master",
				"pod", sn.PodName, "ghost_master", sn.MasterIP, "correct_master", state.RealMasterIP)
			podAddr := fmt.Sprintf("%s:%d", ip, littleredv1alpha1.SentinelPort)
			podSC := redisclient.NewSentinelClient([]string{podAddr}, password, littleRed.Spec.TLS.Enabled)

			_ = podSC.Remove(ctx, redisclient.SentinelMasterName)
			if err := podSC.Monitor(ctx, redisclient.SentinelMasterName, state.RealMasterIP, littleredv1alpha1.RedisPort, quorum); err == nil {
				if password != "" {
					_ = podSC.Set(ctx, redisclient.SentinelMasterName, "auth-pass", password)
				}
				applySentinelSettings(ctx, podSC, littleRed.Spec.Sentinel)
			}

			ghostMasterFound = true
			continue // don't inspect this sentinel's replica list
		}

		// A sentinel monitoring a LIVING but WRONG master must also be re-registered.
		// This requires a consensus on the RealMasterIP.
		if state.RealMasterIP != "" && sn.MasterIP != state.RealMasterIP && !state.IsGhost(state.RealMasterIP) && state.RedisNodes[state.RealMasterIP] != nil && state.RedisNodes[state.RealMasterIP].Reachable {
			auditLog.Info("Sentinel monitoring wrong master IP; re-registering correct master",
				"pod", sn.PodName, "monitored_master", sn.MasterIP, "correct_master", state.RealMasterIP)
			podAddr := fmt.Sprintf("%s:%d", ip, littleredv1alpha1.SentinelPort)
			podSC := redisclient.NewSentinelClient([]string{podAddr}, password, littleRed.Spec.TLS.Enabled)

			_ = podSC.Remove(ctx, redisclient.SentinelMasterName)
			if err := podSC.Monitor(ctx, redisclient.SentinelMasterName, state.RealMasterIP, littleredv1alpha1.RedisPort, quorum); err == nil {
				if password != "" {
					_ = podSC.Set(ctx, redisclient.SentinelMasterName, "auth-pass", password)
				}
				applySentinelSettings(ctx, podSC, littleRed.Spec.Sentinel)
			}

			ghostMasterFound = true // using this flag to trigger requeue
			continue
		}

		// Check replicas for ghost IPs (IPs not belonging to any living pod).
		// We accept s_down here — for ghost replicas, s_down is the correct
		// signal. o_down (objectively down) is never set on replicas by Sentinel;
		// it only applies to the master and requires a quorum vote. Requiring
		// o_down for replicas means the condition is permanently dead.
		//
		// Rule A above ensures we skip this block entirely while any pod is
		// Terminating or a failover is active, giving Sentinel time to finish
		// sending REPLICAOF to surviving replicas before we issue RESET.
		for _, replica := range sn.Replicas {
			if state.IsGhost(replica.IP) && (strings.Contains(replica.Flags, "s_down") || strings.Contains(replica.Flags, "o_down")) {
				stateLog.Info("Ghost node detected in Sentinel topology", "ip", replica.IP, "flags", replica.Flags, "sentinel", sn.PodName)
				ghostFound = true
				break
			}
		}
		if ghostFound {
			break
		}
	}

	if ghostMasterFound {
		// Requeue so the next cycle can verify the sentinels have converged.
		return nil
	}

	// If no master is known yet, we can't do any more healing beyond ghost pruning.
	if state.RealMasterIP == "" {
		return nil
	}

	// Rule D (continued): Prune ghost replicas.
	// Safety: We ONLY issue a global RESET if we have consensus on a living, reachable master.
	// If the master is unreachable or a ghost IP, we MUST NOT issue RESET because it would
	// wipe the Sentinels' failure detection timers (s_down) and block failover.
	if ghostFound && !state.IsGhost(state.RealMasterIP) && state.RedisNodes[state.RealMasterIP] != nil && state.RedisNodes[state.RealMasterIP].Reachable {
		auditLog.Info("Issuing SENTINEL RESET to clear ghost nodes from topology", "master", redisclient.SentinelMasterName)
		sentinelAddresses := r.getSentinelAddresses(ctx, littleRed)
		sc := redisclient.NewSentinelClient(sentinelAddresses, password, littleRed.Spec.TLS.Enabled)
		_ = sc.Reset(ctx, redisclient.SentinelMasterName)
	}

	// Rule R: Replica Rescue
	// Ensure all living Redis pods that are not the consensus master are actually
	// configured as replicas.
	for ip, rn := range state.RedisNodes {
		if !rn.Reachable || ip == state.RealMasterIP {
			continue
		}
		// If the pod thinks it's a master, or is following the wrong master.
		// We DON'T trigger on LinkStatus == "down" alone, because that could be a
		// transient state during a handshake, and re-issuing SLAVEOF would interrupt it.
		if rn.Role == "master" || rn.MasterHost != state.RealMasterIP {
			auditLog.Info("Redis pod is not following the consensus master, issuing SLAVEOF",
				"pod", rn.PodName, "current_role", rn.Role, "target_master", state.RealMasterIP)
			if err := redisclient.SlaveOf(ctx, fmt.Sprintf("%s:%d", ip, littleredv1alpha1.RedisPort), password, state.RealMasterIP, fmt.Sprintf("%d", littleredv1alpha1.RedisPort), littleRed.Spec.TLS.Enabled); err != nil {
				auditLog.Error(err, "Failed to rescue replica", "pod", rn.PodName)
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

	// Also add pod IPs for resilience, but only for healthy pods.
	// Using dead or terminating IPs causes long connection timeouts that hang reconciliation.
	podList := &corev1.PodList{}
	listOpts := []client.ListOption{
		client.InNamespace(littleRed.Namespace),
		client.MatchingLabels(sentinelSelectorLabels(littleRed)),
	}
	if err := r.List(ctx, podList, listOpts...); err == nil {
		for _, pod := range podList.Items {
			// Skip pods that are being deleted or aren't ready yet
			isReady := false
			for _, cond := range pod.Status.Conditions {
				if cond.Type == corev1.PodReady && cond.Status == corev1.ConditionTrue {
					isReady = true
					break
				}
			}

			if pod.Status.PodIP != "" && pod.DeletionTimestamp.IsZero() && isReady {
				addresses = append(addresses, fmt.Sprintf("%s:%d", pod.Status.PodIP, littleredv1alpha1.SentinelPort))
			}
		}
	}

	return addresses
}

// applySentinelSettings applies tunable sentinel configuration parameters to a
// specific sentinel pod for the named master. Must be called after SENTINEL MONITOR
// to apply user-configured thresholds. All Set errors are intentionally swallowed —
// a failure to apply settings is non-fatal and will be retried on the next reconcile.
func applySentinelSettings(ctx context.Context, sc *redisclient.SentinelClient, spec *littleredv1alpha1.SentinelSpec) {
	if spec == nil {
		return
	}
	if spec.DownAfterMilliseconds > 0 {
		_ = sc.Set(ctx, redisclient.SentinelMasterName, "down-after-milliseconds", fmt.Sprintf("%d", spec.DownAfterMilliseconds))
	}
	if spec.FailoverTimeout > 0 {
		_ = sc.Set(ctx, redisclient.SentinelMasterName, "failover-timeout", fmt.Sprintf("%d", spec.FailoverTimeout))
	}
	if spec.ParallelSyncs > 0 {
		_ = sc.Set(ctx, redisclient.SentinelMasterName, "parallel-syncs", fmt.Sprintf("%d", spec.ParallelSyncs))
	}
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

	sc := redisclient.NewSentinelClient(addresses, password, littleRed.Spec.TLS.Enabled)
	masterInfo, err := sc.GetMaster(checkCtx)
	if err != nil {
		// If Sentinel explicitly says "no master", it's a confirmed state
		if strings.Contains(err.Error(), "redis: nil") {
			return "", &SentinelError{
				Code:    SentinelNoMaster,
				Message: "Sentinel explicitly reported no master monitored",
			}
		}
		// Otherwise it's a connection/unreachable error
		return "", &SentinelError{
			Code:    SentinelUnreachable,
			Message: "Failed to reach any Sentinel or get master info",
			Err:     err,
		}
	}

	// masterInfo.IP MUST be an IP address in our strict identity model.
	reportedIdentity := masterInfo.IP

	// Find pod with matching IP
	for _, pod := range podList.Items {
		if pod.Status.PodIP == reportedIdentity {
			return pod.Name, nil
		}
	}

	// Reported master IP not found in current pod list -> Ghost Master
	return "", &SentinelError{
		Code:    SentinelGhostMaster,
		Message: fmt.Sprintf("Sentinel reported master IP %q not found in pod list", reportedIdentity),
		IP:      reportedIdentity,
	}
}

// updateMasterLabel updates the role labels on Redis pods based on current master
func (r *LittleRedReconciler) updateMasterLabel(ctx context.Context, littleRed *littleredv1alpha1.LittleRed) error {
	log := r.getLogger(ctx, littleRed, LogCategoryRecon)

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

	// Skip label updates during pod transitions to avoid churn during failovers
	for _, pod := range podList.Items {
		if !pod.DeletionTimestamp.IsZero() {
			log.Info("Pod terminating, skipping label update to avoid churn during failover", "pod", pod.Name)
			return nil
		}
	}

	stateLog := r.getLogger(ctx, littleRed, LogCategoryState)
	masterPodName, err := r.getMasterPodName(ctx, littleRed, podList)
	if err != nil {
		var sErr *SentinelError
		if errors.As(err, &sErr) {
			switch sErr.Code {
			case SentinelUnreachable:
				log.Info("Sentinel unreachable, skipping label update to avoid churn", "error", sErr.Err)
				return nil
			case SentinelNoMaster:
				stateLog.Info("Sentinel confirms no master is currently monitored. Ensuring no pod is labeled as master.")
				masterPodName = ""
			case SentinelGhostMaster:
				stateLog.Info("Sentinel reported a ghost master. Ensuring no pod is labeled as master.", "ghost_ip", sErr.IP)
				masterPodName = ""
			}
		} else {
			log.Error(err, "Unexpected error identifying master pod, skipping label update")
			return nil
		}
	}

	// surgical role updates:
	// 1. If we have a masterPodName, ensure ONLY that pod is labeled Master.
	// 2. If we DON'T have a masterPodName, ensure NO pod is labeled Master.
	// 3. We only change Replica/Orphan labels if we are sure of the state.
	//    During failover (masterPodName == ""), we just strip the Master label
	//    from whoever had it and leave others alone.

	auditLog := r.getLogger(ctx, littleRed, LogCategoryAudit)
	for i := range podList.Items {
		pod := &podList.Items[i]
		currentRole := pod.Labels[LabelRole]
		expectedRole := currentRole // default: stay as you are

		if masterPodName != "" {
			// We have a consensus master. Ensure ALL pods have correct labels.
			if pod.Name == masterPodName {
				expectedRole = RoleMaster
			} else {
				expectedRole = RoleReplica
			}
		} else {
			// No master identified by Sentinel (failover in progress).
			// Be surgical: only strip the Master label if someone has it.
			if currentRole == RoleMaster {
				expectedRole = RoleReplica // downgrade to replica while waiting
			}
		}

		if currentRole != expectedRole {
			auditLog.Info("Updating pod role label", "pod", pod.Name, "old_role", currentRole, "new_role", expectedRole)
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
//
//nolint:gocyclo
func (r *LittleRedReconciler) updateSentinelStatus(ctx context.Context, lr *littleredv1alpha1.LittleRed) (ctrl.Result, error) {
	log := r.getLogger(ctx, lr, LogCategoryRecon)
	stateLog := r.getLogger(ctx, lr, LogCategoryState)

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
			stateLog.Info("Sentinel unreachable or master unknown, reporting no master in status", "error", err)
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

		// Count healthy replicas as seen by Sentinel. A slave is "up" if it is
		// not marked s_down, o_down, or disconnected. This prevents reporting
		// Running while Sentinel has not yet polled the master and registered all
		// replicas — which would let a caller (test or operator) trigger a
		// failover before Sentinel knows every slave, leaving replicas stuck on
		// the dead master IP.
		sentinelReplicasOK := 0
		if masterPodName != "" {
			password := r.getRedisPassword(ctx, latest)
			sc := redisclient.NewSentinelClient(r.getSentinelAddresses(ctx, latest), password, latest.Spec.TLS.Enabled)
			if replicas, err := sc.GetReplicas(ctx, redisclient.SentinelMasterName); err == nil {
				for _, rep := range replicas {
					if !strings.Contains(rep.Flags, "s_down") &&
						!strings.Contains(rep.Flags, "o_down") &&
						!strings.Contains(rep.Flags, "disconnected") {
						sentinelReplicasOK++
					}
				}
			}
		}

		// Determine phase
		expectedReplicas := int(latest.Status.Redis.Total) - 1
		allReady := latest.Status.Redis.Ready == latest.Status.Redis.Total &&
			latest.Status.Sentinels.Ready == latest.Status.Sentinels.Total &&
			latest.Status.Redis.Ready > 0 &&
			masterPodName != "" &&
			sentinelReplicasOK >= expectedReplicas

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
			// Build a human-readable breakdown of which condition is blocking Running.
			var notReadyReasons []string
			if latest.Status.Redis.Ready == 0 {
				notReadyReasons = append(notReadyReasons, "no Redis pods ready")
			} else if latest.Status.Redis.Ready != latest.Status.Redis.Total {
				notReadyReasons = append(notReadyReasons, fmt.Sprintf("Redis pods %d/%d ready", latest.Status.Redis.Ready, latest.Status.Redis.Total))
			}
			if latest.Status.Sentinels.Ready != latest.Status.Sentinels.Total {
				notReadyReasons = append(notReadyReasons, fmt.Sprintf("Sentinel pods %d/%d ready", latest.Status.Sentinels.Ready, latest.Status.Sentinels.Total))
			}
			if masterPodName == "" {
				notReadyReasons = append(notReadyReasons, "master not yet known to Sentinel")
			}
			if masterPodName != "" && sentinelReplicasOK < expectedReplicas {
				notReadyReasons = append(notReadyReasons, fmt.Sprintf("Sentinel knows %d/%d replicas as healthy", sentinelReplicasOK, expectedReplicas))
			}
			log.Info("Not yet Running, requeueing", "reasons", strings.Join(notReadyReasons, "; "))

			latest.Status.Phase = littleredv1alpha1.PhaseInitializing
			meta.SetStatusCondition(&latest.Status.Conditions, metav1.Condition{
				Type:               littleredv1alpha1.ConditionReady,
				Status:             metav1.ConditionFalse,
				Reason:             "PodsNotReady",
				Message:            fmt.Sprintf("Redis: %d/%d, Sentinels: %d/%d, Sentinel-known replicas: %d/%d", latest.Status.Redis.Ready, latest.Status.Redis.Total, latest.Status.Sentinels.Ready, latest.Status.Sentinels.Total, sentinelReplicasOK, expectedReplicas),
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
	if latest.Annotations[AnnotationDisablePolling] == annotationTrue {
		log.Info("Sentinel polling disabled via annotation")
		return ctrl.Result{}, nil
	}

	return ctrl.Result{RequeueAfter: steady}, nil
}

// setFailedStatus sets the LittleRed status to Failed
func (r *LittleRedReconciler) setFailedStatus(ctx context.Context, lr *littleredv1alpha1.LittleRed, reason, message string) (ctrl.Result, error) {
	log := r.getLogger(ctx, lr, LogCategoryRecon)
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
		For(&littleredv1alpha1.LittleRed{}, builder.WithPredicates(predicate.GenerationChangedPredicate{})).
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
	return r.Patch(ctx, obj, client.Apply, client.FieldOwner(fieldManager), client.ForceOwnership) //nolint:staticcheck
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
	log := r.getLogger(ctx, lr, LogCategoryRecon)

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
	password := r.getRedisPassword(ctx, lr)

	quorum := 2
	if lr.Spec.Sentinel != nil && lr.Spec.Sentinel.Quorum > 0 {
		quorum = lr.Spec.Sentinel.Quorum
	}
	masterAddr := pod0.Status.PodIP

	// 4. Bootstrap each Sentinel pod individually.
	//
	// CRITICAL: We must NOT use the headless Service VIP for MONITOR because the
	// Service load-balances to a single pod. Only that one pod would receive the
	// MONITOR command; the other two would start with an empty config and never
	// reach quorum. We iterate over pod IPs and configure each sentinel directly.
	sentinelPods := &corev1.PodList{}
	if err := r.List(ctx, sentinelPods,
		client.InNamespace(lr.Namespace),
		client.MatchingLabels(sentinelSelectorLabels(lr)),
	); err != nil {
		return fmt.Errorf("failed to list sentinel pods: %w", err)
	}

	auditLog := r.getLogger(ctx, lr, LogCategoryAudit)
	configuredCount := 0
	for i := range sentinelPods.Items {
		pod := &sentinelPods.Items[i]
		if pod.Status.PodIP == "" || !pod.DeletionTimestamp.IsZero() {
			continue
		}
		podAddr := fmt.Sprintf("%s:%d", pod.Status.PodIP, littleredv1alpha1.SentinelPort)
		podSC := redisclient.NewSentinelClient([]string{podAddr}, password, lr.Spec.TLS.Enabled)

		// Check if this sentinel already knows the master (idempotent guard).
		if info, err := podSC.GetMaster(ctx); err == nil && info != nil {
			log.Info("Bootstrap: Sentinel already has master configured", "sentinel", pod.Name, "master", info.IP)
			configuredCount++
			continue
		}

		auditLog.Info("Bootstrap: issuing SENTINEL MONITOR to pod", "sentinel", pod.Name, "master", masterAddr)
		if err := podSC.Monitor(ctx, redisclient.SentinelMasterName, masterAddr, littleredv1alpha1.RedisPort, quorum); err != nil {
			auditLog.Error(err, "Bootstrap: failed to configure sentinel", "sentinel", pod.Name)
			continue // best-effort; don't abort the whole bootstrap
		}

		if password != "" {
			_ = podSC.Set(ctx, redisclient.SentinelMasterName, "auth-pass", password)
		}

		// Apply settings to this sentinel.
		applySentinelSettings(ctx, podSC, lr.Spec.Sentinel)
		configuredCount++
	}

	if configuredCount == 0 {
		log.Info("Bootstrap: no Sentinel pods reachable yet, will retry on next reconcile")
		return nil
	}
	if configuredCount < len(sentinelPods.Items) {
		log.Info("Bootstrap: not all sentinels configured yet", "configured", configuredCount, "total", len(sentinelPods.Items))
		// Continue anyway — the sentinel gossip protocol will propagate the config to the others.
	}

	// 5. Clear bootstrap flag with retry on conflict
	auditLog.Info("Bootstrap: initial master registered, clearing bootstrapRequired flag")
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
