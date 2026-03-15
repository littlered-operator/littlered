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

package cmd

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"

	"github.com/littlered-operator/littlered-operator/internal/cli/k8s"
	"github.com/littlered-operator/littlered-operator/internal/cli/types"
	redisclient "github.com/littlered-operator/littlered-operator/internal/redis"
)

type cliGatherer struct {
	coreClient *kubernetes.Clientset
	config     *rest.Config
	cCtx       *types.ClusterContext
}

func (g *cliGatherer) GetRedisState(ctx context.Context, podName, ip string) (*redisclient.RedisNodeState, error) {
	stdout, _, err := k8s.Exec(g.coreClient, g.config, g.cCtx.Namespace, podName, g.cCtx.RedisContainer, []string{"redis-cli", "info", "replication"})
	if err != nil {
		return nil, err
	}

	role := redisclient.ParseInfoField(stdout, "role")
	mHost := redisclient.ParseInfoField(stdout, "master_host")
	link := redisclient.ParseInfoField(stdout, "master_link_status")

	// If mHost is a hostname, resolve it to an IP using K8s API info
	if mHost != "" {
		mHost = g.resolveIdentityToIP(mHost)
	}

	offsetStr := ""
	if role == "master" {
		offsetStr = redisclient.ParseInfoField(stdout, "master_repl_offset")
	} else {
		offsetStr = redisclient.ParseInfoField(stdout, "slave_repl_offset")
	}
	var offset int64
	if offsetStr != "" {
		_, _ = fmt.Sscanf(offsetStr, "%d", &offset)
	}

	return &redisclient.RedisNodeState{
		PodName:    podName,
		IP:         ip,
		Role:       role,
		MasterHost: mHost,
		LinkStatus: link,
		Offset:     offset,
		Reachable:  true,
	}, nil
}

func (g *cliGatherer) GetSentinelState(ctx context.Context, podName, ip string) (*redisclient.SentinelNodeState, error) {
	// Get Master
	stdout, _, err := k8s.Exec(g.coreClient, g.config, g.cCtx.Namespace, podName, g.cCtx.SentinelContainer, []string{"redis-cli", "-p", "26379", "sentinel", "master", "mymaster"})
	if err != nil {
		if strings.Contains(err.Error(), "ERR No such master") {
			return &redisclient.SentinelNodeState{
				PodName:    podName,
				IP:         ip,
				Monitoring: false,
				Reachable:  true,
			}, nil
		}
		return nil, err
	}

	state := &redisclient.SentinelNodeState{
		PodName:    podName,
		IP:         ip,
		Monitoring: true,
		Reachable:  true,
	}

	// Parse SENTINEL MASTER output
	lines := strings.Split(strings.ReplaceAll(stdout, "\r", ""), "\n")
	for i := 0; i < len(lines)-1; i++ {
		line := strings.TrimSpace(lines[i])
		if line == "ip" {
			mIP := strings.TrimSpace(lines[i+1])
			// Resolve hostname to IP
			state.MasterIP = g.resolveIdentityToIP(mIP)
		}
		if line == "failover-status" {
			state.FailoverStatus = strings.TrimSpace(lines[i+1])
		}
	}

	// Get Replicas
	stdout, _, err = k8s.Exec(g.coreClient, g.config, g.cCtx.Namespace, podName, g.cCtx.SentinelContainer, []string{"redis-cli", "-p", "26379", "sentinel", "replicas", "mymaster"})
	if err == nil {
		redisIPs := g.cCtx.GetRedisIPs()
		for _, rip := range redisIPs {
			if strings.Contains(stdout, rip) {
				state.Replicas = append(state.Replicas, redisclient.ReplicaInfo{IP: rip, Flags: "found"})
			}
		}
		// Try a basic search for IPs in the output to find ghosts
		allLines := strings.Split(stdout, "\n")
		for idx, l := range allLines {
			if strings.Contains(l, "ip") && idx+1 < len(allLines) {
				potentialIP := strings.TrimSpace(allLines[idx+1])
				// Resolve hostname to IP if needed
				resolvedIP := g.resolveIdentityToIP(potentialIP)

				isValid := slices.Contains(redisIPs, resolvedIP)
				if !isValid {
					state.Replicas = append(state.Replicas, redisclient.ReplicaInfo{IP: resolvedIP, Flags: "s_down,ghost"})
				}
			}
		}
	}

	return state, nil
}

// resolveIdentityToIP takes an identity (IP, pod name, or FQDN) and tries to resolve it to a Pod IP
// using the pods already discovered in ClusterContext.
func (g *cliGatherer) resolveIdentityToIP(identity string) string {
	if identity == "" {
		return ""
	}

	// If it's already an IP, return it
	if isIP(identity) {
		return identity
	}

	// For hostnames (pod-0.headless-svc.namespace.svc.cluster.local),
	// the first segment is usually the pod name.
	podNameCandidate := identity
	if idx := strings.Index(identity, "."); idx != -1 {
		podNameCandidate = identity[:idx]
	}

	// Look up in Redis pods
	for _, p := range g.cCtx.RedisPods {
		if p.Name == podNameCandidate && p.Status.PodIP != "" {
			return p.Status.PodIP
		}
	}

	// Look up in Sentinel pods
	for _, p := range g.cCtx.SentinelPods {
		if p.Name == podNameCandidate && p.Status.PodIP != "" {
			return p.Status.PodIP
		}
	}

	return identity // Fallback to original if not found
}

func isIP(s string) bool {
	// Simple IP check: contains 3 dots and only digits/dots
	dots := 0
	for _, c := range s {
		if c == '.' {
			dots++
		} else if c < '0' || c > '9' {
			return false
		}
	}
	return dots == 3
}

func (g *cliGatherer) GetClusterID(ctx context.Context, podName, ip string) (string, error) {
	stdout, _, err := k8s.Exec(g.coreClient, g.config, g.cCtx.Namespace, podName, g.cCtx.RedisContainer, []string{"redis-cli", "cluster", "myid"})
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(stdout), nil
}

func (g *cliGatherer) GetClusterInfo(ctx context.Context, podName, ip string) (*redisclient.ClusterInfo, error) {
	stdout, _, err := k8s.Exec(g.coreClient, g.config, g.cCtx.Namespace, podName, g.cCtx.RedisContainer, []string{"redis-cli", "cluster", "info"})
	if err != nil {
		return nil, err
	}
	return redisclient.ParseClusterInfo(stdout), nil
}

func (g *cliGatherer) GetClusterNodes(ctx context.Context, podName, ip string) ([]redisclient.ClusterNodeInfo, error) {
	stdout, _, err := k8s.Exec(g.coreClient, g.config, g.cCtx.Namespace, podName, g.cCtx.RedisContainer, []string{"redis-cli", "cluster", "nodes"})
	if err != nil {
		return nil, err
	}
	return redisclient.ParseClusterNodes(stdout), nil
}
