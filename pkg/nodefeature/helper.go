package nodefeature

import (
	"fmt"
	"strconv"
	"strings"

	core "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"

	"gpustack.ai/gpustack/pkg/device"
	"gpustack.ai/gpustack/pkg/kubemeta"
	"gpustack.ai/gpustack/pkg/systemname"
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
)

const (
	// AcceleratableFeatureLabelPrefix prefixes the acceleratable(device) feature label keys,
	// e.g. "acceleratable.feature.gpustack.ai/nvidia-tesla-t4.product".
	AcceleratableFeatureLabelPrefix = "acceleratable." + FeatureLabelPrefix

	// NodeAcceleratableLabelKey is the umbrella label set on a node that carries any
	// accelerator, e.g. "feature.gpustack.ai/acceleratable=true". It is the cheap
	// "is this node accelerated?" check, set alongside the per-device keys.
	NodeAcceleratableLabelKey = FeatureLabelPrefix + "acceleratable"

	// SlicedPartitionsLabelSuffix is the suffix of the admin-authored slicing label
	// that enables slicing on an accelerator model, e.g.
	// "acceleratable.feature.gpustack.ai/nvidia-a10g.sliced.partitions". Its value is
	// the partition count N (a power of two validated by the admission webhook).
	SlicedPartitionsLabelSuffix = ".sliced.partitions"
)

// GetAcceleratableSlicedPartitions returns the validated slice partition count the
// admin enabled for accelerator model aKey on node — the value of the
// "<AcceleratableFeatureLabelPrefix><aKey><SlicedPartitionsLabelSuffix>" label — or 0
// when the label is absent, unparseable, or not a valid partition count.
func GetAcceleratableSlicedPartitions(node *core.Node, aKey string) int64 {
	v := node.Labels[AcceleratableFeatureLabelPrefix+aKey+SlicedPartitionsLabelSuffix]
	if v == "" {
		return 0
	}
	n, err := strconvx.Atoi[int64](v)
	if err != nil || !IsValidSlicedPartitions(n) {
		return 0
	}
	return n
}

// FilterAcceleratableSlicedPartitionsLabels returns the admin-authored slicing
// opt-in labels carried by labels — those matching
// "<AcceleratableFeatureLabelPrefix>...<SlicedPartitionsLabelSuffix>" — and drops
// every other entry. The result is nil when labels carries no such opt-in.
func FilterAcceleratableSlicedPartitionsLabels(labels map[string]string) map[string]string {
	return mapx.Filter(labels, func(k, _ string) bool {
		return strings.HasPrefix(k, AcceleratableFeatureLabelPrefix) &&
			strings.HasSuffix(k, SlicedPartitionsLabelSuffix)
	})
}

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
	labels[NodeAcceleratableLabelKey] = "true"

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

	// GeneralGroupGeneric is the sentinel general(CPU) group of a CPU-manufacturer-agnostic
	// pool. It contributes no general.* discriminator, so a collapsed (unaware) pool and an
	// aware pool whose CPU is left generic both aggregate every CPU of the os/arch.
	GeneralGroupGeneric = GeneralManufacturerGeneric
)

// PoolScheduleLabels builds the schedule discriminator labels for a pool from its identity and
// the CPU-manufacturer awareness setting. They are stamped on both the InstanceType and its
// backing ClusterQueue and drive the ResourceFlavor/Devices reverse-lookup:
//   - feature.gpustack.ai/acceleratable=<bool> is always present, separating generic from
//     accelerated pools so an aware generic pool never matches an accelerated flavor sharing
//     its CPU key;
//   - an accelerated pool adds acceleratable.feature.gpustack.ai/<acceleratorGroup>=true;
//   - the general.feature.gpustack.ai/<generalGroup>=true key is added only when aware and the
//     general group is a real CPU (not the "generic" sentinel), so an unaware pool collapses
//     every CPU and an aware pool splits by CPU.
//
// os/arch are included when non-empty.
func PoolScheduleLabels(acceleratable, aware bool, generalGroup, acceleratorGroup, os, arch string) map[string]string {
	lbs := map[string]string{
		NodeAcceleratableLabelKey: strconv.FormatBool(acceleratable),
	}
	if os != "" {
		lbs[core.LabelOSStable] = os
	}
	if arch != "" {
		lbs[core.LabelArchStable] = arch
	}
	if acceleratable && acceleratorGroup != "" {
		lbs[AcceleratableFeatureLabelPrefix+acceleratorGroup] = "true"
	}
	if aware && generalGroup != "" && generalGroup != GeneralGroupGeneric {
		lbs[GeneralFeatureLabelPrefix+generalGroup] = "true"
	}
	return lbs
}

