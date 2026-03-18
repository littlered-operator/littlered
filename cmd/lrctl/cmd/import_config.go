package cmd

import (
	"fmt"
	"strconv"
	"strings"

	"k8s.io/apimachinery/pkg/api/resource"
)

// skipKeys are CONFIG keys managed by the operator or not meaningful for a new deployment.
// These are never included in spec.config.raw.
var skipKeys = map[string]bool{
	// Networking — operator-managed
	"bind": true, "port": true, "protected-mode": true,
	// Persistence — always disabled
	"save": true, "appendonly": true, "appendfsync": true, "appendfilename": true,
	"dbfilename": true, "aof-use-rdb-preamble": true, "rdbchecksum": true,
	"rdbcompression": true, "rdb-del-sync-files": true,
	// Paths — container-specific
	"dir": true, "logfile": true, "pidfile": true,
	// Replication — operator-managed
	"slaveof": true, "replicaof": true, "masterauth": true,
	"replica-serve-stale-data": true, "replica-read-only": true,
	"repl-diskless-sync": true, "repl-diskless-sync-delay": true,
	"repl-diskless-load": true, "replica-announce-ip": true,
	"replica-announce-port": true, "min-replicas-to-write": true,
	"min-replicas-max-lag": true, "repl-min-slaves-to-write": true,
	"repl-min-slaves-max-lag": true,
	// Cluster — operator-managed (cluster-enabled is startup-only, never in CONFIG GET)
	"cluster-config-file":       true,
	"cluster-announce-hostname": true, "cluster-announce-ip": true,
	"cluster-announce-port": true, "cluster-announce-bus-port": true,
	"cluster-announce-tls-port": true, "cluster-preferred-endpoint-type": true,
	// TLS — detected and mapped to spec.tls, not raw
	"tls-port": true, "tls-cert-file": true, "tls-key-file": true,
	"tls-ca-cert-file": true, "tls-ca-cert-dir": true,
	"tls-auth-clients": true, "tls-cluster": true, "tls-replication": true,
	"tls-protocols": true, "tls-ciphersuites": true, "tls-ciphers": true,
	// Auth — detected and mapped to spec.auth, not raw
	"requirepass": true,
	// First-class CR fields — mapped directly, not raw
	"maxmemory": true, "maxmemory-policy": true,
	"timeout": true, "tcp-keepalive": true,
	"cluster-node-timeout": true,
	// Runtime/internal — not useful for a new deployment
	"databases": true, "always-show-logo": true, "daemonize": true,
	"supervised": true, "loglevel": true, "syslog-enabled": true,
	"syslog-ident": true, "syslog-facility": true,
	"crash-log-enabled": true, "crash-memcheck-enabled": true,
}

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
	"latency-tracking": "yes",
	// Cluster-specific tuning
	"cluster-migration-barrier":      "1",
	"cluster-allow-reads-when-down":  "no",
	"cluster-allow-pubsub-when-down": "yes",
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
	var lines []string
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
