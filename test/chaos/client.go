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

// Package chaos provides a test client for Redis resilience testing.
// It continuously writes and reads data while tracking success/failure rates
// and data integrity.
package chaos

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"math/rand"
	"sync"
	"sync/atomic"
	"time"

	"github.com/redis/go-redis/v9"
)

// Metrics tracks test client statistics using atomic counters
type Metrics struct {
	WriteAttempts    atomic.Int64
	WriteSuccesses   atomic.Int64
	WriteFailures    atomic.Int64
	ReadAttempts     atomic.Int64
	ReadSuccesses    atomic.Int64
	ReadFailures     atomic.Int64
	DataCorruptions  atomic.Int64
	HighestConfirmed atomic.Int64
}

// MetricsSnapshot is a point-in-time snapshot of metrics
type MetricsSnapshot struct {
	WriteAttempts    int64 `json:"writeAttempts"`
	WriteSuccesses   int64 `json:"writeSuccesses"`
	WriteFailures    int64 `json:"writeFailures"`
	ReadAttempts     int64 `json:"readAttempts"`
	ReadSuccesses    int64 `json:"readSuccesses"`
	ReadFailures     int64 `json:"readFailures"`
	DataCorruptions  int64 `json:"dataCorruptions"`
	HighestConfirmed int64 `json:"highestConfirmed"`
}

// WriteAvailability returns the ratio of successful writes to attempted writes
func (m MetricsSnapshot) WriteAvailability() float64 {
	if m.WriteAttempts == 0 {
		return 1.0
	}
	return float64(m.WriteSuccesses) / float64(m.WriteAttempts)
}

// ReadAvailability returns the ratio of successful reads to attempted reads
func (m MetricsSnapshot) ReadAvailability() float64 {
	if m.ReadAttempts == 0 {
		return 1.0
	}
	return float64(m.ReadSuccesses) / float64(m.ReadAttempts)
}

// String returns a human-readable summary of the metrics
func (m MetricsSnapshot) String() string {
	return fmt.Sprintf(
		"Writes: %d attempted, %d succeeded, %d failed (%.2f%% availability)\n"+
			"Reads: %d attempted, %d succeeded, %d failed (%.2f%% availability)\n"+
			"Data corruptions: %d\n"+
			"Highest confirmed key: %d",
		m.WriteAttempts, m.WriteSuccesses, m.WriteFailures, m.WriteAvailability()*100,
		m.ReadAttempts, m.ReadSuccesses, m.ReadFailures, m.ReadAvailability()*100,
		m.DataCorruptions,
		m.HighestConfirmed,
	)
}

// Config holds test client configuration
type Config struct {
	// Addrs is the list of Redis addresses (single for standalone/sentinel, multiple for cluster)
	Addrs []string

	// Password for Redis authentication (optional)
	Password string

	// ClusterMode enables Redis Cluster client
	ClusterMode bool

	// WriteRate is the interval between write attempts (default 100ms = 10/sec)
	WriteRate time.Duration

	// OperationTimeout is the timeout for each Redis operation (default 200ms)
	OperationTimeout time.Duration

	// KeyPrefix is an optional prefix for all keys (useful for namespacing)
	KeyPrefix string
}

// TestClient performs continuous read/write operations for resilience testing
type TestClient struct {
	client           redis.UniversalClient
	metrics          *Metrics
	confirmedKeys    sync.Map // map[int64]struct{} - keys we confirmed were written
	writeRate        time.Duration
	operationTimeout time.Duration
	keyPrefix        string
	stopCh           chan struct{}
	stopOnce         sync.Once
	wg               sync.WaitGroup
	counter          atomic.Int64
}

// expectedValue computes the expected value for a given key (sha256 hash)
func expectedValue(key int64) string {
	hash := sha256.Sum256([]byte(fmt.Sprintf("%d", key)))
	return hex.EncodeToString(hash[:])
}

// NewTestClient creates a new test client
func NewTestClient(cfg Config) (*TestClient, error) {
	var client redis.UniversalClient

	if cfg.ClusterMode {
		client = redis.NewClusterClient(&redis.ClusterOptions{
			Addrs:        cfg.Addrs,
			Password:     cfg.Password,
			ReadTimeout:  cfg.OperationTimeout,
			WriteTimeout: cfg.OperationTimeout,
			DialTimeout:  cfg.OperationTimeout,
		})
	} else {
		addr := "localhost:6379"
		if len(cfg.Addrs) > 0 {
			addr = cfg.Addrs[0]
		}
		client = redis.NewClient(&redis.Options{
			Addr:         addr,
			Password:     cfg.Password,
			ReadTimeout:  cfg.OperationTimeout,
			WriteTimeout: cfg.OperationTimeout,
			DialTimeout:  cfg.OperationTimeout,
		})
	}

	// Test connection
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := client.Ping(ctx).Err(); err != nil {
		return nil, fmt.Errorf("failed to connect to Redis: %w", err)
	}

	writeRate := cfg.WriteRate
	if writeRate == 0 {
		writeRate = 100 * time.Millisecond
	}

	opTimeout := cfg.OperationTimeout
	if opTimeout == 0 {
		opTimeout = 200 * time.Millisecond
	}

	return &TestClient{
		client:           client,
		metrics:          &Metrics{},
		writeRate:        writeRate,
		operationTimeout: opTimeout,
		keyPrefix:        cfg.KeyPrefix,
		stopCh:           make(chan struct{}),
	}, nil
}

