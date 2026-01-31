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
	"time"

	"github.com/redis/go-redis/v9"
)

const (
	// SentinelMasterName is the name used to identify the master in sentinel
	SentinelMasterName = "mymaster"
	// DefaultTimeout for redis operations
	DefaultTimeout = 5 * time.Second
)

// MasterInfo contains information about the current master
type MasterInfo struct {
	IP   string
	Port string
}

// SentinelClient wraps sentinel operations
type SentinelClient struct {
	addresses []string
	password  string
}

// NewSentinelClient creates a new sentinel client
func NewSentinelClient(addresses []string, password string) *SentinelClient {
	return &SentinelClient{
		addresses: addresses,
		password:  password,
	}
}

// GetMaster queries sentinels to find the current master
func (c *SentinelClient) GetMaster(ctx context.Context) (*MasterInfo, error) {
	var lastErr error

	for _, addr := range c.addresses {
		master, err := c.getMasterFromSentinel(ctx, addr)
		if err != nil {
			lastErr = err
			continue
		}
		return master, nil
	}

	if lastErr != nil {
		return nil, fmt.Errorf("failed to get master from any sentinel: %w", lastErr)
	}
	return nil, fmt.Errorf("no sentinels available")
}

func (c *SentinelClient) getMasterFromSentinel(ctx context.Context, addr string) (*MasterInfo, error) {
	client := redis.NewSentinelClient(&redis.Options{
		Addr:        addr,
		Password:    c.password,
		DialTimeout: DefaultTimeout,
		ReadTimeout: DefaultTimeout,
	})
	defer client.Close()

	// SENTINEL GET-MASTER-ADDR-BY-NAME mymaster
	result, err := client.GetMasterAddrByName(ctx, SentinelMasterName).Result()
	if err != nil {
		return nil, fmt.Errorf("failed to get master addr: %w", err)
	}

	if len(result) != 2 {
		return nil, fmt.Errorf("unexpected result length: %d", len(result))
	}

	return &MasterInfo{
		IP:   result[0],
		Port: result[1],
	}, nil
}

// Ping checks if a redis instance is reachable
func Ping(ctx context.Context, addr, password string) error {
	client := redis.NewClient(&redis.Options{
		Addr:        addr,
		Password:    password,
		DialTimeout: DefaultTimeout,
		ReadTimeout: DefaultTimeout,
	})
	defer client.Close()

	return client.Ping(ctx).Err()
}

// GetReplicationInfo gets replication info from a redis instance
func GetReplicationInfo(ctx context.Context, addr, password string) (role string, masterHost string, masterPort string, err error) {
	client := redis.NewClient(&redis.Options{
		Addr:        addr,
		Password:    password,
		DialTimeout: DefaultTimeout,
		ReadTimeout: DefaultTimeout,
	})
	defer client.Close()

	info, err := client.Info(ctx, "replication").Result()
	if err != nil {
		return "", "", "", fmt.Errorf("failed to get replication info: %w", err)
	}

	// Parse the info string
	role = parseInfoField(info, "role")
	masterHost = parseInfoField(info, "master_host")
	masterPort = parseInfoField(info, "master_port")

	return role, masterHost, masterPort, nil
}

// parseInfoField extracts a field value from redis INFO output
func parseInfoField(info, field string) string {
	// INFO output format: "field:value\r\n"
	prefix := field + ":"
	start := 0
	for i := 0; i < len(info); i++ {
		if i == 0 || info[i-1] == '\n' {
			if len(info[i:]) > len(prefix) && info[i:i+len(prefix)] == prefix {
				start = i + len(prefix)
				for j := start; j < len(info); j++ {
					if info[j] == '\r' || info[j] == '\n' {
						return info[start:j]
					}
				}
				return info[start:]
			}
		}
	}
	return ""
}
