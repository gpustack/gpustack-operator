package worker

import (
	"context"
	"fmt"
	"net/http"
	"net/http/pprof"
	"runtime"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	admreg "k8s.io/api/admissionregistration/v1"
	genericapiserver "k8s.io/apiserver/pkg/server"
	"k8s.io/apiserver/pkg/server/healthz"
	"k8s.io/apiserver/pkg/server/routes"
	"k8s.io/client-go/dynamic"
	"k8s.io/component-base/logs"
	"k8s.io/component-base/metrics/legacyregistry"
	klog "k8s.io/klog/v2"
	apireg "k8s.io/kube-aggregator/pkg/apis/apiregistration/v1"
	"k8s.io/utils/ptr"
	ctrlmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"

	"gpustack.ai/gpustack/pkg/kubediscovery"
	"gpustack.ai/gpustack/pkg/manager"
	"gpustack.ai/gpustack/pkg/peer"
	"gpustack.ai/gpustack/pkg/system"
	"gpustack.ai/gpustack/pkg/utils/gox"
	"gpustack.ai/gpustack/pkg/utils/httpx"
	"gpustack.ai/gpustack/pkg/worker/apis"
	"gpustack.ai/gpustack/pkg/worker/controllers"
	"gpustack.ai/gpustack/pkg/worker/extensionapis"
	"gpustack.ai/gpustack/pkg/worker/extensionroutes"
	"gpustack.ai/gpustack/pkg/worker/kuberess"
	"gpustack.ai/gpustack/pkg/worker/settings"
	"gpustack.ai/gpustack/pkg/worker/webhooks"
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

type Worker struct {
	RoutingPort     int32
	RoutingCaBundle []byte
	Manufacturers   []string
	Manager         *manager.Manager
	APIServer       *genericapiserver.GenericAPIServer
	PeerDp          *peer.DataPlane
}

func (w *Worker) Prepare(ctx context.Context) error {
	err := w.Manager.Prepare(ctx)
	if err != nil {
		return err
	}

	lpCli := system.LoopbackKubeClient.Get()

	// Install system namespace.
	err = kuberess.InstallSystemNamespace(ctx, lpCli)
	if err != nil {
		return fmt.Errorf("install system namespace: %w", err)
	}

	// Install fake system routing service if needed.
	if !system.LoopbackKubeInside.Get() && system.LoopbackKubeNearby.Get() {
		// NB(thxCode): Need to enable the loopback Kubernetes APIServer's `--enable-aggregator-routing` flag also.
		err = kuberess.InstallFakeSystemRoutingService(ctx, lpCli, w.RoutingPort)
		if err != nil {
			return fmt.Errorf("install fake system routing service: %w", err)
		}
	}

	// Initialize CRDs.
	err = apis.InstallCRDs(ctx, lpCli)
	if err != nil {
		return fmt.Errorf("install CRDs: %w", err)
	}

	// Install extension API services.
	{
		svcRef := apireg.ServiceReference{
			Namespace: kuberess.SystemNamespaceName,
			Name:      kuberess.SystemRoutingServiceName,
			Port:      ptr.To(w.RoutingPort),
		}
		err = apis.InstallServices(ctx, lpCli, svcRef, w.RoutingCaBundle)
		if err != nil {
			return fmt.Errorf("install extension API services: %w", err)
		}
	}

	// Install webhook configurations.
	{
		cc := admreg.WebhookClientConfig{
			Service: &admreg.ServiceReference{
				Namespace: kuberess.SystemNamespaceName,
				Name:      kuberess.SystemRoutingServiceName,
				Port:      ptr.To(w.RoutingPort),
			},
		}
		// If we stand closed to loopback Kubernetes cluster but not inside,
		// we can use the primary IP address to access the webhook server.
		if !system.LoopbackKubeInside.Get() && system.LoopbackKubeNearby.Get() {
			// NB(thxCode): launch multiple instances, only one takes working.
			ep := fmt.Sprintf("https://%s:%d", system.PrimaryIP.Get(), w.RoutingPort)
			cc = admreg.WebhookClientConfig{
				URL: ptr.To(ep),
			}
		}
		// Install webhook configurations.
		cc.CABundle = w.RoutingCaBundle
		err = webhooks.Install(ctx, lpCli, cc)
		if err != nil {
			return fmt.Errorf("install webhook configurations: %w", err)
		}
	}

	// Initialize settings.
	err = settings.Initialize(ctx, lpCli)
	if err != nil {
		return fmt.Errorf("initialize settings: %w", err)
	}

	// Install applications.
	err = kuberess.InstallApplications(ctx, w.Manufacturers)
	if err != nil {
		return fmt.Errorf("install applications: %w", err)
	}

	return nil
}

func (w *Worker) Start(ctx context.Context) error {
	cm := w.Manager.CtrlManager
	mu := w.APIServer.Handler.NonGoRestfulMux

	// Setup extension API handlers.
	err := extensionapis.Setup(ctx, w.APIServer, cm)
	if err != nil {
		return fmt.Errorf("setup extension API handlers: %w", err)
	}

	// Setup webhook handlers.
	err = webhooks.Setup(ctx, cm, mu)
	if err != nil {
		return fmt.Errorf("setup webhooks: %w", err)
	}

	// Register /metrics.
	mu.Handle("/metrics", legacyregistry.Handler())

	// Register /livez.
	{
		err = w.APIServer.AddLivezChecks(10*time.Second,
			healthz.NamedCheck("gopool", func(r *http.Request) error {
				return gox.IsHealthy()
			}),
			healthz.NamedCheck("loopback", func(r *http.Request) error {
				return kubediscovery.IsConnectedWithRestConfig(r.Context(), dynamic.ConfigFor(cm.GetConfig()))
			}),
			healthz.NamedCheck("manager", func(r *http.Request) error {
				return w.Manager.WaitForReady(r.Context())
			}),
		)
		if err != nil {
			return fmt.Errorf("add livez checks: %w", err)
		}
	}

	// Register /debug.
	{
		runtime.SetBlockProfileRate(1)
		mu.Handle("/debug/pprof/", httpx.LoopbackAccessHandlerFunc(pprof.Index))
		mu.Handle("/debug/pprof/cmdline", httpx.LoopbackAccessHandlerFunc(pprof.Cmdline))
		mu.Handle("/debug/pprof/profile", httpx.LoopbackAccessHandlerFunc(pprof.Profile))
		mu.Handle("/debug/pprof/symbol", httpx.LoopbackAccessHandlerFunc(pprof.Symbol))
		mu.Handle("/debug/pprof/trace", httpx.LoopbackAccessHandlerFunc(pprof.Trace))
		mu.Handle("/debug/flags/v", httpx.LoopbackAccessHandlerFunc(routes.StringFlagPutHandler(logs.GlogSetter)))
	}

	// Register extension apis.
	mu.NotFoundHandler(extensionroutes.Index())

	// Start.
	gp := gox.GroupWithContextIn(ctx)
	gp.Go(func(ctx context.Context) error {
		// NB(thxCode): we start the manager after extension api is ready,
		// which allows the controller to index the extension api resources.
		klog.Info("waiting for extension API services to be ready")
		err := apis.WaitForServicesReady(ctx, system.LoopbackKubeClient.Get())
		if err != nil {
			return fmt.Errorf("wait for extension API services to be ready: %w", err)
		}

		// NB(thxCode): install some resource here,
		// don't wait for any resource, finish and go on.
		// ...

		// Setup controllers.
		klog.Info("starting controller manager")
		err = controllers.Setup(ctx, cm)
		if err != nil {
			return fmt.Errorf("setup controllers: %w", err)
		}
		return w.Manager.Start(ctx)
	})
	gp.Go(func(ctx context.Context) error {
		klog.Info("starting api server")
		return w.APIServer.PrepareRun().RunWithContext(ctx)
	})
	if w.PeerDp != nil {
		gp.Go(func(ctx context.Context) error {
			klog.Info("starting peer data plane")
			return w.PeerDp.Start(ctx)
		})
	}
	return gp.Wait()
}