// keyName returns the Redis key name for a given counter value
func (tc *TestClient) keyName(n int64) string {
	if tc.keyPrefix != "" {
		return fmt.Sprintf("%s:%d", tc.keyPrefix, n)
	}
	return fmt.Sprintf("%d", n)
}

// doWrite attempts to write a key-value pair to Redis
func (tc *TestClient) doWrite(n int64) {
	tc.metrics.WriteAttempts.Add(1)

	ctx, cancel := context.WithTimeout(context.Background(), tc.operationTimeout)
	defer cancel()

	key := tc.keyName(n)
	value := expectedValue(n)

	err := tc.client.Set(ctx, key, value, 0).Err()
	if err != nil {
		tc.metrics.WriteFailures.Add(1)
		return
	}

	tc.metrics.WriteSuccesses.Add(1)
	tc.confirmedKeys.Store(n, struct{}{})

	// Update highest confirmed using CAS loop
	for {
		current := tc.metrics.HighestConfirmed.Load()
		if n <= current {
			break
		}
		if tc.metrics.HighestConfirmed.CompareAndSwap(current, n) {
			break
		}
	}
}

// doRead attempts to read and verify a random confirmed key
func (tc *TestClient) doRead() {
	highest := tc.metrics.HighestConfirmed.Load()
	if highest <= 0 {
		return // No confirmed writes yet
	}

	// Pick a random key from [1, highest]
	n := rand.Int63n(highest) + 1

	// Check if this key was actually confirmed
	if _, ok := tc.confirmedKeys.Load(n); !ok {
		// This key wasn't confirmed, skip
		return
	}

	tc.metrics.ReadAttempts.Add(1)

	ctx, cancel := context.WithTimeout(context.Background(), tc.operationTimeout)
	defer cancel()

	key := tc.keyName(n)
	result, err := tc.client.Get(ctx, key).Result()
	if err != nil {
		tc.metrics.ReadFailures.Add(1)
		return
	}

	expected := expectedValue(n)
	if result != expected {
		tc.metrics.DataCorruptions.Add(1)
		return
	}

	tc.metrics.ReadSuccesses.Add(1)
}

// Start begins the test client operations
func (tc *TestClient) Start() {
	tc.wg.Add(1)
	go func() {
		defer tc.wg.Done()

		ticker := time.NewTicker(tc.writeRate)
		defer ticker.Stop()

		for {
			select {
			case <-tc.stopCh:
				return
			case <-ticker.C:
				n := tc.counter.Add(1)
				go tc.doWrite(n)
				go tc.doRead()
			}
		}
	}()
}

// Stop stops the test client and waits for pending operations
func (tc *TestClient) Stop() {
	tc.stopOnce.Do(func() {
		close(tc.stopCh)
	})
	tc.wg.Wait()
	// Give pending goroutines a moment to complete
	time.Sleep(tc.operationTimeout * 2)
}

// Close stops the client and closes the Redis connection
func (tc *TestClient) Close() error {
	tc.Stop()
	return tc.client.Close()
}

// GetMetrics returns a snapshot of current metrics
func (tc *TestClient) GetMetrics() MetricsSnapshot {
	return MetricsSnapshot{
		WriteAttempts:    tc.metrics.WriteAttempts.Load(),
		WriteSuccesses:   tc.metrics.WriteSuccesses.Load(),
		WriteFailures:    tc.metrics.WriteFailures.Load(),
		ReadAttempts:     tc.metrics.ReadAttempts.Load(),
		ReadSuccesses:    tc.metrics.ReadSuccesses.Load(),
		ReadFailures:     tc.metrics.ReadFailures.Load(),
		DataCorruptions:  tc.metrics.DataCorruptions.Load(),
		HighestConfirmed: tc.metrics.HighestConfirmed.Load(),
	}
}

// ConfirmedKeyCount returns the number of keys that were confirmed written
func (tc *TestClient) ConfirmedKeyCount() int64 {
	var count int64
	tc.confirmedKeys.Range(func(_, _ interface{}) bool {
		count++
		return true
	})
	return count
}
