package worker

import (
	"context"
	"errors"
	"fmt"
	"net"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/pflag"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/apiserver/pkg/admission/plugin/namespace/lifecycle"
	"k8s.io/apiserver/pkg/admission/plugin/policy/validating"
	"k8s.io/apiserver/pkg/apis/apiserver"
	genericapiserver "k8s.io/apiserver/pkg/server"
	genericoptions "k8s.io/apiserver/pkg/server/options"
	"k8s.io/client-go/informers"
	kclientset "k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	cliflag "k8s.io/component-base/cli/flag"
	"k8s.io/component-base/compatibility"
	klog "k8s.io/klog/v2"

	"gpustack.ai/gpustack/pkg/manager"
	"gpustack.ai/gpustack/pkg/nodefeature"
	"gpustack.ai/gpustack/pkg/system"
	certcache "gpustack.ai/gpustack/pkg/utils/certs/cache"
	"gpustack.ai/gpustack/pkg/utils/certs/kubecert"
	"gpustack.ai/gpustack/pkg/utils/osx"
	"gpustack.ai/gpustack/pkg/utils/version"
	"gpustack.ai/gpustack/pkg/worker/extensionapis"
	"gpustack.ai/gpustack/pkg/worker/kuberess"
)

type Options struct {
	// Establish.
	BindAddress net.IP
	BindPort    int
	CertDir     string

	// Manager.
	ManagerOptions *manager.Options

	// Control.
	DisableAuths        bool
	DisableApplications []string
	CorsAllowedOrigins  []string

	// Authentication.
	AuthnTokenWebhookCacheTTL time.Duration
	AuthnTokenRequestTimeout  time.Duration

	// Authorization.
	AuthzAllowCacheTTL time.Duration
	AuthzDenyCacheTTL  time.Duration

	// Audit.
	AuditPolicyFile        string
	AuditLogFile           string
	AuditWebhookConfigFile string

	// Device Manager.
	Manufacturers []string
}

func NewOptions() *Options {
	opts := &Options{
		// Establish.
		BindAddress: net.ParseIP("0.0.0.0"),
		BindPort:    31443,

		// Manager.
		ManagerOptions: manager.NewOptions(),

		// Control.
		DisableAuths:        false,
		DisableApplications: []string{},
		CorsAllowedOrigins:  []string{},

		// Authentication.
		AuthnTokenWebhookCacheTTL: 10 * time.Second,
		AuthnTokenRequestTimeout:  10 * time.Second,

		// Authorization.
		AuthzAllowCacheTTL: 10 * time.Second,
		AuthzDenyCacheTTL:  10 * time.Second,

		// Audit.
		AuditPolicyFile:        "",
		AuditLogFile:           "",
		AuditWebhookConfigFile: "",

		// Device Manager.
		Manufacturers: nodefeature.GetKnownAcceleratableManufacturers(),
	}
	opts.ManagerOptions.KubeLeaderElectionID = "worker.gpustack.ai"

	return opts
}

func (o *Options) AddFlags(fs *pflag.FlagSet) {
	// Establish.
	fs.IPVar(&o.BindAddress, "bind-address", o.BindAddress,
		"the IP address(without port) on which to serve.")
	fs.IntVar(&o.BindPort, "secure-port", o.BindPort,
		"the port on which to serve HTTPS.")
	fs.StringVar(&o.CertDir, "cert-dir", o.CertDir,
		"the directory where the TLS certs are located. "+
			"if provided, must place tls.crt and tls.key under --cert-dir.")

	// Manager.
	o.ManagerOptions.AddFlags(fs)

	// Control.
	fs.BoolVar(&o.DisableAuths, "disable-auths", o.DisableAuths,
		"disable checking authentication and authorization.")
	fs.StringSliceVar(&o.DisableApplications, "disable-applications", o.DisableApplications,
		"disable installing applications.")
	fs.StringSliceVar(&o.CorsAllowedOrigins, "cors-allowed-origins", o.CorsAllowedOrigins,
		"the list of origins a cross-domain request can be executed from, comma separated. "+
			"an allowed origin can be a regular expression to support subdomain matching. "+
			"empty means all origins are allowed. "+
			"ensure each expression matches the entire hostname by anchoring to the start with '^' or including the '//' prefix, "+
			"and by anchoring to the end with '$' or including the ':' port separator suffix. "+
			"examples of valid expressions are '//example.com(:|$)' and '^https://example.com(:|$)'.")

	// Authentication.
	fs.DurationVar(&o.AuthnTokenWebhookCacheTTL, "authentication-token-webhook-cache-ttl",
		o.AuthnTokenWebhookCacheTTL,
		"the duration to cache responses from the webhook token authenticator.")
	fs.DurationVar(&o.AuthnTokenRequestTimeout, "authentication-token-request-timeout",
		o.AuthnTokenRequestTimeout,
		"the duration to wait for a response from the webhook token authenticator.")

	// Authorization.
	fs.DurationVar(&o.AuthzAllowCacheTTL, "authorization-webhook-cache-authorized-ttl",
		o.AuthzAllowCacheTTL,
		"the duration to cache 'authorized' responses from the webhook authorizer.")
	fs.DurationVar(&o.AuthzDenyCacheTTL, "authorization-webhook-cache-unauthorized-ttl",
		o.AuthzDenyCacheTTL,
		"the duration to cache 'unauthorized' responses from the webhook authorizer.")

	// Audit.
	fs.StringVar(&o.AuditPolicyFile, "audit-policy-file", o.AuditPolicyFile,
		"path to the file that defines the audit policy configuration.")
	fs.StringVar(&o.AuditLogFile, "audit-log-path", o.AuditLogFile,
		"if set, all requests coming to the server will be logged to this file. "+
			"'-' means standard out.")
	fs.StringVar(&o.AuditWebhookConfigFile, "audit-webhook-config-file", o.AuditWebhookConfigFile,
		"path to a kubeconfig formatted file that defines the audit webhook configuration.")

	// Device Manager.
	fs.StringSliceVar(&o.Manufacturers, "manufacturer", o.Manufacturers,
		"comma separated list of manufacturers to detect.")
}

