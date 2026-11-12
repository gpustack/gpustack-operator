package worker

import (
	"fmt"
	"strings"

	core "k8s.io/api/core/v1"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	"gpustack.ai/gpustack/pkg/nodefeature"
	"gpustack.ai/gpustack/pkg/worker/kvcache/inject"
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

// ModelDeploymentDirectPairRequestsInterfaces reports whether a routed prefill/decode pair
// requests positive fabric interfaces on every Prefill and Decode role (ignoring Encode and Server).
func ModelDeploymentDirectPairRequestsInterfaces(md *workercore.ModelDeployment) bool {
	if md == nil || md.Spec.Router == nil || !ModelDeploymentDeclaresBothHalves(md) {
		return false
	}
	for i := range md.Spec.Roles {
		role := &md.Spec.Roles[i]
		kind := ModelDeploymentEffectiveRoleKind(role)
		if kind != workercore.ModelDeploymentRoleKindPrefill &&
			kind != workercore.ModelDeploymentRoleKindDecode {
			continue
		}
		if role.Resources == nil || role.Resources.Interface == nil || role.Resources.Interface.Sign() <= 0 {
			return false
		}
	}
	return true
}

// ModelDeploymentDirectInterfaceProtocol returns the protocol a managed direct-transfer or
// multi-member Server collective leg actually renders. It covers NIXL remote weight delivery,
// managed multi-Pod vLLM Server roles, and routed prefill/decode pairs on vLLM and SGLang when
// every Prefill and Decode half requests interfaces on a non-Ascend pool.
func ModelDeploymentDirectInterfaceProtocol(
	md *workercore.ModelDeployment, role *workercore.ModelDeploymentRole, manufacturer string,
	remoteDelivery, awsEFAEnabled bool,
) string {
	if md == nil || role == nil || len(role.Command) > 0 ||
		manufacturer == "" || manufacturer == nodefeature.ManufacturerAscend {
		return ""
	}
	directProtocol := "rdma"
	if awsEFAEnabled {
		directProtocol = "efa"
	}
	if remoteDelivery &&
		!modelDeploymentRoleHasConflictingLoadFormat(md.Spec.Engine.Name, role.ExtraArgs) &&
		modelDeploymentModelTransferCandidateBackend(manufacturer, md.Spec.Engine.Name) == "nixl" {
		return directProtocol
	}
	if md.Spec.Engine.Name != workercore.ModelDeploymentEngineVLLM &&
		md.Spec.Engine.Name != workercore.ModelDeploymentEngineSGLang {
		return ""
	}
	// Managed multi-Pod Server collective legs (size > 1 or elasticEp) select the direct
	// fabric device key; SGLang multi-member Server roles are not a leg this answer grants
	// devices for, so they fall through to the pair check and stop there.
	if md.Spec.Engine.Name == workercore.ModelDeploymentEngineVLLM &&
		ModelDeploymentEffectiveRoleKind(role) == workercore.ModelDeploymentRoleKindServer &&
		(modelDeploymentRoleSize(role) > 1 || role.ElasticEP != nil) {
		return directProtocol
	}
	if !modelDeploymentUsesKVTransfer(md, role) || !ModelDeploymentDirectPairRequestsInterfaces(md) {
		return ""
	}
	if ModelDeploymentEffectiveKVTransferBackend(md, manufacturer) == inject.KVTransferBackendHIXL {
		return ""
	}
	return directProtocol
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
