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
- **Commit:** f3de62a
- **Problem:** If a master pod crashes and restarts quickly with a new IP, it starts as a "master" by default. The operator's master identification logic (fallback mode) would pick this new IP as the authoritative master, bypassing the "leaderless" guardrail. This would then trigger Rule A+ (Ghost pruning) which would RESET Sentinels still monitoring the old (dead) IP, blocking their failover.
- **Fix:** Hardened `DetermineRealMaster` to ignore ghost IPs when counting Sentinel votes, and specifically to DISALLOW fallback to Redis-only master identification if a majority of Sentinels are still monitoring a ghost IP.
- **Impacts:** LR-001 (further hardening). Ensures the operator remains passive while Sentinel is timing out a dead master.

## [LR-005] Sentinel Divergent Master Deadlock
- **Date:** 2026-02-21
- **Commit:** 66dcd83
- **Problem:** A sentinel could miss a failover event and remain stuck monitoring a previous master IP. If that IP was still "living" (e.g. the old master restarted and became a replica), the operator's ghost pruning logic wouldn't trigger, and the sentinel would never converge with the majority.
- **Fix:** Added a new healing rule to `reconcileSentinelCluster`: if a sentinel is monitoring a living but incorrect master (divergent from the majority consensus), issue `SENTINEL REMOVE` + `SENTINEL MONITOR <consensus-master-IP>` to force it onto the correct master. *(Note: initially implemented with `SENTINEL RESET`; superseded by LR-008 which changed all divergent-master correction to REMOVE+MONITOR.)*
- **Impacts:** Sentinel convergence safety.

## [LR-006] Surgical Pod Relabeling
- **Date:** 2026-02-21
- **Commit:** d1be0ca
- **Problem:** During failover (leaderless period), the operator would relabel all living pods as 'orphan'. This caused massive K8s churn and triggered new reconciliation loops that could interfere with Sentinel's convergence.
- **Fix:** Refactored `updateMasterLabel` to be surgical. If no living master is known, the operator only ensures that the 'master' label is removed from whoever held it, leaving other pods untouched. Once a master is elected, ALL pods are reconciled to their correct labels (Master/Replica).
- **Impacts:** Cluster stability during failover.

## [LR-007] Sentinel Failover Blocked by Reset (Regression Fix)
- **Date:** 2026-02-21
- **Commit:** b5bd053
- **Problem:** An earlier attempt to allow ghost-master pruning during leaderless periods caused a regression. If a master died, its IP became a ghost. The operator would then issue `SENTINEL RESET` every 2 seconds. Because `RESET` wipes Sentinel's internal state, it reset the `down-after-milliseconds` timer, preventing failover from ever triggering.
- **Fix:** Re-instated the hard gate: NO `SENTINEL RESET` (for master or replicas) is allowed if the cluster is leaderless (`RealMasterIP == ""`). The operator must remain passive and allow Sentinel to complete its built-in failure detection and election.
- **Impacts:** Sentinel failover reliability.

## [LR-008] Sentinel Ghost Master RESET Ineffectiveness & Failure Detection Suppression
- **Date:** 2026-02-22
- **Commit:** d1c9ff9
- **Problem:**
    1. `SENTINEL RESET` does not change the master IP monitored by Sentinel; it only clears state like replicas and other sentinels. Stuck sentinels monitoring ghost IPs remained stuck even after a reset.
    2. Frequent `SENTINEL RESET` (every 2s) reset the `s_down` timer (5s), preventing failover detection for crashed masters when Rule A was bypassed (e.g. by a fast-restarting pod masquerading as a master).
- **Fix:**
    1. Replaced `SENTINEL RESET` with a `SENTINEL REMOVE` + `SENTINEL MONITOR` sequence for correcting stuck sentinels. This forces the sentinel to immediately point to the correct, living consensus master IP.
    2. Hardened Rule D (ghost pruning): `SENTINEL RESET` is now only issued if the consensus master is confirmed to be a living AND reachable pod. This ensures the operator remains passive during any period where failure detection might be in progress.
- **Impacts:** LR-001, LR-005, LR-007 (further hardening). The REMOVE+MONITOR approach now covers both ghost master IPs (LR-008's primary scope) and divergent-but-living master IPs (LR-005's scope). Ensures failover reliability and guaranteed convergence.

## [LR-009] Missing Replica Rescue (Rule R)
- **Date:** 2026-02-22
- **Commit:** 2b12e92
- **Problem:** A Redis pod could remain in "master" mode (e.g., after a restart or crash) even if a different consensus master was already established by Sentinel. The operator was missing the logic to force these rogue pods back into "replica" mode, leaving Sentinel with an incomplete replica count and preventing the cluster from reaching "Running" phase.
- **Fix:** Implemented **Rule R (Replica Rescue)** in `reconcileSentinelCluster`. The operator now iterates over all Redis pods and issues `SLAVEOF <RealMasterIP>` to any pod that is not the consensus master and is not correctly following it.
- **Impacts:** Cluster convergence to "Running" phase.

