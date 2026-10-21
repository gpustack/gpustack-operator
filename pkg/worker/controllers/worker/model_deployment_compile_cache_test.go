package worker

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	core "k8s.io/api/core/v1"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	worker "gpustack.ai/gpustack/api/worker/v1"
	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	"gpustack.ai/gpustack/pkg/nodefeature"
	"gpustack.ai/gpustack/pkg/utils/stringx"
)

const (
	// testCompileCacheDigest is a well-formed manifest digest: 64 hexadecimal digits, which is
	// what makes it a content address rather than a name.
	testCompileCacheDigest = "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	// testCompileCacheImageDigest is the same address, spelled as the OCI reference of an image
	// artifact carries it.
	testCompileCacheImageDigest = "registry.example.com/team/qwen@" + testCompileCacheDigest
)

// newCompileCacheDeployment builds the deployment every case then varies: one vLLM role on an
// NVIDIA pool, pulling its weights itself, which is the shape with the fewest moving parts.
func newCompileCacheDeployment(mutate ...func(*workercore.ModelDeployment)) *workercore.ModelDeployment {
	md := &workercore.ModelDeployment{
		ObjectMeta: meta.ObjectMeta{Name: "qwen", Namespace: "team-a"},
		Spec: workercore.ModelDeploymentSpec{
			Model:  workercore.ModelDeploymentModel{Name: "Qwen/Qwen2.5-72B-Instruct"},
			Engine: workercore.ModelDeploymentEngine{Name: workercore.ModelDeploymentEngineVLLM, Image: "vllm/vllm-openai:v0.25.1"},
			Roles: []workercore.ModelDeploymentRole{{
				Name:  "server",
				Image: "vllm/vllm-openai:v0.25.1",
			}},
		},
	}
	for _, m := range mutate {
		m(md)
	}

	return md
}

// newCompileCacheInstanceType builds the acceleratable NVIDIA type most cases read. Nothing here
// feeds a size calculation, so only the two facts the cache path reads are populated.
func newCompileCacheInstanceType(mutate ...func(*worker.InstanceType)) *worker.InstanceType {
	it := &worker.InstanceType{
		Spec: workercore.InstanceTypeSpec{Acceleratable: true},
		Status: workercore.InstanceTypeStatus{
			Detail: workercore.InstanceTypeDetail{Manufacturer: nodefeature.ManufacturerNVIDIA},
		},
	}
	for _, m := range mutate {
		m(it)
	}

	return it
}

// newCompileCacheInput assembles the inputs of the baseline every case starts from.
func newCompileCacheInput(mutate ...func(*ModelDeploymentCompileCacheInput)) ModelDeploymentCompileCacheInput {
	in := ModelDeploymentCompileCacheInput{
		Deployment:   newCompileCacheDeployment(),
		Role:         nil,
		InstanceType: newCompileCacheInstanceType(),
	}
	in.Role = &in.Deployment.Spec.Roles[0]
	for _, m := range mutate {
		m(&in)
	}

	return in
}

// testCompileCacheSHA256Hex states the identity formula of the cache path in the test itself, so
// a case asserts the documented rule rather than agreeing with whatever the code happens to do.
func testCompileCacheSHA256Hex(s string) string {
	return stringx.SumBySHA256(s)[:32]
}

// testCompileCacheIdentity builds the "m-" segment the path carries for an address, trimming it
// to the digits a directory keeps.
func testCompileCacheIdentity(hexDigits string) string {
	if len(hexDigits) > modelDeploymentCompileCacheIdentityDigits {
		hexDigits = hexDigits[:modelDeploymentCompileCacheIdentityDigits]
	}

	return "m-" + hexDigits
}

// testCompileCacheSplit breaks a subpath into the segments every case names: the vendor and
// engine, the weights, the parallel shape, and the compilation digest.
func testCompileCacheSplit(t *testing.T, subPath string) (root, identity, shape, digest string) {
	t.Helper()
	segments := strings.Split(subPath, "/")
	require.Len(t, segments, 3, "the cache path has three segments, got %q", subPath)
	// The shape carries dashes of its own, so the digest is what follows the last one.
	at := strings.LastIndex(segments[2], "-")
	require.Positive(t, at, "the leaf segment ends in a compilation digest, got %q", segments[2])
	digest = segments[2][at+1:]
	require.Len(t, digest, modelDeploymentCompileCacheHashDigits, "the digest keeps eight digits")

	return segments[0], segments[1], segments[2][:at], digest
}

