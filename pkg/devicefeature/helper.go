package devicefeature

import (
	"strconv"
	"strings"

	core "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	"gpustack.ai/gpustack/pkg/device"
	"gpustack.ai/gpustack/pkg/utils/mapx"
	"gpustack.ai/gpustack/pkg/utils/strconvx"
	"gpustack.ai/gpustack/pkg/utils/typex"
)

const (
	FeatureLabelPrefix = "feature.gpustack.ai/"
	CreditsLabelPrefix = "credits.gpustack.ai/"
	DeviceLabelPrefix  = "device.gpustack.ai/"

	DisfeaturedNodeKey = "cpu-only"
)

// ConstructNodeLabels constructs node feature labels from the given device group list.
func ConstructNodeLabels(groups device.DevicesGroupList) map[string]string {
	labels := map[string]string{}
	if len(groups) != 0 {
		for i := range groups {
			applyLabelsOfAccelerators(labels, groups[i])
		}
	}
	return labels
}

// applyLabelsOfAccelerators applies node feature labels for the given devices group to the given labels map.
func applyLabelsOfAccelerators(labels map[string]string, group device.DevicesGroup) {
	if len(group.Accelerators) == 0 {
		return
	}

	labelKey := FeatureLabelPrefix + group.Manufacturer

	// "${prefix}${manufacturer}=true"
	labels[labelKey] = "true"
	// "${prefix}${manufacturer}.driver-version=${driverVersion}"
	if v := group.DriverVersion; v != "" {
		labels[labelKey+".driver-version"] = v
	}
	// "${prefix}${manufacturer}.runtime-version=${runtimeVersion}"
	if v := group.RuntimeVersion; v != "" {
		labels[labelKey+".runtime-version"] = v
	}
	// "${prefix}${manufacturer}.compute-capability=${computeCapability}"
	if v := group.ComputeCapability; v != "" {
		labels[labelKey+".compute-capability"] = v
	}

	selfLabelKey := labelKey + "." + group.ID

	// "${prefix}${manufacturer}.${id}=true"
	labels[selfLabelKey] = "true"
	// "${prefix}${manufacturer}.${id}.product=${name}"
	labels[selfLabelKey+".product"] = group.Name
	// "${prefix}${manufacturer}.${id}.memory=${memory}"
	labels[selfLabelKey+".memory"] = strconvx.Itoa(group.Memory) + "Mi"
	// "${prefix}${manufacturer}.${id}.cores=${cores}"
	labels[selfLabelKey+".cores"] = strconvx.Itoa(group.Cores)
	// "${prefix}${manufacturer}.${id}.family=${family}"
	if v := group.Family; v != "" {
		labels[selfLabelKey+".family"] = v
	}
	// "${prefix}${manufacturer}.${id}.accelerators=${count}"
	labels[selfLabelKey+".accelerators"] = strconv.Itoa(len(group.Accelerators))
}

// ExtractNodeKeys returns accelerated node keys of the given Node.
func ExtractNodeKeys(node *core.Node) []string {
	ret := mapx.FilterSlice(node.Labels, func(k, v string) (string, bool) {
		if strings.HasPrefix(k, FeatureLabelPrefix) {
			if v == "true" {
				v = strings.TrimPrefix(k, FeatureLabelPrefix)
				if strings.Contains(v, ".") {
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
	// DriverVersion is the version of the driver used by the device.
	DriverVersion string
	// RuntimeVersion is the version of the runtime used by the device.
	RuntimeVersion string
	// ComputeCapability is the compute capability of the device.
	ComputeCapability string
	// Family is the family of the device.
	Family string
	// Accelerator of the node.
	Accelerator resource.Quantity
	// CPU of the node.
	CPU resource.Quantity
	// RAM of the node.
	RAM resource.Quantity
	// LocalStorage of the node.
	LocalStorage resource.Quantity
}

// ExtractNodeFeatureByKey extracts the NodeFeature from given node and key.
func ExtractNodeFeatureByKey(node *core.Node, key string) (ndf NodeFeature) {
	p := strings.SplitN(key, ".", 2)
	if len(p) != 2 {
		ndf.NodeLabels = map[string]string{core.LabelHostname: node.Labels[core.LabelHostname]}
		ndf.CPU = node.Status.Capacity[core.ResourceCPU]
		ndf.RAM = node.Status.Capacity[core.ResourceMemory]
		ndf.LocalStorage = node.Status.Capacity[core.ResourceEphemeralStorage]
		return ndf
	}

	manufacturer := p[0]
	ndf.NodeLabels = map[string]string{FeatureLabelPrefix + key: "true"}
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
	ndf.CPU = node.Status.Capacity[core.ResourceCPU]
	ndf.RAM = node.Status.Capacity[core.ResourceMemory]
	ndf.LocalStorage = node.Status.Capacity[core.ResourceEphemeralStorage]
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
