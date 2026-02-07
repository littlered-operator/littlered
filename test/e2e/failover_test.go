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

			By("Step 3: Wait for new master label and status update")
			start := time.Now()
			Eventually(func(g Gomega) {
				// 1. Check K8s labels
				cmd := exec.Command("kubectl", "get", "pods", "-n", testNamespace, "-l", "littlered.tanne3.de/role=master", "-o", "jsonpath={.items[*].metadata.name}")
				labelsOut, _ := utils.Run(cmd)
				
				// 2. Check CR status master field (the new -o wide info)
				cmd = exec.Command("kubectl", "get", "littlered", crName, "-n", testNamespace, "-o", "jsonpath={.status.master.podName}")
				statusOut, _ := utils.Run(cmd)
				statusOut = strings.TrimSpace(statusOut)

				if strings.Contains(labelsOut, crName+"-redis") && !strings.Contains(labelsOut, initialMaster) &&
					statusOut != "" && statusOut != initialMaster {
					return
				}
				g.Expect(false).To(BeTrue(), "New master label or status not yet applied")
			}, 45*time.Second, 1*time.Second).Should(Succeed(), "Operator failed to update master quickly enough via event")

			duration := time.Since(start)
			fmt.Fprintf(GinkgoWriter, "Event-driven failover took: %v\n", duration)
			Expect(duration).To(BeNumerically("<", 15*time.Second), "Event-driven failover was too slow (likely fell back to other mechanisms)")

			By("Step 4: Verify Operator logs show event reception")
			Eventually(func(g Gomega) {
				since := startTime.Format(time.RFC3339Nano)
				cmd = exec.Command("sh", "-c", fmt.Sprintf("kubectl logs -n littlered-system -l control-plane=controller-manager --since-time=%s | grep %s", since, crName))
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

			By("Step 3: Verify recovery")
			start := time.Now()
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "pods", "-n", testNamespace, "-l", "littlered.tanne3.de/role=master", "-o", "jsonpath={.items[*].metadata.name}")
				out, _ := utils.Run(cmd)
				if !strings.Contains(out, initialMaster) && strings.Contains(out, crName) {
					return
				}
				g.Expect(false).To(BeTrue(), "New master label not yet applied via polling")
			}, 60*time.Second, 2*time.Second).Should(Succeed())
			
			duration := time.Since(start)
			fmt.Fprintf(GinkgoWriter, "Polling-based failover took: %v\n", duration)
			
			By("Step 4: Verify logs show event monitoring was disabled")
			Eventually(func(g Gomega) {
				since := startTime.Format(time.RFC3339Nano)
				cmd = exec.Command("sh", "-c", fmt.Sprintf("kubectl logs -n littlered-system -l control-plane=controller-manager --since-time=%s | grep %s", since, crName))
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

			By("Step 3: Verify fast recovery (Event speed)")
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "pods", "-n", testNamespace, "-l", "littlered.tanne3.de/role=master", "-o", "jsonpath={.items[*].metadata.name}")
				out, _ := utils.Run(cmd)
				if !strings.Contains(out, initialMaster) && strings.Contains(out, crName) {
					return
				}
				g.Expect(false).To(BeTrue(), "New master label not yet applied")
			}, 20*time.Second, 1*time.Second).Should(Succeed())
		})
	})
})