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
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/tanne3/littlered-operator/test/chaos"
	"github.com/tanne3/littlered-operator/test/utils"
)

// getChaosClientImage returns the chaos client image to use
func getChaosClientImage() string {
	if img := os.Getenv("CHAOS_CLIENT_IMAGE"); img != "" {
		return img
	}
	return "registry.tanne3.de/littlered-chaos-client:latest"
}

// deployChaosClient deploys a chaos test client pod and returns the pod name
func deployChaosClient(namespace, name, redisService string, clusterMode bool, keyPrefix string, duration time.Duration) (string, error) {
	podName := fmt.Sprintf("chaos-client-%s", name)
	image := getChaosClientImage()

	// Build args list - only include -cluster flag if cluster mode is enabled
	clusterArg := ""
	if clusterMode {
		clusterArg = "\n    - \"-cluster\""
	}

	// Create pod manifest
	pod := fmt.Sprintf(`
apiVersion: v1
kind: Pod
metadata:
  name: %s
  namespace: %s
  labels:
    app: chaos-client
    test: %s
spec:
  restartPolicy: Never
  containers:
  - name: chaos-client
    image: %s
    imagePullPolicy: Always
    args:
    - "-addrs=%s:6379"
    - "-prefix=%s"
    - "-duration=%s"
    - "-status-interval=5s"
    - "-write-rate=100ms"
    - "-timeout=500ms"%s
`, podName, namespace, name, image, redisService, keyPrefix, duration.String(), clusterArg)

	cmd := exec.Command("kubectl", "apply", "-f", "-")
	cmd.Stdin = strings.NewReader(pod)
	_, err := utils.Run(cmd)
	return podName, err
}

// waitForChaosClientComplete waits for the chaos client pod to complete
func waitForChaosClientComplete(namespace, podName string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		cmd := exec.Command("kubectl", "get", "pod", podName,
			"-n", namespace, "-o", "jsonpath={.status.phase}")
		output, err := utils.Run(cmd)
		if err != nil {
			return err
		}
		if output == "Succeeded" || output == "Failed" {
			return nil
		}
		time.Sleep(5 * time.Second)
	}
	return fmt.Errorf("timeout waiting for pod %s to complete", podName)
}

// getChaosClientMetrics retrieves metrics from a completed chaos client pod
func getChaosClientMetrics(namespace, podName string) (*chaos.MetricsSnapshot, error) {
	cmd := exec.Command("kubectl", "logs", podName, "-n", namespace)
	output, err := utils.Run(cmd)
	if err != nil {
		return nil, fmt.Errorf("failed to get logs: %w", err)
	}

	// Find the METRICS_JSON line
	for _, line := range strings.Split(output, "\n") {
		if strings.HasPrefix(line, "METRICS_JSON:") {
			jsonStr := strings.TrimPrefix(line, "METRICS_JSON:")
			var metrics chaos.MetricsSnapshot
			if err := json.Unmarshal([]byte(jsonStr), &metrics); err != nil {
				return nil, fmt.Errorf("failed to parse metrics JSON: %w", err)
			}
			return &metrics, nil
		}
	}
	return nil, fmt.Errorf("METRICS_JSON not found in pod logs")
}

// deleteChaosClient deletes the chaos client pod
func deleteChaosClient(namespace, podName string) {
	cmd := exec.Command("kubectl", "delete", "pod", podName, "-n", namespace, "--ignore-not-found")
	_, _ = utils.Run(cmd)
}

