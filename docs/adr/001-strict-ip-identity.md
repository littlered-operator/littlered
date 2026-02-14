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

## Future Evolution: Persistence Support
If LittleRed eventually supports PersistentVolumes (PVCs), this decision **must be revisited**.

- **Identity Coupling**: With PVCs, a pod restart does *not* result in data loss. The node's identity is tied to its data on disk.
- **Requirement**: In persistent mode, we **must** use stable hostnames (`Podname`) because the "new" pod is indeed the same logical entity as the "old" pod.
- **Logic Pivot**: The Operator will need an internal `IdentityMode` switch (derived from the `PersistenceEnabled` state) to toggle between IP-based and Hostname-based logic.

## References
- `LLM_STARTUP.md` (Architectural Pillars)
- `docs/ARCHITECTURE.md` (Safe Bootstrap)
