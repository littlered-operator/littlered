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

package controller

import (
	"context"
	"fmt"
	"strings"

	littleredv1alpha1 "github.com/littlered-operator/littlered-operator/api/v1alpha1"
	redisclient "github.com/littlered-operator/littlered-operator/internal/redis"
)

type operatorGatherer struct {
	password   string
	tlsEnabled bool
}

func (g *operatorGatherer) GetRedisState(ctx context.Context, podName, ip string) (*redisclient.RedisNodeState, error) {
	addr := fmt.Sprintf("%s:%d", ip, littleredv1alpha1.RedisPort)
	role, mHost, link, offset, err := redisclient.GetReplicationInfo(ctx, addr, g.password, g.tlsEnabled)
	if err != nil {
		return nil, err
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

func (g *operatorGatherer) GetSentinelState(ctx context.Context, podName, ip string) (*redisclient.SentinelNodeState, error) {
	podAddr := fmt.Sprintf("%s:%d", ip, littleredv1alpha1.SentinelPort)
	sc := redisclient.NewSentinelClient([]string{podAddr}, g.password, g.tlsEnabled)

	masterInfo, err := sc.GetMasterState(ctx, redisclient.SentinelMasterName)
	if err != nil {
		if strings.Contains(err.Error(), "ERR No such master") || strings.Contains(err.Error(), "redis: nil") {
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
		PodName:        podName,
		IP:             ip,
		Monitoring:     true,
		MasterIP:       masterInfo.IP,
		FailoverStatus: masterInfo.FailoverStatus,
		Reachable:      true,
	}

	if reps, err := sc.GetReplicas(ctx, redisclient.SentinelMasterName); err == nil {
		state.Replicas = reps
	}

	return state, nil
}

func (g *operatorGatherer) GetClusterID(ctx context.Context, podName, ip string) (string, error) {
	cc := redisclient.NewClusterClient(g.password, g.tlsEnabled)
	addr := fmt.Sprintf("%s:%d", ip, littleredv1alpha1.RedisPort)
	return cc.GetMyID(ctx, addr)
}

func (g *operatorGatherer) GetClusterInfo(ctx context.Context, podName, ip string) (*redisclient.ClusterInfo, error) {
	cc := redisclient.NewClusterClient(g.password, g.tlsEnabled)
	addr := fmt.Sprintf("%s:%d", ip, littleredv1alpha1.RedisPort)
	return cc.GetClusterInfo(ctx, addr)
}

func (g *operatorGatherer) GetClusterNodes(ctx context.Context, podName, ip string) ([]redisclient.ClusterNodeInfo, error) {
	cc := redisclient.NewClusterClient(g.password, g.tlsEnabled)
	addr := fmt.Sprintf("%s:%d", ip, littleredv1alpha1.RedisPort)
	return cc.GetClusterNodes(ctx, addr)
}
