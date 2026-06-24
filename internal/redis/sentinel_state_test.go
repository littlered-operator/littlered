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

package redis

import "testing"

const (
	masterIP   = "10.0.0.1"
	replicaIP  = "10.0.0.2"
	ghostRepIP = "10.0.0.99" // an IP with no living pod
	flagSlave  = "slave"
)

// healthySentinelState builds a steady-state sentinel view: a living master, one
// healthy known replica, plus a leftover ghost replica entry (s_down, no backing
// pod) that Rule D would legitimately want to prune via SENTINEL RESET.
func healthySentinelState() *SentinelClusterState {
	s := NewSentinelClusterState()
	s.ValidIPs[masterIP] = true
	s.ValidIPs[replicaIP] = true
	// ghostRepIP intentionally absent from ValidIPs → IsGhost(ghostRepIP) == true
	s.RealMasterIP = masterIP
	s.RedisNodes[masterIP] = &RedisNodeState{IP: masterIP, Role: roleMaster, Reachable: true}
	s.RedisNodes[replicaIP] = &RedisNodeState{IP: replicaIP, Role: flagSlave, Reachable: true}
	s.SentinelNodes["10.0.0.10"] = &SentinelNodeState{
		IP: "10.0.0.10", Reachable: true, Monitoring: true, MasterIP: masterIP,
		Replicas: []ReplicaInfo{
			{IP: replicaIP, Flags: flagSlave},
			{IP: ghostRepIP, Flags: "slave,s_down"},
		},
	}
	return s
}

func TestGhostReplicaResetSafe(t *testing.T) {
	tests := []struct {
		name         string
		mutate       func(*SentinelClusterState)
		ghostFound   bool
		clusterWhole bool
		want         bool
	}{
		{
			name:         "whole cluster, ghost replica, living master, healthy replica -> reset",
			ghostFound:   true,
			clusterWhole: true,
			want:         true,
		},
		{
			// The LR-013 bug: a RESET racing a master loss. The cluster is not whole,
			// so we must defer the RESET to avoid wiping replica knowledge.
			name:         "cluster NOT whole -> never reset",
			ghostFound:   true,
			clusterWhole: false,
			want:         false,
		},
		{
			name:         "no ghost replica observed -> nothing to prune",
			ghostFound:   false,
			clusterWhole: true,
			want:         false,
		},
		{
			name:         "leaderless (no consensus master) -> stay passive",
			mutate:       func(s *SentinelClusterState) { s.RealMasterIP = "" },
			ghostFound:   true,
			clusterWhole: true,
			want:         false,
		},
		{
			name:         "consensus master is a ghost -> stay passive",
			mutate:       func(s *SentinelClusterState) { delete(s.ValidIPs, masterIP) },
			ghostFound:   true,
			clusterWhole: true,
			want:         false,
		},
		{
			name:         "consensus master unreachable -> stay passive",
			mutate:       func(s *SentinelClusterState) { s.RedisNodes[masterIP].Reachable = false },
			ghostFound:   true,
			clusterWhole: true,
			want:         false,
		},
		{
			name: "no healthy replica known to sentinel -> would strand RESET",
			mutate: func(s *SentinelClusterState) {
				// Only the ghost replica remains known; the healthy one is gone.
				s.SentinelNodes["10.0.0.10"].Replicas = []ReplicaInfo{
					{IP: ghostRepIP, Flags: "slave,s_down"},
				}
			},
			ghostFound:   true,
			clusterWhole: true,
			want:         false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := healthySentinelState()
			if tt.mutate != nil {
				tt.mutate(s)
			}
			if got := s.GhostReplicaResetSafe(tt.ghostFound, tt.clusterWhole); got != tt.want {
				t.Errorf("GhostReplicaResetSafe(ghostFound=%v, clusterWhole=%v) = %v, want %v",
					tt.ghostFound, tt.clusterWhole, got, tt.want)
			}
		})
	}
}

func TestHasHealthyKnownReplica(t *testing.T) {
	s := healthySentinelState()
	if !s.HasHealthyKnownReplica() {
		t.Errorf("expected a healthy known replica in steady state")
	}

	// An unreachable sentinel's replica list must not count.
	s.SentinelNodes["10.0.0.10"].Reachable = false
	if s.HasHealthyKnownReplica() {
		t.Errorf("unreachable sentinel's replicas should not count as healthy")
	}
}
