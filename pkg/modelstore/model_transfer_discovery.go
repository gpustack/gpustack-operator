package modelstore

// ModelTransferClientConfigAttribute names the public source discovery ConfigMap
// in the consuming Pod's namespace. The CSI driver authenticates as that Pod.
const ModelTransferClientConfigAttribute = "model-transfer.gpustack.ai/client-config"

// ModelTransferEndpoint is public source discovery, without native credentials.
type ModelTransferEndpoint struct {
	Manufacturer string `json:"manufacturer"`
	Address      string `json:"address"`
	ServerName   string `json:"serverName"`
	CAFile       string `json:"caFile"`
	PodUID       string `json:"podUID"`
	PodName      string `json:"podName"`
	PodNamespace string `json:"podNamespace"`
	NodeName     string `json:"nodeName"`
}

// ModelTransferDiscovery is the public configuration consumed by engines and CSI.
type ModelTransferDiscovery struct {
	Version   int                     `json:"version"`
	Endpoints []ModelTransferEndpoint `json:"endpoints"`
}