// TestModelDeploymentCompileCacheSubPathManufacturerAndEngine pins the first segment: which vendor
// and which engine a directory belongs to, and the two inputs that cannot produce one at all.
func TestModelDeploymentCompileCacheSubPathManufacturerAndEngine(t *testing.T) {
	cases := []struct {
		name          string
		engine        string
		acceleratable bool
		manufacturer  string
		wantRoot      string
		wantErr       string
	}{
		{
			name:          "an NVIDIA vLLM role",
			engine:        workercore.ModelDeploymentEngineVLLM,
			acceleratable: true,
			manufacturer:  nodefeature.ManufacturerNVIDIA,
			wantRoot:      "nvidia-vllm",
		},
		{
			name:          "an Ascend vLLM role compiles into its own tree",
			engine:        workercore.ModelDeploymentEngineVLLM,
			acceleratable: true,
			manufacturer:  nodefeature.ManufacturerAscend,
			wantRoot:      "ascend-vllm",
		},
		{
			name:          "an AMD role is named by its vendor",
			engine:        workercore.ModelDeploymentEngineVLLM,
			acceleratable: true,
			manufacturer:  nodefeature.ManufacturerAMD,
			wantRoot:      "amd-vllm",
		},
		{
			name:          "a pool no vendor claims is a CPU pool",
			engine:        workercore.ModelDeploymentEngineVLLM,
			acceleratable: false,
			manufacturer:  "",
			wantRoot:      "cpu-vllm",
		},
		{
			name:          "SGLang compiles differently and is named separately",
			engine:        workercore.ModelDeploymentEngineSGLang,
			acceleratable: true,
			manufacturer:  nodefeature.ManufacturerNVIDIA,
			wantRoot:      "nvidia-sglang",
		},
		{
			name:          "the manufacturer is read in lower case whatever status spells",
			engine:        workercore.ModelDeploymentEngineVLLM,
			acceleratable: true,
			manufacturer:  "  NVIDIA  ",
			wantRoot:      "nvidia-vllm",
		},
		{
			name:          "an acceleratable type with no manufacturer is refused",
			engine:        workercore.ModelDeploymentEngineVLLM,
			acceleratable: true,
			manufacturer:  "   ",
			wantErr:       "no manufacturer in status",
		},
		{
			name:          "an engine this operator does not serve has no cache",
			engine:        "Triton",
			acceleratable: true,
			manufacturer:  nodefeature.ManufacturerNVIDIA,
			wantErr:       `unsupported engine "Triton"`,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			subPath, err := BuildModelDeploymentCompileCacheSubPath(newCompileCacheInput(
				func(in *ModelDeploymentCompileCacheInput) {
					in.Deployment.Spec.Engine.Name = c.engine
					in.InstanceType.Spec.Acceleratable = c.acceleratable
					in.InstanceType.Status.Detail.Manufacturer = c.manufacturer
				}))
			if c.wantErr != "" {
				require.ErrorContains(t, err, c.wantErr)

				return
			}

			require.NoError(t, err)
			root, _, _, _ := testCompileCacheSplit(t, subPath)
			assert.Equal(t, c.wantRoot, root)
		})
	}
}

// TestModelDeploymentCompileCacheSubPathMissingInput covers the three inputs whose absence would
// otherwise render a path from an empty string rather than say so.
func TestModelDeploymentCompileCacheSubPathMissingInput(t *testing.T) {
	cases := []struct {
		name    string
		drop    func(*ModelDeploymentCompileCacheInput)
		wantErr string
	}{
		{
			name:    "without the deployment there is no engine to name",
			drop:    func(in *ModelDeploymentCompileCacheInput) { in.Deployment = nil },
			wantErr: "needs the deployment",
		},
		{
			name:    "without the role there are no arguments to read",
			drop:    func(in *ModelDeploymentCompileCacheInput) { in.Role = nil },
			wantErr: "needs the role",
		},
		{
			name:    "without the instance type there is no manufacturer",
			drop:    func(in *ModelDeploymentCompileCacheInput) { in.InstanceType = nil },
			wantErr: "needs the instance type",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			in := newCompileCacheInput()
			c.drop(&in)

			_, err := BuildModelDeploymentCompileCacheSubPath(in)
			require.ErrorContains(t, err, c.wantErr)
		})
	}
}

