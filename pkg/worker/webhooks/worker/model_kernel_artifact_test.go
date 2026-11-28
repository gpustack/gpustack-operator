package worker

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	admissionv1 "k8s.io/api/admission/v1"
	authn "k8s.io/api/authentication/v1"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrladmission "sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	"gpustack.ai/gpustack/pkg/kubediscovery"
	"gpustack.ai/gpustack/pkg/webhook"
)

const (
	testKernelManifestDigest = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	testKernelDispatchDigest = "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	testKernelCommitSHA      = "0123456789abcdef0123456789abcdef01234567"
	testKernelValidKeyVLLM   = "vllm/0.8/nvidia/sm_90a/rmsnorm/0123456789abcdef"
	testKernelValidKeyVLLM2  = "vllm/0.8/nvidia/sm_90a/rope_kv_append/fedcba9876543210"
	testKernelValidKeySGLang = "sglang/0.4/amd/gfx942/rmsnorm/0123456789abcdef"
	testKernelValidKeyAscend = "vllm/0.8/ascend/910b/moe_router_topk/0123456789abcdef"
	testKernelTenantUsername = "system:serviceaccount:team-a:default"
)

func createAdmissionContext(username string) context.Context {
	return ctrladmission.NewContextWithRequest(context.Background(), ctrladmission.Request{
		AdmissionRequest: admissionv1.AdmissionRequest{
			Operation: admissionv1.Create,
			UserInfo:  authn.UserInfo{Username: username},
		},
	})
}

func newTestKernelArtifactNodeStore(mutate ...func(*workercore.ModelKernelArtifact)) *workercore.ModelKernelArtifact {
	mka := &workercore.ModelKernelArtifact{
		ObjectMeta: meta.ObjectMeta{Namespace: "team-a", Name: "qwen-decode-gen-1"},
		Spec: workercore.ModelKernelArtifactSpec{
			Source: workercore.ModelKernelArtifactSource{
				NodeStore: &workercore.ModelKernelArtifactNodeStoreSource{
					ManifestDigest:         testKernelManifestDigest,
					DispatchManifestDigest: testKernelDispatchDigest,
					SizeBytes:              4096,
					FileCount:              3,
					SeedNode:               "gpu-node-1",
					ForgerRef: workercore.ModelKernelArtifactForgerReference{
						Name: "qwen-forger",
						UID:  "forger-uid-1",
					},
				},
			},
			Compatibility: workercore.ModelKernelArtifactCompatibility{
				Engine:        workercore.ModelDeploymentEngineVLLM,
				EngineVersion: "0.8",
				Manufacturer:  "nvidia",
				Architecture:  "sm_90a",
				KernelKeys:    []string{testKernelValidKeyVLLM},
			},
		},
	}
	for _, m := range mutate {
		m(mka)
	}
	return mka
}

func newTestKernelArtifactHub(mutate ...func(*workercore.ModelKernelArtifact)) *workercore.ModelKernelArtifact {
	mka := &workercore.ModelKernelArtifact{
		ObjectMeta: meta.ObjectMeta{Namespace: "team-a", Name: "qwen-hub-kernels"},
		Spec: workercore.ModelKernelArtifactSpec{
			Source: workercore.ModelKernelArtifactSource{
				HuggingFace: &workercore.ModelArtifactHubSource{
					Repository: "team-a/qwen-kernels",
					Revision:   testKernelCommitSHA,
				},
			},
			Compatibility: workercore.ModelKernelArtifactCompatibility{
				Engine:        workercore.ModelDeploymentEngineVLLM,
				EngineVersion: "0.8",
				Manufacturer:  "nvidia",
				Architecture:  "sm_90a",
				KernelKeys:    []string{testKernelValidKeyVLLM},
			},
		},
	}
	for _, m := range mutate {
		m(mka)
	}
	return mka
}

