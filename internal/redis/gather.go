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
	"sync"
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

// GatherClusterGroundTruth queries all cluster pods to build a view of the cluster topology.
//
// Probes are issued concurrently (see gatherNodeIdentities / gatherTopology): a single
// unreachable IP — e.g. a stale pod IP handed to us by the K8s cache during pod churn —
// must not serialize-block the whole gather, or the reconcile loop cannot heal fast
// enough (LR-012).
func GatherClusterGroundTruth(ctx context.Context, g Gatherer, clusterPods map[string]string) *ClusterGroundTruth {
	gt := NewClusterGroundTruth()

	// 1. Identify every pod (CLUSTER MYID + replication state) and index live nodes.
	nodeIDtoPod := make(map[string]string)
	for _, ns := range gatherNodeIdentities(ctx, g, clusterPods) {
		gt.Nodes[ns.PodName] = ns
		if ns.Reachable {
			nodeIDtoPod[ns.NodeID] = ns.PodName
		}
	}

	// 2. Query topology (CLUSTER INFO + NODES) from all reachable nodes and merge.
	adj := gatherTopology(ctx, g, gt)
	// Retain the per-node adjacency (who each node directly knows) so repair can
	// gate CLUSTER REPLICATE on the empty master actually knowing its target.
	gt.KnownNodes = adj

	// Detect ghosts: NodeIDs seen in the mesh that have no backing pod.
	for nodeID := range gt.AllNodeIDs {
		if _, hasPod := nodeIDtoPod[nodeID]; !hasPod {
			gt.GhostNodes = append(gt.GhostNodes, nodeID)
		}
	}

	if gt.ClusterState == "" {
		gt.ClusterState = "unknown"
	}

	// 3. Compute partitions over the live nodes' adjacency graph.
	gt.Partitions = computePartitions(nodeIDtoPod, adj)

	return gt
}

// gatherNodeIdentities probes every pod concurrently for its CLUSTER MYID and
// replication LinkStatus. Each goroutine writes its own pre-allocated slot, so no
// locking is needed; unreachable pods come back with Reachable=false.
func gatherNodeIdentities(ctx context.Context, g Gatherer, clusterPods map[string]string) []*ClusterNodeState {
	type podProbe struct{ ip, name string }
	probes := make([]podProbe, 0, len(clusterPods))
	for ip, name := range clusterPods {
		probes = append(probes, podProbe{ip: ip, name: name})
	}

	states := make([]*ClusterNodeState, len(probes))
	var wg sync.WaitGroup
	for i := range probes {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			p := probes[i]
			id, err := g.GetClusterID(ctx, p.name, p.ip)
			if err != nil {
				states[i] = &ClusterNodeState{PodName: p.name, PodIP: p.ip, Reachable: false}
				return
			}
			ns := &ClusterNodeState{PodName: p.name, PodIP: p.ip, NodeID: id, Reachable: true}
			// Replication state is only queried for reachable nodes, so it never hits a dead IP.
			if rs, err := g.GetRedisState(ctx, p.name, p.ip); err == nil {
				ns.LinkStatus = rs.LinkStatus
			}
			states[i] = ns
		}(i)
	}
	wg.Wait()
	return states
}

// topoView is one reachable node's contribution to the merged topology.
type topoView struct {
	state         string
	haveState     bool
	slotsAssigned int
	seenIDs       []string // every NodeID seen in CLUSTER NODES (for ghost detection)
	known         []string // non-failed neighbor IDs
	hasNodes      bool     // CLUSTER NODES succeeded → record adjacency
}

// gatherTopology queries CLUSTER INFO + NODES from all reachable nodes concurrently,
// mutates each node's own role/slots, and merges the shared fields (ClusterState,
// TotalSlots, AllNodeIDs) single-threaded after the barrier. Returns the adjacency map.
func gatherTopology(ctx context.Context, g Gatherer, gt *ClusterGroundTruth) map[string][]string {
	reachable := make([]*ClusterNodeState, 0, len(gt.Nodes))
	for _, n := range gt.Nodes {
		if n.Reachable {
			reachable = append(reachable, n)
		}
	}

	views := make([]*topoView, len(reachable))
	var wg sync.WaitGroup
	for i := range reachable {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			views[i] = probeNodeTopology(ctx, g, reachable[i])
		}(i)
	}
	wg.Wait()

	adj := make(map[string][]string)
	for i, v := range views {
		if v == nil {
			continue
		}
		if v.haveState {
			if v.state == "ok" {
				gt.ClusterState = "ok"
			} else if gt.ClusterState == "" || gt.ClusterState == "unknown" {
				gt.ClusterState = v.state
			}
			if v.slotsAssigned > gt.TotalSlots {
				gt.TotalSlots = v.slotsAssigned
			}
		}
		for _, id := range v.seenIDs {
			gt.AllNodeIDs[id] = true
		}
		if v.hasNodes {
			adj[reachable[i].NodeID] = v.known
		}
	}
	return adj
}

// probeNodeTopology queries one node's CLUSTER INFO + NODES and folds its own
// role/slots in place. It mutates only the node it was given, so it is safe to run
// concurrently across distinct nodes.
func probeNodeTopology(ctx context.Context, g Gatherer, n *ClusterNodeState) *topoView {
	v := &topoView{}

	if info, err := g.GetClusterInfo(ctx, n.PodName, n.PodIP); err == nil {
		v.haveState = true
		v.state = info.State
		v.slotsAssigned = info.SlotsAssigned
	}

	nodes, err := g.GetClusterNodes(ctx, n.PodName, n.PodIP)
	if err != nil {
		return v
	}
	v.hasNodes = true

	for _, knownNode := range nodes {
		v.seenIDs = append(v.seenIDs, knownNode.NodeID)

		isFailed := false
		for _, f := range knownNode.Flags {
			if f == "fail" || f == "noaddr" || f == "handshake" {
				isFailed = true
			}
		}
		if !isFailed {
			v.known = append(v.known, knownNode.NodeID)
		}

		if knownNode.NodeID == n.NodeID {
			n.Slots = knownNode.Slots
			n.Role = roleMaster
			if knownNode.IsReplica() {
				n.Role = roleReplica
			}
			n.MasterNodeID = knownNode.MasterID
		}
	}
	return v
}

// computePartitions groups live nodes into connected components over the adjacency
// graph built from each node's CLUSTER NODES view.
func computePartitions(nodeIDtoPod map[string]string, adj map[string][]string) [][]string {
	var partitions [][]string
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
		partitions = append(partitions, partition)
	}
	return partitions
}
