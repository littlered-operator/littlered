package cmd

import (
	"context"
	"fmt"
	"strings"

	"github.com/littlered-operator/littlered-operator/internal/cli/discovery"
	"github.com/littlered-operator/littlered-operator/internal/cli/k8s"
	"github.com/spf13/cobra"
)

var inspectCmd = &cobra.Command{
	Use:   "inspect [name]",
	Short: "Perform a deep-dive diagnostic of a Redis cluster",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		name := args[0]

		// Setup clients
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

		fmt.Printf("Deep Inspect: %s/%s (Mode: %s)\n", cCtx.Namespace, cCtx.Name, cCtx.Mode)
		fmt.Println(strings.Repeat("-", 40))

		// Inspect Sentinels
		for _, pod := range cCtx.SentinelPods {
			fmt.Printf("Sentinel Pod: %s (IP: %s)\n", pod.Name, pod.Status.PodIP)
			cmdArgs := []string{"redis-cli", "-p", "26379", "sentinel", "master", "mymaster"}

			stdout, stderr, err := k8s.Exec(coreClient, config, cCtx.Namespace, pod.Name, cCtx.SentinelContainer, cmdArgs)
			if err != nil {
				fmt.Printf("  [!] Error: %v (stderr: %q)\n", err, stderr)
			} else {
				printLines(stdout)
			}
			fmt.Println()
		}

		// Inspect Redis Nodes
		for _, pod := range cCtx.RedisPods {
			fmt.Printf("Redis Pod: %s (IP: %s)\n", pod.Name, pod.Status.PodIP)

			var cmdArgs []string
			if cCtx.Mode == "cluster" {
				cmdArgs = []string{"sh", "-c", "redis-cli cluster nodes && echo --- && redis-cli cluster info"}
			} else {
				cmdArgs = []string{"redis-cli", "info", "replication"}
			}

			stdout, stderr, err := k8s.Exec(coreClient, config, cCtx.Namespace, pod.Name, cCtx.RedisContainer, cmdArgs)
			if err != nil {
				fmt.Printf("  [!] Error: %v (stderr: %q)\n", err, stderr)
			} else {
				printLines(stdout)
			}
			fmt.Println()
		}

		return nil
	},
}

func printLines(stdout string) {
	lines := strings.Split(strings.TrimSpace(stdout), "\n")
	for _, line := range lines {
		fmt.Printf("  %s\n", line)
	}
}

func init() {
	rootCmd.AddCommand(inspectCmd)
}
