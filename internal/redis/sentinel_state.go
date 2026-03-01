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
	"strings"
)

// RedisNodeState represents the replication state of a Redis/Valkey pod
type RedisNodeState struct {
	PodName    string
	IP         string
	Role       string
	MasterHost string
	LinkStatus string
	Offset     int64
	Reachable  bool
}

// SentinelNodeState represents the monitoring state of a Sentinel pod
type SentinelNodeState struct {
	PodName        string
	IP             string
	Monitoring     bool
	MasterIP       string
	FailoverStatus string
	Reachable      bool
	Replicas       []ReplicaInfo
}

// SentinelClusterState represents the combined "Ground Truth" of the entire cluster
type SentinelClusterState struct {
	RedisNodes    map[string]*RedisNodeState
	SentinelNodes map[string]*SentinelNodeState
	ValidIPs      map[string]bool

	// Derived Truth
	RealMasterIP   string
	FailoverActive bool
}

// NewSentinelClusterState initializes a new cluster state
func NewSentinelClusterState() *SentinelClusterState {
	return &SentinelClusterState{
		RedisNodes:    make(map[string]*RedisNodeState),
		SentinelNodes: make(map[string]*SentinelNodeState),
		ValidIPs:      make(map[string]bool),
	}
}

// DetermineRealMaster uses the gathered information to decide who the authoritative master is.
func (s *SentinelClusterState) DetermineRealMaster() {
	// 1. Check for active failover
	for _, sn := range s.SentinelNodes {
		if sn.Reachable && sn.Monitoring && sn.FailoverStatus != "" &&
			sn.FailoverStatus != "none" && sn.FailoverStatus != "no-failover" {
			s.FailoverActive = true
			break
		}
	}

	// 2. Count what Sentinels think
	masterCounts := make(map[string]int)
	reachableSentinels := 0
	for _, sn := range s.SentinelNodes {
		if sn.Reachable {
			reachableSentinels++
			if sn.Monitoring && sn.MasterIP != "" {
				masterCounts[sn.MasterIP]++
			}
		}
	}

	// 3. Majority of Sentinels wins (if IP is still valid)
	ghostMasterCount := 0
	for ip, count := range masterCounts {
		if s.IsGhost(ip) {
			ghostMasterCount += count
		}
		if count >= (reachableSentinels/2)+1 && s.ValidIPs[ip] {
			s.RealMasterIP = ip
			return
		}
	}

	// 4. If Sentinels are idle/split, fallback to identifying the one Redis master.
	// Safety: We ONLY fallback to the Redis-only view if Sentinels are NOT
	// unanimous (majority) about a ghost master. If a majority of Sentinels
	// see a master but that IP is a ghost, it strongly implies a recent pod
	// death and we MUST wait for Sentinel's down-after-milliseconds timeout
	// and subsequent failover. Falling back here would cause us to identify
	// a "stale" or "default" master (like a restarting pod) and potentially
	// issue RESETs that wipe Sentinel's failover state.
	if !s.FailoverActive && ghostMasterCount < (reachableSentinels/2)+1 {
		for _, rn := range s.RedisNodes {
			if rn.Reachable && rn.Role == roleMaster {
				s.RealMasterIP = rn.IP
				return
			}
		}
	}
}

// IsGhost returns true if the given IP is not in the set of valid pod IPs
func (s *SentinelClusterState) IsGhost(ip string) bool {
	if ip == "" {
		return false
	}
	return !s.ValidIPs[ip]
}

// GetHealActions returns a list of recommended actions to fix the cluster state
func (s *SentinelClusterState) GetHealActions() []string {
	var actions []string
	if s.RealMasterIP == "" {
		return actions
	}

	for _, sn := range s.SentinelNodes {
		if sn.Reachable && (!sn.Monitoring || sn.MasterIP != s.RealMasterIP) {
			actions = append(actions, "MONITOR "+s.RealMasterIP+" ON "+sn.PodName)
		}
	}

	for _, rn := range s.RedisNodes {
		if !rn.Reachable || rn.IP == s.RealMasterIP {
			continue
		}
		if rn.Role == roleMaster || rn.MasterHost != s.RealMasterIP || rn.LinkStatus == "down" {
			actions = append(actions, "SLAVEOF "+s.RealMasterIP+" ON "+rn.PodName)
		}
	}

	ghostFound := false
	for _, sn := range s.SentinelNodes {
		if sn.Reachable && sn.Monitoring {
			for _, r := range sn.Replicas {
				if s.IsGhost(r.IP) && (strings.Contains(r.Flags, "s_down") || strings.Contains(r.Flags, "o_down")) {
					ghostFound = true
					break
				}
			}
		}
	}
	if ghostFound {
		actions = append(actions, "SENTINEL RESET mymaster")
	}

	return actions
}
