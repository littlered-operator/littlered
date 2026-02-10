# E2E Testing Guide

This guide explains how to build, deploy, and test the LittleRed operator end-to-end.

## Overview

E2E (end-to-end) tests verify that the operator works correctly in a real Kubernetes cluster. The tests:

1. Create LittleRed custom resources
2. Verify the operator creates the expected Kubernetes resources (StatefulSets, Services, etc.)
3. Verify Redis is functional (PING, SET/GET operations)
4. Test failover scenarios in sentinel mode

## Prerequisites

- **Kubernetes cluster** with `kubectl` access
- **Podman** or **Docker** for building images
- **Container registry** to push images (e.g., `registry.tanne3.de`)
- **Go 1.24+** for running tests
- **ArgoCD** configured (for GitOps deployment)

## Step 1: Build and Push the Operator Image

Build the operator container image and push it to your registry.

```bash
# Navigate to the project directory
cd /path/to/littlered-operator

# Build the image (using podman)
podman build -t registry.tanne3.de/littlered-operator:0.1.0 .

# Or with docker
docker build -t registry.tanne3.de/littlered-operator:0.1.0 .

# Push to your registry
podman push registry.tanne3.de/littlered-operator:0.1.0

# Or with docker
docker push registry.tanne3.de/littlered-operator:0.1.0
```

Replace `registry.tanne3.de` with your own registry.

## Step 2: Deploy the Operator via ArgoCD

Add your Git repository to ArgoCD and create an application that deploys the operator.

### Add the Repository

```bash
argocd repo add https://gitea.tanne3.de/tanne3/littlered.git
```

### Create the ArgoCD Application

```bash
argocd app create littlered-operator \
  --repo https://gitea.tanne3.de/tanne3/littlered.git \
  --path charts/littlered-operator \
  --dest-server https://kubernetes.default.svc \
  --dest-namespace littlered-system \
  --sync-policy automated \
  --auto-prune \
  --self-heal \
  --helm-set image.repository=registry.tanne3.de/littlered-operator \
  --helm-set image.tag=0.1.0
```

**What this does:**
- `--repo`: Points to your Git repository containing the Helm chart
- `--path`: Location of the Helm chart within the repo
- `--dest-namespace`: Kubernetes namespace where the operator will run
- `--sync-policy automated`: Automatically deploys when you push changes to Git
- `--auto-prune`: Removes resources that are no longer in Git
- `--self-heal`: Reverts manual changes made outside of Git
- `--helm-set`: Overrides values in the Helm chart (your image location)

### Create the Namespace

The namespace must exist before ArgoCD can deploy:

```bash
kubectl create namespace littlered-system
```

### Verify Deployment

Check that the operator is running:

```bash
# Check ArgoCD app status
argocd app get littlered-operator

# Check the operator pod
kubectl get pods -n littlered-system
```

You should see output like:
```
NAME                                READY   STATUS    RESTARTS   AGE
littlered-operator-b7645665-8n9ph   1/1     Running   0          2m
```

## Step 3: Run the E2E Tests

Once the operator is deployed, run the e2e tests against it using the `Makefile` targets. The `Makefile` ensures all required environment variables (like `CHAOS_CLIENT_IMAGE`) are correctly set.

### Run All Tests

To run the full suite (this will also create/destroy a local Kind cluster if `SKIP_OPERATOR_DEPLOY` is not true):

```bash
make test-e2e
```

### Run with an Existing Deployment

If the operator is already deployed (e.g., via ArgoCD as described in Step 2), use `SKIP_OPERATOR_DEPLOY=true` to run tests against the existing cluster without attempting to manage a local Kind instance:

```bash
make test-e2e SKIP_OPERATOR_DEPLOY=true
```

### Run Specific Test Suites

Use the `FOCUS` variable to filter tests by name (regex). This is passed to Ginkgo's `-focus` flag.

Run only standalone mode tests:
```bash
make test-e2e SKIP_OPERATOR_DEPLOY=true FOCUS="Standalone"
```

Run only sentinel mode tests:
```bash
make test-e2e SKIP_OPERATOR_DEPLOY=true FOCUS="Sentinel Mode"
```

Run only failover tests:
```bash
make test-e2e SKIP_OPERATOR_DEPLOY=true FOCUS="Failover"
```

