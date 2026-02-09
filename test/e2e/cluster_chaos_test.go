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

var _ = Describe("Cluster Mode Chaos Testing", Ordered, func() {
	const testNamespace = "littlered-cluster-chaos-test"

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

	Context("3-node 0-replica Cluster Stability", Ordered, func() {
		const crName = "chaos-3node-0replica"

		BeforeAll(func() {
			By("creating a 3-shard cluster with no replicas")
			cr := fmt.Sprintf(`
apiVersion: littlered.tanne3.de/v1alpha1
kind: LittleRed
metadata:
  name: %s
  namespace: %s
spec:
  mode: cluster
  cluster:
    shards: 3
    replicasPerShard: 0
  resources:
    requests:
      cpu: "100m"
      memory: "128Mi"
    limits:
      cpu: "100m"
      memory: "128Mi"
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
			}, 5*time.Minute, 5*time.Second).Should(Succeed())

			By("waiting for cluster to be fully formed and ok")
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "exec", crName+"-cluster-0",
					"-n", testNamespace, "-c", "redis", "--",
					"valkey-cli", "CLUSTER", "INFO")
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).To(ContainSubstring("cluster_state:ok"))
				g.Expect(output).To(ContainSubstring("cluster_slots_assigned:16384"))
			}, 2*time.Minute, 5*time.Second).Should(Succeed())
		})

		AfterAll(func() {
			By("cleaning up cluster")
			cmd := exec.Command("kubectl", "delete", "littlered", crName, "-n", testNamespace, "--ignore-not-found", "--timeout=2m")
			_, _ = utils.Run(cmd)
		})

		It("should achieve 100% availability and 0% corruption under stable conditions", func() {
			By("Test ID: CLUST-CHAOS-001")
			const testName = "stable-baseline"
			const testDuration = 10 * time.Second

			By("deploying chaos client pod")
			// The operator creates a service with the same name as the CR
			podName, err := deployChaosClient(testNamespace, testName, crName, true, "chaos-stable", testDuration)
			Expect(err).NotTo(HaveOccurred())
			defer deleteChaosClient(testNamespace, podName)

			By("waiting for chaos client to complete")
			err = waitForChaosClientComplete(testNamespace, podName, testDuration+2*time.Minute)
			Expect(err).NotTo(HaveOccurred())

			By("collecting metrics from chaos client")
			metrics, err := getChaosClientMetrics(testNamespace, podName)
			Expect(err).NotTo(HaveOccurred())
			GinkgoWriter.Printf("Final metrics for stable cluster:\n%s\n", metrics.String())

			By("asserting 100% write success")
			Expect(metrics.WriteAvailability()).To(BeNumerically(">=", 0.999), "Write availability should be 100%")
			
			By("asserting 100% read success")
			Expect(metrics.ReadAvailability()).To(BeNumerically(">=", 0.999), "Read availability should be 100%")

			By("asserting 0 corruption")
			Expect(metrics.DataCorruptions).To(Equal(int64(0)), "Data corruption detected!")
			
			By("asserting some work was actually done")
			Expect(metrics.WriteAttempts).To(BeNumerically(">", 0), "Should have performed some writes")
			Expect(metrics.ReadAttempts).To(BeNumerically(">", 0), "Should have performed some reads")
		})
	})
})
