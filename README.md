# LittleRed

A Kubernetes operator for deploying Valkey/Redis as a pure in-memory data store.

LittleRed is built for workloads where persistence is explicitly disabled and never enabled—not even by accident. It provides a full reconciliation engine to manage node identities and cluster membership across restarts and failures: the class of problem where static Helm charts and startup scripts reach their limits.

## Quick Start

### 1. Install the Operator

```bash
helm install littlered oci://ghcr.io/littlered-operator/charts/littlered \
  -n littlered-system --create-namespace
```

This installs the latest release. For a pinned version, add `--version <version>` — see the [releases page](https://github.com/littlered-operator/littlered-operator/releases).

### 2. Deploy an Instance

Set `spec.mode` to choose your deployment type:

```yaml
apiVersion: chuck-chuck-chuck.net/v1alpha1
kind: LittleRed
metadata:
  name: my-store
spec:
  mode: sentinel   # standalone | sentinel | cluster
```

```bash
kubectl apply -f my-store.yaml
```

`standalone` runs a single Redis pod. `sentinel` runs 3 Redis pods (1 master + 2 replicas) monitored by 3 sentinels for automatic failover. `cluster` runs a sharded Redis Cluster across multiple pods for horizontal scaling.

### 3. Verify Health

```bash
# Install the CLI as a kubectl plugin
make install-plugin

# Check cluster consistency
kubectl lr verify my-store
```

## Key Features

- **Three deployment modes**: `standalone` (single pod), `sentinel` (1 master + 2 replicas monitored by 3 sentinels for automatic failover), and `cluster` (sharded Redis Cluster for horizontal scaling).
- **Valkey 8.0 by default**, compatible with Redis 7.2+.
- **Guaranteed QoS**: resource limits always equal requests, preventing OOM surprises and CPU throttling.
- **`noeviction` by default**: memory exhaustion returns an error rather than silently dropping data. Explicitly configure a different policy if you need eviction semantics.
- **Security**: password authentication and TLS encryption, both via Kubernetes Secrets.
- **Observability**: `redis_exporter` sidecar included by default, with optional `ServiceMonitor` for Prometheus.
- **`lrctl`**: a CLI tool (installable as a `kubectl lr` plugin) for direct state inspection and verification.

> **Current scope:** Cluster mode is validated for **3 shards** with **0 or 1 replica per shard**. Variable shard counts are planned.

## Configuration Reference

```yaml
apiVersion: chuck-chuck-chuck.net/v1alpha1
kind: LittleRed
metadata:
  name: my-store
spec:
  mode: standalone          # standalone | sentinel | cluster

  image:
    registry: docker.io
    path: valkey/valkey
    tag: "8.0"
    pullPolicy: IfNotPresent

  resources:                # Guaranteed QoS: keep requests == limits
    requests: { cpu: 250m, memory: 256Mi }
    limits:   { cpu: 250m, memory: 256Mi }

  config:
    maxmemoryPolicy: noeviction
    tcpKeepalive: 300
    timeout: 0

  # Mode-specific settings
  sentinel:                 # For mode: sentinel
    quorum: 2
    downAfterMilliseconds: 5000
    failoverTimeout: 60000

  cluster:                  # For mode: cluster
    shards: 3
    replicasPerShard: 1

  # Security
  auth:
    enabled: false
    existingSecret: ""      # Secret must have a 'password' key

  tls:
    enabled: false
    existingSecret: ""      # Secret must have 'tls.crt' and 'tls.key'
    caCertSecret: ""        # Optional: separate Secret with 'ca.crt'
    clientAuth: false       # Require client certificates

  # Observability
  metrics:
    enabled: true
    serviceMonitor:
      enabled: false
```

Full field reference: [docs/API_SPEC.md](docs/API_SPEC.md).

## Why LittleRed?

Running Redis or Valkey without persistence creates a lifecycle problem that standard tooling doesn't handle well.

**The risk:** When a non-persistent node restarts, it comes back with an empty dataset. If it returns with its previous identity—same IP or hostname—the cluster may accept it as the authoritative source of truth and trigger a full sync from it, wiping data on healthy replicas.

**The solution:** LittleRed treats every restart as a new entity. It tracks node identities in Kubernetes, not inside Redis. When a pod disappears and a replacement arrives, the operator:

1. Removes the stale identity ("ghost node") from the cluster before the replacement joins.
2. Waits for any in-progress replica promotion to complete before healing the partition.
3. Intervenes with a forced promotion only when the cluster cannot self-recover (e.g., quorum loss).

The core principle is **minimal interference**: trust Sentinel and Cluster Gossip to handle their own state transitions, and intervene only when the cluster lacks the context to heal itself—specifically when it cannot see what the Kubernetes API already knows (that a pod is gone for good).

## Documentation

- [Usage Guide](docs/USAGE.md) — deployment examples for all three modes
- [API Reference](docs/API_SPEC.md) — full spec field documentation
- [Architecture](docs/ARCHITECTURE.md) — reconciliation design, ADRs, and [terminology conventions](docs/ARCHITECTURE.md#terminology)
- [E2E Testing](docs/E2E_TESTING.md) — running the test suite and manual chaos testing
- [Test Cases](docs/TEST_CASES.md) — full list of covered scenarios and their status
- [CLI Reference](docs/LRCTL.md) — `lrctl` / `kubectl lr` guide
- [Development Guide](docs/DEVELOPMENT.md) — building from source, custom registry, local Kind workflow

## Behind the Name

The name **LittleRed** is an homage to the fable of the *Little Red Hen*. When we searched for a tool that handled the complexities of pure in-memory Redis lifecycles with technical rigor, we found that the existing solutions were often unmaintained or focused on different problems. Like the hen in the story, we decided to build it ourselves.

## License

Apache License 2.0
