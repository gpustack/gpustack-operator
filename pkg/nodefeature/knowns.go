package nodefeature

import (
	"regexp"
	"slices"
	"sort"
	"strings"

	"golang.org/x/exp/maps"
	core "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/apimachinery/pkg/util/validation"
	klog "k8s.io/klog/v2"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	"gpustack.ai/gpustack/pkg/utils/osx"
	"gpustack.ai/gpustack/pkg/utils/strconvx"
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
	// advertised by the device-plugin (its value is the sharing ownership accelerator
	// count) and used for Kueue credits accounting.
	SharedResourceNameSuffix = ".shared"
	// SlicedResourceNameSuffix is the coarse sliced injection-token key, advertised
	// by the device-plugin as a loose per-accelerator token pool sized by the device
	// group's max slice count (so node allocatable is accelerators*maxSlices, not the
	// accelerator count); it only triggers the allocator's Allocate() injection hook.
	// The binding credits accounting lives on the fine-grained ".sliced.units" key.
	SlicedResourceNameSuffix = ".sliced"
	// SlicedUnitsResourceNameSuffix is the fine-grained sliced counting key,
	// reported per node via Patch Node and used for Kueue credits accounting.
	SlicedUnitsResourceNameSuffix = ".sliced.units"
	// SlicedCoresPercentageResourceNameSuffix is the per-accelerator SM (compute) budget
	// key for sliced allocations, reported per node by the NodeCapacityReconciler and
	// sized from the device group's max slice count and compute-overcommit flag:
	//
	//   - With compute overcommit, each slice is granted a full 100%, so the
	//     per-accelerator budget scales with the slice count.
	//   - Without it, the per-accelerator compute stays a single 100%.
	//
	// It is a gate-2 node-level counting resource consumed by the default
	// scheduler/kubelet and the device-plugin (CUDA_DEVICE_SM_LIMIT); it is never folded
	// into Kueue credits.
	SlicedCoresPercentageResourceNameSuffix = ".sliced.cores-percentage"
	// SlicedMemoryPercentageResourceNameSuffix is the per-accelerator VRAM-percentage
	// budget key for sliced allocations, reported per node as count*100. Gate-2 node-level
	// only (the webhook folds it into .sliced.units); never folded into Kueue credits.
	SlicedMemoryPercentageResourceNameSuffix = ".sliced.memory-percentage"
	// SlicedMemoryMibResourceNameSuffix is the per-accelerator absolute VRAM budget key
	// (MiB) for sliced allocations, reported per node as count*cardVRAMMib. Gate-2
	// node-level only (drives CUDA_DEVICE_MEMORY_LIMIT_IN_BYTES; the webhook folds it
	// into .sliced.units via floor(mib/cardVRAM*M)); never folded into Kueue credits.
	SlicedMemoryMibResourceNameSuffix = ".sliced.memory-mib"
	// PartitionedResourceNameSuffix is the coarse physical-partition token key,
	// advertised by the device-plugin for the accelerators put into a hardware partitioning
	// mode. It only triggers the allocator's Allocate() hook, which places the
	// instance itself; the counting lives on ".partitioned.units" and on the
	// per-profile keys. It is advertised only by a manufacturer that has a partition
	// kind.
	PartitionedResourceNameSuffix = ".partitioned"
	// PartitionedUnitsResourceNameSuffix is the fine-grained physical-partition counting
	// key, reported per node via Patch Node and used for Kueue credits accounting. It
	// values a partitioned accelerator at whole-accelerator units, mirroring
	// ".sliced.units" for a logically sliceable accelerator.
	PartitionedUnitsResourceNameSuffix = ".partitioned.units"

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
	// single per-accelerator unit basis shared by every allocation mode: one whole
	// accelerator is worth D normalized units, Shared yields D/10 per ownership, and an
	// accelerator sliced into N partitions yields D/N units per slice. D keeps the 2^9
	// factor so every power-of-two partition size up to SlicedResourceMaxSize=512 divides
	// it exactly, and the 5^5 factor (vs the former 12800 = 2^9 * 5^2) makes the
	// memory-1% step D/100 = 16000 an integer for the per-accelerator VRAM-percentage
	// slice keys. It is also the integer credit base B = CreditsPerAccelerator (one whole
	// accelerator = D credits), so the .sliced.units→credits Kueue factor is B/D = 1 and
	// every per-mode credit value stays integer-valued. It also seeds the device-plugin
	// per-accelerator unit grid and the Devices CR AcceleratorAllocation ruler.
	ResourceMaxUnits = 1_600_000
	// SharedResourceMaxSize is the maximum number of owners an accelerator can be shared
	// among.
	SharedResourceMaxSize = 10
	// SlicedResourceMaxSize is the maximum number of partitions an accelerator can be
	// sliced into (a power of two; the largest divisor of D below).
	SlicedResourceMaxSize = 512
)

