package cmd

import (
	"flag"
	"os"
	"strings"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
	klog "k8s.io/klog/v2"

	"gpustack.ai/gpustack/pkg/utils/osx"
	"gpustack.ai/gpustack/pkg/utils/version"
)

func Harness(c *cobra.Command) *cobra.Command {
	gcl, pcl := flag.CommandLine, pflag.CommandLine

	// Support klog configuration.
	klog.InitFlags(gcl)
	// Default klog configuration.
	{
		_ = gcl.Set("logtostderr", "true")
		_ = gcl.Set("v", "0")
		_ = gcl.Set("add_dir_header", "false")
		_ = gcl.Set("skip_headers", "false")
	}
	// Add klog flags to pflag, so that they can be configured via command line.
	pcl.AddGoFlag(gcl.Lookup("v"))                // --v
	pcl.AddGoFlag(gcl.Lookup("vmodule"))          // --vmodule
	pcl.AddGoFlag(gcl.Lookup("log_backtrace_at")) // --log_backtrace_at
	pcl.AddGoFlag(gcl.Lookup("add_dir_header"))   // --add_dir_header
	pcl.AddGoFlag(gcl.Lookup("logtostderr"))      // --logtostderr
	pcl.AddGoFlag(gcl.Lookup("skip_headers"))     // --skip_headers
	// Hide some flags.
	{
		_ = pcl.MarkHidden("logtostderr")
		_ = pcl.MarkHidden("skip_headers")
	}

	// Support printing command line.
	printCmdline := pcl.Bool("print-cmdline", false,
		"print cmdline, which includes the arguments retrieved from environment.")
	c.PersistentPreRunE = func(c *cobra.Command, args []string) error {
		if *printCmdline {
			c.Printf("%s\n\n", strings.Join(os.Args, " "))
		}
		return nil
	}

	// Silence usage/errors,
	// and return help message if flag error occurs.
	c.SilenceUsage = true
	c.SetFlagErrorFunc(func(c *cobra.Command, err error) error {
		_ = c.Help()
		return err
	})

	// Append version.
	c.Version = version.Get()

	// Retrieve args from environment variables.
	osx.RetrieveArgsFromEnvInto(c)

	return c
}
