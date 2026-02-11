# LittleRed

A lightweight Kubernetes operator for deploying Redis/Valkey as an in-memory cache.

## Features

- **Standalone mode**: Single Redis instance for simple caching
- **Sentinel mode**: 1 master + 2 replicas + 3 sentinels with automatic failover
- **Cluster mode**: Horizontally scaled Redis Cluster with automatic sharding (16384 hash slots)
- **Valkey/Redis support**: Uses Valkey 8.0 by default (Redis 7.2+ compatible)
- **Prometheus metrics**: Built-in redis_exporter sidecar
- **Optional ServiceMonitor**: For Prometheus Operator integration
- **Optional auth & TLS**: Password authentication and TLS encryption
- **Safe-by-default performance**: No persistence (in-memory only), noeviction policy, Guaranteed QoS.

## Quick Start

### Install with Helm

```bash
helm install littlered charts/littlered-operator -n littlered-system --create-namespace
```

### Create a Redis Cache

Minimal standalone cache:

```yaml
apiVersion: littlered.chuck-chuck-chuck.net/v1alpha1
kind: LittleRed
metadata:
  name: my-cache
spec: {}
```

Sentinel mode with automatic failover:

```yaml
apiVersion: littlered.chuck-chuck-chuck.net/v1alpha1
kind: LittleRed
metadata:
  name: my-cache
spec:
  mode: sentinel
```

Apply:

```bash
kubectl apply -f my-cache.yaml
```

### Connect to Redis

```bash
# Standalone mode
kubectl exec -it my-cache-redis-0 -- valkey-cli PING

# Sentinel mode - query sentinel for master address
kubectl exec -it my-cache-sentinel-0 -- valkey-cli -p 26379 SENTINEL get-master-addr-by-name mymaster

# Sentinel mode - connect to Redis master
kubectl exec -it my-cache-redis-0 -c redis -- valkey-cli PING
```

## Configuration

### Deployment Modes

| Mode | Pods | Use Case |
|------|------|----------|
| `standalone` | 1 Redis | Development, simple caching |
| `sentinel` | 3 Redis + 3 Sentinel | Production, high availability |
| `cluster` | Shards × (1 + replicas) | Production, horizontal scaling with sharding |

### Spec Reference

```yaml
apiVersion: littlered.chuck-chuck-chuck.net/v1alpha1
kind: LittleRed
metadata:
  name: my-cache
spec:
  # Deployment mode: standalone, sentinel, or cluster
  mode: standalone

  # Container image
  image:
    registry: docker.io
    path: valkey/valkey
    tag: "8.0"
    pullPolicy: IfNotPresent
    pullSecrets: []

  # Redis container resources (Guaranteed QoS by default)
  resources:
    requests:
      cpu: "250m"
      memory: "256Mi"
    limits:
      cpu: "250m"
      memory: "256Mi"

  # Redis configuration
  config:
    maxmemory: ""           # e.g., "900Mi" (auto-calculated from memory limit if empty)
    maxmemoryPolicy: noeviction
    timeout: 0              # Client idle timeout (0 = disabled)
    tcpKeepalive: 300
    raw: ""                 # Raw redis.conf content (expert mode)

  # Authentication
  auth:
    enabled: false
    existingSecret: ""      # Secret with 'password' key

  # TLS encryption
  tls:
    enabled: false
    existingSecret: ""      # Secret with tls.crt, tls.key
    caCertSecret: ""        # Secret with ca.crt
    clientAuth: false

  # Prometheus metrics
  metrics:
    enabled: true
    exporter:
      path: oliver006/redis_exporter
      tag: v1.66.0
    serviceMonitor:
      enabled: false
      namespace: ""
      labels: {}
      interval: "30s"
      scrapeTimeout: "10s"

  # Service configuration
  service:
    type: ClusterIP
    annotations: {}
    labels: {}

  # Update strategy
  updateStrategy:
    type: RollingUpdate     # or Recreate

  # Pod customizations
  podTemplate:
    annotations: {}
    labels: {}
    nodeSelector: {}
    tolerations: []
    affinity: {}
    priorityClassName: ""
    securityContext: {}
    topologySpreadConstraints: []

  # Sentinel settings (sentinel mode only)
  sentinel:
    quorum: 2
    downAfterMilliseconds: 30000
    failoverTimeout: 180000
    parallelSyncs: 1
    resources:
      requests:
        cpu: "100m"
        memory: "64Mi"
      limits:
        cpu: "100m"
        memory: "64Mi"

  # Cluster settings (cluster mode only)
  cluster:
    shards: 3               # Number of master shards (minimum 3)
    replicasPerShard: 1     # Replicas per master (0 = no replicas)
    clusterNodeTimeout: 15000

  # Requeue intervals for tuning large-scale deployments
  requeueIntervals:
    fast: "2s"              # Used during initialization/recovery
    steadyState: "30s"      # Used for periodic health checks when Running
```

