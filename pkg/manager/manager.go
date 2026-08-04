package manager

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/pprof"
	"runtime"
	"sync/atomic"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"k8s.io/apiserver/pkg/server/healthz"
	"k8s.io/apiserver/pkg/server/routes"
	"k8s.io/component-base/logs"
	klog "k8s.io/klog/v2"
	ctrlhealthz "sigs.k8s.io/controller-runtime/pkg/healthz"
	ctrlmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"

	"gpustack.ai/gpustack/pkg/kubeclients/kubernetes"
	"gpustack.ai/gpustack/pkg/kubediscovery"
	"gpustack.ai/gpustack/pkg/utils/gox"
	"gpustack.ai/gpustack/pkg/utils/httpx"
)

type Manager struct {
	KubeClient  kubernetes.Interface
	CtrlManager CtrlManager

	sentinel _CtrlManagerSentinel
	stopped  atomic.Bool
}

// Prepare prepares the runtime for the manager,
// including installing CRDs and registering metric collectors.
func (m *Manager) Prepare(_ context.Context) error {
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

// Start starts the manager.
//
// Start sets up controllers and registers assistant routes,
// before starting the controller manager.
func (m *Manager) Start(ctx context.Context) error {
	cm := m.CtrlManager
	mu := cm.GetWebhookServer()

	// Register /metrics.
	{
		h := promhttp.HandlerOpts{
			ErrorLog:      klog.NewStandardLogger("WARNING"),
			ErrorHandling: promhttp.HTTPErrorOnError,
		}
		mu.Register("/metrics", promhttp.HandlerFor(ctrlmetrics.Registry, h))
	}

	// Register /readyz.
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
				"loopback": func(r *http.Request) error {
					return kubediscovery.IsConnected(r.Context(), m.KubeClient.Discovery())
				},
				"informer": func(r *http.Request) error {
					if cm.GetCache().WaitForCacheSync(r.Context()) {
						return nil
					}
					return errors.New("informer cache is not synced yet")
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

	// Start.
	//
	// The controller manager returning is terminal, and not only when it returns an error:
	// controller-runtime stops every controller and returns as soon as the leader lease is lost,
	// which a single slow apiserver round trip is enough to cause. Record that it happened,
	// because the process does not necessarily exit when it does — a sibling task that fails to
	// return on cancellation keeps the process, and its HTTP handlers, alive — and a process that
	// keeps answering its liveness probe while reconciling nothing is the worst of both outcomes:
	// Kubernetes sees a healthy Pod and the whole chain silently stops converging.
	defer m.stopped.Store(true)
	return cm.Start(ctx)
}

// WaitForReady waits for the manager to be ready.
//
// It reports an error once the manager has stopped, and never becomes ready again after that:
// a manager that returned has no controllers running, whatever else about the process still works.
func (m *Manager) WaitForReady(ctx context.Context) error {
	// Asked before the waits below, which a stopped manager would still satisfy: the sentinel
	// stays closed once started, and a stopped cache reports itself synced.
	if m.stopped.Load() {
		return errors.New("controller manager has stopped")
	}

	// Wait for controller manager to start.
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-m.sentinel.Done():
	}

	// Wait for cache sync.
	if !m.CtrlManager.GetCache().WaitForCacheSync(ctx) {
		return errors.New("cache is not synced yet")
	}

	return nil
}
