// Command chartvalues generates the YAML blocks the operator Helm chart's
// values.yaml embeds from pkg/nodefeature, so pkg/nodefeature stays their single
// source of truth: the Kueue managerConfig "resources.transformations" list and
// the NodeFeatureRule PCI vendor ID list (see generate.go).
//
// A target file adopts a block by embedding a matching pair of marker comments,
// indented however the surrounding YAML requires:
//
//	# gpustack:chartvalues:kueue-transformations:begin
//	# gpustack:chartvalues:kueue-transformations:end
//
//	# gpustack:chartvalues:nfd-pci-vendor-ids:begin
//	# gpustack:chartvalues:nfd-pci-vendor-ids:end
//
// Running with -values rewrites the text between each pair found in that file to
// the freshly generated content, re-indented to the begin marker's own
// indentation (see patch.go); a block whose markers are absent is left untouched,
// so a target file can adopt one block at a time. A marker name may repeat (e.g.
// once under a "kueue:" values block and once under "kueue-legacy:"); every
// occurrence is kept in sync.
//
// -stdout prints every block, wrapped in its markers, instead of patching a file —
// useful to inspect the generated shape before wiring it into a target.
package main

import (
	"flag"
	"fmt"
	"os"

	klog "k8s.io/klog/v2"
)

func main() {
	stdout := flag.Bool("stdout", false, "print the generated blocks (with markers) to stdout instead of patching a file")
	valuesPath := flag.String("values", "", "path to the file to patch in place between the blocks' markers; required unless -stdout is set")
	flag.Parse()

	if err := run(*stdout, *valuesPath); err != nil {
		klog.Fatalf("error generating chart values: %v", err)
	}
}

func run(stdout bool, valuesPath string) error {
	bs := blocks()

	if stdout {
		for _, b := range bs {
			fmt.Println(beginMarker(b.Name))
			fmt.Print(b.Content)
			fmt.Println(endMarker(b.Name))
			fmt.Println()
		}
		return nil
	}

	if valuesPath == "" {
		return fmt.Errorf("-values is required unless -stdout is set")
	}

	info, err := os.Stat(valuesPath)
	if err != nil {
		return fmt.Errorf("stat %s: %w", valuesPath, err)
	}
	src, err := os.ReadFile(valuesPath)
	if err != nil {
		return fmt.Errorf("read %s: %w", valuesPath, err)
	}
	out, err := Patch(src, bs)
	if err != nil {
		return fmt.Errorf("patch %s: %w", valuesPath, err)
	}
	if err := os.WriteFile(valuesPath, out, info.Mode()); err != nil {
		return fmt.Errorf("write %s: %w", valuesPath, err)
	}
	return nil
}
