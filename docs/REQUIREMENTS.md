# LittleRed - Requirements Document

> A Kubernetes operator for Redis and Valkey, focused on high-performance in-memory data storage with durability through replication.

**Document Status**: Active
**Last Updated**: 2026-02-24
**Version**: 0.4.0

---

## 1. Mission Statement

Create a Kubernetes operator to run Redis and Valkey instances, based on upstream open source images, providing a high-performance in-memory data store with durability through replication.

---

## 2. Project Overview

### 2.1 Project Name
- **Product Name**: LittleRed
- **Technical Name**: `littlered`
- **API Group**: `chuck-chuck-chuck.net`

### 2.2 Compatibility Target
- **Redis**: 7.2+
- **Valkey**: Compatible versions (7.2+ equivalent)
- **Default image**: `docker.io/valkey/valkey:8.0`

### 2.3 Technology Stack
- **Language**: Go 1.24+
- **Framework**: Kubebuilder v4 / controller-runtime
- **Target Kubernetes**: 1.28+
- **Redis client**: go-redis v9

---

## 3. Phased Delivery

### Phase 0: Pre-MVP - Standalone Instance ✅
Single Redis/Valkey instance with no high availability.

### Phase 1: MVP - Sentinel High Availability ✅
Redis Sentinel-based HA setup with automatic failover.

### Phase 2: Cluster Mode ✅
Redis Cluster for horizontal scaling and sharding.

### Phase 3: Auth & TLS ✅
Password authentication and TLS encryption across all modes.

### Phase 4: Resilience Hardening ✅
Kill-9 / crash protection, ghost node healing, operator-driven topology repair.

### Phase 5+: Future Considerations
- Persistence (RDB/AOF with PVC management)
- Redis 8.x / newer Valkey support
- ACL-based authentication
- Cluster slot migration for dynamic scaling
- Configurable replica counts in Sentinel mode

---

## 4. Functional Requirements

### 4.1 Custom Resource Design

Single CR with mode selector:

```yaml
apiVersion: chuck-chuck-chuck.net/v1alpha1
kind: LittleRed
metadata:
  name: my-cache
spec:
  mode: standalone | sentinel | cluster
  image:
    repository: valkey/valkey
    tag: "8.0"
  auth:
    enabled: true
    existingSecret: my-redis-auth  # Secret with 'password' key
  tls:
    enabled: true
    existingSecret: my-redis-tls   # Secret with tls.crt, tls.key, ca.crt
  cluster:
    shards: 3
    replicasPerShard: 1
  sentinel:
    quorum: 2
    downAfterMilliseconds: 5000
```

### 4.2 Configuration Philosophy

| Aspect | Approach |
|--------|----------|
| **Defaults** | Performance-optimized: noeviction, no persistence, Guaranteed QoS |
| **Override** | Full redis.conf override via `spec.config.raw` |
| **Explicit Config** | Key settings exposed as CRD fields (maxmemory, maxmemoryPolicy) |
| **Safety** | Persistence actively disabled (`save ""`, `appendonly no`) |

### 4.3 Persistence

**Default**: Pure in-memory. No disk persistence. No PersistentVolumeClaims.

**Guarantees**:
- No disk I/O for snapshots or AOF
- Data durability achieved through replication, not disk
- Pod restart = clean slate (by design)

**Expert Override**: Users can re-enable persistence via `spec.config.raw` (their responsibility for PVC management).

### 4.4 Authentication

| Option | Description | Status |
|--------|-------------|--------|
| None | No authentication | Default |
| Password | `requirepass` + `masterauth` via existing Secret | ✅ Implemented |
| ACL | ACL-based auth | Out of scope |

### 4.5 TLS

| Feature | Status |
|---------|--------|
| Transport encryption (all modes) | ✅ Implemented |
| User-provided TLS secrets | ✅ Implemented |
| Separate CA cert secret | ✅ Implemented |
| Client certificate auth | ✅ Implemented |
| Operator-to-pod: InsecureSkipVerify | ✅ By design (ADR-004) |

### 4.6 Resilience

