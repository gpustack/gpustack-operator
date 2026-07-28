package main

import (
	"flag"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	yaml "gopkg.in/yaml.v3"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	"gpustack.ai/gpustack/pkg/nodefeature"
)

// update, when passed as "-args -update" to `go test`, rewrites the golden files
// under testdata to the freshly generated content instead of comparing against
// them — the same convention the project's other golden/baseline tests use.
var update = flag.Bool("update", false, "update the golden files in testdata")

// assertGolden asserts got equals the committed content of testdata/name.
func assertGolden(t *testing.T, name, got string) {
	t.Helper()

	path := filepath.Join("testdata", name)
	if *update {
		require.NoError(t, os.WriteFile(path, []byte(got), 0o644))
	}

	want, err := os.ReadFile(path)
	require.NoError(t, err, "read golden file %s", path)
	assert.Equal(t, string(want), got, "generated content must match the committed golden file %s", path)
}

// kueueTransformation mirrors one Kueue config.ResourceTransformation entry.
type kueueTransformation struct {
	Input      string            `yaml:"input"`
	Strategy   string            `yaml:"strategy"`
	MultiplyBy string            `yaml:"multiplyBy"`
	Outputs    map[string]string `yaml:"outputs"`
}

// TestKueueTransformations pins the generated block against its golden file and
// ties every rule's credit factor back to pkg/nodefeature: it recomputes each
// expected factor from CreditsPerCard/SharedResourceMaxSize/ResourceMaxUnits
// rather than hard-coding a number, so a change to any of those constants changes
// this test's expectation along with the generator's output.
func TestKueueTransformations(t *testing.T) {
	got := kueueTransformations()
	assertGolden(t, "kueue-transformations.yaml", got)

	var doc struct {
		Transformations []kueueTransformation `yaml:"transformations"`
	}
	require.NoError(t, yaml.Unmarshal([]byte(got), &doc))

	byInput := make(map[string]kueueTransformation, len(doc.Transformations))
	for _, tr := range doc.Transformations {
		byInput[tr.Input] = tr
	}

	exclusiveFactor := strconv.Itoa(nodefeature.CreditsPerCard)
	sharedFactor := strconv.Itoa(nodefeature.CreditsPerCard / nodefeature.SharedResourceMaxSize)
	unitsFactor := strconv.Itoa(nodefeature.CreditsPerCard / nodefeature.ResourceMaxUnits)

	manus := nodefeature.GetKnownAcceleratableManufacturers()
	require.NotEmpty(t, manus, "precondition: pkg/nodefeature knows at least one manufacturer")

	wantTotal := 3 * len(manus)
	for _, manu := range manus {
		if nodefeature.GetAcceleratablePartitionedUnitsResourceName(manu) != "" {
			wantTotal++
		}
	}
	assert.Len(t, doc.Transformations, wantTotal, "no unexpected transformations")

	cases := []struct {
		name string
		key  func(manu string) string
		want string
	}{
		{
			name: "exclusive whole card is CreditsPerCard credits",
			key: func(manu string) string {
				return string(nodefeature.GetAcceleratableResourceName(manu, workercore.DeviceAllocationModeExclusive))
			},
			want: exclusiveFactor,
		},
		{
			name: "shared ownership is CreditsPerCard/SharedResourceMaxSize credits",
			key: func(manu string) string {
				return string(nodefeature.GetAcceleratableResourceName(manu, workercore.DeviceAllocationModeShared))
			},
			want: sharedFactor,
		},
		{
			name: "sliced unit factor is CreditsPerCard/ResourceMaxUnits, folded by multiplyBy",
			key: func(manu string) string {
				return string(nodefeature.GetAcceleratableSlicedUnitsResourceName(manu))
			},
			want: unitsFactor,
		},
	}

	for _, manu := range manus {
		manu := manu
		credits := string(nodefeature.GetAcceleratableCreditsResourceName(manu))

		for _, c := range cases {
			c := c
			t.Run(manu+"/"+c.name, func(t *testing.T) {
				input := c.key(manu)
				tr, ok := byInput[input]
				require.True(t, ok, "transformation for input %q must exist", input)
				assert.Equal(t, "Replace", tr.Strategy)
				assert.Equal(t, c.want, tr.Outputs[credits])
			})
		}

		t.Run(manu+"/partitioned unit only exists with a partition kind", func(t *testing.T) {
			partitionedUnits := string(nodefeature.GetAcceleratablePartitionedUnitsResourceName(manu))
			if partitionedUnits == "" {
				_, ok := byInput[""]
				assert.False(t, ok, "must not emit a transformation keyed by an empty input")
				return
			}
			tr, ok := byInput[partitionedUnits]
			require.True(t, ok, "transformation for input %q must exist", partitionedUnits)
			assert.Equal(t, "Replace", tr.Strategy)
			assert.Empty(t, tr.MultiplyBy, "a partitioned unit already denotes a fraction of one card")
			assert.Equal(t, unitsFactor, tr.Outputs[credits])
		})
	}
}
