package manager

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/spf13/pflag"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/client-go/rest"

	"gpustack.ai/gpustack/pkg/kubeconfig"
	"gpustack.ai/gpustack/pkg/utils/version"
)

type Options struct {
	// Control.
	WorkerConnMode              string
	WorkerConnGPUStackAPIScheme string
	WorkerConnGPUStackAPIPort   int
	InformerCacheResyncPeriod   time.Duration

	// Connect Kubernetes.
	KubeConnTimeout time.Duration
	KubeConnQPS     float64
	KubeConnBurst   int
	KubeContentType string
}

func NewOptions() *Options {
	return &Options{
		// Control.
		WorkerConnMode:            "gpustack-api",
		WorkerConnGPUStackAPIPort: 30080,
		InformerCacheResyncPeriod: 1 * time.Hour,

		// Connect Kubernetes.
		KubeConnTimeout: 5 * time.Minute,
		KubeConnQPS:     200,
		KubeConnBurst:   400,
		KubeContentType: "",
	}
}

func (o *Options) AddFlags(fs *pflag.FlagSet) {
	// Control.
	fs.StringVar(&o.WorkerConnMode, "worker-conn-mode", o.WorkerConnMode,
		"the connection mode between worker and manager, currently supports 'gpustack-api' and 'loopback'.")
	fs.StringVar(&o.WorkerConnGPUStackAPIScheme, "worker-conn-gpustack-api-scheme", o.WorkerConnGPUStackAPIScheme,
		"the scheme of the GPUStack server when using 'gpustack-api' connection mode, default to http or https based on the port.")
	fs.IntVar(&o.WorkerConnGPUStackAPIPort, "worker-conn-gpustack-api-port", o.WorkerConnGPUStackAPIPort,
		"the port of the GPUStack server when using 'gpustack-api' connection mode.")
	fs.DurationVar(&o.InformerCacheResyncPeriod, "informer-cache-resync-period", o.InformerCacheResyncPeriod,
		"the period at which the informer's cache is resynced.")

	// Connect Kubernetes.
	fs.DurationVar(&o.KubeConnTimeout, "kube-conn-timeout", o.KubeConnTimeout,
		"the timeout for dialing the loopback Kubernetes cluster.")
	fs.Float64Var(&o.KubeConnQPS, "kube-conn-qps", o.KubeConnQPS,
		"the QPS(maximum average number per second) when dialing the loopback Kubernetes cluster.")
	fs.IntVar(&o.KubeConnBurst, "kube-conn-burst", o.KubeConnBurst,
		"the burst(maximum number at the same moment) when dialing the loopback Kubernetes cluster.")
	fs.StringVar(&o.KubeContentType, "kube-content-type", o.KubeContentType,
		"the content type of the requests when dialing the loopback Kubernetes cluster.")
}

func (o *Options) Validate(_ context.Context) error {
	// Control.
	if !sets.New[string]("gpustack-api", "loopback").Has(o.WorkerConnMode) {
		return errors.New("--worker-conn-mode: invalid connection mode")
	}
	if o.WorkerConnGPUStackAPIPort <= 0 || o.WorkerConnGPUStackAPIPort > 65535 {
		return errors.New("--worker-conn-gpustack-api-port: invalid port")
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

	return nil
}

func (o *Options) Complete(_ context.Context) (*Config, error) {
	var constructRestConfig ConstructRestConfigFunc
	switch o.WorkerConnMode {
	case "gpustack-api":
		constructRestConfig = constructRestConfigByGPUStackAPI(o.WorkerConnGPUStackAPIPort, o)
	case "loopback":
		c, err := constructRestConfigByLoopback(o)
		if err != nil {
			return nil, fmt.Errorf("construct rest config by loopback: %w", err)
		}
		constructRestConfig = c
	default:
		return nil, errors.New("invalid worker connection mode")
	}

	return &Config{
		ConstructRestConfig: constructRestConfig,
		ResyncPeriod:        o.InformerCacheResyncPeriod,
	}, nil
}

func constructRestConfigByGPUStackAPI(port int, o *Options) ConstructRestConfigFunc {
	return func(cluster, token string) (*rest.Config, error) {
		scheme := o.WorkerConnGPUStackAPIScheme
		if scheme == "" {
			scheme = "http"
			if port == 443 {
				scheme = "https"
			}
		}

		cfg := &rest.Config{
			Host:        fmt.Sprintf("%s://localhost:%d/v2/clusters/%s/proxy", scheme, port, cluster),
			BearerToken: token,
		}
		cfg.UserAgent = version.GetUserAgent()
		cfg.Timeout = o.KubeConnTimeout
		cfg.QPS = float32(o.KubeConnQPS)
		cfg.Burst = o.KubeConnBurst
		cfg.ContentType = o.KubeContentType
		if scheme == "https" {
			cfg.Insecure = true
		}

		return cfg, nil
	}
}

func constructRestConfigByLoopback(o *Options) (ConstructRestConfigFunc, error) {
	_, cfg, _, err := kubeconfig.LoadRestConfigNonInteractive()
	if err != nil {
		return nil, fmt.Errorf("load kubeconfig: %w", err)
	}

	cfg.UserAgent = version.GetUserAgent()
	cfg.Timeout = o.KubeConnTimeout
	cfg.QPS = float32(o.KubeConnQPS)
	cfg.Burst = o.KubeConnBurst
	cfg.ContentType = o.KubeContentType

	return func(_, _ string) (*rest.Config, error) {
		return cfg, nil
	}, nil
}
