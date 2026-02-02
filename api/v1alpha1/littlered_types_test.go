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

package v1alpha1

import (
	"testing"
)

func TestImageSpec_FullImage(t *testing.T) {
	tests := []struct {
		name     string
		spec     ImageSpec
		expected string
	}{
		{
			name:     "all defaults",
			spec:     ImageSpec{},
			expected: "docker.io/valkey/valkey:8.0",
		},
		{
			name: "custom registry",
			spec: ImageSpec{
				Registry: "gcr.io",
			},
			expected: "gcr.io/valkey/valkey:8.0",
		},
		{
			name: "custom path",
			spec: ImageSpec{
				Path: "redis/redis",
			},
			expected: "docker.io/redis/redis:8.0",
		},
		{
			name: "custom tag",
			spec: ImageSpec{
				Tag: "7.2",
			},
			expected: "docker.io/valkey/valkey:7.2",
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
			expected:     "docker.io/oliver006/redis_exporter:v1.66.0",
		},
		{
			name:         "inherit main registry",
			spec:         ExporterSpec{},
			mainRegistry: "gcr.io",
			expected:     "gcr.io/oliver006/redis_exporter:v1.66.0",
		},
		{
			name: "override main registry",
			spec: ExporterSpec{
				Registry: "quay.io",
			},
			mainRegistry: "gcr.io",
			expected:     "quay.io/oliver006/redis_exporter:v1.66.0",
		},
		{
			name: "custom path and tag",
			spec: ExporterSpec{
				Path: "bitnami/redis-exporter",
				Tag:  "1.50.0",
			},
			mainRegistry: "docker.io",
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
