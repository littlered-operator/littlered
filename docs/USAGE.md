# Using LittleRed

This guide shows how to deploy and use Redis instances with the LittleRed operator.

## Prerequisites

- Kubernetes 1.28+
- `kubectl` configured to access your cluster
- Helm 3.x (for Helm installation)

---

## Installing the Operator

### Option 1: Helm (Recommended)

```bash
# Clone the repository
git clone https://github.com/tanne3/littlered-operator.git
cd littlered-operator

# Install the operator
helm install littlered ./charts/littlered-operator \
  -n littlered-system \
  --create-namespace
```

#### Verify installation

```bash
# Check operator pod is running
kubectl get pods -n littlered-system

# Expected output:
# NAME                                  READY   STATUS    RESTARTS   AGE
# littlered-operator-xxxxxxxxx-xxxxx   1/1     Running   0          30s

# Check CRD is installed
kubectl get crd littlereds.chuck-chuck-chuck.net
```

#### Custom configuration

Create a `values.yaml` file:

```yaml
image:
  repository: ghcr.io/tanne3/littlered-operator
  tag: "0.1.0"

resources:
  limits:
    cpu: 500m
    memory: 256Mi
  requests:
    cpu: 100m
    memory: 128Mi

# For HA setups (multiple operator replicas)
replicas: 2
leaderElection:
  enabled: true
```

Install with custom values:

```bash
helm install littlered ./charts/littlered-operator \
  -n littlered-system \
  --create-namespace \
  -f values.yaml
```

#### Upgrade

```bash
helm upgrade littlered ./charts/littlered-operator -n littlered-system
```

**Important: Upgrading CRDs**

Helm does not automatically update CRDs on `helm upgrade`. If the LittleRed CRD schema has changed (e.g., new fields like `spec.cluster`), you must apply the CRD manually:

```bash
kubectl apply -f charts/littlered-operator/crds/chuck-chuck-chuck.net_littlereds.yaml
```

#### Uninstall

```bash
helm uninstall littlered -n littlered-system

# CRDs are not deleted automatically. To remove them:
kubectl delete crd littlereds.chuck-chuck-chuck.net
```

### Option 2: Kustomize

```bash
# Clone the repository
git clone https://github.com/tanne3/littlered-operator.git
cd littlered-operator

# Install CRDs and operator
kubectl apply -k config/default
```

#### Verify installation

```bash
kubectl get pods -n redis-operator-system
kubectl get crd littlereds.chuck-chuck-chuck.net
```

#### Uninstall

```bash
kubectl delete -k config/default
```

### Option 3: ArgoCD

Create an ArgoCD Application:

```yaml
apiVersion: argoproj.io/v1alpha1
kind: Application
metadata:
  name: littlered-operator
  namespace: argocd
spec:
  project: default
  source:
    repoURL: https://github.com/tanne3/littlered-operator.git
    targetRevision: main
    path: charts/littlered-operator
    helm:
      values: |
        image:
          tag: "0.1.0"
  destination:
    server: https://kubernetes.default.svc
    namespace: littlered-system
  syncPolicy:
    automated:
      prune: true
      selfHeal: true
    syncOptions:
      - CreateNamespace=true
```

---

## Standalone Mode

A single Redis instance for development or simple caching.

### Deploy

```yaml
# standalone.yaml
apiVersion: chuck-chuck-chuck.net/v1alpha1
kind: LittleRed
metadata:
  name: my-cache
spec:
  mode: standalone
```

```bash
kubectl apply -f standalone.yaml
```

### Verify

```bash
# Check status
kubectl get littlered my-cache

# Expected output:
# NAME       MODE         PHASE     READY   AGE
# my-cache   standalone   Running   1       30s

# Check pods
kubectl get pods -l app.kubernetes.io/instance=my-cache

# Check service
kubectl get svc my-cache
```

### Connect

```bash
# Test Redis connection
kubectl exec -it my-cache-redis-0 -c redis -- valkey-cli PING
# Output: PONG

# Set and get a value
kubectl exec -it my-cache-redis-0 -c redis -- valkey-cli SET hello world
kubectl exec -it my-cache-redis-0 -c redis -- valkey-cli GET hello
# Output: world

# Check Redis info
kubectl exec -it my-cache-redis-0 -c redis -- valkey-cli INFO server
```

