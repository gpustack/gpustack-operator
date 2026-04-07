package system

import (
	"fmt"
	"net"

	knet "k8s.io/apimachinery/pkg/util/net"
	"k8s.io/apimachinery/pkg/util/sets"

	"gpustack.ai/gpustack/pkg/utils/osx"
	"gpustack.ai/gpustack/pkg/utils/varx"
)

var (
	// IPs is a set of non-loopback IP address of the system.
	IPs = varx.NewOnce(sets.New[string]())

	// Subnets is a set of subnet of the system.
	Subnets = varx.NewOnce(sets.New[string]())

	// PrimaryIP is the primary (outbound) IP address of the system.
	//
	// Prefer to get the assigned IP address from environment variable "KUBERNETES_POD_IP" if exists,
	// which is injected by downward API in Kubernetes Pod.
	PrimaryIP = varx.Once[string]{}

	// PrimarySubnet is the primary subnet of the system.
	PrimarySubnet = varx.Once[string]{}
)

// ConfigureNetwork configures the network of the system.
func ConfigureNetwork() error {
	var (
		hostIP, _     = knet.ChooseHostInterface()
		ifaces, _     = net.Interfaces()
		ips           = sets.New[string]()
		subnets       = sets.New[string]()
		primaryIP     string
		primarySubnet string
	)

	var assignedHostIP net.IP
	if v := osx.Getenv("KUBERNETES_POD_IP", ""); v != "" {
		assignedHostIP = net.ParseIP(v)
	}

	for _, _if := range ifaces {
		if _if.Flags&net.FlagUp == 0 || _if.Flags&net.FlagLoopback != 0 {
			// Skip down or loopback interfaces.
			continue
		}

		addrs, err := _if.Addrs()
		if err != nil {
			return fmt.Errorf("query addresses from interface %s(%d): %w",
				_if.Name, _if.Index, err)
		}

		for _, addr := range addrs {
			if ipnet, ok := addr.(*net.IPNet); ok && !ipnet.IP.IsLoopback() {
				// NB(thxCode): There is no NAT for IPv6,
				// so we only need to consider IPv4.
				v4 := ipnet.IP.To4()
				if v4 == nil || ips.Has(v4.String()) {
					continue
				}
				if v4[3] == 0x01 {
					continue
				}
				if assignedHostIP != nil && !assignedHostIP.Equal(v4) {
					continue
				}

				ips.Insert(v4.String())
				subnets.Insert(ipnet.String())
				if hostIP != nil && hostIP.Equal(v4) {
					primaryIP = v4.String()
					primarySubnet = ipnet.String()
				}
			}
		}
	}

	if primaryIP == "" {
		if assignedHostIP == nil {
			return fmt.Errorf("primary IP is not found")
		}
		primaryIP = assignedHostIP.String()
	}

	IPs.Configure(ips)
	Subnets.Configure(subnets)
	PrimaryIP.Configure(primaryIP)
	PrimarySubnet.Configure(primarySubnet)

	return nil
}
