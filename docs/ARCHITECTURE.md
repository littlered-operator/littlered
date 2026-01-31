# LittleRed - Architecture Document

> Technical architecture for the LittleRed Kubernetes operator.

**Document Status**: Draft
**Last Updated**: 2026-01-30

---

## 1. High-Level Overview

```
┌─────────────────────────────────────────────────────────────────┐
│                     Kubernetes Cluster                          │
│                                                                  │
│  ┌──────────────────────────────────────────────────────────┐   │
│  │                  LittleRed Operator                       │   │
│  │  ┌─────────────────┐  ┌─────────────────────────────┐    │   │
│  │  │ Controller      │  │ Webhooks (optional)          │    │   │
│  │  │ Manager         │  │ - Validation                 │    │   │
│  │  │                 │  │ - Defaulting                 │    │   │
│  │  └────────┬────────┘  └─────────────────────────────┘    │   │
│  │           │                                               │   │
│  │           ▼                                               │   │
│  │  ┌─────────────────────────────────────────────────┐     │   │
│  │  │              Reconcilers                         │     │   │
│  │  │  ┌───────────────┐  ┌───────────────────────┐   │     │   │
│  │  │  │ Standalone    │  │ Sentinel              │   │     │   │
│  │  │  │ Reconciler    │  │ Reconciler            │   │     │   │
│  │  │  └───────────────┘  └───────────────────────┘   │     │   │
│  │  └─────────────────────────────────────────────────┘     │   │
│  └──────────────────────────────────────────────────────────┘   │
│                              │                                   │
│                              │ watches/manages                   │
│                              ▼                                   │
│  ┌──────────────────────────────────────────────────────────┐   │
│  │                    Managed Resources                      │   │
│  │                                                           │   │
│  │  ┌─────────────┐ ┌─────────────┐ ┌─────────────────────┐ │   │
│  │  │ StatefulSet │ │ Services    │ │ ConfigMaps          │ │   │
│  │  │ (Redis)     │ │             │ │ (redis.conf)        │ │   │
│  │  └─────────────┘ └─────────────┘ └─────────────────────┘ │   │
│  │                                                           │   │
│  │  ┌─────────────┐ ┌─────────────────────────────────────┐ │   │
│  │  │ Secrets     │ │ ServiceMonitor (optional)           │ │   │
│  │  │ (auth/TLS)  │ │                                     │ │   │
│  │  └─────────────┘ └─────────────────────────────────────┘ │   │
│  └──────────────────────────────────────────────────────────┘   │
└─────────────────────────────────────────────────────────────────┘
```

---

## 2. CRD Design

### 2.1 API Structure

```yaml
apiVersion: littlered.tanne3.de/v1alpha1
kind: LittleRed
metadata:
  name: my-cache
  namespace: default
spec:
  # Mode selection
  mode: standalone | sentinel  # Required, default: standalone

  # Image configuration
  image:
    repository: redis          # or valkey/valkey
    tag: "7.2"
    pullPolicy: IfNotPresent
    pullSecrets: []

  # Redis configuration
  config:
    # Curated settings (typed, validated)
    maxmemory: "1gb"
    maxmemoryPolicy: allkeys-lru
    # Raw config passthrough (escape hatch)
    raw: |
      tcp-keepalive 300
      hz 10

  # Resources
  resources:
    requests:
      cpu: "500m"
      memory: "1Gi"
    limits:
      cpu: "500m"
      memory: "1Gi"

  # Authentication
  auth:
    enabled: false
    secretName: ""           # Secret containing 'password' key

  # TLS
  tls:
    enabled: false
    secretName: ""           # Secret containing tls.crt, tls.key, ca.crt

  # Metrics
  metrics:
    enabled: true
    exporter:
      image: oliver006/redis_exporter:v1.x
      resources: {}
    serviceMonitor:
      enabled: false
      namespace: ""          # Override namespace for ServiceMonitor
      labels: {}             # Additional labels
      interval: 30s

  # Update strategy (sentinel mode only uses this for rolling)
  updateStrategy: RollingUpdate | Recreate

  # Pod configuration
  podTemplate:
    annotations: {}
    labels: {}
    nodeSelector: {}
    tolerations: []
    affinity: {}
    securityContext: {}

  # Service configuration
  service:
    type: ClusterIP
    annotations: {}

status:
  phase: Pending | Initializing | Running | Failed
  conditions:
    - type: Ready
      status: "True" | "False" | "Unknown"
      reason: ""
      message: ""
      lastTransitionTime: ""
  master:                    # Sentinel mode only
    podName: ""
    podIP: ""
  replicas:                  # Sentinel mode only
    ready: 0
    total: 0
  sentinels:                 # Sentinel mode only
    ready: 0
    total: 0
  observedGeneration: 0
```

