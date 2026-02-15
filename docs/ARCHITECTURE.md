# LittleRed - Architecture Document

> Technical architecture for the LittleRed Kubernetes operator.

**Document Status**: Draft
**Last Updated**: 2026-02-11

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
│  │  │                      ┌───────────────────────┐   │     │   │
│  │  │                      │ Cluster               │   │     │   │
│  │  │                      │ Reconciler            │   │     │   │
│  │  │                      └───────────────────────┘   │     │   │
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
apiVersion: chuck-chuck-chuck.net/v1alpha1
kind: LittleRed
metadata:
  name: my-cache
  namespace: default
spec:
  # Mode selection
  mode: standalone | sentinel | cluster  # Required, default: standalone

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
    maxmemoryPolicy: noeviction
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

  # Cluster mode settings
  cluster:
    shards: 3               # Minimum 3
    replicasPerShard: 1
    clusterNodeTimeout: 15000

status:
  phase: Pending | Initializing | Running | Failed
  bootstrapRequired: true    # Sentinel mode only
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
  cluster:                   # Cluster mode only
    state: ok
    nodes: []
    lastBootstrap: ""
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

### 3.2.1 Safe Bootstrap (Sentinel Mode)

To prevent data loss in pure in-memory mode (where an empty master restarting could wipe live replicas), LittleRed implements a strict "Operator-Authorized" bootstrap mechanism:

1.  **Operator Intent**: On CR creation (or total cluster loss), the operator sets `status.bootstrapRequired: true`.
2.  **Pod Strategy (Wait-Loop)**:
    *   **All** Redis pods start with a shell script that enters a loop.
    *   Inside the loop, the pod uses `redis-cli` to query the Sentinels for the current master.
    *   The pod **refuses** to start `redis-server` until Sentinel returns a valid master address.
3.  **Operator Strategy (Authorization)**:
    *   The operator monitors `redis-0`. Once `redis-0` has been assigned a **PodIP**, the operator identifies it as the "seed" master.
    *   The operator issues a `SENTINEL MONITOR` command to the Sentinels, registering `redis-0`'s **Pod IP** as the master.
4.  **Handshake**:
    *   `redis-0` sees its own **Pod IP** in the Sentinel reply. It exits the loop and starts as **master**.
    *   `redis-1` and `redis-2` see the master in Sentinel, exit their loops, and start as **replicas**.
5.  **Completion**: Once all pods are ready and the cluster is healthy, the operator clears `status.bootstrapRequired: false`.

### 3.2.2 Strict IP-Only Identity

For pure in-memory mode, LittleRed enforces **IP-based identity** instead of stable hostnames (FQDNs).

**Rationale**:
- **Identity Coupling**: In K8s, a StatefulSet Pod keeps its name across restarts but gets a **new IP**.
- **Data Loss Protection**: Since we are pure in-memory, a restarted pod is an **empty node**. If we used hostnames, Sentinel might recognize the restarted `redis-0` as the previous master and re-connect to it, leading to a "Ghost Master" scenario where an empty node accepts writes.
- **Race Prevention**: Ephemeral IPs ensure that a restarted pod is treated as a "stranger" node. Sentinel will have already promoted a replica, and the restarted pod will be forced to join as a replica and sync from the new master.
- **DNS Resilience**: Eliminates dependency on CoreDNS resolution during critical failover windows.

**Implementation**:
- `sentinel announce-hostnames no` and `sentinel resolve-hostnames no` are set.
- The operator strictly maps Sentinel-reported IPs to Pod names via the K8s API.
- All internal communication (Operator → Sentinel, Pod → Sentinel) uses Pod IPs where possible.

---

## 3.3 Cluster Mode

