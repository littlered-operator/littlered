# Reconciliation Loop

This diagram describes the high-level reconciliation flow of the LittleRed operator. Mode-specific details are in dedicated documents:

- **[Sentinel Mode](RECONCILIATION_LOOP_SENTINEL.md)** — ground truth gathering, healing rules, kill-9 protection, DetermineRealMaster algorithm
- **[Cluster Mode](RECONCILIATION_LOOP_CLUSTER.md)** — ground truth gathering, repair loop (quorum recovery, partitions, ghosts, slots, replication), kill-9 protection

```mermaid
graph TD
    Start((Reconcile)) --> FetchCR[Fetch LittleRed CR]
    FetchCR --> ApplyDefaults[Apply Defaults]
    ApplyDefaults --> IsDeleted{Is Deleted?}

    IsDeleted -- Yes --> Cleanup["Reconcile Delete<br/><i>Remove finalizer, stop monitors</i>"]
    IsDeleted -- No --> HasFinalizer{Has Finalizer?}

    HasFinalizer -- No --> AddFinalizer[Add Finalizer & Requeue]
    HasFinalizer -- Yes --> Validate[Validate Spec<br/><i>Auth secret, TLS secret, cluster config</i>]

    Validate -- Fail --> SetFailed[Phase: Failed]
    Validate -- OK --> InitBootstrap{Sentinel & New?}

    InitBootstrap -- Yes --> SetBootstrap[Set bootstrapRequired = true]
    InitBootstrap -- No --> ModeSwitch
    SetBootstrap --> ModeSwitch{Spec.Mode?}

    %% Standalone
    ModeSwitch -- standalone --> StandaloneFlow["Reconcile Standalone<br/><i>ConfigMap, StatefulSet, Service</i>"]
    StandaloneFlow --> StandaloneStatus["Update Status & Requeue"]

    %% Sentinel
    ModeSwitch -- sentinel --> SentinelBox

    subgraph SentinelBox ["Sentinel Mode  → RECONCILIATION_LOOP_SENTINEL.md"]
        direction TB
        SentinelRes["Ensure Resources"]
        SentinelRes --> SentinelBoot["Bootstrap / Labels / Healing Rules"]
        SentinelBoot --> SentinelStatus["Update Status"]
    end

    SentinelBox --> SentinelRequeue["Requeue"]

    %% Cluster
    ModeSwitch -- cluster --> ClusterBox

    subgraph ClusterBox ["Cluster Mode  → RECONCILIATION_LOOP_CLUSTER.md"]
        direction TB
        ClusterRes["Ensure Resources"]
        ClusterRes --> ClusterGT["Gather Ground Truth"]
        ClusterGT --> ClusterRepair["Repair Loop / Bootstrap"]
        ClusterRepair --> ClusterStatus["Update Status"]
    end

    ClusterBox --> ClusterRequeue["Requeue"]
```
