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
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"

	monitoringv1 "github.com/prometheus-operator/prometheus-operator/pkg/apis/monitoring/v1"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"

	littleredv1alpha1 "github.com/tanne3/littlered-operator/api/v1alpha1"
)

// Annotation keys for config hash
const (
	AnnotationConfigHash              = "littlered.chuck-chuck-chuck.net/config-hash"
	AnnotationDisablePolling          = "littlered.chuck-chuck-chuck.net/disable-polling"
	AnnotationDisableEventMonitoring  = "littlered.chuck-chuck-chuck.net/disable-event-monitoring"
	AnnotationDebugSkipSlotAssignment = "littlered.chuck-chuck-chuck.net/debug-skip-slot-assignment"
)

// Resource name helpers
func configMapName(lr *littleredv1alpha1.LittleRed) string {
	return fmt.Sprintf("%s-config", lr.Name)
}

func sentinelConfigMapName(lr *littleredv1alpha1.LittleRed) string {
	return fmt.Sprintf("%s-sentinel-config", lr.Name)
}

func statefulSetName(lr *littleredv1alpha1.LittleRed) string {
	return fmt.Sprintf("%s-redis", lr.Name)
}

func sentinelStatefulSetName(lr *littleredv1alpha1.LittleRed) string {
	return fmt.Sprintf("%s-sentinel", lr.Name)
}

func serviceName(lr *littleredv1alpha1.LittleRed) string {
	return lr.Name
}

func replicasServiceName(lr *littleredv1alpha1.LittleRed) string {
	return fmt.Sprintf("%s-replicas", lr.Name)
}

func sentinelServiceName(lr *littleredv1alpha1.LittleRed) string {
	return fmt.Sprintf("%s-sentinel", lr.Name)
}

func clusterStatefulSetName(lr *littleredv1alpha1.LittleRed) string {
	return fmt.Sprintf("%s-cluster", lr.Name)
}

func clusterHeadlessServiceName(lr *littleredv1alpha1.LittleRed) string {
	return fmt.Sprintf("%s-cluster", lr.Name)
}

func serviceMonitorName(lr *littleredv1alpha1.LittleRed) string {
	return lr.Name
}

// Label keys
const (
	LabelRole   = "littlered.chuck-chuck-chuck.net/role"
	RoleMaster  = "master"
	RoleReplica = "replica"
)

// computeConfigHash computes a SHA256 hash of the ConfigMap data
// This is used to trigger pod restarts when config changes
func computeConfigHash(data map[string]string) string {
	h := sha256.New()
	// Sort keys for deterministic output
	keys := make([]string, 0, len(data))
	for k := range data {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		h.Write([]byte(k))
		h.Write([]byte(data[k]))
	}
	return hex.EncodeToString(h.Sum(nil))[:16] // Use first 16 chars for brevity
}

// commonLabels returns the standard labels applied to all resources
func commonLabels(lr *littleredv1alpha1.LittleRed) map[string]string {
	return map[string]string{
		"app.kubernetes.io/name":               "littlered",
		"app.kubernetes.io/instance":           lr.Name,
		"app.kubernetes.io/managed-by":         "littlered-operator",
		"app.kubernetes.io/version":            lr.Spec.Image.Tag,
		"littlered.chuck-chuck-chuck.net/mode": lr.Spec.Mode,
	}
}

// selectorLabels returns labels used for selecting pods
func selectorLabels(lr *littleredv1alpha1.LittleRed) map[string]string {
	return map[string]string{
		"app.kubernetes.io/name":     "littlered",
		"app.kubernetes.io/instance": lr.Name,
	}
}

// redisSelectorLabels returns labels for selecting Redis pods
func redisSelectorLabels(lr *littleredv1alpha1.LittleRed) map[string]string {
	return map[string]string{
		"app.kubernetes.io/name":      "littlered",
		"app.kubernetes.io/instance":  lr.Name,
		"app.kubernetes.io/component": "redis",
	}
}

// sentinelSelectorLabels returns labels for selecting Sentinel pods
func sentinelSelectorLabels(lr *littleredv1alpha1.LittleRed) map[string]string {
	return map[string]string{
		"app.kubernetes.io/name":      "littlered",
		"app.kubernetes.io/instance":  lr.Name,
		"app.kubernetes.io/component": "sentinel",
	}
}

// masterSelectorLabels returns labels for selecting the master pod
func masterSelectorLabels(lr *littleredv1alpha1.LittleRed) map[string]string {
	return map[string]string{
		"app.kubernetes.io/name":      "littlered",
		"app.kubernetes.io/instance":  lr.Name,
		"app.kubernetes.io/component": "redis",
		LabelRole:                     RoleMaster,
	}
}

// clusterSelectorLabels returns labels for selecting cluster pods
func clusterSelectorLabels(lr *littleredv1alpha1.LittleRed) map[string]string {
	return map[string]string{
		"app.kubernetes.io/name":      "littlered",
		"app.kubernetes.io/instance":  lr.Name,
		"app.kubernetes.io/component": "cluster",
	}
}

// buildConfigMap creates the ConfigMap for redis.conf
func buildConfigMap(lr *littleredv1alpha1.LittleRed) *corev1.ConfigMap {
	return &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      configMapName(lr),
			Namespace: lr.Namespace,
			Labels:    commonLabels(lr),
		},
		Data: map[string]string{
			"redis.conf": buildRedisConfig(lr),
		},
	}
}

// buildRedisConfig generates the redis.conf content
func buildRedisConfig(lr *littleredv1alpha1.LittleRed) string {
	var sb strings.Builder

	// Basic settings
	sb.WriteString("# LittleRed generated configuration\n")
	sb.WriteString("bind 0.0.0.0\n")
	sb.WriteString(fmt.Sprintf("port %d\n", littleredv1alpha1.RedisPort))
	sb.WriteString("dir /data\n")

	// Disable persistence (pure cache)
	sb.WriteString("\n# Persistence disabled (cache mode)\n")
	sb.WriteString("save \"\"\n")
	sb.WriteString("appendonly no\n")

	// Memory settings
	sb.WriteString("\n# Memory configuration\n")
	maxmemory := lr.CalculateMaxmemory()
	sb.WriteString(fmt.Sprintf("maxmemory %s\n", maxmemory))
	sb.WriteString(fmt.Sprintf("maxmemory-policy %s\n", lr.GetEffectiveMaxmemoryPolicy()))

	// Timeout settings
	sb.WriteString("\n# Connection settings\n")
	sb.WriteString(fmt.Sprintf("timeout %d\n", lr.Spec.Config.Timeout))
	sb.WriteString(fmt.Sprintf("tcp-keepalive %d\n", lr.Spec.Config.TCPKeepalive))

	// TLS settings
	if lr.Spec.TLS.Enabled {
		sb.WriteString("\n# TLS configuration\n")
		sb.WriteString(fmt.Sprintf("tls-port %d\n", littleredv1alpha1.RedisPort))
		sb.WriteString("port 0\n") // Disable non-TLS port
		sb.WriteString("tls-cert-file /tls/tls.crt\n")
		sb.WriteString("tls-key-file /tls/tls.key\n")
		if lr.Spec.TLS.ClientAuth {
			sb.WriteString("tls-ca-cert-file /tls/ca.crt\n")
			sb.WriteString("tls-auth-clients yes\n")
		} else {
			sb.WriteString("tls-auth-clients no\n")
		}
	}

	// Auth settings (requirepass is set via command line args to reference secret)
	// The actual password is mounted as env var and used with --requirepass

	// Raw config (expert mode)
	if lr.Spec.Config.Raw != "" {
		sb.WriteString("\n# Custom configuration\n")
		sb.WriteString(lr.Spec.Config.Raw)
		if !strings.HasSuffix(lr.Spec.Config.Raw, "\n") {
			sb.WriteString("\n")
		}
	}

	return sb.String()
}

