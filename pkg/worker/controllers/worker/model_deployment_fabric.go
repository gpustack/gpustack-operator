package worker

import (
	"fmt"
	"strings"

	core "k8s.io/api/core/v1"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	"gpustack.ai/gpustack/pkg/nodefeature"
	"gpustack.ai/gpustack/pkg/worker/kvcache/mooncake"
)

func modelDeploymentRequestsInterfaces(md *workercore.ModelDeployment) bool {
	for i := range md.Spec.Roles {
		resources := md.Spec.Roles[i].Resources
		if resources != nil && resources.Interface != nil && resources.Interface.Sign() > 0 {
			return true
		}
	}
	return false
}

// ModelDeploymentDirectInterfaceProtocol returns the protocol a managed direct-transfer leg
// actually renders. A role with a user-owned command, an absent pair, or an engine that ignores
// the declared protocol cannot use that declaration to select a device grant.
func ModelDeploymentDirectInterfaceProtocol(
	md *workercore.ModelDeployment, role *workercore.ModelDeploymentRole, manufacturer string,
) string {
	if len(role.Command) > 0 || !modelDeploymentUsesKVTransfer(md, role, manufacturer) ||
		md.Spec.Engine.Name != workercore.ModelDeploymentEngineVLLM ||
		manufacturer == "" || manufacturer == nodefeature.ManufacturerAscend {
		return ""
	}
	if md.Spec.KVTransfer != nil && md.Spec.KVTransfer.Protocol != "" {
		return md.Spec.KVTransfer.Protocol
	}
	return "tcp"
}

// ModelDeploymentInterfaceResource selects one device-plugin key for a positive role interface
// count. Member groups must agree even when one uses TCP, because a single count cannot describe
// allocations for a backend whose groups use different protocols.
func ModelDeploymentInterfaceResource(
	storeProtocols []string, directProtocol string, count int64,
) (core.ResourceName, error) {
	if count < 1 {
		return "", fmt.Errorf("interface count must be positive")
	}
	storeProtocol := ""
	if len(storeProtocols) > 0 {
		storeProtocol = storeProtocols[0]
		for _, protocol := range storeProtocols[1:] {
			if protocol != storeProtocol {
				return "", fmt.Errorf("cache member groups use conflicting protocols: %s",
					strings.Join(storeProtocols, ", "))
			}
		}
	}

	fabric := ""
	for _, protocol := range []string{storeProtocol, directProtocol} {
		if protocol != "rdma" && protocol != "efa" {
			continue
		}
		if fabric != "" && fabric != protocol {
			return "", fmt.Errorf("cache and direct-transfer legs use %s and %s", fabric, protocol)
		}
		fabric = protocol
	}
	if fabric == "" {
		return "", fmt.Errorf("no effective RDMA or EFA transfer leg uses the requested interfaces")
	}

	width := int32(1)
	if count > 1 {
		width = 2
	}
	return mooncake.FabricDeviceResource(fabric, width), nil
}
