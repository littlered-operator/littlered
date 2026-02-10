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
	"os/exec"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	littleredv1alpha1 "github.com/tanne3/littlered-operator/api/v1alpha1"
	"github.com/tanne3/littlered-operator/test/utils"
)

// verifyClusterTopologySync cross-validates the Operator's reported Status
// against the actual ground truth from Redis 'CLUSTER NODES'.
func verifyClusterTopologySync(namespace, crName string, expectedNodes int) {
	By(fmt.Sprintf("verifying that Operator status for %s matches actual Redis topology", crName))

	// 1. Get the CR from Kubernetes
	cmd := exec.Command("kubectl", "get", "littlered", crName, "-n", namespace, "-o", "json")
	output, err := utils.Run(cmd)
	Expect(err).NotTo(HaveOccurred(), "Failed to get LittleRed CR")

	var lr littleredv1alpha1.LittleRed
	err = json.Unmarshal([]byte(output), &lr)
	Expect(err).NotTo(HaveOccurred(), "Failed to parse LittleRed JSON")

	// 2. Get ground truth from any available pod
	var clusterNodesOutput string
	var success bool
	for i := 0; i < expectedNodes; i++ {
		podName := fmt.Sprintf("%s-cluster-%d", crName, i)
		cmd = exec.Command("kubectl", "exec", podName, "-n", namespace, "-c", "redis", "--", "valkey-cli", "CLUSTER", "NODES")
		clusterNodesOutput, err = utils.Run(cmd)
		if err == nil {
			success = true
			break
		}
	}
	Expect(success).To(BeTrue(), "Failed to execute CLUSTER NODES on any pod")

	// 3. Parse Redis output into a map for easy comparison
	redisNodes := make(map[string]struct {
		role     string
		masterID string
		slots    string
	})
	
	lines := strings.Split(strings.TrimSpace(clusterNodesOutput), "\n")
	for _, line := range lines {
		fields := strings.Fields(line)
		if len(fields) < 4 {
			continue
		}
		id := fields[0]
		flags := fields[2]
		masterID := fields[3]
		
		role := "master"
		if strings.Contains(flags, "slave") {
			role = "replica"
		}
		
		slots := ""
		if len(fields) > 8 {
			slots = strings.Join(fields[8:], ",")
		}
		
		redisNodes[id] = struct {
			role     string
			masterID string
			slots    string
		}{role: role, masterID: masterID, slots: slots}
	}

	// 4. Perform Assertions
	Expect(lr.Status.Cluster).NotTo(BeNil(), "CR Status.Cluster is nil")
	Expect(len(lr.Status.Cluster.Nodes)).To(Equal(expectedNodes), "Number of nodes in Status doesn't match expected")
	
	for _, nodeStatus := range lr.Status.Cluster.Nodes {
		actual, exists := redisNodes[nodeStatus.NodeID]
		Expect(exists).To(BeTrue(), fmt.Sprintf("Status reports NodeID %s (pod %s) but it's missing from CLUSTER NODES", nodeStatus.NodeID, nodeStatus.PodName))
		Expect(nodeStatus.Role).To(Equal(actual.role), fmt.Sprintf("Role mismatch for node %s", nodeStatus.PodName))
		
		if nodeStatus.Role == "replica" {
			Expect(nodeStatus.MasterNodeID).To(Equal(actual.masterID), fmt.Sprintf("MasterID mismatch for replica %s", nodeStatus.PodName))
		} else {
			Expect(nodeStatus.SlotRanges).NotTo(BeEmpty(), fmt.Sprintf("Master %s has no slots in Status", nodeStatus.PodName))
		}
	}
	
	By("Topology sync validation passed")
}

