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

---

## Chaos Testing

The chaos tests validate resilience and data integrity under failure scenarios using a dedicated chaos test client.

### Chaos Test Client

The chaos client (`cmd/chaos-client/`) is a standalone tool that:

- **Writes** at a configurable rate (default: 10 ops/sec)
- **Keys**: Auto-incrementing integers (0, 1, 2, ...)
- **Values**: SHA256 hash of the key (deterministic, verifiable)
- **Reads**: Random keys from confirmed-written set, verifies value integrity
- **Tracks**: Write/read success, failures, and data corruptions
- **Timeout**: Short operation timeouts (500ms default) for fast failure detection

#### Building the Chaos Client

```bash
# Build locally
go build -o chaos-client ./cmd/chaos-client

# Build container image
docker build -t registry.tanne3.de/littlered-chaos-client:latest -f cmd/chaos-client/Dockerfile .
docker push registry.tanne3.de/littlered-chaos-client:latest
```

#### Running Manually

```bash
# Against a cluster
./chaos-client -addrs=redis-cluster:6379 -cluster -duration=60s -prefix=test

# Against standalone
./chaos-client -addrs=redis:6379 -duration=60s -prefix=test

# All flags
./chaos-client \
  -addrs=host1:6379,host2:6379 \  # Comma-separated addresses
  -password=secret \               # Redis password (optional)
  -cluster \                       # Enable cluster mode
  -write-rate=100ms \              # Interval between writes
  -timeout=500ms \                 # Per-operation timeout
  -prefix=mytest \                 # Key prefix
  -duration=60s \                  # Test duration (0=until interrupted)
  -status-interval=5s              # Status output interval
```

#### Output Format

The client outputs periodic status and a final JSON metrics line:

```
[5s] Writes: 50/50 (100.0%), Reads: 48/48 (100.0%), Corruptions: 0
[10s] Writes: 100/100 (100.0%), Reads: 95/95 (100.0%), Corruptions: 0
...
========== FINAL RESULTS ==========
Writes: 600 attempted, 580 succeeded, 20 failed (96.67% availability)
Reads: 590 attempted, 570 succeeded, 20 failed (96.61% availability)
Data corruptions: 0
Highest confirmed key: 580

METRICS_JSON:{"writeAttempts":600,"writeSuccesses":580,...}
```

### Chaos Test Suites

#### 6. Cluster Mode Resilience
**Test Suite:** `Context("Cluster Mode Resilience")`

Tests cluster behavior under chaos conditions:

- ✅ **Replica Pod Deletion**: Continuous load while deleting replica
  - Expects: ≥80% write availability, ≥80% read availability, 0 data corruptions

- ✅ **Master Pod Deletion with Failover**: Continuous load while deleting master
  - Expects: ≥50% write availability (one shard affected), 0 data corruptions

- ✅ **Rolling Restart**: Continuous load during cluster annotation change
  - Expects: ≥70% availability, 0 data corruptions

#### 7. Standalone Mode Resilience
**Test Suite:** `Context("Standalone Mode Resilience")`

- ✅ **Pod Restart**: Tests recovery after standalone pod deletion
  - Expects: Data loss (no replication), but no data corruption
  - Verifies operator restores the pod

### Running Chaos Tests

#### Prerequisites

1. Chaos client image pushed to registry
2. Kubernetes cluster with operator deployed
3. Registry accessible from cluster

#### Run All Chaos Tests

```bash
# Set the chaos client image
export CHAOS_CLIENT_IMAGE=registry.tanne3.de/littlered-chaos-client:latest

# Run chaos tests
SKIP_OPERATOR_DEPLOY=true go test -tags=e2e -v ./test/e2e/... \
  -ginkgo.focus="Chaos Testing"
```

#### Run Specific Chaos Test

```bash
# Test replica deletion resilience
CHAOS_CLIENT_IMAGE=registry.tanne3.de/littlered-chaos-client:latest \
SKIP_OPERATOR_DEPLOY=true go test -tags=e2e -v ./test/e2e/... \
  -ginkgo.focus="replica pod deletion"

# Test master failover
CHAOS_CLIENT_IMAGE=registry.tanne3.de/littlered-chaos-client:latest \
SKIP_OPERATOR_DEPLOY=true go test -tags=e2e -v ./test/e2e/... \
  -ginkgo.focus="master pod deletion"
```

### Chaos Test Architecture

```
┌─────────────────────────────────────────────────────────────────┐
│                         E2E Test Process                         │
│  1. Creates LittleRed cluster                                   │
│  2. Deploys chaos-client Pod                                    │
│  3. Waits for baseline                                          │
│  4. Performs chaos action (delete pod, annotate, etc.)          │
│  5. Waits for chaos-client to complete                          │
│  6. Parses METRICS_JSON from pod logs                           │
│  7. Asserts availability and data integrity                     │
└─────────────────────────────────────────────────────────────────┘
         │                                      │
         │ kubectl apply                        │ kubectl logs
         ▼                                      ▼
┌─────────────────────┐              ┌─────────────────────┐
│   chaos-client Pod  │─────────────▶│   Redis Cluster     │
│                     │   SET/GET    │   (6 pods)          │
│  - Writes 10/sec    │◀─────────────│                     │
│  - Reads & verifies │              │                     │
│  - Tracks metrics   │              │                     │
└─────────────────────┘              └─────────────────────┘
```

### Key Metrics

| Metric | Description |
|--------|-------------|
| `writeAttempts` | Total SET operations attempted |
| `writeSuccesses` | SET operations confirmed (got OK) |
| `writeFailures` | SET operations that failed/timed out |
| `readAttempts` | GET operations attempted |
| `readSuccesses` | GET operations with correct value |
| `readFailures` | GET operations that failed/timed out |
| `dataCorruptions` | GET returned wrong value (**critical!**) |

### Test Expectations

The chaos tests assert **data integrity only** - availability metrics are logged but not asserted:

| Scenario | Data Corruption | Availability |
|----------|-----------------|--------------|
| Replica deletion | **Must be 0** | Logged (informational) |
| Master deletion (failover) | **Must be 0** | Logged (informational) |
| Rolling restart | **Must be 0** | Logged (informational) |
| Standalone restart | **Must be 0** | Logged (informational) |

**Rationale**: Availability during chaos varies based on timing, cluster resources, and network conditions. Data integrity is the critical guarantee - we must never return wrong data.

### Debugging Chaos Tests

#### Check Chaos Client Logs

```bash
# Get chaos client pod logs
kubectl logs chaos-client-replica-delete -n littlered-chaos-e2e-test

# Watch chaos client in real-time
kubectl logs -f chaos-client-replica-delete -n littlered-chaos-e2e-test
```

#### Check Cluster During Chaos

```bash
# Cluster state
kubectl exec chaos-cluster-cluster-0 -n littlered-chaos-e2e-test -c redis -- \
  valkey-cli CLUSTER INFO

# Node status
kubectl exec chaos-cluster-cluster-0 -n littlered-chaos-e2e-test -c redis -- \
  valkey-cli CLUSTER NODES
```

#### Common Issues

1. **Chaos client can't connect**: Check service DNS resolution and network policies
2. **High failure rate**: Check operation timeout (-timeout flag) vs network latency
3. **Data corruption detected**: Critical bug - investigate immediately
4. **Test timeout**: Chaos client pod might be stuck - check pod status and logs
