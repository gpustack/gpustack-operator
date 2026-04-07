package device

import (
	"os"
	"strconv"
	"strings"

	"gpustack.ai/gpustack/binding"
	"gpustack.ai/gpustack/pkg/utils/typex"
)

// Toggle of the functions.
var (
	constructGroupIDWithMemory bool
)

func init() {
	constructGroupIDWithMemory = os.Getenv("GPUSTACK_DEVICES_DETECT_GROUP_ID_WITH_MEMORY") == "true"
}

// ConvertBytesToMiB converts the given bytes to MiB.
func ConvertBytesToMiB[T typex.Integer](bytes T) uint64 {
	return uint64(bytes) >> 20
}

// ConvertKiBToMiB converts the given KiB to MiB.
func ConvertKiBToMiB[T typex.Integer](kib T) uint64 {
	return uint64(kib) >> 10
}

// NormalizeVersion normalizes the given version string to the format "major.minor".
func NormalizeVersion(ver string) string {
	if ver == "" {
		return ""
	}
	ps := strings.Split(ver, ".")
	if len(ps) < 2 {
		return ver
	}
	return ps[0] + "." + ps[1]
}

// ConstructGroupID constructs a group ID for the given manufacturer, name, and memory.
func ConstructGroupID(manufacturer, name string, memory uint64) string {
	n := formatName(name, manufacturer)
	if !constructGroupIDWithMemory {
		return n
	}
	m := formatMemory(memory, manufacturer == "ascend")
	return n + "-" + m
}

// ConstructTopology constructs a Topology for the given PCI bus ID, root ID, and class.
func ConstructTopology(pciBusId, pciRootId, pciClass string) Topology {
	return Topology{
		PciBusID:     pciBusId,
		PciRootID:    pciRootId,
		PciClass:     pciClass,
		NumaAffinity: binding.GetNumaNodeByBDF(pciBusId),
		CpuAffinity:  binding.MapNumaNodeStrToCPUAffinity(binding.GetNumaNodeByBDF(pciBusId)),
	}
}

// CalculateUtilization calculates the utilization percentage based on the given usage and total values.
func CalculateUtilization[U, T typex.Integer](usage U, total T) uint32 {
	if usage <= 0 || total <= 0 {
		return 0
	}
	return uint32(float64(usage) / float64(total) * 100)
}

func formatName(name, manufacturer string) string {
	var b strings.Builder
	b.Grow(len(name))
	var pr rune
	for _, r := range name {
		switch {
		case r >= 'A' && r <= 'Z':
			r += 'a' - 'A'
		case r >= 'a' && r <= 'z':
		case r >= '0' && r <= '9':
		case r == '-' || r == '_' || r == '.' || r == ' ':
			if b.Len() == 0 {
				continue
			}
			if pr == '-' {
				continue
			}
			if r == ' ' {
				r = '-'
			}
		default:
			continue
		}
		b.WriteRune(r)
		pr = r

		if b.String() == manufacturer {
			b.Reset()
			pr = 0
		}
	}
	return b.String()
}

// formatMemory formats the given memory size in MiB to a string with the unit "Gi",
// and returns the formatted string.
func formatMemory(mb uint64, withBias bool) string {
	if mb == 0 {
		return "0gb"
	}
	gb := (mb + 1023) >> 10
	if withBias && (gb&1 != 0) {
		gb++
	}
	return strconv.FormatUint(gb, 10) + "gb"
}
