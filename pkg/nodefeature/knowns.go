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
	SharedResourceNameSuffix = ".shared"
	SlicedResourceNameSuffix = ".sliced.units"
)

const (
	ResourceMaxUnits      = 10000
	SharedResourceMaxSize = 10
	SlicedResourceMaxSize = 16
)

var (
	_ManufacturerRuntimeNameMap     map[string]string
	_ManufacturerResourceNameMap    map[string]core.ResourceName
	_ManufacturerPciIDMap           map[string]string
	_ResourceNameSet                sets.Set[core.ResourceName]
	_SlicedResourceSizes            []int64
	_SlicedResourceOperatedSizesSet sets.Set[string]
	_SlicedResourceMicroScaledBase  map[int64]int64
)

func init() {
	// Map manufacturer to runtime name.
	_ManufacturerRuntimeNameMap = map[string]string{
		ManufacturerAMD:       "amd",
		ManufacturerAscend:    "ascend",
		ManufacturerCambricon: "cambricon",
		ManufacturerHygon:     "hygon",
		ManufacturerIluvatar:  "iluvatar",
		ManufacturerMetaX:     "metax",
		ManufacturerMThreads:  "mthreads",
		ManufacturerNVIDIA:    "nvidia",
	}
	for _, manufacturer := range maps.Keys(_ManufacturerResourceNameMap) {
		// Extract runtime name from environment variable if exists,
		// and override the default value in the map.
		//
		// E.g. for AMD, the environment variable is "GPUSTACK_AMD_RUNTIME_NAME".
		if v := osx.Getenv("GPUSTACK_" + strings.ToUpper(manufacturer) + "_RUNTIME_NAME"); v != "" {
			_ManufacturerRuntimeNameMap[manufacturer] = v
		}
	}

	// Map manufacturer to resource name.
	_ManufacturerResourceNameMap = map[string]core.ResourceName{
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
	for _, manufacturer := range maps.Keys(_ManufacturerResourceNameMap) {
		// Extract resource name from environment variable if exists,
		// and override the default value in the map.
		//
		// E.g. for AMD, the environment variable is "GPUSTACK_AMD_RESOURCE_NAME".
		if v := osx.Getenv("GPUSTACK_" + strings.ToUpper(manufacturer) + "_RESOURCE_NAME"); v != "" {
			_ManufacturerResourceNameMap[manufacturer] = core.ResourceName(v)
		}
	}

	// Map manufacturer to PCI vendor ID.
	_ManufacturerPciIDMap = map[string]string{
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
	for _, manufacturer := range maps.Keys(_ManufacturerPciIDMap) {
		// Extract PCI vendor ID from environment variable if exists,
		// and override the default value in the map.
		//
		// E.g. for AMD, the environment variable is "GPUSTACK_AMD_PCI_VENDOR".
		//
		// Allow pattern like ${class}_${vendor} or ${vendor} only.
		if v := osx.Getenv("GPUSTACK_" + strings.ToUpper(manufacturer) + "_PCI_ID"); v != "" {
			_ManufacturerPciIDMap[manufacturer] = v
		}
	}

	// Make a set of all known resource names for quick lookup.
	_ResourceNameSet = sets.New[core.ResourceName](maps.Values(_ManufacturerResourceNameMap)...)

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

// GetKnownManufacturers returns the list of known manufacturers.
func GetKnownManufacturers() []string {
	manus := maps.Keys(_ManufacturerResourceNameMap)
	sort.Strings(manus)
	return manus
}

// IsKnownManufacturer checks if the given manufacturer is a well-known manufacturer.
func IsKnownManufacturer(manufacturer string) bool {
	return _ManufacturerResourceNameMap[manufacturer] != ""
}

// GetRuntimeName returns the runtime name for the given manufacturer.
func GetRuntimeName(manufacturer string) string {
	return _ManufacturerRuntimeNameMap[manufacturer]
}

// GetResourceName returns the resource name for the given manufacturer.
func GetResourceName(manufacturer string, mode workercore.DeviceAllocationMode) core.ResourceName {
	resName := _ManufacturerResourceNameMap[manufacturer]
	switch mode {
	default:
		return resName
	case workercore.DeviceAllocationModeShared:
		return resName + SharedResourceNameSuffix
	case workercore.DeviceAllocationModeSliced:
		return resName + SlicedResourceNameSuffix
	}
}

// IsKnownResourceName checks if the given resource name is a well-known resource name.
func IsKnownResourceName(name core.ResourceName) bool {
	switch {
	case stringx.HasSuffix(name, SharedResourceNameSuffix):
		name = name[:len(name)-len(SharedResourceNameSuffix)]
	case stringx.HasSuffix(name, SlicedResourceNameSuffix):
		name = name[:len(name)-len(SlicedResourceNameSuffix)]
	}
	return _ResourceNameSet.Has(name)
}

// GetPciID returns the PCI vendor for the given manufacturer.
func GetPciID(manufacturer string) string {
	return _ManufacturerPciIDMap[manufacturer]
}

// GetPciIDs returns the list of PCI vendor IDs for all known manufacturers.
func GetPciIDs() []string {
	ids := maps.Values(_ManufacturerPciIDMap)
	sort.Strings(ids)
	return ids
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