// buildStatefulSet creates the StatefulSet for Redis
func buildStatefulSet(lr *littleredv1alpha1.LittleRed) *appsv1.StatefulSet {
	labels := commonLabels(lr)
	labels["app.kubernetes.io/component"] = "redis"

	podLabels := make(map[string]string)
	for k, v := range selectorLabels(lr) {
		podLabels[k] = v
	}
	podLabels["app.kubernetes.io/component"] = "redis"
	// Add user-defined pod labels
	for k, v := range lr.Spec.PodTemplate.Labels {
		podLabels[k] = v
	}

	// Compute config hash for pod annotations to trigger rolling update on config change
	configData := map[string]string{"redis.conf": buildRedisConfig(lr)}
	configHash := computeConfigHash(configData)

	podAnnotations := make(map[string]string)
	for k, v := range lr.Spec.PodTemplate.Annotations {
		podAnnotations[k] = v
	}
	podAnnotations[AnnotationConfigHash] = configHash

	replicas := int32(1)

	containers := []corev1.Container{buildRedisContainer(lr)}

	// Add exporter sidecar if metrics enabled
	if lr.Spec.Metrics.IsEnabled() {
		containers = append(containers, buildExporterContainer(lr))
	}

	sts := &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      statefulSetName(lr),
			Namespace: lr.Namespace,
			Labels:    labels,
		},
		Spec: appsv1.StatefulSetSpec{
			Replicas:    &replicas,
			ServiceName: serviceName(lr),
			Selector: &metav1.LabelSelector{
				MatchLabels: selectorLabels(lr),
			},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels:      podLabels,
					Annotations: podAnnotations,
				},
				Spec: corev1.PodSpec{
					SecurityContext:           lr.Spec.PodTemplate.SecurityContext,
					Containers:                containers,
					Volumes:                   buildVolumes(lr),
					NodeSelector:              lr.Spec.PodTemplate.NodeSelector,
					Tolerations:               lr.Spec.PodTemplate.Tolerations,
					Affinity:                  lr.Spec.PodTemplate.Affinity,
					PriorityClassName:         lr.Spec.PodTemplate.PriorityClassName,
					TopologySpreadConstraints: lr.Spec.PodTemplate.TopologySpreadConstraints,
					ImagePullSecrets:          lr.Spec.Image.PullSecrets,
				},
			},
			UpdateStrategy: appsv1.StatefulSetUpdateStrategy{
				Type: appsv1.StatefulSetUpdateStrategyType(lr.Spec.UpdateStrategy.Type),
			},
		},
	}

	return sts
}

// buildRedisContainer creates the main Redis container
func buildRedisContainer(lr *littleredv1alpha1.LittleRed) corev1.Container {
	args := []string{
		"/etc/redis/redis.conf",
	}

	container := corev1.Container{
		Name:            "redis",
		Image:           lr.Spec.Image.FullImage(),
		ImagePullPolicy: lr.Spec.Image.PullPolicy,
		Args:            args,
		Ports: []corev1.ContainerPort{
			{
				Name:          "redis",
				ContainerPort: int32(littleredv1alpha1.RedisPort),
				Protocol:      corev1.ProtocolTCP,
			},
		},
		Resources: lr.Spec.Resources,
		VolumeMounts: []corev1.VolumeMount{
			{
				Name:      "config",
				MountPath: "/etc/redis",
				ReadOnly:  true,
			},
			{
				Name:      "data",
				MountPath: "/data",
			},
		},
		LivenessProbe:  buildLivenessProbe(lr),
		ReadinessProbe: buildReadinessProbe(lr),
		SecurityContext: &corev1.SecurityContext{
			AllowPrivilegeEscalation: ptr(false),
			ReadOnlyRootFilesystem:   ptr(true),
			Capabilities: &corev1.Capabilities{
				Drop: []corev1.Capability{"ALL"},
			},
		},
	}

	// Add TLS volume mounts
	if lr.Spec.TLS.Enabled {
		container.VolumeMounts = append(container.VolumeMounts, corev1.VolumeMount{
			Name:      "tls",
			MountPath: "/tls",
			ReadOnly:  true,
		})
		if lr.Spec.TLS.ClientAuth && lr.Spec.TLS.CACertSecret != "" {
			container.VolumeMounts = append(container.VolumeMounts, corev1.VolumeMount{
				Name:      "ca-cert",
				MountPath: "/tls/ca.crt",
				SubPath:   "ca.crt",
				ReadOnly:  true,
			})
		}
	}

	// Add auth env var
	if lr.Spec.Auth.Enabled {
		container.Env = append(container.Env, corev1.EnvVar{
			Name: "REDIS_PASSWORD",
			ValueFrom: &corev1.EnvVarSource{
				SecretKeyRef: &corev1.SecretKeySelector{
					LocalObjectReference: corev1.LocalObjectReference{
						Name: lr.Spec.Auth.ExistingSecret,
					},
					Key: "password",
				},
			},
		})
		// Add requirepass to args
		container.Args = append(container.Args, "--requirepass", "$(REDIS_PASSWORD)")
	}

	return container
}

// buildExporterContainer creates the redis_exporter sidecar container
func buildExporterContainer(lr *littleredv1alpha1.LittleRed) corev1.Container {
	env := []corev1.EnvVar{
		{
			Name:  "REDIS_ADDR",
			Value: fmt.Sprintf("redis://localhost:%d", littleredv1alpha1.RedisPort),
		},
	}

	// Add password env if auth enabled
	if lr.Spec.Auth.Enabled {
		env = append(env, corev1.EnvVar{
			Name: "REDIS_PASSWORD",
			ValueFrom: &corev1.EnvVarSource{
				SecretKeyRef: &corev1.SecretKeySelector{
					LocalObjectReference: corev1.LocalObjectReference{
						Name: lr.Spec.Auth.ExistingSecret,
					},
					Key: "password",
				},
			},
		})
	}

	// Handle TLS
	if lr.Spec.TLS.Enabled {
		env[0].Value = fmt.Sprintf("rediss://localhost:%d", littleredv1alpha1.RedisPort)
		env = append(env,
			corev1.EnvVar{Name: "REDIS_EXPORTER_SKIP_TLS_VERIFICATION", Value: "true"},
		)
	}

	container := corev1.Container{
		Name:            "exporter",
		Image:           lr.Spec.Metrics.Exporter.FullImage(lr.Spec.Image.Registry),
		ImagePullPolicy: lr.Spec.Image.PullPolicy,
		Env:             env,
		Ports: []corev1.ContainerPort{
			{
				Name:          "metrics",
				ContainerPort: int32(littleredv1alpha1.RedisExporterPort),
				Protocol:      corev1.ProtocolTCP,
			},
		},
		Resources: lr.Spec.Metrics.Exporter.Resources,
		SecurityContext: &corev1.SecurityContext{
			AllowPrivilegeEscalation: ptr(false),
			ReadOnlyRootFilesystem:   ptr(true),
			Capabilities: &corev1.Capabilities{
				Drop: []corev1.Capability{"ALL"},
			},
		},
	}

	return container
}

