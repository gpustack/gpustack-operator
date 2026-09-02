package quantityx

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"k8s.io/apimachinery/pkg/api/resource"
)

func TestFormatBinarySI(t *testing.T) {
	cases := []struct {
		name string
		q    resource.Quantity
		want string
	}{
		{
			name: "zero",
			q:    *resource.NewQuantity(0, resource.BinarySI),
			want: "0",
		},
		{
			name: "below Ki keeps raw bytes",
			q:    *resource.NewQuantity(512, resource.BinarySI),
			want: "512",
		},
		{
			name: "exact Ki",
			q:    *resource.NewQuantity(1024, resource.BinarySI),
			want: "1Ki",
		},
		{
			name: "exact Mi",
			q:    *resource.NewQuantity(2*Mi, resource.BinarySI),
			want: "2Mi",
		},
		{
			name: "exact Gi",
			q:    *resource.NewQuantity(16*Gi, resource.BinarySI),
			want: "16Gi",
		},
		{
			name: "exact Ti",
			q:    *resource.NewQuantity(3*Ti, resource.BinarySI),
			want: "3Ti",
		},
		{
			name: "16Gi minus 512 rounds to 16Gi",
			q:    *resource.NewQuantity(16*Gi-512, resource.BinarySI),
			want: "16Gi",
		},
		{
			name: "1.5 Gi rounds to 2Gi at largest unit",
			q:    *resource.NewQuantity(Gi+Gi/2, resource.BinarySI),
			want: "2Gi",
		},
		{
			name: "1.4 Gi rounds down to 1Gi",
			q:    *resource.NewQuantity(Gi+(4*Gi/10), resource.BinarySI),
			want: "1Gi",
		},
		{
			name: "DecimalSI input still formats with binary suffix",
			q:    *resource.NewQuantity(16*Gi, resource.DecimalSI),
			want: "16Gi",
		},
		{
			name: "MustParse from canonical Gi",
			q:    resource.MustParse("32Gi"),
			want: "32Gi",
		},
		{
			name: "MustParse from raw bytes is recovered",
			q:    resource.MustParse("17179868672"), // 16Gi - 512
			want: "16Gi",
		},
		{
			name: "negative value preserves sign",
			q:    *resource.NewQuantity(-16*Gi, resource.BinarySI),
			want: "-16Gi",
		},
		{
			name: "1023 stays raw",
			q:    *resource.NewQuantity(1023, resource.BinarySI),
			want: "1023",
		},
	}

	for _, cs := range cases {
		t.Run(cs.name, func(t *testing.T) {
			got := FormatBinarySI(cs.q)
			assert.Equal(t, cs.want, got)
		})
	}
}

func TestFormatDecimalSI(t *testing.T) {
	cases := []struct {
		name string
		q    resource.Quantity
		want string
	}{
		{
			name: "zero",
			q:    *resource.NewQuantity(0, resource.DecimalSI),
			want: "0",
		},
		{
			name: "below k keeps raw value",
			q:    *resource.NewQuantity(4, resource.DecimalSI),
			want: "4",
		},
		{
			name: "exact k",
			q:    *resource.NewQuantity(1000, resource.DecimalSI),
			want: "1k",
		},
		{
			name: "1500 rounds to 2k",
			q:    *resource.NewQuantity(1500, resource.DecimalSI),
			want: "2k",
		},
		{
			name: "exact M",
			q:    *resource.NewQuantity(16_000_000, resource.DecimalSI),
			want: "16M",
		},
		{
			name: "exact G",
			q:    *resource.NewQuantity(8_000_000_000, resource.DecimalSI),
			want: "8G",
		},
		{
			name: "millis collapse via Value round-up",
			q:    resource.MustParse("1500m"),
			want: "2",
		},
		{
			name: "BinarySI input still formats with decimal suffix",
			q:    *resource.NewQuantity(16*Gi, resource.BinarySI),
			want: "17G",
		},
		{
			name: "negative value preserves sign",
			q:    *resource.NewQuantity(-1500, resource.DecimalSI),
			want: "-2k",
		},
	}

	for _, cs := range cases {
		t.Run(cs.name, func(t *testing.T) {
			got := FormatDecimalSI(cs.q)
			assert.Equal(t, cs.want, got)
		})
	}
}

