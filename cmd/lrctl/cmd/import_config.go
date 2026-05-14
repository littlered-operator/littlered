package cmd

import (
	"fmt"
	"strconv"
	"strings"

	"k8s.io/apimachinery/pkg/api/resource"
)

const redisConfYes = "yes"

// interestingKeys are performance-relevant CONFIG keys worth including in spec.config.raw
// when they differ from Redis defaults.
var interestingKeys = map[string]string{
	// Memory tuning
	"maxmemory-samples":             "10",
	"lazyfree-lazy-eviction":        "no",
	"lazyfree-lazy-expire":          "no",
	"lazyfree-lazy-server-del":      "no",
	"lazyfree-lazy-user-del":        "no",
	"lazyfree-lazy-user-flush":      "no",
	"active-defrag-enabled":         "no",
	"active-defrag-threshold-lower": "10",
	"active-defrag-threshold-upper": "100",
	"active-defrag-cycle-min":       "1",
	"active-defrag-cycle-max":       "25",
	// Connection tuning
	"maxclients":  "10000",
	"hz":          "10",
	"dynamic-hz":  "yes",
	"tcp-backlog": "511",
	// Client buffers (Redis reports these as bytes, not human-readable units)
	"proto-max-bulk-len":        "536870912",  // 512mb
	"client-query-buffer-limit": "1073741824", // 1gb
	// Slow log
	"slowlog-log-slower-than": "10000",
	"slowlog-max-len":         "128",
	// Notifications
	"notify-keyspace-events": "",
	// IO threads
	"io-threads":          "1",
	"io-threads-do-reads": "no",
	// Scripting
	"lua-time-limit": "5000",
	// Latency
	"latency-tracking": redisConfYes,
	// Cluster-specific tuning
	"cluster-migration-barrier":      "1",
	"cluster-allow-reads-when-down":  "no",
	"cluster-allow-pubsub-when-down": redisConfYes,
	// Encoding thresholds
	"list-max-listpack-size":    "-2",
	"list-max-ziplist-size":     "-2",
	"hash-max-listpack-entries": "128",
	"hash-max-listpack-value":   "64",
	"hash-max-ziplist-entries":  "128",
	"hash-max-ziplist-value":    "64",
	"set-max-intset-entries":    "512",
	"set-max-listpack-entries":  "128",
	"zset-max-listpack-entries": "128",
	"zset-max-listpack-value":   "64",
	"zset-max-ziplist-entries":  "128",
	"zset-max-ziplist-value":    "64",
	// Close-on-OOM (Valkey 8+)
	"close-on-oom": "no",
}

// buildRawConfig collects interesting non-default CONFIG values into a redis.conf snippet.
func buildRawConfig(config map[string]string) string {
	lines := make([]string, 0, len(interestingKeys))
	for key, defaultVal := range interestingKeys {
		val, ok := config[key]
		if !ok || val == defaultVal {
			continue
		}
		lines = append(lines, fmt.Sprintf("%s %s", key, val))
	}
	// Stable ordering for reproducibility.
	if len(lines) == 0 {
		return ""
	}
	// Sort for determinism.
	sortStrings(lines)
	return strings.Join(lines, "\n")
}

// sortStrings sorts a slice in place (avoids importing "sort" for one call).
func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}

// bytesToQuantity converts raw bytes (as returned by CONFIG GET maxmemory) to
// a Kubernetes resource.Quantity string like "256Mi" or "1Gi".
func bytesToQuantity(bytesStr string) string {
	b, err := strconv.ParseInt(bytesStr, 10, 64)
	if err != nil || b <= 0 {
		return ""
	}
	q := resource.NewQuantity(b, resource.BinarySI)
	return q.String()
}

// atoiOr returns the parsed int, or fallback on error.
func atoiOr(s string, fallback int) int {
	v, err := strconv.Atoi(s)
	if err != nil {
		return fallback
	}
	return v
}
