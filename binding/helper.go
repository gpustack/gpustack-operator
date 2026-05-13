package binding

import (
	"maps"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"

	"golang.org/x/exp/constraints"
	"k8s.io/apimachinery/pkg/util/sets"
	klog "k8s.io/klog/v2"

	"gpustack.ai/gpustack/pkg/utils/osx"
)

var (
	bitsSize           int
	cpuSize            int
	cpuSetSize         int
	numaNodeSize       int
	numaNodeSetSize    int
	cpuNumaNodeMapping []int
	numaNodeCpuMapping map[int][]int
)

func init() {
	bitsSize = getBitsSize()
	cpuSize = getCPUSize()
	cpuSetSize = (cpuSize + bitsSize) / bitsSize
	numaNodeSize = getNumaNodeSize()
	numaNodeSetSize = (numaNodeSize + bitsSize) / bitsSize
	cpuNumaNodeMapping = getCPUNumaNodeMapping()
	numaNodeCpuMapping = getNumaNodeCPUMapping()
}

// GetBitsSize returns the size of the system architecture in bits (32 or 64).
func GetBitsSize() int {
	return bitsSize
}

// GetCPUSize returns the number of CPU cores available on the system.
func GetCPUSize() int {
	return cpuSize
}

// GetCPUSetSize returns the size of the CPU set, which is the number of CPU cores that can be used by the process.
func GetCPUSetSize() int {
	return cpuSetSize
}

// GetNumaNodeSize returns the number of NUMA nodes available on the system.
func GetNumaNodeSize() int {
	return numaNodeSize
}

// GetNumaNodeSetSize returns the size of the NUMA node set, which is the number of NUMA nodes that can be used by the process.
func GetNumaNodeSetSize() int {
	return numaNodeSetSize
}

// GetCPUNumaNodeMapping returns a slice that maps each CPU core to its corresponding NUMA node.
//
// The index is the CPU core number, the value is the NUMA node number.
// If a CPU core is not associated with any NUMA node, the value will be -1.
func GetCPUNumaNodeMapping() []int {
	return slices.Clone(cpuNumaNodeMapping)
}

// GetNumaNodeCPUMapping returns a map that maps each NUMA node to the list of CPU cores associated with it.
//
// The key is the NUMA node number, the value is a slice of CPU core numbers.
// If a NUMA node has no associated CPU cores, the value will be a nil slice.
func GetNumaNodeCPUMapping() map[int][]int {
	return maps.Clone(numaNodeCpuMapping)
}

// GetNumaNodeByBDF returns the NUMA node associated with the given PCI bus ID (BDF).
//
// The input is a string in the format "domain:bus:device.function" (e.g., "0000:00:1f.2").
// The output is the NUMA node number as a string, or an empty string if the BDF is invalid or if there is no associated NUMA node.
func GetNumaNodeByBDF(bdf string) string {
	return getNumaNodeByBDF(bdf)
}

// GetPhysicalFunctionByBDF returns the physical function BDF associated with the given PCI bus ID (BDF).
//
// The input is a string in the format "domain:bus:device.function" (e.g., "0000:00:1f.2").
// The output is the BDF of the physical function as a string, or the original BDF if there is an error or if there is no associated physical function.
func GetPhysicalFunctionByBDF(bdf string) string {
	return getPhysicalPackageIdByBDF(bdf)
}

// MapCPUAffinityStrToNumaNode maps a CPU affinity specification string to the corresponding NUMA nodes.
//
// The input is a string representation of CPU core numbers (e.g., "0-3,5").
// The output is a string representation of the NUMA node numbers corresponding to the specified CPU cores.
// If the input is nil, empty, or invalid, the output will be an empty string.
func MapCPUAffinityStrToNumaNode(affinity string) string {
	if affinity == "" {
		return ""
	}

	cpus := StrRangeToList(affinity)

	nodes := map[int]struct{}{}
	for _, c := range cpus {
		if c >= 0 && c < len(cpuNumaNodeMapping) && cpuNumaNodeMapping[c] >= 0 {
			nodes[cpuNumaNodeMapping[c]] = struct{}{}
		}
	}
	if len(nodes) == 0 {
		return ""
	}

	var list []int
	for n := range nodes {
		list = append(list, n)
	}

	return ListToStrRange(list)
}

