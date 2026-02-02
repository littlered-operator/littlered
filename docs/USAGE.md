# Using LittleRed

This guide shows how to deploy and use Redis caches with the LittleRed operator.

## Prerequisites

- LittleRed operator running in your cluster
- `kubectl` configured to access your cluster

## Standalone Mode

A single Redis instance for development or simple caching.

### Deploy

```yaml
# standalone.yaml
apiVersion: littlered.tanne3.de/v1alpha1
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
apiVersion: littlered.tanne3.de/v1alpha1
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

## Custom Configuration

### With resources and memory policy

```yaml
apiVersion: littlered.tanne3.de/v1alpha1
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
    maxmemoryPolicy: allkeys-lru
```

### With authentication

```bash
# Create password secret
kubectl create secret generic redis-password --from-literal=password=mysecretpassword
```

```yaml
apiVersion: littlered.tanne3.de/v1alpha1
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
apiVersion: littlered.tanne3.de/v1alpha1
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
apiVersion: littlered.tanne3.de/v1alpha1
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
    maxmemoryPolicy: allkeys-lru

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
