package kernelkey

import (
	"testing"

	core "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"gpustack.ai/gpustack/pkg/device"
	"gpustack.ai/gpustack/pkg/nodefeature"
)

func TestNormalizeManufacturerAndArch(t *testing.T) {
	cases := []struct {
		name         string
		manufacturer string
		arch         string
		wantManu     string
		wantArch     string
		wantErr      bool
	}{
		{name: "nvidia sm_90a", manufacturer: "NVIDIA", arch: "SM_90A", wantManu: "nvidia", wantArch: "sm_90a"},
		{name: "nvidia comcap 9.0", manufacturer: "nvidia", arch: "9.0", wantManu: "nvidia", wantArch: "sm_90"},
		{name: "nvidia comcap 9.0a", manufacturer: "nvidia", arch: "9.0a", wantManu: "nvidia", wantArch: "sm_90a"},
		{name: "nvidia comcap 8.0", manufacturer: "nvidia", arch: "8.0", wantManu: "nvidia", wantArch: "sm_80"},
		{name: "nvidia compute_89", manufacturer: "nvidia", arch: "compute_89", wantManu: "nvidia", wantArch: "sm_89"},
		{name: "nvidia comcap 10.0", manufacturer: "nvidia", arch: "10.0", wantManu: "nvidia", wantArch: "sm_100"},
		{name: "amd gfx942", manufacturer: "amd", arch: "GFX942", wantManu: "amd", wantArch: "gfx942"},
		{name: "amd gfx942 with target features", manufacturer: "amd", arch: "gfx942:sramecc+:xnack-", wantManu: "amd", wantArch: "gfx942"},
		{name: "hygon gfx928", manufacturer: "hygon", arch: "gfx928", wantManu: "hygon", wantArch: "gfx928"},
		{name: "ascend 910B", manufacturer: "ascend", arch: "910B", wantManu: "ascend", wantArch: "910b"},
		{name: "ascend-910b family", manufacturer: "ascend", arch: "ascend-910b", wantManu: "ascend", wantArch: "910b"},
		{name: "ascend 950", manufacturer: "ascend", arch: nodefeature.AcceleratorFamilyAscend950, wantManu: "ascend", wantArch: "950"},
		{name: "metax family", manufacturer: "metax", arch: "metax-mxc500", wantManu: "metax", wantArch: "mxc500"},
		{name: "empty manufacturer", manufacturer: "", arch: "sm_90a", wantErr: true},
		{name: "unknown manufacturer", manufacturer: "intel", arch: "pvc", wantErr: true},
		{name: "empty arch", manufacturer: "nvidia", arch: "", wantErr: true},
		{name: "arch with slash", manufacturer: "nvidia", arch: "sm_90a/bad", wantErr: true},
		{name: "arch with invalid chars", manufacturer: "nvidia", arch: "sm@90a", wantErr: true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			manu, err := NormalizeManufacturer(tc.manufacturer)
			if tc.wantErr {
				if err == nil {
					if _, archErr := NormalizeArch(manu, tc.arch); archErr == nil {
						t.Fatalf("expected error for (%q, %q), got nil", tc.manufacturer, tc.arch)
					}
				}
				return
			}
			if err != nil {
				t.Fatalf("NormalizeManufacturer(%q) unexpected error: %v", tc.manufacturer, err)
			}
			if manu != tc.wantManu {
				t.Fatalf("NormalizeManufacturer(%q) = %q, want %q", tc.manufacturer, manu, tc.wantManu)
			}
			arch, err := NormalizeArch(manu, tc.arch)
			if err != nil {
				t.Fatalf("NormalizeArch(%q, %q) unexpected error: %v", manu, tc.arch, err)
			}
			if arch != tc.wantArch {
				t.Fatalf("NormalizeArch(%q, %q) = %q, want %q", manu, tc.arch, arch, tc.wantArch)
			}
		})
	}
}

