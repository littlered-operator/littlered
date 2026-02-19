# ADR 001: Strict IP-Only Identity for Sentinel Mode

## Status
Accepted

## Context
In a Kubernetes environment, Redis clusters traditionally use stable network identities (hostnames like `redis-0.svc...`) provided by StatefulSets. This ensures that a pod maintains its identity across restarts.

However, LittleRed is designed as a **pure in-memory** store. This introduces a critical safety concern:
1.  A pod restart results in total data loss for that node.
2.  In Kubernetes, a StatefulSet Pod keeps its **name** but receives a **new IP** on restart.
3.  If we use hostnames for identification, Sentinel sees the restarted (empty) `redis-0` as the "same" node that was master 10 seconds ago.
4.  This can lead to a "Ghost Master" scenario where an empty node is reclaimed as master and accepts writes, while live replicas (with data) are forced to wipe themselves to sync from the empty master.

## Decision
We will enforce **strict IP-based identity** for all Sentinel and Redis nodes in the current in-memory architecture.

### Implementation Details:
- Explicitly disable `sentinel announce-hostnames` and `sentinel resolve-hostnames`.
- All `SENTINEL MONITOR` commands issued by the Operator use **Pod IPs**.
- Redis startup scripts strictly compare their own `POD_IP` against the Sentinel-reported master identity.
- The Operator status (`status.master.ip`) strictly reflects the IP reported by Sentinel.

## Consequences

### Positive:
- **Automatic Disenfranchisement**: A restarted pod (new IP) is treated as a complete stranger by Sentinel. It cannot accidentally "resume" its previous role.
- **Data Safety**: Prevents empty nodes from being reclaimed as masters if a failover was already in progress or completed.
- **DNS Resilience**: Failover and master identification no longer depend on CoreDNS resolution speed or cache TTLs.

### Negative:
- **Debugging**: Human operators must map IPs to Pod names (though the Operator Status field automates this mapping).
- **Log Churn**: Sentinel logs will show "stranger" nodes more frequently during pod churn.

---

## Amendment: In-Pod Process Crash — Known Limitation (Accepted)

### The residual vulnerability

The IP-identity design closes the "pod restarts → new IP → clean rejoin" path. It does **not** protect against an in-pod process crash (OOM kill by the kernel, Redis bug, hardware fault) where the **container restarts but the pod is not deleted**.

In that scenario:
1. The Redis master process is killed (`SIGKILL`).
2. The container restarts in ~1–2 s (first restart carries no backoff).
3. The pod IP is unchanged — the master re-appears at the same address.
4. Because LittleRed runs with `save ""` and `appendonly no`, the restarted process has **no data**.
5. Sentinel's `down-after-milliseconds` (default 30 s) has not elapsed — no failover is triggered.
6. The empty master resumes writes; replicas detect the new `run-id` and perform a full resync, erasing their own copies of the data.
7. **All data for that shard is lost.**

This is the one scenario where "same IP" and "empty node" coincide — the exact combination the rest of the architecture is designed to prevent.

### Why this risk is accepted

LittleRed is an **in-memory cache**, not a durable store. The consuming application:

- Requires maximum throughput and minimum latency, which rules out AOF/RDB persistence.
- Treats a cache miss or service interruption as acceptable (data is reconstructible from the source of truth).
- Does **not** treat a cache wipe as *persistent* data loss — the data never existed solely in Redis.

Therefore, a master process crash causing a full shard wipe is classified as a **service interruption** of acceptable severity, not an unacceptable data-loss event.

### Mitigations (operational, not software)

The appropriate response is to make the failure rare through correct sizing, not to add software complexity that would compromise the cache's performance profile:

- **Set `requests` == `limits` for memory** on all Redis containers. This prevents the kernel from overcommitting memory and makes OOM kills impossible as long as the actual working set stays below the limit.
- **Size `maxmemory` conservatively** (e.g. `maxmemory` ≤ 80 % of the container memory limit) to leave headroom for replication buffers and Lua/cluster overhead.
- **Use a sane eviction policy** (`allkeys-lru` or `volatile-lru`) so Redis evicts keys gracefully instead of hitting the OS limit.

