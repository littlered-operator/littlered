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
	"os/exec"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/littlered-operator/littlered-operator/test/utils"
)

// These tests exercise Rule L (leaderless bootstrap-deadlock recovery). The deadlock
// is reproduced by deleting pods: Sentinel /data is an EmptyDir, so a restarted
// Sentinel comes back BARE (no monitored master), and a restarted Redis pod parks
// in the startup wait-loop — exactly the "all Sentinels bare, no reachable master,
// bootstrapRequired already cleared" state the operator previously could not escape.
var _ = Describe("Sentinel Leaderless Bootstrap Deadlock Recovery", func() {

	deploySentinel := func(crName string, allowUnsafe bool) {
		AddReportEntry("cr:" + crName)
		cr := fmt.Sprintf(`
apiVersion: redis.chuck-chuck-chuck.net/v1alpha1
kind: LittleRed
metadata:
  name: %s
  namespace: %s
spec:
  mode: sentinel
  sentinel:
    quorum: 2
    downAfterMilliseconds: 5000
    failoverTimeout: 10000
    allowUnsafeRebootstrapOnDeadlock: %t
`, crName, testNamespace, allowUnsafe)
		cmd := exec.Command("kubectl", "apply", "-f", "-")
		cmd.Stdin = strings.NewReader(cr)
		_, err := utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred())

		By("waiting for cluster to be ready")
		Eventually(func(g Gomega) {
			g.Expect(getPhase(crName)).To(Equal("Running"))
		}, 3*time.Minute, 5*time.Second).Should(Succeed())
		verifySentinelTopologySync(testNamespace, crName, 3, 2)
	}

	cleanup := func(crName string) {
		if debugOnFailure && suiteOrSpecFailed() {
			By("skipping cleanup to allow debugging")
			return
		}
		cmd := exec.Command("kubectl", "delete", "littlered", crName, "-n", testNamespace, "--ignore-not-found")
		_, _ = utils.Run(cmd)
	}

	// --- Tier 1: no data anywhere (all pods mass-restart) ------------
	Context("No-data deadlock", Ordered, func() {
		var crName string
		BeforeAll(func() { crName = fmt.Sprintf("leaderless-nodata-%d", time.Now().Unix()); deploySentinel(crName, false) })
		AfterAll(func() { cleanup(crName) })

		It("self-heals after every pod is recycled (no opt-in required)", func() {
			By("deleting ALL pods of the instance (sentinels return bare, redis wait-loops)")
			_, err := deletePodsWithLabel(testNamespace, "app.kubernetes.io/instance="+crName)
			Expect(err).NotTo(HaveOccurred())

			By("the operator must break the deadlock on its own and return to Running")
			Eventually(func(g Gomega) {
				g.Expect(getPhase(crName)).To(Equal("Running"))
			}, 5*time.Minute, 5*time.Second).Should(Succeed(), "operator did not self-heal the leaderless deadlock")

			verifySentinelTopologySync(testNamespace, crName, 3, 2)
			By("a master must be named again")
			Expect(getMasterPod(crName)).NotTo(BeEmpty())
		})
	})

	// --- Tier 2: a single surviving replica still holds the data ----------
	Context("Single-survivor deadlock", Ordered, func() {
		var crName string
		BeforeAll(func() { crName = fmt.Sprintf("leaderless-survivor-%d", time.Now().Unix()); deploySentinel(crName, false) })
		AfterAll(func() { cleanup(crName) })

		It("promotes the sole data holder and preserves its data (no opt-in required)", func() {
			master := getMasterPod(crName)
			Expect(master).NotTo(BeEmpty())

			By("writing data to the master")
			_, err := redisExec(testNamespace, master, "SET", "survivor-key", "survivor-value")
			Expect(err).NotTo(HaveOccurred())

			replicas := otherRedisPods(crName, master)
			Expect(replicas).To(HaveLen(2))
			survivor, doomedReplica := replicas[0], replicas[1]

			By("waiting for the survivor replica to receive the data")
			Eventually(func(g Gomega) {
				out, _ := redisExec(testNamespace, survivor, "DBSIZE")
				g.Expect(strings.TrimSpace(out)).NotTo(Equal("0"))
			}, 30*time.Second, 2*time.Second).Should(Succeed())

			By("killing the master, one replica, and all sentinels — keeping only the survivor")
			_, _ = deletePodsWithLabel(testNamespace, "app.kubernetes.io/instance="+crName+",app.kubernetes.io/component=sentinel")
			_, _ = deletePod(testNamespace, doomedReplica)
			_, err = deletePod(testNamespace, master)
			Expect(err).NotTo(HaveOccurred())

			By("the operator must promote the survivor and return to Running")
			Eventually(func(g Gomega) {
				g.Expect(getPhase(crName)).To(Equal("Running"))
			}, 5*time.Minute, 5*time.Second).Should(Succeed())

			By("the data must have survived on the new master")
			newMaster := getMasterPod(crName)
			Expect(newMaster).NotTo(BeEmpty())
			Eventually(func(g Gomega) {
				out, _ := redisExec(testNamespace, newMaster, "GET", "survivor-key")
				g.Expect(strings.TrimSpace(out)).To(Equal("survivor-value"))
			}, 1*time.Minute, 3*time.Second).Should(Succeed(), "data was lost — survivor was not promoted correctly")
		})
	})

	// --- Tier 3: two survivors hold data — gate, then opt-in --------------
	Context("Multi-holder deadlock", Ordered, func() {
		var crName string
		BeforeAll(func() { crName = fmt.Sprintf("leaderless-multi-%d", time.Now().Unix()); deploySentinel(crName, false) })
		AfterAll(func() { cleanup(crName) })

		It("refuses while data is present and the opt-in is off, then recovers once enabled", func() {
			master := getMasterPod(crName)
			Expect(master).NotTo(BeEmpty())

			By("writing data (replicated to both replicas)")
			_, err := redisExec(testNamespace, master, "SET", "multi-key", "multi-value")
			Expect(err).NotTo(HaveOccurred())

			By("waiting for BOTH replicas to actually hold the data before we kill the master")
			// Establish the tier's own precondition: the ≥2-holder gate is only meaningful if
			// both survivors genuinely hold keys. (The single-survivor tier does the same.)
			replicas := otherRedisPods(crName, master)
			Expect(replicas).To(HaveLen(2))
			for _, r := range replicas {
				Eventually(func(g Gomega) {
					out, _ := redisExec(testNamespace, r, "DBSIZE")
					g.Expect(strings.TrimSpace(out)).NotTo(Equal("0"))
				}, 30*time.Second, 2*time.Second).Should(Succeed(), "replica %s never received the replicated data", r)
			}

			By("killing the master and all sentinels — both replicas survive with data")
			_, _ = deletePodsWithLabel(testNamespace, "app.kubernetes.io/instance="+crName+",app.kubernetes.io/component=sentinel")
			_, err = deletePod(testNamespace, master)
			Expect(err).NotTo(HaveOccurred())

			By("GATE: the operator must REFUSE (not Running) and flag the condition")
			Eventually(func(g Gomega) {
				reason, _ := getConditionField(crName, "LeaderlessRecovery", "reason")
				g.Expect(reason).To(Equal("RefusedDataPresent"))
			}, 3*time.Minute, 5*time.Second).Should(Succeed(), "operator did not surface the refuse condition")
			Consistently(func(g Gomega) {
				g.Expect(getPhase(crName)).NotTo(Equal("Running"))
			}, 20*time.Second, 5*time.Second).Should(Succeed(), "operator must NOT rebootstrap over data without opt-in")

			By("enabling the opt-in flag")
			cmd := exec.Command("kubectl", "patch", "littlered", crName, "-n", testNamespace, "--type=merge",
				"-p", `{"spec":{"sentinel":{"allowUnsafeRebootstrapOnDeadlock":true}}}`)
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())

			By("the operator must now force-elect a master and return to Running")
			Eventually(func(g Gomega) {
				g.Expect(getPhase(crName)).To(Equal("Running"))
			}, 5*time.Minute, 5*time.Second).Should(Succeed(), "operator did not recover after opt-in")

			By("the elected master retains its data")
			newMaster := getMasterPod(crName)
			Expect(newMaster).NotTo(BeEmpty())
			out, _ := redisExec(testNamespace, newMaster, "GET", "multi-key")
			Expect(strings.TrimSpace(out)).To(Equal("multi-value"))
		})
	})
})