// TestModelDeploymentCompileCacheSubPathModelIdentity pins the weight segment across every source
// the operator delivers from: a hub artifact the reconciler resolved, one still unresolved, an
// image, a claim, the render's own view of each, and a deployment that pulls its weights itself.
func TestModelDeploymentCompileCacheSubPathModelIdentity(t *testing.T) {
	const (
		hubDigestHex = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
		imageUID     = "uid-qwen-image"
	)

	cases := []struct {
		name         string
		mutate       func(*ModelDeploymentCompileCacheInput)
		wantIdentity string
	}{
		{
			name: "a hub artifact is addressed by its manifest digest",
			mutate: func(in *ModelDeploymentCompileCacheInput) {
				in.ModelArtifact = &workercore.ModelArtifact{
					Spec:   workercore.ModelArtifactSpec{Source: workercore.ModelArtifactSource{HuggingFace: &workercore.ModelArtifactHubSource{Repository: "Qwen/Qwen2.5-72B"}}},
					Status: workercore.ModelArtifactStatus{Resolved: &workercore.ModelArtifactResolved{ManifestDigest: testCompileCacheDigest}},
				}
			},
			wantIdentity: testCompileCacheIdentity(hubDigestHex),
		},
		{
			name: "a ModelScope artifact is addressed the same way",
			mutate: func(in *ModelDeploymentCompileCacheInput) {
				in.ModelArtifact = &workercore.ModelArtifact{
					Spec:   workercore.ModelArtifactSpec{Source: workercore.ModelArtifactSource{ModelScope: &workercore.ModelArtifactHubSource{Repository: "Qwen/Qwen2.5-72B"}}},
					Status: workercore.ModelArtifactStatus{Resolved: &workercore.ModelArtifactResolved{ManifestDigest: testCompileCacheDigest}},
				}
			},
			wantIdentity: testCompileCacheIdentity(hubDigestHex),
		},
		{
			name: "a digest spelled in upper case names the same directory",
			mutate: func(in *ModelDeploymentCompileCacheInput) {
				in.ModelArtifact = &workercore.ModelArtifact{
					Spec: workercore.ModelArtifactSpec{Source: workercore.ModelArtifactSource{
						HuggingFace: &workercore.ModelArtifactHubSource{Repository: "Qwen/Qwen2.5-72B"},
					}},
					Status: workercore.ModelArtifactStatus{Resolved: &workercore.ModelArtifactResolved{
						ManifestDigest: "sha256:" + strings.ToUpper(hubDigestHex),
					}},
				}
			},
			wantIdentity: testCompileCacheIdentity(hubDigestHex),
		},
		{
			name: "an unresolved hub artifact falls back to its UID",
			mutate: func(in *ModelDeploymentCompileCacheInput) {
				in.ModelArtifact = &workercore.ModelArtifact{
					ObjectMeta: newCompileCacheObjectMeta("qwen", "ma-uid"),
					Spec: workercore.ModelArtifactSpec{Source: workercore.ModelArtifactSource{
						HuggingFace: &workercore.ModelArtifactHubSource{Repository: "Qwen/Qwen2.5-72B"},
					}},
				}
			},
			wantIdentity: testCompileCacheIdentity(testCompileCacheSHA256Hex("ma-uid")),
		},
		{
			name: "an image artifact is addressed by the digest it pins",
			mutate: func(in *ModelDeploymentCompileCacheInput) {
				in.ModelArtifact = &workercore.ModelArtifact{
					Spec: workercore.ModelArtifactSpec{Source: workercore.ModelArtifactSource{
						Image: &workercore.ModelArtifactImageSource{Reference: testCompileCacheImageDigest},
					}},
				}
			},
			wantIdentity: testCompileCacheIdentity(hubDigestHex),
		},
		{
			name: "an image reference naming a tag is hashed whole, because a tag moves",
			mutate: func(in *ModelDeploymentCompileCacheInput) {
				in.ModelArtifact = &workercore.ModelArtifact{
					Spec: workercore.ModelArtifactSpec{Source: workercore.ModelArtifactSource{
						Image: &workercore.ModelArtifactImageSource{Reference: "registry.example.com/team/qwen:latest"},
					}},
				}
			},
			wantIdentity: testCompileCacheIdentity(testCompileCacheSHA256Hex("registry.example.com/team/qwen:latest")),
		},
		{
			name: "a claim artifact is the artifact, not what the claim holds",
			mutate: func(in *ModelDeploymentCompileCacheInput) {
				in.ModelArtifact = &workercore.ModelArtifact{
					ObjectMeta: newCompileCacheObjectMeta("qwen-claim", "claim-uid"),
					Spec: workercore.ModelArtifactSpec{Source: workercore.ModelArtifactSource{
						PersistentVolumeClaim: &workercore.ModelArtifactPersistentVolumeClaimSource{
							ClaimName: "models", Path: "qwen/72b",
						},
					}},
				}
			},
			wantIdentity: testCompileCacheIdentity(testCompileCacheSHA256Hex(
				"team-a/qwen-claim/claim-uid/models/qwen/72b")),
		},
		{
			name: "a node-delivered artifact is addressed by the digest it published",
			mutate: func(in *ModelDeploymentCompileCacheInput) {
				in.Artifact = &ModelDeploymentArtifactRender{
					Delivery:     workercore.ModelDeploymentModelDeliveryNode,
					ArtifactName: "qwen", ArtifactUID: "uid-qwen", ManifestDigest: testCompileCacheDigest,
				}
			},
			wantIdentity: testCompileCacheIdentity(hubDigestHex),
		},
		{
			name: "an engine download with no digest is addressed by the artifact's UID",
			mutate: func(in *ModelDeploymentCompileCacheInput) {
				in.Artifact = &ModelDeploymentArtifactRender{
					Delivery:     workercore.ModelDeploymentModelDeliveryEngine,
					ArtifactName: "qwen", ArtifactUID: "uid-qwen",
					Hub: modelDeploymentHubHuggingFace, Repository: "Qwen/Qwen2.5-72B", Revision: "abc",
				}
			},
			wantIdentity: testCompileCacheIdentity(testCompileCacheSHA256Hex("uid-qwen")),
		},
		{
			name: "an engine download naming only a revision is addressed by the repository",
			mutate: func(in *ModelDeploymentCompileCacheInput) {
				in.Artifact = &ModelDeploymentArtifactRender{
					Delivery:   workercore.ModelDeploymentModelDeliveryEngine,
					Hub:        modelDeploymentHubHuggingFace,
					Repository: "Qwen/Qwen2.5-72B", Revision: "0123456789abcdef",
				}
			},
			wantIdentity: testCompileCacheIdentity(testCompileCacheSHA256Hex(
				modelDeploymentHubHuggingFace + "/Qwen/Qwen2.5-72B@0123456789abcdef")),
		},
		{
			name: "an engine download naming nothing at all falls back to the model name",
			mutate: func(in *ModelDeploymentCompileCacheInput) {
				in.Artifact = &ModelDeploymentArtifactRender{Delivery: workercore.ModelDeploymentModelDeliveryEngine}
			},
			wantIdentity: testCompileCacheIdentity(testCompileCacheSHA256Hex("Qwen/Qwen2.5-72B-Instruct")),
		},
		{
			name: "an image delivery is addressed by the digest its reference pins",
			mutate: func(in *ModelDeploymentCompileCacheInput) {
				in.Artifact = &ModelDeploymentArtifactRender{
					Delivery: workercore.ModelDeploymentModelDeliveryImage, ImageReference: testCompileCacheImageDigest,
				}
			},
			wantIdentity: testCompileCacheIdentity(hubDigestHex),
		},
		{
			name: "a claim delivery is addressed by the claim it mounts",
			mutate: func(in *ModelDeploymentCompileCacheInput) {
				in.Deployment.Spec.Model.ArtifactRef = &core.LocalObjectReference{Name: "qwen-claim"}
				in.Artifact = &ModelDeploymentArtifactRender{
					Delivery:    workercore.ModelDeploymentModelDeliveryPVC,
					ArtifactUID: imageUID, ClaimName: "models", Path: "qwen/72b",
				}
			},
			wantIdentity: testCompileCacheIdentity(testCompileCacheSHA256Hex(
				"team-a/qwen-claim/" + imageUID + "/models/qwen/72b")),
		},
		{
			name:         "a deployment pulling its own weights is addressed by the model name",
			mutate:       func(in *ModelDeploymentCompileCacheInput) {},
			wantIdentity: testCompileCacheIdentity(testCompileCacheSHA256Hex("Qwen/Qwen2.5-72B-Instruct")),
		},
		{
			name: "the resolved artifact is read ahead of the render the reconciler derived from it",
			mutate: func(in *ModelDeploymentCompileCacheInput) {
				in.ModelArtifact = &workercore.ModelArtifact{
					Spec: workercore.ModelArtifactSpec{Source: workercore.ModelArtifactSource{
						Image: &workercore.ModelArtifactImageSource{Reference: testCompileCacheImageDigest},
					}},
				}
				in.Artifact = &ModelDeploymentArtifactRender{
					Delivery:       workercore.ModelDeploymentModelDeliveryImage,
					ImageReference: "registry.example.com/team/other@sha256:" + strings.Repeat("b", 64),
				}
			},
			wantIdentity: testCompileCacheIdentity(hubDigestHex),
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			subPath, err := BuildModelDeploymentCompileCacheSubPath(newCompileCacheInput(c.mutate))
			require.NoError(t, err)

			_, identity, _, _ := testCompileCacheSplit(t, subPath)
			assert.Equal(t, c.wantIdentity, identity)
		})
	}
}

