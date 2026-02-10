# LittleRed - Requirements Document

> A Kubernetes operator for Redis and Valkey, focused on high-performance in-memory caching.

**Document Status**: Active
**Last Updated**: 2026-02-03
**Version**: 0.2.0

---

## 1. Mission Statement

Create a Kubernetes operator to run Redis and Valkey instances, based on upstream open source images, providing a high-performance in-memory data store with durability through replication.

---

## 2. Project Overview

### 2.1 Project Name
- **Product Name**: LittleRed
- **Technical Name**: `littlered`
- **Repository**: `littlered-operator` (or similar)

### 2.2 Compatibility Target
- **Redis**: 7.2+
- **Valkey**: Compatible versions (7.2+ equivalent)
- **Future**: Redis 8.x, newer Valkey versions (out of initial scope)

### 2.3 Technology Stack
- **Language**: Go
- **Framework**: Kubebuilder / controller-runtime
- **Target Kubernetes**: 1.28+

---

## 3. Phased Delivery

### Phase 0: Pre-MVP - Standalone Instance
Single Redis/Valkey instance with no high availability.

**Deliverables**:
- Single-node deployment
- Basic CR (`LittleRed`) with standalone mode
- Prometheus metrics endpoint
- Configurable resources
- Optional TLS
- Optional password auth

### Phase 1: MVP - Sentinel High Availability
Redis Sentinel-based HA setup.

**Deliverables**:
- Sentinel mode: 1 master + 2 replicas + 3 sentinels
- Automatic failover via Sentinel
- Rolling and recreate upgrade strategies
- ServiceMonitor support (optional, user-enabled)

### Phase 2: Cluster Mode (✅ COMPLETED)
Redis Cluster for horizontal scaling and sharding.

**Deliverables**:
- Cluster mode: Configurable shards with replicas per shard
- Automatic cluster bootstrapping
- Operator-managed recovery (stores topology in CR status, not nodes.conf)
- No PersistentVolumes required (works on any K8s cluster)
- Data durability through replication

### Phase 3+: Future Considerations
- Persistence (RDB/AOF with PVC management)
- Redis 8.x / newer Valkey support
- ACL-based authentication
- Cluster slot migration for scaling

---

## 4. Functional Requirements

### 4.1 Custom Resource Design

Single CR with mode selector:

```yaml
apiVersion: littlered.chuck-chuck-chuck.net/v1alpha1
kind: LittleRed
metadata:
  name: my-cache
spec:
  mode: standalone | sentinel  # Default: standalone
  image: redis:7.2 | valkey/valkey:7.2  # User specifies
  # ... additional config
```

### 4.2 Configuration Philosophy

| Aspect | Approach |
|--------|----------|
| **Defaults** | Follow upstream Redis/Valkey defaults where reasonable |
| **Override** | Full redis.conf override capability available |
| **Explicit Config** | Key settings exposed as CRD fields for easy configuration |
| **Footgun Policy** | User can break things; not our job to prevent |

**Operator-managed settings** (always set by operator):
- `save ""`: Persistence disabled (no disk I/O)
- `appendonly no`: AOF disabled (no disk I/O)
- Performance-tuned settings for in-memory workloads

**User-configurable via CRD** (explicit fields, not raw config):
- `maxmemory`: Memory limit (optional, follows upstream default if not set)
- `maxmemoryPolicy`: Eviction policy like `noeviction`, `noeviction`, etc. (optional, follows upstream default `noeviction` if not set)
- Other common Redis settings as needed

**Rationale**: We follow upstream defaults (like `noeviction`) rather than imposing "cache-first" opinions. This means out-of-the-box, the operator does NOT evict data when memory is full. Users who want caching behavior can explicitly set `maxmemoryPolicy: allkeys-lru`. This makes the operator more general-purpose and honest about its behavior.

### 4.3 Persistence

**Default Behavior**: Pure in-memory data store. **No disk persistence. No volumes.**

The operator actively disables any disk persistence that Redis/Valkey images might enable by default:

```
save ""                 # Disable RDB snapshots
appendonly no           # Disable AOF
```

**Guarantees**:
- No PersistentVolumeClaims created by default
- No disk I/O for snapshots or AOF
- Data resides entirely in memory
- **Data durability achieved through replication, not disk persistence**

**User Override**: If an expert user configures persistence via `spec.config.raw`, that's their choice. The operator won't prevent it, but won't manage volumes for it either.

**Future Consideration**: Operator-managed persistence (with proper PVC handling) may be added as an opt-in feature later. But the default promise remains: **in-memory data store with no disk storage.**

### 4.4 Authentication

| Option | Description | Default |
|--------|-------------|---------|
| None | No authentication | Yes (controlled environment) |
| Password | `requirepass` configuration | Optional |

ACL-based auth is out of scope for MVP.

### 4.5 TLS

- **Status**: Optional in MVP
- **Implementation**: User provides Secret with certs
- **Default**: Disabled

### 4.6 Monitoring & Observability

| Feature | Status | Notes |
|---------|--------|-------|
| Prometheus metrics | In scope | Via redis_exporter sidecar or native |
| ServiceMonitor CR | In scope | Optional, user-enabled via CR flag |
| Alerts/PrometheusRules | Out of scope | May add if low effort |

### 4.7 Resource Management

| Aspect | Approach |
|--------|----------|
| Defaults | Sensible defaults provided |
| Override | User can specify via CR |
| QoS | Target Guaranteed QoS (requests = limits) |
| Auto-sizing | Not implemented (app-driven, not cluster-driven) |

### 4.8 Upgrade Strategy

