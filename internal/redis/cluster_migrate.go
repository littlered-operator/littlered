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
	"fmt"
	"strconv"
	"strings"
)

// This file holds the key-preserving slot-migration primitives used by the LR-018
// reshard executor. Two mechanisms:
//
//   - Baseline (pre-8.4, always available): the classic redis-cli --cluster reshard
//     dance — SETSLOT IMPORTING/MIGRATING, GETKEYSINSLOT + MIGRATE key batches, then
//     SETSLOT NODE. Works on every Redis/Valkey that supports Redis Cluster.
//   - Native (Redis 8.4+): CLUSTER MIGRATION IMPORT — one atomic, server-managed task.
//
// Completion is primarily observed via the normal gather (the destination owning the
// moved range shows up in CLUSTER NODES). These calls only drive/inspect the migration.
// See docs/CLUSTER_CONSOLIDATED_SHARD_RECOVERY.md §7.2.

// -----------------------------------------------------------------------------
// Baseline dance
// -----------------------------------------------------------------------------

// ClusterSetSlotImporting marks slot as importing from sourceNodeID. Run on the
// destination (importing) node.
func (c *ClusterClient) ClusterSetSlotImporting(ctx context.Context, addr string, slot int, sourceNodeID string) error {
	client := c.getClient(addr)
	defer func() { _ = client.Close() }()
	if err := client.Do(ctx, "CLUSTER", "SETSLOT", slot, "IMPORTING", sourceNodeID).Err(); err != nil {
		return fmt.Errorf("SETSLOT %d IMPORTING %s: %w", slot, sourceNodeID, err)
	}
	return nil
}

// ClusterSetSlotMigrating marks slot as migrating to destNodeID. Run on the source
// (migrating) node.
func (c *ClusterClient) ClusterSetSlotMigrating(ctx context.Context, addr string, slot int, destNodeID string) error {
	client := c.getClient(addr)
	defer func() { _ = client.Close() }()
	if err := client.Do(ctx, "CLUSTER", "SETSLOT", slot, "MIGRATING", destNodeID).Err(); err != nil {
		return fmt.Errorf("SETSLOT %d MIGRATING %s: %w", slot, destNodeID, err)
	}
	return nil
}

// ClusterSetSlotNode assigns definitive ownership of slot to ownerNodeID. Broadcast to
// every master so gossip converges promptly; issue on source and destination last.
func (c *ClusterClient) ClusterSetSlotNode(ctx context.Context, addr string, slot int, ownerNodeID string) error {
	client := c.getClient(addr)
	defer func() { _ = client.Close() }()
	if err := client.Do(ctx, "CLUSTER", "SETSLOT", slot, "NODE", ownerNodeID).Err(); err != nil {
		return fmt.Errorf("SETSLOT %d NODE %s: %w", slot, ownerNodeID, err)
	}
	return nil
}

// ClusterSetSlotStable clears any importing/migrating state on slot. Used to recover a
// slot left mid-dance by an interrupted reconcile before restarting the migration.
func (c *ClusterClient) ClusterSetSlotStable(ctx context.Context, addr string, slot int) error {
	client := c.getClient(addr)
	defer func() { _ = client.Close() }()
	if err := client.Do(ctx, "CLUSTER", "SETSLOT", slot, "STABLE").Err(); err != nil {
		return fmt.Errorf("SETSLOT %d STABLE: %w", slot, err)
	}
	return nil
}

// ClusterCountKeysInSlot returns the number of keys currently in slot on the node.
func (c *ClusterClient) ClusterCountKeysInSlot(ctx context.Context, addr string, slot int) (int, error) {
	client := c.getClient(addr)
	defer func() { _ = client.Close() }()
	n, err := client.ClusterCountKeysInSlot(ctx, slot).Result()
	if err != nil {
		return 0, fmt.Errorf("COUNTKEYSINSLOT %d: %w", slot, err)
	}
	return int(n), nil
}

// ClusterGetKeysInSlot returns up to count keys currently stored in slot on the node.
func (c *ClusterClient) ClusterGetKeysInSlot(ctx context.Context, addr string, slot, count int) ([]string, error) {
	client := c.getClient(addr)
	defer func() { _ = client.Close() }()
	keys, err := client.ClusterGetKeysInSlot(ctx, slot, count).Result()
	if err != nil {
		return nil, fmt.Errorf("GETKEYSINSLOT %d: %w", slot, err)
	}
	return keys, nil
}

