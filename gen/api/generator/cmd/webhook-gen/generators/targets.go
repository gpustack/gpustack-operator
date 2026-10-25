package generators

import (
	gengov2 "k8s.io/gengo/v2"
	"k8s.io/gengo/v2/generator"
	"k8s.io/gengo/v2/namer"
	"k8s.io/gengo/v2/types"
	klog "k8s.io/klog/v2"

	"gpustack.ai/gpustack/gen/api/generator/cmd/webhook-gen/args"
	"gpustack.ai/gpustack/gen/api/generator/utils"
)

// NameSystems returns the name system used by the generators in this package.
func NameSystems() namer.NameSystems {
	return namer.NameSystems{
		"public": namer.NewPublicNamer(0),
	}
}

// DefaultNameSystem returns the default name system for ordering the types to be
// processed by the generators in this package.
func DefaultNameSystem() string {
	return "public"
}

// GetTargets returns the targets the webhook generator writes to.
//
// There is one target per input package. A target writes into the directory of
// the package it describes, so the generated file lands next to the types it
// was generated from.
func GetTargets(context *generator.Context, arguments *args.Args) []generator.Target {
	header, err := utils.Header(gengov2.StdBuildTag, arguments.GoHeaderFile)
	if err != nil {
		klog.Fatalf("Failed loading boilerplate: %v", err)
	}

	targets := []generator.Target{}
	for i := range context.Inputs {
		klog.V(5).Infof("considering pkg %q", context.Inputs[i])
		pkg := context.Universe[context.Inputs[i]]
		if pkg == nil {
			// If the input had no Go files, for example.
			continue
		}

		targets = append(targets, &generator.SimpleTarget{
			PkgName:       pkg.Name,
			PkgPath:       pkg.Path,
			PkgDir:        pkg.Dir,
			HeaderComment: header,
			FilterFunc: func(c *generator.Context, t *types.Type) bool {
				return t.Name.Package == pkg.Path
			},
			GeneratorsFunc: func(c *generator.Context) []generator.Generator {
				return []generator.Generator{
					NewWebhookGen(arguments.OutputFile, pkg.Path),
				}
			},
		})
	}

	return targets
}