```
┌─────────────────────────────────────────────────────────────────────┐
│                        Namespace: user-ns                            │
│                                                                      │
│  ┌────────────────────────────────────────────────────────────────┐ │
│  │  StatefulSet: {name}-cluster (replicas: shards × (1+replicasPerShard)) │ │
│  │                                                                 │ │
│  │  Example: 3 shards, 1 replica per shard = 6 pods                │ │
│  │  ┌──────────────┐  ┌──────────────┐  ┌──────────────┐          │ │
│  │  │ Pod: -0      │  │ Pod: -1      │  │ Pod: -2      │          │ │
│  │  │ (shard-0     │  │ (shard-0     │  │ (shard-1     │          │ │
│  │  │  master)     │  │  replica)    │  │  master)     │          │ │
│  │  │ slots 0-5460 │  │              │  │ slots 5461-  │          │ │
│  │  │              │  │              │  │ 10922        │          │ │
│  │  │ redis:6379   │  │ redis:6379   │  │ redis:6379   │          │ │
│  │  │ bus:16379    │  │ bus:16379    │  │ bus:16379    │          │ │
│  │  │ export:9121  │  │ export:9121  │  │ export:9121  │          │ │
│  │  └──────────────┘  └──────────────┘  └──────────────┘          │ │
│  │  ┌──────────────┐  ┌──────────────┐  ┌──────────────┐          │ │
│  │  │ Pod: -3      │  │ Pod: -4      │  │ Pod: -5      │          │ │
│  │  │ (shard-1     │  │ (shard-2     │  │ (shard-2     │          │ │
│  │  │  replica)    │  │  master)     │  │  replica)    │          │ │
│  │  │              │  │ slots 10923- │  │              │          │ │
│  │  │              │  │ 16383        │  │              │          │ │
│  │  └──────────────┘  └──────────────┘  └──────────────┘          │ │
│  └────────────────────────────────────────────────────────────────┘ │
│                                                                      │
│  ┌─────────────────────┐  ┌─────────────────────────────────────┐   │
│  │ Service:            │  │ Service: {name}-cluster             │   │
│  │ {name}              │  │ (headless for cluster gossip)       │   │
│  │ (ClusterIP, client) │  │ Ports: 6379, 16379                  │   │
│  └─────────────────────┘  └─────────────────────────────────────┘   │
│                                                                      │
│  ┌─────────────────────────────────────────────────────────────┐   │
│  │ ConfigMap: {name}-cluster-config                            │   │
│  └─────────────────────────────────────────────────────────────┘   │
└─────────────────────────────────────────────────────────────────────┘
```

**Cluster Mode Resources**:
- StatefulSet: `{name}-cluster` (shards × (1 + replicasPerShard) pods)
- Service: `{name}` (ClusterIP, client access)
- Service: `{name}-cluster` (headless, cluster gossip)
- ConfigMap: `{name}-cluster-config` (redis.conf with cluster settings)

**Key design decisions**:
- No PersistentVolumes: Cluster topology stored in CR status, not nodes.conf
- Data durability through replication, not disk persistence
- Operator manages cluster formation, slot assignment, and recovery
- Port 16379 used for cluster bus communication

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
                │ Reconcile Pod RBAC    │◄─── SA, Role, Binding
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
                │ Bootstrap master      │◄─── Wait for redis-0 PodIP,
                │ (if first deploy)     │     issue SENTINEL MONITOR
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

### 4.3 Cluster Reconciler

```
┌────────────────────────────────────────────────────────────────┐
│                    Reconcile(LittleRed)                         │
└───────────────────────────┬────────────────────────────────────┘
                            │
                            ▼
                ┌───────────────────────┐
                │ Validate spec         │
                │ (mode == cluster?)    │
                └───────────┬───────────┘
                            │
                            ▼
                ┌───────────────────────┐
                │ Reconcile ConfigMap   │
                │ (cluster-enabled)     │
                └───────────┬───────────┘
                            │
                            ▼
                ┌───────────────────────┐
                │ Reconcile Services    │
                │ - client              │
                │ - headless (cluster)  │
                └───────────┬───────────┘
                            │
                            ▼
                ┌───────────────────────┐
                │ Reconcile StatefulSet │
                │ (shards × replicas)   │
                └───────────┬───────────┘
                            │
                            ▼
                ┌───────────────────────┐
                │ Wait for all pods     │◄─── All nodes must be
                │ ready                 │     running before
                └───────────┬───────────┘     cluster formation
                            │
                            ▼
                ┌───────────────────────┐
                │ Gather ground truth   │◄─── Query each node for
                │ (actual cluster state)│     CLUSTER INFO/NODES
                └───────────┬───────────┘
                            │
                            ▼
                ┌───────────────────────┐
                │ Cluster healthy?      │
                └───────────┬───────────┘
                            │
             ┌──────────────┼──────────────┐
             │ No           │              │ Yes
             ▼              │              ▼
┌────────────────────────┐  │  ┌────────────────────────┐
│ Run repair loop        │  │  │ Update CR Status       │
│ - Quorum recovery      │  │  │ - cluster state        │
│ - Partition healing    │  │  │ - node info            │
│ - Ghost node removal   │  │  │ - slot assignments     │
│ - Slot restoration     │  │  └────────────────────────┘
│ - Replica assignment   │  │
└────────────────────────┘  │
                            │
                            ▼
                ┌───────────────────────┐
                │ Requeue (fast or      │
                │ steady state)         │
                └───────────────────────┘
```

