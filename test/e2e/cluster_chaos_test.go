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
	"math/rand"
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

	Context("Baseline Stability (3 Masters, 0 Replicas)", Ordered, func() {
		const crName = "chaos-cluster-stable"
		var chaosPodName string
		const testDuration = 30 * time.Second

		BeforeAll(func() {
			By("creating a 3-shard cluster with no replicas")
			cr := fmt.Sprintf(`
apiVersion: littlered.chuck-chuck-chuck.net/v1alpha1
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

			By("deploying chaos client pod simultaneously")
			chaosPodName, err = deployChaosClient(testNamespace, "stable", crName, true, "chaos-stable", testDuration)
			Expect(err).NotTo(HaveOccurred())

			By("waiting for cluster to be ready and ok")
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "littlered", crName,
					"-n", testNamespace, "-o", "jsonpath={.status.phase}")
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).To(Equal("Running"))

				cmd = exec.Command("kubectl", "exec", crName+"-cluster-0",
					"-n", testNamespace, "-c", "redis", "--",
					"valkey-cli", "CLUSTER", "INFO")
				output, err = utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).To(ContainSubstring("cluster_state:ok"))
			}, 5*time.Minute, 5*time.Second).Should(Succeed())

			By("waiting 10 seconds for baseline traffic")
			time.Sleep(10 * time.Second)
		})

		AfterAll(func() {
			deleteChaosClient(testNamespace, chaosPodName)
			cmd := exec.Command("kubectl", "delete", "littlered", crName, "-n", testNamespace, "--ignore-not-found", "--timeout=2m")
			_, _ = utils.Run(cmd)
		})

		It("should maintain 100% availability under stable conditions", func() {
			err := waitForChaosClientComplete(testNamespace, chaosPodName, testDuration+2*time.Minute)
			Expect(err).NotTo(HaveOccurred())

			metrics, err := getChaosClientMetrics(testNamespace, chaosPodName)
			Expect(err).NotTo(HaveOccurred())

			Expect(metrics.WriteAvailability()).To(BeNumerically(">=", 0.99))
			Expect(metrics.DataCorruptions).To(Equal(int64(0)))
		})
	})

	Context("Resilience with Replicas (3 Masters, 1 Replica/Shard)", Ordered, func() {
		const crName = "chaos-cluster-resilience"

		AfterEach(func() {
			By("cleaning up cluster")
			cmd := exec.Command("kubectl", "delete", "littlered", crName, "-n", testNamespace, "--ignore-not-found", "--timeout=2m")
			_, _ = utils.Run(cmd)
		})

		It("should maintain high availability during master failure", func() {
			const testDuration = 60 * time.Second
			
			By("creating a 3-shard cluster with 1 replica per shard")
			cr := fmt.Sprintf(`
apiVersion: littlered.chuck-chuck-chuck.net/v1alpha1
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

			By("deploying chaos client pod simultaneously")
			chaosPodName, err := deployChaosClient(testNamespace, "master-fail", crName, true, "chaos-master", testDuration)
			Expect(err).NotTo(HaveOccurred())
			defer deleteChaosClient(testNamespace, chaosPodName)

			By("waiting for cluster to be ready and ok")
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "littlered", crName,
					"-n", testNamespace, "-o", "jsonpath={.status.phase}")
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).To(Equal("Running"))

				cmd = exec.Command("kubectl", "exec", crName+"-cluster-0",
					"-n", testNamespace, "-c", "redis", "--",
					"valkey-cli", "CLUSTER", "INFO")
				output, err = utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).To(ContainSubstring("cluster_state:ok"))
			}, 5*time.Minute, 5*time.Second).Should(Succeed())

			By("waiting 10 seconds for baseline traffic")
			time.Sleep(10 * time.Second)

			By("deleting master pod-0")
			cmd = exec.Command("kubectl", "delete", "pod", crName+"-cluster-0",
				"-n", testNamespace, "--grace-period=0", "--force")
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())

			err = waitForChaosClientComplete(testNamespace, chaosPodName, testDuration+2*time.Minute)
			Expect(err).NotTo(HaveOccurred())

			metrics, err := getChaosClientMetrics(testNamespace, chaosPodName)
			Expect(err).NotTo(HaveOccurred())
			GinkgoWriter.Printf("Master Failure Availability: %.2f%%\n", metrics.WriteAvailability()*100)

			Expect(metrics.DataCorruptions).To(Equal(int64(0)))
			Expect(metrics.WriteAvailability()).To(BeNumerically(">=", 0.80))
		})

		It("should maintain 100% availability during replica failure", func() {
			const testDuration = 60 * time.Second
			
			By("creating a 3-shard cluster with 1 replica per shard")
			cr := fmt.Sprintf(`
apiVersion: littlered.chuck-chuck-chuck.net/v1alpha1
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

			By("deploying chaos client pod simultaneously")
			chaosPodName, err := deployChaosClient(testNamespace, "replica-fail", crName, true, "chaos-replica", testDuration)
			Expect(err).NotTo(HaveOccurred())
			defer deleteChaosClient(testNamespace, chaosPodName)

			By("waiting for cluster to be ready and ok")
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "littlered", crName,
					"-n", testNamespace, "-o", "jsonpath={.status.phase}")
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).To(Equal("Running"))

				cmd = exec.Command("kubectl", "exec", crName+"-cluster-0",
					"-n", testNamespace, "-c", "redis", "--",
					"valkey-cli", "CLUSTER", "INFO")
				output, err = utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).To(ContainSubstring("cluster_state:ok"))
			}, 5*time.Minute, 5*time.Second).Should(Succeed())

			By("waiting 10 seconds for baseline traffic")
			time.Sleep(10 * time.Second)

			By("deleting replica pod-3")
			cmd = exec.Command("kubectl", "delete", "pod", crName+"-cluster-3",
				"-n", testNamespace, "--grace-period=0", "--force")
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())

			err = waitForChaosClientComplete(testNamespace, chaosPodName, testDuration+2*time.Minute)
			Expect(err).NotTo(HaveOccurred())

			metrics, err := getChaosClientMetrics(testNamespace, chaosPodName)
			Expect(err).NotTo(HaveOccurred())

			Expect(metrics.DataCorruptions).To(Equal(int64(0)))
			Expect(metrics.WriteAvailability()).To(BeNumerically(">=", 0.99))
		})

		It("should maintain data integrity during rolling restart", func() {
			const testDuration = 90 * time.Second
			
			By("creating a 3-shard cluster with 1 replica per shard")
			cr := fmt.Sprintf(`
apiVersion: littlered.chuck-chuck-chuck.net/v1alpha1
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

			By("deploying chaos client pod simultaneously")
			chaosPodName, err := deployChaosClient(testNamespace, "rolling", crName, true, "chaos-rolling", testDuration)
			Expect(err).NotTo(HaveOccurred())
			defer deleteChaosClient(testNamespace, chaosPodName)

			By("waiting for cluster to be ready and ok")
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "littlered", crName,
					"-n", testNamespace, "-o", "jsonpath={.status.phase}")
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).To(Equal("Running"))

				cmd = exec.Command("kubectl", "exec", crName+"-cluster-0",
					"-n", testNamespace, "-c", "redis", "--",
					"valkey-cli", "CLUSTER", "INFO")
				output, err = utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).To(ContainSubstring("cluster_state:ok"))
			}, 5*time.Minute, 5*time.Second).Should(Succeed())

			By("waiting 10 seconds for baseline traffic")
			time.Sleep(10 * time.Second)

			By("triggering rolling restart via annotation")
			cmd = exec.Command("kubectl", "annotate", "littlered", crName,
				"-n", testNamespace, fmt.Sprintf("chaos-test=%d", time.Now().Unix()), "--overwrite")
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())

			err = waitForChaosClientComplete(testNamespace, chaosPodName, testDuration+2*time.Minute)
			Expect(err).NotTo(HaveOccurred())

			metrics, err := getChaosClientMetrics(testNamespace, chaosPodName)
			Expect(err).NotTo(HaveOccurred())

			Expect(metrics.DataCorruptions).To(Equal(int64(0)))
			// Rolling restart with replicas should maintain availability (data accessible).
			Expect(metrics.ReadAvailability()).To(BeNumerically(">=", 0.95))

			By("verifying final cluster topology (no lost shards)")
			cmd = exec.Command("kubectl", "exec", crName+"-cluster-0",
				"-n", testNamespace, "-c", "redis", "--",
				"valkey-cli", "CLUSTER", "INFO")
			output, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())
			Expect(output).To(ContainSubstring("cluster_state:ok"))
			Expect(output).To(ContainSubstring("cluster_slots_assigned:16384"))

			verifyClusterTopologySync(testNamespace, crName, 6)
		})
	})

	Context("Continuous Multi-Pod Failure Resilience", Ordered, func() {
		const crName = "chaos-cluster-multipod"
		var chaosPodName string
		// 5 iterations usually complete in ~3-4 minutes.
		const testDuration = 5 * time.Minute

		BeforeAll(func() {
			By("creating a 3-shard cluster with 1 replica per shard")
			cr := fmt.Sprintf(`
apiVersion: littlered.chuck-chuck-chuck.net/v1alpha1
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

			By("deploying chaos client pod simultaneously")
			chaosPodName, err = deployChaosClient(testNamespace, "multipod", crName, true, "chaos-multi", testDuration)
			Expect(err).NotTo(HaveOccurred())

			By("waiting for cluster to be ready and ok")
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "littlered", crName,
					"-n", testNamespace, "-o", "jsonpath={.status.phase}")
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).To(Equal("Running"))

				cmd = exec.Command("kubectl", "exec", crName+"-cluster-0",
					"-n", testNamespace, "-c", "redis", "--",
					"valkey-cli", "CLUSTER", "INFO")
				output, err = utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).To(ContainSubstring("cluster_state:ok"))
			}, 5*time.Minute, 5*time.Second).Should(Succeed())

			By("waiting 10 seconds for baseline traffic")
			time.Sleep(10 * time.Second)
		})

		AfterAll(func() {
			deleteChaosClient(testNamespace, chaosPodName)
			cmd := exec.Command("kubectl", "delete", "littlered", crName, "-n", testNamespace, "--ignore-not-found", "--timeout=2m")
			_, _ = utils.Run(cmd)
		})

		It("should survive multiple rounds of random pod deletions without data loss", func() {
			const iterations = 5
			for i := 1; i <= iterations; i++ {
				By(fmt.Sprintf("=== Chaos Round %d/%d ===", i, iterations))

				shardGroups, err := getShardGroups(testNamespace, crName, 6)
				Expect(err).NotTo(HaveOccurred())
				
				victims := make([]string, 0)
				for _, group := range shardGroups {
					if len(group) < 2 {
						continue 
					}
					// 50% chance to kill a pod in this shard
					if rand.Intn(2) == 0 {
						victimIdx := rand.Intn(len(group))
						victims = append(victims, group[victimIdx])
					}
				}
				
				if len(victims) == 0 {
					victims = append(victims, shardGroups[0][0])
				}

				By(fmt.Sprintf("Deleting victims: %v", victims))
				args := append([]string{"delete", "pod", "-n", testNamespace, "--grace-period=0", "--force"}, victims...)
				cmd := exec.Command("kubectl", args...)
				_, err = utils.Run(cmd)
				Expect(err).NotTo(HaveOccurred())

				By("Waiting for cluster to recover")
				Eventually(func(g Gomega) {
					cmd := exec.Command("kubectl", "get", "littlered", crName,
						"-n", testNamespace, "-o", "jsonpath={.status.phase}")
					output, err := utils.Run(cmd)
					g.Expect(err).NotTo(HaveOccurred())
					g.Expect(output).To(Equal("Running"))

					var info string
					for j := 0; j < 6; j++ {
						podName := fmt.Sprintf("%s-cluster-%d", crName, j)
						cmd = exec.Command("kubectl", "exec", podName,
							"-n", testNamespace, "-c", "redis", "--",
							"valkey-cli", "CLUSTER", "INFO")
						out, err := utils.Run(cmd)
						if err == nil {
							info = out
							break
						}
					}
					g.Expect(info).To(ContainSubstring("cluster_state:ok"))
					g.Expect(info).To(ContainSubstring("cluster_slots_assigned:16384"))
				}, 5*time.Minute, 5*time.Second).Should(Succeed())

				verifyClusterTopologySync(testNamespace, crName, 6)

				By("Stabilizing for 5 seconds")
				time.Sleep(5 * time.Second)
			}

			err := waitForChaosClientComplete(testNamespace, chaosPodName, testDuration+2*time.Minute)
			Expect(err).NotTo(HaveOccurred())

			metrics, err := getChaosClientMetrics(testNamespace, chaosPodName)
			Expect(err).NotTo(HaveOccurred())
			
			GinkgoWriter.Printf("Final Multi-Pod Chaos Metrics:\n%s\n", metrics.String())

			Expect(metrics.DataCorruptions).To(Equal(int64(0)))
			Expect(metrics.WriteAvailability()).To(BeNumerically(">=", 0.95))
			Expect(metrics.ReadAvailability()).To(BeNumerically(">=", 0.95))
		})
	})
})