func newTestKernelArtifactImage(mutate ...func(*workercore.ModelKernelArtifact)) *workercore.ModelKernelArtifact {
	mka := &workercore.ModelKernelArtifact{
		ObjectMeta: meta.ObjectMeta{Namespace: "team-a", Name: "qwen-image-kernels"},
		Spec: workercore.ModelKernelArtifactSpec{
			Source: workercore.ModelKernelArtifactSource{
				Image: &workercore.ModelArtifactImageSource{
					Reference: "registry.example.com/team/kernels@" + testKernelManifestDigest,
				},
			},
			Compatibility: workercore.ModelKernelArtifactCompatibility{
				Engine:        workercore.ModelDeploymentEngineVLLM,
				EngineVersion: "0.8",
				Manufacturer:  "nvidia",
				Architecture:  "sm_90a",
				KernelKeys:    []string{testKernelValidKeyVLLM},
			},
		},
	}
	for _, m := range mutate {
		m(mka)
	}
	return mka
}

func TestModelKernelArtifactWebhookValidateCreate(t *testing.T) {
	defaultVersion := modelArtifactClusterVersion
	t.Cleanup(func() { modelArtifactClusterVersion = defaultVersion })
	supportedVersion := func() kubediscovery.Version { return kubediscovery.Version{GitVersion: "v1.35.5"} }
	unsupportedVersion := func() kubediscovery.Version { return kubediscovery.Version{GitVersion: "v1.32.9"} }

	cases := []struct {
		name        string
		worker      string
		ctx         context.Context
		version     func() kubediscovery.Version
		in          *workercore.ModelKernelArtifact
		wantField   string
		wantMessage string
	}{
		{
			name:   "worker creates valid nodeStore artifact",
			worker: testWorkerUsername,
			ctx:    createAdmissionContext(testWorkerUsername),
			in:     newTestKernelArtifactNodeStore(),
		},
		{
			name:        "tenant cannot create nodeStore artifact",
			worker:      testWorkerUsername,
			ctx:         createAdmissionContext(testKernelTenantUsername),
			in:          newTestKernelArtifactNodeStore(),
			wantField:   "spec.source.nodeStore",
			wantMessage: workercore.ModelKernelArtifactReasonNodeStoreSourceForbidden,
		},
		{
			name:        "admin cannot create nodeStore artifact",
			worker:      testWorkerUsername,
			ctx:         createAdmissionContext("kubernetes-admin"),
			in:          newTestKernelArtifactNodeStore(),
			wantField:   "spec.source.nodeStore",
			wantMessage: workercore.ModelKernelArtifactReasonNodeStoreSourceForbidden,
		},
		{
			name:        "unlearned worker identity refuses nodeStore creation",
			worker:      "",
			ctx:         createAdmissionContext(testWorkerUsername),
			in:          newTestKernelArtifactNodeStore(),
			wantField:   "spec.source.nodeStore",
			wantMessage: workercore.ModelKernelArtifactReasonNodeStoreSourceForbidden,
		},
		{
			name:        "missing admission request refuses nodeStore creation",
			worker:      testWorkerUsername,
			ctx:         context.Background(),
			in:          newTestKernelArtifactNodeStore(),
			wantField:   "spec.source.nodeStore",
			wantMessage: workercore.ModelKernelArtifactReasonNodeStoreSourceForbidden,
		},
		{
			name:   "nodeStore rejects invalid manifestDigest",
			worker: testWorkerUsername,
			ctx:    createAdmissionContext(testWorkerUsername),
			in: newTestKernelArtifactNodeStore(func(m *workercore.ModelKernelArtifact) {
				m.Spec.Source.NodeStore.ManifestDigest = "sha256:short"
			}),
			wantField: "spec.source.nodeStore.manifestDigest",
		},
		{
			name:   "nodeStore rejects invalid dispatchManifestDigest",
			worker: testWorkerUsername,
			ctx:    createAdmissionContext(testWorkerUsername),
			in: newTestKernelArtifactNodeStore(func(m *workercore.ModelKernelArtifact) {
				m.Spec.Source.NodeStore.DispatchManifestDigest = "md5:1234"
			}),
			wantField: "spec.source.nodeStore.dispatchManifestDigest",
		},
		{
			name:   "nodeStore rejects non-positive sizeBytes",
			worker: testWorkerUsername,
			ctx:    createAdmissionContext(testWorkerUsername),
			in: newTestKernelArtifactNodeStore(func(m *workercore.ModelKernelArtifact) {
				m.Spec.Source.NodeStore.SizeBytes = 0
			}),
			wantField: "spec.source.nodeStore.sizeBytes",
		},
		{
			name:   "nodeStore rejects fileCount below 2",
			worker: testWorkerUsername,
			ctx:    createAdmissionContext(testWorkerUsername),
			in: newTestKernelArtifactNodeStore(func(m *workercore.ModelKernelArtifact) {
				m.Spec.Source.NodeStore.FileCount = 1
			}),
			wantField: "spec.source.nodeStore.fileCount",
		},
		{
			name:   "nodeStore rejects empty seedNode",
			worker: testWorkerUsername,
			ctx:    createAdmissionContext(testWorkerUsername),
			in: newTestKernelArtifactNodeStore(func(m *workercore.ModelKernelArtifact) {
				m.Spec.Source.NodeStore.SeedNode = ""
			}),
			wantField: "spec.source.nodeStore.seedNode",
		},
		{
			name:   "nodeStore rejects whitespace-padded seedNode",
			worker: testWorkerUsername,
			ctx:    createAdmissionContext(testWorkerUsername),
			in: newTestKernelArtifactNodeStore(func(m *workercore.ModelKernelArtifact) {
				m.Spec.Source.NodeStore.SeedNode = " gpu-node-1 "
			}),
			wantField: "spec.source.nodeStore.seedNode",
		},
		{
			name:   "nodeStore rejects empty forgerRef name",
			worker: testWorkerUsername,
			ctx:    createAdmissionContext(testWorkerUsername),
			in: newTestKernelArtifactNodeStore(func(m *workercore.ModelKernelArtifact) {
				m.Spec.Source.NodeStore.ForgerRef.Name = ""
			}),
			wantField: "spec.source.nodeStore.forgerRef.name",
		},
		{
			name:   "nodeStore rejects overlong forgerRef name",
			worker: testWorkerUsername,
			ctx:    createAdmissionContext(testWorkerUsername),
			in: newTestKernelArtifactNodeStore(func(m *workercore.ModelKernelArtifact) {
				m.Spec.Source.NodeStore.ForgerRef.Name = strings.Repeat("a", 254)
			}),
			wantField: "spec.source.nodeStore.forgerRef.name",
		},
		{
			name:   "nodeStore rejects empty forgerRef uid",
			worker: testWorkerUsername,
			ctx:    createAdmissionContext(testWorkerUsername),
			in: newTestKernelArtifactNodeStore(func(m *workercore.ModelKernelArtifact) {
				m.Spec.Source.NodeStore.ForgerRef.UID = ""
			}),
			wantField: "spec.source.nodeStore.forgerRef.uid",
		},
		{
			name:   "nodeStore rejects overlong forgerRef uid",
			worker: testWorkerUsername,
			ctx:    createAdmissionContext(testWorkerUsername),
			in: newTestKernelArtifactNodeStore(func(m *workercore.ModelKernelArtifact) {
				m.Spec.Source.NodeStore.ForgerRef.UID = strings.Repeat("u", 129)
			}),
			wantField: "spec.source.nodeStore.forgerRef.uid",
		},
		{
			name: "source union rejects empty source",
			ctx:  createAdmissionContext(testKernelTenantUsername),
			in: newTestKernelArtifactHub(func(m *workercore.ModelKernelArtifact) {
				m.Spec.Source = workercore.ModelKernelArtifactSource{}
			}),
			wantField: "spec.source",
		},
		{
			name: "source union rejects multiple sources",
			ctx:  createAdmissionContext(testWorkerUsername),
			in: newTestKernelArtifactNodeStore(func(m *workercore.ModelKernelArtifact) {
				m.Spec.Source.HuggingFace = &workercore.ModelArtifactHubSource{
					Repository: "team-a/qwen-kernels",
					Revision:   testKernelCommitSHA,
				}
			}),
			wantField: "spec.source",
		},
		{
			name: "tenant creates valid huggingFace commit-pinned artifact",
			ctx:  createAdmissionContext(testKernelTenantUsername),
			in:   newTestKernelArtifactHub(),
		},
		{
			name: "huggingFace accepts bare repository name with 40-hex commit",
			ctx:  createAdmissionContext(testKernelTenantUsername),
			in: newTestKernelArtifactHub(func(m *workercore.ModelKernelArtifact) {
				m.Spec.Source.HuggingFace.Repository = "qwen-kernels"
			}),
		},
		{
			name: "huggingFace rejects invalid repository",
			ctx:  createAdmissionContext(testKernelTenantUsername),
			in: newTestKernelArtifactHub(func(m *workercore.ModelKernelArtifact) {
				m.Spec.Source.HuggingFace.Repository = "a/b/c"
			}),
			wantField: "spec.source.huggingFace.repository",
		},
		{
			name: "huggingFace rejects empty revision",
			ctx:  createAdmissionContext(testKernelTenantUsername),
			in: newTestKernelArtifactHub(func(m *workercore.ModelKernelArtifact) {
				m.Spec.Source.HuggingFace.Revision = ""
			}),
			wantField: "spec.source.huggingFace.revision",
		},
		{
			name: "huggingFace rejects mutable branch revision",
			ctx:  createAdmissionContext(testKernelTenantUsername),
			in: newTestKernelArtifactHub(func(m *workercore.ModelKernelArtifact) {
				m.Spec.Source.HuggingFace.Revision = "main"
			}),
			wantField: "spec.source.huggingFace.revision",
		},
		{
			name: "huggingFace rejects tag revision",
			ctx:  createAdmissionContext(testKernelTenantUsername),
			in: newTestKernelArtifactHub(func(m *workercore.ModelKernelArtifact) {
				m.Spec.Source.HuggingFace.Revision = "v1.0.0"
			}),
			wantField: "spec.source.huggingFace.revision",
		},
		{
			name: "huggingFace rejects uppercase commit SHA",
			ctx:  createAdmissionContext(testKernelTenantUsername),
			in: newTestKernelArtifactHub(func(m *workercore.ModelKernelArtifact) {
				m.Spec.Source.HuggingFace.Revision = strings.ToUpper(testKernelCommitSHA)
			}),
			wantField: "spec.source.huggingFace.revision",
		},
		{
			name:    "tenant creates valid digest-pinned image artifact on k8s 1.35",
			ctx:     createAdmissionContext(testKernelTenantUsername),
			version: supportedVersion,
			in:      newTestKernelArtifactImage(),
		},
		{
			name:      "image source rejected when k8s version lacks ImageVolume",
			ctx:       createAdmissionContext(testKernelTenantUsername),
			version:   unsupportedVersion,
			in:        newTestKernelArtifactImage(),
			wantField: "spec.source.image",
		},
		{
			name:    "image source rejects tag instead of digest",
			ctx:     createAdmissionContext(testKernelTenantUsername),
			version: supportedVersion,
			in: newTestKernelArtifactImage(func(m *workercore.ModelKernelArtifact) {
				m.Spec.Source.Image.Reference = "registry.example.com/team/kernels:v1"
			}),
			wantField: "spec.source.image.reference",
		},
		{
			name: "compatibility accepts SGLang AMD gfx942",
			ctx:  createAdmissionContext(testKernelTenantUsername),
			in: newTestKernelArtifactHub(func(m *workercore.ModelKernelArtifact) {
				m.Spec.Compatibility = workercore.ModelKernelArtifactCompatibility{
					Engine:        workercore.ModelDeploymentEngineSGLang,
					EngineVersion: "0.4",
					Manufacturer:  "amd",
					Architecture:  "gfx942",
					KernelKeys:    []string{testKernelValidKeySGLang},
				}
			}),
		},
		{
			name: "compatibility accepts Ascend 910b",
			ctx:  createAdmissionContext(testKernelTenantUsername),
			in: newTestKernelArtifactHub(func(m *workercore.ModelKernelArtifact) {
				m.Spec.Compatibility = workercore.ModelKernelArtifactCompatibility{
					Engine:        workercore.ModelDeploymentEngineVLLM,
					EngineVersion: "0.8",
					Manufacturer:  "ascend",
					Architecture:  "910b",
					KernelKeys:    []string{testKernelValidKeyAscend},
				}
			}),
		},
		{
			name: "compatibility rejects empty engine",
			ctx:  createAdmissionContext(testKernelTenantUsername),
			in: newTestKernelArtifactHub(func(m *workercore.ModelKernelArtifact) {
				m.Spec.Compatibility.Engine = ""
			}),
			wantField: "spec.compatibility.engine",
		},
		{
			name: "compatibility rejects unsupported engine",
			ctx:  createAdmissionContext(testKernelTenantUsername),
			in: newTestKernelArtifactHub(func(m *workercore.ModelKernelArtifact) {
				m.Spec.Compatibility.Engine = "TensorRT-LLM"
			}),
			wantField: "spec.compatibility.engine",
		},
		{
			name: "compatibility rejects unnormalized patch engineVersion",
			ctx:  createAdmissionContext(testKernelTenantUsername),
			in: newTestKernelArtifactHub(func(m *workercore.ModelKernelArtifact) {
				m.Spec.Compatibility.EngineVersion = "0.8.5"
			}),
			wantField: "spec.compatibility.engineVersion",
		},
		{
			name: "compatibility rejects unnormalized engineVersion with build suffix",
			ctx:  createAdmissionContext(testKernelTenantUsername),
			in: newTestKernelArtifactHub(func(m *workercore.ModelKernelArtifact) {
				m.Spec.Compatibility.EngineVersion = "0.8.5+cu128"
			}),
			wantField: "spec.compatibility.engineVersion",
		},
		{
			name: "compatibility rejects empty engineVersion",
			ctx:  createAdmissionContext(testKernelTenantUsername),
			in: newTestKernelArtifactHub(func(m *workercore.ModelKernelArtifact) {
				m.Spec.Compatibility.EngineVersion = ""
			}),
			wantField: "spec.compatibility.engineVersion",
		},
		{
			name: "compatibility rejects invalid engineVersion",
			ctx:  createAdmissionContext(testKernelTenantUsername),
			in: newTestKernelArtifactHub(func(m *workercore.ModelKernelArtifact) {
				m.Spec.Compatibility.EngineVersion = "not-a-version"
			}),
			wantField: "spec.compatibility.engineVersion",
		},
		{
			name: "compatibility rejects uppercase manufacturer",
			ctx:  createAdmissionContext(testKernelTenantUsername),
			in: newTestKernelArtifactHub(func(m *workercore.ModelKernelArtifact) {
				m.Spec.Compatibility.Manufacturer = "NVIDIA"
			}),
			wantField: "spec.compatibility.manufacturer",
		},
		{
			name: "compatibility rejects unknown manufacturer",
			ctx:  createAdmissionContext(testKernelTenantUsername),
			in: newTestKernelArtifactHub(func(m *workercore.ModelKernelArtifact) {
				m.Spec.Compatibility.Manufacturer = "bogusgpu"
			}),
			wantField: "spec.compatibility.manufacturer",
		},
		{
			name: "compatibility rejects unnormalized architecture",
			ctx:  createAdmissionContext(testKernelTenantUsername),
			in: newTestKernelArtifactHub(func(m *workercore.ModelKernelArtifact) {
				m.Spec.Compatibility.Architecture = "9.0a"
			}),
			wantField: "spec.compatibility.architecture",
		},
		{
			name: "compatibility rejects empty architecture",
			ctx:  createAdmissionContext(testKernelTenantUsername),
			in: newTestKernelArtifactHub(func(m *workercore.ModelKernelArtifact) {
				m.Spec.Compatibility.Architecture = ""
			}),
			wantField: "spec.compatibility.architecture",
		},
		{
			name: "compatibility rejects empty kernelKeys",
			ctx:  createAdmissionContext(testKernelTenantUsername),
			in: newTestKernelArtifactHub(func(m *workercore.ModelKernelArtifact) {
				m.Spec.Compatibility.KernelKeys = nil
			}),
			wantField: "spec.compatibility.kernelKeys",
		},
		{
			name: "compatibility rejects more than 128 kernelKeys",
			ctx:  createAdmissionContext(testKernelTenantUsername),
			in: newTestKernelArtifactHub(func(m *workercore.ModelKernelArtifact) {
				keys := make([]string, 129)
				for i := range keys {
					keys[i] = fmt.Sprintf("vllm/0.8/nvidia/sm_90a/rmsnorm/%016x", i)
				}
				m.Spec.Compatibility.KernelKeys = keys
			}),
			wantField: "spec.compatibility.kernelKeys",
		},
		{
			name: "compatibility rejects duplicate kernelKeys",
			ctx:  createAdmissionContext(testKernelTenantUsername),
			in: newTestKernelArtifactHub(func(m *workercore.ModelKernelArtifact) {
				m.Spec.Compatibility.KernelKeys = []string{testKernelValidKeyVLLM, testKernelValidKeyVLLM}
			}),
			wantField: "spec.compatibility.kernelKeys[1]",
		},
		{
			name: "compatibility rejects dense GEMM kernelKey",
			ctx:  createAdmissionContext(testKernelTenantUsername),
			in: newTestKernelArtifactHub(func(m *workercore.ModelKernelArtifact) {
				m.Spec.Compatibility.KernelKeys = []string{"vllm/0.8/nvidia/sm_90a/gemm/0123456789abcdef"}
			}),
			wantField:   "spec.compatibility.kernelKeys[0]",
			wantMessage: "excluded",
		},
		{
			name: "compatibility rejects block-scaled GEMM kernelKey",
			ctx:  createAdmissionContext(testKernelTenantUsername),
			in: newTestKernelArtifactHub(func(m *workercore.ModelKernelArtifact) {
				m.Spec.Compatibility.KernelKeys = []string{"vllm/0.8/nvidia/sm_90a/nvfp4_gemm/0123456789abcdef"}
			}),
			wantField:   "spec.compatibility.kernelKeys[0]",
			wantMessage: "excluded",
		},
		{
			name: "compatibility rejects collective kernelKey",
			ctx:  createAdmissionContext(testKernelTenantUsername),
			in: newTestKernelArtifactHub(func(m *workercore.ModelKernelArtifact) {
				m.Spec.Compatibility.KernelKeys = []string{"vllm/0.8/nvidia/sm_90a/all_reduce/0123456789abcdef"}
			}),
			wantField:   "spec.compatibility.kernelKeys[0]",
			wantMessage: "excluded",
		},
		{
			name: "compatibility rejects key mismatched with engine",
			ctx:  createAdmissionContext(testKernelTenantUsername),
			in: newTestKernelArtifactHub(func(m *workercore.ModelKernelArtifact) {
				m.Spec.Compatibility.KernelKeys = []string{"sglang/0.8/nvidia/sm_90a/rmsnorm/0123456789abcdef"}
			}),
			wantField:   "spec.compatibility.kernelKeys[0]",
			wantMessage: "engine mismatch",
		},
		{
			name: "compatibility rejects key mismatched with engineVersion",
			ctx:  createAdmissionContext(testKernelTenantUsername),
			in: newTestKernelArtifactHub(func(m *workercore.ModelKernelArtifact) {
				m.Spec.Compatibility.KernelKeys = []string{"vllm/0.9/nvidia/sm_90a/rmsnorm/0123456789abcdef"}
			}),
			wantField:   "spec.compatibility.kernelKeys[0]",
			wantMessage: "engineVersion mismatch",
		},
		{
			name: "compatibility rejects key mismatched with manufacturer",
			ctx:  createAdmissionContext(testKernelTenantUsername),
			in: newTestKernelArtifactHub(func(m *workercore.ModelKernelArtifact) {
				m.Spec.Compatibility.KernelKeys = []string{"vllm/0.8/amd/gfx942/rmsnorm/0123456789abcdef"}
			}),
			wantField:   "spec.compatibility.kernelKeys[0]",
			wantMessage: "manufacturer mismatch",
		},
		{
			name: "compatibility rejects key mismatched with architecture",
			ctx:  createAdmissionContext(testKernelTenantUsername),
			in: newTestKernelArtifactHub(func(m *workercore.ModelKernelArtifact) {
				m.Spec.Compatibility.KernelKeys = []string{"vllm/0.8/nvidia/sm_80/rmsnorm/0123456789abcdef"}
			}),
			wantField:   "spec.compatibility.kernelKeys[0]",
			wantMessage: "arch mismatch",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if c.version != nil {
				modelArtifactClusterVersion = c.version
			} else {
				modelArtifactClusterVersion = supportedVersion
			}

			wh := &ModelKernelArtifactWebhook{worker: c.worker}
			_, err := wh.ValidateCreate(c.ctx, c.in)
			if c.wantField == "" {
				require.NoError(t, err)
				return
			}
			require.Error(t, err)
			assertInvalidField(t, err, c.wantField)
			if c.wantMessage != "" {
				assert.Contains(t, err.Error(), c.wantMessage)
			}
		})
	}
}

