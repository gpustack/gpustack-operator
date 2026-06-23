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