// verifySentinelTopologySync cross-validates the Operator's reported Sentinel Status
// against the ground truth from the Sentinel nodes.
func verifySentinelTopologySync(namespace, crName string, expectedSentinels, expectedReplicas int) {
	By(fmt.Sprintf("verifying that Operator status for %s matches actual Sentinel topology", crName))

	// 1. Get the CR from Kubernetes
	cmd := exec.Command("kubectl", "get", "littlered", crName, "-n", namespace, "-o", "json")
	output, err := utils.Run(cmd)
	Expect(err).NotTo(HaveOccurred(), "Failed to get LittleRed CR")

	var lr littleredv1alpha1.LittleRed
	err = json.Unmarshal([]byte(output), &lr)
	Expect(err).NotTo(HaveOccurred(), "Failed to parse LittleRed JSON")

	// 2. Get ground truth from Sentinel
	// We try sentinel-0
	sentinelPod := fmt.Sprintf("%s-sentinel-0", crName)
	cmd = exec.Command("kubectl", "exec", sentinelPod, "-n", namespace, "-c", "sentinel", "--", "valkey-cli", "-p", "26379", "SENTINEL", "master", "mymaster")
	sentinelOutput, err := utils.Run(cmd)
	Expect(err).NotTo(HaveOccurred(), "Failed to execute SENTINEL master on sentinel pod")

	// Parse Sentinel output (it's a list of key-value pairs)
	sentinelData := make(map[string]string)
	lines := strings.Split(strings.TrimSpace(sentinelOutput), "\n")
	for i := 0; i < len(lines)-1; i += 2 {
		sentinelData[lines[i]] = lines[i+1]
	}

	actualMasterIP := sentinelData["ip"]
	
	// 3. Perform Assertions
	Expect(lr.Status.Master).NotTo(BeNil(), "CR Status.Master is nil")
	Expect(lr.Status.Master.IP).To(Equal(actualMasterIP), "Master IP mismatch in Status")
	
	// Map IP back to pod name to verify Status.Master.PodName
	cmd = exec.Command("kubectl", "get", "pods", "-n", namespace, "-l", "app.kubernetes.io/instance="+crName+",littlered.chuck-chuck-chuck.net/mode=sentinel", "-o", "json")
	podsOutput, _ := utils.Run(cmd)
	Expect(podsOutput).To(ContainSubstring(actualMasterIP), "Master IP not found in any pod")
	
	Expect(lr.Status.Sentinels).NotTo(BeNil(), "CR Status.Sentinels is nil")
	Expect(int(lr.Status.Sentinels.Total)).To(Equal(expectedSentinels), "Sentinel total count mismatch")
	
	Expect(lr.Status.Replicas).NotTo(BeNil(), "CR Status.Replicas is nil")
	Expect(int(lr.Status.Replicas.Total)).To(Equal(expectedReplicas), "Replica total count mismatch")

	// Also verify that Sentinel agrees on the number of slaves
	var actualNumSlaves int
	fmt.Sscanf(sentinelData["num-slaves"], "%d", &actualNumSlaves)
	Expect(actualNumSlaves).To(Equal(expectedReplicas), "Sentinel reports different number of slaves than expected")

	By("Sentinel topology sync validation passed")
}

// getShardGroups returns a list of pod names grouped by their shard.
func getShardGroups(namespace, crName string, totalNodes int) ([][]string, error) {
	nodeIDToPodName := make(map[string]string)
	
	for i := 0; i < totalNodes; i++ {
		podName := fmt.Sprintf("%s-cluster-%d", crName, i)
		cmd := exec.Command("kubectl", "exec", podName, "-n", namespace, "-c", "redis", "--", "valkey-cli", "CLUSTER", "MYID")
		output, err := utils.Run(cmd)
		if err != nil {
			return nil, fmt.Errorf("failed to get ID for pod %s: %w", podName, err)
		}
		nodeIDToPodName[strings.TrimSpace(output)] = podName
	}

	var clusterNodesOutput string
	for i := 0; i < totalNodes; i++ {
		podName := fmt.Sprintf("%s-cluster-%d", crName, i)
		cmd := exec.Command("kubectl", "exec", podName, "-n", namespace, "-c", "redis", "--", "valkey-cli", "CLUSTER", "NODES")
		var err error
		clusterNodesOutput, err = utils.Run(cmd)
		if err == nil {
			break
		}
	}
	if clusterNodesOutput == "" {
		return nil, fmt.Errorf("failed to get CLUSTER NODES from any pod")
	}

	shardMap := make(map[string][]string)
	
	type nodeInfo struct {
		id       string
		role     string
		masterID string
	}
	nodes := []nodeInfo{}

	lines := strings.Split(strings.TrimSpace(clusterNodesOutput), "\n")
	for _, line := range lines {
		fields := strings.Fields(line)
		if len(fields) < 4 {
			continue
		}
		id := fields[0]
		flags := fields[2]
		masterID := fields[3]
		
		role := "master"
		if strings.Contains(flags, "slave") {
			role = "replica"
		}
		nodes = append(nodes, nodeInfo{id: id, role: role, masterID: masterID})
	}

	for _, n := range nodes {
		podName, ok := nodeIDToPodName[n.id]
		if !ok {
			continue
		}

		targetShardID := n.id
		if n.role == "replica" {
			targetShardID = n.masterID
		}
		shardMap[targetShardID] = append(shardMap[targetShardID], podName)
	}

	result := make([][]string, 0, len(shardMap))
	for _, group := range shardMap {
		result = append(result, group)
	}

	return result, nil
}