# LittleRed - Test Cases

> Test case definitions for the LittleRed Kubernetes operator.

**Document Status**: Active
**Last Updated**: 2026-02-25

---

## 1. Test Strategy

### 1.1 Test Levels

| Level | Description | Tools |
|-------|-------------|-------|
| **Unit Tests** | Controller logic, helpers, validation | Go testing, gomega |
| **Integration Tests** | Controller + fake K8s API | envtest (controller-runtime) |
| **E2E Tests** | Full operator in real cluster | kind/k3d, Ginkgo |

### 1.2 Status Legend

- ✅ **Implemented**: Covered in E2E test suite
- ❌ **Missing**: Not yet implemented
- 🚧 **Partial**: Partially covered

---

## 2. Standalone Mode Tests

*Source: `littlered_test.go`*

### 2.1 CR Lifecycle & Deployment

| ID | Test Case | Status | Expected Result |
|----|-----------|--------|-----------------|
| STAN-001 | Create minimal LittleRed CR | ✅ | StatefulSet + Service created, 1 pod running |
| STAN-002 | Delete LittleRed CR | ✅ | All owned resources (STS, SVC, Pods) cleaned up |
| STAN-003 | Create CR with invalid spec | ❌ | Admission rejected / status shows error |
| STAN-004 | Rolling update (resource change) | ✅ | Pod recreated with new limits, status returns Running |
| STAN-005 | Rolling update (config change) | ✅ | ConfigMap hash change triggers pod restart |

### 2.2 Functional Verification

| ID | Test Case | Status | Expected Result |
|----|-----------|--------|-----------------|
| STAN-010 | Redis responds to PING | ✅ | Returns PONG |
| STAN-011 | SET/GET operations | ✅ | Data stored and retrieved correctly |
| STAN-012 | Metrics exposed | ✅ | Prometheus metrics available on port 9121 |
| STAN-013 | Pod recreation recovery | ✅ | Pod deleted manually is recreated by STS; data lost (expected) |

### 2.3 Persistence Disabled (Critical)

| ID | Test Case | Status | Expected Result |
|----|-----------|--------|-----------------|
| PERS-001 | No PVCs created | ✅ | `kubectl get pvc` returns none |
| PERS-002 | RDB/AOF disabled in config | ❌ | Config shows persistence explicitly disabled |
| PERS-003 | Data lost on restart | ✅ | Empty keyspace after pod restart |

---

## 3. Sentinel Mode Tests

*Source: `littlered_test.go`, `failover_test.go`*

### 3.1 Deployment & Lifecycle

| ID | Test Case | Status | Expected Result |
|----|-----------|--------|-----------------|
| SEN-001 | Deploy Sentinel mode | ✅ | 3 Redis + 3 Sentinel pods running |
| SEN-002 | Services created | ✅ | Master, Replica, and Sentinel services exist |
| SEN-003 | Sentinel quorum established | ✅ | Sentinels report quorum and correct master |
| SEN-004 | Status reporting | ✅ | CR status shows master pod name, bootstrap flag cleared |
| SEN-005 | Correct role labels | ✅ | Pods labeled master/replica correctly |
| SEN-006 | Rolling update (resources) | ✅ | All pods updated, quorum maintained throughout |
| SEN-007 | Rolling update (quorum survives) | ✅ | Sentinel quorum intact after rolling update |

### 3.2 Replication & Basic Failover

| ID | Test Case | Status | Expected Result |
|----|-----------|--------|-----------------|
| SEN-010 | Replication active | ✅ | Write to master, read from replica |
| SEN-011 | Master pod deletion (graceful) | ✅ | New master elected, cluster recovers |
| SEN-012 | Master pod deletion (kill-9 / force) | ✅ | New master elected, cluster recovers |
| SEN-013 | Data survival after failover | ✅ | Data on surviving replicas accessible after master election |
| SEN-014 | Full cluster recovery | ✅ | All pods return to Ready state after failover |