### Connect from your application

```bash
# Service endpoint
my-cache.<namespace>.svc.cluster.local:6379
```

---

## Sentinel Mode

High-availability setup with automatic failover: 3 Redis pods (1 master + 2 replicas) and 3 Sentinel pods.

### Deploy

```yaml
# sentinel.yaml
apiVersion: chuck-chuck-chuck.net/v1alpha1
kind: LittleRed
metadata:
  name: my-cache
spec:
  mode: sentinel
```

```bash
kubectl apply -f sentinel.yaml
```

### Verify

```bash
# Check status
kubectl get littlered my-cache

# Expected output:
# NAME       MODE       PHASE     READY   AGE
# my-cache   sentinel   Running   3       2m

# Check all pods (3 Redis + 3 Sentinel)
kubectl get pods -l app.kubernetes.io/instance=my-cache

# Expected:
# my-cache-redis-0      2/2     Running
# my-cache-redis-1      2/2     Running
# my-cache-redis-2      2/2     Running
# my-cache-sentinel-0   1/1     Running
# my-cache-sentinel-1   1/1     Running
# my-cache-sentinel-2   1/1     Running

# Check services
kubectl get svc -l app.kubernetes.io/instance=my-cache

# Expected:
# my-cache            ClusterIP   ...   6379/TCP,9121/TCP   (master)
# my-cache-replicas   ClusterIP   ...   6379/TCP,9121/TCP   (all replicas)
# my-cache-sentinel   ClusterIP   ...   26379/TCP           (sentinels)
```

### Check replication

```bash
# Query sentinel for master
kubectl exec -it my-cache-sentinel-0 -- valkey-cli -p 26379 SENTINEL get-master-addr-by-name mymaster

# Check master info
kubectl exec -it my-cache-sentinel-0 -- valkey-cli -p 26379 SENTINEL master mymaster

# Check replicas
kubectl exec -it my-cache-sentinel-0 -- valkey-cli -p 26379 SENTINEL replicas mymaster
```

### Test failover

```bash
# Find current master
kubectl get littlered my-cache -o jsonpath='{.status.master.podName}'

# Kill the master pod
kubectl delete pod my-cache-redis-0 --grace-period=0 --force

# Watch failover (new master elected in ~5-30 seconds)
kubectl get littlered my-cache -w

# Verify new master
kubectl exec -it my-cache-sentinel-0 -- valkey-cli -p 26379 SENTINEL get-master-addr-by-name mymaster
```

### Connect from your application

For sentinel-aware clients:

```
Sentinel endpoints: my-cache-sentinel.<namespace>.svc.cluster.local:26379
Master name: mymaster
```

For simple clients (connects to current master):

```
my-cache.<namespace>.svc.cluster.local:6379
```

---

## Cluster Mode

Horizontally scaled setup with automatic sharding across multiple master nodes. Data is distributed using hash slots (16384 total). No PersistentVolumes required - cluster state is stored in the CR status.

### Deploy

```yaml
# cluster.yaml
apiVersion: chuck-chuck-chuck.net/v1alpha1
kind: LittleRed
metadata:
  name: my-cache
spec:
  mode: cluster
  cluster:
    shards: 3           # Number of master nodes (minimum 3)
    replicasPerShard: 1 # Replicas per master (0 = no replicas)
```

```bash
kubectl apply -f cluster.yaml
```

### Verify

```bash
# Check status
kubectl get littlered my-cache

# Expected output:
# NAME       MODE      PHASE     READY   AGE
# my-cache   cluster   Running   6       2m

# Check all pods (3 masters + 3 replicas = 6 pods)
kubectl get pods -l app.kubernetes.io/instance=my-cache

# Expected:
# my-cache-cluster-0   2/2     Running   (shard-0 master)
# my-cache-cluster-1   2/2     Running   (shard-0 replica)
# my-cache-cluster-2   2/2     Running   (shard-1 master)
# my-cache-cluster-3   2/2     Running   (shard-1 replica)
# my-cache-cluster-4   2/2     Running   (shard-2 master)
# my-cache-cluster-5   2/2     Running   (shard-2 replica)

# Check services
kubectl get svc -l app.kubernetes.io/instance=my-cache

# Expected:
# my-cache           ClusterIP   ...   6379/TCP,9121/TCP         (client access)
# my-cache-cluster   ClusterIP   None  6379/TCP,16379/TCP        (headless for cluster)
```

