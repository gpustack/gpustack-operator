package manager

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/spf13/pflag"
	"go.uber.org/automaxprocs/maxprocs"
	"k8s.io/client-go/rest"
	klog "k8s.io/klog/v2"

	ipk "gpustack.ai/gpustack/pkg/internalprocesses/kubernetes"
	"gpustack.ai/gpustack/pkg/kubeclients/kubernetes"
	"gpustack.ai/gpustack/pkg/kubeconfig"
	"gpustack.ai/gpustack/pkg/kubediscovery"
	"gpustack.ai/gpustack/pkg/system"
	"gpustack.ai/gpustack/pkg/utils/gox"
	"gpustack.ai/gpustack/pkg/utils/version"
	"gpustack.ai/gpustack/pkg/webserver"
)

type Options struct {
	FlagOptions

	// Control.
	GopoolWorkerFactor        int
	InformerCacheResyncPeriod time.Duration
	AggressiveEventFiltering  bool

	// Connect Kubernetes.
	KubeConnTimeout        time.Duration
	KubeConnQPS            float64
	KubeConnBurst          int
	KubeContentType        string
	KubeLeaderElection     bool
	KubeLeaderElectionID   string
	KubeLeaderLease        time.Duration
	KubeLeaderRenewTimeout time.Duration
}

func NewOptions() *Options {
	return &Options{
		// Control.
		GopoolWorkerFactor:        100,
		InformerCacheResyncPeriod: 1 * time.Hour,

		// Connect Kubernetes.
		KubeConnTimeout:        5 * time.Minute,
		KubeConnQPS:            200,
		KubeConnBurst:          400,
		KubeContentType:        "",
		KubeLeaderElection:     true,
		KubeLeaderElectionID:   "leader.gpustack.ai",
		KubeLeaderLease:        15 * time.Second,
		KubeLeaderRenewTimeout: 10 * time.Second,
	}
}

type (
	FlagOptions struct {
		noKubeElectionOptions bool
	}

	FlagOption func(opts *FlagOptions)
)

func WithoutKubeElectionOptions() FlagOption {
	return func(opts *FlagOptions) {
		opts.noKubeElectionOptions = true
	}
}

func (o *Options) AddFlags(fs *pflag.FlagSet, opts ...FlagOption) {
	for i := range opts {
		opts[i](&o.FlagOptions)
	}

	// Control.
	fs.IntVar(&o.GopoolWorkerFactor, "gopool-worker-factor", o.GopoolWorkerFactor,
		"the number of tasks of the goroutine worker pool, "+
			"it is calculated by the number of CPU cores multiplied by this factor.")
	fs.DurationVar(&o.InformerCacheResyncPeriod, "informer-cache-resync-period", o.InformerCacheResyncPeriod,
		"the period at which the informer's cache is resynced.")
	fs.BoolVar(&o.AggressiveEventFiltering, "aggressive-event-filtering", o.AggressiveEventFiltering,
		"indicates to reduce event filtering threshold to make the controllers more aggressive to react to the changes of the cluster.")

	// Connect Kubernetes.
	fs.DurationVar(&o.KubeConnTimeout, "kube-conn-timeout", o.KubeConnTimeout,
		"the timeout for dialing the loopback Kubernetes cluster.")
	fs.Float64Var(&o.KubeConnQPS, "kube-conn-qps", o.KubeConnQPS,
		"the QPS(maximum average number per second) when dialing the loopback Kubernetes cluster.")
	fs.IntVar(&o.KubeConnBurst, "kube-conn-burst", o.KubeConnBurst,
		"the burst(maximum number at the same moment) when dialing the loopback Kubernetes cluster.")
	fs.StringVar(&o.KubeContentType, "kube-content-type", o.KubeContentType,
		"the content type of the requests when dialing the loopback Kubernetes cluster.")
	if !o.noKubeElectionOptions {
		fs.BoolVar(&o.KubeLeaderElection, "kube-leader-election", o.KubeLeaderElection,
			"the config to determines whether or not to use leader election, "+
				"leader election is primarily used in multi-instance deployments.")
		fs.StringVar(&o.KubeLeaderElectionID, "kube-leader-election-id", o.KubeLeaderElectionID,
			"the unique ID of the leader election")
		fs.DurationVar(&o.KubeLeaderLease, "kube-leader-lease", o.KubeLeaderLease,
			"the duration to keep the leadership. "+
				"if --kube-leader-election=false, this flag will be ignored. "+
				"when the network environment is not ideal or do not want to cause frequent access to the cluster, "+
				"please increase the value appropriately.")
		fs.DurationVar(&o.KubeLeaderRenewTimeout, "kube-leader-renew-timeout", o.KubeLeaderRenewTimeout,
			"the duration to renew the leadership before give up, "+
				"must be less than the duration of --kube-leader-lease."+
				"if --kube-leader-election=false, this flag will be ignored. "+
				"when the network environment is not ideal, please increase the value appropriately.")
	} else {
		o.KubeLeaderElection = false
	}
}

