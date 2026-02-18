# LittleRed - LLM Startup Guide

Welcome! This document provides a high-level, condensed overview of the LittleRed project to help you get up to speed quickly and contribute effectively.

---

## 1. Project Essence
**LittleRed** is a lightweight Kubernetes operator for deploying and managing **Redis/Valkey** as a high-performance, pure in-memory store. It is built using the **Kubebuilder** framework.

### Core Philosophy
- **Pure In-Memory**: Designed for speed and simplicity. No persistence (RDB/AOF) is ever enabled by default, not even "by accident" for internal metadata.
- **No Eviction by Default**: Follows a strict `noeviction` policy unless the user explicitly configures otherwise. It acts as a reliable in-memory data store that doesn't "forget" data under memory pressure.
- **Cloud Native**: Leverages Kubernetes primitives (StatefulSets, Services) while handling Redis-specific cluster logic in the operator.

---

## 2. Architectural Pillars

### 2.1 Strictly No Persistence
- **Decision**: Persistence (RDB/AOF) is **actively disabled** across all components.
- **Rationale**: Eliminates dependencies on PersistentVolumes (PVCs), simplifies disaster recovery, and ensures predictable performance.
- **Implication**: Pod restarts result in a clean slate. Data durability is achieved via replication (Sentinel/Cluster modes) across live nodes, never via disk.

### 2.2 Default 'noeviction' Policy
- **Decision**: The default `maxmemory-policy` is `noeviction`.
- **Rationale**: Provides an "honest" data store behavior where memory exhaustion results in errors rather than silent data loss (eviction).
- **Instruction**: Avoid calling the project "optimized for caching" to prevent users from assuming a default LRU/LFU policy. It is a general-purpose in-memory store.

### 2.3 Guaranteed QoS
- **Decision**: Resources (CPU/Memory) `limits` always equal `requests` by default.
- **Rationale**: Prevents "OOM Killer" surprises and CPU throttling, ensuring Redis has stable performance.

### 2.4 Kubernetes as "Source of Truth"
- **Decision**: For Cluster mode, the operator uses the Kubernetes Pod list to detect "ghost" nodes.
- **Rationale**: Redis gossip can lag (up to 15s+). Knowing a Pod is gone via the K8s API allows immediate `CLUSTER FORGET` and faster healing.

### 2.5 Minimal Interference (Enablement over Intervention)
- **Philosophy**: Trust and enable Redis's internal mechanisms (Gossip, Sentinel) to handle their own state transitions. Don't "work against" them or attempt to "accelerate" their built-in timers (like `cluster-node-timeout`) unless absolutely necessary.
- **When to Intervene**:
    1. **Loss of Quorum**: When Redis cannot self-heal because it lacks a majority (e.g., `CLUSTER FAILOVER TAKEOVER`).
    2. **Deadlocks**: When a specific failure sequence prevents auto-recovery (e.g., a master failing before a replica has fully synced).
    3. **External Knowledge**: When the operator knows something Redis doesn't (e.g., "The Pod for this NodeID is deleted from K8s, it's never coming back").
- **Key Goal**: Support the internal workings of Sentinel and Gossip, only "helping" when a permanent stall or cluster-wide failure is detected.

### 2.6 Safe Bootstrap (Sentinel Mode)
- **Decision**: Uses `status.bootstrapRequired` and Operator-led registration in Sentinel.
- **Rationale**: Prevents empty restarted masters from wiping data on live replicas via full sync by strictly authorizing mastership via Sentinel.
- **Instruction**: All Redis pods must start in a wait-loop querying Sentinel until a master is assigned by the Operator.

### 2.7 Strict IP-Only Identity (Sentinel Mode)
- **Decision**: Sentinel and Redis nodes strictly use **Pod IPs** for identification, with hostname announcement and resolution explicitly disabled.
- **Rationale**: In a pure in-memory architecture, a pod restart results in total data loss. By using ephemeral IPs, a restarted pod (with a new IP) is treated as a completely new node by Sentinel. This prevents "Ghost Masters" (empty pods reclaimed as masters) and eliminates DNS-related race conditions during failover.
- **Implication**: Any transition to persistent storage (PVCs) will require a pivot to stable Podname-based identities. (See ADR-001)

### 2.8 Discovery Deadlock Prevention (Sentinel Mode)
- **Decision**: Removed `PING` connectivity check from the Redis startup script. (See ADR-002)
- **Rationale**: Replicas must start `redis-server` even if the reported master is unreachable. This allows them to register with Sentinel as living replicas, enabling Sentinel to perform a failover when the master is dead. Keeping the `PING` check leads to a deadlock where no replicas ever start because they are waiting for a master that Sentinel hasn't promoted yet.
- **Assumed Risk**: We assume Redis/Valkey handles unreachable masters gracefully at startup via standard retry logic.

### 2.9 Ghost Node Removal (Sentinel Mode)
- **Decision**: Proactively prune dead IPs from Sentinel's topology via `SENTINEL RESET`.
- **Rationale**: To prevent Sentinel's state from being cluttered with "ghost" IPs from previous pod generations, the operator cross-references Sentinel's replica list with the Kubernetes Pod list.
- **Safety**: Reset is only issued if the current master is a verified living Pod, ensuring Sentinel has a reliable source to re-discover the topology.

---

## 3. Deployment Modes

| Mode | Architecture | Use Case |
| :--- | :--- | :--- |
| **Standalone** | 1 Redis Pod | Dev / Simple caching |
| **Sentinel** | 3 Redis (1M+2R) + 3 Sentinels | High Availability (HA) |
| **Cluster** | $Shards 	imes (1 + Replicas)$ Pods | Horizontal Scaling / Large Data |

