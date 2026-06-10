package nodefeature

import (
	"sort"
	"strings"

	"golang.org/x/exp/maps"
	core "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/util/sets"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	"gpustack.ai/gpustack/pkg/utils/osx"
	"gpustack.ai/gpustack/pkg/utils/strconvx"
	"gpustack.ai/gpustack/pkg/utils/stringx"
)

const (
	ManufacturerAMD       = "amd"
	ManufacturerAscend    = "ascend"
	ManufacturerCambricon = "cambricon"
	ManufacturerHygon     = "hygon"
	ManufacturerIluvatar  = "iluvatar"
	ManufacturerMetaX     = "metax"
	ManufacturerMThreads  = "mthreads"
	ManufacturerNVIDIA    = "nvidia"
	ManufacturerTHead     = "thead"
)

const (
	// GeneralManufacturerGeneric is the fallback general(CPU) manufacturer used
	// when the NFD cpu-model labels are absent or unrecognizable.
	GeneralManufacturerGeneric = "generic"
)

const (
	SharedResourceNameSuffix = ".shared"
	SlicedResourceNameSuffix = ".sliced.units"
)

const (
	ResourceMaxUnits      = 10000
	SharedResourceMaxSize = 10
	SlicedResourceMaxSize = 16
)

var (
	_ManufacturerPciVendorIDMap               map[string]string
	_ManufacturerAcceleratableResourceNameMap map[string]core.ResourceName
	_ManufacturerAcceleratableRuntimeNameMap  map[string]string
	_AcceleratableResourceNameSet             sets.Set[core.ResourceName]
	_SlicedResourceSizes                      []int64
	_SlicedResourceOperatedSizesSet           sets.Set[string]
	_SlicedResourceMicroScaledBase            map[int64]int64
)

func init() {
	// Map manufacturer to PCI vendor ID.
	_ManufacturerPciVendorIDMap = map[string]string{
		ManufacturerAMD:       "1002",
		ManufacturerAscend:    "19e5",
		ManufacturerCambricon: "cabc",
		ManufacturerHygon:     "1d94",
		ManufacturerIluvatar:  "1e3e",
		ManufacturerMetaX:     "9999",
		ManufacturerMThreads:  "1ed5",
		ManufacturerNVIDIA:    "10de",
		ManufacturerTHead:     "1ded",
	}
	for _, manufacturer := range maps.Keys(_ManufacturerPciVendorIDMap) {
		// Extract PCI vendor ID from environment variable if exists,
		// and override the default value in the map.
		//
		// E.g. for AMD, the environment variable is "GPUSTACK_AMD_PCI_VENDOR_ID".
		//
		// Allow pattern like ${class}_${vendor} or ${vendor} only.
		if v := osx.Getenv("GPUSTACK_" + strings.ToUpper(manufacturer) + "_PCI_VENDOR_ID"); v != "" {
			_ManufacturerPciVendorIDMap[manufacturer] = v
		}
	}

	// Map manufacturer to resource name,
	// the resource name is usually used as the prefix of the accelerator resource name in Kubernetes.
	_ManufacturerAcceleratableResourceNameMap = map[string]core.ResourceName{
		ManufacturerAMD:       "amd.com/gpu",
		ManufacturerAscend:    "huawei.com/npu",
		ManufacturerCambricon: "cambricon.com/mlu",
		ManufacturerHygon:     "hygon.com/dcu",
		ManufacturerIluvatar:  "iluvatar.com/gpu",
		ManufacturerMetaX:     "metax-tech.com/gpu",
		ManufacturerMThreads:  "mthreads.com/gpu",
		ManufacturerNVIDIA:    "nvidia.com/gpu",
		ManufacturerTHead:     "alibabacloud.com/ppu",
	}
	for _, manufacturer := range maps.Keys(_ManufacturerAcceleratableResourceNameMap) {
		// Extract resource name from environment variable if exists,
		// and override the default value in the map.
		//
		// E.g. for AMD, the environment variable is "GPUSTACK_AMD_ACCELERATABLE_RESOURCE_NAME".
		if v := osx.Getenv("GPUSTACK_" + strings.ToUpper(manufacturer) + "_ACCELERATABLE_RESOURCE_NAME"); v != "" {
			_ManufacturerAcceleratableResourceNameMap[manufacturer] = core.ResourceName(v)
		}
	}

	// Map manufacturer to runtime name,
	// the runtime name is usually used as the container runtime class name for the accelerator resource.
	_ManufacturerAcceleratableRuntimeNameMap = map[string]string{
		ManufacturerAMD:       "amd",
		ManufacturerAscend:    "ascend",
		ManufacturerCambricon: "cambricon",
		ManufacturerHygon:     "hygon",
		ManufacturerIluvatar:  "iluvatar",
		ManufacturerMetaX:     "metax",
		ManufacturerMThreads:  "mthreads",
		ManufacturerNVIDIA:    "nvidia",
	}
	for _, manufacturer := range maps.Keys(_ManufacturerAcceleratableResourceNameMap) {
		// Extract runtime name from environment variable if exists,
		// and override the default value in the map.
		//
		// E.g. for AMD, the environment variable is "GPUSTACK_AMD_ACCELERATABLE_RUNTIME_NAME".
		if v := osx.Getenv("GPUSTACK_" + strings.ToUpper(manufacturer) + "_ACCELERATABLE_RUNTIME_NAME"); v != "" {
			_ManufacturerAcceleratableRuntimeNameMap[manufacturer] = v
		}
	}

	// Make a set of all known resource names for quick lookup.
	_AcceleratableResourceNameSet = sets.New[core.ResourceName](maps.Values(_ManufacturerAcceleratableResourceNameMap)...)

	// Define the available sizes for sliced resources.
	for val := int64(1); val <= SlicedResourceMaxSize; val <<= 1 {
		_SlicedResourceSizes = append(_SlicedResourceSizes, val)
	}

	// Make a set of available sizes in string format for quick lookup.
	_SlicedResourceOperatedSizesSet = sets.New[string]()
	for _, size := range _SlicedResourceSizes {
		if size < 2 {
			continue
		}
		_SlicedResourceOperatedSizesSet.Insert(strconvx.FormatInt(size, 10))
	}

	// Pre-calculate the micro-scaled quantity base for sliced resources.
	_SlicedResourceMicroScaledBase = make(map[int64]int64)
	for _, size := range _SlicedResourceSizes {
		_SlicedResourceMicroScaledBase[size] = 1e6 / size
	}
}

