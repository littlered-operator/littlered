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
	"reflect"
	"testing"
)

func TestClusterPodRefs(t *testing.T) {
	tests := []struct {
		name             string
		instance         string
		shards           int
		replicasPerShard int
		want             []ClusterPodRef
	}{
		{
			name:             "3 shards x 1 replica",
			instance:         "my-cache",
			shards:           3,
			replicasPerShard: 1,
			want: []ClusterPodRef{
				{Name: "my-cache-shard-0-0", ShardIdx: 0, Ordinal: 0, IsMaster: true},
				{Name: "my-cache-shard-0-1", ShardIdx: 0, Ordinal: 1, IsMaster: false},
				{Name: "my-cache-shard-1-0", ShardIdx: 1, Ordinal: 0, IsMaster: true},
				{Name: "my-cache-shard-1-1", ShardIdx: 1, Ordinal: 1, IsMaster: false},
				{Name: "my-cache-shard-2-0", ShardIdx: 2, Ordinal: 0, IsMaster: true},
				{Name: "my-cache-shard-2-1", ShardIdx: 2, Ordinal: 1, IsMaster: false},
			},
		},
		{
			name:             "3 shards x 0 replicas (master only)",
			instance:         "c",
			shards:           3,
			replicasPerShard: 0,
			want: []ClusterPodRef{
				{Name: "c-shard-0-0", ShardIdx: 0, Ordinal: 0, IsMaster: true},
				{Name: "c-shard-1-0", ShardIdx: 1, Ordinal: 0, IsMaster: true},
				{Name: "c-shard-2-0", ShardIdx: 2, Ordinal: 0, IsMaster: true},
			},
		},
		{
			name:             "3 shards x 2 replicas",
			instance:         "big",
			shards:           3,
			replicasPerShard: 2,
			want: []ClusterPodRef{
				{Name: "big-shard-0-0", ShardIdx: 0, Ordinal: 0, IsMaster: true},
				{Name: "big-shard-0-1", ShardIdx: 0, Ordinal: 1, IsMaster: false},
				{Name: "big-shard-0-2", ShardIdx: 0, Ordinal: 2, IsMaster: false},
				{Name: "big-shard-1-0", ShardIdx: 1, Ordinal: 0, IsMaster: true},
				{Name: "big-shard-1-1", ShardIdx: 1, Ordinal: 1, IsMaster: false},
				{Name: "big-shard-1-2", ShardIdx: 1, Ordinal: 2, IsMaster: false},
				{Name: "big-shard-2-0", ShardIdx: 2, Ordinal: 0, IsMaster: true},
				{Name: "big-shard-2-1", ShardIdx: 2, Ordinal: 1, IsMaster: false},
				{Name: "big-shard-2-2", ShardIdx: 2, Ordinal: 2, IsMaster: false},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ClusterPodRefs(tt.instance, tt.shards, tt.replicasPerShard)
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("ClusterPodRefs(%q, %d, %d):\n got: %#v\nwant: %#v",
					tt.instance, tt.shards, tt.replicasPerShard, got, tt.want)
			}
		})
	}
}

// TestShardMasterPodName pins the master-identity convention that the bootstrap and
// missing-shard-recovery paths depend on: shard K's master is always {name}-shard-K-0.
func TestShardMasterPodName(t *testing.T) {
	cases := map[int]string{
		0: "my-cache-shard-0-0",
		1: "my-cache-shard-1-0",
		2: "my-cache-shard-2-0",
	}
	for shardIdx, want := range cases {
		if got := shardMasterPodName("my-cache", shardIdx); got != want {
			t.Errorf("shardMasterPodName(my-cache, %d) = %q, want %q", shardIdx, got, want)
		}
	}
}
