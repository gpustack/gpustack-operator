package workergateway

import (
	"context"
	"errors"
	"fmt"

	"github.com/spf13/pflag"
	"go.uber.org/automaxprocs/maxprocs"
	klog "k8s.io/klog/v2"

	"gpustack.ai/gpustack/pkg/utils/gox"
	"gpustack.ai/gpustack/pkg/webserver"
	"gpustack.ai/gpustack/pkg/workergateway/manager"
)

type Options struct {
	// Control.
	GopoolWorkerFactor int

	// Manager.
	ManagerOptions *manager.Options

	// Server.
	ServerOptions *webserver.Options
}

func NewOptions() *Options {
	opts := &Options{
		// Control.
		GopoolWorkerFactor: 100,

		// Manager.
		ManagerOptions: manager.NewOptions(),

		// Server.
		ServerOptions: webserver.NewOptions(),
	}

	opts.ServerOptions.BindUnixPath = "/var/lib/gpustack/gpustack-operator-worker-gateway.sock"
	opts.ServerOptions.BindPort = 0
	return opts
}

func (o *Options) AddFlags(fs *pflag.FlagSet) {
	// Control.
	fs.IntVar(&o.GopoolWorkerFactor, "gopool-worker-factor", o.GopoolWorkerFactor,
		"the number of tasks of the goroutine worker pool, "+
			"it is calculated by the number of CPU cores multiplied by this factor.")

	// Manager.
	o.ManagerOptions.AddFlags(fs)

	// Server.
	o.ServerOptions.AddFlags(fs)
}

func (o *Options) Validate(ctx context.Context) error {
	// Control.
	if o.GopoolWorkerFactor < 100 {
		return errors.New("--gopool-worker-factor: less than 100")
	}

	// Manager.
	if err := o.ManagerOptions.Validate(ctx); err != nil {
		return err
	}

	// Server.
	if err := o.ServerOptions.Validate(ctx); err != nil {
		return err
	}

	return nil
}

func (o *Options) Complete(ctx context.Context) (*Config, error) {
	// Configure goruntime,
	// which is able to query by `runtime.GOMAXPROCS(0)`.
	_, err := maxprocs.Set(maxprocs.Logger(klog.NewStandardLogger("INFO").Printf))
	if err != nil {
		return nil, fmt.Errorf("set maxprocs: %w", err)
	}

	// Configure goroutine pool.
	gox.Configure(o.GopoolWorkerFactor)

	// Manager.
	mgrConfig, err := o.ManagerOptions.Complete(ctx)
	if err != nil {
		return nil, err
	}

	// Server.
	srvConfig, err := o.ServerOptions.Complete(ctx)
	if err != nil {
		return nil, err
	}

	return &Config{
		ManagerConfig: mgrConfig,
		ServerConfig:  srvConfig,
	}, nil
}
