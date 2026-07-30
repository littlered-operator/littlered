/*
Copyright 2026 The littlered Authors.

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

package v1alpha1

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
)

func TestPlacementSetDefaults(t *testing.T) {
	// nil placement stays nil (never auto-created).
	lr := &LittleRed{Spec: LittleRedSpec{Mode: "cluster"}}
	lr.SetDefaults()
	if lr.Spec.Placement != nil {
		t.Error("nil placement should stay nil after SetDefaults")
	}

	// shardAntiAffinity with empty fields → documented defaults filled.
	lr = &LittleRed{Spec: LittleRedSpec{Mode: "cluster",
		Placement: &PlacementSpec{ShardAntiAffinity: &ShardAntiAffinitySpec{}}}}
	lr.SetDefaults()
	saa := lr.Spec.Placement.ShardAntiAffinity
	if saa.TopologyKey != DefaultShardTopologyKey {
		t.Errorf("TopologyKey = %q, want %q", saa.TopologyKey, DefaultShardTopologyKey)
	}
	if saa.WhenUnsatisfiable != corev1.ScheduleAnyway {
		t.Errorf("WhenUnsatisfiable = %q, want ScheduleAnyway (soft default)", saa.WhenUnsatisfiable)
	}

	// Explicit values are preserved.
	lr = &LittleRed{Spec: LittleRedSpec{Mode: "cluster",
		Placement: &PlacementSpec{ShardAntiAffinity: &ShardAntiAffinitySpec{
			TopologyKey: "topology.kubernetes.io/zone", WhenUnsatisfiable: corev1.DoNotSchedule}}}}
	lr.SetDefaults()
	saa = lr.Spec.Placement.ShardAntiAffinity
	if saa.TopologyKey != "topology.kubernetes.io/zone" || saa.WhenUnsatisfiable != corev1.DoNotSchedule {
		t.Errorf("explicit placement values must be preserved, got %+v", saa)
	}
}

const testRegistryGCR = "gcr.io"

func TestImageSpec_FullImage(t *testing.T) {
	tests := []struct {
		name     string
		spec     ImageSpec
		expected string
	}{
		{
			name:     "all defaults",
			spec:     ImageSpec{},
			expected: "docker.io/library/redis:8.4.2",
		},
		{
			name: "custom registry",
			spec: ImageSpec{
				Registry: testRegistryGCR,
			},
			expected: "gcr.io/library/redis:8.4.2",
		},
		{
			name: "custom path",
			spec: ImageSpec{
				Path: "myorg/custom-image",
			},
			expected: "docker.io/myorg/custom-image:8.4.2",
		},
		{
			name: "custom tag",
			spec: ImageSpec{
				Tag: "7.2",
			},
			expected: "docker.io/library/redis:7.2",
		},
		{
			name: "all custom",
			spec: ImageSpec{
				Registry: "my-registry.io",
				Path:     "my-org/my-redis",
				Tag:      "latest",
			},
			expected: "my-registry.io/my-org/my-redis:latest",
		},
		{
			name: "private registry with port",
			spec: ImageSpec{
				Registry: "registry.example.com:5000",
				Path:     "redis",
				Tag:      "7.0",
			},
			expected: "registry.example.com:5000/redis:7.0",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := tt.spec.FullImage()
			if result != tt.expected {
				t.Errorf("FullImage() = %q, want %q", result, tt.expected)
			}
		})
	}
}

func TestExporterSpec_FullImage(t *testing.T) {
	tests := []struct {
		name         string
		spec         ExporterSpec
		mainRegistry string
		expected     string
	}{
		{
			name:         "all defaults with empty main registry",
			spec:         ExporterSpec{},
			mainRegistry: "",
			expected:     DefaultRegistry + "/" + DefaultExporterPath + ":" + DefaultExporterTag,
		},
		{
			name:         "inherit main registry",
			spec:         ExporterSpec{},
			mainRegistry: testRegistryGCR,
			expected:     testRegistryGCR + "/" + DefaultExporterPath + ":" + DefaultExporterTag,
		},
		{
			name: "override main registry",
			spec: ExporterSpec{
				Registry: "quay.io",
			},
			mainRegistry: testRegistryGCR,
			expected:     "quay.io/" + DefaultExporterPath + ":" + DefaultExporterTag,
		},
		{
			name: "custom path and tag",
			spec: ExporterSpec{
				Path: "bitnami/redis-exporter",
				Tag:  "1.50.0",
			},
			mainRegistry: DefaultRegistry,
			expected:     "docker.io/bitnami/redis-exporter:1.50.0",
		},
		{
			name: "all custom",
			spec: ExporterSpec{
				Registry: "my-registry.io",
				Path:     "monitoring/redis-exporter",
				Tag:      "custom",
			},
			mainRegistry: "ignored.io",
			expected:     "my-registry.io/monitoring/redis-exporter:custom",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := tt.spec.FullImage(tt.mainRegistry)
			if result != tt.expected {
				t.Errorf("FullImage(%q) = %q, want %q", tt.mainRegistry, result, tt.expected)
			}
		})
	}
}

func TestMetricsSpec_IsEnabled(t *testing.T) {
	trueVal := true
	falseVal := false

	tests := []struct {
		name     string
		spec     MetricsSpec
		expected bool
	}{
		{
			name:     "nil enabled defaults to true",
			spec:     MetricsSpec{Enabled: nil},
			expected: true,
		},
		{
			name:     "explicitly enabled",
			spec:     MetricsSpec{Enabled: &trueVal},
			expected: true,
		},
		{
			name:     "explicitly disabled",
			spec:     MetricsSpec{Enabled: &falseVal},
			expected: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := tt.spec.IsEnabled()
			if result != tt.expected {
				t.Errorf("IsEnabled() = %v, want %v", result, tt.expected)
			}
		})
	}
}
