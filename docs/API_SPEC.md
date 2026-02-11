# LittleRed - API Specification

> Detailed Custom Resource Definition schema for LittleRed.

**Document Status**: Draft
**Last Updated**: 2026-02-11
**API Version**: `littlered.chuck-chuck-chuck.net/v1alpha1`

---

## 1. Resource Overview

```yaml
apiVersion: littlered.chuck-chuck-chuck.net/v1alpha1
kind: LittleRed
metadata:
  name: my-cache
spec:
  # ... specification
status:
  # ... status
```

---

## 2. Spec Fields

### 2.1 Core Fields

| Field | Type | Required | Default | Description |
|-------|------|----------|---------|-------------|
| `mode` | `string` | No | `standalone` | Deployment mode: `standalone`, `sentinel`, or `cluster` |

### 2.2 Image Configuration

Image is composed from three parts: `{registry}/{path}:{tag}`

```yaml
spec:
  image:
    registry: docker.io         # Registry hostname
    path: valkey/valkey         # Image path (without registry or tag)
    tag: "8.0"                  # Version tag

    pullPolicy: IfNotPresent
    pullSecrets:
      - name: my-registry-secret
```

| Field | Type | Required | Default | Description |
|-------|------|----------|---------|-------------|
| `image.registry` | `string` | No | `docker.io` | Registry hostname |
| `image.path` | `string` | No | `valkey/valkey` | Image path (e.g., `library/redis`, `valkey/valkey`) |
| `image.tag` | `string` | No | `8.0` | Image version tag |
| `image.pullPolicy` | `string` | No | `IfNotPresent` | `Always`, `IfNotPresent`, `Never` |
| `image.pullSecrets` | `[]LocalObjectReference` | No | `[]` | Pull secret references |

**Resulting image**: `{registry}/{path}:{tag}`

**Default**: `docker.io/valkey/valkey:8.0`

**Examples**:

```yaml
# Default: docker.io/valkey/valkey:8.0
spec:
  image: {}

# Use Redis instead of Valkey
spec:
  image:
    path: library/redis
    # Result: docker.io/library/redis:8.0

# Use a registry mirror (only change registry, keep path)
spec:
  image:
    registry: docker.io
    # Result: docker.io/valkey/valkey:8.0

# Mirror + Redis
spec:
  image:
    registry: docker.io
    path: library/redis
    # Result: docker.io/library/redis:8.0

# Different version
spec:
  image:
    tag: "7.2"
    # Result: docker.io/valkey/valkey:7.2

# Harbor proxy cache (different path structure)
spec:
  image:
    registry: harbor.internal
    path: dockerhub-proxy/valkey/valkey
    # Result: harbor.internal/dockerhub-proxy/valkey/valkey:8.0
```

**Why fully qualified by default?**

Unqualified images (like `redis` without a registry) are deprecated in modern container runtimes (CRI-O, containerd). Using `docker.io` explicitly ensures portability across all Kubernetes environments.

### 2.3 Redis Configuration

```yaml
spec:
  config:
    # Typed, validated fields
    maxmemory: "1Gi"            # Memory limit for Redis
    maxmemoryPolicy: noeviction # Eviction policy
    timeout: 0                   # Client timeout (0 = disabled)
    tcpKeepalive: 300           # TCP keepalive interval

    # Raw config passthrough (expert mode)
    raw: |
      hz 10
      dynamic-hz yes
      slowlog-log-slower-than 10000
```

| Field | Type | Required | Default | Description |
|-------|------|----------|---------|-------------|
| `config.maxmemory` | `string` | No | (from resources) | Redis maxmemory (e.g., `1Gi`, `512Mi`) |
| `config.maxmemoryPolicy` | `string` | No | `noeviction` | Eviction policy |
| `config.timeout` | `int` | No | `0` | Client idle timeout (seconds, 0=disabled) |
| `config.tcpKeepalive` | `int` | No | `300` | TCP keepalive (seconds) |
| `config.raw` | `string` | No | `""` | Raw redis.conf lines (expert mode) |

**Valid `maxmemoryPolicy` values**:
- `noeviction` - Return errors when memory limit reached (default)
- `allkeys-lru` - Evict any key using LRU
- `allkeys-lfu` - Evict any key using LFU
- `allkeys-random` - Evict any key randomly
- `volatile-lru` - Evict keys with TTL using LRU
- `volatile-lfu` - Evict keys with TTL using LFU
- `volatile-random` - Evict keys with TTL randomly
- `volatile-ttl` - Evict keys with shortest TTL

