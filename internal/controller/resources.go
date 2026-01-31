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
	"fmt"
	"strings"

	monitoringv1 "github.com/prometheus-operator/prometheus-operator/pkg/apis/monitoring/v1"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"

	littleredv1alpha1 "github.com/tanne3/littlered-operator/api/v1alpha1"
)

// Resource name helpers
func configMapName(lr *littleredv1alpha1.LittleRed) string {
	return fmt.Sprintf("%s-config", lr.Name)
}

func statefulSetName(lr *littleredv1alpha1.LittleRed) string {
	return fmt.Sprintf("%s-redis", lr.Name)
}

func serviceName(lr *littleredv1alpha1.LittleRed) string {
	return lr.Name
}

func serviceMonitorName(lr *littleredv1alpha1.LittleRed) string {
	return lr.Name
}

// commonLabels returns the standard labels applied to all resources
func commonLabels(lr *littleredv1alpha1.LittleRed) map[string]string {
	return map[string]string{
		"app.kubernetes.io/name":       "littlered",
		"app.kubernetes.io/instance":   lr.Name,
		"app.kubernetes.io/managed-by": "littlered-operator",
		"app.kubernetes.io/version":    lr.Spec.Image.Tag,
		"littlered.tanne3.de/mode":     lr.Spec.Mode,
	}
}

// selectorLabels returns labels used for selecting pods
func selectorLabels(lr *littleredv1alpha1.LittleRed) map[string]string {
	return map[string]string{
		"app.kubernetes.io/name":     "littlered",
		"app.kubernetes.io/instance": lr.Name,
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
					Annotations: lr.Spec.PodTemplate.Annotations,
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

func ptr[T any](v T) *T {
	return &v
}