// buildVolumes creates the volumes for the pod
func buildVolumes(lr *littleredv1alpha1.LittleRed) []corev1.Volume {
	volumes := []corev1.Volume{
		{
			Name: "config",
			VolumeSource: corev1.VolumeSource{
				ConfigMap: &corev1.ConfigMapVolumeSource{
					LocalObjectReference: corev1.LocalObjectReference{
						Name: configMapName(lr),
					},
				},
			},
		},
		{
			Name: "data",
			VolumeSource: corev1.VolumeSource{
				EmptyDir: &corev1.EmptyDirVolumeSource{},
			},
		},
	}

	// Add TLS volumes
	if lr.Spec.TLS.Enabled {
		volumes = append(volumes, corev1.Volume{
			Name: "tls",
			VolumeSource: corev1.VolumeSource{
				Secret: &corev1.SecretVolumeSource{
					SecretName: lr.Spec.TLS.ExistingSecret,
				},
			},
		})
		if lr.Spec.TLS.ClientAuth && lr.Spec.TLS.CACertSecret != "" {
			volumes = append(volumes, corev1.Volume{
				Name: "ca-cert",
				VolumeSource: corev1.VolumeSource{
					Secret: &corev1.SecretVolumeSource{
						SecretName: lr.Spec.TLS.CACertSecret,
					},
				},
			})
		}
	}

	return volumes
}

// buildLivenessProbe creates the liveness probe for Redis
func buildLivenessProbe(lr *littleredv1alpha1.LittleRed) *corev1.Probe {
	cmd := []string{"redis-cli"}
	if lr.Spec.Auth.Enabled {
		cmd = append(cmd, "-a", "$(REDIS_PASSWORD)")
	}
	if lr.Spec.TLS.Enabled {
		cmd = append(cmd, "--tls", "--insecure")
	}
	cmd = append(cmd, "ping")

	return &corev1.Probe{
		ProbeHandler: corev1.ProbeHandler{
			Exec: &corev1.ExecAction{
				Command: []string{
					"sh", "-c",
					strings.Join(cmd, " "),
				},
			},
		},
		InitialDelaySeconds: 5,
		PeriodSeconds:       10,
		TimeoutSeconds:      5,
		FailureThreshold:    3,
	}
}

// buildReadinessProbe creates the readiness probe for Redis
func buildReadinessProbe(lr *littleredv1alpha1.LittleRed) *corev1.Probe {
	cmd := []string{"redis-cli"}
	if lr.Spec.Auth.Enabled {
		cmd = append(cmd, "-a", "$(REDIS_PASSWORD)")
	}
	if lr.Spec.TLS.Enabled {
		cmd = append(cmd, "--tls", "--insecure")
	}
	cmd = append(cmd, "ping")

	return &corev1.Probe{
		ProbeHandler: corev1.ProbeHandler{
			Exec: &corev1.ExecAction{
				Command: []string{
					"sh", "-c",
					strings.Join(cmd, " "),
				},
			},
		},
		InitialDelaySeconds: 5,
		PeriodSeconds:       5,
		TimeoutSeconds:      3,
		FailureThreshold:    3,
	}
}

// buildService creates the Service for Redis
func buildService(lr *littleredv1alpha1.LittleRed) *corev1.Service {
	labels := commonLabels(lr)
	// Add user-defined labels
	for k, v := range lr.Spec.Service.Labels {
		labels[k] = v
	}

	ports := []corev1.ServicePort{
		{
			Name:       "redis",
			Port:       int32(littleredv1alpha1.RedisPort),
			TargetPort: intstr.FromString("redis"),
			Protocol:   corev1.ProtocolTCP,
		},
	}

	// Add metrics port if enabled
	if lr.Spec.Metrics.IsEnabled() {
		ports = append(ports, corev1.ServicePort{
			Name:       "metrics",
			Port:       int32(littleredv1alpha1.RedisExporterPort),
			TargetPort: intstr.FromString("metrics"),
			Protocol:   corev1.ProtocolTCP,
		})
	}

	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:        serviceName(lr),
			Namespace:   lr.Namespace,
			Labels:      labels,
			Annotations: lr.Spec.Service.Annotations,
		},
		Spec: corev1.ServiceSpec{
			Type:     lr.Spec.Service.Type,
			Selector: selectorLabels(lr),
			Ports:    ports,
		},
	}

	return svc
}

// buildServiceMonitor creates the ServiceMonitor for Prometheus
func buildServiceMonitor(lr *littleredv1alpha1.LittleRed) *monitoringv1.ServiceMonitor {
	labels := commonLabels(lr)
	// Add user-defined labels
	for k, v := range lr.Spec.Metrics.ServiceMonitor.Labels {
		labels[k] = v
	}

	namespace := lr.Namespace
	if lr.Spec.Metrics.ServiceMonitor.Namespace != "" {
		namespace = lr.Spec.Metrics.ServiceMonitor.Namespace
	}

	interval := lr.Spec.Metrics.ServiceMonitor.Interval
	if interval == "" {
		interval = "30s"
	}

	scrapeTimeout := lr.Spec.Metrics.ServiceMonitor.ScrapeTimeout
	if scrapeTimeout == "" {
		scrapeTimeout = "10s"
	}

	sm := &monitoringv1.ServiceMonitor{
		ObjectMeta: metav1.ObjectMeta{
			Name:      serviceMonitorName(lr),
			Namespace: namespace,
			Labels:    labels,
		},
		Spec: monitoringv1.ServiceMonitorSpec{
			Selector: metav1.LabelSelector{
				MatchLabels: selectorLabels(lr),
			},
			NamespaceSelector: monitoringv1.NamespaceSelector{
				MatchNames: []string{lr.Namespace},
			},
			Endpoints: []monitoringv1.Endpoint{
				{
					Port:          "metrics",
					Interval:      monitoringv1.Duration(interval),
					ScrapeTimeout: monitoringv1.Duration(scrapeTimeout),
				},
			},
		},
	}

	return sm
}

// ============================================================================
// Sentinel Mode Resources
// ============================================================================

// buildSentinelConfigMap creates the ConfigMap for sentinel.conf
func buildSentinelConfigMap(lr *littleredv1alpha1.LittleRed) *corev1.ConfigMap {
	labels := commonLabels(lr)
	labels["app.kubernetes.io/component"] = "sentinel"

	return &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      sentinelConfigMapName(lr),
			Namespace: lr.Namespace,
			Labels:    labels,
		},
		Data: map[string]string{
			"sentinel.conf": buildSentinelConfig(lr),
		},
	}
}

