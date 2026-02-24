# Sentinel Mode Reconciliation Loop

This document describes the detailed reconciliation logic for **Sentinel mode** in the LittleRed operator.

For the high-level view that includes standalone and cluster modes, see [RECONCILIATION_LOOP.md](RECONCILIATION_LOOP.md).

---

## Overview

Sentinel mode manages three components:
- **Redis pods** (StatefulSet `<name>-redis`): one master + N-1 replicas
- **Sentinel pods** (StatefulSet `<name>-sentinel`): 3 sentinels forming a quorum
- **The operator**: observes ground truth, applies healing rules, stays passive during transitions

The operator follows a strict **enablement-over-intervention** philosophy (ADR-003): trust Sentinel's built-in failure detection (SDOWN/ODOWN) and failover mechanism. Only intervene when Sentinel cannot self-heal (ghost nodes, divergent masters, bare sentinels).

---

## Main Flow

```mermaid
graph TD
    Start((reconcileSentinel)) --> Resources["Ensure Resources<br/><i>Redis CM, Sentinel CM, SVCs, STSs</i>"]
    Resources --> BootstrapCheck{bootstrapRequired?}

    BootstrapCheck -- Yes --> BootstrapFlow
    BootstrapCheck -- No --> Labels

    subgraph BootstrapFlow ["Bootstrap (First Deploy)"]
        direction TB
        WaitIP{redis-0 has IP?}
        WaitIP -- No --> ReturnBoot[Requeue]
        WaitIP -- Yes --> ConfigEach["For each sentinel pod IP:<br/>SENTINEL MONITOR mymaster &lt;redis-0-IP&gt;<br/>SENTINEL SET auth-pass, down-after, etc."]
        ConfigEach --> ClearFlag["Clear bootstrapRequired"]
    end

    Labels["updateMasterLabel<br/><i>Surgical pod role labeling</i>"]
    Labels --> Healing["reconcileSentinelCluster<br/><i>Ground Truth + Healing Rules</i>"]

    Healing --> HealingDetail
    HealingDetail --> Monitor["ensureSentinelMonitor<br/><i>Background +switch-master subscriber</i>"]
    Monitor --> Status["updateSentinelStatus"]
    Status --> PhaseCheck{Phase?}

    PhaseCheck -- Running --> SteadyRequeue["Requeue @ steady interval"]
    PhaseCheck -- Not Running --> FastRequeue["Requeue @ fast interval"]

    subgraph HealingDetail ["reconcileSentinelCluster"]
        direction TB
        Gather["Gather Ground Truth<br/><i>Query all Redis + Sentinel pods</i>"]
        Gather --> DetermineRM["DetermineRealMaster<br/><i>Sentinel majority vote → RealMasterIP</i>"]
        DetermineRM --> Rule0

        Rule0["Rule 0: Re-register bare sentinels<br/><i>Sentinel reachable but not monitoring</i><br/><i>→ SENTINEL MONITOR + SET</i>"]
        Rule0 --> RuleA

        RuleA{"Rule A: Guardrails<br/>Any terminating pods?<br/>Failover active?"}
        RuleA -- Yes --> SkipAll["Skip all healing<br/><i>Let Sentinel/K8s finish</i>"]
        RuleA -- No --> GhostMaster

        GhostMaster{"Sentinel monitoring<br/>ghost or wrong master?"}
        GhostMaster -- Yes --> Reregister["SENTINEL REMOVE + MONITOR<br/><i>Point to correct RealMasterIP</i>"]
        Reregister --> RequeueGhost["Requeue to verify convergence"]
        GhostMaster -- No --> GhostReplica

        GhostReplica{"Ghost replicas in s_down?"}
        GhostReplica -- Yes --> Reset["SENTINEL RESET mymaster<br/><i>Only if master is living + reachable</i>"]
        GhostReplica -- No --> RuleR

        RuleR["Rule R: Replica Rescue<br/><i>Any non-master pod with role=master<br/>or following wrong master?<br/>→ SLAVEOF RealMasterIP</i>"]
    end
```

