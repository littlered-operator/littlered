# Manual Chaos Testing Guide

This guide explains how to use the `sentinel-chaos` Helm chart to manually investigate failover behavior while a continuous load is being applied by the chaos client.

## 1. Deploy the Test Environment

### Option A: Sentinel Mode (3 Nodes + 3 Sentinels)
Install the Sentinel cluster and the chaos client pod:

```bash
# Install or Upgrade
helm upgrade --install sentinel-investigation ./charts/sentinel-chaos \
  --set chaosClient.image.tag=$(git rev-parse --short HEAD) \
  --namespace default

# Uninstall
helm delete sentinel-investigation
```

### Option B: Cluster Mode (3 Shards x 2 Replicas = 6 Nodes)
Install the Redis Cluster and the chaos client pod:

```bash
# Install or Upgrade
helm upgrade --install cluster-investigation ./charts/cluster-chaos \
  --set chaosClient.image.tag=$(git rev-parse --short HEAD) \
  --namespace default

# Uninstall
helm delete cluster-investigation
```

## 2. Monitor the System

It is highly recommended to open multiple parallel terminal windows or use a terminal multiplexer (like `tmux`) to watch the various components of the system:

### Window 1: Operator Logs
Observe how the operator reacts to events and performs reconciliation:
```bash
kubectl logs -n littlered-system -l control-plane=controller-manager --tail=-1 -f
```

### Window 2: Chaos Client Output
Monitor throughput, availability, and data integrity in real-time:
```bash
kubectl logs manual-chaos-chaos-client -f
```

### Window 3: Redis Pod Logs
Watch the individual Redis nodes (open one per pod or use a loop):
```bash
# Example for sentinel mode
while true; do kubectl logs manual-chaos-redis-0 -f; sleep 1; done

# Example for cluster mode
while true; do kubectl logs manual-chaos-cluster-0 -f; sleep 1; done
```

### Window 4: Sentinel Pod Logs (Sentinel Mode only)
Watch the Sentinels performing elections:
```bash
while true; do kubectl logs manual-chaos-sentinel-0 -f; sleep 1; done
```

### Window 5: High-Level Status
Monitor the CR status and the current Master identity:
```bash
# Sentinel mode
while true; do kubectl get littlereds.chuck-chuck-chuck.net manual-chaos -o wide; sleep 1; done

# Cluster mode
while true; do kubectl get littlereds.chuck-chuck-chuck.net manual-chaos-cluster -o wide; sleep 1; done
```

## 3. Perform Manual Chaos

While watching the logs, randomly kill one or more pods to stress the system. 

### Scenarios to try:
- **Kill the Master (Sentinel)**: Find the current master in Window 5 and kill it.
- **Kill a Shard Master (Cluster)**: Use `kubectl exec` to find a master node and kill its pod.
- **Kill a Master and a Replica**: Kill two Redis pods in the same shard/set simultaneously.
- **Kill Sentinel Peers (Sentinel)**: Kill one or two Sentinel pods.

**Survival Criteria**:
- **Sentinel**: As long as at least one Redis replica survives and a Sentinel quorum can be reached, the cluster must eventually stabilize.
- **Cluster**: As long as a majority of masters are up and at least one node for every slot range is available (eventually), the cluster must recover.
- The Kubernetes Service must eventually point to healthy nodes.
- The `chaos-client` should show recovery of throughput and **0 data corruptions**.

## 4. Investigate

You can observe:
- How quickly the chaos client recovers connectivity.
- Whether any data corruption is detected.
- The Operator logs to verify event-driven vs polling reconciliation.
- The CR status using `kubectl get littlered -o wide`.