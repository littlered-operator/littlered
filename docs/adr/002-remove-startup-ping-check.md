# ADR-002: Removal of PING Connectivity Check in Sentinel Startup

## Status
Proposed (Under Testing)

## Context
In Sentinel mode, Redis pods use a startup shell script to query Sentinel for the current master before starting the `redis-server` process. 

Previously, we implemented a `PING` check in this script: a pod would only start as a replica if it could successfully ping the master IP reported by Sentinel. This was intended to prevent "Zombie Replicas" from joining a dead master IP (ghost nodes).

However, in "Strict IP Identity" mode (see ADR-001), this created a **Discovery Deadlock**:
1. After a master restart (new IP), Sentinel still reports the old (ghost) IP as master.
2. Replicas fail to `PING` the ghost IP.
3. Replicas never start `redis-server`.
4. Sentinel never discovers the living replicas (because they aren't running).
5. Sentinel never promotes a new master because its replica list is empty.

## Decision
We decided to **remove the PING connectivity check** from the Redis startup script. 

Replicas will now `exec redis-server --replicaof <IP> <PORT>` immediately after receiving an IP from Sentinel, regardless of whether that IP is currently reachable.

## Assumptions
- We assume that the Redis/Valkey server process is robust enough to handle an unreachable master at startup. It should enter a retry loop, logging connection errors until the master becomes available or Sentinel performs a failover and reconfigures the replica via `CONFIG SET`.
- We assume this behavior is preferable to the "Discovery Deadlock" described above, which requires manual intervention to recover.

## Consequences & Risks
- **Zombie Replicas**: If the assumption about Redis robustness is invalid, pods might hang or enter a crash loop if the master is unreachable.
- **Operator Overhead**: If replicas join a ghost master and Sentinel fails to perform a timely failover, the cluster remains in a degraded state.
- **Data Loss**: In a full cluster shutdown, where all identities (IPs) have changed, this removal allows the cluster to self-heal (at the cost of a clean slate/data loss, which is expected in our pure in-memory model).

## Verification Plan
- Monitor E2E tests specifically for pods getting stuck or crashing when starting up against a dead master IP.
- Observe Sentinel logs during failover to ensure it correctly identifies and reconfigures replicas that started up against the "ghost" IP.
