# ADR-014: Namespace-Scoped Operator

## Status
Proposed (targets a new **0.2.3** and the **0.3.x** line; developed on their fork point `b60cb0a` so
it merges cleanly into both). Prerequisite for safely running more than one littlered operator on a
cluster — and specifically the unblock for the ADR-013 migration e2e (which must deploy a pre-0.3
operator without trampling unrelated instances). Formalizes `docs/E2E_SOAK_HARNESS_DESIGN.md` Path B
into a first-class product feature. Default behavior is unchanged (cluster-scoped); scoping is opt-in.

> ADR number: 014, not 010–013 (claimed on sibling branches: 010 ghost-replica prune, 011 failover,
> 012 multi-site, 013 legacy→per-shard migration).

## Context

The operator is **cluster-scoped today**. `cmd/littlered/main.go` builds the manager with no
`WATCH_NAMESPACE` / `cache.Options.DefaultNamespaces` (no namespace field of any kind); RBAC ships as
`ClusterRole` + `ClusterRoleBinding`; leader election (off by default) uses a single fixed lease ID
`64adfe7c.chuck-chuck-chuck.net`. So **any** deployed operator watches and reconciles **every**
`LittleRed` CR in every namespace, and two operators on one cluster (a) double-reconcile each other's
CRs and (b) — if leader election is on — both acquire the one lease and both act.

This is the root cause of a real incident (2026-08-07, scm-cp). The ADR-013 migration e2e was pointed
at the live multi-site control-plane cluster. Its bootstrap step does `make deploy IMG=<pre-split>`,
installing a **pre-0.3 operator cluster-wide**. Being cluster-scoped, that 0.2-era operator reconciled
the unrelated multi-site instance `ms-smoke` and — because pre-0.3 code still has the single-STS
builder — created a genuine `ms-smoke-cluster` StatefulSet for it. The (also cluster-scoped) migration
operator then correctly detected that single-STS and began migrating it, forking `ms-smoke` into two
clusters. No single link is solely to blame, but **cluster scope is the keystone**: with a scoped
operator, neither operator would have seen `ms-smoke` at all.

Cluster scope is also wrong beyond testing: it blocks multi-tenancy (one operator per team/namespace),
prevents least-privilege deployment (a namespaced Role instead of a cluster-wide grant), and makes
staged rollouts (a new operator version managing one namespace) impossible.

## Decision

### 1. Two mutually-exclusive scoping modes (allow-list / deny-list)
- **Allow-list — `WATCH_NAMESPACE`** (comma-separated): manager built with
  `cache.Options{DefaultNamespaces: {ns: {} …}}`; informers/reconcilers see only those namespaces.
  This is the least-privilege mode (namespaced RBAC — Decision 2). Single namespace is the common
  case; the list supports "manage these N, not all."
- **Deny-list — `IGNORE_NAMESPACE`** (comma-separated): manager built with
  `cache.Options{DefaultFieldSelector: fields.AndSelectors(metadata.namespace != ns …)}`; the operator
  watches **all namespaces except** the listed ones. This is the "one global operator, but hands off
  these" model — e.g. a global operator runs `IGNORE_NAMESPACE=staging` while a new operator version
  runs `WATCH_NAMESPACE=staging`, partitioning the cluster with **zero overlap**. Deny-list inherently
  watches cluster-wide, so it keeps **cluster-wide RBAC** (no least-privilege gain — expected for a
  global operator).
- **Empty / both-unset ⇒ cluster-scoped, exactly as today** — backward-compatible default, no breaking
  change.
- The two modes are **mutually exclusive**; setting both is a **fatal startup error** (fail fast,
  never guess a merge).

Community precedent: the allow-list is the Operator-SDK `WATCH_NAMESPACE` / OLM
OwnNamespace·SingleNamespace·MultiNamespace convention. The deny-list mirrors the Kubernetes
admission-webhook `namespaceSelector` `NotIn` pattern (the canonical "all namespaces except these",
keyed on the auto-applied immutable `kubernetes.io/metadata.name` label); the controller-cache analog
is a `metadata.namespace != …` field selector, which the API server supports for List/Watch on every
namespaced resource.