### Check cluster health

```bash
# Cluster info
kubectl exec -it my-cache-cluster-0 -c redis -- valkey-cli CLUSTER INFO

# Expected output includes:
# cluster_state:ok
# cluster_slots_assigned:16384
# cluster_slots_ok:16384
# cluster_known_nodes:6
# cluster_size:3

# Cluster nodes (shows all nodes with their roles and slots)
kubectl exec -it my-cache-cluster-0 -c redis -- valkey-cli CLUSTER NODES

# Check slot distribution
kubectl exec -it my-cache-cluster-0 -c redis -- valkey-cli CLUSTER SLOTS
```

### Check cluster state in CR status

```bash
# View stored cluster topology
kubectl get littlered my-cache -o jsonpath='{.status.cluster}' | jq

# Expected output:
# {
#   "state": "ok",
#   "lastBootstrap": "2026-02-03T...",
#   "nodes": [
#     {"podName": "my-cache-cluster-0", "nodeId": "abc123...", "role": "master", "slotRanges": "0-5460"},
#     {"podName": "my-cache-cluster-1", "nodeId": "def456...", "role": "replica", "masterNodeId": "abc123..."},
#     ...
#   ]
# }
```

### Test recovery

```bash
# Delete a master pod
kubectl delete pod my-cache-cluster-0

# Watch the operator recover (new pod gets new node ID, operator re-adds slots)
kubectl logs -n littlered-system deployment/littlered-operator -f

# Verify cluster health after recovery
kubectl exec -it my-cache-cluster-2 -c redis -- valkey-cli CLUSTER INFO
```

### Connect from your application

Redis Cluster clients automatically discover all nodes:

```
Initial endpoint: my-cache.<namespace>.svc.cluster.local:6379
```

Example with redis-cli:

```bash
# -c flag enables cluster mode (follows redirects)
kubectl exec -it my-cache-cluster-0 -c redis -- valkey-cli -c SET mykey myvalue
kubectl exec -it my-cache-cluster-0 -c redis -- valkey-cli -c GET mykey
```

### Slot distribution

With 3 shards, slots are distributed as:
- Shard 0 (pod-0): slots 0-5460
- Shard 1 (pod-2): slots 5461-10922
- Shard 2 (pod-4): slots 10923-16383

### Important notes

- **In-memory mode**: No persistence. Data will be lost on full cluster restart. By default, 'noeviction' is used, so data is not forgotten when memory is full (Redis will return errors instead).
- **No PVCs**: Cluster state stored in CR status, not nodes.conf.
- **Minimum 3 shards**: Redis Cluster requires at least 3 masters.
- **Scaling not yet supported**: Adding/removing shards requires manual intervention.

---

## Custom Configuration

### With resources and memory policy

```yaml
apiVersion: chuck-chuck-chuck.net/v1alpha1
kind: LittleRed
metadata:
  name: my-cache
spec:
  mode: standalone

  resources:
    requests:
      cpu: "500m"
      memory: "1Gi"
    limits:
      cpu: "500m"
      memory: "1Gi"

  config:
    maxmemory: "900Mi"
    maxmemoryPolicy: noeviction
```

### With authentication

```bash
# Create password secret
kubectl create secret generic redis-password --from-literal=password=mysecretpassword
```

```yaml
apiVersion: chuck-chuck-chuck.net/v1alpha1
kind: LittleRed
metadata:
  name: my-cache
spec:
  mode: standalone
  auth:
    enabled: true
    existingSecret: redis-password
```

```bash
# Connect with password
kubectl exec -it my-cache-redis-0 -c redis -- valkey-cli -a mysecretpassword PING
```

### With ServiceMonitor (Prometheus)

```yaml
apiVersion: chuck-chuck-chuck.net/v1alpha1
kind: LittleRed
metadata:
  name: my-cache
spec:
  mode: standalone
  metrics:
    enabled: true
    serviceMonitor:
      enabled: true
      labels:
        release: prometheus  # Match your Prometheus operator selector
```

