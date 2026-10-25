package generators

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	gengov2 "k8s.io/gengo/v2"
	"k8s.io/gengo/v2/generator"

	webhookgenargs "gpustack.ai/gpustack/gen/api/generator/cmd/webhook-gen/args"
)

func Test_webhookGen(t *testing.T) {
	var (
		dir = filepath.Join("testdata", "webhook_gen")
		pkg = "gpustack.ai/gpustack/gen/api/generator/cmd/webhook-gen/generators/testdata/webhook_gen"
	)

	g := webhookgenargs.Args{
		OutputFile:   "zz_generated.webhooks.go",
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
		t.Fatalf("failed to execute webhook-gen: %v", err)
	}

	actualBytes, err := os.ReadFile(filepath.Join(dir, "zz_generated.webhooks.go"))
	if err != nil {
		t.Fatalf("failed to read actual file: %v", err)
	}

	expectedBytes, err := os.ReadFile(filepath.Join(dir, "zz_generated.webhooks.go.expected"))
	if err != nil {
		t.Fatalf("failed to read expected file: %v", err)
	}

	assert.Equal(t, string(expectedBytes), string(actualBytes))
}
