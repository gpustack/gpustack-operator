package worker

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	openapinamer "k8s.io/apiserver/pkg/endpoints/openapi"
	genericapiserver "k8s.io/apiserver/pkg/server"
	genericoptions "k8s.io/apiserver/pkg/server/options"
	"k8s.io/apiserver/pkg/util/feature"
	"k8s.io/client-go/dynamic"
	kinformers "k8s.io/client-go/informers"
	kclientset "k8s.io/client-go/kubernetes"
	klog "k8s.io/klog/v2"

	"gpustack.ai/gpustack/api"
	"gpustack.ai/gpustack/api/worker"
	"gpustack.ai/gpustack/pkg/extensionapi"
	"gpustack.ai/gpustack/pkg/manager"
	"gpustack.ai/gpustack/pkg/system"
	"gpustack.ai/gpustack/pkg/utils/osx"
	"gpustack.ai/gpustack/pkg/utils/version"
	"gpustack.ai/gpustack/pkg/utils/waitx"
	"gpustack.ai/gpustack/pkg/worker/extensionapis"
	"gpustack.ai/gpustack/pkg/worker/kuberess"
)

type Config struct {
	Manufacturers      []string
	ManagerConfig      *manager.Config
	APIServerConfig    *genericapiserver.RecommendedConfig
	Serve              *genericoptions.SecureServingOptions
	Authn              *genericoptions.DelegatingAuthenticationOptions
	Authz              *genericoptions.DelegatingAuthorizationOptions
	Audit              *genericoptions.AuditOptions
	Admit              *genericoptions.AdmissionOptions
	KubeNativeClient   kclientset.Interface
	KubeNativeInformer kinformers.SharedInformerFactory
}

func (c *Config) Apply(ctx context.Context) (*Worker, error) {
	mgr, err := c.ManagerConfig.Apply(ctx)
	if err != nil {
		return nil, err
	}

	apiSrvCfg := c.APIServerConfig

	// Apply the server configuration.
	err = c.Serve.ApplyTo(&apiSrvCfg.SecureServing)
	if err != nil {
		return nil, fmt.Errorf("apply server config: %w", err)
	}

	// Apply the authentication configuration.
	if c.Authn != nil {
		err = c.Authn.ApplyTo(&apiSrvCfg.Authentication, apiSrvCfg.SecureServing, nil)
		if err != nil {
			return nil, fmt.Errorf("apply authentication config: %w", err)
		}
	}

	// Apply the authorization configuration.
	if c.Authz != nil {
		err = c.Authz.ApplyTo(&apiSrvCfg.Authorization)
		if err != nil {
			return nil, fmt.Errorf("apply authorization config: %w", err)
		}
	}

	// Apply the audit configuration.
	err = c.Audit.ApplyTo(&apiSrvCfg.Config)
	if err != nil {
		return nil, fmt.Errorf("apply audit config: %w", err)
	}

	// Apply the admission configuration.
	{
		dcli, err := dynamic.NewForConfig(apiSrvCfg.ClientConfig)
		if err != nil {
			return nil, fmt.Errorf("create dynamic client: %w", err)
		}
		err = c.Admit.ApplyTo(
			&apiSrvCfg.Config,
			apiSrvCfg.SharedInformerFactory,
			c.KubeNativeClient,
			dcli,
			feature.DefaultFeatureGate,
			apiSrvCfg.EffectiveVersion,
		)
		if err != nil {
			return nil, fmt.Errorf("apply admission config: %w", err)
		}
	}

	// Apply OpenAPI configuration.
	var (
		title       = "GPUStack - Open Source Accelerator Management Platform"
		fullVersion = version.Get()
	)
	openapiDefinitionsGetter := extensionapi.MergeOpenAPIDefinitionsGetter(
		worker.GetOpenAPIDefinitions,
		api.GetOpenAPIDefinitions,
	)
	apiSrvCfg.OpenAPIConfig = genericapiserver.DefaultOpenAPIConfig(
		openapiDefinitionsGetter, openapinamer.NewDefinitionNamer(extensionapis.Scheme))
	apiSrvCfg.OpenAPIConfig.Info.Title = title
	apiSrvCfg.OpenAPIConfig.Info.Version = fullVersion
	apiSrvCfg.OpenAPIV3Config = genericapiserver.DefaultOpenAPIV3Config(
		openapiDefinitionsGetter, openapinamer.NewDefinitionNamer(extensionapis.Scheme))
	apiSrvCfg.OpenAPIV3Config.Info.Title = title
	apiSrvCfg.OpenAPIV3Config.Info.Version = fullVersion

	apiSrvCompletedCfg := apiSrvCfg.Complete()
	apiSrv, err := apiSrvCompletedCfg.New("gpustack", genericapiserver.NewEmptyDelegate())
	if err != nil {
		return nil, fmt.Errorf("create APIServer: %w", err)
	}

	lpCli := system.LoopbackKubeClient.Get()

	// Get routing port and CA bundle for webhook and extension API services.
	var (
		routingPort     = int32(c.Serve.BindPort)
		routingCaBundle []byte
	)
	if system.LoopbackKubeInside.Get() || !system.LoopbackKubeNearby.Get() {
		svc, err := lpCli.CoreV1().Services(kuberess.SystemNamespaceName).
			Get(ctx, kuberess.SystemRoutingServiceName,
				meta.GetOptions{
					ResourceVersion: "0",
				})
		if err == nil {
			routingPort = svc.Spec.Ports[0].Port
		}
	}
	if c.Serve.ServerCert.CertKey.KeyFile == "" {
		err = waitx.PollUntilContextTimeout(ctx, time.Second, 30*time.Second, true,
			func(ctx context.Context) error {
				for _, ns := range []string{
					kuberess.SystemNamespaceName,
					meta.NamespacePublic,
					meta.NamespaceDefault,
					meta.NamespaceSystem,
				} {
					cm, err := lpCli.CoreV1().ConfigMaps(ns).
						Get(ctx, "kube-root-ca.crt",
							meta.GetOptions{
								ResourceVersion: "0",
							})
					if err != nil {
						continue
					}
					if cm.Data["ca.crt"] == "" {
						continue
					}
					routingCaBundle = []byte(cm.Data["ca.crt"])
					break
				}
				if len(routingCaBundle) == 0 {
					return fmt.Errorf("cluster CA bundle not found")
				}
				return nil
			})
		if err != nil {
			return nil, fmt.Errorf("get kube-root-ca.crt: %w", err)
		}
	} else {
		caFile := filepath.Join(filepath.Dir(c.Serve.ServerCert.CertKey.KeyFile), "ca.crt")
		if osx.Exists(caFile) {
			caBs, err := os.ReadFile(caFile)
			if err != nil {
				return nil, fmt.Errorf("read CA file: %w", err)
			}
			routingCaBundle = caBs
		} else {
			klog.InfoS("CA bundle not found", "path", caFile)
		}
	}
	klog.Infof("routing port: %d, CA bundle:\n %s", routingPort, string(routingCaBundle))

	return &Worker{
		RoutingPort:     routingPort,
		RoutingCaBundle: routingCaBundle,
		Manufacturers:   c.Manufacturers,
		Manager:         mgr,
		APIServer:       apiSrv,
	}, nil
}