// TestModelDeploymentCompileCacheSubPathUnnamableWeights covers the inputs where no identity can be
// derived, rather than a directory being shared by everything that reached the branch.
func TestModelDeploymentCompileCacheSubPathUnnamableWeights(t *testing.T) {
	cases := []struct {
		name    string
		mutate  func(*ModelDeploymentCompileCacheInput)
		wantErr string
	}{
		{
			name:    "a deployment naming no model has no weights to address",
			mutate:  func(in *ModelDeploymentCompileCacheInput) { in.Deployment.Spec.Model.Name = "" },
			wantErr: "names no model",
		},
		{
			name: "an artifact naming no source has no weights to address",
			mutate: func(in *ModelDeploymentCompileCacheInput) {
				in.ModelArtifact = &workercore.ModelArtifact{
					ObjectMeta: newCompileCacheObjectMeta("empty", "empty-uid"),
				}
			},
			wantErr: "names no source",
		},
		{
			name: "a delivery this operator does not render has no weights to address",
			mutate: func(in *ModelDeploymentCompileCacheInput) {
				in.Artifact = &ModelDeploymentArtifactRender{Delivery: "Rsync"}
			},
			wantErr: `unsupported model delivery "Rsync"`,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := BuildModelDeploymentCompileCacheSubPath(newCompileCacheInput(c.mutate))
			require.ErrorContains(t, err, c.wantErr)
		})
	}
}

// TestModelDeploymentCompileCacheSubPathParallelShape pins the segment naming the parallel shape the
// kernels were generated for, per engine, with an undeclared degree reading as the engine's own
// default of one.
func TestModelDeploymentCompileCacheSubPathParallelShape(t *testing.T) {
	cases := []struct {
		name      string
		engine    string
		args      []string
		wantShape string
	}{
		{
			name:      "an undeclared shape is the engine's default of one everywhere",
			engine:    workercore.ModelDeploymentEngineVLLM,
			wantShape: "tp1-pp1-dp1-ep1-cp1_1",
		},
		{
			name:      "vLLM reads its degrees from the single-dash spellings",
			engine:    workercore.ModelDeploymentEngineVLLM,
			args:      []string{"-tp", "8", "-pp", "2", "-dp", "4", "-pcp", "2", "-dcp", "4"},
			wantShape: "tp8-pp2-dp4-ep1-cp2_4",
		},
		{
			name:      "vLLM's expert parallelism is a mode, so it names no degree",
			engine:    workercore.ModelDeploymentEngineVLLM,
			args:      []string{"--tensor-parallel-size=8", "--enable-expert-parallel"},
			wantShape: "tp8-pp1-dp1-ep1-cp1_1",
		},
		{
			name:      "SGLang declares an expert-parallel degree of its own",
			engine:    workercore.ModelDeploymentEngineSGLang,
			args:      []string{"--tp-size", "8", "--pp-size", "2", "--dp-size", "4", "--ep-size", "8", "--attn-cp-size", "2", "--dcp-size", "4"},
			wantShape: "tp8-pp2-dp4-ep8-cp1_4",
		},
		{
			name:      "a degree below one is the default rather than a negative width",
			engine:    workercore.ModelDeploymentEngineVLLM,
			args:      []string{"--data-parallel-size-local", "0"},
			wantShape: "tp1-pp1-dp1-ep1-cp1_1",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			subPath, err := BuildModelDeploymentCompileCacheSubPath(newCompileCacheInput(
				func(in *ModelDeploymentCompileCacheInput) {
					in.Deployment.Spec.Engine.Name = c.engine
					in.Role.ExtraArgs = c.args
				}))
			require.NoError(t, err)

			_, _, shape, _ := testCompileCacheSplit(t, subPath)
			assert.Equal(t, c.wantShape, shape)
		})
	}
}

