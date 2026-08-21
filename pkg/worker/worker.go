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
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
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
}

// routingServiceReference is the system routing service the extension api services are fronted by.
func (w *Worker) routingServiceReference() apireg.ServiceReference {
	return apireg.ServiceReference{
		Namespace: kuberess.SystemNamespaceName,
		Name:      kuberess.SystemRoutingServiceName,
		Port:      ptr.To(w.RoutingPort),
	}
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
	err = apis.InstallServices(ctx, lpCli, w.routingServiceReference(), w.RoutingCaBundle)
	if err != nil {
		return fmt.Errorf("install extension API services: %w", err)
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

	// Install the two custom resources the scheduling chain needs but no chart can carry:
	// their CRDs belong to Node Feature Discovery and to Kueue, and Helm maps a whole
	// manifest before creating anything, so a chart cannot order a CRD ahead of a custom
	// resource that needs it. Both stay outside the --disable-applications gate — whoever
	// deploys NFD and Kueue, the chain does not start without them — and both retry until
	// their CRD is served.
	err = kuberess.InstallCPUInfoNodeFeatureRule(ctx, w.Manufacturers)
	if err != nil {
		return fmt.Errorf("install cpu info node feature rule: %w", err)
	}

	err = kuberess.InstallNodeDevicesAdmissionCheck(ctx)
	if err != nil {
		return fmt.Errorf("install node devices admission check: %w", err)
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
	gp.Go(func(ctx context.Context) error {
		// The installation in Prepare cannot outlive the boot, so the definitions are kept
		// here instead: a definition deleted at runtime leaves every controller watching it
		// failing forever, and only a restart brings it back.
		klog.Info("starting custom resource definitions ensurer")
		// The ensurer returns only once the context is done, and what it returns then can be a
		// failure it recorded on the way rather than the cancellation, so the context decides
		// whether this is a shutdown or something to fail on.
		err := apis.EnsureCRDs(ctx, system.LoopbackKubeClient.Get())
		if err != nil && ctx.Err() == nil {
			return fmt.Errorf("ensure CRDs: %w", err)
		}
		return nil
	})
	gp.Go(func(ctx context.Context) error {
		klog.Info("starting extension API services ensurer")
		err := apis.EnsureServices(ctx, system.LoopbackKubeClient.Get(),
			w.routingServiceReference(), w.RoutingCaBundle)
		w.deregisterOnTeardown()
		if err != nil && ctx.Err() == nil {
			return fmt.Errorf("ensure extension API services: %w", err)
		}
		return nil
	})
	return gp.Wait()
}

// deregisterOnTeardown removes the cluster-scoped objects of this install — every API
// service backed by the system namespace and the webhook configurations this worker
// registered — when the shutdown comes from the system namespace going away. Both would
// otherwise outlive their namespaced routing service: the stale API services wedge the
// namespace's deletion on discovery it can no longer serve, and the stale webhook
// configurations fail admissions against a service that is gone (gpustack-operator#123).
// The API services are matched by backing namespace rather than by the worker's own list
// because a chart-installed one — Kueue's visibility pair — wedges the deletion the same
// way, and a direct namespace deletion runs no chart uninstall to take it down. An ordinary
// restart or upgrade leaves the namespace standing, so nothing is removed then — a surviving
// replica's ensurer or the next boot keeps them served. Best effort and bounded: this must
// never hold a shutdown up.
func (w *Worker) deregisterOnTeardown() {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	lpCli := system.LoopbackKubeClient.Get()
	ns, err := lpCli.CoreV1().Namespaces().Get(ctx, kuberess.SystemNamespaceName, meta.GetOptions{})
	switch {
	case err == nil && ns.DeletionTimestamp == nil:
		// The namespace stands: a restart or an upgrade, not a teardown.
		return
	case err != nil && !kerrors.IsNotFound(err):
		// A read failure cannot tell a teardown from a restart — leave the registrations
		// rather than strip them off a living install.
		klog.InfoS("leaving the extension API services and webhook configurations registered: cannot read the system namespace", "err", err)
		return
	}

	if err = apis.DeleteServicesBackedBy(ctx, lpCli, kuberess.SystemNamespaceName); err != nil {
		klog.InfoS("failed to delete the aggregated API services on teardown", "err", err)
	}
	if err = webhooks.Delete(ctx, lpCli); err != nil {
		klog.InfoS("failed to delete webhook configurations on teardown", "err", err)
	}
}
