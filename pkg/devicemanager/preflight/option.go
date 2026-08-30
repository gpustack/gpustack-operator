package preflight

import (
	"context"
	"errors"
	"slices"
	"strings"

	"github.com/spf13/pflag"
	"k8s.io/apimachinery/pkg/util/sets"

	"gpustack.ai/gpustack/pkg/nodefeature"
)

type Options struct {
	// Control.
	NoPCICheck    bool
	Manufacturers []string
	DryRun        bool
	ProbeImage    string
	HostRoot      string
	Runtime       string
}

func NewOptions() *Options {
	return &Options{
		// Control.
		NoPCICheck:    false,
		Manufacturers: nodefeature.GetKnownAcceleratableManufacturers(),
		HostRoot:      DefaultHostRoot,
	}
}

func (o *Options) AddFlags(fs *pflag.FlagSet) {
	// Control.
	fs.BoolVar(&o.NoPCICheck, "no-pci-check", o.NoPCICheck,
		"disable pci check.")
	fs.StringSliceVar(&o.Manufacturers, "manufacturer", o.Manufacturers,
		"comma separated list of manufacturers to preflight.")
	fs.BoolVar(&o.DryRun, "dry-run", o.DryRun,
		"print the container steps instead of taking them, and write nothing to the host; each "+
			"answer then reports what would have run, complete and runnable as printed.")
	fs.StringVar(&o.ProbeImage, "probe-image", o.ProbeImage,
		"the image the probe containers run, overriding the default derived from the accelerator "+
			"family the detect pass reported; required for a family that has no default.")
	fs.StringVar(&o.HostRoot, "host-root", o.HostRoot,
		"where the host's own root filesystem is mounted into this container; "+
			"the host's container and vendor CLIs are invoked by entering it.")
	fs.StringVar(&o.Runtime, "runtime", o.Runtime,
		"the host container runtime to drive, overriding what was resolved; empty follows the "+
			"kubelet's own CRI endpoint, falling back to probing "+strings.Join(hostRuntimes, ", ")+
			" on a host that names none.")
}

// Validate refuses a command line this command cannot act on, before any of it runs.
//
// --runtime names one of a fixed set, so a name outside it is a typo rather than a request, and the
// distinction matters for what happens next: the resolution downstream treats a runtime it cannot
// drive the same way it treats a host that has none, and falls through to emitting the steps for
// somebody else to run. That is the right answer for a node whose runtime this command cannot reach,
// and the wrong one for "--runtime dokcer" -- it would exit 0 having preflighted nothing the operator
// asked about. Deciding it here keeps the two apart: a name is a usage error, a host is a host.
func (o *Options) Validate(_ context.Context) error {
	// Control.
	if len(o.Manufacturers) != 0 {
		knownManufacturers := nodefeature.GetKnownAcceleratableManufacturers()
		if !sets.New[string](knownManufacturers...).HasAll(o.Manufacturers...) {
			return errors.New("--manufacturer: should be a comma separated list of valid manufacturers, " +
				"valid manufacturers: " + strings.Join(knownManufacturers, ","))
		}
	}

	if o.Runtime != "" && !slices.Contains(hostRuntimes, o.Runtime) {
		return errors.New("--runtime: should be a container runtime this command drives, " +
			"valid runtimes: " + strings.Join(hostRuntimes, ","))
	}

	return nil
}

func (o *Options) Complete(_ context.Context) (*Config, error) {
	return &Config{
		NoPCICheck:    o.NoPCICheck,
		Manufacturers: sets.New(o.Manufacturers...),
		DryRun:        o.DryRun,
		ProbeImage:    o.ProbeImage,
		HostRoot:      o.HostRoot,
		Runtime:       o.Runtime,
	}, nil
}
