package nodefeature

import (
	"fmt"
	"strings"

	core "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"

	"gpustack.ai/gpustack/pkg/device"
	"gpustack.ai/gpustack/pkg/kubemeta"
	"gpustack.ai/gpustack/pkg/systemname"
	"gpustack.ai/gpustack/pkg/utils/funcx"
	"gpustack.ai/gpustack/pkg/utils/mapx"
	"gpustack.ai/gpustack/pkg/utils/quantityx"
	"gpustack.ai/gpustack/pkg/utils/strconvx"
	"gpustack.ai/gpustack/pkg/utils/stringx"
)

const (
	// FeatureLabelPrefix prefixes the node feature label/annotation keys.
	FeatureLabelPrefix = "feature." + systemname.LabelPrefix
	// CreditsLabelPrefix prefixes the node feature credits label/annotation keys.
	CreditsLabelPrefix = "credits." + systemname.LabelPrefix
	// ScheduleLabelPrefix prefixes the schedule label/annatation keys.
	ScheduleLabelPrefix = "schedule." + systemname.LabelPrefix
)

const (
	// AcceleratableFeatureLabelPrefix prefixes the acceleratable(device) feature label keys,
	// e.g. "acceleratable.feature.gpustack.ai/nvidia-tesla-t4.product".
	AcceleratableFeatureLabelPrefix = "acceleratable." + FeatureLabelPrefix
)

// ConstructAcceleratableNodeLabels constructs accelerator feature labels from the given device group list.
func ConstructAcceleratableNodeLabels(groups device.DevicesGroupList) map[string]string {
	labels := map[string]string{}
	for i := range groups {
		applyAcceleratorLabels(labels, groups[i])
	}
	return labels
}

// applyAcceleratorLabels adds the accelerator feature labels of the given device group into labels.
func applyAcceleratorLabels(labels map[string]string, group device.DevicesGroup) {
	if len(group.Accelerators) == 0 {
		return
	}

	// "${prefix}acceleratable=true"
	labels[FeatureLabelPrefix+"acceleratable"] = "true"

	manuKey := AcceleratableFeatureLabelPrefix + group.Manufacturer

	// "acceleratable.${prefix}${manufacturer}=true"
	labels[manuKey] = "true"
	// "acceleratable.${prefix}${manufacturer}.driver-version=${driverVersion}"
	if v := group.DriverVersion; v != "" {
		labels[manuKey+".driver-version"] = v
	}
	// "acceleratable.${prefix}${manufacturer}.runtime-version=${runtimeVersion}"
	if v := group.RuntimeVersion; v != "" {
		labels[manuKey+".runtime-version"] = v
	}

	nodeKey := manuKey + "-" + group.ID

	// "acceleratable.${prefix}${manufacturer}-${id}=true"
	labels[nodeKey] = "true"
	// "acceleratable.${prefix}${manufacturer}-${id}.product=${name}"
	labels[nodeKey+".product"] = group.Name
	// "acceleratable.${prefix}${manufacturer}-${id}.memory=${memory}"
	labels[nodeKey+".memory"] = quantityx.Format(resource.MustParse(strconvx.Itoa(group.Memory) + "Mi"))
	// "acceleratable.${prefix}${manufacturer}-${id}.cores=${cores}"
	labels[nodeKey+".cores"] = strconvx.Itoa(group.Cores)
	// "acceleratable.${prefix}${manufacturer}-${id}.count=${accelerator}"
	labels[nodeKey+".count"] = strconvx.Itoa(len(group.Accelerators))
	// "acceleratable.${prefix}${manufacturer}-${id}.family=${family}"
	if v := group.Family; v != "" {
		labels[nodeKey+".family"] = v
	}
	// "acceleratable.${prefix}${manufacturer}-${id}.comcap=${computeCapability}"
	if v := group.ComputeCapability; v != "" {
		labels[nodeKey+".comcap"] = v
	}

	// Match Kubernetes label values' requirements.
	for k := range labels {
		labels[k] = kubemeta.SanitizeLabelValue(labels[k])
	}
}

// ExtractAcceleratableNodeKeys returns the acceleratable node keys of the given Node,
// each in the format "${manufacturer}-${id}".
func ExtractAcceleratableNodeKeys(node *core.Node) []string {
	return mapx.FilterSlice(node.Labels, func(k, v string) (string, bool) {
		if strings.HasPrefix(k, AcceleratableFeatureLabelPrefix) {
			if v == "true" {
				v = strings.TrimPrefix(k, AcceleratableFeatureLabelPrefix)
				manufacturer, _, found := strings.Cut(v, "-")
				if found && IsKnownAcceleratableManufacturer(manufacturer) {
					return v, true
				}
			}
		}
		return "", false
	})
}

const (
	// GeneralFeatureLabelPrefix prefixes the general(CPU) feature label keys,
	// e.g. "general.feature.gpustack.ai/amd-25-1.cpu".
	GeneralFeatureLabelPrefix = "general." + FeatureLabelPrefix
)

// generalOSAbbrev maps data-center GOOS values to compact codes; values
// absent from the map pass through unchanged.
var generalOSAbbrev = map[string]string{
	"linux": "ln", "windows": "wn", "darwin": "dw", "freebsd": "fb",
	"openbsd": "ob", "netbsd": "nb", "dragonfly": "df", "solaris": "so",
	"illumos": "im", "aix": "ax", "zos": "zo",
}

