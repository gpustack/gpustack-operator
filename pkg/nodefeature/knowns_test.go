package nodefeature

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	core "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/util/validation"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
)

func TestAcceleratableResourceNames(t *testing.T) {
	// The sliced mode advertises two distinct keys: the bare card-count /
	// injection-token key `.sliced` (via device-plugin, returned by
	// GetAcceleratableResourceName) and the fine-grained counting key
	// `.sliced.units` (via Patch Node). They must not collide.
	assert.Equal(t, core.ResourceName("nvidia.com/gpu"),
		GetAcceleratableResourceName(ManufacturerNVIDIA, workercore.DeviceAllocationModeExclusive))
	assert.Equal(t, core.ResourceName("nvidia.com/gpu.shared"),
		GetAcceleratableResourceName(ManufacturerNVIDIA, workercore.DeviceAllocationModeShared))
	assert.Equal(t, core.ResourceName("nvidia.com/gpu.sliced"),
		GetAcceleratableResourceName(ManufacturerNVIDIA, workercore.DeviceAllocationModeSliced))
	assert.Equal(t, core.ResourceName("nvidia.com/gpu.sliced.units"),
		GetAcceleratableSlicedUnitsResourceName(ManufacturerNVIDIA))

	// The three gate-2 node-level sliced budget keys are distinct fine-grained
	// suffixes layered under ".sliced"; they must not collide with ".sliced.units"
	// or with each other.
	assert.Equal(t, core.ResourceName("nvidia.com/gpu.sliced.cores-percentage"),
		GetAcceleratableSlicedCoresPercentageResourceName(ManufacturerNVIDIA))
	assert.Equal(t, core.ResourceName("nvidia.com/gpu.sliced.memory-percentage"),
		GetAcceleratableSlicedMemoryPercentageResourceName(ManufacturerNVIDIA))
	assert.Equal(t, core.ResourceName("nvidia.com/gpu.sliced.memory-mib"),
		GetAcceleratableSlicedMemoryMibResourceName(ManufacturerNVIDIA))

	// The physical-slice (MIG) request key layers the card's own profile name under
	// ".sliced.mig-"; the profile name is variable (and may itself contain a dot).
	assert.Equal(t, core.ResourceName("nvidia.com/gpu.sliced.mig-1g.10gb"),
		GetAcceleratableSlicedMigResourceName(ManufacturerNVIDIA, "1g.10gb"))

	// The SSH sidecar's device-only visibility resource lives under a distinct domain,
	// outside the accelerator families, so admission does not read it as a mode.
	assert.Equal(t, core.ResourceName("device.gpustack.ai/nvidia.visibility"),
		GetAcceleratableResourceName(ManufacturerNVIDIA, workercore.DeviceAllocationModeVisibility))
}

func TestAcceleratablePartitionedResourceNames(t *testing.T) {
	// The physical-partition family mirrors the logical one: a coarse device-plugin
	// token key, a fine-grained node-capacity counting key, and a variable-tailed
	// per-profile key whose segment prefix is the manufacturer's own partitioning name.
	assert.Equal(t, core.ResourceName("nvidia.com/gpu.partitioned"),
		GetAcceleratableResourceName(ManufacturerNVIDIA, workercore.DeviceAllocationModePartitioned))
	assert.Equal(t, core.ResourceName("nvidia.com/gpu.partitioned.units"),
		GetAcceleratablePartitionedUnitsResourceName(ManufacturerNVIDIA))
	assert.Equal(t, core.ResourceName("nvidia.com/gpu.partitioned.mig-3g.40gb"),
		GetAcceleratablePartitionedProfileResourceName(ManufacturerNVIDIA, "3g.40gb"))

	// A manufacturer without hardware partitioning has no kind, so it advertises no
	// key of this family at all.
	assert.Empty(t, GetAcceleratableResourceName(ManufacturerAMD, workercore.DeviceAllocationModePartitioned))
	assert.Empty(t, GetAcceleratablePartitionedUnitsResourceName(ManufacturerAMD))
	assert.Empty(t, GetAcceleratablePartitionedProfileResourceName(ManufacturerAMD, "3g.40gb"))
	assert.Empty(t, GetAcceleratablePartitionedProfileResourceName("example", "3g.40gb"))
}

