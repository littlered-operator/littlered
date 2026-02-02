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

Once the operator is deployed, run the e2e tests against it.

### Run All Tests

```bash
# Skip operator deployment (use existing ArgoCD deployment)
SKIP_OPERATOR_DEPLOY=true go test -tags=e2e ./test/e2e/ -v -ginkgo.v
```

The `-v` and `-ginkgo.v` flags provide verbose output so you can see test progress.

### Run Specific Test Suites

Run only standalone mode tests:
```bash
SKIP_OPERATOR_DEPLOY=true go test -tags=e2e ./test/e2e/ -v -ginkgo.v -ginkgo.focus="Standalone"
```

Run only sentinel mode tests:
```bash
SKIP_OPERATOR_DEPLOY=true go test -tags=e2e ./test/e2e/ -v -ginkgo.v -ginkgo.focus="Sentinel Mode"
```

Run only failover tests:
```bash
SKIP_OPERATOR_DEPLOY=true go test -tags=e2e ./test/e2e/ -v -ginkgo.v -ginkgo.focus="Failover"
```

## What the Tests Cover

### Standalone Mode Tests

| Test | Description |
|------|-------------|
| CR Creation | Creates a LittleRed resource with `mode: standalone` |
| StatefulSet | Verifies StatefulSet is created with 1 replica |
| Service | Verifies Service is created |
| Status | Verifies LittleRed status becomes "Running" |
| Redis PING | Executes `PING` command, expects `PONG` |
| SET/GET | Writes and reads a key-value pair |
| Metrics | Verifies Prometheus metrics are exposed on port 9121 |
| Pod Recreation | Deletes the pod, verifies it's recreated and Redis works |
| Cleanup | Deletes the CR, verifies all resources are removed |

### Sentinel Mode Tests

| Test | Description |
|------|-------------|
| CR Creation | Creates a LittleRed resource with `mode: sentinel` |
| StatefulSet | Verifies StatefulSet is created with 3 replicas |
| Services | Verifies master, replicas, and sentinel services exist |
| Status | Verifies LittleRed status becomes "Running" |
| Sentinel Quorum | Verifies sentinels have established quorum |
| Master Info | Verifies status reports current master pod |
| Replication | Writes to master, reads from replica |

### Failover Tests

| Test | Description |
|------|-------------|
| Setup | Creates sentinel cluster with fast failover settings |
| Master Deletion | Deletes the master pod |
| New Master Election | Verifies a new master is elected within timeout |
| Data Preservation | Verifies data written before failover is still accessible |
| Cluster Recovery | Verifies all 3 pods return to ready state |
| Replica Count | Verifies sentinel sees 2 replicas after recovery |

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

Increase timeouts by setting Ginkgo flags:
```bash
SKIP_OPERATOR_DEPLOY=true go test -tags=e2e ./test/e2e/ -v -ginkgo.v -timeout 20m
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
