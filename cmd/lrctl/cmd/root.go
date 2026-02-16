package cmd

import (
	"github.com/spf13/cobra"
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

func init() {
	rootCmd.PersistentFlags().StringVarP(&namespace, "namespace", "n", "", "If present, the namespace scope for this CLI request")
	rootCmd.PersistentFlags().StringVar(&kubeconfig, "kubeconfig", "", "Path to the kubeconfig file to use for CLI requests")
	rootCmd.PersistentFlags().BoolVar(&unmanaged, "unmanaged", false, "If true, skip looking for a LittleRed CR and use heuristics to find pods")
	rootCmd.PersistentFlags().StringVar(&kind, "kind", "sentinel", "The cluster kind (sentinel|cluster) when using --unmanaged")
}
