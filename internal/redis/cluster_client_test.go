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

// TestParseClusterNodes_SkipsMigrationMarkers guards a bug LR-018's reshard dance
// exposed: while a shard range is mid-migration, CLUSTER NODES lists the owned ranges
// followed by per-slot migrating "[slot->-id]" / importing "[slot-<-id]" notations.
// Those brackets are NOT owned slots and must be excluded from node.Slots — otherwise
// ParseSlotRange chokes on them and an importing-but-slotless node looks like a
// slot-owning master (so the operator wrongly believes the cluster is healthy and
// abandons the reshard mid-flight).
func TestParseClusterNodes_SkipsMigrationMarkers(t *testing.T) {
	// Source node: owns two ranges, with the second range being migrated out.
	src := "fdae637e 10.0.0.1:6379@16379 myself,master - 0 0 6 connected 0-5461 10923-16383 " +
		"[10923->-383ab0b3] [10924->-383ab0b3] [10925->-383ab0b3]"
	// Destination node: owns nothing yet, importing the slots.
	dst := "383ab0b3 10.0.0.2:6379@16379 master - 0 0 5 connected " +
		"[10923-<-fdae637e] [10924-<-fdae637e] [10925-<-fdae637e]"

	nodes := ParseClusterNodes(src + "\n" + dst)
	if len(nodes) != 2 {
		t.Fatalf("expected 2 nodes, got %d", len(nodes))
	}

	// Source keeps only its two owned ranges — no bracket tokens.
	wantSrc := []string{"0-5461", "10923-16383"}
	if len(nodes[0].Slots) != len(wantSrc) || nodes[0].Slots[0] != wantSrc[0] || nodes[0].Slots[1] != wantSrc[1] {
		t.Errorf("source Slots = %v, want %v (migrating markers must be dropped)", nodes[0].Slots, wantSrc)
	}

	// Destination owns NO slots — importing markers are not ownership.
	if len(nodes[1].Slots) != 0 {
		t.Errorf("importing dest Slots = %v, want empty (importing markers are not owned slots)", nodes[1].Slots)
	}
}