func TestGetAcceleratablePartitionedProfileResourceName(t *testing.T) {
	longProfile := strings.Repeat("a", 64)

	cases := []struct {
		name    string
		profile string
		want    core.ResourceName
	}{
		{"dotted profile", "3g.40gb", "nvidia.com/gpu.partitioned.mig-3g.40gb"},
		{"whole card profile", "7g.80gb", "nvidia.com/gpu.partitioned.mig-7g.80gb"},
		{"empty profile", "", ""},
		// A "+" variant is not a valid resource-name character; the profile is
		// excluded rather than rewritten to something key-safe.
		{"plus variant", "1g.10gb+me", ""},
		{"plus all variant", "1g.10gb+me.all", ""},
		// The part after "/" is limited to 63 characters.
		{"over the length limit", longProfile, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := GetAcceleratablePartitionedProfileResourceName(ManufacturerNVIDIA, c.profile)
			assert.Equal(t, c.want, got)
			if got != "" {
				assert.Empty(t, validation.IsQualifiedName(string(got)),
					"every generated key must pass resource-name validation")
			}
		})
	}
}

func TestPartitionedProfileOf(t *testing.T) {
	cases := []struct {
		name    string
		in      core.ResourceName
		profile string
		ok      bool
	}{
		{"dotted profile", "nvidia.com/gpu.partitioned.mig-3g.40gb", "3g.40gb", true},
		{"counting key is not a profile", "nvidia.com/gpu.partitioned.units", "", false},
		{"coarse token key is not a profile", "nvidia.com/gpu.partitioned", "", false},
		{"empty profile", "nvidia.com/gpu.partitioned.mig-", "", false},
		{"unknown base", "example.com/foo.partitioned.mig-3g.40gb", "", false},
		{"manufacturer without a kind", "amd.com/gpu.partitioned.mig-3g.40gb", "", false},
		{"legacy sliced mig key", "nvidia.com/gpu.sliced.mig-1g.10gb", "", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			profile, ok := PartitionedProfileOf(c.in)
			assert.Equal(t, c.ok, ok)
			assert.Equal(t, c.profile, profile)
		})
	}
}

func TestResourceFamilyOf(t *testing.T) {
	cases := []struct {
		name string
		in   core.ResourceName
		want ResourceFamily
	}{
		{"exclusive", "nvidia.com/gpu", ResourceFamilyExclusive},
		{"exclusive ascend", "huawei.com/npu", ResourceFamilyExclusive},
		{"shared", "nvidia.com/gpu.shared", ResourceFamilyShared},
		{"sliced token", "nvidia.com/gpu.sliced", ResourceFamilySliced},
		{"sliced units", "nvidia.com/gpu.sliced.units", ResourceFamilySliced},
		{"sliced cores percentage", "nvidia.com/gpu.sliced.cores-percentage", ResourceFamilySliced},
		{"sliced memory percentage", "nvidia.com/gpu.sliced.memory-percentage", ResourceFamilySliced},
		{"sliced memory mib", "nvidia.com/gpu.sliced.memory-mib", ResourceFamilySliced},
		{"legacy sliced mig profile", "nvidia.com/gpu.sliced.mig-1g.10gb", ResourceFamilySliced},
		{"partitioned token", "nvidia.com/gpu.partitioned", ResourceFamilyPartitioned},
		{"partitioned units", "nvidia.com/gpu.partitioned.units", ResourceFamilyPartitioned},
		{"partitioned profile", "nvidia.com/gpu.partitioned.mig-3g.40gb", ResourceFamilyPartitioned},
		{"visibility", "device.gpustack.ai/nvidia.visibility", ResourceFamilyVisibility},
		{"visibility of an unknown manufacturer", "device.gpustack.ai/example.visibility", ResourceFamilyNone},
		{"credits", "credits.gpustack.ai/nvidia", ResourceFamilyNone},
		{"unknown base", "example.com/foo", ResourceFamilyNone},
		{"unknown base sliced", "example.com/foo.sliced", ResourceFamilyNone},
		{"unknown base partitioned", "example.com/foo.partitioned", ResourceFamilyNone},
		{"unrecognized sliced sub-key", "nvidia.com/gpu.sliced.bogus", ResourceFamilyNone},
		{"unrecognized partitioned sub-key", "nvidia.com/gpu.partitioned.bogus", ResourceFamilyNone},
		{"empty per-profile tail", "nvidia.com/gpu.partitioned.mig-", ResourceFamilyNone},
		{"plain cpu", "cpu", ResourceFamilyNone},
		{"empty", "", ResourceFamilyNone},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			assert.Equal(t, c.want, ResourceFamilyOf(c.in))
		})
	}
}

