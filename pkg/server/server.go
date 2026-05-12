package server

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
	"gpustack.ai/gpustack/pkg/server/apis"
	"gpustack.ai/gpustack/pkg/server/authz"
	"gpustack.ai/gpustack/pkg/server/controllers"
	"gpustack.ai/gpustack/pkg/server/extensionapis"
	"gpustack.ai/gpustack/pkg/server/extensionroutes"
	"gpustack.ai/gpustack/pkg/server/kuberess"
	"gpustack.ai/gpustack/pkg/server/settings"
	"gpustack.ai/gpustack/pkg/server/webhooks"
	"gpustack.ai/gpustack/pkg/system"
	"gpustack.ai/gpustack/pkg/utils/funcx"
	"gpustack.ai/gpustack/pkg/utils/gox"
	"gpustack.ai/gpustack/pkg/utils/httpx"
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

type Server struct {
	RoutingPort     int32
	RoutingCaBundle []byte
	Manager         *manager.Manager
	APIServer       *genericapiserver.GenericAPIServer
	PeerCp          *peer.ControlPlane
}

// Prepare prepares the runtime for the server,
// including installing system resources, etc.
func (s *Server) Prepare(ctx context.Context) error {
	err := s.Manager.Prepare(ctx)
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
		err = kuberess.InstallFakeSystemRoutingService(ctx, lpCli, s.RoutingPort)
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
			Port:      ptr.To(s.RoutingPort),
		}
		err = apis.InstallServices(ctx, lpCli, svcRef, s.RoutingCaBundle)
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
				Port:      ptr.To(s.RoutingPort),
			},
		}
		// If we stand closed to loopback Kubernetes cluster but not inside,
		// we can use the primary IP address to access the webhook server.
		if !system.LoopbackKubeInside.Get() && system.LoopbackKubeNearby.Get() {
			// NB(thxCode): launch multiple instances, only one takes working.
			ep := fmt.Sprintf("https://%s:%d", system.PrimaryIP.Get(), s.RoutingPort)
			cc = admreg.WebhookClientConfig{
				URL: ptr.To(ep),
			}
		}
		// Install webhook configurations.
		cc.CABundle = s.RoutingCaBundle
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
	err = kuberess.InstallApplications(ctx)
	if err != nil {
		return fmt.Errorf("install application: %w", err)
	}

	// Install authorization.
	err = authz.Initialize(ctx, lpCli)
	if err != nil {
		return fmt.Errorf("install authorization: %w", err)
	}

	return nil
}

func (s *Server) Start(ctx context.Context) error {
	cm := s.Manager.CtrlManager
	mu := s.APIServer.Handler.NonGoRestfulMux

	// Setup extension API handlers.
	err := extensionapis.Setup(ctx, s.APIServer, cm)
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
		err = s.APIServer.AddLivezChecks(10*time.Second,
			healthz.NamedCheck("gopool", func(r *http.Request) error {
				return gox.IsHealthy()
			}),
			healthz.NamedCheck("loopback", func(r *http.Request) error {
				return kubediscovery.IsConnectedWithRestConfig(r.Context(), dynamic.ConfigFor(cm.GetConfig()))
			}),
			healthz.NamedCheck("manager", func(r *http.Request) error {
				return s.Manager.WaitForReady(r.Context())
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

	// Register extension routes.
	mu.NotFoundHandler(extensionroutes.Index(s.PeerCp))

	// Start.
	gp := gox.GroupWithContextIn(ctx)
	gp.Go(func(ctx context.Context) error {
		lpCli := system.LoopbackKubeClient.Get()

		// NB(thxCode): we start the manager after extension api is ready,
		// which allows the controller to index the extension api resources.
		klog.Info("waiting for extension API services to be ready")
		err := apis.WaitForServicesReady(ctx, lpCli)
		if err != nil {
			return fmt.Errorf("wait for extension API services to be ready: %w", err)
		}

		// NB(thxCode): install some resource here,
		// don't wait for any resource, finish and go on.

		// Initialize default subject provider.
		err = kuberess.InstallDefaultSubjectProvider(ctx, lpCli)
		if err != nil {
			return err
		}
		// Initialize default subject.
		err = kuberess.InstallAdminSubject(ctx, lpCli, system.BootstrapPassword.Get())
		if err != nil {
			return err
		}
		// Initialize default team.
		err = kuberess.InstallDefaultTeam(ctx, lpCli)
		if err != nil {
			return err
		}
		// Initialize default project.
		err = kuberess.InstallDefaultProject(ctx, lpCli)
		if err != nil {
			return err
		}
		// Import local cluster if needed.
		err = kuberess.ImportLocalCluster(ctx, lpCli)
		if err != nil {
			return err
		}
		// Configure serve URL setting.
		if funcx.NoError(settings.ServeUrl.Value(ctx)) == "" {
			err = settings.ServeUrl.Configure(ctx, fmt.Sprintf("https://%s:%d", system.PrimaryIP.Get(), s.RoutingPort))
			if err != nil {
				return err
			}
		}

		// Setup controllers.
		klog.Info("starting controller manager")
		err = controllers.Setup(ctx, cm)
		if err != nil {
			return fmt.Errorf("setup controllers: %w", err)
		}
		return s.Manager.Start(ctx)
	})
	gp.Go(func(ctx context.Context) error {
		klog.Info("starting api server")
		return s.APIServer.PrepareRun().RunWithContext(ctx)
	})
	gp.Go(func(ctx context.Context) error {
		klog.Info("starting peer control plane")
		return s.PeerCp.Start(ctx)
	})
	return gp.Wait()
}