### 2.2 CRD Versioning Strategy

| Version | Status | Notes |
|---------|--------|-------|
| `v1alpha1` | Initial | Breaking changes allowed |
| `v1beta1` | Future | Stabilized API, deprecation warnings |
| `v1` | Future | Stable, full compatibility guarantees |

---

## 3. Component Architecture

### 3.1 Standalone Mode

```
┌─────────────────────────────────────────────────────────────┐
│                    Namespace: user-ns                        │
│                                                              │
│  ┌────────────────────────────────────────────────────────┐ │
│  │           StatefulSet: {name}                          │ │
│  │  ┌──────────────────────────────────────────────────┐  │ │
│  │  │                   Pod: {name}-0                   │  │ │
│  │  │  ┌─────────────────┐  ┌────────────────────────┐ │  │ │
│  │  │  │ Container:      │  │ Container:             │ │  │ │
│  │  │  │ redis           │  │ redis-exporter         │ │  │ │
│  │  │  │                 │  │                        │ │  │ │
│  │  │  │ Port: 6379      │  │ Port: 9121             │ │  │ │
│  │  │  └─────────────────┘  └────────────────────────┘ │  │ │
│  │  └──────────────────────────────────────────────────┘  │ │
│  └────────────────────────────────────────────────────────┘ │
│                                                              │
│  ┌────────────────────────┐  ┌────────────────────────────┐ │
│  │ Service: {name}        │  │ ConfigMap: {name}-config   │ │
│  │ Port: 6379 → 6379      │  │ - redis.conf               │ │
│  │ Port: 9121 → 9121      │  │                            │ │
│  └────────────────────────┘  └────────────────────────────┘ │
│                                                              │
│  ┌────────────────────────────────────────────────────────┐ │
│  │ ServiceMonitor: {name} (optional)                      │ │
│  └────────────────────────────────────────────────────────┘ │
└─────────────────────────────────────────────────────────────┘
```

**Resource Naming Convention**:
- StatefulSet: `{cr-name}`
- Service: `{cr-name}`
- ConfigMap: `{cr-name}-config`
- ServiceMonitor: `{cr-name}`

### 3.2 Sentinel Mode

