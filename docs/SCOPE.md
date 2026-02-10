# LittleRed - Scope Definition

> Quick reference for what's in and out of scope.

**Last Updated**: 2026-01-30

---

## Project Identity

| Attribute | Value |
|-----------|-------|
| Product Name | LittleRed |
| Technical Name | `littlered` |
| API Group | `littlered.chuck-chuck-chuck.net` |
| API Version | `v1alpha1` (initial) |
| Default Image | `docker.io/valkey/valkey:7.4` |
| Image Format | `{registry}/{path}:{tag}` (each component overridable) |

---

## In Scope

### Pre-MVP (Standalone)

- [ ] Single Redis/Valkey instance (StatefulSet, 1 replica)
- [ ] Redis 7.2+ / Valkey 7.2+ support
- [ ] In-memory only (no persistence)
- [ ] Opinionated defaults (cache-optimized)
- [ ] Full redis.conf override capability
- [ ] Optional password authentication
- [ ] Optional TLS
- [ ] Prometheus metrics (redis_exporter sidecar)
- [ ] Configurable resources (CPU/memory)
- [ ] Guaranteed QoS by default
- [ ] CR status reporting

### MVP (Sentinel)

- [ ] Sentinel mode: 1 master + 2 replicas + 3 sentinels
- [ ] Automatic failover
- [ ] Rolling update strategy
- [ ] Recreate update strategy
- [ ] Optional ServiceMonitor creation
- [ ] Master/replica/sentinel services

### Infrastructure

- [ ] Go implementation (kubebuilder)
- [ ] Kubernetes 1.28+ support
- [ ] Helm chart distribution
- [ ] Configurable namespace/cluster scope
- [ ] Unit tests
- [ ] Integration tests (envtest)
- [ ] E2E tests (kind)

---

## Out of Scope (Initial Release)

### Explicitly Excluded

| Feature | Reason | Future? |
|---------|--------|---------|
| Redis Cluster (sharding) | Complexity, different architecture | Yes, possibly separate operator |
| Persistence (RDB/AOF) | Not needed for cache use case | Maybe |
| Redis < 7.2 | Legacy, not required | No |
| Redis 8.x | Too new, evaluate later | Yes |
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

Design decisions that must support future extensibility:

1. **Sentinel topology**: Start with fixed 1+2+3, but don't hardcode in a way that prevents configurable counts later
2. **Redis Cluster**: Keep reconciliation logic modular enough that Cluster support could be added (or extracted to separate operator)
3. **Persistence**:
   - **Default**: Actively disable persistence (`save ""`, `appendonly no`) to guarantee pure in-memory cache behavior
   - **No volumes**: Default deployment requires no PVCs
   - **Expert override**: User can re-enable persistence via `spec.config.raw` if they really want it (their responsibility)
   - **Future**: Operator-managed persistence (with proper PVC handling) could be added as opt-in feature later

---

## Success Criteria

### Pre-MVP Complete When

1. Can deploy standalone Redis via CR
2. Can deploy standalone Valkey via CR
3. SET/GET operations work
4. Prometheus metrics accessible
5. Pod deletion triggers recreation
6. All unit and integration tests pass

### MVP Complete When

1. Sentinel mode deploys all 6 pods
2. Automatic failover works (kill master → new master elected)
3. Rolling updates maintain availability
4. ServiceMonitor created when enabled
5. E2E tests pass on kind
6. Helm chart installable

---

## Milestones

```
Pre-MVP
├── Project setup (kubebuilder init)
├── CR definition (types.go)
├── Standalone controller
├── Metrics sidecar
├── Basic tests
└── Documentation

MVP
├── Sentinel controller
├── Failover handling
├── Update strategies
├── ServiceMonitor support
├── Helm chart
├── E2E tests
└── Release automation
```