// generalArchAbbrev maps 64-bit GOARCH values to compact codes; values absent
// from the map (32-bit, wasm, ...) pass through unchanged.
var generalArchAbbrev = map[string]string{
	"amd64": "x64", "amd64p32": "x64p", "arm64": "a64", "arm64be": "a64b",
	"ppc64": "p64", "ppc64le": "p64l", "mips64": "m64", "mips64le": "m64l",
	"mips64p32": "m64p", "mips64p32le": "m64pl", "riscv64": "r64",
	"s390x": "s64", "sparc64": "sp64", "loong64": "l64",
}

// abbreviate returns m[v] when present, otherwise v unchanged.
func abbreviate(m map[string]string, v string) string {
	if s, ok := m[v]; ok {
		return s
	}
	return v
}

const (
	_NFDCPUModelVendorIDLabelKey = "feature.node.kubernetes.io/cpu-model.vendor_id"
	_NFDCPUModelFamilyLabelKey   = "feature.node.kubernetes.io/cpu-model.family"
	_NFDCPUModelIDLabelKey       = "feature.node.kubernetes.io/cpu-model.id"
)

// ExtractGeneralNodeKey derives the general(CPU) node key of the given Node,
// in the format "${manufacturer}-${id}", e.g. "intel-xeon-platinum-8358-ln-x64".
// The id leads with the sanitized "feature.gpustack.ai/cpu-name" annotation when reported,
// or with the NFD cpu-model family and id labels otherwise, and trails with the
// node's os and arch labels abbreviated via generalOSAbbrev/generalArchAbbrev.
// It returns an empty string when neither the cpu-name annotation nor the
// cpu-model labels are usable.
func ExtractGeneralNodeKey(node *core.Node) string {
	var idSuffix string
	if os := node.Labels[core.LabelOSStable]; os != "" {
		idSuffix += "-" + abbreviate(generalOSAbbrev, os)
	}
	if arch := node.Labels[core.LabelArchStable]; arch != "" {
		idSuffix += "-" + abbreviate(generalArchAbbrev, arch)
	}

	manu := extractGeneralNodeKeyManufacturer(node)

	var idPrefix string
	if name := generalFeatureAnnotation(node, "name"); name != "" {
		// "${manufacturer}-${idPrefix}${idSuffix}"
		budget := 63 - len(manu) - 1 - len(idSuffix)
		idPrefix = device.NormalizeName(name, manu, budget, true)
	} else {
		family := node.Labels[_NFDCPUModelFamilyLabelKey]
		modelID := node.Labels[_NFDCPUModelIDLabelKey]
		if family != "" && modelID != "" {
			idPrefix = family + "-" + modelID
		}
	}
	if idPrefix == "" {
		return ""
	}

	return manu + "-" + idPrefix + idSuffix
}

// extractGeneralNodeKeyManufacturer returns the manufacturer part of the general(CPU) node key of the given Node,
// derived from the NFD cpu-model vendor_id label.
// It returns "generic" when the vendor_id is unknown or empty.
func extractGeneralNodeKeyManufacturer(node *core.Node) string {
	// feature.node.kubernetes.io/cpu-model.vendor_id is reported by
	// https://github.com/klauspost/cpuid,
	// it always to be a meaningful string if the CPU information is properly collected.
	manu := strings.ToLower(node.Labels[_NFDCPUModelVendorIDLabelKey])
	if manu == "vendorunknown" || manu == "" {
		manu = GeneralManufacturerGeneric
	}
	return manu
}

// generalFeatureAnnotation returns the "feature.gpustack.ai/cpu-${name}" annotation
// of the given Node, which is reported by the NFD NodeFeatureRule. NFD keeps the
// "@cpu.model.*" template reference as-is when the attribute is unresolved,
// so values leading with "@" are treated as empty.
func generalFeatureAnnotation(node *core.Node, name string) string {
	v := node.Annotations[FeatureLabelPrefix+"cpu-"+name]
	if strings.HasPrefix(v, "@") {
		return ""
	}
	return v
}

type (
	ConstructNodeCapacityLabelsOptions struct {
		// GeneralRAMGiPerCPU overrides the default RAM Gi per CPU used in the general view when constructing node capacity labels.
		GeneralRAMGiPerCPU int64
	}

	ConstructNodeCapacityLabelsOption func(*ConstructNodeCapacityLabelsOptions)
)

// OverrideGeneralRAMGiPerCPU overrides the default RAM Gi per CPU used in the general view when constructing node capacity labels.
// By default, RAM Gi per CPU is discovered from node capacity and may be overridden by user-supplied labels;
// this option allows an additional override that takes precedence at first discovery.
func OverrideGeneralRAMGiPerCPU(v int64) ConstructNodeCapacityLabelsOption {
	return func(opts *ConstructNodeCapacityLabelsOptions) {
		opts.GeneralRAMGiPerCPU = v
	}
}