```
┌─────────────────────────────────────────────────────────────────────┐
│                        Namespace: user-ns                            │
│                                                                      │
│  ┌────────────────────────────────────────────────────────────────┐ │
│  │              StatefulSet: {name}-redis (replicas: 3)           │ │
│  │                                                                 │ │
│  │  ┌──────────────┐  ┌──────────────┐  ┌──────────────┐          │ │
│  │  │ Pod: -0      │  │ Pod: -1      │  │ Pod: -2      │          │ │
│  │  │ (master)     │  │ (replica)    │  │ (replica)    │          │ │
│  │  │              │  │              │  │              │          │ │
│  │  │ redis:6379   │  │ redis:6379   │  │ redis:6379   │          │ │
│  │  │ export:9121  │  │ export:9121  │  │ export:9121  │          │ │
│  │  └──────────────┘  └──────────────┘  └──────────────┘          │ │
│  └────────────────────────────────────────────────────────────────┘ │
│                                                                      │
│  ┌────────────────────────────────────────────────────────────────┐ │
│  │              StatefulSet: {name}-sentinel (replicas: 3)        │ │
│  │                                                                 │ │
│  │  ┌──────────────┐  ┌──────────────┐  ┌──────────────┐          │ │
│  │  │ Pod: -0      │  │ Pod: -1      │  │ Pod: -2      │          │ │
│  │  │ sentinel     │  │ sentinel     │  │ sentinel     │          │ │
│  │  │ :26379       │  │ :26379       │  │ :26379       │          │ │
│  │  └──────────────┘  └──────────────┘  └──────────────┘          │ │
│  └────────────────────────────────────────────────────────────────┘ │
│                                                                      │
│  ┌─────────────────────┐  ┌─────────────────────┐                   │
│  │ Service:            │  │ Service:            │                   │
│  │ {name}-master       │  │ {name}-replicas     │                   │
│  │ (to current master) │  │ (headless, all)     │                   │
│  └─────────────────────┘  └─────────────────────┘                   │
│                                                                      │
│  ┌─────────────────────┐  ┌─────────────────────────────────────┐   │
│  │ Service:            │  │ ConfigMap: {name}-config            │   │
│  │ {name}-sentinel     │  │ ConfigMap: {name}-sentinel-config   │   │
│  └─────────────────────┘  └─────────────────────────────────────┘   │
└─────────────────────────────────────────────────────────────────────┘
```

**Sentinel Mode Resources**:
- StatefulSet: `{name}-redis` (3 replicas: 1 master + 2 replicas)
- StatefulSet: `{name}-sentinel` (3 replicas)
- Service: `{name}-master` (ClusterIP, points to current master)
- Service: `{name}-replicas` (headless, all Redis pods)
- Service: `{name}-sentinel` (headless, all Sentinel pods)
- ConfigMap: `{name}-config` (redis.conf)
- ConfigMap: `{name}-sentinel-config` (sentinel.conf)

---

## 4. Reconciliation Flow

### 4.1 Standalone Reconciler

```
┌────────────────────────────────────────────────────────────────┐
│                    Reconcile(LittleRed)                         │
└───────────────────────────┬────────────────────────────────────┘
                            │
                            ▼
                ┌───────────────────────┐
                │ Validate spec         │
                │ (mode == standalone?) │
                └───────────┬───────────┘
                            │
                            ▼
                ┌───────────────────────┐
                │ Reconcile ConfigMap   │──────────────────┐
                │ (redis.conf)          │                  │
                └───────────┬───────────┘                  │
                            │                              │
                            ▼                              │ Create/Update
                ┌───────────────────────┐                  │ if not exists
                │ Reconcile Service     │──────────────────┤ or changed
                └───────────┬───────────┘                  │
                            │                              │
                            ▼                              │
                ┌───────────────────────┐                  │
                │ Reconcile StatefulSet │──────────────────┤
                └───────────┬───────────┘                  │
                            │                              │
                            ▼                              │
                ┌───────────────────────┐                  │
                │ Reconcile             │──────────────────┘
                │ ServiceMonitor (opt)  │
                └───────────┬───────────┘
                            │
                            ▼
                ┌───────────────────────┐
                │ Update CR Status      │
                │ - phase               │
                │ - conditions          │
                └───────────────────────┘
```

### 4.2 Sentinel Reconciler