### Status

```yaml
status:
  phase: Running           # Pending, Initializing, Running, Failed, Terminating
  observedGeneration: 1
  conditions:
    - type: Ready
      status: "True"
  redis:
    ready: 1
    total: 1
  # Sentinel mode only:
  master:
    podName: my-cache-redis-0
    ip: 10.244.0.5
  replicas:
    ready: 2
    total: 2
  sentinels:
    ready: 3
    total: 3
  # Cluster mode only:
  cluster:
    state: ok
    lastBootstrap: "2026-02-03T12:00:00Z"
    nodes:
      - podName: my-cache-cluster-0
        nodeId: abc123...
        role: master
        slotRanges: "0-5460"
```

## Examples

See [docs/USAGE.md](docs/USAGE.md) for detailed usage guide with step-by-step instructions.

Sample manifests in [config/samples/](config/samples/):

- `littlered_v1alpha1_littlered.yaml` - Minimal standalone
- `littlered_v1alpha1_littlered_full.yaml` - Full standalone configuration
- `littlered_v1alpha1_littlered_sentinel.yaml` - Sentinel mode
- `littlered_v1alpha1_littlered_cluster.yaml` - Cluster mode
- `littlered_v1alpha1_littlered_auth.yaml` - With password authentication
- `littlered_v1alpha1_littlered_production.yaml` - Production-ready sentinel with auth, ServiceMonitor, and pod spreading

## Services

### Standalone Mode

| Service | Port | Description |
|---------|------|-------------|
| `{name}` | 6379, 9121 | Redis + metrics |

### Sentinel Mode

| Service | Port | Description |
|---------|------|-------------|
| `{name}` | 6379, 9121 | Redis master (follows failover) |
| `{name}-replicas` | 6379, 9121 | All replicas (read replicas) |
| `{name}-sentinel` | 26379 | Sentinel endpoints |

### Cluster Mode

| Service | Port | Description |
|---------|------|-------------|
| `{name}` | 6379, 9121 | Client access (any node) |
| `{name}-cluster` | 6379, 16379 | Headless for cluster communication |

## Development

### Prerequisites

- Go 1.24+
- kubectl
- Access to a Kubernetes cluster

### Run Locally

```bash
# Install CRDs
make install

# Run controller locally
make run

# Apply a sample
kubectl apply -f config/samples/littlered_v1alpha1_littlered.yaml
```

### Run Tests

```bash
# Unit and integration tests
make test

# E2E tests (requires kind)
make test-e2e
```

### E2E Test Details

See [docs/E2E_TESTING.md](docs/E2E_TESTING.md) for the complete guide on building, deploying, and running e2e tests.

Quick start (against existing deployment):

```bash
SKIP_OPERATOR_DEPLOY=true go test -tags=e2e ./test/e2e/ -v -ginkgo.v -timeout 30m
```

### Build

```bash
# Build binary
make build

# Build container image
make docker-build IMG=myregistry/littlered-operator:v0.1.0

# Push image
make docker-push IMG=myregistry/littlered-operator:v0.1.0
```

## License

Apache License 2.0
