package system

import (
	"net"
	"net/http"
	"net/url"
	"slices"
	"strings"

	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/client-go/rest"
	ctrlcli "sigs.k8s.io/controller-runtime/pkg/client"

	"gpustack.ai/gpustack/pkg/kubeclients/kubernetes"
	"gpustack.ai/gpustack/pkg/utils/netx"
	"gpustack.ai/gpustack/pkg/utils/varx"
)

var (
	// LoopbackKubeInside is a flag that indicates whether the system runs inside the loopback Kubernetes cluster.
	LoopbackKubeInside varx.Once[bool]

	// LoopbackKubeNearby is a flag that indicates whether the system runs nearby the loopback Kubernetes cluster.
	// If the system runs nearby, it can connect to the loopback Kubernetes cluster even if it is not inside the cluster.
	LoopbackKubeNearby varx.Once[bool]

	// LoopbackKubeClientConfigPath is the path to the loopback Kubernetes client configuration file.
	LoopbackKubeClientConfigPath varx.Once[string]

	// LoopbackKubeRestConfig is the loopback Kubernetes rest configuration.
	LoopbackKubeRestConfig varx.Once[rest.Config]

	// LoopbackKubeHTTPClient is the loopback Kubernetes HTTP client.
	LoopbackKubeHTTPClient varx.Once[*http.Client]

	// LoopbackKubeClient is the loopback Kubernetes client.
	LoopbackKubeClient varx.Once[kubernetes.Interface]
)

// ConfigureLoopbackKube configures the loopback Kubernetes.
func ConfigureLoopbackKube(inside bool, configPath string, config rest.Config, httpClient *http.Client, client kubernetes.Interface) {
	LoopbackKubeInside.Configure(inside)
	LoopbackKubeNearby.Configure(inside || isLoopbackClusterNearby(&config))
	LoopbackKubeClientConfigPath.Configure(configPath)
	LoopbackKubeRestConfig.Configure(config)
	LoopbackKubeHTTPClient.Configure(httpClient)
	LoopbackKubeClient.Configure(client)
}

var (
	// LoopbackCtrlClient is the controller client for the loopback Kubernetes cluster.
	//
	// LoopbackCtrlClient is similar to LoopbackKubeClient,
	// but it has a self-manager cache for tuning read-only accessing,
	// which means we don't need to handle list/watch manually.
	LoopbackCtrlClient varx.Once[ctrlcli.Client]

	// LoopbackCtrlAPIReader is the controller API reader for the loopback Kubernetes cluster.
	//
	// LoopbackCtrlAPIReader is similar to LoopbackCtrlClient,
	// but it only has the read-only access to the API server.
	LoopbackCtrlAPIReader varx.Once[ctrlcli.Reader]
)

// ConfigureLoopbackCtrlRuntime configures the loopback Kubernetes controller runtime.
func ConfigureLoopbackCtrlRuntime(client ctrlcli.Client, apiReader ctrlcli.Reader) {
	LoopbackCtrlClient.Configure(client)
	LoopbackCtrlAPIReader.Configure(apiReader)
}

func isLoopbackClusterNearby(restCfg *rest.Config) bool {
	// Extract host from rest config.
	var host string
	if strings.Contains(restCfg.Host, "://") {
		u, _ := url.Parse(restCfg.Host)
		host = u.Host
	} else {
		host = restCfg.Host
	}
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	} else if strings.Contains(host, ":") {
		host = strings.Split(host, ":")[0]
	}

	// Detect host in a fast pass way.
	knownLoopbackHosts := []string{
		"kubernetes.docker.internal",
		"host.docker.internal",
		"localhost",
		"127.0.0.1",
		"[::1]",
		"[::1%lo0]",
	}
	if slices.Contains(knownLoopbackHosts, host) {
		return true
	}

	// Detect host in a slow pass way.
	subnets := make([]netx.IPv4, 0, Subnets.Get().Len())
	for _, v := range sets.List(Subnets.Get()) {
		sn := netx.MustIPv4FromCIDR(v)
		subnets = append(subnets, sn)
	}

	// IP detect.
	if ip := net.ParseIP(host); ip != nil {
		for j := range subnets {
			if subnets[j].Contains(ip) {
				return true
			}
		}

		return false
	}

	// Or DNS lookup.
	ips, err := net.LookupIP(host)
	if err != nil {
		return false
	}

	for i := range ips {
		if ips[i].IsLoopback() {
			return true
		}
		for j := range subnets {
			if subnets[j].Contains(ips[i]) {
				return true
			}
		}
	}

	return false
}