// ConstructNodeCapacityLabels constructs node capacity labels from the given node status and existing labels.
func ConstructNodeCapacityLabels(node *core.Node, opt ...ConstructNodeCapacityLabelsOption) map[string]string {
	var opts ConstructNodeCapacityLabelsOptions
	for i := range opt {
		opt[i](&opts)
	}

	labels := map[string]string{
		systemname.ManagedLabelKey: "true",
	}
	if node.Labels != nil && node.Labels[systemname.ManagedLabelKey] != "" {
		labels[systemname.ManagedLabelKey] = node.Labels[systemname.ManagedLabelKey]
	}

	if gKey := ExtractGeneralNodeKey(node); gKey != "" {
		generalKey := GeneralFeatureLabelPrefix + gKey
		gManu, _, _ := strings.Cut(gKey, "-")

		// "general.${prefix}${manufacturer}=true"
		labels[GeneralFeatureLabelPrefix+gManu] = "true"
		// "general.${prefix}${manufacturer}-${id}=true"
		labels[generalKey] = "true"

		// "general.${prefix}${manufacturer}-${id}.cpu=${cpu}"
		cpuKey := generalKey + ".cpu"
		var cpuQ resource.Quantity
		if v := node.Labels[cpuKey]; v != "" {
			cpuQ = funcx.NoError(resource.ParseQuantity(v))
		}
		if cpuQ.Value() <= 0 {
			cpuQ = node.Status.Capacity[core.ResourceCPU]
		}
		cpuC := cpuQ.Value()
		if cpuC <= 0 {
			cpuC = 1
		}
		labels[cpuKey] = strconvx.Itoa(cpuC)

		// "general.${prefix}${manufacturer}-${id}.ram=${ram}"
		ramKey := generalKey + ".ram"
		var ramQ resource.Quantity
		if v := node.Labels[ramKey]; v != "" {
			ramQ = funcx.NoError(resource.ParseQuantity(v))
		}
		if ramQ.Value() <= 0 {
			ramQ = node.Status.Capacity[core.ResourceMemory]
		}
		ramC := ramQ.Value() / quantityx.Gi
		if ramC&1 != 0 {
			ramC += 1
		}
		if ramC <= cpuC {
			ramC = cpuC
		}
		generalRamC := ramC
		if node.Labels[ramKey] == "" && opts.GeneralRAMGiPerCPU > 0 {
			generalRamC = opts.GeneralRAMGiPerCPU * cpuC
		}
		labels[ramKey] = strconvx.Itoa(generalRamC) + "Gi"

		// "general.${prefix}${manufacturer}-${id}.storage=${stg}"
		stgKey := generalKey + ".storage"
		var stgQ resource.Quantity
		if v := node.Labels[stgKey]; v != "" {
			stgQ = funcx.NoError(resource.ParseQuantity(v))
		}
		if stgQ.Value() <= 0 {
			stgQ = node.Status.Capacity[core.ResourceEphemeralStorage]
		}
		stgC := stgQ.Value() / quantityx.Gi
		if stgC&1 != 0 {
			stgC -= 1
		}
		if stgC <= 0 {
			stgC = 15 * cpuC
		}
		labels[stgKey] = strconvx.Itoa(stgC) + "Gi"

		// General has no sliced concept, so z-queue and z-cohort
		// always carry the same per-unit value.
		//
		// "general.${prefix}${manufacturer}-${id}.z-flavor=${cpu}c-${ram}g-${stg}g"
		labels[generalKey+".z-flavor"] = fmt.Sprintf("%dc-%dg-%dg", cpuC, generalRamC, stgC)

		// "general.${prefix}${manufacturer}-${id}.z-queue=1c-${ramUnit}g"
		// "general.${prefix}${manufacturer}-${id}.z-cohort=1c-${ramUnit}g"
		ramUnit := generalRamC / cpuC
		generalUnit := fmt.Sprintf("1c-%dg", ramUnit)
		labels[generalKey+".z-queue"] = generalUnit
		labels[generalKey+".z-cohort"] = generalUnit
	}

	for _, ndKey := range ExtractAcceleratableNodeKeys(node) {
		nodeKey := AcceleratableFeatureLabelPrefix + ndKey

		// "acceleratable.${prefix}${manufacturer}-${id}.count=${accelerator}".
		accKey := nodeKey + ".count"
		var accQ resource.Quantity
		if v := node.Labels[accKey]; v != "" {
			accQ = funcx.NoError(resource.ParseQuantity(v))
		}
		if accQ.Value() <= 0 {
			continue
		}
		accC := accQ.Value()

		// "acceleratable.${prefix}${manufacturer}-${id}.cpu=${cpu}"
		cpuKey := nodeKey + ".cpu"
		var cpuQ resource.Quantity
		if v := node.Labels[cpuKey]; v != "" {
			cpuQ = funcx.NoError(resource.ParseQuantity(v))
		}
		if cpuQ.Value() <= 0 {
			cpuQ = node.Status.Capacity[core.ResourceCPU]
		}
		cpuC := cpuQ.Value()
		if cpuC < accC {
			cpuC = accC
		}
		labels[cpuKey] = strconvx.Itoa(cpuC)

		// "acceleratable.${prefix}${manufacturer}-${id}.ram=${ram}"
		ramKey := nodeKey + ".ram"
		var ramQ resource.Quantity
		if v := node.Labels[ramKey]; v != "" {
			ramQ = funcx.NoError(resource.ParseQuantity(v))
		}
		if ramQ.Value() <= 0 {
			ramQ = node.Status.Capacity[core.ResourceMemory]
		}
		ramC := ramQ.Value() / quantityx.Gi
		if ramC&1 != 0 {
			ramC += 1
		}
		if ramC < accC {
			ramC = accC
		}
		labels[ramKey] = strconvx.Itoa(ramC) + "Gi"

		// "acceleratable.${prefix}${manufacturer}-${id}.storage=${stg}"
		stgKey := nodeKey + ".storage"
		var stgQ resource.Quantity
		if v := node.Labels[stgKey]; v != "" {
			stgQ = funcx.NoError(resource.ParseQuantity(v))
		}
		if stgQ.Value() <= 0 {
			stgQ = node.Status.Capacity[core.ResourceEphemeralStorage]
		}
		stgC := stgQ.Value() / quantityx.Gi
		if stgC&1 != 0 {
			stgC -= 1
		}
		if stgC <= 0 {
			stgC = 15 * accC
		}
		labels[stgKey] = strconvx.Itoa(stgC) + "Gi"

		// "acceleratable.${prefix}${manufacturer}-${id}.sliced.partitions=${slicedC}" is a
		// user-supplied input: when present and positive it appends
		// "-${slicedC}s" to z-flavor and z-queue. z-cohort
		// is the cohort-level per-unit view and never carries a sliced
		// suffix — it is what cohort matching compares on.
		var slicedC int64
		if v := node.Labels[nodeKey+".sliced.partitions"]; v != "" {
			if n, err := strconvx.Atoi[int64](v); err == nil && n > 0 {
				slicedC = n
			}
		}

		// "acceleratable.${prefix}${manufacturer}-${id}.z-flavor=${cpu}c-${ram}g-${stg}g-${acc}d[-${sliced}s]"
		profFlavor := fmt.Sprintf("%dc-%dg-%dg-%dd", cpuC, ramC, stgC, accC)
		if slicedC > 0 {
			profFlavor = fmt.Sprintf("%s-%ds", profFlavor, slicedC)
		}
		labels[nodeKey+".z-flavor"] = profFlavor

		// "acceleratable.${prefix}${manufacturer}-${id}.z-queue=${cpuUnit}c-${ramUnit}g-1d[-${sliced}s]"
		// "acceleratable.${prefix}${manufacturer}-${id}.z-cohort=${cpuUnit}c-${ramUnit}g-1d"
		cpuUnit := cpuC / accC
		ramUnit := ramC / accC
		profCohort := fmt.Sprintf("%dc-%dg-1d", cpuUnit, ramUnit)
		profQueue := profCohort
		if slicedC > 0 {
			profQueue = fmt.Sprintf("%s-%ds", profCohort, slicedC)
		}
		labels[nodeKey+".z-queue"] = profQueue
		labels[nodeKey+".z-cohort"] = profCohort
	}

	return labels
}

