# LittleRed

A Kubernetes operator for deploying Valkey/Redis as a non-persistent, pure in-memory store.

LittleRed is designed for high-throughput caching and transient data workloads where persistence is explicitly disabled. It addresses the specific lifecycle requirements of non-persistent clusters by using a full reconciliation engine to manage node identities and cluster membership—scenarios where static startup scripts and Helm charts often reach their limits.

## Technical Rationale: The In-Memory Challenge

Operating Redis or Valkey without persistence requires a specialized approach to lifecycle management. Standard deployments often assume data is preserved on disk, which influences how nodes manage their identity and how clusters handle rejoining members.

> **Current Support:** To ensure maximum stability for the initial release, Cluster mode is specifically validated for **3 shards** with **0 or 1 replica** each. Support for variable cluster sizes is planned for future versions.

### The Risk of State Corruption
In a non-persistent setup, a node restart results in an **empty dataset**. If a node returns with its previous identity (IP or Hostname), the cluster may incorrectly identify it as a "returning master." This can lead the cluster to accept the empty node as the authoritative source of truth, triggering a cascading data wipe across healthy replicas.

### The LittleRed Solution
LittleRed implements a reconciliation-based approach to ensure cluster stability:
- **Ephemeral Identity Management**: The operator ensures that every pod restart—whether a graceful deletion, a `SIGKILL`, or an OOM event—is treated as a new entity. 
- **Automated Topology Correction**: LittleRed identifies and removes stale identities ("Ghost Nodes") from the cluster configuration before new nodes are permitted to join.
- **Healing via External Visibility**: Unlike internal cluster mechanisms (Sentinel or Gossip), the operator has global visibility into the Kubernetes API. It can identify when a cluster is in a deadlock or an unrecoverable state and intervene precisely to restore consistency.
- **Chaos-Hardened**: The implementation is validated against an extensive E2E suite simulating diverse failure modes to verify that the cluster remains stable and data is not lost due to identity reuse.

## Operational Philosophy

LittleRed follows a **low-interference** principle:
1.  **Trust the Native Protocols**: We allow Sentinel and Cluster Gossip to manage the data plane and failovers.
2.  **Strategic Intervention**: The operator only intervenes when the cluster lacks the context to heal itself (e.g., when all nodes agree on a master that no longer exists in Kubernetes).
3.  **Heal-ability**: As long as the cluster state is recoverable and data hasn't been lost, the reconciliation loop is designed to return the cluster to a healthy, consistent state.

## Key Features

- **Deployment Modes**: Support for `standalone`, `sentinel` (1 master + 2 replicas), and `cluster` (sharded) modes.
- **Valkey & Redis Native**: Uses **Valkey 8.0** by default (compatible with Redis 7.2+).
- **Observability**: Built-in `redis_exporter` sidecars and optional `ServiceMonitor` for Prometheus.
- **Security**: Support for password authentication and TLS encryption.
- **Integrated Diagnostics**: Includes `lrctl`, a CLI tool for direct state verification.

## Quick Start

### 1. Install the Operator

```bash
helm install littlered charts/littlered-operator -n littlered-system --create-namespace
```

### 2. Deploy a Cluster (Sentinel Mode)

```yaml
# my-cache.yaml
apiVersion: chuck-chuck-chuck.net/v1alpha1
kind: LittleRed
metadata:
  name: my-cache
spec:
  mode: sentinel
```

```bash
kubectl apply -f my-cache.yaml
```

### 3. Verify Health

```bash
# Install the CLI as a kubectl plugin
make install-plugin

# Check cluster consistency
kubectl lr verify my-cache
```

## Configuration Reference

### Spec Reference

```yaml
apiVersion: chuck-chuck-chuck.net/v1alpha1
kind: LittleRed
metadata:
  name: my-cache
spec:
  mode: standalone          # standalone, sentinel, or cluster
  
  image:
    registry: docker.io
    path: valkey/valkey
    tag: "8.0"
    pullPolicy: IfNotPresent

  resources:                # Guaranteed QoS recommended (requests == limits)
    requests: { cpu: 250m, memory: 256Mi }
    limits:   { cpu: 250m, memory: 256Mi }

  config:
    maxmemoryPolicy: noeviction
    tcpKeepalive: 300
    timeout: 0

  # Mode-specific settings
  sentinel:                 # For mode: sentinel
    quorum: 2
    downAfterMilliseconds: 5000
    failoverTimeout: 60000

  cluster:                  # For mode: cluster
    shards: 3
    replicasPerShard: 1
```

## Documentation
- [Usage Guide](docs/USAGE.md) - Deployment examples.
- [Architecture](docs/ARCHITECTURE.md) - Reconciliation logic.
- [E2E Testing](docs/E2E_TESTING.md) - Test suite details.
- [CLI Reference](docs/LRCTL.md) - `lrctl` guide.

## Behind the Name
The name **LittleRed** is an homage to the fable of the *Little Red Hen*. When we searched for a tool that handled the complexities of pure in-memory Redis lifecycles with technical rigor, we found that the existing solutions were often unmaintained or focused on different problems. Like the hen in the story, we decided to build it ourselves.

## License
Apache License 2.0
