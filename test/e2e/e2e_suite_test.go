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

	"github.com/tanne3/littlered-operator/test/utils"
)

var (
	// operatorImage is the operator image to use for testing.
	// Override with OPERATOR_IMAGE env var.
	operatorImage = "ghcr.io/tanne3/littlered-operator:latest"

	// operatorNamespace is where the operator is deployed.
	operatorNamespace = "littlered-system"

	// testNamespace is the namespace used for all e2e test resources.
	// Override with TEST_NAMESPACE env var.
	testNamespace = "default"

	// skipOperatorDeploy skips operator deployment (use existing deployment).
	skipOperatorDeploy = false

	// useHelm deploys operator via Helm instead of make deploy.
	useHelm = false

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
//   - OPERATOR_IMAGE: Operator image to use (default: ghcr.io/tanne3/littlered-operator:latest)
//   - SKIP_OPERATOR_DEPLOY: Set to "true" to skip operator deployment
//   - USE_HELM: Set to "true" to deploy via Helm instead of make deploy
//   - DEBUG_ON_FAILURE: Set to "true" to skip cleanup on failure
//   - TEST_NAMESPACE: Namespace for test resources (default: "default")
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
	// Load configuration from environment
	if img := os.Getenv("OPERATOR_IMAGE"); img != "" {
		operatorImage = img
	}
	if ns := os.Getenv("TEST_NAMESPACE"); ns != "" {
		testNamespace = ns
	}
	if os.Getenv("SKIP_OPERATOR_DEPLOY") == "true" {
		skipOperatorDeploy = true
	}
	if os.Getenv("USE_HELM") == "true" {
		useHelm = true
	}
	if os.Getenv("DEBUG_ON_FAILURE") == "true" {
		debugOnFailure = true
	}

	_, _ = fmt.Fprintf(GinkgoWriter, "Operator image: %s\n", operatorImage)
	_, _ = fmt.Fprintf(GinkgoWriter, "Test namespace: %s\n", testNamespace)
	_, _ = fmt.Fprintf(GinkgoWriter, "Skip operator deploy: %v\n", skipOperatorDeploy)
	_, _ = fmt.Fprintf(GinkgoWriter, "Use Helm: %v\n", useHelm)
	_, _ = fmt.Fprintf(GinkgoWriter, "Debug on failure: %v\n", debugOnFailure)

	// Create test namespace if it's not "default"
	if testNamespace != "default" {
		By("creating test namespace " + testNamespace)
		cmd := exec.Command("kubectl", "create", "ns", testNamespace)
		_, _ = utils.Run(cmd) // Ignore if exists
	}

	if skipOperatorDeploy {
		_, _ = fmt.Fprintf(GinkgoWriter, "Skipping operator deployment (SKIP_OPERATOR_DEPLOY=true)\n")
		return
	}

	if useHelm {
		deployWithHelm()
	} else {
		deployWithMake()
	}
})

var _ = BeforeEach(func() {
	// Track when each test starts for log filtering
	testStartTime = time.Now()
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

	return namespace, crName, chaosPod
}

var _ = AfterSuite(func() {
	if debugOnFailure && suiteOrSpecFailed() {
		_, _ = fmt.Fprintf(GinkgoWriter, "Skipping cleanup due to failure and DEBUG_ON_FAILURE=true\n")
		return
	}

	// Delete test namespace if it's not "default"
	if testNamespace != "default" {
		By("cleaning up test namespace " + testNamespace)
		cmd := exec.Command("kubectl", "delete", "ns", testNamespace, "--ignore-not-found", "--timeout=2m")
		_, _ = utils.Run(cmd)
	}

	if skipOperatorDeploy {
		return
	}

	if useHelm {
		undeployWithHelm()
	} else {
		undeployWithMake()
	}
})

func deployWithMake() {
	By("building the operator image")
	cmd := exec.Command("make", "img-build", fmt.Sprintf("IMG=%s", operatorImage))
	_, err := utils.Run(cmd)
	ExpectWithOffset(1, err).NotTo(HaveOccurred(), "Failed to build the operator image")

	By("loading the operator image to Kind")
	err = utils.LoadImageToKindClusterWithName(operatorImage)
	ExpectWithOffset(1, err).NotTo(HaveOccurred(), "Failed to load the operator image into Kind")

	By("installing CRDs")
	cmd = exec.Command("make", "install")
	_, err = utils.Run(cmd)
	ExpectWithOffset(1, err).NotTo(HaveOccurred(), "Failed to install CRDs")

	By("deploying the operator")
	cmd = exec.Command("make", "deploy", fmt.Sprintf("IMG=%s", operatorImage))
	_, err = utils.Run(cmd)
	ExpectWithOffset(1, err).NotTo(HaveOccurred(), "Failed to deploy the operator")

	By("waiting for operator to be ready")
	waitForOperator()
}

func undeployWithMake() {
	By("undeploying the operator")
	cmd := exec.Command("make", "undeploy")
	_, _ = utils.Run(cmd)

	By("uninstalling CRDs")
	cmd = exec.Command("make", "uninstall")
	_, _ = utils.Run(cmd)
}

func deployWithHelm() {
	By("loading the operator image to Kind")
	err := utils.LoadImageToKindClusterWithName(operatorImage)
	ExpectWithOffset(1, err).NotTo(HaveOccurred(), "Failed to load the operator image into Kind")

	By("installing operator with Helm")
	cmd := exec.Command("helm", "install", "littlered", "charts/littlered-operator",
		"--namespace", operatorNamespace,
		"--create-namespace",
		"--set", fmt.Sprintf("image.repository=%s", getImageRepository(operatorImage)),
		"--set", fmt.Sprintf("image.tag=%s", getImageTag(operatorImage)),
		"--set", "image.pullPolicy=Never",
		"--wait",
		"--timeout", "2m",
	)
	_, err = utils.Run(cmd)
	ExpectWithOffset(1, err).NotTo(HaveOccurred(), "Failed to install operator with Helm")

	By("waiting for operator to be ready")
	waitForOperator()
}

func undeployWithHelm() {
	By("uninstalling operator with Helm")
	cmd := exec.Command("helm", "uninstall", "littlered",
		"--namespace", operatorNamespace,
		"--ignore-not-found",
	)
	_, _ = utils.Run(cmd)

	By("deleting operator namespace")
	cmd = exec.Command("kubectl", "delete", "namespace", operatorNamespace, "--ignore-not-found")
	_, _ = utils.Run(cmd)

	By("deleting CRDs")
	cmd = exec.Command("kubectl", "delete", "crd", "littlereds.chuck-chuck-chuck.net", "--ignore-not-found")
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

func getImageRepository(image string) string {
	// Split image:tag and return the repository part
	parts := splitImageTag(image)
	return parts[0]
}

func getImageTag(image string) string {
	// Split image:tag and return the tag part
	parts := splitImageTag(image)
	if len(parts) > 1 {
		return parts[1]
	}
	return "latest"
}

func splitImageTag(image string) []string {
	// Handle images with registry port (e.g., localhost:5000/image:tag)
	lastColon := -1
	for i := len(image) - 1; i >= 0; i-- {
		if image[i] == ':' {
			lastColon = i
			break
		}
		if image[i] == '/' {
			break
		}
	}
	if lastColon > 0 {
		return []string{image[:lastColon], image[lastColon+1:]}
	}
	return []string{image}
}
