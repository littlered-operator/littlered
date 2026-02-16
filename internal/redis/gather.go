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

import (
	"context"
)

// Gatherer is an interface for collecting state from Redis and Sentinel nodes.
// This allows the Operator (direct TCP) and the CLI (K8s Exec) to share logic.
type Gatherer interface {
	GetRedisState(ctx context.Context, podName, ip string) (*RedisNodeState, error)
	GetSentinelState(ctx context.Context, podName, ip string) (*SentinelNodeState, error)

	// Cluster mode
	GetClusterID(ctx context.Context, podName, ip string) (string, error)
	GetClusterInfo(ctx context.Context, podName, ip string) (*ClusterInfo, error)
	GetClusterNodes(ctx context.Context, podName, ip string) ([]ClusterNodeInfo, error)
}

// GatherClusterState uses a Gatherer to populate a SentinelClusterState
func GatherClusterState(ctx context.Context, g Gatherer, redisPods, sentinelPods map[string]string) *SentinelClusterState {
	state := NewSentinelClusterState()

	for ip, name := range redisPods {
		state.ValidIPs[ip] = true
		if rs, err := g.GetRedisState(ctx, name, ip); err == nil {
			state.RedisNodes[ip] = rs
		} else {
			state.RedisNodes[ip] = &RedisNodeState{PodName: name, IP: ip, Reachable: false}
		}
	}

	for ip, name := range sentinelPods {
		state.ValidIPs[ip] = true
		if ss, err := g.GetSentinelState(ctx, name, ip); err == nil {
			state.SentinelNodes[ip] = ss
		} else {
			state.SentinelNodes[ip] = &SentinelNodeState{PodName: name, IP: ip, Reachable: false}
		}
	}

	state.DetermineRealMaster()
	return state
}

// GatherClusterGroundTruth queries all cluster pods to build a view of the cluster topology
func GatherClusterGroundTruth(ctx context.Context, g Gatherer, clusterPods map[string]string) *ClusterGroundTruth {
	gt := NewClusterGroundTruth()
	nodeIDtoPod := make(map[string]string)

	// 1. Gather Pod Identities (IP + MyID) and Redis replication state
	for ip, name := range clusterPods {
		id, err := g.GetClusterID(ctx, name, ip)
		if err != nil {
			gt.Nodes[name] = &ClusterNodeState{PodName: name, PodIP: ip, Reachable: false}
			continue
		}

		nodeState := &ClusterNodeState{
			PodName:   name,
			PodIP:     ip,
			NodeID:    id,
			Reachable: true,
		}

		// Also get Redis replication state for LinkStatus (symmetry with Sentinel)
		if rs, err := g.GetRedisState(ctx, name, ip); err == nil {
			nodeState.LinkStatus = rs.LinkStatus
		}

		gt.Nodes[name] = nodeState
		nodeIDtoPod[id] = name
	}

	// 2. Query Topology from ALL reachable nodes
	adj := make(map[string][]string)

	for _, n := range gt.Nodes {
		if !n.Reachable {
			continue
		}

		info, err := g.GetClusterInfo(ctx, n.PodName, n.PodIP)
		if err == nil {
			if info.State == "ok" {
				gt.ClusterState = "ok"
			} else if gt.ClusterState == "" || gt.ClusterState == "unknown" {
				gt.ClusterState = info.State
			}
			if info.SlotsAssigned > gt.TotalSlots {
				gt.TotalSlots = info.SlotsAssigned
			}
		}

		nodes, err := g.GetClusterNodes(ctx, n.PodName, n.PodIP)
		if err != nil {
			continue
		}

		var known []string
		for _, knownNode := range nodes {
			gt.AllNodeIDs[knownNode.NodeID] = true

			isFailed := false
			for _, f := range knownNode.Flags {
				if f == "fail" || f == "noaddr" || f == "handshake" {
					isFailed = true
				}
			}

			if !isFailed {
				known = append(known, knownNode.NodeID)
			}

			if knownNode.NodeID == n.NodeID {
				n.Slots = knownNode.Slots
				n.Role = "master"
				if knownNode.IsReplica() {
					n.Role = "replica"
				}
				n.MasterNodeID = knownNode.MasterID
			}
		}
		adj[n.NodeID] = known
	}

	// Detect ghosts
	for nodeID := range gt.AllNodeIDs {
		if _, hasPod := nodeIDtoPod[nodeID]; !hasPod {
			gt.GhostNodes = append(gt.GhostNodes, nodeID)
		}
	}

	if gt.ClusterState == "" {
		gt.ClusterState = "unknown"
	}

	// 3. Compute Partitions
	visited := make(map[string]bool)
	for id := range nodeIDtoPod {
		if visited[id] {
			continue
		}

		var partition []string
		queue := []string{id}
		visited[id] = true

		for len(queue) > 0 {
			curr := queue[0]
			queue = queue[1:]
			partition = append(partition, curr)

			for _, neighbor := range adj[curr] {
				if _, valid := nodeIDtoPod[neighbor]; valid && !visited[neighbor] {
					visited[neighbor] = true
					queue = append(queue, neighbor)
				}
			}
		}
		gt.Partitions = append(gt.Partitions, partition)
	}

	return gt
}