See [CLUSTER_RECONCILIATION.md](CLUSTER_RECONCILIATION.md) for detailed cluster repair logic.

### 4.4 Reconciliation Triggers

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

**Recommended: Option C** - Add a label `chuck-chuck-chuck.net/role: master` to the master pod, update it on failover. Service selector uses this label.

### 5.2 Honest Labeling (Sentinel Mode)

To improve observability and ensure correct traffic routing during cluster transitions, LittleRed implements "Honest Labeling." Instead of a binary master/replica choice, pods are labeled based on their actual relationship to the cluster:

| Role | Meaning | Traffic Impact |
|------|---------|----------------|
| `master` | A living pod authorized as master by Sentinel. | Receives all traffic (Read/Write). |
| `replica` | A living pod tracking a **living** master. | Receives read-only traffic (if configured). |
| `orphan` | Sentinel reports a master, but it is a **ghost IP** (dead pod). | Removed from all Service endpoints. |
| `undefined` | Sentinel has **no master information** registered. | Removed from all Service endpoints. |

This prevents "Zombie Replicas" (pods following dead IPs) from being perceived as healthy cluster members.

### 5.3 Proactive Topology Introduction (Sentinel Mode)

Because "Strict IP Identity" prevents nodes from automatically re-discovering each other via hostnames, the Operator proactively "introduces" and repairs node relationships during reconciliation:

1.  **Sentinel Membership Insurance**: The Operator ensures every Sentinel pod is monitoring the current master. If a Sentinel restarts with a new IP and enters an idle state, the Operator re-introduces the master to it.
2.  **Replica Rescue**: The Operator verifies every Redis pod's replication state. If a pod is found pointing to a ghost IP (the "Zombie" state), the Operator authoritatively reconfigures it via `SLAVEOF` to follow the current living master.
3.  **Ghost Node Removal**: The Operator proactively prunes dead IPs from Sentinel's topology via `SENTINEL RESET` once a new living master is stable.

### 5.4 Failover Detection

The operator doesn't perform failover—Sentinel does. The operator only needs to:
1. Detect that failover occurred (master changed)
2. Update the `role` label on pods
3. Update CR status

Detection happens during reconciliation by querying Sentinel.

---

## 6. Configuration Management

### 6.1 Default redis.conf

```
# LittleRed defaults - optimized for pure in-memory storage
# IMPORTANT: Persistence is ACTIVELY DISABLED to ensure pure in-memory behavior.
# Note: By default, we use 'noeviction', meaning data is never forgotten unless explicitly deleted.

# Networking
bind 0.0.0.0
port 6379
tcp-backlog 511
tcp-keepalive 300

# Memory
maxmemory-policy noeviction

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
- Pod restart = clean slate (due to no persistence)

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
│  │ 2. Performance-optimized defaults (noeviction, no-persistence)│    │
│  │    - maxmemory-policy noeviction                   │    │
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

**Key Point**: Layer 2 actively overrides any persistence defaults from upstream images. This guarantees that out-of-the-box, LittleRed is a pure in-memory store with no disk dependencies and no eviction by default.

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
  - apiGroups: ["chuck-chuck-chuck.net"]
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
│   ├── littlered/                  # Operator entrypoint
│   │   ├── main.go                 # Entrypoint
│   │   └── Dockerfile              # Operator Dockerfile
│   └── littlered-chaos-client/     # Chaos test client
├── config/
│   ├── crd/                        # Generated CRD YAML
│   ├── manager/                    # Operator deployment
│   ├── rbac/                       # RBAC manifests
│   └── samples/                    # Example CRs
├── internal/
│   ├── controller/
│   │   ├── littlered_controller.go # Main reconciler
│   │   ├── resources.go            # Resource builders (ConfigMaps, Services, etc.)
│   │   ├── cluster_reconcile.go    # Cluster mode reconciliation logic
│   │   └── sentinel_monitor.go     # Sentinel event monitoring
│   └── redis/
│       ├── client.go               # Sentinel client wrapper
│       └── cluster_client.go       # Cluster client wrapper
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