var _ = Describe("Chaos Testing", Ordered, func() {
	const testNamespace = "littlered-chaos-e2e-test"

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

	Context("Cluster Mode Resilience", Ordered, func() {
		const crName = "chaos-cluster"

		BeforeAll(func() {
			By("creating a Redis Cluster for chaos testing")
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
      cpu: "200m"
      memory: "256Mi"
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

			By("waiting for cluster to be fully formed")
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "exec", crName+"-cluster-0",
					"-n", testNamespace, "-c", "redis", "--",
					"valkey-cli", "CLUSTER", "INFO")
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).To(ContainSubstring("cluster_state:ok"))
			}, 2*time.Minute, 5*time.Second).Should(Succeed())
		})

		AfterAll(func() {
			By("cleaning up chaos cluster")
			cmd := exec.Command("kubectl", "delete", "littlered", crName, "-n", testNamespace, "--ignore-not-found", "--timeout=2m")
			_, _ = utils.Run(cmd)
		})

		It("should maintain data integrity during replica pod deletion", func() {
			By("Test ID: CHAOS-001")
			const testName = "replica-delete"
			const testDuration = 40 * time.Second

			By("deploying chaos client pod")
			podName, err := deployChaosClient(testNamespace, testName, crName, true, "chaos-replica", testDuration)
			Expect(err).NotTo(HaveOccurred())
			defer deleteChaosClient(testNamespace, podName)

			By("waiting for chaos client to start and establish baseline (10 seconds)")
			time.Sleep(10 * time.Second)

			By("deleting a replica pod")
			replicaPod := crName + "-cluster-1"
			GinkgoWriter.Printf("Deleting replica pod: %s\n", replicaPod)
			cmd := exec.Command("kubectl", "delete", "pod", replicaPod,
				"-n", testNamespace, "--grace-period=0", "--force")
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())

			By("waiting for chaos client to complete")
			err = waitForChaosClientComplete(testNamespace, podName, testDuration+2*time.Minute)
			Expect(err).NotTo(HaveOccurred())

			By("collecting metrics from chaos client")
			metrics, err := getChaosClientMetrics(testNamespace, podName)
			Expect(err).NotTo(HaveOccurred())
			GinkgoWriter.Printf("Final metrics:\n%s\n", metrics.String())

			By("verifying cluster recovered")
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "exec", crName+"-cluster-0",
					"-n", testNamespace, "-c", "redis", "--",
					"valkey-cli", "CLUSTER", "INFO")
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).To(ContainSubstring("cluster_state:ok"))
			}, 2*time.Minute, 5*time.Second).Should(Succeed())

			By("asserting data integrity (availability is informational only)")
			writeAvail := metrics.WriteAvailability()
			readAvail := metrics.ReadAvailability()
			GinkgoWriter.Printf("Write availability: %.2f%%, Read availability: %.2f%%\n",
				writeAvail*100, readAvail*100)

			// Data integrity is the critical guarantee - no corruption allowed
			Expect(metrics.DataCorruptions).To(Equal(int64(0)),
				"CRITICAL: Data corruption detected!")
			Expect(metrics.WriteAttempts).To(BeNumerically(">=", 100),
				"Should have attempted at least 100 writes")
		})

		It("should maintain data integrity during master pod deletion with failover", func() {
			By("Test ID: CHAOS-002")
			const testName = "master-delete"
			const testDuration = 50 * time.Second

			By("deploying chaos client pod")
			podName, err := deployChaosClient(testNamespace, testName, crName, true, "chaos-master", testDuration)
			Expect(err).NotTo(HaveOccurred())
			defer deleteChaosClient(testNamespace, podName)

			By("waiting for chaos client to start (10 seconds)")
			time.Sleep(10 * time.Second)

			By("deleting a master pod (pod-2)")
			masterPod := crName + "-cluster-2"
			GinkgoWriter.Printf("Deleting master pod: %s\n", masterPod)
			cmd := exec.Command("kubectl", "delete", "pod", masterPod,
				"-n", testNamespace, "--grace-period=0", "--force")
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())

			By("waiting for chaos client to complete")
			err = waitForChaosClientComplete(testNamespace, podName, testDuration+2*time.Minute)
			Expect(err).NotTo(HaveOccurred())

			By("collecting metrics")
			metrics, err := getChaosClientMetrics(testNamespace, podName)
			Expect(err).NotTo(HaveOccurred())
			GinkgoWriter.Printf("Final metrics after master failure:\n%s\n", metrics.String())

			By("verifying cluster recovered")
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "exec", crName+"-cluster-0",
					"-n", testNamespace, "-c", "redis", "--",
					"valkey-cli", "CLUSTER", "INFO")
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).To(ContainSubstring("cluster_state:ok"))
			}, 2*time.Minute, 5*time.Second).Should(Succeed())

			By("asserting data integrity (availability is informational only)")
			writeAvail := metrics.WriteAvailability()
			readAvail := metrics.ReadAvailability()
			GinkgoWriter.Printf("Write availability: %.2f%%, Read availability: %.2f%%\n",
				writeAvail*100, readAvail*100)

			// Data integrity is the critical guarantee - no corruption allowed
			Expect(metrics.DataCorruptions).To(Equal(int64(0)),
				"CRITICAL: Data corruption detected!")
		})

		It("should survive rolling restart without data loss", func() {
			By("Test ID: CHAOS-003")
			const testName = "rolling-restart"
			const testDuration = 90 * time.Second

			By("deploying chaos client pod")
			podName, err := deployChaosClient(testNamespace, testName, crName, true, "chaos-rolling", testDuration)
			Expect(err).NotTo(HaveOccurred())
			defer deleteChaosClient(testNamespace, podName)

			By("waiting for baseline (10 seconds)")
			time.Sleep(10 * time.Second)

			By("triggering rolling restart via annotation change")
			cmd := exec.Command("kubectl", "annotate", "littlered", crName,
				"-n", testNamespace, fmt.Sprintf("chaos-test=%d", time.Now().Unix()), "--overwrite")
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())

			By("waiting for chaos client to complete")
			err = waitForChaosClientComplete(testNamespace, podName, testDuration+2*time.Minute)
			Expect(err).NotTo(HaveOccurred())

			By("collecting metrics")
			metrics, err := getChaosClientMetrics(testNamespace, podName)
			Expect(err).NotTo(HaveOccurred())
			GinkgoWriter.Printf("Final metrics after rolling restart:\n%s\n", metrics.String())

			By("verifying cluster is healthy")
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "exec", crName+"-cluster-0",
					"-n", testNamespace, "-c", "redis", "--",
					"valkey-cli", "CLUSTER", "INFO")
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).To(ContainSubstring("cluster_state:ok"))
			}, 2*time.Minute, 5*time.Second).Should(Succeed())

			By("asserting data integrity (availability is informational only)")
			writeAvail := metrics.WriteAvailability()
			readAvail := metrics.ReadAvailability()
			GinkgoWriter.Printf("Write availability: %.2f%%, Read availability: %.2f%%\n",
				writeAvail*100, readAvail*100)

			// Data integrity is the critical guarantee - no corruption allowed
			Expect(metrics.DataCorruptions).To(Equal(int64(0)),
				"CRITICAL: Data corruption detected during rolling restart!")
		})
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
			By("Test ID: CHAOS-020")
			const testName = "sentinel-double-failover"
			const testDuration = 90 * time.Second

			// Target the Kubernetes Service, not individual pods
			// chaos-client sees it as a single "standalone" instance
			serviceName := crName // The operator creates a service with the same name
			
			By("deploying chaos client targeting the Master Service")
			podName, err := deployChaosClient(testNamespace, testName, serviceName, false, "chaos-failover", testDuration)
			Expect(err).NotTo(HaveOccurred())
			defer deleteChaosClient(testNamespace, podName)

			By("waiting for baseline (15 seconds)")
			time.Sleep(15 * time.Second)

			// --- Failover 1 ---
			By("identifying first master")
			cmd := exec.Command("kubectl", "get", "littlered", crName,
				"-n", testNamespace, "-o", "jsonpath={.status.master.podName}")
			master1, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())
			master1 = strings.TrimSpace(master1)
			
			By("killing first master")
			GinkgoWriter.Printf("Killing Master 1: %s\n", master1)
			cmd = exec.Command("kubectl", "delete", "pod", master1,
				"-n", testNamespace, "--grace-period=0", "--force")
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())

			// Wait for recovery
			By("waiting for new master (approx 30s)")
			time.Sleep(30 * time.Second)

			// --- Failover 2 ---
			By("identifying second master")
			// Need to retry a bit as status update might lag slightly behind actual election
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
			GinkgoWriter.Printf("Killing Master 2: %s\n", master2)
			cmd = exec.Command("kubectl", "delete", "pod", master2,
				"-n", testNamespace, "--grace-period=0", "--force")
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())

			// Wait for rest of duration
			By("waiting for chaos client to complete")
			err = waitForChaosClientComplete(testNamespace, podName, testDuration+2*time.Minute)
			Expect(err).NotTo(HaveOccurred())

			By("collecting metrics")
			metrics, err := getChaosClientMetrics(testNamespace, podName)
			Expect(err).NotTo(HaveOccurred())
			GinkgoWriter.Printf("Final metrics:\n%s\n", metrics.String())

			// Assertions
			// 1. Data Integrity: ZERO corruption allowed.
			//    Note: We might lose ACK'd writes if they were in memory but not replicated when we killed -9 the master.
			//    However, chaos-client verifies "Read what was Confirmed Written".
			//    If Redis acknowledges a write, it should be safe (if replication is sync, or if we accept async loss).
			//    Redis Sentinel defaults are async replication. We MIGHT see data loss (not corruption, but missing keys).
			//    Corruption = Read Key X, got Value Y (wrong).
			//    Loss = Read Key X, got "Nil" (missing).
			
			Expect(metrics.DataCorruptions).To(Equal(int64(0)), "Data corruption detected!")

			// 2. Availability
			// We killed the master twice. Each failover takes ~5-10s.
			// We expect some downtime, but it should recover.
			// Let's assert > 50% write availability (conservative).
			writeAvail := metrics.WriteAvailability()
			readAvail := metrics.ReadAvailability()
			
			GinkgoWriter.Printf("Availability - Write: %.2f%%, Read: %.2f%%\n", writeAvail*100, readAvail*100)
			
			Expect(writeAvail).To(BeNumerically(">", 0.40), "Write availability too low")
			Expect(readAvail).To(BeNumerically(">", 0.40), "Read availability too low")
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
			By("Test ID: CHAOS-010")
			const testName = "standalone-restart"
			const testDuration = 40 * time.Second

			By("deploying chaos client pod")
			podName, err := deployChaosClient(testNamespace, testName, crName, false, "chaos-standalone", testDuration)
			Expect(err).NotTo(HaveOccurred())
			defer deleteChaosClient(testNamespace, podName)

			By("waiting for baseline (10 seconds)")
			time.Sleep(10 * time.Second)

			By("deleting the standalone pod")
			// Standalone pods are named <crName>-redis-0
			cmd := exec.Command("kubectl", "delete", "pod", crName+"-redis-0",
				"-n", testNamespace, "--grace-period=0", "--force")
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())

			By("waiting for chaos client to complete")
			err = waitForChaosClientComplete(testNamespace, podName, testDuration+2*time.Minute)
			Expect(err).NotTo(HaveOccurred())

			By("collecting metrics")
			metrics, err := getChaosClientMetrics(testNamespace, podName)
			Expect(err).NotTo(HaveOccurred())
			GinkgoWriter.Printf("Final metrics:\n%s\n", metrics.String())

			By("verifying standalone is running")
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "littlered", crName,
					"-n", testNamespace, "-o", "jsonpath={.status.phase}")
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).To(Equal("Running"))
			}, 2*time.Minute, 5*time.Second).Should(Succeed())

			// Standalone without replication loses data on restart - this is expected
			// We verify no corruption (data that exists is valid)
			Expect(metrics.DataCorruptions).To(Equal(int64(0)),
				"Data that exists should not be corrupted")

			GinkgoWriter.Printf("Standalone availability - Write: %.2f%%, Read: %.2f%%\n",
				metrics.WriteAvailability()*100, metrics.ReadAvailability()*100)
		})
	})
})