User-selectable via CR:
- **Rolling**: Gradual pod replacement, maintains availability
- **Recreate**: All pods replaced simultaneously, brief downtime

---

## 5. Non-Functional Requirements

### 5.1 Performance
- Optimize for low-latency, high-throughput caching
- Guaranteed QoS class by default
- No persistence overhead

### 5.2 Reliability
- Sentinel mode provides automatic failover
- Operator should be crash-safe (reconciliation-based)

### 5.3 Security
- Runs in highly controlled environment
- TLS available but optional
- Password auth available but optional

### 5.4 Operability
- Clear status reporting via CR `.status`
- Prometheus metrics for monitoring
- Standard Kubernetes patterns (labels, annotations)

---

## 6. Scope Definition

### 6.1 In Scope

| Item | Phase | Status |
|------|-------|--------|
| Single standalone Redis/Valkey | Pre-MVP | ✅ |
| Sentinel HA (1+2+3 topology) | MVP | ✅ |
| Cluster mode (sharding with replicas) | Phase 2 | ✅ |
| Redis 7.2+ / Valkey 7.2+ | Pre-MVP | ✅ |
| In-memory operation (no persistence) | Pre-MVP | ✅ |
| Data durability through replication | Phase 2 | ✅ |
| Prometheus metrics | Pre-MVP | ✅ |
| Optional ServiceMonitor | MVP | ✅ |
| Optional TLS | MVP | ✅ |
| Optional password auth | Pre-MVP | ✅ |
| Explicit maxmemory config (CRD field) | Phase 2 | 🔴 TODO |
| Explicit maxmemory-policy config (CRD field) | Phase 2 | 🔴 TODO |
| Rolling & recreate upgrades | MVP | ✅ |
| Full config override | Pre-MVP | ✅ |

### 6.2 Out of Scope (Current)

| Item | Notes |
|------|-------|
| Persistence (RDB/AOF with PVC) | Future consideration |
| Cluster slot migration | Future - needed for dynamic scaling |
| Redis < 7.2 | Not required |
| Redis 8.x | Future consideration |
| ACL-based auth | Future consideration |
| Auto-sizing | Not planned |
| Prometheus alerts | Only if trivial to add |
| Auto-detection of Prometheus Operator | User must explicitly enable |

### 6.3 Architecture Considerations for Future

While out of initial scope, architecture should not preclude:
- Adding Redis Cluster support later
- Adding persistence options later
- Supporting configurable replica counts in Sentinel mode

---

## 7. Open Questions

1. ~~**Metrics export**: Sidecar (redis_exporter) vs. native Redis metrics (7.0+)?~~ **RESOLVED**: redis_exporter sidecar
2. ~~**Operator distribution**: Helm chart? OLM? Kustomize?~~ **RESOLVED**: Helm chart
3. ~~**CR API group**: `littlered.io`? Something else?~~ **RESOLVED**: `littlered.chuck-chuck-chuck.net`
4. ~~**Namespace scope**: Namespace-scoped or cluster-scoped operator?~~ **RESOLVED**: Configurable (can run either way)

---

## 8. Decision Log

| Date | Decision | Rationale |
|------|----------|-----------|
| 2026-01-30 | Go as implementation language | Best K8s operator ecosystem support |
| 2026-01-30 | K8s 1.28+ only | Can use latest APIs, reduce compat burden |
| 2026-01-30 | No persistence | Target use case requires pure cache |
| 2026-01-30 | Single CR with mode selector | Simpler UX than multiple CRs |
| 2026-01-30 | LittleRed name | Redis/Valkey agnostic, memorable |
| 2026-01-30 | Fixed 1+2+3 Sentinel topology initially | Simplest, architect for flexibility |
| 2026-01-30 | redis_exporter sidecar for metrics | Industry standard, Grafana dashboard compat |
| 2026-01-30 | Helm chart for distribution | Most common, familiar to users |
| 2026-01-30 | Configurable namespace/cluster scope | Flexibility for different deployment models |
| 2026-01-30 | API group: `littlered.chuck-chuck-chuck.net` | User-owned domain |
| 2026-01-30 | No admission webhooks (initially) | Operational complexity, validate in controller |
| 2026-01-30 | Watch Secrets for rotation | Auto-reconcile on password/cert changes |
| 2026-01-30 | Actively disable persistence by default | Override image defaults with `save ""`, `appendonly no` to guarantee pure in-memory cache |
| 2026-01-30 | Default to Valkey (not Redis) | Personal preference, easily overridable |
| 2026-01-30 | Image composed from registry/path:tag | Easy registry mirror support without copy-pasting image names |
| 2026-01-30 | Fully qualified image refs by default | Unqualified images deprecated in modern runtimes (CRI-O, containerd) |
| 2026-01-30 | Exporter inherits registry from main image | Single registry setting applies to all images |
| 2026-02-03 | Cluster mode: No PVCs, topology in CR status | Enables deployment on K8s without local storage, simpler than managing nodes.conf |
| 2026-02-03 | Data durability through replication, not disk | Replicas exist to prevent data loss during pod restarts |
| 2026-02-03 | Follow upstream defaults for eviction | More honest/general-purpose; users opt-in to caching behavior explicitly |
| 2026-02-03 | Expose maxmemory/maxmemory-policy as CRD fields | Easy configuration without raw config manipulation |

---

## 9. References

- [Redis Documentation](https://redis.io/docs/)
- [Valkey Documentation](https://valkey.io/docs/)
- [Kubebuilder Book](https://book.kubebuilder.io/)
- [Redis Sentinel](https://redis.io/docs/management/sentinel/)
