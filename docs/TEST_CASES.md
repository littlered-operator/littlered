# LittleRed - Test Cases

> Test case definitions for the LittleRed Kubernetes operator.

**Document Status**: Draft
**Last Updated**: 2026-01-30

---

## 1. Test Strategy

### 1.1 Test Levels

| Level | Description | Tools |
|-------|-------------|-------|
| **Unit Tests** | Controller logic, helpers, validation | Go testing, gomega |
| **Integration Tests** | Controller + fake K8s API | envtest (controller-runtime) |
| **E2E Tests** | Full operator in real cluster | kind/k3d, Ginkgo |

### 1.2 Test Environments

- **CI**: kind cluster (GitHub Actions or similar)
- **Local Dev**: kind/k3d/minikube
- **Pre-prod**: Real K8s cluster (staging)

---

## 2. Pre-MVP Test Cases (Standalone Mode)

### 2.1 CR Lifecycle

| ID | Test Case | Expected Result |
|----|-----------|-----------------|
| CR-001 | Create minimal LittleRed CR | StatefulSet + Service created, pods running |
| CR-002 | Delete LittleRed CR | All owned resources cleaned up |
| CR-003 | Create CR with invalid spec | Admission rejected / status shows error |
| CR-004 | Update CR (non-breaking change) | Resources reconciled, no restart |
| CR-005 | Update CR (breaking change, e.g., image) | Rolling/recreate update per strategy |

### 2.2 Standalone Redis Deployment

| ID | Test Case | Expected Result |
|----|-----------|-----------------|
| STAN-001 | Deploy standalone Redis 7.2 | Single pod running, accepting connections |
| STAN-002 | Deploy standalone Valkey 7.2 | Single pod running, accepting connections |
| STAN-003 | Redis responds to PING | Returns PONG |
| STAN-004 | SET/GET operations work | Data stored and retrieved correctly |
| STAN-005 | Memory limit respected | Redis `maxmemory` configured correctly |

### 2.3 Persistence Disabled (Critical)

These tests verify our core promise: **pure in-memory cache with no persistence by default**.

| ID | Test Case | Expected Result |
|----|-----------|-----------------|
| PERS-001 | No PVCs created by default | `kubectl get pvc` returns none for the CR |
| PERS-002 | RDB disabled in config | `CONFIG GET save` returns empty string |
| PERS-003 | AOF disabled in config | `CONFIG GET appendonly` returns `no` |
| PERS-004 | No dump.rdb file exists | `ls /data/dump.rdb` fails (file not found) |
| PERS-005 | No appendonly.aof file exists | `ls /data/appendonly.aof` fails (file not found) |
| PERS-006 | No persistence after writes | After 1000 SET operations, still no dump.rdb or AOF |
| PERS-007 | No persistence after time | Wait 5 minutes with data, verify no RDB created |
| PERS-008 | INFO persistence shows disabled | `INFO persistence` shows `rdb_bgsave_in_progress:0`, `aof_enabled:0` |
| PERS-009 | BGSAVE returns error or no-op | Manual `BGSAVE` doesn't create persistence (or is disabled) |
| PERS-010 | Pod restart loses all data | Kill pod, verify new pod has empty keyspace |
| PERS-011 | ConfigMap contains disable directives | ConfigMap has `save ""` and `appendonly no` |
| PERS-012 | Overrides upstream image defaults | Deploy with redis:7.2 (which has default saves), verify still disabled |
| PERS-013 | Overrides Valkey image defaults | Deploy with valkey:7.2, verify persistence disabled |

**Test Implementation Notes**:

```bash
# PERS-002: Verify RDB disabled
redis-cli CONFIG GET save
# Expected: 1) "save" 2) ""

# PERS-003: Verify AOF disabled
redis-cli CONFIG GET appendonly
# Expected: 1) "appendonly" 2) "no"

# PERS-008: Check INFO persistence section
redis-cli INFO persistence | grep -E "(rdb_|aof_)"
# Expected: aof_enabled:0, no active saves

# PERS-010: Data loss on restart (this is correct behavior!)
redis-cli SET testkey testvalue
kubectl delete pod <pod-name>
# Wait for pod recreation
redis-cli GET testkey
# Expected: (nil)
```