// TestModelDeploymentCompileCacheOperationalFlagsShareTheCache is the property that keeps the cache
// warm while a deployment is tuned: an argument that changes no compiled code must not name a new
// directory, or every capacity change would cost every replica a cold start.
func TestModelDeploymentCompileCacheOperationalFlagsShareTheCache(t *testing.T) {
	operational := [][]string{
		{"--max-running-requests", "512"},
		{"--gpu-memory-utilization", "0.92"},
		{"--mem-fraction-static", "0.85"},
		{"--served-model-name", "qwen"},
		{"--uvicorn-log-level", "debug"},
		{"--port", "8000"},
		{"--seed", "1234"},
		{"--enable-prefix-caching"},
		{"--block-size", "16"},
	}

	baseline, err := BuildModelDeploymentCompileCacheSubPath(newCompileCacheInput())
	require.NoError(t, err)

	for _, args := range operational {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			subPath, err := BuildModelDeploymentCompileCacheSubPath(newCompileCacheInput(
				func(in *ModelDeploymentCompileCacheInput) { in.Role.ExtraArgs = args }))
			require.NoError(t, err)
			assert.Equal(t, baseline, subPath,
				"an operational argument must not move the cache")
		})
	}
}

// TestModelDeploymentCompileCacheReorderedAndTrailingFlagsShareTheCache is the second half of that
// property: appending an unrelated argument, or restating one the parser resolves the same way,
// leaves the directory alone.
func TestModelDeploymentCompileCacheReorderedAndTrailingFlagsShareTheCache(t *testing.T) {
	baselineArgs := []string{"--dtype", "bfloat16"}
	baseline, err := BuildModelDeploymentCompileCacheSubPath(newCompileCacheInput(
		func(in *ModelDeploymentCompileCacheInput) { in.Role.ExtraArgs = baselineArgs }))
	require.NoError(t, err)

	cases := []struct {
		name string
		args []string
	}{
		{
			name: "an operational flag appended after a compilation one",
			args: []string{"--dtype", "bfloat16", "--max-running-requests", "512"},
		},
		{
			name: "the same two flags in the other order",
			args: []string{"--max-running-requests", "512", "--dtype", "bfloat16"},
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			subPath, err := BuildModelDeploymentCompileCacheSubPath(newCompileCacheInput(
				func(in *ModelDeploymentCompileCacheInput) { in.Role.ExtraArgs = c.args }))
			require.NoError(t, err)
			assert.Equal(t, baseline, subPath)
		})
	}
}

// TestModelDeploymentCompileCacheCompilationFactorsIsolateTheCache is the opposite property, and
// the reason the digest exists: every input the compiler can see must land on its own directory,
// or one role would read another's compiled graph.
func TestModelDeploymentCompileCacheCompilationFactorsIsolateTheCache(t *testing.T) {
	baseline, err := BuildModelDeploymentCompileCacheSubPath(newCompileCacheInput(
		func(in *ModelDeploymentCompileCacheInput) {
			in.Role.ExtraArgs = []string{"--dtype", "bfloat16", "--max-model-len", "4096"}
		}))
	require.NoError(t, err)

	cases := []struct {
		name   string
		mutate func(*ModelDeploymentCompileCacheInput)
	}{
		{
			name: "the dtype the kernels are generated for",
			mutate: func(in *ModelDeploymentCompileCacheInput) {
				in.Role.ExtraArgs = []string{"--dtype", "float16", "--max-model-len", "4096"}
			},
		},
		{
			name: "the context length that fixes graph shapes",
			mutate: func(in *ModelDeploymentCompileCacheInput) {
				in.Role.ExtraArgs = []string{"--dtype", "bfloat16", "--max-model-len", "8192"}
			},
		},
		{
			name: "a second engine spelling of the context length",
			mutate: func(in *ModelDeploymentCompileCacheInput) {
				in.Role.ExtraArgs = []string{"--dtype", "bfloat16", "--context-length", "8192"}
			},
		},
		{
			name: "the compilation configuration as a whole document",
			mutate: func(in *ModelDeploymentCompileCacheInput) {
				in.Role.ExtraArgs = []string{
					"--dtype", "bfloat16", "--max-model-len", "4096",
					"--compilation-config", `{"level":3}`,
				}
			},
		},
		{
			name: "a dotted override of one compilation field",
			mutate: func(in *ModelDeploymentCompileCacheInput) {
				in.Role.ExtraArgs = []string{
					"--dtype", "bfloat16", "--max-model-len", "4096",
					"--compilation-config.level", "3",
				}
			},
		},
		{
			name: "the optimization level",
			mutate: func(in *ModelDeploymentCompileCacheInput) {
				in.Role.ExtraArgs = []string{"--dtype", "bfloat16", "--max-model-len", "4096", "-O3"}
			},
		},
		{
			name: "eager mode skips the graphs entirely",
			mutate: func(in *ModelDeploymentCompileCacheInput) {
				in.Role.ExtraArgs = []string{
					"--dtype", "bfloat16", "--max-model-len", "4096",
					"--enforce-eager",
				}
			},
		},
		{
			name: "the torch compile switch",
			mutate: func(in *ModelDeploymentCompileCacheInput) {
				in.Role.ExtraArgs = []string{
					"--dtype", "bfloat16", "--max-model-len", "4096",
					"--enable-torch-compile",
				}
			},
		},
		{
			name: "the batch sizes a graph is captured for",
			mutate: func(in *ModelDeploymentCompileCacheInput) {
				in.Role.ExtraArgs = []string{
					"--dtype", "bfloat16", "--max-model-len", "4096",
					"--cuda-graph-max-bs", "64",
				}
			},
		},
		{
			name: "the captured batch sizes themselves",
			mutate: func(in *ModelDeploymentCompileCacheInput) {
				in.Role.ExtraArgs = []string{
					"--dtype", "bfloat16", "--max-model-len", "4096",
					"--cudagraph-capture-sizes", "1", "2", "4",
				}
			},
		},
		{
			name: "the quantization the kernels are specialized for",
			mutate: func(in *ModelDeploymentCompileCacheInput) {
				in.Role.ExtraArgs = []string{
					"--dtype", "bfloat16", "--max-model-len", "4096",
					"--quantization", "fp8",
				}
			},
		},
		{
			name: "the speculative decoding configuration",
			mutate: func(in *ModelDeploymentCompileCacheInput) {
				in.Role.ExtraArgs = []string{
					"--dtype", "bfloat16", "--max-model-len", "4096",
					"--speculative-config", `{"method":"eagle"}`,
				}
			},
		},
		{
			name: "the attention backend that owns its kernels",
			mutate: func(in *ModelDeploymentCompileCacheInput) {
				in.Role.ExtraArgs = []string{
					"--dtype", "bfloat16", "--max-model-len", "4096",
					"--attention-backend", "FLASH_ATTN",
				}
			},
		},
		{
			name: "the engine image the compiler itself comes from",
			mutate: func(in *ModelDeploymentCompileCacheInput) {
				in.Role.ExtraArgs = []string{"--dtype", "bfloat16", "--max-model-len", "4096"}
				in.Role.Image = "vllm/vllm-openai:v0.26.0"
			},
		},
		{
			name: "a role falling back to the deployment's image",
			mutate: func(in *ModelDeploymentCompileCacheInput) {
				in.Role.ExtraArgs = []string{"--dtype", "bfloat16", "--max-model-len", "4096"}
				in.Role.Image = ""
				in.Deployment.Spec.Engine.Image = "vllm/vllm-openai:v0.27.0"
			},
		},
		{
			name: "the tensor-parallel width the kernels are generated for",
			mutate: func(in *ModelDeploymentCompileCacheInput) {
				in.Role.ExtraArgs = []string{
					"--dtype", "bfloat16", "--max-model-len", "4096",
					"--tensor-parallel-size", "8",
				}
			},
		},
		{
			name: "the local data-parallel width",
			mutate: func(in *ModelDeploymentCompileCacheInput) {
				in.Role.ExtraArgs = []string{
					"--dtype", "bfloat16", "--max-model-len", "4096",
					"--data-parallel-size", "4", "--data-parallel-size-local", "2",
				}
			},
		},
		{
			name: "the attention backend chosen through the environment",
			mutate: func(in *ModelDeploymentCompileCacheInput) {
				in.Role.ExtraArgs = []string{"--dtype", "bfloat16", "--max-model-len", "4096"}
				in.Role.Env = []workercore.ModelDeploymentEnvVar{{Name: "VLLM_ATTENTION_BACKEND", Value: "FLASH_ATTN"}}
			},
		},
		{
			name: "the architecture list the kernels are built for",
			mutate: func(in *ModelDeploymentCompileCacheInput) {
				in.Role.ExtraArgs = []string{"--dtype", "bfloat16", "--max-model-len", "4096"}
				in.Role.Env = []workercore.ModelDeploymentEnvVar{{Name: "TORCH_CUDA_ARCH_LIST", Value: "9.0"}}
			},
		},
		{
			name: "a role that replaced its command line",
			mutate: func(in *ModelDeploymentCompileCacheInput) {
				in.Role.Command = []string{"serve", "--dtype", "float16"}
			},
		},
	}

	seen := map[string]string{baseline: "the baseline"}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			subPath, err := BuildModelDeploymentCompileCacheSubPath(newCompileCacheInput(c.mutate))
			require.NoError(t, err)
			assert.NotEqual(t, baseline, subPath,
				"a compilation-affecting change must isolate the cache")
			if other, seen := seen[subPath]; seen {
				t.Errorf("this path collides with %q, which is a different configuration", other)
			}
			seen[subPath] = c.name
		})
	}
}

