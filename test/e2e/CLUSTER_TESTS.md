# LittleRed End-to-End Tests

This document describes the comprehensive end-to-end tests for LittleRed, including cluster mode and chaos/resilience testing.

## Test Coverage

### 1. Basic Cluster Operations
**Test Suite:** `Context("Basic Cluster Operations")`

Tests the fundamental cluster creation and operation:
- ✅ Creates Redis Cluster with 3 shards and 1 replica per shard (6 total pods)
- ✅ Verifies StatefulSet with 6 replicas
- ✅ Verifies client and headless services are created
- ✅ Checks LittleRed status reaches "Running" phase
- ✅ Validates cluster state is "ok"
- ✅ Confirms all 16384 slots are assigned
- ✅ Verifies correct topology (3 masters, 3 replicas)
- ✅ Validates slot distribution across masters
- ✅ Tests SET/GET operations in cluster mode
- ✅ Verifies key distribution across different shards
- ✅ Checks cluster state tracking in CR status
- ✅ Validates ClusterReady condition

### 2. Cluster Recovery and Resilience
**Test Suite:** `Context("Cluster Recovery and Resilience")`

Tests cluster behavior under failure scenarios:
- ✅ **Replica Pod Deletion**: Deletes a replica pod and verifies:
  - Pod is automatically recreated by StatefulSet
  - Cluster remains healthy (state: ok)
  - Data remains accessible

- ✅ **Master Pod Deletion**: Deletes a master pod and tests:
  - Pod recreation
  - Replica promotion to master (if applicable)
  - Cluster re-stabilization
  - All slots remain assigned

- ✅ **Multiple Pod Deletion**: Deletes 3 pods (half the cluster) and verifies:
  - All pods are recreated
  - Cluster fully recovers
  - All 6 nodes are tracked
  - Cluster state returns to "ok"

### 3. Cluster Configuration
**Test Suite:** `Context("Cluster Configuration")`

Tests custom configuration options:
- ✅ Creates cluster with custom shard/replica settings
- ✅ Validates custom `maxmemory` configuration
- ✅ Validates custom `maxmemory-policy` (allkeys-lru)
- ✅ Validates custom `cluster-node-timeout` setting

### 4. Cluster Status Tracking
**Test Suite:** `Context("Cluster Status Tracking")`

Tests CR status field accuracy:
- ✅ Tracks `lastBootstrap` timestamp
- ✅ Tracks all 6 nodes with complete details:
  - `podName`
  - `nodeId` (40-character cluster node ID)
  - `role` (master/replica)
  - `slotRanges` (for masters)
  - `masterNodeId` (for replicas)
- ✅ Validates master nodes have slot ranges
- ✅ Validates replica nodes reference their masters
- ✅ Checks Ready condition is True
- ✅ Verifies correct ready/total pod counts

### 5. Cluster Cleanup
**Test Suite:** `Context("Cluster Cleanup")`

Tests resource cleanup:
- ✅ Creates a cluster
- ✅ Deletes the LittleRed CR
- ✅ Verifies all resources are cleaned up:
  - StatefulSet deleted
  - All pods deleted
  - Services deleted (client and headless)
  - ConfigMap deleted

## Running the Tests

### Prerequisites

1. **Kind cluster** (created automatically by `make setup-test-e2e`)
2. **Operator deployed** to the test cluster
3. **CRDs installed**

### Run All Cluster Tests

```bash
# Set up test environment (creates Kind cluster)
make setup-test-e2e

# Run all cluster mode e2e tests
make test-e2e
```

### Run Specific Test Suites

```bash
# Run only cluster tests
KIND_CLUSTER=redis-operator-test-e2e go test -tags=e2e ./test/e2e/cluster_test.go -v -ginkgo.v -timeout 30m -ginkgo.focus="Cluster Mode"

# Run specific test context
KIND_CLUSTER=redis-operator-test-e2e go test -tags=e2e ./test/e2e/cluster_test.go -v -timeout 30m -ginkgo.focus="Basic Cluster Operations"

# Run specific test
KIND_CLUSTER=redis-operator-test-e2e go test -tags=e2e ./test/e2e/cluster_test.go -v -timeout 30m -ginkgo.focus="should create a Redis Cluster"
```

