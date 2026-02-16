package main

import (
	"fmt"
	"os"

	"github.com/littlered-operator/littlered-operator/cmd/lrctl/cmd"
)

func main() {
	if err := cmd.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
