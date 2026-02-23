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
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/littlered-operator/littlered-operator/test/utils"
)

var (
	// operatorNamespace is where the operator is deployed.
	operatorNamespace = "littlered-system"

	// testNamespace is the namespace used for all e2e test resources.
	// Override with TEST_NAMESPACE env var. Must not be "default".
	testNamespace = "littlered-e2e"

	// skipOperatorDeploy skips operator deployment (use existing deployment).
	skipOperatorDeploy = false

	// debugOnFailure skips cleanup on failure.
	debugOnFailure = false

	// suiteFailed tracks if any test in the suite failed.
	suiteFailed = false
)

func suiteOrSpecFailed() bool {
	failed := suiteFailed || CurrentSpecReport().Failed() || GinkgoT().Failed()
	if failed {
		// Using fmt.Fprintf instead of By because we might be in an AfterSuite where By is not allowed
		_, _ = fmt.Fprintf(GinkgoWriter, "Failure detected (suiteFailed: %v, SpecReport.Failed: %v, GinkgoT.Failed: %v)\n",
			suiteFailed, CurrentSpecReport().Failed(), GinkgoT().Failed())
	}
	return failed
}

// TestE2E runs the e2e test suite.
//
// Environment variables:
//   - SKIP_OPERATOR_DEPLOY: Set to "true" to skip operator deployment
//   - DEBUG_ON_FAILURE: Set to "true" to skip cleanup on failure
//   - TEST_NAMESPACE: Namespace for test resources (default: "littlered-e2e"); must not be "default"
//   - KIND_CLUSTER: Kind cluster name (default: kind)
func TestE2E(t *testing.T) {
	RegisterFailHandler(func(message string, callerSkip ...int) {
		_, _ = fmt.Fprintf(GinkgoWriter, "Fail handler called: %s\n", message)
		suiteFailed = true
		Fail(message, callerSkip...)
	})
	_, _ = fmt.Fprintf(GinkgoWriter, "Starting LittleRed operator e2e test suite\n")
	RunSpecs(t, "LittleRed E2E Suite")
}

var _ = BeforeSuite(func() {
	// Initialize streaming log tmp directory (suite-wide)
	initE2ETmpDir()

	// Load configuration from environment
	if ns := os.Getenv("TEST_NAMESPACE"); ns != "" {
		testNamespace = ns
	}
	if os.Getenv("SKIP_OPERATOR_DEPLOY") == "true" {
		skipOperatorDeploy = true
	}
	if os.Getenv("DEBUG_ON_FAILURE") == "true" {
		debugOnFailure = true
	}

	_, _ = fmt.Fprintf(GinkgoWriter, "Test namespace: %s\n", testNamespace)
	_, _ = fmt.Fprintf(GinkgoWriter, "Skip operator deploy: %v\n", skipOperatorDeploy)
	_, _ = fmt.Fprintf(GinkgoWriter, "Debug on failure: %v\n", debugOnFailure)

	// Require a dedicated namespace — "default" is not safe for e2e tests.
	Expect(testNamespace).NotTo(Equal("default"),
		"TEST_NAMESPACE must not be 'default'; use a dedicated namespace such as 'littlered-e2e'")

	// Require the namespace to not exist yet. Stale resources from a previous run
	// (Pods, LittleRed CRs, etc.) would corrupt the tests, so we refuse to start
	// rather than silently inheriting leftover state.
	By("verifying test namespace " + testNamespace + " does not exist")
	cmd := exec.Command("kubectl", "get", "ns", testNamespace)
	_, err := utils.Run(cmd)
	Expect(err).To(HaveOccurred(),
		"Test namespace %q already exists — delete it first to ensure a clean slate:\n  kubectl delete ns %s",
		testNamespace, testNamespace)

	By("creating test namespace " + testNamespace)
	cmd = exec.Command("kubectl", "create", "ns", testNamespace)
	_, err = utils.Run(cmd)
	Expect(err).NotTo(HaveOccurred(), "Failed to create test namespace %q", testNamespace)

	if skipOperatorDeploy {
		_, _ = fmt.Fprintf(GinkgoWriter, "Skipping operator deployment (SKIP_OPERATOR_DEPLOY=true)\n")
		return
	}

	deployOperator()
})