Redis bugs and hardware faults are accepted as force-majeure events without further mitigation.

### Why hard node failures are safe

A full Kubernetes node failure follows a different, safe path:

1. Node becomes `NotReady`; K8s applies `node.kubernetes.io/not-ready:NoExecute` taint after `node-monitor-grace-period` (~40 s).
2. Pods are *deleted* after `tolerationSeconds` (default 300 s, often tuned to 60–120 s).
3. Sentinel's `down-after-milliseconds` (30 s) elapses well before the pod is rescheduled elsewhere → **failover completes first**.
4. The replacement pod lands on a different node and gets a **new IP** → treated as a stranger → joins cleanly as a replica.

The eviction timeline (minutes) is always longer than the Sentinel failover time (~30–60 s), so the "same IP, empty node" scenario cannot occur on a hard node failure.

### What is NOT protected in Sentinel mode (explicitly out of scope)

- In-pod Redis process crash (`SIGKILL`, OOM within container limits, Redis abort) on a **Sentinel master** where the container restarts in < `down-after-milliseconds`.
- No software fix is possible without persistence: Sentinel identity is IP-based, and there is no file equivalent to `nodes.conf` that could be cleared to force a new identity.
- Correct resource configuration (requests == limits, conservative `maxmemory`) is the expected mitigation.

### Cluster mode has an active fix

Unlike Sentinel, **Cluster mode identity is node-ID-based** (stored in `nodes.conf`). The cluster startup script unconditionally deletes `nodes.conf` before starting Redis, guaranteeing a fresh node ID on every process start — including in-pod container restarts:

```sh
rm -f /data/nodes.conf
exec redis-server /etc/redis/redis.conf ...
```

Without this, a restarted Redis process would read the surviving `nodes.conf` from the emptyDir (which is pod-scoped, not container-scoped), restore its old node ID and slot assignments, and re-announce itself as the same cluster member with no data. The cluster would not failover (restart speed < `cluster-node-timeout`), and replicas would FULLRESYNC from the empty master — identical to the Sentinel data loss scenario.

With the deletion, the restarted process is a stranger with a fresh ID. The operator's existing reconciliation path — the same one exercised by normal pod deletion — handles it: `cluster-node-timeout` elapses, the replica promotes, and the stranger is assigned as the new replica for that shard. The trade-off is explicit: up to `cluster-node-timeout` (~15 s) of slot unavailability in exchange for guaranteed data safety.

On normal pod deletion, the emptyDir is already gone, so the `rm -f` is a harmless no-op.

### Impact on test strategy

A "super-hard" restart mode (`kubectl exec -- kill -9 1`, container restarts in place) would demonstrate:
- **Sentinel**: data loss — a known, accepted limitation, not tested.
- **Cluster**: up to ~15 s of availability loss for the affected shard, then full recovery — this *could* be tested, but the timing sensitivity (container restart speed vs. `cluster-node-timeout`) makes it environment-dependent. Not currently included in the e2e suite.

---

## Future Evolution: Persistence Support
If LittleRed eventually supports PersistentVolumes (PVCs), this decision **must be revisited**.

- **Identity Coupling**: With PVCs, a pod restart does *not* result in data loss. The node's identity is tied to its data on disk.
- **Requirement**: In persistent mode, we **must** use stable hostnames (`Podname`) because the "new" pod is indeed the same logical entity as the "old" pod.
- **Logic Pivot**: The Operator will need an internal `IdentityMode` switch (derived from the `PersistenceEnabled` state) to toggle between IP-based and Hostname-based logic.

## References
- `LLM_STARTUP.md` (Architectural Pillars)
- `docs/ARCHITECTURE.md` (Safe Bootstrap)
