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
	"context"
	"errors"
	"testing"
)

// info84 is the tail of a real Redis 8.4.2 CLUSTER INFO (from debug-0720): it carries
// the cluster_slot_migration_* machinery that signals native atomic slot migration.
const info84 = `cluster_state:ok
cluster_slots_assigned:16384
cluster_slots_ok:16384
cluster_known_nodes:6
cluster_size:2
cluster_current_epoch:22
cluster_my_epoch:22
cluster_slot_migration_active_tasks:0
cluster_slot_migration_active_trim_running:0
`

// infoPre84 is a pre-8.4 CLUSTER INFO: no slot-migration fields.
const infoPre84 = `cluster_state:ok
cluster_slots_assigned:16384
cluster_slots_ok:16384
cluster_known_nodes:6
cluster_size:3
cluster_current_epoch:6
cluster_my_epoch:6
`

func TestParseClusterInfo_AtomicSlotMigration(t *testing.T) {
	if got := ParseClusterInfo(info84); !got.AtomicSlotMigration {
		t.Error("expected AtomicSlotMigration=true for 8.4 CLUSTER INFO with cluster_slot_migration_* fields")
	}
	if got := ParseClusterInfo(infoPre84); got.AtomicSlotMigration {
		t.Error("expected AtomicSlotMigration=false for pre-8.4 CLUSTER INFO without slot-migration fields")
	}
	// Sanity: the non-ASM fields still parse from the 8.4 sample.
	if got := ParseClusterInfo(info84); got.State != "ok" || got.SlotsAssigned != 16384 {
		t.Errorf("regression parsing base fields: state=%q slots=%d", got.State, got.SlotsAssigned)
	}
}

// asmGatherer is a fakeGatherer variant whose per-IP CLUSTER INFO reports ASM support
// according to f.asm, so we can exercise the AND-over-reachable-nodes verdict.
type asmGatherer struct {
	fakeGatherer
	asm map[string]bool
}

func (g *asmGatherer) GetClusterInfo(_ context.Context, _, ip string) (*ClusterInfo, error) {
	if g.dead[ip] {
		return nil, errors.New("dial timeout")
	}
	return &ClusterInfo{State: "ok", SlotsAssigned: 16384, AtomicSlotMigration: g.asm[ip]}, nil
}

// TestGatherClusterGroundTruth_ASMVerdict: gt.AtomicSlotMigration is true only when
// EVERY reachable node reports support (safe for rolling upgrades).
func TestGatherClusterGroundTruth_ASMVerdict(t *testing.T) {
	clusterPods := map[string]string{ipPod0: "pod-0", ipPod1: "pod-1"}
	nodeID := map[string]string{ipPod0: "m1", ipPod1: "m2"}

	t.Run("all nodes support ASM", func(t *testing.T) {
		g := &asmGatherer{
			fakeGatherer: fakeGatherer{nodeID: nodeID, dead: map[string]bool{}, gossip: twoMasterOneReplicaGossip()},
			asm:          map[string]bool{ipPod0: true, ipPod1: true},
		}
		gt := GatherClusterGroundTruth(context.Background(), g, clusterPods)
		if !gt.AtomicSlotMigration {
			t.Error("expected AtomicSlotMigration=true when all reachable nodes support it")
		}
	})

	t.Run("mixed versions fall back", func(t *testing.T) {
		g := &asmGatherer{
			fakeGatherer: fakeGatherer{nodeID: nodeID, dead: map[string]bool{}, gossip: twoMasterOneReplicaGossip()},
			asm:          map[string]bool{ipPod0: true, ipPod1: false}, // mid rolling-upgrade
		}
		gt := GatherClusterGroundTruth(context.Background(), g, clusterPods)
		if gt.AtomicSlotMigration {
			t.Error("expected AtomicSlotMigration=false when any reachable node lacks support")
		}
	})
}
