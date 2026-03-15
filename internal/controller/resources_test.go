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
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	littleredv1alpha1 "github.com/littlered-operator/littlered-operator/api/v1alpha1"
)

const (
	testLRName       = "my-cache"
	testNamespace    = "test-ns"
	portNameMetrics  = "metrics"
)

// Helper to create a minimal LittleRed for testing
func newTestLittleRed(name, namespace string) *littleredv1alpha1.LittleRed {
	lr := &littleredv1alpha1.LittleRed{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
		},
		Spec: littleredv1alpha1.LittleRedSpec{},
	}
	// Apply defaults
	lr.SetDefaults()
	return lr
}

// ============================================================================
// Name Helper Tests
// ============================================================================

func TestConfigMapName(t *testing.T) {
	lr := newTestLittleRed(testLRName, "default")
	expected := "my-cache-config"
	if got := configMapName(lr); got != expected {
		t.Errorf("configMapName() = %q, want %q", got, expected)
	}
}

func TestSentinelConfigMapName(t *testing.T) {
	lr := newTestLittleRed(testLRName, "default")
	expected := "my-cache-sentinel-config"
	if got := sentinelConfigMapName(lr); got != expected {
		t.Errorf("sentinelConfigMapName() = %q, want %q", got, expected)
	}
}

func TestStatefulSetName(t *testing.T) {
	lr := newTestLittleRed(testLRName, "default")
	expected := "my-cache-redis"
	if got := statefulSetName(lr); got != expected {
		t.Errorf("statefulSetName() = %q, want %q", got, expected)
	}
}

func TestSentinelStatefulSetName(t *testing.T) {
	lr := newTestLittleRed(testLRName, "default")
	expected := "my-cache-sentinel"
	if got := sentinelStatefulSetName(lr); got != expected {
		t.Errorf("sentinelStatefulSetName() = %q, want %q", got, expected)
	}
}

func TestServiceName(t *testing.T) {
	lr := newTestLittleRed(testLRName, "default")
	expected := testLRName
	if got := serviceName(lr); got != expected {
		t.Errorf("serviceName() = %q, want %q", got, expected)
	}
}

func TestReplicasServiceName(t *testing.T) {
	lr := newTestLittleRed(testLRName, "default")
	expected := "my-cache-replicas"
	if got := replicasServiceName(lr); got != expected {
		t.Errorf("replicasServiceName() = %q, want %q", got, expected)
	}
}

func TestSentinelServiceName(t *testing.T) {
	lr := newTestLittleRed(testLRName, "default")
	expected := "my-cache-sentinel"
	if got := sentinelServiceName(lr); got != expected {
		t.Errorf("sentinelServiceName() = %q, want %q", got, expected)
	}
}

// ============================================================================
// Label Tests
// ============================================================================

func TestCommonLabels(t *testing.T) {
	lr := newTestLittleRed(testLRName, "default")
	labels := commonLabels(lr)

	tests := []struct {
		key      string
		expected string
	}{
		{"app.kubernetes.io/name", "littlered"},
		{"app.kubernetes.io/instance", testLRName},
		{"app.kubernetes.io/managed-by", "littlered-operator"},
		{"app.kubernetes.io/version", "8.0"},
		{"chuck-chuck-chuck.net/mode", ModeStandalone},
	}

	for _, tt := range tests {
		t.Run(tt.key, func(t *testing.T) {
			if got := labels[tt.key]; got != tt.expected {
				t.Errorf("commonLabels()[%q] = %q, want %q", tt.key, got, tt.expected)
			}
		})
	}
}

func TestSelectorLabels(t *testing.T) {
	lr := newTestLittleRed(testLRName, "default")
	labels := selectorLabels(lr)

	if labels["app.kubernetes.io/name"] != "littlered" {
		t.Errorf("selectorLabels() missing app.kubernetes.io/name")
	}
	if labels["app.kubernetes.io/instance"] != testLRName {
		t.Errorf("selectorLabels() missing app.kubernetes.io/instance")
	}
	// Should not have other labels
	if len(labels) != 2 {
		t.Errorf("selectorLabels() has %d labels, want 2", len(labels))
	}
}

func TestRedisSelectorLabels(t *testing.T) {
	lr := newTestLittleRed(testLRName, "default")
	labels := redisSelectorLabels(lr)

	if labels["app.kubernetes.io/component"] != ComponentRedis {
		t.Errorf("redisSelectorLabels() missing component=redis")
	}
}

func TestSentinelSelectorLabels(t *testing.T) {
	lr := newTestLittleRed(testLRName, "default")
	labels := sentinelSelectorLabels(lr)

	if labels["app.kubernetes.io/component"] != ComponentSentinel {
		t.Errorf("sentinelSelectorLabels() missing component=sentinel")
	}
}

