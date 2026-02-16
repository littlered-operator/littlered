package cmd

import (
	"context"
	"fmt"
	"strings"

	"github.com/spf13/cobra"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"

	"github.com/littlered-operator/littlered-operator/internal/cli/discovery"
	"github.com/littlered-operator/littlered-operator/internal/cli/k8s"
	"github.com/littlered-operator/littlered-operator/internal/cli/types"
)

var verifyCmd = &cobra.Command{
	Use:   "verify [name]",
	Short: "Verify consistency of a Redis cluster",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		name := args[0]

		k8sClient, coreClient, config, defaultNS, err := k8s.NewClient(kubeconfig)
		if err != nil {
			return err
		}

		targetNS := namespace
		if targetNS == "" {
			targetNS = defaultNS
		}

		ctx := context.Background()
		cCtx, err := discovery.GetContext(ctx, k8sClient, targetNS, name, kind, unmanaged)
		if err != nil {
			return err
		}

		fmt.Printf("Verifying Cluster: %s/%s (Mode: %s)\n", cCtx.Namespace, cCtx.Name, cCtx.Mode)

		if cCtx.Mode == "sentinel" {
			return verifySentinel(ctx, coreClient, config, cCtx)
		} else if cCtx.Mode == "cluster" {
			return verifyCluster(ctx, coreClient, config, cCtx)
		} else {
			fmt.Printf("Verification for mode %q not yet fully implemented\n", cCtx.Mode)
			return nil
		}
	},
}

func verifyCluster(ctx context.Context, coreClient *kubernetes.Clientset, config *rest.Config, cCtx *types.ClusterContext) error {
	ipToPod := make(map[string]string)
	for _, p := range cCtx.RedisPods {
		if p.Status.PodIP != "" {
			ipToPod[p.Status.PodIP] = p.Name
		}
	}

	fmt.Printf("Checking %d cluster pods...\n", len(cCtx.RedisPods))

	states := make(map[string]int)
	slots := make(map[string]int)
	nodeCounts := make(map[int]int)
	var topologyOutput string

	for _, pod := range cCtx.RedisPods {
		// Get Info
		stdout, _, err := k8s.Exec(coreClient, config, cCtx.Namespace, pod.Name, cCtx.RedisContainer, []string{"redis-cli", "cluster", "info"})
		if err != nil {
			fmt.Printf("  [!] Pod %s unreachable: %v\n", pod.Name, err)
			continue
		}

		state := "unknown"
		assigned := "unknown"
		infoLines := strings.Split(strings.ReplaceAll(stdout, "\r", ""), "\n")
		for _, line := range infoLines {
			if strings.HasPrefix(line, "cluster_state:") {
				state = strings.TrimSpace(strings.TrimPrefix(line, "cluster_state:"))
			}
			if strings.HasPrefix(line, "cluster_slots_assigned:") {
				assigned = strings.TrimSpace(strings.TrimPrefix(line, "cluster_slots_assigned:"))
			}
		}
		states[state]++
		slots[assigned]++

		// Get Nodes
		stdout, _, err = k8s.Exec(coreClient, config, cCtx.Namespace, pod.Name, cCtx.RedisContainer, []string{"redis-cli", "cluster", "nodes"})
		if err == nil {
			nodesOutput := strings.TrimSpace(strings.ReplaceAll(stdout, "\r", ""))
			if topologyOutput == "" && state == "ok" {
				topologyOutput = nodesOutput
			}
			count := len(strings.Split(nodesOutput, "\n"))
			nodeCounts[count]++
			fmt.Printf("  - Pod %s: state=%s, slots=%s, nodes=%d\n", pod.Name, state, assigned, count)
		}
	}

	fmt.Println("\nSummary:")
	if len(states) == 1 {
		for s := range states {
			fmt.Printf("  [OK] All nodes report cluster_state:%s\n", s)
		}
	} else {
		fmt.Printf("  [FAIL] Nodes disagree on state: %v\n", states)
	}

	if len(slots) == 1 {
		for s := range slots {
			if s == "16384" {
				fmt.Printf("  [OK] All nodes report all slots (16384) assigned.\n")
			} else {
				fmt.Printf("  [FAIL] All nodes report %s slots assigned (expected 16384)!\n", s)
			}
		}
	} else {
		fmt.Printf("  [FAIL] Nodes disagree on assigned slots: %v\n", slots)
	}

	if topologyOutput != "" {
		fmt.Println("\nCluster Topology:")
		lines := strings.Split(topologyOutput, "\n")
		idToPod := make(map[string]string)
		for _, line := range lines {
			parts := strings.Fields(line)
			if len(parts) < 2 {
				continue
			}
			id := parts[0]
			addr := parts[1]
			ip := strings.Split(addr, "@")[0] // Handle both ip:port and ip:port@bus
			ip = strings.Split(ip, ":")[0]
			if name, ok := ipToPod[ip]; ok {
				idToPod[id] = name
			} else {
				idToPod[id] = "unknown-" + ip
			}
		}

		for _, line := range lines {
			parts := strings.Fields(line)
			if len(parts) < 3 {
				continue
			}
			flags := parts[2]
			if !strings.Contains(flags, "master") {
				continue
			}
			id := parts[0]
			podName := idToPod[id]
			slots := strings.Join(parts[8:], " ")
			fmt.Printf("  Master: %s (Slots: %s)\n", podName, slots)

			for _, rLine := range lines {
				rParts := strings.Fields(rLine)
				if len(rParts) < 4 {
					continue
				}
				if strings.Contains(rParts[2], "slave") && rParts[3] == id {
					fmt.Printf("    └── Replica: %s\n", idToPod[rParts[0]])
				}
			}
		}
	}
	return nil
}