func TestIsKnownAcceleratableResourceName(t *testing.T) {
	cases := []struct {
		name string
		in   core.ResourceName
		want bool
	}{
		{"exclusive", "nvidia.com/gpu", true},
		{"shared", "nvidia.com/gpu.shared", true},
		{"sliced units", "nvidia.com/gpu.sliced.units", true},
		{"sliced card", "nvidia.com/gpu.sliced", true},
		{"mig profile (dotted name)", "nvidia.com/gpu.sliced.mig-1g.10gb", true},
		{"mig profile amd", "amd.com/gpu.sliced.mig-1g.10gb", true},
		{"mig profile unknown base", "example.com/foo.sliced.mig-1g.10gb", false},
		{"mig profile empty suffix", "nvidia.com/gpu.sliced.mig-", false},
		{"amd exclusive", "amd.com/gpu", true},
		{"unknown", "example.com/foo", false},
		{"credits is not a device resource", "credits.gpustack.ai/nvidia", false},
		{"sidecar visibility is not an accelerator mode", "device.gpustack.ai/nvidia.visibility", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			assert.Equal(t, c.want, IsKnownAcceleratableResourceName(c.in))
		})
	}
}

// TestSlicedResourceDenominatorInvariants pins the global credits denominator D
// invariants required by the normalization model: every power-of-two partition
// size up to the maximum divides D evenly (so per-slice unit values are exact
// integers), and 1/D is representable cleanly in nano (so the Kueue credits
// factor 1/D is exact).
func TestSlicedResourceDenominatorInvariants(t *testing.T) {
	assert.Equal(t, int64(1_600_000), int64(ResourceMaxUnits), "D = 2^9 * 5^5")
	assert.Equal(t, int64(512), int64(SlicedResourceMaxSize), "max partitions")

	// D must divide evenly by every per-mode max size so per-slice/per-ownership
	// unit values are exact integers.
	assert.Zerof(t, ResourceMaxUnits%SharedResourceMaxSize, "D %% %d must be 0", SharedResourceMaxSize)
	for _, size := range _SlicedResourceSizes {
		assert.Zerof(t, ResourceMaxUnits%size,
			"D %% %d must be 0 for exact per-slice units", size)
	}
	// D must divide evenly by 100 so the memory-1% slice step D/100 is an exact
	// integer for the per-card VRAM-percentage keys (the 5^5 factor provides this).
	assert.Zero(t, ResourceMaxUnits%100, "D %% 100 must be 0 for integer memory-1% units")
	assert.Zero(t, int64(1e9)%ResourceMaxUnits, "1/D must be nano-clean")
}

func TestCreditsPerCardInvariants(t *testing.T) {
	assert.Equal(t, int64(1_600_000), int64(CreditsPerCard), "B = D = 1600000")

	// B must divide evenly by every per-mode max size so the per-mode credit
	// magnitudes (shared B/10, sliced B/size) are exact integers and Kueue's
	// ResourceValue int64 ceil never rounds them up.
	assert.Zerof(t, CreditsPerCard%SharedResourceMaxSize,
		"B %% %d must be 0", SharedResourceMaxSize)
	for _, size := range _SlicedResourceSizes {
		assert.Zerof(t, CreditsPerCard%size,
			"B %% %d must be 0 for integer per-slice credits", size)
	}
}

func TestCardsToCredits(t *testing.T) {
	cases := []struct {
		name     string
		cards    string
		expected string
	}{
		{name: "1 card", cards: "1", expected: "1600k"},
		{name: "4 cards", cards: "4", expected: "6400k"},
		{name: "0 cards", cards: "0", expected: "0"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			q, err := resource.ParseQuantity(c.cards)
			assert.NoError(t, err)
			got := CardsToCredits(q)
			assert.Equal(t, c.expected, got.String())
		})
	}
}

func TestCreditsToCards(t *testing.T) {
	cases := []struct {
		name     string
		credits  string
		expected string
	}{
		{name: "whole card", credits: "1600000", expected: "1"},
		{name: "four cards", credits: "6400000", expected: "4"},
		{name: "1/8 slice", credits: "200000", expected: "125m"},
		{name: "2/8 slice", credits: "400000", expected: "250m"},
		{name: "29 1/8 slices", credits: "5800000", expected: "3625m"},
		{name: "smallest slice (1/512)", credits: "3125", expected: "1953u"},
		{name: "zero", credits: "0", expected: "0"},
		// Misconfigured fractional credit: Value() ceils like Kueue's ResourceValue
		// (199999.5 → 200000) before the divide, so the card view stays consistent.
		{name: "fractional credit ceils", credits: "199999500m", expected: "125m"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			q, err := resource.ParseQuantity(c.credits)
			assert.NoError(t, err)
			got := CreditsToCards(q)
			assert.Equal(t, c.expected, got.String())
		})
	}
}

// TestCreditsRoundTrip pins that whole-card counts survive the credits↔cards
// round trip exactly.
func TestCreditsRoundTrip(t *testing.T) {
	for _, cards := range []string{"1", "2", "4", "8"} {
		q, err := resource.ParseQuantity(cards)
		assert.NoError(t, err)
		got := CreditsToCards(CardsToCredits(q))
		assert.Equal(t, cards, got.String(), "round trip for %s cards", cards)
	}
}