func TestMasterSelectorLabels(t *testing.T) {
	lr := newTestLittleRed(testLRName, "default")
	labels := masterSelectorLabels(lr)

	if labels[LabelRole] != RoleMaster {
		t.Errorf("masterSelectorLabels() missing role=master")
	}
	if labels["app.kubernetes.io/component"] != ComponentRedis {
		t.Errorf("masterSelectorLabels() missing component=redis")
	}
}

// ============================================================================
// ConfigMap Tests
// ============================================================================

func TestBuildConfigMap(t *testing.T) {
	lr := newTestLittleRed(testLRName, testNamespace)
	cm := buildConfigMap(lr)

	// Check metadata
	if cm.Name != "my-cache-config" {
		t.Errorf("ConfigMap name = %q, want %q", cm.Name, "my-cache-config")
	}
	if cm.Namespace != testNamespace {
		t.Errorf("ConfigMap namespace = %q, want %q", cm.Namespace, testNamespace)
	}

	// Check data has redis.conf
	if _, ok := cm.Data["redis.conf"]; !ok {
		t.Error("ConfigMap missing redis.conf key")
	}
}

func TestBuildRedisConfig(t *testing.T) {
	tests := []struct {
		name        string
		setupLR     func(*littleredv1alpha1.LittleRed)
		mustHave    []string
		mustNotHave []string
	}{
		{
			name:    "basic config",
			setupLR: func(lr *littleredv1alpha1.LittleRed) {},
			mustHave: []string{
				"bind 0.0.0.0",
				"port 6379",
				"save \"\"",
				"appendonly no",
				"maxmemory-policy noeviction",
			},
		},
		{
			name: "with TLS enabled",
			setupLR: func(lr *littleredv1alpha1.LittleRed) {
				lr.Spec.TLS.Enabled = true
				lr.Spec.TLS.ExistingSecret = "tls-secret"
			},
			mustHave: []string{
				"tls-port 6379",
				"port 0",
				"tls-cert-file /tls/tls.crt",
				"tls-key-file /tls/tls.key",
			},
		},
		{
			name: "with TLS client auth",
			setupLR: func(lr *littleredv1alpha1.LittleRed) {
				lr.Spec.TLS.Enabled = true
				lr.Spec.TLS.ExistingSecret = "tls-secret"
				lr.Spec.TLS.CACertSecret = "tls-secret" // CA is in the same secret → mounted at /tls
				lr.Spec.TLS.ClientAuth = true
			},
			mustHave: []string{
				"tls-ca-cert-file /tls/ca.crt",
				"tls-auth-clients yes",
			},
		},
		{
			name: "with raw config",
			setupLR: func(lr *littleredv1alpha1.LittleRed) {
				lr.Spec.Config.Raw = "custom-setting value"
			},
			mustHave: []string{
				"custom-setting value",
			},
		},
		{
			name: "with custom maxmemory policy",
			setupLR: func(lr *littleredv1alpha1.LittleRed) {
				lr.Spec.Config.MaxmemoryPolicy = "volatile-lru"
			},
			mustHave: []string{
				"maxmemory-policy volatile-lru",
			},
			mustNotHave: []string{
				"maxmemory-policy noeviction",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			lr := newTestLittleRed("test", "default")
			tt.setupLR(lr)
			config := buildRedisConfig(lr)

			for _, s := range tt.mustHave {
				if !strings.Contains(config, s) {
					t.Errorf("redis.conf missing %q\nGot:\n%s", s, config)
				}
			}
			for _, s := range tt.mustNotHave {
				if strings.Contains(config, s) {
					t.Errorf("redis.conf should not contain %q\nGot:\n%s", s, config)
				}
			}
		})
	}
}

// ============================================================================
// Config Hash Tests
// ============================================================================

func TestComputeConfigHash(t *testing.T) {
	// Same data should produce same hash
	data1 := map[string]string{"key1": "value1", "key2": "value2"}
	data2 := map[string]string{"key2": "value2", "key1": "value1"} // Different order
	hash1 := computeConfigHash(data1)
	hash2 := computeConfigHash(data2)

	if hash1 != hash2 {
		t.Errorf("Same data different order should produce same hash: %q != %q", hash1, hash2)
	}

	// Different data should produce different hash
	data3 := map[string]string{"key1": "value1", "key2": "different"}
	hash3 := computeConfigHash(data3)

	if hash1 == hash3 {
		t.Error("Different data should produce different hash")
	}

	// Hash should be 16 characters
	if len(hash1) != 16 {
		t.Errorf("Hash length = %d, want 16", len(hash1))
	}
}

func TestConfigHashChangesWithConfig(t *testing.T) {
	lr1 := newTestLittleRed("test", "default")
	lr2 := newTestLittleRed("test", "default")
	lr2.Spec.Config.MaxmemoryPolicy = "volatile-lru"

	config1 := buildRedisConfig(lr1)
	config2 := buildRedisConfig(lr2)

	hash1 := computeConfigHash(map[string]string{"redis.conf": config1})
	hash2 := computeConfigHash(map[string]string{"redis.conf": config2})

	if hash1 == hash2 {
		t.Error("Config change should produce different hash")
	}
}

