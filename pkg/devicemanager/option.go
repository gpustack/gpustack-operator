package devicemanager

import (
	"context"

	"github.com/spf13/pflag"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/sets"

	"gpustack.ai/gpustack/pkg/devicemanager/allocator"
	"gpustack.ai/gpustack/pkg/devicemanager/detector"
	"gpustack.ai/gpustack/pkg/manager"
	"gpustack.ai/gpustack/pkg/webserver"
)

type Options struct {
	// Server.
	ServerOptions *webserver.Options

	// Manager.
	ManagerOptions *manager.Options

	// Detector.
	DetectorOptions *detector.Options

	// Allocator.
	AllocatorOptions *allocator.Options
}

func NewOptions() *Options {
	opts := &Options{
		// Server.
		ServerOptions: webserver.NewOptions(),

		// Manager.
		ManagerOptions: manager.NewOptions(),

		// Detector.
		DetectorOptions: detector.NewOptions(),

		// Allocator.
		AllocatorOptions: allocator.NewOptions(),
	}
	opts.ServerOptions.BindPort = 32443
	opts.ManagerOptions.KubeContentType = runtime.ContentTypeJSON

	return opts
}

func (o *Options) AddFlags(fs *pflag.FlagSet) {
	// Server.
	o.ServerOptions.AddFlags(fs)

	// Manager.
	o.ManagerOptions.AddFlags(fs, manager.WithoutKubeElectionOptions())

	// Detector.
	o.DetectorOptions.AddFlags(fs)

	// Allocator.
	o.AllocatorOptions.AddFlags(fs)
}

func (o *Options) Validate(ctx context.Context) error {
	// Server.
	if err := o.ServerOptions.Validate(ctx); err != nil {
		return err
	}

	// Manager.
	if err := o.ManagerOptions.Validate(ctx); err != nil {
		return err
	}

	// Detector.
	if err := o.DetectorOptions.Validate(ctx); err != nil {
		return err
	}

	// Allocator.
	if err := o.AllocatorOptions.Validate(ctx); err != nil {
		return err
	}

	return nil
}

func (o *Options) Complete(ctx context.Context) (*Config, error) {
	// Server.
	srvConfig, err := o.ServerOptions.Complete(ctx)
	if err != nil {
		return nil, err
	}

	// Manager.
	mgrConfig, err := o.ManagerOptions.Complete(ctx)
	if err != nil {
		return nil, err
	}

	// Detector.
	detConfig, err := o.DetectorOptions.Complete(ctx)
	if err != nil {
		return nil, err
	}

	// Allocator.
	alcConfig, err := o.AllocatorOptions.Complete(ctx)
	if err != nil {
		return nil, err
	}

	// Create a channel for sharing detected manufacturers between detector and allocator.
	detectedManufacturersCh := make(chan sets.Set[string], 4)
	detConfig.DetectedManufacturersCh = detectedManufacturersCh
	alcConfig.DetectedManufacturersCh = detectedManufacturersCh

	return &Config{
		ServerConfig:    srvConfig,
		ManagerConfig:   mgrConfig,
		DetectorConfig:  detConfig,
		AllocatorConfig: alcConfig,
	}, nil
}
