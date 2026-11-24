package mockspec

import (
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"path"
	"slices"
	"sort"
	"strings"

	"sigs.k8s.io/yaml"

	"gpustack.ai/gpustack/pkg/nodefeature"
)

//go:embed presets/*.yaml
var embeddedPresetsFS embed.FS

type presetEntry struct {
	manufacturer string
	groups       []GroupSpec
}

// presetFileSpec is the on-disk schema of an embedded preset YAML file.
type presetFileSpec struct {
	Manufacturer string      `json:"manufacturer"`
	Preset       string      `json:"preset,omitempty"`
	Groups       []GroupSpec `json:"groups"`
}

var builtInPresets = mustLoadPresets(embeddedPresetsFS)

// PresetNames returns the sorted list of built-in hardware preset names.
func PresetNames() []string {
	names := slices.Collect(maps.Keys(builtInPresets))
	sort.Strings(names)
	return names
}

// LookupPreset returns the manufacturer and a deep-copied ManufacturerSpec for a built-in preset.
func LookupPreset(name string) (string, ManufacturerSpec, bool) {
	entry, ok := builtInPresets[name]
	if !ok {
		return "", ManufacturerSpec{}, false
	}
	return entry.manufacturer, ManufacturerSpec{Groups: cloneGroups(entry.groups)}, true
}

func mustLoadPresets(fsys fs.FS) map[string]presetEntry {
	table, err := loadPresetsFromFS(fsys)
	if err != nil {
		panic(fmt.Sprintf("mockspec: invalid built-in presets: %v", err))
	}
	return table
}

func loadPresetsFromFS(fsys fs.FS) (map[string]presetEntry, error) {
	entries, err := fs.ReadDir(fsys, "presets")
	if err != nil {
		return nil, fmt.Errorf("read presets directory: %w", err)
	}

	table := make(map[string]presetEntry, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".yaml") {
			continue
		}
		name := strings.TrimSuffix(entry.Name(), ".yaml")
		raw, err := fs.ReadFile(fsys, path.Join("presets", entry.Name()))
		if err != nil {
			return nil, fmt.Errorf("read preset %q: %w", name, err)
		}

		var fileSpec presetFileSpec
		if err := yaml.Unmarshal(raw, &fileSpec, yaml.DisallowUnknownFields); err != nil {
			return nil, fmt.Errorf("parse preset %q: %w", name, err)
		}
		if fileSpec.Preset != "" {
			return nil, fmt.Errorf("preset %q must not reference another preset %q", name, fileSpec.Preset)
		}
		if !nodefeature.IsKnownAcceleratableManufacturer(fileSpec.Manufacturer) {
			return nil, fmt.Errorf("preset %q declares unknown manufacturer %q", name, fileSpec.Manufacturer)
		}
		if len(fileSpec.Groups) == 0 {
			return nil, fmt.Errorf("preset %q must declare non-empty groups", name)
		}

		mfrSpec := ManufacturerSpec{
			Groups: fileSpec.Groups,
		}
		if _, err := resolveManufacturerSpec("preset "+name, fileSpec.Manufacturer, mfrSpec, nil); err != nil {
			return nil, fmt.Errorf("validate preset %q: %w", name, err)
		}

		table[name] = presetEntry{
			manufacturer: fileSpec.Manufacturer,
			groups:       cloneGroups(fileSpec.Groups),
		}
	}

	if len(table) == 0 {
		return nil, errors.New("no preset files found under presets")
	}
	return table, nil
}

func cloneGroups(in []GroupSpec) []GroupSpec {
	if in == nil {
		return nil
	}
	out := make([]GroupSpec, len(in))
	for i := range in {
		out[i] = cloneGroupSpec(in[i])
	}
	return out
}

func cloneGroupSpec(in GroupSpec) GroupSpec {
	out := in
	if in.Accelerators != nil {
		out.Accelerators = make([]AcceleratorSpec, len(in.Accelerators))
		for i := range in.Accelerators {
			out.Accelerators[i] = cloneAcceleratorSpec(in.Accelerators[i])
		}
	}
	return out
}

func cloneAcceleratorSpec(in AcceleratorSpec) AcceleratorSpec {
	out := in
	if in.Index != nil {
		v := *in.Index
		out.Index = &v
	}
	if in.PhysicalIndexes != nil {
		out.PhysicalIndexes = slices.Clone(in.PhysicalIndexes)
	}
	in.Topology.DeepCopyInto(&out.Topology)
	if in.LogicalSliced != nil {
		cp := *in.LogicalSliced
		out.LogicalSliced = &cp
	}
	if in.PhysicalSliced != nil {
		cp := *in.PhysicalSliced.DeepCopy()
		out.PhysicalSliced = &cp
	}
	if in.Metrics != nil {
		cp := *in.Metrics
		if in.Metrics.MemoryUtilization != nil {
			u := *in.Metrics.MemoryUtilization
			cp.MemoryUtilization = &u
		}
		out.Metrics = &cp
	}
	return out
}