// ============================================================================
// StatefulSet Tests
// ============================================================================

func TestBuildStatefulSet(t *testing.T) {
	lr := newTestLittleRed(testLRName, testNamespace)
	sts := buildStatefulSet(lr)

	// Check metadata
	if sts.Name != "my-cache-redis" {
		t.Errorf("StatefulSet name = %q, want %q", sts.Name, "my-cache-redis")
	}
	if sts.Namespace != testNamespace {
		t.Errorf("StatefulSet namespace = %q, want %q", sts.Namespace, testNamespace)
	}

	// Check replicas
	if *sts.Spec.Replicas != 1 {
		t.Errorf("StatefulSet replicas = %d, want 1", *sts.Spec.Replicas)
	}

	// Check serviceName
	if sts.Spec.ServiceName != testLRName {
		t.Errorf("StatefulSet serviceName = %q, want %q", sts.Spec.ServiceName, testLRName)
	}

	// Check containers (should have redis + exporter by default)
	containers := sts.Spec.Template.Spec.Containers
	if len(containers) != 2 {
		t.Errorf("StatefulSet has %d containers, want 2 (redis + exporter)", len(containers))
	}

	// Verify redis container exists
	var hasRedis bool
	for _, c := range containers {
		if c.Name == ComponentRedis {
			hasRedis = true
			// Check image
			if c.Image != "docker.io/valkey/valkey:8.0" {
				t.Errorf("Redis container image = %q, want default", c.Image)
			}
			// Check port
			if len(c.Ports) == 0 || c.Ports[0].ContainerPort != 6379 {
				t.Error("Redis container missing port 6379")
			}
			// Check security context
			if c.SecurityContext == nil {
				t.Error("Redis container missing security context")
			} else {
				if c.SecurityContext.AllowPrivilegeEscalation == nil || *c.SecurityContext.AllowPrivilegeEscalation {
					t.Error("Redis container should not allow privilege escalation")
				}
				if c.SecurityContext.ReadOnlyRootFilesystem == nil || !*c.SecurityContext.ReadOnlyRootFilesystem {
					t.Error("Redis container should have read-only root filesystem")
				}
			}
		}
	}
	if !hasRedis {
		t.Error("StatefulSet missing redis container")
	}

	// Check config hash annotation is present
	annotations := sts.Spec.Template.Annotations
	if annotations == nil {
		t.Fatal("StatefulSet pod template missing annotations")
	}
	if _, ok := annotations[AnnotationConfigHash]; !ok {
		t.Error("StatefulSet pod template missing config hash annotation")
	}
}

func TestBuildStatefulSetConfigHashChangesOnConfigChange(t *testing.T) {
	lr1 := newTestLittleRed(testLRName, testNamespace)
	lr2 := newTestLittleRed(testLRName, testNamespace)
	lr2.Spec.Config.MaxmemoryPolicy = "volatile-lru"

	sts1 := buildStatefulSet(lr1)
	sts2 := buildStatefulSet(lr2)

	hash1 := sts1.Spec.Template.Annotations[AnnotationConfigHash]
	hash2 := sts2.Spec.Template.Annotations[AnnotationConfigHash]

	if hash1 == hash2 {
		t.Error("Config hash should change when config changes")
	}
}

func TestBuildStatefulSetWithoutMetrics(t *testing.T) {
	lr := newTestLittleRed(testLRName, testNamespace)
	enabled := false
	lr.Spec.Metrics.Enabled = &enabled
	sts := buildStatefulSet(lr)

	// Should only have redis container
	containers := sts.Spec.Template.Spec.Containers
	if len(containers) != 1 {
		t.Errorf("StatefulSet has %d containers, want 1 (redis only)", len(containers))
	}
	if containers[0].Name != ComponentRedis {
		t.Errorf("Container name = %q, want redis", containers[0].Name)
	}
}

func TestBuildStatefulSetWithAuth(t *testing.T) {
	lr := newTestLittleRed(testLRName, testNamespace)
	lr.Spec.Auth.Enabled = true
	lr.Spec.Auth.ExistingSecret = "redis-password"
	sts := buildStatefulSet(lr)

	// Find redis container
	var redisContainer *corev1.Container
	for i := range sts.Spec.Template.Spec.Containers {
		if sts.Spec.Template.Spec.Containers[i].Name == ComponentRedis {
			redisContainer = &sts.Spec.Template.Spec.Containers[i]
			break
		}
	}

	if redisContainer == nil {
		t.Fatal("redis container not found")
	}

	// Check for REDIS_PASSWORD env var
	var hasPasswordEnv bool
	for _, env := range redisContainer.Env {
		if env.Name == "REDIS_PASSWORD" {
			hasPasswordEnv = true
			if env.ValueFrom == nil || env.ValueFrom.SecretKeyRef == nil {
				t.Error("REDIS_PASSWORD should reference a secret")
			} else if env.ValueFrom.SecretKeyRef.Name != "redis-password" {
				t.Errorf("REDIS_PASSWORD secret = %q, want %q",
					env.ValueFrom.SecretKeyRef.Name, "redis-password")
			}
		}
	}
	if !hasPasswordEnv {
		t.Error("redis container missing REDIS_PASSWORD env var")
	}
}

