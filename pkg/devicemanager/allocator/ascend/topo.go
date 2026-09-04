package ascend

import (
	"gpustack.ai/gpustack/pkg/devicemanager/ascendproduct"
)

// hcclTopoFilePathEnv names the file HCCL reads a node's fabric topology from. Only the A5
// generation carries it: the vendor's own device plugin returns early for every other card type,
// and the operator does the same, keyed on family950.
const hcclTopoFilePathEnv = "HCCL_TOPO_FILE_PATH"

// hcclTopoFilePaths is the vendor's product-to-topology-file table, as ascend-device-plugin ships
// it, keyed on the product shape rather than the vendor's bare number -- see ascendproduct.Type.
//
// The directory holding these files is mounted by ascend-docker-runtime rather than by this
// allocator, so what is injected here is the env alone -- which is also why the file is never
// stat'ed: it exists in the container, not in the device-manager.
var hcclTopoFilePaths = map[ascendproduct.Type]string{
	ascendproduct.TypeServer8P:  "/usr/local/Ascend/driver/topo/950/atlas_850_1.json",
	ascendproduct.TypePod1D:     "/usr/local/Ascend/driver/topo/950/atlas_950_1.json",
	ascendproduct.TypePod2D:     "/usr/local/Ascend/driver/topo/950/atlas_950_2.json",
	ascendproduct.TypeServer16P: "/usr/local/Ascend/driver/topo/950/atlas_850_2.json",
	ascendproduct.TypeServer32P: "/usr/local/Ascend/driver/topo/950/atlas_850_3.json",
	ascendproduct.TypeCard1P:    "/usr/local/Ascend/driver/topo/950/atlas_350_1.json",
	ascendproduct.TypeCard4P:    "/usr/local/Ascend/driver/topo/950/atlas_350_3.json",
}
