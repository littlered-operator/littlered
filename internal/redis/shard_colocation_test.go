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

func TestShardIndexFromPodName(t *testing.T) {
	cases := map[string]int{
		"store-cluster-shard-0-0":  0,
		"store-cluster-shard-1-1":  1,
		"store-cluster-shard-2-0":  2,
		"store-cluster-shard-10-3": 10,
		"store-cluster-cluster-0":  -1, // pre-0.3.0 striped name
		"store-cluster":            -1,
	}
	for name, want := range cases {
		if got := ShardIndexFromPodName(name); got != want {
			t.Errorf("ShardIndexFromPodName(%q) = %d, want %d", name, got, want)
		}
	}
}

// pmaster/preplica build pod-named ClusterNodeState fixtures (the shared master/replica
// helpers in cluster_state_test.go don't set PodName, which the colocation check needs).
func pmaster(pod, id string) *ClusterNodeState {
	return &ClusterNodeState{PodName: pod, NodeID: id, Role: roleMaster, MasterNodeID: "-", Slots: []string{"0-100"}}
}
func preplica(pod, id, masterID string) *ClusterNodeState {
	return &ClusterNodeState{PodName: pod, NodeID: id, Role: roleReplica, MasterNodeID: masterID}
}

func TestCheckShardColocation(t *testing.T) {
	// Correct: each shard's replica follows the master in its own STS.
	clean := gtFrom(
		pmaster("c-shard-0-0", "id0"), preplica("c-shard-0-1", "r0", "id0"),
		pmaster("c-shard-1-0", "id1"), preplica("c-shard-1-1", "r1", "id1"),
		pmaster("c-shard-2-0", "id2"), preplica("c-shard-2-1", "r2", "id2"),
	)
	if v := clean.CheckShardColocation(); len(v) != 0 {
		t.Fatalf("clean topology: expected no violations, got %+v", v)
	}

	// Scrambled: the exact cross-STS pairing observed at bootstrap in debug-artifacts-20260730.
	scrambled := gtFrom(
		pmaster("c-shard-0-0", "id0"), preplica("c-shard-1-1", "r1", "id0"), // shard-1 replica follows shard-0
		pmaster("c-shard-1-0", "id1"), preplica("c-shard-2-1", "r2", "id1"), // shard-2 replica follows shard-1
		pmaster("c-shard-2-0", "id2"), preplica("c-shard-0-1", "r0", "id2"), // shard-0 replica follows shard-2
	)
	v := scrambled.CheckShardColocation()
	if len(v) != 3 {
		t.Fatalf("scrambled topology: expected 3 violations, got %d: %+v", len(v), v)
	}
	// Deterministic order (by replica pod name) and correct shard attribution.
	if v[0].ReplicaPod != "c-shard-0-1" || v[0].ReplicaShard != 0 || v[0].MasterShard != 2 {
		t.Errorf("violation[0] = %+v, want replica c-shard-0-1 (shard 0) under master shard 2", v[0])
	}
}