### 2.4 Configuration

| ID | Test Case | Expected Result |
|----|-----------|-----------------|
| CFG-001 | Default config applied | Performance-optimized settings active |
| CFG-002 | Custom config via CR | User settings override defaults |
| CFG-003 | Full redis.conf override | Complete custom config used |
| CFG-004 | Invalid config provided | Status shows config error, pod may fail |
| CFG-005 | Config update triggers reload | Redis reloaded (if hot-reloadable) or restarted |

### 2.4 Authentication

| ID | Test Case | Expected Result |
|----|-----------|-----------------|
| AUTH-001 | No auth (default) | Connections succeed without password |
| AUTH-002 | Password auth enabled | Connections require password |
| AUTH-003 | Password from Secret | Password sourced from K8s Secret |
| AUTH-004 | Wrong password rejected | AUTH fails, connection rejected |
| AUTH-005 | Password rotation | New password effective after update |

### 2.5 TLS

| ID | Test Case | Expected Result |
|----|-----------|-----------------|
| TLS-001 | TLS disabled (default) | Plain TCP connections work |
| TLS-002 | TLS enabled | Only TLS connections accepted |
| TLS-003 | TLS cert from Secret | Certs loaded from specified Secret |
| TLS-004 | Invalid cert provided | Pod fails to start, status shows error |
| TLS-005 | Cert rotation | New certs effective (may require restart) |

### 2.6 Resource Management

| ID | Test Case | Expected Result |
|----|-----------|-----------------|
| RES-001 | Default resources applied | Sensible defaults set on pod |
| RES-002 | Custom resources via CR | User-specified resources applied |
| RES-003 | Guaranteed QoS achieved | requests == limits when defaults used |
| RES-004 | Pod evicted at memory limit | Redis eviction before OOMKill |

### 2.7 Metrics & Observability

| ID | Test Case | Expected Result |
|----|-----------|-----------------|
| MET-001 | redis_exporter sidecar running | Sidecar container present and healthy |
| MET-002 | Metrics endpoint accessible | `/metrics` returns Prometheus format |
| MET-003 | Key metrics present | `redis_connected_clients`, `redis_memory_used_bytes`, etc. |
| MET-004 | ServiceMonitor created | When enabled, ServiceMonitor CR exists |
| MET-005 | ServiceMonitor not created | When disabled, no ServiceMonitor CR |
| MET-006 | CR status reflects health | `.status.conditions` accurate |

### 2.8 Networking

| ID | Test Case | Expected Result |
|----|-----------|-----------------|
| NET-001 | Service created | ClusterIP service for Redis |
| NET-002 | Service selects correct pods | Traffic routes to Redis pod |
| NET-003 | Headless service (if needed) | StatefulSet has headless service |
| NET-004 | Custom service annotations | User annotations applied |

---

## 3. MVP Test Cases (Sentinel Mode)

### 3.1 Sentinel Deployment

| ID | Test Case | Expected Result |
|----|-----------|-----------------|
| SEN-001 | Deploy Sentinel mode | 1 master + 2 replicas + 3 sentinels running |
| SEN-002 | All pods become ready | All 6 pods in Ready state |
| SEN-003 | Sentinels discover master | `SENTINEL master` returns master info |
| SEN-004 | Replicas connected to master | `INFO replication` shows connected replicas |
| SEN-005 | Sentinels agree on quorum | All sentinels report same master |

### 3.2 Persistence Disabled - Sentinel Mode (Critical)

Same guarantees as standalone: no persistence, pure in-memory.

| ID | Test Case | Expected Result |
|----|-----------|-----------------|
| SENP-001 | No PVCs created for Redis pods | No PVCs for any of the 3 Redis pods |
| SENP-002 | No PVCs created for Sentinel pods | No PVCs for any of the 3 Sentinel pods |
| SENP-003 | Master has RDB disabled | Master: `CONFIG GET save` returns empty |
| SENP-004 | Replicas have RDB disabled | All replicas: `CONFIG GET save` returns empty |
| SENP-005 | Master has AOF disabled | Master: `CONFIG GET appendonly` returns `no` |
| SENP-006 | Replicas have AOF disabled | All replicas: `CONFIG GET appendonly` returns `no` |
| SENP-007 | No persistence files on any pod | No dump.rdb or AOF on master or replicas |
| SENP-008 | Full cluster restart loses data | Delete all pods, verify empty keyspace after recovery |

