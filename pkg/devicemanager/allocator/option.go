package allocator

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/spf13/pflag"
	deviceplugin "k8s.io/kubelet/pkg/apis/deviceplugin/v1beta1"

	"gpustack.ai/gpustack/pkg/device"
	"gpustack.ai/gpustack/pkg/utils/osx"
)

type Options struct {
	// Connect.
	KubeSocket string

	// Control.
	NoShared      bool
	NoSliced      bool
	SlicingPolicy string
}

func NewOptions() *Options {
	return &Options{
		// Connect.
		KubeSocket: deviceplugin.KubeletSocket,

		// Control.
		NoShared:      false,
		NoSliced:      false,
		SlicingPolicy: "best-effort",
	}
}

func (o *Options) AddFlags(fs *pflag.FlagSet) {
	// Connect.
	fs.StringVar(&o.KubeSocket, "kube-socket", o.KubeSocket,
		"the kubelet socket path to connect.")

	// Control.
	fs.BoolVar(&o.NoShared, "no-shared", o.NoShared,
		"whether to disable creating shared devices.")
	fs.BoolVar(&o.NoSliced, "no-sliced", o.NoSliced,
		"whether to disable creating sliced devices.")
	fs.StringVar(&o.SlicingPolicy, "slicing-policy", o.SlicingPolicy,
		"the device slicing policy to use. "+
			fmt.Sprintf("Valid values are: %v.", device.GetAllSlicingPolicies()))
}

func (o *Options) Validate(_ context.Context) error {
	// Connect.
	if o.KubeSocket == "" {
		return errors.New("--kube-socket: empty")
	}
	if !osx.ExistsSocket(o.KubeSocket) {
		return errors.New("--kube-socket: not existed or not a socket")
	}

	// Control.
	if !slices.Contains(device.GetAllSlicingPolicies(), o.SlicingPolicy) {
		return errors.New("--slicing-policy: invalid value")
	}

	return nil
}

func (o *Options) Complete(_ context.Context) (*Config, error) {
	return &Config{
		KubeSocket:    o.KubeSocket,
		NoShared:      o.NoShared,
		NoSliced:      o.NoSliced,
		SlicingPolicy: o.SlicingPolicy,
	}, nil
}