// buildSentinelConfig generates the sentinel.conf content
func buildSentinelConfig(lr *littleredv1alpha1.LittleRed) string {
	var sb strings.Builder

	sentinel := lr.Spec.Sentinel
	if sentinel == nil {
		sentinel = &littleredv1alpha1.SentinelSpec{}
	}

	quorum := sentinel.Quorum
	if quorum == 0 {
		quorum = 2
	}
	downAfterMs := sentinel.DownAfterMilliseconds
	if downAfterMs == 0 {
		downAfterMs = 30000
	}
	failoverTimeout := sentinel.FailoverTimeout
	if failoverTimeout == 0 {
		failoverTimeout = 180000
	}
	parallelSyncs := sentinel.ParallelSyncs
	if parallelSyncs == 0 {
		parallelSyncs = 1
	}

	// Use the headless service for initial master discovery
	// The first pod (index 0) will be the initial master
	initialMaster := fmt.Sprintf("%s-redis-0.%s.%s.svc.cluster.local",
		lr.Name, replicasServiceName(lr), lr.Namespace)

	sb.WriteString("# LittleRed Sentinel configuration\n")
	sb.WriteString(fmt.Sprintf("port %d\n", littleredv1alpha1.SentinelPort))
	sb.WriteString("dir /data\n")
	sb.WriteString("\n# Master monitoring\n")
	sb.WriteString(fmt.Sprintf("sentinel monitor mymaster %s %d %d\n",
		initialMaster, littleredv1alpha1.RedisPort, quorum))
	sb.WriteString(fmt.Sprintf("sentinel down-after-milliseconds mymaster %d\n", downAfterMs))
	sb.WriteString(fmt.Sprintf("sentinel failover-timeout mymaster %d\n", failoverTimeout))
	sb.WriteString(fmt.Sprintf("sentinel parallel-syncs mymaster %d\n", parallelSyncs))

	// Auth configuration
	if lr.Spec.Auth.Enabled {
		// Password will be set via command line
		sb.WriteString("\n# Auth will be configured via environment\n")
	}

	// Announce settings for proper discovery
	sb.WriteString("\n# Announce settings\n")
	sb.WriteString("sentinel resolve-hostnames yes\n")
	sb.WriteString("sentinel announce-hostnames yes\n")

	return sb.String()
}

// buildRedisConfigSentinel generates redis.conf for sentinel mode (includes replication)
func buildRedisConfigSentinel(lr *littleredv1alpha1.LittleRed) string {
	var sb strings.Builder

	// Start with base config
	sb.WriteString("# LittleRed generated configuration (sentinel mode)\n")
	sb.WriteString("bind 0.0.0.0\n")
	sb.WriteString(fmt.Sprintf("port %d\n", littleredv1alpha1.RedisPort))
	sb.WriteString("dir /data\n")

	// Disable persistence (pure cache)
	sb.WriteString("\n# Persistence disabled (cache mode)\n")
	sb.WriteString("save \"\"\n")
	sb.WriteString("appendonly no\n")

	// Memory settings
	sb.WriteString("\n# Memory configuration\n")
	maxmemory := lr.CalculateMaxmemory()
	sb.WriteString(fmt.Sprintf("maxmemory %s\n", maxmemory))
	sb.WriteString(fmt.Sprintf("maxmemory-policy %s\n", lr.GetEffectiveMaxmemoryPolicy()))

	// Timeout settings
	sb.WriteString("\n# Connection settings\n")
	sb.WriteString(fmt.Sprintf("timeout %d\n", lr.Spec.Config.Timeout))
	sb.WriteString(fmt.Sprintf("tcp-keepalive %d\n", lr.Spec.Config.TCPKeepalive))

	// Replication settings - allow replicas to serve stale data during sync
	sb.WriteString("\n# Replication settings\n")
	sb.WriteString("replica-serve-stale-data yes\n")
	sb.WriteString("replica-read-only yes\n")
	sb.WriteString("repl-diskless-sync yes\n")
	sb.WriteString("repl-diskless-sync-delay 5\n")
	sb.WriteString("repl-diskless-load on-empty-db\n")

	// TLS settings
	if lr.Spec.TLS.Enabled {
		sb.WriteString("\n# TLS configuration\n")
		sb.WriteString(fmt.Sprintf("tls-port %d\n", littleredv1alpha1.RedisPort))
		sb.WriteString("port 0\n")
		sb.WriteString("tls-cert-file /tls/tls.crt\n")
		sb.WriteString("tls-key-file /tls/tls.key\n")
		sb.WriteString("tls-replication yes\n")
		if lr.Spec.TLS.ClientAuth {
			sb.WriteString("tls-ca-cert-file /tls/ca.crt\n")
			sb.WriteString("tls-auth-clients yes\n")
		} else {
			sb.WriteString("tls-auth-clients no\n")
		}
	}

	// Raw config (expert mode)
	if lr.Spec.Config.Raw != "" {
		sb.WriteString("\n# Custom configuration\n")
		sb.WriteString(lr.Spec.Config.Raw)
		if !strings.HasSuffix(lr.Spec.Config.Raw, "\n") {
			sb.WriteString("\n")
		}
	}

	return sb.String()
}

// buildConfigMapSentinelMode creates the ConfigMap for redis.conf in sentinel mode
func buildConfigMapSentinelMode(lr *littleredv1alpha1.LittleRed) *corev1.ConfigMap {
	labels := commonLabels(lr)
	labels["app.kubernetes.io/component"] = "redis"

	return &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      configMapName(lr),
			Namespace: lr.Namespace,
			Labels:    labels,
		},
		Data: map[string]string{
			"redis.conf": buildRedisConfigSentinel(lr),
		},
	}
}

// buildRedisStatefulSetSentinel creates the Redis StatefulSet for sentinel mode (3 replicas)
func buildRedisStatefulSetSentinel(lr *littleredv1alpha1.LittleRed) *appsv1.StatefulSet {
	labels := commonLabels(lr)
	labels["app.kubernetes.io/component"] = "redis"

	podLabels := make(map[string]string)
	for k, v := range redisSelectorLabels(lr) {
		podLabels[k] = v
	}
	for k, v := range lr.Spec.PodTemplate.Labels {
		podLabels[k] = v
	}

	// Compute config hash for pod annotations to trigger rolling update on config change
	configData := map[string]string{"redis.conf": buildRedisConfigSentinel(lr)}
	configHash := computeConfigHash(configData)

	podAnnotations := make(map[string]string)
	for k, v := range lr.Spec.PodTemplate.Annotations {
		podAnnotations[k] = v
	}
	podAnnotations[AnnotationConfigHash] = configHash

	replicas := int32(3)

	containers := []corev1.Container{buildRedisContainerSentinel(lr)}

	// Add exporter sidecar if metrics enabled
	if lr.Spec.Metrics.IsEnabled() {
		containers = append(containers, buildExporterContainer(lr))
	}

	sts := &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      statefulSetName(lr),
			Namespace: lr.Namespace,
			Labels:    labels,
		},
		Spec: appsv1.StatefulSetSpec{
			Replicas:    &replicas,
			ServiceName: replicasServiceName(lr),
			Selector: &metav1.LabelSelector{
				MatchLabels: redisSelectorLabels(lr),
			},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels:      podLabels,
					Annotations: podAnnotations,
				},
				Spec: corev1.PodSpec{
					SecurityContext:           lr.Spec.PodTemplate.SecurityContext,
					Containers:                containers,
					Volumes:                   buildVolumes(lr),
					NodeSelector:              lr.Spec.PodTemplate.NodeSelector,
					Tolerations:               lr.Spec.PodTemplate.Tolerations,
					Affinity:                  lr.Spec.PodTemplate.Affinity,
					PriorityClassName:         lr.Spec.PodTemplate.PriorityClassName,
					TopologySpreadConstraints: lr.Spec.PodTemplate.TopologySpreadConstraints,
					ImagePullSecrets:          lr.Spec.Image.PullSecrets,
				},
			},
			UpdateStrategy: appsv1.StatefulSetUpdateStrategy{
				Type: appsv1.RollingUpdateStatefulSetStrategyType,
			},
			PodManagementPolicy: appsv1.ParallelPodManagement,
		},
	}

	return sts
}

