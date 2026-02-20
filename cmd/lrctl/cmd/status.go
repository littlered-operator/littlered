package cmd

import (
	"context"
	"fmt"

	littleredv1alpha1 "github.com/littlered-operator/littlered-operator/api/v1alpha1"
	"github.com/littlered-operator/littlered-operator/internal/cli/k8s"
	"github.com/spf13/cobra"
)

var statusCmd = &cobra.Command{
	Use:   "status [name]",
	Short: "Show status of a LittleRed cluster (omit name to list all in namespace)",
	Args:  cobra.MaximumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		k8sClient, _, _, defaultNS, err := k8s.NewClient(kubeconfig)
		if err != nil {
			return err
		}

		targetNS := namespace
		if targetNS == "" {
			targetNS = defaultNS
		}

		ctx := context.Background()

		targets, err := resolveTargets(ctx, k8sClient, args, targetNS)
		if err != nil {
			return err
		}
		if len(targets) == 0 {
			if allNamespaces {
				fmt.Println("No LittleRed resources found in any namespace")
			} else {
				fmt.Printf("No LittleRed resources found in namespace %q\n", targetNS)
			}
			return nil
		}

		var jsonResults []statusJSON

		for i, key := range targets {
			lr := &littleredv1alpha1.LittleRed{}
			if err := k8sClient.Get(ctx, key, lr); err != nil {
				return fmt.Errorf("failed to get LittleRed %s/%s: %w", key.Namespace, key.Name, err)
			}
			if jsonOutput {
				jsonResults = append(jsonResults, lrToStatusJSON(lr))
			} else {
				if i > 0 {
					fmt.Println()
				}
				printStatus(lr)
			}
		}

		if jsonOutput {
			return printJSON(jsonResults)
		}
		return nil
	},
}

func printStatus(lr *littleredv1alpha1.LittleRed) {
	fmt.Printf("Cluster: %s\n", lr.Name)
	fmt.Printf("Namespace: %s\n", lr.Namespace)
	fmt.Printf("Phase: %s\n", lr.Status.Phase)
	fmt.Printf("Mode: %s\n", lr.Spec.Mode)

	if lr.Status.Master != nil {
		fmt.Printf("Master: %s (IP: %s)\n", lr.Status.Master.PodName, lr.Status.Master.IP)
	} else {
		fmt.Printf("Master: <none>\n")
	}

	if lr.Status.Sentinels != nil {
		fmt.Printf("Sentinels: %d/%d Ready\n", lr.Status.Sentinels.Ready, lr.Status.Sentinels.Total)
	}
	fmt.Printf("Redis Nodes: %d/%d Ready\n", lr.Status.Redis.Ready, lr.Status.Redis.Total)
}

func init() {
	rootCmd.AddCommand(statusCmd)
}
