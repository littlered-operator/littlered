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
	appsv1 "k8s.io/api/apps/v1"
)

// clusterShardRolloutSettled reports whether a shard StatefulSet has fully converged on its
// desired pod template: the controller has observed the latest spec (ObservedGeneration ==
// Generation), no rollout is in progress (UpdateRevision == CurrentRevision), and every
// replica is updated and ready. The operator uses this to serialize rollouts across shards
// (LR-021): it only rolls the next shard once the current one is settled, so an
// operator-driven template change never restarts more than one shard's pods at a time —
// restoring the global one-pod-at-a-time serialization the single pre-0.3.0 StatefulSet gave
// for free.
func clusterShardRolloutSettled(sts *appsv1.StatefulSet) bool {
	if sts == nil || sts.Spec.Replicas == nil {
		return false
	}
	want := *sts.Spec.Replicas
	st := sts.Status
	return st.ObservedGeneration == sts.Generation &&
		st.UpdateRevision != "" &&
		st.UpdateRevision == st.CurrentRevision &&
		st.UpdatedReplicas == want &&
		st.ReadyReplicas == want &&
		st.Replicas == want
}
