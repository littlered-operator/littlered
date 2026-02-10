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
		var crName string

		BeforeAll(func() {
			crName = fmt.Sprintf("chaos-sentinel-%d", time.Now().Unix())
			By(fmt.Sprintf("creating Sentinel cluster %s for chaos testing", crName))
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
  resources:
    requests:
      cpu: "100m"
      memory: "128Mi"
    limits:
      cpu: "100m"
      memory: "128Mi"
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

		AfterAll(func() {
			By("cleaning up sentinel cluster")
			cmd := exec.Command("kubectl", "delete", "littlered", crName, "-n", testNamespace, "--ignore-not-found", "--timeout=2m")
			_, _ = utils.Run(cmd)
		})

		It("should maintain availability during rapid double failover", func() {
			const testName = "sentinel-double-failover"
			const testDuration = 90 * time.Second
			serviceName := crName

			By("deploying chaos client targeting the Master Service")
			podName, err := deployChaosClient(testNamespace, testName, serviceName, false, "chaos-failover", testDuration)
			Expect(err).NotTo(HaveOccurred())
			defer deleteChaosClient(testNamespace, podName)

			time.Sleep(15 * time.Second)

			By("identifying first master")
			cmd := exec.Command("kubectl", "get", "littlered", crName,
				"-n", testNamespace, "-o", "jsonpath={.status.master.podName}")
			master1, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())
			master1 = strings.TrimSpace(master1)

			By("killing first master")
			cmd = exec.Command("kubectl", "delete", "pod", master1,
				"-n", testNamespace, "--grace-period=0", "--force")
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())

			time.Sleep(30 * time.Second)

			var master2 string
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "littlered", crName,
					"-n", testNamespace, "-o", "jsonpath={.status.master.podName}")
				out, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				master2 = strings.TrimSpace(out)
				g.Expect(master2).NotTo(Equal(master1), "New master should be different")
				g.Expect(master2).NotTo(BeEmpty())
			}, 30*time.Second, 2*time.Second).Should(Succeed())

			By("killing second master")
			cmd = exec.Command("kubectl", "delete", "pod", master2,
				"-n", testNamespace, "--grace-period=0", "--force")
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())

			err = waitForChaosClientComplete(testNamespace, podName, testDuration+2*time.Minute)
			Expect(err).NotTo(HaveOccurred())

			metrics, err := getChaosClientMetrics(testNamespace, podName)
			Expect(err).NotTo(HaveOccurred())
			
			Expect(metrics.DataCorruptions).To(Equal(int64(0)), "Data corruption detected!")
			Expect(metrics.WriteAvailability()).To(BeNumerically(">", 0.40))
		})
	})

	Context("Standalone Mode Resilience", Ordered, func() {
		const crName = "chaos-standalone"

		BeforeAll(func() {
			By("creating a standalone Redis for chaos testing")
			cr := fmt.Sprintf(`
apiVersion: littlered.tanne3.de/v1alpha1
kind: LittleRed
metadata:
  name: %s
  namespace: %s
spec:
  mode: standalone
  resources:
    requests:
      cpu: "100m"
      memory: "128Mi"
    limits:
      cpu: "200m"
      memory: "256Mi"
`, crName, testNamespace)
			cmd := exec.Command("kubectl", "apply", "-f", "-")
			cmd.Stdin = strings.NewReader(cr)
			_, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())

			By("waiting for standalone to be ready")
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "littlered", crName,
					"-n", testNamespace, "-o", "jsonpath={.status.phase}")
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).To(Equal("Running"))
			}, 3*time.Minute, 5*time.Second).Should(Succeed())
		})

		AfterAll(func() {
			By("cleaning up standalone")
			cmd := exec.Command("kubectl", "delete", "littlered", crName, "-n", testNamespace, "--ignore-not-found", "--timeout=1m")
			_, _ = utils.Run(cmd)
		})

		It("should recover after pod restart", func() {
			const testName = "standalone-restart"
			const testDuration = 40 * time.Second

			By("deploying chaos client pod")
			podName, err := deployChaosClient(testNamespace, testName, crName, false, "chaos-standalone", testDuration)
			Expect(err).NotTo(HaveOccurred())
			defer deleteChaosClient(testNamespace, podName)

			time.Sleep(10 * time.Second)

			By("deleting the standalone pod")
			cmd := exec.Command("kubectl", "delete", "pod", crName+"-redis-0",
				"-n", testNamespace, "--grace-period=0", "--force")
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())

			err = waitForChaosClientComplete(testNamespace, podName, testDuration+2*time.Minute)
			Expect(err).NotTo(HaveOccurred())

			metrics, err := getChaosClientMetrics(testNamespace, podName)
			Expect(err).NotTo(HaveOccurred())
			
			Expect(metrics.DataCorruptions).To(Equal(int64(0)))
		})
	})
})