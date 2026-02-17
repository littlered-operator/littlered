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

	"github.com/littlered-operator/littlered-operator/test/utils"
)

// restartMode defines how a pod deletion is performed.
type restartMode struct {
	Name     string
	Graceful bool
}

// restartModes contains the two deletion modes used in dual-mode tests.
var restartModes = []restartMode{
	{"graceful", true},
	{"crash", false},
}

// deletePod deletes a pod in the given namespace.
// If NON_GRACEFUL_RESTART environment variable is set to "true", it performs a non-graceful deletion (--grace-period=0 --force).
// Otherwise, it performs a graceful deletion (default kubectl behavior).
func deletePod(namespace, podName string) (string, error) {
	graceful := os.Getenv("NON_GRACEFUL_RESTART") != "true"
	return deletePodMode(namespace, podName, graceful)
}

// deletePodMode deletes a pod with explicit control over graceful vs crash mode.
func deletePodMode(namespace, podName string, graceful bool) (string, error) {
	args := []string{"delete", "pod", podName, "-n", namespace}

	if !graceful {
		fmt.Printf("[Utility] Performing NON-GRACEFUL deletion of pod %s/%s\n", namespace, podName)
		args = append(args, "--grace-period=0", "--force")
	} else {
		fmt.Printf("[Utility] Performing GRACEFUL deletion of pod %s/%s\n", namespace, podName)
	}

	cmd := exec.Command("kubectl", args...)
	return utils.Run(cmd)
}

// deletePodsWithLabel deletes all pods matching the label selector in the given namespace.
func deletePodsWithLabel(namespace, labelSelector string) (string, error) {
	args := []string{"delete", "pods", "-n", namespace, "-l", labelSelector}

	if os.Getenv("NON_GRACEFUL_RESTART") == "true" {
		fmt.Printf("[Utility] Performing NON-GRACEFUL deletion of pods with label %s in %s\n", labelSelector, namespace)
		args = append(args, "--grace-period=0", "--force")
	} else {
		fmt.Printf("[Utility] Performing GRACEFUL deletion of pods with label %s in %s\n", labelSelector, namespace)
	}

	cmd := exec.Command("kubectl", args...)
	return utils.Run(cmd)
}