// PoolFlavorSelector is the inverse of PoolScheduleLabels: it extracts the ResourceFlavor selector
// from a pool's labels (a ClusterQueue's or a ResourceFlavor's) — the generic-vs-accelerated
// boolean, any feature key, and kubernetes.io/os|arch. It returns nil when the labels carry no
// discriminator, so a caller never matches every object. The feature.gpustack.ai/acceleratable
// boolean is a sufficient discriminator on its own (a collapsed generic pool carries only it, no
// general.* key) and, kept in the selector, stops an aware generic pool (general.<gKey>=true) from
// matching an accelerated flavor that shares that CPU key (acceleratable=true).
func PoolFlavorSelector(labels map[string]string) map[string]string {
	lbs := make(map[string]string, 4)
	hasDiscriminator := false
	for k, v := range labels {
		switch {
		case k == core.LabelOSStable, k == core.LabelArchStable:
			lbs[k] = v
		case k == NodeAcceleratableLabelKey:
			lbs[k] = v
			hasDiscriminator = true
		case v == "true" &&
			(strings.HasPrefix(k, GeneralFeatureLabelPrefix) ||
				strings.HasPrefix(k, AcceleratableFeatureLabelPrefix)):
			lbs[k] = v
			hasDiscriminator = true
		}
	}
	if !hasDiscriminator {
		return nil
	}
	return lbs
}

const (
	_NFDCPUModelVendorIDLabelKey = "feature.node.kubernetes.io/cpu-model.vendor_id"
	_NFDCPUModelFamilyLabelKey   = "feature.node.kubernetes.io/cpu-model.family"
	_NFDCPUModelIDLabelKey       = "feature.node.kubernetes.io/cpu-model.id"
)

