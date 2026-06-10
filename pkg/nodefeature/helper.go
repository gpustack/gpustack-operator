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
	"gpustack.ai/gpustack/pkg/utils/typex"
)

const (
	FeatureLabelPrefix = "feature." + systemname.LabelPrefix
	CreditsLabelPrefix = "credits." + systemname.LabelPrefix
	DeviceLabelPrefix  = "device." + systemname.LabelPrefix
)

// ConstructNodeDeviceLabels constructs node feature labels from the given device group list.
func ConstructNodeDeviceLabels(groups device.DevicesGroupList) map[string]string {
	labels := map[string]string{}
	for i := range groups {
		applyDeviceLabels(labels, groups[i])
	}
	return labels
}

// applyDeviceLabels applies device feature labels of the given device group to the given labels map.
func applyDeviceLabels(labels map[string]string, group device.DevicesGroup) {
	if len(group.Accelerators) == 0 {
		return
	}

	// "${prefix}acceleratable=true"
	labels[FeatureLabelPrefix+"acceleratable"] = "true"

	manuKey := FeatureLabelPrefix + group.Manufacturer

	// "${prefix}${manufacturer}=true"
	labels[manuKey] = "true"
	// "${prefix}${manufacturer}.driver-version=${driverVersion}"
	if v := group.DriverVersion; v != "" {
		labels[manuKey+".driver-version"] = v
	}
	// "${prefix}${manufacturer}.runtime-version=${runtimeVersion}"
	if v := group.RuntimeVersion; v != "" {
		labels[manuKey+".runtime-version"] = v
	}

	nodeKey := manuKey + "-" + group.ID

	// "${prefix}${manufacturer}-${id}=true"
	labels[nodeKey] = "true"
	// "${prefix}${manufacturer}-${id}.product=${name}"
	labels[nodeKey+".product"] = group.Name
	// "${prefix}${manufacturer}-${id}.memory=${memory}"
	labels[nodeKey+".memory"] = quantityx.Format(resource.MustParse(strconvx.Itoa(group.Memory) + "Mi"))
	// "${prefix}${manufacturer}-${id}.cores=${cores}"
	labels[nodeKey+".cores"] = strconvx.Itoa(group.Cores)
	// "${prefix}${manufacturer}-${id}.accelerators=${accelerator}"
	labels[nodeKey+".accelerators"] = strconvx.Itoa(len(group.Accelerators))
	// "${prefix}${manufacturer}-${id}.family=${family}"
	if v := group.Family; v != "" {
		labels[nodeKey+".family"] = v
	}
	// "${prefix}${manufacturer}-${id}.compute-capability=${computeCapability}"
	if v := group.ComputeCapability; v != "" {
		labels[nodeKey+".compute-capability"] = v
	}

	// Match Kubernetes label values' requirements.
	for k := range labels {
		labels[k] = kubemeta.SanitizeLabelValue(labels[k])
	}
}

