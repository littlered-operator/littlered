# ADR-005: Leaderless Bootstrap-Deadlock Recovery (Sentinel Mode)

## Status
Accepted

## Context
A production mass-restart incident (node maintenance). After the restart of two already-running
sentinel-mode instances, both sat stuck and not serving for ~50 minutes. The state:

- Every Sentinel was **bare** — reachable on `:26379` but monitoring no master
  (`current-epoch 0`, no persisted `sentinel monitor` line).
- Every Redis pod was `1/2 Running` — `redis-server` parked in the ADR-002/§3.8 startup
  wait-loop, waiting for Sentinel to name a master, so no Redis node was a reachable master.
- Therefore `DetermineRealMaster` returned `RealMasterIP == ""`.

Two design facts combine into a deadlock the operator could not escape:

1. `bootstrapSentinel()` runs only when `Status.BootstrapRequired == true`. That flag is set
   exactly once, when `Status.Phase == ""`, and is never re-armed. An already-initialized
   instance therefore never re-bootstraps.
2. Every runtime healing rule — Rule 0 (re-register a bare sentinel), LR-008 (ghost-master
   REMOVE+MONITOR), LR-011/LR-013 (ghost-replica RESET) — is gated on `RealMasterIP != ""`.
   This gating is correct: acting while genuinely leaderless risks fighting an in-flight
   Sentinel failover. But it means that when the *entire* Sentinel quorum loses its config,
   no rule fires.

Recovery required a human to `redis-cli SENTINEL MONITOR` one sentinel by hand (after which
Rule 0 propagated the master to the rest), or a full `LittleRed` CR delete + redeploy. Neither
is an acceptable steady-state runbook — the CR is frequently GitOps-managed and not something an
operator may delete by hand.

This is distinct from the LR-013 follow-up (an `o_down` master with no known replicas but living
pods): there the Sentinels still *monitor* a dead master; here they are *bare*.

## Decision
Add **Rule L — leaderless recovery** (`recoverLeaderlessDeadlock`), the only rule that runs while
`RealMasterIP == ""`. It lives inside the former `if RealMasterIP == "" { return nil }` early-out
of `reconcileSentinelCluster`, so no `RealMasterIP != ""` path changes behavior.

It is deliberately conservative and fires only in the deadlock signature:

1. **All reachable Sentinels are bare** (`AllSentinelsBare()`). A bare quorum cannot self-heal
   (a sentinel with no master config cannot join the pubsub channel gossip needs), and this is
   what distinguishes a bootstrap deadlock from a recent master death (where sentinels still
   monitor the dead master and can fail over — a state the pre-existing rules correctly own).
2. **A reachable Sentinel quorum exists**, so the seed can form consensus.
3. **Rule A has passed** (no pod terminating, no active failover).
4. **The state has persisted** past a 30s cooldown, tracked in `Status.LeaderlessSince`. The 2s
   fast requeue re-checks well within that window, so a transient rollout blip clears the marker
   long before the cooldown expires.

The action is **data-aware**. The gatherer now collects per-pod key count (`INFO keyspace`) and
replication id into `RedisNodeState.Keys` / `.Replid`:

- **No reachable pod holds data** → safe. Seed `redis-0` as master via `seedSentinelsWithMaster`
  (the per-pod-IP MONITOR loop shared with `bootstrapSentinel`). In a pure in-memory store an
  unreachable or wait-looping server has no data by definition, so this is the common
  mass-restart case.
- **Some pod holds data** → destructive to break. **Refuse** and wait for a human, unless the
  owner opted in via `sentinel.allowUnsafeRebootstrapOnDeadlock`. When opted in, force-elect the
  most-complete pod (`BestDataHolder()`: highest replication offset, tie-broken by key count then
  IP) and log — loudly — that data on the other pods will be discarded, and, when the holders
  span multiple replication lineages (distinct `master_replid`), that genuinely independent
  writes will be lost.

## Rationale
- **Why key count, not role.** Role does not answer "does this pod hold data" — a freshly
  restarted empty pod reports `role:master`. `DBSIZE` / `INFO keyspace` is the direct, mode-
  agnostic signal (no slot map, which sentinel mode lacks).
- **Why offset for the unsafe election.** The usual real-world failure is a lost replication
  link leaving one node newer/more-complete and another with an older snapshot — the higher
  offset is the newer node, so full-syncs should flow outward from it. Offsets are only
  comparable *within* a replication lineage, hence the divergence warning when replids differ.
- **Why default-refuse over live data.** The pre-existing behavior risks nothing by inaction; it
  merely deadlocks. Making recovery unconditional would trade a recoverable stall for silent data
  loss. Opt-in keeps the safe case automatic and the destructive case a deliberate choice.

## Consequences
- The common mass-restart deadlock now self-heals in ~30s instead of requiring manual
  intervention.
- A new status field `leaderlessSince` and a new spec field
  `sentinel.allowUnsafeRebootstrapOnDeadlock` (default false).
- `lrctl verify` now reports per-pod `keys:N`, making empty-vs-holding-data visible in sentinel
  mode.
- This partially realizes ADR-003a deferred item #2 ("detect sentinels that lost their
  configuration after restart and re-bootstrap").

## Alternatives considered
- **Re-arm `BootstrapRequired` instead of a dedicated rule.** Rejected: `bootstrapSentinel`
  hardcodes `redis-0` as master, which is wrong for the unsafe with-data path (we must elect the
  most-complete pod), and re-arming adds a reconcile of latency. Rule L calls the shared
  `seedSentinelsWithMaster` directly with a chosen master.
- **Act purely on leaderlessness (no data gate).** Rejected — see Rationale (silent data loss).
- **Act immediately (no cooldown).** Rejected: risks acting on a transient during a rollout,
  before pods obtain IPs or the normal bootstrap path runs.

## References
- CLAUDE.md §3.10
- `docs/RECONCILIATION_ALGORITHM_CHANGELOG.md` (LR-015)