Run only cluster mode tests:
```bash
make test-e2e SKIP_OPERATOR_DEPLOY=true FOCUS="Cluster Mode"
```

The `make` command also supports passing additional arguments to `go test` via the `ARGS` variable:
```bash
make test-e2e SKIP_OPERATOR_DEPLOY=true ARGS="-timeout 45m"
```

## Cluster Mode Testing

Cluster mode testing is organized into functional and chaos-oriented suites to ensure both correct topology management and high availability.

### 1. Functional Testing (`cluster_functional_test.go`)
Focuses on correct cluster formation, configuration, and self-healing.
- **Basic Operations**: Cluster creation (3 masters, 3 replicas), data redirection, and status tracking.
- **0-Replica Mode**: Verifies that the operator can restore slots to new nodes even without replicas.
- **Functional Recovery**: Validates correct topology (no empty masters, correct replica count) after master or replica pod deletion.
- **Custom Configuration**: Application of custom Redis settings and node timeouts.
- **Cleanup**: Ensures all K8s resources are removed when the CR is deleted.

### 2. Chaos Testing (`cluster_chaos_test.go`)
Validates stability and data integrity under continuous load using a chaos client.
- **Stability Baseline**: Verifies 100% availability in a stable cluster.
- **Master/Replica Failure**: Ensures >90% write availability and 0% data corruption during unplanned failovers.
- **Rolling Restarts**: Verifies that controlled updates (via annotation changes) do not cause data loss or downtime.

### Key Topology Guarantees
The E2E tests strictly validate the following cluster invariants after any failure:
1. **No Empty Masters**: Every master node must have slots assigned.
2. **Correct Replica Count**: Every shard must have the expected number of healthy replicas.
3. **Ghost Cleanup**: Nodes that no longer exist in K8s must be forgotten by the cluster gossip.
4. **Data Integrity**: Chaos clients verify that every successful write can be read back, even during failover events.

## Test Coverage

For a detailed list of implemented test cases, scenarios, and their IDs, please refer to [TEST_CASES.md](TEST_CASES.md).

## Running the Tests

## Troubleshooting

### Tests Fail to Connect to Cluster

Verify your kubectl context:
```bash
kubectl config current-context
kubectl get nodes
```

### Operator Not Running

Check operator logs:
```bash
kubectl logs -n littlered-system deployment/littlered-operator
```

Check ArgoCD sync status:
```bash
argocd app get littlered-operator
```

### Tests Timeout Waiting for Resources

Increase timeouts by setting Ginkgo flags (the default is 30m):
```bash
SKIP_OPERATOR_DEPLOY=true go test -tags=e2e ./test/e2e/ -v -ginkgo.v -timeout 45m
```

### View Test Resources

During or after test runs, inspect created resources:
```bash
kubectl get littlered -n littlered-e2e-test
kubectl get pods -n littlered-e2e-test
kubectl get svc -n littlered-e2e-test
```

### Clean Up Test Resources

Tests clean up after themselves, but if interrupted:
```bash
kubectl delete namespace littlered-e2e-test --ignore-not-found
```

## Updating the Operator

When you make changes to the operator:

1. **Build and push a new image:**
   ```bash
   podman build -t registry.tanne3.de/littlered-operator:0.1.1 .
   podman push registry.tanne3.de/littlered-operator:0.1.1
   ```

2. **Update the ArgoCD application:**
   ```bash
   argocd app set littlered-operator --helm-set image.tag=0.1.1
   ```

   Or update the Helm chart's `values.yaml` in Git and push — ArgoCD will automatically sync.

3. **Run tests again:**
   ```bash
   SKIP_OPERATOR_DEPLOY=true go test -tags=e2e ./test/e2e/ -v -ginkgo.v
   ```

## Environment Variables Reference

| Variable | Description | Default |
|----------|-------------|---------|
| `SKIP_OPERATOR_DEPLOY` | Skip deploying operator (use existing) | `false` |
| `OPERATOR_IMAGE` | Image to use when deploying | `ghcr.io/tanne3/littlered-operator:latest` |
| `USE_HELM` | Deploy via Helm instead of make | `false` |
| `KIND_CLUSTER` | Kind cluster name (for local testing) | `kind` |
