package generators

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	apiext "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apimachinery/pkg/util/sets"
	gengov2 "k8s.io/gengo/v2"
	"k8s.io/gengo/v2/generator"

	crdgenargs "gpustack.ai/gpustack/gen/api/generator/cmd/crd-gen/args"
	crdgen "gpustack.ai/gpustack/gen/api/generator/cmd/crd-gen/generators/testdata/crd_gen"
	crdgennegative "gpustack.ai/gpustack/gen/api/generator/cmd/crd-gen/generators/testdata/crd_gen_alias_marker_negative"
)

const (
	crdGenFixturePkg         = "gpustack.ai/gpustack/gen/api/generator/cmd/crd-gen/generators/testdata/crd_gen"
	crdGenNegativeFixturePkg = "gpustack.ai/gpustack/gen/api/generator/cmd/crd-gen/generators/testdata/crd_gen_alias_marker_negative"
)

// generateFixture runs the generator over the given fixture package and returns
// the file it generated.
func generateFixture(t *testing.T, dir, pkg string) []byte {
	t.Helper()

	g := crdgenargs.Args{
		OutputFile:   "zz_generated.crds.go",
		GoHeaderFile: filepath.Join(dir, "go.txt"),
	}

	err := gengov2.Execute(
		NameSystems(),
		DefaultNameSystem(),
		func(ctx *generator.Context) []generator.Target {
			return GetTargets(ctx, &g)
		},
		gengov2.StdBuildTag,
		[]string{pkg},
	)
	if err != nil {
		t.Fatalf("failed to execute crd-gen: %v", err)
	}

	actualBytes, err := os.ReadFile(filepath.Join(dir, "zz_generated.crds.go"))
	if err != nil {
		t.Fatalf("failed to read actual file: %v", err)
	}

	return actualBytes
}

// assertGolden compares the generated file of a fixture with the golden file
// that records it.
func assertGolden(t *testing.T, dir string, actualBytes []byte) {
	t.Helper()

	expectedBytes, err := os.ReadFile(filepath.Join(dir, "zz_generated.crds.go.expected"))
	if err != nil {
		t.Fatalf("failed to read expected file: %v", err)
	}

	assert.Equal(t, string(expectedBytes), string(actualBytes))
}

// printerColumns returns the names of the printer columns of the first version
// of the given CRD.
func printerColumns(crd *apiext.CustomResourceDefinition) []string {
	columns := make([]string, 0, len(crd.Spec.Versions[0].AdditionalPrinterColumns))
	for _, column := range crd.Spec.Versions[0].AdditionalPrinterColumns {
		columns = append(columns, column.Name)
	}
	return columns
}

func Test_crdGen(t *testing.T) {
	dir := filepath.Join("testdata", "crd_gen")

	assertGolden(t, dir, generateFixture(t, dir, crdGenFixturePkg))
}

// Test_crdGen_aliasCommentTag pins what a comment tag on an alias means.
//
// The fixture gives one of its aliases a print column tag of its own. A tag on
// an alias configures nothing: the CRD takes its tags from the CRD type, so the
// tag of an alias must not become a printer column, and the alias must not
// become a CRD of its own. The alias still has to resolve to the schema of the
// type it aliases, so that a tag reaching the CRD is a change of the CRD and
// not a change of what the field means.
//
// The assertions read the generated file that is checked in, which Test_crdGen
// keeps in step with a fresh generation. They say what the CRD means, so a
// generator that started attaching the tag of an alias is caught here even if
// the golden file were updated to match.
func Test_crdGen_aliasCommentTag(t *testing.T) {
	crds := crdgen.GetCustomResourceDefinitions()

	crd, ok := crds["Dummy"]
	if !ok {
		t.Fatalf("expected a CRD for Dummy, got %v", sets.List(sets.KeySet(crds)))
	}

	version := crd.Spec.Versions[0]
	markerAlias := version.Schema.OpenAPIV3Schema.Properties["spec"].Properties["markerAlias"]

	tests := []struct {
		name string
		got  any
		want any
	}{
		{
			name: "an alias does not become a CRD of its own",
			got:  sets.List(sets.KeySet(crds)),
			want: []string{"Dummy"},
		},
		{
			name: "the tag of an alias does not become a printer column",
			got:  printerColumns(crd),
			want: []string{"Detached", "String", "Integer", "Filtered", "Age"},
		},
		{
			name: "an alias still resolves to the schema of the type it aliases",
			got:  markerAlias,
			want: apiext.JSONSchemaProps{
				Type:     "object",
				Required: []string{"field"},
				Properties: map[string]apiext.JSONSchemaProps{
					"field": {
						Type: "string",
					},
				},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, tt.got)
		})
	}
}

// Test_crdGen_aliasCommentTagNegativeControl is the negative control of the
// alias comment tag fixture.
//
// The comment tag is the same one the alias fixture puts on its alias, and here
// it sits on the type the CRD is generated from. It does configure a printer
// column, which is what shows that the column the alias fixture does not
// produce is a real distinction and not a tag that never reaches anything.
func Test_crdGen_aliasCommentTagNegativeControl(t *testing.T) {
	dir := filepath.Join("testdata", "crd_gen_alias_marker_negative")

	assertGolden(t, dir, generateFixture(t, dir, crdGenNegativeFixturePkg))

	crds := crdgennegative.GetCustomResourceDefinitions()

	crd, ok := crds["Target"]
	if !ok {
		t.Fatalf("expected a CRD for Target, got %v", sets.List(sets.KeySet(crds)))
	}

	assert.Equal(t, []string{"MarkerAlias"}, printerColumns(crd))
}