// TestModelDeploymentCompileCacheEquivalentSpellingsShareTheCache pins the normalization: the
// engine accepts one flag under more than one spelling, and those spellings describe one
// configuration, so they must describe one directory.
func TestModelDeploymentCompileCacheEquivalentSpellingsShareTheCache(t *testing.T) {
	cases := []struct {
		name  string
		one   []string
		other []string
	}{
		{
			name:  "the underscore spelling vLLM rewrites is the dash spelling",
			one:   []string{"--max_model_len", "4096"},
			other: []string{"--max-model-len", "4096"},
		},
		{
			name:  "the equals form is the separated form",
			one:   []string{"--max_model_len=4096"},
			other: []string{"--max-model-len", "4096"},
		},
		{
			name:  "the short quantization flag is the long one",
			one:   []string{"-q", "fp8"},
			other: []string{"--quantization", "fp8"},
		},
		{
			name:  "a short flag with its value glued on",
			one:   []string{"-qfp8"},
			other: []string{"-q", "fp8"},
		},
		{
			name:  "a short flag with its value behind an equals sign",
			one:   []string{"-q=fp8"},
			other: []string{"-q", "fp8"},
		},
		{
			name:  "an optimization level with its value glued on",
			one:   []string{"-O3"},
			other: []string{"-O", "3"},
		},
		{
			name:  "an optimization level behind an equals sign",
			one:   []string{"-O=3"},
			other: []string{"-O", "3"},
		},
		{
			name:  "an abbreviation argparse resolves to the flag it stands for",
			one:   []string{"--dt", "bfloat16"},
			other: []string{"--dtype", "bfloat16"},
		},
		{
			name:  "an abbreviation resolving to the flag a dotted override belongs to",
			one:   []string{"--compilation", `{"level":3}`},
			other: []string{"--compilation-config", `{"level":3}`},
		},
		{
			name:  "a dotted override written with underscores",
			one:   []string{"--compilation_config.level", "3"},
			other: []string{"--compilation-config.level", "3"},
		},
		{
			name:  "a dotted override written inline",
			one:   []string{"--attention-config.backend=FLASH_ATTN"},
			other: []string{"--attention-config.backend", "FLASH_ATTN"},
		},
		{
			name:  "a multi-value flag reads every token after it",
			one:   []string{"--cudagraph-capture-sizes", "1", "2", "4"},
			other: []string{"--cudagraph-capture-sizes=1,2,4"},
		},
		{
			name:  "a multi-value flag stops at the next flag",
			one:   []string{"--cudagraph-capture-sizes", "1", "2", "--dtype", "bfloat16"},
			other: []string{"--cudagraph-capture-sizes=1,2", "--dtype", "bfloat16"},
		},
		{
			name:  "a switch leaves the next argument to itself",
			one:   []string{"--enforce-eager", "--dtype", "bfloat16"},
			other: []string{"--dtype", "bfloat16", "--enforce-eager"},
		},
		{
			name:  "the options end where the engine says they do",
			one:   []string{"--dtype", "bfloat16", "--", "--dtype", "float16"},
			other: []string{"--dtype", "bfloat16"},
		},
		{
			name:  "the last value of a repeated flag is the one the engine runs",
			one:   []string{"--dtype", "bfloat16", "--dtype", "float16"},
			other: []string{"--dtype", "float16"},
		},
		{
			name:  "a negative number is a value, not the next flag",
			one:   []string{"--rope-theta", "-1"},
			other: []string{"--rope-theta=-1"},
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			require.NotEqual(t, c.one, c.other, "a case states two spellings or none")
			one, err := BuildModelDeploymentCompileCacheSubPath(newCompileCacheInput(
				func(in *ModelDeploymentCompileCacheInput) { in.Role.ExtraArgs = c.one }))
			require.NoError(t, err)
			other, err := BuildModelDeploymentCompileCacheSubPath(newCompileCacheInput(
				func(in *ModelDeploymentCompileCacheInput) { in.Role.ExtraArgs = c.other }))
			require.NoError(t, err)
			assert.Equal(t, one, other)
		})
	}
}