| Scenario | Protection | Status |
|----------|-----------|--------|
| Pod deletion (`kubectl delete pod`) | Sentinel failover / Cluster gossip | ✅ |
| Hard pod deletion (`--grace-period=0 --force`) | Pre-stop hook triggers failover before termination | ✅ |
| In-pod process crash (kill -9, OOM) | Startup script detects crash via run-id (sentinel) or nodes.conf (cluster), yields until failover completes | ✅ |
| Ghost nodes (dead IPs in topology) | Operator prunes via SENTINEL RESET / CLUSTER FORGET | ✅ |
| Split-brain / divergent sentinels | Operator re-registers via SENTINEL REMOVE + MONITOR | ✅ |
| Quorum loss (cluster mode) | CLUSTER FAILOVER TAKEOVER on orphan replicas | ✅ |
| Partition healing (cluster mode) | CLUSTER MEET with orphan tracking + grace period | ✅ |

### 4.7 Monitoring & Observability

| Feature | Status |
|---------|--------|
| Prometheus metrics via redis_exporter sidecar | ✅ |
| Optional ServiceMonitor CR | ✅ |
| Structured JSON operator logs | ✅ |
| Log categories (recon, state, audit) | ✅ |
| CR status with phase, conditions, master info | ✅ |

### 4.8 CLI Tool (lrctl)

| Feature | Status |
|---------|--------|
| List/describe LittleRed resources | ✅ |
| Verify cluster health (connectivity, topology) | ✅ |
| JSON output mode | ✅ |
| kubectl plugin integration | ✅ |
| Shell completion | ✅ |

---

## 5. Non-Functional Requirements

### 5.1 Performance
- Guaranteed QoS class by default (requests = limits)
- No persistence overhead
- No unnecessary reconciliation (GenerationChangedPredicate)

### 5.2 Reliability
- Sentinel mode: automatic failover with kill-9 protection
- Cluster mode: quorum recovery, partition healing, ghost removal
- Operator crash-safe (reconciliation-based, no in-memory state beyond sentinel monitor goroutines)

### 5.3 Security
- TLS encryption available across all modes
- Password auth available across all modes
- Runs as non-root with dropped capabilities
- Secrets validated at reconcile time

### 5.4 Operability
- Clear status reporting via CR `.status` (phase, conditions, master info, cluster nodes)
- Audit log for all cluster-modifying actions
- lrctl CLI for inspection and verification

---

## 6. Scope Definition

See [SCOPE.md](SCOPE.md) for the full in/out of scope breakdown.

---

## 7. Decision Log

| Date | Decision | Rationale |
|------|----------|-----------|
| 2026-01-30 | Go, Kubebuilder, K8s 1.28+ | Best operator ecosystem support |
| 2026-01-30 | No persistence by default | Pure in-memory use case |
| 2026-01-30 | Single CR with mode selector | Simpler UX |
| 2026-01-30 | redis_exporter sidecar | Industry standard |
| 2026-01-30 | Helm chart distribution | Most common |
| 2026-01-30 | API group: `chuck-chuck-chuck.net` | User-owned domain |
| 2026-01-30 | Default to Valkey | Personal preference, easily overridable |
| 2026-02-03 | Cluster mode: no PVCs, topology in CR status | Works on any K8s cluster |
| 2026-02-03 | Upstream defaults for eviction (noeviction) | Honest/general-purpose |
| 2026-02-11 | Strict IP-only identity (ADR-001) | Prevent ghost master data loss |
| 2026-02-21 | Low-interference sentinel reconciliation (ADR-003) | Trust Sentinel, intervene only on permanent stalls |
| 2026-02-21 | REMOVE + MONITOR over RESET for stuck sentinels (LR-008) | RESET doesn't change monitored master IP |
| 2026-02-22 | Rule R: don't trigger on LinkStatus=down alone (LR-010) | Avoid interrupting in-progress handshakes |
| 2026-02-24 | TLS InsecureSkipVerify (ADR-004) | Identity via K8s API, not PKI |

---

## 8. References

- [Architecture Document](ARCHITECTURE.md)
- [Scope Definition](SCOPE.md)
- [Reconciliation Algorithm Changelog](RECONCILIATION_ALGORITHM_CHANGELOG.md)
- Architecture Decision Records: `docs/adr/`
- [LLM Startup Guide](../LLM_STARTUP.md)
