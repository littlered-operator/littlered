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
	"regexp"
	"sort"
	"strconv"
)

// shardPodNameRE matches a per-shard cluster pod name {instance}-shard-<K>-<O>,
// capturing the shard index K.
var shardPodNameRE = regexp.MustCompile(`-shard-(\d+)-\d+$`)

// ShardIndexFromPodName extracts the shard index K from a per-shard cluster pod name
// {instance}-shard-K-O (0.3.0+). Returns -1 if the name is not in per-shard form (e.g. a
// pre-0.3.0 {instance}-cluster-N name), for which the shard-colocation invariant is N/A.
func ShardIndexFromPodName(podName string) int {
	m := shardPodNameRE.FindStringSubmatch(podName)
	if m == nil {
		return -1
	}
	k, err := strconv.Atoi(m[1])
	if err != nil {
		return -1
	}
	return k
}

// ShardColocationViolation records a replica whose master lives in a different shard
// StatefulSet. It is the symptom of Redis shards decoupling from shard StatefulSets, which
// defeats per-shard failure-domain placement (ADR-007): the operator pins each shard inside
// one StatefulSet (shard-aware reattach + disabled replica migration), so any violation
// means that invariant has been broken.
type ShardColocationViolation struct {
	ReplicaPod   string
	ReplicaShard int
	MasterPod    string
	MasterShard  int
}

// CheckShardColocation verifies that each Redis shard (a master and its replicas) lives
// inside a single shard StatefulSet: every replica's master must be a pod in the same shard
// (…-shard-K-…). It returns the violations in deterministic (replica pod name) order; an
// empty result means the invariant holds. Replicas whose master is not among the gathered
// nodes (ghost/unreachable), and nodes whose names are not in per-shard form (legacy), are
// skipped — there is nothing to assert for them.
func (gt *ClusterGroundTruth) CheckShardColocation() []ShardColocationViolation {
	byID := make(map[string]*ClusterNodeState, len(gt.Nodes))
	for _, n := range gt.Nodes {
		byID[n.NodeID] = n
	}

	var violations []ShardColocationViolation
	for _, n := range gt.Nodes {
		if n.Role != roleReplica {
			continue
		}
		m, ok := byID[n.MasterNodeID]
		if !ok {
			continue // master not gathered (ghost/unreachable) — not judged here
		}
		rShard := ShardIndexFromPodName(n.PodName)
		mShard := ShardIndexFromPodName(m.PodName)
		if rShard < 0 || mShard < 0 {
			continue // legacy / non-per-shard naming — invariant N/A
		}
		if rShard != mShard {
			violations = append(violations, ShardColocationViolation{
				ReplicaPod:   n.PodName,
				ReplicaShard: rShard,
				MasterPod:    m.PodName,
				MasterShard:  mShard,
			})
		}
	}

	sort.Slice(violations, func(i, j int) bool { return violations[i].ReplicaPod < violations[j].ReplicaPod })
	return violations
}
