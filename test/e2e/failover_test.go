//go:build e2e
// +build e2e

/*
Copyright 2026.

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

	"github.com/tanne3/littlered-operator/test/utils"
)

var _ = Describe("Sentinel Advanced Failover", func() {
	const testNamespace = "littlered-failover-test"

	BeforeEach(func() {
		By("creating test namespace")
		cmd := exec.Command("kubectl", "create", "ns", testNamespace)
		_, _ = utils.Run(cmd)
	})

	AfterEach(func() {
		By("cleaning up test namespace")
		cmd := exec.Command("kubectl", "delete", "ns", testNamespace, "--ignore-not-found")
		_, _ = utils.Run(cmd)
	})

	Context("Event-Driven Label Updates", Ordered, func() {
		var crName string

		BeforeAll(func() {
			crName = fmt.Sprintf("event-driven-%d", time.Now().Unix())
			By(fmt.Sprintf("deploying cluster %s with polling disabled", crName))
			cr := fmt.Sprintf(`
apiVersion: littlered.tanne3.de/v1alpha1
kind: LittleRed
metadata:
  name: %s
  namespace: %s
  annotations:
    littlered.tanne3.de/disable-polling: "true"
spec:
  mode: sentinel
  sentinel:
    quorum: 2
    downAfterMilliseconds: 5000
    failoverTimeout: 10000
`, crName, testNamespace)
			cmd := exec.Command("kubectl", "apply", "-f", "-")
			cmd.Stdin = strings.NewReader(cr)
			_, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())

			By("waiting for cluster to be ready")
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "littlered", crName,
					"-n", testNamespace, "-o", "jsonpath={.status.phase}")
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).To(Equal("Running"))
			}, 3*time.Minute, 5*time.Second).Should(Succeed())
		})

		It("should update master role label immediately via Sentinel event", func() {
			startTime := time.Now().Add(-5 * time.Second)
			By("Step 1: Identify initial master")
			cmd := exec.Command("kubectl", "get", "littlered", crName,
				"-n", testNamespace, "-o", "jsonpath={.status.master.podName}")
			initialMaster, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())
			initialMaster = strings.TrimSpace(initialMaster)

			By("Step 2: Kill the Master")
			cmd = exec.Command("kubectl", "delete", "pod", initialMaster,
				"-n", testNamespace, "--grace-period=0", "--force")
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())

			By("Step 3: Wait for new master label")
			start := time.Now()
			Eventually(func(g Gomega) {
				// We check the K8s label directly to see how fast the Operator reacted
				cmd := exec.Command("kubectl", "get", "pods", "-n", testNamespace, "-l", "littlered.tanne3.de/role=master", "-o", "jsonpath={.items[*].metadata.name}")
				out, _ := utils.Run(cmd)

				// Must contain the CR name (belong to this test) and NOT be the old master
				if strings.Contains(out, crName) && !strings.Contains(out, initialMaster) {
					return
				}
				g.Expect(out).To(And(ContainSubstring(crName), Not(ContainSubstring(initialMaster))), 
					fmt.Sprintf("New master label not yet applied. Current masters found: %q", out))
			}, 45*time.Second, 1*time.Second).Should(Succeed(), "Operator failed to update master label")

			duration := time.Since(start)
			fmt.Fprintf(GinkgoWriter, "Event-driven failover took: %v\n", duration)
			Expect(duration).To(BeNumerically("<", 15*time.Second), "Event-driven failover was too slow (likely fell back to other mechanisms)")

			By("Step 4: Verify Operator logs show event reception")
			Eventually(func(g Gomega) {
				since := startTime.Format(time.RFC3339Nano)
				// Use --tail=-1 to ensure we get all logs when using a selector
				cmd = exec.Command("sh", "-c", fmt.Sprintf("kubectl logs -n littlered-system -l control-plane=controller-manager --tail=-1 --since-time=%s | grep %s", since, crName))
				logs, _ := utils.Run(cmd)
				g.Expect(logs).To(ContainSubstring("Triggering reconciliation via Sentinel event"))
				g.Expect(logs).To(ContainSubstring("Master switch detected"))
			}, 30*time.Second, 2*time.Second).Should(Succeed())
		})
	})

	Context("Polling-Only Recovery", Ordered, func() {
		var crName string

		BeforeAll(func() {
			crName = fmt.Sprintf("polling-only-%d", time.Now().Unix())
			By(fmt.Sprintf("deploying cluster %s with events disabled", crName))
			cr := fmt.Sprintf(`
apiVersion: littlered.tanne3.de/v1alpha1
kind: LittleRed
metadata:
  name: %s
  namespace: %s
  annotations:
    littlered.tanne3.de/disable-event-monitoring: "true"
spec:
  mode: sentinel
  sentinel:
    quorum: 2
    downAfterMilliseconds: 5000
    failoverTimeout: 10000
`, crName, testNamespace)
			cmd := exec.Command("kubectl", "apply", "-f", "-")
			cmd.Stdin = strings.NewReader(cr)
			utils.Run(cmd)

			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "littlered", crName, "-n", testNamespace, "-o", "jsonpath={.status.phase}")
				out, _ := utils.Run(cmd)
				g.Expect(out).To(Equal("Running"))
			}, 3*time.Minute, 5*time.Second).Should(Succeed())
		})

		It("should eventually update master label via periodic polling", func() {
			startTime := time.Now().Add(-5 * time.Second)
			By("Step 1: Identify initial master")
			cmd := exec.Command("kubectl", "get", "littlered", crName, "-n", testNamespace, "-o", "jsonpath={.status.master.podName}")
			initialMaster, _ := utils.Run(cmd)
			initialMaster = strings.TrimSpace(initialMaster)

			By("Step 2: Kill the Master")
			exec.Command("kubectl", "delete", "pod", initialMaster, "-n", testNamespace, "--grace-period=0", "--force").Run()

			By("Step 3: Wait for new master label")
			start := time.Now()
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "pods", "-n", testNamespace, "-l", "littlered.tanne3.de/role=master", "-o", "jsonpath={.items[*].metadata.name}")
				out, _ := utils.Run(cmd)
				if strings.Contains(out, crName) && !strings.Contains(out, initialMaster) {
					return
				}
				g.Expect(out).To(And(ContainSubstring(crName), Not(ContainSubstring(initialMaster))), 
					fmt.Sprintf("New master label not yet applied. Current masters found: %q", out))
			}, 60*time.Second, 2*time.Second).Should(Succeed(), "Operator failed to update master label")

			duration := time.Since(start)
			fmt.Fprintf(GinkgoWriter, "Polling-based failover took: %v\n", duration)
			By("Step 4: Verify logs show event monitoring was disabled")
			Eventually(func(g Gomega) {
				since := startTime.Format(time.RFC3339Nano)
				cmd = exec.Command("sh", "-c", fmt.Sprintf("kubectl logs -n littlered-system -l control-plane=controller-manager --tail=-1 --since-time=%s | grep %s", since, crName))
				logs, _ := utils.Run(cmd)
				g.Expect(logs).To(ContainSubstring("Sentinel event monitoring disabled via annotation"))
				g.Expect(logs).NotTo(ContainSubstring("Triggering reconciliation via Sentinel event"))
			}, 30*time.Second, 2*time.Second).Should(Succeed())
		})
	})

	Context("Hybrid (Production) Mode", Ordered, func() {
		var crName string

		BeforeAll(func() {
			crName = fmt.Sprintf("hybrid-%d", time.Now().Unix())
			By(fmt.Sprintf("deploying cluster %s with standard settings", crName))
			cr := fmt.Sprintf(`
apiVersion: littlered.tanne3.de/v1alpha1
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
`, crName, testNamespace)
			cmd := exec.Command("kubectl", "apply", "-f", "-")
			cmd.Stdin = strings.NewReader(cr)
			utils.Run(cmd)

			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "littlered", crName, "-n", testNamespace, "-o", "jsonpath={.status.phase}")
				out, _ := utils.Run(cmd)
				g.Expect(out).To(Equal("Running"))
			}, 3*time.Minute, 5*time.Second).Should(Succeed())
		})

		It("should recover correctly with both mechanisms active", func() {
			By("Step 1: Identify initial master")
			cmd := exec.Command("kubectl", "get", "littlered", crName, "-n", testNamespace, "-o", "jsonpath={.status.master.podName}")
			initialMaster, _ := utils.Run(cmd)
			initialMaster = strings.TrimSpace(initialMaster)

			By("Step 2: Kill the Master")
			exec.Command("kubectl", "delete", "pod", initialMaster, "-n", testNamespace, "--grace-period=0", "--force").Run()

			By("Step 3: Wait for new master label")
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "pods", "-n", testNamespace, "-l", "littlered.tanne3.de/role=master", "-o", "jsonpath={.items[*].metadata.name}")
				out, _ := utils.Run(cmd)
				if strings.Contains(out, crName) && !strings.Contains(out, initialMaster) {
					return
				}
				g.Expect(out).To(And(ContainSubstring(crName), Not(ContainSubstring(initialMaster))), 
					fmt.Sprintf("New master label not yet applied. Current masters found: %q", out))
			}, 20*time.Second, 1*time.Second).Should(Succeed(), "Operator failed to update master label")
		})
	})

	Context("Sentinel Pod Resilience", Ordered, func() {
		var crName string

		BeforeAll(func() {
			crName = fmt.Sprintf("sentinel-death-%d", time.Now().Unix())
			By(fmt.Sprintf("deploying cluster %s for sentinel death testing", crName))
			cr := fmt.Sprintf(`
apiVersion: littlered.tanne3.de/v1alpha1
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
`, crName, testNamespace)
			cmd := exec.Command("kubectl", "apply", "-f", "-")
			cmd.Stdin = strings.NewReader(cr)
			utils.Run(cmd)

			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "littlered", crName, "-n", testNamespace, "-o", "jsonpath={.status.phase}")
				out, _ := utils.Run(cmd)
				g.Expect(out).To(Equal("Running"))
			}, 3*time.Minute, 5*time.Second).Should(Succeed())
		})

		It("should still perform failover after a sentinel pod is restarted", func() {
			By("Step 1: Identify initial master and a sentinel pod")
			cmd := exec.Command("kubectl", "get", "littlered", crName, "-n", testNamespace, "-o", "jsonpath={.status.master.podName}")
			initialMaster, _ := utils.Run(cmd)
			initialMaster = strings.TrimSpace(initialMaster)

			sentinelPod := fmt.Sprintf("%s-sentinel-0", crName)

			By("Step 2: Kill the Sentinel pod")
			// This tests if the Operator's background monitor handles connection loss and reconnects
			exec.Command("kubectl", "delete", "pod", sentinelPod, "-n", testNamespace, "--grace-period=0", "--force").Run()

			By("Step 3: Wait for sentinel pod to be recreated and ready")
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "pod", sentinelPod, "-n", testNamespace, "-o", "jsonpath={.status.phase}")
				out, _ := utils.Run(cmd)
				g.Expect(out).To(Equal("Running"))
			}, 2*time.Minute, 5*time.Second).Should(Succeed())

			By("Step 4: Kill the Redis Master")
			exec.Command("kubectl", "delete", "pod", initialMaster, "-n", testNamespace, "--grace-period=0", "--force").Run()

			By("Step 5: Verify failover still happens (proving monitor re-subscribed)")
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "pods", "-n", testNamespace, "-l", "littlered.tanne3.de/role=master", "-o", "jsonpath={.items[*].metadata.name}")
				out, _ := utils.Run(cmd)
				if strings.Contains(out, crName) && !strings.Contains(out, initialMaster) {
					return
				}
				g.Expect(out).To(And(ContainSubstring(crName), Not(ContainSubstring(initialMaster))), 
					fmt.Sprintf("Failover failed after sentinel restart. Current masters: %q", out))
			}, 45*time.Second, 1*time.Second).Should(Succeed(), "Operator failed to update master label after sentinel restart")
		})
	})
})