**Persistence Behavior**:

The operator actively disables persistence by default to ensure pure in-memory performance:
```
save ""           # No RDB snapshots
appendonly no     # No AOF
```

This means:
- No PersistentVolumeClaims are created
- No disk I/O for persistence
- Pod restart = clean slate (by design)

If you need persistence, you can override via `spec.config.raw`, but you're responsible for providing appropriate storage. The operator won't create PVCs for you.

### 2.4 Resources

```yaml
spec:
  resources:
    requests:
      cpu: "500m"
      memory: "1Gi"
    limits:
      cpu: "500m"
      memory: "1Gi"
```

| Field | Type | Required | Default | Description |
|-------|------|----------|---------|-------------|
| `resources` | `corev1.ResourceRequirements` | No | See below | CPU/memory for Redis container |

**Default resources** (optimized for Guaranteed QoS):
```yaml
resources:
  requests:
    cpu: "250m"
    memory: "256Mi"
  limits:
    cpu: "250m"
    memory: "256Mi"
```

**Behavior**: If `config.maxmemory` is not set, the operator auto-calculates it as ~90% of `resources.limits.memory` to leave headroom for Redis overhead (buffers, connections, etc.). User can always override with explicit value.

### 2.5 Authentication

```yaml
spec:
  auth:
    enabled: false              # Enable password auth
    existingSecret: ""          # Secret name (must have 'password' key)
```

| Field | Type | Required | Default | Description |
|-------|------|----------|---------|-------------|
| `auth.enabled` | `bool` | No | `false` | Enable password authentication |
| `auth.existingSecret` | `string` | Conditional | `""` | Secret name containing `password` key |

**Validation**:
- If `auth.enabled=true`, `existingSecret` is required
- Inline passwords are not supported (must use Secret)

### 2.6 TLS

```yaml
spec:
  tls:
    enabled: false              # Enable TLS
    existingSecret: ""          # Secret with tls.crt, tls.key
    caCertSecret: ""            # Optional: Secret with ca.crt
    clientAuth: false           # Require client certificates
```

| Field | Type | Required | Default | Description |
|-------|------|----------|---------|-------------|
| `tls.enabled` | `bool` | No | `false` | Enable TLS encryption |
| `tls.existingSecret` | `string` | Conditional | `""` | Secret with `tls.crt` and `tls.key` |
| `tls.caCertSecret` | `string` | No | `""` | Secret with `ca.crt` for client verification |
| `tls.clientAuth` | `bool` | No | `false` | Require client certificate authentication |

**Validation**:
- If `tls.enabled=true`, `existingSecret` is required
- If `tls.clientAuth=true`, `caCertSecret` is required

### 2.7 Metrics

```yaml
spec:
  metrics:
    enabled: true               # Enable metrics exporter
    exporter:
      # Same pattern as main image: {registry}/{path}:{tag}
      registry: ""              # Empty = inherit from spec.image.registry
      path: oliver006/redis_exporter
      tag: v1.66.0
      resources:
        requests:
          cpu: "50m"
          memory: "32Mi"
        limits:
          cpu: "100m"
          memory: "64Mi"
    serviceMonitor:
      enabled: false            # Create ServiceMonitor CR
      namespace: ""             # Override namespace (default: same as CR)
      labels: {}                # Additional labels for ServiceMonitor
      interval: "30s"           # Scrape interval
      scrapeTimeout: "10s"      # Scrape timeout
```

| Field | Type | Required | Default | Description |
|-------|------|----------|---------|-------------|
| `metrics.enabled` | `bool` | No | `true` | Enable redis_exporter sidecar |
| `metrics.exporter.registry` | `string` | No | (inherit) | Registry; empty = use `spec.image.registry` |
| `metrics.exporter.path` | `string` | No | `oliver006/redis_exporter` | Image path |
| `metrics.exporter.tag` | `string` | No | `v1.66.0` | Image tag |
| `metrics.exporter.resources` | `ResourceRequirements` | No | See above | Exporter container resources |
| `metrics.serviceMonitor.enabled` | `bool` | No | `false` | Create ServiceMonitor |
| `metrics.serviceMonitor.namespace` | `string` | No | `""` | ServiceMonitor namespace |
| `metrics.serviceMonitor.labels` | `map[string]string` | No | `{}` | Additional labels |
| `metrics.serviceMonitor.interval` | `string` | No | `30s` | Scrape interval |
| `metrics.serviceMonitor.scrapeTimeout` | `string` | No | `10s` | Scrape timeout |