// MigrateKeys moves the given keys from the source node (addr) to the destination at
// destHost:destPort, preserving values. Uses REPLACE (idempotent on retry) and AUTH
// when a password is configured. timeoutMS bounds the per-call transfer.
func (c *ClusterClient) MigrateKeys(ctx context.Context, addr, destHost string, destPort, timeoutMS int, keys ...string) error {
	if len(keys) == 0 {
		return nil
	}
	client := c.getClient(addr)
	defer func() { _ = client.Close() }()

	// MIGRATE host port "" destination-db timeout [REPLACE] [AUTH password] KEYS k...
	args := []any{"MIGRATE", destHost, destPort, "", 0, timeoutMS, "REPLACE"}
	if c.password != "" {
		args = append(args, "AUTH", c.password)
	}
	args = append(args, "KEYS")
	for _, k := range keys {
		args = append(args, k)
	}
	if err := client.Do(ctx, args...).Err(); err != nil {
		return fmt.Errorf("MIGRATE %d key(s) to %s:%d: %w", len(keys), destHost, destPort, err)
	}
	return nil
}

// -----------------------------------------------------------------------------
// Native atomic slot migration (Redis 8.4+)
// -----------------------------------------------------------------------------

// MigrationTask is a parsed CLUSTER MIGRATION STATUS entry (the fields the operator
// acts on). State is e.g. "in_progress" or "completed"; LastError is empty on success.
type MigrationTask struct {
	ID        string
	State     string
	LastError string
}

// ClusterMigrationImport starts a native atomic slot migration of the given inclusive
// ranges, pulling them onto the destination master (destAddr). Returns the task ID.
// Redis 8.4+ only; on older engines this errors (unknown subcommand) and the caller
// falls back to the baseline dance.
func (c *ClusterClient) ClusterMigrationImport(ctx context.Context, destAddr string, ranges [][2]int) (string, error) {
	client := c.getClient(destAddr)
	defer func() { _ = client.Close() }()

	args := []any{"CLUSTER", "MIGRATION", "IMPORT"}
	for _, r := range ranges {
		args = append(args, r[0], r[1])
	}
	id, err := client.Do(ctx, args...).Text()
	if err != nil {
		return "", fmt.Errorf("CLUSTER MIGRATION IMPORT: %w", err)
	}
	return id, nil
}

// ClusterMigrationStatusAll returns all current/completed migration tasks reported by
// the node. Parsing is lenient (see parseMigrationTasks) to tolerate RESP2/RESP3 shapes.
func (c *ClusterClient) ClusterMigrationStatusAll(ctx context.Context, addr string) ([]MigrationTask, error) {
	client := c.getClient(addr)
	defer func() { _ = client.Close() }()
	reply, err := client.Do(ctx, "CLUSTER", "MIGRATION", "STATUS", "ALL").Result()
	if err != nil {
		return nil, fmt.Errorf("CLUSTER MIGRATION STATUS ALL: %w", err)
	}
	return parseMigrationTasks(reply), nil
}

// ClusterMigrationInFlight reports whether the node has any migration task not yet in a
// terminal state. Used for re-entrancy: don't relaunch an IMPORT while one is running.
func (c *ClusterClient) ClusterMigrationInFlight(ctx context.Context, addr string) (bool, error) {
	tasks, err := c.ClusterMigrationStatusAll(ctx, addr)
	if err != nil {
		return false, err
	}
	for _, t := range tasks {
		if !migrationTerminal(t.State) {
			return true, nil
		}
	}
	return false, nil
}

func migrationTerminal(state string) bool {
	switch strings.ToLower(strings.TrimSpace(state)) {
	case "completed", "cancelled", "canceled", "failed", "":
		return true
	default:
		return false
	}
}

// parseMigrationTasks leniently flattens a CLUSTER MIGRATION STATUS reply into tasks.
// The reply is an array of tasks, each a list (RESP2) or map (RESP3) of field/value
// pairs. We only extract id/state/last_error and ignore the rest.
func parseMigrationTasks(reply any) []MigrationTask {
	items, ok := reply.([]any)
	if !ok {
		return nil
	}
	tasks := make([]MigrationTask, 0, len(items))
	for _, it := range items {
		fields := flattenToStringMap(it)
		if len(fields) == 0 {
			continue
		}
		tasks = append(tasks, MigrationTask{
			ID:        fields["id"],
			State:     fields["state"],
			LastError: fields["last_error"],
		})
	}
	return tasks
}

// flattenToStringMap turns a single task reply — either a flat []any of
// alternating key/value, or a map[any]any (RESP3) — into a string map.
func flattenToStringMap(v any) map[string]string {
	out := make(map[string]string)
	switch t := v.(type) {
	case []any:
		for i := 0; i+1 < len(t); i += 2 {
			out[toString(t[i])] = toString(t[i+1])
		}
	case map[any]any:
		for k, val := range t {
			out[toString(k)] = toString(val)
		}
	}
	return out
}

func toString(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case []byte:
		return string(t)
	case int64:
		return strconv.FormatInt(t, 10)
	case fmt.Stringer:
		return t.String()
	default:
		if v == nil {
			return ""
		}
		return fmt.Sprintf("%v", v)
	}
}