// TestModelDeploymentCompileCacheEnvironmentAffectsTheCache pins which environment entries reach
// the digest: the ones that select what gets compiled, and not the ones that say where to read.
func TestModelDeploymentCompileCacheEnvironmentAffectsTheCache(t *testing.T) {
	cases := []struct {
		name   string
		env    []workercore.ModelDeploymentEnvVar
		wantIn bool
	}{
		{
			name:   "a compile path switch",
			env:    []workercore.ModelDeploymentEnvVar{{Name: "VLLM_USE_STANDALONE_COMPILE", Value: "1"}},
			wantIn: true,
		},
		{
			name:   "the engine generation",
			env:    []workercore.ModelDeploymentEnvVar{{Name: "VLLM_USE_V1", Value: "0"}},
			wantIn: true,
		},
		{
			name:   "the mega AOT artifact switch",
			env:    []workercore.ModelDeploymentEnvVar{{Name: "VLLM_USE_MEGA_AOT_ARTIFACT", Value: "1"}},
			wantIn: true,
		},
		{
			name:   "the FlashAttention build",
			env:    []workercore.ModelDeploymentEnvVar{{Name: "VLLM_FLASH_ATTN_VERSION", Value: "3"}},
			wantIn: true,
		},
		{
			name:   "the SGLang torch compile switch",
			env:    []workercore.ModelDeploymentEnvVar{{Name: "SGLANG_ENABLE_TORCH_COMPILE", Value: "1"}},
			wantIn: true,
		},
		{
			name:   "the Triton attention pipeline depth",
			env:    []workercore.ModelDeploymentEnvVar{{Name: "TRITON_ATTN_NUM_STAGES", Value: "3"}},
			wantIn: true,
		},
		{
			name:   "a cache root says where to read, not what to generate",
			env:    []workercore.ModelDeploymentEnvVar{{Name: "VLLM_CACHE_ROOT", Value: "/tmp/elsewhere"}},
			wantIn: false,
		},
		{
			name:   "a hub token says nothing about compilation",
			env:    []workercore.ModelDeploymentEnvVar{{Name: "HF_TOKEN", Value: "secret"}},
			wantIn: false,
		},
		{
			name:   "a proxy moves no kernel",
			env:    []workercore.ModelDeploymentEnvVar{{Name: "HTTPS_PROXY", Value: "http://proxy:3128"}},
			wantIn: false,
		},
		{
			name: "a repeated environment name is read the way the runtime applies it",
			env: []workercore.ModelDeploymentEnvVar{
				{Name: "VLLM_ATTENTION_BACKEND", Value: "FLASH_ATTN"},
				{Name: "VLLM_ATTENTION_BACKEND", Value: "TRITON_ATTN"},
			},
			wantIn: true,
		},
	}

	baseline, err := BuildModelDeploymentCompileCacheSubPath(newCompileCacheInput())
	require.NoError(t, err)

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			subPath, err := BuildModelDeploymentCompileCacheSubPath(newCompileCacheInput(
				func(in *ModelDeploymentCompileCacheInput) { in.Role.Env = c.env }))
			require.NoError(t, err)
			if c.wantIn {
				assert.NotEqual(t, baseline, subPath)

				return
			}
			assert.Equal(t, baseline, subPath)
		})
	}
}