### 2. Namespaced RBAC when scoped
The Helm chart renders **`Role` + `RoleBinding` in each watched namespace** instead of the
`ClusterRole` + `ClusterRoleBinding` when an **allow-list** (`WATCH_NAMESPACE`) is configured. The
**CRD stays cluster-installed** (CustomResourceDefinitions are always cluster-scoped; only the CR
*instances* are namespaced). This is what actually delivers least-privilege — an allow-list-scoped
operator holds no cluster-wide grant. Unscoped (default) and **deny-list** (`IGNORE_NAMESPACE`) mode
keep the ClusterRole (both watch cluster-wide by construction).

### 3. Leader election scoped per instance
The leader lease must not be shared across independent operators. The lease lives in the operator's own
namespace (`OPERATOR_NAMESPACE` / downward-API `POD_NAMESPACE`), and the lease **ID is derived from the
operator's scope** (namespace + watch set) so two operators with disjoint watch-lists never contend for
one lease. (Today's single fixed ID is safe only for the singleton cluster-scoped case.)

### 4. Ownership direction: the operator declares what it watches
Scoping is expressed as *the operator's watch-list* (which namespaces this operator owns), **not** as a
per-CR "which operator manages me" selector nor a per-namespace opt-in/opt-out annotation. This is the
conventional, race-free direction (controller-runtime cache scoping; OLM OwnNamespace/SingleNamespace
install modes): two operators with disjoint watch-lists are provably non-overlapping, whereas CR-side
or namespace-annotation selection still lets an unconfigured cluster-scoped operator grab everything.
Deployment discipline (disjoint watch-lists) is the contract.

## Consequences
- **No breaking change**: unset `WATCH_NAMESPACE` = today's cluster-scoped singleton.
- Two (or more) operators coexist safely on one cluster iff their watch-lists are disjoint — enabling
  multi-tenancy, least-privilege, staged rollout, and isolated e2e.
- **Unblocks ADR-013 WS2** (the isolated migration e2e) — see the backport note below.
- Formalizes `E2E_SOAK_HARNESS_DESIGN.md` Path B; that doc's per-run env (`OPERATOR_NAMESPACE=…`,
  `WATCH_NAMESPACE=…`) becomes the supported mechanism.

### Ships on the 0.2 line too — the migration-e2e legacy operator is scoped, not just contained
Namespace scoping is **orthogonal to the per-shard split** (it touches only the manager cache options,
the chart RBAC, and the lease ID — no cluster-topology code). The pre-per-shard, single-STS code lives
on the **0.2 line** (`v0.2.1` is the latest release; `release/0.2.2` is unreleased staging, still
single-STS). So the same change cherry-picks/merges cleanly across the 0.2 and 0.3 lines, and we
**ship nsscope on the 0.2 line as well**, as a new **0.2.3** release (the `release/0.2.2` staging
branch stays as-is).

Consequence for ADR-013 WS2: the migration e2e's legacy-seeding operator becomes a **0.2.x build that
is itself namespace-scoped** — so the rogue-old-operator vector (the ms-smoke incident) is eliminated
**at its source**, not merely contained. Kind + the refuse-to-run guardrails (WS2) remain as
defense-in-depth (and seeding the legacy layout without any operator — apply a pre-0.3-shaped STS +
form the cluster via redis-cli — is a further option), but scoping alone now covers the primary risk.

**Implementation base:** develop nsscope so it lands on both release lines — base on the common
pre-split ancestor of `release/0.2.2` and `release/0.3.0` (or land on the 0.2 line and merge forward),
rather than the current `release/0.3.0`-only branch.

## Alternatives considered
- **CR-side operator selector** (à la IngressClass `spec.controllerName`). More granular, but requires
  every CR to opt in and does nothing to stop an unconfigured cluster-scoped operator from grabbing
  unowned CRs; also no RBAC least-privilege benefit. Rejected.
- **Per-namespace opt-in/opt-out annotation** ("this namespace may be managed by a global operator").
  Non-standard, still cluster-wide RBAC, and races if two operators disagree. Rejected in favor of the
  watch-list.
- **Leave cluster-scoped, rely on e2e discipline only.** Rejected: the incident shows discipline is
  insufficient, and cluster scope blocks legitimate multi-tenant/least-privilege deployment regardless
  of testing.
