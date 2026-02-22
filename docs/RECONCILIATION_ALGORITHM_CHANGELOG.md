# Reconciliation Algorithm Changelog

This document tracks significant changes to the LittleRed reconciliation logic. Its purpose is to prevent regressions where a fix for one failure scenario (e.g., a deadlock) unintentionally breaks another previously fixed scenario.

## Format
- **ID:** Issue ID or descriptive name.
- **Date:** ISO Date.
- **Commit:** Git hash.
- **Problem:** Description of the failure (deadlock, race condition, sync failure).
- **Fix:** Description of the algorithmic change.
- **Regresses:** (Optional) IDs of previously fixed issues that this change might impact.

---

## [LR-001] Sentinel Failover Blocked by Reset Spam (#11)
- **Date:** 2026-02-21
- **Commit:** 3729af490027bc4b0c1c80187ed36f8c7b73d97a
- **Problem:** When a master crashes, its IP becomes a "ghost". The operator's ghost pruning logic issues `SENTINEL RESET`, which resets Sentinel's `s_down` timer. If the reconcile loop runs faster than the detection timeout, failover never triggers.
- **Fix:** Extended Rule A (Guardrails) to skip all sentinel healing if `state.RealMasterIP == ""`. The operator now remains passive during leaderless periods.
- **Impacts:** ADR-003 (Ghost Master Pruning). Rule D Extension is now gated by the presence of a living master.

## [LR-002] Cluster Mode Bus Port Mismatch
- **Date:** 2026-02-21
- **Commit:** 310510870cac8e35ebd7ade64a38f5be060bc732
- **Problem:** Positional `fmt.Sprintf` parameter mismatch caused `--cluster-announce-bus-port` to be configured with the standard port (6379) instead of the bus port (16379).
- **Fix:** Refactored all multi-line scripts in `resources.go` to use `text/template` with named parameters.
- **Regresses:** None. Internal maintenance.

## [LR-003] Cluster Mode Aggressive Failover
- **Date:** 2026-02-21
- **Commit:** 310510870cac8e35ebd7ade64a38f5be060bc732
- **Problem:** A restarted master pod (same IP, lost memory) could observe its replicas still seeing it as master, leading to a deadlock where the master waits for failover and replicas wait for the master.
- **Fix:** Added a yield loop to the cluster startup script. If failover isn't detected via peers within 30s, the starting pod issues an aggressive `CLUSTER FAILOVER TAKEOVER` to its own best-known replica.
- **Impacts:** Cluster Mode Data Safety.

## [LR-004] Sentinel Ghost Master Fallback Race
- **Date:** 2026-02-21
- **Commit:** <current>
- **Problem:** If a master pod crashes and restarts quickly with a new IP, it starts as a "master" by default. The operator's master identification logic (fallback mode) would pick this new IP as the authoritative master, bypassing the "leaderless" guardrail. This would then trigger Rule A+ (Ghost pruning) which would RESET Sentinels still monitoring the old (dead) IP, blocking their failover.
- **Fix:** Hardened `DetermineRealMaster` to ignore ghost IPs when counting Sentinel votes, and specifically to DISALLOW fallback to Redis-only master identification if a majority of Sentinels are still monitoring a ghost IP.
- **Impacts:** LR-001 (further hardening). Ensures the operator remains passive while Sentinel is timing out a dead master.

## [LR-005] Sentinel Divergent Master Deadlock
- **Date:** 2026-02-21
- **Commit:** <current>
- **Problem:** A sentinel could miss a failover event and remain stuck monitoring a previous master IP. If that IP was still "living" (e.g. the old master restarted and became a replica), the operator's ghost pruning logic wouldn't trigger, and the sentinel would never converge with the majority.
- **Fix:** Added a new healing rule to `reconcileSentinelCluster`: if a sentinel is monitoring a living but incorrect master (divergent from the majority consensus), it is force-reset to rediscover the real master via gossip.
- **Impacts:** Sentinel convergence safety.

