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

// MigrationPhase is the phase of the in-place legacy→per-shard cluster migration
// (ADR-013). It is re-derived from live ground truth every reconcile pass and never
// read back from status (ADR-006).
type MigrationPhase string

const (
	// MigrationStandup: the new {name}-shard-K pods are not yet cluster members and no
	// address is known to MEET them (they are still coming up).
	MigrationStandup MigrationPhase = "Standup"
	// MigrationMeet: new pods have addresses but some are not yet in the cluster mesh.
	MigrationMeet MigrationPhase = "Meet"
	// MigrationDraining: some shard range is not yet owned by its new {name}-shard-K-0 master.
	MigrationDraining MigrationPhase = "Draining"
	// MigrationReplicasAttached: all ranges are on new masters, but some new replica is
	// not yet replicating its shard master.
	MigrationReplicasAttached MigrationPhase = "ReplicasAttached"
	// MigrationDecommission: everything is on the new layout; FORGET the drained legacy
	// nodes and delete the legacy StatefulSet.
	MigrationDecommission MigrationPhase = "Decommission"
	// MigrationComplete: no legacy nodes remain in the cluster.
	MigrationComplete MigrationPhase = "Complete"
)

// totalClusterSlots is the fixed Redis Cluster slot count (0..16383). Matches the
// literal used by IsHealthy and GenerateSlotRanges.
const totalClusterSlots = 16384

// ReplicaAttach describes one CLUSTER REPLICATE the driver should issue: make the
// node at ReplicaAddr replicate the new-shard master identified by MasterID.
type ReplicaAttach struct {
	ReplicaAddr string
	MasterID    string
}

// LegacyFacts carries the migration inputs the plan needs but cannot derive from
// live ground truth alone. It is assembled by the driver from the pod list + gt and
// is strictly pure data (strings/ints/maps, no k8s types) so the plan stays testable.
//
//   - SeedAddrs: reachable legacy master addresses usable as a MEET seed. A MEET can
//     only be issued when a seed exists; the plan holds in Standup otherwise.
//   - LegacyNodeIDs: NodeIDs of the pre-0.3 {name}-cluster-N nodes. A gt node whose
//     NodeID is here is legacy; used to FORGET at Decommission and to detect Complete.
//   - NewPodAddrs: each new {name}-shard-K-M pod name → its dial address (host:port).
//     The driver fills it once the new pods have IPs; a missing/empty entry means the
//     pod is not up yet (⇒ Standup, not Meet). Keyed by pod name so it uniformly serves
//     both MEET (masters + replicas) and replica-attach without fragile ordering.
type LegacyFacts struct {
	SeedAddrs     []string
	LegacyNodeIDs []string
	NewPodAddrs   map[string]string
}

// MigrationPlan is the pure output of planClusterMigration for one reconcile pass.
// It carries the derived Phase plus at most one action set for that phase (idempotent,
// resumable from live state); Reason is a short human log line.
type MigrationPlan struct {
	Phase        MigrationPhase
	Meets        []string        // new-pod addrs to MEET (via a legacy seed)
	Move         *ReshardMove    // next range to move this pass (nil if none)
	Replicates   []ReplicaAttach // {ReplicaAddr, MasterID} to attach
	Forgets      []string        // legacy node IDs to FORGET (sorted)
	DeleteLegacy bool            // decommission: delete {name}-cluster STS + PDB
	ShardsMoved  int
	TotalShards  int
	Reason       string
}

// planClusterMigration derives the migration phase and the single action set for this
// pass PURELY from live ground truth + LegacyFacts (ADR-013 §7). Phase is never read
// back from status; the driver re-plans every reconcile. It emits one range Move per
// pass (the lowest un-migrated shard, for determinism), defers a replica attach whose
// target the replica does not yet NodeKnows, and only FORGETs/deletes legacy once every
// legacy node owns zero slots.
func planClusterMigration(gt *ClusterGroundTruth, shards, replicasPerShard int, name string,
	legacy LegacyFacts) MigrationPlan {
	plan := MigrationPlan{TotalShards: shards}
	if shards <= 0 {
		plan.Reason = "invalid shard count"
		return plan
	}
	ranges := GenerateSlotRanges(shards)

	// Complete: no legacy nodes remain in the cluster.
	if len(presentLegacyNodes(gt, legacy.LegacyNodeIDs)) == 0 {
		plan.Phase = MigrationComplete
		plan.ShardsMoved = countShardsOnNewMasters(gt, name, ranges)
		plan.Reason = "no legacy nodes remain; migration complete"
		return plan
	}

	// Standup / Meet: not all new pods are cluster members yet.
	if !allNewPodsMet(gt, name, shards, replicasPerShard) {
		return planStandupOrMeet(plan, gt, name, shards, replicasPerShard, legacy)
	}

	// Draining: move the lowest un-migrated shard range onto its new master.
	plan.ShardsMoved = countShardsOnNewMasters(gt, name, ranges)
	if plan.ShardsMoved < shards {
		plan.Phase = MigrationDraining
		if move, k, ok := nextDrainMove(gt, name, ranges); ok {
			plan.Move = move
			plan.Reason = fmt.Sprintf("draining shard %d range %s to its new master",
				k, FormatSlotRange(move.Start, move.End))
		} else {
			plan.Reason = "draining in progress; no clean range to move this pass"
		}
		return plan
	}

	// ReplicasAttached: attach new replicas to their new masters.
	replicates, unattached, deferred := replicaAttaches(gt, name, shards, replicasPerShard, legacy.NewPodAddrs)
	if unattached {
		plan.Phase = MigrationReplicasAttached
		plan.Replicates = replicates
		if deferred > 0 {
			plan.Reason = fmt.Sprintf("attaching %d replica(s); %d deferred (target not yet known via gossip)",
				len(replicates), deferred)
		} else {
			plan.Reason = fmt.Sprintf("attaching %d replica(s) to their new masters", len(replicates))
		}
		return plan
	}

	// Decommission: FORGET the drained legacy nodes and delete the legacy STS.
	plan.Phase = MigrationDecommission
	plan.Forgets = presentLegacyIDsSorted(gt, legacy.LegacyNodeIDs)
	plan.DeleteLegacy = !anyLegacyOwnsSlots(gt, legacy.LegacyNodeIDs)
	plan.Reason = fmt.Sprintf("decommissioning %d legacy node(s)", len(plan.Forgets))
	return plan
}