**Registry inheritance**: If `metrics.exporter.registry` is empty, it inherits from `spec.image.registry`. This means setting a registry mirror once applies to all images:

```yaml
spec:
  image:
    registry: docker.io   # Mirror for all images
  # Exporter automatically uses: docker.io/oliver006/redis_exporter:v1.66.0
```

### 2.8 Update Strategy

```yaml
spec:
  updateStrategy:
    type: RollingUpdate         # Or Recreate
```

| Field | Type | Required | Default | Description |
|-------|------|----------|---------|-------------|
| `updateStrategy.type` | `string` | No | `RollingUpdate` | `RollingUpdate` or `Recreate` |

### 2.9 Service Configuration

```yaml
spec:
  service:
    type: ClusterIP             # Service type
    annotations: {}             # Service annotations
    labels: {}                  # Additional labels
```

| Field | Type | Required | Default | Description |
|-------|------|----------|---------|-------------|
| `service.type` | `string` | No | `ClusterIP` | `ClusterIP`, `NodePort`, `LoadBalancer` |
| `service.annotations` | `map[string]string` | No | `{}` | Annotations |
| `service.labels` | `map[string]string` | No | `{}` | Additional labels |

### 2.10 Pod Template

```yaml
spec:
  podTemplate:
    annotations: {}             # Pod annotations
    labels: {}                  # Additional pod labels
    nodeSelector: {}            # Node selector
    tolerations: []             # Tolerations
    affinity: {}                # Affinity rules
    priorityClassName: ""       # Priority class
    securityContext:            # Pod security context
      runAsNonRoot: true
      runAsUser: 999
      fsGroup: 999
    topologySpreadConstraints: [] # Topology spread (sentinel mode)
```

| Field | Type | Required | Default | Description |
|-------|------|----------|---------|-------------|
| `podTemplate.annotations` | `map[string]string` | No | `{}` | Pod annotations |
| `podTemplate.labels` | `map[string]string` | No | `{}` | Additional pod labels |
| `podTemplate.nodeSelector` | `map[string]string` | No | `{}` | Node selector |
| `podTemplate.tolerations` | `[]Toleration` | No | `[]` | Tolerations |
| `podTemplate.affinity` | `Affinity` | No | `nil` | Affinity/anti-affinity |
| `podTemplate.priorityClassName` | `string` | No | `""` | Priority class name |
| `podTemplate.securityContext` | `PodSecurityContext` | No | See below | Pod security context |
| `podTemplate.topologySpreadConstraints` | `[]TopologySpreadConstraint` | No | `[]` | Topology spread |

**Default security context**:
```yaml
securityContext:
  runAsNonRoot: true
  runAsUser: 999
  runAsGroup: 999
  fsGroup: 999
```

### 2.11 Sentinel-Specific Configuration

Only applicable when `mode: sentinel`:

```yaml
spec:
  sentinel:
    quorum: 2                   # Sentinel quorum for failover
    downAfterMilliseconds: 30000  # Time before marking master down
    failoverTimeout: 180000     # Failover timeout
    parallelSyncs: 1            # Replicas to sync in parallel

    # Sentinel container resources (separate from Redis)
    resources:
      requests:
        cpu: "100m"
        memory: "64Mi"
      limits:
        cpu: "100m"
        memory: "64Mi"
```

| Field | Type | Required | Default | Description |
|-------|------|----------|---------|-------------|
| `sentinel.quorum` | `int` | No | `2` | Sentinels needed to agree on failure |
| `sentinel.downAfterMilliseconds` | `int` | No | `30000` | Time to mark master as down |
| `sentinel.failoverTimeout` | `int` | No | `180000` | Failover timeout |
| `sentinel.parallelSyncs` | `int` | No | `1` | Parallel replica syncs |
| `sentinel.resources` | `ResourceRequirements` | No | See above | Sentinel container resources |

### 2.12 Cluster-Specific Configuration

