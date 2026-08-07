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

import (
	"reflect"
	"testing"
)

// Red-first table tests for the ADR-013 pure migration seam. They assert the exact
// MigrationPlan for every phase transition of planClusterMigration, and the truth
// table of the two predicates. Authored against zero-value stubs and observed to
// fail before the real bodies landed (see the milestone report).

// mgt builds a ClusterGroundTruth keyed by PodName from the given nodes. Health
// fields (ClusterState/TotalSlots) default to a healthy legacy cluster; tests that
// exercise the health gate override them explicitly.
func mgt(nodes ...*ClusterNodeState) *ClusterGroundTruth {
	gt := NewClusterGroundTruth()
	gt.ClusterState = "ok"
	gt.TotalSlots = 16384
	for _, n := range nodes {
		gt.Nodes[n.PodName] = n
		gt.AllNodeIDs[n.NodeID] = true
	}
	return gt
}

// legacyFacts for the shards=2, replicasPerShard=1 fixtures below.
func mFacts() LegacyFacts {
	return LegacyFacts{
		SeedAddrs:     []string{"10.0.0.1:6379"},
		LegacyNodeIDs: []string{"L0", "L1", "L2", "L3"},
		NewPodAddrs: map[string]string{
			"mr-shard-0-0": "10.1.0.1:6379",
			"mr-shard-0-1": "10.1.0.2:6379",
			"mr-shard-1-0": "10.1.0.3:6379",
			"mr-shard-1-1": "10.1.0.4:6379",
		},
	}
}

// The four legacy pods of a healthy shards=2/rps=1 legacy cluster, in the given
// slot-ownership state.
func legacyDrained() []*ClusterNodeState {
	return []*ClusterNodeState{
		rEmpty("mr-cluster-0", "L0"),
		rEmpty("mr-cluster-1", "L1"),
		rReplica("mr-cluster-2", "L2", "L0"),
		rReplica("mr-cluster-3", "L3", "L1"),
	}
}

