# Manual Chaos Testing Guide

This guide explains how to use the `sentinel-chaos` Helm chart to manually investigate failover behavior while a continuous load is being applied by the chaos client.

## 1. Deploy the Test Environment

Install the Sentinel cluster and the chaos client pod:

```bash
# Install or Upgrade
helm upgrade --install sentinel-investigation ./charts/sentinel-chaos \
  --set chaosClient.image.tag=$(git rev-parse --short HEAD) \
  --namespace default

# Uninstall
helm delete sentinel-investigation
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
# Example for redis-0
while true; do kubectl logs manual-chaos-redis-0 -f; sleep 1; done
```
*(Repeat for `manual-chaos-redis-1` and `manual-chaos-redis-2`)*

### Window 4: Sentinel Pod Logs
Watch the Sentinels performing elections:
```bash
# Example for sentinel-0
while true; do kubectl logs manual-chaos-sentinel-0 -f; sleep 1; done
```
*(Repeat for `manual-chaos-sentinel-1` and `manual-chaos-sentinel-2`)*

### Window 5: High-Level Status
Monitor the CR status and the current Master identity:
```bash
while true; do kubectl get littlereds.littlered.tanne3.de manual-chaos -o wide; sleep 1; done
```

## 3. Perform Manual Chaos

While watching the logs, randomly kill one or more pods to stress the system. 

### Scenarios to try:
- **Kill the Master**: Find the current master in Window 5 and kill it.
- **Kill a Master and a Replica**: Kill two Redis pods simultaneously.
- **Kill Sentinel Peers**: Kill one or two Sentinel pods.
- **Combined Failure**: Kill the Master Redis and one Sentinel pod.

**Survival Criteria**:
- As long as **at least one Redis replica** survives and a **Sentinel quorum** can be reached (or recovers), the cluster must eventually stabilize.
- The Kubernetes Service must eventually point to a healthy Master.
- The `chaos-client` should show recovery of throughput and **0 data corruptions**.

## 4. Investigate

You can observe:
- How quickly the chaos client recovers connectivity.
- Whether any data corruption is detected.
- The Operator logs to verify event-driven vs polling reconciliation.
- The CR status using `kubectl get littlered -o wide`.