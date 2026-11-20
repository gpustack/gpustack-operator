package runtimecontrol

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"math/big"
	"sort"
	"strings"

	"k8s.io/apimachinery/pkg/api/resource"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	"gpustack.ai/gpustack/pkg/utils/json"
)

type canonicalParameter struct {
	Key   string `json:"key"`
	Value string `json:"value"`
}

type canonicalRuntimeControl struct {
	Parameters []canonicalParameter `json:"parameters,omitempty"`
	Paused     bool                 `json:"paused"`
}

const (
	// maxCanonicalDecimalScaleAbs caps decimal exponent expansion so adversarial or
	// out-of-range quantities cannot trigger proportional memory allocation.
	maxCanonicalDecimalScaleAbs = 18
	maxCanonicalUnscaledBits    = 128
)

// CanonicalQuantityDecimal converts a Kubernetes resource.Quantity into a canonical
// base-10 decimal string without trailing fractional zeros or unit suffixes.
func CanonicalQuantityDecimal(q *resource.Quantity) string {
	if q == nil {
		return ""
	}
	dec := q.AsDec()
	unscaled := new(big.Int).Set(dec.UnscaledBig())
	if unscaled.Sign() == 0 {
		return "0"
	}

	sign := ""
	if unscaled.Sign() < 0 {
		sign = "-"
		unscaled.Neg(unscaled)
	}

	scale := int(dec.Scale())
	if scale < -maxCanonicalDecimalScaleAbs ||
		scale > maxCanonicalDecimalScaleAbs ||
		unscaled.BitLen() > maxCanonicalUnscaledBits {
		return fmt.Sprintf("%s%se%d", sign, unscaled.String(), -scale)
	}

	digits := unscaled.String()
	if scale <= 0 {
		if scale < 0 {
			digits += strings.Repeat("0", -scale)
		}
		return sign + digits
	}

	for scale > 0 && len(digits) > 0 && digits[len(digits)-1] == '0' {
		digits = digits[:len(digits)-1]
		scale--
	}
	if scale == 0 {
		return sign + digits
	}
	if len(digits) <= scale {
		return sign + "0." + strings.Repeat("0", scale-len(digits)) + digits
	}
	split := len(digits) - scale
	return sign + digits[:split] + "." + digits[split:]
}

// ComputeAppliedConfigDigest returns the canonical sha256 digest covering all sentinel-managed
// runtimeControl parameters (excluding elastic.dataParallelWidth) plus the role's paused state.
// Equivalent parameter spellings (such as "0.5", "0.50", and "500m") produce identical digests.
func ComputeAppliedConfigDigest(
	rc *workercore.ModelDeploymentRoleRuntimeControl,
	paused bool,
) (string, error) {
	doc := canonicalRuntimeControl{
		Paused: paused,
	}

	if rc != nil && len(rc.Parameters) > 0 {
		sentinelParams := make(map[string]string, len(rc.Parameters))
		for key, raw := range rc.Parameters {
			if entry, ok := parameterRegistry[key]; ok && !entry.spec.SentinelManaged {
				continue
			}
			sentinelParams[key] = raw
		}

		canonical, err := CanonicalizeParameters(sentinelParams)
		if err != nil {
			return "", fmt.Errorf("canonicalize runtime control parameters: %w", err)
		}
		if len(canonical) > 0 {
			keys := make([]string, 0, len(canonical))
			for k := range canonical {
				keys = append(keys, k)
			}
			sort.Strings(keys)

			items := make([]canonicalParameter, len(keys))
			for i, k := range keys {
				items[i] = canonicalParameter{
					Key:   k,
					Value: canonical[k],
				}
			}
			doc.Parameters = items
		}
	}

	encoded, err := json.Marshal(doc)
	if err != nil {
		return "", fmt.Errorf("marshal canonical runtime control: %w", err)
	}
	sum := sha256.Sum256(encoded)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}