// NodeResourceFlavor represents the node resource flavor extracted from the labels of a node.
type NodeResourceFlavor struct {
	// ProfileCohort is the name of the Kueue Cohort that the flavor's queue belongs to.
	// Shape: "gpustack--${general-key}-${cpuUnit}c-${ramUnit}g[--${acc-key}-${accUnit}d]",
	// never carries a sliced suffix — it is the matching key at the cohort level.
	ProfileCohort string
	// ProfileQueue is the name of the Kueue ClusterQueue that the flavor belongs to.
	// Shape: "gpustack--${general-key}-${cpuUnit}c-${ramUnit}g[--${acc-key}-${accUnit}d[-${sliced}s]]",
	// identical to ProfileCohort when sliced is unset.
	ProfileQueue string
	// ProfileFlavor is the name of the Kueue ResourceFlavor.
	// Shape: "gpustack--${general-key}-${cpu}c-${ram}g-${stg}g[--${acc-key}-${acc}d[-${sliced}s]]".
	ProfileFlavor string
	// Manufacturer is the device manufacturer for the acceleratable flavor,
	// or the CPU manufacturer for the general(CPU-only) flavor.
	Manufacturer string
	// Acceleratable reports whether the flavor represents accelerated resources.
	Acceleratable bool
	// NodeLabels is the node labels for scheduling.
	NodeLabels map[string]string
	// Tolerations is the tolerations for scheduling.
	Tolerations []core.Toleration

	// Accelerator is the accelerator quantity of the node,
	// empty for the general(CPU-only) flavor.
	Accelerator string
	// CPU is the CPU quantity of the node.
	CPU string
	// RAM is the RAM quantity of the node.
	RAM string
	// LocalStorage is the local storage quantity of the node.
	LocalStorage string
}