func (o *Options) Validate(_ context.Context) error {
	// Control.
	if o.GopoolWorkerFactor < 100 {
		return errors.New("--gopool-worker-factor: less than 100")
	}
	if o.InformerCacheResyncPeriod < 5*time.Minute {
		return errors.New("--informer-cache-resync-period: less than 5 minutes")
	}

	// Connect Kubernetes.
	if o.KubeConnTimeout < 10*time.Second {
		return errors.New("--kube-conn-timeout: less than 10 seconds")
	}
	switch {
	case o.KubeConnQPS < 10:
		return errors.New("--kube-conn-qps: less than 10")
	case o.KubeConnBurst < 10:
		return errors.New("--kube-conn-burst: less than 10")
	case float64(o.KubeConnBurst) <= o.KubeConnQPS:
		return errors.New("--kube-conn-burst: less than --kube-conn-qps")
	}
	if o.noKubeElectionOptions {
		switch {
		case o.KubeLeaderLease < 5*time.Second:
			return errors.New("--kube-leader-lease: less than 5 seconds")
		case o.KubeLeaderRenewTimeout < 5*time.Second:
			return errors.New("--kube-leader-renew-timeout: less than 5 seconds")
		case o.KubeLeaderLease <= o.KubeLeaderRenewTimeout:
			return errors.New("--kube-leader-lease: less than --kube-leader-renew-timeout")
		}
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

	// Configure network.
	err = system.ConfigureNetwork()
	if err != nil {
		return nil, fmt.Errorf("configure network: %w", err)
	}

	// Get loopback config.
	lpCfgPath, lpRestCfg, lpInside, err := kubeconfig.LoadRestConfigNonInteractive()
	if err != nil {
		var (
			embedded    ipk.Embedded
			ctx, cancel = context.WithCancel(ctx)
		)
		gox.Go(func() {
			defer cancel()
			klog.Info("!!! starting embedded Kubernetes !!!")

			err := embedded.Start(ctx)
			if err != nil {
				klog.Error(err, "start embedded Kubernetes")
			}
		})

		lpCfgPath, lpRestCfg, err = embedded.GetConfig(ctx)
		if err != nil {
			return nil, fmt.Errorf("get embedded Kubernetes config: %w", err)
		}
	}

	// Set the timeout, QPS and burst of the rest config.
	lpRestCfg.Timeout = o.KubeConnTimeout
	lpRestCfg.QPS = float32(o.KubeConnQPS)
	lpRestCfg.Burst = o.KubeConnBurst
	lpRestCfg.ContentType = o.KubeContentType
	lpRestCfg.UserAgent = version.GetUserAgent()

	// Get loopback client.
	lpHttpCli, err := rest.HTTPClientFor(lpRestCfg)
	if err != nil {
		return nil, fmt.Errorf("create http client: %w", err)
	}

	lpCli, err := kubernetes.NewForConfigAndClient(rest.CopyConfig(lpRestCfg), lpHttpCli)
	if err != nil {
		return nil, fmt.Errorf("create authorization client: %w", err)
	}
	klog.Info("waiting loopback Kubernetes cluster to be connected")
	err = kubediscovery.WaitUntilConnected(ctx, lpCli.Discovery())
	if err != nil {
		return nil, fmt.Errorf("wait loopback Kubernetes cluster ready: %w", err)
	}

	// Configure loopback Kubernetes,
	// including the client config and client.
	system.ConfigureLoopbackKube(lpInside, lpCfgPath, *lpRestCfg, lpHttpCli, lpCli)

	return &Config{
		InformerCacheResyncPeriod: o.InformerCacheResyncPeriod,
		AggressiveEventFiltering:  o.AggressiveEventFiltering,
		LoopbackKubeConfigPath:    lpCfgPath,
		LoopbackKubeRestConfig:    *lpRestCfg,
		LoopbackKubeHTTPClient:    lpHttpCli,
		LoopbackKubeClient:        lpCli,
		KubeLeaderElection:        o.KubeLeaderElection,
		KubeLeaderElectionID:      o.KubeLeaderElectionID,
		KubeLeaderLease:           o.KubeLeaderLease,
		KubeLeaderRenewTimeout:    o.KubeLeaderRenewTimeout,
		WebhookServer:             webserver.Null(),
	}, nil
}