func TestBuildStatefulSetWithCustomResources(t *testing.T) {
	lr := newTestLittleRed(testLRName, testNamespace)
	lr.Spec.Resources = corev1.ResourceRequirements{
		Requests: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse("500m"),
			corev1.ResourceMemory: resource.MustParse("1Gi"),
		},
		Limits: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse("500m"),
			corev1.ResourceMemory: resource.MustParse("1Gi"),
		},
	}
	sts := buildStatefulSet(lr)

	// Find redis container
	var redisContainer *corev1.Container
	for i := range sts.Spec.Template.Spec.Containers {
		if sts.Spec.Template.Spec.Containers[i].Name == ComponentRedis {
			redisContainer = &sts.Spec.Template.Spec.Containers[i]
			break
		}
	}

	if redisContainer == nil {
		t.Fatal("redis container not found")
	}

	// Verify resources
	if redisContainer.Resources.Requests.Cpu().String() != "500m" {
		t.Errorf("CPU request = %s, want 500m", redisContainer.Resources.Requests.Cpu().String())
	}
	if redisContainer.Resources.Requests.Memory().String() != "1Gi" {
		t.Errorf("Memory request = %s, want 1Gi", redisContainer.Resources.Requests.Memory().String())
	}
}

// ============================================================================
// Service Tests
// ============================================================================

func TestBuildService(t *testing.T) {
	lr := newTestLittleRed(testLRName, testNamespace)
	svc := buildService(lr)

	// Check metadata
	if svc.Name != testLRName {
		t.Errorf("Service name = %q, want %q", svc.Name, testLRName)
	}
	if svc.Namespace != testNamespace {
		t.Errorf("Service namespace = %q, want %q", svc.Namespace, testNamespace)
	}

	// Check type
	if svc.Spec.Type != corev1.ServiceTypeClusterIP {
		t.Errorf("Service type = %q, want ClusterIP", svc.Spec.Type)
	}

	// Check ports (should have redis + metrics by default)
	if len(svc.Spec.Ports) != 2 {
		t.Errorf("Service has %d ports, want 2", len(svc.Spec.Ports))
	}

	var hasRedisPort, hasMetricsPort bool
	for _, p := range svc.Spec.Ports {
		if p.Name == ComponentRedis && p.Port == 6379 {
			hasRedisPort = true
		}
		if p.Name == portNameMetrics && p.Port == 9121 {
			hasMetricsPort = true
		}
	}
	if !hasRedisPort {
		t.Error("Service missing redis port 6379")
	}
	if !hasMetricsPort {
		t.Error("Service missing metrics port 9121")
	}
}

func TestBuildServiceWithoutMetrics(t *testing.T) {
	lr := newTestLittleRed(testLRName, testNamespace)
	enabled := false
	lr.Spec.Metrics.Enabled = &enabled
	svc := buildService(lr)

	// Should only have redis port
	if len(svc.Spec.Ports) != 1 {
		t.Errorf("Service has %d ports, want 1", len(svc.Spec.Ports))
	}
	if svc.Spec.Ports[0].Name != ComponentRedis {
		t.Errorf("Port name = %q, want redis", svc.Spec.Ports[0].Name)
	}
}

func TestBuildServiceWithCustomLabels(t *testing.T) {
	lr := newTestLittleRed(testLRName, testNamespace)
	lr.Spec.Service.Labels = map[string]string{
		"custom-label": "custom-value",
	}
	svc := buildService(lr)

	if svc.Labels["custom-label"] != "custom-value" {
		t.Error("Service missing custom label")
	}
}

// ============================================================================
// Volumes Tests
// ============================================================================

func TestBuildVolumes(t *testing.T) {
	lr := newTestLittleRed(testLRName, testNamespace)
	volumes := buildVolumes(lr)

	// Should have config and data volumes
	if len(volumes) != 2 {
		t.Errorf("got %d volumes, want 2", len(volumes))
	}

	var hasConfig, hasData bool
	for _, v := range volumes {
		if v.Name == "config" {
			hasConfig = true
			if v.ConfigMap == nil {
				t.Error("config volume should be a ConfigMap")
			}
		}
		if v.Name == "data" {
			hasData = true
			if v.EmptyDir == nil {
				t.Error("data volume should be EmptyDir")
			}
		}
	}
	if !hasConfig {
		t.Error("missing config volume")
	}
	if !hasData {
		t.Error("missing data volume")
	}
}

