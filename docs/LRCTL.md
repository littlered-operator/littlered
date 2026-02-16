# lrctl - LittleRed Command Line Interface

`lrctl` is a specialized diagnostic and management tool for LittleRed Redis clusters. It provides syntactic sugar over `kubectl` and performs deep-dive consistency checks by communicating directly with Redis and Sentinel processes inside your pods.

## Installation

### Building from source
From the root of the repository, run:
```bash
make lrctl
```
The binary will be created in `bin/lrctl`. You can move it to your path:
```bash
sudo cp bin/lrctl /usr/local/bin/
```

### Use as a kubectl plugin
If `lrctl` is in your PATH, you can invoke it as a kubectl plugin:
```bash
kubectl lr status <name>
```

## Global Flags
`lrctl` supports standard Kubernetes flags:
- `-n, --namespace`: Specify the namespace (defaults to your current context).
- `--kubeconfig`: Path to a specific kubeconfig file.

---

## Commands

### 1. status
Provides a high-level summary of the cluster health and configuration.

**Usage:**
```bash
lrctl status my-cache
```

**What it shows:**
- Cluster phase and deployment mode.
- Current Master Pod and IP.
- Readiness counts for Redis and Sentinel nodes.

---

### 2. inspect
Performs a "Deep Inspect" by executing diagnostic commands inside every pod associated with the cluster.

**Usage:**
```bash
lrctl inspect my-cache
```

**What it does:**
- Sentinel Mode: Runs `SENTINEL master` on every sentinel pod and `INFO replication` on every redis pod.
- Cluster Mode: Runs `CLUSTER NODES` and `CLUSTER INFO` on every node.
- Aggregates all output into a single report for easy analysis.

---

### 3. verify
Cross-references the Kubernetes state with the live Redis internal state to detect inconsistencies.

**Usage:**
```bash
lrctl verify my-cache
```

**What it checks:**
- **Consensus**: Do all Sentinels agree on who the master is?
- **Ghost Detection**: Is the master reported by Redis/Sentinel actually a living Kubernetes pod?
- **Role Alignment**: Do the `chuck-chuck-chuck.net/role` labels match the actual role reported by the Redis process?
- **Topology (Cluster Mode)**: Prints a visual tree of Master -> Replica relationships and slot assignments.

**Example Topology Output:**
```text
Cluster Topology:
  Master: my-cache-cluster-0 (Slots: 0-5461)
    └── Replica: my-cache-cluster-3
  Master: my-cache-cluster-2 (Slots: 10923-16383)
    └── Replica: my-cache-cluster-4
```

---

## Use Cases

### Debugging a stalled failover
Use `lrctl verify`. It will tell you if Sentinels are stuck on a "Ghost Master" (a pod IP that no longer exists) or if they disagree on the current state.

### Verifying Cluster Balance
Use `lrctl verify` in Cluster mode to see exactly how slots are distributed and ensure every master has its intended replicas.

### Rapid Health Check
Use `lrctl status` for a quick glance at the "Source of Truth" according to the Operator.
