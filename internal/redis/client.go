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

// Subscribe connects to a sentinel and subscribes to the given channels.
// It returns a channel that receives messages and a close function.
// Note: This connects to the first available sentinel address.
func (c *SentinelClient) Subscribe(ctx context.Context, channels ...string) (<-chan *redis.Message, func(), error) {
	var client *redis.Client
	var lastErr error

	// Try to connect to any available sentinel
	for _, addr := range c.addresses {
		// Use a standard client for Pub/Sub connections to Sentinel
		rdb := redis.NewClient(&redis.Options{
			Addr:        addr,
			Password:    c.password,
			DialTimeout: DefaultTimeout,
			// No read timeout for Pub/Sub
			ReadTimeout: -1,
		})

		if err := rdb.Ping(ctx).Err(); err == nil {
			client = rdb
			break
		} else {
			lastErr = err
			rdb.Close()
		}
	}

	if client == nil {
		if lastErr != nil {
			return nil, nil, fmt.Errorf("failed to connect to any sentinel: %w", lastErr)
		}
		return nil, nil, fmt.Errorf("no sentinels available")
	}

	pubsub := client.Subscribe(ctx, channels...)

	// Verify subscription
	if _, err := pubsub.Receive(ctx); err != nil {
		pubsub.Close()
		client.Close()
		return nil, nil, fmt.Errorf("failed to subscribe: %w", err)
	}

	// Return the channel and a cleanup function
	return pubsub.Channel(), func() {
		pubsub.Close()
		client.Close()
	}, nil
}

// Monitor tells the sentinels to start monitoring a new master
func (c *SentinelClient) Monitor(ctx context.Context, name, ip string, port int, quorum int) error {
	var lastErr error
	for _, addr := range c.addresses {
		client := redis.NewSentinelClient(&redis.Options{
			Addr:        addr,
			Password:    c.password,
			DialTimeout: DefaultTimeout,
			ReadTimeout: DefaultTimeout,
		})
		err := client.Process(ctx, redis.NewStatusCmd(ctx, "SENTINEL", "MONITOR", name, ip, port, quorum))
		client.Close()
		if err == nil {
			return nil
		}
		lastErr = err
	}
	return fmt.Errorf("failed to issue MONITOR command to any sentinel: %w", lastErr)
}

// Set updates sentinel configuration for a master
func (c *SentinelClient) Set(ctx context.Context, name, option, value string) error {
	var lastErr error
	for _, addr := range c.addresses {
		client := redis.NewSentinelClient(&redis.Options{
			Addr:        addr,
			Password:    c.password,
			DialTimeout: DefaultTimeout,
			ReadTimeout: DefaultTimeout,
		})
		err := client.Process(ctx, redis.NewStatusCmd(ctx, "SENTINEL", "SET", name, option, value))
		client.Close()
		if err == nil {
			return nil
		}
		lastErr = err
	}
	return fmt.Errorf("failed to issue SET command to any sentinel: %w", lastErr)
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
