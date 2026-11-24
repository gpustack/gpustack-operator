package allocator

import (
	"context"
	"errors"
	"fmt"

	"github.com/spf13/pflag"
	deviceplugin "k8s.io/kubelet/pkg/apis/deviceplugin/v1beta1"

	allocatormock "gpustack.ai/gpustack/pkg/devicemanager/allocator/mock"
	"gpustack.ai/gpustack/pkg/devicemanager/mockspec"
	"gpustack.ai/gpustack/pkg/utils/hostfs"
	"gpustack.ai/gpustack/pkg/utils/osx"
)

// MockDevicesFileEnv names the environment variable supplying the default for --mock-devices-file.
const MockDevicesFileEnv = "GPUSTACK_MOCK_DEVICES_FILE"

type Options struct {
	FlagOptions

	// Connect.
	KubeSocket string
	HostRoot   string

	// Control.
	NoShared        bool
	NoSliced        bool
	NoPartitioned   bool
	MockDevicesFile string
}

func NewOptions() *Options {
	return &Options{
		// Connect.
		KubeSocket: deviceplugin.KubeletSocket,
		HostRoot:   hostfs.DefaultHostRoot,

		// Control.
		NoShared:        false,
		NoSliced:        false,
		NoPartitioned:   false,
		MockDevicesFile: osx.Getenv(MockDevicesFileEnv),
	}
}

type (
	FlagOptions struct {
		noMockDevicesOptions bool
	}

	FlagOption func(opts *FlagOptions)
)

// WithoutMockDevicesOptions skips registering and loading --mock-devices-file on Allocator
// Options when a parent command already registers and shares the mock specification store.
func WithoutMockDevicesOptions() FlagOption {
	return func(opts *FlagOptions) {
		opts.noMockDevicesOptions = true
	}
}

func (o *Options) AddFlags(fs *pflag.FlagSet, opts ...FlagOption) {
	for i := range opts {
		opts[i](&o.FlagOptions)
	}

	// Connect.
	fs.StringVar(&o.KubeSocket, "kube-socket", o.KubeSocket,
		"the kubelet socket path to connect.")
	fs.StringVar(&o.HostRoot, "host-root", o.HostRoot,
		"the mount path of the host root filesystem used for read-only inspection.")

	// Control.
	fs.BoolVar(&o.NoShared, "no-shared", o.NoShared,
		"whether to disable creating shared devices.")
	fs.BoolVar(&o.NoSliced, "no-sliced", o.NoSliced,
		"whether to disable creating sliced devices.")
	fs.BoolVar(&o.NoPartitioned, "no-partitioned", o.NoPartitioned,
		"whether to disable creating hardware-partitioned devices.")
	if !o.noMockDevicesOptions {
		fs.StringVar(&o.MockDevicesFile, "mock-devices-file", o.MockDevicesFile,
			"path to a YAML or JSON mock devices specification file for CPU-only testing "+
				"(defaults to "+MockDevicesFileEnv+").")
	}
}

func (o *Options) Validate(_ context.Context) error {
	// Connect.
	if o.KubeSocket == "" {
		return errors.New("--kube-socket: empty")
	}
	if !osx.ExistsSocket(o.KubeSocket) {
		return errors.New("--kube-socket: not existed or not a socket")
	}
	if !o.noMockDevicesOptions && o.MockDevicesFile != "" {
		if _, err := mockspec.NewStore(o.MockDevicesFile); err != nil {
			return fmt.Errorf("--mock-devices-file: %w", err)
		}
	}

	return nil
}

func (o *Options) Complete(_ context.Context) (*Config, error) {
	cfg := &Config{
		KubeSocket:    o.KubeSocket,
		HostRoot:      o.HostRoot,
		NoShared:      o.NoShared,
		NoSliced:      o.NoSliced,
		NoPartitioned: o.NoPartitioned,
	}
	if !o.noMockDevicesOptions && o.MockDevicesFile != "" {
		specStore, err := mockspec.NewStore(o.MockDevicesFile, mockspec.WithLogger(logger.WithName("mockspec")))
		if err != nil {
			return nil, fmt.Errorf("load mock devices file: %w", err)
		}
		cfg.MockDevicesFile = o.MockDevicesFile
		cfg.MockSpecStore = specStore
		cfg.MockPartitionStore = allocatormock.NewPartitionStore()
	}
	return cfg, nil
}