// ExtractNodeResourceFlavors extracts the NodeResourceFlavor from given node.
func ExtractNodeResourceFlavors(node *core.Node) (ndfs []NodeResourceFlavor) {
	labels := node.Labels
	if labels == nil {
		return nil
	}

	gKey := ExtractGeneralNodeKey(node)

	// Extract the general(CPU) node feature.
	if gKey != "" {
		nodeKey := GeneralFeatureLabelPrefix + gKey

		profFlavorKey := nodeKey + ".z-flavor"
		profQueueKey := nodeKey + ".z-queue"
		profCohortKey := nodeKey + ".z-cohort"
		cpuKey := nodeKey + ".cpu"
		ramKey := nodeKey + ".ram"
		stgKey := nodeKey + ".storage"

		if kubemeta.HasLabels(node, profFlavorKey, profQueueKey, profCohortKey, cpuKey, ramKey, stgKey) {
			profQueue := labels[profQueueKey]
			manufacturer, _, _ := strings.Cut(gKey, "-")

			ndf := NodeResourceFlavor{
				ProfileCohort: FormatNodeProfile(gKey, "", labels[profCohortKey]),
				ProfileQueue:  FormatNodeProfile(gKey, "", profQueue),
				ProfileFlavor: FormatNodeProfile(gKey, "", labels[profFlavorKey]),
				Manufacturer:  manufacturer,
				NodeLabels: map[string]string{
					systemname.ManagedLabelKey: "true",
					profQueueKey:               profQueue,
				},
				Tolerations: []core.Toleration{
					{
						Operator: core.TolerationOpExists,
					},
				},
				CPU:          labels[cpuKey],
				RAM:          labels[ramKey],
				LocalStorage: labels[stgKey],
			}

			ndfs = append(ndfs, ndf)
		}
	}

	// Pair the acceleratable flavors with the general(CPU) key of the node.
	pinGeneral := gKey != ""
	if gKey == "" {
		gKey = GeneralManufacturerGeneric
	}

	for _, ndKey := range ExtractAcceleratableNodeKeys(node) {
		nodeKey := AcceleratableFeatureLabelPrefix + ndKey

		profFlavorKey := nodeKey + ".z-flavor"
		profQueueKey := nodeKey + ".z-queue"
		profCohortKey := nodeKey + ".z-cohort"
		accKey := nodeKey + ".count"
		cpuKey := nodeKey + ".cpu"
		ramKey := nodeKey + ".ram"
		stgKey := nodeKey + ".storage"

		if !kubemeta.HasLabels(node, profFlavorKey, profQueueKey, profCohortKey, accKey, cpuKey, ramKey, stgKey) {
			continue
		}

		profQueue := labels[profQueueKey]
		manufacturer, _, _ := strings.Cut(ndKey, "-")

		nodeLabels := map[string]string{
			systemname.ManagedLabelKey: "true",
			profQueueKey:               profQueue,
		}
		if pinGeneral {
			// Pin the general(CPU) identity so that the flavor never
			// matches a node with the same device but a different CPU.
			nodeLabels[GeneralFeatureLabelPrefix+gKey] = "true"
		}

		ndf := NodeResourceFlavor{
			ProfileCohort: FormatNodeProfile(gKey, ndKey, labels[profCohortKey]),
			ProfileQueue:  FormatNodeProfile(gKey, ndKey, profQueue),
			ProfileFlavor: FormatNodeProfile(gKey, ndKey, labels[profFlavorKey]),
			Manufacturer:  manufacturer,
			Acceleratable: true,
			NodeLabels:    nodeLabels,
			Tolerations: []core.Toleration{
				{
					Operator: core.TolerationOpExists,
				},
			},
			Accelerator:  labels[accKey],
			CPU:          labels[cpuKey],
			RAM:          labels[ramKey],
			LocalStorage: labels[stgKey],
		}

		ndfs = append(ndfs, ndf)
	}

	return ndfs
}

// NodeQueue represents the node queue extracted from the labels/annotations of a node.
type NodeQueue struct {
	// Product is the device product for the acceleratable queue,
	// or the CPU model name for the general(CPU-only) queue.
	Product string `json:"product,omitempty"`
	// Family is the device family for the acceleratable queue,
	// or the CPU model family for the general(CPU-only) queue.
	Family string `json:"family,omitempty"`
	// OS is the operating system of the node.
	OS string `json:"os,omitempty"`
	// Arch is the architecture of the node.
	Arch string `json:"arch,omitempty"`
	// NodeResourceFlavorCPU records the CPU details for the general(CPU-only) queue.
	NodeResourceFlavorCPU `json:",inline"`
	// NodeResourceFlavorAccelerator records the device details for the acceleratable queue.
	NodeResourceFlavorAccelerator `json:",inline"`
}

type (
	// NodeResourceFlavorCPU records the CPU details reported by
	// the "feature.gpustack.ai/cpu-*" annotations of a node.
	NodeResourceFlavorCPU struct {
		// PhysicalCores is the number of physical cores of the CPU.
		PhysicalCores string `json:"physicalCores,omitempty"`
		// ThreadsPerPhysicalCore is the number of threads per physical core of the CPU.
		ThreadsPerPhysicalCore string `json:"threadsPerPhysicalCore,omitempty"`
		// LogicalCores is the number of logical cores of the CPU.
		LogicalCores string `json:"logicalCores,omitempty"`
		// Stepping is the stepping of the CPU.
		Stepping string `json:"stepping,omitempty"`
		// ClockSpeed is the clock speed of the CPU in Hz.
		ClockSpeed string `json:"clockSpeed,omitempty"`
		// MaxClockSpeed is the max clock speed of the CPU in Hz.
		MaxClockSpeed string `json:"maxClockSpeed,omitempty"`
		// CacheLine is the cache line size of the CPU in bytes.
		CacheLine string `json:"cacheLine,omitempty"`
		// Cache records the CPU cache details of the CPU.
		Cache NodeResourceFlavorCPUCache `json:"cache,omitempty"`
	}

	// NodeResourceFlavorCPUCache records the CPU cache details reported by
	// the "feature.gpustack.ai/cpu-cache-*" annotations of a node.
	NodeResourceFlavorCPUCache struct {
		// L1I is the L1 instruction cache size of the CPU in bytes.
		L1I string `json:"l1i,omitempty"`
		// L1D is the L1 data cache size of the CPU in bytes.
		L1D string `json:"l1d,omitempty"`
		// L2 is the L2 cache size of the CPU in bytes.
		L2 string `json:"l2,omitempty"`
		// L3 is the L3 cache size of the CPU in bytes.
		L3 string `json:"l3,omitempty"`
	}
)

