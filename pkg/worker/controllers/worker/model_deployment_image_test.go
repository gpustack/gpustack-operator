package worker

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	"gpustack.ai/gpustack/pkg/kubemeta"
	"gpustack.ai/gpustack/pkg/nodefeature"
)

// imageDetail is one observed hardware descriptor to synthesize from.
func imageDetail(manufacturer, family, runtimeVersion string) workercore.InstanceTypeDetail {
	return workercore.InstanceTypeDetail{
		Manufacturer: manufacturer,
		Family:       family,
		InstanceTypeAcceleratorDetail: workercore.InstanceTypeAcceleratorDetail{
			RuntimeVersion: runtimeVersion,
		},
	}
}

func TestSynthesizeModelDeploymentImage(t *testing.T) {
	testCases := []struct {
		name          string
		engine        string
		engineVersion string
		detail        workercore.InstanceTypeDetail
		want          string
		wantErr       string
		why           string
	}{
		{
			name:          "nvidia_has_no_variant_segment",
			engine:        workercore.ModelDeploymentEngineVLLM,
			engineVersion: "0.25.1",
			detail:        imageDetail(nodefeature.ManufacturerNVIDIA, "ampere", "12.9"),
			want:          "gpustack/runner:cuda12.9-vllm0.25.1",
			why: "the variant is populated for cann alone across all 338 published records, so a " +
				"non-Ascend family contributes no segment even when it is set",
		},
		{
			// THE ONE CASE THAT DISTINGUISHES THE TABLE FROM A LOWERCASING. Every other Ascend
			// family below happens to lowercase correctly, so an implementation that called
			// strings.ToLower would pass all of them and fail only here.
			name:          "ascend_910C_maps_to_a3_which_is_not_a_lowercasing",
			engine:        workercore.ModelDeploymentEngineVLLM,
			engineVersion: "0.23.0",
			detail:        imageDetail(nodefeature.ManufacturerAscend, "910C", "9.1"),
			want:          "gpustack/runner:cann9.1-a3-vllm0.23.0",
			why:           "the detector's own comment names the pair 910C/A3",
		},
		{
			name:          "ascend_910B_lowercases_by_coincidence",
			engine:        workercore.ModelDeploymentEngineSGLang,
			engineVersion: "0.5.18",
			detail:        imageDetail(nodefeature.ManufacturerAscend, "910B", "8.2"),
			want:          "gpustack/runner:cann8.2-910b-sglang0.5.18",
			why:           "kept as a case, but it cannot tell the table from a lowercasing",
		},
		{
			name:          "ascend_310P",
			engine:        workercore.ModelDeploymentEngineVLLM,
			engineVersion: "0.20.2",
			detail:        imageDetail(nodefeature.ManufacturerAscend, "310P", "8.0"),
			want:          "gpustack/runner:cann8.0-310p-vllm0.20.2",
		},
		{
			name:          "amd_maps_to_rocm",
			engine:        workercore.ModelDeploymentEngineSGLang,
			engineVersion: "0.5.7",
			detail:        imageDetail(nodefeature.ManufacturerAMD, "", "6.4"),
			want:          "gpustack/runner:rocm6.4-sglang0.5.7",
		},
		{
			name:          "thead_maps_to_hggc",
			engine:        workercore.ModelDeploymentEngineSGLang,
			engineVersion: "0.5.12",
			detail:        imageDetail(nodefeature.ManufacturerTHead, "", "13.0"),
			want:          "gpustack/runner:hggc13.0-sglang0.5.12",
			why:           "hggc is T-Head's, which the matrix never spells; this repo's csrc path does",
		},
		{
			name:          "cambricon_has_no_runner_backend",
			engine:        workercore.ModelDeploymentEngineVLLM,
			engineVersion: "0.25.1",
			detail:        imageDetail(nodefeature.ManufacturerCambricon, "", "1.0"),
			wantErr:       "has no runner backend",
			why:           "a refusal naming the manufacturer, not an empty image that pulls forever",
		},
		{
			name:          "an_ascend_family_with_no_published_variant",
			engine:        workercore.ModelDeploymentEngineVLLM,
			engineVersion: "0.25.1",
			detail:        imageDetail(nodefeature.ManufacturerAscend, "910", "8.0"),
			wantErr:       "has no runner backend variant",
			why:           "910 and 310B are emittable by the detector and absent from the matrix",
		},
		{
			name:          "hardware_not_observed_yet",
			engine:        workercore.ModelDeploymentEngineVLLM,
			engineVersion: "0.25.1",
			detail:        imageDetail("", "", ""),
			wantErr:       "has not observed its hardware yet",
			why:           "a flavor that has not synced is a wait, not a refusal",
		},
		{
			name:          "runtime_version_not_observed_yet",
			engine:        workercore.ModelDeploymentEngineVLLM,
			engineVersion: "0.25.1",
			detail:        imageDetail(nodefeature.ManufacturerNVIDIA, "ampere", ""),
			wantErr:       "has not observed a runtime version yet",
			why: "distinguished from the manufacturer being unknown, because this one resolves on a " +
				"later reconcile while that one may not",
		},
		{
			name:          "no_engine_version",
			engine:        workercore.ModelDeploymentEngineVLLM,
			engineVersion: "",
			detail:        imageDetail(nodefeature.ManufacturerNVIDIA, "ampere", "12.9"),
			wantErr:       "engine version is empty",
			why:           "the schema makes it required, so reaching here means something else is wrong",
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := SynthesizeModelDeploymentImage(tc.engine, tc.engineVersion, tc.detail)
			if tc.wantErr != "" {
				require.Error(t, err, tc.why)
				assert.Contains(t, err.Error(), tc.wantErr, tc.why)
				assert.Empty(t, got, "a failed synthesis renders nothing rather than a partial tag")

				return
			}
			require.NoError(t, err, tc.why)
			assert.Equal(t, tc.want, got, tc.why)
		})
	}
}

