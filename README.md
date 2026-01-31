# LittleRed

A lightweight Kubernetes operator for deploying Redis/Valkey as an in-memory cache.

## Features

- **Standalone mode**: Single Redis instance for simple caching
- **Sentinel mode**: 1 master + 2 replicas + 3 sentinels with automatic failover
- **Valkey/Redis support**: Uses Valkey 8.0 by default (Redis 7.2+ compatible)
- **Prometheus metrics**: Built-in redis_exporter sidecar
- **Optional ServiceMonitor**: For Prometheus Operator integration
- **Optional auth & TLS**: Password authentication and TLS encryption
- **Cache-optimized defaults**: No persistence, allkeys-lru eviction, Guaranteed QoS

## Quick Start

### Install with Helm

```bash
helm install littlered charts/littlered-operator -n littlered-system --create-namespace
```

### Create a Redis Cache

Minimal standalone cache:

```yaml
apiVersion: littlered.tanne3.de/v1alpha1
kind: LittleRed
metadata:
  name: my-cache
spec: {}
```

Sentinel mode with automatic failover:

```yaml
apiVersion: littlered.tanne3.de/v1alpha1
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

# Sentinel mode - connect to master
kubectl exec -it my-cache-redis-0 -- valkey-cli -p 26379 SENTINEL get-master-addr-by-name mymaster
```

## Configuration

### Deployment Modes

| Mode | Pods | Use Case |
|------|------|----------|
| `standalone` | 1 Redis | Development, simple caching |
| `sentinel` | 3 Redis + 3 Sentinel | Production, high availability |

### Spec Reference

```yaml
apiVersion: littlered.tanne3.de/v1alpha1
kind: LittleRed
metadata:
  name: my-cache
spec:
  # Deployment mode: standalone or sentinel
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
    maxmemoryPolicy: allkeys-lru
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
```

## Examples

See [config/samples/](config/samples/) for more examples:

- `littlered_v1alpha1_littlered.yaml` - Minimal standalone
- `littlered_v1alpha1_littlered_full.yaml` - Full standalone configuration
- `littlered_v1alpha1_littlered_sentinel.yaml` - Sentinel mode
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