### 3.3 Advanced Failover

| ID | Test Case | Status | Expected Result |
|----|-----------|--------|-----------------|
| SEN-020 | Event-driven label update | ✅ | Master role label updated immediately via `+switch-master` pub/sub event |
| SEN-021 | Event-only recovery | ✅ | Polling disabled; only pub/sub events drive label updates |
| SEN-022 | Polling-only recovery | ✅ | Events disabled; only periodic polling drives label updates |
| SEN-023 | Hybrid (production) recovery — graceful | ✅ | Both events and polling active; graceful master kill recovers |
| SEN-024 | Hybrid (production) recovery — kill-9 | ✅ | Both events and polling active; crash kill recovers |
| SEN-025 | Sentinel pod resilience | ✅ | Failover still works after 1/3 sentinel pod is restarted |

---

## 4. Cluster Mode Tests

*Source: `cluster_functional_test.go`, `cluster_rolling_test.go`*

### 4.1 Cluster Deployment

| ID | Test Case | Status | Expected Result |
|----|-----------|--------|-----------------|
| CLUST-001 | Deploy Redis Cluster (3M + 3R) | ✅ | 6 pods running; 3 masters, 3 replicas |
| CLUST-002 | Services created | ✅ | Client (ClusterIP) and headless services exist |
| CLUST-003 | Cluster state OK | ✅ | `CLUSTER INFO` reports `cluster_state:ok` |
| CLUST-004 | Slot assignment | ✅ | All 16384 slots assigned across masters |
| CLUST-005 | Topology verification | ✅ | 3 masters, 3 replicas correctly paired |

### 4.2 Functional Verification

| ID | Test Case | Status | Expected Result |
|----|-----------|--------|-----------------|
| CLUST-010 | SET/GET with cluster routing | ✅ | Data stored/retrieved using `-c` flag |
| CLUST-011 | Sharding distribution | ✅ | Keys distributed across multiple shards |
| CLUST-012 | Custom configuration | ✅ | Custom `maxmemory` and policies applied to all nodes |

### 4.3 Status & Observability

| ID | Test Case | Status | Expected Result |
|----|-----------|--------|-----------------|
| CLUST-020 | Status tracking | ✅ | CR status tracks all nodes with NodeIDs and roles |
| CLUST-021 | Ready condition | ✅ | `ClusterReady` condition is True |
| CLUST-022 | Node details | ✅ | Status includes slot ranges and master links |

### 4.4 Cluster Recovery

| ID | Test Case | Status | Expected Result |
|----|-----------|--------|-----------------|
| CLUST-030 | Replica pod deletion | ✅ | Pod recreated, cluster remains OK, data accessible |
| CLUST-031 | Master pod deletion | ✅ | Replica promoted, new pod joins as replica, data preserved |
| CLUST-032 | 0-replica mode self-healing | ✅ | Ghost forgotten, new pod MEETed, slots reassigned |
| CLUST-033 | Cluster cleanup | ✅ | Deleting CR removes all resources |

### 4.5 Rolling Update

| ID | Test Case | Status | Expected Result |
|----|-----------|--------|-----------------|
| CLUST-040 | Rolling update (resources) | ✅ | All pods updated, cluster remains healthy |
| CLUST-041 | Data preservation across rolling update | ✅ | Keys written before update readable after |
| CLUST-042 | Status Running after rolling update | ✅ | CR phase returns to Running |

---

## 5. Kill-9 / In-Pod Process Crash Tests

*Source: `kill9_chaos_test.go`*

These tests use `kubectl exec <pod> -- kill -9 1` to simulate OOM / SIGKILL scenarios where the pod continues running (emptyDir survives) but Redis loses all in-memory data.