// buildRedisContainerSentinel creates the Redis container for sentinel mode
func buildRedisContainerSentinel(lr *littleredv1alpha1.LittleRed) corev1.Container {
	// Script to configure replication based on pod index
	// Pod-0 starts as master, others start as replicas
	// We copy the config to /data so it's writable for CONFIG REWRITE
	startupScript := `#!/bin/sh
set -e

cp /etc/redis/redis.conf /data/redis.conf

HOSTNAME=$(hostname)
INDEX=${HOSTNAME##*-}
MASTER_HOST="%s-redis-0.%s.%s.svc.cluster.local"

if [ "$INDEX" = "0" ]; then
  echo "Starting as initial master"
  exec redis-server /data/redis.conf %s
else
  echo "Starting as replica of $MASTER_HOST"
  exec redis-server /data/redis.conf --replicaof $MASTER_HOST %d %s
fi
`
	authArgs := ""
	if lr.Spec.Auth.Enabled {
		authArgs = "--requirepass $(REDIS_PASSWORD) --masterauth $(REDIS_PASSWORD)"
	}

	script := fmt.Sprintf(startupScript,
		lr.Name, replicasServiceName(lr), lr.Namespace,
		authArgs,
		littleredv1alpha1.RedisPort, authArgs)

	container := corev1.Container{
		Name:            "redis",
		Image:           lr.Spec.Image.FullImage(),
		ImagePullPolicy: lr.Spec.Image.PullPolicy,
		Command:         []string{"sh", "-c", script},
		Ports: []corev1.ContainerPort{
			{
				Name:          "redis",
				ContainerPort: int32(littleredv1alpha1.RedisPort),
				Protocol:      corev1.ProtocolTCP,
			},
		},
		Resources: lr.Spec.Resources,
		VolumeMounts: []corev1.VolumeMount{
			{
				Name:      "config",
				MountPath: "/etc/redis",
				ReadOnly:  true,
			},
			{
				Name:      "data",
				MountPath: "/data",
			},
		},
		LivenessProbe:  buildLivenessProbe(lr),
		ReadinessProbe: buildReadinessProbe(lr),
		SecurityContext: &corev1.SecurityContext{
			AllowPrivilegeEscalation: ptr(false),
			ReadOnlyRootFilesystem:   ptr(true),
			Capabilities: &corev1.Capabilities{
				Drop: []corev1.Capability{"ALL"},
			},
		},
	}

	// Add TLS volume mounts
	if lr.Spec.TLS.Enabled {
		container.VolumeMounts = append(container.VolumeMounts, corev1.VolumeMount{
			Name:      "tls",
			MountPath: "/tls",
			ReadOnly:  true,
		})
	}

	// Add auth env var
	if lr.Spec.Auth.Enabled {
		container.Env = append(container.Env, corev1.EnvVar{
			Name: "REDIS_PASSWORD",
			ValueFrom: &corev1.EnvVarSource{
				SecretKeyRef: &corev1.SecretKeySelector{
					LocalObjectReference: corev1.LocalObjectReference{
						Name: lr.Spec.Auth.ExistingSecret,
					},
					Key: "password",
				},
			},
		})
	}

	return container
}

// buildSentinelStatefulSet creates the Sentinel StatefulSet
func buildSentinelStatefulSet(lr *littleredv1alpha1.LittleRed) *appsv1.StatefulSet {
	labels := commonLabels(lr)
	labels["app.kubernetes.io/component"] = "sentinel"

	podLabels := make(map[string]string)
	for k, v := range sentinelSelectorLabels(lr) {
		podLabels[k] = v
	}

	// Compute config hash for pod annotations to trigger rolling update on config change
	configData := map[string]string{"sentinel.conf": buildSentinelConfig(lr)}
	configHash := computeConfigHash(configData)

	podAnnotations := map[string]string{
		AnnotationConfigHash: configHash,
	}

	replicas := int32(3)

	sentinel := lr.Spec.Sentinel
	if sentinel == nil {
		sentinel = &littleredv1alpha1.SentinelSpec{}
	}

	sts := &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      sentinelStatefulSetName(lr),
			Namespace: lr.Namespace,
			Labels:    labels,
		},
		Spec: appsv1.StatefulSetSpec{
			Replicas:    &replicas,
			ServiceName: sentinelServiceName(lr),
			Selector: &metav1.LabelSelector{
				MatchLabels: sentinelSelectorLabels(lr),
			},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels:      podLabels,
					Annotations: podAnnotations,
				},
				Spec: corev1.PodSpec{
					SecurityContext: lr.Spec.PodTemplate.SecurityContext,
					Containers:      []corev1.Container{buildSentinelContainer(lr)},
					Volumes: []corev1.Volume{
						{
							Name: "config",
							VolumeSource: corev1.VolumeSource{
								ConfigMap: &corev1.ConfigMapVolumeSource{
									LocalObjectReference: corev1.LocalObjectReference{
										Name: sentinelConfigMapName(lr),
									},
								},
							},
						},
						{
							Name: "data",
							VolumeSource: corev1.VolumeSource{
								EmptyDir: &corev1.EmptyDirVolumeSource{},
							},
						},
					},
					NodeSelector:     lr.Spec.PodTemplate.NodeSelector,
					Tolerations:      lr.Spec.PodTemplate.Tolerations,
					Affinity:         lr.Spec.PodTemplate.Affinity,
					ImagePullSecrets: lr.Spec.Image.PullSecrets,
				},
			},
			UpdateStrategy: appsv1.StatefulSetUpdateStrategy{
				Type: appsv1.RollingUpdateStatefulSetStrategyType,
			},
			PodManagementPolicy: appsv1.ParallelPodManagement,
		},
	}

	return sts
}