func verifySentinel(ctx context.Context, coreClient *kubernetes.Clientset, config *rest.Config, cCtx *types.ClusterContext) error {
	ipToPod := make(map[string]corev1.Pod)
	for _, p := range cCtx.RedisPods {
		if p.Status.PodIP != "" {
			ipToPod[p.Status.PodIP] = p
		}
	}

	fmt.Println("Checking Sentinels...")
	masterIPs := make(map[string]int)

	for _, pod := range cCtx.SentinelPods {
		stdout, _, err := k8s.Exec(coreClient, config, cCtx.Namespace, pod.Name, cCtx.SentinelContainer, []string{"redis-cli", "-p", "26379", "sentinel", "get-master-addr-by-name", "mymaster"})
		if err != nil {
			fmt.Printf("  [!] Sentinel %s unreachable: %v\n", pod.Name, err)
			continue
		}

		parts := strings.Fields(stdout)
		if len(parts) < 1 {
			fmt.Printf("  [!] Sentinel %s reports no master!\n", pod.Name)
			masterIPs["none"]++
			continue
		}
		mIP := parts[0]
		masterIPs[mIP]++
		fmt.Printf("  - Sentinel %s sees master at: %s\n", pod.Name, mIP)
	}

	if len(masterIPs) > 1 {
		fmt.Printf("[FAIL] Sentinels disagree on master: %v\n", masterIPs)
	} else if len(masterIPs) == 1 {
		fmt.Println("[OK] All reachable Sentinels agree on master IP.")
	}

	var currentMasterAddr string
	for addr := range masterIPs {
		currentMasterAddr = addr
		break
	}

	if currentMasterAddr != "" && currentMasterAddr != "none" {
		found := false
		for _, pod := range cCtx.RedisPods {
			// Match by IP or Hostname
			if pod.Status.PodIP == currentMasterAddr || strings.Contains(currentMasterAddr, pod.Name) {
				fmt.Printf("[OK] Master %s (IP: %s) belongs to pod %s\n", currentMasterAddr, pod.Status.PodIP, pod.Name)
				stdout, _, err := k8s.Exec(coreClient, config, cCtx.Namespace, pod.Name, cCtx.RedisContainer, []string{"redis-cli", "info", "replication"})
				if err == nil && strings.Contains(stdout, "role:master") {
					fmt.Printf("[OK] Pod %s correctly reports role:master\n", pod.Name)
				} else {
					fmt.Printf("[FAIL] Pod %s does NOT think it is master! (Error: %v)\n", pod.Name, err)
				}
				found = true
				break
			}
		}
		if !found {
			fmt.Printf("[FAIL] Master %s is a GHOST! No pod found matching this address.\n", currentMasterAddr)
		}
	}

	fmt.Println("Checking Replicas...")
	if len(cCtx.SentinelPods) > 0 {
		sentinelPod := cCtx.SentinelPods[0].Name
		stdout, _, err := k8s.Exec(coreClient, config, cCtx.Namespace, sentinelPod, cCtx.SentinelContainer, []string{"redis-cli", "-p", "26379", "sentinel", "replicas", "mymaster"})
		if err == nil {
			for _, pod := range cCtx.RedisPods {
				// Skip if this pod is the master
				if pod.Status.PodIP == currentMasterAddr || strings.Contains(currentMasterAddr, pod.Name) {
					continue
				}

				// Match by IP or Hostname in the replicas output
				if strings.Contains(stdout, pod.Status.PodIP) || strings.Contains(stdout, pod.Name) {
					fmt.Printf("  [OK] Pod %s (IP: %s) is known to Sentinel as replica.\n", pod.Name, pod.Status.PodIP)
				} else {
					fmt.Printf("  [FAIL] Pod %s (IP: %s) is NOT known to Sentinel!\n", pod.Name, pod.Status.PodIP)
				}
			}
		}
	}

	return nil
}

func init() {
	rootCmd.AddCommand(verifyCmd)
}