---

## Ground Truth Gathering

The operator queries **every** Redis and Sentinel pod on each reconcile cycle to build a `SentinelClusterState`:

| Source | Data Collected |
|--------|---------------|
| Each Redis pod (`INFO replication`) | Role, MasterHost, LinkStatus, Offset, Reachable |
| Each Sentinel pod (`SENTINEL MASTER`, `SENTINEL REPLICAS`) | MasterIP, FailoverStatus, Monitoring, Reachable, Replica list |

### DetermineRealMaster Algorithm

```mermaid
graph TD
    Start["Count sentinel votes per master IP"] --> Failover{Any sentinel<br/>reports active<br/>failover?}
    Failover -- Yes --> SetFlag["FailoverActive = true"]
    Failover -- No --> Majority

    SetFlag --> Majority

    Majority{"IP with majority<br/>of sentinel votes<br/>AND IP is a living pod?"}
    Majority -- Yes --> Accept["RealMasterIP = that IP"]
    Majority -- No --> GhostCheck

    GhostCheck{"Majority pointing<br/>at a ghost IP?"}
    GhostCheck -- Yes --> NoMaster["RealMasterIP = '' (leaderless)<br/><i>Wait for Sentinel SDOWN + failover</i>"]
    GhostCheck -- No --> RedisFallback

    RedisFallback["Fallback: find any reachable<br/>Redis pod with role=master"]
    RedisFallback --> FoundRedis{Found?}
    FoundRedis -- Yes --> AcceptRedis["RealMasterIP = that IP"]
    FoundRedis -- No --> NoMasterRedis["RealMasterIP = ''"]
```

The **ghost-majority guard** (LR-004) is critical: if most sentinels still point at a dead IP, it means Sentinel hasn't timed out yet. Falling back to Redis self-report would identify a restarted pod as master and trigger ghost pruning that resets Sentinel's SDOWN timers — blocking failover indefinitely.

---

## Healing Rules Detail

### Rule 0: Re-register Bare Sentinels

**Trigger**: Sentinel pod is reachable but `Monitoring == false` (no master configured).

**Cause**: A sentinel pod restarted with a new IP after bootstrapRequired was already cleared. Sentinel gossip cannot help here — without a MONITOR command, the pod doesn't know which pubsub channel to subscribe to.

**Action**: `SENTINEL MONITOR mymaster <RealMasterIP>` + apply all settings (auth-pass, down-after, failover-timeout, parallel-syncs) directly to that pod's IP.

**Safety**: Always safe — adding a monitor to an unconfigured sentinel is non-disruptive.

### Rule A: Guardrails

**Trigger**: Any pod has `DeletionTimestamp != nil` OR `FailoverActive == true`.

**Action**: Skip all healing rules. Return immediately.

**Rationale**: Kubernetes (pod termination) or Sentinel (failover election) is already performing a transition. Operator interference during transitions causes race conditions and timer resets.

### Ghost Master Correction (LR-005, LR-008)

**Trigger**: A sentinel is monitoring a master IP that is either:
- A ghost (IP not in current pod list), OR
- A living pod that is NOT the consensus RealMasterIP (divergent sentinel)

**Action**: `SENTINEL REMOVE mymaster` followed by `SENTINEL MONITOR mymaster <RealMasterIP>` + settings. This is a targeted fix on the individual sentinel pod, not a broadcast.

**Safety**: Only performed when `RealMasterIP != ""` AND `RealMasterIP` is living and reachable. If the cluster is leaderless, the operator stays passive.

**Why not RESET?** (LR-008): `SENTINEL RESET` does not change the monitored master IP. It only clears the replica list. A stuck sentinel pointing at a ghost IP stays stuck after RESET. `REMOVE + MONITOR` is the correct correction.

### Ghost Replica Pruning (Rule D)

**Trigger**: A sentinel's replica list contains IPs that are ghosts (not in K8s pod list) AND those replicas are in `s_down` state.

