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
	"fmt"
	"sort"
)

// ReshardMove describes a single key-preserving slot-range migration the operator
// should perform to restore one-master-per-shard: move the inclusive slot range
// [Start,End] (which aligns with exactly one expected shard range) from Source — a
// reachable master that currently owns more than one shard range — to Dest, a
// reachable empty master. See LR-018 (docs/CLUSTER_CONSOLIDATED_SHARD_RECOVERY.md).
type ReshardMove struct {
	Start  int
	End    int
	Source *ClusterNodeState
	Dest   *ClusterNodeState
}

// ReshardPlan is the output of PlanReshard: the set of moves that would restore a
// distinct slot-owning master per shard. Moves is empty when no reshard is needed
// (already one-master-per-shard) or when the state is not actionable (e.g. an
// over-consolidated master exists but there is no reachable empty master to
// receive the surplus range); Reason then carries a short log explanation.
type ReshardPlan struct {
	Moves  []ReshardMove
	Reason string
}

// SafeMissingShardTarget reports whether it is safe to assign a currently-unowned
// (missing) shard range to node during Step 3 recovery. It is safe only when node is
// a reachable EMPTY master (a master owning no slots). Assigning a shard range to a
// master that already owns a different range is exactly what consolidates two shards
// onto one node and creates the LR-018 deadlock; the strict pod-index→shard model in
// Step 3 must never do that when roles have drifted.
func SafeMissingShardTarget(node *ClusterNodeState) bool {
	return node != nil && node.Reachable && node.Role == roleMaster && len(node.Slots) == 0
}

// SlotsNeedingDrain returns, in ascending order, the slots that still hold keys on the
// migration source (count > 0). An empty result means the range is fully drained and
// ownership can be flipped to the destination. Pure helper for the pre-8.4 key-preserving
// reshard dance (LR-018 §7.2).
func SlotsNeedingDrain(counts map[int]int) []int {
	var slots []int
	for slot, n := range counts {
		if n > 0 {
			slots = append(slots, slot)
		}
	}
	sort.Ints(slots)
	return slots
}

// PlanReshard is the pure decision seam for LR-018. It detects the consolidated-shard
// deadlock — a reachable master owning more than one expected shard range while other
// reachable masters sit slotless (empty) — and emits the key-preserving moves that
// restore a distinct slot-owning master per shard.
//
// It is deliberately conservative and cannot deadlock:
//   - It only reasons about REACHABLE masters; it never plans a move to/from a node it
//     cannot act on (execution re-checks reachability regardless).
//   - It keeps the lowest-index shard on an over-consolidated master and moves the rest,
//     so the choice is deterministic and never fragments a shard.
//   - If a master owns a non-aligned/fragmented range, it refuses (defers) exactly like
//     Step 3, to avoid data loss.
//   - If there is no reachable empty master to receive a surplus range, it defers (no
//     moves) rather than inventing a destination.
//   - On a one-master-per-shard topology it returns no moves.
//
// Destination selection is distinctness-only (LR-018 §11.3, least interference): the
// lowest-PodName reachable empty master, tie-broken by NodeID. Restoring the strict
// bootstrap pod-index→shard model is intentionally NOT attempted here.
func PlanReshard(gt *ClusterGroundTruth, shards int) ReshardPlan {
	if shards <= 0 {
		return ReshardPlan{Reason: "invalid shard count"}
	}
	expected := GenerateSlotRanges(shards)

	// ownedShards: reachable slot-owning master -> the expected shard indices it owns.
	ownedShards := make(map[*ClusterNodeState][]int)
	var emptyMasters []*ClusterNodeState

	for _, n := range gt.Nodes {
		if n.Role != roleMaster || !n.Reachable {
			continue
		}
		if len(n.Slots) == 0 {
			emptyMasters = append(emptyMasters, n)
			continue
		}
		for _, slotStr := range n.Slots {
			start, end, err := ParseSlotRange(slotStr)
			if err != nil {
				return ReshardPlan{Reason: fmt.Sprintf("unparseable slot range %q on %s; deferring", slotStr, n.PodName)}
			}
			idx := -1
			for i, r := range expected {
				if r.Start == start && r.End == end {
					idx = i
					break
				}
			}
			if idx == -1 {
				// Fragmented / non-aligned range — Step 3 refuses to touch this to
				// avoid data loss, and so do we.
				return ReshardPlan{Reason: fmt.Sprintf("non-aligned slot range %d-%d on %s; deferring to avoid data loss", start, end, n.PodName)}
			}
			ownedShards[n] = append(ownedShards[n], idx)
		}
	}

	// Surplus ranges: an over-consolidated master (owns >1 shard) keeps its lowest-index
	// shard; every other owned shard is surplus to migrate off.
	type surplus struct {
		shardIdx int
		source   *ClusterNodeState
	}
	var surpluses []surplus
	for node, idxs := range ownedShards {
		if len(idxs) <= 1 {
			continue
		}
		sort.Ints(idxs)
		for _, idx := range idxs[1:] {
			surpluses = append(surpluses, surplus{shardIdx: idx, source: node})
		}
	}

	if len(surpluses) == 0 {
		return ReshardPlan{Reason: "one master per shard; no reshard needed"}
	}
	if len(emptyMasters) == 0 {
		return ReshardPlan{Reason: "over-consolidated master(s) present but no reachable empty master to receive surplus; deferring"}
	}

	// Deterministic pairing: surplus ranges by shard index, empty masters by PodName
	// (tie-broken by NodeID).
	sort.Slice(surpluses, func(i, j int) bool { return surpluses[i].shardIdx < surpluses[j].shardIdx })
	sort.Slice(emptyMasters, func(i, j int) bool {
		if emptyMasters[i].PodName != emptyMasters[j].PodName {
			return emptyMasters[i].PodName < emptyMasters[j].PodName
		}
		return emptyMasters[i].NodeID < emptyMasters[j].NodeID
	})

	moveCount := min(len(emptyMasters), len(surpluses))
	moves := make([]ReshardMove, 0, moveCount)
	for i := range moveCount {
		r := expected[surpluses[i].shardIdx]
		moves = append(moves, ReshardMove{
			Start:  r.Start,
			End:    r.End,
			Source: surpluses[i].source,
			Dest:   emptyMasters[i],
		})
	}

	reason := ""
	if len(surpluses) > len(emptyMasters) {
		reason = fmt.Sprintf("%d surplus range(s) but %d empty master(s); moving %d this pass",
			len(surpluses), len(emptyMasters), moveCount)
	}
	return ReshardPlan{Moves: moves, Reason: reason}
}
