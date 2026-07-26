package allocator

import (
	"context"
	"errors"

	"github.com/spf13/pflag"
	deviceplugin "k8s.io/kubelet/pkg/apis/deviceplugin/v1beta1"

	"gpustack.ai/gpustack/pkg/utils/osx"
)

type Options struct {
	// Connect.
	KubeSocket string

	// Control.
	NoShared      bool
	NoSliced      bool
	NoPartitioned bool
}

func NewOptions() *Options {
	return &Options{
		// Connect.
		KubeSocket: deviceplugin.KubeletSocket,

		// Control.
		NoShared:      false,
		NoSliced:      false,
		NoPartitioned: false,
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
	fs.BoolVar(&o.NoPartitioned, "no-partitioned", o.NoPartitioned,
		"whether to disable creating hardware-partitioned devices.")
}

func (o *Options) Validate(_ context.Context) error {
	// Connect.
	if o.KubeSocket == "" {
		return errors.New("--kube-socket: empty")
	}
	if !osx.ExistsSocket(o.KubeSocket) {
		return errors.New("--kube-socket: not existed or not a socket")
	}

	return nil
}

func (o *Options) Complete(_ context.Context) (*Config, error) {
	return &Config{
		KubeSocket:    o.KubeSocket,
		NoShared:      o.NoShared,
		NoSliced:      o.NoSliced,
		NoPartitioned: o.NoPartitioned,
	}, nil
}