func (o *Options) Validate(ctx context.Context) error {
	// Establish.
	if o.BindPort < 1 || o.BindPort > 65535 {
		return errors.New("--secure-port: out of range")
	}
	if o.CertDir != "" {
		if !osx.ExistsDir(o.CertDir) {
			return errors.New("--cert-dir: no found directory")
		}
		if !osx.Exists(filepath.Join(o.CertDir, "tls.crt")) {
			return errors.New("--cert-dir: no found tls.crt")
		}
		if !osx.Exists(filepath.Join(o.CertDir, "tls.key")) {
			return errors.New("--cert-dir: no found tls.key")
		}
	}

	// Manager.
	if err := o.ManagerOptions.Validate(ctx); err != nil {
		return err
	}

	// Authentication and Authorization.
	if !o.DisableAuths {
		if o.AuthnTokenWebhookCacheTTL < 10*time.Second {
			return errors.New("--authentication-token-webhook-cache-ttl: less than 10s")
		}
		if o.AuthnTokenRequestTimeout < 10*time.Second {
			return errors.New("--authentication-token-request-timeout: less than 10s")
		}
		if o.AuthzAllowCacheTTL < 10*time.Second {
			return errors.New("--authorization-webhook-cache-authorized-ttl: less than 10s")
		}
		if o.AuthzDenyCacheTTL < 10*time.Second {
			return errors.New("--authorization-webhook-cache-unauthorized-ttl: less than 10s")
		}
	}

	// Audit.
	if o.AuditPolicyFile != "" && !osx.ExistsFile(o.AuditPolicyFile) {
		return errors.New("--audit-policy-file: no found file")
	}
	if o.AuditLogFile != "" && o.AuditLogFile != "-" && !osx.ExistsDir(filepath.Dir(o.AuditLogFile)) {
		return errors.New("--audit-log-path: no found parent directory")
	}
	if o.AuditWebhookConfigFile != "" && !osx.ExistsFile(o.AuditWebhookConfigFile) {
		return errors.New("--audit-webhook-config-file: no found file")
	}

	// Device Manager.
	if len(o.Manufacturers) != 0 {
		knownManufacturers := nodefeature.GetKnownAcceleratableManufacturers()
		if !sets.New[string](knownManufacturers...).HasAll(o.Manufacturers...) {
			return errors.New("--manufacturer: should be a comma separated list of valid manufacturers, " +
				"valid manufacturers: " + strings.Join(knownManufacturers, ","))
		}
	}

	return nil
}