// planStandupOrMeet decides between Standup (nothing MET-able yet) and Meet (addresses
// known + a seed exists, so emit MEETs for the not-yet-joined new pods).
func planStandupOrMeet(plan MigrationPlan, gt *ClusterGroundTruth, name string, shards, rps int,
	legacy LegacyFacts) MigrationPlan {
	meets := missingNewPodMeets(gt, name, shards, rps, legacy.NewPodAddrs)
	if len(meets) > 0 && len(legacy.SeedAddrs) > 0 {
		plan.Phase = MigrationMeet
		plan.Meets = meets
		plan.Reason = fmt.Sprintf("MEET %d new pod(s) into the cluster", len(meets))
		return plan
	}
	plan.Phase = MigrationStandup
	plan.Reason = "waiting for new shard pods to come up"
	return plan
}

// legacyMigrationReady is the health gate (ADR-013 §2). Migration begins only when the
// legacy cluster is safe to rewrite: cluster_state ok, all 16384 slots assigned, all
// legacy pods Ready (kubelet readiness, injected as a bool — blackhole-proof per
// LR-017/023), and a reachable master quorum.
func legacyMigrationReady(gt *ClusterGroundTruth, allLegacyPodsReady bool) bool {
	if gt.ClusterState != "ok" {
		return false
	}
	if gt.TotalSlots != totalClusterSlots {
		return false
	}
	if !allLegacyPodsReady {
		return false
	}
	reachable, total := countReachableMasters(gt)
	return total > 0 && reachable*2 > total
}

// legacyShapePreserved reports whether the legacy cluster is shape-preserving (ADR-013
// §5): exactly `shards` slot-owning masters, each owning exactly one aligned
// GenerateSlotRanges(shards)[K] range, and a member count of shards×(1+replicasPerShard).
// Anything else is refused (the 1:1 range mapping only holds for the identical shape).
func legacyShapePreserved(gt *ClusterGroundTruth, shards, replicasPerShard int) bool {
	if shards <= 0 {
		return false
	}
	if len(gt.Nodes) != shards*(1+replicasPerShard) {
		return false
	}
	ranges := GenerateSlotRanges(shards)
	ownedIdx := make(map[int]bool, shards)
	masters := 0
	for _, n := range gt.Nodes {
		if n.Role != roleMaster || len(n.Slots) == 0 {
			continue
		}
		masters++
		if len(n.Slots) != 1 {
			return false // an aligned shard owns exactly one contiguous range
		}
		idx := alignedRangeIndex(ranges, n.Slots[0])
		if idx == -1 || ownedIdx[idx] {
			return false // non-aligned/fragmented range, or a duplicate owner
		}
		ownedIdx[idx] = true
	}
	return masters == shards && len(ownedIdx) == shards
}

// --- pure helpers ---

func newMasterPodName(name string, k int) string { return fmt.Sprintf("%s-shard-%d-0", name, k) }
func newReplicaPodName(name string, k, m int) string {
	return fmt.Sprintf("%s-shard-%d-%d", name, k, m)
}

// expectedNewPods enumerates the new per-shard pod names in deterministic order
// (shard K ascending, then master, then replicas 1..rps).
func expectedNewPods(name string, shards, rps int) []string {
	pods := make([]string, 0, shards*(1+rps))
	for k := range shards {
		pods = append(pods, newMasterPodName(name, k))
		for m := 1; m <= rps; m++ {
			pods = append(pods, newReplicaPodName(name, k, m))
		}
	}
	return pods
}

func allNewPodsMet(gt *ClusterGroundTruth, name string, shards, rps int) bool {
	for _, p := range expectedNewPods(name, shards, rps) {
		if gt.Nodes[p] == nil {
			return false
		}
	}
	return true
}

