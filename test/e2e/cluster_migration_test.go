//go:build e2e
// +build e2e

/*
Copyright 2026 The littlered Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package e2e

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	redisclient "github.com/littlered-operator/littlered-operator/internal/redis"
	"github.com/littlered-operator/littlered-operator/test/utils"
)

// ADR-013 — in-place legacy → per-shard cluster migration, end to end.
//
// The feature: upgrading the 0.3 operator over a pre-0.3 single-StatefulSet cluster
// ({name}-cluster, pods {name}-cluster-N) no longer refuses (ADR-007 §5's terminal
// LegacyClusterTopology) — it drives an online, data-safe, in-cluster migration into
// the 0.3 per-shard layout ({name}-shard-K, pods {name}-shard-K-M) on the SAME running
// Redis Cluster: Standup → Meet → Draining → ReplicasAttached → Decommission → Complete.
//
// The harness's distinctive need is a REAL legacy layout to migrate FROM. The CRD is
// byte-identical pre-0.3 vs 0.3 (ADR-013 Context 1: the split touched only workloads,
// never spec.cluster.shards/replicasPerShard), so the only way to produce the legacy
// {name}-cluster single STS is to run the PRE-SPLIT operator (git ref 85e1a93^, the last
// single-STS commit) against a normal cluster CR. This test therefore deploys TWO operator
// images in sequence:
//
//  1. pre-split image  — bootstrap the legacy single-STS cluster + seed data;
//  2. migration image  — this branch (the git-hash-tagged image the suite already built),
//     redeployed over the same CR with no spec change, to drive the migration.
//
// REQUIRED external capability (escape hatch — see the milestone report): the pre-split
// image must be BUILT and PUSHED out-of-band from git ref 85e1a93^ (a different working
// tree than this branch) and its reference supplied via LEGACY_OPERATOR_IMAGE=<repo>:<tag>.
// Building a different git ref cannot happen from inside a Go test; this harness only
// *deploys* an already-published image (make deploy IMG=...). Absent the env var the spec
// self-skips with an actionable message rather than silently mis-running.
//
// Heavy + opt-in (the two-image redeploy is disruptive): Label("extended","migration").
// It is excluded by the default `!extended` filter; run it explicitly with
// `make test-e2e LABEL_FILTER='migration'` (see the Makefile E2E label convention).
var _ = Describe("Cluster Legacy→Per-Shard In-Place Migration (ADR-013)", Label("extended", "migration"), Ordered, func() {

	expectedNodes := clusterTotalNodes(clusterReplicasPerShard)

	// --- default (opt-out) migration: upgrade drives to Complete, data + topology intact ---
	Context("default migration drives to Complete", Ordered, func() {
		var crName string
		var dataset map[string]string

		BeforeAll(func() {
			crName = fmt.Sprintf("mig-default-%d", time.Now().Unix())
			AddReportEntry("cr:" + crName)

			bootstrapLegacyCluster(crName, expectedNodes)

			By("seeding a known dataset spanning all shards through a legacy seed node")
			dataset = writeDatasetSpanningShards(legacyPod(crName), 50)
			Expect(dataset).To(HaveLen(clusterShards * 50))
		})

		AfterAll(func() {
			restoreMigrationOperator() // leave the suite on the migration image regardless of outcome
			if debugOnFailure && suiteOrSpecFailed() {
				By("skipping cleanup to allow debugging")
				return
			}
			By("cleaning up cluster CR")
			_, _ = utils.Run(exec.Command("kubectl", "delete", "littlered", crName,
				"-n", testNamespace, "--ignore-not-found", "--timeout=2m"))
		})

		It("migrates in place to the per-shard layout, preserving data and topology", func() {
			By("upgrading to the migration-capable operator image (same CR, no spec change)")
			// This is the migration TRIGGER (ADR-013 Consequences): the moment the 0.3
			// migration operator sees the legacy {name}-cluster STS it enters migration mode.
			upgradeToMigrationOperator()

			By("the data plane must stay served through the shared Service while migration runs")
			// Best-effort coexistence probe (ADR-013 §8 / DESIGN §3 "coexistence"): the shared
			// headless Service {name}-cluster selects component=cluster (shard-agnostic), so it
			// fronts legacy AND new pods; a -c client through a legacy node keeps being served
			// across -ASK/-MOVED during Draining. Opportunistic like LR-017's tiers — it asserts
			// no data-plane outage during the early migration window, not an exact phase capture.
			assertSharedServiceCoexistence(crName)
			assertClientStaysServed(crName, dataset)

			By("waiting for the operator to drive migration all the way to Complete")
			waitMigrationComplete(crName)

			By("(a) all seeded keys must survive the migration, byte-for-byte")
			verifyDataset(clusterMasterPod(crName, clusterShards-1), dataset)

			By("(b) cluster_state must be ok with all 16384 slots assigned")
			Eventually(func(g Gomega) {
				out, err := redisExec(testNamespace, clusterMasterPod(crName, 0), "CLUSTER", "INFO")
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(out).To(ContainSubstring("cluster_state:ok"))
				g.Expect(out).To(ContainSubstring("cluster_slots_assigned:16384"))
			}, 2*time.Minute, 5*time.Second).Should(Succeed())

			By("(c) the legacy {name}-cluster STS must be gone and N per-shard STSs must exist")
			expectPerShardLayout(crName)

			By("(d) lrctl-verify colocation must pass (operator status matches Redis, shards colocated)")
			// verifyClusterTopologySync is the suite's colocation/verify assertion (same check
			// lrctl verify performs: status↔ground-truth agreement + the ADR-007/LR-020
			// shard-colocation invariant that each Redis shard lives in ONE StatefulSet).
			verifyClusterTopologySync(testNamespace, crName, expectedNodes)
		})
	})

	// --- hold escape hatch: annotation parks migration (non-mutating) until removed ---
	Context("hold annotation parks migration until removed", Ordered, func() {
		var crName string
		var dataset map[string]string

		BeforeAll(func() {
			crName = fmt.Sprintf("mig-hold-%d", time.Now().Unix())
			AddReportEntry("cr:" + crName)

			bootstrapLegacyCluster(crName, expectedNodes)

			By("seeding a known dataset spanning all shards")
			dataset = writeDatasetSpanningShards(legacyPod(crName), 20)
			Expect(dataset).To(HaveLen(clusterShards * 20))
		})

		AfterAll(func() {
			restoreMigrationOperator()
			if debugOnFailure && suiteOrSpecFailed() {
				By("skipping cleanup to allow debugging")
				return
			}
			By("cleaning up cluster CR")
			_, _ = utils.Run(exec.Command("kubectl", "delete", "littlered", crName,
				"-n", testNamespace, "--ignore-not-found", "--timeout=2m"))
		})

		It("does not mutate while held, then proceeds when the annotation is cleared", func() {
			By("setting the hold annotation BEFORE the upgrade (ADR-013 §3)")
			_, err := utils.Run(exec.Command("kubectl", "annotate", "littlered", crName,
				"-n", testNamespace,
				"redis.chuck-chuck-chuck.net/migrate-legacy-sts=hold", "--overwrite"))
			Expect(err).NotTo(HaveOccurred())

			By("upgrading to the migration-capable operator image")
			upgradeToMigrationOperator()

			By("the operator must report the held state (Ready=False, reason MigrationHeld)")
			Eventually(func(g Gomega) {
				reason, err := getConditionField(crName, "Ready", "reason")
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(reason).To(Equal("MigrationHeld"))
			}, 3*time.Minute, 5*time.Second).Should(Succeed())

			By("status.status must be Migrating while held")
			Eventually(func(g Gomega) {
				out, err := utils.Run(exec.Command("kubectl", "get", "littlered", crName,
					"-n", testNamespace, "-o", "jsonpath={.status.status}"))
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(strings.TrimSpace(out)).To(Equal("Migrating"))
			}, 1*time.Minute, 5*time.Second).Should(Succeed())

			By("while held the operator MUST NOT mutate: legacy STS untouched, no per-shard STS created")
			// The hold branch stamps only status (no ensureMigrationResources), so no {name}-shard-K
			// StatefulSet must appear and the legacy {name}-cluster STS must remain. Consistently
			// over a window catches an operator that would mutate a beat later.
			Consistently(func(g Gomega) {
				g.Expect(stsExists(clusterStatefulSetName(crName))).To(BeTrue(),
					"legacy StatefulSet was removed while migration is held")
				g.Expect(stsExists(fmt.Sprintf("%s-shard-0", crName))).To(BeFalse(),
					"a per-shard StatefulSet was created while migration is held")
			}, 45*time.Second, 5*time.Second).Should(Succeed())

			By("removing the hold annotation — migration must now proceed to Complete")
			_, err = utils.Run(exec.Command("kubectl", "annotate", "littlered", crName,
				"-n", testNamespace,
				"redis.chuck-chuck-chuck.net/migrate-legacy-sts-")) // trailing '-' deletes the annotation
			Expect(err).NotTo(HaveOccurred())

			waitMigrationComplete(crName)

			By("data + topology intact after the resumed migration")
			verifyDataset(clusterMasterPod(crName, clusterShards-1), dataset)
			expectPerShardLayout(crName)
			verifyClusterTopologySync(testNamespace, crName, expectedNodes)
		})
	})
})

// =============================================================================
// Migration-specific helpers
// =============================================================================

// legacyPod returns the pre-0.3 single-STS cluster seed pod name ({crName}-cluster-0).
func legacyPod(crName string) string {
	return fmt.Sprintf("%s-cluster-0", crName)
}

// clusterStatefulSetName returns the legacy single StatefulSet name ({crName}-cluster).
// Mirrors internal/controller resources.clusterStatefulSetName (not importable from the
// e2e package); the two are pinned to the same shape by the layout assertions below.
func clusterStatefulSetName(crName string) string {
	return crName + "-cluster"
}

// preSplitOperatorImage returns the pre-split (git 85e1a93^) operator image reference, or
// self-skips the spec if it was not supplied. Building that ref is out-of-band (a different
// git working tree); this harness only deploys an already-published image.
func preSplitOperatorImage() string {
	ref := os.Getenv("LEGACY_OPERATOR_IMAGE")
	if ref == "" {
		Skip("LEGACY_OPERATOR_IMAGE not set — the ADR-013 migration e2e needs the PRE-SPLIT " +
			"operator image (built + pushed out-of-band from git ref 85e1a93^) to bootstrap a real " +
			"legacy single-STS cluster. Set LEGACY_OPERATOR_IMAGE=<repo>:<tag> to run this tier.")
	}
	return ref
}

// migrationOperatorImage returns this branch's migration-capable operator image reference.
// `make run-test-e2e` exports OPERATOR_IMAGE (= the git-hash-tagged image the suite built
// and pushed in BeforeSuite), so the "upgrade" step redeploys exactly that image.
func migrationOperatorImage() string {
	ref := os.Getenv("OPERATOR_IMAGE")
	if ref == "" {
		Skip("OPERATOR_IMAGE not set — cannot determine the migration operator image to upgrade to " +
			"(normally exported by `make run-test-e2e`).")
	}
	return ref
}

// deployOperatorImage (re)deploys the operator Helm release pinned to a specific image
// reference (repo:tag) via `make deploy IMG=...`, then waits for that image to be rolled
// out and Ready. This is the two-image capability the migration harness needs; it mirrors
// deployOperator() in e2e_suite_test.go but targets an explicit image.
func deployOperatorImage(imageRef string) {
	By("deploying operator image " + imageRef)
	cmd := exec.Command("make", "deploy", "IMG="+imageRef)
	_, err := utils.Run(cmd)
	ExpectWithOffset(1, err).NotTo(HaveOccurred(), "failed to deploy operator image %s", imageRef)
	waitForOperatorImage(imageRef)
}

// waitForOperatorImage waits until the operator Deployment has rolled out the given image
// and is Ready (updatedReplicas==1, availableReplicas==1, observedGeneration current). A
// plain availableReplicas check (waitForOperator) can pass on the OLD pod mid-rollout, which
// would race the upgrade; this confirms the NEW image is actually serving.
func waitForOperatorImage(imageRef string) {
	Eventually(func(g Gomega) {
		image, err := utils.Run(exec.Command("kubectl", "get", "deployment", "littlered",
			"-n", operatorNamespace, "-o", "jsonpath={.spec.template.spec.containers[*].image}"))
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(image).To(ContainSubstring(imageRef), "operator Deployment still on the previous image")

		gen, err := utils.Run(exec.Command("kubectl", "get", "deployment", "littlered",
			"-n", operatorNamespace, "-o", "jsonpath={.metadata.generation}"))
		g.Expect(err).NotTo(HaveOccurred())
		observed, err := utils.Run(exec.Command("kubectl", "get", "deployment", "littlered",
			"-n", operatorNamespace, "-o", "jsonpath={.status.observedGeneration}"))
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(strings.TrimSpace(observed)).To(Equal(strings.TrimSpace(gen)))

		updated, err := utils.Run(exec.Command("kubectl", "get", "deployment", "littlered",
			"-n", operatorNamespace, "-o", "jsonpath={.status.updatedReplicas}"))
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(strings.TrimSpace(updated)).To(Equal("1"))

		avail, err := utils.Run(exec.Command("kubectl", "get", "deployment", "littlered",
			"-n", operatorNamespace, "-o", "jsonpath={.status.availableReplicas}"))
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(strings.TrimSpace(avail)).To(Equal("1"))
	}, 3*time.Minute, 5*time.Second).Should(Succeed(), "operator did not roll out image %s", imageRef)
}

// upgradeToMigrationOperator redeploys this branch's migration-capable operator image.
func upgradeToMigrationOperator() { deployOperatorImage(migrationOperatorImage()) }

// restoreMigrationOperator ensures the suite is left on the migration operator image, so a
// subsequent tier does not inherit the pre-split operator. Idempotent (helm upgrade no-op if
// already there). Mirrors the reshard tier's `scaleOperator(1)` cleanup discipline.
func restoreMigrationOperator() {
	if os.Getenv("OPERATOR_IMAGE") == "" {
		return
	}
	deployOperatorImage(os.Getenv("OPERATOR_IMAGE"))
}

// bootstrapLegacyCluster deploys the PRE-SPLIT operator, creates a shape-preserving cluster
// CR, waits for it Running, and asserts the LEGACY single-STS layout materialized.
func bootstrapLegacyCluster(crName string, expectedNodes int) {
	By("deploying the pre-split operator to seed a real legacy single-STS cluster")
	deployOperatorImage(preSplitOperatorImage())

	By(fmt.Sprintf("creating a %d-shard / %d-replica cluster CR (%d nodes)",
		clusterShards, clusterReplicasPerShard, expectedNodes))
	cr := clusterCR(crName, clusterReplicasPerShard, "", `  resources:
    requests:
      cpu: "100m"
      memory: "128Mi"
    limits:
      cpu: "100m"
      memory: "128Mi"
`)
	cmd := exec.Command("kubectl", "apply", "-f", "-")
	cmd.Stdin = strings.NewReader(cr)
	_, err := utils.Run(cmd)
	Expect(err).NotTo(HaveOccurred())

	By("waiting for the legacy cluster to be Running")
	Eventually(func(g Gomega) {
		g.Expect(getPhase(crName)).To(Equal("Running"))
	}, 6*time.Minute, 5*time.Second).Should(Succeed())

	By("asserting the LEGACY layout exists ({name}-cluster single STS, pods {name}-cluster-N)")
	expectLegacyLayout(crName, expectedNodes)
}

// expectLegacyLayout asserts the pre-0.3 shape: one {crName}-cluster StatefulSet with all
// nodes Ready, legacy pods {crName}-cluster-N present, and NO per-shard {crName}-shard-K STS.
func expectLegacyLayout(crName string, expectedNodes int) {
	Eventually(func(g Gomega) {
		g.Expect(stsExists(clusterStatefulSetName(crName))).To(BeTrue(),
			"legacy single StatefulSet %s not found", clusterStatefulSetName(crName))

		ready, err := utils.Run(exec.Command("kubectl", "get", "statefulset",
			clusterStatefulSetName(crName), "-n", testNamespace,
			"-o", "jsonpath={.status.readyReplicas}"))
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(strings.TrimSpace(ready)).To(Equal(fmt.Sprintf("%d", expectedNodes)))

		// No per-shard STS must exist under the pre-split operator.
		for k := 0; k < clusterShards; k++ {
			g.Expect(stsExists(fmt.Sprintf("%s-shard-%d", crName, k))).To(BeFalse(),
				"unexpected per-shard StatefulSet under the pre-split operator")
		}
	}, 2*time.Minute, 5*time.Second).Should(Succeed())

	By("confirming the legacy pods {name}-cluster-N are present and cluster_state is ok")
	Eventually(func(g Gomega) {
		out, err := redisExec(testNamespace, legacyPod(crName), "CLUSTER", "INFO")
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(out).To(ContainSubstring("cluster_state:ok"))
		g.Expect(out).To(ContainSubstring("cluster_slots_assigned:16384"))
	}, 2*time.Minute, 5*time.Second).Should(Succeed())
}

// expectPerShardLayout asserts the 0.3 end state: the legacy {crName}-cluster STS is GONE,
// N per-shard {crName}-shard-K StatefulSets exist each with 1+replicasPerShard Ready pods,
// and status.cluster.migration has been cleared.
func expectPerShardLayout(crName string) {
	Eventually(func(g Gomega) {
		g.Expect(stsExists(clusterStatefulSetName(crName))).To(BeFalse(),
			"legacy StatefulSet %s should be deleted at Decommission", clusterStatefulSetName(crName))

		for k := 0; k < clusterShards; k++ {
			name := fmt.Sprintf("%s-shard-%d", crName, k)
			g.Expect(stsExists(name)).To(BeTrue(), "per-shard StatefulSet %s missing", name)
			ready, err := utils.Run(exec.Command("kubectl", "get", "statefulset", name,
				"-n", testNamespace, "-o", "jsonpath={.status.readyReplicas}"))
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(strings.TrimSpace(ready)).To(Equal(fmt.Sprintf("%d", 1+clusterReplicasPerShard)))
		}

		// Monitoring surface cleared on Complete (driver sets migration=nil).
		mig, err := utils.Run(exec.Command("kubectl", "get", "littlered", crName,
			"-n", testNamespace, "-o", "jsonpath={.status.cluster.migration.phase}"))
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(strings.TrimSpace(mig)).To(BeEmpty(), "status.cluster.migration should be cleared after Complete")
	}, 3*time.Minute, 5*time.Second).Should(Succeed())
}

// waitMigrationComplete waits until the operator has driven migration to Complete: phase
// back to Running, legacy STS gone, and the migration monitoring status cleared. Generous
// timeout consistent with the reshard/wipe tiers (draining is per-range, one range/reconcile).
func waitMigrationComplete(crName string) {
	Eventually(func(g Gomega) {
		g.Expect(getPhase(crName)).To(Equal("Running"))
		g.Expect(stsExists(clusterStatefulSetName(crName))).To(BeFalse(),
			"legacy StatefulSet still present — migration not yet Complete")
	}, 15*time.Minute, 10*time.Second).Should(Succeed(),
		"operator did not drive the legacy cluster to the per-shard layout")
}

// stsExists reports whether a StatefulSet exists in the test namespace.
func stsExists(name string) bool {
	_, err := utils.Run(exec.Command("kubectl", "get", "statefulset", name,
		"-n", testNamespace, "-o", "name"))
	return err == nil
}

// writeDatasetSpanningShards writes perShard keys into EACH shard's slot range (via a hash
// tag routing into GenerateSlotRanges(shards)[K]) through a -c client on seedPod, and returns
// the key→value map for a later integrity check. Guarantees the dataset genuinely spans all
// shards (not just probabilistically). Reuses findHashTagInSlotRange/keySlot from the reshard
// tier (same package).
func writeDatasetSpanningShards(seedPod string, perShard int) map[string]string {
	data := make(map[string]string, clusterShards*perShard)
	for _, rng := range redisclient.GenerateSlotRanges(clusterShards) {
		tag := findHashTagInSlotRange(seedPod, rng.Start, rng.End)
		for j := 1; j <= perShard; j++ {
			key := fmt.Sprintf("%smig:%d", tag, j)
			val := fmt.Sprintf("v-%s-%d", tag, j)
			_, err := redisExec(testNamespace, seedPod, "-c", "SET", key, val)
			Expect(err).NotTo(HaveOccurred(), "failed to seed key %s", key)
			data[key] = val
		}
	}
	return data
}

// verifyDataset asserts every seeded key still resolves to its exact value, read via a -c
// client through readPod (redirection resolves whichever new master now owns the slot).
func verifyDataset(readPod string, data map[string]string) {
	Eventually(func(g Gomega) {
		for key, want := range data {
			out, err := redisExec(testNamespace, readPod, "-c", "GET", key)
			g.Expect(err).NotTo(HaveOccurred(), "GET %s failed", key)
			g.Expect(strings.TrimSpace(out)).To(Equal(want), "value mismatch for key %s", key)
		}
	}, 3*time.Minute, 10*time.Second).Should(Succeed(), "seeded dataset was not preserved across migration")
}

// assertSharedServiceCoexistence asserts the structural coexistence property (DESIGN §3 /
// §8): the shared headless Service {name}-cluster selects component=cluster (shard-agnostic),
// so it fronts BOTH legacy {name}-cluster-N pods AND new {name}-shard-K-M pods during the
// migration window. This is the deterministic half of the coexistence claim.
func assertSharedServiceCoexistence(crName string) {
	selector, err := utils.Run(exec.Command("kubectl", "get", "service",
		clusterStatefulSetName(crName), "-n", testNamespace,
		"-o", "jsonpath={.spec.selector.app\\.kubernetes\\.io/component}"))
	Expect(err).NotTo(HaveOccurred(), "shared headless Service %s not found", clusterStatefulSetName(crName))
	Expect(strings.TrimSpace(selector)).To(Equal("cluster"),
		"shared Service selector is not shard-agnostic — it would not front both legacy and new pods")
}

// assertClientStaysServed is a best-effort data-plane availability probe (ADR-013 §8): a -c
// client (through a legacy node, which stays up until Decommission) keeps being served across
// -ASK/-MOVED redirection while migration runs. Opportunistic — the migration may complete
// before or during the window; the assertion is "no read ever hard-failed", not an exact
// mid-Draining capture. Reads a stable sentinel-per-shard key from the seeded dataset.
func assertClientStaysServed(crName string, data map[string]string) {
	// Pick one representative key per shard from the dataset for a light, spanning probe.
	var probeKeys []string
	for _, rng := range redisclient.GenerateSlotRanges(clusterShards) {
		tag := findHashTagInSlotRange(legacyPod(crName), rng.Start, rng.End)
		if _, ok := data[fmt.Sprintf("%smig:1", tag)]; ok {
			probeKeys = append(probeKeys, fmt.Sprintf("%smig:1", tag))
		}
	}

	Consistently(func(g Gomega) {
		// Serve reads through whichever legacy pod is still up (0 may be drained/renamed late,
		// but during the early window it is present). A failure here is a genuine data-plane
		// outage, which the shared-Service coexistence is meant to prevent.
		for _, key := range probeKeys {
			out, err := redisExec(testNamespace, legacyPod(crName), "-c", "GET", key)
			g.Expect(err).NotTo(HaveOccurred(), "data-plane read of %s failed during migration", key)
			g.Expect(strings.TrimSpace(out)).To(Equal(data[key]))
		}
	}, 30*time.Second, 3*time.Second).Should(Succeed(),
		"client was not continuously served through the shared Service during migration")
}
