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
	"slices"
	"testing"
	"time"
)

func TestPlanClusterWipeRecovery(t *testing.T) {
	cooldown := 90 * time.Second
	now := time.Unix(1_000_000, 0)
	within := now.Add(-30 * time.Second)   // marker 30s ago: still in cooldown
	elapsed := now.Add(-120 * time.Second) // marker 120s ago: cooldown passed

	ready := clusterPodHealth{Name: "c-shard-0-0", RedisReady: true, Restarted: false}
	readyButRestarted := clusterPodHealth{Name: "c-shard-0-1", RedisReady: true, Restarted: true}
	stuck0 := clusterPodHealth{Name: "c-shard-0-0", RedisReady: false, Restarted: true}
	stuck1 := clusterPodHealth{Name: "c-shard-1-0", RedisReady: false, Restarted: true}
	notReadyNoRestart := clusterPodHealth{Name: "c-shard-2-0", RedisReady: false, Restarted: false}
	oom := clusterPodHealth{Name: "c-shard-2-1", RedisReady: false, Restarted: true, OOMKilled: true}

	tests := []struct {
		name        string
		pods        []clusterPodHealth
		since       *time.Time
		wantAction  clusterWipeAction
		wantRecycle []string
	}{
		{
			name:       "no pods -> clear",
			pods:       nil,
			since:      nil,
			wantAction: wipeClearMarker,
		},
		{
			name:       "all healthy -> clear",
			pods:       []clusterPodHealth{ready, ready},
			since:      &elapsed,
			wantAction: wipeClearMarker,
		},
		{
			name:       "ready-but-restarted is not recyclable (redis up holds data) -> clear",
			pods:       []clusterPodHealth{readyButRestarted},
			since:      nil,
			wantAction: wipeClearMarker,
		},
		{
			name:       "not-ready but never restarted (still booting) -> clear",
			pods:       []clusterPodHealth{notReadyNoRestart},
			since:      nil,
			wantAction: wipeClearMarker,
		},
		{
			name:       "OOMKilled not recyclable -> clear",
			pods:       []clusterPodHealth{oom},
			since:      nil,
			wantAction: wipeClearMarker,
		},
		{
			name:       "signature first seen -> start cooldown",
			pods:       []clusterPodHealth{stuck0, stuck1},
			since:      nil,
			wantAction: wipeStartCooldown,
		},
		{
			name:       "signature within cooldown -> wait",
			pods:       []clusterPodHealth{stuck0, stuck1},
			since:      &within,
			wantAction: wipeWait,
		},
		{
			name:        "signature past cooldown -> recycle the stuck pods only",
			pods:        []clusterPodHealth{stuck0, stuck1, ready, oom},
			since:       &elapsed,
			wantAction:  wipeRecycle,
			wantRecycle: []string{"c-shard-0-0", "c-shard-1-0"},
		},
		{
			name:        "total wipe past cooldown -> recycle every stuck pod",
			pods:        []clusterPodHealth{stuck0, stuck1},
			since:       &elapsed,
			wantAction:  wipeRecycle,
			wantRecycle: []string{"c-shard-0-0", "c-shard-1-0"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := planClusterWipeRecovery(tc.pods, tc.since, now, cooldown)
			if got.action != tc.wantAction {
				t.Fatalf("action = %v, want %v", got.action, tc.wantAction)
			}
			if tc.wantAction == wipeRecycle {
				slices.Sort(got.recycle)
				want := slices.Clone(tc.wantRecycle)
				slices.Sort(want)
				if !slices.Equal(got.recycle, want) {
					t.Fatalf("recycle = %v, want %v", got.recycle, want)
				}
			}
		})
	}
}
