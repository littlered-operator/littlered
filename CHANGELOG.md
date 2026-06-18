# Changelog

All notable changes to this project are documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

Record changes under `[Unreleased]`; see [RELEASING.md](RELEASING.md) for how to
cut a release (`scripts/prepare-release.sh`).

## [Unreleased]

Everything below has landed since `v0.2.1`.

### Added

- **Namespace-scoped operation** (ADR-014). The operator can now be restricted to
  a subset of namespaces instead of always reconciling every `LittleRed` CR in the
  cluster. Two mutually-exclusive, opt-in modes:
  - `WATCH_NAMESPACE` — allow-list. The manager cache watches only those
    namespaces, and the Helm chart renders a per-namespace manager `Role` +
    `RoleBinding` instead of a `ClusterRole` (least privilege).
  - `IGNORE_NAMESPACE` — deny-list. Watches everything except those namespaces
    (cache field selector); keeps the `ClusterRole`.

  Both unset means cluster-scoped, exactly as before; setting both is a fatal
  startup error. The leader-election lease ID is derived from the effective scope,
  so operators with disjoint scopes never contend for the same lease. Configure via
  the chart values `scope.watchNamespaces` / `scope.ignoreNamespaces`. The CRD stays
  cluster-installed. See the *Namespace Scoping* section in
  [`docs/USAGE.md`](docs/USAGE.md).

- **Cluster mode: self-healing of the consolidated-shard deadlock** (LR-018,
  ADR-006). An instance where one master owned two or more shard ranges while other
  masters sat slotless could stay in `phase: Initializing` indefinitely — no repair
  step could act. The operator now detects this (Step 3b) and relocates the surplus
  range onto an empty master, **preserving keys unconditionally**:
  - Redis 8.4+ (detected at gather time from `CLUSTER INFO`, nothing persisted):
    native atomic slot migration (`CLUSTER MIGRATION IMPORT`).
  - Older engines: the classic `IMPORTING`/`MIGRATING` → `MIGRATE` → `SETSLOT NODE`
    dance, made incremental across reconciles so it never blocks the reconcile
    worker, resuming from the cluster's own on-node markers (no operator state).

  The root cause is also closed: a missing shard range may now only be assigned to
  a reachable *empty* master, so recovery can never pile a second range onto a
  master that already owns one. New advanced tunables under `spec.cluster`:
  `reshardKeyBatchSize` (128), `reshardMaxKeysPerReconcile` (2000),
  `reshardMigrateTimeoutMillis` (5000) — ignored on the native 8.4+ path.

- **Sentinel mode: recovery from a leaderless bootstrap deadlock** (LR-015,
  ADR-005). After a mass pod restart, an already-initialized instance whose entire
  Sentinel quorum lost its master config was unrecoverable without manual
  `SENTINEL MONITOR` or a CR delete + redeploy: with no master, every healing rule
  short-circuited. The new Rule L fires only on that exact signature (all reachable
  Sentinels bare, reachable quorum, no disruption in flight, state persisted past a
  30s cooldown) and decides by how many reachable pods still hold keys:
  - **0 holders** → seed `redis-0` as master.
  - **exactly 1 holder** → promote it (the sole surviving copy of the data; nothing
    else can be lost). No opt-in required; reported as `ReseededFromSurvivor`.
  - **2 or more holders** → refuse and wait, because electing one discards the
    others. Set `spec.sentinel.allowUnsafeRebootstrapOnDeadlock: true` to force-elect
    the most complete pod (highest replication offset, keys as tiebreak).

  Observability: new `LeaderlessRecovery` status condition, `status.leaderlessSince`,
  Kubernetes Events (the operator now needs — and the chart grants — `events` RBAC),
  and per-pod key counts in `lrctl verify` / `lrctl status`.

