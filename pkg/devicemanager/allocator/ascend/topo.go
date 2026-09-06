package ascend

import (
	productascend "gpustack.ai/gpustack/pkg/devicemanager/product/ascend"
)

// hcclTopoFilePathEnv names the file HCCL reads a node's fabric topology from. Only the A5
// generation carries it: the vendor's own device plugin returns early for every other card type,
// and the operator does the same, keyed on family950.
const hcclTopoFilePathEnv = "HCCL_TOPO_FILE_PATH"

// hcclTopoFilePaths is the vendor's product-to-topology-file table, as ascend-device-plugin ships
// it, keyed on the product shape rather than the vendor's bare number -- see productascend.Type.
//
// The directory holding these files is mounted by ascend-docker-runtime rather than by this
// allocator, so what is injected here is the env alone -- which is also why the file is never
// stat'ed: it exists in the container, not in the device-manager.
var hcclTopoFilePaths = map[productascend.Type]string{
	productascend.TypeServer8P:  "/usr/local/Ascend/driver/topo/950/atlas_850_1.json",
	productascend.TypePod1D:     "/usr/local/Ascend/driver/topo/950/atlas_950_1.json",
	productascend.TypePod2D:     "/usr/local/Ascend/driver/topo/950/atlas_950_2.json",
	productascend.TypeServer16P: "/usr/local/Ascend/driver/topo/950/atlas_850_2.json",
	productascend.TypeServer32P: "/usr/local/Ascend/driver/topo/950/atlas_850_3.json",
	productascend.TypeCard1P:    "/usr/local/Ascend/driver/topo/950/atlas_350_1.json",
	productascend.TypeCard4P:    "/usr/local/Ascend/driver/topo/950/atlas_350_3.json",
}
