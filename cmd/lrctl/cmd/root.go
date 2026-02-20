package cmd

import (
	"context"
	"fmt"

	littleredv1alpha1 "github.com/littlered-operator/littlered-operator/api/v1alpha1"
	"github.com/spf13/cobra"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

var (
	namespace  string
	kubeconfig string
	unmanaged  bool
	kind       string
)

var rootCmd = &cobra.Command{
	Use:   "lrctl",
	Short: "lrctl is a CLI tool for managing LittleRed and foreign Redis clusters",
	Long: `lrctl provides syntactic sugar for interacting with LittleRed resources,
but also supports unmanaged clusters via heuristics.`,
}

func Execute() error {
	return rootCmd.Execute()
}

// listLittleRedNames returns the names of all LittleRed CRs in the namespace.
func listLittleRedNames(ctx context.Context, k8sClient client.Client, namespace string) ([]string, error) {
	lrList := &littleredv1alpha1.LittleRedList{}
	if err := k8sClient.List(ctx, lrList, client.InNamespace(namespace)); err != nil {
		return nil, fmt.Errorf("failed to list LittleRed resources: %w", err)
	}
	names := make([]string, len(lrList.Items))
	for i, lr := range lrList.Items {
		names[i] = lr.Name
	}
	return names, nil
}

func init() {
	rootCmd.PersistentFlags().StringVarP(&namespace, "namespace", "n", "", "If present, the namespace scope for this CLI request")
	rootCmd.PersistentFlags().StringVar(&kubeconfig, "kubeconfig", "", "Path to the kubeconfig file to use for CLI requests")
	rootCmd.PersistentFlags().BoolVar(&unmanaged, "unmanaged", false, "If true, skip looking for a LittleRed CR and use heuristics to find pods")
	rootCmd.PersistentFlags().StringVar(&kind, "kind", "sentinel", "The cluster kind (sentinel|cluster) when using --unmanaged")
}
