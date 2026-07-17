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
	// SharedResourceNameSuffix is the coarse-grained shared counting key,
	// advertised by the device-plugin (its value is the sharing ownership card count)
	// and used for Kueue credits accounting.
	SharedResourceNameSuffix = ".shared"
	// SlicedResourceNameSuffix is the coarse sliced injection-token key, advertised
	// by the device-plugin as a loose per-card token pool sized by the device group's
	// max slice count (so node allocatable is cards*maxSlices, not the card count); it
	// only triggers the allocator's Allocate() injection hook. The binding credits
	// accounting lives on the fine-grained ".sliced.units" key.
	SlicedResourceNameSuffix = ".sliced"
	// SlicedUnitsResourceNameSuffix is the fine-grained sliced counting key,
	// reported per node via Patch Node and used for Kueue credits accounting.
	SlicedUnitsResourceNameSuffix = ".sliced.units"
	// SlicedCoresPercentageResourceNameSuffix is the per-card SM (compute) budget
	// key for sliced allocations, reported per node as count*SlicedResourceMaxSize*100
	// (slices may oversubscribe compute, so each of the MaxPartitions slots is worth
	// 100%). It is a gate-2 node-level counting resource consumed by the default
	// scheduler/kubelet and the device-plugin (CUDA_DEVICE_SM_LIMIT); it is never
	// folded into Kueue credits.
	SlicedCoresPercentageResourceNameSuffix = ".sliced.cores-percentage"
	// SlicedMemoryPercentageResourceNameSuffix is the per-card VRAM-percentage budget
	// key for sliced allocations, reported per node as count*100. Gate-2 node-level
	// only (the webhook folds it into .sliced.units); never folded into Kueue credits.
	SlicedMemoryPercentageResourceNameSuffix = ".sliced.memory-percentage"
	// SlicedMemoryMibResourceNameSuffix is the per-card absolute VRAM budget key (MiB)
	// for sliced allocations, reported per node as count*cardVRAMMib. Gate-2
	// node-level only (drives CUDA_DEVICE_MEMORY_LIMIT_IN_BYTES; the webhook folds it
	// into .sliced.units via floor(mib/cardVRAM*M)); never folded into Kueue credits.
	SlicedMemoryMibResourceNameSuffix = ".sliced.memory-mib"

	// VisibilityResourceNamePrefix and VisibilityResourceNameSuffix compose the device-only
	// "visibility" resource the SSH sidecar requests to co-allocate the same physical
	// accelerator(s) its workload container (main) was granted, e.g.
	// "device.gpustack.ai/nvidia.visibility". It is deliberately outside the accelerator
	// resource families ("<vendor>/<device>[.shared|.sliced…]") so the Pod webhook's
	// one-mode check ignores it, and the device-plugin serves it as a virtual resource that
	// injects only device visibility (no slice, no ledger unit).
	VisibilityResourceNamePrefix = "device.gpustack.ai/"
	VisibilityResourceNameSuffix = ".visibility"
)

const (
	// ResourceMaxUnits is the global denominator D = 2^9 * 5^5 = 1600000 and the
	// single per-card unit basis shared by every allocation mode: one whole card is
	// worth D normalized units, Shared yields D/10 per ownership, and a card sliced
	// into N partitions yields D/N units per slice. D keeps the 2^9 factor so every
	// power-of-two partition size up to SlicedResourceMaxSize=512 divides it exactly,
	// and the 5^5 factor (vs the former 12800 = 2^9 * 5^2) makes the memory-1% step
	// D/100 = 16000 an integer for the per-card VRAM-percentage slice keys. It is also
	// the integer credit base B = CreditsPerCard (one whole card = D credits), so the
	// .sliced.units→credits Kueue factor is B/D = 1 and every per-mode credit value
	// stays integer-valued. It also seeds the device-plugin per-card unit grid and the
	// Devices CR AcceleratorAllocation ruler.
	ResourceMaxUnits = 1_600_000
	// SharedResourceMaxSize is the maximum number of owners a card can be shared among.
	SharedResourceMaxSize = 10
	// SlicedResourceMaxSize is the maximum number of partitions a card can be
	// sliced into (a power of two; the largest divisor of D below).
	SlicedResourceMaxSize = 512
)

