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

var _ = Describe("Cluster Mode Functional Testing", Ordered, func() {
	const testNamespace = "littlered-cluster-func-test"

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

	Context("Basic Operations (3 Masters, 1 Replica/Shard)", Ordered, func() {
		const crName = "func-cluster-basic"

		BeforeAll(func() {
			By("creating a Redis Cluster with 3 shards and 1 replica per shard")
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

			By("waiting for cluster to be Running")
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "littlered", crName,
					"-n", testNamespace, "-o", "jsonpath={.status.phase}")
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).To(Equal("Running"))
			}, 5*time.Minute, 5*time.Second).Should(Succeed())
		})

		AfterAll(func() {
			By("cleaning up cluster CR")
			cmd := exec.Command("kubectl", "delete", "littlered", crName, "-n", testNamespace, "--ignore-not-found", "--timeout=1m")
			_, _ = utils.Run(cmd)
		})

		It("should have correct topology and state", func() {
			By("checking StatefulSet replicas")
			cmd := exec.Command("kubectl", "get", "statefulset", crName+"-cluster",
				"-n", testNamespace, "-o", "jsonpath={.status.readyReplicas}")
			output, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())
			Expect(output).To(Equal("6"))

			By("checking cluster state is ok")
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "exec", crName+"-cluster-0",
					"-n", testNamespace, "-c", "redis", "--",
					"valkey-cli", "CLUSTER", "INFO")
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).To(ContainSubstring("cluster_state:ok"))
				g.Expect(output).To(ContainSubstring("cluster_slots_assigned:16384"))
			}, 1*time.Minute, 5*time.Second).Should(Succeed())

			By("verifying 3 masters and 3 replicas")
			cmd = exec.Command("kubectl", "exec", crName+"-cluster-0",
				"-n", testNamespace, "-c", "redis", "--",
				"valkey-cli", "CLUSTER", "NODES")
			output, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())
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
			Expect(masterCount).To(Equal(3))
			Expect(replicaCount).To(Equal(3))
		})

		It("should store and retrieve data with redirection", func() {
			By("setting keys")
			keys := []string{"key1", "key2", "key3"}
			for _, key := range keys {
				cmd := exec.Command("kubectl", "exec", crName+"-cluster-0",
					"-n", testNamespace, "-c", "redis", "--",
					"valkey-cli", "-c", "SET", key, "value-"+key)
				_, err := utils.Run(cmd)
				Expect(err).NotTo(HaveOccurred())
			}

			By("getting keys")
			for _, key := range keys {
				cmd := exec.Command("kubectl", "exec", crName+"-cluster-2",
					"-n", testNamespace, "-c", "redis", "--",
					"valkey-cli", "-c", "GET", key)
				output, err := utils.Run(cmd)
				Expect(err).NotTo(HaveOccurred())
				Expect(strings.TrimSpace(output)).To(Equal("value-"+key))
			}
		})

		It("should track status correctly in the CR", func() {
			By("checking status.cluster.state")
			cmd := exec.Command("kubectl", "get", "littlered", crName,
				"-n", testNamespace, "-o", "jsonpath={.status.cluster.state}")
			output, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())
			Expect(output).To(Equal("ok"))

			By("checking node list in status")
			cmd = exec.Command("kubectl", "get", "littlered", crName,
				"-n", testNamespace, "-o", "jsonpath={.status.cluster.nodes[*].podName}")
			output, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())
			nodes := strings.Fields(output)
			Expect(len(nodes)).To(Equal(6))
		})
	})

	Context("0-Replica Mode Self-Healing", Ordered, func() {
		const crName = "func-cluster-0replica"

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

		AfterAll(func() {
			By("cleaning up cluster")
			cmd := exec.Command("kubectl", "delete", "littlered", crName, "-n", testNamespace, "--ignore-not-found", "--timeout=1m")
			_, _ = utils.Run(cmd)
		})

		It("should heal cluster after master pod deletion in 0-replica mode", func() {
			// In 0-replica mode, deleting a pod means losing data and slots.
			// The operator should re-assign slots to the new node.
			
			victimPod := crName + "-cluster-1"
			By(fmt.Sprintf("deleting master pod %s", victimPod))
			cmd := exec.Command("kubectl", "delete", "pod", victimPod,
				"-n", testNamespace, "--grace-period=0", "--force")
			_, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())

			By("waiting for pod to return and cluster to stabilize")
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "exec", crName+"-cluster-0",
					"-n", testNamespace, "-c", "redis", "--",
					"valkey-cli", "CLUSTER", "INFO")
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).To(ContainSubstring("cluster_state:ok"))
				g.Expect(output).To(ContainSubstring("cluster_slots_assigned:16384"))
				g.Expect(output).To(ContainSubstring("cluster_known_nodes:3"))
			}, 5*time.Minute, 10*time.Second).Should(Succeed())
		})
	})

	Context("Failover Recovery (3 Masters, 1 Replica/Shard)", Ordered, func() {
		const crName = "func-cluster-failover"

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

		AfterAll(func() {
			By("cleaning up cluster")
			cmd := exec.Command("kubectl", "delete", "littlered", crName, "-n", testNamespace, "--ignore-not-found", "--timeout=1m")
			_, _ = utils.Run(cmd)
		})

		It("should promote replica when master pod is deleted", func() {
			By("identifying initial masters")
			cmd := exec.Command("kubectl", "exec", crName+"-cluster-0",
				"-n", testNamespace, "-c", "redis", "--",
				"valkey-cli", "CLUSTER", "NODES")
			_, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())
			
			By("deleting master pod-0")
			cmd = exec.Command("kubectl", "delete", "pod", crName+"-cluster-0",
				"-n", testNamespace, "--grace-period=0", "--force")
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())

			By("waiting for cluster to become ok again")
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "exec", crName+"-cluster-1",
					"-n", testNamespace, "-c", "redis", "--",
					"valkey-cli", "CLUSTER", "INFO")
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).To(ContainSubstring("cluster_state:ok"))
			}, 4*time.Minute, 5*time.Second).Should(Succeed())

			By("verifying topology is recovered (6 nodes, 3 masters)")
			cmd = exec.Command("kubectl", "exec", crName+"-cluster-1",
				"-n", testNamespace, "-c", "redis", "--",
				"valkey-cli", "CLUSTER", "NODES")
			output, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())
			lines := strings.Split(strings.TrimSpace(output), "\n")
			
			mastersWithSlots := 0
			for _, line := range lines {
				if strings.Contains(line, "master") {
					fields := strings.Fields(line)
					if len(fields) > 8 {
						mastersWithSlots++
					}
				}
			}
			Expect(mastersWithSlots).To(Equal(3))
			Expect(len(lines)).To(Equal(6))
			Expect(output).NotTo(ContainSubstring("fail"))
		})

		It("should reassign replica when replica pod is deleted", func() {
			By("deleting replica pod-3")
			cmd := exec.Command("kubectl", "delete", "pod", crName+"-cluster-3",
				"-n", testNamespace, "--grace-period=0", "--force")
			_, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())

			By("waiting for cluster to stabilize")
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "exec", crName+"-cluster-0",
					"-n", testNamespace, "-c", "redis", "--",
					"valkey-cli", "CLUSTER", "INFO")
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).To(ContainSubstring("cluster_state:ok"))
				g.Expect(output).To(ContainSubstring("cluster_known_nodes:6"))
			}, 4*time.Minute, 5*time.Second).Should(Succeed())
		})
	})

	Context("Cluster Cleanup", Ordered, func() {
		const crName = "func-cluster-cleanup"

		It("should clean up all resources when CR is deleted", func() {
			By("creating cluster for cleanup")
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
`, crName, testNamespace)
			cmd := exec.Command("kubectl", "apply", "-f", "-")
			cmd.Stdin = strings.NewReader(cr)
			_, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())

			By("waiting for Running state")
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "littlered", crName,
					"-n", testNamespace, "-o", "jsonpath={.status.phase}")
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).To(Equal("Running"))
			}, 4*time.Minute, 5*time.Second).Should(Succeed())

			By("deleting the CR")
			cmd = exec.Command("kubectl", "delete", "littlered", crName, "-n", testNamespace)
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())

			By("verifying resources are gone")
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "statefulset", crName+"-cluster", "-n", testNamespace)
				_, err := utils.Run(cmd)
				g.Expect(err).To(HaveOccurred())
			}, 2*time.Minute, 5*time.Second).Should(Succeed())
		})
	})

	Context("Custom Configuration", Ordered, func() {
		const crName = "func-cluster-config"

		BeforeAll(func() {
			By("creating cluster with custom configuration")
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
  config:
    maxmemory: "256Mi"
    maxmemoryPolicy: allkeys-lru
`, crName, testNamespace)
			cmd := exec.Command("kubectl", "apply", "-f", "-")
			cmd.Stdin = strings.NewReader(cr)
			_, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())

			By("waiting for Running phase")
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "littlered", crName,
					"-n", testNamespace, "-o", "jsonpath={.status.phase}")
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).To(Equal("Running"))
			}, 4*time.Minute, 5*time.Second).Should(Succeed())
		})

		AfterAll(func() {
			By("cleaning up cluster")
			cmd := exec.Command("kubectl", "delete", "littlered", crName, "-n", testNamespace, "--ignore-not-found", "--timeout=1m")
			_, _ = utils.Run(cmd)
		})

		It("should apply custom redis configuration to all nodes", func() {
			for i := 0; i < 3; i++ {
				podName := fmt.Sprintf("%s-cluster-%d", crName, i)
				cmd := exec.Command("kubectl", "exec", podName,
					"-n", testNamespace, "-c", "redis", "--",
					"valkey-cli", "CONFIG", "GET", "maxmemory-policy")
				output, err := utils.Run(cmd)
				Expect(err).NotTo(HaveOccurred())
				Expect(output).To(ContainSubstring("allkeys-lru"))

				cmd = exec.Command("kubectl", "exec", podName,
					"-n", testNamespace, "-c", "redis", "--",
					"valkey-cli", "CONFIG", "GET", "cluster-node-timeout")
				output, err = utils.Run(cmd)
				Expect(err).NotTo(HaveOccurred())
				Expect(output).To(ContainSubstring("10000"))
			}
		})
	})

	Context("Debug Research Mode (No slots)", Ordered, func() {
		const crName = "func-cluster-debug"

		BeforeAll(func() {
			By("Creating an empty cluster with debug-skip-slot-assignment annotation")
			cr := fmt.Sprintf(`
apiVersion: littlered.tanne3.de/v1alpha1
kind: LittleRed
metadata:
  name: %s
  namespace: %s
  annotations:
    littlered.tanne3.de/debug-skip-slot-assignment: "true"
spec:
  mode: cluster
  cluster:
    shards: 3
    replicasPerShard: 0
`, crName, testNamespace)
			cmd := exec.Command("kubectl", "apply", "-f", "-")
			cmd.Stdin = strings.NewReader(cr)
			_, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())
		})

		AfterAll(func() {
			By("cleaning up cluster")
			cmd := exec.Command("kubectl", "delete", "littlered", crName, "-n", testNamespace, "--ignore-not-found", "--timeout=1m")
			_, _ = utils.Run(cmd)
		})

		It("should stay in 'fail' state without slots assigned", func() {
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "exec", crName+"-cluster-0",
					"-n", testNamespace, "-c", "redis", "--",
					"valkey-cli", "CLUSTER", "INFO")
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).To(ContainSubstring("cluster_state:fail"))
				g.Expect(output).To(ContainSubstring("cluster_slots_assigned:0"))
			}, 2*time.Minute, 5*time.Second).Should(Succeed())
		})
	})
})