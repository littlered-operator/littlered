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

var _ = Describe("Cluster Restart Recovery", Ordered, func() {
	const testNamespace = "littlered-restart-test"

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

	Context("0-Replica Cluster Self-Healing", Ordered, func() {
		const crName = "restart-3node-0replica"

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

		It("should self-heal after a master pod is deleted", func() {
			By("Test ID: CLUST-RESTART-001")
			
			// 1. Write some data to verify it is lost on the restarted node
			By("writing data to all masters")
			for i := 0; i < 3; i++ {
				key := fmt.Sprintf("key-%d", i)
				// We don't know which node holds which slot, but -c handles redirect
				cmd := exec.Command("kubectl", "exec", crName+"-cluster-0",
					"-n", testNamespace, "-c", "redis", "--",
					"valkey-cli", "-c", "SET", key, "value")
				_, err := utils.Run(cmd)
				Expect(err).NotTo(HaveOccurred())
			}

			// 2. Identify a victim
			victimPod := crName + "-cluster-2"
			By(fmt.Sprintf("deleting master pod %s", victimPod))
			cmd := exec.Command("kubectl", "delete", "pod", victimPod,
				"-n", testNamespace, "--grace-period=0", "--force")
			_, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())

			// 3. Wait for pod to return
			By("waiting for pod to be running again")
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "pod", victimPod,
					"-n", testNamespace, "-o", "jsonpath={.status.phase}")
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).To(Equal("Running"))
			}, 2*time.Minute, 5*time.Second).Should(Succeed())

			// 4. Verify self-healing
			// The operator should:
			// - Detect the new node has no slots (orphaned slots in cluster)
			// - Re-assign the missing slots to the empty master
			// - Meet the new node with the cluster
			// - Forget the old ghost node
			
			By("waiting for cluster state to become OK")
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "exec", crName+"-cluster-0",
					"-n", testNamespace, "-c", "redis", "--",
					"valkey-cli", "CLUSTER", "INFO")
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				// We expect the cluster to heal and become OK
				g.Expect(output).To(ContainSubstring("cluster_state:ok"))
				g.Expect(output).To(ContainSubstring("cluster_slots_assigned:16384"))
				g.Expect(output).To(ContainSubstring("cluster_known_nodes:3"))
			}, 5*time.Minute, 10*time.Second).Should(Succeed())

			// 5. Verify Ghost Node Cleanup
			// We check CLUSTER NODES to ensure we don't have "fail" nodes lingering
			By("verifying ghost nodes are gone")
			cmd = exec.Command("kubectl", "exec", crName+"-cluster-0",
				"-n", testNamespace, "-c", "redis", "--",
				"valkey-cli", "CLUSTER", "NODES")
			output, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())
			
			// We expect exactly 3 nodes
			lines := strings.Split(strings.TrimSpace(output), "\n")
			Expect(len(lines)).To(Equal(3), fmt.Sprintf("Should have exactly 3 nodes, got:\n%s", output))
			
			// None should be in 'fail' state (except transiently PFAIL)
			Expect(output).NotTo(ContainSubstring("fail"), "Should not have failed nodes")
		})
	})
})