// --- helpers --------------------------------------------------------------

func getPhase(crName string) string {
	out, _ := utils.Run(exec.Command("kubectl", "get", "littlered", crName, "-n", testNamespace, "-o", "jsonpath={.status.phase}"))
	return strings.TrimSpace(out)
}

func getMasterPod(crName string) string {
	out, _ := utils.Run(exec.Command("kubectl", "get", "littlered", crName, "-n", testNamespace, "-o", "jsonpath={.status.master.podName}"))
	return strings.TrimSpace(out)
}

// getConditionField returns a field (e.g. "reason", "status") of a named status
// condition via jsonpath filtering.
func getConditionField(crName, condType, field string) (string, error) {
	jp := fmt.Sprintf("jsonpath={.status.conditions[?(@.type==%q)].%s}", condType, field)
	out, err := utils.Run(exec.Command("kubectl", "get", "littlered", crName, "-n", testNamespace, "-o", jp))
	return strings.TrimSpace(out), err
}

func redisExec(namespace, pod string, args ...string) (string, error) {
	full := append([]string{"exec", pod, "-n", namespace, "-c", "redis", "--", "redis-cli"}, args...)
	return utils.Run(exec.Command("kubectl", full...))
}

// otherRedisPods returns the instance's redis pods except the given one.
func otherRedisPods(crName, exclude string) []string {
	out, _ := utils.Run(exec.Command("kubectl", "get", "pods", "-n", testNamespace,
		"-l", "app.kubernetes.io/instance="+crName+",app.kubernetes.io/component=redis",
		"-o", "jsonpath={.items[*].metadata.name}"))
	var others []string
	for _, p := range strings.Fields(out) {
		if p != exclude {
			others = append(others, p)
		}
	}
	return others
}
