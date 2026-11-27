package main

import (
	"os"

	"github.com/spf13/cobra"

	"gpustack.ai/gpustack/cmd"
	"gpustack.ai/gpustack/pkg/devicemanager"
	"gpustack.ai/gpustack/pkg/modelmanager"
	"gpustack.ai/gpustack/pkg/modeltransfer"
	"gpustack.ai/gpustack/pkg/utils/signalx"
	"gpustack.ai/gpustack/pkg/worker"
	"gpustack.ai/gpustack/pkg/worker/forger"
	"gpustack.ai/gpustack/pkg/workergateway"
)

func main() {
	n := "gpustack-operator"
	c := &cobra.Command{
		Use:   n,
		Short: "manages GPUStack Kubernetes resources.",
	}
	c.AddCommand(worker.NewCommand())
	c.AddCommand(workergateway.NewCommand())
	c.AddCommand(devicemanager.NewCommand())
	c.AddCommand(modelmanager.NewCommand())
	c.AddCommand(modeltransfer.NewCommand())
	c.AddCommand(forger.NewForgeMockCommand())
	c.AddCommand(forger.NewForgeVerifyCommand())
	c.AddCommand(forger.NewForgeHoldCommand())
	c = cmd.Harness(c)

	if err := c.ExecuteContext(signalx.Handler()); err != nil {
		os.Exit(1)
	}
}
