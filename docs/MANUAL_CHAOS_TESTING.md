# Manual Chaos Testing Guide

This guide explains how to use the `sentinel-chaos` Helm chart to manually investigate failover behavior while a continuous load is being applied by the chaos client.

## 1. Deploy the Test Environment

Deploy the Sentinel cluster and the chaos client pod:

```bash
# Get the current tag for the chaos client
TAG=$(git rev-parse --short HEAD)

# Install the chart
helm upgrade --install sentinel-investigation ./charts/sentinel-chaos 
  --set chaosClient.image.tag=$TAG 
  --namespace default
```

## 2. Monitor the Chaos Client

Follow the logs of the chaos client to see real-time availability and data corruption metrics:

```bash
kubectl logs -f manual-chaos-chaos-client
```

The client will run indefinitely (`duration: 0`) until you delete the pod or the helm release.

## 3. Perform Manual Chaos

While watching the logs, kill the Redis master pod to trigger a failover:

```bash
# Find the current master
MASTER=$(kubectl get littlered manual-chaos -o jsonpath='{.status.master.podName}')
echo "Current Master: $MASTER"

# Kill it forcefully
kubectl delete pod $MASTER --grace-period=0 --force
```

## 4. Investigate

You can observe:
- How quickly the chaos client recovers connectivity.
- Whether any data corruption is detected.
- The Operator logs to verify event-driven vs polling reconciliation.
- The CR status using `kubectl get littlered -o wide`.

## 5. Cleanup

```bash
helm uninstall sentinel-investigation --namespace default
```
