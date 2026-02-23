# lrctl - LittleRed Command Line Interface

`lrctl` is a powerful diagnostic and management tool for Redis clusters. While it is designed specifically for clusters managed by the LittleRed operator, it also provides robust support for "unmanaged" (non-LittleRed) Redis deployments using a discovery engine.

`lrctl` performs deep-dive consistency checks by communicating directly with Redis and Sentinel processes inside your pods, cross-referencing this live data with the Kubernetes API.

## Installation

### Building from source
From the root of the repository, run:
```bash
make lrctl
```
The binary will be created in `bin/lrctl`. To install it globally:
```bash
make install
```

### Use as a kubectl plugin
If `lrctl` is named `kubectl-lr` and available in your `PATH`, you can use it as a standard plugin.
```bash
# Set up the symlinks automatically
make install-plugin

# Now use it via kubectl
kubectl lr status <name>
```
*Note: This also enables shell completion for resource names and namespaces when used via `kubectl`.*

## Global Flags
`lrctl` supports standard Kubernetes flags and some specialized diagnostic toggles:
- `-n, --namespace`: Specify the namespace (defaults to your current context).
- `-A, --all-namespaces`: List or verify resources across all namespaces.
- `--kubeconfig`: Path to a specific kubeconfig file.
- `--json`: Output results in pure JSON for automation/scripting.
- `--unmanaged`: Treat the target as a raw set of pods rather than a LittleRed CR.
- `--kind [sentinel|cluster]`: Used with `--unmanaged` to hint at the cluster type.

---

## Commands

### 1. status
Provides a high-level summary of the health and configuration.

**Usage:**
```bash
lrctl status [name] [-n namespace] [-A]
```
If `name` is omitted, it lists all LittleRed resources in the namespace.

**Example Output (Sentinel Mode):**
```text
Cluster: my-cache-sentinel
Namespace: default
Phase: Running
Mode: sentinel
Master: my-cache-sentinel-redis-0 (IP: 10.233.66.107)
Sentinels: 3/3 Ready
Redis Nodes: 3/3 Ready
```

**Example Output (Cluster Mode):**
```text
Cluster: my-cache-cluster
Namespace: default
Phase: Running
Mode: cluster
Master: <none>
Redis Nodes: 6/6 Ready
```

---

### 2. inspect
Performs a "Deep Inspect" by executing diagnostic commands inside every pod.

**Usage:**
```bash
lrctl inspect <name>
```

**What it does:**
- **Sentinel Mode**: Runs `SENTINEL master` on every sentinel pod and `INFO replication` on every redis pod.
- **Cluster Mode**: Runs `CLUSTER NODES` and `CLUSTER INFO` on every node.
- **Result**: Aggregates all raw output into a single report, allowing you to see exactly what every individual process believes the state to be.

---

### 3. verify
The primary troubleshooting tool. It detects inconsistencies between Kubernetes state and live Redis state.

**Usage:**
```bash
lrctl verify <name> [--json]
```

**Example Output (Sentinel Mode):**
```text
Verifying Cluster: default/my-cache-sentinel (Mode: sentinel)
Gathering Cluster Ground Truth...

Sentinel Status:
  - Sentinel my-cache-sentinel-sentinel-1: monitoring 10.233.66.107
  - Sentinel my-cache-sentinel-sentinel-2: monitoring 10.233.66.107
  - Sentinel my-cache-sentinel-sentinel-0: monitoring 10.233.66.107

Redis Status:
  - Redis my-cache-sentinel-redis-0: role:master
  - Redis my-cache-sentinel-redis-1: role:slave, following:10.233.66.107, link:up
  - Redis my-cache-sentinel-redis-2: role:slave, following:10.233.66.107, link:up

Ground Truth Summary:
  [OK] Authority Master: my-cache-sentinel-redis-0 (10.233.66.107)

[OK] Cluster configuration is consistent.
```

