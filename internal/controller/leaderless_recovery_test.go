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

package controller

import (
	"testing"

	littleredv1alpha1 "github.com/littlered-operator/littlered-operator/api/v1alpha1"
)

func TestPickBootstrapMasterIP(t *testing.T) {
	lr := &littleredv1alpha1.LittleRed{}
	lr.Name = "store"
	r := &LittleRedReconciler{}

	tests := []struct {
		name     string
		redisMap map[string]string
		want     string
	}{
		{
			name:     "prefers redis-0",
			redisMap: map[string]string{"10.0.0.2": "store-redis-1", "10.0.0.1": "store-redis-0", "10.0.0.3": "store-redis-2"},
			want:     "10.0.0.1",
		},
		{
			name:     "redis-0 absent falls back to lowest-ordinal name",
			redisMap: map[string]string{"10.0.0.3": "store-redis-2", "10.0.0.2": "store-redis-1"},
			want:     "10.0.0.2",
		},
		{
			name:     "no pods yields empty",
			redisMap: map[string]string{},
			want:     "",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := r.pickBootstrapMasterIP(lr, tt.redisMap); got != tt.want {
				t.Errorf("pickBootstrapMasterIP() = %q, want %q", got, tt.want)
			}
		})
	}
}