| ID | Test Case | Status | Expected Result |
|----|-----------|--------|-----------------|
| KILL9-001 | Standalone — kill-9 smoke test | ✅ | Redis process killed; container restarts; PING works again |
| KILL9-002 | Sentinel — master process crash | ✅ | Sentinel detects failure, elects new master, 0 data corruption |
| KILL9-003 | Cluster — master process crash | ✅ | Replica promoted, cluster heals, 0 data corruption |

---

## 6. Chaos & Resilience Tests

*Source: `cluster_chaos_test.go`, `sentinel_standalone_chaos_test.go`*

All chaos tests run a continuous-write client (`chaos-client`) and verify zero data corruption and sufficient availability during and after fault injection.

### 6.1 Cluster Chaos

| ID | Test Case | Status | Expected Result |
|----|-----------|--------|-----------------|
| CHAOS-001 | Cluster — baseline stability (0 replicas) | ✅ | 100% availability under stable conditions |
| CHAOS-002 | Cluster — master failure under load (graceful) | ✅ | High availability, 0 data corruption |
| CHAOS-003 | Cluster — master failure under load (kill-9) | ✅ | High availability, 0 data corruption |
| CHAOS-004 | Cluster — replica failure under load (graceful) | ✅ | 100% availability, 0 data corruption |
| CHAOS-005 | Cluster — replica failure under load (kill-9) | ✅ | 100% availability, 0 data corruption |
| CHAOS-006 | Cluster — rolling restart under load | ✅ | >95% availability, 0 data corruption; uses `kubectl rollout restart` with `minReadySeconds=30s` |
| CHAOS-007 | Cluster — multi-pod random deletion | ✅ | Cluster survives multiple rounds of random pod loss, 0 corruption |

### 6.2 Sentinel Chaos

| ID | Test Case | Status | Expected Result |
|----|-----------|--------|-----------------|
| CHAOS-020 | Sentinel — rapid double failover (graceful) | ✅ | >40% availability, 0 corruption, client follows master |
| CHAOS-021 | Sentinel — rapid double failover (kill-9) | ✅ | >40% availability, 0 corruption, client follows master |

### 6.3 Standalone Chaos

| ID | Test Case | Status | Expected Result |
|----|-----------|--------|-----------------|
| CHAOS-030 | Standalone — pod restart under load | ✅ | 0 data corruption (checked against surviving data) |

---

## 7. Security Tests

*Source: `security_test.go`*

### 7.1 Password Authentication

| ID | Test Case | Status | Expected Result |
|----|-----------|--------|-----------------|
| SEC-001 | Standalone — password auth enforcement | ✅ | Unauthenticated access rejected; authenticated access works |
| SEC-002 | Sentinel — password auth enforcement | ✅ | Unauthenticated access rejected; authenticated access works |
| SEC-003 | Cluster — password auth enforcement | ✅ | Unauthenticated access rejected; authenticated access works |

### 7.2 TLS Encryption

| ID | Test Case | Status | Expected Result |
|----|-----------|--------|-----------------|
| SEC-010 | Standalone — TLS encryption enforcement | ✅ | Plaintext connections rejected; TLS connections work |
| SEC-011 | Sentinel — TLS encryption enforcement | ✅ | Plaintext connections rejected; TLS connections work |
| SEC-012 | Cluster — TLS encryption enforcement | ✅ | Plaintext connections rejected; TLS connections work |

---

## 8. Negative Tests

| ID | Test Case | Status | Expected Result |
|----|-----------|--------|-----------------|
| NEG-001 | Invalid mode in spec | ❌ | Admission rejected |
| NEG-002 | Missing referenced secret | ❌ | Pods pending / status shows error condition |

---

## 9. Performance Benchmarks

These are observed rather than asserted; thresholds are guidelines, not hard test gates.

| ID | Test Case | Status | Expected Result |
|----|-----------|--------|-----------------|
| PERF-001 | Sentinel failover time | 🚧 | < 30s from master death to new master serving traffic |
| PERF-002 | Cluster boot time | 🚧 | Cluster ready < 4 minutes |
