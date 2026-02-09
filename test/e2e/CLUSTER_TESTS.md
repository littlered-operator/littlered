# Cluster Mode E2E Test Organization

## Test Files

### 1. `cluster_test.go` (Legacy - Candidate for Cleanup)
**Status**: Contains useful basic tests but recovery tests are superseded by newer files

**Keep these test contexts**:
- ✅ **Basic Cluster Operations** - Tests cluster creation, services, status (CLUST-001 to CLUST-011, CLUST-020, CLUST-021)
- ✅ **Cluster Configuration** - Tests custom config application (maxmemory, cluster-node-timeout)
- ✅ **Cluster Status Tracking** - Tests CR status fields (CLUST-022, lastBootstrap, node details)
- ✅ **Cluster Cleanup** - Tests resource cleanup on CR deletion (CLUST-033)

**Deprecate/Remove these test contexts**:
- ❌ **Cluster Recovery and Resilience** (CLUST-030, CLUST-031, CLUST-032)
  - These tests don't verify correct topology after recovery
  - Superseded by `cluster_failover_test.go` which properly validates:
    - No empty masters remaining
    - Correct replica reassignment
    - Ghost node cleanup

### 2. `cluster_restart_test.go` (Current - 0-Replica Mode)
**Purpose**: Tests self-healing in 0-replica mode (no failover, just slot reassignment)

**Tests**:
- ✅ **0-Replica Cluster Self-Healing** (CLUST-RESTART-001)
  - Master pod deletion
  - Verifies slot reassignment to empty master
  - Verifies ghost cleanup

**Status**: Keep as-is. Covers the 0-replica edge case.

### 3. `cluster_chaos_test.go` (Current - 0-Replica Mode)
**Purpose**: Chaos testing for 0-replica mode

**Tests**:
- ✅ **3-node 0-replica Cluster Stability** (CLUST-CHAOS-001)
  - Baseline stable cluster with chaos client
  - 100% availability expected

**Status**: Keep as-is. Covers chaos testing for 0-replica mode.

### 4. `cluster_failover_test.go` (NEW - Replica Mode with Failover)
**Purpose**: Tests failover and recovery in replica mode (the fix we just implemented)

**Tests**:
- ✅ **Master Pod Deletion with Replica Promotion** (CLUST-FAILOVER-001)
  - Functional test: Master dies, replica promoted, new pod becomes replica
  - Verifies correct topology: 3 masters with slots, 3 replicas, no empty masters

- ✅ **Replica Pod Deletion and Reassignment** (CLUST-FAILOVER-002)
  - Functional test: Replica dies, new pod joins and becomes replica
  - Verifies correct topology restoration

- ✅ **Chaos Testing - Master Deletion** (CLUST-FAILOVER-CHAOS-001)
  - Master deletion with active chaos client load
  - Verifies >95% availability during failover
  - Verifies correct topology after recovery
  - Verifies no data corruption

- ✅ **Chaos Testing - Replica Deletion** (CLUST-FAILOVER-CHAOS-002)
  - Replica deletion with active chaos client load
  - Verifies ~100% availability (replica deletion shouldn't impact availability)
  - Verifies correct topology after recovery

**Status**: New file. This is the comprehensive test for the failover fix.

## Test Coverage Matrix

| Scenario | 0-Replica Mode | Replica Mode (with Failover) |
|----------|----------------|------------------------------|
| **Functional Tests** | | |
| Basic cluster creation | ✅ cluster_test.go | ✅ cluster_test.go |
| Master pod deletion | ✅ cluster_restart_test.go | ✅ cluster_failover_test.go |
| Replica pod deletion | N/A | ✅ cluster_failover_test.go |
| Services & Config | ✅ cluster_test.go | ✅ cluster_test.go |
| Status tracking | ✅ cluster_test.go | ✅ cluster_test.go |
| **Chaos Tests** | | |
| Stable baseline | ✅ cluster_chaos_test.go | ✅ cluster_failover_test.go (implicit) |
| Master deletion | N/A (no failover) | ✅ cluster_failover_test.go |
| Replica deletion | N/A | ✅ cluster_failover_test.go |

## Recommendations

### Immediate Actions:
1. **Keep** `cluster_test.go` but remove the "Cluster Recovery and Resilience" context (tests CLUST-030, 031, 032)
2. **Keep** `cluster_restart_test.go` (0-replica mode tests)
3. **Keep** `cluster_chaos_test.go` (0-replica chaos tests)
4. **Add** `cluster_failover_test.go` (new comprehensive failover tests)

### Future Enhancements:
- Consider adding multi-pod deletion scenarios (e.g., master + replica simultaneously)
- Consider adding network partition scenarios
- Consider adding rolling update tests to verify failover doesn't trigger during controlled updates

## Running Tests

```bash
# Run all cluster tests
make test-e2e

# Run specific test file
go test -v -tags=e2e ./test/e2e -ginkgo.focus="Cluster Failover"

# Run specific test case
go test -v -tags=e2e ./test/e2e -ginkgo.focus="CLUST-FAILOVER-001"
```

## Key Improvements in New Tests

The new `cluster_failover_test.go` tests improve upon the old recovery tests by:

1. **Topology Validation**: Explicitly verifies no "empty masters" remain (masters without slots)
2. **Replica Count**: Ensures exactly 3 replicas after recovery
3. **Ghost Cleanup**: Verifies no ghost nodes remain
4. **Failover Testing**: Tests the actual failover mechanism we just fixed
5. **Chaos Integration**: Tests recovery under active load with metrics
6. **Availability Metrics**: Quantifies availability during failover (>95% for master, ~100% for replica)
7. **Data Integrity**: Verifies no data corruption during recovery