Only applicable when `mode: cluster`:

```yaml
spec:
  cluster:
    shards: 3                   # Number of master shards (minimum 3)
    replicasPerShard: 1         # Replicas per master (0 = no replicas)
    clusterNodeTimeout: 15000   # Node timeout in milliseconds
```

| Field | Type | Required | Default | Description |
|-------|------|----------|---------|-------------|
| `cluster.shards` | `int` | No | `3` | Number of master shards (min: 3) |
| `cluster.replicasPerShard` | `int` | No | `1` | Replicas per shard (0 = no replicas) |
| `cluster.clusterNodeTimeout` | `int` | No | `15000` | Node timeout in ms |

**Cluster mode creates**:
- Total pods: `shards × (1 + replicasPerShard)`
- Example: 3 shards with 1 replica = 6 pods (3 masters + 3 replicas)
- 16384 hash slots distributed across masters

**Important notes**:
- No PersistentVolumes required - cluster topology stored in CR status
- Data durability through replication, not disk persistence
- Minimum 3 shards required by Redis Cluster protocol

### 2.13 Requeue Intervals

For tuning large-scale installations to reduce API server pressure:

```yaml
spec:
  requeueIntervals:
    fast: "2s"           # During initialization/recovery
    steadyState: "30s"   # Periodic health checks when Running
```

| Field | Type | Required | Default | Description |
|-------|------|----------|---------|-------------|
| `requeueIntervals.fast` | `Duration` | No | `2s` | Interval during init/recovery |
| `requeueIntervals.steadyState` | `Duration` | No | `30s` | Interval when stable |

---

## 3. Status Fields

```yaml
status:
  phase: Running                # Overall phase
  observedGeneration: 1         # Last observed generation
  conditions:                   # Detailed conditions
    - type: Ready
      status: "True"
      reason: AllPodsReady
      message: "All pods are ready"
      lastTransitionTime: "2026-01-30T12:00:00Z"

  # Common fields
  redis:
    ready: 1                    # Ready Redis pods
    total: 1                    # Total Redis pods

  # Sentinel mode only
  master:
    podName: my-cache-redis-0   # Current master pod
    ip: 10.0.0.5                # Current master IP
  replicas:
    ready: 2
    total: 2
  sentinels:
    ready: 3
    total: 3

  # Cluster mode only
  cluster:
    state: ok                   # ok, fail, or initializing
    lastBootstrap: "2026-02-03T12:00:00Z"
    nodes:
      - podName: my-cache-cluster-0
        nodeId: abc123def456...
        role: master
        slotRanges: "0-5460"
      - podName: my-cache-cluster-1
        nodeId: ghi789jkl012...
        role: replica
        masterNodeId: abc123def456...
```

| Field | Type | Description |
|-------|------|-------------|
| `phase` | `string` | `Pending`, `Initializing`, `Running`, `Failed`, `Terminating` |
| `observedGeneration` | `int64` | Last processed `.metadata.generation` |
| `conditions` | `[]Condition` | Detailed status conditions |
| `redis.ready` | `int32` | Ready Redis pod count |
| `redis.total` | `int32` | Total Redis pod count |
| `master.podName` | `string` | Current master pod name (sentinel mode) |
| `master.ip` | `string` | Current master pod IP (sentinel mode) |
| `replicas.ready` | `int32` | Ready replica count (sentinel mode) |
| `replicas.total` | `int32` | Total replica count (sentinel mode) |
| `sentinels.ready` | `int32` | Ready sentinel count (sentinel mode) |
| `sentinels.total` | `int32` | Total sentinel count (sentinel mode) |
| `cluster.state` | `string` | Cluster state: ok, fail, initializing (cluster mode) |
| `cluster.lastBootstrap` | `Time` | Last bootstrap timestamp (cluster mode) |
| `cluster.nodes` | `[]ClusterNodeState` | Per-node state for recovery (cluster mode) |

### 3.1 Condition Types

| Type | Description |
|------|-------------|
| `Ready` | All components are ready and operational |
| `Initialized` | Initial setup complete |
| `ConfigValid` | Configuration is valid |
| `TLSReady` | TLS secrets are valid (if enabled) |
| `AuthReady` | Auth secrets are valid (if enabled) |
| `SentinelReady` | Sentinel quorum established (sentinel mode) |
| `ClusterReady` | Cluster is formed and healthy (cluster mode) |

