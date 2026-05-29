package devicefeature

import (
	"strconv"
	"strings"

	core "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	"gpustack.ai/gpustack/pkg/device"
	"gpustack.ai/gpustack/pkg/kubemeta"
	"gpustack.ai/gpustack/pkg/systemname"
	"gpustack.ai/gpustack/pkg/utils/mapx"
	"gpustack.ai/gpustack/pkg/utils/quantityx"
	"gpustack.ai/gpustack/pkg/utils/strconvx"
	"gpustack.ai/gpustack/pkg/utils/typex"
)

const (
	FeatureLabelPrefix = "feature." + systemname.LabelPrefix
	CreditsLabelPrefix = "credits." + systemname.LabelPrefix
	DeviceLabelPrefix  = "device." + systemname.LabelPrefix

	DisfeaturedNodeKey = "cpu-only"
)

// ConstructNodeLabels constructs node feature labels from the given device group list.
func ConstructNodeLabels(node *core.Node, groups device.DevicesGroupList) map[string]string {
	labels := map[string]string{
		systemname.ManagedLabelKey: "true",
	}
	if node.Labels != nil && node.Labels[systemname.ManagedLabelKey] != "" {
		labels[systemname.ManagedLabelKey] = node.Labels[systemname.ManagedLabelKey]
	}
	for i := range groups {
		applyDeviceFeatureLabels(labels, groups[i], node)
	}
	return labels
}

// applyDeviceFeatureLabels applies device feature labels of the given device group to the given labels map.
func applyDeviceFeatureLabels(labels map[string]string, group device.DevicesGroup, node *core.Node) {
	if len(group.Accelerators) == 0 {
		return
	}

	manuLabelKey := FeatureLabelPrefix + group.Manufacturer

	// "${prefix}${manufacturer}=true"
	labels[manuLabelKey] = "true"
	// "${prefix}${manufacturer}.driver-version=${driverVersion}"
	if v := group.DriverVersion; v != "" {
		labels[manuLabelKey+".driver-version"] = v
	}
	// "${prefix}${manufacturer}.runtime-version=${runtimeVersion}"
	if v := group.RuntimeVersion; v != "" {
		labels[manuLabelKey+".runtime-version"] = v
	}
	// "${prefix}${manufacturer}.compute-capability=${computeCapability}"
	if v := group.ComputeCapability; v != "" {
		labels[manuLabelKey+".compute-capability"] = v
	}

	// Per-device unit CPU/RAM derived from the host's allocatable budget.
	// Kept as side labels rather than folded into selfLabelKey so small
	// allocatable drift does not churn the node key. The display variants
	// (whole cores / whole GiB, ceiled, 1C/1Gi minimum) are recorded here
	// for user-facing metadata.
	units := GetDeviceUnitResources(
		node.Status.Allocatable[core.ResourceCPU],
		node.Status.Allocatable[core.ResourceMemory],
		int64(len(group.Accelerators)),
	)

	selfLabelKey := manuLabelKey + "-" + group.ID

	// "${prefix}${manufacturer}-${id}=true"
	labels[selfLabelKey] = "true"
	// "${prefix}${manufacturer}-${id}.product=${name}"
	labels[selfLabelKey+".product"] = group.Name
	// "${prefix}${manufacturer}-${id}.memory=${memory}"
	labels[selfLabelKey+".memory"] = quantityx.Format(resource.MustParse(strconvx.Itoa(group.Memory) + "Mi"))
	// "${prefix}${manufacturer}-${id}.cores=${cores}"
	labels[selfLabelKey+".cores"] = strconvx.Itoa(group.Cores)
	// "${prefix}${manufacturer}-${id}.family=${family}"
	if v := group.Family; v != "" {
		labels[selfLabelKey+".family"] = v
	}
	// "${prefix}${manufacturer}-${id}.accelerators=${count}"
	labels[selfLabelKey+".accelerators"] = strconv.Itoa(len(group.Accelerators))
	// "${prefix}${manufacturer}-${id}.cpu=${displayCPU}"
	labels[selfLabelKey+".cpu"] = units.DisplayCPU
	// "${prefix}${manufacturer}-${id}.ram=${displayRAM}"
	labels[selfLabelKey+".ram"] = units.DisplayRAM

	// Match Kubernetes label values' requirements.
	for k := range labels {
		labels[k] = kubemeta.SanitizeLabelValue(labels[k])
	}
}