```
┌────────────────────────────────────────────────────────────────┐
│                    Reconcile(LittleRed)                         │
└───────────────────────────┬────────────────────────────────────┘
                            │
                            ▼
                ┌───────────────────────┐
                │ Validate spec         │
                │ (mode == sentinel?)   │
                └───────────┬───────────┘
                            │
                            ▼
                ┌───────────────────────┐
                │ Reconcile ConfigMaps  │
                │ - redis.conf          │
                │ - sentinel.conf       │
                └───────────┬───────────┘
                            │
                            ▼
                ┌───────────────────────┐
                │ Reconcile Services    │
                │ - master              │
                │ - replicas (headless) │
                │ - sentinel (headless) │
                └───────────┬───────────┘
                            │
                            ▼
                ┌───────────────────────┐
                │ Reconcile Redis       │
                │ StatefulSet           │
                └───────────┬───────────┘
                            │
                            ▼
                ┌───────────────────────┐
                │ Bootstrap master      │◄─── Only on initial
                │ (if first deploy)     │     deployment
                └───────────┬───────────┘
                            │
                            ▼
                ┌───────────────────────┐
                │ Reconcile Sentinel    │
                │ StatefulSet           │
                └───────────┬───────────┘
                            │
                            ▼
                ┌───────────────────────┐
                │ Discover current      │◄─── Query sentinels
                │ master                │     for master info
                └───────────┬───────────┘
                            │
                            ▼
                ┌───────────────────────┐
                │ Update master Service │◄─── Point to actual
                │ endpoints             │     master pod
                └───────────┬───────────┘
                            │
                            ▼
                ┌───────────────────────┐
                │ Reconcile             │
                │ ServiceMonitor (opt)  │
                └───────────┬───────────┘
                            │
                            ▼
                ┌───────────────────────┐
                │ Update CR Status      │
                │ - phase               │
                │ - master info         │
                │ - replica counts      │
                │ - sentinel counts     │
                └───────────────────────┘
```

### 4.3 Reconciliation Triggers

| Event | Action |
|-------|--------|
| CR created | Full reconciliation |
| CR updated | Diff-based reconciliation |
| CR deleted | Cleanup owned resources (OwnerReference) |
| Pod deleted | StatefulSet recreates, operator updates status |
| Periodic resync | Verify state matches desired (default: 10h) |

---

## 5. Master Service Management (Sentinel Mode)

One of the key challenges is keeping the `{name}-master` Service pointing to the current master.

### 5.1 Approach: Endpoint Management

```
┌─────────────────────────────────────────────────────────────┐
│                     Reconciliation Loop                      │
│                                                              │
│  1. Query any Sentinel: SENTINEL get-master-addr-by-name    │
│                              │                               │
│                              ▼                               │
│  2. Get current master IP                                    │
│                              │                               │
│                              ▼                               │
│  3. Find Pod with that IP                                    │
│                              │                               │
│                              ▼                               │
│  4. Update Service selector or Endpoints to match           │
│                                                              │
└─────────────────────────────────────────────────────────────┘
```

**Options for master service**:

| Option | Pros | Cons |
|--------|------|------|
| **A: Selector-based** | Simple, K8s-native | Can't dynamically change selector |
| **B: Manual Endpoints** | Full control | More code, manage Endpoints directly |
| **C: Label on pod** | Elegant, selector works | Need to update pod labels |

**Recommended: Option C** - Add a label `littlered.tanne3.de/role: master` to the master pod, update it on failover. Service selector uses this label.

### 5.2 Failover Detection

The operator doesn't perform failover—Sentinel does. The operator only needs to:
1. Detect that failover occurred (master changed)
2. Update the `role` label on pods
3. Update CR status

Detection happens during reconciliation by querying Sentinel.

---

## 6. Configuration Management

### 6.1 Default redis.conf

```
# LittleRed defaults - optimized for in-memory caching
# IMPORTANT: Persistence is ACTIVELY DISABLED to ensure pure cache behavior

# Networking
bind 0.0.0.0
port 6379
tcp-backlog 511
tcp-keepalive 300

# Memory
maxmemory-policy allkeys-lru

# Persistence - ACTIVELY DISABLED
# These settings override any defaults from the Redis/Valkey image
save ""                    # Disable RDB snapshots entirely
appendonly no              # Disable AOF persistence
rdbcompression no          # No compression (nothing to compress)
rdbchecksum no             # No checksum (nothing to write)

# Performance
hz 10
dynamic-hz yes

# Limits
maxclients 10000
```

