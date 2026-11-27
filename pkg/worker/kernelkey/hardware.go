package kernelkey

import (
	"fmt"
	"regexp"
	"sort"
	"strings"

	core "k8s.io/api/core/v1"

	"gpustack.ai/gpustack/pkg/nodefeature"
)

var (
	nvidiaDotComCapPattern = regexp.MustCompile(`^(\d+)\.(\d+)([a-z]*)$`)
	nvidiaSMOrComputeRegex = regexp.MustCompile(`^(?:sm|compute)_(\d+)([a-z]*)$`)
	safeArchPattern        = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]*$`)
)

// CompatibilityTarget describes the engine and accelerator environment against
// which a LocalKernelKey or ModelKernelArtifact compatibility spec is matched.
type CompatibilityTarget struct {
	Engine        string
	EngineVersion string
	Manufacturer  string
	Arch          string
}

// NodeHardware describes one accelerator group discovered from a Kubernetes Node's
// pkg/nodefeature labels, along with exact node-selector labels for targeting it.
type NodeHardware struct {
	Key               string
	Manufacturer      string
	Arch              string
	ComputeCapability string
	Family            string
	Product           string
	SelectorLabels    map[string]string
}

// NormalizeManufacturer validates and lowercases an accelerator manufacturer
// against pkg/nodefeature's known acceleratable manufacturer registry.
func NormalizeManufacturer(manufacturer string) (string, error) {
	m := strings.ToLower(strings.TrimSpace(manufacturer))
	if m == "" {
		return "", fmt.Errorf("manufacturer must not be empty")
	}
	if !nodefeature.IsKnownAcceleratableManufacturer(m) {
		return "", fmt.Errorf("unsupported manufacturer %q: known manufacturers %v",
			manufacturer, nodefeature.GetKnownAcceleratableManufacturers())
	}
	return m, nil
}

// NormalizeArch normalizes an accelerator architecture, compute capability, or
// family string into the canonical lowercase LocalKernelKey arch segment for
// the given manufacturer.
func NormalizeArch(manufacturer, arch string) (string, error) {
	manu, err := NormalizeManufacturer(manufacturer)
	if err != nil {
		return "", err
	}
	raw := strings.ToLower(strings.TrimSpace(arch))
	if raw == "" {
		return "", fmt.Errorf("arch must not be empty")
	}
	if strings.ContainsAny(raw, " \t\r\n/") {
		return "", fmt.Errorf("arch %q contains invalid characters", arch)
	}

	switch manu {
	case nodefeature.ManufacturerNVIDIA:
		if m := nvidiaSMOrComputeRegex.FindStringSubmatch(raw); m != nil {
			return "sm_" + m[1] + m[2], nil
		}
		if m := nvidiaDotComCapPattern.FindStringSubmatch(raw); m != nil {
			return "sm_" + m[1] + m[2] + m[3], nil
		}
	case nodefeature.ManufacturerAMD, nodefeature.ManufacturerHygon:
		// ROCm / DTK target IDs sometimes append feature flags (e.g. gfx942:sramecc+:xnack-).
		if colon := strings.IndexByte(raw, ':'); colon > 0 {
			raw = raw[:colon]
		}
	case nodefeature.ManufacturerAscend:
		raw = strings.TrimPrefix(raw, "ascend-")
		raw = strings.TrimPrefix(raw, "ascend")
	default:
		raw = strings.TrimPrefix(raw, manu+"-")
	}

	if !safeArchPattern.MatchString(raw) {
		return "", fmt.Errorf("invalid arch %q for manufacturer %q", arch, manu)
	}
	return raw, nil
}

// ArchCompatible reports whether two architecture strings for manufacturer are
// compatible after canonical normalization.
//
// For NVIDIA, a node reporting compute capability without a variant suffix
// (such as "9.0" -> "sm_90" from NVML) matches an architecture-accelerated target
// ("sm_90a") on the same base SM capability, while distinct explicit variants
// or different SM levels do not match.
func ArchCompatible(manufacturer, a, b string) (bool, error) {
	manu, err := NormalizeManufacturer(manufacturer)
	if err != nil {
		return false, err
	}
	na, err := NormalizeArch(manu, a)
	if err != nil {
		return false, err
	}
	nb, err := NormalizeArch(manu, b)
	if err != nil {
		return false, err
	}
	if na == nb {
		return true, nil
	}

	if manu == nodefeature.ManufacturerNVIDIA {
		baseA, varA := splitNvidiaSM(na)
		baseB, varB := splitNvidiaSM(nb)
		if baseA != "" && baseA == baseB && (varA == "" || varB == "") {
			return true, nil
		}
	}
	return false, nil
}

func splitNvidiaSM(arch string) (base, variant string) {
	m := nvidiaSMOrComputeRegex.FindStringSubmatch(arch)
	if m == nil {
		return "", ""
	}
	return "sm_" + m[1], m[2]
}

// ExtractNodeHardwareFromNode extracts accelerator hardware descriptors from
// a Kubernetes Node's pkg/nodefeature labels.
func ExtractNodeHardwareFromNode(node *core.Node) []NodeHardware {
	if node == nil {
		return nil
	}
	return ExtractNodeHardware(node.Labels)
}

// ExtractNodeHardware extracts accelerator hardware descriptors and selector
// labels from a pkg/nodefeature label map.
func ExtractNodeHardware(labels map[string]string) []NodeHardware {
	keys := nodefeature.ExtractAcceleratableKeys(labels)
	if len(keys) == 0 {
		return nil
	}
	sort.Strings(keys)

	out := make([]NodeHardware, 0, len(keys))
	for _, aKey := range keys {
		manu := nodefeature.AcceleratableManufacturer(aKey)
		prefix := nodefeature.AcceleratableFeatureLabelPrefix + aKey
		comcap := labels[prefix+".comcap"]
		family := labels[prefix+".family"]
		product := labels[prefix+".product"]

		rawArch := comcap
		if rawArch == "" {
			rawArch = family
		}
		if rawArch == "" && product != "" {
			rawArch = nodefeature.PresetAcceleratorFamily(manu, product)
		}

		arch := ""
		if rawArch != "" {
			arch, _ = NormalizeArch(manu, rawArch)
		}

		sel := map[string]string{
			prefix: "true",
		}
		if comcap != "" {
			sel[prefix+".comcap"] = comcap
		} else if family != "" {
			sel[prefix+".family"] = family
		}

		out = append(out, NodeHardware{
			Key:               aKey,
			Manufacturer:      manu,
			Arch:              arch,
			ComputeCapability: comcap,
			Family:            family,
			Product:           product,
			SelectorLabels:    sel,
		})
	}
	return out
}

// MatchHardware reports whether a NodeHardware descriptor satisfies the requested
// manufacturer and architecture.
func MatchHardware(manufacturer, arch string, hw NodeHardware) bool {
	manu, err := NormalizeManufacturer(manufacturer)
	if err != nil {
		return false
	}
	if hw.Manufacturer != manu {
		return false
	}
	if hw.Arch == "" {
		return false
	}
	ok, err := ArchCompatible(manu, arch, hw.Arch)
	return err == nil && ok
}

// MatchNodeLabels finds the first pkg/nodefeature accelerator group in labels
// matching manufacturer and architecture.
func MatchNodeLabels(manufacturer, arch string, labels map[string]string) (NodeHardware, bool) {
	for _, hw := range ExtractNodeHardware(labels) {
		if MatchHardware(manufacturer, arch, hw) {
			return hw, true
		}
	}
	return NodeHardware{}, false
}

// MatchCompatibility verifies that key is valid and matches target across
// engine, normalized engineVersion, manufacturer, and architecture.
func MatchCompatibility(key LocalKernelKey, target CompatibilityTarget) error {
	if err := key.Validate(); err != nil {
		return fmt.Errorf("invalid kernel key: %w", err)
	}

	targetEngine, err := NormalizeEngine(target.Engine)
	if err != nil {
		return fmt.Errorf("invalid target engine: %w", err)
	}
	if key.Engine != targetEngine {
		return fmt.Errorf("engine mismatch: key %q requires %q, target has %q",
			key.String(), key.Engine, targetEngine)
	}

	targetVersion, err := NormalizeEngineVersion(target.EngineVersion)
	if err != nil {
		return fmt.Errorf("invalid target engineVersion: %w", err)
	}
	if key.EngineVersion != targetVersion {
		return fmt.Errorf("engineVersion mismatch: key %q requires %q, target has %q",
			key.String(), key.EngineVersion, targetVersion)
	}

	targetManu, err := NormalizeManufacturer(target.Manufacturer)
	if err != nil {
		return fmt.Errorf("invalid target manufacturer: %w", err)
	}
	if key.Manufacturer != targetManu {
		return fmt.Errorf("manufacturer mismatch: key %q requires %q, target has %q",
			key.String(), key.Manufacturer, targetManu)
	}

	ok, err := ArchCompatible(key.Manufacturer, key.Arch, target.Arch)
	if err != nil {
		return fmt.Errorf("invalid target arch %q: %w", target.Arch, err)
	}
	if !ok {
		return fmt.Errorf("arch mismatch: key %q requires %q, target has %q",
			key.String(), key.Arch, target.Arch)
	}
	return nil
}
