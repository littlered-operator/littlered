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
	"fmt"
	"strconv"
	"strings"

	"github.com/redis/go-redis/v9"
)

// ClusterNodeInfo contains parsed information about a cluster node
type ClusterNodeInfo struct {
	NodeID      string
	Addr        string
	Hostname    string
	Flags       []string
	MasterID    string // "-" if this is a master
	PingSent    int64
	PongRecv    int64
	ConfigEpoch int64
	LinkState   string // "connected" or "disconnected"
	Slots       []string
}

// IsMaster returns true if this node is a master
func (n *ClusterNodeInfo) IsMaster() bool {
	for _, flag := range n.Flags {
		if flag == "master" {
			return true
		}
	}
	return false
}

// IsReplica returns true if this node is a replica
func (n *ClusterNodeInfo) IsReplica() bool {
	for _, flag := range n.Flags {
		if flag == "slave" {
			return true
		}
	}
	return false
}

// ClusterInfo contains parsed CLUSTER INFO output
type ClusterInfo struct {
	State                    string
	SlotsAssigned            int
	SlotsOk                  int
	SlotsPfail               int
	SlotsFail                int
	KnownNodes               int
	Size                     int
	CurrentEpoch             int64
	MyEpoch                  int64
	StatsMessagesSent        int64
	StatsMessagesReceived    int64
	TotalLinksBufferLimitExc int64
}

// ClusterClient wraps Redis cluster operations
type ClusterClient struct {
	password string
}

// NewClusterClient creates a new cluster client
func NewClusterClient(password string) *ClusterClient {
	return &ClusterClient{
		password: password,
	}
}

// getClient creates a redis client for the given address
func (c *ClusterClient) getClient(addr string) *redis.Client {
	return redis.NewClient(&redis.Options{
		Addr:        addr,
		Password:    c.password,
		DialTimeout: DefaultTimeout,
		ReadTimeout: DefaultTimeout,
	})
}

// GetMyID returns the cluster node ID for the node at the given address
func (c *ClusterClient) GetMyID(ctx context.Context, addr string) (string, error) {
	client := c.getClient(addr)
	defer client.Close()

	result, err := client.Do(ctx, "CLUSTER", "MYID").Result()
	if err != nil {
		return "", fmt.Errorf("failed to get node ID: %w", err)
	}

	nodeID, ok := result.(string)
	if !ok {
		return "", fmt.Errorf("unexpected result type for CLUSTER MYID")
	}

	return nodeID, nil
}

// GetClusterNodes returns parsed CLUSTER NODES output
func (c *ClusterClient) GetClusterNodes(ctx context.Context, addr string) ([]ClusterNodeInfo, error) {
	client := c.getClient(addr)
	defer client.Close()

	result, err := client.ClusterNodes(ctx).Result()
	if err != nil {
		return nil, fmt.Errorf("failed to get cluster nodes: %w", err)
	}

	return parseClusterNodes(result), nil
}

// GetClusterInfo returns parsed CLUSTER INFO output
func (c *ClusterClient) GetClusterInfo(ctx context.Context, addr string) (*ClusterInfo, error) {
	client := c.getClient(addr)
	defer client.Close()

	result, err := client.ClusterInfo(ctx).Result()
	if err != nil {
		return nil, fmt.Errorf("failed to get cluster info: %w", err)
	}

	return parseClusterInfo(result), nil
}

// ClusterMeet introduces a new node to the cluster
func (c *ClusterClient) ClusterMeet(ctx context.Context, addr, newHost string, newPort int) error {
	client := c.getClient(addr)
	defer client.Close()

	err := client.ClusterMeet(ctx, newHost, strconv.Itoa(newPort)).Err()
	if err != nil {
		return fmt.Errorf("failed to meet node %s:%d: %w", newHost, newPort, err)
	}

	return nil
}

// ClusterForget removes a node from the cluster's known nodes
func (c *ClusterClient) ClusterForget(ctx context.Context, addr, nodeIDToForget string) error {
	client := c.getClient(addr)
	defer client.Close()

	err := client.ClusterForget(ctx, nodeIDToForget).Err()
	if err != nil {
		return fmt.Errorf("failed to forget node %s: %w", nodeIDToForget, err)
	}

	return nil
}

// ClusterAddSlots assigns slots to the node at the given address
func (c *ClusterClient) ClusterAddSlots(ctx context.Context, addr string, slots ...int) error {
	client := c.getClient(addr)
	defer client.Close()

	err := client.ClusterAddSlots(ctx, slots...).Err()
	if err != nil {
		return fmt.Errorf("failed to add slots: %w", err)
	}

	return nil
}

// ClusterReplicate makes the node at replicaAddr a replica of the given master
func (c *ClusterClient) ClusterReplicate(ctx context.Context, replicaAddr, masterNodeID string) error {
	client := c.getClient(replicaAddr)
	defer client.Close()

	err := client.ClusterReplicate(ctx, masterNodeID).Err()
	if err != nil {
		return fmt.Errorf("failed to replicate master %s: %w", masterNodeID, err)
	}

	return nil
}

// ClusterResetSoft performs a soft reset of the cluster node
func (c *ClusterClient) ClusterResetSoft(ctx context.Context, addr string) error {
	client := c.getClient(addr)
	defer client.Close()

	err := client.Do(ctx, "CLUSTER", "RESET", "SOFT").Err()
	if err != nil {
		return fmt.Errorf("failed to reset node: %w", err)
	}

	return nil
}

// ClusterFailover initiates a manual failover
func (c *ClusterClient) ClusterFailover(ctx context.Context, addr string) error {
	client := c.getClient(addr)
	defer client.Close()

	err := client.ClusterFailover(ctx).Err()
	if err != nil {
		return fmt.Errorf("failed to initiate failover: %w", err)
	}

	return nil
}