func TestModelKernelArtifactWebhookValidateUpdate(t *testing.T) {
	wh := &ModelKernelArtifactWebhook{worker: testWorkerUsername}

	t.Run("spec mutation is rejected", func(t *testing.T) {
		old := newTestKernelArtifactHub()
		updated := old.DeepCopy()
		updated.Spec.Compatibility.KernelKeys = append(updated.Spec.Compatibility.KernelKeys, testKernelValidKeyVLLM2)

		_, err := wh.ValidateUpdate(admissionContext(testKernelTenantUsername, ""), old, updated)
		require.Error(t, err)
		assertInvalidField(t, err, "spec")
	})

	t.Run("metadata and finalizer update is allowed", func(t *testing.T) {
		old := newTestKernelArtifactNodeStore()
		updated := old.DeepCopy()
		updated.Labels = map[string]string{"generation": "1"}
		updated.Finalizers = []string{workercore.ModelKernelArtifactProtectionFinalizer}

		_, err := wh.ValidateUpdate(admissionContext(testKernelTenantUsername, ""), old, updated)
		require.NoError(t, err)
	})

	t.Run("delete is allowed", func(t *testing.T) {
		_, err := wh.ValidateDelete(context.Background(), newTestKernelArtifactHub())
		require.NoError(t, err)
	})
}

