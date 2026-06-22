package nodefeature

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"k8s.io/apimachinery/pkg/api/resource"
)

// TestSlicedResourceDenominatorInvariants pins the global credits denominator D
// invariants required by the normalization model: every power-of-two partition
// size up to the maximum divides D evenly (so per-slice unit values are exact
// integers), and 1/D is representable cleanly in nano (so the Kueue credits
// factor 1/D is exact).
func TestSlicedResourceDenominatorInvariants(t *testing.T) {
	assert.Equal(t, int64(12800), int64(ResourceMaxUnits), "D = 2^9 * 5^2")
	assert.Equal(t, int64(512), int64(SlicedResourceMaxSize), "max partitions")

	// D must divide evenly by every per-mode max size so per-slice/per-ownership
	// unit values are exact integers.
	assert.Zerof(t, ResourceMaxUnits%SharedResourceMaxSize, "D %% %d must be 0", SharedResourceMaxSize)
	for _, size := range _SlicedResourceSizes {
		assert.Zerof(t, ResourceMaxUnits%size,
			"D %% %d must be 0 for exact per-slice units", size)
	}
	assert.Zero(t, int64(1e9)%ResourceMaxUnits, "1/D must be nano-clean")
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
		{512, true}, // the maximum
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
			expected: "19200",
		},
		{
			name:     "1: sliced 2",
			quantity: "1",
			sliced:   2,
			expected: "6400",
		},
		{
			name:     "1: sliced 4",
			quantity: "1",
			sliced:   4,
			expected: "3200",
		},
		{
			name:     "1: sliced 8",
			quantity: "1",
			sliced:   8,
			expected: "1600",
		},
		{
			name:     "1: sliced 16",
			quantity: "1",
			sliced:   16,
			expected: "800",
		},
		{
			name:     "1: sliced 512",
			quantity: "1",
			sliced:   512,
			expected: "25",
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
			name:     "19200: sliced 1",
			quantity: "19200",
			sliced:   1,
			expected: "1500m",
		},
		{
			name:     "6400: sliced 2",
			quantity: "6400",
			sliced:   2,
			expected: "1",
		},
		{
			name:     "3200: sliced 4",
			quantity: "3200",
			sliced:   4,
			expected: "1",
		},
		{
			name:     "1600: sliced 8",
			quantity: "1600",
			sliced:   8,
			expected: "1",
		},
		{
			name:     "800: sliced 16",
			quantity: "800",
			sliced:   16,
			expected: "1",
		},
		{
			name:     "25: sliced 512",
			quantity: "25",
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