// buildSentinelContainer creates the Sentinel container
func buildSentinelContainer(lr *littleredv1alpha1.LittleRed) corev1.Container {
	sentinel := lr.Spec.Sentinel
	if sentinel == nil {
		sentinel = &littleredv1alpha1.SentinelSpec{}
	}

	// Copy config to writable location and start sentinel
	startupScript := `#!/bin/sh
set -e
cp /etc/sentinel/sentinel.conf /data/sentinel.conf
exec redis-sentinel /data/sentinel.conf %s
`
	authArgs := ""
	if lr.Spec.Auth.Enabled {
		authArgs = "--sentinel auth-pass mymaster $(REDIS_PASSWORD)"
	}

	script := fmt.Sprintf(startupScript, authArgs)

	container := corev1.Container{
		Name:            "sentinel",
		Image:           lr.Spec.Image.FullImage(),
		ImagePullPolicy: lr.Spec.Image.PullPolicy,
		Command:         []string{"sh", "-c", script},
		Ports: []corev1.ContainerPort{
			{
				Name:          "sentinel",
				ContainerPort: int32(littleredv1alpha1.SentinelPort),
				Protocol:      corev1.ProtocolTCP,
			},
		},
		Resources: sentinel.Resources,
		VolumeMounts: []corev1.VolumeMount{
			{
				Name:      "config",
				MountPath: "/etc/sentinel",
				ReadOnly:  true,
			},
			{
				Name:      "data",
				MountPath: "/data",
			},
		},
		LivenessProbe: &corev1.Probe{
			ProbeHandler: corev1.ProbeHandler{
				Exec: &corev1.ExecAction{
					Command: []string{
						"sh", "-c",
						fmt.Sprintf("redis-cli -p %d ping", littleredv1alpha1.SentinelPort),
					},
				},
			},
			InitialDelaySeconds: 5,
			PeriodSeconds:       10,
			TimeoutSeconds:      5,
			FailureThreshold:    3,
		},
		ReadinessProbe: &corev1.Probe{
			ProbeHandler: corev1.ProbeHandler{
				Exec: &corev1.ExecAction{
					Command: []string{
						"sh", "-c",
						fmt.Sprintf("redis-cli -p %d ping", littleredv1alpha1.SentinelPort),
					},
				},
			},
			InitialDelaySeconds: 5,
			PeriodSeconds:       5,
			TimeoutSeconds:      3,
			FailureThreshold:    3,
		},
		SecurityContext: &corev1.SecurityContext{
			AllowPrivilegeEscalation: ptr(false),
			ReadOnlyRootFilesystem:   ptr(true),
			Capabilities: &corev1.Capabilities{
				Drop: []corev1.Capability{"ALL"},
			},
		},
	}

	// Add auth env var
	if lr.Spec.Auth.Enabled {
		container.Env = append(container.Env, corev1.EnvVar{
			Name: "REDIS_PASSWORD",
			ValueFrom: &corev1.EnvVarSource{
				SecretKeyRef: &corev1.SecretKeySelector{
					LocalObjectReference: corev1.LocalObjectReference{
						Name: lr.Spec.Auth.ExistingSecret,
					},
					Key: "password",
				},
			},
		})
	}

	return container
}

// buildMasterService creates the Service that points to the current master
func buildMasterService(lr *littleredv1alpha1.LittleRed) *corev1.Service {
	labels := commonLabels(lr)
	for k, v := range lr.Spec.Service.Labels {
		labels[k] = v
	}

	ports := []corev1.ServicePort{
		{
			Name:       "redis",
			Port:       int32(littleredv1alpha1.RedisPort),
			TargetPort: intstr.FromString("redis"),
			Protocol:   corev1.ProtocolTCP,
		},
	}

	if lr.Spec.Metrics.IsEnabled() {
		ports = append(ports, corev1.ServicePort{
			Name:       "metrics",
			Port:       int32(littleredv1alpha1.RedisExporterPort),
			TargetPort: intstr.FromString("metrics"),
			Protocol:   corev1.ProtocolTCP,
		})
	}

	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:        serviceName(lr),
			Namespace:   lr.Namespace,
			Labels:      labels,
			Annotations: lr.Spec.Service.Annotations,
		},
		Spec: corev1.ServiceSpec{
			Type:     lr.Spec.Service.Type,
			Selector: masterSelectorLabels(lr),
			Ports:    ports,
		},
	}
}

// buildReplicasHeadlessService creates the headless Service for all Redis pods
func buildReplicasHeadlessService(lr *littleredv1alpha1.LittleRed) *corev1.Service {
	labels := commonLabels(lr)
	labels["app.kubernetes.io/component"] = "redis"

	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      replicasServiceName(lr),
			Namespace: lr.Namespace,
			Labels:    labels,
		},
		Spec: corev1.ServiceSpec{
			Type:                     corev1.ServiceTypeClusterIP,
			ClusterIP:                "None",
			Selector:                 redisSelectorLabels(lr),
			PublishNotReadyAddresses: true,
			Ports: []corev1.ServicePort{
				{
					Name:       "redis",
					Port:       int32(littleredv1alpha1.RedisPort),
					TargetPort: intstr.FromString("redis"),
					Protocol:   corev1.ProtocolTCP,
				},
			},
		},
	}
}

// buildSentinelHeadlessService creates the headless Service for Sentinel pods
func buildSentinelHeadlessService(lr *littleredv1alpha1.LittleRed) *corev1.Service {
	labels := commonLabels(lr)
	labels["app.kubernetes.io/component"] = "sentinel"

	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      sentinelServiceName(lr),
			Namespace: lr.Namespace,
			Labels:    labels,
		},
		Spec: corev1.ServiceSpec{
			Type:                     corev1.ServiceTypeClusterIP,
			ClusterIP:                "None",
			Selector:                 sentinelSelectorLabels(lr),
			PublishNotReadyAddresses: true,
			Ports: []corev1.ServicePort{
				{
					Name:       "sentinel",
					Port:       int32(littleredv1alpha1.SentinelPort),
					TargetPort: intstr.FromString("sentinel"),
					Protocol:   corev1.ProtocolTCP,
				},
			},
		},
	}
}

func ptr[T any](v T) *T {
	return &v
}

// ============================================================================
// Cluster Mode Resources
// ============================================================================

// buildClusterConfigMap creates the ConfigMap for cluster mode redis.conf
func buildClusterConfigMap(lr *littleredv1alpha1.LittleRed) *corev1.ConfigMap {
	labels := commonLabels(lr)
	labels["app.kubernetes.io/component"] = "cluster"

	return &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      configMapName(lr),
			Namespace: lr.Namespace,
			Labels:    labels,
		},
		Data: map[string]string{
			"redis.conf": buildClusterRedisConfig(lr),
		},
	}
}