// MapNumaNodeStrToCPUAffinity maps a NUMA node specification str to the corresponding CPU cores.
//
// The input is a string representation of NUMA node numbers (e.g., "0-1,3").
// The output is a string representation of the CPU core numbers corresponding to the specified NUMA nodes.
// If the input is nil, empty, or invalid, the output will be an empty string.
func MapNumaNodeStrToCPUAffinity(node string) string {
	if node == "" {
		return ""
	}

	nodes := StrRangeToList(node)

	cpuSet := map[int]struct{}{}
	for _, n := range nodes {
		for _, c := range numaNodeCpuMapping[n] {
			cpuSet[c] = struct{}{}
		}
	}
	if len(cpuSet) == 0 {
		return ""
	}

	var list []int
	for c := range cpuSet {
		list = append(list, c)
	}

	return ListToStrRange(list)
}

// BitmaskToStr converts a slice of integer bitmasks to a string representation of the set bits.
//
// The input is a slice of integer bitmasks, where each bitmask represents a set of integers (e.g., CPU cores or NUMA nodes).
// The output is a string that represents the positions of the set bits in the input bitmasks, formatted as ranges.
// For example, if the input is [0b1011, 0b0101] and bitsSize is 4, the output will be "0-1,3,4,6" because bits 0, 1, and 3 are set in the first bitmask (0-3), and bits 0 and 2 are set in the second bitmask (4-7).
func BitmaskToStr[I constraints.Integer](masks []I) string {
	var parts []int // nolint:prealloc
	offset := 0
	for i := range masks {
		parts = append(parts, BitmaskToList(masks[i], offset)...)
		offset += bitsSize
	}
	return ListToStrRange(parts)
}

// BitmaskToList converts a bitmask to a list of integers, where each set bit corresponds to an integer in the output list.
//
// The input is an integer bitmask and an offset value.
// The output is a slice of integers representing the positions of the set bits in the bitmask, starting from the offset.
// For example, if the bitmask is 0b1011 (11 in decimal) and the offset is 0, the output will be [0, 1, 3] because bits 0, 1, and 3 are set.
// If the bitmask is 0b1011 and the offset is 10, the output will be [10, 11, 13].
func BitmaskToList[I constraints.Integer](mask I, offset int) []int {
	var out []int
	i := 0
	for mask > 0 {
		if mask&1 == 1 {
			out = append(out, offset+i)
		}
		mask >>= 1
		i++
	}
	return out
}

// ListToStrRange converts a list of integers to a string representation of ranges.
//
// The input is a slice of integers, and the output is a string that represents consecutive integers as ranges.
// For example, if the input is [0, 1, 2, 4, 5, 7],
// the output will be "0-2,4-5,7" because 0, 1, and 2 are consecutive (0-2), 4 and 5 are consecutive (4-5), and 7 is standalone.
func ListToStrRange[I constraints.Integer](indices []I) string {
	sets.NewInt()
	if len(indices) == 0 {
		return ""
	}

	slices.Sort(indices)

	start := indices[0]
	end := start

	var parts []string
	for _, v := range indices[1:] {
		if v == end+1 {
			end = v
		} else {
			parts = append(parts, formatRange(start, end))
			start, end = v, v
		}
	}
	parts = append(parts, formatRange(start, end))

	return strings.Join(parts, ",")
}

