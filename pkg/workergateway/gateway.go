package workergateway

import (
	"context"
	"fmt"
	"net/http"
	"net/http/pprof"
	"runtime"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"k8s.io/apiserver/pkg/server/healthz"
	"k8s.io/apiserver/pkg/server/routes"
	"k8s.io/component-base/logs"
	"k8s.io/component-base/metrics/legacyregistry"
	klog "k8s.io/klog/v2"
	ctrlhealthz "sigs.k8s.io/controller-runtime/pkg/healthz"
	ctrlmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"

	"gpustack.ai/gpustack/pkg/utils/gox"
	"gpustack.ai/gpustack/pkg/utils/httpx"
	"gpustack.ai/gpustack/pkg/webserver"
	"gpustack.ai/gpustack/pkg/workergateway/manager"
)

func init() {
	ctrlmetrics.Registry = struct {
		prometheus.Registerer
		prometheus.Gatherer
	}{
		Registerer: legacyregistry.Registerer(),
		Gatherer:   legacyregistry.DefaultGatherer,
	}
}

type WorkerGateway struct {
	// Manager.
	Manager manager.Manager

	// Server.
	Server webserver.Server
}

// Prepare prepares the runtime for the worker gateway,
// including installing system resources, etc.
func (wg *WorkerGateway) Prepare(ctx context.Context) error {
	// Register metric collectors.
	{
		reg := ctrlmetrics.Registry
		cs := []prometheus.Collector{
			collectors.NewBuildInfoCollector(),
			gox.NewStatsCollector(),
		}
		for i := range cs {
			err := reg.Register(cs[i])
			if err != nil {
				return fmt.Errorf("register metric collector: %w", err)
			}
		}
	}

	return nil
}

func (wg *WorkerGateway) Start(ctx context.Context) error {
	mu := wg.Server

	// Register /metrics.
	{
		h := promhttp.HandlerOpts{
			ErrorLog:      klog.NewStandardLogger("WARNING"),
			ErrorHandling: promhttp.HTTPErrorOnError,
		}
		mu.Register("/metrics", promhttp.HandlerFor(ctrlmetrics.Registry, h))
	}

	// Register /healthz.
	{
		p := "/readyz"
		h := &ctrlhealthz.Handler{
			Checks: map[string]ctrlhealthz.Checker{
				"ping": ctrlhealthz.Ping,
				"log":  healthz.LogHealthz.Check,
			},
		}
		mu.Register(p, http.StripPrefix(p, h))
	}

	// Register /livez.
	{
		p := "/livez"
		h := &ctrlhealthz.Handler{
			Checks: map[string]ctrlhealthz.Checker{
				"ping": ctrlhealthz.Ping,
				"log":  healthz.LogHealthz.Check,
				"gopool": func(r *http.Request) error {
					return gox.IsHealthy()
				},
			},
		}
		mu.Register(p, http.StripPrefix(p, h))
	}

	// Register /debug.
	{
		runtime.SetBlockProfileRate(1)
		mu.Register("/debug/pprof/", httpx.LoopbackAccessHandlerFunc(pprof.Index))
		mu.Register("/debug/pprof/cmdline", httpx.LoopbackAccessHandlerFunc(pprof.Cmdline))
		mu.Register("/debug/pprof/profile", httpx.LoopbackAccessHandlerFunc(pprof.Profile))
		mu.Register("/debug/pprof/symbol", httpx.LoopbackAccessHandlerFunc(pprof.Symbol))
		mu.Register("/debug/pprof/trace", httpx.LoopbackAccessHandlerFunc(pprof.Trace))
		mu.Register("/debug/flags/v", httpx.LoopbackAccessHandlerFunc(routes.StringFlagPutHandler(logs.GlogSetter)))
	}

	// Register API routes.
	{
		p := "/apis"
		mu.Register(p+"/", http.StripPrefix(p, wg.getHandleApis()))
	}

	klog.Info("starting worker gateway")
	return mu.Start(ctx)
}
