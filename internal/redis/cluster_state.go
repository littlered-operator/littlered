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

import "slices"

const (
	roleMaster  = "master"
	roleReplica = "replica"
)

// ClusterNodeState represents the state of a single node in the Redis Cluster
type ClusterNodeState struct {
	PodName      string
	PodIP        string
	NodeID       string
	Slots        []string
	Role         string // "master" or "replica"
	MasterNodeID string // "-" if master
	LinkStatus   string // "up" or "down"
	Reachable    bool
}

// ClusterGroundTruth represents the combined view of the Redis Cluster topology
type ClusterGroundTruth struct {
	Nodes        map[string]*ClusterNodeState // Map PodName -> ClusterNodeState
	Partitions   [][]string                   // Sets of NodeIDs that see each other
	GhostNodes   []string                     // NodeIDs present in cluster but not in K8s
	ClusterState string                       // "ok" if ANY node says ok, else "fail" or "unknown"
	TotalSlots   int                          // Max slots assigned reported by any node
	AllNodeIDs   map[string]bool              // Set of all NodeIDs seen in the mesh
	// KnownNodes maps a reachable node's NodeID to the set of NodeIDs it directly
	// knows in its own CLUSTER NODES view (excluding fail/noaddr/handshake). It is
	// the same adjacency used to compute Partitions, retained so the operator can
	// avoid issuing CLUSTER REPLICATE before gossip has propagated the target
	// master's NodeID to the executing node (which returns ERR Unknown node).
	KnownNodes map[string][]string
}

// NewClusterGroundTruth initializes a new cluster ground truth
func NewClusterGroundTruth() *ClusterGroundTruth {
	return &ClusterGroundTruth{
		Nodes:      make(map[string]*ClusterNodeState),
		AllNodeIDs: make(map[string]bool),
	}
}

func (gt *ClusterGroundTruth) IsHealthy(expectedNodes, expectedShards int32) bool {
	if len(gt.Nodes) < int(expectedNodes) {
		return false
	}
	if len(gt.AllNodeIDs) != int(expectedNodes) {
		return false
	}
	if gt.HasPartitions() {
		return false
	}
	if gt.CountMasters() != int(expectedShards) {
		return false
	}
	// An empty master (a node that is a master with no slots) means a shard is
	// under-replicated and a node is dead weight — not a healthy steady state.
	// Reporting it as healthy lets updateClusterStatus declare Phase=Running and
	// drop to the slow steady requeue cadence, which can stall the empty-master
	// reattach (Step 4) past the e2e topology-sync window. Step 4 always has a
	// reattach target while an empty master exists, so this clause is operator-
	// actionable and cannot deadlock. See RECONCILIATION_ALGORITHM_CHANGELOG.md (LR-014).
	if gt.HasEmptyMasters() {
		return false
	}
	return gt.ClusterState == "ok" && gt.TotalSlots == 16384
}

func (gt *ClusterGroundTruth) HasPartitions() bool {
	return len(gt.Partitions) > 1
}

func (gt *ClusterGroundTruth) HasGhostNodes() bool {
	return len(gt.GhostNodes) > 0
}

func (gt *ClusterGroundTruth) HasOrphanedSlots() bool {
	return gt.TotalSlots < 16384
}

func (gt *ClusterGroundTruth) CountMasters() int {
	count := 0
	for _, n := range gt.Nodes {
		if n.Role == roleMaster && len(n.Slots) > 0 {
			count++
		}
	}
	return count
}

func (gt *ClusterGroundTruth) GetLargestPartitionSeed() *ClusterNodeState {
	if len(gt.Partitions) == 0 {
		for _, n := range gt.Nodes {
			return n
		}
		return nil
	}

	maxIdx := 0
	maxLen := 0
	for i, p := range gt.Partitions {
		if len(p) > maxLen {
			maxLen = len(p)
			maxIdx = i
		}
	}

	targetID := gt.Partitions[maxIdx][0]
	for _, n := range gt.Nodes {
		if n.NodeID == targetID {
			return n
		}
	}
	return nil
}

func (gt *ClusterGroundTruth) GetEmptyMasters() []*ClusterNodeState {
	var empty []*ClusterNodeState
	for _, n := range gt.Nodes {
		if n.Role == roleMaster && len(n.Slots) == 0 {
			empty = append(empty, n)
		}
	}
	return empty
}

// HasEmptyMasters returns true if any node is a master with no slots assigned.
func (gt *ClusterGroundTruth) HasEmptyMasters() bool {
	for _, n := range gt.Nodes {
		if n.Role == roleMaster && len(n.Slots) == 0 {
			return true
		}
	}
	return false
}

// NodeKnows reports whether the node identified by observerID directly knows
// targetID in its own gossip view. Returns false when the observer's view was
// not gathered (e.g. it was unreachable, or KnownNodes was never populated) —
// the safe default for gating CLUSTER REPLICATE, which fails with ERR Unknown
// node if the executing node does not yet know the target.
func (gt *ClusterGroundTruth) NodeKnows(observerID, targetID string) bool {
	return slices.Contains(gt.KnownNodes[observerID], targetID)
}

func (gt *ClusterGroundTruth) GetMastersWithReplicas() map[string][]string {
	m := make(map[string][]string)
	for _, n := range gt.Nodes {
		if n.Role == roleReplica && n.MasterNodeID != "-" {
			m[n.MasterNodeID] = append(m[n.MasterNodeID], n.NodeID)
		}
	}
	return m
}
