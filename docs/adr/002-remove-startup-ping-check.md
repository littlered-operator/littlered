# ADR-002: Startup Script — Option 3 (PING, if fail start bare)

## Status
Accepted (Supersedes original ADR-002)

## Context
In Sentinel mode, Redis pods use a startup shell script to query Sentinel for the current master before starting the `redis-server` process.

The original ADR-002 removed the PING connectivity check entirely, allowing replicas to join a potentially dead master IP unconditionally. This solved the "Discovery Deadlock" (where replicas refused to start because the master IP was unreachable) but introduced the "Zombie Replica" problem: replicas would start with `--replicaof <ghost-IP>` and hang in a retry loop, unable to serve traffic until Sentinel eventually reconfigured them.

## Decision
We implement **Option 3**: PING the reported master, and if it fails, start as a bare (masterless) server.

The startup script now has three paths:
1. **I am the master** (`$CURRENT_MASTER_HOST == $POD_IP`): Start bare (no `--replicaof`).
2. **Master is alive** (PING succeeds): Start as replica with `--replicaof`.
3. **Master is unreachable** (PING fails): Start bare so Sentinel can discover the pod and perform a failover if needed.

This avoids both the Discovery Deadlock (ADR-002's original concern) and the Zombie Replica problem.

## Consequences
- Pods starting bare when the master is unreachable will temporarily report as masters until Sentinel reconfigures them. This is acceptable because Sentinel's `SLAVEOF` command will correct the topology within seconds.
- A brief window of split-brain is possible if the master is actually alive but slow to respond to PING. The 2-second PING timeout makes this unlikely in practice.
- This approach is consistent with the "Minimal Interference" principle: the operator provides initial configuration, and Sentinel handles ongoing topology management.
