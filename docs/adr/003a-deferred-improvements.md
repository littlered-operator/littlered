# ADR-003a: Deferred Improvements

## Status
Deferred

## Context
During the revert to low-interference sentinel reconciliation (ADR-003), several potential improvements were identified but intentionally deferred to keep the change focused and low-risk.

## Deferred Items

### 1. Sentinel Settings Reconciliation
The operator currently applies sentinel settings (down-after-milliseconds, failover-timeout, parallel-syncs) only during bootstrap and after SENTINEL RESET. A dedicated reconciliation step could periodically verify and correct these settings without interfering with failover state.

### 2. Sentinel Health Monitoring
The operator could monitor sentinel health more actively (e.g., detecting sentinels that lost their configuration after restart) and re-bootstrap individual sentinels. Currently this is handled by the bootstrap flow and RESET.

### 3. Startup Script Improvements
- The startup script could use a more sophisticated master reachability check (e.g., `INFO replication` instead of `PING`) to distinguish between a master that is alive but slow vs. one that is actually dead.
- Consider adding a timeout for the initial Sentinel query loop to prevent pods from hanging indefinitely if all sentinels are down.

### 4. Label Update Debouncing
The `updateMasterLabel` function could benefit from debouncing to avoid rapid label changes during cascading failovers. The current `anyTerminating` guard is a simple version of this.

### 5. Status Phase Granularity
The phase check could distinguish between "Initializing" (first boot) and "Degraded" (running but not all pods ready) to give operators better visibility into cluster state.