func TestPlanClusterMigration(t *testing.T) {
	const name, shards, rps = "mr", 2, 1
	seedOnly := LegacyFacts{SeedAddrs: []string{"10.0.0.1:6379"}, LegacyNodeIDs: []string{"L0", "L1", "L2", "L3"}}

	// Shared node fixtures.
	legNodesFull := func(m0slots, m1slots []string) []*ClusterNodeState {
		return []*ClusterNodeState{
			rMaster("mr-cluster-0", "L0", m0slots...),
			rMaster("mr-cluster-1", "L1", m1slots...),
			rReplica("mr-cluster-2", "L2", "L0"),
			rReplica("mr-cluster-3", "L3", "L1"),
		}
	}

	tests := []struct {
		name  string
		gt    *ClusterGroundTruth
		facts LegacyFacts
		want  MigrationPlan
	}{
		{
			name:  "standup: new pods not up yet (no addrs)",
			gt:    mgt(legNodesFull([]string{"0-8191"}, []string{"8192-16383"})...),
			facts: seedOnly,
			want:  MigrationPlan{Phase: MigrationStandup, TotalShards: 2},
		},
		{
			name:  "meet: all new pods up, none MET",
			gt:    mgt(legNodesFull([]string{"0-8191"}, []string{"8192-16383"})...),
			facts: mFacts(),
			want: MigrationPlan{
				Phase:       MigrationMeet,
				Meets:       []string{"10.1.0.1:6379", "10.1.0.2:6379", "10.1.0.3:6379", "10.1.0.4:6379"},
				TotalShards: 2,
			},
		},
		{
			name: "meet: partially MET (shard 0 joined, shard 1 not)",
			gt: mgt(append(legNodesFull([]string{"0-8191"}, []string{"8192-16383"}),
				rEmpty("mr-shard-0-0", "N00"), rEmpty("mr-shard-0-1", "N01"))...),
			facts: mFacts(),
			want: MigrationPlan{
				Phase:       MigrationMeet,
				Meets:       []string{"10.1.0.3:6379", "10.1.0.4:6379"},
				TotalShards: 2,
			},
		},
		{
			name: "draining: shard 0 moved, shard 1 still on legacy",
			gt: mgt(
				rMaster("mr-shard-0-0", "N00", "0-8191"),
				rEmpty("mr-shard-0-1", "N01"),
				rEmpty("mr-shard-1-0", "N10"),
				rEmpty("mr-shard-1-1", "N11"),
				rEmpty("mr-cluster-0", "L0"),
				rMaster("mr-cluster-1", "L1", "8192-16383"),
				rReplica("mr-cluster-2", "L2", "L0"),
				rReplica("mr-cluster-3", "L3", "L1"),
			),
			facts: mFacts(),
			want: MigrationPlan{
				Phase: MigrationDraining,
				Move: &ReshardMove{
					Start:  8192,
					End:    16383,
					Source: rMaster("mr-cluster-1", "L1", "8192-16383"),
					Dest:   rEmpty("mr-shard-1-0", "N10"),
				},
				ShardsMoved: 1,
				TotalShards: 2,
			},
		},
		{
			name: "draining: nothing moved yet, lowest-K first",
			gt: mgt(
				rEmpty("mr-shard-0-0", "N00"),
				rEmpty("mr-shard-0-1", "N01"),
				rEmpty("mr-shard-1-0", "N10"),
				rEmpty("mr-shard-1-1", "N11"),
				rMaster("mr-cluster-0", "L0", "0-8191"),
				rMaster("mr-cluster-1", "L1", "8192-16383"),
				rReplica("mr-cluster-2", "L2", "L0"),
				rReplica("mr-cluster-3", "L3", "L1"),
			),
			facts: mFacts(),
			want: MigrationPlan{
				Phase: MigrationDraining,
				Move: &ReshardMove{
					Start:  0,
					End:    8191,
					Source: rMaster("mr-cluster-0", "L0", "0-8191"),
					Dest:   rEmpty("mr-shard-0-0", "N00"),
				},
				ShardsMoved: 0,
				TotalShards: 2,
			},
		},
		{
			name: "replicas-attached: ranges moved, replicas known but unattached",
			gt: func() *ClusterGroundTruth {
				gt := mgt(
					rMaster("mr-shard-0-0", "N00", "0-8191"),
					rEmpty("mr-shard-0-1", "N01"),
					rMaster("mr-shard-1-0", "N10", "8192-16383"),
					rEmpty("mr-shard-1-1", "N11"),
				)
				for _, n := range legacyDrained() {
					gt.Nodes[n.PodName] = n
					gt.AllNodeIDs[n.NodeID] = true
				}
				gt.KnownNodes = map[string][]string{"N01": {"N00"}, "N11": {"N10"}}
				return gt
			}(),
			facts: mFacts(),
			want: MigrationPlan{
				Phase: MigrationReplicasAttached,
				Replicates: []ReplicaAttach{
					{ReplicaAddr: "10.1.0.2:6379", MasterID: "N00"},
					{ReplicaAddr: "10.1.0.4:6379", MasterID: "N10"},
				},
				ShardsMoved: 2,
				TotalShards: 2,
			},
		},
		{
			name: "replicas-attached: one replica deferred (does not NodeKnows master)",
			gt: func() *ClusterGroundTruth {
				gt := mgt(
					rMaster("mr-shard-0-0", "N00", "0-8191"),
					rEmpty("mr-shard-0-1", "N01"),
					rMaster("mr-shard-1-0", "N10", "8192-16383"),
					rEmpty("mr-shard-1-1", "N11"),
				)
				for _, n := range legacyDrained() {
					gt.Nodes[n.PodName] = n
					gt.AllNodeIDs[n.NodeID] = true
				}
				gt.KnownNodes = map[string][]string{"N11": {"N10"}} // N01 does not know N00
				return gt
			}(),
			facts: mFacts(),
			want: MigrationPlan{
				Phase: MigrationReplicasAttached,
				Replicates: []ReplicaAttach{
					{ReplicaAddr: "10.1.0.4:6379", MasterID: "N10"},
				},
				ShardsMoved: 2,
				TotalShards: 2,
			},
		},
		{
			name: "decommission: all moved and attached, legacy still present",
			gt: func() *ClusterGroundTruth {
				gt := mgt(
					rMaster("mr-shard-0-0", "N00", "0-8191"),
					rReplica("mr-shard-0-1", "N01", "N00"),
					rMaster("mr-shard-1-0", "N10", "8192-16383"),
					rReplica("mr-shard-1-1", "N11", "N10"),
				)
				for _, n := range legacyDrained() {
					gt.Nodes[n.PodName] = n
					gt.AllNodeIDs[n.NodeID] = true
				}
				return gt
			}(),
			facts: mFacts(),
			want: MigrationPlan{
				Phase:        MigrationDecommission,
				Forgets:      []string{"L0", "L1", "L2", "L3"},
				DeleteLegacy: true,
				ShardsMoved:  2,
				TotalShards:  2,
			},
		},
		{
			name: "complete: no legacy nodes remain",
			gt: mgt(
				rMaster("mr-shard-0-0", "N00", "0-8191"),
				rReplica("mr-shard-0-1", "N01", "N00"),
				rMaster("mr-shard-1-0", "N10", "8192-16383"),
				rReplica("mr-shard-1-1", "N11", "N10"),
			),
			facts: mFacts(),
			want:  MigrationPlan{Phase: MigrationComplete, ShardsMoved: 2, TotalShards: 2},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := planClusterMigration(tc.gt, shards, rps, name, tc.facts)
			assertPlanEqual(t, got, tc.want)
		})
	}
}

