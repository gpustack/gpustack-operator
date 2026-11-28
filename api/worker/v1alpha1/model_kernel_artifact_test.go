package v1alpha1

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apiextensions "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions"
	extension "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apiservervalidation "k8s.io/apiextensions-apiserver/pkg/apiserver/validation"
)

func crdValidator(t *testing.T, kind string) apiservervalidation.SchemaValidator {
	t.Helper()

	crd := GetCustomResourceDefinitions()[kind]
	require.NotNil(t, crd, "%s CRD must be registered", kind)
	require.Len(t, crd.Spec.Versions, 1)
	require.NotNil(t, crd.Spec.Versions[0].Schema)
	require.NotNil(t, crd.Spec.Versions[0].Schema.OpenAPIV3Schema)

	var internalSchema apiextensions.JSONSchemaProps
	require.NoError(t, extension.Convert_v1_JSONSchemaProps_To_apiextensions_JSONSchemaProps(
		crd.Spec.Versions[0].Schema.OpenAPIV3Schema, &internalSchema, nil,
	))

	validator, _, err := apiservervalidation.NewSchemaValidator(&internalSchema)
	require.NoError(t, err)
	return validator
}

func TestModelKernelArtifactCRDSchemaAndConfigMapOmission(t *testing.T) {
	crd := GetCustomResourceDefinitions()["ModelKernelArtifact"]
	require.NotNil(t, crd, "ModelKernelArtifact CRD must be registered")
	assert.Equal(t, extension.NamespaceScoped, crd.Spec.Scope)
	assert.Contains(t, crd.Spec.Names.ShortNames, "mkart")
	assert.Contains(t, crd.Spec.Names.Categories, "gpustack")
	require.Len(t, crd.Spec.Versions, 1)
	require.NotNil(t, crd.Spec.Versions[0].Subresources)
	assert.NotNil(t, crd.Spec.Versions[0].Subresources.Status, "v1alpha1 must expose status subresource")

	root := crd.Spec.Versions[0].Schema.OpenAPIV3Schema
	source := root.Properties["spec"].Properties["source"]
	assert.Contains(t, source.Properties, "nodeStore")
	assert.Contains(t, source.Properties, "huggingFace")
	assert.Contains(t, source.Properties, "image")
	assert.NotContains(t, source.Properties, "configMap", "configMap source must be structurally omitted")
	assert.NotContains(t, source.Properties, "persistentVolumeClaim")
	assert.NotContains(t, source.Properties, "modelScope")

	compat := root.Properties["spec"].Properties["compatibility"]
	assert.ElementsMatch(t, []string{`"vLLM"`, `"SGLang"`}, enumValues(t, compat, "engine"))
	keys := compat.Properties["kernelKeys"]
	require.NotNil(t, keys.MinItems)
	assert.Equal(t, int64(1), *keys.MinItems)
	require.NotNil(t, keys.MaxItems)
	assert.Equal(t, int64(128), *keys.MaxItems)

	status := root.Properties["status"]
	for _, fieldName := range []string{
		"observedGeneration", "resolved", "nodes",
		"quarantined", "quarantineReason", "quarantineMessage", "conditions",
	} {
		assert.Contains(t, status.Properties, fieldName)
	}
	resolved := status.Properties["resolved"]
	assert.ElementsMatch(t, []string{`"Forge"`, `"Hub"`}, enumValues(t, resolved, "digestSource"))

	validator := crdValidator(t, "ModelKernelArtifact")
	digestA := "sha256:" + strings.Repeat("a", 64)
	digestB := "sha256:" + strings.Repeat("b", 64)

	cases := []struct {
		name    string
		doc     map[string]any
		wantErr bool
	}{
		{
			name: "valid nodeStore source",
			doc: map[string]any{
				"apiVersion": "worker.gpustack.ai/v1alpha1",
				"kind":       "ModelKernelArtifact",
				"metadata":   map[string]any{"name": "qwen-decode-gen-1", "namespace": "team-a"},
				"spec": map[string]any{
					"source": map[string]any{
						"nodeStore": map[string]any{
							"manifestDigest":         digestA,
							"dispatchManifestDigest": digestB,
							"sizeBytes":              int64(4096),
							"fileCount":              int64(3),
							"seedNode":               "gpu-node-1",
							"forgerRef":              map[string]any{"name": "qwen-forger", "uid": "uid-1"},
						},
					},
					"compatibility": map[string]any{
						"engine":        "vLLM",
						"engineVersion": "0.8.5",
						"manufacturer":  "nvidia",
						"architecture":  "sm_90a",
						"kernelKeys":    []any{"vLLM/0.8.5/nvidia/sm_90a/rmsnorm/0123456789abcdef"},
					},
				},
			},
		},
		{
			name: "invalid digest pattern rejected",
			doc: map[string]any{
				"apiVersion": "worker.gpustack.ai/v1alpha1",
				"kind":       "ModelKernelArtifact",
				"metadata":   map[string]any{"name": "bad-digest", "namespace": "team-a"},
				"spec": map[string]any{
					"source": map[string]any{
						"nodeStore": map[string]any{
							"manifestDigest":         "sha256:not-64-hex",
							"dispatchManifestDigest": digestB,
							"sizeBytes":              int64(4096),
							"fileCount":              int64(2),
							"seedNode":               "gpu-node-1",
							"forgerRef":              map[string]any{"name": "qwen-forger", "uid": "uid-1"},
						},
					},
					"compatibility": map[string]any{
						"engine":        "vLLM",
						"engineVersion": "0.8.5",
						"manufacturer":  "nvidia",
						"architecture":  "sm_90a",
						"kernelKeys":    []any{"vLLM/0.8.5/nvidia/sm_90a/rmsnorm/0123456789abcdef"},
					},
				},
			},
			wantErr: true,
		},
		{
			name: "fileCount below 2 rejected",
			doc: map[string]any{
				"apiVersion": "worker.gpustack.ai/v1alpha1",
				"kind":       "ModelKernelArtifact",
				"metadata":   map[string]any{"name": "bad-files", "namespace": "team-a"},
				"spec": map[string]any{
					"source": map[string]any{
						"nodeStore": map[string]any{
							"manifestDigest":         digestA,
							"dispatchManifestDigest": digestB,
							"sizeBytes":              int64(4096),
							"fileCount":              int64(1),
							"seedNode":               "gpu-node-1",
							"forgerRef":              map[string]any{"name": "qwen-forger", "uid": "uid-1"},
						},
					},
					"compatibility": map[string]any{
						"engine":        "SGLang",
						"engineVersion": "0.4.6",
						"manufacturer":  "amd",
						"architecture":  "gfx942",
						"kernelKeys":    []any{"SGLang/0.4.6/amd/gfx942/rmsnorm/0123456789abcdef"},
					},
				},
			},
			wantErr: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res := validator.Validate(tc.doc)
			if tc.wantErr {
				assert.False(t, res.IsValid(), "expected schema validation failure")
			} else {
				assert.True(t, res.IsValid(), "unexpected schema errors: %v", res.Errors)
			}
		})
	}

	t.Run("strict JSON decoding rejects configMap source", func(t *testing.T) {
		raw := `{"source":{"configMap":{"name":"illegal"}},"compatibility":{"engine":"vLLM","engineVersion":"0.8.5","manufacturer":"nvidia","architecture":"sm_90a","kernelKeys":["k1"]}}`
		dec := json.NewDecoder(strings.NewReader(raw))
		dec.DisallowUnknownFields()
		var spec ModelKernelArtifactSpec
		err := dec.Decode(&spec)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "configMap")
	})

	assert.Equal(t, "worker.gpustack.ai/model-kernel-artifact-protection", ModelKernelArtifactProtectionFinalizer)
}

func TestNodeModelStoreSourceIncludesForge(t *testing.T) {
	crd := GetCustomResourceDefinitions()["NodeModelStore"]
	require.NotNil(t, crd)
	models := crd.Spec.Versions[0].Schema.OpenAPIV3Schema.Properties["status"].Properties["models"].Items.Schema
	assert.ElementsMatch(t, []string{`"Hub"`, `"Peer"`, `"Forge"`}, enumValues(t, *models, "source"))
	assert.Equal(t, NodeModelStoreModelSource("Forge"), NodeModelStoreModelSourceForge)
}
