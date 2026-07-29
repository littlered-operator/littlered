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
	"context"
	"fmt"
	"strings"

	ctrl "sigs.k8s.io/controller-runtime"

	littleredv1alpha1 "github.com/littlered-operator/littlered-operator/api/v1alpha1"
	redisclient "github.com/littlered-operator/littlered-operator/internal/redis"
)

// reshardConsolidated is the LR-018 repair step (Step 3b). It restores a distinct
// slot-owning master per shard when the cluster has drifted into the consolidated
// state — one master owning more than one shard range while other masters sit empty —
// which no other repair branch can heal (see docs/CLUSTER_CONSOLIDATED_SHARD_RECOVERY.md).
//
// The decision is the pure PlanReshard seam; this method only executes one move per
// reconcile and returns acted=true (with a fast requeue) whenever it drives or waits on
// a migration, so the caller returns immediately. It preserves keys.
//
// Returns (result, acted): acted=true means the caller should return result now.
// Transient failures are logged and turned into a fast requeue rather than a hard error
// (matching the rest of repairCluster), so there is no error return.
func (r *LittleRedReconciler) reshardConsolidated(
	ctx context.Context,
	littleRed *littleredv1alpha1.LittleRed,
	gt *redisclient.ClusterGroundTruth,
	clusterClient *redisclient.ClusterClient,
) (ctrl.Result, bool) {
	fast, _ := littleRed.GetRequeueIntervals()
	stateLog := r.getLogger(ctx, littleRed, LogCategoryState)
	auditLog := r.getLogger(ctx, littleRed, LogCategoryAudit)

	plan := redisclient.PlanReshard(gt, littleRed.Spec.Cluster.Shards)
	if len(plan.Moves) == 0 {
		// Nothing to do, or not actionable (e.g. no empty master yet). Only log the
		// non-trivial deferrals; "one master per shard" is the healthy steady state.
		if plan.Reason != "" && !strings.HasPrefix(plan.Reason, "one master per shard") {
			stateLog.Info("Consolidated-shard reshard deferred", "reason", plan.Reason)
		}
		return ctrl.Result{}, false
	}

	// One move per reconcile; the next gather observes progress and re-plans.
	move := plan.Moves[0]
	destAddr := fmt.Sprintf("%s:%d", move.Dest.PodIP, littleredv1alpha1.RedisPort)

	auditLog.Info("Consolidated-shard reshard: relocating surplus range (LR-018)",
		"range", fmt.Sprintf("%d-%d", move.Start, move.End),
		"source", move.Source.PodName, "dest", move.Dest.PodName,
		"atomicSlotMigration", gt.AtomicSlotMigration)

	if !gt.AtomicSlotMigration {
		// Pre-8.4 / non-ASM engine: the key-preserving MIGRATE-dance executor is a
		// scoped follow-up (LR-018 §7.2). Detection and prevention (Step 3 hardening)
		// are already in effect; we defer rather than block. Logged explicitly so this
		// is never a silent no-op in the field.
		stateLog.Info("Consolidated-shard detected but native atomic slot migration is unavailable "+
			"(pre-8.4 engine); key-preserving reshard-dance not yet implemented — deferring. "+
			"Upgrade to Redis 8.4+ for automatic recovery, or reshard manually.",
			"range", fmt.Sprintf("%d-%d", move.Start, move.End),
			"source", move.Source.PodName, "dest", move.Dest.PodName)
		return ctrl.Result{}, false
	}

	// Native atomic slot migration (Redis 8.4+). Re-entrant: if a task is already in
	// flight on the destination, wait for it (the gather shows completion when the
	// destination owns the range) rather than relaunching IMPORT.
	inFlight, err := clusterClient.ClusterMigrationInFlight(ctx, destAddr)
	if err != nil {
		auditLog.Error(err, "Failed to query atomic slot migration status; will retry", "dest", move.Dest.PodName)
		return ctrl.Result{RequeueAfter: fast}, true
	}
	if inFlight {
		stateLog.Info("Atomic slot migration in progress; waiting", "dest", move.Dest.PodName,
			"range", fmt.Sprintf("%d-%d", move.Start, move.End))
		return ctrl.Result{RequeueAfter: fast}, true
	}

	taskID, err := clusterClient.ClusterMigrationImport(ctx, destAddr, [][2]int{{move.Start, move.End}})
	if err != nil {
		auditLog.Error(err, "Failed to start atomic slot migration; will retry",
			"dest", move.Dest.PodName, "range", fmt.Sprintf("%d-%d", move.Start, move.End))
		return ctrl.Result{RequeueAfter: fast}, true
	}
	auditLog.Info("Started atomic slot migration", "taskID", taskID,
		"dest", move.Dest.PodName, "range", fmt.Sprintf("%d-%d", move.Start, move.End))
	return ctrl.Result{RequeueAfter: fast}, true
}
