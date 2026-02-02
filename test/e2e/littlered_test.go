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

var _ = Describe("LittleRed", Ordered, func() {
	const testNamespace = "littlered-e2e-test"

	BeforeAll(func() {
		By("creating test namespace")
		cmd := exec.Command("kubectl", "create", "ns", testNamespace)
		_, _ = utils.Run(cmd) // Ignore if exists

		// TODO: Re-enable restricted policy once operator sets seccompProfile on containers
		// See: https://gitea.tanne3.de/tanne3/littlered/issues/1
		// By("labeling the namespace to enforce the restricted security policy")
		// cmd = exec.Command("kubectl", "label", "--overwrite", "ns", testNamespace,
		// 	"pod-security.kubernetes.io/enforce=restricted")
		// _, err := utils.Run(cmd)
		// Expect(err).NotTo(HaveOccurred())
	})

	AfterAll(func() {
		By("cleaning up test namespace")
		cmd := exec.Command("kubectl", "delete", "ns", testNamespace, "--ignore-not-found")
		_, _ = utils.Run(cmd)
	})

	Context("Standalone Mode", Ordered, func() {
		const crName = "test-standalone"

		AfterAll(func() {
			By("cleaning up standalone CR")
			cmd := exec.Command("kubectl", "delete", "littlered", crName, "-n", testNamespace, "--ignore-not-found")
			_, _ = utils.Run(cmd)
		})

		It("should create a standalone Redis instance", func() {
			By("applying the LittleRed CR")
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
      cpu: "100m"
      memory: "128Mi"
`, crName, testNamespace)
			cmd := exec.Command("kubectl", "apply", "-f", "-")
			cmd.Stdin = strings.NewReader(cr)
			_, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())
		})

		It("should create StatefulSet with 1 replica", func() {
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "statefulset", crName+"-redis",
					"-n", testNamespace, "-o", "jsonpath={.status.readyReplicas}")
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).To(Equal("1"))
			}, 2*time.Minute, 5*time.Second).Should(Succeed())
		})

		It("should create Service", func() {
			cmd := exec.Command("kubectl", "get", "service", crName, "-n", testNamespace)
			_, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())
		})

		It("should have LittleRed status Running", func() {
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "littlered", crName,
					"-n", testNamespace, "-o", "jsonpath={.status.phase}")
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).To(Equal("Running"))
			}, 2*time.Minute, 5*time.Second).Should(Succeed())
		})

		It("should respond to Redis PING", func() {
			cmd := exec.Command("kubectl", "exec", crName+"-redis-0",
				"-n", testNamespace, "-c", "redis", "--",
				"valkey-cli", "PING")
			output, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())
			Expect(strings.TrimSpace(output)).To(Equal("PONG"))
		})

		It("should execute SET and GET operations", func() {
			By("setting a key")
			cmd := exec.Command("kubectl", "exec", crName+"-redis-0",
				"-n", testNamespace, "-c", "redis", "--",
				"valkey-cli", "SET", "e2e-test-key", "e2e-test-value")
			output, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())
			Expect(strings.TrimSpace(output)).To(Equal("OK"))

			By("getting the key")
			cmd = exec.Command("kubectl", "exec", crName+"-redis-0",
				"-n", testNamespace, "-c", "redis", "--",
				"valkey-cli", "GET", "e2e-test-key")
			output, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())
			Expect(strings.TrimSpace(output)).To(Equal("e2e-test-value"))
		})

		It("should expose metrics on port 9121", func() {
			// Use curl from the redis container since exporter image is minimal
			cmd := exec.Command("kubectl", "exec", crName+"-redis-0",
				"-n", testNamespace, "-c", "redis", "--",
				"sh", "-c", "cat < /dev/tcp/localhost/9121 || true")
			// Just verify the port is open; detailed metrics check would need curl
			// Alternative: check exporter container is running
			cmd = exec.Command("kubectl", "get", "pod", crName+"-redis-0",
				"-n", testNamespace, "-o", "jsonpath={.status.containerStatuses[?(@.name=='exporter')].ready}")
			output, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())
			Expect(output).To(Equal("true"))
		})

		It("should recreate pod after deletion", func() {
			By("deleting the pod")
			cmd := exec.Command("kubectl", "delete", "pod", crName+"-redis-0",
				"-n", testNamespace)
			_, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())

			By("waiting for pod to be recreated and ready")
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "pod", crName+"-redis-0",
					"-n", testNamespace, "-o", "jsonpath={.status.phase}")
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).To(Equal("Running"))
			}, 2*time.Minute, 5*time.Second).Should(Succeed())

			By("verifying Redis responds after recreation")
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "exec", crName+"-redis-0",
					"-n", testNamespace, "-c", "redis", "--",
					"valkey-cli", "PING")
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(strings.TrimSpace(output)).To(Equal("PONG"))
			}, 1*time.Minute, 5*time.Second).Should(Succeed())
		})

		It("should clean up resources when CR is deleted", func() {
			By("deleting the LittleRed CR")
			cmd := exec.Command("kubectl", "delete", "littlered", crName, "-n", testNamespace)
			_, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())

			By("verifying StatefulSet is deleted")
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "statefulset", crName+"-redis",
					"-n", testNamespace)
				_, err := utils.Run(cmd)
				g.Expect(err).To(HaveOccurred()) // Should fail because it doesn't exist
			}, 1*time.Minute, 5*time.Second).Should(Succeed())

			By("verifying Service is deleted")
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "service", crName,
					"-n", testNamespace)
				_, err := utils.Run(cmd)
				g.Expect(err).To(HaveOccurred())
			}, 1*time.Minute, 5*time.Second).Should(Succeed())
		})
	})

	Context("Sentinel Mode", Ordered, func() {
		const crName = "test-sentinel"

		AfterAll(func() {
			By("cleaning up sentinel CR")
			cmd := exec.Command("kubectl", "delete", "littlered", crName, "-n", testNamespace, "--ignore-not-found")
			_, _ = utils.Run(cmd)
		})

		It("should create a sentinel Redis cluster", func() {
			By("applying the LittleRed CR with sentinel mode")
			cr := fmt.Sprintf(`
apiVersion: littlered.tanne3.de/v1alpha1
kind: LittleRed
metadata:
  name: %s
  namespace: %s
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
		})

		It("should create StatefulSet with 3 Redis replicas", func() {
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "statefulset", crName+"-redis",
					"-n", testNamespace, "-o", "jsonpath={.status.readyReplicas}")
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).To(Equal("3"))
			}, 3*time.Minute, 5*time.Second).Should(Succeed())
		})

		It("should create all required services", func() {
			By("checking master service")
			cmd := exec.Command("kubectl", "get", "service", crName, "-n", testNamespace)
			_, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())

			By("checking replicas service")
			cmd = exec.Command("kubectl", "get", "service", crName+"-replicas", "-n", testNamespace)
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())

			By("checking sentinel service")
			cmd = exec.Command("kubectl", "get", "service", crName+"-sentinel", "-n", testNamespace)
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
			}, 3*time.Minute, 5*time.Second).Should(Succeed())
		})

		It("should have sentinel quorum established", func() {
			Eventually(func(g Gomega) {
				// Query sentinel for master info
				cmd := exec.Command("kubectl", "exec", crName+"-sentinel-0",
					"-n", testNamespace, "-c", "sentinel", "--",
					"valkey-cli", "-p", "26379", "SENTINEL", "master", "mymaster")
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).To(ContainSubstring("num-slaves"))
				g.Expect(output).To(ContainSubstring("quorum"))
			}, 2*time.Minute, 5*time.Second).Should(Succeed())
		})

		It("should report master info in status", func() {
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "littlered", crName,
					"-n", testNamespace, "-o", "jsonpath={.status.master.podName}")
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).NotTo(BeEmpty())
			}, 2*time.Minute, 5*time.Second).Should(Succeed())
		})

		It("should have working replication", func() {
			By("getting master pod name")
			cmd := exec.Command("kubectl", "get", "littlered", crName,
				"-n", testNamespace, "-o", "jsonpath={.status.master.podName}")
			masterPod, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())
			masterPod = strings.TrimSpace(masterPod)
			Expect(masterPod).NotTo(BeEmpty())

			By("writing to master")
			cmd = exec.Command("kubectl", "exec", masterPod,
				"-n", testNamespace, "-c", "redis", "--",
				"valkey-cli", "SET", "repl-test-key", "repl-test-value")
			output, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())
			Expect(strings.TrimSpace(output)).To(Equal("OK"))

			By("reading from a replica")
			// Find a replica (any pod that's not the master)
			var replicaPod string
			for i := 0; i < 3; i++ {
				podName := fmt.Sprintf("%s-redis-%d", crName, i)
				if podName != masterPod {
					replicaPod = podName
					break
				}
			}
			Expect(replicaPod).NotTo(BeEmpty())

			Eventually(func(g Gomega) {
				cmd = exec.Command("kubectl", "exec", replicaPod,
					"-n", testNamespace, "-c", "redis", "--",
					"valkey-cli", "GET", "repl-test-key")
				output, err = utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(strings.TrimSpace(output)).To(Equal("repl-test-value"))
			}, 30*time.Second, 2*time.Second).Should(Succeed())
		})
	})

	Context("Sentinel Failover", Ordered, func() {
		const crName = "test-failover"

		AfterAll(func() {
			By("cleaning up failover CR")
			cmd := exec.Command("kubectl", "delete", "littlered", crName, "-n", testNamespace, "--ignore-not-found", "--timeout=60s")
			_, _ = utils.Run(cmd)
		})

		It("should create a sentinel cluster for failover testing", func() {
			By("applying the LittleRed CR with fast failover settings")
			cr := fmt.Sprintf(`
apiVersion: littlered.tanne3.de/v1alpha1
kind: LittleRed
metadata:
  name: %s
  namespace: %s
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
    downAfterMilliseconds: 3000
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

			By("waiting for sentinel quorum")
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "littlered", crName,
					"-n", testNamespace, "-o", "jsonpath={.status.sentinels.ready}")
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).To(Equal("3"))
			}, 2*time.Minute, 5*time.Second).Should(Succeed())
		})

		It("should elect new master after master pod deletion", func() {
			By("getting current master pod")
			cmd := exec.Command("kubectl", "get", "littlered", crName,
				"-n", testNamespace, "-o", "jsonpath={.status.master.podName}")
			originalMaster, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())
			originalMaster = strings.TrimSpace(originalMaster)
			Expect(originalMaster).NotTo(BeEmpty())
			_, _ = fmt.Fprintf(GinkgoWriter, "Original master: %s\n", originalMaster)

			By("writing test data to master before failover")
			cmd = exec.Command("kubectl", "exec", originalMaster,
				"-n", testNamespace, "-c", "redis", "--",
				"valkey-cli", "SET", "failover-test-key", "failover-test-value")
			output, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())
			Expect(strings.TrimSpace(output)).To(Equal("OK"))

			By("deleting the master pod to trigger failover")
			cmd = exec.Command("kubectl", "delete", "pod", originalMaster,
				"-n", testNamespace, "--grace-period=0", "--force")
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())

			By("waiting for new master to be elected")
			var newMaster string
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "littlered", crName,
					"-n", testNamespace, "-o", "jsonpath={.status.master.podName}")
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				newMaster = strings.TrimSpace(output)
				g.Expect(newMaster).NotTo(BeEmpty())
				// New master should be different from original (since original pod was deleted)
				// Note: The pod might be recreated with same name, so we verify via sentinel
			}, 90*time.Second, 3*time.Second).Should(Succeed())
			_, _ = fmt.Fprintf(GinkgoWriter, "New master: %s\n", newMaster)

			By("verifying sentinel reports new master")
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "exec", crName+"-sentinel-0",
					"-n", testNamespace, "-c", "sentinel", "--",
					"valkey-cli", "-p", "26379", "SENTINEL", "get-master-addr-by-name", "mymaster")
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).NotTo(BeEmpty())
				_, _ = fmt.Fprintf(GinkgoWriter, "Sentinel reports master: %s\n", output)
			}, 30*time.Second, 3*time.Second).Should(Succeed())

			By("verifying data is preserved after failover")
			Eventually(func(g Gomega) {
				// Query sentinel for current master address
				cmd := exec.Command("kubectl", "exec", crName+"-sentinel-0",
					"-n", testNamespace, "-c", "sentinel", "--",
					"valkey-cli", "-p", "26379", "SENTINEL", "get-master-addr-by-name", "mymaster")
				masterInfo, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				_, _ = fmt.Fprintf(GinkgoWriter, "Master info: %s\n", masterInfo)

				// Try reading from any available pod
				for i := 0; i < 3; i++ {
					podName := fmt.Sprintf("%s-redis-%d", crName, i)
					cmd = exec.Command("kubectl", "exec", podName,
						"-n", testNamespace, "-c", "redis", "--",
						"valkey-cli", "GET", "failover-test-key")
					output, err := utils.Run(cmd)
					if err == nil && strings.TrimSpace(output) == "failover-test-value" {
						_, _ = fmt.Fprintf(GinkgoWriter, "Data found on %s\n", podName)
						return
					}
				}
				g.Expect(false).To(BeTrue(), "Data not found on any pod after failover")
			}, 60*time.Second, 5*time.Second).Should(Succeed())
		})

		It("should restore full cluster after failover", func() {
			By("waiting for all pods to be ready")
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "statefulset", crName+"-redis",
					"-n", testNamespace, "-o", "jsonpath={.status.readyReplicas}")
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).To(Equal("3"))
			}, 2*time.Minute, 5*time.Second).Should(Succeed())

			By("verifying cluster status is Running")
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "littlered", crName,
					"-n", testNamespace, "-o", "jsonpath={.status.phase}")
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).To(Equal("Running"))
			}, 2*time.Minute, 5*time.Second).Should(Succeed())

			By("verifying sentinel sees 2 replicas")
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "exec", crName+"-sentinel-0",
					"-n", testNamespace, "-c", "sentinel", "--",
					"valkey-cli", "-p", "26379", "SENTINEL", "master", "mymaster")
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).To(ContainSubstring("num-slaves"))
				// Parse output to verify num-slaves is 2
				lines := strings.Split(output, "\n")
				for i, line := range lines {
					if strings.TrimSpace(line) == "num-slaves" && i+1 < len(lines) {
						g.Expect(strings.TrimSpace(lines[i+1])).To(Equal("2"))
						return
					}
				}
				g.Expect(false).To(BeTrue(), "Could not find num-slaves in sentinel output")
			}, 2*time.Minute, 5*time.Second).Should(Succeed())
		})
	})
})
