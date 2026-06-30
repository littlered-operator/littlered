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
	"os"
	"strings"
	"testing"
)

func TestParseExporterImage(t *testing.T) {
	const (
		wantPath = "oliver006/redis_exporter"
		wantTag  = "v1.85.0"
	)
	tests := []struct {
		name       string
		dockerfile string
		wantPath   string
		wantTag    string
	}{
		{
			name:       "registry, path and tag",
			dockerfile: "FROM docker.io/oliver006/redis_exporter:v1.85.0\n",
			wantPath:   wantPath,
			wantTag:    wantTag,
		},
		{
			name:       "no registry host",
			dockerfile: "FROM oliver006/redis_exporter:v1.85.0\n",
			wantPath:   wantPath,
			wantTag:    wantTag,
		},
		{
			name:       "registry with port",
			dockerfile: "FROM localhost:5000/oliver006/redis_exporter:v1.85.0\n",
			wantPath:   wantPath,
			wantTag:    wantTag,
		},
		{
			name:       "comments and blank lines before FROM",
			dockerfile: "# a comment\n\nFROM docker.io/oliver006/redis_exporter:v1.85.0\n",
			wantPath:   wantPath,
			wantTag:    wantTag,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path, tag := parseExporterImage(tt.dockerfile)
			if path != tt.wantPath || tag != tt.wantTag {
				t.Errorf("parseExporterImage() = (%q, %q), want (%q, %q)", path, tag, tt.wantPath, tt.wantTag)
			}
		})
	}
}

// TestExporterDefaultsMatchDockerfile ensures the embedded source of truth, the
// kubebuilder default literal, and the generated CRD stay in lockstep. A
// Dependabot bump to redis-exporter.Dockerfile that hasn't been mirrored into
// the kubebuilder marker + `make manifests` will fail here.
func TestExporterDefaultsMatchDockerfile(t *testing.T) {
	const crdPath = "../../config/crd/bases/redis.chuck-chuck-chuck.net_littlereds.yaml"
	crd, err := os.ReadFile(crdPath)
	if err != nil {
		t.Fatalf("reading generated CRD: %v", err)
	}
	crdStr := string(crd)

	if want := "default: " + DefaultExporterTag + "\n"; !strings.Contains(crdStr, want) {
		t.Errorf("CRD %s missing exporter tag default %q; run `make manifests` and update the kubebuilder marker on ExporterSpec.Tag", crdPath, DefaultExporterTag)
	}
	if want := "default: " + DefaultExporterPath + "\n"; !strings.Contains(crdStr, want) {
		t.Errorf("CRD %s missing exporter path default %q; run `make manifests`", crdPath, DefaultExporterPath)
	}
}