### Run Tests with Custom Cluster

If you have a specific Kubernetes cluster configured:

```bash
# Ensure kubectl is configured for your cluster
kubectl config current-context

# Run tests
go test -tags=e2e ./test/e2e/cluster_test.go -v -ginkgo.v -timeout 30m
```

### Cleanup After Tests

```bash
# Delete the Kind cluster
make cleanup-test-e2e

# Or manually
kind delete cluster --name redis-operator-test-e2e
```

## Test Timeouts and Expectations

- **Cluster Bootstrap**: 4 minutes (allows time for 6 pods + cluster formation)
- **Pod Recreation**: 2 minutes per pod
- **Cluster Stabilization**: 3-4 minutes after major failures
- **Individual Operations**: 1-2 minutes

## Known Behaviors

1. **Data Loss on Pod Restart**: Since cluster mode uses EmptyDir volumes (no persistence), data is lost when pods restart. This is expected for cache-first design.

2. **Master Pod Deletion**: When a master is deleted:
   - StatefulSet recreates the pod with a new Redis node ID
   - Operator detects the change and reassigns slots
   - Cluster may temporarily have failed slots during recovery

3. **Multiple Pod Deletion**: The cluster can recover from up to 3 simultaneous pod failures, but recovery takes longer as the operator must:
   - Wait for all pods to be recreated
   - Detect node ID changes for each pod
   - Run CLUSTER commands to rebuild topology

4. **Bootstrap Loop Prevention**: The operator uses pod IP matching (not hostname) to track node identity, preventing infinite bootstrap loops.

## Debugging Failed Tests

### Check Operator Logs

```bash
kubectl logs -n redis-operator-system deployment/redis-operator-controller-manager -f
```

### Check Cluster State

```bash
# Get cluster info
kubectl exec <pod-name> -n <namespace> -c redis -- redis-cli CLUSTER INFO

# Get cluster nodes
kubectl exec <pod-name> -n <namespace> -c redis -- redis-cli CLUSTER NODES

# Get CR status
kubectl get littlered <name> -n <namespace> -o yaml
```

### Check Pod Logs

```bash
# Redis container
kubectl logs <pod-name> -n <namespace> -c redis

# Exporter container
kubectl logs <pod-name> -n <namespace> -c exporter
```

### Common Issues

1. **Cluster stuck in "Initializing"**: Check operator logs for bootstrap errors
2. **Pods not starting**: Check resource constraints and image pull errors
3. **Tests timeout**: Increase timeout values or check cluster resources
4. **Slots not assigned**: Check for CLUSTER command errors in operator logs

## Test Structure

Each test suite follows this pattern:

```go
Context("Test Category", Ordered, func() {
    const crName = "unique-test-name"

    AfterAll(func() {
        // Cleanup
    })

    It("should do something", func() {
        By("step 1")
        // Test code

        By("step 2")
        // Verification
    })
})
```

The `Ordered` flag ensures tests run sequentially within each Context, allowing state to build up (e.g., create cluster, then test operations, then test recovery).

## Contributing

When adding new cluster tests:

1. Use unique `crName` for each Context to avoid conflicts
2. Add `AfterAll` cleanup to delete resources
3. Use `Eventually` with appropriate timeouts for async operations
4. Use `By()` to document test steps
5. Add descriptive test names starting with "should"
6. Consider test execution time (aim for < 5 minutes per test)

## CI/CD Integration

These tests are designed to run in CI/CD pipelines:

```yaml
# Example GitHub Actions workflow
- name: Run e2e tests
  run: |
    make setup-test-e2e
    make test-e2e
    make cleanup-test-e2e
```

The tests use isolated namespaces (`littlered-cluster-e2e-test`) to avoid conflicts with other test suites.