func TestBuildVolumesWithTLS(t *testing.T) {
	lr := newTestLittleRed(testLRName, testNamespace)
	lr.Spec.TLS.Enabled = true
	lr.Spec.TLS.ExistingSecret = "tls-secret"
	volumes := buildVolumes(lr)

	// Should have config, data, and tls volumes
	if len(volumes) != 3 {
		t.Errorf("got %d volumes, want 3", len(volumes))
	}

	var hasTLS bool
	for _, v := range volumes {
		if v.Name == "tls" {
			hasTLS = true
			if v.Secret == nil || v.Secret.SecretName != "tls-secret" {
				t.Error("tls volume should reference tls-secret")
			}
		}
	}
	if !hasTLS {
		t.Error("missing tls volume")
	}
}

// ============================================================================
// Probe Tests
// ============================================================================

func TestBuildLivenessProbe(t *testing.T) {
	lr := newTestLittleRed(testLRName, testNamespace)
	probe := buildLivenessProbe(lr)

	if probe.Exec == nil {
		t.Fatal("liveness probe should use exec")
	}

	cmd := strings.Join(probe.Exec.Command, " ")
	if !strings.Contains(cmd, "redis-cli") || !strings.Contains(cmd, "ping") {
		t.Errorf("liveness probe command = %q, should contain redis-cli ping", cmd)
	}

	if probe.InitialDelaySeconds != 5 {
		t.Errorf("InitialDelaySeconds = %d, want 5", probe.InitialDelaySeconds)
	}
}

func TestBuildLivenessProbeWithAuth(t *testing.T) {
	lr := newTestLittleRed(testLRName, testNamespace)
	lr.Spec.Auth.Enabled = true
	probe := buildLivenessProbe(lr)

	cmd := strings.Join(probe.Exec.Command, " ")
	if !strings.Contains(cmd, "-a") || !strings.Contains(cmd, "$(REDIS_PASSWORD)") {
		t.Errorf("liveness probe should include auth flag, got: %s", cmd)
	}
}

func TestBuildLivenessProbeWithTLS(t *testing.T) {
	lr := newTestLittleRed(testLRName, testNamespace)
	lr.Spec.TLS.Enabled = true
	probe := buildLivenessProbe(lr)

	cmd := strings.Join(probe.Exec.Command, " ")
	if !strings.Contains(cmd, "--tls") {
		t.Errorf("liveness probe should include --tls flag, got: %s", cmd)
	}
}

// ============================================================================
// Sentinel Mode Tests
// ============================================================================

func TestBuildSentinelLivenessProbe(t *testing.T) {
	lr := newTestLittleRed(testLRName, testNamespace)
	lr.Spec.Sentinel = &littleredv1alpha1.SentinelSpec{
		Quorum:                2,
		DownAfterMilliseconds: 5000,
		FailoverTimeout:       10000,
	}
	probe := buildSentinelLivenessProbe(lr)

	if probe.Exec == nil {
		t.Fatal("sentinel liveness probe should use exec")
	}

	cmd := strings.Join(probe.Exec.Command, " ")
	if !strings.Contains(cmd, "info replication") {
		t.Errorf("probe command should query 'info replication', got: %s", cmd)
	}
	if !strings.Contains(cmd, "role:master") {
		t.Errorf("probe command should check role:master, got: %s", cmd)
	}
	if !strings.Contains(cmd, "master_link_status:up") {
		t.Errorf("probe command should check master_link_status:up, got: %s", cmd)
	}
	if !strings.Contains(cmd, "master_host") {
		t.Errorf("probe command should extract master_host for reachability check, got: %s", cmd)
	}
	if !strings.Contains(cmd, "bootstrap-in-progress") {
		t.Errorf("probe command should skip check while bootstrap is in progress, got: %s", cmd)
	}

	// downAfterMs=5000 + failoverTimeout=10000 + buffer=15000 = 30000ms
	// ceil(30000 / 10000ms-period) = 3 → hits the minimum
	if probe.FailureThreshold != 3 {
		t.Errorf("FailureThreshold = %d, want 3 for downAfter=5000 failoverTimeout=10000", probe.FailureThreshold)
	}
	if probe.InitialDelaySeconds != 15 {
		t.Errorf("InitialDelaySeconds = %d, want 15", probe.InitialDelaySeconds)
	}
}