// ExtractNodeKeys returns accelerated node keys of the given Node.
func ExtractNodeKeys(node *core.Node) []string {
	ret := mapx.FilterSlice(node.Labels, func(k, v string) (string, bool) {
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
	if len(ret) == 0 {
		return []string{DisfeaturedNodeKey}
	}
	return ret
}

// NodeFeature represents the device feature of a node, which is used for scheduling.
type NodeFeature struct {
	// NodeLabels is the node labels for scheduling.
	NodeLabels map[string]string
	// Tolerations is the tolerations for scheduling.
	Tolerations []core.Toleration
	// Sliced is the flag indicating whether the device is sliced.
	Sliced string
	// Manufacturer is the name of the device manufacturer.
	Manufacturer string
	// Product is the name of the device product.
	Product string
	// Memory is the memory size of the device in MiB.
	Memory string
	// Cores is the number of cores of the device.
	Cores string
	// ComputeCapability is the compute capability of the device.
	ComputeCapability string
	// Family is the family of the device.
	Family string
	// Accelerator of the node that can be allocated for workloads.
	Accelerator resource.Quantity
	// CPU of the node.
	CPU resource.Quantity
	// RAM of the node.
	RAM resource.Quantity
	// LocalStorage of the node.
	LocalStorage resource.Quantity
	// Per-Device Units derived from the host's allocatable budget and device count.
	UnitResources DeviceUnitResources
}

// ExtractNodeFeatureByKey extracts the NodeFeature from given node and key.
//
// The key is in the format of "${manufacturer}-${id}",
// which is used to identify the device feature of the node.
func ExtractNodeFeatureByKey(node *core.Node, key string) (ndf NodeFeature) {
	p := strings.SplitN(key, "-", 2)
	if len(p) != 2 || p[0] == "cpu" {
		hostname := node.Labels[core.LabelHostname]
		if hostname == "" {
			hostname = node.Name
		}
		// kubernetes.io/hostname: ${hostname}
		ndf.NodeLabels = map[string]string{
			core.LabelHostname: hostname,
		}
		ndf.CPU = node.Status.Allocatable[core.ResourceCPU]
		ndf.RAM = node.Status.Allocatable[core.ResourceMemory]
		ndf.LocalStorage = node.Status.Allocatable[core.ResourceEphemeralStorage]
		return ndf
	}

	manufacturer := p[0]
	// gpustack.ai/managed: "true"
	// feature.gpustack.ai/${manufacturer}-${id}: "true"
	// feature.gpustack.ai/${manufacturer}-${id}.cpu: ${displayCPU}
	// feature.gpustack.ai/${manufacturer}-${id}.ram: ${displayRAM}
	ndf.NodeLabels = map[string]string{
		systemname.ManagedLabelKey:        "true",
		FeatureLabelPrefix + key:          "true",
		FeatureLabelPrefix + key + ".cpu": node.Labels[FeatureLabelPrefix+key+".cpu"],
		FeatureLabelPrefix + key + ".ram": node.Labels[FeatureLabelPrefix+key+".ram"],
	}
	for i := range node.Spec.Taints {
		taints := &node.Spec.Taints[i]
		if taints.Key != DeviceLabelPrefix+"acclerator.sliced" {
			continue
		}
		if _SlicedResourceOperatedSizesSet.Has(taints.Value) {
			ndf.Tolerations = []core.Toleration{
				{
					Key:      taints.Key,
					Operator: core.TolerationOpEqual,
					Value:    taints.Value,
					Effect:   taints.Effect,
				},
			}
			ndf.Sliced = taints.Value
		}
		break
	}
	ndf.Manufacturer = manufacturer
	ndf.Product = node.Labels[FeatureLabelPrefix+key+".product"]
	ndf.Memory = node.Labels[FeatureLabelPrefix+key+".memory"]
	ndf.Cores = node.Labels[FeatureLabelPrefix+key+".cores"]
	ndf.ComputeCapability = node.Labels[FeatureLabelPrefix+manufacturer+".compute-capability"]
	ndf.Family = node.Labels[FeatureLabelPrefix+key+".family"]
	ndf.Accelerator = node.Status.Allocatable[GetResourceName(manufacturer, workercore.DeviceAllocationModeExclusive)]
	ndf.CPU = node.Status.Allocatable[core.ResourceCPU]
	ndf.RAM = node.Status.Allocatable[core.ResourceMemory]
	ndf.LocalStorage = node.Status.Allocatable[core.ResourceEphemeralStorage]
	ndf.UnitResources = GetDeviceUnitResources(
		node.Status.Allocatable[core.ResourceCPU],
		node.Status.Allocatable[core.ResourceMemory],
		ndf.Accelerator.Value(),
	)
	return ndf
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

// DeviceUnitResources bundles the per-device unit CPU and unit RAM in two
// forms — actual (for scheduling) and display (for user-facing metadata).
type DeviceUnitResources struct {
	// ActualCPU is the per-device CPU as milli-cores, e.g. "2500m". Derived
	// by flooring the host CPU total (in milli) divided by deviceCount, so
	// the sum across all devices never exceeds the host's allocatable
	// budget. No minimum is enforced.
	ActualCPU string
	// ActualRAM is the per-device RAM as MiB, e.g. "5888Mi". The host RAM
	// is floored to whole MiB first to discard sub-MiB noise, then divided
	// by deviceCount with the quotient floored. No minimum is enforced.
	ActualRAM string
	// DisplayCPU is the per-device CPU as whole cores, e.g. "3" — ceiled
	// from ActualCPU and clamped to at least "1".
	DisplayCPU string
	// DisplayRAM is the per-device RAM as whole GiB, e.g. "6Gi" — ceiled
	// from ActualRAM and clamped to at least "1Gi".
	DisplayRAM string
}

// GetDeviceUnitResources computes the per-device unit CPU and unit RAM for the given
// node allocatable totals, divided across deviceCount devices. See the
// DeviceUnitResources field comments for the semantics of each value.
func GetDeviceUnitResources(cpu, ram resource.Quantity, deviceCount int64) DeviceUnitResources {
	n := deviceCount
	if n <= 0 {
		n = 1
	}

	actualCPUMilli := cpu.MilliValue() / n
	actualRAMMi := (ram.Value() / int64(quantityx.Mi)) / n

	displayCPUCores := (actualCPUMilli + 999) / 1000
	if displayCPUCores < 1 {
		displayCPUCores = 1
	}
	displayRAMGi := (actualRAMMi + 1023) / 1024
	if displayRAMGi < 1 {
		displayRAMGi = 1
	}

	return DeviceUnitResources{
		ActualCPU:  strconv.FormatInt(actualCPUMilli, 10) + "m",
		ActualRAM:  strconv.FormatInt(actualRAMMi, 10) + "Mi",
		DisplayCPU: strconv.FormatInt(displayCPUCores, 10),
		DisplayRAM: strconv.FormatInt(displayRAMGi, 10) + "Gi",
	}
}