**Persistence Guarantee**: The operator explicitly disables persistence to ensure:
- No PVCs are needed
- No disk I/O overhead
- No background save processes
- Pod restart = clean slate (cache behavior)

This overrides any persistence defaults that might be baked into Redis/Valkey container images.

### 6.2 Config Merging Strategy

```
┌─────────────────────────────────────────────────────────────┐
│                    Configuration Layers                      │
│                                                              │
│  ┌─────────────────────────────────────────────────────┐    │
│  │ 1. Hardcoded base (non-overridable)                 │    │
│  │    - daemonize no (required for container)          │    │
│  │    - dir /data                                      │    │
│  └─────────────────────────────────────────────────────┘    │
│                          │                                   │
│                          ▼                                   │
│  ┌─────────────────────────────────────────────────────┐    │
│  │ 2. Cache-optimized defaults (override image defaults)│    │
│  │    - maxmemory-policy allkeys-lru                   │    │
│  │    - save ""           ◄── ACTIVELY DISABLE RDB     │    │
│  │    - appendonly no     ◄── ACTIVELY DISABLE AOF     │    │
│  │    - rdbcompression no                              │    │
│  │    - rdbchecksum no                                 │    │
│  │    These ensure pure in-memory behavior regardless  │    │
│  │    of what the upstream image might default to.     │    │
│  └─────────────────────────────────────────────────────┘    │
│                          │                                   │
│                          ▼ (merged, user overrides win)      │
│  ┌─────────────────────────────────────────────────────┐    │
│  │ 3. User config (spec.config.*)                      │    │
│  │    - Typed fields (validated)                       │    │
│  │    - Raw passthrough (expert mode - can re-enable   │    │
│  │      persistence if user really wants it)           │    │
│  └─────────────────────────────────────────────────────┘    │
│                          │                                   │
│                          ▼                                   │
│  ┌─────────────────────────────────────────────────────┐    │
│  │ 4. Final redis.conf                                 │    │
│  └─────────────────────────────────────────────────────┘    │
└─────────────────────────────────────────────────────────────┘
```

**Key Point**: Layer 2 actively overrides any persistence defaults from upstream images. This guarantees that out-of-the-box, LittleRed is a pure in-memory cache with no disk dependencies.

---

## 7. Security Considerations

### 7.1 Pod Security

```yaml
securityContext:
  runAsNonRoot: true
  runAsUser: 999       # redis user
  runAsGroup: 999
  fsGroup: 999
  allowPrivilegeEscalation: false
  readOnlyRootFilesystem: true  # May need /data writable for temp files
  capabilities:
    drop:
      - ALL
```

### 7.2 Network Security

- Services are ClusterIP by default (no external exposure)
- TLS available for transport encryption
- No network policies created by operator (user responsibility)

### 7.3 Secret Handling

- Password read from Secret, not stored in CR
- TLS certs read from Secret
- Operator uses RBAC to read Secrets in target namespace

---

## 8. Operator Deployment

### 8.1 Deployment Architecture

```
┌─────────────────────────────────────────────────────────────┐
│              Namespace: littlered-system                     │
│                                                              │
│  ┌────────────────────────────────────────────────────────┐ │
│  │             Deployment: littlered-operator              │ │
│  │                                                         │ │
│  │  ┌─────────────────────────────────────────────────┐   │ │
│  │  │              Pod: operator                       │   │ │
│  │  │  ┌─────────────────────────────────────────┐    │   │ │
│  │  │  │ Container: manager                       │    │   │ │
│  │  │  │ - Controller Manager                     │    │   │ │
│  │  │  │ - Health probes: :8081                   │    │   │ │
│  │  │  │ - Metrics: :8080                         │    │   │ │
│  │  │  └─────────────────────────────────────────┘    │   │ │
│  │  └─────────────────────────────────────────────────┘   │ │
│  └────────────────────────────────────────────────────────┘ │
│                                                              │
│  ┌─────────────────────┐  ┌─────────────────────────────┐   │
│  │ ServiceAccount      │  │ Service: metrics            │   │
│  │ ClusterRole         │  │ ServiceMonitor (optional)   │   │
│  │ ClusterRoleBinding  │  └─────────────────────────────┘   │
│  └─────────────────────┘                                    │
└─────────────────────────────────────────────────────────────┘
```