### Production sentinel setup

```yaml
apiVersion: chuck-chuck-chuck.net/v1alpha1
kind: LittleRed
metadata:
  name: prod-cache
spec:
  mode: sentinel

  resources:
    requests:
      cpu: "1"
      memory: "2Gi"
    limits:
      cpu: "1"
      memory: "2Gi"

  config:
    maxmemory: "1800Mi"
    maxmemoryPolicy: noeviction

  auth:
    enabled: true
    existingSecret: redis-password

  metrics:
    enabled: true
    serviceMonitor:
      enabled: true
      labels:
        release: prometheus

  sentinel:
    quorum: 2
    downAfterMilliseconds: 5000
    failoverTimeout: 60000

  podTemplate:
    topologySpreadConstraints:
      - maxSkew: 1
        topologyKey: kubernetes.io/hostname
        whenUnsatisfiable: DoNotSchedule
        labelSelector:
          matchLabels:
            app.kubernetes.io/instance: prod-cache
```

### Production cluster setup

```yaml
apiVersion: chuck-chuck-chuck.net/v1alpha1
kind: LittleRed
metadata:
  name: prod-cluster
spec:
  mode: cluster

  cluster:
    shards: 3
    replicasPerShard: 1
    clusterNodeTimeout: 15000

  resources:
    requests:
      cpu: "1"
      memory: "2Gi"
    limits:
      cpu: "1"
      memory: "2Gi"

  config:
    maxmemory: "1800Mi"
    maxmemoryPolicy: noeviction

  auth:
    enabled: true
    existingSecret: redis-password

  metrics:
    enabled: true
    serviceMonitor:
      enabled: true
      labels:
        release: prometheus

  podTemplate:
    topologySpreadConstraints:
      - maxSkew: 1
        topologyKey: kubernetes.io/hostname
        whenUnsatisfiable: DoNotSchedule
        labelSelector:
          matchLabels:
            app.kubernetes.io/instance: prod-cluster
```

---

## Rolling Restarts and Updates

### Overview

When you update a LittleRed resource (e.g., change image version, resource limits, or configuration), the operator triggers a rolling restart of the StatefulSet. The `minReadySeconds` setting controls how long to wait after each pod becomes ready before restarting the next pod.

### Why minReadySeconds Matters

For Redis deployments with high availability (replicas or sentinel), restarting pods too quickly can cause:

- **Cluster mode**: Multiple masters down simultaneously, losing quorum
- **Sentinel mode**: Master restarts before sentinel detects failure and promotes a replica
- **Data unavailability**: Clients can't reach Redis during cascading failures

The `minReadySeconds` setting ensures:
1. Pod is fully ready and healthy
2. Failover (if needed) completes successfully
3. Cluster stabilizes before next pod restarts

### Default Behavior

| Mode | Default minReadySeconds | Reason |
|------|------------------------|--------|
| Cluster with replicas | 30s | Allows automatic failover (cluster-node-timeout + promotion + buffer) |
| Sentinel mode | 35s | Allows sentinel-managed failover (down-after-milliseconds + promotion) |
| Standalone or 0-replica | 0s | No failover mechanism, immediate restart is safe |

### Performing a Rolling Restart

#### Trigger via kubectl

```bash
# Restart all pods in the StatefulSet
kubectl rollout restart statefulset <name>-cluster -n <namespace>

# Example for a cluster named "my-cluster"
kubectl rollout restart statefulset my-cluster-cluster -n default
```

#### Trigger via CR Update

Change any field that affects the pod template (image, resources, config):

```bash
kubectl patch littlered my-cluster -n default --type merge -p '
spec:
  resources:
    requests:
      memory: "256Mi"
'
```

#### Monitor Rollout Status

```bash
# Watch rollout progress
kubectl rollout status statefulset my-cluster-cluster -n default

# Check pod restarts
kubectl get pods -n default -l app.kubernetes.io/instance=my-cluster -w
```

### Customizing minReadySeconds

If you need faster or slower rolling restarts, you can override the default:

```yaml
apiVersion: chuck-chuck-chuck.net/v1alpha1
kind: LittleRed
metadata:
  name: my-cluster
spec:
  mode: cluster
  cluster:
    shards: 3
    replicasPerShard: 1
  updateStrategy:
    type: RollingUpdate
    minReadySeconds: 45  # Wait 45s between pod restarts
```