**Example Output (Cluster Mode):**
```text
Verifying Cluster: default/my-cache-cluster (Mode: cluster)
Gathering Cluster Ground Truth...

Cluster State: ok
Total Slots Assigned: 16384 / 16384

Node Status:
  - Pod my-cache-cluster-cluster-0: role:master, id:db7c8c37cc2badde942fc5cb37b8f11c05d6996f, slots:0-5461
  - Pod my-cache-cluster-cluster-1: role:master, id:f8c8f5c33c309771dfca901442d7ace22f006dcd, slots:5462-10922
  - Pod my-cache-cluster-cluster-2: role:master, id:f450773574d834f4ab549a76f5804fc66bcdf1cb, slots:10923-16383
  - Pod my-cache-cluster-cluster-3: role:replica, id:4ef162bdbdfba00dfeebaa6c078ed358e0c1a555, following:f8c8f5c33c309771dfca901442d7ace22f006dcd, link:up
  - Pod my-cache-cluster-cluster-4: role:replica, id:34139e215f431b9582b17fb4fd4d8cf210ba73ce, following:f450773574d834f4ab549a76f5804fc66bcdf1cb, link:up
  - Pod my-cache-cluster-cluster-5: role:replica, id:b7645a823e77866a607b9557507eed42ba6bac77, following:db7c8c37cc2badde942fc5cb37b8f11c05d6996f, link:up

Cluster Topology:
  Master: my-cache-cluster-cluster-0 (db7c8c37cc2badde942fc5cb37b8f11c05d6996f)
    Slots: 0-5461
    └── Replica: my-cache-cluster-cluster-5 (b7645a823e77866a607b9557507eed42ba6bac77, link:up)
  Master: my-cache-cluster-cluster-1 (f8c8f5c33c309771dfca901442d7ace22f006dcd)
    Slots: 5462-10922
    └── Replica: my-cache-cluster-cluster-3 (4ef162bdbdfba00dfeebaa6c078ed358e0c1a555, link:up)
  Master: my-cache-cluster-cluster-2 (f450773574d834f4ab549a76f5804fc66bcdf1cb)
    Slots: 10923-16383
    └── Replica: my-cache-cluster-cluster-4 (34139e215f431b9582b17fb4fd4d8cf210ba73ce, link:up)

Summary:
  [OK] Cluster is healthy and consistent.
```

**Advanced Checks:**
- **Consensus**: Do all Sentinels agree on the master?
- **Ghost Detection**: Is the master reported by Redis/Sentinel actually a living Kubernetes pod?
- **Role Alignment**: Do the `chuck-chuck-chuck.net/role` labels match the actual process role?
- **Topology (Cluster Mode)**: Visualizes the tree of Master -> Replica relationships and slot coverage.
- **Partition Detection**: Identifies if nodes see different versions of the cluster topology.

---

## Advanced Usage

### Working with Unmanaged Clusters
If you have a Redis cluster that wasn't created by LittleRed (e.g., via a manual StatefulSet), you can still use `lrctl`:

```bash
# Inspect a manually created Sentinel cluster named 'my-custom-redis'
lrctl inspect my-custom-redis --unmanaged --kind sentinel
```
The tool will use heuristics to find the pods and perform the same deep-dive diagnostics.

### Automated Auditing (JSON)
All major commands support the `--json` flag. This is ideal for CI/CD pipelines or custom monitoring:

```bash
# Check if a cluster is healthy via script
if [ "$(lrctl verify my-cache --json | jq '.[0].healthy')" = "true" ]; then
  echo "Cluster is OK"
fi
```

## Troubleshooting Failovers
If a failover seems stuck, run `lrctl verify`. It specifically looks for common failure modes:
1.  **Ghost Masters**: Sentinels pointing to an IP of a pod that was deleted.
2.  **Split Brain**: Different nodes disagreeing on the master.
3.  **Role Mismatch**: A pod labeled as `master` that is actually running as a `slave`.