var _ = BeforeEach(func() {
	// Track when each test starts for log filtering
	testStartTime = time.Now()
	// Clear any pre-delete logs captured by a previous test
	resetPreDeleteLogs()
	// Stop any streaming log processes from the previous test
	resetStreamingLogs()
})

var _ = AfterEach(func() {
	if CurrentSpecReport().Failed() {
		suiteFailed = true

		// Collect debug artifacts if enabled
		if debugOnFailure {
			// Try to extract namespace and CR name from the spec labels
			// This is best-effort - if we can't find them, we'll just collect what we can
			namespace, crName, chaosPod := extractTestContext()
			CollectDebugArtifacts(namespace, crName, chaosPod)
		}
	}
})

// extractTestContext extracts CR name and chaos pod from spec labels.
// Namespace is always the global testNamespace.
func extractTestContext() (namespace, crName, chaosPod string) {
	namespace = testNamespace

	labels := CurrentSpecReport().Labels()
	for _, label := range labels {
		if strings.HasPrefix(label, "cr:") {
			crName = strings.TrimPrefix(label, "cr:")
		} else if strings.HasPrefix(label, "chaos:") {
			chaosPod = strings.TrimPrefix(label, "chaos:")
		}
	}

	// Also check report entries for dynamic names
	for _, entry := range CurrentSpecReport().ReportEntries {
		name := entry.Name
		if strings.HasPrefix(name, "cr:") {
			crName = strings.TrimPrefix(name, "cr:")
		} else if strings.HasPrefix(name, "chaos:") {
			chaosPod = strings.TrimPrefix(name, "chaos:")
		}
	}

	return namespace, crName, chaosPod
}

var _ = AfterSuite(func() {
	// Always clean up the streaming log tmp dir (even on failure — artifacts were
	// already copied to the debug-artifacts dir by CollectDebugArtifacts).
	cleanupE2ETmpDir()

	if debugOnFailure && suiteOrSpecFailed() {
		_, _ = fmt.Fprintf(GinkgoWriter, "Skipping cleanup due to failure and DEBUG_ON_FAILURE=true\n")
		return
	}

	By("cleaning up test namespace " + testNamespace)
	cmd := exec.Command("kubectl", "delete", "ns", testNamespace, "--ignore-not-found", "--timeout=5m")
	_, _ = utils.Run(cmd)

	if skipOperatorDeploy {
		return
	}

	undeployOperator()
})

func deployOperator() {
	By("building the operator image")
	cmd := exec.Command("make", "images")
	_, err := utils.Run(cmd)
	ExpectWithOffset(1, err).NotTo(HaveOccurred(), "Failed to build the operator images")

	By("loading images into Kind")
	cmd = exec.Command("make", "kind-load")
	_, err = utils.Run(cmd)
	ExpectWithOffset(1, err).NotTo(HaveOccurred(), "Failed to load images into Kind")

	By("deploying the operator via Helm")
	// We use PULL_POLICY=Never to ensure Kind uses the local image we just loaded
	cmd = exec.Command("make", "deploy", "PULL_POLICY=Never")
	_, err = utils.Run(cmd)
	ExpectWithOffset(1, err).NotTo(HaveOccurred(), "Failed to deploy the operator via Helm")

	By("waiting for operator to be ready")
	waitForOperator()
}

func undeployOperator() {
	By("undeploying the operator")
	cmd := exec.Command("make", "undeploy")
	_, _ = utils.Run(cmd)
}

func waitForOperator() {
	Eventually(func(g Gomega) {
		cmd := exec.Command("kubectl", "get", "deployment", "-n", operatorNamespace,
			"-l", "control-plane=controller-manager",
			"-o", "jsonpath={.items[0].status.availableReplicas}")
		output, err := utils.Run(cmd)
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(output).To(Equal("1"))
	}, "2m", "5s").Should(Succeed())
}
