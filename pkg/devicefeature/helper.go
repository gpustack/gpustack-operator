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

	selfLabelKey := manuLabelKey + "-" + group.ID
	nodeKey := group.Manufacturer + "-" + group.ID

	// Per-device unit CPU/RAM derived from the host's allocatable budget
	// minus a fixed system reservation. Kept as side labels rather than
	// folded into selfLabelKey so small allocatable drift does not churn the
	// node key. Once written, GetDeviceUnitResources re-reads these labels
	// instead of recomputing — keeping the value stable across reconciles
	// and letting operators override via direct label edits.
	units := GetDeviceUnitResources(node, nodeKey, int64(len(group.Accelerators)))

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
	// "${prefix}${manufacturer}-${id}.unit-cpu=${unitCPU}"
	labels[selfLabelKey+".unit-cpu"] = units.CPU
	// "${prefix}${manufacturer}-${id}.unit-ram=${unitRAM}"
	labels[selfLabelKey+".unit-ram"] = units.RAM

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
	selfLabelKey := FeatureLabelPrefix + key
	selfUnitCPUKey := selfLabelKey + ".unit-cpu"
	selfUnitRAMKey := selfLabelKey + ".unit-ram"
	// gpustack.ai/managed: "true"
	// feature.gpustack.ai/${manufacturer}-${id}: "true"
	// feature.gpustack.ai/${manufacturer}-${id}.unit-cpu: ${unitCPU}
	// feature.gpustack.ai/${manufacturer}-${id}.unit-ram: ${unitRAM}
	ndf.NodeLabels = map[string]string{
		systemname.ManagedLabelKey: "true",
		selfLabelKey:               "true",
		selfUnitCPUKey:             node.Labels[selfUnitCPUKey],
		selfUnitRAMKey:             node.Labels[selfUnitRAMKey],
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
	ndf.LocalStorage = node.Status.Allocatable[core.ResourceEphemeralStorage]
	ndf.UnitResources = GetDeviceUnitResources(node, key, ndf.Accelerator.Value())

	// Scale the per-device units up by the allocatable accelerator count to
	// get the node-level CPU/RAM the scheduler should book alongside these
	// devices. With zero allocatable accelerators the product is zero,
	// which correctly reports "no headroom" for new device-bound workloads.
	accelCount := ndf.Accelerator.Value()
	unitCPU := resource.MustParse(ndf.UnitResources.CPU)
	unitRAM := resource.MustParse(ndf.UnitResources.RAM)
	ndf.CPU = *resource.NewMilliQuantity(unitCPU.MilliValue()*accelCount, resource.DecimalSI)
	ndf.RAM = *resource.NewQuantity(unitRAM.Value()*accelCount, resource.BinarySI)
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

// DeviceUnitResources bundles the per-device unit CPU and unit RAM that a
// single device on a node is expected to receive when it joins a pod.
type DeviceUnitResources struct {
	// CPU is the per-device CPU in milli-cores, e.g. "2000m". Derived from
	// the reservation-deducted per-device CPU budget rounded up to integer
	// cores minus one for headroom. Defaults to "1000m" when no positive
	// suggestion can be formed.
	CPU string
	// RAM is the per-device RAM in MiB, e.g. "8192Mi". Derived by walking
	// descending CPU:RAM ratios (8, 7, ..., 2) and picking the largest
	// ratio whose induced RAM (suggestedCPU * ratio Gi) stays strictly
	// under the per-device RAM budget. Defaults to "1024Mi" when no ratio
	// fits.
	RAM string
}

// _DeviceUnitResourceReservedCPUMilli and _DeviceUnitResourceReservedRAMMi
// are the per-node system reservation subtracted from allocatable before
// dividing the remainder across devices. They cover GPUStack agents,
// kubelet, CRI, and other system pods that share the node with workloads —
// allocating the raw allocatable across devices would leave nothing for
// these and cause pods to fail scheduling.
const (
	_DeviceUnitResourceReservedCPUMilli int64 = 1000
	_DeviceUnitResourceReservedRAMMi    int64 = 2 * 1024
)

// _DeviceUnitResourceDefault is the 1C/1Gi fallback returned when the
// per-device budget cannot induce a positive (CPU, RAM) suggestion —
// e.g. very small hosts or very high device counts.
var _DeviceUnitResourceDefault = DeviceUnitResources{CPU: "1000m", RAM: "1024Mi"}

// _deviceUnitResourceCPUToRAMRatios is the descending list of integer
// RAM:CPU ratios (Gi per core) tried when deriving a per-device unit. The
// first ratio whose induced RAM stays strictly under the per-device RAM
// budget wins, which biases toward giving each device as much RAM as the
// budget can support while keeping CPU and RAM in an integer ratio.
var _deviceUnitResourceCPUToRAMRatios = []int64{8, 7, 6, 5, 4, 3, 2}

// GetDeviceUnitResources returns the per-device CPU and RAM units for a
// device identified by nodeKey ("${manufacturer}-${id}") on node, split
// deviceCount-ways.
//
// Resolution:
//  1. If node already carries "${FeatureLabelPrefix}${nodeKey}.unit-cpu"
//     and ".unit-ram" labels with non-empty values, parse and return them
//     reformatted. This makes the value sticky across reconciles and lets
//     operators override by directly editing the labels.
//  2. Otherwise, subtract a fixed system reservation
//     (_DeviceUnitResourceReservedCPUMilli / _DeviceUnitResourceReservedRAMMi)
//     from the node's allocatable CPU/RAM, divide the remainder by
//     deviceCount (clamped to >= 1) to get the raw per-device share. Take
//     the per-device CPU rounded up to integer cores minus one — i.e.
//     (unitCPUMilli - 1) / 1000 — as the suggested CPU, then walk
//     descending CPU:RAM ratios (8, 7, ..., 2) and return the first ratio
//     whose induced RAM (suggestedCPU * ratio Gi) is strictly less than
//     the per-device RAM share.
//  3. If suggested CPU is below 1 or no ratio fits, return
//     _DeviceUnitResourceDefault ("1000m" / "1024Mi" = 1C/1Gi).
func GetDeviceUnitResources(node *core.Node, nodeKey string, deviceCount int64) DeviceUnitResources {
	selfLabelKey := FeatureLabelPrefix + nodeKey
	if cpuLabel, ramLabel := node.Labels[selfLabelKey+".unit-cpu"], node.Labels[selfLabelKey+".unit-ram"]; cpuLabel != "" && ramLabel != "" {
		cpuQ, errCPU := resource.ParseQuantity(cpuLabel)
		ramQ, errRAM := resource.ParseQuantity(ramLabel)
		if errCPU == nil && errRAM == nil {
			return DeviceUnitResources{
				CPU: strconv.FormatInt(cpuQ.MilliValue(), 10) + "m",
				RAM: strconv.FormatInt(ramQ.Value()/int64(quantityx.Mi), 10) + "Mi",
			}
		}
	}

	n := deviceCount
	if n <= 0 {
		n = 1
	}

	allocCPU := node.Status.Allocatable[core.ResourceCPU]
	allocRAM := node.Status.Allocatable[core.ResourceMemory]

	availCPUMilli := allocCPU.MilliValue() - _DeviceUnitResourceReservedCPUMilli
	if availCPUMilli < 0 {
		availCPUMilli = 0
	}
	availRAMMi := (allocRAM.Value() / int64(quantityx.Mi)) - _DeviceUnitResourceReservedRAMMi
	if availRAMMi < 0 {
		availRAMMi = 0
	}

	unitCPUMilli := availCPUMilli / n
	unitRAMMi := availRAMMi / n

	// ceil(unitCPUMilli / 1000) - 1 in integer form (for unitCPUMilli > 0).
	// One core of headroom over the strict per-device CPU budget — also
	// makes the exact-integer case (e.g. 12000m) yield 11 rather than 12.
	if unitCPUMilli <= 0 {
		return _DeviceUnitResourceDefault
	}
	suggestedCPUCores := (unitCPUMilli - 1) / 1000
	if suggestedCPUCores < 1 {
		return _DeviceUnitResourceDefault
	}

	// 1 Gi = 1024 Mi; the budget comparison stays entirely in MiB.
	const miPerGi int64 = 1024
	for _, ratio := range _deviceUnitResourceCPUToRAMRatios {
		suggestedRAMMi := suggestedCPUCores * ratio * miPerGi
		if suggestedRAMMi < unitRAMMi {
			return DeviceUnitResources{
				CPU: strconv.FormatInt(suggestedCPUCores*1000, 10) + "m",
				RAM: strconv.FormatInt(suggestedRAMMi, 10) + "Mi",
			}
		}
	}
	return _DeviceUnitResourceDefault
}