- **Metrics for Sentinel pods and replicas** (#6). The `redis_exporter` sidecar was
  missing from the Sentinel pods, so they emitted no metrics at all. They now carry
  a sidecar pointed at port 26379 (`redis_sentinel_*` series), and the Sentinel
  headless service exposes the metrics port with the `prometheus.io` annotations.
  Additionally, in sentinel mode the metrics port moved from the role-scoped master
  service to the all-pods replicas headless service, so master *and* replicas are
  each scraped exactly once (previously only the master was scraped). The existing
  `ServiceMonitor` picks both services up automatically.

- **Admission-time rejection of mode-mismatched specs** (#61). CEL
  `x-kubernetes-validations` on `LittleRedSpec` now make the apiserver reject a CR
  that carries `spec.cluster` when `spec.mode` is not `cluster`, or `spec.sentinel`
  when the mode is not `sentinel`. Previously the mismatched block was silently
  ignored.

- **Scheduling: `topologySpreadConstraints` for the operator Deployment**, as a new
  Helm chart value (rendered only when set), so a multi-replica operator can be
  spread across nodes and zones. `docs/USAGE.md` gained a *Spreading pods across
  nodes and failure domains* section covering the `spec.podTemplate` passthrough for
  managed instances.

- **Release tooling.** `scripts/prepare-release.sh` promotes `[Unreleased]` to a
  versioned section and bumps the Helm chart version; [`RELEASING.md`](RELEASING.md)
  documents the process; the publish workflow now generates the GitHub Release notes
  from the matching changelog section instead of a hardcoded body, and verifies the
  chart version matches the tag before publishing.

- **Licensing artifacts.** Apache-2.0 `LICENSE`, `AUTHORS`, `NOTICE` with upstream
  attributions, a generated `THIRD_PARTY_LICENSES` inventory with a `make licenses`
  target, and the collective `Copyright <year> The littlered Authors.` header across
  all Go sources.

- **E2E test selection.** Heavy or opt-in specs carry a shared Ginkgo `extended`
  label: `make test-e2e` runs everything except those, `make test-e2e-all` (or
  `E2E_ALL=true`) runs the lot, and `LABEL_FILTER` accepts any Ginkgo label
  expression. New coverage for reshard recovery (native and pre-8.4 paths) and for
  leaderless recovery.

### Changed

- Leader election is now always enabled and is no longer configurable. The
  operator manager hardcodes `LeaderElection: true`, the `--leader-elect`
  command-line flag has been removed, and the Helm chart's
  `leaderElection.enabled` value has been removed. This guarantees that only
  one controller manager reconciles at a time regardless of how the deployment
  is scaled (via Helm or directly with `kubectl`/`k9s`), preventing concurrent
  reconcilers from racing over Sentinel master/failover state. The
  leader-election RBAC (Role/RoleBinding) is now rendered unconditionally.

  **Upgrade note:** installs that previously set `leaderElection.enabled: false`
  will have leader election forced on at the next `helm upgrade`. This is the
  intended hardening and requires no action, but the now-unknown
  `leaderElection.enabled` value should be removed from custom `values.yaml`
  files to avoid confusion.

- **BREAKING: PodDisruptionBudgets for managed instances are created by default**
  (#69). `spec.podDisruptionBudget.create` is now a `*bool` with a CRD-level
  `default: true`, so the default lives in the OpenAPI schema and is materialized on
  the object at admission. Opt out per-CR with `create: false`. The
  `--pdb-create-default` operator flag and its chart wiring are **removed** — with a
  CRD default they were redundant. The chart's top-level `podDisruptionBudget` values
  block now governs only the operator Deployment's own PDB.

  **Upgrade note:** the new CRD must be applied for the new default to take effect.
  Upgrading only the operator image against an old CRD is safe but leaves the feature
  inert for CRs that omit `create`. `helm upgrade` does not upgrade CRDs in the
  chart's `crds/` directory, so Helm users must apply the CRD manually.

- Default `redis_exporter` sidecar image is now **v1.88.0**. The version has a single
  Dependabot-tracked source of truth (`api/v1alpha1/redis-exporter.Dockerfile`,
  `go:embed`-ed and parsed) instead of being hardcoded across Go constants, the
  kubebuilder default marker and the generated CRD, with a drift guard test that
  fails CI if a bump is not mirrored into the marker + `make manifests`.

- **Documentation now matches the project's actual resource conventions.** Example
  manifests no longer set CPU *limits* (they contradicted both the operator's
  convention and production practice), and the rationale was rewritten: Redis's CPU
  use is bounded by its thread budget (main thread + `io-threads`), so the CPU
  *request* should be sized to that budget while a CPU *limit* can only throttle
  Redis into rising latency and cascading timeouts. Claims of "Guaranteed QoS by
  default" corrected to Burstable (memory limit = request, no CPU limit). Also adds
  PodDisruptionBudget coverage (`docs/USAGE.md` example, `docs/API_SPEC.md` field
  reference, production samples) and stops presenting `sentinel.quorum: 2` as
  something to set — it is the default.

- **Makefile targets follow kubebuilder conventions.** `install`/`uninstall` now
  mean apply/delete the CRDs (the old lrctl-installing `install` is
  `install-lrctl`); canonical `docker-build`/`docker-push` driven by a new `IMG`
  variable, which `deploy` also honors; `img-buildx` renamed `docker-buildx`; default
  `CONTAINER_TOOL` is `docker` (override via env). The Helm `deploy`/`undeploy` path
  and the two-image e2e build set are kept as deliberate divergences. Makefile tool
  versions (controller-gen, golangci-lint, go-licenses) are now derived from `go.mod`
  `tool` directives so Dependabot can see them.

- Helm chart: namespaced resources now carry an explicit `namespace` in their
  templates, and the repository URL in `Chart.yaml` was corrected. Manager RBAC rules
  are generated into a single shared template helper so the `ClusterRole` and the
  namespaced `Role` cannot drift.

- Dependency updates: Kubernetes libraries 0.36.2 with controller-runtime 0.24.1,
  `go-redis` 9.22.0, prometheus-operator apis 0.93.0, Ginkgo 2.32.0 / Gomega 1.42.1,
  `logr` 1.4.4, Go 1.26.5 and Alpine 3.24.1 base images, plus `golang.org/x/text`
  0.39.0 and `google.golang.org/grpc` 1.82.1 to pick up security fixes.

- Internal maintenance: migrated off deprecated APIs (`client.Apply` (#32),
  controller-runtime's `scheme.Builder`), adopted Go 1.26's `new(expr)` builtin in
  place of pointer helpers, and cleaned up lint findings across the operator, `lrctl`
  and tests. No behavior change.

### Fixed

- **Sentinel mode: a reconcile could stall ~146s on dead pod IPs** (LR-017), on
  clouds where a killed pod's IP blackholes rather than refusing connections. The
  stall froze status at a stale value and starved the leaderless-recovery rule, which
  surfaced as apparent data loss in recovery testing. The sentinel gather now probes
  all Redis and Sentinel pods concurrently (as cluster mode already did), and every
  Sentinel read-path address loop plus the gatherer's Redis probe is bounded by a
  per-address `ProbeTimeout` (3s), so a dead address fails in ≤3s regardless of
  client retries. This is the sentinel-mode completion of LR-012.

- **Sentinel mode: the Redis liveness probe could wipe the last surviving copy of
  the data** (LR-016). The probe restarted any replica whose master was unreachable,
  intending to self-heal replicas following a ghost master — but a pod's local `INFO`
  cannot distinguish that case from a leaderless survivor, and since storage is
  EmptyDir the restart destroyed exactly the data leaderless recovery exists to
  preserve. The probe is now a plain local health check (bootstrap guard + local
  `PING`), matching standalone and cluster mode; topology repair stays
  operator-owned (`SLAVEOF` redirect without a restart). Readiness is unchanged and
  still gated on `link:up`, so a masterless replica is pulled from traffic without
  being killed.

- **Sentinel mode: ghost-replica `SENTINEL RESET` could deadlock a failover**
  (LR-013). After a force-deleted master, a broadcast `RESET` wiped Sentinel's
  replica list, which can only be rebuilt from the (now permanently dead) master's
  `INFO` — leaving Sentinel monitoring a dead IP with no known replicas and aborting
  failover indefinitely. The destructive ghost-replica `RESET` is now additionally
  gated on cluster wholeness (every expected Redis pod reachable, computed from
  already-gathered ground truth); when not whole the operator defers, since a stale
  ghost entry is harmless and is pruned on a later reconcile. Ghost-master
  correction, divergent-master correction and replica rescue still run during
  disruption.

- **Cluster mode: a restarted pod returning as an empty master could take minutes to
  reattach** (LR-014). Health was computed from slot-owning masters only, so an
  empty master still read "healthy": the operator dropped to the 30s steady cadence
  right when it needed the 2s healing cadence, and transient `ERR Unknown node`
  failures compounded. Empty masters now count as unhealthy, and `CLUSTER REPLICATE`
  is deferred until the empty master actually knows the target's node ID (using
  adjacency data already gathered — no extra round-trips).

- **Cluster mode: one stale pod IP could stall the whole reconcile loop** (LR-012).
  Ground-truth gathering dialed every pod IP serially with a 5s dial timeout and
  client-side retries, so each dead IP blocked ~25s and rapid pod churn starved the
  loop of the iterations needed to forget ghosts, re-`MEET` survivors and reassign
  replicas. Gathering now fans out concurrently with a hard 3s per-probe deadline,
  and `CLUSTER FORGET` skips unreachable nodes.

- **Cluster mode: `CLUSTER NODES` parsing counted migration markers as owned slots**
  (LR-018). The `[slot->-id]` / `[slot-<-id]` notations that appear mid-migration
  were parsed as slots, which made a range unparseable on the source and made an
  importing destination look like a slot-owning master — enough for the operator to
  declare the cluster healthy and abandon a reshard halfway, stranding keys on a node
  that did not own the slots. Latent until the reshard dance became the first code
  path to mark slots.

- **No PodDisruptionBudget is created for single-pod workloads** (#92). A PDB over a
  single pod can only ever block node drains and never protect availability. This
  now applies to standalone instances, cluster instances with
  `replicasPerShard: 0`, and — in the chart — the operator's own Deployment while it
  runs a single replica. Reconciliation also cleans up a PDB left behind by an
  earlier default-on version.

- The operator's own PDB template rendered both `minAvailable` and `maxUnavailable`,
  which is an invalid PDB spec. It now renders `minAvailable` when set (taking
  precedence, matching the operator's own resolution) and otherwise `maxUnavailable`,
  defaulting to 1.

- The Sentinel-monitor StatefulSet silently dropped `priorityClassName` and
  `topologySpreadConstraints` while the other three builders honored them; all four
  now propagate the full `spec.podTemplate` scheduling surface. The production sample's
  topology-spread `labelSelector` was also fixed — it selected on
  `app.kubernetes.io/name`, which is the constant `littlered`, so it matched no pods;
  the per-instance discriminator is `app.kubernetes.io/instance`.

- `.gitignore` matched the bare path component `littlered`, so new files under
  `charts/littlered/` and `cmd/littlered/` were silently ignored (existing files
  stayed tracked only because they predated the pattern). The pattern is now anchored
  to the repository root build binary.
