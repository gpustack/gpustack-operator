package worker

import (
	"fmt"
	"math/big"

	"k8s.io/apimachinery/pkg/api/resource"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
)

// modelDeploymentScalerDefaultLatencyQuantile is the quantile of the latency distribution a
// scaler is held to when its specification names none.
//
// IT IS THE NINETY FIFTH PERCENTILE, and a scaler that names none is held to it.
const modelDeploymentScalerDefaultLatencyQuantile = 0.95

// ModelDeploymentScalerLatencyQuantile returns the quantile of the latency distribution a
// scaler is held to.
//
// IT READS THE QUANTILE THE SPECIFICATION NAMES and the ninety fifth percentile when the
// specification names none.
//
// IT READS THE VALUE THE QUANTITY RESOLVED, because a quantity holds a value at a fixed
// precision and the resolved value is the one every comparison and every status uses. A
// quantile written with more digits than that precision keeps the rounded value.
//
// IT IS THE SAME VALUE FOR EVERY ALGORITHM THAT CARRIES LATENCY TARGETS, because the
// predictive algorithm holds its latency targets in a nested token throughput specification
// and the manual and absent capacity prior paths hold them in the same place.
//
// IT REFUSES A QUANTILE OUTSIDE THE UNIT INTERVAL, because a quantile that names no
// percentile cannot be held to.
func ModelDeploymentScalerLatencyQuantile(
	algorithm *workercore.ModelDeploymentScalerAlgorithmSpec,
) (float64, error) {
	if algorithm == nil {
		return modelDeploymentScalerDefaultLatencyQuantile, nil
	}

	var quantity *resource.Quantity
	switch algorithm.Type {
	case workercore.ModelDeploymentScalerAlgorithmTypeTokenThroughput:
		if algorithm.TokenThroughput != nil {
			quantity = algorithm.TokenThroughput.LatencyQuantile
		}
	case workercore.ModelDeploymentScalerAlgorithmTypePredictive:
		if algorithm.Predictive != nil {
			quantity = algorithm.Predictive.TokenThroughput.LatencyQuantile
		}
	default:
		return modelDeploymentScalerDefaultLatencyQuantile, nil
	}
	if quantity == nil {
		return modelDeploymentScalerDefaultLatencyQuantile, nil
	}

	value, ok := modelDeploymentScalerQuantileValue(quantity)
	if !ok {
		return 0, fmt.Errorf("the latency quantile %s is not a number", ModelDeploymentScalerFormatQuantity(quantity))
	}
	if value <= 0 || value > 1 {
		return 0, fmt.Errorf(
			"the latency quantile must be greater than zero and no greater than one, got %s",
			ModelDeploymentScalerFormatQuantity(quantity),
		)
	}
	return value, nil
}

const (
	maxModelDeploymentScalerQuantityBitLen = 128
	maxModelDeploymentScalerQuantityScale  = 38
)

// ModelDeploymentScalerBoundedRat converts a Quantity into an exact rational without
// mutating the input Quantity or allocating unbounded powers of ten.
func ModelDeploymentScalerBoundedRat(quantity *resource.Quantity) (*big.Rat, bool) {
	if quantity == nil {
		return nil, false
	}

	cp := quantity.DeepCopy()
	dec := cp.AsDec()
	unscaled := dec.UnscaledBig()
	scale := int64(dec.Scale())
	if unscaled.BitLen() > maxModelDeploymentScalerQuantityBitLen ||
		scale < -maxModelDeploymentScalerQuantityScale ||
		scale > maxModelDeploymentScalerQuantityScale {
		return nil, false
	}

	num := new(big.Int).Set(unscaled)
	den := big.NewInt(1)
	if scale >= 0 {
		den.Exp(big.NewInt(10), big.NewInt(scale), nil)
	} else {
		num.Mul(num, new(big.Int).Exp(big.NewInt(10), big.NewInt(-scale), nil))
	}
	return new(big.Rat).SetFrac(num, den), true
}

// ModelDeploymentScalerFormatQuantity renders a Quantity for diagnostics without
// mutating the receiver or expanding out-of-range decimal exponents.
func ModelDeploymentScalerFormatQuantity(quantity *resource.Quantity) string {
	if quantity == nil {
		return "<nil>"
	}
	cp := quantity.DeepCopy()
	dec := cp.AsDec()
	unscaled := dec.UnscaledBig()
	scale := int64(dec.Scale())
	if unscaled.BitLen() > maxModelDeploymentScalerQuantityBitLen {
		return "<out-of-range>"
	}
	if scale < -maxModelDeploymentScalerQuantityScale || scale > maxModelDeploymentScalerQuantityScale {
		return fmt.Sprintf("%se%d", unscaled.String(), -scale)
	}
	return cp.String()
}

// modelDeploymentScalerQuantileValue reads the value a quantity holds.
//
// IT READS THE EXACT VALUE THE QUANTITY HOLDS and rounds it once to the nearest
// float, because a quantity holds a decimal and a float cannot hold it exactly.
//
// IT DOES NOT USE THE APPROXIMATE READER OF THE QUANTITY, because that reader
// multiplies the value by a rounded power of ten and lands one unit away from
// the number the quantity holds. A quantile of 0.95 read that way is not the
// 0.95 a measurement of the ninety fifth percentile carries, and a comparison
// between the two would refuse an evidence that answers the question.
//
// IT ROUNDS THE SAME WAY A READER OF THE DECIMAL WOULD, so the quantile a
// scaler names is the quantile every other reader of that number sees.
func modelDeploymentScalerQuantileValue(quantity *resource.Quantity) (float64, bool) {
	rat, ok := ModelDeploymentScalerBoundedRat(quantity)
	if !ok {
		return 0, false
	}
	value, _ := rat.Float64()
	return value, true
}
