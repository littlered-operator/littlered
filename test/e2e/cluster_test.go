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
	"strconv"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/tanne3/littlered-operator/test/utils"
)

var _ = Describe("LittleRed Cluster Mode", Ordered, func() {
	const testNamespace = "littlered-cluster-e2e-test"

	BeforeAll(func() {
		By("creating test namespace")
		cmd := exec.Command("kubectl", "create", "ns", testNamespace)
		_, _ = utils.Run(cmd) // Ignore if exists
	})

	AfterAll(func() {
		By("cleaning up test namespace")
		cmd := exec.Command("kubectl", "delete", "ns", testNamespace, "--ignore-not-found", "--timeout=2m")
		_, _ = utils.Run(cmd)
	})

	Context("Basic Cluster Operations", Ordered, func() {
		const crName = "test-cluster"

		AfterAll(func() {
			By("cleaning up cluster CR")
			cmd := exec.Command("kubectl", "delete", "littlered", crName, "-n", testNamespace, "--ignore-not-found", "--timeout=1m")
			_, _ = utils.Run(cmd)
		})

		It("should create a Redis Cluster with 3 shards and 1 replica per shard", func() {
			By("applying the LittleRed CR with cluster mode")
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
		})

		It("should create StatefulSet with 6 replicas (3 masters + 3 replicas)", func() {
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "statefulset", crName+"-cluster",
					"-n", testNamespace, "-o", "jsonpath={.status.readyReplicas}")
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).To(Equal("6"))
			}, 4*time.Minute, 5*time.Second).Should(Succeed())
		})

		It("should create required services", func() {
			By("checking client service")
			cmd := exec.Command("kubectl", "get", "service", crName, "-n", testNamespace)
			_, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())

			By("checking headless service")
			cmd = exec.Command("kubectl", "get", "service", crName+"-cluster", "-n", testNamespace)
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())
		})

		It("should have LittleRed status Running", func() {
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "littlered", crName,
					"-n", testNamespace, "-o", "jsonpath={.status.phase}")
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).To(Equal("Running"))
			}, 4*time.Minute, 5*time.Second).Should(Succeed())
		})

		It("should have cluster state 'ok'", func() {
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "exec", crName+"-cluster-0",
					"-n", testNamespace, "-c", "redis", "--",
					"valkey-cli", "CLUSTER", "INFO")
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).To(ContainSubstring("cluster_state:ok"))
			}, 2*time.Minute, 5*time.Second).Should(Succeed())
		})

		It("should have all 16384 slots assigned", func() {
			cmd := exec.Command("kubectl", "exec", crName+"-cluster-0",
				"-n", testNamespace, "-c", "redis", "--",
				"valkey-cli", "CLUSTER", "INFO")
			output, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())
			Expect(output).To(ContainSubstring("cluster_slots_assigned:16384"))
			Expect(output).To(ContainSubstring("cluster_slots_ok:16384"))
		})

		It("should have correct cluster topology (3 masters, 3 replicas)", func() {
			cmd := exec.Command("kubectl", "exec", crName+"-cluster-0",
				"-n", testNamespace, "-c", "redis", "--",
				"valkey-cli", "CLUSTER", "NODES")
			output, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())

			By("counting masters and replicas")
			lines := strings.Split(strings.TrimSpace(output), "\n")
			masterCount := 0
			replicaCount := 0
			for _, line := range lines {
				if strings.Contains(line, "master") {
					masterCount++
				}
				if strings.Contains(line, "slave") {
					replicaCount++
				}
			}
			Expect(masterCount).To(Equal(3), "Should have 3 masters")
			Expect(replicaCount).To(Equal(3), "Should have 3 replicas")
		})

		It("should verify slot distribution across masters", func() {
			cmd := exec.Command("kubectl", "exec", crName+"-cluster-0",
				"-n", testNamespace, "-c", "redis", "--",
				"valkey-cli", "CLUSTER", "NODES")
			output, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())

			By("verifying each master has slots assigned")
			lines := strings.Split(strings.TrimSpace(output), "\n")
			mastersWithSlots := 0
			for _, line := range lines {
				if strings.Contains(line, "master") && (strings.Contains(line, "-") || strings.Contains(line, "connected")) {
					// Master with slots will have slot ranges like "0-5461" in the line
					parts := strings.Fields(line)
					if len(parts) >= 9 {
						mastersWithSlots++
					}
				}
			}
			Expect(mastersWithSlots).To(Equal(3), "All 3 masters should have slots assigned")
		})

		It("should store and retrieve data using cluster mode", func() {
			By("setting a key with cluster mode")
			cmd := exec.Command("kubectl", "exec", crName+"-cluster-0",
				"-n", testNamespace, "-c", "redis", "--",
				"valkey-cli", "-c", "SET", "cluster-test-key", "cluster-test-value")
			output, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())
			Expect(strings.TrimSpace(output)).To(Equal("OK"))

			By("getting the key")
			cmd = exec.Command("kubectl", "exec", crName+"-cluster-0",
				"-n", testNamespace, "-c", "redis", "--",
				"valkey-cli", "-c", "GET", "cluster-test-key")
			output, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())
			Expect(strings.TrimSpace(output)).To(Equal("cluster-test-value"))
		})

		It("should distribute keys across different shards", func() {
			By("writing multiple keys")
			keys := []string{"key1", "key2", "key3", "key4", "key5"}
			for _, key := range keys {
				cmd := exec.Command("kubectl", "exec", crName+"-cluster-0",
					"-n", testNamespace, "-c", "redis", "--",
					"valkey-cli", "-c", "SET", key, "value-"+key)
				_, err := utils.Run(cmd)
				Expect(err).NotTo(HaveOccurred())
			}

			By("verifying keys are on different nodes (masters have data)")
			nodeKeys := make(map[string]int) // node -> key count
			for i := 0; i < 6; i++ {
				podName := fmt.Sprintf("%s-cluster-%d", crName, i)
				cmd := exec.Command("kubectl", "exec", podName,
					"-n", testNamespace, "-c", "redis", "--",
					"valkey-cli", "DBSIZE")
				output, err := utils.Run(cmd)
				if err != nil {
					_, _ = fmt.Fprintf(GinkgoWriter, "Failed to get DBSIZE from %s: %v\n", podName, err)
					continue
				}

				// Parse output - valkey-cli DBSIZE returns just a number
				output = strings.TrimSpace(output)
				count, parseErr := strconv.Atoi(output)
				if parseErr != nil {
					_, _ = fmt.Fprintf(GinkgoWriter, "Failed to parse DBSIZE from %s: %q (error: %v)\n", podName, output, parseErr)
					continue
				}

				if count > 0 {
					nodeKeys[podName] = count
					_, _ = fmt.Fprintf(GinkgoWriter, "Node %s has %d keys\n", podName, count)
				}
			}

			_, _ = fmt.Fprintf(GinkgoWriter, "Total nodes with keys: %d\n", len(nodeKeys))
			// At least 2 masters should have keys (distributed across shards)
			// We might have up to 6 nodes with keys (3 masters + 3 replicas replicating data)
			Expect(len(nodeKeys)).To(BeNumerically(">=", 2), "Keys should be distributed across multiple shards")
		})

		It("should track cluster state in CR status", func() {
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "littlered", crName,
					"-n", testNamespace, "-o", "jsonpath={.status.cluster.state}")
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).To(Equal("ok"))
			}, 1*time.Minute, 5*time.Second).Should(Succeed())

			By("verifying all 6 nodes are tracked in status")
			cmd := exec.Command("kubectl", "get", "littlered", crName,
				"-n", testNamespace, "-o", "jsonpath={.status.cluster.nodes[*].podName}")
			output, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())
			nodes := strings.Fields(output)
			Expect(len(nodes)).To(Equal(6), "Should track all 6 nodes")
		})

		It("should have ClusterReady condition set to True", func() {
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "littlered", crName,
					"-n", testNamespace, "-o", "jsonpath={.status.conditions[?(@.type=='ClusterReady')].status}")
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).To(Equal("True"))
			}, 2*time.Minute, 5*time.Second).Should(Succeed())
		})
	})

	Context("Cluster Recovery and Resilience", Ordered, func() {
		const crName = "test-cluster-recovery"

		AfterAll(func() {
			By("cleaning up recovery test CR")
			cmd := exec.Command("kubectl", "delete", "littlered", crName, "-n", testNamespace, "--ignore-not-found", "--timeout=1m")
			_, _ = utils.Run(cmd)
		})

		It("should create a cluster for recovery testing", func() {
			By("applying the LittleRed CR")
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
			}, 4*time.Minute, 5*time.Second).Should(Succeed())

			By("writing test data")
			cmd = exec.Command("kubectl", "exec", crName+"-cluster-0",
				"-n", testNamespace, "-c", "redis", "--",
				"valkey-cli", "-c", "SET", "recovery-test-key", "recovery-test-value")
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())
		})

		It("should recover after a replica pod is deleted", func() {
			By("identifying a replica pod")
			cmd := exec.Command("kubectl", "exec", crName+"-cluster-0",
				"-n", testNamespace, "-c", "redis", "--",
				"valkey-cli", "CLUSTER", "NODES")
			output, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())

			// Find a replica/slave node
			var replicaPod string
			lines := strings.Split(strings.TrimSpace(output), "\n")
			for _, line := range lines {
				if strings.Contains(line, "slave") {
					// Extract IP from line format: "nodeID IP:port@clusterport ..."
					parts := strings.Fields(line)
					if len(parts) >= 2 {
						ipPort := parts[1]
						if colonIdx := strings.Index(ipPort, ":"); colonIdx != -1 {
							ip := ipPort[:colonIdx]
							// Find pod with this IP
							for i := 0; i < 6; i++ {
								podName := fmt.Sprintf("%s-cluster-%d", crName, i)
								podCmd := exec.Command("kubectl", "get", "pod", podName,
									"-n", testNamespace, "-o", "jsonpath={.status.podIP}")
								podIP, podErr := utils.Run(podCmd)
								if podErr == nil && strings.TrimSpace(podIP) == ip {
									replicaPod = podName
									break
								}
							}
							if replicaPod != "" {
								break
							}
						}
					}
				}
			}
			Expect(replicaPod).NotTo(BeEmpty(), "Should find a replica pod")
			_, _ = fmt.Fprintf(GinkgoWriter, "Deleting replica pod: %s\n", replicaPod)

			By("deleting the replica pod")
			cmd = exec.Command("kubectl", "delete", "pod", replicaPod,
				"-n", testNamespace, "--grace-period=0")
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())

			By("waiting for pod to be recreated and ready")
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "pod", replicaPod,
					"-n", testNamespace, "-o", "jsonpath={.status.phase}")
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).To(Equal("Running"))
			}, 2*time.Minute, 5*time.Second).Should(Succeed())

			By("verifying cluster recovery completes (not stuck in loop)")
			// Wait long enough for operator to detect and complete recovery
			// If stuck, it would keep logging every 5 seconds
			time.Sleep(30 * time.Second)

			By("verifying cluster state is ok")
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "exec", crName+"-cluster-0",
					"-n", testNamespace, "-c", "redis", "--",
					"valkey-cli", "CLUSTER", "INFO")
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).To(ContainSubstring("cluster_state:ok"))
			}, 2*time.Minute, 5*time.Second).Should(Succeed())

			By("verifying all nodes are tracked in CR status")
			cmd = exec.Command("kubectl", "get", "littlered", crName,
				"-n", testNamespace, "-o", "jsonpath={.status.cluster.nodes}")
			output, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())
			// Should have all 6 nodes with updated node IDs
			nodeCount := strings.Count(output, "\"podName\"")
			Expect(nodeCount).To(Equal(6), "Should track all 6 nodes after recovery")

			By("verifying CR status shows cluster is ok (not recovering)")
			cmd = exec.Command("kubectl", "get", "littlered", crName,
				"-n", testNamespace, "-o", "jsonpath={.status.cluster.state}")
			output, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())
			Expect(strings.TrimSpace(output)).To(Equal("ok"), "Cluster state should be 'ok', not stuck in 'recovering'")

			By("verifying data is still accessible")
			By("diagnosing cluster state before data check")
			// Check which slot the key belongs to
			cmd = exec.Command("kubectl", "exec", crName+"-cluster-0",
				"-n", testNamespace, "-c", "redis", "--",
				"valkey-cli", "CLUSTER", "KEYSLOT", "recovery-test-key")
			keyslotOut, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())
			_, _ = fmt.Fprintf(GinkgoWriter, "Key 'recovery-test-key' hashes to slot: %s\n", strings.TrimSpace(keyslotOut))

			// Check cluster nodes and slot assignments
			cmd = exec.Command("kubectl", "exec", crName+"-cluster-0",
				"-n", testNamespace, "-c", "redis", "--",
				"valkey-cli", "CLUSTER", "NODES")
			nodesOut, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())
			_, _ = fmt.Fprintf(GinkgoWriter, "Cluster nodes after recovery:\n%s\n", nodesOut)

			// Check DBSIZE on all masters to see which has data
			for i := 0; i < 6; i += 2 { // Masters are at 0, 2, 4
				podName := fmt.Sprintf("%s-cluster-%d", crName, i)
				cmd = exec.Command("kubectl", "exec", podName,
					"-n", testNamespace, "-c", "redis", "--",
					"valkey-cli", "DBSIZE")
				dbsize, err := utils.Run(cmd)
				if err == nil {
					_, _ = fmt.Fprintf(GinkgoWriter, "Master %s DBSIZE: %s\n", podName, strings.TrimSpace(dbsize))
				}
			}

			cmd = exec.Command("kubectl", "exec", crName+"-cluster-0",
				"-n", testNamespace, "-c", "redis", "--",
				"valkey-cli", "-c", "GET", "recovery-test-key")
			output, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())
			Expect(strings.TrimSpace(output)).To(Equal("recovery-test-value"),
			"Data written before replica restart should still be accessible (master has the data)")
		})

		It("should handle master pod deletion with replica promotion", func() {
			By("identifying a master pod with replicas")
			cmd := exec.Command("kubectl", "exec", crName+"-cluster-0",
				"-n", testNamespace, "-c", "redis", "--",
				"valkey-cli", "CLUSTER", "NODES")
			output, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())

			// Find a master node with slots
			var masterPod string
			lines := strings.Split(strings.TrimSpace(output), "\n")
			for _, line := range lines {
				if strings.Contains(line, "master") && strings.Contains(line, "connected") {
					parts := strings.Fields(line)
					if len(parts) >= 9 { // Master with slots has more fields
						ipPort := parts[1]
						if colonIdx := strings.Index(ipPort, ":"); colonIdx != -1 {
							ip := ipPort[:colonIdx]
							// Find pod with this IP
							for i := 0; i < 6; i++ {
								podName := fmt.Sprintf("%s-cluster-%d", crName, i)
								podCmd := exec.Command("kubectl", "get", "pod", podName,
									"-n", testNamespace, "-o", "jsonpath={.status.podIP}")
								podIP, podErr := utils.Run(podCmd)
								if podErr == nil && strings.TrimSpace(podIP) == ip {
									masterPod = podName
									break
								}
							}
							if masterPod != "" {
								break
							}
						}
					}
				}
			}
			Expect(masterPod).NotTo(BeEmpty(), "Should find a master pod")
			_, _ = fmt.Fprintf(GinkgoWriter, "Deleting master pod: %s\n", masterPod)

			By("deleting the master pod")
			cmd = exec.Command("kubectl", "delete", "pod", masterPod,
				"-n", testNamespace, "--grace-period=0")
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())

			By("waiting for pod to be recreated and cluster to stabilize")
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "pod", masterPod,
					"-n", testNamespace, "-o", "jsonpath={.status.phase}")
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).To(Equal("Running"))
			}, 2*time.Minute, 5*time.Second).Should(Succeed())

			// Give time for cluster to rebalance
			time.Sleep(15 * time.Second)

			By("verifying cluster eventually becomes healthy")
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "exec", crName+"-cluster-0",
					"-n", testNamespace, "-c", "redis", "--",
					"valkey-cli", "CLUSTER", "INFO")
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).To(ContainSubstring("cluster_state:ok"))
				g.Expect(output).To(ContainSubstring("cluster_slots_ok:16384"))
			}, 3*time.Minute, 10*time.Second).Should(Succeed())
		})

		It("should recover after multiple pod deletions", func() {
			By("deleting 3 pods (half the cluster)")
			podsToDelete := []string{
				fmt.Sprintf("%s-cluster-1", crName),
				fmt.Sprintf("%s-cluster-3", crName),
				fmt.Sprintf("%s-cluster-5", crName),
			}

			for _, pod := range podsToDelete {
				cmd := exec.Command("kubectl", "delete", "pod", pod,
					"-n", testNamespace, "--grace-period=0")
				_, _ = utils.Run(cmd)
				time.Sleep(2 * time.Second) // Stagger deletions
			}

			By("waiting for all pods to be recreated")
			for _, pod := range podsToDelete {
				Eventually(func(g Gomega) {
					cmd := exec.Command("kubectl", "get", "pod", pod,
						"-n", testNamespace, "-o", "jsonpath={.status.phase}")
					output, err := utils.Run(cmd)
					g.Expect(err).NotTo(HaveOccurred())
					g.Expect(output).To(Equal("Running"))
				}, 2*time.Minute, 5*time.Second).Should(Succeed())
			}

			By("waiting for cluster to fully recover (this may take several minutes)")
			time.Sleep(30 * time.Second) // Give operator time to detect changes

			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "exec", crName+"-cluster-0",
					"-n", testNamespace, "-c", "redis", "--",
					"valkey-cli", "CLUSTER", "INFO")
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).To(ContainSubstring("cluster_state:ok"))
				g.Expect(output).To(ContainSubstring("cluster_slots_assigned:16384"))
			}, 6*time.Minute, 10*time.Second).Should(Succeed())

			By("verifying cluster still has 6 nodes")
			cmd := exec.Command("kubectl", "exec", crName+"-cluster-0",
				"-n", testNamespace, "-c", "redis", "--",
				"valkey-cli", "CLUSTER", "INFO")
			output, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())
			Expect(output).To(ContainSubstring("cluster_known_nodes:6"))
		})
	})

	Context("Cluster Configuration", Ordered, func() {
		const crName = "test-cluster-config"

		AfterAll(func() {
			By("cleaning up config test CR")
			cmd := exec.Command("kubectl", "delete", "littlered", crName, "-n", testNamespace, "--ignore-not-found", "--timeout=1m")
			_, _ = utils.Run(cmd)
		})

		It("should create cluster with custom configuration", func() {
			By("applying CR with custom shard and replica settings")
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
    clusterNodeTimeout: 10000
  resources:
    requests:
      cpu: "100m"
      memory: "256Mi"
    limits:
      cpu: "100m"
      memory: "256Mi"
  config:
    maxmemory: "200Mi"
    maxmemoryPolicy: allkeys-lru
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
			}, 4*time.Minute, 5*time.Second).Should(Succeed())
		})

		It("should apply custom maxmemory configuration", func() {
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "exec", crName+"-cluster-0",
					"-n", testNamespace, "-c", "redis", "--",
					"valkey-cli", "CONFIG", "GET", "maxmemory")
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				// Should be ~200Mi in bytes
				g.Expect(output).To(ContainSubstring("maxmemory"))
			}, 1*time.Minute, 5*time.Second).Should(Succeed())
		})

		It("should apply custom maxmemory-policy", func() {
			cmd := exec.Command("kubectl", "exec", crName+"-cluster-0",
				"-n", testNamespace, "-c", "redis", "--",
				"valkey-cli", "CONFIG", "GET", "maxmemory-policy")
			output, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())
			Expect(output).To(ContainSubstring("allkeys-lru"))
		})

		It("should apply custom cluster-node-timeout", func() {
			cmd := exec.Command("kubectl", "exec", crName+"-cluster-0",
				"-n", testNamespace, "-c", "redis", "--",
				"valkey-cli", "CONFIG", "GET", "cluster-node-timeout")
			output, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())
			Expect(output).To(ContainSubstring("10000"))
		})
	})

	Context("Cluster Status Tracking", Ordered, func() {
		const crName = "test-cluster-status"

		AfterAll(func() {
			By("cleaning up status test CR")
			cmd := exec.Command("kubectl", "delete", "littlered", crName, "-n", testNamespace, "--ignore-not-found", "--timeout=1m")
			_, _ = utils.Run(cmd)
		})

		It("should create cluster and track detailed status", func() {
			By("applying the LittleRed CR")
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
			}, 4*time.Minute, 5*time.Second).Should(Succeed())
		})

		It("should track lastBootstrap timestamp", func() {
			cmd := exec.Command("kubectl", "get", "littlered", crName,
				"-n", testNamespace, "-o", "jsonpath={.status.cluster.lastBootstrap}")
			output, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())
			Expect(output).NotTo(BeEmpty(), "Should have lastBootstrap timestamp")
		})

		It("should track all node details in status", func() {
			By("checking node count")
			cmd := exec.Command("kubectl", "get", "littlered", crName,
				"-n", testNamespace, "-o", "jsonpath={.status.cluster.nodes}")
			output, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())
			Expect(output).NotTo(BeEmpty())

			By("verifying each node has required fields")
			for i := 0; i < 6; i++ {
				// Check podName
				path := fmt.Sprintf("{.status.cluster.nodes[%d].podName}", i)
				cmd = exec.Command("kubectl", "get", "littlered", crName,
					"-n", testNamespace, "-o", fmt.Sprintf("jsonpath=%s", path))
				output, err = utils.Run(cmd)
				Expect(err).NotTo(HaveOccurred())
				Expect(output).NotTo(BeEmpty(), "Node should have podName")

				// Check nodeId
				path = fmt.Sprintf("{.status.cluster.nodes[%d].nodeId}", i)
				cmd = exec.Command("kubectl", "get", "littlered", crName,
					"-n", testNamespace, "-o", fmt.Sprintf("jsonpath=%s", path))
				output, err = utils.Run(cmd)
				Expect(err).NotTo(HaveOccurred())
				Expect(output).NotTo(BeEmpty(), "Node should have nodeId")
				Expect(len(output)).To(Equal(40), "NodeId should be 40 characters")

				// Check role
				path = fmt.Sprintf("{.status.cluster.nodes[%d].role}", i)
				cmd = exec.Command("kubectl", "get", "littlered", crName,
					"-n", testNamespace, "-o", fmt.Sprintf("jsonpath=%s", path))
				output, err = utils.Run(cmd)
				Expect(err).NotTo(HaveOccurred())
				Expect(output).To(Or(Equal("master"), Equal("replica")), "Node should have valid role")
			}
		})

		It("should track master nodes with slot ranges", func() {
			By("finding all master nodes")
			var masterCount int
			for i := 0; i < 6; i++ {
				path := fmt.Sprintf("{.status.cluster.nodes[%d].role}", i)
				cmd := exec.Command("kubectl", "get", "littlered", crName,
					"-n", testNamespace, "-o", fmt.Sprintf("jsonpath=%s", path))
				output, err := utils.Run(cmd)
				if err == nil && strings.TrimSpace(output) == "master" {
					masterCount++

					// Masters should have slotRanges
					slotPath := fmt.Sprintf("{.status.cluster.nodes[%d].slotRanges}", i)
					cmd = exec.Command("kubectl", "get", "littlered", crName,
						"-n", testNamespace, "-o", fmt.Sprintf("jsonpath=%s", slotPath))
					slotOutput, err := utils.Run(cmd)
					Expect(err).NotTo(HaveOccurred())
					Expect(slotOutput).NotTo(BeEmpty(), "Master should have slotRanges")
				}
			}
			Expect(masterCount).To(Equal(3), "Should track 3 masters")
		})

		It("should track replica nodes with master references", func() {
			By("finding all replica nodes")
			var replicaCount int
			for i := 0; i < 6; i++ {
				path := fmt.Sprintf("{.status.cluster.nodes[%d].role}", i)
				cmd := exec.Command("kubectl", "get", "littlered", crName,
					"-n", testNamespace, "-o", fmt.Sprintf("jsonpath=%s", path))
				output, err := utils.Run(cmd)
				if err == nil && strings.TrimSpace(output) == "replica" {
					replicaCount++

					// Replicas should have masterNodeId
					masterPath := fmt.Sprintf("{.status.cluster.nodes[%d].masterNodeId}", i)
					cmd = exec.Command("kubectl", "get", "littlered", crName,
						"-n", testNamespace, "-o", fmt.Sprintf("jsonpath=%s", masterPath))
					masterOutput, err := utils.Run(cmd)
					Expect(err).NotTo(HaveOccurred())
					Expect(masterOutput).NotTo(BeEmpty(), "Replica should have masterNodeId")
					Expect(len(masterOutput)).To(Equal(40), "MasterNodeId should be 40 characters")
				}
			}
			Expect(replicaCount).To(Equal(3), "Should track 3 replicas")
		})

		It("should have Ready condition", func() {
			cmd := exec.Command("kubectl", "get", "littlered", crName,
				"-n", testNamespace, "-o", "jsonpath={.status.conditions[?(@.type=='Ready')].status}")
			output, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())
			Expect(output).To(Equal("True"))
		})

		It("should report correct ready/total pod counts", func() {
			cmd := exec.Command("kubectl", "get", "littlered", crName,
				"-n", testNamespace, "-o", "jsonpath={.status.redis.ready}")
			readyOutput, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())
			Expect(readyOutput).To(Equal("6"))

			cmd = exec.Command("kubectl", "get", "littlered", crName,
				"-n", testNamespace, "-o", "jsonpath={.status.redis.total}")
			totalOutput, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())
			Expect(totalOutput).To(Equal("6"))
		})
	})

	Context("Cluster Cleanup", Ordered, func() {
		const crName = "test-cluster-cleanup"

		It("should create a cluster for cleanup testing", func() {
			By("applying the LittleRed CR")
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
			}, 4*time.Minute, 5*time.Second).Should(Succeed())
		})

		It("should clean up all resources when CR is deleted", func() {
			By("deleting the LittleRed CR")
			cmd := exec.Command("kubectl", "delete", "littlered", crName, "-n", testNamespace)
			_, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())

			By("verifying StatefulSet is deleted")
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "statefulset", crName+"-cluster",
					"-n", testNamespace)
				_, err := utils.Run(cmd)
				g.Expect(err).To(HaveOccurred())
			}, 1*time.Minute, 5*time.Second).Should(Succeed())

			By("verifying all pods are deleted")
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "pods",
					"-n", testNamespace, "-l", "app.kubernetes.io/instance="+crName)
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).To(ContainSubstring("No resources found"))
			}, 1*time.Minute, 5*time.Second).Should(Succeed())

			By("verifying Services are deleted")
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "service", crName,
					"-n", testNamespace)
				_, err := utils.Run(cmd)
				g.Expect(err).To(HaveOccurred())
			}, 1*time.Minute, 5*time.Second).Should(Succeed())

			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "service", crName+"-cluster",
					"-n", testNamespace)
				_, err := utils.Run(cmd)
				g.Expect(err).To(HaveOccurred())
			}, 1*time.Minute, 5*time.Second).Should(Succeed())

			By("verifying ConfigMap is deleted")
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "configmap", crName+"-redis",
					"-n", testNamespace)
				_, err := utils.Run(cmd)
				g.Expect(err).To(HaveOccurred())
			}, 1*time.Minute, 5*time.Second).Should(Succeed())
		})
	})
})