func TestIsValidSlicedPartitions(t *testing.T) {
	cases := []struct {
		n    int64
		want bool
	}{
		{0, false},
		{1, false}, // below the minimum of 2 (a single slice is a whole card)
		{2, true},
		{3, false}, // not a power of two
		{8, true},
		{16, true},
		{512, true},   // the maximum
		{1024, false}, // above SlicedResourceMaxSize
		{-8, false},
	}
	for _, c := range cases {
		assert.Equalf(t, c.want, IsValidSlicedPartitions(c.n), "IsValidSlicedPartitions(%d)", c.n)
	}
}

func TestQuantityToSliceCount(t *testing.T) {
	cases := []struct {
		name     string
		quantity string
		sliced   int64
		expected string
	}{
		{
			name:     "1: sliced 0",
			quantity: "1",
			sliced:   0,
			expected: "1",
		},
		{
			name:     "1.5: sliced 1",
			quantity: "1.5",
			sliced:   1,
			expected: "1",
		},
		{
			name:     "1: sliced 2",
			quantity: "1",
			sliced:   2,
			expected: "2",
		},
		{
			name:     "0.5: sliced 4",
			quantity: "0.5",
			sliced:   4,
			expected: "2",
		},
		{
			name:     "0.25: sliced 8",
			quantity: "0.25",
			sliced:   8,
			expected: "2",
		},
		{
			name:     "0.125: sliced 16",
			quantity: "0.125",
			sliced:   16,
			expected: "2",
		},
		{
			name:     "1: sliced 512",
			quantity: "1",
			sliced:   512,
			expected: "512",
		},
	}
	for _, cs := range cases {
		t.Run(cs.name, func(t *testing.T) {
			q, err := resource.ParseQuantity(cs.quantity)
			assert.NoError(t, err)
			scaled := QuantityToSliceCount(q, cs.sliced)
			assert.Equal(t, cs.expected, scaled.String())
		})
	}
}

func TestQuantityToAlignedValue(t *testing.T) {
	cases := []struct {
		name     string
		quantity string
		sliced   int64
		expected string
	}{
		{
			name:     "1: sliced 0",
			quantity: "1",
			sliced:   0,
			expected: "1",
		},
		{
			name:     "1.5: sliced 1",
			quantity: "1.5",
			sliced:   1,
			expected: "2400k",
		},
		{
			name:     "1: sliced 2",
			quantity: "1",
			sliced:   2,
			expected: "800k",
		},
		{
			name:     "1: sliced 4",
			quantity: "1",
			sliced:   4,
			expected: "400k",
		},
		{
			name:     "1: sliced 8",
			quantity: "1",
			sliced:   8,
			expected: "200k",
		},
		{
			name:     "1: sliced 16",
			quantity: "1",
			sliced:   16,
			expected: "100k",
		},
		{
			name:     "1: sliced 512",
			quantity: "1",
			sliced:   512,
			expected: "3125",
		},
	}
	for _, cs := range cases {
		t.Run(cs.name, func(t *testing.T) {
			q, err := resource.ParseQuantity(cs.quantity)
			assert.NoError(t, err)
			aligned := QuantityToAlignedValue(q, cs.sliced)
			assert.Equal(t, cs.expected, aligned.String())
		})
	}
}

func TestQuantityToOriginalValue(t *testing.T) {
	cases := []struct {
		name     string
		quantity string
		sliced   int64
		expected string
	}{
		{
			name:     "1: sliced 0",
			quantity: "1",
			sliced:   0,
			expected: "1",
		},
		{
			name:     "2400000: sliced 1",
			quantity: "2400000",
			sliced:   1,
			expected: "1500m",
		},
		{
			name:     "800000: sliced 2",
			quantity: "800000",
			sliced:   2,
			expected: "1",
		},
		{
			name:     "400000: sliced 4",
			quantity: "400000",
			sliced:   4,
			expected: "1",
		},
		{
			name:     "200000: sliced 8",
			quantity: "200000",
			sliced:   8,
			expected: "1",
		},
		{
			name:     "100000: sliced 16",
			quantity: "100000",
			sliced:   16,
			expected: "1",
		},
		{
			name:     "3125: sliced 512",
			quantity: "3125",
			sliced:   512,
			expected: "1",
		},
	}
	for _, cs := range cases {
		t.Run(cs.name, func(t *testing.T) {
			q, err := resource.ParseQuantity(cs.quantity)
			assert.NoError(t, err)
			original := QuantityToOriginalValue(q, cs.sliced)
			assert.Equal(t, cs.expected, original.String())
		})
	}
}