type (
	// NodeResourceFlavorAccelerator records the device details reported by
	// the acceleratable feature labels of a node, paired with the CPU details.
	NodeResourceFlavorAccelerator struct {
		// Memory is the VRAM size of the accelerator, e.g. "65535Mi".
		Memory string `json:"memory,omitempty"`
		// Cores is the number of cores of the accelerator, e.g. "128".
		Cores string `json:"cores,omitempty"`
		// ComputeCapability is the compute capability of the accelerator, e.g. "7.5".
		ComputeCapability string `json:"computeCapability,omitempty"`
		// CPU is the CPU details paired with the accelerator.
		CPU NodeResourceFlavorAcceleratorCPU `json:"cpu,omitempty"`
	}

	// NodeResourceFlavorAcceleratorCPU records the CPU details paired
	// with an acceleratable queue.
	NodeResourceFlavorAcceleratorCPU struct {
		// Manufacturer is the CPU manufacturer of the node.
		Manufacturer string `json:"manufacturer,omitempty"`
		// Product is the CPU model name of the node.
		Product string `json:"product,omitempty"`
		// Family is the CPU model family of the node.
		Family string `json:"family,omitempty"`
		// Detail inlines the CPU details of the node.
		NodeResourceFlavorCPU `json:",inline"`
	}
)

// extractGeneralDetail extracts the CPU details from the
// "feature.gpustack.ai/cpu-*" annotations of the given node.
func extractGeneralDetail(node *core.Node) NodeResourceFlavorCPU {
	return NodeResourceFlavorCPU{
		PhysicalCores:          generalFeatureAnnotation(node, "physical-cores"),
		ThreadsPerPhysicalCore: generalFeatureAnnotation(node, "threads-per-core"),
		LogicalCores:           generalFeatureAnnotation(node, "logical-cores"),
		Stepping:               generalFeatureAnnotation(node, "stepping"),
		ClockSpeed:             generalFeatureAnnotation(node, "hz"),
		MaxClockSpeed:          generalFeatureAnnotation(node, "boost-freq"),
		CacheLine:              generalFeatureAnnotation(node, "cache-line"),
		Cache: NodeResourceFlavorCPUCache{
			L1I: generalFeatureAnnotation(node, "cache-l1i"),
			L1D: generalFeatureAnnotation(node, "cache-l1d"),
			L2:  generalFeatureAnnotation(node, "cache-l2"),
			L3:  generalFeatureAnnotation(node, "cache-l3"),
		},
	}
}

// ExtractNodeQueue extracts the NodeQueue from the given node.
// If the acceleratableNodeKey is empty, it extracts the general(CPU-only) queue;
// otherwise, it extracts the acceleratable queue with the given key.
func ExtractNodeQueue(node *core.Node, acceleratableNodeKey string) (NodeQueue, bool) {
	nq := NodeQueue{
		OS:   node.Labels[core.LabelOSStable],
		Arch: node.Labels[core.LabelArchStable],
	}

	if acceleratableNodeKey == "" {
		nq.Product = generalFeatureAnnotation(node, "name")
		nq.Family = generalFeatureAnnotation(node, "family")
		nq.NodeResourceFlavorCPU = extractGeneralDetail(node)
		return nq, true
	}

	nodeKey := AcceleratableFeatureLabelPrefix + acceleratableNodeKey
	if node.Labels[nodeKey] != "true" {
		return NodeQueue{}, false
	}
	nq.Product = node.Labels[nodeKey+".product"]
	nq.Family = node.Labels[nodeKey+".family"]
	nq.NodeResourceFlavorAccelerator = NodeResourceFlavorAccelerator{
		Memory:            node.Labels[nodeKey+".memory"],
		Cores:             node.Labels[nodeKey+".cores"],
		ComputeCapability: node.Labels[nodeKey+".comcap"],
		CPU: NodeResourceFlavorAcceleratorCPU{
			Manufacturer:          extractGeneralNodeKeyManufacturer(node),
			Product:               generalFeatureAnnotation(node, "name"),
			Family:                generalFeatureAnnotation(node, "family"),
			NodeResourceFlavorCPU: extractGeneralDetail(node),
		},
	}
	return nq, true
}

// NodeProfile represents the node profile extracted from the node feature labels of a node.
type NodeProfile struct {
	// Flavor is the per-node profile used to name the Kueue ResourceFlavor.
	// Shape: "gpustack--${general-key}-${cpu}c-${ram}g-${stg}g[--${acc-key}-${acc}d[-${sliced}s]]".
	Flavor string
	// Queue is the per-unit profile used to name the Kueue ClusterQueue.
	// Shape: "gpustack--${general-key}-${cpuUnit}c-${ramUnit}g[--${acc-key}-${accUnit}d[-${sliced}s]]".
	// Equals Cohort when sliced is unset.
	Queue string
	// Cohort is the per-unit profile used to name the Kueue Cohort.
	// Shape: "gpustack--${general-key}-${cpuUnit}c-${ramUnit}g[--${acc-key}-${accUnit}d]".
	// Never carries a sliced suffix.
	Cohort string
}