func TestBuildSentinelLivenessProbeDefaultTimings(t *testing.T) {
	// When Sentinel spec is nil, probe uses hardcoded defaults (30s + 180s + 15s buffer).
	lr := newTestLittleRed(testLRName, testNamespace)
	probe := buildSentinelLivenessProbe(lr)

	// ceil((30000 + 180000 + 15000) / 10000) = ceil(22.5) = 23
	if probe.FailureThreshold != 23 {
		t.Errorf("FailureThreshold = %d, want 23 for default sentinel timings", probe.FailureThreshold)
	}
}

func TestBuildSentinelLivenessProbeWithTLS(t *testing.T) {
	lr := newTestLittleRed(testLRName, testNamespace)
	lr.Spec.TLS.Enabled = true
	probe := buildSentinelLivenessProbe(lr)

	cmd := strings.Join(probe.Exec.Command, " ")
	if !strings.Contains(cmd, "--tls") {
		t.Errorf("sentinel liveness probe should include --tls flag, got: %s", cmd)
	}
}

func TestBuildSentinelReadinessProbe(t *testing.T) {
	lr := newTestLittleRed(testLRName, testNamespace)
	probe := buildSentinelReadinessProbe(lr)

	if probe.Exec == nil {
		t.Fatal("sentinel readiness probe should use exec")
	}

	cmd := strings.Join(probe.Exec.Command, " ")
	if !strings.Contains(cmd, "info replication") {
		t.Errorf("probe command should query 'info replication', got: %s", cmd)
	}
	if !strings.Contains(cmd, "role:master") {
		t.Errorf("probe command should check role:master, got: %s", cmd)
	}
	if !strings.Contains(cmd, "master_link_status:up") {
		t.Errorf("probe command should check master_link_status:up, got: %s", cmd)
	}
	// Readiness probe exits 1 during bootstrap (opposite of liveness probe)
	if !strings.Contains(cmd, "exit 1") {
		t.Errorf("readiness probe should fail (exit 1) while bootstrap is in progress, got: %s", cmd)
	}
	// Readiness probe should NOT perform master-reachability check — its only job is
	// to remove traffic from a zombie replica quickly, not to keep the pod alive.
	if strings.Contains(cmd, "redis-cli -h \"$master_host\"") {
		t.Errorf("readiness probe should not check master reachability (that is the liveness probe's job), got: %s", cmd)
	}

	if probe.InitialDelaySeconds != 5 {
		t.Errorf("InitialDelaySeconds = %d, want 5", probe.InitialDelaySeconds)
	}
}

func TestBuildSentinelReadinessProbeWithTLS(t *testing.T) {
	lr := newTestLittleRed(testLRName, testNamespace)
	lr.Spec.TLS.Enabled = true
	probe := buildSentinelReadinessProbe(lr)

	cmd := strings.Join(probe.Exec.Command, " ")
	if !strings.Contains(cmd, "--tls") {
		t.Errorf("sentinel readiness probe should include --tls flag, got: %s", cmd)
	}
}

func TestBuildSentinelConfig(t *testing.T) {
	// The static sentinel.conf is intentionally minimal: sentinels start with no master
	// configured. The operator issues SENTINEL MONITOR at runtime (bootstrapSentinel /
	// Rule 0), so timing parameters (quorum, downAfterMs, failoverTimeout) are not baked
	// into the config file. IP-only mode (ADR-001) means resolve/announce-hostnames are
	// both set to "no".
	lr := newTestLittleRed(testLRName, testNamespace)
	lr.Spec.Mode = ModeSentinel
	lr.Spec.Sentinel = &littleredv1alpha1.SentinelSpec{
		Quorum:                2,
		DownAfterMilliseconds: 5000,
		FailoverTimeout:       60000,
	}
	config := buildSentinelConfig(lr)

	mustHave := []string{
		"port 26379",
		"sentinel resolve-hostnames no",
		"sentinel announce-hostnames no",
	}
	for _, s := range mustHave {
		if !strings.Contains(config, s) {
			t.Errorf("sentinel.conf missing %q\nGot:\n%s", s, config)
		}
	}

	// Static config must NOT contain a monitor stanza — that is issued at runtime.
	mustNotHave := []string{
		"sentinel monitor mymaster",
		"sentinel down-after-milliseconds",
		"sentinel failover-timeout",
		"resolve-hostnames yes",
		"announce-hostnames yes",
	}
	for _, s := range mustNotHave {
		if strings.Contains(config, s) {
			t.Errorf("sentinel.conf should not contain %q (must be configured at runtime)\nGot:\n%s", s, config)
		}
	}
}

func TestBuildSentinelConfigMap(t *testing.T) {
	lr := newTestLittleRed(testLRName, testNamespace)
	lr.Spec.Mode = ModeSentinel
	cm := buildSentinelConfigMap(lr)

	if cm.Name != "my-cache-sentinel-config" {
		t.Errorf("ConfigMap name = %q, want %q", cm.Name, "my-cache-sentinel-config")
	}

	if _, ok := cm.Data["sentinel.conf"]; !ok {
		t.Error("ConfigMap missing sentinel.conf key")
	}

	if cm.Labels["app.kubernetes.io/component"] != ComponentSentinel {
		t.Error("ConfigMap missing component=sentinel label")
	}
}

