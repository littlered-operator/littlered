package cmd

import (
	"context"
	"fmt"

	littleredv1alpha1 "github.com/littlered-operator/littlered-operator/api/v1alpha1"
	"github.com/spf13/cobra"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

var (
	namespace     string
	kubeconfig    string
	unmanaged     bool
	kind          string
	allNamespaces bool
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

// listLittleReds returns ObjectKeys for all LittleRed CRs.
// Pass namespace="" to list across all namespaces.
func listLittleReds(ctx context.Context, k8sClient client.Client, namespace string) ([]client.ObjectKey, error) {
	lrList := &littleredv1alpha1.LittleRedList{}
	opts := []client.ListOption{}
	if namespace != "" {
		opts = append(opts, client.InNamespace(namespace))
	}
	if err := k8sClient.List(ctx, lrList, opts...); err != nil {
		return nil, fmt.Errorf("failed to list LittleRed resources: %w", err)
	}
	keys := make([]client.ObjectKey, len(lrList.Items))
	for i, lr := range lrList.Items {
		keys[i] = client.ObjectKey{Name: lr.Name, Namespace: lr.Namespace}
	}
	return keys, nil
}

// resolveTargets returns the set of (namespace, name) pairs a command should operate on.
// It honours --all-namespaces / -A and the positional name argument.
func resolveTargets(ctx context.Context, k8sClient client.Client, args []string, targetNS string) ([]client.ObjectKey, error) {
	if allNamespaces && len(args) > 0 {
		return nil, fmt.Errorf("a resource name may not be specified when --all-namespaces (-A) is set")
	}
	if len(args) == 1 {
		return []client.ObjectKey{{Name: args[0], Namespace: targetNS}}, nil
	}
	// No name given: list all in the target namespace (or all namespaces).
	listNS := targetNS
	if allNamespaces {
		listNS = ""
	}
	return listLittleReds(ctx, k8sClient, listNS)
}

func init() {
	rootCmd.PersistentFlags().StringVarP(&namespace, "namespace", "n", "", "If present, the namespace scope for this CLI request")
	rootCmd.PersistentFlags().StringVar(&kubeconfig, "kubeconfig", "", "Path to the kubeconfig file to use for CLI requests")
	rootCmd.PersistentFlags().BoolVar(&unmanaged, "unmanaged", false, "If true, skip looking for a LittleRed CR and use heuristics to find pods")
	rootCmd.PersistentFlags().StringVar(&kind, "kind", "sentinel", "The cluster kind (sentinel|cluster) when using --unmanaged")
	rootCmd.PersistentFlags().BoolVarP(&allNamespaces, "all-namespaces", "A", false, "If present, list resources across all namespaces")
}