### 3.2 Phase Transitions

```
           create
              │
              ▼
         ┌─────────┐
         │ Pending │ ◄── Waiting for resources
         └────┬────┘
              │
              ▼
       ┌──────────────┐
       │ Initializing │ ◄── Pods starting
       └──────┬───────┘
              │
              ├─────────────────┐
              ▼                 ▼
         ┌─────────┐       ┌────────┐
         │ Running │       │ Failed │ ◄── Validation error,
         └────┬────┘       └────────┘     pod crash, etc.
              │
              ▼ delete
        ┌─────────────┐
        │ Terminating │
        └─────────────┘
```

---

## 4. Complete Examples

### 4.1 Minimal Standalone

```yaml
apiVersion: littlered.chuck-chuck-chuck.net/v1alpha1
kind: LittleRed
metadata:
  name: my-cache
spec: {}
```

Uses all defaults: standalone mode, `docker.io/valkey/valkey:8.0`, no auth, no TLS, metrics enabled.

### 4.2 Standalone with Custom Resources

```yaml
apiVersion: littlered.chuck-chuck-chuck.net/v1alpha1
kind: LittleRed
metadata:
  name: my-cache
spec:
  resources:
    requests:
      cpu: "1"
      memory: "2Gi"
    limits:
      cpu: "1"
      memory: "2Gi"
  config:
    maxmemoryPolicy: allkeys-lfu
```

### 4.3 Standalone with Redis (instead of Valkey)

```yaml
apiVersion: littlered.chuck-chuck-chuck.net/v1alpha1
kind: LittleRed
metadata:
  name: my-cache
spec:
  image:
    path: library/redis
    # Result: docker.io/library/redis:8.0
```

### 4.4 Registry Mirror

```yaml
apiVersion: littlered.chuck-chuck-chuck.net/v1alpha1
kind: LittleRed
metadata:
  name: my-cache
spec:
  image:
    registry: docker.io
    # Result: docker.io/valkey/valkey:8.0
    # Exporter: docker.io/oliver006/redis_exporter:v1.66.0
```

### 4.5 Standalone with Auth

```yaml
apiVersion: littlered.chuck-chuck-chuck.net/v1alpha1
kind: LittleRed
metadata:
  name: my-cache
spec:
  auth:
    enabled: true
    existingSecret: my-redis-password
---
apiVersion: v1
kind: Secret
metadata:
  name: my-redis-password
type: Opaque
stringData:
  password: "my-secret-password"
```

### 4.6 Standalone with TLS

```yaml
apiVersion: littlered.chuck-chuck-chuck.net/v1alpha1
kind: LittleRed
metadata:
  name: my-cache
spec:
  tls:
    enabled: true
    existingSecret: my-redis-tls
---
apiVersion: v1
kind: Secret
metadata:
  name: my-redis-tls
type: kubernetes.io/tls
data:
  tls.crt: <base64>
  tls.key: <base64>
```

### 4.7 Full Standalone

```yaml
apiVersion: littlered.chuck-chuck-chuck.net/v1alpha1
kind: LittleRed
metadata:
  name: my-cache
  namespace: production
spec:
  mode: standalone

  image:
    registry: docker.io   # Company mirror
    path: library/redis            # Use Redis instead of Valkey
    tag: "8.0"
    pullPolicy: IfNotPresent

  resources:
    requests:
      cpu: "500m"
      memory: "1Gi"
    limits:
      cpu: "500m"
      memory: "1Gi"

  config:
    maxmemory: "900Mi"
    maxmemoryPolicy: noeviction
    timeout: 0
    tcpKeepalive: 300
    raw: |
      hz 10
      dynamic-hz yes

  auth:
    enabled: true
    existingSecret: redis-password

  tls:
    enabled: true
    existingSecret: redis-tls

  metrics:
    enabled: true
    # Exporter inherits registry: docker.io/oliver006/redis_exporter:v1.66.0
    serviceMonitor:
      enabled: true
      labels:
        release: prometheus
      interval: "15s"

  updateStrategy:
    type: RollingUpdate

  service:
    type: ClusterIP
    annotations:
      prometheus.io/scrape: "true"

  podTemplate:
    annotations:
      sidecar.istio.io/inject: "false"
    nodeSelector:
      workload-type: cache
    tolerations:
      - key: "cache-only"
        operator: "Exists"
        effect: "NoSchedule"
    priorityClassName: high-priority
```