// missingNewPodMeets returns, in enumeration order, the dial addresses of new pods that
// are not yet cluster members and whose address is known.
func missingNewPodMeets(gt *ClusterGroundTruth, name string, shards, rps int,
	addrs map[string]string) []string {
	var meets []string
	for _, p := range expectedNewPods(name, shards, rps) {
		if gt.Nodes[p] != nil {
			continue
		}
		if addr := addrs[p]; addr != "" {
			meets = append(meets, addr)
		}
	}
	return meets
}

func nodeOwnsRange(n *ClusterNodeState, start, end int) bool {
	for _, s := range n.Slots {
		st, en, err := ParseSlotRange(s)
		if err == nil && st == start && en == end {
			return true
		}
	}
	return false
}

// alignedRangeIndex returns the shard index whose aligned range equals slotStr, or -1
// if slotStr is fragmented / not one of the expected ranges.
func alignedRangeIndex(ranges []struct{ Start, End int }, slotStr string) int {
	st, en, err := ParseSlotRange(slotStr)
	if err != nil {
		return -1
	}
	for i, r := range ranges {
		if r.Start == st && r.End == en {
			return i
		}
	}
	return -1
}

func countShardsOnNewMasters(gt *ClusterGroundTruth, name string, ranges []struct{ Start, End int }) int {
	count := 0
	for k, r := range ranges {
		if m := gt.Nodes[newMasterPodName(name, k)]; m != nil && nodeOwnsRange(m, r.Start, r.End) {
			count++
		}
	}
	return count
}

// nextDrainMove returns the Move for the lowest shard K whose new master does not yet own
// its range, sourced from whatever node currently owns that range (a legacy node). It
// skips a shard whose range has no clean single owner this pass (mid-dance) so a later
// pass resumes it; ok=false means no cleanly-movable range remains this pass.
func nextDrainMove(gt *ClusterGroundTruth, name string,
	ranges []struct{ Start, End int }) (*ReshardMove, int, bool) {
	for k, r := range ranges {
		destName := newMasterPodName(name, k)
		dest := gt.Nodes[destName]
		if dest != nil && nodeOwnsRange(dest, r.Start, r.End) {
			continue // already migrated
		}
		source := nodeOwningRange(gt, r.Start, r.End, destName)
		if source == nil || dest == nil {
			continue // no clean owner this pass; resume later
		}
		return &ReshardMove{Start: r.Start, End: r.End, Source: source, Dest: dest}, k, true
	}
	return nil, 0, false
}

func nodeOwningRange(gt *ClusterGroundTruth, start, end int, excludePod string) *ClusterNodeState {
	for _, n := range gt.Nodes {
		if n.PodName == excludePod {
			continue
		}
		if nodeOwnsRange(n, start, end) {
			return n
		}
	}
	return nil
}

// replicaAttaches returns the CLUSTER REPLICATE actions for new replicas not yet attached
// to their shard master. A replica whose master the replica does not yet NodeKnows is
// deferred (counted, not emitted) to avoid ERR Unknown node. unattached is true if any
// new replica still needs attaching (emitted or deferred).
func replicaAttaches(gt *ClusterGroundTruth, name string, shards, rps int,
	addrs map[string]string) (replicates []ReplicaAttach, unattached bool, deferred int) {
	for k := range shards {
		master := gt.Nodes[newMasterPodName(name, k)]
		if master == nil {
			continue
		}
		for m := 1; m <= rps; m++ {
			repName := newReplicaPodName(name, k, m)
			rep := gt.Nodes[repName]
			if rep == nil {
				continue
			}
			if rep.Role == roleReplica && rep.MasterNodeID == master.NodeID {
				continue // already attached
			}
			unattached = true
			if !gt.NodeKnows(rep.NodeID, master.NodeID) {
				deferred++
				continue
			}
			replicates = append(replicates, ReplicaAttach{ReplicaAddr: addrs[repName], MasterID: master.NodeID})
		}
	}
	return replicates, unattached, deferred
}

func presentLegacyNodes(gt *ClusterGroundTruth, legacyIDs []string) []*ClusterNodeState {
	idset := make(map[string]bool, len(legacyIDs))
	for _, id := range legacyIDs {
		idset[id] = true
	}
	var out []*ClusterNodeState
	for _, n := range gt.Nodes {
		if idset[n.NodeID] {
			out = append(out, n)
		}
	}
	return out
}

func presentLegacyIDsSorted(gt *ClusterGroundTruth, legacyIDs []string) []string {
	present := presentLegacyNodes(gt, legacyIDs)
	ids := make([]string, 0, len(present))
	for _, n := range present {
		ids = append(ids, n.NodeID)
	}
	sort.Strings(ids)
	return ids
}

func anyLegacyOwnsSlots(gt *ClusterGroundTruth, legacyIDs []string) bool {
	for _, n := range presentLegacyNodes(gt, legacyIDs) {
		if len(n.Slots) > 0 {
			return true
		}
	}
	return false
}

func countReachableMasters(gt *ClusterGroundTruth) (reachable, total int) {
	for _, n := range gt.Nodes {
		if n.Role == roleMaster && len(n.Slots) > 0 {
			total++
			if n.Reachable {
				reachable++
			}
		}
	}
	return reachable, total
}
