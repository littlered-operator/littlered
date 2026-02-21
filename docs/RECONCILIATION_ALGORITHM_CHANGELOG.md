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