### 3.3 Failover

| ID | Test Case | Expected Result |
|----|-----------|-----------------|
| FAIL-001 | Master pod deleted | New master elected, replicas reconfigure |
| FAIL-002 | Master pod killed (SIGKILL) | Automatic failover within timeout |
| FAIL-003 | Master node drained | Graceful failover before pod terminates |
| FAIL-004 | Old master rejoins | Becomes replica of new master |
| FAIL-005 | Multiple sequential failures | System recovers each time |

### 3.4 Data Consistency (Sentinel)

Data survives failover via **replication**, not persistence.

| ID | Test Case | Expected Result |
|----|-----------|-----------------|
| DATA-001 | Writes go to master | Master receives SET commands |
| DATA-002 | Reads from replicas (if configured) | Data available on replicas |
| DATA-003 | Data survives single failover | Data replicated to new master (via replica promotion) |
| DATA-004 | Write during failover | Client retries succeed after failover |
| DATA-005 | Data lost on full cluster restart | All pods deleted → empty keyspace (expected, no persistence) |

### 3.4 Sentinel Services

| ID | Test Case | Expected Result |
|----|-----------|-----------------|
| SVC-001 | Master service exists | Points to current master |
| SVC-002 | Master service updates on failover | Endpoints update to new master |
| SVC-003 | Replica service exists | Points to all replicas |
| SVC-004 | Sentinel service exists | Points to all sentinels |

### 3.5 Upgrades (Sentinel Mode)

| ID | Test Case | Expected Result |
|----|-----------|-----------------|
| UPG-001 | Rolling update (replicas first) | Replicas updated, then master, minimal downtime |
| UPG-002 | Rolling update preserves quorum | Always ≥2 sentinels available |
| UPG-003 | Recreate update | All pods replaced, controlled downtime |
| UPG-004 | Version upgrade 7.2→7.4 | Successful upgrade, no data loss |

---

## 4. Operator Tests

### 4.1 Operator Lifecycle

| ID | Test Case | Expected Result |
|----|-----------|-----------------|
| OPR-001 | Operator starts successfully | Logs show ready, no errors |
| OPR-002 | Operator handles CRD not installed | Graceful error, waits for CRD |
| OPR-003 | Operator restart recovery | Reconciles all existing CRs |
| OPR-004 | Operator leader election | Only one active controller |
| OPR-005 | Operator namespace-scoped mode | Only watches specified namespace |
| OPR-006 | Operator cluster-scoped mode | Watches all namespaces |

### 4.2 Error Handling

| ID | Test Case | Expected Result |
|----|-----------|-----------------|
| ERR-001 | Network partition (temporary) | Retries, eventually reconciles |
| ERR-002 | API server unavailable | Retries with backoff |
| ERR-003 | Resource quota exceeded | Status shows quota error |
| ERR-004 | Invalid image specified | Pod fails, status shows pull error |
| ERR-005 | RBAC insufficient | Clear error in operator logs |

### 4.3 Reconciliation

| ID | Test Case | Expected Result |
|----|-----------|-----------------|
| REC-001 | Manual pod deletion | Operator recreates pod |
| REC-002 | Manual service deletion | Operator recreates service |
| REC-003 | Manual configmap deletion | Operator recreates configmap |
| REC-004 | Drift detection | Operator corrects manual changes |
| REC-005 | Concurrent reconciliation | No race conditions |

---

## 5. Negative Tests

### 5.1 Invalid Inputs

| ID | Test Case | Expected Result |
|----|-----------|-----------------|
| NEG-001 | Empty spec | Admission rejected or defaults applied |
| NEG-002 | Invalid mode value | Admission rejected |
| NEG-003 | Negative replica count | Admission rejected |
| NEG-004 | Invalid image reference | Status shows error |
| NEG-005 | Missing required Secret | Status shows error, pods pending |