// TestModelDeploymentBackendsCoverEveryKnownManufacturer asserts the backend table against this
// project's own manufacturer list, in BOTH directions.
//
// The point is what happens when a manufacturer is added: the exact-set assertion fails, which
// forces whoever adds it to answer whether the runner publishes images for that vendor. A count, or
// a one-directional check, would let a new vendor silently render no image.
func TestModelDeploymentBackendsCoverEveryKnownManufacturer(t *testing.T) {
	known := nodefeature.GetKnownAcceleratableManufacturers()
	require.NotEmpty(t, known, "an empty list would make both assertions below vacuous")

	var missing []string
	for _, m := range known {
		if _, ok := ModelDeploymentImageBackend(m); !ok {
			missing = append(missing, m)
		}
	}

	// Cambricon is EXPECTED here, not tolerated: the runner publishes no cambricon backend. Any
	// other name appearing means a vendor landed without anyone checking the matrix for it.
	assert.Equal(t, []string{nodefeature.ManufacturerCambricon}, missing,
		"a manufacturer with no runner backend must be a deliberate entry in this assertion")

	for m := range modelDeploymentBackends {
		assert.Contains(t, known, m, "%q is not a manufacturer this project knows", m)
	}
}

// TestModelDeploymentAscendVariantKeysSurviveLabelSanitizing pins the one link in the family's path
// that this package cannot see.
//
// The family is capitalized by the detector, travels to a node label, and arrives here through a
// ResourceFlavor note -- so these keys only match if the label sanitizer leaves them alone. It does,
// because they are alphanumeric, but that is a property of another package. Asserting it here means
// a change to the sanitizer fails in this test rather than silently refusing every Ascend
// deployment in a cluster.
func TestModelDeploymentAscendVariantKeysSurviveLabelSanitizing(t *testing.T) {
	require.NotEmpty(t, modelDeploymentAscendVariants)

	for family := range modelDeploymentAscendVariants {
		assert.Equal(t, family, kubemeta.SanitizeLabelValue(family),
			"the family reaches the observed detail through a label, so a sanitizer that rewrote "+
				"%q would make this table unmatchable", family)
	}
}

// TestModelDeploymentImageVariantIsAscendOnly separates "no variant" from "unknown family".
func TestModelDeploymentImageVariantIsAscendOnly(t *testing.T) {
	// A non-Ascend vendor always succeeds with an empty variant, whatever its family says. Reading
	// the empty string as failure would refuse every NVIDIA and AMD pool.
	variant, ok := ModelDeploymentImageVariant(nodefeature.ManufacturerNVIDIA, "hopper")
	assert.True(t, ok)
	assert.Empty(t, variant)

	// Ascend is the only vendor where an unrecognized family is a failure.
	_, ok = ModelDeploymentImageVariant(nodefeature.ManufacturerAscend, "910")
	assert.False(t, ok)
}
