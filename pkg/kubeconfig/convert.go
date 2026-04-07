package kubeconfig

import (
	"errors"
	"fmt"
	"os"

	kmeta "k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/cli-runtime/pkg/genericclioptions"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/discovery/cached/memory"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/restmapper"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/tools/clientcmd/api"
)

// ConvertRestConfigToApiConfig converts a rest config to an api config,
// all data locate with a file path will be load into the api config.
func ConvertRestConfigToApiConfig(cfg *rest.Config) (api.Config, error) {
	if cfg == nil {
		return api.Config{}, errors.New("nil rest config")
	}

	const context = "default"

	cc := *api.NewConfig()

	// Convert context info.
	{
		info := api.NewContext()

		info.Cluster = context
		info.AuthInfo = context

		// TODO: Namespace, Extensions.

		cc.Contexts[context] = info
	}

	cc.CurrentContext = context

	// Convert cluster info.
	{
		info := api.NewCluster()

		info.Server = cfg.Host
		info.TLSServerName = cfg.ServerName
		info.InsecureSkipTLSVerify = cfg.Insecure
		info.DisableCompression = cfg.DisableCompression

		info.CertificateAuthorityData = cfg.CAData
		if cfg.CAFile != "" {
			// Load the CA data from the file.
			d, err := os.ReadFile(cfg.CAFile)
			if err != nil {
				return api.Config{}, fmt.Errorf("read CA file: %w", err)
			}
			info.CertificateAuthorityData = d
		}

		if cfg.Proxy != nil {
			// Get the proxy URL with a nil request.
			u, err := cfg.Proxy(nil)
			if err == nil {
				info.ProxyURL = u.String()
			}
		}

		// TODO: Extensions.

		cc.Clusters[context] = info
	}

	// Convert auth info.
	{
		info := api.NewAuthInfo()

		info.ClientCertificateData = cfg.CertData
		if cfg.CertFile != "" {
			// Load the certificate data from the file.
			d, err := os.ReadFile(cfg.CertFile)
			if err != nil {
				return api.Config{}, fmt.Errorf("read certificate file: %w", err)
			}
			info.ClientCertificateData = d
		}

		info.ClientKeyData = cfg.KeyData
		if cfg.KeyFile != "" {
			// Load the key data from the file.
			d, err := os.ReadFile(cfg.KeyFile)
			if err != nil {
				return api.Config{}, fmt.Errorf("read key file: %w", err)
			}
			info.ClientKeyData = d
		}

		info.Token = cfg.BearerToken
		if cfg.BearerTokenFile != "" {
			// Load the token from the file.
			d, err := os.ReadFile(cfg.BearerTokenFile)
			if err != nil {
				return api.Config{}, fmt.Errorf("read bearer token file: %w", err)
			}
			info.Token = string(d)
		}

		info.Impersonate = cfg.Impersonate.UserName
		info.ImpersonateGroups = cfg.Impersonate.Groups
		info.ImpersonateUserExtra = cfg.Impersonate.Extra

		info.Username = cfg.Username
		info.Password = cfg.Password

		info.AuthProvider = cfg.AuthProvider
		info.Exec = cfg.ExecProvider

		// TODO: Extensions.

		cc.AuthInfos[context] = info
	}

	return cc, nil
}

// ConvertRestConfigToApiConfigString converts a rest config to an api config string,
// the returned api config string is in YAML format.
func ConvertRestConfigToApiConfigString(cfg *rest.Config) (string, error) {
	cc, err := ConvertRestConfigToApiConfig(cfg)
	if err != nil {
		return "", fmt.Errorf("convert rest config to api config: %w", err)
	}

	data, err := clientcmd.Write(cc)
	if err != nil {
		return "", fmt.Errorf("write api config to bytes: %w", err)
	}

	return string(data), nil
}

// ConvertRestConfigToRestClientGetter converts a rest config to a RESTClientGetter,
func ConvertRestConfigToRestClientGetter(cfg *rest.Config) genericclioptions.RESTClientGetter {
	rcg := restClientGetter(*cfg)
	return &rcg
}

// ConvertApiConfigToRestConfig converts an api config to a rest config,
// the returned rest config will not contain any file path, all data will be loaded into memory.
func ConvertApiConfigToRestConfig(cc api.Config) (*rest.Config, error) {
	cfg, err := clientcmd.NewNonInteractiveClientConfig(cc, cc.CurrentContext, &clientcmd.ConfigOverrides{}, nil).ClientConfig()
	if err != nil {
		return nil, fmt.Errorf("convert api config to rest config: %w", err)
	}

	return cfg, nil
}

// restClientGetter is a RESTClientGetter interface implementation for the
// Helm Go packages.
type restClientGetter rest.Config

func (g restClientGetter) ToRESTConfig() (*rest.Config, error) {
	r := rest.Config(g)
	return &r, nil
}

func (g restClientGetter) ToDiscoveryClient() (discovery.CachedDiscoveryInterface, error) {
	config, err := g.ToRESTConfig()
	if err != nil {
		return nil, err
	}
	return memory.NewMemCacheClient(discovery.NewDiscoveryClientForConfigOrDie(config)), nil
}

func (g restClientGetter) ToRESTMapper() (kmeta.RESTMapper, error) {
	discoveryClient, err := g.ToDiscoveryClient()
	if err != nil {
		return nil, err
	}

	mapper := restmapper.NewDeferredDiscoveryRESTMapper(discoveryClient)
	expander := restmapper.NewShortcutExpander(mapper, discoveryClient, nil)

	return expander, nil
}

func (g restClientGetter) ToRawKubeConfigLoader() clientcmd.ClientConfig {
	// Build our config and client.
	return clientcmd.NewNonInteractiveDeferredLoadingClientConfig(
		clientcmd.NewDefaultClientConfigLoadingRules(),
		&clientcmd.ConfigOverrides{},
	)
}
