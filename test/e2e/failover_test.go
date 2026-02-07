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

var _ = Describe("Sentinel Advanced Failover", Ordered, func() {
	const testNamespace = "littlered-failover-test"
	const crName = "advanced-failover"

	BeforeAll(func() {
		By("creating test namespace")
		cmd := exec.Command("kubectl", "create", "ns", testNamespace)
		_, _ = utils.Run(cmd)
	})

	AfterAll(func() {
		By("cleaning up test namespace")
		cmd := exec.Command("kubectl", "delete", "ns", testNamespace, "--ignore-not-found")
		_, _ = utils.Run(cmd)
	})

	Context("Event-Driven Recovery", Ordered, func() {
		BeforeAll(func() {
			By("deploying a Sentinel cluster with aggressive timeouts")
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
    downAfterMilliseconds: 5000  # Fast failure detection
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

		It("should immediately route traffic to new master after double failover", func() {
			By("Step 1: Identify initial master")
			cmd := exec.Command("kubectl", "get", "littlered", crName,
				"-n", testNamespace, "-o", "jsonpath={.status.master.podName}")
			initialMaster, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())
			initialMaster = strings.TrimSpace(initialMaster)
			fmt.Fprintf(GinkgoWriter, "Initial Master: %s
", initialMaster)

			By("Step 2: Start continuous write loop to Service")
			// We use a background process to write to the SERVICE (not the pod)
			// This verifies that the Operator updates the service endpoints correctly
			stopCh := make(chan struct{})
			errCh := make(chan error)
			
			go func() {
				defer close(errCh)
				clientPod := "client-" + crName
				// Ensure client pod exists
				setupCmd := exec.Command("kubectl", "run", clientPod, "--image=redis:alpine", 
					"-n", testNamespace, "--restart=Never", "--", "sleep", "3600")
				utils.Run(setupCmd)
				
				// Wait for client running
				time.Sleep(10 * time.Second)

				// Loop writes
				i := 0
				for {
					select {
					case <-stopCh:
						return
					default:
						i++
						key := fmt.Sprintf("continuous-key-%d", i)
						// Write to the SERVICE
						writeCmd := exec.Command("kubectl", "exec", clientPod, "-n", testNamespace, "--",
							"redis-cli", "-h", crName, "SET", key, "val", "EX", "60")
						_, err := utils.Run(writeCmd)
						if err != nil {
							// Log error but keep trying (expected during failover window)
							// fmt.Fprintf(GinkgoWriter, "Write failed at %d: %v
", i, err)
						}
						time.Sleep(500 * time.Millisecond)
					}
				}
			}()

			By("Step 3: Kill the Master (Failover 1)")
			cmd = exec.Command("kubectl", "delete", "pod", initialMaster, 
				"-n", testNamespace, "--grace-period=0", "--force")
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())

			By("Step 4: Wait for new master to be elected")
			var secondMaster string
			Eventually(func(g Gomega) {
				// Ask Sentinel who is master
				cmd := exec.Command("kubectl", "exec", crName+"-sentinel-0",
					"-n", testNamespace, "-c", "sentinel", "--",
					"redis-cli", "-p", "26379", "SENTINEL", "get-master-addr-by-name", "mymaster")
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				
				// Find the pod name from the IP in output
				// Output format is usually IP
PORT
				lines := strings.Split(strings.TrimSpace(output), "
")
				if len(lines) > 0 {
					ip := lines[0]
					// Find pod by IP
					podCmd := exec.Command("kubectl", "get", "pods", "-n", testNamespace, 
						"-o", "jsonpath={.items[?(@.status.podIP=='"+ip+"')].metadata.name}")
					podName, _ := utils.Run(podCmd)
					podName = strings.TrimSpace(podName)
					
					if podName != "" && podName != initialMaster {
						secondMaster = podName
						return // Found new master
					}
				}
				g.Fail("New master not yet elected or found")
			}, 1*time.Minute, 2*time.Second).Should(Succeed())
			
			fmt.Fprintf(GinkgoWriter, "Second Master: %s
", secondMaster)

			By("Step 5: Verify Service routing to Second Master")
			// The operator should have updated labels by now via event
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "pod", secondMaster, "-n", testNamespace, 
					"-o", "jsonpath={.metadata.labels.littlered\.tanne3\.de/role}")
				role, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(role).To(Equal("master"))
			}, 10*time.Second, 1*time.Second).Should(Succeed(), "Operator failed to update master label quickly")

			By("Step 6: Kill the Second Master IMMEDIATELY (Failover 2)")
			cmd = exec.Command("kubectl", "delete", "pod", secondMaster, 
				"-n", testNamespace, "--grace-period=0", "--force")
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())

			By("Step 7: Wait for Third Master")
			var thirdMaster string
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "exec", crName+"-sentinel-0",
					"-n", testNamespace, "-c", "sentinel", "--",
					"redis-cli", "-p", "26379", "SENTINEL", "get-master-addr-by-name", "mymaster")
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				
				lines := strings.Split(strings.TrimSpace(output), "
")
				if len(lines) > 0 {
					ip := lines[0]
					podCmd := exec.Command("kubectl", "get", "pods", "-n", testNamespace, 
						"-o", "jsonpath={.items[?(@.status.podIP=='"+ip+"')].metadata.name}")
					podName, _ := utils.Run(podCmd)
					podName = strings.TrimSpace(podName)
					
					if podName != "" && podName != secondMaster && podName != initialMaster {
						thirdMaster = podName
						return
					}
				}
				g.Fail("Third master not elected")
			}, 1*time.Minute, 2*time.Second).Should(Succeed())
			fmt.Fprintf(GinkgoWriter, "Third Master: %s
", thirdMaster)

			By("Step 8: Verify Service routing to Third Master")
			Eventually(func(g Gomega) {
				// Check that a write to the SERVICE succeeds
				clientPod := "client-" + crName
				writeCmd := exec.Command("kubectl", "exec", clientPod, "-n", testNamespace, "--",
					"redis-cli", "-h", crName, "SET", "final-check", "ok", "EX", "60")
				out, err := utils.Run(writeCmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(strings.TrimSpace(out)).To(Equal("OK"))
			}, 20*time.Second, 1*time.Second).Should(Succeed(), "Service traffic not routing to third master")

			close(stopCh) // Stop writer
		})
	})
})
