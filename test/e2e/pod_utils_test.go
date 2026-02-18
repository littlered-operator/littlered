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
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/littlered-operator/littlered-operator/test/utils"
)

// =============================================================================
// Pre-delete log snapshots (instant capture, no dying logs)
// =============================================================================

// preDeleteLogs stores logs captured from pods just before they are deleted.
// Keyed by "podName/containerName". Cleared before each test.
var (
	preDeleteLogsMu sync.Mutex
	preDeleteLogs   = map[string][]byte{}
)

// resetPreDeleteLogs clears the pre-deletion log buffer. Called in BeforeEach.
func resetPreDeleteLogs() {
	preDeleteLogsMu.Lock()
	defer preDeleteLogsMu.Unlock()
	preDeleteLogs = map[string][]byte{}
}

// capturePreDeleteLogs saves the current logs of all containers in a pod so they
// can be written to debug artifacts even after the pod has been replaced.
func capturePreDeleteLogs(namespace, podName string) {
	cmd := exec.Command("kubectl", "get", "pod", podName,
		"-n", namespace,
		"-o", "jsonpath={.spec.containers[*].name}")
	out, err := utils.Run(cmd)
	if err != nil || out == "" {
		return
	}

	preDeleteLogsMu.Lock()
	defer preDeleteLogsMu.Unlock()

	for _, container := range strings.Fields(out) {
		logCmd := exec.Command("kubectl", "logs", podName,
			"-n", namespace,
			"-c", container,
			"--timestamps",
			"--tail", "2000")
		logs, err := logCmd.CombinedOutput()
		if err != nil {
			continue
		}
		preDeleteLogs[podName+"/"+container] = logs
		fmt.Printf("[Utility] Captured %d bytes pre-delete snapshot for %s/%s\n", len(logs), podName, container)
	}
}

// =============================================================================
// Streaming logs — follow a pod through its death (catches preStop output)
// =============================================================================

// streamEntry holds a running "kubectl logs -f" process and its output file.
type streamEntry struct {
	cmd      *exec.Cmd
	filePath string
}

var (
	streamLogsMu sync.Mutex
	streamLogs   = map[string]*streamEntry{} // key: "podName/containerName"

	// e2eTmpDir is created once for the whole suite and cleaned up in AfterSuite.
	e2eTmpDir string
)

// initE2ETmpDir creates the suite-wide temporary directory for streaming logs.
// Called from BeforeSuite.
func initE2ETmpDir() {
	dir := filepath.Join(os.TempDir(), fmt.Sprintf("tmp-e2e-logs-%d", time.Now().Unix()))
	if err := os.MkdirAll(dir, 0755); err != nil {
		fmt.Printf("[Utility] Failed to create tmp log dir %s: %v\n", dir, err)
		return
	}
	e2eTmpDir = dir
	fmt.Printf("[Utility] Streaming log tmp dir: %s\n", e2eTmpDir)
}

// cleanupE2ETmpDir kills any still-running log streamers and removes the tmp dir.
// Called from AfterSuite.
func cleanupE2ETmpDir() {
	stopAllStreamers()
	if e2eTmpDir != "" {
		_ = os.RemoveAll(e2eTmpDir)
		fmt.Printf("[Utility] Removed tmp log dir %s\n", e2eTmpDir)
	}
}

// stopAllStreamers kills every active kubectl logs -f process and clears the map.
func stopAllStreamers() {
	streamLogsMu.Lock()
	defer streamLogsMu.Unlock()
	for key, entry := range streamLogs {
		if entry.cmd != nil && entry.cmd.Process != nil {
			_ = entry.cmd.Process.Kill()
		}
		fmt.Printf("[Utility] Stopped log streamer for %s\n", key)
	}
	streamLogs = map[string]*streamEntry{}
}

// resetStreamingLogs stops streamers from the previous test and clears the map.
// Called from BeforeEach so each test starts with a clean slate.
func resetStreamingLogs() {
	stopAllStreamers()
}

