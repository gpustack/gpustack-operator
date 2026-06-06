// Package quantityx provides helpers around k8s.io/apimachinery/pkg/api/resource.Quantity
// that the upstream API does not expose.
package quantityx

import (
	"strconv"

	"k8s.io/apimachinery/pkg/api/resource"
)

const (
	Ki = 1 << 10
	Mi = 1 << 20
	Gi = 1 << 30
	Ti = 1 << 40
	Pi = 1 << 50
	Ei = 1 << 60
)

var binaryUnits = [...]struct {
	scale  int64
	suffix string
}{
	{Ei, "Ei"},
	{Pi, "Pi"},
	{Ti, "Ti"},
	{Gi, "Gi"},
	{Mi, "Mi"},
	{Ki, "Ki"},
}

var decimalUnits = [...]struct {
	scale  int64
	suffix string
}{
	{1e18, "E"},
	{1e15, "P"},
	{1e12, "T"},
	{1e9, "G"},
	{1e6, "M"},
	{1e3, "k"},
}

// Format dispatches on q.Format: BinarySI uses FormatBinarySI, anything else
// (DecimalSI, DecimalExponent, zero-value) uses FormatDecimalSI.
func Format(q resource.Quantity) string {
	if q.Format == resource.BinarySI {
		return FormatBinarySI(q)
	}
	return FormatDecimalSI(q)
}

// FormatBinarySI returns a string for q using a binary SI suffix (Ki, Mi, Gi, Ti, Pi, Ei).
//
// resource.Quantity.String() with BinarySI format falls back to a raw decimal
// integer when the value is not an exact power-of-1024 multiple — e.g.
// 16*1024^3 prints as "16Gi" but 16*1024^3-512 prints as "17179868672".
// FormatBinarySI instead always picks the largest unit U where |q.Value()| >= U
// and rounds the value to the nearest integer at that scale:
//
//	16*1024^3        -> "16Gi"
//	16*1024^3 - 512  -> "16Gi"
//	1.5 * 1024^3     -> "2Gi"
//	1023             -> "1023"   (no binary unit applies)
//	0                -> "0"
//
// The function is lossy by construction — callers that need an exact
// round-trippable representation should keep q.String().
func FormatBinarySI(q resource.Quantity) string {
	return formatScaled(q, binaryUnits[:])
}

// FormatDecimalSI mirrors FormatBinarySI but uses decimal SI suffixes
// (k, M, G, T, P, E). It always picks the largest unit U where |q.Value()| >= U
// and rounds to the nearest integer at that scale:
//
//	4                -> "4"      (no suffix, < 1k)
//	1000             -> "1k"
//	1500             -> "2k"     (rounded — q.String() would keep "1500")
//	1500000          -> "2M"
//	16 * 1e9         -> "16G"
//	1500m            -> "2"      (q.Value() rounds 1.5 up to 2)
//	0                -> "0"
//
// As with FormatBinarySI this is lossy and uses q.Value(), which rounds any
// sub-unit fraction up — so sub-1 quantities like "1500m" collapse to an
// integer. Keep q.String() if you need exact representation.
func FormatDecimalSI(q resource.Quantity) string {
	return formatScaled(q, decimalUnits[:])
}

func formatScaled(q resource.Quantity, units []struct {
	scale  int64
	suffix string
},
) string {
	if q.IsZero() {
		return "0"
	}

	v := q.Value()
	sign := ""
	if v < 0 {
		sign = "-"
		v = -v
	}

	for _, u := range units {
		if v >= u.scale {
			n := (v + u.scale/2) / u.scale
			return sign + strconv.FormatInt(n, 10) + u.suffix
		}
	}
	return sign + strconv.FormatInt(v, 10)
}

// Multiply multiplies q by multiplier and returns the result,
// modifying the input quantity in-place.
// If multiplier is 1, q is returned unmodified.
func Multiply(quantity resource.Quantity, multiplier int64) resource.Quantity {
	if multiplier == 1 {
		return quantity
	}
	quantity.Mul(multiplier)
	return quantity
}

// SafeMultiply is like Multiply but does not modify the input quantity.
func SafeMultiply(quantity resource.Quantity, multiplier int64) resource.Quantity {
	q := quantity.DeepCopy()
	return Multiply(q, multiplier)
}

// StringMultiply multiplies q by multiplier and returns the result.
// The input q is a string, which will be parsed into a resource.Quantity.
func StringMultiply(quantityStr string, multiplier int64) (resource.Quantity, error) {
	q, err := resource.ParseQuantity(quantityStr)
	if err != nil {
		return resource.Quantity{}, err
	}
	return Multiply(q, multiplier), nil
}