func TestModelKernelArtifactStatusGuard(t *testing.T) {
	cases := []struct {
		name        string
		worker      string
		ctx         context.Context
		terminating bool
		wantRule    string
		wantIntern  bool
	}{
		{
			name:   "worker writes status",
			worker: testWorkerUsername,
			ctx:    admissionContext(testWorkerUsername, "status"),
		},
		{
			name:     "tenant writes status",
			worker:   testWorkerUsername,
			ctx:      admissionContext(testKernelTenantUsername, "status"),
			wantRule: modelKernelArtifactStatusRuleWriter,
		},
		{
			name:     "admin writes status",
			worker:   testWorkerUsername,
			ctx:      admissionContext("kubernetes-admin", "status"),
			wantRule: modelKernelArtifactStatusRuleWriter,
		},
		{
			name:        "tenant writes status of Terminating artifact",
			worker:      testWorkerUsername,
			ctx:         admissionContext(testKernelTenantUsername, "status"),
			terminating: true,
			wantRule:    modelKernelArtifactStatusRuleWriter,
		},
		{
			name:     "no learned worker identity refuses status writes",
			worker:   "",
			ctx:      admissionContext(testWorkerUsername, "status"),
			wantRule: modelKernelArtifactStatusRuleWriter,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			old := newTestKernelArtifactNodeStore()
			if c.terminating {
				now := meta.Now()
				old.DeletionTimestamp = &now
				old.Finalizers = []string{workercore.ModelKernelArtifactProtectionFinalizer}
			}
			updated := old.DeepCopy()
			updated.Status.Quarantined = false

			_, err := (&ModelKernelArtifactWebhook{worker: c.worker}).ValidateUpdate(c.ctx, old, updated)
			if c.wantRule == "" {
				require.NoError(t, err)
				return
			}
			require.Error(t, err)
			assert.Contains(t, err.Error(), c.wantRule)
		})
	}

	t.Run("missing admission request returns internal error", func(t *testing.T) {
		wh := &ModelKernelArtifactWebhook{worker: testWorkerUsername}
		err := wh.validateModelKernelArtifactStatus(context.Background(), newTestKernelArtifactNodeStore())
		require.Error(t, err)
		assert.True(t, kerrors.IsInternalError(err))
	})

	t.Run("implements ReceiveDeletionUpdate", func(t *testing.T) {
		_, ok := any(new(ModelKernelArtifactWebhook)).(webhook.ReceiveDeletionUpdate)
		assert.True(t, ok)
	})
}