func TestFormat(t *testing.T) {
	cases := []struct {
		name string
		q    resource.Quantity
		want string
	}{
		{
			name: "BinarySI dispatches to binary",
			q:    *resource.NewQuantity(16*Gi, resource.BinarySI),
			want: "16Gi",
		},
		{
			name: "DecimalSI dispatches to decimal",
			q:    *resource.NewQuantity(16_000_000_000, resource.DecimalSI),
			want: "16G",
		},
		{
			name: "DecimalExponent dispatches to decimal",
			q:    *resource.NewQuantity(1_500_000, resource.DecimalExponent),
			want: "2M",
		},
		{
			name: "BinarySI near-miss rounds via binary path",
			q:    *resource.NewQuantity(16*Gi-512, resource.BinarySI),
			want: "16Gi",
		},
	}

	for _, cs := range cases {
		t.Run(cs.name, func(t *testing.T) {
			got := Format(cs.q)
			assert.Equal(t, cs.want, got)
		})
	}
}

func TestPercentMultiply(t *testing.T) {
	cases := []struct {
		name    string
		q       string
		percent int64
		want    string
	}{
		{"full percent is unchanged", "16", 100, "16"},
		{"above full is clamped to unchanged", "16", 200, "16"},
		{"half", "16", 50, "8"},
		{"quarter", "16", 25, "4"},
		{"round down decimal", "16", 20, "3"},
		{"positive floors to one", "2", 20, "1"},
		{"zero percent floors to one", "2", 0, "1"},
		{"negative percent floors to one", "2", -5, "1"},
		{"zero quantity stays zero", "0", 50, "0"},
		{"binary SI half preserves unit", "32Gi", 50, "16Gi"},
		{"binary SI round down", "10Gi", 25, "2560Mi"},
		// v*percent overflows int64 (100Pi bytes × 99 > math.MaxInt64); big.Int keeps it exact.
		{"large value does not overflow", "100Pi", 99, "99Pi"},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			got := PercentMultiply(resource.MustParse(c.q), c.percent)
			assert.Equal(t, c.want, got.String(), "PercentMultiply(%s, %d)", c.q, c.percent)
		})
	}
}

func TestStringPercentMultiply(t *testing.T) {
	got, err := StringPercentMultiply("16", 25)
	assert.NoError(t, err)
	assert.Equal(t, "4", got.String())

	_, err = StringPercentMultiply("not-a-quantity", 25)
	assert.Error(t, err)
}

func TestDivide(t *testing.T) {
	cases := []struct {
		name    string
		q       string
		divisor int64
		want    string
	}{
		{"exact decimal", "48", 8, "6"},
		{"round down decimal", "12", 8, "1"},
		{"divisor one is unchanged", "12", 1, "12"},
		{"non-positive divisor is unchanged", "12", 0, "12"},
		{"binary SI preserves unit", "48Gi", 8, "6Gi"},
		{"round down binary SI", "12Gi", 8, "1536Mi"},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			got := Divide(resource.MustParse(c.q), c.divisor)
			assert.Equal(t, c.want, got.String(), "Divide(%s, %d)", c.q, c.divisor)
		})
	}
}

func TestStringDivide(t *testing.T) {
	got, err := StringDivide("48", 8)
	assert.NoError(t, err)
	assert.Equal(t, "6", got.String())

	_, err = StringDivide("not-a-quantity", 8)
	assert.Error(t, err)
}

func TestOverflowsInt64(t *testing.T) {
	cases := []struct {
		name string
		q    string
		want bool
	}{
		{"the largest int64 survives", "9223372036854775807", false},
		{"one past the largest int64, which Value() reports as MinInt64", "9223372036854775808", true},
		{"an exponent form Value() reports as 0", "1e30", true},
		{"a binary suffix ParseQuantity already saturated", "8Ei", false},
		{"an ordinary size", "1Gi", false},
		{"zero", "0", false},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			assert.Equal(t, c.want, OverflowsInt64(resource.MustParse(c.q)), "OverflowsInt64(%s)", c.q)
		})
	}
}

// TestOverflowsInt64_BothBounds pins the contract the name states, at both ends.
//
// The lower bound catches nothing in production today — every caller rejects a negative sign first —
// but Value() misreports below MinInt64 exactly as it does above MaxInt64, and the measured answers
// are what these cases are built from rather than what the doc comment used to assert.
func TestOverflowsInt64_BothBounds(t *testing.T) {
	testCases := []struct {
		name     string
		quantity string
		expected bool
	}{
		{name: "above the maximum", quantity: "9223372036854775808", expected: true},
		{name: "far above, decimal exponent", quantity: "1e30", expected: true},
		{name: "below the minimum", quantity: "-9223372036854775809", expected: true},
		{name: "far below, decimal exponent", quantity: "-1e30", expected: true},
		{name: "the maximum itself", quantity: "9223372036854775807", expected: false},
		{name: "the minimum itself", quantity: "-9223372036854775808", expected: false},
		{name: "an ordinary ceiling", quantity: "20Ti", expected: false},
		{name: "zero", quantity: "0", expected: false},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.expected, OverflowsInt64(resource.MustParse(tc.quantity)))
		})
	}
}
