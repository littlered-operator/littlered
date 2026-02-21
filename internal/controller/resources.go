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
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"
	"text/template"

	monitoringv1 "github.com/prometheus-operator/prometheus-operator/pkg/apis/monitoring/v1"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"

	littleredv1alpha1 "github.com/littlered-operator/littlered-operator/api/v1alpha1"
)

// Annotation keys for config hash
const (
	AnnotationConfigHash              = "chuck-chuck-chuck.net/config-hash"
	AnnotationDisablePolling          = "chuck-chuck-chuck.net/disable-polling"
	AnnotationDisableEventMonitoring  = "chuck-chuck-chuck.net/disable-event-monitoring"
	AnnotationDebugSkipSlotAssignment = "chuck-chuck-chuck.net/debug-skip-slot-assignment"
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
	LabelRole     = "chuck-chuck-chuck.net/role"
	RoleMaster    = "master"
	RoleReplica   = "replica"
	RoleOrphan    = "orphan"
	RoleUndefined = "undefined"
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
		"app.kubernetes.io/name":       "littlered",
		"app.kubernetes.io/instance":   lr.Name,
		"app.kubernetes.io/managed-by": "littlered-operator",
		"app.kubernetes.io/version":    lr.Spec.Image.Tag,
		"chuck-chuck-chuck.net/mode":   lr.Spec.Mode,
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

	// Disable persistence (in-memory only)
	sb.WriteString("\n# Persistence disabled (in-memory mode)\n")
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
	// While the bootstrap-in-progress file exists, we report success to avoid being killed
	// by K8s before authorization is complete.
	cmd := []string{
		"if [ -f /data/bootstrap-in-progress ]; then exit 0; fi;",
		"redis-cli",
	}
	if lr.Spec.Auth.Enabled {
		cmd = append(cmd, "-a", "$(REDIS_PASSWORD)")
	}
	if lr.Spec.TLS.Enabled {
		cmd = append(cmd, "--tls", "--insecure")
	}
	cmd = append(cmd, "-t", "2", "ping")

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
	// While bootstrapping, we are NOT ready.
	cmd := []string{
		"if [ -f /data/bootstrap-in-progress ]; then exit 1; fi;",
		"redis-cli",
	}
	if lr.Spec.Auth.Enabled {
		cmd = append(cmd, "-a", "$(REDIS_PASSWORD)")
	}
	if lr.Spec.TLS.Enabled {
		cmd = append(cmd, "--tls", "--insecure")
	}
	cmd = append(cmd, "-t", "2", "ping")

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

// buildSentinelLivenessProbe creates the liveness probe for the Redis container in sentinel mode.
//
// Beyond the basic PING check used in standalone mode, this probe detects "zombie replicas":
// pods that started following a ghost master IP (an IP no longer belonging to any running pod).
// This happens when a pod restarts during a Sentinel failover transition and the ghost master
// responds briefly to PING before dying — the startup script commits to REPLICAOF <ghost>,
// then the ghost disappears, leaving the replica permanently stuck with link:down.
// Sentinel cannot self-heal this because the zombie never syncs to the real master, so the
// real master doesn't list it as a replica, and Sentinel therefore never issues SLAVEOF to it.
//
// The probe logic:
//  1. Bootstrap guard: pass while /data/bootstrap-in-progress exists (pod still starting)
//  2. Masters always pass
//  3. Replicas with link:up pass
//  4. Replicas with link:down but a still-reachable master pass — this is a legitimate failover
//     in progress; Sentinel will issue SLAVEOF to redirect us once the new master is elected.
//  5. Replicas with link:down and an unreachable master fail — the master is gone forever
//     (ghost), Sentinel will never redirect us, so k8s should restart the pod.
//
// The failureThreshold is computed from the CR's downAfterMilliseconds + failoverTimeout so
// that a legitimate crash failover always completes (and Sentinel redirects the replica) before
// the threshold is reached, avoiding false-positive restarts of healthy replicas.
func buildSentinelLivenessProbe(lr *littleredv1alpha1.LittleRed) *corev1.Probe {
	tlsFlags := ""
	if lr.Spec.TLS.Enabled {
		tlsFlags = " --tls --insecure"
	}

	// Compute a failure threshold large enough to survive a legitimate crash failover.
	// A replica has master_link_status:down (with an unreachable master) for the entire
	// duration of: downAfterMilliseconds + failoverTimeout + buffer.
	// Once Sentinel issues SLAVEOF, the replica's master_host changes and the probe passes again.
	const periodSeconds = int64(10)
	downAfterMs := int64(30000)        // Sentinel default
	failoverTimeoutMs := int64(180000) // Sentinel default
	if lr.Spec.Sentinel != nil {
		if lr.Spec.Sentinel.DownAfterMilliseconds > 0 {
			downAfterMs = int64(lr.Spec.Sentinel.DownAfterMilliseconds)
		}
		if lr.Spec.Sentinel.FailoverTimeout > 0 {
			failoverTimeoutMs = int64(lr.Spec.Sentinel.FailoverTimeout)
		}
	}
	const bufferMs = int64(15000)
	failoverWindowMs := downAfterMs + failoverTimeoutMs + bufferMs
	failureThreshold := int32((failoverWindowMs + periodSeconds*1000 - 1) / (periodSeconds * 1000))
	if failureThreshold < 3 {
		failureThreshold = 3
	}

	tmpl := template.Must(template.New("sentinel-liveness").Delims("[[", "]]").Parse(
		`if [ -f /data/bootstrap-in-progress ]; then exit 0; fi
AUTH=""; [ -n "$REDIS_PASSWORD" ] && AUTH="-a $REDIS_PASSWORD"
info=$(redis-cli -h 127.0.0.1 $AUTH[[.TLSFlags]] -t 2 info replication 2>/dev/null) || exit 1
echo "$info" | grep -q "^role:master" && exit 0
echo "$info" | grep -q "^master_link_status:up" && exit 0
master_host=$(echo "$info" | grep "^master_host:" | cut -d: -f2 | tr -d "\r ")
[ -z "$master_host" ] && exit 1
redis-cli -h "$master_host" -p [[.RedisPort]] $AUTH[[.TLSFlags]] -t 2 ping >/dev/null 2>&1 && exit 0
exit 1`))
	var buf bytes.Buffer
	err := tmpl.Execute(&buf, struct {
		TLSFlags  string
		RedisPort int
	}{
		TLSFlags:  tlsFlags,
		RedisPort: littleredv1alpha1.RedisPort,
	})
	if err != nil {
		panic(fmt.Sprintf("failed to execute sentinel liveness template: %v", err))
	}
	script := buf.String()

	return &corev1.Probe{
		ProbeHandler: corev1.ProbeHandler{
			Exec: &corev1.ExecAction{
				Command: []string{"sh", "-c", script},
			},
		},
		InitialDelaySeconds: 15,
		PeriodSeconds:       int32(periodSeconds),
		TimeoutSeconds:      5,
		FailureThreshold:    failureThreshold,
	}
}

// buildSentinelReadinessProbe creates the readiness probe for the Redis container in sentinel mode.
//
// A replica is ready only when its replication link to the master is up.
// This ensures that a zombie replica (link:down following a ghost master) stops receiving
// traffic immediately, before the liveness probe eventually kills and replaces the pod.
func buildSentinelReadinessProbe(lr *littleredv1alpha1.LittleRed) *corev1.Probe {
	tlsFlags := ""
	if lr.Spec.TLS.Enabled {
		tlsFlags = " --tls --insecure"
	}

	tmpl := template.Must(template.New("sentinel-readiness").Delims("[[", "]]").Parse(
		`if [ -f /data/bootstrap-in-progress ]; then exit 1; fi
AUTH=""; [ -n "$REDIS_PASSWORD" ] && AUTH="-a $REDIS_PASSWORD"
info=$(redis-cli -h 127.0.0.1 $AUTH[[.TLSFlags]] -t 2 info replication 2>/dev/null) || exit 1
echo "$info" | grep -q "^role:master" && exit 0
echo "$info" | grep -q "^master_link_status:up" && exit 0
exit 1`))
	var buf bytes.Buffer
	err := tmpl.Execute(&buf, struct {
		TLSFlags string
	}{
		TLSFlags: tlsFlags,
	})
	if err != nil {
		panic(fmt.Sprintf("failed to execute sentinel readiness template: %v", err))
	}
	script := buf.String()

	return &corev1.Probe{
		ProbeHandler: corev1.ProbeHandler{
			Exec: &corev1.ExecAction{
				Command: []string{"sh", "-c", script},
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

	sb.WriteString("# LittleRed Sentinel configuration\n")
	sb.WriteString(fmt.Sprintf("port %d\n", littleredv1alpha1.SentinelPort))
	sb.WriteString("dir /data\n")

	// Auth configuration
	if lr.Spec.Auth.Enabled {
		// Password will be set via command line
		sb.WriteString("\n# Auth will be configured via environment\n")
	}

	// Announce settings for proper discovery
	// For pure in-memory mode, we strictly use IPs. This ensures that when a pod
	// restarts and gets a new IP, it is treated as a new (empty) node, preventing
	// it from being incorrectly trusted as a former master with data.
	sb.WriteString("\n# Announce settings\n")
	sb.WriteString("sentinel resolve-hostnames no\n")
	sb.WriteString("sentinel announce-hostnames no\n")

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

	// Disable persistence (in-memory only)
	sb.WriteString("\n# Persistence disabled (in-memory mode)\n")
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

	// MinReadySeconds for Sentinel mode: allow time for Sentinel-managed failover
	// Sentinels need to detect master is down, reach quorum, and promote a replica
	// Default down-after-milliseconds is 30000ms, plus promotion time
	minReadySeconds := int32(35)
	if lr.Spec.UpdateStrategy.MinReadySeconds != nil {
		minReadySeconds = *lr.Spec.UpdateStrategy.MinReadySeconds
	}

	sts := &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      statefulSetName(lr),
			Namespace: lr.Namespace,
			Labels:    labels,
		},
		Spec: appsv1.StatefulSetSpec{
			Replicas:        &replicas,
			ServiceName:     replicasServiceName(lr),
			MinReadySeconds: minReadySeconds,
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
	// Script to configure replication strictly based on Sentinel state.
	// This is "Operator-Authorized" startup: no pod assumes mastership
	// unless Sentinel (configured by the Operator) says so.
	script := `#!/bin/sh
set -e
set -x

# Helper to log with timestamp
log() {
  echo "$(date '+%Y-%m-%d %H:%M:%S') [Startup] $1"
}

# Create marker file to tell liveness probe we are starting up
touch /data/bootstrap-in-progress

cp /etc/redis/redis.conf /data/redis.conf

HOSTNAME=$(hostname)
SENTINEL_SVC="[[.Name]]-sentinel.[[.Namespace]].svc"

log "Starting Redis node $HOSTNAME. Waiting for Sentinel authorization..."

AUTH_ARGS=""
SENTINEL_AUTH_ARGS=""
if [ -n "$REDIS_PASSWORD" ]; then
  AUTH_ARGS="--requirepass $REDIS_PASSWORD --masterauth $REDIS_PASSWORD"
  SENTINEL_AUTH_ARGS="-a $REDIS_PASSWORD"
fi

# Kill-9 / in-pod crash protection (see ADR-001 amendment):
# If Sentinel already has a non-empty run-id stored for a master at MY IP, a previous
# Redis process ran here and Sentinel observed it. The container must have restarted
# (kill-9, OOM, etc.) while the pod IP stayed the same.
# Starting as master in that state means an empty node becomes master and replicas
# perform a FULLRESYNC from empty data — all data for this shard is lost.
#
# Defence: set YIELD_MASTER=true. The main loop sleeps (Redis NOT started) until
# Sentinel's down-after-milliseconds fires, failover completes, and a different
# master is elected. We then join as a replica.
STORED_RUNID=$(redis-cli -h $SENTINEL_SVC -p 26379 $SENTINEL_AUTH_ARGS -t 2 --raw sentinel master mymaster 2>/dev/null \
  | awk 'prev=="runid"{print; exit} {prev=$0}' || true)
SENTINEL_MASTER_IP=$(redis-cli -h $SENTINEL_SVC -p 26379 $SENTINEL_AUTH_ARGS -t 2 --raw sentinel get-master-addr-by-name mymaster 2>/dev/null \
  | head -n 1 || true)

YIELD_MASTER=false
YIELD_COUNT=0
# 60 iterations x 2 s = 120 s: enough for 30 s SDOWN + failover + margin.
# If failover never completes (no eligible replica), we give up and start as master.
YIELD_LIMIT=60

if [ "$SENTINEL_MASTER_IP" = "$POD_IP" ] && [ -n "$STORED_RUNID" ]; then
  log "Kill-9 protection: Sentinel knows $POD_IP as master (run-id=$STORED_RUNID)."
  log "Suppressing Redis start. Waiting for Sentinel SDOWN detection and failover..."
  YIELD_MASTER=true
fi

# Loop until Sentinel has a master for us
while true; do
  # Use --raw to get just the values (IP/Host on line 1, Port on line 2)
  # Use -t to avoid hanging on DNS or network issues
  SENTINEL_REPLY=$(redis-cli -h $SENTINEL_SVC -p 26379 $SENTINEL_AUTH_ARGS -t 2 --raw sentinel get-master-addr-by-name mymaster || true)
  CURRENT_MASTER_HOST=$(echo "$SENTINEL_REPLY" | head -n 1)
  CURRENT_MASTER_PORT=$(echo "$SENTINEL_REPLY" | sed -n '2p')

  if [ -n "$CURRENT_MASTER_HOST" ]; then
    log "Sentinel reported master at $CURRENT_MASTER_HOST:$CURRENT_MASTER_PORT"

    # Check if reported master is ME (compare IP)
    if [ "$CURRENT_MASTER_HOST" = "$POD_IP" ]; then
      if [ "$YIELD_MASTER" = "true" ]; then
        YIELD_COUNT=$((YIELD_COUNT + 1))
        if [ "$YIELD_COUNT" -ge "$YIELD_LIMIT" ]; then
          log "Yield timeout (${YIELD_LIMIT} attempts). No failover completed — no eligible replica? Starting as master."
          YIELD_MASTER=false
        else
          log "Yielding ($YIELD_COUNT/$YIELD_LIMIT): still master per Sentinel. Waiting for SDOWN + failover..."
          sleep 2
          continue
        fi
      fi
      log "I am the authorized master. Starting redis-server..."
      rm -f /data/bootstrap-in-progress
      exec redis-server /data/redis.conf --replica-announce-ip ${POD_IP} $AUTH_ARGS
    fi

    # I am a replica. Check if master is reachable before committing.
    # Retry several times: during initial cluster boot all pods start in parallel,
    # so the master's redis-server may not be listening yet even though the IP is valid.
    PING_ATTEMPTS=6
    PING_DELAY=3
    MASTER_ALIVE=false
    for attempt in $(seq 1 $PING_ATTEMPTS); do
      if redis-cli -h $CURRENT_MASTER_HOST -p $CURRENT_MASTER_PORT $SENTINEL_AUTH_ARGS -t 2 ping > /dev/null 2>&1; then
        MASTER_ALIVE=true
        break
      fi
      log "Master $CURRENT_MASTER_HOST not yet reachable (attempt $attempt/$PING_ATTEMPTS). Waiting ${PING_DELAY}s..."
      sleep $PING_DELAY
    done

    if [ "$MASTER_ALIVE" = "true" ]; then
       log "Master is alive. Joining $CURRENT_MASTER_HOST as replica..."
       rm -f /data/bootstrap-in-progress
       exec redis-server /data/redis.conf --replicaof $CURRENT_MASTER_HOST $CURRENT_MASTER_PORT --replica-announce-ip ${POD_IP} $AUTH_ARGS
    fi

    # Master is unreachable after retries (likely a ghost IP). Start as bare server
    # so Sentinel can discover us and perform a failover. This avoids both the
    # deadlock (ADR-002) and the zombie-replica problem.
    log "Master $CURRENT_MASTER_HOST unreachable after $PING_ATTEMPTS attempts. Starting bare for Sentinel discovery..."
    rm -f /data/bootstrap-in-progress
    exec redis-server /data/redis.conf --replica-announce-ip ${POD_IP} $AUTH_ARGS
  fi

  log "Sentinel has no master info. Waiting..."
  sleep 2
done`
	tmpl := template.Must(template.New("sentinel-startup").Delims("[[", "]]").Parse(script))
	var buf bytes.Buffer
	err := tmpl.Execute(&buf, struct {
		Name      string
		Namespace string
	}{
		Name:      lr.Name,
		Namespace: lr.Namespace,
	})
	if err != nil {
		panic(fmt.Sprintf("failed to execute sentinel startup template: %v", err))
	}
	finalScript := buf.String()

	preStopTmpl := template.Must(template.New("sentinel-prestop").Delims("[[", "]]").Parse(`
# Redirect all output from this point forward
exec >/proc/1/fd/1 2>&1

export PS4='preStop + '
set -x

echo "preStop hook starting at $(date), PID $$"

ROLE=$(redis-cli info replication | grep role | cut -d: -f2 | tr -d '\r')
if [ "$ROLE" = "master" ]; then
  echo "I am the master. Proactively failing over..."
  AUTH_ARGS=""
  if [ -n "$REDIS_PASSWORD" ]; then
    AUTH_ARGS="-a $REDIS_PASSWORD"
  fi
  SENTINEL_SVC="[[.Name]]-sentinel.[[.Namespace]].svc"
  # Wait until Sentinel has discovered all expected replicas before forcing a
  # failover. If the master was just restarted, Sentinel may not yet know about
  # replicas that connected within the last polling cycle (~1 s). Triggering
  # SENTINEL FAILOVER too early means those replicas miss the REPLICAOF
  # reconfiguration step and get stuck pointing at the dead master IP.
  EXPECTED_SLAVES=2
  for i in $(seq 1 10); do
    SLAVE_COUNT=$(redis-cli --raw -h $SENTINEL_SVC -p 26379 $AUTH_ARGS SENTINEL SLAVES mymaster 2>/dev/null | grep -c "^name$" || echo 0)
    if [ "$SLAVE_COUNT" -ge "$EXPECTED_SLAVES" ]; then
      echo "Sentinel knows $SLAVE_COUNT/$EXPECTED_SLAVES replicas. Proceeding with failover."
      break
    fi
    echo "Waiting for Sentinel to discover replicas ($SLAVE_COUNT/$EXPECTED_SLAVES)..."
    sleep 1
  done
  # Pause writes for 30s to ensure a clean handover
  redis-cli $AUTH_ARGS CLIENT PAUSE 30000 WRITE || true
  # Trigger failover
  redis-cli -h $SENTINEL_SVC -p 26379 $AUTH_ARGS SENTINEL failover mymaster || true
  # Wait for Sentinel to acknowledge the new master
  for i in $(seq 1 10); do
    MASTER_IP=$(redis-cli -h $SENTINEL_SVC -p 26379 $AUTH_ARGS -t 2 --raw sentinel get-master-addr-by-name mymaster | head -n 1 || true)
    if [ -n "$MASTER_IP" ] && [ "$MASTER_IP" != "$POD_IP" ]; then
       echo "Failover confirmed. New master: $MASTER_IP"
       exit 0
    fi
    sleep 1
  done
fi`))
	var preStopBuf bytes.Buffer
	err = preStopTmpl.Execute(&preStopBuf, struct {
		Name      string
		Namespace string
	}{
		Name:      lr.Name,
		Namespace: lr.Namespace,
	})
	if err != nil {
		panic(fmt.Sprintf("failed to execute sentinel prestop template: %v", err))
	}
	preStopScript := preStopBuf.String()

	container := corev1.Container{
		Name:            "redis",
		Image:           lr.Spec.Image.FullImage(),
		ImagePullPolicy: lr.Spec.Image.PullPolicy,
		Command:         []string{"sh", "-c", finalScript},
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
		Env: []corev1.EnvVar{
			{
				Name: "LITTLERED_NAME",
				ValueFrom: &corev1.EnvVarSource{
					FieldRef: &corev1.ObjectFieldSelector{
						FieldPath: "metadata.labels['app.kubernetes.io/instance']",
					},
				},
			},
			{
				Name: "LITTLERED_NAMESPACE",
				ValueFrom: &corev1.EnvVarSource{
					FieldRef: &corev1.ObjectFieldSelector{
						FieldPath: "metadata.namespace",
					},
				},
			},
			{
				Name: "POD_IP",
				ValueFrom: &corev1.EnvVarSource{
					FieldRef: &corev1.ObjectFieldSelector{
						FieldPath: "status.podIP",
					},
				},
			},
		},
		LivenessProbe:  buildSentinelLivenessProbe(lr),
		ReadinessProbe: buildSentinelReadinessProbe(lr),
		Lifecycle: &corev1.Lifecycle{
			PreStop: &corev1.LifecycleHandler{
				Exec: &corev1.ExecAction{
					Command: []string{"sh", "-c", preStopScript},
				},
			},
		},
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

	// Use text/template for consistency across all startup scripts
	scriptTmpl := template.Must(template.New("sentinel-startup").Delims("[[", "]]").Parse(`#!/bin/sh
set -e
set -x

# Helper to log with timestamp
log() {
  echo "$(date '+%Y-%m-%d %H:%M:%S') [Startup] $1"
}

cp /etc/sentinel/sentinel.conf /data/sentinel.conf

AUTH_ARGS=""
if [ -n "$REDIS_PASSWORD" ]; then
  AUTH_ARGS="--sentinel auth-pass mymaster $REDIS_PASSWORD"
fi

log "Starting Sentinel node with IP ${POD_IP}..."
exec redis-sentinel /data/sentinel.conf --sentinel announce-ip ${POD_IP} $AUTH_ARGS
`))
	var buf bytes.Buffer
	err := scriptTmpl.Execute(&buf, struct{}{})
	if err != nil {
		panic(fmt.Sprintf("failed to execute sentinel container template: %v", err))
	}
	script := buf.String()

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

	// Add POD_IP env var
	container.Env = append(container.Env, corev1.EnvVar{
		Name: "POD_IP",
		ValueFrom: &corev1.EnvVarSource{
			FieldRef: &corev1.ObjectFieldSelector{
				FieldPath: "status.podIP",
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

	// Disable persistence (in-memory only)
	sb.WriteString("\n# Persistence disabled (in-memory mode)\n")
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

	// MinReadySeconds ensures pods are stable before next pod is restarted during rolling updates.
	// For cluster mode with replicas, this allows time for automatic failover to complete
	// before the next master is taken down.
	// - cluster-node-timeout (default 15000ms) for replica to detect master is down
	// - A few seconds for election and promotion
	// - Buffer for operator reconciliation
	// Total: 30 seconds is a safe default for replica mode
	minReadySeconds := int32(0)
	if lr.Spec.UpdateStrategy.MinReadySeconds != nil {
		// User-specified value takes precedence
		minReadySeconds = *lr.Spec.UpdateStrategy.MinReadySeconds
	} else if cluster.ReplicasPerShard != nil && *cluster.ReplicasPerShard > 0 {
		// Default for replica mode: 30 seconds
		minReadySeconds = 30
	}

	sts := &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      clusterStatefulSetName(lr),
			Namespace: lr.Namespace,
			Labels:    labels,
		},
		Spec: appsv1.StatefulSetSpec{
			Replicas:        &replicas,
			ServiceName:     clusterHeadlessServiceName(lr),
			MinReadySeconds: minReadySeconds,
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
	// Startup script that implements "Kill-9 / Crash Protection" (see ADR-001 amendment):
	//
	// 1. Detects if this is a container restart (memory lost, IP same) by checking
	//    for a surviving nodes.conf.
	// 2. If it is a restart, it YIELDS startup by polling peers until they confirm
	//    that a failover has occurred (i.e. this IP is no longer seen as master with SLOTS).
	//    This prevents the "empty master" from wiping its promoted replicas.
	// 3. If failover is not confirmed within 30s, it actively attempts to find its
	//    own replica and issues an aggressive CLUSTER FAILOVER TAKEOVER to break the deadlock.
	// 4. If failover still fails after 60s, it yields to the K8s liveness probe.
	// 5. Copies the read-only config to /data to allow for CONFIG REWRITE.
	// 6. Always deletes nodes.conf before starting to ensure a fresh identity.
	startupScript := `#!/bin/sh
set -e

# Helper to log with timestamp
log() {
  echo "$(date '+%Y-%m-%dT%H:%M:%SZ') [Startup] $1"
}

log "Starting Redis cluster node ${POD_NAME} with IP: ${POD_IP}"

# Create marker file to tell liveness probe we are starting up
touch /data/bootstrap-in-progress

# STEP 1: Detect if this is a container restart (Dangerous) or a fresh Pod (Safe)
RESTART_DETECTED=false
if [ -f /data/nodes.conf ]; then
  log "Marker nodes.conf found. This is a container restart (memory lost, IP same). Data loss risk is HIGH."
  RESTART_DETECTED=true
else
  log "No nodes.conf found. This is a fresh Pod start or bootstrap. Safe to proceed."
fi

# STEP 2: Copy config to writable /data to support CONFIG REWRITE
cp /etc/redis/redis.conf /data/redis.conf

# STEP 3: Yield Loop (Only for restarts)
if [ "$RESTART_DETECTED" = "true" ]; then
  log "Safety Check: Verifying failover state on peers..."

  # Configuration from environment
  AUTH_ARGS=""
  [ -n "$REDIS_PASSWORD" ] && AUTH_ARGS="-a $REDIS_PASSWORD"
  PEER_SERVICE="[[.Name]]-cluster.[[.Namespace]].svc"

  # Wait up to 60s for peers to confirm failover
  for i in $(seq 1 12); do
    PEER_IPS=$(getent hosts "$PEER_SERVICE" | awk '{print $1}')
    STILL_OWN_SLOTS=false
    PEERS_CONTACTED=0
    MY_NODE_ID=""
    MY_REPLICA_IP=""

    for peer in $PEER_IPS; do
      [ "$peer" = "$POD_IP" ] && continue
      NODES_INFO=$(redis-cli -h "$peer" -p [[.RedisPort]] $AUTH_ARGS -t 2 CLUSTER NODES 2>/dev/null || true)
      if [ -n "$NODES_INFO" ]; then
        PEERS_CONTACTED=$((PEERS_CONTACTED + 1))

        # Identify myself in the peer's view
        MY_LINE=$(echo "$NODES_INFO" | grep "$POD_IP:[[.RedisPort]]")
        if [ -n "$MY_LINE" ]; then
           MY_NODE_ID=$(echo "$MY_LINE" | awk '{print $1}')
           IS_MASTER=$(echo "$MY_LINE" | awk '{print $3}' | grep -q "master" && echo "true" || echo "false")
           HAS_SLOTS=$(echo "$MY_LINE" | awk '{print $9}')

           if [ "$IS_MASTER" = "true" ] && [ -n "$HAS_SLOTS" ]; then
             STILL_OWN_SLOTS=true
             log "Attempt $i/12: Peer $peer still sees me as master with slots assigned ($HAS_SLOTS). Waiting..."

             # If we've waited > 30s (attempt 6), try to find a replica to force failover
             if [ "$i" -ge 6 ]; then
                MY_REPLICA_IP=$(echo "$NODES_INFO" | grep "slave $MY_NODE_ID" | grep -v "fail" | head -n 1 | awk '{print $2}' | cut -d: -f1 | cut -d@ -f1)
                if [ -n "$MY_REPLICA_IP" ]; then
                   log "Deadlock detected! Forcing replica $MY_REPLICA_IP to TAKEOVER slots for dead node $MY_NODE_ID..."
                   redis-cli -h "$MY_REPLICA_IP" -p [[.RedisPort]] $AUTH_ARGS -t 2 CLUSTER FAILOVER TAKEOVER || true
                fi
             fi
             break
           fi
        fi
      fi
    done

    if [ "$STILL_OWN_SLOTS" = "false" ] && [ "$PEERS_CONTACTED" -gt 0 ]; then
      log "Verified: Failover confirmed (I no longer own slots). Safe to start."
      break
    fi
    [ "$i" -lt 12 ] && sleep 5
  done

  if [ "$STILL_OWN_SLOTS" = "true" ]; then
    log "FATAL: Failover timeout. I still own slots in the cluster map. Yielding to Kubernetes liveness probe..."
    rm -f /data/bootstrap-in-progress
    while true; do sleep 3600; done
  fi
fi

# STEP 4: Guarantee a fresh node ID on every process start
if [ "$RESTART_DETECTED" = "true" ]; then
  log "Removing stale nodes.conf"
  rm -f /data/nodes.conf
fi

# STEP 5: Start Redis
log "Final launch..."
rm -f /data/bootstrap-in-progress
exec redis-server /data/redis.conf \
  --cluster-enabled yes \
  --cluster-config-file /data/nodes.conf \
  --cluster-announce-ip ${POD_IP} \
  --cluster-announce-port [[.RedisPort]] \
  --cluster-announce-bus-port [[.ClusterBusPort]] \
  --requirepass "${REDIS_PASSWORD}" \
  --masterauth "${REDIS_PASSWORD}"
`
	tmpl := template.Must(template.New("startup").Delims("[[", "]]").Parse(startupScript))
	var buf bytes.Buffer
	err := tmpl.Execute(&buf, struct {
		Name           string
		Namespace      string
		RedisPort      int
		ClusterBusPort int
	}{
		Name:           lr.Name,
		Namespace:      lr.Namespace,
		RedisPort:      littleredv1alpha1.RedisPort,
		ClusterBusPort: littleredv1alpha1.ClusterBusPort,
	})
	if err != nil {
		// This should never happen with Must() and valid data
		panic(fmt.Sprintf("failed to execute cluster startup template: %v", err))
	}
	script := buf.String()

	preStopTmpl := template.Must(template.New("cluster-prestop").Delims("[[", "]]").Parse(`
# Redirect all output from this point forward
exec >/proc/1/fd/1 2>&1

export PS4='preStop + '
set -x

echo "preStop hook starting at $(date), PID $$"

AUTH_ARGS=""
if [ -n "$REDIS_PASSWORD" ]; then
  AUTH_ARGS="-a $REDIS_PASSWORD"
fi

# Check if I am a master
MY_ID=$(redis-cli $AUTH_ARGS cluster nodes | grep myself | awk '{print $1}')
IS_MASTER=$(redis-cli $AUTH_ARGS cluster nodes | grep myself | grep -q master && echo "yes" || echo "no")

if [ "$IS_MASTER" != "yes" ]; then
  echo "I am not a master. Safe to stop."
  exit 0
fi

# Find a healthy replica for my shard
REPLICA_IP=$(redis-cli $AUTH_ARGS cluster nodes | grep "$MY_ID" | grep "slave" | grep -v "fail" | head -n 1 | awk '{print $2}' | cut -d: -f1 | cut -d@ -f1)

if [ -z "$REPLICA_IP" ]; then
  echo "No healthy replica found to take over. Proceeding with restart."
  exit 0
fi

echo "Requesting replica $REPLICA_IP to take over shard..."
redis-cli -h $REPLICA_IP $AUTH_ARGS CLUSTER FAILOVER

# Wait for role swap (I become slave)
for i in $(seq 1 10); do
  NEW_ROLE=$(redis-cli $AUTH_ARGS cluster nodes | grep myself | awk '{print $3}')
  if echo "$NEW_ROLE" | grep -q "slave"; then
    echo "Failover successful. I am now a replica."
    exit 0
  fi
  sleep 1
done`))
	var preStopBuf bytes.Buffer
	err = preStopTmpl.Execute(&preStopBuf, struct{}{})
	if err != nil {
		panic(fmt.Sprintf("failed to execute cluster prestop template: %v", err))
	}
	preStopScript := preStopBuf.String()

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
		Lifecycle: &corev1.Lifecycle{
			PreStop: &corev1.LifecycleHandler{
				Exec: &corev1.ExecAction{
					Command: []string{"sh", "-c", preStopScript},
				},
			},
		},
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

	// Add NAMESPACE env var
	container.Env = append(container.Env, corev1.EnvVar{
		Name: "NAMESPACE",
		ValueFrom: &corev1.EnvVarSource{
			FieldRef: &corev1.ObjectFieldSelector{
				FieldPath: "metadata.namespace",
			},
		},
	})

	// Add CLUSTER_NAME env var
	container.Env = append(container.Env, corev1.EnvVar{
		Name:  "CLUSTER_NAME",
		Value: lr.Name,
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