### 4.8 Minimal Sentinel

```yaml
apiVersion: littlered.chuck-chuck-chuck.net/v1alpha1
kind: LittleRed
metadata:
  name: my-cache
spec:
  mode: sentinel
```

Deploys: 1 master + 2 replicas + 3 sentinels with defaults (`docker.io/valkey/valkey:8.0`).

### 4.9 Sentinel with Production Settings

```yaml
apiVersion: littlered.chuck-chuck-chuck.net/v1alpha1
kind: LittleRed
metadata:
  name: my-cache
  namespace: production
spec:
  mode: sentinel

  image:
    registry: docker.io
    # Uses default path (valkey/valkey) and tag (8.0)

  resources:
    requests:
      cpu: "1"
      memory: "4Gi"
    limits:
      cpu: "1"
      memory: "4Gi"

  config:
    maxmemory: "3500Mi"
    maxmemoryPolicy: noeviction

  auth:
    enabled: true
    existingSecret: redis-password

  sentinel:
    quorum: 2
    downAfterMilliseconds: 5000
    failoverTimeout: 60000
    resources:
      requests:
        cpu: "100m"
        memory: "128Mi"
      limits:
        cpu: "100m"
        memory: "128Mi"

  metrics:
    enabled: true
    # Exporter inherits registry from image.registry
    serviceMonitor:
      enabled: true
      labels:
        release: prometheus

  podTemplate:
    affinity:
      podAntiAffinity:
        requiredDuringSchedulingIgnoredDuringExecution:
          - labelSelector:
              matchLabels:
                app.kubernetes.io/name: littlered
                app.kubernetes.io/instance: my-cache
            topologyKey: kubernetes.io/hostname
```

### 4.10 Minimal Cluster

```yaml
apiVersion: littlered.chuck-chuck-chuck.net/v1alpha1
kind: LittleRed
metadata:
  name: my-cache
spec:
  mode: cluster
```

Deploys: 3 masters + 3 replicas (6 pods total) with default settings.

### 4.11 Cluster with Custom Shards

```yaml
apiVersion: littlered.chuck-chuck-chuck.net/v1alpha1
kind: LittleRed
metadata:
  name: my-cache
spec:
  mode: cluster
  cluster:
    shards: 6               # 6 masters
    replicasPerShard: 1     # 6 replicas
    clusterNodeTimeout: 10000

  resources:
    requests:
      cpu: "1"
      memory: "2Gi"
    limits:
      cpu: "1"
      memory: "2Gi"

  config:
    maxmemory: "1800Mi"
    maxmemoryPolicy: noeviction

  auth:
    enabled: true
    existingSecret: redis-password

  metrics:
    enabled: true
    serviceMonitor:
      enabled: true
      labels:
        release: prometheus

  podTemplate:
    topologySpreadConstraints:
      - maxSkew: 1
        topologyKey: kubernetes.io/hostname
        whenUnsatisfiable: DoNotSchedule
        labelSelector:
          matchLabels:
            app.kubernetes.io/instance: my-cache
```

---

## 5. Labels and Annotations

### 5.1 Standard Labels (Applied by Operator)

```yaml
labels:
  app.kubernetes.io/name: littlered
  app.kubernetes.io/instance: my-cache
  app.kubernetes.io/component: redis | sentinel | exporter
  app.kubernetes.io/managed-by: littlered-operator
  app.kubernetes.io/version: "8.0"
  littlered.chuck-chuck-chuck.net/mode: standalone | sentinel | cluster
```

### 5.2 Sentinel Mode Labels

```yaml
# On Redis pods (dynamic, updated on failover)
labels:
  littlered.chuck-chuck-chuck.net/role: master | replica
```

---

## 6. Go Types (Reference)

