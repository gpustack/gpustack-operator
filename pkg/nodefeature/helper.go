package nodefeature

import (
	"fmt"
	"strings"

	core "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"

	"gpustack.ai/gpustack/pkg/device"
	"gpustack.ai/gpustack/pkg/kubemeta"
	"gpustack.ai/gpustack/pkg/systemname"
	"gpustack.ai/gpustack/pkg/utils/mapx"
	"gpustack.ai/gpustack/pkg/utils/osx"
	"gpustack.ai/gpustack/pkg/utils/quantityx"
	"gpustack.ai/gpustack/pkg/utils/strconvx"
	"gpustack.ai/gpustack/pkg/utils/stringx"
)

// Toggle of the functions.
var (
	generalNodeKeyWithCPUName bool
)

func init() {
	generalNodeKeyWithCPUName = osx.Getenv("GPUSTACK_GENERAL_NODE_KEY_WITH_CPU_NAME") == valueTrue
}

// valueTrue is the canonical string value of a boolean feature label ("true"),
// shared so the many label-value writes/checks below stay a single literal.
const valueTrue = "true"

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
	labels[NodeAcceleratableLabelKey] = valueTrue

	manuKey := AcceleratableFeatureLabelPrefix + group.Manufacturer

	// "acceleratable.${prefix}${manufacturer}=true"
	labels[manuKey] = valueTrue
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
	labels[nodeKey] = valueTrue
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
			if v == valueTrue {
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

const (
	_NFDCPUModelVendorIDLabelKey = "feature.node.kubernetes.io/cpu-model.vendor_id"
	_NFDCPUModelFamilyLabelKey   = "feature.node.kubernetes.io/cpu-model.family"
	_NFDCPUModelIDLabelKey       = "feature.node.kubernetes.io/cpu-model.id"
)

// ExtractGeneralNodeKey derives the general(CPU) node key of the given Node,
// it always returns a non-empty key.
// Unless the GPUSTACK_GENERAL_NODE_KEY_WITH_CPU_NAME environment variable is
// truthy at startup, it returns "generic". Otherwise, the key is in the format
// "${manufacturer}-${id}", e.g. "intel-xeon-platinum-8358": the id leads with
// the sanitized "feature.gpustack.ai/cpu-name" annotation when reported, or with
// the NFD cpu-model family and id labels otherwise. It falls back to "generic"
// when neither the cpu-name annotation nor the cpu-model labels are usable.
func ExtractGeneralNodeKey(node *core.Node) string {
	return extractGeneralNodeKey(node, generalNodeKeyWithCPUName)
}

// extractGeneralNodeKey is ExtractGeneralNodeKey with the CPU-name blending
// toggle passed in explicitly, so tests can exercise both modes.
//
// os/arch are not part of the key: the ResourceFlavor/ClusterQueue name carries
// os/arch explicitly and spec.nodeLabels pins kubernetes.io/os|arch, so nodes of
// different ISAs cannot pool into one flavor/queue even when the key collides
// (the cpu-model family and id labels are independent numbering spaces on x86
// versus arm64, so a small value such as "25-1" can legitimately appear on both).
//
// When generalNodeKeyWithCPUName is false, the key is "generic": every CPU pools
// together, separated only by the os/arch carried in the flavor/queue name.
//
// When it is true, the manufacturer (the lowercased NFD cpu-model vendor_id, or
// "generic" when unknown) leads and a CPU identity is blended in between: the
// sanitized "feature.gpustack.ai/cpu-name" annotation when reported (e.g.
// "amd-epyc-7763"), the NFD cpu-model family and id labels as the rare fallback
// when the annotation is absent (e.g. "amd-25-1"), or nothing when no CPU
// identity is usable (e.g. "amd", or "generic" when the vendor is unknown too).
func extractGeneralNodeKey(node *core.Node, generalNodeKeyWithCPUName bool) string {
	// If generalNodeKeyWithCPUName is enabled,
	// the manufacturer part of the general node key is derived from the NFD cpu-model vendor_id label.
	var manu string
	if generalNodeKeyWithCPUName {
		manu = extractGeneralNodeKeyManufacturer(node)
	} else {
		manu = GeneralManufacturerGeneric
	}

	// os/arch are no longer part of the key: the ResourceFlavor/ClusterQueue name
	// carries os/arch explicitly and spec.nodeLabels pins kubernetes.io/os|arch, so
	// cross-ISA pools cannot collide — the key need not also bake them in.
	//
	// When generalNodeKeyWithCPUName is enabled,
	// try to blend the CPU name into the general node key for better readability and differentiation.
	// The CPU name is reported by the NFD NodeFeatureRule with the "feature.gpustack.ai/cpu-name" annotation,
	// it is sanitized and truncated to fit the Kubernetes label value requirements when constructing the node key.
	//
	// When the annotation is unavailable,
	// the NFD cpu-model family and id labels are used as the fallback id prefix,
	// which are always present and meaningful when CPU information is properly collected.
	//
	// If NFD is not deployed, or the CPU information is not properly collected,
	// the id prefix is empty and the general node key falls back to "${manufacturer}-${idSuffix}".
	var idPrefix string
	if generalNodeKeyWithCPUName {
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
		labels[systemname.ManagedLabelKey] = valueTrue
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
	labels[GeneralFeatureLabelPrefix+gManu] = valueTrue
	// "general.${prefix}${manufacturer}-${id}=true"
	labels[GeneralFeatureLabelPrefix+gKey] = valueTrue

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
	// Name is the ResourceFlavor name:
	// "gpustack-${key}-${os}-${arch}-${count}c" for a CPU flavor or
	// "gpustack-${key}-${os}-${arch}-${count}d" for a device flavor, with full
	// os/arch.
	Name string
	// Key is the general(CPU) node key or the acceleratable(device) node key.
	Key string
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
	// NodeLabels selects the pooled nodes: the managed mark, kubernetes.io/os and
	// kubernetes.io/arch (full values), and the flavor's feature key label.
	NodeLabels map[string]string
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

	// CPU flavor: keyed by the general(CPU) node key, sized by the node's CPU cores.
	gKey := ExtractGeneralNodeKey(node)
	cpuQ := node.Status.Capacity[core.ResourceCPU]
	if cpuCount := cpuQ.Value(); cpuCount > 0 {
		manufacturer, _, _ := strings.Cut(gKey, "-")
		nodeLabels := map[string]string{
			systemname.ManagedLabelKey: valueTrue,
			core.LabelOSStable:         os,
			core.LabelArchStable:       arch,
		}
		nodeLabels[GeneralFeatureLabelPrefix+gKey] = valueTrue

		nf := NodeFlavor{
			Name:         formatNodeFlavorName(gKey, os, arch, cpuCount, false),
			Key:          gKey,
			OS:           os,
			Arch:         arch,
			Count:        cpuCount,
			Manufacturer: manufacturer,
			NodeLabels:   nodeLabels,
		}
		// The default "generic" key pools heterogeneous CPUs, so a single node's
		// CPU identity is only meaningful when CPU-name blending is enabled.
		if generalNodeKeyWithCPUName {
			nf.Product = generalFeatureAnnotation(node, "name")
			nf.Family = generalFeatureAnnotation(node, "family")
		}

		flavors = append(flavors, nf)
	}

	// Device flavors: one per acceleratable key, sized by its reported device count.
	for _, aKey := range ExtractAcceleratableNodeKeys(node) {
		nodeKey := AcceleratableFeatureLabelPrefix + aKey
		count, err := strconvx.Atoi[int64](node.Labels[nodeKey+".count"])
		if err != nil || count <= 0 {
			continue
		}
		manufacturer, _, _ := strings.Cut(aKey, "-")
		nodeLabels := map[string]string{
			systemname.ManagedLabelKey: valueTrue,
			core.LabelOSStable:         os,
			core.LabelArchStable:       arch,
		}
		nodeLabels[AcceleratableFeatureLabelPrefix+aKey] = valueTrue

		flavors = append(flavors, NodeFlavor{
			Name:          formatNodeFlavorName(aKey, os, arch, count, true),
			Key:           aKey,
			OS:            os,
			Arch:          arch,
			Count:         count,
			Acceleratable: true,
			Manufacturer:  manufacturer,
			Product:       node.Labels[nodeKey+".product"],
			Family:        node.Labels[nodeKey+".family"],
			Memory:        node.Labels[nodeKey+".memory"],
			NodeLabels:    nodeLabels,
		})
	}

	return flavors
}

// formatNodeFlavorName builds a ResourceFlavor name from a node key, the node's
// full os/arch and the per-node count. The suffix is "c" for a CPU flavor and "d"
// for a device flavor.
func formatNodeFlavorName(key, os, arch string, count int64, acceleratable bool) string {
	suffix := "c"
	if acceleratable {
		suffix = "d"
	}
	return fmt.Sprintf("gpustack-%s-%s-%s-%d%s",
		key, os, arch, count, suffix)
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