### 8.2 RBAC Requirements

**Cluster-scoped mode**:
```yaml
rules:
  - apiGroups: ["littlered.tanne3.de"]
    resources: ["littlereds", "littlereds/status", "littlereds/finalizers"]
    verbs: ["*"]
  - apiGroups: [""]
    resources: ["configmaps", "services", "secrets", "pods", "endpoints"]
    verbs: ["*"]
  - apiGroups: ["apps"]
    resources: ["statefulsets"]
    verbs: ["*"]
  - apiGroups: ["monitoring.coreos.com"]
    resources: ["servicemonitors"]
    verbs: ["*"]
```

**Namespace-scoped mode**: Same rules, but Role instead of ClusterRole.

---

## 9. Project Structure

```
littlered/
├── api/
│   └── v1alpha1/
│       ├── littlered_types.go      # CRD struct definitions
│       ├── littlered_webhook.go    # Validation/defaulting webhooks
│       └── zz_generated.deepcopy.go
├── cmd/
│   └── main.go                     # Entrypoint
├── config/
│   ├── crd/                        # Generated CRD YAML
│   ├── manager/                    # Operator deployment
│   ├── rbac/                       # RBAC manifests
│   └── samples/                    # Example CRs
├── internal/
│   ├── controller/
│   │   ├── littlered_controller.go # Main reconciler
│   │   ├── standalone.go           # Standalone mode logic
│   │   └── sentinel.go             # Sentinel mode logic
│   ├── resources/
│   │   ├── configmap.go            # ConfigMap builder
│   │   ├── statefulset.go          # StatefulSet builder
│   │   ├── service.go              # Service builder
│   │   └── servicemonitor.go       # ServiceMonitor builder
│   └── redis/
│       └── client.go               # Redis/Sentinel client wrapper
├── charts/
│   └── littlered/                  # Helm chart
│       ├── Chart.yaml
│       ├── values.yaml
│       └── templates/
├── test/
│   ├── e2e/                        # End-to-end tests
│   └── fixtures/                   # Test CRs
├── docs/
│   ├── REQUIREMENTS.md
│   ├── ARCHITECTURE.md
│   ├── SCOPE.md
│   └── TEST_CASES.md
├── Dockerfile
├── Makefile
├── go.mod
└── README.md
```

---

## 10. Dependencies

| Dependency | Purpose | Version |
|------------|---------|---------|
| controller-runtime | K8s controller framework | latest stable |
| client-go | K8s API client | matches K8s version |
| redis (go-redis) | Redis client for health checks | v9 |
| zap | Structured logging | via controller-runtime |
| prometheus/client_golang | Operator metrics | latest |

---

## 11. Design Decisions

| Question | Decision | Rationale |
|----------|----------|-----------|
| Use admission webhooks? | **No** (initially) | Operational complexity, cert management burden. Validate in controller, report via status. Add later if needed. |
| Leader election? | **Yes** | Required for HA operator deployments |
| Finalizers? | **TBD** | Evaluate during implementation |
| Watch Secrets for rotation? | **Yes** | Auto-reconcile on password/cert changes |

---

## 12. Diagrams Key

```
┌─────────┐   Box: Component/Resource
└─────────┘

    │
    ▼         Arrow: Flow direction

─────────    Line: Connection/relationship

◄────────    Arrow with reference: Annotation/note
```
