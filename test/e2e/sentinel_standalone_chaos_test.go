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

var _ = Describe("Sentinel and Standalone Chaos Testing", Ordered, func() {

	Context("Sentinel Resilience", Ordered, func() {
		It("should maintain availability during rapid double failover", func() {
			crName := fmt.Sprintf("chaos-sentinel-%d", time.Now().Unix())
			const testDuration = 120 * time.Second

			By(fmt.Sprintf("creating Sentinel cluster %s and chaos client simultaneously", crName))
			cr := fmt.Sprintf(`
apiVersion: chuck-chuck-chuck.net/v1alpha1
kind: LittleRed
metadata:
  name: %s
  namespace: %s
  annotations:
    chuck-chuck-chuck.net/disable-polling: "true"
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

			chaosPodName, err := deployChaosClient(testNamespace, "sentinel-chaos", crName, false, "chaos-sent", testDuration)
			Expect(err).NotTo(HaveOccurred())
			defer func() {
				if debugOnFailure && suiteOrSpecFailed() {
					return
				}
				deleteChaosClient(testNamespace, chaosPodName)
			}()
			defer func() {
				cmd := exec.Command("kubectl", "delete", "littlered", crName, "-n", testNamespace, "--ignore-not-found")
				_, _ = utils.Run(cmd)
			}()

			By("waiting for sentinel cluster to be ready")
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "littlered", crName,
					"-n", testNamespace, "-o", "jsonpath={.status.phase}")
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).To(Equal("Running"))

				cmd = exec.Command("kubectl", "get", "littlered", crName,
					"-n", testNamespace, "-o", "jsonpath={.status.master.podName}")
				master, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(master).NotTo(BeEmpty())
			}, 5*time.Minute, 5*time.Second).Should(Succeed())

			By("waiting 10 seconds for baseline traffic")
			time.Sleep(10 * time.Second)

			// --- Failover 1 ---
			By("identifying and killing first master")
			cmd = exec.Command("kubectl", "get", "littlered", crName,
				"-n", testNamespace, "-o", "jsonpath={.status.master.podName}")
			master1, _ := utils.Run(cmd)
			master1 = strings.TrimSpace(master1)

			oldRunID1, _ := getPodRunID(testNamespace, master1)

			cmd = exec.Command("kubectl", "delete", "pod", master1,
				"-n", testNamespace, "--grace-period=0", "--force")
			_, _ = utils.Run(cmd)

			By("waiting for failover to complete (approx 20s)")
			time.Sleep(20 * time.Second)

			// --- Failover 2 ---
			By("identifying and killing second master")
			var master2 string
			var oldRunID2 string
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "littlered", crName,
					"-n", testNamespace, "-o", "jsonpath={.status.master.podName}")
				out, _ := utils.Run(cmd)
				master2 = strings.TrimSpace(out)
				g.Expect(master2).NotTo(Equal(master1), "Master should have changed")
				g.Expect(master2).NotTo(BeEmpty())

				runID, err := getPodRunID(testNamespace, master2)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(runID).NotTo(Equal(oldRunID1), "New master must have a different RunID")
				oldRunID2 = runID
			}, 1*time.Minute, 2*time.Second).Should(Succeed())

			cmd = exec.Command("kubectl", "delete", "pod", master2,
				"-n", testNamespace, "--grace-period=0", "--force")
			_, _ = utils.Run(cmd)

			By("verifying third master eventually emerges with different RunID")
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "littlered", crName,
					"-n", testNamespace, "-o", "jsonpath={.status.master.podName}")
				out, _ := utils.Run(cmd)
				master3 := strings.TrimSpace(out)
				g.Expect(master3).NotTo(Equal(master2), "Master should have changed again")
				g.Expect(master3).NotTo(BeEmpty())

				runID, err := getPodRunID(testNamespace, master3)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(runID).NotTo(Equal(oldRunID2), "Third master must have a different RunID")
			}, 1*time.Minute, 2*time.Second).Should(Succeed())

			err = waitForChaosClientComplete(testNamespace, chaosPodName, testDuration+2*time.Minute)
			Expect(err).NotTo(HaveOccurred())

			metrics, err := getChaosClientMetrics(testNamespace, chaosPodName)
			Expect(err).NotTo(HaveOccurred())

			Expect(metrics.DataCorruptions).To(Equal(int64(0)), "Data corruption detected!")
			Expect(metrics.WriteAvailability()).To(BeNumerically(">", 0.40))
		})
	})

	Context("Standalone Mode Resilience", Ordered, func() {
		It("should recover after pod restart", func() {
			crName := "chaos-standalone"
			const testDuration = 60 * time.Second

			By("creating standalone and chaos client simultaneously")
			cr := fmt.Sprintf(`
apiVersion: chuck-chuck-chuck.net/v1alpha1
kind: LittleRed
metadata:
  name: %s
  namespace: %s
spec:
  mode: standalone
`, crName, testNamespace)
			cmd := exec.Command("kubectl", "apply", "-f", "-")
			cmd.Stdin = strings.NewReader(cr)
			_, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())

			chaosPodName, err := deployChaosClient(testNamespace, "standalone-restart", crName, false, "chaos-stand", testDuration)
			Expect(err).NotTo(HaveOccurred())
			defer func() {
				if debugOnFailure && suiteOrSpecFailed() {
					return
				}
				deleteChaosClient(testNamespace, chaosPodName)
			}()
			defer func() {
				cmd := exec.Command("kubectl", "delete", "littlered", crName, "-n", testNamespace, "--ignore-not-found")
				_, _ = utils.Run(cmd)
			}()

			By("waiting for standalone to be ready")
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "littlered", crName,
					"-n", testNamespace, "-o", "jsonpath={.status.phase}")
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).To(Equal("Running"))
			}, 3*time.Minute, 5*time.Second).Should(Succeed())

			By("waiting 10 seconds for baseline traffic")
			time.Sleep(10 * time.Second)

			By("recording initial RunID")
			oldRunID, _ := getPodRunID(testNamespace, crName+"-redis-0")

			By("deleting the standalone pod")
			cmd = exec.Command("kubectl", "delete", "pod", crName+"-redis-0",
				"-n", testNamespace, "--grace-period=0", "--force")
			_, _ = utils.Run(cmd)

			By("verifying pod restarts with different RunID")
			Eventually(func(g Gomega) {
				newRunID, err := getPodRunID(testNamespace, crName+"-redis-0")
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(newRunID).NotTo(Equal(oldRunID), "Pod should have a different RunID after restart")
			}, 2*time.Minute, 5*time.Second).Should(Succeed())

			err = waitForChaosClientComplete(testNamespace, chaosPodName, testDuration+2*time.Minute)
			Expect(err).NotTo(HaveOccurred())

			metrics, err := getChaosClientMetrics(testNamespace, chaosPodName)
			Expect(err).NotTo(HaveOccurred())

			Expect(metrics.DataCorruptions).To(Equal(int64(0)))
		})
	})
})