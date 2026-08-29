package preflight

import (
	"os"
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"gpustack.ai/gpustack/pkg/nodefeature"
)

// _ProbeImageDockerfilePath is the build the two default tables are copied from.
const _ProbeImageDockerfilePath = "../../../pack/gpustack-operator/Dockerfile"

// _ProbeImageStageRE matches the xbuild stage headers those tables mirror, capturing the vendor
// image a stage builds against and the suffix naming the stage.
var _ProbeImageStageRE = regexp.MustCompile(
	`(?m)^FROM (\S+) AS xbuild-(ascend-cann-\S+|nvidia-cuda-\S+)$`)

// A probe container runs the manufacturer's preload libraries, and each of those was compiled
// against one specific vendor image: the xbuild stage the final image stages it from. Nothing but
// this keeps the two in step. Bumping a CANN or CUDA tag in the Dockerfile leaves the tables naming
// the tag before it, and the probe then loads libraries built against a runtime the image it started
// does not carry -- surfacing as a loader error inside a container that names neither the tag nor the
// file that disagreed.
func TestProbeImageDefaultsMatchTheBuild(t *testing.T) {
	dockerfile, err := os.ReadFile(_ProbeImageDockerfilePath)
	require.NoError(t, err)

	matches := _ProbeImageStageRE.FindAllStringSubmatch(string(dockerfile), -1)
	require.NotEmpty(t, matches, "the stage headers changed shape; this test now checks nothing")

	// A stage is named for the lib subdirectory it is staged into, minus the manufacturer, which is
	// exactly how the tables are keyed.
	built := make(map[string]string, len(matches))
	for _, m := range matches {
		key := strings.TrimPrefix(strings.TrimPrefix(m[2], "ascend-"), "nvidia-")
		built[key] = m[1]
	}

	want := make(map[string]string, len(ascendProbeImages)+len(nvidiaProbeImages))
	for k, v := range ascendProbeImages {
		want[k] = v
	}
	for k, v := range nvidiaProbeImages {
		want[k] = v
	}

	// Compared whole rather than key by key, so a stage added to the build without a default here
	// fails just as loudly as a default that drifted off its stage.
	assert.Equal(t, want, built)
}

// One tag cannot be right for every family of a manufacturer -- Ascend's 910B, 910C, 950 and 310P
// need different CANN images, and NVIDIA's CUDA major differs by card generation -- so the default
// is derived from what the detect pass recorded, and a family/major this package has no image for
// is a named failure pointing at --probe-image, not a guess.
func TestResolveProbeImage(t *testing.T) {
	testCases := []struct {
		name           string
		manufacturer   string
		family         string
		runtimeVersion string
		override       string
		want           string
		wantErr        string
	}{
		{
			name:         "ascend 910B on CANN 8 resolves the matching quay.io tag",
			manufacturer: nodefeature.ManufacturerAscend, family: "910B", runtimeVersion: "8.0.0",
			want: "quay.io/ascend/cann:8.5.0-910b-ubuntu22.04-py3.11",
		},
		{
			name:         "ascend 910C on CANN 9 resolves a different tag than 910B",
			manufacturer: nodefeature.ManufacturerAscend, family: "910C", runtimeVersion: "9.1.0",
			want: "quay.io/ascend/cann:9.1.0-beta.3-a3-ubuntu22.04-py3.12",
		},
		{
			name:         "ascend 950 resolves its own tag",
			manufacturer: nodefeature.ManufacturerAscend, family: "950", runtimeVersion: "9.1.0",
			want: "quay.io/ascend/cann:9.1.0-beta.3-950-ubuntu22.04-py3.12",
		},
		{
			name:         "ascend 310P resolves its own tag",
			manufacturer: nodefeature.ManufacturerAscend, family: "310P", runtimeVersion: "9.1.0",
			want: "quay.io/ascend/cann:9.1.0-beta.3-310p-ubuntu22.04-py3.12",
		},
		{
			// The family this image builds no vcann-rt for at all: a guess here would start a
			// container that exits 127, and report a node that is fine as broken.
			name:         "an ascend family with no build stage is a named failure, not a guess",
			manufacturer: nodefeature.ManufacturerAscend, family: "910A", runtimeVersion: "8.0.0",
			wantErr: "--probe-image",
		},
		{
			// Same family, but a CANN major this image never built vcann-rt against -- the failure
			// has to be keyed on the pair, not the family alone.
			name:         "an ascend family on an unbuilt CANN major is a named failure",
			manufacturer: nodefeature.ManufacturerAscend, family: "910B", runtimeVersion: "10.0.0",
			wantErr: "--probe-image",
		},
		{
			name:         "nvidia CUDA 12 resolves the matching tag",
			manufacturer: nodefeature.ManufacturerNVIDIA, runtimeVersion: "12.9",
			want: "nvidia/cuda:12.9.2-cudnn-devel-ubi8",
		},
		{
			name:         "nvidia CUDA 13 resolves a different tag than CUDA 12",
			manufacturer: nodefeature.ManufacturerNVIDIA, runtimeVersion: "13.0",
			want: "nvidia/cuda:13.0.3-cudnn-devel-ubi8",
		},
		{
			name:         "an nvidia CUDA major this image never built HAMi-core against is a named failure",
			manufacturer: nodefeature.ManufacturerNVIDIA, runtimeVersion: "14.0",
			wantErr: "--probe-image",
		},
		{
			// AMD's shim links only glibc, so unlike Ascend and NVIDIA it needs no family-specific
			// image at all -- one default serves every AMD family.
			name:         "amd gets one default regardless of family",
			manufacturer: nodefeature.ManufacturerAMD, family: "gfx942",
			want: amdProbeImage,
		},
		{
			// A manufacturer this package has no evidence for at all: not guessed.
			name:         "a manufacturer with no default is a named failure",
			manufacturer: nodefeature.ManufacturerMThreads,
			wantErr:      "--probe-image",
		},
		{
			name:         "override wins verbatim over a resolvable default",
			manufacturer: nodefeature.ManufacturerAscend, family: "910B", runtimeVersion: "8.0.0",
			override: "registry.example.com/custom:tag",
			want:     "registry.example.com/custom:tag",
		},
		{
			// The override is the only way to probe a family this package cannot resolve, so it
			// has to win there too, not just where a default already exists.
			name:         "override wins even for a family with no default",
			manufacturer: nodefeature.ManufacturerMThreads,
			override:     "registry.example.com/custom:tag",
			want:         "registry.example.com/custom:tag",
		},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ResolveProbeImage(tc.manufacturer, tc.family, tc.runtimeVersion, tc.override)

			if tc.wantErr != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tc.wantErr)
				assert.Empty(t, got)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}
}