var (
	_ManufacturerPciVendorIDMap               map[string]string
	_ManufacturerAcceleratableResourceNameMap map[string]core.ResourceName
	_ManufacturerAcceleratableRuntimeNameMap  map[string]string
	_ManufacturerPartitionKindMap             map[string]string
	_AcceleratableResourceNameSet             sets.Set[core.ResourceName]
	_PartitionedProfileResourceNamePrefixMap  map[string]string
	_SlicedResourceSizes                      []int64
	_SlicedResourceOperatedSizesSet           sets.Set[string]
	_SlicedResourceUnitsPerSlice              map[int64]int64
)

// _AcceleratablePciClassPrefixes are the PCI device-class prefixes a device carrying an
// accelerator presents as. NFD is configured to label exactly these classes, and the
// gpustack-cpu-info NodeFeatureRule matches them, so the two lists are one fact.
var _AcceleratablePciClassPrefixes = []string{"02", "03", "0b", "12"}

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

	// _ManufacturerPartitionKindMap maps a manufacturer to its own name for hardware
	// partitioning, which becomes the per-profile key's segment prefix — "mig" is NVIDIA's
	// name, not the concept's. A manufacturer absent from the map has no hardware
	// partitioning, so it advertises no ".partitioned" family at all. Each entry is
	// overridable by GPUSTACK_<MANUFACTURER>_PARTITION_KIND.
	_ManufacturerPartitionKindMap = map[string]string{
		ManufacturerNVIDIA: "mig",
		// T-Head PPU carves an accelerator into GPU instances under the same feature name its
		// own management library and CLI use, so it shares the word rather than coining one.
		ManufacturerTHead: "mig",
	}
	for _, manufacturer := range maps.Keys(_ManufacturerPartitionKindMap) {
		// Extract the hardware partitioning name from environment variable if exists,
		// and override the default value in the map.
		//
		// E.g. for NVIDIA, the environment variable is "GPUSTACK_NVIDIA_PARTITION_KIND".
		if v := osx.Getenv("GPUSTACK_" + strings.ToUpper(manufacturer) + "_PARTITION_KIND"); v != "" {
			_ManufacturerPartitionKindMap[manufacturer] = v
		}
	}

	// Make a set of all known resource names for quick lookup.
	_AcceleratableResourceNameSet = sets.New[core.ResourceName](maps.Values(_ManufacturerAcceleratableResourceNameMap)...)

	// Resolve the hardware partitioning names, which build on the resource names above.
	_PartitionedProfileResourceNamePrefixMap = make(map[string]string, len(_ManufacturerPartitionKindMap))
	for manufacturer, kind := range _ManufacturerPartitionKindMap {
		resName := _ManufacturerAcceleratableResourceNameMap[manufacturer]
		if resName == "" || kind == "" {
			continue
		}
		_PartitionedProfileResourceNamePrefixMap[manufacturer] = string(resName) + PartitionedResourceNameSuffix + "." + kind + "-"
	}

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