// startStreamingLogs spawns one "kubectl logs -f" child process per container.
// Each process writes to a file in e2eTmpDir and runs until the container exits.
// Must be called BEFORE kubectl delete so we don't miss the last log lines.
func startStreamingLogs(namespace, podName string) {
	if e2eTmpDir == "" {
		return
	}

	cmd := exec.Command("kubectl", "get", "pod", podName,
		"-n", namespace,
		"-o", "jsonpath={.spec.containers[*].name}")
	out, err := utils.Run(cmd)
	if err != nil || out == "" {
		fmt.Printf("[Utility] Could not list containers for streaming %s/%s: %v\n", namespace, podName, err)
		return
	}

	streamLogsMu.Lock()
	defer streamLogsMu.Unlock()

	for _, container := range strings.Fields(out) {
		key := podName + "/" + container
		// Don't start a second streamer for the same pod/container.
		if _, exists := streamLogs[key]; exists {
			continue
		}

		filePath := filepath.Join(e2eTmpDir, fmt.Sprintf("pod-%s-%s-streaming.log", podName, container))
		f, err := os.Create(filePath)
		if err != nil {
			fmt.Printf("[Utility] Failed to create streaming log file %s: %v\n", filePath, err)
			continue
		}

		logCmd := exec.Command("kubectl", "logs", "-f", podName,
			"-n", namespace,
			"-c", container,
			"--timestamps")
		logCmd.Stdout = f
		logCmd.Stderr = f

		if err := logCmd.Start(); err != nil {
			fmt.Printf("[Utility] Failed to start log streamer for %s/%s: %v\n", podName, container, err)
			_ = f.Close()
			continue
		}

		entry := &streamEntry{cmd: logCmd, filePath: filePath}
		streamLogs[key] = entry
		fmt.Printf("[Utility] Started log streamer for %s/%s → %s\n", podName, container, filePath)

		// Wait for the process to finish in a goroutine so the file gets flushed and closed.
		go func(e *streamEntry, k string) {
			_ = e.cmd.Wait()
			_ = f.Sync()
			_ = f.Close()
			fmt.Printf("[Utility] Log streamer finished for %s\n", k)
		}(entry, key)
	}
}

// copyStreamingLogsToDir copies all streaming log files currently tracked to dst.
// It gives streamers a short grace period to flush their last bytes.
func copyStreamingLogsToDir(dst string) {
	// Give streaming processes a moment to flush after the pod terminated.
	time.Sleep(2 * time.Second)

	streamLogsMu.Lock()
	// Snapshot the file paths; don't hold the lock while doing file I/O.
	paths := make([]string, 0, len(streamLogs))
	for _, entry := range streamLogs {
		paths = append(paths, entry.filePath)
	}
	streamLogsMu.Unlock()

	for _, src := range paths {
		dstFile := filepath.Join(dst, filepath.Base(src))
		if err := copyFile(src, dstFile); err != nil {
			fmt.Printf("[Utility] Failed to copy streaming log %s → %s: %v\n", src, dstFile, err)
		} else {
			info, _ := os.Stat(dstFile)
			if info != nil {
				fmt.Printf("[Utility] Copied streaming log → %s (%d bytes)\n", dstFile, info.Size())
			}
		}
	}
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer out.Close()

	_, err = io.Copy(out, in)
	return err
}

// =============================================================================
// Pod deletion helpers
// =============================================================================

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
// If NON_GRACEFUL_RESTART environment variable is set to "true", it performs a
// non-graceful deletion (--grace-period=0 --force). Otherwise graceful.
func deletePod(namespace, podName string) (string, error) {
	graceful := os.Getenv("NON_GRACEFUL_RESTART") != "true"
	return deletePodMode(namespace, podName, graceful)
}

// deletePodMode deletes a pod with explicit control over graceful vs crash mode.
// It captures a log snapshot AND starts a streaming follower before deletion so
// the full dying sequence (including preStop output) is available in artifacts.
func deletePodMode(namespace, podName string, graceful bool) (string, error) {
	// 1. Instant snapshot — catches all lines up to this moment.
	capturePreDeleteLogs(namespace, podName)
	// 2. Start streaming — catches lines produced during Terminating (preStop, SIGTERM).
	startStreamingLogs(namespace, podName)

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