var (
	_ManufacturerPciVendorIDMap               map[string]string
	_ManufacturerAcceleratableResourceNameMap map[string]core.ResourceName
	_ManufacturerAcceleratableRuntimeNameMap  map[string]string
	_AcceleratableResourceNameSet             sets.Set[core.ResourceName]
	_SlicedResourceSizes                      []int64
	_SlicedResourceOperatedSizesSet           sets.Set[string]
	_SlicedResourceUnitsPerSlice              map[int64]int64
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

	// Pre-calculate the normalized per-slice unit value D/size for each sliced
	// size. D is divisible by every power-of-two size up to SlicedResourceMaxSize,
	// so these are exact integers.
	_SlicedResourceUnitsPerSlice = make(map[int64]int64)
	for _, size := range _SlicedResourceSizes {
		_SlicedResourceUnitsPerSlice[size] = ResourceMaxUnits / size
	}
}

// GetPciVendorID returns the PCI vendor ID for the given manufacturer.
func GetPciVendorID(manufacturer string) string {
	return _ManufacturerPciVendorIDMap[manufacturer]
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

// GetAcceleratableResourceName returns the resource name advertised by the device-plugin
// to the kubelet for the given manufacturer and allocation mode:
//   - Exclusive → "nvidia.com/gpu", Shared → "nvidia.com/gpu.shared",
//     Sliced → "nvidia.com/gpu.sliced" (the accelerator families).
//   - Visibility → "device.gpustack.ai/nvidia.visibility": the device-only resource the SSH
//     sidecar requests (with the SAME quantity its workload container asks of the real
//     accelerator) to co-allocate visibility to the same physical device(s). It is
//     deliberately outside the accelerator families, so IsKnownAcceleratableResourceName is
//     false for it and admission does not read it as an allocation mode.
func GetAcceleratableResourceName(manufacturer string, mode workercore.DeviceAllocationMode) core.ResourceName {
	resName := _ManufacturerAcceleratableResourceNameMap[manufacturer]
	switch mode {
	default:
		return resName
	case workercore.DeviceAllocationModeShared:
		return resName + SharedResourceNameSuffix
	case workercore.DeviceAllocationModeSliced:
		return resName + SlicedResourceNameSuffix
	case workercore.DeviceAllocationModeVisibility:
		return core.ResourceName(VisibilityResourceNamePrefix + manufacturer + VisibilityResourceNameSuffix)
	}
}

// GetAcceleratableSlicedUnitsResourceName returns the fine-grained sliced counting
// key for the given manufacturer (e.g. "nvidia.com/gpu.sliced.units"). It is
// reported per node via Patch Node and used as the Kueue credits transformation
// input, distinct from the coarse injection-token key returned by
// GetAcceleratableResourceName with DeviceAllocationModeSliced (".sliced").
func GetAcceleratableSlicedUnitsResourceName(manufacturer string) core.ResourceName {
	return _ManufacturerAcceleratableResourceNameMap[manufacturer] + SlicedUnitsResourceNameSuffix
}

// GetAcceleratableSlicedCoresPercentageResourceName returns the per-card SM budget
// key for the given manufacturer (e.g. "nvidia.com/gpu.sliced.cores-percentage").
// It is a gate-2 node-level resource, distinct from the credits input returned by
// GetAcceleratableSlicedUnitsResourceName.
func GetAcceleratableSlicedCoresPercentageResourceName(manufacturer string) core.ResourceName {
	return _ManufacturerAcceleratableResourceNameMap[manufacturer] + SlicedCoresPercentageResourceNameSuffix
}

// GetAcceleratableSlicedMemoryPercentageResourceName returns the per-card
// VRAM-percentage budget key for the given manufacturer
// (e.g. "nvidia.com/gpu.sliced.memory-percentage"). Gate-2 node-level resource.
func GetAcceleratableSlicedMemoryPercentageResourceName(manufacturer string) core.ResourceName {
	return _ManufacturerAcceleratableResourceNameMap[manufacturer] + SlicedMemoryPercentageResourceNameSuffix
}

// GetAcceleratableSlicedMemoryMibResourceName returns the per-card absolute VRAM
// budget key in MiB for the given manufacturer
// (e.g. "nvidia.com/gpu.sliced.memory-mib"). Gate-2 node-level resource.
func GetAcceleratableSlicedMemoryMibResourceName(manufacturer string) core.ResourceName {
	return _ManufacturerAcceleratableResourceNameMap[manufacturer] + SlicedMemoryMibResourceNameSuffix
}

// IsKnownAcceleratableResourceName reports whether the given resource name is a well-known accelerator resource name.
func IsKnownAcceleratableResourceName(name core.ResourceName) bool {
	switch {
	case stringx.HasSuffix(name, SharedResourceNameSuffix):
		name = name[:len(name)-len(SharedResourceNameSuffix)]
	case stringx.HasSuffix(name, SlicedUnitsResourceNameSuffix):
		name = name[:len(name)-len(SlicedUnitsResourceNameSuffix)]
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

// IsValidSlicedPartitions reports whether n is a usable slice partition count: a
// power of two in [2, SlicedResourceMaxSize]. A single slice (n=1) is a whole card
// and is not a valid slicing request.
func IsValidSlicedPartitions(n int64) bool {
	return n >= 2 && n <= SlicedResourceMaxSize && n&(n-1) == 0
}

// QuantityToSliceCount converts a per-card credits quantity to the number of
// slices it represents on a card sliced into `sliced` partitions: floor(q * sliced).
// It is independent of the global denominator D (a whole card always yields
// `sliced` slices), and floors to an integer count.
func QuantityToSliceCount(q resource.Quantity, sliced int64) resource.Quantity {
	if sliced <= 0 {
		return q
	}
	if _, ok := _SlicedResourceUnitsPerSlice[sliced]; !ok {
		return q
	}
	// Multiply-first (×sliced before ÷1e6) keeps full precision; inputs are
	// per-card credits bounded by physical accelerator counts, so the int64
	// intermediate (q·1e6·sliced) never overflows in practice.
	q.Set(q.ScaledValue(resource.Micro) * sliced / 1e6)
	return q
}

// QuantityToAlignedValue converts a slice count to the normalized per-card unit
// value written to the `.sliced.units` resource: q * (D / sliced).
func QuantityToAlignedValue(q resource.Quantity, sliced int64) resource.Quantity {
	if sliced <= 0 {
		return q
	}
	unitsPerSlice, ok := _SlicedResourceUnitsPerSlice[sliced]
	if !ok {
		return q
	}
	q.Mul(unitsPerSlice)
	return q
}

// MemoryMibToUnits converts an absolute per-card VRAM request in MiB to the
// normalized per-card `.sliced.units` value: floor(mib / cardVRAMMib × D), where
// D is ResourceMaxUnits. VRAM is the non-oversubscribable anchor, so the
// conversion floors and never over-allocates. It returns 0 when cardVRAMMib is
// non-positive (the caller treats that as "cannot compute").
func MemoryMibToUnits(mib, cardVRAMMib int64) int64 {
	if cardVRAMMib <= 0 {
		return 0
	}
	return mib * ResourceMaxUnits / cardVRAMMib
}

// QuantityToOriginalValue converts a normalized per-card unit value back to the
// original slice count: q / (D / sliced).
func QuantityToOriginalValue(q resource.Quantity, sliced int64) resource.Quantity {
	if sliced <= 0 {
		return q
	}
	unitsPerSlice, ok := _SlicedResourceUnitsPerSlice[sliced]
	if !ok {
		return q
	}
	q.SetScaled(q.ScaledValue(resource.Micro)/unitsPerSlice, resource.Micro)
	return q
}

const (
	// CreditsPerCard is the integer credit base B: one whole accelerator card is
	// worth B credits. It equals the global denominator D (ResourceMaxUnits), so
	// the finest sliced unit (1/SlicedResourceMaxSize of a card) maps to the
	// integer B/SlicedResourceMaxSize (=3125) and the ".sliced.units"→credits Kueue
	// factor is exactly B/D=1. Scoring credits as B×card-fraction keeps every
	// per-mode value an integer, so Kueue's ResourceValue int64 quantization
	// (q.Value(), which ceils non-CPU resources) never rounds a fractional credit
	// up to 1 — the failure that broke the sliced borrow accounting.
	CreditsPerCard = ResourceMaxUnits
)

// CardsToCredits scales a whole-card count to its integer credit value (cards×B).
// It is used to build the Kueue ClusterQueue credits NominalQuota so the quota is
// expressed on the same integer basis as the transformed credit requests.
func CardsToCredits(cards resource.Quantity) resource.Quantity {
	cards.Mul(CreditsPerCard)
	return cards
}

// CreditsToCards converts a credit quantity back to card units (credits÷B),
// preserving the fraction at micro scale so the exclusive whole-card display and
// the sliced per-partition display (×partitions) stay exact. It first reads the
// credit count via Value() — the same int64 quantization Kueue's ResourceValue
// applies to non-CPU resources (Value() ceils) — so the operator's card view
// always agrees with Kueue's accounting and a misconfigured fractional credit
// degrades to a safe integer rather than a misleading fraction.
func CreditsToCards(credits resource.Quantity) resource.Quantity {
	credits.SetScaled(credits.Value()*1_000_000/CreditsPerCard, resource.Micro)
	return credits
}