func TestArchCompatible(t *testing.T) {
	cases := []struct {
		name         string
		manufacturer string
		a            string
		b            string
		want         bool
		wantErr      bool
	}{
		{name: "nvidia sm_90a vs 9.0", manufacturer: "nvidia", a: "sm_90a", b: "9.0", want: true},
		{name: "nvidia distinct variants sm_90a vs sm_90f", manufacturer: "nvidia", a: "sm_90a", b: "sm_90f", want: false},
		{name: "nvidia distinct sm levels", manufacturer: "nvidia", a: "sm_90a", b: "sm_80", want: false},
		{name: "invalid manufacturer", manufacturer: "invalid", a: "sm_90", b: "sm_90", wantErr: true},
		{name: "invalid arch a", manufacturer: "nvidia", a: "", b: "sm_90", wantErr: true},
		{name: "invalid arch b", manufacturer: "nvidia", a: "sm_90", b: "", wantErr: true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ArchCompatible(tc.manufacturer, tc.a, tc.b)
			if (err != nil) != tc.wantErr {
				t.Fatalf("ArchCompatible err = %v, wantErr = %v", err, tc.wantErr)
			}
			if !tc.wantErr && got != tc.want {
				t.Fatalf("ArchCompatible = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestExtractNodeHardwareAndMatchLabels(t *testing.T) {
	nvidiaLabels := nodefeature.ConstructAcceleratableNodeLabels(device.DevicesGroupList{
		{
			ID:                "h100",
			Manufacturer:      nodefeature.ManufacturerNVIDIA,
			Name:              "NVIDIA-H100-80GB-HBM3",
			Memory:            81920,
			Cores:             16896,
			Family:            "Hopper",
			ComputeCapability: "9.0",
			Accelerators:      []device.Accelerator{{ID: "0"}},
		},
	})
	amdLabels := nodefeature.ConstructAcceleratableNodeLabels(device.DevicesGroupList{
		{
			ID:                "mi300x",
			Manufacturer:      nodefeature.ManufacturerAMD,
			Name:              "AMD-Instinct-MI300X",
			Memory:            196608,
			Cores:             304,
			Family:            "MI300",
			ComputeCapability: "gfx942",
			Accelerators:      []device.Accelerator{{ID: "0"}},
		},
	})
	ascendLabels := nodefeature.ConstructAcceleratableNodeLabels(device.DevicesGroupList{
		{
			ID:           "910b2",
			Manufacturer: nodefeature.ManufacturerAscend,
			Name:         "910B2",
			Memory:       65536,
			Cores:        24,
			Family:       "910B",
			Accelerators: []device.Accelerator{{ID: "0"}},
		},
	})

	if len(ExtractNodeHardwareFromNode(nil)) != 0 {
		t.Fatal("ExtractNodeHardwareFromNode(nil) expected empty slice")
	}

	nvidiaNode := &core.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "gpu-node-1", Labels: nvidiaLabels},
	}
	hwList := ExtractNodeHardwareFromNode(nvidiaNode)
	if len(hwList) != 1 {
		t.Fatalf("ExtractNodeHardwareFromNode len = %d, want 1", len(hwList))
	}
	if hwList[0].Manufacturer != "nvidia" || hwList[0].Arch != "sm_90" {
		t.Fatalf("unexpected nvidia hardware: %+v", hwList[0])
	}
	if hwList[0].SelectorLabels[nodefeature.AcceleratableFeatureLabelPrefix+"nvidia-h100"] != "true" {
		t.Fatalf("missing acceleratable node selector label: %+v", hwList[0].SelectorLabels)
	}
	if hwList[0].SelectorLabels[nodefeature.AcceleratableFeatureLabelPrefix+"nvidia-h100.comcap"] != "9.0" {
		t.Fatalf("missing comcap selector label: %+v", hwList[0].SelectorLabels)
	}

	cases := []struct {
		name         string
		manufacturer string
		arch         string
		labels       map[string]string
		wantMatch    bool
	}{
		{name: "nvidia sm_90a matches comcap 9.0 node", manufacturer: "nvidia", arch: "sm_90a", labels: nvidiaLabels, wantMatch: true},
		{name: "nvidia sm_90 matches comcap 9.0 node", manufacturer: "nvidia", arch: "sm_90", labels: nvidiaLabels, wantMatch: true},
		{name: "nvidia sm_80 does not match comcap 9.0 node", manufacturer: "nvidia", arch: "sm_80", labels: nvidiaLabels, wantMatch: false},
		{name: "amd gfx942 matches gfx942 node", manufacturer: "amd", arch: "gfx942", labels: amdLabels, wantMatch: true},
		{name: "amd gfx90a does not match gfx942 node", manufacturer: "amd", arch: "gfx90a", labels: amdLabels, wantMatch: false},
		{name: "ascend 910b matches 910B family node", manufacturer: "ascend", arch: "910b", labels: ascendLabels, wantMatch: true},
		{name: "ascend 950 does not match 910B node", manufacturer: "ascend", arch: "950", labels: ascendLabels, wantMatch: false},
		{name: "empty labels do not match", manufacturer: "nvidia", arch: "sm_90a", labels: nil, wantMatch: false},
		{name: "invalid manufacturer does not match", manufacturer: "invalid", arch: "sm_90a", labels: nvidiaLabels, wantMatch: false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			hw, ok := MatchNodeLabels(tc.manufacturer, tc.arch, tc.labels)
			if ok != tc.wantMatch {
				t.Fatalf("MatchNodeLabels(%q, %q) = %v, want %v (hw=%+v)", tc.manufacturer, tc.arch, ok, tc.wantMatch, hw)
			}
		})
	}

	if MatchHardware("nvidia", "sm_90a", NodeHardware{Manufacturer: "nvidia", Arch: ""}) {
		t.Fatal("MatchHardware with empty Arch expected false")
	}
	if MatchHardware("amd", "gfx942", NodeHardware{Manufacturer: "nvidia", Arch: "sm_90a"}) {
		t.Fatal("MatchHardware with mismatched manufacturer expected false")
	}
}
