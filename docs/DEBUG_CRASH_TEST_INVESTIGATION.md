# Debug Investigation: "both mechanisms active (crash)" e2e Test Failure

**Date:** 2026-02-18
**Status:** OPEN — hypothesis identified, counter-evidence pending
**Branch:** `littlered-cli`
**Debug artifacts:** `debug-artifacts-20260218-090058-should-recover-correctly-with-both-mechanisms-active-crash/`

---

## Test Overview

The `Hybrid (Production) Mode` context deploys ONE CR (`hybrid-<timestamp>`) and runs two sequential `It` specs against it using a `for _, mode := range restartModes` loop:
1. `should recover correctly with both mechanisms active (graceful)` — passes
2. `should recover correctly with both mechanisms active (crash)` — **fails**

The crash test crash-deletes the CURRENT master (whichever pod was elected during the graceful test's failover) using `kubectl delete pod --grace-period=0 --force`.

**Failure message:**
```
Timed out after 120.001s.
Sentinel reports 1 up replicas, but expected 2
```

---

## Pod IP Map (this run)

| Pod | IP | Role at crash-test start |
|---|---|---|
| `redis-0` (replacement after graceful test) | `10.233.65.127` | replica |
| `redis-1` | `10.233.66.102` | replica |
| `redis-2` | `10.233.64.156` | **master** (elected during graceful failover) |
| `redis-0` (original, deleted by graceful test) | `10.233.65.229` | dead |

The crash test identifies `redis-2` as the initial master and crash-deletes it.

---

## Key Event Timeline

All times are UTC (CET−1).

| Time | Source | Event |
|---|---|---|
| 07:58:28 | sentinel-1 | Sentinel pod starts (cluster being deployed by BeforeAll) |
| 07:58:35 | sentinel-1 | `+slave redis-2 10.233.64.156` discovered |
| 07:58:45 | sentinel-1 | `+slave redis-1 10.233.66.102` discovered |
| ~07:58:47 | operator | `status.phase = Running` → BeforeAll finishes → graceful test starts |
| 07:58:49 | sentinel-1 | **FIRST `Executing user requested FAILOVER`** (of master `10.233.65.229`) |
| 07:58:49 | redis-2 pre-delete | `PRIMARY MODE enabled` — redis-2 becomes master |
| 07:58:51 | sentinel-1 | `+switch-master 10.233.65.229 → 10.233.64.156` (redis-2 is new master) |
| 07:58:51 | sentinel-1 | redis-1 reconfigured as replica (`slave-reconf-done`) |
| 07:58:53 | sentinel-1 | New pod (replacement redis-0, IP `10.233.65.127`) asks for sync |
| ~07:58:53 | test code | Graceful test passes `verifySentinelTopologySync`. **Crash test begins.** |
| ~07:58:53 | test code | `capturePreDeleteLogs` snapshot taken for redis-2 |
| ~07:58:53 | test code | `kubectl delete pod redis-2 --grace-period=0 --force` issued |
| 07:58:55 | sentinel-1 | `+slave 10.233.65.127` (new redis-0) discovered |
| 07:58:56 | sentinel-1 | **SECOND `Executing user requested FAILOVER`** (of master `10.233.64.156`!) |
| 07:58:56 | operator | `Sentinel reported a ghost master, ghost_ip: 10.233.64.156` (redis-2 gone) |
| 07:58:56 | operator | Relabels all pods as `orphan` (correct, since ghost master) |
| 07:58:56 | operator | `Sentinel master is a ghost node, skipping RESET` (ghost-master guard fires) |
| 07:58:57 | sentinel-1 | `+switch-master 10.233.64.156 → 10.233.66.102` (redis-1 becomes master) |
| 07:58:57 | sentinel-1 | `+slave-reconf-sent 10.233.65.127` (new redis-0 reconfigured) |
| 07:59:02 | sentinel-1 | `+sdown master 10.233.64.156` (deleted redis-2 goes subjectively down) |
| 07:59:02 | sentinel-1 | `+failover-end`, `+switch-master` completes to `10.233.66.102` |
| 07:59:07 | sentinel-1 | `+sdown slave 10.233.64.156` (redis-2 dead) |
| 07:59:07 | sentinel-1 | `+sdown slave 10.233.65.229` (old master dead) |
| ~09:01:03 | test | **Times out.** Sentinel reports 1 up replica, test expects 2. |

---

## Why Does the Test Fail?

After the double failover, the final Sentinel topology is:
- **Master:** `10.233.66.102` (redis-1) — healthy
- **Slave 1:** `10.233.65.127` (new redis-0) — healthy ✓
- **Slave 2:** `10.233.64.156` (deleted redis-2) — `sdown` ✗
- **Slave 3:** `10.233.65.229` (old deleted master) — `sdown` ✗

Sentinel reports only **1 up replica** (`10.233.65.127`). The test expects 2.

The root problem is the SECOND failover at 07:58:56, which:
1. Triggers while redis-2 (at `10.233.64.156`) is already dead
2. Sends `REPLICAOF 10.233.64.156` to `10.233.65.127` (new redis-0) as a reconf step
3. Leaves `10.233.65.127` pointing at a dead master briefly
4. Eventually `10.233.65.127` reconfigures to `10.233.66.102` (the new master), but Sentinel still shows `10.233.64.156` and `10.233.65.229` as `sdown` slaves — counting against the expected replica count

---

## Leading Hypothesis: PreStop Hook Fires During Crash Deletion

The only code that sends `SENTINEL FAILOVER mymaster` is the preStop lifecycle hook (`internal/controller/resources.go`).

The second "user requested FAILOVER" at 07:58:56 implies this command was sent to Sentinel. The operator logs show it did NOT send this command (it detected the ghost master and skipped). No other code issues `SENTINEL FAILOVER`.

**Proposed mechanism:**
1. `kubectl delete pod redis-2 --grace-period=0 --force` is issued
2. Pod is immediately removed from etcd (force delete)
3. kubelet is notified asynchronously and begins container cleanup
4. **kubelet starts the preStop hook process** (a shell, separate from redis-server)
5. kubelet sends SIGKILL to redis-server (grace period = 0)
6. redis-server dies (SIGKILL)
7. **The preStop shell script is still running** (SIGKILL only kills the main container PID)
8. preStop: `redis-cli info replication` — may succeed if it ran BEFORE redis-server died (race)
9. preStop: checks slave count from Sentinel → finds 2 slaves → breaks immediately
10. preStop: issues `SENTINEL FAILOVER mymaster` to Sentinel → SUCCESS
11. preStop is eventually killed by SIGKILL or cgroup cleanup
12. Sentinel executes second failover unnecessarily

**Timing consistency:** The 3-second window (crash delete at ~07:58:53 → second SENTINEL FAILOVER at 07:58:56) is consistent with a preStop hook executing a few `redis-cli` commands before being killed.

**Kubernetes behavior note:** Kubernetes source code (`kuberuntime_container.go`) shows `gracePeriod > 0` is the guard for running preStop. With `DeletionGracePeriodSeconds=0`, the preStop *should* be skipped. However, force deletion takes a different (orphaned-pod cleanup) code path in kubelet, and behavior may differ by Kubernetes version/implementation.

---

## Counter-Evidence Status

**The user has added a mechanism to capture preStop hook stdout/stderr in debug artifacts** (as of 2026-02-18). The next failing test run should include preStop debug output files. If the second SENTINEL FAILOVER is NOT from the preStop hook, this output will be absent or contradictory.

**If the double-failover hypothesis is WRONG**, alternative sources for the second `SENTINEL FAILOVER` to investigate:
1. The sentinel monitor goroutine (check `internal/controller/sentinel_monitor.go` for any failover-initiation code)
2. The `lrctl` CLI tool if it was running concurrently
3. Sentinel's own automatic failover (but `down-after-milliseconds=5000` rules this out for a 3-second trigger)
4. A preStop hook from the ORIGINAL graceful test's pod that was still running (the pod at `10.233.65.229` terminates slowly)

---

## Operator Behavior: What Worked Correctly

- **Ghost master guard:** At 07:58:56, operator correctly identifies `10.233.64.156` as a ghost master (pod no longer exists) and skips SENTINEL RESET. The log: `"Sentinel master is a ghost node, skipping RESET until failover completes"`.
- **anyTerminating guard:** During 07:58:49–07:58:53, operator correctly skips all healing because the deleted pod still has `DeletionTimestamp` set.
- **ValidIPs population:** `GatherClusterState` correctly populates `ValidIPs` from the live pod map (excluding pods with `DeletionTimestamp` or no IP). The ghost detection is working as designed.

---

## Fixes Applied in Previous Sessions (2026-02-18)

| Fix | Location | Description |
|---|---|---|
| Fix 1 | `resources.go` preStop | Wait for Sentinel to discover all `EXPECTED_SLAVES=2` replicas before issuing `SENTINEL FAILOVER` |
| Fix 2 | `littlered_controller.go` | Ghost pruning uses `o_down` only (not `s_down`), preventing premature SENTINEL RESET during active failover |
| Fix 3 | `littlered_controller.go` | `status.phase = Running` requires `sentinelReplicasOK >= expectedReplicas` (Sentinel must know all replicas) |
| Fix 4 | `littlered_controller.go` | `updateMasterLabel` skips updates when any pod has `DeletionTimestamp` |
| Infra | `pod_utils_test.go` | Added pre-delete log snapshots and streaming log capture for dying pods |
| Infra | `e2e_suite_test.go` | Wired streaming log lifecycle (init, reset per test, cleanup on suite end) |
| Infra | `debug_artifacts.go` | Streaming logs copied to debug artifact directory on failure |

These fixes were NOT sufficient to resolve the crash test failure — the double failover persists.

---

## Remaining Work

1. **Verify hypothesis** — check next run's debug artifacts for preStop output in the crash mode test
2. **If hypothesis confirmed:** decide between:
   - Changing crash test to use `kubectl exec -- kill -9 1` (process crash, no preStop)
   - Making the operator resilient to double failovers (idempotent recovery)
   - Making the preStop hook skip if redis-server is unresponsive (defensive ping before SENTINEL FAILOVER)
3. **If hypothesis wrong:** investigate the sentinel monitor goroutine and other SENTINEL FAILOVER sources
4. **Regardless:** the operator must handle `--grace-period=0 --force` correctly in production — this scenario is valid and the operator should recover cleanly even if a double failover is triggered externally