## [LR-010] Redundant Reconciliation & Aggressive Rule R
- **Date:** 2026-02-22
- **Commit:** b2ef31e
- **Problem:**
    1. The operator often issued duplicate `SLAVEOF` commands for the same pod within milliseconds. This was caused by status updates triggering immediate reconciliations that collided with the periodic requeue timer.
    2. Rule R was too aggressive, triggering `SLAVEOF` if `LinkStatus == "down"`. Since the replica handshake takes time, a second reconciliation would often trigger and interrupt an ongoing successful handshake.
- **Fix:**
    1. Added `GenerationChangedPredicate` to the `For` watch to ignore status-only updates, ensuring the 2-second timer remains the primary source of truth for periodic healing.
    2. Refined Rule R to only trigger on incorrect `Role` or `MasterHost`. It no longer triggers on `LinkStatus` alone, allowing transient handshakes to complete.
- **Impacts:** Reduction in audit log noise and faster cluster convergence.

## [LR-011] SENTINEL RESET After Failover Wipes Replica Knowledge
- **Date:** 2026-03-24
- **Problem:**
    1. After a `+switch-master` event, the old master's IP becomes a ghost `s_down,slave` within seconds. The operator detects this ghost and issues `SENTINEL RESET` — which wipes Sentinel's knowledge of ALL replicas, including the healthy ones that just reconnected to the new master. If the new master is then killed (e.g., by a test or a second failure), Sentinel cannot promote anyone: `-failover-abort-no-good-slave`.
    2. Secondary: the preStop hook's replica count check (`grep -c "^name$" || echo 0`) produced a multiline value (`0\n0`) because both `grep -c`'s output and the `echo 0` fallback were captured by the command substitution. This broke the `[ $SLAVE_COUNT -ge 2 ]` arithmetic with `Illegal number`.
- **Fix:**
    1. Added a guard to Rule D: `SENTINEL RESET` now requires at least 1 healthy (non-ghost, non-`s_down`) replica to be known to Sentinel before firing. This prevents the race where RESET fires seconds after failover, before Sentinel has re-learned the surviving replicas. The operator logs the skip when no healthy replicas are known yet.
    2. Fixed the preStop hook: replaced `|| echo 0` with `|| true` + `${SLAVE_COUNT:-0}` fallback, so the command substitution captures only the single-line count from `grep -c`.
- **Regresses:** None. The new guard is strictly additive to the existing conditions from LR-008 (living + reachable master). It covers the gap where the master IS reachable but Sentinel hasn't yet re-discovered its replicas after a recent failover.
- **Impacts:** LR-001, LR-007, LR-008 (further hardening of the SENTINEL RESET safety chain).

## [LR-012] Cluster Ground-Truth Gather Stalls on Stale Pod IPs (Slow Convergence)
- **Date:** 2026-06-24
- **Problem:** The cluster repair logic was *correct* but too *slow* to converge under rapid multi-round pod churn — the e2e test `Cluster Mode Chaos Testing > should survive multiple rounds of random pod deletions` timed out while the cluster was mid-heal (it fully recovered minutes after the test gave up). Root cause: `gatherGroundTruth` builds its pod set from the cache-backed `r.Get(pod)`, which during churn returns a **stale `Status.PodIP`** for a recently-recreated pod. `GatherClusterGroundTruth` then dialed every pod IP **serially**, and each dead IP blocked ~25s (go-redis: 5 dial attempts × 5s `DialTimeout`) before failing. With one or two stale IPs in the set, every reconcile spent 25–50s blocked inside gather, so the loop turned over only 2–3 times inside the test's 120s window — too few to FORGET ghosts, re-MEET survivors, and re-assign new replicas. A secondary stall: the ghost `CLUSTER FORGET` loop dialed *every* node including unreachable ones.
- **Fix:**
    1. **Parallelized** `GatherClusterGroundTruth` (identity probes and topology probes both fan out concurrently), so total gather latency is bounded by the slowest single probe rather than their sum. Decomposed into `gatherNodeIdentities` / `gatherTopology` / `probeNodeTopology` / `computePartitions` for testability.
    2. Added a hard per-probe deadline (`ClusterProbeTimeout = 3s`) to the operator's `GetClusterID` / `GetClusterInfo` / `GetClusterNodes`, so a dead IP fails in ~3s instead of ~25s while staying far above a live in-cluster node's sub-second response.
    3. `CLUSTER FORGET` now skips unreachable nodes.
- **Regresses:** None. This changes only gather *latency*, not decision logic or requeue cadence. Critically — and unlike the Sentinel `SENTINEL RESET` timer-reset trap (LR-001/LR-007) where a faster loop *suppressed* failover — the destructive cluster-mode interventions are gated independently of loop speed: quorum-loss `CLUSTER FAILOVER TAKEOVER` fires on a topology condition (`votingMasters <= shards/2`), and orphan force-promotion is gated by a wall-clock `orphanTimeout` (`ClusterNodeTimeout + failoverGracePeriod`) tracked via the persisted `DetectedAt`. A faster gather only lets the operator *observe and act* sooner; it cannot trip these guards prematurely.
- **Impacts:** Cluster-mode convergence speed under churn. No change to Sentinel-mode paths.
