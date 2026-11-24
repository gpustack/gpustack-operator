package mockspec

import (
	"slices"
	"testing"
	"testing/fstest"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"gpustack.ai/gpustack/pkg/device"
	"gpustack.ai/gpustack/pkg/nodefeature"
)

func TestBuiltInPresets(t *testing.T) {
	testCases := []struct {
		name         string
		manufacturer string
		wantProduct  string
		wantMemory   uint64
		wantCount    int
		wantMIG      bool
	}{
		{
			name:         "nvidia-h100",
			manufacturer: nodefeature.ManufacturerNVIDIA,
			wantProduct:  "NVIDIA H100 80GB HBM3",
			wantMemory:   81559,
			wantCount:    8,
		},
		{
			name:         "nvidia-h100-mig",
			manufacturer: nodefeature.ManufacturerNVIDIA,
			wantProduct:  "NVIDIA H100 80GB HBM3",
			wantMemory:   81559,
			wantCount:    8,
			wantMIG:      true,
		},
		{
			name:         "nvidia-h200",
			manufacturer: nodefeature.ManufacturerNVIDIA,
			wantProduct:  "NVIDIA H200",
			wantMemory:   143771,
			wantCount:    8,
		},
		{
			name:         "nvidia-rtx-4090-48g",
			manufacturer: nodefeature.ManufacturerNVIDIA,
			wantProduct:  "NVIDIA GeForce RTX 4090",
			wantMemory:   49140,
			wantCount:    2,
		},
		{
			name:         "nvidia-rtx-5090d",
			manufacturer: nodefeature.ManufacturerNVIDIA,
			wantProduct:  "NVIDIA GeForce RTX 5090 D",
			wantMemory:   32607,
			wantCount:    1,
		},
		{
			name:         "nvidia-gb10",
			manufacturer: nodefeature.ManufacturerNVIDIA,
			wantProduct:  "NVIDIA GB10",
			wantMemory:   138932,
			wantCount:    1,
		},
		{
			name:         "amd-mi300x",
			manufacturer: nodefeature.ManufacturerAMD,
			wantProduct:  "AMD Instinct MI300X OAM",
			wantMemory:   196592,
			wantCount:    2,
		},
		{
			name:         "amd-mi308x",
			manufacturer: nodefeature.ManufacturerAMD,
			wantProduct:  "AMD Instinct MI308X OAM",
			wantMemory:   196592,
			wantCount:    8,
		},
		{
			name:         "amd-rx-7800-xt",
			manufacturer: nodefeature.ManufacturerAMD,
			wantProduct:  "AMD Radeon RX 7800 XT",
			wantMemory:   16368,
			wantCount:    2,
		},
		{
			name:         "ascend-910b2",
			manufacturer: nodefeature.ManufacturerAscend,
			wantProduct:  "910B2",
			wantMemory:   65536,
			wantCount:    8,
		},
		{
			name:         "ascend-310p3",
			manufacturer: nodefeature.ManufacturerAscend,
			wantProduct:  "310P3",
			wantMemory:   44280,
			wantCount:    2,
		},
		{
			name:         "hygon-k100-ai",
			manufacturer: nodefeature.ManufacturerHygon,
			wantProduct:  "K100_AI",
			wantMemory:   65520,
			wantCount:    8,
		},
		{
			name:         "hygon-k100-ai-mig",
			manufacturer: nodefeature.ManufacturerHygon,
			wantProduct:  "K100_AI",
			wantMemory:   65520,
			wantCount:    8,
			wantMIG:      true,
		},
		{
			name:         "metax-c500",
			manufacturer: nodefeature.ManufacturerMetaX,
			wantProduct:  "MXC500",
			wantMemory:   65536,
			wantCount:    8,
		},
		{
			name:         "mthreads-s5000",
			manufacturer: nodefeature.ManufacturerMThreads,
			wantProduct:  "MTT S5000",
			wantMemory:   81920,
			wantCount:    8,
		},
		{
			name:         "thead-ppu",
			manufacturer: nodefeature.ManufacturerTHead,
			wantProduct:  "PPU-ZW810E",
			wantMemory:   98304,
			wantCount:    16,
		},
		{
			name:         "thead-ppu-mig",
			manufacturer: nodefeature.ManufacturerTHead,
			wantProduct:  "PPU-ZW810E",
			wantMemory:   98304,
			wantCount:    8,
			wantMIG:      true,
		},
		{
			name:         "cambricon-mlu590",
			manufacturer: nodefeature.ManufacturerCambricon,
			wantProduct:  "MLU590",
			wantMemory:   81920,
			wantCount:    8,
		},
		{
			name:         "iluvatar-bi-v150",
			manufacturer: nodefeature.ManufacturerIluvatar,
			wantProduct:  "Iluvatar BI-V150",
			wantMemory:   32768,
			wantCount:    8,
		},
	}

	names := PresetNames()
	require.Len(t, names, len(testCases))
	assert.True(t, slices.IsSorted(names))

	coveredManufacturers := make(map[string]bool)

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			require.Contains(t, names, tc.name)

			mfr, spec, ok := LookupPreset(tc.name)
			require.True(t, ok)
			assert.Equal(t, tc.manufacturer, mfr)
			coveredManufacturers[mfr] = true

			doc, err := Parse([]byte("preset: " + tc.name + "\n"))
			require.NoError(t, err)

			resolved := doc.ResolveNode("worker-0")
			mfrSpec, exists := resolved.Manufacturers[tc.manufacturer]
			require.True(t, exists)
			require.Len(t, mfrSpec.Slots, tc.wantCount)

			groups := mfrSpec.BuildGroups()
			require.Len(t, groups, 1)
			grp := groups[0]
			assert.Equal(t, tc.manufacturer, grp.Manufacturer)
			assert.Equal(t, tc.wantProduct, grp.Name)
			assert.Equal(t, tc.wantMemory, grp.Memory)
			assert.NotEmpty(t, grp.ID)
			require.Len(t, grp.Accelerators, tc.wantCount)

			for i, accel := range grp.Accelerators {
				assert.NotEmpty(t, accel.ID)
				assert.Equal(t, uint32(i), accel.Index)
				assert.NotEmpty(t, accel.PhysicalIndexes)
				if tc.wantMIG {
					assert.True(t, device.IsPartitioned(accel.Status))
					assert.False(t, device.IsLogicallySliceable(accel.Status))
					assert.NotEmpty(t, accel.Status.PhysicalSliced.Profiles)
				} else {
					assert.True(t, device.IsLogicallySliceable(accel.Status))
					assert.False(t, device.IsPartitioned(accel.Status))
				}
			}

			labels := nodefeature.ConstructAcceleratableNodeLabels(groups)
			assert.Equal(t, "true", labels[nodefeature.NodeAcceleratableLabelKey])

			// Mutating a returned preset copy must never affect subsequent lookups.
			spec.Groups[0].Name = "mutated-name"
			_, fresh, ok := LookupPreset(tc.name)
			require.True(t, ok)
			assert.Equal(t, tc.wantProduct, fresh.Groups[0].Name)
		})
	}

	for _, known := range nodefeature.GetKnownAcceleratableManufacturers() {
		assert.True(t, coveredManufacturers[known], "missing preset coverage for manufacturer %s", known)
	}

	_, _, ok := LookupPreset("does-not-exist")
	assert.False(t, ok)
}

func TestLoadPresetsFromFSErrors(t *testing.T) {
	testCases := []struct {
		name    string
		files   fstest.MapFS
		wantErr string
	}{
		{
			name:    "empty preset directory",
			files:   fstest.MapFS{},
			wantErr: "presets",
		},
		{
			name: "invalid yaml in preset",
			files: fstest.MapFS{
				"presets/bad.yaml": &fstest.MapFile{Data: []byte(":::not-yaml")},
			},
			wantErr: "bad",
		},
		{
			name: "nested preset reference inside builtin preset is forbidden",
			files: fstest.MapFS{
				"presets/nested.yaml": &fstest.MapFile{Data: []byte("manufacturer: nvidia\npreset: nvidia-h100\n")},
			},
			wantErr: "preset",
		},
		{
			name: "builtin preset missing groups",
			files: fstest.MapFS{
				"presets/empty.yaml": &fstest.MapFile{Data: []byte("manufacturer: nvidia\n")},
			},
			wantErr: "groups",
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := loadPresetsFromFS(tc.files)
			require.Error(t, err)
			assert.ErrorContains(t, err, tc.wantErr)
		})
	}
}