### 5.2 Resource Constraints

| ID | Test Case | Expected Result |
|----|-----------|-----------------|
| NEG-006 | Node memory exhausted | Pod pending, status reflects |
| NEG-007 | PV provisioner unavailable | N/A (no persistence needed) |
| NEG-008 | Network policy blocks Redis | Connection failures surfaced |

### 5.3 Persistence Guard Rails

Tests to catch regressions that might accidentally enable persistence.

| ID | Test Case | Expected Result |
|----|-----------|-----------------|
| NEG-009 | ConfigMap missing `save ""` | Test fails - regression detected |
| NEG-010 | ConfigMap missing `appendonly no` | Test fails - regression detected |
| NEG-011 | StatefulSet has volumeClaimTemplates | Test fails - no PVCs should be templated |
| NEG-012 | Pod has PVC mounted | Test fails - no persistent volumes |
| NEG-013 | Redis defaults override our config | After deploy, `CONFIG GET save` must be empty |
| NEG-014 | Image entrypoint enables persistence | Verify our config takes precedence over image defaults |

**Regression Test Implementation**:

```go
// This should be a unit test that fails if someone accidentally
// removes the persistence-disabling config
func TestConfigMapDisablesPersistence(t *testing.T) {
    cm := buildConfigMap(cr)
    config := cm.Data["redis.conf"]

    assert.Contains(t, config, `save ""`)
    assert.Contains(t, config, "appendonly no")
}

// E2E test to verify runtime behavior
func TestNoPersistenceAtRuntime(t *testing.T) {
    // Deploy CR
    // Wait for ready

    // Check CONFIG
    save := redisClient.ConfigGet("save")
    assert.Equal(t, "", save)

    aof := redisClient.ConfigGet("appendonly")
    assert.Equal(t, "no", aof)

    // Write data
    redisClient.Set("key", "value")

    // Verify no files created
    _, err := exec.Command("kubectl", "exec", pod, "--",
        "ls", "/data/dump.rdb").Output()
    assert.Error(t, err) // File should not exist
}
```

---

## 6. Performance Tests

| ID | Test Case | Expected Result |
|----|-----------|-----------------|
| PERF-001 | Baseline throughput | Establish ops/sec baseline |
| PERF-002 | Latency p99 | < 1ms for simple operations |
| PERF-003 | Memory efficiency | Overhead < X% of maxmemory |
| PERF-004 | Failover time | < 30 seconds to new master |
| PERF-005 | Operator reconcile latency | < 5 seconds for typical changes |

---

## 7. Test Data & Fixtures

### 7.1 Sample CRs

Location: `test/fixtures/`

- `minimal-standalone.yaml` - Bare minimum standalone CR
- `full-standalone.yaml` - All options specified
- `minimal-sentinel.yaml` - Bare minimum sentinel CR
- `full-sentinel.yaml` - All sentinel options
- `invalid-*.yaml` - Various invalid configurations

### 7.2 Test Utilities

- Redis client wrapper for test assertions
- Wait helpers for pod/service readiness
- Failover trigger utilities
- Metrics scraping helpers

---

## 8. CI/CD Integration

### 8.1 PR Checks

- Unit tests (fast, always run)
- Integration tests with envtest (medium, always run)
- **Persistence guard tests (PERS-*, NEG-009 to NEG-014)** - MANDATORY, always run
- Linting (golangci-lint)
- YAML validation

### 8.2 Merge to Main

- Full E2E suite
- Multiple K8s versions (1.28, 1.29, 1.30)
- Both Redis and Valkey images
- **Persistence verification on all image variants**

### 8.3 Release

- Full E2E suite
- Performance benchmarks
- Upgrade tests from previous version
- **Persistence tests across all supported images**

### 8.4 Critical Test Categories

Tests that MUST NOT be skipped or marked flaky:

| Category | Test IDs | Reason |
|----------|----------|--------|
| Persistence disabled | PERS-001 to PERS-013 | Core promise of the operator |
| Sentinel persistence | SENP-001 to SENP-008 | Applies to HA mode too |
| Regression guards | NEG-009 to NEG-014 | Catch accidental persistence enablement |
