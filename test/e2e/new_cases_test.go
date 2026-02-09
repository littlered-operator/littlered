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

var _ = Describe("LittleRed New E2E Cases", Ordered, func() {
	const testNamespace = "littlered-new-e2e-test"

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

	Context("Debug Research Mode", Ordered, func() {
		const crName = "empty-cluster-no-slots"

		BeforeAll(func() {
			By("Creating an empty cluster with 3 masters, no slots, no replicas")
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

		AfterAll(func() {
			By("cleaning up cluster CR")
			cmd := exec.Command("kubectl", "delete", "littlered", crName, "-n", testNamespace, "--ignore-not-found", "--timeout=1m")
			_, _ = utils.Run(cmd)
		})

		It("should create StatefulSet with 3 replicas", func() {
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "statefulset", crName+"-cluster",
					"-n", testNamespace, "-o", "jsonpath={.status.readyReplicas}")
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).To(Equal("3"))
			}, 4*time.Minute, 5*time.Second).Should(Succeed())
		})

		It("should have cluster state 'fail' (no slots assigned)", func() {
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

		It("should have 3 known nodes", func() {
			cmd := exec.Command("kubectl", "exec", crName+"-cluster-0",
				"-n", testNamespace, "-c", "redis", "--",
				"valkey-cli", "CLUSTER", "INFO")
			output, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())
			Expect(output).To(ContainSubstring("cluster_known_nodes:3"))
		})

		It("should show 3 masters with no slots in CLUSTER NODES", func() {
			cmd := exec.Command("kubectl", "exec", crName+"-cluster-0",
				"-n", testNamespace, "-c", "redis", "--",
				"valkey-cli", "CLUSTER", "NODES")
			output, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())

			lines := strings.Split(strings.TrimSpace(output), "\n")
			masterCount := 0
			for _, line := range lines {
				if strings.Contains(line, "master") {
					masterCount++
					// Check that there are no slots assigned (slots appear at the end of the line)
					fields := strings.Fields(line)
					// CLUSTER NODES format: <id> <ip:port@cport> <flags> <master> <ping-sent> <pong-recv> <config-epoch> <link-state> [slots...]
					// Minimum fields is 8. If slots are present, there are more.
					Expect(len(fields)).To(Equal(8), fmt.Sprintf("Master node should have no slots: %s", line))
				}
			}
			Expect(masterCount).To(Equal(3))
		})
	})

	Context("Functional 3-node cluster (No replicas)", Ordered, func() {
		const crName = "functional-3node-cluster"

		BeforeAll(func() {
			By("Creating a functional 3-shard cluster with no replicas")
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
		})

		AfterAll(func() {
			By("cleaning up cluster CR")
			cmd := exec.Command("kubectl", "delete", "littlered", crName, "-n", testNamespace, "--ignore-not-found", "--timeout=1m")
			_, _ = utils.Run(cmd)
		})

		It("should have cluster state 'ok'", func() {
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "exec", crName+"-cluster-0",
					"-n", testNamespace, "-c", "redis", "--",
					"valkey-cli", "CLUSTER", "INFO")
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).To(ContainSubstring("cluster_state:ok"))
				g.Expect(output).To(ContainSubstring("cluster_slots_assigned:16384"))
			}, 4*time.Minute, 5*time.Second).Should(Succeed())
		})

		It("should have 3 masters with slots in CLUSTER NODES", func() {
			cmd := exec.Command("kubectl", "exec", crName+"-cluster-0",
				"-n", testNamespace, "-c", "redis", "--",
				"valkey-cli", "CLUSTER", "NODES")
			output, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())

			lines := strings.Split(strings.TrimSpace(output), "\n")
			masterCount := 0
			for _, line := range lines {
				if strings.Contains(line, "master") {
					masterCount++
					fields := strings.Fields(line)
					// If slots are present, fields length should be > 8
					Expect(len(fields)).To(BeNumerically(">", 8), fmt.Sprintf("Master node should have slots: %s", line))
				}
			}
			Expect(masterCount).To(Equal(3))
		})

		It("should be able to set and get a key", func() {
			By("setting a key")
			cmd := exec.Command("kubectl", "exec", crName+"-cluster-0",
				"-n", testNamespace, "-c", "redis", "--",
				"valkey-cli", "-c", "SET", "foo", "bar")
			output, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())
			Expect(strings.TrimSpace(output)).To(Equal("OK"))

			By("getting the key")
			cmd = exec.Command("kubectl", "exec", crName+"-cluster-0",
				"-n", testNamespace, "-c", "redis", "--",
				"valkey-cli", "-c", "GET", "foo")
			output, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())
			Expect(strings.TrimSpace(output)).To(Equal("bar"))
		})
	})
})
