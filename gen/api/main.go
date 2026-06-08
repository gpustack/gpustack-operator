package main

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"

	klog "k8s.io/klog/v2"
	"k8s.io/utils/ptr"

	"gpustack.ai/gpustack/gen/api/builder"
	"gpustack.ai/gpustack/pkg/utils/stringx"
)

func main() {
	err := generate()
	if err != nil {
		klog.Fatalf("error generating: %v", err)
	}
}

func generate() error {
	// Prepare.
	pwd, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("get working directory: %w", err)
	}

	header, err := os.ReadFile(filepath.Join(pwd, "/hack/boilerplate/go.txt"))
	if err != nil {
		return err
	}

	cfg := builder.Config{
		ProjectDir:    pwd,
		Project:       "gpustack.ai/gpustack",
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