### Key Logic:
- **Sentinel Mode**: The operator manages a `chuck-chuck-chuck.net/role: master` label on Pods. The `{name}` Service uses this label as a selector to always route traffic to the current master.
- **Cluster Mode**: Sophisticated repair loop handles:
    1. Quorum loss (via `CLUSTER FAILOVER TAKEOVER`).
    2. Partition healing (via `CLUSTER MEET`).
    3. Ghost node removal (via `CLUSTER FORGET`).
    4. Slot reassignment and replica management.

---

## 4. Tech Stack & Tooling

- **Language**: Go (1.24+)
- **Framework**: Kubebuilder (v4 layout)
- **Testing**: Ginkgo & Gomega (BDD style)
- **Metrics**: `redis_exporter` as a sidecar; optional `ServiceMonitor`.
- **Image**: Defaults to **Valkey 8.0** (compatible with Redis 7.2+).

---

## 5. Directory Structure

```text
api/v1alpha1/               # CRD definitions (LittleRed types)
cmd/littlered/              # Operator entrypoint
config/                     # Kustomize manifests (CRDs, RBAC, Samples)
internal/controller/        # Core Reconciliation Logic
  ├── littlered_controller.go # Entrypoint reconciler
  ├── cluster_reconcile.go    # Cluster-specific logic
  ├── sentinel_monitor.go     # Sentinel health/failover logic
  └── resources.go            # K8s resource builders (STS, SVC, CM)
internal/redis/             # Redis/Cluster API clients
docs/                       # Detailed specs (ARCHITECTURE.md, CLUSTER_RECONCILIATION.md)
test/e2e/                   # End-to-end tests (requires Kind)
```

---

## 6. Critical Development Rules

1. **Idempotency**: Reconciliation must be idempotent. Always re-fetch the latest object state before updates to avoid conflicts.
2. **Scaffold Markers**: Never remove `// +kubebuilder:scaffold:*` markers.
3. **Auto-Generated Files**: Do not manually edit files marked `DO NOT EDIT` (e.g., `zz_generated.*`, `config/crd/bases/*`). Run `make manifests generate` instead.
4. **Owner References**: Use `SetControllerReference` so K8s garbage collects child resources when the `LittleRed` CR is deleted.
5. **Testing**: Add unit tests in `internal/controller/` and E2E tests in `test/e2e/` for any new feature or bug fix.
6. **Documentation Maintenance**: After any non-trivial change to the data model (API/Status), operator logic, or architectural decisions, you **MUST** update all relevant documentation files (e.g., `docs/API_SPEC.md`, `docs/ARCHITECTURE.md`, `LLM_STARTUP.md`, etc.).

---

## 7. Useful Commands

```bash
make manifests generate # Update CRDs and DeepCopy code
make test               # Run unit tests (envtest)
make test-e2e           # Run E2E tests (Kind)
make deploy             # Deploy operator to current cluster
kubectl apply -f config/samples/ # Try out sample CRs
```

---

## 8. Current Context & Ongoing Work
Refer to:
- `PROJECT`: Kubebuilder metadata.
- `docs/CLUSTER_RECONCILIATION.md`: Deep dive into cluster healing.
- `.gemini/GEMINI.md` or `.claud/CLAUDE.md`: (If they exist) for LLM-specific session notes and active task status.
- `docs/DEBUG_CRASH_TEST_INVESTIGATION.md`: Detailed investigation of the failing "both mechanisms active (crash)" e2e test (open as of 2026-02-18, not yet resolved).

### Active Investigation (2026-02-18)
**Failing test:** `Sentinel Advanced Failover Hybrid (Production) Mode > should recover correctly with both mechanisms active (crash)`

**Symptom:** After crash-deleting the master pod, `verifySentinelTopologySync` times out with "Sentinel reports 1 up replicas, but expected 2". Two slaves go `sdown` after the failover.

**Root observation:** Sentinel log shows TWO successive `Executing user requested FAILOVER` events ~7 seconds apart. A "user requested" failover must come from an explicit `SENTINEL FAILOVER mymaster` command — the only code that issues this is the preStop hook.

**Leading hypothesis (NOT YET CONFIRMED):** The preStop lifecycle hook fires briefly even during `--grace-period=0 --force` pod deletion on this Kubernetes version. The hook is a separate shell process; SIGKILL to redis-server doesn't kill it. It manages to send `SENTINEL FAILOVER mymaster` to Sentinel before being killed itself.

**Counter-evidence pending:** The user added a mechanism to capture preStop hook stdout/stderr in debug artifacts. Check the next run's artifacts for `preStop` output before concluding the double-failover hypothesis is correct.

**Other confirmed findings:**
- `ghost_ip: 10.233.64.156` — the operator correctly identifies the crash-deleted master's IP as a ghost and skips SENTINEL RESET (the ghost-master safety guard works)
- The operator's `anyTerminating` guard correctly suppresses healing during failover
- The second failover leaves redis-2 (`sdown`) and the old deleted pod's IP (`sdown`) as two dead slaves, leaving only 1 live replica

**Files examined:** `debug-artifacts-20260218-090058-should-recover-correctly-with-both-mechanisms-active-crash/`

See `docs/DEBUG_CRASH_TEST_INVESTIGATION.md` for full timeline and operator/sentinel log excerpts.

---
*Generated by Gemini CLI - Feb 2026*
