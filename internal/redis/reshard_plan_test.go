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

// Reachable node builders for reshard planning. Unlike the IsHealthy helpers
// (master/emptyMaster/replica), these set PodName and Reachable=true, because
// PlanReshard reasons about reachable pods and selects a destination
// deterministically by PodName.
func rMaster(pod, id string, slots ...string) *ClusterNodeState {
	return &ClusterNodeState{PodName: pod, NodeID: id, Role: roleMaster, MasterNodeID: "-", Slots: slots, Reachable: true}
}

func rEmpty(pod, id string) *ClusterNodeState {
	return &ClusterNodeState{PodName: pod, NodeID: id, Role: roleMaster, MasterNodeID: "-", Reachable: true}
}

func rReplica(pod, id, masterID string) *ClusterNodeState {
	return &ClusterNodeState{PodName: pod, NodeID: id, Role: roleReplica, MasterNodeID: masterID, Reachable: true}
}

// TestSlotsNeedingDrain guards the pre-8.4 dance's drain/flip decision: only slots with
// keys remaining are returned (ascending), and an empty range yields none (ready to flip).
func TestSlotsNeedingDrain(t *testing.T) {
	got := SlotsNeedingDrain(map[int]int{10923: 5, 10924: 0, 10925: 3, 10926: 0})
	want := []int{10923, 10925}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Errorf("SlotsNeedingDrain = %v, want %v", got, want)
	}
	if got := SlotsNeedingDrain(map[int]int{1: 0, 2: 0}); len(got) != 0 {
		t.Errorf("fully-drained range should need no drain, got %v", got)
	}
	if got := SlotsNeedingDrain(map[int]int{}); len(got) != 0 {
		t.Errorf("empty counts should need no drain, got %v", got)
	}
}

// TestSafeMissingShardTarget guards the Step 3 hardening: a missing shard may be
// assigned only to a reachable empty master. Assigning to a master that already owns
// a range is the consolidation bug that creates the LR-018 deadlock.
func TestSafeMissingShardTarget(t *testing.T) {
	tests := []struct {
		name string
		node *ClusterNodeState
		want bool
	}{
		{"reachable empty master", rEmpty("cluster-4", "e4"), true},
		{"master already owning a range (the LR-018-creating case)", rMaster("cluster-2", "m2", "0-5461"), false},
		{"unreachable empty master", &ClusterNodeState{PodName: "cluster-5", NodeID: "e5", Role: roleMaster, MasterNodeID: "-", Reachable: false}, false},
		{"replica", rReplica("cluster-0", "r0", "m2"), false},
		{"nil", nil, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := SafeMissingShardTarget(tc.node); got != tc.want {
				t.Errorf("SafeMissingShardTarget(%s) = %v, want %v", tc.name, got, tc.want)
			}
		})
	}
}

// TestPlanReshard_ConsolidatedShard is the LR-018 field-report repro, built from
// the exact 6-node topology in debug-0720: shards=3, replicasPerShard=1, but one
// master (cluster-2) owns TWO expected shard ranges (0-5461 AND 10923-16383) while
// two pods (cluster-4, cluster-5) are slotless empty masters. GenerateSlotRanges(3)
// yields exactly [0-5461, 5462-10922, 10923-16383], so cluster-2 owns shard 0 and
// shard 2. The plan must move the surplus range (shard 2, keeping the lowest-index
// shard on the source) onto a reachable empty master.
func TestPlanReshard_ConsolidatedShard(t *testing.T) {
	gt := gtFrom(
		rMaster("cluster-2", "efa6f3a9", "0-5461", "10923-16383"),
		rMaster("cluster-3", "b0fefa4a", "5462-10922"),
		rEmpty("cluster-4", "fd527ae1"),
		rEmpty("cluster-5", "8b43246d"),
		rReplica("cluster-0", "033f", "efa6f3a9"),
		rReplica("cluster-1", "da90", "b0fefa4a"),
	)

	plan := PlanReshard(gt, 3)

	if len(plan.Moves) != 1 {
		t.Fatalf("expected exactly 1 reshard move, got %d (reason=%q)", len(plan.Moves), plan.Reason)
	}
	m := plan.Moves[0]
	if m.Start != 10923 || m.End != 16383 {
		t.Errorf("expected surplus range 10923-16383 to move, got %d-%d", m.Start, m.End)
	}
	if m.Source == nil || m.Source.NodeID != "efa6f3a9" {
		t.Errorf("expected source cluster-2 (efa6f3a9), got %+v", m.Source)
	}
	// Deterministic destination: no empty master matches the moved shard's intended
	// pod index (cluster-2 is the source), so fall back to the lowest-PodName empty
	// master → cluster-4.
	if m.Dest == nil || m.Dest.PodName != "cluster-4" {
		t.Errorf("expected dest cluster-4, got %+v", m.Dest)
	}
}

// TestPlanReshard_Healthy: a correct one-master-per-shard topology yields no moves.
func TestPlanReshard_Healthy(t *testing.T) {
	gt := gtFrom(
		rMaster("cluster-0", "m0", "0-5461"),
		rMaster("cluster-1", "m1", "5462-10922"),
		rMaster("cluster-2", "m2", "10923-16383"),
		rReplica("cluster-3", "r0", "m0"),
		rReplica("cluster-4", "r1", "m1"),
		rReplica("cluster-5", "r2", "m2"),
	)

	plan := PlanReshard(gt, 3)
	if len(plan.Moves) != 0 {
		t.Fatalf("expected no moves for a healthy topology, got %d", len(plan.Moves))
	}
}

// TestPlanReshard_NoEmptyMasterDefers: an over-consolidated master exists but there
// is no reachable empty master to receive the surplus range, so the planner defers
// (no moves) rather than inventing a destination. This is the "cannot deadlock"
// boundary: with no actionable target, PlanReshard must be a safe no-op.
func TestPlanReshard_NoEmptyMasterDefers(t *testing.T) {
	gt := gtFrom(
		rMaster("cluster-0", "m0", "0-5461", "10923-16383"),
		rMaster("cluster-1", "m1", "5462-10922"),
		rReplica("cluster-2", "r0", "m0"),
		rReplica("cluster-3", "r1", "m1"),
	)

	plan := PlanReshard(gt, 3)
	if len(plan.Moves) != 0 {
		t.Fatalf("expected no moves when no empty master is available, got %d", len(plan.Moves))
	}
	if plan.Reason == "" {
		t.Error("expected a non-empty Reason explaining the deferral")
	}
}