// assertPlanEqual compares the load-bearing fields of a MigrationPlan, ignoring Reason
// (a human string). Move is compared by value (Start/End/Source/Dest identity by NodeID).
func assertPlanEqual(t *testing.T, got, want MigrationPlan) {
	t.Helper()
	if got.Phase != want.Phase {
		t.Errorf("Phase = %q, want %q", got.Phase, want.Phase)
	}
	if !reflect.DeepEqual(got.Meets, want.Meets) {
		t.Errorf("Meets = %v, want %v", got.Meets, want.Meets)
	}
	if !reflect.DeepEqual(got.Replicates, want.Replicates) {
		t.Errorf("Replicates = %v, want %v", got.Replicates, want.Replicates)
	}
	if !reflect.DeepEqual(got.Forgets, want.Forgets) {
		t.Errorf("Forgets = %v, want %v", got.Forgets, want.Forgets)
	}
	if got.DeleteLegacy != want.DeleteLegacy {
		t.Errorf("DeleteLegacy = %v, want %v", got.DeleteLegacy, want.DeleteLegacy)
	}
	if got.ShardsMoved != want.ShardsMoved {
		t.Errorf("ShardsMoved = %d, want %d", got.ShardsMoved, want.ShardsMoved)
	}
	if got.TotalShards != want.TotalShards {
		t.Errorf("TotalShards = %d, want %d", got.TotalShards, want.TotalShards)
	}
	assertMoveEqual(t, got.Move, want.Move)
}

func assertMoveEqual(t *testing.T, got, want *ReshardMove) {
	t.Helper()
	if (got == nil) != (want == nil) {
		t.Fatalf("Move = %+v, want %+v", got, want)
	}
	if got == nil {
		return
	}
	if got.Start != want.Start || got.End != want.End {
		t.Errorf("Move range = %d-%d, want %d-%d", got.Start, got.End, want.Start, want.End)
	}
	if got.Source == nil || got.Source.NodeID != want.Source.NodeID {
		t.Errorf("Move.Source = %+v, want NodeID %s", got.Source, want.Source.NodeID)
	}
	if got.Dest == nil || got.Dest.NodeID != want.Dest.NodeID {
		t.Errorf("Move.Dest = %+v, want NodeID %s", got.Dest, want.Dest.NodeID)
	}
}

func TestLegacyMigrationReady(t *testing.T) {
	healthy := func() *ClusterGroundTruth {
		return mgt(
			rMaster("mr-cluster-0", "L0", "0-8191"),
			rMaster("mr-cluster-1", "L1", "8192-16383"),
			rReplica("mr-cluster-2", "L2", "L0"),
			rReplica("mr-cluster-3", "L3", "L1"),
		)
	}

	tests := []struct {
		name      string
		mutate    func(gt *ClusterGroundTruth)
		podsReady bool
		want      bool
	}{
		{name: "healthy", mutate: func(*ClusterGroundTruth) {}, podsReady: true, want: true},
		{name: "missing slots", mutate: func(gt *ClusterGroundTruth) { gt.TotalSlots = 16000 }, podsReady: true, want: false},
		{name: "state not ok", mutate: func(gt *ClusterGroundTruth) { gt.ClusterState = "fail" }, podsReady: true, want: false},
		{name: "a legacy pod not ready", mutate: func(*ClusterGroundTruth) {}, podsReady: false, want: false},
		{
			name: "no master quorum",
			mutate: func(gt *ClusterGroundTruth) {
				gt.Nodes["mr-cluster-1"].Reachable = false // 1 of 2 masters reachable
			},
			podsReady: true,
			want:      false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			gt := healthy()
			tc.mutate(gt)
			if got := legacyMigrationReady(gt, tc.podsReady); got != tc.want {
				t.Errorf("legacyMigrationReady = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestLegacyShapePreserved(t *testing.T) {
	const shards, rps = 2, 1
	exact := func() *ClusterGroundTruth {
		return mgt(
			rMaster("mr-cluster-0", "L0", "0-8191"),
			rMaster("mr-cluster-1", "L1", "8192-16383"),
			rReplica("mr-cluster-2", "L2", "L0"),
			rReplica("mr-cluster-3", "L3", "L1"),
		)
	}

	tests := []struct {
		name string
		gt   *ClusterGroundTruth
		want bool
	}{
		{name: "exact shape", gt: exact(), want: true},
		{
			name: "wrong master count (consolidated)",
			gt: mgt(
				rMaster("mr-cluster-0", "L0", "0-8191", "8192-16383"),
				rReplica("mr-cluster-2", "L2", "L0"),
				rReplica("mr-cluster-3", "L3", "L0"),
				rReplica("mr-cluster-1", "L1", "L0"),
			),
			want: false,
		},
		{
			name: "fragmented / non-aligned range",
			gt: mgt(
				rMaster("mr-cluster-0", "L0", "0-4000"),
				rMaster("mr-cluster-1", "L1", "8192-16383"),
				rReplica("mr-cluster-2", "L2", "L0"),
				rReplica("mr-cluster-3", "L3", "L1"),
			),
			want: false,
		},
		{
			name: "wrong member count",
			gt: mgt(
				rMaster("mr-cluster-0", "L0", "0-8191"),
				rMaster("mr-cluster-1", "L1", "8192-16383"),
				rReplica("mr-cluster-2", "L2", "L0"),
				rReplica("mr-cluster-3", "L3", "L1"),
				rReplica("mr-cluster-4", "L4", "L0"),
			),
			want: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := legacyShapePreserved(tc.gt, shards, rps); got != tc.want {
				t.Errorf("legacyShapePreserved = %v, want %v", got, tc.want)
			}
		})
	}
}