// ExtractNodeProfiles extracts the node profiles from the given node.
func ExtractNodeProfiles(node *core.Node) (profiles []NodeProfile) {
	labels := node.Labels
	if len(labels) == 0 {
		return profiles
	}

	gKey := ExtractGeneralNodeKey(node)

	emit := func(generalKey, accKey string) {
		nodeKey := GeneralFeatureLabelPrefix + generalKey
		if accKey != "" {
			nodeKey = AcceleratableFeatureLabelPrefix + accKey
		}
		flavor := labels[nodeKey+".z-flavor"]
		queue := labels[nodeKey+".z-queue"]
		cohort := labels[nodeKey+".z-cohort"]
		if flavor == "" || queue == "" || cohort == "" {
			return
		}
		profiles = append(profiles, NodeProfile{
			Flavor: FormatNodeProfile(generalKey, accKey, flavor),
			Queue:  FormatNodeProfile(generalKey, accKey, queue),
			Cohort: FormatNodeProfile(generalKey, accKey, cohort),
		})
	}

	if gKey != "" {
		emit(gKey, "")
	} else {
		gKey = GeneralManufacturerGeneric
	}
	for _, ndKey := range ExtractAcceleratableNodeKeys(node) {
		emit(gKey, ndKey)
	}

	return profiles
}

const (
	// _NodeProfilePrefix is the required leading prefix on every node profile
	// emitted by FormatNodeProfile and consumed by ParseNodeProfile.
	_NodeProfilePrefix = "gpustack--"

	// _NodeProfileSegmentSeparator separates the general(CPU) segment from the
	// acceleratable(device) segment in a node profile. Segments never contain
	// consecutive dashes, so the separator is unambiguous.
	_NodeProfileSegmentSeparator = "--"
)

// NodeProfileSpec holds the resource segments parsed out of a node profile.
//
// CPU and RAM are always populated when ParseNodeProfile reports ok; the
// remaining fields are populated only when the corresponding segment is
// present.
type NodeProfileSpec struct {
	// CPU is the cpu count (segment "<digits>c"). Required.
	CPU string
	// RAM is the ram size in Gi (segment "<digits>g"). Required.
	RAM string
	// LocalStorage is the local storage size in Gi (trailing "<digits>g"
	// after RAM). Optional.
	LocalStorage string
	// Accelerator is the accelerator count (segment "<digits>d"). Optional.
	Accelerator string
	// SlicedAccelerator is the sliced count (segment "<digits>s"). Optional;
	// only valid when Accelerator is also present.
	SlicedAccelerator string
}

// FormatNodeProfile formats the profile string with the given general(CPU) key,
// acceleratable(device) key and spec.
//
// The general(CPU-only) profile, i.e. accKey is empty, is formatted as:
//
//	"gpustack--${generalKey}-${spec}"
//
// The acceleratable profile splits the spec at its "${acc}d" segment, keeping
// the host resources in the general segment and the device count(and the
// optional sliced suffix) in the acceleratable segment:
//
//	"gpustack--${generalKey}-${cpu}c-${ram}g[-${stg}g]--${accKey}-${acc}d[-${sliced}s]"
func FormatNodeProfile(generalKey, accKey, spec string) string {
	if accKey == "" {
		return _NodeProfilePrefix + generalKey + "-" + spec
	}
	hostSpec, devSpec := splitNodeProfileSpec(spec)
	if devSpec == "" {
		// No accelerator segment in the spec, fall back to the general format.
		return _NodeProfilePrefix + generalKey + "-" + spec
	}
	return _NodeProfilePrefix + generalKey + "-" + hostSpec +
		_NodeProfileSegmentSeparator + accKey + "-" + devSpec
}

// splitNodeProfileSpec splits "${cpu}c-${ram}g[-${stg}g]-${acc}d[-${sliced}s]"
// into the host part "${cpu}c-${ram}g[-${stg}g]" and the device part
// "${acc}d[-${sliced}s]". The device part is empty when the spec carries no
// accelerator segment.
func splitNodeProfileSpec(spec string) (host, dev string) {
	parts := strings.Split(spec, "-")
	for i := range parts {
		if strings.HasSuffix(parts[i], "d") && isUnsignedDecimal(strings.TrimSuffix(parts[i], "d")) {
			return strings.Join(parts[:i], "-"), strings.Join(parts[i:], "-")
		}
	}
	return spec, ""
}

