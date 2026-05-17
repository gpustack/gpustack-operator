package main

import (
	"os"

	"github.com/spf13/cobra"

	"gpustack.ai/gpustack/cmd"
	"gpustack.ai/gpustack/pkg/devicemanager"
	"gpustack.ai/gpustack/pkg/utils/signalx"
	"gpustack.ai/gpustack/pkg/worker"
)

func main() {
	n := "gpustack"
	c := &cobra.Command{
		Use:   n,
		Short: "manages GPUStack Kubernetes resources.",
	}
	c.AddCommand(worker.NewCommand())
	c.AddCommand(devicemanager.NewCommand())
	c = cmd.Harness(c)

	if err := c.ExecuteContext(signalx.Handler()); err != nil {
		os.Exit(1)
	}
}
