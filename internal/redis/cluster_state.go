/*
Copyright 2026.

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

const roleMaster = "master"

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

func (gt *ClusterGroundTruth) GetMastersWithReplicas() map[string][]string {
	m := make(map[string][]string)
	for _, n := range gt.Nodes {
		if n.Role == "replica" && n.MasterNodeID != "-" {
			m[n.MasterNodeID] = append(m[n.MasterNodeID], n.NodeID)
		}
	}
	return m
}