// GetPciVendorID returns the PCI vendor ID for the given manufacturer.
func GetPciVendorID(manufacturer string) string {
	return _ManufacturerPciVendorIDMap[manufacturer]
}

// NormalizeGeneralManufacturer normalizes an NFD-reported CPU vendor ID,
// which is report by https://github.com/klauspost/cpuid,
// it always to be a meaningful string if the CPU information is properly collected.
// Otherwise, falling back to GeneralManufacturerGeneric when received an empty string or VendorUnknown.
func NormalizeGeneralManufacturer(vendorID string) string {
	v := strings.ToLower(vendorID)
	if v == "vendorunknown" || v == "" {
		return GeneralManufacturerGeneric
	}
	return v
}

// GetKnownAcceleratableManufacturers returns the list of known accelerator manufacturers.
func GetKnownAcceleratableManufacturers() []string {
	manus := maps.Keys(_ManufacturerAcceleratableResourceNameMap)
	sort.Strings(manus)
	return manus
}

// IsKnownAcceleratableManufacturer reports whether the given manufacturer is a well-known accelerator manufacturer.
func IsKnownAcceleratableManufacturer(manufacturer string) bool {
	return _ManufacturerAcceleratableResourceNameMap[manufacturer] != ""
}

// GetAcceleratablePciVendorIDs returns the list of PCI vendor IDs for all known accelerator manufacturers.
func GetAcceleratablePciVendorIDs() []string {
	ids := make([]string, 0, len(_ManufacturerPciVendorIDMap))
	for manu := range _ManufacturerAcceleratableResourceNameMap {
		ids = append(ids, _ManufacturerPciVendorIDMap[manu])
	}
	sort.Strings(ids)
	return ids
}

// GetAcceleratableResourceName returns the accelerator resource name for the given manufacturer and allocation mode.
func GetAcceleratableResourceName(manufacturer string, mode workercore.DeviceAllocationMode) core.ResourceName {
	resName := _ManufacturerAcceleratableResourceNameMap[manufacturer]
	switch mode {
	default:
		return resName
	case workercore.DeviceAllocationModeShared:
		return resName + SharedResourceNameSuffix
	case workercore.DeviceAllocationModeSliced:
		return resName + SlicedResourceNameSuffix
	}
}

// IsKnownAcceleratableResourceName reports whether the given resource name is a well-known accelerator resource name.
func IsKnownAcceleratableResourceName(name core.ResourceName) bool {
	switch {
	case stringx.HasSuffix(name, SharedResourceNameSuffix):
		name = name[:len(name)-len(SharedResourceNameSuffix)]
	case stringx.HasSuffix(name, SlicedResourceNameSuffix):
		name = name[:len(name)-len(SlicedResourceNameSuffix)]
	}
	return _AcceleratableResourceNameSet.Has(name)
}

// GetAcceleratableRuntimeName returns the accelerator runtime name for the given manufacturer,
// usually, it's used as the container runtime class name for the accelerator resource.
func GetAcceleratableRuntimeName(manufacturer string) string {
	return _ManufacturerAcceleratableRuntimeNameMap[manufacturer]
}

// QuantityToSliceCount converts the given quantity to the count of slices for sliced resources based on the sliced size.
func QuantityToSliceCount(q resource.Quantity, sliced int64) resource.Quantity {
	if sliced <= 0 {
		return q
	}
	base, ok := _SlicedResourceMicroScaledBase[sliced]
	if !ok {
		return q
	}
	q.Set(q.ScaledValue(resource.Micro) / base)
	return q
}

// QuantityToAlignedValue converts the given quantity to the aligned value for sliced resources based on the sliced size.
func QuantityToAlignedValue(q resource.Quantity, sliced int64) resource.Quantity {
	if sliced <= 0 {
		return q
	}
	base, ok := _SlicedResourceMicroScaledBase[sliced]
	if !ok {
		return q
	}
	q.Mul(base / 100)
	return q
}

// QuantityToOriginalValue converts the given quantity of slices back to the original value for sliced resources based on the sliced size.
func QuantityToOriginalValue(q resource.Quantity, sliced int64) resource.Quantity {
	if sliced <= 0 {
		return q
	}
	base, ok := _SlicedResourceMicroScaledBase[sliced]
	if !ok {
		return q
	}
	q.SetScaled(q.ScaledValue(resource.Micro)/base/10, resource.Milli)
	return q
}