func TestBuildRedisStatefulSetSentinel(t *testing.T) {
	lr := newTestLittleRed(testLRName, testNamespace)
	lr.Spec.Mode = ModeSentinel
	sts := buildRedisStatefulSetSentinel(lr)

	// Check replicas (should be 3 for sentinel mode)
	if *sts.Spec.Replicas != 3 {
		t.Errorf("StatefulSet replicas = %d, want 3", *sts.Spec.Replicas)
	}

	// Check serviceName (should be headless replicas service)
	if sts.Spec.ServiceName != "my-cache-replicas" {
		t.Errorf("StatefulSet serviceName = %q, want %q", sts.Spec.ServiceName, "my-cache-replicas")
	}

	// Check PodManagementPolicy
	if sts.Spec.PodManagementPolicy != "Parallel" {
		t.Errorf("PodManagementPolicy = %q, want Parallel", sts.Spec.PodManagementPolicy)
	}

	// Verify selector uses redisSelectorLabels
	if sts.Spec.Selector.MatchLabels["app.kubernetes.io/component"] != ComponentRedis {
		t.Error("StatefulSet selector should have component=redis")
	}

	// Check config hash annotation is present
	annotations := sts.Spec.Template.Annotations
	if annotations == nil {
		t.Fatal("StatefulSet pod template missing annotations")
	}
	if _, ok := annotations[AnnotationConfigHash]; !ok {
		t.Error("StatefulSet pod template missing config hash annotation")
	}
}

func TestBuildSentinelStatefulSet(t *testing.T) {
	lr := newTestLittleRed(testLRName, testNamespace)
	lr.Spec.Mode = ModeSentinel
	sts := buildSentinelStatefulSet(lr)

	// Check name
	if sts.Name != "my-cache-sentinel" {
		t.Errorf("StatefulSet name = %q, want %q", sts.Name, "my-cache-sentinel")
	}

	// Check replicas
	if *sts.Spec.Replicas != 3 {
		t.Errorf("StatefulSet replicas = %d, want 3", *sts.Spec.Replicas)
	}

	// Check serviceName
	if sts.Spec.ServiceName != "my-cache-sentinel" {
		t.Errorf("StatefulSet serviceName = %q, want %q", sts.Spec.ServiceName, "my-cache-sentinel")
	}

	// Check container
	containers := sts.Spec.Template.Spec.Containers
	if len(containers) != 1 {
		t.Errorf("StatefulSet has %d containers, want 1", len(containers))
	}
	if containers[0].Name != ComponentSentinel {
		t.Errorf("Container name = %q, want sentinel", containers[0].Name)
	}

	// Check sentinel port
	var hasSentinelPort bool
	for _, p := range containers[0].Ports {
		if p.Name == ComponentSentinel && p.ContainerPort == 26379 {
			hasSentinelPort = true
		}
	}
	if !hasSentinelPort {
		t.Error("Sentinel container missing port 26379")
	}

	// Check config hash annotation is present
	annotations := sts.Spec.Template.Annotations
	if annotations == nil {
		t.Fatal("StatefulSet pod template missing annotations")
	}
	if _, ok := annotations[AnnotationConfigHash]; !ok {
		t.Error("StatefulSet pod template missing config hash annotation")
	}
}

func TestBuildMasterService(t *testing.T) {
	lr := newTestLittleRed(testLRName, testNamespace)
	lr.Spec.Mode = ModeSentinel
	svc := buildMasterService(lr)

	// Check name (same as standalone)
	if svc.Name != testLRName {
		t.Errorf("Service name = %q, want %q", svc.Name, testLRName)
	}

	// Check selector includes role=master
	if svc.Spec.Selector[LabelRole] != RoleMaster {
		t.Error("Master service selector should have role=master")
	}
}

func TestBuildReplicasHeadlessService(t *testing.T) {
	lr := newTestLittleRed(testLRName, testNamespace)
	lr.Spec.Mode = ModeSentinel
	svc := buildReplicasHeadlessService(lr)

	// Check name
	if svc.Name != "my-cache-replicas" {
		t.Errorf("Service name = %q, want %q", svc.Name, "my-cache-replicas")
	}

	// Check ClusterIP is None (headless)
	if svc.Spec.ClusterIP != "None" {
		t.Errorf("Service ClusterIP = %q, want None", svc.Spec.ClusterIP)
	}

	// Check publishNotReadyAddresses
	if !svc.Spec.PublishNotReadyAddresses {
		t.Error("Headless service should publishNotReadyAddresses")
	}
}

