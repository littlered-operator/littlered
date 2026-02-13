# LittleRed - Scope Definition

> Quick reference for what's in and out of scope.

**Last Updated**: 2026-02-11

---

## Project Identity

| Attribute | Value |
|-----------|-------|
| Product Name | LittleRed |
| Technical Name | `littlered` |
| API Group | `littlered.chuck-chuck-chuck.net` |
| API Version | `v1alpha1` (initial) |
| Default Image | `docker.io/valkey/valkey:8.0` |
| Image Format | `{registry}/{path}:{tag}` (each component overridable) |

---

## In Scope

### Pre-MVP (Standalone) ✅

- [x] Single Redis/Valkey instance (StatefulSet, 1 replica)
- [x] Redis 7.2+ / Valkey 7.2+ support
- [x] In-memory only (no persistence)
- [x] Opinionated defaults (performance-optimized)
- [x] Full redis.conf override capability
- [x] Optional password authentication
- [x] Optional TLS
- [x] Prometheus metrics (redis_exporter sidecar)
- [x] Configurable resources (CPU/memory)
- [x] Guaranteed QoS by default
- [x] CR status reporting

### MVP (Sentinel) ✅

- [x] Sentinel mode: 1 master + 2 replicas + 3 sentinels
- [x] Automatic failover
- [x] Rolling update strategy
- [x] Recreate update strategy
- [x] Optional ServiceMonitor creation
- [x] Master/replica/sentinel services

### Phase 2 (Cluster) ✅

- [x] Cluster mode: configurable shards with replicas per shard
- [x] Automatic cluster bootstrapping
- [x] Operator-managed recovery (topology in CR status)
- [x] No PersistentVolumes required
- [x] Data durability through replication

### Infrastructure

- [x] Go implementation (kubebuilder)
- [x] Kubernetes 1.28+ support
- [x] Helm chart distribution
- [x] Configurable namespace/cluster scope
- [x] Unit tests
- [x] Integration tests (envtest)
- [x] E2E tests (kind)

---

## Out of Scope (Current)

### Explicitly Excluded

| Feature | Reason | Future? |
|---------|--------|---------|
| Persistence (RDB/AOF with PVC) | Not needed for pure in-memory use case | Maybe |
| Redis < 7.2 | Legacy, not required | No |
| Cluster slot migration | Needed for dynamic scaling | Yes |
| ACL-based auth | Complexity, password sufficient | Maybe |
| Auto-sizing | Sizing is app-driven | No |
| Prometheus alerts | Low priority | If trivial |
| OLM distribution | Helm sufficient | Maybe |
| Multi-cluster | Out of scope | Unlikely |

### Will Not Do

- Auto-detection of Prometheus Operator (explicit enable only)
- Backup/restore (no persistence)
- Cross-namespace replication
- Custom Redis modules

---

## Architecture Constraints

Design decisions that support extensibility:

1. **Sentinel topology**: Fixed 1+2+3, architected for configurable counts in future
2. **Redis Cluster**: ✅ Implemented with operator-managed topology (no nodes.conf dependency)
3. **Persistence**:
   - **Default**: Actively disable persistence (`save ""`, `appendonly no`) to guarantee pure in-memory behavior
   - **No volumes**: Default deployment requires no PVCs
   - **Expert override**: User can re-enable persistence via `spec.config.raw` if they really want it (their responsibility)
   - **Future**: Operator-managed persistence (with proper PVC handling) could be added as opt-in feature later
4. **Cluster state storage**: Topology stored in CR status, enabling deployment on any K8s cluster without local storage requirements

---

## Success Criteria

### Pre-MVP Complete ✅

1. ✅ Can deploy standalone Redis via CR
2. ✅ Can deploy standalone Valkey via CR
3. ✅ SET/GET operations work
4. ✅ Prometheus metrics accessible
5. ✅ Pod deletion triggers recreation
6. ✅ All unit and integration tests pass

### MVP Complete ✅

1. ✅ Sentinel mode deploys all 6 pods
2. ✅ Automatic failover works (kill master → new master elected)
3. ✅ Rolling updates maintain availability
4. ✅ ServiceMonitor created when enabled
5. ✅ E2E tests pass on kind
6. ✅ Helm chart installable

### Phase 2 Complete ✅

1. ✅ Cluster mode deploys configurable shards with replicas
2. ✅ Automatic slot assignment (16384 slots)
3. ✅ Cluster recovery from node failures
4. ✅ Topology stored in CR status (no PVC dependency)
5. ✅ Chaos testing validates resilience

---

## Milestones

```
Pre-MVP ✅
├── Project setup (kubebuilder init)
├── CR definition (types.go)
├── Standalone controller
├── Metrics sidecar
├── Basic tests
└── Documentation

MVP ✅
├── Sentinel controller
├── Failover handling
├── Update strategies
├── ServiceMonitor support
├── Helm chart
├── E2E tests
└── Release automation

Phase 2 ✅
├── Cluster controller
├── Slot management
├── Quorum recovery
├── Partition healing
├── Ghost node removal
└── Chaos testing
```