**Use cases for customization:**

- **Faster restarts** (e.g., `minReadySeconds: 15`): For clusters with very short cluster-node-timeout or when you've verified failover completes quickly
- **Slower restarts** (e.g., `minReadySeconds: 60`): For large clusters with many keys where replication catch-up takes time
- **Testing** (`minReadySeconds: 0`): For development environments where you want rapid rollouts

**Valid range:** 0-300 seconds

### Example: Safe Production Rolling Restart

```yaml
apiVersion: chuck-chuck-chuck.net/v1alpha1
kind: LittleRed
metadata:
  name: prod-cluster
spec:
  mode: cluster
  cluster:
    shards: 5
    replicasPerShard: 2
    clusterNodeTimeout: 15000  # 15s default
  updateStrategy:
    type: RollingUpdate
    minReadySeconds: 40  # Conservative: 15s + 10s promotion + 15s buffer
  resources:
    requests:
      cpu: "500m"
      memory: "512Mi"
    limits:
      cpu: "1000m"
      memory: "1Gi"
```

**Rollout timeline for this config:**
- Pod 0 terminates, new pod 0 starts
- New pod 0 becomes ready
- **Wait 40 seconds** (minReadySeconds)
- Repeat for pod 1, 2, 3, etc.
- Total time: ~(40s × 10 pods) = 6-7 minutes for full rollout

### Testing Rolling Restarts

You can test rolling restart behavior with the e2e test suite:

```bash
DEBUG_ON_FAILURE=true make test-e2e-cluster-chaos
```

The `should maintain data integrity during rolling restart` test verifies:
- 0 data corruptions
- ≥95% read availability during rollout
- All slots remain assigned (cluster_slots_assigned:16384)

---

## Large-Scale Tuning

For installations with hundreds or thousands of instances, you can tune the reconciliation frequency to reduce pressure on the Kubernetes API server and the Redis nodes.

| Field | Default | Description |
|-------|---------|-------------|
| `requeueIntervals.fast` | `2s` | Interval used during initialization, recovery, or when the system is not 'Running'. |
| `requeueIntervals.steadyState` | `30s` | Interval used for periodic health checks once the system is stable ('Running'). |

**Example:**

```yaml
apiVersion: chuck-chuck-chuck.net/v1alpha1
kind: LittleRed
metadata:
  name: tuned-cache
spec:
  mode: sentinel
  requeueIntervals:
    fast: "10s"
    steadyState: "1m"
```

---

## Cleanup

```bash
# Delete the LittleRed resource (cleans up all managed resources)
kubectl delete littlered my-cache

# Verify cleanup
kubectl get pods -l app.kubernetes.io/instance=my-cache
# No resources found
```

---

## Troubleshooting

### Check operator logs

```bash
kubectl logs -n littlered-system deployment/littlered-operator
```

### Check LittleRed status and conditions

```bash
kubectl describe littlered my-cache
```

### Check pod events

```bash
kubectl describe pod my-cache-redis-0
```

### Redis logs

```bash
kubectl logs my-cache-redis-0 -c redis
```

### Sentinel logs

```bash
kubectl logs my-cache-sentinel-0
```

### Cluster diagnostics

```bash
# Check cluster state
kubectl exec -it my-cache-cluster-0 -c redis -- valkey-cli CLUSTER INFO

# Check for failed slots
kubectl exec -it my-cache-cluster-0 -c redis -- valkey-cli CLUSTER SLOTS

# View cluster topology
kubectl exec -it my-cache-cluster-0 -c redis -- valkey-cli CLUSTER NODES

# Check stored cluster state in CR
kubectl get littlered my-cache -o jsonpath='{.status.cluster.state}'

# Force cluster re-bootstrap (if cluster is broken)
# 1. Delete all cluster pods
kubectl delete pods -l app.kubernetes.io/instance=my-cache,app.kubernetes.io/component=cluster
# 2. Clear stored state by patching CR
kubectl patch littlered my-cache --type=merge -p '{"status":{"cluster":null}}'
# 3. Operator will re-bootstrap on next reconcile
```