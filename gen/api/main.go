package main

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	klog "k8s.io/klog/v2"
	"k8s.io/utils/ptr"

	"gpustack.ai/gpustack/gen/api/builder"
	"gpustack.ai/gpustack/pkg/utils/stringx"
)

// projectModule is this repository's module import path. The generators derive their
// output base by trimming it off the working directory (see builder.Config.Project), so
// it is both the value they are configured with and the suffix the working directory has
// to carry -- one constant for both, because a mismatch between them is silent.
const projectModule = "gpustack.ai/gpustack"

func main() {
	err := generate()
	if err != nil {
		klog.Fatalf("error generating: %v", err)
	}
}

// resolveProjectDir turns the working directory into the path the generators may write
// against, or refuses.
//
// The generators compute their output base by trimming projectModule off this path, so a
// directory that does not end in it makes the trim a no-op and sends every generator one
// module path too shallow. That failure is not clean: go-to-protobuf rewrites the
// generated files before anything notices the path is wrong, so the refusal has to happen
// above the first write.
//
// Two details carry the whole check:
//
//   - the path is resolved first, because os.Getwd can return the logical $PWD while the
//     Go toolchain follows symlinks. A checkout reached through a link named after the
//     module path would pass a check on the unresolved path and still fail underneath.
//   - the separator is part of the suffix, making it a directory boundary rather than a
//     string tail: "/tmp/mygpustack.ai/gpustack" ends in the module path, and trimming it
//     yields "/tmp/my" -- exactly the wrong-tree write this exists to stop.
func resolveProjectDir(pwd string) (string, error) {
	resolved, err := filepath.EvalSymlinks(pwd)
	if err != nil {
		return "", fmt.Errorf("resolve working directory %q: %w", pwd, err)
	}

	if !strings.HasSuffix(filepath.ToSlash(resolved), "/"+projectModule) {
		return "", fmt.Errorf(
			"working directory %q does not end in the module import path %q: "+
				"the generators derive their output base by trimming it off, so running here "+
				"would write to the wrong tree; run from a checkout whose resolved path ends in %[2]q",
			resolved, projectModule)
	}

	return resolved, nil
}

func generate() error {
	// Prepare.
	pwd, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("get working directory: %w", err)
	}

	// REFUSE BEFORE WRITING ANYTHING, and use the resolved path for everything after.
	pwd, err = resolveProjectDir(pwd)
	if err != nil {
		return err
	}

	header, err := os.ReadFile(filepath.Join(pwd, "/hack/boilerplate/go.txt"))
	if err != nil {
		return err
	}

	cfg := builder.Config{
		ProjectDir:    pwd,
		Project:       projectModule,
		ClientsName:   "kubeclients",
		ClientSetName: "kubernetes",
		Header:        stringx.FromBytes(ptr.To(bytes.TrimSpace(header))),
		/*
			Specify the package paths of the CRD APIs.
		*/
		APIs: []string{
			"gpustack.ai/gpustack/api/worker/v1alpha1",
		},
		/*
			Specify the package paths of the Extension APIs.
		*/
		ExtensionAPIs: []string{
			"gpustack.ai/gpustack/api/v1",
			"gpustack.ai/gpustack/api/worker/v1",
		},
		/*
			Specify the package paths of the 3rd-party packages which APIs and ExtensionAPIs rely on.
		*/
		MachineryAPIs: []string{
			"k8s.io/apimachinery/pkg/api/resource",
			"k8s.io/apimachinery/pkg/apis/meta/v1",
			"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured",
			"k8s.io/apimachinery/pkg/runtime",
			"k8s.io/apimachinery/pkg/runtime/schema",
			"k8s.io/apimachinery/pkg/types",
			"k8s.io/apimachinery/pkg/version",
			"k8s.io/apimachinery/pkg/util/intstr",
			"k8s.io/api/core/v1",
		},
		/*
			Specify the package paths of the External APIs which embed into the clientset.
		*/
		ExternalAPIs: []string{
			// Kubernetes.
			"k8s.io/api/admission/v1",
			"k8s.io/api/admissionregistration/v1",
			"k8s.io/api/apidiscovery/v2",
			"k8s.io/api/apiserverinternal/v1alpha1",
			"k8s.io/api/apps/v1",
			"k8s.io/api/authentication/v1",
			"k8s.io/api/authorization/v1",
			"k8s.io/api/autoscaling/v1",
			"k8s.io/api/autoscaling/v2",
			"k8s.io/api/batch/v1",
			"k8s.io/api/certificates/v1",
			"k8s.io/api/coordination/v1",
			"k8s.io/api/core/v1",
			"k8s.io/api/discovery/v1",
			"k8s.io/api/events/v1",
			"k8s.io/api/flowcontrol/v1",
			"k8s.io/api/imagepolicy/v1alpha1",
			"k8s.io/api/networking/v1",
			"k8s.io/api/node/v1",
			"k8s.io/api/policy/v1",
			"k8s.io/api/rbac/v1",
			"k8s.io/api/resource/v1",
			"k8s.io/api/scheduling/v1",
			"k8s.io/api/storage/v1",
			"k8s.io/api/storagemigration/v1beta1",
			"k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1",
			"k8s.io/kube-aggregator/pkg/apis/apiregistration/v1",
			// Kueue.
			"sigs.k8s.io/kueue/apis/kueue/v1beta2",
			// Node Feature Discovery.
			"sigs.k8s.io/node-feature-discovery/api/nfd/v1alpha1",
		},
		/*
			Specify the package paths of the Admission webhooks.
		*/
		Webhooks: []string{
			"gpustack.ai/gpustack/pkg/worker/webhooks/worker",
		},
		/*
			Specify the exceptions to the plural form.
		*/
		PluralExceptions: map[string]string{
			"Endpoints": "Endpoints",
			"Devices":   "Devices",
		},
		/*
			The physical location to provide the protobuf files for proto generation.
		*/
		ProtoImports: []string{
			// NB(thxCode): the go-to-protobuf under code-generator relies on a deprecated project,
			// https://github.com/gogo/protobuf.
			// The upstream already filed an issue about this,
			// https://github.com/kubernetes/kubernetes/issues/96564.
			// In order to support generating protobuf code for extension APIs,
			// we need to tell protoc where to find the gogo/protobuf.
			filepath.Join(pwd, "staging"),
		},
	}

	// Generate.
	return builder.Generate(cfg)
}
