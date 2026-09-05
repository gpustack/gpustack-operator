package worker

import (
	"fmt"
	"strings"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	"gpustack.ai/gpustack/pkg/nodefeature"
)

// modelDeploymentRunnerRepository is the published name of the runner image family.
//
// It is a constant rather than a setting because the tag is a formula over the runner project's own
// release matrix: pointing it at a different repository would produce names that repository has no
// reason to carry. A role that needs another image names it directly through its template.
const modelDeploymentRunnerRepository = "gpustack/runner"

// modelDeploymentBackends maps this project's accelerator manufacturer onto the runner image's
// backend token.
//
// The two vocabularies are independent: the runner names the software stack (cuda, cann, rocm) while
// this project names the vendor (nvidia, ascend, amd), and the matrix never spells the vendor at
// all. Nor does the pairing follow from the vendor name everywhere -- hygon's token is dtk and
// t-head's is hggc -- so none of it is derived here. Every pair below is the one
// gpustack_runtime's _MANUFACTURER_BACKEND_MAPPING gives, a map whose doc comment states its names
// are meant to be the runner's own backend names; hggc has a second witness in this repository's
// csrc/thead/ppu-slicing-shim/hggc/. And every token is one the release matrix publishes.
//
// CAMBRICON IS ABSENT ON PURPOSE, and it is where this table stops short of that upstream map:
// upstream pairs cambricon with neuware, but the matrix carries no neuware image at all. A role on
// such a pool therefore has no image to synthesize and must name one. That is a refusal, not an
// empty string: an empty image fails as an ImagePullBackOff, whose symptom is nowhere near its
// cause.
var modelDeploymentBackends = map[string]string{
	nodefeature.ManufacturerNVIDIA:   "cuda",
	nodefeature.ManufacturerAscend:   "cann",
	nodefeature.ManufacturerAMD:      "rocm",
	nodefeature.ManufacturerMetaX:    "maca",
	nodefeature.ManufacturerMThreads: "musa",
	nodefeature.ManufacturerIluvatar: "corex",
	nodefeature.ManufacturerHygon:    "dtk",
	nodefeature.ManufacturerTHead:    "hggc",
}

// modelDeploymentAscendVariants maps an Ascend family onto the runner image's backend variant.
//
// ONLY ASCEND HAS A VARIANT. Across the whole matrix the variant is populated for cann alone and
// empty for the other seven backends, so this table is per vendor rather than general.
//
// IT IS NOT A LOWERCASING, and that is why it is a table: 910C maps to a3. The detector's own
// comments name that pair, and a test that passes against a strings.ToLower implementation is not
// testing this table at all.
//
// 910 and 310B are absent because the matrix publishes no variant for them, which is the same case
// as cambricon above: no synthesized image, and a refusal rather than a malformed tag.
//
// The keys are the detector's spelling, capitalized. That survives the whole path to the observed
// detail because the label sanitizer leaves alphanumerics alone; the test asserts that rather than
// assuming it, so a change to the sanitizer fails here instead of in a cluster.
var modelDeploymentAscendVariants = map[string]string{
	"310P": "310p",
	"910B": "910b",
	"910C": "a3",
	"950":  "950",
}

// ModelDeploymentImageBackend returns the runner backend token for an accelerator manufacturer.
//
// The second result reports whether the manufacturer has one at all, which is the question a
// validating webhook asks: a manufacturer with no backend can never produce an image, whatever else
// converges later.
func ModelDeploymentImageBackend(manufacturer string) (string, bool) {
	backend, ok := modelDeploymentBackends[manufacturer]

	return backend, ok
}

// ModelDeploymentImageVariant returns the runner backend variant for a manufacturer and family.
//
// An empty variant with ok=true is the normal answer for every vendor except Ascend, so a caller
// must not read empty as failure. For Ascend, ok=false means the family has no published variant.
func ModelDeploymentImageVariant(manufacturer, family string) (string, bool) {
	if manufacturer != nodefeature.ManufacturerAscend {
		return "", true
	}

	variant, ok := modelDeploymentAscendVariants[family]

	return variant, ok
}

// SynthesizeModelDeploymentImage assembles one role's runner image from the engine the deployment
// declares and the hardware its InstanceType observed.
//
// The shape is a formula, verified against the runner project's own 338 published records with zero
// mismatches:
//
//	gpustack/runner:<backend><runtimeVersion>[-<variant>]-<engine><engineVersion>
//
// It reads no release matrix, so it cannot check that the combination was ever published. That is
// the stated trade: the user guarantees version alignment, and the failure it declines to prevent is
// an ImagePullBackOff on a tag that does not exist. The platform is not part of the formula, which
// is measured rather than assumed -- no published tag carries an architecture, and the 338 records
// collapse to 208 distinct names, the signature of one multi-arch manifest each.
//
// Every error names what is missing, because each one has a different remedy: a manufacturer with no
// backend or a family with no variant will never resolve and the role has to name an image, while an
// unobserved runtime version resolves on a later reconcile.
func SynthesizeModelDeploymentImage(
	engine, engineVersion string, detail workercore.InstanceTypeDetail,
) (string, error) {
	if engineVersion == "" {
		return "", fmt.Errorf("engine version is empty")
	}
	if detail.Manufacturer == "" {
		return "", fmt.Errorf("the instance type has not observed its hardware yet")
	}

	backend, ok := ModelDeploymentImageBackend(detail.Manufacturer)
	if !ok {
		return "", fmt.Errorf("manufacturer %q has no runner backend, so the role must name an image",
			detail.Manufacturer)
	}

	variant, ok := ModelDeploymentImageVariant(detail.Manufacturer, detail.Family)
	if !ok {
		return "", fmt.Errorf("%s family %q has no runner backend variant, so the role must name an image",
			detail.Manufacturer, detail.Family)
	}

	if detail.RuntimeVersion == "" {
		return "", fmt.Errorf("the instance type has not observed a runtime version yet")
	}

	var tag strings.Builder
	tag.WriteString(backend)
	tag.WriteString(detail.RuntimeVersion)
	if variant != "" {
		tag.WriteString("-")
		tag.WriteString(variant)
	}
	tag.WriteString("-")
	tag.WriteString(engine)
	tag.WriteString(engineVersion)

	return modelDeploymentRunnerRepository + ":" + tag.String(), nil
}
