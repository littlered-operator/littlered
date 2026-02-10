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
	const testNamespace = "littlered-chaos-sentinel-standalone"

	BeforeAll(func() {
		By("creating test namespace")
		cmd := exec.Command("kubectl", "create", "ns", testNamespace)
		_, _ = utils.Run(cmd)
	})

	AfterAll(func() {
		By("cleaning up test namespace")
		cmd := exec.Command("kubectl", "delete", "ns", testNamespace, "--ignore-not-found", "--timeout=2m")
		_, _ = utils.Run(cmd)
	})

	Context("Sentinel Resilience", Ordered, func() {
		It("should maintain availability during rapid double failover", func() {
			crName := fmt.Sprintf("chaos-sentinel-%d", time.Now().Unix())
			const testDuration = 120 * time.Second
			
			By(fmt.Sprintf("creating Sentinel cluster %s and chaos client simultaneously", crName))
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

			chaosPodName, err := deployChaosClient(testNamespace, "sentinel-chaos", crName, false, "chaos-sent", testDuration)
			Expect(err).NotTo(HaveOccurred())
			defer deleteChaosClient(testNamespace, chaosPodName)
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

			cmd = exec.Command("kubectl", "delete", "pod", master1,
				"-n", testNamespace, "--grace-period=0", "--force")
			_, _ = utils.Run(cmd)

			By("waiting for failover to complete (approx 20s)")
			time.Sleep(20 * time.Second)

			// --- Failover 2 ---
			By("identifying and killing second master")
			var master2 string
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "littlered", crName,
					"-n", testNamespace, "-o", "jsonpath={.status.master.podName}")
				out, _ := utils.Run(cmd)
				master2 = strings.TrimSpace(out)
				g.Expect(master2).NotTo(Equal(master1))
				g.Expect(master2).NotTo(BeEmpty())
			}, 1*time.Minute, 2*time.Second).Should(Succeed())

			cmd = exec.Command("kubectl", "delete", "pod", master2,
				"-n", testNamespace, "--grace-period=0", "--force")
			_, _ = utils.Run(cmd)

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
apiVersion: littlered.tanne3.de/v1alpha1
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
			defer deleteChaosClient(testNamespace, chaosPodName)
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

			By("deleting the standalone pod")
			cmd = exec.Command("kubectl", "delete", "pod", crName+"-redis-0",
				"-n", testNamespace, "--grace-period=0", "--force")
			_, _ = utils.Run(cmd)

			err = waitForChaosClientComplete(testNamespace, chaosPodName, testDuration+2*time.Minute)
			Expect(err).NotTo(HaveOccurred())

			metrics, err := getChaosClientMetrics(testNamespace, chaosPodName)
			Expect(err).NotTo(HaveOccurred())
			
			Expect(metrics.DataCorruptions).To(Equal(int64(0)))
		})
	})
})