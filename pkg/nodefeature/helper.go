package nodefeature

import (
	"fmt"
	"sort"
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
	// "acceleratable.${prefix}${manufacturer}-${id}.accelerators=${accelerator}"
	labels[nodeKey+".accelerators"] = strconvx.Itoa(len(group.Accelerators))
	// "acceleratable.${prefix}${manufacturer}-${id}.family=${family}"
	if v := group.Family; v != "" {
		labels[nodeKey+".family"] = v
	}
	// "acceleratable.${prefix}${manufacturer}-${id}.compute-capability=${computeCapability}"
	if v := group.ComputeCapability; v != "" {
		labels[nodeKey+".compute-capability"] = v
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

	// NFDCPUModelLabelPrefix prefixes the cpu-model labels produced by the NFD cpu source.
	NFDCPUModelLabelPrefix = "feature.node.kubernetes.io/cpu-model."
)

// ExtractGeneralNodeKey derives the general(CPU) node key of the given Node
// from the NFD cpu-model labels, in the format "${manufacturer}-${family}-${id}",
// e.g. "amd-25-1". It returns GeneralManufacturerGeneric when the cpu-model
// labels are absent or unrecognizable.
func ExtractGeneralNodeKey(node *core.Node) string {
	manu := NormalizeGeneralManufacturer(node.Labels[NFDCPUModelLabelPrefix+"vendor_id"])
	if manu == GeneralManufacturerGeneric {
		return GeneralManufacturerGeneric
	}
	family := node.Labels[NFDCPUModelLabelPrefix+"family"]
	id := node.Labels[NFDCPUModelLabelPrefix+"id"]
	if family == "" || id == "" {
		return GeneralManufacturerGeneric
	}
	return manu + "-" + family + "-" + id
}

// ExtractGeneralNodeKeys returns the general(CPU) node keys recorded on the given Node
// by ConstructNodeCapacityLabels, each in the format "${manufacturer}-${family}-${id}"
// or "generic", sorted for stability.
func ExtractGeneralNodeKeys(node *core.Node) []string {
	keys := mapx.FilterSlice(node.Labels, func(k, v string) (string, bool) {
		if rest, found := strings.CutPrefix(k, GeneralFeatureLabelPrefix); found {
			if key, found := strings.CutSuffix(rest, ".profile-flavor"); found && key != "" {
				return key, true
			}
		}
		return "", false
	})
	sort.Strings(keys)
	return keys
}

// splitGeneralNodeKey splits the general(CPU) node key into its manufacturer,
// family and id segments. Family and id are empty for the generic key.
func splitGeneralNodeKey(key string) (manufacturer, family, id string) {
	parts := strings.SplitN(key, "-", 3)
	manufacturer = parts[0]
	if len(parts) > 1 {
		family = parts[1]
	}
	if len(parts) > 2 {
		id = parts[2]
	}
	return manufacturer, family, id
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

	gKey := ExtractGeneralNodeKey(node)
	generalKey := GeneralFeatureLabelPrefix + gKey

	{
		gManu, gFamily, _ := splitGeneralNodeKey(gKey)

		// "general.${prefix}${manufacturer}=true"
		labels[GeneralFeatureLabelPrefix+gManu] = "true"
		// "general.${prefix}${manufacturer}-${family}-${id}=true"
		labels[generalKey] = "true"
		// "general.${prefix}${manufacturer}-${family}-${id}.family=${family}"
		if gFamily != "" {
			labels[generalKey+".family"] = gFamily
		}

		// "general.${prefix}${manufacturer}-${family}-${id}.cpu=${cpu}"
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

		// "general.${prefix}${manufacturer}-${family}-${id}.ram=${ram}"
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

		// "general.${prefix}${manufacturer}-${family}-${id}.local-storage=${stg}"
		stgKey := generalKey + ".local-storage"
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

		// General has no sliced concept, so profile-queue and profile-cohort
		// always carry the same per-unit value.
		//
		// "general.${prefix}${manufacturer}-${family}-${id}.profile-flavor=${cpu}c-${ram}g-${stg}g"
		labels[generalKey+".profile-flavor"] = fmt.Sprintf("%dc-%dg-%dg", cpuC, generalRamC, stgC)

		// "general.${prefix}${manufacturer}-${family}-${id}.profile-queue=1c-${ramUnit}g"
		// "general.${prefix}${manufacturer}-${family}-${id}.profile-cohort=1c-${ramUnit}g"
		ramUnit := generalRamC / cpuC
		generalUnit := fmt.Sprintf("1c-%dg", ramUnit)
		labels[generalKey+".profile-queue"] = generalUnit
		labels[generalKey+".profile-cohort"] = generalUnit
	}

	for _, ndKey := range ExtractAcceleratableNodeKeys(node) {
		nodeKey := AcceleratableFeatureLabelPrefix + ndKey

		// "acceleratable.${prefix}${manufacturer}-${id}.accelerators=${accelerator}".
		accKey := nodeKey + ".accelerators"
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

		// "acceleratable.${prefix}${manufacturer}-${id}.local-storage=${stg}"
		stgKey := nodeKey + ".local-storage"
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
		// "-${slicedC}s" to profile-flavor and profile-queue. profile-cohort
		// is the cohort-level per-unit view and never carries a sliced
		// suffix — it is what cohort matching compares on.
		var slicedC int64
		if v := node.Labels[nodeKey+".sliced.partitions"]; v != "" {
			if n, err := strconvx.Atoi[int64](v); err == nil && n > 0 {
				slicedC = n
			}
		}

		// "acceleratable.${prefix}${manufacturer}-${id}.profile-flavor=${cpu}c-${ram}g-${stg}g-${acc}d[-${sliced}s]"
		profFlavor := fmt.Sprintf("%dc-%dg-%dg-%dd", cpuC, ramC, stgC, accC)
		if slicedC > 0 {
			profFlavor = fmt.Sprintf("%s-%ds", profFlavor, slicedC)
		}
		labels[nodeKey+".profile-flavor"] = profFlavor

		// "acceleratable.${prefix}${manufacturer}-${id}.profile-queue=${cpuUnit}c-${ramUnit}g-1d[-${sliced}s]"
		// "acceleratable.${prefix}${manufacturer}-${id}.profile-cohort=${cpuUnit}c-${ramUnit}g-1d"
		cpuUnit := cpuC / accC
		ramUnit := ramC / accC
		profCohort := fmt.Sprintf("%dc-%dg-1d", cpuUnit, ramUnit)
		profQueue := profCohort
		if slicedC > 0 {
			profQueue = fmt.Sprintf("%s-%ds", profCohort, slicedC)
		}
		labels[nodeKey+".profile-queue"] = profQueue
		labels[nodeKey+".profile-cohort"] = profCohort
	}

	return labels
}

// NodeResourceFlavor represents the node resource flavor extracted from the node feature labels of a node.
type NodeResourceFlavor struct {
	// GeneralKey is the general(CPU) node key of the node resource flavor,
	// which is in the format of "${manufacturer}-${family}-${id}" or "generic".
	GeneralKey string
	// Key is the acceleratable node feature key of the node resource flavor,
	// which is in the format of "${manufacturer}-${id}",
	// or empty for the general(CPU-only) flavor.
	Key string
	// ProfileFlavorSpec is the per-node resource flavor spec used to name the
	// ResourceFlavor object. Shape:
	//   "${cpu}c-${ram}g-${stg}g[-${acc}d][-${sliced}s]"
	ProfileFlavorSpec string
	// ProfileQueueSpec is the per-unit profile spec used to name the
	// ClusterQueue. Shape:
	//   "${cpuUnit}c-${ramUnit}g[-${accUnit}d][-${sliced}s]"
	// When sliced is unset, ProfileQueueSpec is identical to ProfileCohortSpec.
	ProfileQueueSpec string
	// ProfileCohortSpec is the per-unit profile spec used to name the Cohort.
	// Shape:
	//   "${cpuUnit}c-${ramUnit}g[-${accUnit}d]"
	// ProfileCohortSpec never carries a sliced suffix — it is the matching
	// key at the cohort level.
	ProfileCohortSpec string
	// NodeLabels is the node labels for scheduling.
	NodeLabels map[string]string
	// Tolerations is the tolerations for scheduling.
	Tolerations []core.Toleration
	// Acceleratable reports whether the flavor represents accelerated resources.
	Acceleratable bool
	// CPUManufacturer is the CPU manufacturer segment of GeneralKey.
	CPUManufacturer string
	// CPUFamily is the CPU family segment of GeneralKey, empty for the generic key.
	CPUFamily string
	// CPUID is the CPU model id segment of GeneralKey, empty for the generic key.
	CPUID string
	// Manufacturer is the device manufacturer of the node.
	Manufacturer string
	// Product is the device product of the node.
	Product string
	// Memory is the device memory size in MiB of the node.
	Memory string
	// Cores is the devices cores of the node.
	Cores string
	// Family is the device family of the node.
	Family string
	// ComputeCapability is the device compute capability of the node.
	ComputeCapability string
	// Accelerator is the accelerator quantity of the node.
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

	gKeys := ExtractGeneralNodeKeys(node)

	// Extract general(CPU) node features.
	for _, gKey := range gKeys {
		nodeKey := GeneralFeatureLabelPrefix + gKey

		profFlavorKey := nodeKey + ".profile-flavor"
		profQueueKey := nodeKey + ".profile-queue"
		profCohortKey := nodeKey + ".profile-cohort"
		cpuKey := nodeKey + ".cpu"
		ramKey := nodeKey + ".ram"
		stgKey := nodeKey + ".local-storage"

		if !kubemeta.HasLabels(node, profFlavorKey, profQueueKey, profCohortKey, cpuKey, ramKey, stgKey) {
			continue
		}

		profQueue := labels[profQueueKey]
		cpuManu, cpuFamily, cpuID := splitGeneralNodeKey(gKey)

		ndf := NodeResourceFlavor{
			GeneralKey:        gKey,
			ProfileFlavorSpec: labels[profFlavorKey],
			ProfileQueueSpec:  profQueue,
			ProfileCohortSpec: labels[profCohortKey],
			NodeLabels: map[string]string{
				systemname.ManagedLabelKey: "true",
				profQueueKey:               profQueue,
			},
			Tolerations: []core.Toleration{
				{
					Operator: core.TolerationOpExists,
				},
			},
			CPUManufacturer: cpuManu,
			CPUFamily:       cpuFamily,
			CPUID:           cpuID,
			CPU:             labels[cpuKey],
			RAM:             labels[ramKey],
			LocalStorage:    labels[stgKey],
		}

		ndfs = append(ndfs, ndf)
	}

	// Pair the acceleratable flavors with the general(CPU) key of the node.
	gKey := GeneralManufacturerGeneric
	if len(gKeys) > 0 {
		gKey = gKeys[0]
	}
	cpuManu, cpuFamily, cpuID := splitGeneralNodeKey(gKey)

	for _, ndKey := range ExtractAcceleratableNodeKeys(node) {
		nodeKey := AcceleratableFeatureLabelPrefix + ndKey

		profFlavorKey := nodeKey + ".profile-flavor"
		profQueueKey := nodeKey + ".profile-queue"
		profCohortKey := nodeKey + ".profile-cohort"
		accKey := nodeKey + ".accelerators"
		cpuKey := nodeKey + ".cpu"
		ramKey := nodeKey + ".ram"
		stgKey := nodeKey + ".local-storage"

		if !kubemeta.HasLabels(node, profFlavorKey, profQueueKey, profCohortKey, accKey, cpuKey, ramKey, stgKey) {
			continue
		}

		profFlavor := labels[profFlavorKey]
		profQueue := labels[profQueueKey]
		profCohort := labels[profCohortKey]
		product := labels[nodeKey+".product"]
		memory := labels[nodeKey+".memory"]
		cores := labels[nodeKey+".cores"]
		family := labels[nodeKey+".family"]
		computeCapability := labels[nodeKey+".compute-capability"]
		manufacturer, _, _ := strings.Cut(ndKey, "-")

		acc := labels[accKey]
		cpu := labels[cpuKey]
		ram := labels[ramKey]
		stg := labels[stgKey]

		ndf := NodeResourceFlavor{
			GeneralKey:        gKey,
			Key:               ndKey,
			ProfileFlavorSpec: profFlavor,
			ProfileQueueSpec:  profQueue,
			ProfileCohortSpec: profCohort,
			NodeLabels: map[string]string{
				systemname.ManagedLabelKey: "true",
				// Pin the general(CPU) identity so that the flavor never
				// matches a node with the same device but a different CPU.
				GeneralFeatureLabelPrefix + gKey: "true",
				profQueueKey:                     profQueue,
			},
			Tolerations: []core.Toleration{
				{
					Operator: core.TolerationOpExists,
				},
			},
			Acceleratable:     true,
			CPUManufacturer:   cpuManu,
			CPUFamily:         cpuFamily,
			CPUID:             cpuID,
			Manufacturer:      manufacturer,
			Product:           product,
			Memory:            memory,
			Cores:             cores,
			Family:            family,
			ComputeCapability: computeCapability,
			Accelerator:       acc,
			CPU:               cpu,
			RAM:               ram,
			LocalStorage:      stg,
		}

		ndfs = append(ndfs, ndf)
	}

	return ndfs
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

	gKeys := ExtractGeneralNodeKeys(node)

	emit := func(generalKey, accKey string) {
		nodeKey := GeneralFeatureLabelPrefix + generalKey
		if accKey != "" {
			nodeKey = AcceleratableFeatureLabelPrefix + accKey
		}
		flavor := labels[nodeKey+".profile-flavor"]
		queue := labels[nodeKey+".profile-queue"]
		cohort := labels[nodeKey+".profile-cohort"]
		if flavor == "" || queue == "" || cohort == "" {
			return
		}
		profiles = append(profiles, NodeProfile{
			Flavor: FormatNodeProfile(generalKey, accKey, flavor),
			Queue:  FormatNodeProfile(generalKey, accKey, queue),
			Cohort: FormatNodeProfile(generalKey, accKey, cohort),
		})
	}

	for _, gKey := range gKeys {
		emit(gKey, "")
	}

	gKey := GeneralManufacturerGeneric
	if len(gKeys) > 0 {
		gKey = gKeys[0]
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
