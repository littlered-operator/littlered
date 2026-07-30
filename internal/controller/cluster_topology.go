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

package controller

import (
	"fmt"
	"strconv"

	littleredv1alpha1 "github.com/littlered-operator/littlered-operator/api/v1alpha1"
)

// ClusterPodRef identifies one pod in a per-shard cluster topology: which shard it
// belongs to, its ordinal within that shard's StatefulSet, and whether it is that
// shard's master. In the per-shard model (0.3.0) each shard is its own StatefulSet
// {name}-shard-K with pods {name}-shard-K-0..R; the pod at ordinal 0 is the shard's
// master and 1..R are its replicas. This replaces the pre-0.3.0 striped
// pod-index->shard model (pod N was shard N's master, replicas mapped via
// (i-shards)%shards over one flat StatefulSet).
type ClusterPodRef struct {
	// Name is the stable pod name, e.g. my-cache-shard-0-1.
	Name string
	// ShardIdx is the shard this pod belongs to (0..shards-1).
	ShardIdx int
	// Ordinal is the pod's ordinal within its shard StatefulSet (0..replicasPerShard).
	Ordinal int
	// IsMaster is true iff this pod is its shard's intended master (Ordinal == 0).
	IsMaster bool
}

// clusterShardStatefulSetName returns the StatefulSet name for shard shardIdx.
func clusterShardStatefulSetName(lr *littleredv1alpha1.LittleRed, shardIdx int) string {
	return fmt.Sprintf("%s-shard-%d", lr.Name, shardIdx)
}

// clusterShardPodName returns the pod name for the given ordinal within shard shardIdx.
func clusterShardPodName(name string, shardIdx, ordinal int) string {
	return fmt.Sprintf("%s-shard-%d-%d", name, shardIdx, ordinal)
}

// shardMasterPodName returns shard shardIdx's intended master pod name (ordinal 0).
func shardMasterPodName(name string, shardIdx int) string {
	return clusterShardPodName(name, shardIdx, 0)
}

// clusterShardPDBName returns the PodDisruptionBudget name for shard shardIdx.
func clusterShardPDBName(lr *littleredv1alpha1.LittleRed, shardIdx int) string {
	return fmt.Sprintf("%s-shard-%d-pdb", lr.Name, shardIdx)
}

// clusterShardLabelValue is the value stamped on the per-shard identity label.
func clusterShardLabelValue(shardIdx int) string {
	return strconv.Itoa(shardIdx)
}

// clusterReplicasPerShard returns the configured replicas per shard, treating a nil
// spec as 0 (matching ClusterSpec.GetTotalNodes semantics).
func clusterReplicasPerShard(cluster *littleredv1alpha1.ClusterSpec) int {
	if cluster == nil || cluster.ReplicasPerShard == nil {
		return 0
	}
	return *cluster.ReplicasPerShard
}

// ClusterPodRefs enumerates every pod of a per-shard cluster in a stable order:
// shard 0's pods (ordinal 0..replicasPerShard), then shard 1's, and so on. It is the
// single source of truth for "which pods exist, which shard each belongs to, and which
// is its shard's master," replacing the ad-hoc {name}-cluster-N ordinal loops.
func ClusterPodRefs(name string, shards, replicasPerShard int) []ClusterPodRef {
	refs := make([]ClusterPodRef, 0, shards*(1+replicasPerShard))
	for shardIdx := range shards {
		for ordinal := 0; ordinal <= replicasPerShard; ordinal++ {
			refs = append(refs, ClusterPodRef{
				Name:     clusterShardPodName(name, shardIdx, ordinal),
				ShardIdx: shardIdx,
				Ordinal:  ordinal,
				IsMaster: ordinal == 0,
			})
		}
	}
	return refs
}