## [LR-006] Surgical Pod Relabeling
- **Date:** 2026-02-21
- **Commit:** <current>
- **Problem:** During failover (leaderless period), the operator would relabel all living pods as 'orphan'. This caused massive K8s churn and triggered new reconciliation loops that could interfere with Sentinel's convergence.
- **Fix:** Refactored `updateMasterLabel` to be surgical. If no living master is known, the operator only ensures that the 'master' label is removed from whoever held it, leaving other pods untouched. Once a master is elected, ALL pods are reconciled to their correct labels (Master/Replica).
- **Impacts:** Cluster stability during failover.

## [LR-007] Sentinel Failover Blocked by Reset (Regression Fix)
- **Date:** 2026-02-21
- **Commit:** <current>
- **Problem:** An earlier attempt to allow ghost-master pruning during leaderless periods caused a regression. If a master died, its IP became a ghost. The operator would then issue `SENTINEL RESET` every 2 seconds. Because `RESET` wipes Sentinel's internal state, it reset the `down-after-milliseconds` timer, preventing failover from ever triggering.
- **Fix:** Re-instated the hard gate: NO `SENTINEL RESET` (for master or replicas) is allowed if the cluster is leaderless (`RealMasterIP == ""`). The operator must remain passive and allow Sentinel to complete its built-in failure detection and election.
- **Impacts:** Sentinel failover reliability.

## [LR-008] Sentinel Ghost Master RESET Ineffectiveness & Failure Detection Suppression
- **Date:** 2026-02-22
- **Commit:** <current>
- **Problem:**
    1. `SENTINEL RESET` does not change the master IP monitored by Sentinel; it only clears state like replicas and other sentinels. Stuck sentinels monitoring ghost IPs remained stuck even after a reset.
    2. Frequent `SENTINEL RESET` (every 2s) reset the `s_down` timer (5s), preventing failover detection for crashed masters when Rule A was bypassed (e.g. by a fast-restarting pod masquerading as a master).
- **Fix:**
    1. Replaced `SENTINEL RESET` with a `SENTINEL REMOVE` + `SENTINEL MONITOR` sequence for correcting stuck sentinels. This forces the sentinel to immediately point to the correct, living consensus master IP.
    2. Hardened Rule D (ghost pruning): `SENTINEL RESET` is now only issued if the consensus master is confirmed to be a living AND reachable pod. This ensures the operator remains passive during any period where failure detection might be in progress.
- **Impacts:** LR-001, LR-007 (further hardening). Ensures failover reliability and guaranteed convergence.

## [LR-009] Missing Replica Rescue (Rule R)
- **Date:** 2026-02-22
- **Commit:** <current>
- **Problem:** A Redis pod could remain in "master" mode (e.g., after a restart or crash) even if a different consensus master was already established by Sentinel. The operator was missing the logic to force these rogue pods back into "replica" mode, leaving Sentinel with an incomplete replica count and preventing the cluster from reaching "Running" phase.
- **Fix:** Implemented **Rule R (Replica Rescue)** in `reconcileSentinelCluster`. The operator now iterates over all Redis pods and issues `SLAVEOF <RealMasterIP>` to any pod that is not the consensus master and is not correctly following it.
- **Impacts:** Cluster convergence to "Running" phase.

## [LR-010] Redundant Reconciliation & Aggressive Rule R
- **Date:** 2026-02-22
- **Commit:** <current>
- **Problem:**
    1. The operator often issued duplicate `SLAVEOF` commands for the same pod within milliseconds. This was caused by status updates triggering immediate reconciliations that collided with the periodic requeue timer.
    2. Rule R was too aggressive, triggering `SLAVEOF` if `LinkStatus == "down"`. Since the replica handshake takes time, a second reconciliation would often trigger and interrupt an ongoing successful handshake.
- **Fix:**
    1. Added `GenerationChangedPredicate` to the `For` watch to ignore status-only updates, ensuring the 2-second timer remains the primary source of truth for periodic healing.
    2. Refined Rule R to only trigger on incorrect `Role` or `MasterHost`. It no longer triggers on `LinkStatus` alone, allowing transient handshakes to complete.
- **Impacts:** Reduction in audit log noise and faster cluster convergence.
