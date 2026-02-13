# Reconciliation Loop

This diagram describes the reconciliation logic of the LittleRed operator, including the high-level flow and mode-specific behaviors for Standalone, Sentinel, and Cluster modes.

```mermaid
graph TD
    Start((Reconcile)) --> FetchCR[Fetch LittleRed CR]
    FetchCR --> ApplyDefaults[Apply Defaults]
    ApplyDefaults --> IsDeleted{Is Deleted?}
    
    IsDeleted -- Yes --> Cleanup[Reconcile Delete: Remove Finalizer, Stop Monitors]
    IsDeleted -- No --> HasFinalizer{Has Finalizer?}
    
    HasFinalizer -- No --> AddFinalizer[Add Finalizer & Requeue]
    HasFinalizer -- Yes --> Validate[Validate Spec]
    
    Validate --Fail --> SetFailed[Set Status Phase: Failed]
    Validate -- Success --> InitBootstrap{Is Sentinel & New?}
    
    InitBootstrap -- Yes --> SetBootstrap[Set status.bootstrapRequired=true]
    InitBootstrap -- No --> ModeSwitch{Spec.Mode?}
    SetBootstrap --> ModeSwitch

    %% Standalone Mode
    ModeSwitch -- standalone --> StandaloneFlow[Reconcile Standalone Resources: CM, STS, SVC]
    StandaloneFlow --> UpdateStatus[Update Status & Requeue]

    %% Sentinel Mode
    ModeSwitch -- sentinel --> SentinelFlow[Reconcile Sentinel Resources: Redis/Sentinel CMs, STS, SVCs]
    SentinelFlow --> UpdateMasterLabel[Update Master Pod Labels]
    UpdateMasterLabel --> StartMonitor[Ensure Background Sentinel Monitor]
    StartMonitor --> UpdateSentinelStatus[Update Sentinel Status]
    UpdateSentinelStatus --> IsRunning{Phase == Running?}
    IsRunning -- Yes --> ClearBootstrap[Set status.bootstrapRequired=false]
    IsRunning -- No --> RequeueSentinel[Requeue]
    ClearBootstrap --> RequeueSentinel

    %% Cluster Mode
    ModeSwitch -- cluster --> EnsureClusterRes[Ensure Cluster Resources: CM, Headless SVC, STS]
    EnsureClusterRes --> AllPodsReady{All Pods Ready?}
    
    AllPodsReady -- No --> WaitPods[Set Initializing & Requeue]
    AllPodsReady -- Yes --> GatherGT[Gather Ground Truth: Query all Redis Nodes]
    
    GatherGT --> IsHealthy{Cluster Healthy?}
    
    IsHealthy -- Yes --> UpdateClusterStatus[Update Cluster Status & Requeue]
    IsHealthy -- No --> RepairCluster[Repair Cluster Loop]
    
    subgraph RepairLoop [Repair Cluster Loop]
        direction TB
        Quorum[Quorum Recovery: Force Takeover if Majority Lost]
        --> Partitions[Heal Partitions: CLUSTER MEET]
        --> Ghosts[Forget Ghost Nodes: CLUSTER FORGET]
        --> Shards[Recover Missing Shards: CLUSTER ADD SLOTS]
        --> Replicate[Replication Repair: CLUSTER REPLICATE]
        --> Bootstrap[Bootstrap: If 0 slots and no replicas]
    end
    
    RepairCluster --> RepairLoop
    RepairLoop --> UpdateClusterStatus
```