func (o *Options) Complete(ctx context.Context) (*Config, error) {
	system.ConfigureControl(
		"",
		o.DisableAuths,
		o.DisableApplications,
	)

	mgrConfig, err := o.ManagerOptions.Complete(ctx)
	if err != nil {
		return nil, err
	}

	serve := &genericoptions.SecureServingOptions{
		BindAddress:                  o.BindAddress,
		BindPort:                     o.BindPort,
		CipherSuites:                 cliflag.PreferredTLSCipherNames(),
		MinTLSVersion:                "VersionTLS12",
		HTTP2MaxStreamsPerConnection: 1000,
	}
	if o.CertDir == "" {
		klog.Info("no cert dir provided, going to use Kubecert to serve")
		lpCli := mgrConfig.LoopbackKubeClient
		certCache, err := certcache.NewK8sCache(ctx,
			"worker", lpCli.CoreV1().Secrets(kuberess.SystemNamespaceName))
		if err != nil {
			return nil, fmt.Errorf("create cert cache: %w", err)
		}
		certMgr := &kubecert.StaticManager{
			CertCli: lpCli.CertificatesV1().CertificateSigningRequests(),
			Cache:   certCache,
			Host:    kuberess.SystemRoutingServiceName,
			AlternateIPs: func() []net.IP {
				if system.LoopbackKubeInside.Get() {
					return nil
				}
				return []net.IP{
					net.ParseIP("127.0.0.1"),
					net.ParseIP(system.PrimaryIP.Get()),
				}
			}(),
			AlternateDNSNames: []string{
				fmt.Sprintf("%s.%s.svc", kuberess.SystemRoutingServiceName, kuberess.SystemNamespaceName),
				fmt.Sprintf("%s.%s", kuberess.SystemRoutingServiceName, kuberess.SystemNamespaceName),
				kuberess.SystemRoutingServiceName,
				"localhost",
			},
		}
		serve.ServerCert.GeneratedCert = certMgr
	} else {
		serve.ServerCert.CertKey = genericoptions.CertKey{
			CertFile: filepath.Join(o.CertDir, "tls.crt"),
			KeyFile:  filepath.Join(o.CertDir, "tls.key"),
		}
		klog.InfoS("cert dir provided, going to serve", "path", o.CertDir)
	}

	var (
		authn *genericoptions.DelegatingAuthenticationOptions
		authz *genericoptions.DelegatingAuthorizationOptions
	)
	if !o.DisableAuths {
		authn = &genericoptions.DelegatingAuthenticationOptions{
			CacheTTL:             o.AuthnTokenWebhookCacheTTL,
			TokenRequestTimeout:  o.AuthnTokenRequestTimeout,
			WebhookRetryBackoff:  genericoptions.DefaultAuthWebhookRetryBackoff(),
			RemoteKubeConfigFile: mgrConfig.LoopbackKubeConfigPath,
			Anonymous: &apiserver.AnonymousAuthConfig{
				Enabled: false,
			},
		}
		authz = &genericoptions.DelegatingAuthorizationOptions{
			AllowCacheTTL:        o.AuthzAllowCacheTTL,
			DenyCacheTTL:         o.AuthzDenyCacheTTL,
			WebhookRetryBackoff:  genericoptions.DefaultAuthWebhookRetryBackoff(),
			RemoteKubeConfigFile: mgrConfig.LoopbackKubeConfigPath,
			ClientTimeout:        10 * time.Second,
			AlwaysAllowGroups:    []string{"system:masters"},
			AlwaysAllowPaths: []string{
				"/mutate-*", "/validate-*", // Webhooks
				"/livez", "/readyz", "/metrics", "/debug/*", // Measure
				"/openapi", "/openapi/*", // OpenAPI
				"/swagger", "/swagger/*", // Swagger
			},
		}
	}

	audit := genericoptions.NewAuditOptions()
	audit.PolicyFile = o.AuditPolicyFile
	audit.LogOptions.Path = o.AuditLogFile
	audit.WebhookOptions.ConfigFile = o.AuditWebhookConfigFile

	admit := genericoptions.NewAdmissionOptions()
	admit.DisablePlugins = []string{lifecycle.PluginName, validating.PluginName}

	// Recreate the loopback Kubernetes client for server as the runtime scheme is limited from the manager.
	lpRestCfg, lpHttpCli := rest.CopyConfig(&mgrConfig.LoopbackKubeRestConfig), mgrConfig.LoopbackKubeHTTPClient
	lpCli, err := kclientset.NewForConfigAndClient(rest.CopyConfig(lpRestCfg), lpHttpCli)
	if err != nil {
		return nil, fmt.Errorf("create kubernete native client: %w", err)
	}
	lpInf := informers.NewSharedInformerFactory(lpCli, o.ManagerOptions.InformerCacheResyncPeriod)

	apiSrvCfg := genericapiserver.NewRecommendedConfig(extensionapis.Codecs)
	{
		// Configure shared informer factory.
		apiSrvCfg.SharedInformerFactory = lpInf
		// Configure CORS allowed origins.
		apiSrvCfg.CorsAllowedOriginList = o.CorsAllowedOrigins
		// Feedback Kubernetes client configuration.
		apiSrvCfg.LoopbackClientConfig = lpRestCfg
		apiSrvCfg.ClientConfig = lpRestCfg
		// Disable default metrics service.
		apiSrvCfg.EnableMetrics = false
		// Disable default profiling service.
		apiSrvCfg.EnableProfiling = false
		// Disable default index service.
		apiSrvCfg.EnableIndex = false
		// Disable following post start hooks,
		// because the registered apiserver can manage them.
		apiSrvCfg.DisabledPostStartHooks.Insert(
			"priority-and-fairness-filter",
			"max-in-flight-filter",
			"storage-object-count-tracker-hook",
		)
		// Configure the effective version.
		apiSrvCfg.EffectiveVersion = compatibility.NewEffectiveVersionFromString(version.MajorMinor(), "", "")
		// Configure the shutdown watch termination grace period.
		apiSrvCfg.ShutdownWatchTerminationGracePeriod = 15 * time.Second
	}

	return &Config{
		Manufacturers:      o.Manufacturers,
		ManagerConfig:      mgrConfig,
		APIServerConfig:    apiSrvCfg,
		Serve:              serve,
		Authn:              authn,
		Authz:              authz,
		Audit:              audit,
		Admit:              admit,
		KubeNativeClient:   lpCli,
		KubeNativeInformer: lpInf,
	}, nil
}