// ExtractNodeKeys returns accelerated node keys of the given Node.
func ExtractNodeKeys(node *core.Node) []string {
	return mapx.FilterSlice(node.Labels, func(k, v string) (string, bool) {
		if strings.HasPrefix(k, FeatureLabelPrefix) {
			if v == "true" {
				v = strings.TrimPrefix(k, FeatureLabelPrefix)
				if strings.Contains(v, "-") {
					return v, true
				}
			}
		}
		return "", false
	})
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

	generalKey := FeatureLabelPrefix + "general"

	{
		// "${prefix}general.cpu=${cpu}"
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

		// "${prefix}general.ram=${ram}"
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

		// "${prefix}general.local-storage=${stg}"
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
		// "${prefix}general.profile-flavor=${cpu}c-${ram}g-${stg}g"
		labels[generalKey+".profile-flavor"] = fmt.Sprintf("%dc-%dg-%dg", cpuC, generalRamC, stgC)

		// "${prefix}general.profile-queue=1c-${ramUnit}g"
		// "${prefix}general.profile-cohort=1c-${ramUnit}g"
		ramUnit := generalRamC / cpuC
		generalUnit := fmt.Sprintf("1c-%dg", ramUnit)
		labels[generalKey+".profile-queue"] = generalUnit
		labels[generalKey+".profile-cohort"] = generalUnit
	}

	for _, ndKey := range ExtractNodeKeys(node) {
		nodeKey := FeatureLabelPrefix + ndKey

		// "${prefix}${manufacturer}-${id}.accelerators=${accelerator}".
		accKey := nodeKey + ".accelerators"
		var accQ resource.Quantity
		if v := node.Labels[accKey]; v != "" {
			accQ = funcx.NoError(resource.ParseQuantity(v))
		}
		if accQ.Value() <= 0 {
			continue
		}
		accC := accQ.Value()

		// "${prefix}${manufacturer}-${id}.cpu=${cpu}"
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

		// "${prefix}${manufacturer}-${id}.ram=${ram}"
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

		// ${prefix}${manufacturer}-${id}.local-storage=${stg}
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

		// "${prefix}${manufacturer}-${id}.sliced.partitions=${slicedC}" is a
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

		// "${prefix}${manufacturer}-${id}.profile-flavor=${cpu}c-${ram}g-${stg}g-${acc}d[-${sliced}s]"
		profFlavor := fmt.Sprintf("%dc-%dg-%dg-%dd", cpuC, ramC, stgC, accC)
		if slicedC > 0 {
			profFlavor = fmt.Sprintf("%s-%ds", profFlavor, slicedC)
		}
		labels[nodeKey+".profile-flavor"] = profFlavor

		// "${prefix}${manufacturer}-${id}.profile-queue=${cpuUnit}c-${ramUnit}g-1d[-${sliced}s]"
		// "${prefix}${manufacturer}-${id}.profile-cohort=${cpuUnit}c-${ramUnit}g-1d"
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
	// Key is the node feature key of the node resource flavor,
	// which is in the format of "${manufacturer}-${id}" or "general".
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

	generalKey := FeatureLabelPrefix + "general"

	// Extract CPU node feature.
	{
		profFlavorKey := generalKey + ".profile-flavor"
		profQueueKey := generalKey + ".profile-queue"
		profCohortKey := generalKey + ".profile-cohort"
		cpuKey := generalKey + ".cpu"
		ramKey := generalKey + ".ram"
		stgKey := generalKey + ".local-storage"

		if kubemeta.HasLabels(node, profFlavorKey, profQueueKey, profCohortKey, cpuKey, ramKey, stgKey) {
			profFlavor := labels[profFlavorKey]
			profQueue := labels[profQueueKey]
			profCohort := labels[profCohortKey]
			cpu := labels[cpuKey]
			ram := labels[ramKey]
			stg := labels[stgKey]

			ndf := NodeResourceFlavor{
				Key:               "general",
				ProfileFlavorSpec: profFlavor,
				ProfileQueueSpec:  profQueue,
				ProfileCohortSpec: profCohort,
				NodeLabels: map[string]string{
					systemname.ManagedLabelKey: "true",
					profQueueKey:               profQueue,
				},
				Tolerations: []core.Toleration{
					{
						Operator: core.TolerationOpExists,
					},
				},
				CPU:          cpu,
				RAM:          ram,
				LocalStorage: stg,
			}

			ndfs = append(ndfs, ndf)
		}
	}

	for _, ndKey := range ExtractNodeKeys(node) {
		nodeKey := FeatureLabelPrefix + ndKey
		manufacturer, _, _ := strings.Cut(ndKey, "-")
		if !IsKnownManufacturer(manufacturer) {
			continue
		}

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

		acc := labels[accKey]
		cpu := labels[cpuKey]
		ram := labels[ramKey]
		stg := labels[stgKey]

		ndf := NodeResourceFlavor{
			Key:               ndKey,
			ProfileFlavorSpec: profFlavor,
			ProfileQueueSpec:  profQueue,
			ProfileCohortSpec: profCohort,
			NodeLabels: map[string]string{
				systemname.ManagedLabelKey: "true",
				profQueueKey:               profQueue,
			},
			Tolerations: []core.Toleration{
				{
					Operator: core.TolerationOpExists,
				},
			},
			Acceleratable:     true,
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
	// Shape: "gpustack-${node-key}-${cpu}c-${ram}g-${stg}g[-${acc}d][-${sliced}s]".
	Flavor string
	// Queue is the per-unit profile used to name the Kueue ClusterQueue.
	// Shape: "gpustack-${node-key}-${cpuUnit}c-${ramUnit}g[-${accUnit}d][-${sliced}s]".
	// Equals Cohort when sliced is unset.
	Queue string
	// Cohort is the per-unit profile used to name the Kueue Cohort.
	// Shape: "gpustack-${node-key}-${cpuUnit}c-${ramUnit}g[-${accUnit}d]".
	// Never carries a sliced suffix.
	Cohort string
}

// ExtractNodeProfiles extracts the node profiles from the given node.
func ExtractNodeProfiles(node *core.Node) (profiles []NodeProfile) {
	labels := node.Labels
	if len(labels) == 0 {
		return profiles
	}

	emit := func(key string) {
		nodeKey := FeatureLabelPrefix + key
		flavor := labels[nodeKey+".profile-flavor"]
		queue := labels[nodeKey+".profile-queue"]
		cohort := labels[nodeKey+".profile-cohort"]
		if flavor == "" || queue == "" || cohort == "" {
			return
		}
		profiles = append(profiles, NodeProfile{
			Flavor: FormatNodeProfile(key, flavor),
			Queue:  FormatNodeProfile(key, queue),
			Cohort: FormatNodeProfile(key, cohort),
		})
	}

	emit("general")

	for _, ndKey := range ExtractNodeKeys(node) {
		emit(ndKey)
	}

	return profiles
}

// _NodeProfilePrefix is the required leading prefix on every node profile
// emitted by FormatNodeProfile and consumed by ParseNodeProfile.
const _NodeProfilePrefix = "gpustack-"

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

// FormatNodeProfile formats the profile string with the given key and spec in the format of "gpustack-${key}-${spec}".
func FormatNodeProfile(key, spec string) string {
	return _NodeProfilePrefix + key + "-" + spec
}

// ParseNodeProfile parses a node profile string into its key and spec.
//
// The expected format is:
//
//	"gpustack-${key}-${cpu}c-${ram}g[-${stg}g][-${acc}d][-${sliced}s]"
//
// where the leading "gpustack-${key}-" is required, ${key} may itself
// contain dashes, ${cpu} and ${ram} are required, and ${stg}, ${acc},
// ${sliced} are optional. ${sliced} is only valid when ${acc} is also
// present. Each segment's numeric part must be a non-empty ASCII decimal.
//
// Examples:
//
//	"gpustack-general-16c-32g-88g"            -> key="general",    cpu=16, ram=32, stg=88
//	"gpustack-nvidia-t4-4c-16g-88g-1d"        -> key="nvidia-t4",  cpu=4,  ram=16, stg=88, acc=1
//	"gpustack-nvidia-t4-4c-16g-1d"            -> key="nvidia-t4",  cpu=4,  ram=16, acc=1
//	"gpustack-general-4c-16g"                 -> key="general",    cpu=4,  ram=16
//	"gpustack-nvidia-t4-4c-16g-88g-1d-8s"     -> key="nvidia-t4",  cpu=4,  ram=16, stg=88, acc=1, sliced=8
//	"gpustack-nvidia-t4-4c-16g-1d-8s"         -> key="nvidia-t4",  cpu=4,  ram=16, acc=1, sliced=8
//
// Returns ok=false (and zero-valued key/spec) when the prefix is missing,
// the key is empty, cpu or ram is missing or malformed, sliced is present
// without accelerator, or any numeric segment is invalid.
func ParseNodeProfile(profile string) (key string, spec NodeProfileSpec, ok bool) {
	rest, found := strings.CutPrefix(profile, _NodeProfilePrefix)
	if !found {
		return "", NodeProfileSpec{}, false
	}

	parts := strings.Split(rest, "-")
	idx := len(parts) - 1

	// Optional trailing sliced: "<digits>s".
	if idx >= 0 && strings.HasSuffix(parts[idx], "s") {
		v := strings.TrimSuffix(parts[idx], "s")
		if !isUnsignedDecimal(v) {
			return "", NodeProfileSpec{}, false
		}
		spec.SlicedAccelerator = v
		idx--
	}

	// Optional trailing accelerator: "<digits>d".
	if idx >= 0 && strings.HasSuffix(parts[idx], "d") {
		v := strings.TrimSuffix(parts[idx], "d")
		if !isUnsignedDecimal(v) {
			return "", NodeProfileSpec{}, false
		}
		spec.Accelerator = v
		idx--
	}

	// Sliced requires accelerator.
	if spec.SlicedAccelerator != "" && spec.Accelerator == "" {
		return "", NodeProfileSpec{}, false
	}

	// Optional trailing localStorage: "<digits>g". Distinguished from ram
	// only when the segment immediately before also ends with "g" (that
	// being ram).
	if idx >= 1 && strings.HasSuffix(parts[idx], "g") && strings.HasSuffix(parts[idx-1], "g") {
		v := strings.TrimSuffix(parts[idx], "g")
		if !isUnsignedDecimal(v) {
			return "", NodeProfileSpec{}, false
		}
		spec.LocalStorage = v
		idx--
	}

	// Required ram: "<digits>g".
	if idx < 0 || !strings.HasSuffix(parts[idx], "g") {
		return "", NodeProfileSpec{}, false
	}
	ramV := strings.TrimSuffix(parts[idx], "g")
	if !isUnsignedDecimal(ramV) {
		return "", NodeProfileSpec{}, false
	}
	spec.RAM = ramV
	idx--

	// Required cpu: "<digits>c".
	if idx < 0 || !strings.HasSuffix(parts[idx], "c") {
		return "", NodeProfileSpec{}, false
	}
	cpuV := strings.TrimSuffix(parts[idx], "c")
	if !isUnsignedDecimal(cpuV) {
		return "", NodeProfileSpec{}, false
	}
	spec.CPU = cpuV
	idx--

	// Required key: at least one leading segment, and not empty.
	if idx < 0 {
		return "", NodeProfileSpec{}, false
	}
	key = strings.Join(parts[:idx+1], "-")
	if key == "" {
		return "", NodeProfileSpec{}, false
	}

	return key, spec, true
}

// GetCreditsResourceName returns the resource name for the credits of the given manufacturer.
func GetCreditsResourceName(manufacturer string) core.ResourceName {
	return core.ResourceName(CreditsLabelPrefix + manufacturer)
}

// PowersOfTwoUpTo returns a slice of powers of two up to the given number n (inclusive).
func PowersOfTwoUpTo[I typex.Integer](n I) []I {
	if n < 1 {
		return []I{}
	}
	var result []I
	for val := I(1); val <= n; val <<= 1 {
		result = append(result, val)
	}
	return result
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