// StrRangeToList converts a string representation of ranges to a list of integers.
//
// The input is a string that represents integers and ranges (e.g., "0-2,4,6-7").
// The output is a slice of integers that includes all the integers specified in the input string.
// For example, if the input is "0-2,4,6-7", the output will be [0, 1, 2, 4, 6, 7] because 0-2 represents 0, 1, and 2; 4 is standalone; and 6-7 represents 6 and 7.
func StrRangeToList(s string) []int {
	if s == "" {
		return nil
	}

	set := map[int]struct{}{}

	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)

		if strings.Contains(part, "-") {
			p := strings.Split(part, "-")
			lo := safeInt(p[0], -1)
			hi := safeInt(p[1], -1)
			if lo >= 0 && hi >= lo {
				for i := lo; i <= hi; i++ {
					set[i] = struct{}{}
				}
			}
		} else {
			i := safeInt(part, -1)
			if i >= 0 {
				set[i] = struct{}{}
			}
		}
	}

	out := make([]int, 0, len(set))
	for v := range set {
		out = append(out, v)
	}
	sort.Ints(out)
	return out
}

type (
	// PCIDevice represents a PCI device with its relevant information for topology and affinity calculations.
	PCIDevice struct {
		// Address is the PCI address of the device,
		// formatted as "domain:bus:device.function" (e.g., "0000:00:1f.2").
		Address PCIDeviceAddress
		// Class is the PCI class of the device,
		// represented as a hexadecimal string (e.g., "020000" for a network controller).
		Class string
		// Vendor is the PCI vendor ID of the device,
		// represented as a hexadecimal string (e.g., "8086" for Intel).
		Vendor string
		// Device is the PCI device ID,
		// represented as a hexadecimal string (e.g., "1234").
		Device string
		// Path is the sysfs path to the PCI device (e.g., "/sys/bus/pci/devices/0000:00:1f.2").
		Path string
		// Root is the root complex ID of the PCI device,
		// which can be used to determine if two devices are on the same PCI root complex.
		Root string
		// Config is the raw PCI configuration space of the device,
		// read from the "config" file in sysfs.
		Config []byte
		// SubVendor is the PCI subsystem vendor ID of the device,
		// represented as a hexadecimal string (e.g., "8086" for Intel).
		SubVendor string
		// SubDevice is the PCI subsystem device ID,
		// represented as a hexadecimal string (e.g., "5678").
		SubDevice string
		// Switches is a list of PCI switch IDs that are upstream of the device,
		// which can be used to determine if two devices share the same PCI switch.
		Switches []string
	}
	// PCIDeviceAddress represents the PCI address of a device, formatted as "domain:bus:device.function" (e.g., "0000:00:1f.2").
	PCIDeviceAddress = string
	// PCIDevices is a mapping of PCI device addresses to their corresponding PCIDevice information.
	PCIDevices map[PCIDeviceAddress]PCIDevice
)

// GetPCIDevices returns a list of PCI devices that match the specified criteria.
func GetPCIDevices(vendors, classPrefixes []string) PCIDevices {
	return getPCIDevices(vendors, classPrefixes)
}

// ComparePCIDevices compares two PCI devices and returns an integer indicating their relationship.
func ComparePCIDevices(a, b PCIDevice) int {
	if a.Root != "" && b.Root != "" {
		isSameRoot := a.Root == b.Root

		isSameSwitch := isSameRoot && len(a.Switches) == len(b.Switches)
		if isSameSwitch {
			for i := range a.Switches {
				if a.Switches[i] != b.Switches[i] {
					isSameSwitch = false
					break
				}
			}
		}

		if isSameSwitch {
			return 1
		}

		if isSameRoot {
			return 0
		}
	}

	return -1
}

type (
	// PCIDeviceNameID represents the unique identifier for a PCI device based on its vendor and device IDs.
	PCIDeviceNameID struct {
		// Vendor is the PCI vendor ID, represented as a hexadecimal string (e.g., "8086" for Intel).
		Vendor string
		// Device is the PCI device ID, represented as a hexadecimal string (e.g., "1234").
		Device string
	}
	// PCIDeviceName represents the human-readable name of a PCI device, along with its subsystem devices.
	PCIDeviceName struct {
		// Name is the human-readable name of the PCI device (e.g., "Intel(R) Ethernet Controller X550").
		Name string
		// SubDevices is a mapping of subsystem vendor and device IDs to their human-readable names,
		SubDevices PCIDeviceNames
	}
	// PCIDeviceNames is a mapping of PCI device identifiers to their human-readable names.
	PCIDeviceNames map[PCIDeviceNameID]PCIDeviceName
)