// ParseNodeProfile parses a node profile string into its general(CPU) key,
// acceleratable(device) key and spec.
//
// The expected format is:
//
//	"gpustack--${generalKey}-${cpu}c-${ram}g[-${stg}g][--${accKey}-${acc}d[-${sliced}s]]"
//
// where the leading "gpustack--" is required, ${generalKey} and ${accKey} may
// themselves contain single dashes, ${cpu} and ${ram} are required, and
// ${stg}, the whole acceleratable segment, and ${sliced} are optional. Each
// segment's numeric part must be a non-empty ASCII decimal.
//
// Examples:
//
//	"gpustack--amd-25-1-16c-32g-88g"                          -> generalKey="amd-25-1", cpu=16, ram=32, stg=88
//	"gpustack--generic-4c-16g"                                -> generalKey="generic",  cpu=4,  ram=16
//	"gpustack--amd-25-1-4c-16g-88g--nvidia-t4-1d"             -> generalKey="amd-25-1", accKey="nvidia-t4", cpu=4, ram=16, stg=88, acc=1
//	"gpustack--amd-25-1-4c-16g--nvidia-t4-1d-8s"              -> generalKey="amd-25-1", accKey="nvidia-t4", cpu=4, ram=16, acc=1, sliced=8
//
// Returns ok=false (and zero-valued keys/spec) when the prefix is missing,
// either key is empty, cpu or ram is missing or malformed, the acceleratable
// segment misses the accelerator count, or any numeric segment is invalid.
func ParseNodeProfile(profile string) (generalKey, accKey string, spec NodeProfileSpec, ok bool) {
	rest, found := strings.CutPrefix(profile, _NodeProfilePrefix)
	if !found {
		return "", "", NodeProfileSpec{}, false
	}

	segs := strings.Split(rest, _NodeProfileSegmentSeparator)
	if len(segs) > 2 {
		return "", "", NodeProfileSpec{}, false
	}

	// General(CPU) segment: "${generalKey}-${cpu}c-${ram}g[-${stg}g]".
	{
		parts := strings.Split(segs[0], "-")
		idx := len(parts) - 1

		// Optional trailing localStorage: "<digits>g". Distinguished from ram
		// only when the segment immediately before also ends with "g" (that
		// being ram).
		if idx >= 1 && strings.HasSuffix(parts[idx], "g") && strings.HasSuffix(parts[idx-1], "g") {
			v := strings.TrimSuffix(parts[idx], "g")
			if !isUnsignedDecimal(v) {
				return "", "", NodeProfileSpec{}, false
			}
			spec.LocalStorage = v
			idx--
		}

		// Required ram: "<digits>g".
		if idx < 0 || !strings.HasSuffix(parts[idx], "g") {
			return "", "", NodeProfileSpec{}, false
		}
		ramV := strings.TrimSuffix(parts[idx], "g")
		if !isUnsignedDecimal(ramV) {
			return "", "", NodeProfileSpec{}, false
		}
		spec.RAM = ramV
		idx--

		// Required cpu: "<digits>c".
		if idx < 0 || !strings.HasSuffix(parts[idx], "c") {
			return "", "", NodeProfileSpec{}, false
		}
		cpuV := strings.TrimSuffix(parts[idx], "c")
		if !isUnsignedDecimal(cpuV) {
			return "", "", NodeProfileSpec{}, false
		}
		spec.CPU = cpuV
		idx--

		// Required general key: at least one leading segment, and not empty.
		if idx < 0 {
			return "", "", NodeProfileSpec{}, false
		}
		generalKey = strings.Join(parts[:idx+1], "-")
		if generalKey == "" {
			return "", "", NodeProfileSpec{}, false
		}
	}

	// Acceleratable(device) segment: "${accKey}-${acc}d[-${sliced}s]".
	if len(segs) == 2 {
		parts := strings.Split(segs[1], "-")
		idx := len(parts) - 1

		// Optional trailing sliced: "<digits>s".
		if idx >= 0 && strings.HasSuffix(parts[idx], "s") {
			v := strings.TrimSuffix(parts[idx], "s")
			if !isUnsignedDecimal(v) {
				return "", "", NodeProfileSpec{}, false
			}
			spec.SlicedAccelerator = v
			idx--
		}

		// Required accelerator: "<digits>d".
		if idx < 0 || !strings.HasSuffix(parts[idx], "d") {
			return "", "", NodeProfileSpec{}, false
		}
		accV := strings.TrimSuffix(parts[idx], "d")
		if !isUnsignedDecimal(accV) {
			return "", "", NodeProfileSpec{}, false
		}
		spec.Accelerator = accV
		idx--

		// Required acceleratable key: at least one leading segment, and not empty.
		if idx < 0 {
			return "", "", NodeProfileSpec{}, false
		}
		accKey = strings.Join(parts[:idx+1], "-")
		if accKey == "" {
			return "", "", NodeProfileSpec{}, false
		}
	}

	return generalKey, accKey, spec, true
}

// isUnsignedDecimal reports whether s is a non-empty string of ASCII digits.
func isUnsignedDecimal(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

// FormatLocalQueueName formats the LocalQueue name for the given ClusterQueue name.
//
// ClusterQueue names may exceed the 63-character label value limit that the
// "kueue.x-k8s.io/queue-name" label is subject to, so the LocalQueue is named
// by the FNV-64a hash of the ClusterQueue name instead:
//
//	"gpustack-fnv64-${fnv64a-hex}"
//
// which is always 31 characters long.
func FormatLocalQueueName(clusterQueueName string) string {
	return "gpustack-fnv64-" + stringx.SumByFNV64a(clusterQueueName)
}

// GetAcceleratableCreditsResourceName returns the accelerator credits resource name for the given manufacturer.
func GetAcceleratableCreditsResourceName(manufacturer string) core.ResourceName {
	return core.ResourceName(CreditsLabelPrefix + manufacturer)
}