func TestBuildSentinelHeadlessService(t *testing.T) {
	lr := newTestLittleRed(testLRName, testNamespace)
	lr.Spec.Mode = ModeSentinel
	svc := buildSentinelHeadlessService(lr)

	// Check name
	if svc.Name != "my-cache-sentinel" {
		t.Errorf("Service name = %q, want %q", svc.Name, "my-cache-sentinel")
	}

	// Check ClusterIP is None (headless)
	if svc.Spec.ClusterIP != "None" {
		t.Errorf("Service ClusterIP = %q, want None", svc.Spec.ClusterIP)
	}

	// Check port
	if len(svc.Spec.Ports) != 1 || svc.Spec.Ports[0].Port != 26379 {
		t.Error("Sentinel service should have port 26379")
	}
}

// ============================================================================
// ServiceMonitor Tests
// ============================================================================

func TestBuildServiceMonitor(t *testing.T) {
	lr := newTestLittleRed(testLRName, testNamespace)
	lr.Spec.Metrics.ServiceMonitor.Enabled = true
	sm := buildServiceMonitor(lr)

	// Check name
	if sm.Name != testLRName {
		t.Errorf("ServiceMonitor name = %q, want %q", sm.Name, testLRName)
	}

	// Check namespace (defaults to LittleRed namespace)
	if sm.Namespace != testNamespace {
		t.Errorf("ServiceMonitor namespace = %q, want %q", sm.Namespace, testNamespace)
	}

	// Check endpoints
	if len(sm.Spec.Endpoints) != 1 {
		t.Errorf("ServiceMonitor has %d endpoints, want 1", len(sm.Spec.Endpoints))
	}
	if sm.Spec.Endpoints[0].Port != portNameMetrics {
		t.Errorf("ServiceMonitor endpoint port = %q, want %q", sm.Spec.Endpoints[0].Port, portNameMetrics)
	}
}

func TestBuildServiceMonitorWithCustomNamespace(t *testing.T) {
	lr := newTestLittleRed(testLRName, testNamespace)
	lr.Spec.Metrics.ServiceMonitor.Enabled = true
	lr.Spec.Metrics.ServiceMonitor.Namespace = "monitoring"
	sm := buildServiceMonitor(lr)

	if sm.Namespace != "monitoring" {
		t.Errorf("ServiceMonitor namespace = %q, want monitoring", sm.Namespace)
	}
}

func TestBuildServiceMonitorWithCustomLabels(t *testing.T) {
	lr := newTestLittleRed(testLRName, testNamespace)
	lr.Spec.Metrics.ServiceMonitor.Enabled = true
	lr.Spec.Metrics.ServiceMonitor.Labels = map[string]string{
		"release": "prometheus",
	}
	sm := buildServiceMonitor(lr)

	if sm.Labels["release"] != "prometheus" {
		t.Error("ServiceMonitor missing custom label 'release'")
	}
}

// ============================================================================
// Exporter Container Tests
// ============================================================================

func TestBuildExporterContainer(t *testing.T) {
	lr := newTestLittleRed(testLRName, testNamespace)
	container := buildExporterContainer(lr)

	// Check name
	if container.Name != "exporter" {
		t.Errorf("Container name = %q, want exporter", container.Name)
	}

	// Check image
	expectedImage := "docker.io/oliver006/redis_exporter:v1.66.0"
	if container.Image != expectedImage {
		t.Errorf("Container image = %q, want %q", container.Image, expectedImage)
	}

	// Check REDIS_ADDR env var
	var hasRedisAddr bool
	for _, env := range container.Env {
		if env.Name == "REDIS_ADDR" {
			hasRedisAddr = true
			if env.Value != "redis://localhost:6379" {
				t.Errorf("REDIS_ADDR = %q, want redis://localhost:6379", env.Value)
			}
		}
	}
	if !hasRedisAddr {
		t.Error("Exporter container missing REDIS_ADDR env var")
	}

	// Check metrics port
	var hasMetricsPort bool
	for _, p := range container.Ports {
		if p.Name == portNameMetrics && p.ContainerPort == 9121 {
			hasMetricsPort = true
		}
	}
	if !hasMetricsPort {
		t.Error("Exporter container missing metrics port 9121")
	}
}

func TestBuildExporterContainerWithTLS(t *testing.T) {
	lr := newTestLittleRed(testLRName, testNamespace)
	lr.Spec.TLS.Enabled = true
	container := buildExporterContainer(lr)

	// Check REDIS_ADDR uses rediss://
	var hasRedisAddr bool
	for _, env := range container.Env {
		if env.Name == "REDIS_ADDR" {
			hasRedisAddr = true
			if !strings.HasPrefix(env.Value, "rediss://") {
				t.Errorf("REDIS_ADDR = %q, should use rediss:// for TLS", env.Value)
			}
		}
	}
	if !hasRedisAddr {
		t.Error("Exporter container missing REDIS_ADDR env var")
	}
}