// ExtractGeneralNodeKey derives the general(CPU) node key of the given Node; it always
// returns a non-empty key blending the node's real CPU identity.
//
// os/arch are not part of the key: the ResourceFlavor/ClusterQueue name carries os/arch
// explicitly and spec.nodeLabels pins kubernetes.io/os|arch, so nodes of different ISAs
// cannot pool into one flavor/queue even when the key collides (the cpu-model family and
// id labels are independent numbering spaces on x86 versus arm64, so a small value such as
// "25-1" can legitimately appear on both).
//
// The manufacturer (the lowercased NFD cpu-model vendor_id, or "generic" when unknown)
// leads and a CPU identity is blended in after it: the sanitized
// "feature.gpustack.ai/cpu-name" annotation when reported (e.g. "amd-epyc-7763"), the NFD
// cpu-model family and id labels as the fallback when the annotation is absent (e.g.
// "amd-25-1"), or nothing when no CPU identity is usable (e.g. "amd", or "generic" when the
// vendor is unknown too — the graceful degradation when NFD is absent).
func ExtractGeneralNodeKey(node *core.Node) string {
	// The manufacturer part of the general node key is derived from the NFD cpu-model
	// vendor_id label.
	manu := extractGeneralNodeKeyManufacturer(node)

	// Blend the CPU name into the general node key for readability and differentiation. The
	// CPU name is reported by the NFD NodeFeatureRule with the "feature.gpustack.ai/cpu-name"
	// annotation; it is sanitized and truncated to fit the Kubernetes label value
	// requirements. When the annotation is unavailable, the NFD cpu-model family and id
	// labels are the fallback id prefix. If NFD is not deployed, or the CPU information is
	// not collected, the id prefix is empty and the key falls back to "${manufacturer}".
	var idPrefix string
	if name := generalFeatureAnnotation(node, "name"); name != "" {
		// "${manufacturer}-${idPrefix}", limit the key to 63 characters in total.
		budget := 63 - len(manu) - 1
		idPrefix = device.NormalizeName(name, manu, budget, true)
	} else {
		family := node.Labels[_NFDCPUModelFamilyLabelKey]
		modelID := node.Labels[_NFDCPUModelIDLabelKey]
		if family != "" && modelID != "" {
			idPrefix = family + "-" + modelID
		}
	}

	// "${manufacturer}"
	if idPrefix == "" {
		return manu
	}

	// "${manufacturer}-${idPrefix}"
	return manu + "-" + idPrefix
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

// CPUDetail is the raw CPU information read from a node's "feature.gpustack.ai/cpu-*"
// annotations (reported by the NFD NodeFeatureRule). It is recorded verbatim onto a
// ResourceFlavor's cpuDetail note; the InstanceType webhook folds it back into the
// InstanceType spec when CPU-manufacturer awareness is on.
type CPUDetail struct {
	Manufacturer           string
	Product                string
	Family                 string
	PhysicalCores          string
	ThreadsPerPhysicalCore string
	LogicalCores           string
	Stepping               string
	ClockSpeed             string
	MaxClockSpeed          string
	CacheLine              string
	CacheL1I               string
	CacheL1D               string
	CacheL2                string
	CacheL3                string
}

// ExtractGeneralDetail reads the node's CPU detail from its "feature.gpustack.ai/cpu-*"
// annotations. An unreported attribute — an unresolved NFD "@cpu.model.*" template or an
// absent annotation — is returned empty (see generalFeatureAnnotation).
func ExtractGeneralDetail(node *core.Node) CPUDetail {
	return CPUDetail{
		Manufacturer:           extractGeneralNodeKeyManufacturer(node),
		Product:                generalFeatureAnnotation(node, "name"),
		Family:                 generalFeatureAnnotation(node, "family"),
		PhysicalCores:          generalFeatureAnnotation(node, "physical-cores"),
		ThreadsPerPhysicalCore: generalFeatureAnnotation(node, "threads-per-core"),
		LogicalCores:           generalFeatureAnnotation(node, "logical-cores"),
		Stepping:               generalFeatureAnnotation(node, "stepping"),
		ClockSpeed:             generalFeatureAnnotation(node, "hz"),
		MaxClockSpeed:          generalFeatureAnnotation(node, "boost-freq"),
		CacheLine:              generalFeatureAnnotation(node, "cache-line"),
		CacheL1I:               generalFeatureAnnotation(node, "cache-l1i"),
		CacheL1D:               generalFeatureAnnotation(node, "cache-l1d"),
		CacheL2:                generalFeatureAnnotation(node, "cache-l2"),
		CacheL3:                generalFeatureAnnotation(node, "cache-l3"),
	}
}

type (
	ConstructNodeCapacityLabelsOptions struct {
		// ManualNodeManagement, when true, skips auto-injecting the managed label so
		// an administrator opts nodes in by hand (the node-management-manual setting).
		ManualNodeManagement bool
	}

	ConstructNodeCapacityLabelsOption func(*ConstructNodeCapacityLabelsOptions)
)

// WithManualNodeManagement skips auto-injecting the managed label when manual is
// true, so the operator does not auto-onboard nodes. The caller passes the
// node-management-manual setting value, read per-reconcile, so a runtime flip
// applies on the next reconcile without a restart.
func WithManualNodeManagement(manual bool) ConstructNodeCapacityLabelsOption {
	return func(opts *ConstructNodeCapacityLabelsOptions) {
		opts.ManualNodeManagement = manual
	}
}

// ConstructNodeCapacityLabels constructs node capacity labels from the given node status and existing labels.
func ConstructNodeCapacityLabels(node *core.Node, opt ...ConstructNodeCapacityLabelsOption) map[string]string {
	var opts ConstructNodeCapacityLabelsOptions
	for i := range opt {
		opt[i](&opts)
	}

	labels := map[string]string{}
	// Auto-inject the managed label unless node management is manual. An explicit
	// admin-set value always wins, so an admin can opt a node in or out by hand —
	// the only path to management when manual mode is on.
	if !opts.ManualNodeManagement {
		labels[systemname.ManagedLabelKey] = "true"
	}
	if node.Labels != nil && node.Labels[systemname.ManagedLabelKey] != "" {
		labels[systemname.ManagedLabelKey] = node.Labels[systemname.ManagedLabelKey]
	}

	// Record the general(CPU) key presence so the NodeFlavorReconciler can pool
	// CPU-only nodes under it. The per-unit spec (cpu/ram/storage) and the .z-*
	// profiles are no longer derived onto the node: NodeFlavorReconciler reads
	// node status.capacity directly and writes the unit spec into ResourceFlavor
	// notes, decoupling unit management from the Node.
	gKey := ExtractGeneralNodeKey(node)
	gManu, _, _ := strings.Cut(gKey, "-")
	// "general.${prefix}${manufacturer}=true"
	labels[GeneralFeatureLabelPrefix+gManu] = "true"
	// "general.${prefix}${manufacturer}-${id}=true"
	labels[GeneralFeatureLabelPrefix+gKey] = "true"
	// "general.${prefix}${manufacturer}-${id}.count=${count}", the node's CPU core
	// count rounded up. ExtractNodeFlavors reads the CPU flavor size from here, and the
	// flavor's node selector pins to this label, so a reserved CPU flavor lands on one
	// homogeneous batch of same-sized nodes.
	cpuQ := node.Status.Capacity[core.ResourceCPU]
	if count := cpuQ.Value(); count > 0 {
		labels[GeneralFeatureLabelPrefix+gKey+".count"] = strconvx.Itoa(count)
	}

	return labels
}

// NodeFlavor identifies one Kueue ResourceFlavor a node contributes to, derived
// from the node's feature labels and status capacity. Every managed node yields
// one CPU flavor (its general key, sized by CPU cores) plus one device flavor per
// acceleratable key it carries (sized by device count).
//
// The NodeFlavorReconciler pools the nodes that share a Name: the flavor's capacity
// is the count of pooled nodes times Count. The unit spec is not derived here — it is
// a fixed default on the InstanceType.
type NodeFlavor struct {
	// Name is the ResourceFlavor name, with the CPU key always encoded and full os/arch:
	// "gpustack--${gKey}-${os}-${arch}-${count}c" for a CPU flavor or
	// "gpustack--${gKey}--${aKey}-${os}-${arch}-${count}d" for a device flavor.
	Name string
	// GeneralKey is the node's general(CPU) key, always set. It mirrors the InstanceType's
	// GeneralGroup.
	GeneralKey string
	// AcceleratorKey is the acceleratable(device) key of a device flavor, empty for a CPU
	// flavor. It mirrors the InstanceType's AcceleratorGroup, so an accelerated flavor encodes
	// both the CPU (GeneralKey) and the accelerator (AcceleratorKey).
	AcceleratorKey string
	// OS and Arch are the node's kubernetes.io/os and kubernetes.io/arch values
	// (full, e.g. "linux"/"amd64"), carried verbatim into the Name and the
	// schedule labels.
	OS   string
	Arch string
	// Count is the node's CPU core count (CPU flavor) or device count (device flavor).
	Count int64
	// Acceleratable reports whether the flavor represents accelerated resources.
	Acceleratable bool
	// Manufacturer is the device manufacturer (device flavor) or the CPU
	// manufacturer (CPU flavor).
	Manufacturer string
	// Product and Family describe the device (device flavor); for a CPU flavor they
	// are populated only when CPU-name blending is enabled, since the default
	// "generic" key pools heterogeneous CPUs where one node's identity is misleading.
	Product string
	Family  string
	// Memory is the per-card VRAM of a device flavor (e.g. "24576Mi"); empty for a
	// CPU flavor. Carried into ResourceFlavor notes so the webhook can fold a
	// memory-mib request without reading the node.
	Memory string
	// Cores is the per-card accelerator core count of a device flavor (e.g. "9216");
	// empty for a CPU flavor. Carried into the ResourceFlavor notes.
	Cores string
	// NodeLabels selects the pooled nodes: the managed mark, kubernetes.io/os and
	// kubernetes.io/arch (full values), the flavor's feature key label, and its
	// per-node .count sibling so a reserved flavor binds one homogeneous batch of nodes.
	NodeLabels map[string]string
}

// OwnKey returns the flavor's own feature key — the accelerator key when accelerated,
// otherwise the general(CPU) key — which the flavor sizes its own .count/.capacity on.
func (f NodeFlavor) OwnKey() string {
	if f.Acceleratable {
		return f.AcceleratorKey
	}
	return f.GeneralKey
}

// DerivedInstanceTypeIdentity returns the setting-correct name and group identity for the pool's
// derived InstanceType. When CPU-manufacturer awareness is off the pool collapses — a
// non-accelerated pool to the "generic" CPU group (gpustack--generic-${os}-${arch}) and an
// accelerated pool to just its accelerator (gpustack--${aKey}-${os}-${arch}, CPU ignored); when
// on, both split by the CPU key (gpustack--${gKey}-${os}-${arch} /
// gpustack--${gKey}--${aKey}-${os}-${arch}).
func (f NodeFlavor) DerivedInstanceTypeIdentity(aware bool) (name, generalGroup, acceleratorGroup string) {
	generalGroup = GeneralGroupGeneric
	if aware {
		generalGroup = f.GeneralKey
	}
	if f.Acceleratable {
		acceleratorGroup = f.AcceleratorKey
		if aware {
			name = fmt.Sprintf("gpustack--%s--%s-%s-%s", f.GeneralKey, f.AcceleratorKey, f.OS, f.Arch)
		} else {
			name = fmt.Sprintf("gpustack--%s-%s-%s", f.AcceleratorKey, f.OS, f.Arch)
		}
		return name, generalGroup, acceleratorGroup
	}
	if aware {
		name = fmt.Sprintf("gpustack--%s-%s-%s", f.GeneralKey, f.OS, f.Arch)
	} else {
		name = fmt.Sprintf("gpustack--%s-%s-%s", GeneralGroupGeneric, f.OS, f.Arch)
	}
	return name, generalGroup, acceleratorGroup
}

// ExtractNodeFlavors derives the ResourceFlavors a node contributes to from its
// feature labels and status capacity: one CPU flavor keyed by the general node
// key, plus one device flavor per acceleratable key. It returns nil when the node
// carries no labels.
func ExtractNodeFlavors(node *core.Node) (flavors []NodeFlavor) {
	if node.Labels == nil {
		return nil
	}

	os := node.Labels[core.LabelOSStable]
	arch := node.Labels[core.LabelArchStable]

	// CPU flavor: keyed by the general(CPU) node key, sized by the node's CPU count.
	// The count is read from the ".count" label ConstructNodeCapacityLabels stamped
	// (the rounded-up CPU capacity), not from status.capacity directly, so the flavor
	// name and its node selector agree on one canonical value.
	gKey := ExtractGeneralNodeKey(node)
	gNodeKey := GeneralFeatureLabelPrefix + gKey
	gCountKey := gNodeKey + ".count"
	if cpuCount, err := strconvx.Atoi[int64](node.Labels[gCountKey]); err == nil && cpuCount > 0 {
		manufacturer, _, _ := strings.Cut(gKey, "-")
		nodeLabels := map[string]string{
			systemname.ManagedLabelKey: "true",
			core.LabelOSStable:         os,
			core.LabelArchStable:       arch,
			gNodeKey:                   "true",
			// Pin to same-count nodes so a reserved flavor stays on one homogeneous batch.
			gCountKey: node.Labels[gCountKey],
		}

		flavors = append(flavors, NodeFlavor{
			Name:         formatNodeFlavorName(gKey, "", os, arch, cpuCount, false),
			GeneralKey:   gKey,
			OS:           os,
			Arch:         arch,
			Count:        cpuCount,
			Manufacturer: manufacturer,
			// The general key now always reflects the node's real CPU, so the CPU
			// identity is meaningful and always recorded.
			Product:    generalFeatureAnnotation(node, "name"),
			Family:     generalFeatureAnnotation(node, "family"),
			NodeLabels: nodeLabels,
		})
	}

	// Device flavors: one per acceleratable key, sized by its reported device count.
	for _, aKey := range ExtractAcceleratableNodeKeys(node) {
		aNodeKey := AcceleratableFeatureLabelPrefix + aKey
		aCountKey := aNodeKey + ".count"
		count, err := strconvx.Atoi[int64](node.Labels[aCountKey])
		if err != nil || count <= 0 {
			continue
		}
		manufacturer, _, _ := strings.Cut(aKey, "-")
		nodeLabels := map[string]string{
			systemname.ManagedLabelKey: "true",
			core.LabelOSStable:         os,
			core.LabelArchStable:       arch,
			// Pin the CPU-key presence so an accelerated flavor binds nodes of the
			// paired CPU (the aggregation layer decides whether to split on it).
			gNodeKey:  "true",
			aNodeKey:  "true",
			aCountKey: node.Labels[aCountKey],
		}

		flavors = append(flavors, NodeFlavor{
			Name:           formatNodeFlavorName(gKey, aKey, os, arch, count, true),
			GeneralKey:     gKey,
			AcceleratorKey: aKey,
			OS:             os,
			Arch:           arch,
			Count:          count,
			Acceleratable:  true,
			Manufacturer:   manufacturer,
			Product:        node.Labels[aNodeKey+".product"],
			Family:         node.Labels[aNodeKey+".family"],
			Memory:         node.Labels[aNodeKey+".memory"],
			Cores:          node.Labels[aNodeKey+".cores"],
			NodeLabels:     nodeLabels,
		})
	}

	return flavors
}

// formatNodeFlavorName builds a ResourceFlavor name that always encodes the CPU key gKey
// and, for a device flavor, the accelerator key aKey after a "--" separator; the node's
// full os/arch and the per-node count follow. The suffix is "c" for a CPU flavor and "d"
// for a device flavor. The "gpustack--" prefix and "--" key separator are deliberate:
// gKey/aKey themselves contain single dashes, so the doubled dash keeps the segments
// unambiguous.
func formatNodeFlavorName(gKey, aKey, os, arch string, count int64, acceleratable bool) string {
	if acceleratable {
		return fmt.Sprintf("gpustack--%s--%s-%s-%s-%dd", gKey, aKey, os, arch, count)
	}
	return fmt.Sprintf("gpustack--%s-%s-%s-%dc", gKey, os, arch, count)
}

// generalFeatureAnnotation returns the "feature.gpustack.ai/cpu-${name}"
// annotation of the given Node, the shared accessor for every CPU attribute
// reported by the NFD NodeFeatureRule (name, family, the core counts, the cache
// sizes, ...). When NFD cannot resolve an attribute it leaves the "@cpu.model.*"
// template reference verbatim as the value, so any value leading with "@" is
// treated as unreported and returned as empty.
func generalFeatureAnnotation(node *core.Node, name string) string {
	v := node.Annotations[FeatureLabelPrefix+"cpu-"+name]
	if strings.HasPrefix(v, "@") {
		return ""
	}
	return v
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
