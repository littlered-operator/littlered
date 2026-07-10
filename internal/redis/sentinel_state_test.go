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

func TestAllSentinelsBare(t *testing.T) {
	tests := []struct {
		name          string
		sentinels     []*SentinelNodeState
		wantBare      bool
		wantReachable int
	}{
		{
			name: "all reachable, none monitoring -> bare deadlock",
			sentinels: []*SentinelNodeState{
				{IP: "10.0.0.10", Reachable: true, Monitoring: false},
				{IP: "10.0.0.11", Reachable: true, Monitoring: false},
				{IP: "10.0.0.12", Reachable: true, Monitoring: false},
			},
			wantBare:      true,
			wantReachable: 3,
		},
		{
			name: "one sentinel monitoring -> not bare",
			sentinels: []*SentinelNodeState{
				{IP: "10.0.0.10", Reachable: true, Monitoring: true, MasterIP: masterIP},
				{IP: "10.0.0.11", Reachable: true, Monitoring: false},
			},
			wantBare:      false,
			wantReachable: 2,
		},
		{
			name: "unreachable sentinels do not count",
			sentinels: []*SentinelNodeState{
				{IP: "10.0.0.10", Reachable: true, Monitoring: false},
				{IP: "10.0.0.11", Reachable: false},
			},
			wantBare:      true,
			wantReachable: 1,
		},
		{
			name:          "no reachable sentinels -> not bare",
			sentinels:     []*SentinelNodeState{{IP: "10.0.0.10", Reachable: false}},
			wantBare:      false,
			wantReachable: 0,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := NewSentinelClusterState()
			for _, sn := range tt.sentinels {
				s.SentinelNodes[sn.IP] = sn
			}
			gotBare, gotReachable := s.AllSentinelsBare()
			if gotBare != tt.wantBare || gotReachable != tt.wantReachable {
				t.Errorf("AllSentinelsBare() = (%v, %d), want (%v, %d)",
					gotBare, gotReachable, tt.wantBare, tt.wantReachable)
			}
		})
	}
}

func TestDataHoldersAndBestDataHolder(t *testing.T) {
	t.Run("no data holders", func(t *testing.T) {
		s := NewSentinelClusterState()
		s.RedisNodes["10.0.0.1"] = &RedisNodeState{IP: "10.0.0.1", Reachable: true, Keys: 0}
		s.RedisNodes["10.0.0.2"] = &RedisNodeState{IP: "10.0.0.2", Reachable: false, Keys: 100}
		if got := s.DataHolders(); len(got) != 0 {
			t.Errorf("DataHolders() = %d holders, want 0 (empty pod + unreachable pod)", len(got))
		}
		best, diverged := s.BestDataHolder()
		if best != nil || diverged {
			t.Errorf("BestDataHolder() = (%v, %v), want (nil, false)", best, diverged)
		}
	})

	t.Run("highest offset wins", func(t *testing.T) {
		s := NewSentinelClusterState()
		s.RedisNodes["10.0.0.1"] = &RedisNodeState{IP: "10.0.0.1", Reachable: true, Keys: 500, Offset: 100, Replid: "A"}
		s.RedisNodes["10.0.0.2"] = &RedisNodeState{IP: "10.0.0.2", Reachable: true, Keys: 10, Offset: 900, Replid: "A"}
		if got := s.DataHolders(); len(got) != 2 {
			t.Fatalf("DataHolders() = %d, want 2", len(got))
		}
		best, diverged := s.BestDataHolder()
		if best == nil || best.IP != "10.0.0.2" {
			t.Errorf("BestDataHolder() picked %v, want 10.0.0.2 (higher offset despite fewer keys)", best)
		}
		if diverged {
			t.Errorf("BestDataHolder() diverged = true, want false (shared replid)")
		}
	})

	t.Run("tiebreak on keys then IP", func(t *testing.T) {
		s := NewSentinelClusterState()
		s.RedisNodes["10.0.0.3"] = &RedisNodeState{IP: "10.0.0.3", Reachable: true, Keys: 50, Offset: 100, Replid: "A"}
		s.RedisNodes["10.0.0.1"] = &RedisNodeState{IP: "10.0.0.1", Reachable: true, Keys: 50, Offset: 100, Replid: "A"}
		best, _ := s.BestDataHolder()
		if best == nil || best.IP != "10.0.0.1" {
			t.Errorf("BestDataHolder() picked %v, want 10.0.0.1 (equal offset+keys, lowest IP)", best)
		}
	})

	t.Run("divergent lineages flagged", func(t *testing.T) {
		s := NewSentinelClusterState()
		s.RedisNodes["10.0.0.1"] = &RedisNodeState{IP: "10.0.0.1", Reachable: true, Keys: 10, Offset: 100, Replid: "A"}
		s.RedisNodes["10.0.0.2"] = &RedisNodeState{IP: "10.0.0.2", Reachable: true, Keys: 10, Offset: 200, Replid: "B"}
		best, diverged := s.BestDataHolder()
		if best == nil || best.IP != "10.0.0.2" {
			t.Errorf("BestDataHolder() picked %v, want 10.0.0.2", best)
		}
		if !diverged {
			t.Errorf("BestDataHolder() diverged = false, want true (distinct replids)")
		}
	})
}