// TestModelDeploymentCompileCacheVolumes pins the volume a managed role mounts: the host path the
// subpath resolves to, created on demand, and mounted where the engine's cache root points.
func TestModelDeploymentCompileCacheVolumes(t *testing.T) {
	subPath := "nvidia-vllm/m-0123456789abcdef0123456789abcdef/tp1-pp1-dp1-ep1-cp1_1-deadbeef"

	volume, mount := modelDeploymentCompileCacheVolumes(subPath)

	assert.Equal(t, ModelDeploymentCompileCacheVolumeName, volume.Name)
	require.NotNil(t, volume.HostPath, "the cache is a host directory, not a claim")
	assert.Equal(t,
		DefaultModelDeploymentCompileCacheHostDir+"/"+subPath, volume.HostPath.Path)
	require.NotNil(t, volume.HostPath.Type)
	assert.Equal(t, core.HostPathDirectoryOrCreate, *volume.HostPath.Type,
		"the directory is created on first write rather than demanded up front")

	assert.Equal(t, ModelDeploymentCompileCacheVolumeName, mount.Name)
	assert.Equal(t, ModelDeploymentCompileCacheMountPath, mount.MountPath)
	assert.False(t, mount.ReadOnly, "an engine writes its compiled graphs into the cache")
	assert.Empty(t, mount.SubPath, "the whole directory is the role's own")
}

// TestModelDeploymentCompileCacheDefaultedEnv pins which engine is told where its cache lives, and
// that the names are DEFAULTED: a role supplying its own keeps it, so neither name may be owned.
func TestModelDeploymentCompileCacheDefaultedEnv(t *testing.T) {
	cases := []struct {
		name    string
		engine  string
		wantEnv []core.EnvVar
	}{
		{
			name:    "vLLM is told where its cache root is",
			engine:  workercore.ModelDeploymentEngineVLLM,
			wantEnv: []core.EnvVar{{Name: "VLLM_CACHE_ROOT", Value: ModelDeploymentCompileCacheMountPath}},
		},
		{
			name:    "SGLang spells the same root its own way",
			engine:  workercore.ModelDeploymentEngineSGLang,
			wantEnv: []core.EnvVar{{Name: "SGLANG_CACHE_DIR", Value: ModelDeploymentCompileCacheMountPath}},
		},
		{
			name:    "an engine with no such variable gets nothing rather than a wrong one",
			engine:  "Triton",
			wantEnv: nil,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			assert.Equal(t, c.wantEnv, modelDeploymentCompileCacheDefaultedEnv(c.engine))
		})
	}

	t.Run("both names are defaulted, so a role's own entry wins", func(t *testing.T) {
		for _, name := range []string{"VLLM_CACHE_ROOT", "SGLANG_CACHE_DIR"} {
			assert.True(t, ModelDeploymentDefaultsEnv(name), "%s is a default, not a refusal", name)
			for _, engine := range []string{
				workercore.ModelDeploymentEngineVLLM, workercore.ModelDeploymentEngineSGLang,
			} {
				assert.False(t, ModelDeploymentOwnsEnv(engine, name),
					"%s must not be owned on %s, or a user could not redirect the cache",
					name, engine)
			}
		}
	})
}

// TestModelDeploymentCompileCacheSubPathIsStable checks the property the whole table rests on: the
// same inputs render the same path every time, on any machine and in any process, because a
// directory that moved between passes would silently drop a warm cache.
func TestModelDeploymentCompileCacheSubPathIsStable(t *testing.T) {
	first, err := BuildModelDeploymentCompileCacheSubPath(newCompileCacheInput(
		func(in *ModelDeploymentCompileCacheInput) {
			in.Role.ExtraArgs = []string{"--cudagraph-capture-sizes", "1", "2", "--dtype", "bfloat16"}
			in.Role.Env = []workercore.ModelDeploymentEnvVar{
				{Name: "TORCH_CUDA_ARCH_LIST", Value: "9.0"},
				{Name: "VLLM_ATTENTION_BACKEND", Value: "FLASH_ATTN"},
				{Name: "VLLM_USE_STANDALONE_COMPILE", Value: "1"},
			}
		}))
	require.NoError(t, err)

	for range 8 {
		next, err := BuildModelDeploymentCompileCacheSubPath(newCompileCacheInput(
			func(in *ModelDeploymentCompileCacheInput) {
				in.Role.ExtraArgs = []string{"--cudagraph-capture-sizes", "1", "2", "--dtype", "bfloat16"}
				in.Role.Env = []workercore.ModelDeploymentEnvVar{
					{Name: "TORCH_CUDA_ARCH_LIST", Value: "9.0"},
					{Name: "VLLM_ATTENTION_BACKEND", Value: "FLASH_ATTN"},
					{Name: "VLLM_USE_STANDALONE_COMPILE", Value: "1"},
				}
			}))
		require.NoError(t, err)
		require.Equal(t, first, next)
	}
}

// newCompileCacheObjectMeta is the minimal object identity the artifact cases need; the cache path
// reads the namespace, the name and the UID and nothing else an object carries.
func newCompileCacheObjectMeta(name, uid string) meta.ObjectMeta {
	return meta.ObjectMeta{Name: name, Namespace: "team-a", UID: types.UID(uid)}
}

// TestModelDeploymentCompileCacheFlagTableRowsAreDistinct guards the table itself: a duplicated
// canonical name would silently merge two rows, and a row keyed by an alias rather than a name the
// engines read would never match a command line.
func TestModelDeploymentCompileCacheFlagTableRowsAreDistinct(t *testing.T) {
	for canonical, row := range modelDeploymentCompileCacheFlagTable {
		assert.True(t, strings.HasPrefix(canonical, "--") || canonical == modelDeploymentCompileCacheOptimizationFlag,
			"%q is not a canonical flag name", canonical)
		assert.True(t, row.valued || !row.multi,
			"%q takes values only if it takes one value", canonical)
	}
	for short, alias := range modelDeploymentCompileCacheFlagAliases {
		assert.True(t, strings.HasPrefix(short, "-") && !strings.HasPrefix(short, "--"),
			"%q is not a short flag", short)
		_, known := modelDeploymentCompileCacheFlagTable[alias]
		assert.True(t, known, "the alias %q names no row in the table", alias)
	}
}
