package device

import (
	"os"
	"regexp"
	"slices"
	"strings"

	"k8s.io/apimachinery/pkg/api/resource"

	"gpustack.ai/gpustack/binding"
	"gpustack.ai/gpustack/pkg/utils/quantityx"
	"gpustack.ai/gpustack/pkg/utils/strconvx"
	"gpustack.ai/gpustack/pkg/utils/typex"
)

// Toggle of the functions.
var (
	constructGroupIDWithMemory bool
)

func init() {
	constructGroupIDWithMemory = os.Getenv("GPUSTACK_DEVICES_GROUP_ID_WITH_MEMORY") == "true"
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

// ConstructGroupID constructs a group ID from the given name,
// removes the manufacturer prefix and formats the memory size if enabled.
func ConstructGroupID(manufacturer, name string, memory uint64) string {
	// ${manufacturer}-${name}[-${memory}], limit to 63 characters in total.
	budget := 63 - len(manufacturer) - 1
	if constructGroupIDWithMemory {
		budget -= 7 // 7 is the max length of the suffix "-memory".
	}
	n := NormalizeName(name, manufacturer, budget, false)
	if !constructGroupIDWithMemory {
		return n
	}
	m := formatMemory(memory)
	return n + "-" + m
}

var (
	// _CPUNameTrademarkMarkerReplacer drops the trademark markers carried by CPU product names.
	_CPUNameTrademarkMarkerReplacer = strings.NewReplacer("(R)", "", "(r)", "", "(TM)", "", "(tm)", "")

	// _CPUNamePrefixCruft matches the leading cruft carried by CPU product names,
	// e.g. "Genuine " and "11th Gen ".
	_CPUNamePrefixCruft = regexp.MustCompile(`^(Genuine |[0-9]+(st|nd|rd|th) Gen )+`)

	// _CPUNameCoreCountSuffix matches the trailing core-count carried by CPU product
	// names, e.g. " 64-Core" and " 16-Cores".
	_CPUNameCoreCountSuffix = regexp.MustCompile(`(?i) [0-9]+-cores?$`)
)

// NormalizeName sanitizes the given device or CPU product name into a Kubernetes
// label-safe slug: it lowercases letters, keeps only [a-z0-9-_.], converts
// whitespace to "-", collapses consecutive separators, trims the leading prefix
// when matched case-insensitively, and trims leading/trailing separators.
// When stripCruft is true, it first strips CPU-name cruft: the "@ <freq>" tail,
// trademark markers like "(R)"/"(TM)", the "Genuine"/"Nth Gen" leading words, the
// trailing "CPU"/"Processor" words, and the trailing "<n>-Core(s)" count.
// When maxLength is positive, the result is truncated to at most maxLength runes.
func NormalizeName(name, prefix string, maxLength int, stripCruft bool) string {
	if stripCruft {
		name, _, _ = strings.Cut(name, " @ ")
		name = _CPUNameTrademarkMarkerReplacer.Replace(name)
		name = _CPUNamePrefixCruft.ReplaceAllString(name, "")
		name = strings.TrimSpace(name)
		name = strings.TrimSuffix(name, " CPU")
		name = strings.TrimSuffix(name, " Processor")
		name = _CPUNameCoreCountSuffix.ReplaceAllString(name, "")
	}

	if maxLength <= 0 {
		maxLength = len(name)
	}

	buf := make([]rune, 0, min(len(name), maxLength))
	var pr rune
	for _, r := range name {
		switch {
		case r >= 'A' && r <= 'Z':
			r += 'a' - 'A'
		case r >= 'a' && r <= 'z':
		case r >= '0' && r <= '9':
		case r == ' ':
			r = '-'
			fallthrough
		case r == '-' || r == '_' || r == '.':
			if len(buf) == 0 {
				continue
			}
			if pr == '-' || pr == '_' || pr == '.' {
				continue
			}
		default:
			continue
		}
		buf = append(buf, r)
		pr = r

		// Trim prefix.
		if len(buf) == len(prefix) {
			if strings.EqualFold(string(buf), prefix) {
				buf = buf[:0]
				pr = 0
			}
		}

		// Stop processing if the buffer has reached the maximum length.
		if len(buf) >= maxLength {
			break
		}
	}

	if len(buf) == 0 {
		return ""
	}

	// Trim trailing non-alphanumeric character.
	if c := buf[len(buf)-1]; c == '-' || c == '_' || c == '.' {
		buf = buf[:len(buf)-1]
	}

	return string(buf)
}

// ConstructTopology constructs a Topology for the given PCI bus ID, root ID, class and upstream
// switch path.
//
// pciSwitches is stored as a COPY. Its ordering is the caller's — innermost first, as
// binding.GetPCIDevices produces it — and it has to stay stable across passes: a reordered slice is
// NOT equal under the semantic equality the detector compares with (measured), so an unstable order
// would make it rewrite the object every pass forever, with no wrong value anywhere to notice it
// by. Every current caller hands over a freshly allocated slice it never touches again, so the copy
// changes nothing today; it is what keeps the stability a property of this value rather than of
// caller discipline, since a caller passing a reused scratch buffer would reorder a stored topology
// with no symptom but write volume. A nil and an empty slice ARE equal there, so neither needs
// normalising.
func ConstructTopology(pciBusId, pciRootId, pciClass string, pciSwitches []string) Topology {
	return Topology{
		PciBusID:     pciBusId,
		PciRootID:    pciRootId,
		PciClass:     pciClass,
		PciSwitches:  slices.Clone(pciSwitches),
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

// formatMemory formats the given memory size in MiB to a string.
func formatMemory(mib uint64) string {
	switch {
	case mib == 0:
		return "0g"
	case mib < 1024:
		return "1g"
	}
	r := strings.ToLower(quantityx.Format(resource.MustParse(strconvx.Itoa(mib) + "Mi")))
	return r[:len(r)-1] // Remove the "i" in "gi".
}

// RuntimeMajor returns the major component of a normalized runtime version (e.g.
// "12.4" -> "12", "9.0" -> "9", "8" -> "8"), falling back to fallback when the version
// is empty.
func RuntimeMajor(runtimeVersion, fallback string) string {
	if runtimeVersion == "" {
		return fallback
	}
	return strings.SplitN(runtimeVersion, ".", 2)[0]
}