// GetPartitionKind returns the manufacturer's own name for hardware partitioning
// ("mig" for NVIDIA), which the per-profile key's segment prefix is built from. It
// returns "" when the manufacturer has no hardware partitioning, in which case the
// manufacturer advertises no key of the ".partitioned" family.
func GetPartitionKind(manufacturer string) string {
	if _PartitionedProfileResourceNamePrefixMap[manufacturer] == "" {
		return ""
	}
	return _ManufacturerPartitionKindMap[manufacturer]
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

// GetAcceleratablePciClassPrefixes returns the PCI device-class prefixes an accelerator
// presents as: display controllers, processing accelerators and the co-processor class
// (see https://admin.pci-ids.ucw.cz/read/PD).
func GetAcceleratablePciClassPrefixes() []string {
	return slices.Clone(_AcceleratablePciClassPrefixes)
}

// GetAcceleratableResourceName returns the resource name advertised by the device-plugin
// to the kubelet for the given manufacturer and allocation mode:
//   - Exclusive → "nvidia.com/gpu", Shared → "nvidia.com/gpu.shared",
//     Sliced → "nvidia.com/gpu.sliced" (the accelerator families).
//   - Partitioned → "nvidia.com/gpu.partitioned", the hardware-partitioning family. It is
//     empty for a manufacturer with no partition kind, which advertises no key of that
//     family at all.
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
	case workercore.DeviceAllocationModePartitioned:
		if GetPartitionKind(manufacturer) == "" {
			return ""
		}
		return resName + PartitionedResourceNameSuffix
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

// GetAcceleratableSlicedCoresPercentageResourceName returns the per-accelerator SM budget
// key for the given manufacturer (e.g. "nvidia.com/gpu.sliced.cores-percentage").
// It is a gate-2 node-level resource, distinct from the credits input returned by
// GetAcceleratableSlicedUnitsResourceName.
func GetAcceleratableSlicedCoresPercentageResourceName(manufacturer string) core.ResourceName {
	return _ManufacturerAcceleratableResourceNameMap[manufacturer] + SlicedCoresPercentageResourceNameSuffix
}

// GetAcceleratableSlicedMemoryPercentageResourceName returns the per-accelerator
// VRAM-percentage budget key for the given manufacturer
// (e.g. "nvidia.com/gpu.sliced.memory-percentage"). Gate-2 node-level resource.
func GetAcceleratableSlicedMemoryPercentageResourceName(manufacturer string) core.ResourceName {
	return _ManufacturerAcceleratableResourceNameMap[manufacturer] + SlicedMemoryPercentageResourceNameSuffix
}

// GetAcceleratableSlicedMemoryMibResourceName returns the per-accelerator absolute VRAM
// budget key in MiB for the given manufacturer
// (e.g. "nvidia.com/gpu.sliced.memory-mib"). Gate-2 node-level resource.
func GetAcceleratableSlicedMemoryMibResourceName(manufacturer string) core.ResourceName {
	return _ManufacturerAcceleratableResourceNameMap[manufacturer] + SlicedMemoryMibResourceNameSuffix
}

// GetAcceleratablePartitionedUnitsResourceName returns the fine-grained
// physical-partition counting key for the given manufacturer (e.g.
// "nvidia.com/gpu.partitioned.units"), reported per node via Patch Node and used as a
// Kueue credits transformation input. It returns "" when the manufacturer has no
// partition kind.
func GetAcceleratablePartitionedUnitsResourceName(manufacturer string) core.ResourceName {
	if GetPartitionKind(manufacturer) == "" {
		return ""
	}
	return _ManufacturerAcceleratableResourceNameMap[manufacturer] + PartitionedUnitsResourceNameSuffix
}

// GetAcceleratablePartitionedProfileResourceName returns the per-profile physical-partition
// key for a manufacturer and profile — profile "3g.40gb" for nvidia yields
// "nvidia.com/gpu.partitioned.mig-3g.40gb". The profile is published through
// PublishPartitionedProfileName, so the key always carries the published spelling whichever
// spelling the caller holds, and VendorPartitionedProfileOf maps it back. Beyond that the
// name is used verbatim: one that is not a valid resource-name segment is excluded upstream
// when the accelerator's inventory is built. It returns "" when the manufacturer has no partition
// kind, or when the name would not yield a valid resource name.
func GetAcceleratablePartitionedProfileResourceName(manufacturer, profile string) core.ResourceName {
	prefix := _PartitionedProfileResourceNamePrefixMap[manufacturer]
	if prefix == "" || profile == "" {
		return ""
	}
	name := prefix + PublishPartitionedProfileName(manufacturer, profile)
	if errs := validation.IsQualifiedName(name); len(errs) != 0 {
		// The profile name is never rewritten to make it key-safe: a rewritten key
		// could not be mapped back to the hardware profile it names.
		klog.Warningf("excluding %s partition profile %q: it does not yield a valid resource name: %s",
			manufacturer, profile, strings.Join(errs, "; "))
		return ""
	}
	return core.ResourceName(name)
}

// NormalizePartitionedProfileName reduces a vendor-reported hardware-partition profile
// name to the bare geometry a partition resource key carries: whitespace-trimmed,
// lower-cased, and stripped of the feature-name prefix a vendor's library or CLI displays
// ahead of the geometry ("MIG 1c.12g" → "1c.12g"). No vendor geometry name contains a
// space, so the geometry is the final whitespace-separated field. Nothing here knows a
// vendor's prefix words, so a lone field is taken as the geometry; a name of only
// whitespace normalizes to the empty string.
//
// It never rewrites a character to make a name key-safe. A name that still cannot form a
// valid resource-name segment is unusable and must be dropped by the caller — checked with
// GetAcceleratablePartitionedProfileResourceName, which returns "" for it — because a
// rewritten key could not be mapped back to the profile it names.
//
// Both ends of the round trip must normalize through this one function: a detector records a
// profile under the normalized name, and the vendor driver seam resolves that name back to a
// raw driver profile id by comparing the normalized name against the names the driver
// reports. Two copies of this transform that drifted apart would leave a published profile
// silently unrequestable — the key would exist and admit a request that allocation could then
// never match. The name this function yields is the vendor's own spelling; publishing it as a
// resource key is PublishPartitionedProfileName's separate job.
func NormalizePartitionedProfileName(raw string) string {
	fields := strings.Fields(strings.ToLower(raw))
	if len(fields) == 0 {
		return ""
	}
	return fields[len(fields)-1]
}

// _SeparatorlessPartitionedProfileManufacturerSet holds the manufacturers whose own library
// spells a partition profile's geometry with no separator between its two numbers — T-Head
// reports "MIG 4g48gb" where NVIDIA reports "3g.40gb". The two describe the same shape of
// thing and read the same way to whoever writes a Pod spec only if they are spelled the same,
// so a profile of a manufacturer listed here is published with the separator and handled
// internally without one.
var _SeparatorlessPartitionedProfileManufacturerSet = sets.New(ManufacturerTHead)

var (
	// _VendorPartitionedProfileGeometryRegex matches a profile geometry spelled the way a
	// separator-less manufacturer writes it, capturing the compute-slice and memory numbers.
	_VendorPartitionedProfileGeometryRegex = regexp.MustCompile(`^([0-9]+)g([0-9]+)gb$`)
	// _PublishedPartitionedProfileGeometryRegex matches the same geometry carrying the
	// separator, which is the form this operator publishes.
	_PublishedPartitionedProfileGeometryRegex = regexp.MustCompile(`^([0-9]+)g\.([0-9]+)gb$`)
)

// PublishPartitionedProfileName converts a partition profile name from the spelling its
// manufacturer's library uses to the spelling this operator publishes — for a manufacturer
// that omits the separator, "4g48gb" becomes "4g.48gb". It is the forward half of the naming
// boundary: the resource key, the per-profile ledgers a user reads, and the request a user
// writes all carry the published name, while the Devices record, the ownership markers and
// every name handed to or matched against a vendor library keep the vendor's own.
//
// A name outside the two-number geometry is published unchanged rather than guessed at, and a
// name already carrying the separator is returned as-is — so the conversion is idempotent and
// a caller need not know which spelling it holds. A manufacturer whose library writes the
// separator itself (NVIDIA) is never touched, so its published keys are unaffected.
func PublishPartitionedProfileName(manufacturer, profile string) string {
	if !_SeparatorlessPartitionedProfileManufacturerSet.Has(manufacturer) {
		return profile
	}
	m := _VendorPartitionedProfileGeometryRegex.FindStringSubmatch(profile)
	if m == nil {
		return profile
	}
	return m[1] + "g." + m[2] + "gb"
}

// VendorPartitionedProfileName is the reverse of PublishPartitionedProfileName: it converts a
// published profile name back to the spelling the manufacturer's own library uses, so a name
// crossing back into the operator can be matched against the Devices record, an ownership
// marker or the vendor library. It is equally idempotent, and equally leaves a name of another
// shape — and every manufacturer that writes the separator itself — untouched.
//
// An empty manufacturer is a pass-through, so a caller that has not resolved one yet reads the
// name it was given rather than a silently rewritten one.
func VendorPartitionedProfileName(manufacturer, profile string) string {
	if !_SeparatorlessPartitionedProfileManufacturerSet.Has(manufacturer) {
		return profile
	}
	m := _PublishedPartitionedProfileGeometryRegex.FindStringSubmatch(profile)
	if m == nil {
		return profile
	}
	return m[1] + "g" + m[2] + "gb"
}

// PartitionedProfileOf returns the physical-partition profile name encoded in a
// "<base>.partitioned.<kind>-<profile>" resource key of a known accelerator base whose
// manufacturer declares that kind, and whether name is such a key. It is the plain reverse of
// GetAcceleratablePartitionedProfileResourceName: the name comes back in the published
// spelling the key carries, which is what a user-facing ledger or message wants. A caller
// about to match it against the Devices record, an ownership marker or a vendor library wants
// VendorPartitionedProfileOf instead.
//
// The counting key "<base>.partitioned.units" shares the ".partitioned." prefix but is
// not a per-profile key, so it is never read as a profile.
func PartitionedProfileOf(name core.ResourceName) (string, bool) {
	_, profile, ok := partitionedProfileOf(name)
	return profile, ok
}

// VendorPartitionedProfileOf returns the profile a "<base>.partitioned.<kind>-<profile>" key
// names, in the spelling the manufacturer's own library uses, and whether name is such a key.
// It is the reverse half of the naming boundary — the one every caller crossing back into the
// operator's internals must use, since the Devices record, the ownership markers and the
// vendor library all carry the vendor's spelling.
func VendorPartitionedProfileOf(name core.ResourceName) (string, bool) {
	manufacturer, profile, ok := partitionedProfileOf(name)
	if !ok {
		return "", false
	}
	return VendorPartitionedProfileName(manufacturer, profile), true
}

// partitionedProfileOf resolves a per-profile partition key to the manufacturer that owns it
// and the profile name it carries, so the two public readers cannot disagree about which key
// is a profile key.
func partitionedProfileOf(name core.ResourceName) (manufacturer, profile string, ok bool) {
	s := string(name)
	for m, prefix := range _PartitionedProfileResourceNamePrefixMap {
		p, cut := strings.CutPrefix(s, prefix)
		if cut && p != "" {
			return m, p, true
		}
	}
	return "", "", false
}

// IsKnownAcceleratableResourceName reports whether the given resource name is a key of one
// of the four accelerator families. It answers "is this one of ours"; ResourceFamilyOf, which
// it defers to, answers "which one" — keeping a single definition means a newly added family
// can never be known to one caller and unknown to the other.
//
// The visibility resource is deliberately excluded: it is outside the accelerator families,
// so admission never reads it as an allocation mode.
func IsKnownAcceleratableResourceName(name core.ResourceName) bool {
	switch ResourceFamilyOf(name) {
	case ResourceFamilyExclusive, ResourceFamilyShared, ResourceFamilySliced, ResourceFamilyPartitioned:
		return true
	}
	return false
}

// ResourceFamily names the accelerator resource family a resource name belongs to.
// Every key of a family — the coarse device-plugin token key, the fine-grained counting
// keys, and any variable-tailed per-profile key — classifies as its family, since the
// question the admission rules ask of a key is "which family does it belong to".
type ResourceFamily string

const (
	// ResourceFamilyNone is the classification of a resource name outside every
	// accelerator family, e.g. "cpu" or a credits resource.
	ResourceFamilyNone ResourceFamily = "none"
	// ResourceFamilyExclusive is the whole-accelerator family, "<base>".
	ResourceFamilyExclusive ResourceFamily = "exclusive"
	// ResourceFamilyShared is the accelerator-sharing family, "<base>.shared".
	ResourceFamilyShared ResourceFamily = "shared"
	// ResourceFamilySliced is the logical (software injection) slicing family,
	// "<base>.sliced" and its sub-keys.
	ResourceFamilySliced ResourceFamily = "sliced"
	// ResourceFamilyPartitioned is the physical (hardware partitioning) family,
	// "<base>.partitioned" and its sub-keys.
	ResourceFamilyPartitioned ResourceFamily = "partitioned"
	// ResourceFamilyVisibility is the device-only co-allocation family the SSH sidecar
	// requests, "device.gpustack.ai/<manufacturer>.visibility". It is deliberately
	// outside the accelerator families, so the one-family rules ignore it.
	ResourceFamilyVisibility ResourceFamily = "visibility"
)

// _ResourceFamilyFixedSuffixes maps each fixed key suffix to its family. The suffixes are
// listed longest first for readability; the classification is order-independent, because a
// key matches at most one suffix with a known accelerator base in front of it.
var _ResourceFamilyFixedSuffixes = []struct {
	suffix string
	family ResourceFamily
}{
	{SlicedMemoryPercentageResourceNameSuffix, ResourceFamilySliced},
	{SlicedCoresPercentageResourceNameSuffix, ResourceFamilySliced},
	{SlicedMemoryMibResourceNameSuffix, ResourceFamilySliced},
	{PartitionedUnitsResourceNameSuffix, ResourceFamilyPartitioned},
	{SlicedUnitsResourceNameSuffix, ResourceFamilySliced},
	{PartitionedResourceNameSuffix, ResourceFamilyPartitioned},
	{SlicedResourceNameSuffix, ResourceFamilySliced},
	{SharedResourceNameSuffix, ResourceFamilyShared},
}

// ResourceFamilyOf classifies a resource name into the accelerator family it belongs to,
// the single decision the Pod webhooks and the node-devices admission check drive their
// one-family rules through.
//
// A name is classified only when it is well-formed: its base must be a known accelerator
// resource name (or, for visibility, a known manufacturer) and any variable tail must be
// non-empty. Anything else — an unrecognized sub-key, an unknown base, a non-accelerator
// resource — is ResourceFamilyNone.
func ResourceFamilyOf(name core.ResourceName) ResourceFamily {
	s := string(name)

	if manufacturer, ok := strings.CutPrefix(s, VisibilityResourceNamePrefix); ok {
		manufacturer, ok = strings.CutSuffix(manufacturer, VisibilityResourceNameSuffix)
		if ok && IsKnownAcceleratableManufacturer(manufacturer) {
			return ResourceFamilyVisibility
		}
		return ResourceFamilyNone
	}

	// The variable-tailed per-profile key carries its own base and tail checks.
	if _, ok := PartitionedProfileOf(name); ok {
		return ResourceFamilyPartitioned
	}

	for _, e := range _ResourceFamilyFixedSuffixes {
		if base, ok := strings.CutSuffix(s, e.suffix); ok &&
			_AcceleratableResourceNameSet.Has(core.ResourceName(base)) {
			return e.family
		}
	}

	if _AcceleratableResourceNameSet.Has(name) {
		return ResourceFamilyExclusive
	}
	return ResourceFamilyNone
}

// GetAcceleratableRuntimeName returns the accelerator runtime name for the given manufacturer,
// usually, it's used as the container runtime class name for the accelerator resource.
func GetAcceleratableRuntimeName(manufacturer string) string {
	return _ManufacturerAcceleratableRuntimeNameMap[manufacturer]
}

// IsValidSlicedPartitions reports whether n is a usable slice partition count: a
// power of two in [2, SlicedResourceMaxSize]. A single slice (n=1) is a whole
// accelerator and is not a valid slicing request.
func IsValidSlicedPartitions(n int64) bool {
	return n >= 2 && n <= SlicedResourceMaxSize && n&(n-1) == 0
}

// QuantityToSliceCount converts a per-accelerator credits quantity to the number of
// slices it represents on an accelerator sliced into `sliced` partitions:
// floor(q * sliced). It is independent of the global denominator D (a whole
// accelerator always yields `sliced` slices), and floors to an integer count.
func QuantityToSliceCount(q resource.Quantity, sliced int64) resource.Quantity {
	if sliced <= 0 {
		return q
	}
	if _, ok := _SlicedResourceUnitsPerSlice[sliced]; !ok {
		return q
	}
	// Multiply-first (×sliced before ÷1e6) keeps full precision; inputs are
	// per-accelerator credits bounded by physical accelerator counts, so the int64
	// intermediate (q·1e6·sliced) never overflows in practice.
	q.Set(q.ScaledValue(resource.Micro) * sliced / 1e6)
	return q
}

// QuantityToAlignedValue converts a slice count to the normalized per-accelerator unit
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

// MemoryMibToUnits converts an absolute per-accelerator VRAM request in MiB to the
// normalized per-accelerator `.sliced.units` value: floor(mib / cardVRAMMib × D), where
// D is ResourceMaxUnits. VRAM is the non-oversubscribable anchor, so the
// conversion floors and never over-allocates. It returns 0 when cardVRAMMib is
// non-positive (the caller treats that as "cannot compute").
func MemoryMibToUnits(mib, cardVRAMMib int64) int64 {
	if cardVRAMMib <= 0 {
		return 0
	}
	return mib * ResourceMaxUnits / cardVRAMMib
}

// QuantityToOriginalValue converts a normalized per-accelerator unit value back to the
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
	// CreditsPerAccelerator is the integer credit base B: one whole accelerator is
	// worth B credits. It equals the global denominator D (ResourceMaxUnits), so
	// the finest sliced unit (1/SlicedResourceMaxSize of an accelerator) maps to the
	// integer B/SlicedResourceMaxSize (=3125) and the ".sliced.units"→credits Kueue
	// factor is exactly B/D=1. Scoring credits as B×accelerator-fraction keeps every
	// per-mode value an integer, so Kueue's ResourceValue int64 quantization
	// (q.Value(), which ceils non-CPU resources) never rounds a fractional credit
	// up to 1 — the failure that broke the sliced borrow accounting.
	CreditsPerAccelerator = ResourceMaxUnits
)

// AcceleratorsToCredits scales a whole-accelerator count to its integer credit value
// (accelerators×B). It is used to build the Kueue ClusterQueue credits NominalQuota so
// the quota is expressed on the same integer basis as the transformed credit requests.
func AcceleratorsToCredits(accelerators resource.Quantity) resource.Quantity {
	accelerators.Mul(CreditsPerAccelerator)
	return accelerators
}

// CreditsToAccelerators converts a credit quantity back to accelerator units (credits÷B),
// preserving the fraction at micro scale so the exclusive whole-accelerator display and
// the sliced per-partition display (×partitions) stay exact. It first reads the
// credit count via Value() — the same int64 quantization Kueue's ResourceValue
// applies to non-CPU resources (Value() ceils) — so the operator's accelerator view
// always agrees with Kueue's accounting and a misconfigured fractional credit
// degrades to a safe integer rather than a misleading fraction.
func CreditsToAccelerators(credits resource.Quantity) resource.Quantity {
	credits.SetScaled(credits.Value()*1_000_000/CreditsPerAccelerator, resource.Micro)
	return credits
}