// buildClusterRedisConfig generates redis.conf for cluster mode
func buildClusterRedisConfig(lr *littleredv1alpha1.LittleRed) string {
	var sb strings.Builder

	cluster := lr.Spec.Cluster
	if cluster == nil {
		cluster = &littleredv1alpha1.ClusterSpec{}
		cluster.SetDefaults()
	}

	sb.WriteString("# LittleRed generated configuration (cluster mode)\n")
	sb.WriteString("bind 0.0.0.0\n")
	sb.WriteString(fmt.Sprintf("port %d\n", littleredv1alpha1.RedisPort))
	sb.WriteString("dir /data\n")

	// Cluster configuration
	sb.WriteString("\n# Cluster configuration\n")
	sb.WriteString("cluster-enabled yes\n")
	sb.WriteString("cluster-config-file /data/nodes.conf\n")
	sb.WriteString(fmt.Sprintf("cluster-node-timeout %d\n", cluster.ClusterNodeTimeout))
	sb.WriteString("cluster-announce-hostname yes\n")

	// Replication settings
	sb.WriteString("\n# Replication settings\n")
	sb.WriteString("repl-diskless-sync yes\n")
	sb.WriteString("repl-diskless-sync-delay 5\n")
	sb.WriteString("repl-diskless-load on-empty-db\n")

	// Disable persistence (pure cache)
	sb.WriteString("\n# Persistence disabled (cache mode)\n")
	sb.WriteString("save \"\"\n")
	sb.WriteString("appendonly no\n")

	// Memory settings
	sb.WriteString("\n# Memory configuration\n")
	maxmemory := lr.CalculateMaxmemory()
	sb.WriteString(fmt.Sprintf("maxmemory %s\n", maxmemory))
	sb.WriteString(fmt.Sprintf("maxmemory-policy %s\n", lr.GetEffectiveMaxmemoryPolicy()))

	// Timeout settings
	sb.WriteString("\n# Connection settings\n")
	sb.WriteString(fmt.Sprintf("timeout %d\n", lr.Spec.Config.Timeout))
	sb.WriteString(fmt.Sprintf("tcp-keepalive %d\n", lr.Spec.Config.TCPKeepalive))

	// TLS settings
	if lr.Spec.TLS.Enabled {
		sb.WriteString("\n# TLS configuration\n")
		sb.WriteString(fmt.Sprintf("tls-port %d\n", littleredv1alpha1.RedisPort))
		sb.WriteString("port 0\n")
		sb.WriteString("tls-cert-file /tls/tls.crt\n")
		sb.WriteString("tls-key-file /tls/tls.key\n")
		sb.WriteString("tls-cluster yes\n")
		sb.WriteString("tls-replication yes\n")
		if lr.Spec.TLS.ClientAuth {
			sb.WriteString("tls-ca-cert-file /tls/ca.crt\n")
			sb.WriteString("tls-auth-clients yes\n")
		} else {
			sb.WriteString("tls-auth-clients no\n")
		}
	}

	// Raw config (expert mode)
	if lr.Spec.Config.Raw != "" {
		sb.WriteString("\n# Custom configuration\n")
		sb.WriteString(lr.Spec.Config.Raw)
		if !strings.HasSuffix(lr.Spec.Config.Raw, "\n") {
			sb.WriteString("\n")
		}
	}

	return sb.String()
}

// buildClusterStatefulSet creates the StatefulSet for cluster mode
func buildClusterStatefulSet(lr *littleredv1alpha1.LittleRed) *appsv1.StatefulSet {
	labels := commonLabels(lr)
	labels["app.kubernetes.io/component"] = "cluster"

	podLabels := make(map[string]string)
	for k, v := range clusterSelectorLabels(lr) {
		podLabels[k] = v
	}
	for k, v := range lr.Spec.PodTemplate.Labels {
		podLabels[k] = v
	}

	// Compute config hash for pod annotations to trigger rolling update on config change
	configData := map[string]string{"redis.conf": buildClusterRedisConfig(lr)}
	configHash := computeConfigHash(configData)

	podAnnotations := make(map[string]string)
	for k, v := range lr.Spec.PodTemplate.Annotations {
		podAnnotations[k] = v
	}
	podAnnotations[AnnotationConfigHash] = configHash

	cluster := lr.Spec.Cluster
	if cluster == nil {
		cluster = &littleredv1alpha1.ClusterSpec{}
		cluster.SetDefaults()
	}

	replicas := int32(cluster.GetTotalNodes())

	containers := []corev1.Container{buildClusterRedisContainer(lr)}

	// Add exporter sidecar if metrics enabled
	if lr.Spec.Metrics.IsEnabled() {
		containers = append(containers, buildExporterContainer(lr))
	}

	sts := &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      clusterStatefulSetName(lr),
			Namespace: lr.Namespace,
			Labels:    labels,
		},
		Spec: appsv1.StatefulSetSpec{
			Replicas:    &replicas,
			ServiceName: clusterHeadlessServiceName(lr),
			Selector: &metav1.LabelSelector{
				MatchLabels: clusterSelectorLabels(lr),
			},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels:      podLabels,
					Annotations: podAnnotations,
				},
				Spec: corev1.PodSpec{
					SecurityContext:           lr.Spec.PodTemplate.SecurityContext,
					Containers:                containers,
					Volumes:                   buildClusterVolumes(lr),
					NodeSelector:              lr.Spec.PodTemplate.NodeSelector,
					Tolerations:               lr.Spec.PodTemplate.Tolerations,
					Affinity:                  lr.Spec.PodTemplate.Affinity,
					PriorityClassName:         lr.Spec.PodTemplate.PriorityClassName,
					TopologySpreadConstraints: lr.Spec.PodTemplate.TopologySpreadConstraints,
					ImagePullSecrets:          lr.Spec.Image.PullSecrets,
				},
			},
			UpdateStrategy: appsv1.StatefulSetUpdateStrategy{
				Type: appsv1.RollingUpdateStatefulSetStrategyType,
			},
			PodManagementPolicy: appsv1.ParallelPodManagement,
		},
	}

	return sts
}

