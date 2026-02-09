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

var _ = Describe("Cluster Failover and Recovery", Ordered, func() {
	const testNamespace = "littlered-failover-test"

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

	Context("Master Pod Deletion with Replica Promotion", Ordered, func() {
		const crName = "failover-master"

		BeforeAll(func() {
			By("creating a 3-shard cluster with 1 replica per shard")
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
    replicasPerShard: 1
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

		It("should recover with correct topology after master pod deletion", func() {
			By("Test ID: CLUST-FAILOVER-001")

			By("identifying current topology")
			cmd := exec.Command("kubectl", "exec", crName+"-cluster-0",
				"-n", testNamespace, "-c", "redis", "--",
				"valkey-cli", "CLUSTER", "NODES")
			initialTopology, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())
			GinkgoWriter.Printf("Initial topology:\n%s\n", initialTopology)

			By("deleting master pod-0 (slots 0-5461)")
			victimPod := crName + "-cluster-0"
			cmd = exec.Command("kubectl", "delete", "pod", victimPod,
				"-n", testNamespace, "--grace-period=0", "--force")
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())

			By("waiting for pod to be running again")
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "pod", victimPod,
					"-n", testNamespace, "-o", "jsonpath={.status.phase}")
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).To(Equal("Running"))
			}, 2*time.Minute, 5*time.Second).Should(Succeed())

			By("waiting for cluster to stabilize and become ok")
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "exec", crName+"-cluster-1",
					"-n", testNamespace, "-c", "redis", "--",
					"valkey-cli", "CLUSTER", "INFO")
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).To(ContainSubstring("cluster_state:ok"))
				g.Expect(output).To(ContainSubstring("cluster_slots_assigned:16384"))
				g.Expect(output).To(ContainSubstring("cluster_known_nodes:6"))
			}, 3*time.Minute, 5*time.Second).Should(Succeed())

			By("verifying final cluster topology")
			cmd = exec.Command("kubectl", "exec", crName+"-cluster-1",
				"-n", testNamespace, "-c", "redis", "--",
				"valkey-cli", "CLUSTER", "NODES")
			finalTopology, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())
			GinkgoWriter.Printf("Final topology:\n%s\n", finalTopology)

			By("verifying exactly 3 masters with slots")
			lines := strings.Split(strings.TrimSpace(finalTopology), "\n")
			mastersWithSlots := 0
			emptyMasters := 0
			replicas := 0

			for _, line := range lines {
				if strings.Contains(line, "master") {
					// Check if master has slots (slot ranges appear after "connected")
					parts := strings.Fields(line)
					hasSlots := false
					for i, part := range parts {
						if part == "connected" && i+1 < len(parts) {
							// Next field would be slot range like "0-5461"
							hasSlots = true
							break
						}
					}
					if hasSlots {
						mastersWithSlots++
					} else {
						emptyMasters++
					}
				}
				if strings.Contains(line, "slave") {
					replicas++
				}
			}

			Expect(mastersWithSlots).To(Equal(3), "Should have exactly 3 masters with slots")
			Expect(emptyMasters).To(Equal(0), "Should have no empty masters (all should be replicas)")
			Expect(replicas).To(Equal(3), "Should have exactly 3 replicas")

			By("verifying no ghost nodes remain")
			Expect(finalTopology).NotTo(ContainSubstring("fail"), "Should not have failed nodes")
			Expect(len(lines)).To(Equal(6), "Should have exactly 6 nodes")

			By("verifying cluster is healthy in CR status")
			cmd = exec.Command("kubectl", "get", "littlered", crName,
				"-n", testNamespace, "-o", "jsonpath={.status.cluster.state}")
			output, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())
			Expect(output).To(Equal("ok"))
		})
	})

	Context("Replica Pod Deletion and Reassignment", Ordered, func() {
		const crName = "failover-replica"

		BeforeAll(func() {
			By("creating a 3-shard cluster with 1 replica per shard")
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
    replicasPerShard: 1
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

		It("should recover with correct topology after replica pod deletion", func() {
			By("Test ID: CLUST-FAILOVER-002")

			By("deleting replica pod-3")
			victimPod := crName + "-cluster-3"
			cmd := exec.Command("kubectl", "delete", "pod", victimPod,
				"-n", testNamespace, "--grace-period=0", "--force")
			_, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())

			By("waiting for pod to be running again")
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "pod", victimPod,
					"-n", testNamespace, "-o", "jsonpath={.status.phase}")
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).To(Equal("Running"))
			}, 2*time.Minute, 5*time.Second).Should(Succeed())

			By("waiting for cluster to stabilize")
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "exec", crName+"-cluster-0",
					"-n", testNamespace, "-c", "redis", "--",
					"valkey-cli", "CLUSTER", "INFO")
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).To(ContainSubstring("cluster_state:ok"))
				g.Expect(output).To(ContainSubstring("cluster_known_nodes:6"))
			}, 2*time.Minute, 5*time.Second).Should(Succeed())

			By("verifying final cluster topology")
			cmd = exec.Command("kubectl", "exec", crName+"-cluster-0",
				"-n", testNamespace, "-c", "redis", "--",
				"valkey-cli", "CLUSTER", "NODES")
			finalTopology, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())
			GinkgoWriter.Printf("Final topology:\n%s\n", finalTopology)

			By("verifying correct master/replica distribution")
			lines := strings.Split(strings.TrimSpace(finalTopology), "\n")
			mastersWithSlots := 0
			emptyMasters := 0
			replicas := 0

			for _, line := range lines {
				if strings.Contains(line, "master") {
					parts := strings.Fields(line)
					hasSlots := false
					for i, part := range parts {
						if part == "connected" && i+1 < len(parts) {
							hasSlots = true
							break
						}
					}
					if hasSlots {
						mastersWithSlots++
					} else {
						emptyMasters++
					}
				}
				if strings.Contains(line, "slave") {
					replicas++
				}
			}

			Expect(mastersWithSlots).To(Equal(3), "Should have exactly 3 masters with slots")
			Expect(emptyMasters).To(Equal(0), "Should have no empty masters")
			Expect(replicas).To(Equal(3), "Should have exactly 3 replicas")
		})
	})

	Context("Chaos Testing - Master Deletion", Ordered, func() {
		const crName = "chaos-failover-master"

		BeforeAll(func() {
			By("creating a 3-shard cluster with 1 replica per shard")
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
    replicasPerShard: 1
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

		It("should maintain availability during master pod deletion with chaos client running", func() {
			By("Test ID: CLUST-FAILOVER-CHAOS-001")
			const testDuration = 60 * time.Second

			By("deploying chaos client pod")
			podName, err := deployChaosClient(testNamespace, "master-delete", crName, true, "chaos-master", testDuration)
			Expect(err).NotTo(HaveOccurred())
			defer deleteChaosClient(testNamespace, podName)

			By("waiting for chaos client to start generating load")
			time.Sleep(5 * time.Second)

			By("deleting master pod during load")
			victimPod := crName + "-cluster-0"
			cmd := exec.Command("kubectl", "delete", "pod", victimPod,
				"-n", testNamespace, "--grace-period=0", "--force")
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())

			By("waiting for chaos client to complete")
			err = waitForChaosClientComplete(testNamespace, podName, testDuration+2*time.Minute)
			Expect(err).NotTo(HaveOccurred())

			By("collecting metrics from chaos client")
			metrics, err := getChaosClientMetrics(testNamespace, podName)
			Expect(err).NotTo(HaveOccurred())
			GinkgoWriter.Printf("Final metrics after master deletion:\n%s\n", metrics.String())

			By("verifying cluster recovered to healthy state")
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "exec", crName+"-cluster-1",
					"-n", testNamespace, "-c", "redis", "--",
					"valkey-cli", "CLUSTER", "INFO")
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).To(ContainSubstring("cluster_state:ok"))
				g.Expect(output).To(ContainSubstring("cluster_slots_assigned:16384"))
			}, 2*time.Minute, 5*time.Second).Should(Succeed())

			By("verifying correct topology after recovery")
			cmd = exec.Command("kubectl", "exec", crName+"-cluster-1",
				"-n", testNamespace, "-c", "redis", "--",
				"valkey-cli", "CLUSTER", "NODES")
			finalTopology, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())

			lines := strings.Split(strings.TrimSpace(finalTopology), "\n")
			mastersWithSlots := 0
			emptyMasters := 0
			replicas := 0

			for _, line := range lines {
				if strings.Contains(line, "master") {
					parts := strings.Fields(line)
					hasSlots := false
					for i, part := range parts {
						if part == "connected" && i+1 < len(parts) {
							hasSlots = true
							break
						}
					}
					if hasSlots {
						mastersWithSlots++
					} else {
						emptyMasters++
					}
				}
				if strings.Contains(line, "slave") {
					replicas++
				}
			}

			Expect(mastersWithSlots).To(Equal(3), "Should have exactly 3 masters with slots")
			Expect(emptyMasters).To(Equal(0), "Should have no empty masters")
			Expect(replicas).To(Equal(3), "Should have exactly 3 replicas")

			By("asserting acceptable availability (>95% during failover)")
			Expect(metrics.WriteAvailability()).To(BeNumerically(">=", 0.95),
				"Write availability should be >95% during master failover with replicas")
			Expect(metrics.ReadAvailability()).To(BeNumerically(">=", 0.95),
				"Read availability should be >95% during master failover with replicas")

			By("asserting no data corruption")
			Expect(metrics.DataCorruptions).To(Equal(int64(0)), "Data corruption detected!")

			By("asserting work was done")
			Expect(metrics.WriteAttempts).To(BeNumerically(">", 0), "Should have performed writes")
			Expect(metrics.ReadAttempts).To(BeNumerically(">", 0), "Should have performed reads")
		})
	})

	Context("Chaos Testing - Replica Deletion", Ordered, func() {
		const crName = "chaos-failover-replica"

		BeforeAll(func() {
			By("creating a 3-shard cluster with 1 replica per shard")
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
    replicasPerShard: 1
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

		It("should maintain 100% availability during replica pod deletion", func() {
			By("Test ID: CLUST-FAILOVER-CHAOS-002")
			const testDuration = 60 * time.Second

			By("deploying chaos client pod")
			podName, err := deployChaosClient(testNamespace, "replica-delete", crName, true, "chaos-replica", testDuration)
			Expect(err).NotTo(HaveOccurred())
			defer deleteChaosClient(testNamespace, podName)

			By("waiting for chaos client to start generating load")
			time.Sleep(5 * time.Second)

			By("deleting replica pod during load")
			victimPod := crName + "-cluster-3"
			cmd := exec.Command("kubectl", "delete", "pod", victimPod,
				"-n", testNamespace, "--grace-period=0", "--force")
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())

			By("waiting for chaos client to complete")
			err = waitForChaosClientComplete(testNamespace, podName, testDuration+2*time.Minute)
			Expect(err).NotTo(HaveOccurred())

			By("collecting metrics from chaos client")
			metrics, err := getChaosClientMetrics(testNamespace, podName)
			Expect(err).NotTo(HaveOccurred())
			GinkgoWriter.Printf("Final metrics after replica deletion:\n%s\n", metrics.String())

			By("verifying cluster is healthy")
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "exec", crName+"-cluster-0",
					"-n", testNamespace, "-c", "redis", "--",
					"valkey-cli", "CLUSTER", "INFO")
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).To(ContainSubstring("cluster_state:ok"))
			}, 2*time.Minute, 5*time.Second).Should(Succeed())

			By("verifying correct topology")
			cmd = exec.Command("kubectl", "exec", crName+"-cluster-0",
				"-n", testNamespace, "-c", "redis", "--",
				"valkey-cli", "CLUSTER", "NODES")
			finalTopology, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())

			lines := strings.Split(strings.TrimSpace(finalTopology), "\n")
			mastersWithSlots := 0
			replicas := 0

			for _, line := range lines {
				if strings.Contains(line, "master") {
					parts := strings.Fields(line)
					for i, part := range parts {
						if part == "connected" && i+1 < len(parts) {
							mastersWithSlots++
							break
						}
					}
				}
				if strings.Contains(line, "slave") {
					replicas++
				}
			}

			Expect(mastersWithSlots).To(Equal(3), "Should have exactly 3 masters with slots")
			Expect(replicas).To(Equal(3), "Should have exactly 3 replicas")

			By("asserting near-100% availability (replica deletion should not affect availability)")
			Expect(metrics.WriteAvailability()).To(BeNumerically(">=", 0.99),
				"Write availability should be ~100% during replica deletion")
			Expect(metrics.ReadAvailability()).To(BeNumerically(">=", 0.99),
				"Read availability should be ~100% during replica deletion")

			By("asserting no data corruption")
			Expect(metrics.DataCorruptions).To(Equal(int64(0)), "Data corruption detected!")
		})
	})
})
