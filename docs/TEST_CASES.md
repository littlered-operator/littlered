# LittleRed - Test Cases

> Test case definitions for the LittleRed Kubernetes operator.

**Document Status**: Active
**Last Updated**: 2026-02-07

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

### 2.1 CR Lifecycle & Deployment

| ID | Test Case | Status | Expected Result |
|----|-----------|--------|-----------------|
| STAN-001 | Create minimal LittleRed CR | ✅ | StatefulSet + Service created, 1 pod running |
| STAN-002 | Delete LittleRed CR | ✅ | All owned resources (STS, SVC, Pods) cleaned up |
| STAN-003 | Create CR with invalid spec | ❌ | Admission rejected / status shows error |
| STAN-004 | Update CR (resources) | ✅ | Rolling update, pod recreated with new limits |
| STAN-005 | Update CR (config) | ✅ | Rolling update, pod recreated with new config hash |

### 2.2 Functional Verification

| ID | Test Case | Status | Expected Result |
|----|-----------|--------|-----------------|
| STAN-010 | Redis responds to PING | ✅ | Returns PONG |
| STAN-011 | SET/GET operations work | ✅ | Data stored and retrieved correctly |
| STAN-012 | Metrics exposed | ✅ | Prometheus metrics available on port 9121 |
| STAN-013 | Pod recreation recovery | ✅ | Pod deleted manually is recreated by STS, data lost (expected) |

### 2.3 Persistence Disabled (Critical)

| ID | Test Case | Status | Expected Result |
|----|-----------|--------|-----------------|
| PERS-001 | No PVCs created | ✅ | `kubectl get pvc` returns none |
| PERS-002 | RDB/AOF disabled | ❌ | Config checks show persistence disabled |
| PERS-003 | Data lost on restart | ✅ | Verify empty keyspace after pod restart |

---

## 3. Sentinel Mode Tests

### 3.1 Deployment & Lifecycle

| ID | Test Case | Status | Expected Result |
|----|-----------|--------|-----------------|
| SEN-001 | Deploy Sentinel mode | ✅ | 3 Redis + 3 Sentinel pods running |
| SEN-002 | Services created | ✅ | Master, Replica, and Sentinel services exist |
| SEN-003 | Sentinel Quorum | ✅ | Sentinels report quorum and correct master |
| SEN-004 | Status Reporting | ✅ | CR status shows Master Pod name |
| SEN-005 | Rolling Update (Resources) | ✅ | All pods updated, quorum maintained throughout |

### 3.2 Replication & Failover

| ID | Test Case | Status | Expected Result |
|----|-----------|--------|-----------------|
| SEN-010 | Replication Active | ✅ | Write to master, read from replica |
| SEN-011 | Master Pod Deletion | ✅ | New master elected, cluster recovers |
| SEN-012 | Data Survival (Failover) | ✅ | Data exists on new master after failover |
| SEN-013 | Cluster Recovery | ✅ | All pods eventually return to Ready state |
| SEN-014 | Rapid Double Failover | ✅ | Traffic follows master through two rapid kills (Event-driven) |
| SEN-015 | Event-Only Recovery | ✅ | Polling disabled, only Pub/Sub events trigger updates |
| SEN-016 | Polling-Only Recovery | ✅ | Events disabled, only periodic polling triggers updates |
| SEN-017 | Hybrid (Prod) Recovery | ✅ | Both events and polling active (standard prod config) |

---

## 4. Cluster Mode Tests

### 4.1 Cluster Deployment

| ID | Test Case | Status | Expected Result |
|----|-----------|--------|-----------------|
| CLUST-001 | Deploy Redis Cluster | ✅ | 6 pods (3 master, 3 replica) created & running |
| CLUST-002 | Cluster Services | ✅ | Client (ClusterIP) and Headless services created |
| CLUST-003 | Cluster State OK | ✅ | `CLUSTER INFO` reports `cluster_state:ok` |
| CLUST-004 | Slot Assignment | ✅ | All 16384 slots assigned across masters |
| CLUST-005 | Topology Verification | ✅ | 3 masters, 3 replicas correctly paired |

### 4.2 Functional Verification

| ID | Test Case | Status | Expected Result |
|----|-----------|--------|-----------------|
| CLUST-010 | SET/GET (Cluster Mode) | ✅ | Data stored/retrieved using `-c` flag |
| CLUST-011 | Sharding Distribution | ✅ | Keys distributed across multiple shards/nodes |
| CLUST-012 | Configuration Apply | ✅ | Custom `maxmemory` and policies applied to all nodes |

### 4.3 Status & Observability

| ID | Test Case | Status | Expected Result |
|----|-----------|--------|-----------------|
| CLUST-020 | Status Tracking | ✅ | CR status tracks all 6 nodes with IDs and Roles |
| CLUST-021 | Ready Condition | ✅ | `ClusterReady` condition is True |
| CLUST-022 | Node Details | ✅ | Status includes slot ranges and master links |

### 4.4 Cluster Recovery

| ID | Test Case | Status | Expected Result |
|----|-----------|--------|-----------------|
| CLUST-030 | Replica Pod Deletion | ✅ | Pod recreated, cluster remains OK, data accessible |
| CLUST-031 | Master Pod Deletion | ✅ | Pod recreated, cluster re-stabilizes, no data loss |
| CLUST-032 | Multiple Pod Deletion | ✅ | Cluster recovers from losing 3 pods (half cluster) |
| CLUST-033 | Cluster Cleanup | ✅ | Deleting CR removes all resources (STS, PVCs if any, Pods) |

---

## 5. Chaos & Resilience Tests

These tests use the `chaos-client` to inject faults under load.

### 5.1 Cluster Resilience

| ID | Test Case | Status | Expected Result |
|----|-----------|--------|-----------------|
| CHAOS-001 | Replica Deletion under Load | ✅ | 0 Data Corruption, High availability |
| CHAOS-002 | Master Deletion under Load | ✅ | 0 Data Corruption, Cluster recovers automatically |
| CHAOS-003 | Rolling Restart under Load | ✅ | 0 Data Corruption, >70% availability |

### 5.2 Sentinel Resilience

| ID | Test Case | Status | Expected Result |
|----|-----------|--------|-----------------|
| CHAOS-020 | Rapid Double Failover (Service) | ✅ | >40% Availability, 0 Corruption, Client follows Master |

### 5.3 Standalone Resilience

| ID | Test Case | Status | Expected Result |
|----|-----------|--------|-----------------|
| CHAOS-010 | Pod Restart under Load | ✅ | 0 Data Corruption (checked against surviving data) |

---

## 6. Negative & Security Tests

| ID | Test Case | Status | Expected Result |
|----|-----------|--------|-----------------|
| NEG-001 | Invalid Mode | ❌ | Admission rejected |
| NEG-002 | Missing Secret | ❌ | Pods pending / Status Error |
| SEC-001 | Non-root User | 🚧 | Pods run as non-root (Partial coverage) |
| SEC-002 | Read-only Root FS | ❌ | Container uses read-only root filesystem |

---

## 7. Performance Benchmarks

| ID | Test Case | Status | Expected Result |
|----|-----------|--------|-----------------|
| PERF-001 | Failover Time | 🚧 | < 30s failover (Observed in logs, not asserted) |
| PERF-002 | Boot Time | 🚧 | Cluster ready < 4m (Implicit in timeouts) |