// buildClusterRedisContainer creates the Redis container for cluster mode
func buildClusterRedisContainer(lr *littleredv1alpha1.LittleRed) corev1.Container {
	// Startup script that announces pod IP.
	// We let Redis generate its own Node ID and manage nodes.conf.
	// This results in a new Node ID on every pod restart (since data is on EmptyDir).
	startupScript := `#!/bin/sh
set -e

echo "Starting Redis cluster node ${POD_NAME} with IP: ${POD_IP}"

exec redis-server /etc/redis/redis.conf \
  --cluster-announce-ip ${POD_IP} \
  --cluster-announce-port %d \
  --cluster-announce-bus-port %d %s
`
	authArgs := ""
	if lr.Spec.Auth.Enabled {
		authArgs = "--requirepass $(REDIS_PASSWORD) --masterauth $(REDIS_PASSWORD)"
	}

	script := fmt.Sprintf(startupScript,
		littleredv1alpha1.RedisPort,
		littleredv1alpha1.ClusterBusPort,
		authArgs)

	container := corev1.Container{
		Name:            "redis",
		Image:           lr.Spec.Image.FullImage(),
		ImagePullPolicy: lr.Spec.Image.PullPolicy,
		Command:         []string{"sh", "-c", script},
		Ports: []corev1.ContainerPort{
			{
				Name:          "redis",
				ContainerPort: int32(littleredv1alpha1.RedisPort),
				Protocol:      corev1.ProtocolTCP,
			},
			{
				Name:          "cluster-bus",
				ContainerPort: int32(littleredv1alpha1.ClusterBusPort),
				Protocol:      corev1.ProtocolTCP,
			},
		},
		Resources: lr.Spec.Resources,
		VolumeMounts: []corev1.VolumeMount{
			{
				Name:      "config",
				MountPath: "/etc/redis",
				ReadOnly:  true,
			},
			{
				Name:      "data",
				MountPath: "/data",
			},
		},
		LivenessProbe:  buildClusterLivenessProbe(lr),
		ReadinessProbe: buildClusterReadinessProbe(lr),
		SecurityContext: &corev1.SecurityContext{
			AllowPrivilegeEscalation: ptr(false),
			ReadOnlyRootFilesystem:   ptr(true),
			Capabilities: &corev1.Capabilities{
				Drop: []corev1.Capability{"ALL"},
			},
		},
	}

	// Add TLS volume mounts
	if lr.Spec.TLS.Enabled {
		container.VolumeMounts = append(container.VolumeMounts, corev1.VolumeMount{
			Name:      "tls",
			MountPath: "/tls",
			ReadOnly:  true,
		})
	}

	// Add POD_IP env var (required for cluster-announce-ip)
	container.Env = append(container.Env, corev1.EnvVar{
		Name: "POD_IP",
		ValueFrom: &corev1.EnvVarSource{
			FieldRef: &corev1.ObjectFieldSelector{
				FieldPath: "status.podIP",
			},
		},
	})

	// Add POD_NAME env var (useful for logging)
	container.Env = append(container.Env, corev1.EnvVar{
		Name: "POD_NAME",
		ValueFrom: &corev1.EnvVarSource{
			FieldRef: &corev1.ObjectFieldSelector{
				FieldPath: "metadata.name",
			},
		},
	})

	// Add auth env var
	if lr.Spec.Auth.Enabled {
		container.Env = append(container.Env, corev1.EnvVar{
			Name: "REDIS_PASSWORD",
			ValueFrom: &corev1.EnvVarSource{
				SecretKeyRef: &corev1.SecretKeySelector{
					LocalObjectReference: corev1.LocalObjectReference{
						Name: lr.Spec.Auth.ExistingSecret,
					},
					Key: "password",
				},
			},
		})
	}

	return container
}

// buildClusterVolumes creates volumes for cluster mode pods
func buildClusterVolumes(lr *littleredv1alpha1.LittleRed) []corev1.Volume {
	volumes := []corev1.Volume{
		{
			Name: "config",
			VolumeSource: corev1.VolumeSource{
				ConfigMap: &corev1.ConfigMapVolumeSource{
					LocalObjectReference: corev1.LocalObjectReference{
						Name: configMapName(lr),
					},
				},
			},
		},
		{
			Name: "data",
			VolumeSource: corev1.VolumeSource{
				EmptyDir: &corev1.EmptyDirVolumeSource{},
			},
		},
	}

	// Add TLS volumes
	if lr.Spec.TLS.Enabled {
		volumes = append(volumes, corev1.Volume{
			Name: "tls",
			VolumeSource: corev1.VolumeSource{
				Secret: &corev1.SecretVolumeSource{
					SecretName: lr.Spec.TLS.ExistingSecret,
				},
			},
		})
	}

	return volumes
}

// buildClusterLivenessProbe creates the liveness probe for cluster mode
func buildClusterLivenessProbe(lr *littleredv1alpha1.LittleRed) *corev1.Probe {
	cmd := []string{"redis-cli"}
	if lr.Spec.Auth.Enabled {
		cmd = append(cmd, "-a", "$(REDIS_PASSWORD)")
	}
	if lr.Spec.TLS.Enabled {
		cmd = append(cmd, "--tls", "--insecure")
	}
	cmd = append(cmd, "ping")

	return &corev1.Probe{
		ProbeHandler: corev1.ProbeHandler{
			Exec: &corev1.ExecAction{
				Command: []string{
					"sh", "-c",
					strings.Join(cmd, " "),
				},
			},
		},
		InitialDelaySeconds: 10,
		PeriodSeconds:       10,
		TimeoutSeconds:      5,
		FailureThreshold:    3,
	}
}

// buildClusterReadinessProbe creates the readiness probe for cluster mode
func buildClusterReadinessProbe(lr *littleredv1alpha1.LittleRed) *corev1.Probe {
	cmd := []string{"redis-cli"}
	if lr.Spec.Auth.Enabled {
		cmd = append(cmd, "-a", "$(REDIS_PASSWORD)")
	}
	if lr.Spec.TLS.Enabled {
		cmd = append(cmd, "--tls", "--insecure")
	}
	cmd = append(cmd, "ping")

	return &corev1.Probe{
		ProbeHandler: corev1.ProbeHandler{
			Exec: &corev1.ExecAction{
				Command: []string{
					"sh", "-c",
					strings.Join(cmd, " "),
				},
			},
		},
		InitialDelaySeconds: 5,
		PeriodSeconds:       5,
		TimeoutSeconds:      3,
		FailureThreshold:    3,
	}
}

// buildClusterHeadlessService creates the headless Service for cluster pods
func buildClusterHeadlessService(lr *littleredv1alpha1.LittleRed) *corev1.Service {
	labels := commonLabels(lr)
	labels["app.kubernetes.io/component"] = "cluster"

	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      clusterHeadlessServiceName(lr),
			Namespace: lr.Namespace,
			Labels:    labels,
		},
		Spec: corev1.ServiceSpec{
			Type:                     corev1.ServiceTypeClusterIP,
			ClusterIP:                "None",
			Selector:                 clusterSelectorLabels(lr),
			PublishNotReadyAddresses: true,
			Ports: []corev1.ServicePort{
				{
					Name:       "redis",
					Port:       int32(littleredv1alpha1.RedisPort),
					TargetPort: intstr.FromString("redis"),
					Protocol:   corev1.ProtocolTCP,
				},
				{
					Name:       "cluster-bus",
					Port:       int32(littleredv1alpha1.ClusterBusPort),
					TargetPort: intstr.FromString("cluster-bus"),
					Protocol:   corev1.ProtocolTCP,
				},
			},
		},
	}
}

// buildClusterClientService creates the client Service for cluster mode
func buildClusterClientService(lr *littleredv1alpha1.LittleRed) *corev1.Service {
	labels := commonLabels(lr)
	for k, v := range lr.Spec.Service.Labels {
		labels[k] = v
	}

	ports := []corev1.ServicePort{
		{
			Name:       "redis",
			Port:       int32(littleredv1alpha1.RedisPort),
			TargetPort: intstr.FromString("redis"),
			Protocol:   corev1.ProtocolTCP,
		},
	}

	if lr.Spec.Metrics.IsEnabled() {
		ports = append(ports, corev1.ServicePort{
			Name:       "metrics",
			Port:       int32(littleredv1alpha1.RedisExporterPort),
			TargetPort: intstr.FromString("metrics"),
			Protocol:   corev1.ProtocolTCP,
		})
	}

	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:        serviceName(lr),
			Namespace:   lr.Namespace,
			Labels:      labels,
			Annotations: lr.Spec.Service.Annotations,
		},
		Spec: corev1.ServiceSpec{
			Type:     lr.Spec.Service.Type,
			Selector: clusterSelectorLabels(lr),
			Ports:    ports,
		},
	}
}
