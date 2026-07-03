// Package quantityx provides helpers around k8s.io/apimachinery/pkg/api/resource.Quantity
// that the upstream API does not expose.
package quantityx

import (
	"math/big"
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

type _Unit struct {
	scale  int64
	suffix string
}

var binaryUnits = [...]_Unit{
	{Ei, "Ei"},
	{Pi, "Pi"},
	{Ti, "Ti"},
	{Gi, "Gi"},
	{Mi, "Mi"},
	{Ki, "Ki"},
}

var decimalUnits = [...]_Unit{
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

func formatScaled(
	q resource.Quantity,
	units []_Unit,
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

// Multiply multiplies quantity by multiplier and returns the result as a new
// quantity; the input is not modified. If multiplier is 1, quantity is returned
// unchanged.
func Multiply(quantity resource.Quantity, multiplier int64) resource.Quantity {
	if multiplier == 1 {
		return quantity
	}
	q := quantity.DeepCopy()
	q.Mul(multiplier)
	return q
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

// PercentMultiply scales quantity by percent/100, flooring the result to an integer
// in the quantity's base unit, with a floor of 1 so a positive quantity never scales
// down to zero. percent is clamped to [0,100]; percent >= 100 returns quantity unchanged
// and a zero quantity stays zero. The input is not modified. Like Divide, it floors the
// base-unit value, so it is intended for integer/count quantities (e.g. CPU cores, Gi of
// RAM).
//
// A percent of 0 does not yield 0: a positive quantity still floors to 1, so a caller that
// treats 0 as "no scaling" (the full quantity) must map it to 100 (or any value >= 100).
func PercentMultiply(quantity resource.Quantity, percent int64) resource.Quantity {
	if percent >= 100 {
		return quantity
	}
	if percent < 0 {
		percent = 0
	}
	v := quantity.Value()
	// v*percent can overflow int64 for very large quantities, so scale through big.Int
	// (truncating toward zero, like integer division); the floored result is always <= |v|,
	// so it always fits back into int64.
	scaled := new(big.Int).Mul(big.NewInt(v), big.NewInt(percent))
	scaled.Quo(scaled, big.NewInt(100))
	s := scaled.Int64()
	if s < 1 && v > 0 {
		s = 1
	}
	return *resource.NewQuantity(s, quantity.Format)
}

// StringPercentMultiply scales q by percent/100 and returns the result.
// The input q is a string, which will be parsed into a resource.Quantity.
func StringPercentMultiply(quantityStr string, percent int64) (resource.Quantity, error) {
	q, err := resource.ParseQuantity(quantityStr)
	if err != nil {
		return resource.Quantity{}, err
	}
	return PercentMultiply(q, percent), nil
}

// Divide divides quantity by divisor, rounding the result DOWN, and returns it as
// a new quantity preserving the input's format. The input is not modified (so no
// Safe variant is needed). A divisor <= 0, or 1, returns quantity unchanged.
// It floors the quantity's base-unit value, so it is intended for integer/count
// quantities (e.g. CPU cores, Gi of RAM).
func Divide(quantity resource.Quantity, divisor int64) resource.Quantity {
	if divisor <= 0 || divisor == 1 {
		return quantity
	}
	return *resource.NewQuantity(quantity.Value()/divisor, quantity.Format)
}

// StringDivide divides q by divisor and returns the result.
// The input q is a string, which will be parsed into a resource.Quantity.
func StringDivide(quantityStr string, divisor int64) (resource.Quantity, error) {
	q, err := resource.ParseQuantity(quantityStr)
	if err != nil {
		return resource.Quantity{}, err
	}
	return Divide(q, divisor), nil
}