// GetName returns the human-readable name of a PCI device based on its vendor and device IDs.
func (n PCIDeviceNames) GetName(vendor, device, subvendor, subdevice string) string {
	nameID := PCIDeviceNameID{
		Vendor: vendor,
		Device: device,
	}
	if name, ok := n[nameID]; ok {
		if len(name.SubDevices) != 0 && subvendor != "" && subdevice != "" {
			subnameID := PCIDeviceNameID{
				Vendor: subvendor,
				Device: subdevice,
			}
			if subname, ok := name.SubDevices[subnameID]; ok {
				return subname.Name
			}
		}
		return name.Name
	}
	return ""
}

// GetPCIDeviceNames returns a mapping of PCI device names based on the specified vendor IDs.
func GetPCIDeviceNames(vendors []string) PCIDeviceNames {
	return getPCIDeviceNames(vendors)
}

// GetLibFromPaths returns the first existing library path from the provided list of paths.
func GetLibFromPaths(paths []string) string {
	if len(paths) == 0 {
		return ""
	}

	for i := range paths {
		if filepath.IsAbs(paths[i]) {
			if osx.Exists(paths[i]) {
				klog.V(3).InfoS("found library in absolute path", "path", paths[i])
				return paths[i]
			}
			continue
		}
		if path := getLibFromEnv(paths[i]); path != "" {
			klog.V(3).InfoS("found library in environment variable", "name", paths[i], "path", path)
			return path
		}
		if path := getLibFromLdCache(paths[i]); path != "" {
			klog.V(3).InfoS("found library in ld cache", "name", paths[i], "path", path)
			return path
		}
	}

	klog.V(3).InfoS("used first library", "name", paths[0])
	return paths[0]
}

func formatRange[T constraints.Integer](lo, hi T) string {
	if lo == hi {
		return strconv.FormatUint(uint64(lo), 10)
	}
	return strconv.FormatUint(uint64(lo), 10) + "-" + strconv.FormatUint(uint64(hi), 10)
}

func getBitsSize() int {
	if ^uint(0)>>32 > 0 {
		return 64
	}
	return 32
}

func getNumaNodeCPUMapping() map[int][]int {
	res := map[int][]int{}
	for cpu, node := range cpuNumaNodeMapping {
		if node >= 0 {
			res[node] = append(res[node], cpu)
		}
	}
	return res
}

func safeInt(s string, def int) int {
	if v, err := strconv.Atoi(strings.TrimSpace(s)); err == nil {
		return v
	}
	return def
}

func readText(path string) (string, error) { // nolint:unused
	b, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(b)), nil
}

func readBinary(path string) ([]byte, error) { // nolint:unused
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return b, nil
}

// hasPrefix returns true if the string s has any of the specified prefixes.
//
// if the prefixes slice is empty, it returns true,
// meaning that any string is considered to have a valid prefix.
func hasPrefix(s string, prefixes []string) bool { // nolint:unused
	if len(prefixes) == 0 {
		return true
	}
	idx := slices.IndexFunc(prefixes, func(p string) bool {
		return strings.HasPrefix(s, p)
	})
	return idx >= 0
}

// contains returns true if the string s is in the slice of strings strs.
//
// if the strs is empty, it returns true,
// meaning that any string is considered to be contained in the set.
func contains(s string, strs []string) bool { // nolint:unused
	if len(strs) == 0 {
		return true
	}

	return slices.Contains(strs, s)
}

type SystemDevice struct {
	Path     string
	Type     string
	Major    int
	Minor    int
	FileMode int
	UID      int
	GID      int
}

// GetSystemDeviceFromPath returns a SystemDevice struct containing information about the device at the specified path.
func GetSystemDeviceFromPath(path string) *SystemDevice {
	return getSystemDeviceFromPath(path)
}