**Action**: `SENTINEL RESET mymaster` (broadcast to all sentinels via headless service).

**Safety**: Only issued when `RealMasterIP` is confirmed living and reachable. The `s_down` requirement prevents resetting during the brief window where a deleted pod's IP hasn't been marked down yet.

### Rule R: Replica Rescue (LR-009, LR-010)

**Trigger**: A reachable Redis pod that is NOT the RealMasterIP has:
- `Role == "master"` (thinks it's master, but consensus says otherwise), OR
- `MasterHost != RealMasterIP` (following the wrong master)

**Action**: `SLAVEOF <RealMasterIP> 6379`

**Safety**: Does NOT trigger on `LinkStatus == "down"` alone (LR-010). A transient link-down during handshake is normal and re-issuing SLAVEOF would interrupt it.

---

## Pod-Level Safety: Kill-9 / Crash Protection

The Redis startup script (in the container entrypoint) implements its own crash detection independent of the operator:

```mermaid
graph TD
    Start["Container starts"] --> QuerySentinel["Query Sentinel for stored run-id<br/>of master at my POD_IP"]
    QuerySentinel --> CrashCheck{"Sentinel master IP == my IP<br/>AND stored run-id is non-empty?"}

    CrashCheck -- Yes --> Yield["YIELD_MASTER = true<br/><i>Do NOT start Redis</i><br/>Sleep in 2s loop"]
    CrashCheck -- No --> NormalBoot["Normal Sentinel query loop"]

    Yield --> YieldLoop{"Sentinel still says<br/>I am master?"}
    YieldLoop -- Yes, count < 60 --> Yield
    YieldLoop -- No --> JoinAsReplica["Start as replica of new master"]
    YieldLoop -- Yes, count >= 60 --> Timeout["Timeout: start as master<br/><i>(no eligible replica existed)</i>"]

    NormalBoot --> AmIMaster{"Sentinel says<br/>my IP is master?"}
    AmIMaster -- Yes --> StartMaster["exec redis-server<br/>(no --replicaof)"]
    AmIMaster -- No --> PingMaster{"Master reachable?"}
    PingMaster -- Yes --> StartReplica["exec redis-server<br/>--replicaof masterIP"]
    PingMaster -- No, after 6 retries --> StartBare["exec redis-server<br/>(bare, no replicaof)<br/><i>Let Sentinel discover + failover</i>"]
```

This mechanism is documented in [ADR-001 (amendment)](adr/001-strict-ip-identity.md#amendment-in-pod-process-crash--known-limitation-accepted).

---

## Pre-Stop Hook

The sentinel-mode Redis pre-stop hook ensures graceful shutdown:

1. Checks if this pod is the master via `redis-cli ROLE`
2. If master: triggers `SENTINEL FAILOVER mymaster` on a sentinel so a replica is promoted before this pod terminates
3. Waits for Sentinel to confirm a different master before allowing shutdown
4. If replica: simply shuts down (Sentinel will detect and update its replica list)

---

## Status Determination

The operator reports `Phase: Running` only when ALL of these are true:
- All Redis pods ready (StatefulSet)
- All Sentinel pods ready (StatefulSet)
- Sentinel reports a known master (`masterPodName != ""`)
- Sentinel knows N-1 replicas as healthy (no `s_down`, `o_down`, or `disconnected` flags)

This prevents premature "Running" status before Sentinel has fully discovered the topology — which would allow tests or users to trigger failover before all replicas are registered.

---

## References
- [ADR-001: Strict IP-Only Identity](adr/001-strict-ip-identity.md)
- [ADR-003: Low-Interference Sentinel Reconciliation](adr/003-low-interference-sentinel-reconciliation.md)
- [Reconciliation Algorithm Changelog](RECONCILIATION_ALGORITHM_CHANGELOG.md)
- [RECONCILIATION_LOOP.md](RECONCILIATION_LOOP.md) — high-level view