// parseClusterNodes parses the output of CLUSTER NODES command
// Format: <id> <ip:port@cport,hostname> <flags> <master> <ping-sent> <pong-recv> <config-epoch> <link-state> <slot> ...
func parseClusterNodes(output string) []ClusterNodeInfo {
	var nodes []ClusterNodeInfo
	lines := strings.Split(strings.TrimSpace(output), "\n")

	for _, line := range lines {
		if line == "" {
			continue
		}

		parts := strings.Fields(line)
		if len(parts) < 8 {
			continue
		}

		node := ClusterNodeInfo{
			NodeID:   parts[0],
			MasterID: parts[3],
		}

		// Parse address (ip:port@cport,hostname or ip:port@cport)
		addrParts := strings.Split(parts[1], ",")
		if len(addrParts) >= 2 {
			node.Hostname = addrParts[1]
		}
		// Get ip:port (strip @cport if present)
		ipPort := addrParts[0]
		if atIdx := strings.Index(ipPort, "@"); atIdx != -1 {
			ipPort = ipPort[:atIdx]
		}
		node.Addr = ipPort

		// Parse flags
		node.Flags = strings.Split(parts[2], ",")

		// Parse ping/pong times
		node.PingSent, _ = strconv.ParseInt(parts[4], 10, 64)
		node.PongRecv, _ = strconv.ParseInt(parts[5], 10, 64)

		// Parse config epoch
		node.ConfigEpoch, _ = strconv.ParseInt(parts[6], 10, 64)

		// Parse link state
		node.LinkState = parts[7]

		// Parse slots (remaining parts)
		if len(parts) > 8 {
			node.Slots = parts[8:]
		}

		nodes = append(nodes, node)
	}

	return nodes
}

// parseClusterInfo parses the output of CLUSTER INFO command
func parseClusterInfo(output string) *ClusterInfo {
	info := &ClusterInfo{}
	lines := strings.Split(strings.TrimSpace(output), "\n")

	for _, line := range lines {
		line = strings.TrimSpace(line)
		parts := strings.SplitN(line, ":", 2)
		if len(parts) != 2 {
			continue
		}

		key := parts[0]
		value := strings.TrimSpace(parts[1])

		switch key {
		case "cluster_state":
			info.State = value
		case "cluster_slots_assigned":
			info.SlotsAssigned, _ = strconv.Atoi(value)
		case "cluster_slots_ok":
			info.SlotsOk, _ = strconv.Atoi(value)
		case "cluster_slots_pfail":
			info.SlotsPfail, _ = strconv.Atoi(value)
		case "cluster_slots_fail":
			info.SlotsFail, _ = strconv.Atoi(value)
		case "cluster_known_nodes":
			info.KnownNodes, _ = strconv.Atoi(value)
		case "cluster_size":
			info.Size, _ = strconv.Atoi(value)
		case "cluster_current_epoch":
			info.CurrentEpoch, _ = strconv.ParseInt(value, 10, 64)
		case "cluster_my_epoch":
			info.MyEpoch, _ = strconv.ParseInt(value, 10, 64)
		case "cluster_stats_messages_sent":
			info.StatsMessagesSent, _ = strconv.ParseInt(value, 10, 64)
		case "cluster_stats_messages_received":
			info.StatsMessagesReceived, _ = strconv.ParseInt(value, 10, 64)
		case "total_cluster_links_buffer_limit_exceeded":
			info.TotalLinksBufferLimitExc, _ = strconv.ParseInt(value, 10, 64)
		}
	}

	return info
}

// GenerateSlotRanges generates slot ranges for the given number of shards
// Redis Cluster has 16384 slots (0-16383)
func GenerateSlotRanges(shards int) []struct {
	Start int
	End   int
} {
	if shards <= 0 {
		return nil
	}

	const totalSlots = 16384
	slotsPerShard := totalSlots / shards
	remainder := totalSlots % shards

	ranges := make([]struct {
		Start int
		End   int
	}, shards)

	start := 0
	for i := 0; i < shards; i++ {
		count := slotsPerShard
		if i < remainder {
			count++ // Distribute remainder slots
		}
		ranges[i].Start = start
		ranges[i].End = start + count - 1
		start += count
	}

	return ranges
}

// FormatSlotRange formats a slot range as a string (e.g., "0-5460")
func FormatSlotRange(start, end int) string {
	if start == end {
		return strconv.Itoa(start)
	}
	return fmt.Sprintf("%d-%d", start, end)
}

// ParseSlotRange parses a slot range string (e.g., "0-5460") into start and end
func ParseSlotRange(s string) (start, end int, err error) {
	parts := strings.Split(s, "-")
	if len(parts) == 1 {
		start, err = strconv.Atoi(parts[0])
		if err != nil {
			return 0, 0, err
		}
		return start, start, nil
	}
	if len(parts) != 2 {
		return 0, 0, fmt.Errorf("invalid slot range: %s", s)
	}

	start, err = strconv.Atoi(parts[0])
	if err != nil {
		return 0, 0, err
	}
	end, err = strconv.Atoi(parts[1])
	if err != nil {
		return 0, 0, err
	}

	return start, end, nil
}

// ExpandSlotRange expands a slot range string into individual slot numbers
func ExpandSlotRange(s string) ([]int, error) {
	start, end, err := ParseSlotRange(s)
	if err != nil {
		return nil, err
	}

	slots := make([]int, end-start+1)
	for i := start; i <= end; i++ {
		slots[i-start] = i
	}
	return slots, nil
}