```go
// LittleRedSpec defines the desired state of LittleRed
type LittleRedSpec struct {
    // Mode is the deployment mode: standalone, sentinel, or cluster
    // +kubebuilder:validation:Enum=standalone;sentinel;cluster
    // +kubebuilder:default=standalone
    Mode string `json:"mode,omitempty"`

    // Image defines the container image to use
    Image ImageSpec `json:"image,omitempty"`

    // Resources defines CPU/memory for Redis container
    Resources corev1.ResourceRequirements `json:"resources,omitempty"`

    // Config defines Redis configuration
    Config ConfigSpec `json:"config,omitempty"`

    // Auth defines authentication settings
    Auth AuthSpec `json:"auth,omitempty"`

    // TLS defines TLS/SSL settings
    TLS TLSSpec `json:"tls,omitempty"`

    // Metrics defines Prometheus metrics settings
    Metrics MetricsSpec `json:"metrics,omitempty"`

    // UpdateStrategy defines how updates are rolled out
    UpdateStrategy UpdateStrategySpec `json:"updateStrategy,omitempty"`

    // Service defines Service configuration
    Service ServiceSpec `json:"service,omitempty"`

    // PodTemplate defines pod customizations
    PodTemplate PodTemplateSpec `json:"podTemplate,omitempty"`

    // Sentinel defines sentinel-specific settings (sentinel mode only)
    Sentinel *SentinelSpec `json:"sentinel,omitempty"`

    // Cluster defines cluster-specific settings (cluster mode only)
    Cluster *ClusterSpec `json:"cluster,omitempty"`

    // RequeueIntervals for tuning reconciliation frequency
    RequeueIntervals *RequeueIntervals `json:"requeueIntervals,omitempty"`
}

type ImageSpec struct {
    // Registry is the container registry hostname
    // +kubebuilder:default=docker.io
    Registry string `json:"registry,omitempty"`

    // Path is the image path (without registry or tag)
    // +kubebuilder:default=valkey/valkey
    Path string `json:"path,omitempty"`

    // Tag is the image version tag
    // +kubebuilder:default="8.0"
    Tag string `json:"tag,omitempty"`

    // PullPolicy is the image pull policy
    // +kubebuilder:validation:Enum=Always;IfNotPresent;Never
    // +kubebuilder:default=IfNotPresent
    PullPolicy corev1.PullPolicy `json:"pullPolicy,omitempty"`

    // PullSecrets are references to secrets for pulling the image
    PullSecrets []corev1.LocalObjectReference `json:"pullSecrets,omitempty"`
}

// FullImage returns the complete image reference: {registry}/{path}:{tag}
func (i *ImageSpec) FullImage() string {
    return fmt.Sprintf("%s/%s:%s", i.Registry, i.Path, i.Tag)
}

type ConfigSpec struct {
    // Maxmemory sets Redis maxmemory (e.g., "1Gi")
    Maxmemory string `json:"maxmemory,omitempty"`

    // MaxmemoryPolicy sets the eviction policy
    // +kubebuilder:validation:Enum=noeviction;allkeys-lru;allkeys-lfu;allkeys-random;volatile-lru;volatile-lfu;volatile-random;volatile-ttl
    // +kubebuilder:default=noeviction
    MaxmemoryPolicy string `json:"maxmemoryPolicy,omitempty"`

    // Timeout is client idle timeout in seconds (0 = disabled)
    // +kubebuilder:default=0
    Timeout int `json:"timeout,omitempty"`

    // TCPKeepalive interval in seconds
    // +kubebuilder:default=300
    TCPKeepalive int `json:"tcpKeepalive,omitempty"`

    // Raw is raw redis.conf content (expert mode)
    Raw string `json:"raw,omitempty"`
}

// ... additional types follow same pattern
```

---

## 7. Validation Rules

### 7.1 Controller-Level Validation

| Rule | Error Condition |
|------|-----------------|
| `mode` must be `standalone`, `sentinel`, or `cluster` | Invalid mode value |
| If `auth.enabled`, must have `existingSecret` | Missing authentication secret |
| If `tls.enabled`, must have `existingSecret` | Missing TLS certificate |
| If `tls.clientAuth`, must have `caCertSecret` | Missing CA certificate |
| `sentinel` config ignored if `mode=standalone` | Warning in status |
| `cluster.shards` must be >= 3 | Minimum 3 shards required |
| `cluster.replicasPerShard` must be >= 0 | Non-negative value required |
| `maxmemory` must parse as quantity | Invalid memory format |

### 7.2 Status Condition on Validation Failure

```yaml
status:
  phase: Failed
  conditions:
    - type: ConfigValid
      status: "False"
      reason: ValidationFailed
      message: "auth.enabled is true but no secret configured"
```
