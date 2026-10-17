package worker

import (
	"context"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	core "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrlcli "sigs.k8s.io/controller-runtime/pkg/client"

	worker "gpustack.ai/gpustack/api/worker/v1"
	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	"gpustack.ai/gpustack/pkg/nodefeature"
	workerctrl "gpustack.ai/gpustack/pkg/worker/controllers/worker"
	"gpustack.ai/gpustack/pkg/worker/kvcache/inject"
)

// epdInstanceType builds an acceleratable InstanceType reporting one manufacturer, so a case names
// the platform it wants by naming a type and every other rule reads the same.
func epdInstanceType(name, manufacturer string) *worker.InstanceType {
	instType := servingInstanceType(name, 8)
	instType.Status.Detail.Manufacturer = manufacturer

	return instType
}

// epdPlatformTypes is the one instance type per platform the cases below run against, which is
// enough for a shape whose roles are meant to share one platform and for the case that proves they
// need not.
func epdPlatformTypes() []ctrlcli.Object {
	return []ctrlcli.Object{
		epdInstanceType("ascend-8x", nodefeature.ManufacturerAscend),
		epdInstanceType("amd-8x", nodefeature.ManufacturerAMD),
		epdInstanceType("nvidia-8x", nodefeature.ManufacturerNVIDIA),
		epdInstanceType("hygon-8x", nodefeature.ManufacturerHygon),
		epdInstanceType("unknown-8x", ""),
	}
}

// epdRole builds one role of an encoder/prefill/decode deployment. ReplicaSize is one everywhere,
// which the encoder's own rule requires and which the other two roles do not object to.
func epdRole(name string, kind workercore.ModelDeploymentRoleKind, instType string) workercore.ModelDeploymentRole {
	return workercore.ModelDeploymentRole{
		Name:         name,
		Kind:         kind,
		Replicas:     1,
		ReplicaSize:  1,
		InstanceType: instType,
		Resources: &workercore.ModelDeploymentRoleResources{
			Accelerator: resource.NewQuantity(1, resource.DecimalSI),
		},
	}
}

// epdClaimMount is the shared ReadWriteMany claim the encoder leg is carried through.
func epdClaimMount(claimName, subPath, mountPath string, readOnly bool) []workercore.ModelDeploymentAdditionalVolume {
	return []workercore.ModelDeploymentAdditionalVolume{{
		MountPath: mountPath,
		SubPath:   subPath,
		ReadOnly:  readOnly,
		PersistentVolumeClaim: &core.PersistentVolumeClaimVolumeSource{
			ClaimName: claimName,
		},
	}}
}

// epdDeployment builds a valid encoder/prefill/decode deployment on the admitted Ascend row: an
// llm-d-router in front of vLLM, one role of each kind, and the shared claim both sides of the
// encoder leg mounting. Every case then makes it invalid in exactly one way, so a refusal belongs to
// the rule it names and to no other.
func epdDeployment(mutate func(*workercore.ModelDeployment)) *workercore.ModelDeployment {
	md := &workercore.ModelDeployment{
		ObjectMeta: meta.ObjectMeta{Name: "qwen3-vl", Namespace: "team-a"},
		Spec: workercore.ModelDeploymentSpec{
			Model: workercore.ModelDeploymentModel{Name: "Qwen/Qwen3-VL-8B-Instruct"},
			Engine: workercore.ModelDeploymentEngine{
				Name:  workercore.ModelDeploymentEngineVLLM,
				Image: "vllm/vllm-openai:v0.30.0",
			},
			Router: &workercore.ModelDeploymentRouter{Name: workercore.ModelDeploymentRouterLLMD},
			Roles: []workercore.ModelDeploymentRole{
				epdRole("encode", workercore.ModelDeploymentRoleKindEncode, "ascend-8x"),
				epdRole("prefill", workercore.ModelDeploymentRoleKindPrefill, "ascend-8x"),
				epdRole("decode", workercore.ModelDeploymentRoleKindDecode, "ascend-8x"),
			},
		},
	}
	// The encoder writes the embeddings and the prefiller reads them, so only the encoder's mount is
	// required to be writable.
	md.Spec.Roles[0].AdditionalVolumes = epdClaimMount("ec-shared", "qwen3-vl", "/var/ec", false)
	md.Spec.Roles[1].AdditionalVolumes = epdClaimMount("ec-shared", "qwen3-vl", "/var/ec", true)

	if mutate != nil {
		mutate(md)
	}

	return md
}

// epdRoleByName returns the role carrying this name, so a case can change one role without
// restating the other two.
func epdRoleByName(md *workercore.ModelDeployment, name string) *workercore.ModelDeploymentRole {
	for i := range md.Spec.Roles {
		if md.Spec.Roles[i].Name == name {
			return &md.Spec.Roles[i]
		}
	}

	return nil
}

// epdDropRole removes the role carrying this name.
func epdDropRole(md *workercore.ModelDeployment, name string) {
	kept := make([]workercore.ModelDeploymentRole, 0, len(md.Spec.Roles))
	for i := range md.Spec.Roles {
		if md.Spec.Roles[i].Name != name {
			kept = append(kept, md.Spec.Roles[i])
		}
	}
	md.Spec.Roles = kept
}

// epdNetworkRowDeployment builds the same three-role shape on the admitted network row: vLLM on
// ROCm, where the encoder leg is pulled over the network rather than carried through a mounted
// directory. It is a separate builder rather than a flag on epdDeployment because the two rows
// differ in what the roles mount and in what a case is allowed to set -- a helper that took either
// would have to clear two fields to answer a question about one.
func epdNetworkRowDeployment(mutate func(*workercore.ModelDeployment)) *workercore.ModelDeployment {
	md := epdDeployment(func(md *workercore.ModelDeployment) {
		for i := range md.Spec.Roles {
			md.Spec.Roles[i].InstanceType = "amd-8x"
			md.Spec.Roles[i].AdditionalVolumes = nil
		}
	})
	if mutate != nil {
		mutate(md)
	}

	return md
}

// epdSGLangLeg moves a deployment onto an admitted SGLang row, where the encoder's output reaches
// the prefiller by address rather than through a mounted directory or a named encoder-cache
// connector. It is a builder rather than a flag on epdDeployment for the reason
// epdNetworkRowDeployment is: the rows differ in what the roles mount and in which routers name
// them.
func epdSGLangLeg(router, instanceType string) func(*workercore.ModelDeployment) {
	return func(md *workercore.ModelDeployment) {
		md.Spec.Router.Name = router
		md.Spec.Engine.Name = workercore.ModelDeploymentEngineSGLang
		md.Spec.Engine.Image = "lmsysorg/sglang:v0.5.18"
		for i := range md.Spec.Roles {
			md.Spec.Roles[i].InstanceType = instanceType
			// The address leg mounts no claim, because no directory carries it.
			md.Spec.Roles[i].AdditionalVolumes = nil
		}
	}
}

// TestModelDeploymentWebhook_EncoderAdmission covers every rule that bears on a deployment declaring
// an Encode role.
//
// EACH CASE RUNS TWICE: once as a create and once as an update whose previous state is a valid
// admitted deployment. The second run is what the rules are for on an update -- the platform, the
// engine and the router are all read from the object as submitted, so an edit cannot carry
// a deployment off the matrix without being refused here first.
func TestModelDeploymentWebhook_EncoderAdmission(t *testing.T) {
	testCases := []struct {
		name string
		// mutate makes the admitted deployment invalid in exactly one way.
		mutate func(*workercore.ModelDeployment)
		// wantRefuse is a phrase the refusal must carry. An empty one admits the deployment.
		wantRefuse string
		// wantField is the field path the refusal has to land on, for a case that pins one. A
		// refusal landing on a field the user did not edit sends them to edit a field that is
		// already correct, so the path is part of what the case is about rather than a detail of it.
		wantField string
		// selfStored marks a case whose deployment was already stored in this shape, so the update
		// half compares it against itself rather than against an admitted encoder deployment.
		selfStored bool
	}{
		{
			name: "the admitted Ascend row is admitted",
		},
		{
			name: "the admitted ROCm row is admitted",
			mutate: func(md *workercore.ModelDeployment) {
				for i := range md.Spec.Roles {
					md.Spec.Roles[i].InstanceType = "amd-8x"
					// The ROCm row carries the encoder's output over the network, so no role mounts
					// the shared claim.
					md.Spec.Roles[i].AdditionalVolumes = nil
				}
			},
		},
		{
			name: "the admitted NVIDIA row is admitted",
			mutate: func(md *workercore.ModelDeployment) {
				for i := range md.Spec.Roles {
					md.Spec.Roles[i].InstanceType = "nvidia-8x"
					// The NVIDIA row carries the encoder's output over the network, so no role
					// mounts the shared claim.
					md.Spec.Roles[i].AdditionalVolumes = nil
				}
			},
		},
		{
			name: "the admitted vllm-router row is admitted on NVIDIA",
			mutate: func(md *workercore.ModelDeployment) {
				md.Spec.Router.Name = workercore.ModelDeploymentRouterVLLM
				for i := range md.Spec.Roles {
					md.Spec.Roles[i].InstanceType = "nvidia-8x"
					md.Spec.Roles[i].AdditionalVolumes = nil
				}
			},
			selfStored: true,
		},
		{
			name: "the admitted vllm-router row is admitted on ROCm",
			mutate: func(md *workercore.ModelDeployment) {
				md.Spec.Router.Name = workercore.ModelDeploymentRouterVLLM
				for i := range md.Spec.Roles {
					md.Spec.Roles[i].InstanceType = "amd-8x"
					md.Spec.Roles[i].AdditionalVolumes = nil
				}
			},
			selfStored: true,
		},
		{
			// SGLANG IS ADMITTED BEHIND BOTH ROUTERS ON ALL THREE PLATFORMS, and the leg is the same
			// one on every row: the encoder runs --encoder-only, the prefiller runs --language-only
			// and dials it through --encoder-urls. No role mounts a claim, because an address leg
			// carries no directory.
			//
			// These compare against themselves on the update half, because the engine and the router
			// are immutable: an edit between two engines is a different deployment, not a changed one.
			name:       "the admitted SGLang row behind llm-d-router is admitted on NVIDIA",
			mutate:     epdSGLangLeg(workercore.ModelDeploymentRouterLLMD, "nvidia-8x"),
			selfStored: true,
		},
		{
			name:       "the admitted SGLang row behind llm-d-router is admitted on ROCm",
			mutate:     epdSGLangLeg(workercore.ModelDeploymentRouterLLMD, "amd-8x"),
			selfStored: true,
		},
		{
			name:       "the admitted SGLang row behind llm-d-router is admitted on Ascend",
			mutate:     epdSGLangLeg(workercore.ModelDeploymentRouterLLMD, "ascend-8x"),
			selfStored: true,
		},
		{
			name:       "the admitted SGLang row behind sglang-gateway is admitted on NVIDIA",
			mutate:     epdSGLangLeg(workercore.ModelDeploymentRouterSGLang, "nvidia-8x"),
			selfStored: true,
		},
		{
			name:       "the admitted SGLang row behind sglang-gateway is admitted on ROCm",
			mutate:     epdSGLangLeg(workercore.ModelDeploymentRouterSGLang, "amd-8x"),
			selfStored: true,
		},
		{
			name:       "the admitted SGLang row behind sglang-gateway is admitted on Ascend",
			mutate:     epdSGLangLeg(workercore.ModelDeploymentRouterSGLang, "ascend-8x"),
			selfStored: true,
		},
		{
			name: "a platform with no admitted row is refused and names the admitted rows",
			mutate: func(md *workercore.ModelDeployment) {
				for i := range md.Spec.Roles {
					md.Spec.Roles[i].InstanceType = "hygon-8x"
					md.Spec.Roles[i].AdditionalVolumes = nil
				}
			},
			wantRefuse: `platform "hygon" is not admitted for encoder roles under router "llm-d-router" and engine "vLLM"`,
		},
		{
			name: "a platform the router does not admit for an encoder role is refused",
			mutate: func(md *workercore.ModelDeployment) {
				md.Spec.Router.Name = workercore.ModelDeploymentRouterVLLM
			},
			wantRefuse: `platform "ascend" is not admitted for encoder roles under router "vllm-router" and engine "vLLM"`,
			selfStored: true,
		},
		{
			name:       "vllm-router paired with SGLang is refused",
			mutate:     epdSGLangLeg(workercore.ModelDeploymentRouterVLLM, "nvidia-8x"),
			wantRefuse: `router "vllm-router" is not admitted with engine "SGLang"`,
			selfStored: true,
		},
		{
			name: "a router and engine the matrix does not pair is refused",
			mutate: func(md *workercore.ModelDeployment) {
				md.Spec.Router.Name = workercore.ModelDeploymentRouterSGLang
				for i := range md.Spec.Roles {
					md.Spec.Roles[i].InstanceType = "amd-8x"
					md.Spec.Roles[i].AdditionalVolumes = nil
				}
			},
			wantRefuse: `router "sglang-gateway" is not admitted with engine "vLLM"`,
		},
		{
			name:   "an Encode role without a Prefill role is refused",
			mutate: func(md *workercore.ModelDeployment) { epdDropRole(md, "prefill") },
			wantRefuse: `an "Encode" role needs a "Prefill" role beside it, and none is declared: ` +
				`the encoder produces embeddings that only a prefiller reads`,
		},
		{
			name:   "an Encode role without a Decode role is refused",
			mutate: func(md *workercore.ModelDeployment) { epdDropRole(md, "decode") },
			wantRefuse: `an "Encode" role needs a "Decode" role beside it, and none is declared: ` +
				`the prefiller's output needs a "Decode" role to generate from`,
		},
		{
			name: "an Encode role beside a Server role is refused",
			mutate: func(md *workercore.ModelDeployment) {
				md.Spec.Roles = append(md.Spec.Roles,
					epdRole("server", workercore.ModelDeploymentRoleKindServer, "ascend-8x"))
			},
			wantRefuse: `cannot be combined with another kind in one deployment`,
		},
		{
			name: "a second role of the same kind is refused",
			mutate: func(md *workercore.ModelDeployment) {
				md.Spec.Roles = append(md.Spec.Roles,
					epdRole("encode-2", workercore.ModelDeploymentRoleKindEncode, "ascend-8x"))
			},
			wantRefuse: `kind "Encode" is already declared by role "encode"`,
		},
		{
			name:       "an Encode role without a router is refused",
			mutate:     func(md *workercore.ModelDeployment) { md.Spec.Router = nil },
			wantRefuse: "an Encode role requires a router",
		},
		{
			name: "a deployment-wide cache pool is refused",
			mutate: func(md *workercore.ModelDeployment) {
				md.Spec.KVCache = &workercore.ModelDeploymentKVCache{
					PoolRef: core.LocalObjectReference{Name: "shared-kv"},
				}
			},
			wantRefuse: "spec.kvCache is refused on a deployment with an \"Encode\" role",
		},
		{
			name: "a command on the encoder is refused",
			mutate: func(md *workercore.ModelDeployment) {
				epdRoleByName(md, "encode").Command = []string{"/bin/serve"}
			},
			wantRefuse: `role "encode" is one of three roles the operator pairs through a router`,
		},
		{
			name: "a command on the prefiller is refused",
			mutate: func(md *workercore.ModelDeployment) {
				epdRoleByName(md, "prefill").Command = []string{"/bin/serve"}
			},
			wantRefuse: `role "prefill" is one of three roles the operator pairs through a router`,
		},
		{
			name: "a command on the decoder is refused",
			mutate: func(md *workercore.ModelDeployment) {
				epdRoleByName(md, "decode").Command = []string{"/bin/serve"}
			},
			wantRefuse: `role "decode" is one of three roles the operator pairs through a router`,
		},
		{
			name: "roles on two platforms are refused",
			mutate: func(md *workercore.ModelDeployment) {
				epdRoleByName(md, "decode").InstanceType = "nvidia-8x"
			},
			wantRefuse: "the roles of this deployment run on more than one accelerator platform",
		},
		{
			name: "a platform the instance type has not reported yet is refused",
			mutate: func(md *workercore.ModelDeployment) {
				for i := range md.Spec.Roles {
					md.Spec.Roles[i].InstanceType = "unknown-8x"
				}
			},
			wantRefuse: `instance type "unknown-8x" reports no accelerator platform yet`,
		},
		{
			name: "a claim entry naming no claim is refused",
			mutate: func(md *workercore.ModelDeployment) {
				epdRoleByName(md, "encode").AdditionalVolumes[0].PersistentVolumeClaim.ClaimName = ""
			},
			wantRefuse: "the claim name must not be empty",
		},
		{
			name: "a claim entry naming a second source is refused",
			mutate: func(md *workercore.ModelDeployment) {
				epdRoleByName(md, "encode").AdditionalVolumes[0].ConfigMap = &core.LocalObjectReference{Name: "ec-config"}
			},
			wantRefuse: `"configMap" is set beside persistentVolumeClaim on the same entry`,
		},
		{
			name: "the encoder mounting nothing is refused on the shared-storage row",
			mutate: func(md *workercore.ModelDeployment) {
				epdRoleByName(md, "encode").AdditionalVolumes = nil
			},
			wantRefuse: "attach the shared claim the prefiller mounts",
		},
		{
			name: "the prefiller mounting nothing is refused on the shared-storage row",
			mutate: func(md *workercore.ModelDeployment) {
				epdRoleByName(md, "prefill").AdditionalVolumes = nil
			},
			wantRefuse: "attach the same claim role \"encode\" mounts",
		},
		{
			// The three mismatches below each pin the field they land on, because a rule that
			// compares three fields has to say which one it refused: a refusal naming mountPath
			// for a differing claim sends the user to edit a field that is already correct.
			name: "the two roles mounting different claims is refused",
			mutate: func(md *workercore.ModelDeployment) {
				epdRoleByName(md, "prefill").AdditionalVolumes[0].PersistentVolumeClaim.ClaimName = "other"
			},
			wantRefuse: "must name the same claimName",
			wantField:  "spec.roles[0].additionalVolumes.persistentVolumeClaim.claimName",
		},
		{
			name: "the two roles mounting different sub-paths is refused",
			mutate: func(md *workercore.ModelDeployment) {
				epdRoleByName(md, "prefill").AdditionalVolumes[0].SubPath = "other"
			},
			wantRefuse: "must name the same subPath",
			wantField:  "spec.roles[0].additionalVolumes.subPath",
		},
		{
			name: "the two roles mounting at different paths is refused",
			mutate: func(md *workercore.ModelDeployment) {
				epdRoleByName(md, "prefill").AdditionalVolumes[0].MountPath = "/var/other"
			},
			wantRefuse: "must name the same mountPath",
			wantField:  "spec.roles[0].additionalVolumes.mountPath",
		},
		{
			name: "a read-only encoder mount is refused",
			mutate: func(md *workercore.ModelDeployment) {
				epdRoleByName(md, "encode").AdditionalVolumes[0].ReadOnly = true
			},
			wantRefuse: "so its mount must not be read-only",
		},
		{
			name: "an encoder replica size above one is refused",
			mutate: func(md *workercore.ModelDeployment) {
				epdRoleByName(md, "encode").ReplicaSize = 2
			},
			wantRefuse: "so its replica size is 1",
		},
		{
			name: "an encoder carrying the elastic-EP profile is refused",
			mutate: func(md *workercore.ModelDeployment) {
				epdRoleByName(md, "encode").ElasticEP = &workercore.ModelDeploymentRoleElasticEP{Width: 2}
			},
			wantRefuse: `role "encode" cannot carry the elastic-EP profile`,
		},
		{
			name: "an encoder balancing data parallelism outside the engine is refused",
			mutate: func(md *workercore.ModelDeployment) {
				epdRoleByName(md, "encode").ExtraArgs = []string{"--data-parallel-external-lb"}
			},
			wantRefuse: `role "encode" declares a data parallelism the engine balances outside itself`,
		},
		{
			name: "an encoder assigning itself a data-parallel rank is refused",
			mutate: func(md *workercore.ModelDeployment) {
				epdRoleByName(md, "encode").ExtraArgs = []string{"--data-parallel-rank=1"}
			},
			wantRefuse: `role "encode" declares a data parallelism the engine balances outside itself`,
		},
		{
			name: "a deployment without an Encode role keeps its command",
			mutate: func(md *workercore.ModelDeployment) {
				epdDropRole(md, "encode")
				md.Spec.KVCache = &workercore.ModelDeploymentKVCache{
					PoolRef: core.LocalObjectReference{Name: "shared-kv"},
				}
				epdRoleByName(md, "decode").Command = []string{"/bin/serve"}
			},
			selfStored: true,
		},
		{
			name: "a deployment without an Encode role may sit on NVIDIA",
			mutate: func(md *workercore.ModelDeployment) {
				epdDropRole(md, "encode")
				md.Spec.KVCache = &workercore.ModelDeploymentKVCache{
					PoolRef: core.LocalObjectReference{Name: "shared-kv"},
				}
				for i := range md.Spec.Roles {
					md.Spec.Roles[i].InstanceType = "nvidia-8x"
					md.Spec.Roles[i].AdditionalVolumes = nil
				}
			},
			selfStored: true,
		},
		{
			name: "a deployment without an Encode role may carry a claim of its own",
			mutate: func(md *workercore.ModelDeployment) {
				epdDropRole(md, "encode")
				md.Spec.KVCache = &workercore.ModelDeploymentKVCache{
					PoolRef: core.LocalObjectReference{Name: "shared-kv"},
				}
				epdRoleByName(md, "prefill").AdditionalVolumes = nil
			},
			selfStored: true,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Run("create", func(t *testing.T) {
				assertModelDeploymentEPDAdmission(t, tc.mutate, nil, tc.wantRefuse, tc.wantField)
			})
			t.Run("update", func(t *testing.T) {
				// The previous state is a valid admitted deployment, so every refusal below belongs
				// to the edit the case made rather than to a state it inherited. A case marked as
				// already stored compares against itself instead, because dropping an Encode role is
				// an identity change this webhook refuses for a different reason.
				old := epdDeployment(nil)
				if tc.selfStored {
					old = epdDeployment(tc.mutate)
				}

				assertModelDeploymentEPDAdmission(t, tc.mutate, old, tc.wantRefuse, tc.wantField)
			})
		})
	}
}

// assertModelDeploymentEPDAdmission runs one case against both entry points' handlers. A case that
// pins a field is checked on that field and not only on the message, because the message says what
// the rule refused and the path says where the user edits it.
func assertModelDeploymentEPDAdmission(
	t *testing.T, mutate func(*workercore.ModelDeployment), old *workercore.ModelDeployment,
	wantRefuse, wantField string,
) {
	t.Helper()

	w := newModelDeploymentWebhookWith(epdPlatformTypes())
	md := epdDeployment(mutate)

	var err error
	if old == nil {
		_, err = w.ValidateCreate(context.Background(), md)
	} else {
		_, err = w.ValidateUpdate(context.Background(), old, md)
	}

	if wantRefuse == "" {
		assert.NoError(t, err)

		return
	}

	require.Error(t, err, "the deployment is invalid and must be refused")
	assert.Contains(t, err.Error(), wantRefuse)
	if wantField != "" {
		assertInvalidField(t, err, wantField)
	}
}

// TestModelDeploymentWebhook_EncoderKindIsFrozen covers the identity rule on a shape carrying an
// Encode role: the kinds are frozen the way the other kinds are, and a pure reorder stays legal
// because a deployment is not ordered by its role list.
func TestModelDeploymentWebhook_EncoderKindIsFrozen(t *testing.T) {
	testCases := []struct {
		name       string
		old, new   func(*workercore.ModelDeployment)
		wantRefuse string
	}{
		{
			name: "a pure reorder of the roles stays legal",
			new: func(md *workercore.ModelDeployment) {
				md.Spec.Roles[0], md.Spec.Roles[2] = md.Spec.Roles[2], md.Spec.Roles[0]
			},
		},
		{
			name: "a role re-declared as another kind is refused",
			new: func(md *workercore.ModelDeployment) {
				epdRoleByName(md, "encode").Kind = workercore.ModelDeploymentRoleKindDecode
			},
			wantRefuse: modelDeploymentIdentityMessage,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			w := newModelDeploymentWebhookWith(epdPlatformTypes())
			old := epdDeployment(tc.old)
			md := epdDeployment(tc.new)

			_, err := w.ValidateUpdate(context.Background(), old, md)
			if tc.wantRefuse == "" {
				assert.NoError(t, err)

				return
			}

			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.wantRefuse)
		})
	}
}

// TestModelDeploymentWebhook_EncoderReservesNoKVTransferPort covers the transfer ports the webhook
// keeps off a user's --port. The KV split reserves the event publisher's two ports on every role but
// a decoder, and an encoder keeps no KV cache to publish events about, so it reserves neither.
//
// The reservation is read from the function that decides it rather than through an admission, because
// a deployment on which one role reserves a port and another does not cannot be written: the rules
// require every routed role to serve on one port, so putting the number on all three would have the
// prefiller refuse the whole object.
func TestModelDeploymentWebhook_EncoderReservesNoKVTransferPort(t *testing.T) {
	md := epdDeployment(nil)

	reserved := map[string][]int32{}
	for i := range md.Spec.Roles {
		r := &md.Spec.Roles[i]
		reserved[r.Name] = modelDeploymentRouterReservedPorts(md, r)
	}

	assert.Contains(t, reserved["prefill"], inject.VLLMKVEventsPort,
		"the prefiller publishes KV events and keeps those ports off a user's --port")
	assert.NotContains(t, reserved["encode"], inject.VLLMKVEventsPort,
		"the encoder keeps no KV cache, so no listener is rendered onto it to collide with")
	assert.NotContains(t, reserved["encode"], inject.VLLMKVEventsReplayPort)
	assert.Empty(t, reserved["decode"])
}

// TestModelDeploymentWebhook_EncoderReservesBothSGLangBootstrapPorts pins the two listeners a
// language-only SGLang prefiller binds beside an encoder.
//
// THE ENCODER BOOTSTRAP PORT IS RESERVED ONLY BECAUSE AN ENCODE ROLE IS DECLARED. Without one the
// prefiller binds a single registry, and 8997 stays free for a user to claim; with one it binds both,
// so a user naming 8997 would collide with a listener the render creates on that Pod.
//
// Only the prefiller is checked for a reservation, because only the prefiller dials the encoder: the
// encoder serves the addresses it is given and publishes nothing, and the decoder is handed blocks
// rather than calling out.
func TestModelDeploymentWebhook_EncoderReservesBothSGLangBootstrapPorts(t *testing.T) {
	md := epdDeployment(epdSGLangLeg(workercore.ModelDeploymentRouterSGLang, "nvidia-8x"))

	reserved := map[string][]int32{}
	for i := range md.Spec.Roles {
		r := &md.Spec.Roles[i]
		reserved[r.Name] = modelDeploymentRouterReservedPorts(md, r)
	}

	assert.Contains(t, reserved["prefill"], inject.SGLangBootstrapPort,
		"the prefiller serves the bootstrap registry of the split it belongs to")
	assert.Contains(t, reserved["prefill"], inject.SGLangEncoderBootstrapPort,
		"a language-only prefiller beside an Encode role also serves the encoder's registry")
	assert.NotContains(t, reserved["encode"], inject.SGLangEncoderBootstrapPort,
		"the encoder serves the addresses it is given and binds no bootstrap registry")
	assert.NotContains(t, reserved["encode"], inject.SGLangBootstrapPort)
	assert.NotContains(t, reserved["decode"], inject.SGLangEncoderBootstrapPort,
		"the decoder is handed blocks and calls neither registry")
	assert.NotContains(t, reserved["decode"], inject.SGLangBootstrapPort)
}

// TestModelDeploymentWebhook_SGLangPairWithoutAnEncoderReservesOnlyItsOwnPort holds the reservation
// above to the case that justifies it: the same pair with no Encode role binds one registry, so the
// encoder's port is released rather than held against a listener nothing creates.
func TestModelDeploymentWebhook_SGLangPairWithoutAnEncoderReservesOnlyItsOwnPort(t *testing.T) {
	md := epdDeployment(epdSGLangLeg(workercore.ModelDeploymentRouterSGLang, "nvidia-8x"))
	epdDropRole(md, "encode")

	prefill := epdRoleByName(md, "prefill")
	reserved := modelDeploymentRouterReservedPorts(md, prefill)

	assert.Contains(t, reserved, inject.SGLangBootstrapPort)
	assert.NotContains(t, reserved, inject.SGLangEncoderBootstrapPort,
		"a pair with no encoder binds one registry, so the other port stays the user's to claim")
}

// TestModelDeploymentWebhook_ReservedTransferPortIsRefusedWhereRendered proves the reservation above
// reaches the writer, on the role that does render a listener.
func TestModelDeploymentWebhook_ReservedTransferPortIsRefusedWhereRendered(t *testing.T) {
	w := newModelDeploymentWebhookWith(epdPlatformTypes())
	md := epdDeployment(func(md *workercore.ModelDeployment) {
		for i := range md.Spec.Roles {
			md.Spec.Roles[i].ExtraArgs = []string{fmt.Sprintf("--port=%d", inject.VLLMKVEventsPort)}
		}
	})

	_, err := w.ValidateCreate(context.Background(), md)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "which the operator reserves")
}

// TestModelDeploymentWebhook_EncoderServingTheSideChannelPortIsRefused is the reservation as a user
// reaches it, through both spellings a role can name its serving port.
//
// THE DECLARED PORT IS THE ONE THE RENDER HONORS. A role declaring ports is held to the first of
// them and its own --port is not consulted at all (`modelDeploymentTargetPort`), so a case that
// only passed --port would exercise a spelling the render discards. The flag is refused too, because
// a role may pass both and a rule reading one leaves the other open.
//
// BOTH RUN THROUGH AN ADMISSION rather than beside the rule, so the refusal is read the way a writer
// meets it: on the object they submitted, naming the field they wrote.
func TestModelDeploymentWebhook_EncoderServingTheSideChannelPortIsRefused(t *testing.T) {
	for _, tc := range []struct {
		name    string
		serving func(md *workercore.ModelDeployment)
	}{
		{
			name: "a declared container port",
			serving: func(md *workercore.ModelDeployment) {
				for i := range md.Spec.Roles {
					md.Spec.Roles[i].Ports = []workercore.ModelDeploymentPort{
						{Port: inject.VLLMECSideChannelPort, Protocol: core.ProtocolTCP},
					}
				}
			},
		},
		{
			name: "the role's own --port",
			serving: func(md *workercore.ModelDeployment) {
				for i := range md.Spec.Roles {
					md.Spec.Roles[i].ExtraArgs = []string{
						fmt.Sprintf("--port=%d", inject.VLLMECSideChannelPort),
					}
				}
			},
		},
		{
			// A role may name both. The declared set is the one the render prefers, so the refusal
			// has to land even where the flag agrees with it.
			name: "both at once",
			serving: func(md *workercore.ModelDeployment) {
				for i := range md.Spec.Roles {
					md.Spec.Roles[i].Ports = []workercore.ModelDeploymentPort{
						{Port: inject.VLLMECSideChannelPort, Protocol: core.ProtocolTCP},
					}
					md.Spec.Roles[i].ExtraArgs = []string{
						fmt.Sprintf("--port=%d", inject.VLLMECSideChannelPort),
					}
				}
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			md := epdNetworkRowDeployment(tc.serving)

			_, err := newModelDeploymentWebhookWith(epdPlatformTypes()).ValidateCreate(
				context.Background(), md)
			require.Error(t, err,
				"the encoder and its side channel would contend for one number in one container")
			assert.Contains(t, err.Error(), "side channel",
				"the refusal names the socket rather than a bare number")
		})
	}
}

// TestModelDeploymentWebhook_SideChannelPortIsOrdinaryOnTheSharedRow is the other half of the
// reservation: the number is only special on the row that binds the socket.
//
// THE SHARED ROW CARRIES ITS EMBEDDINGS THROUGH A MOUNTED DIRECTORY and opens no second socket, so
// its encoder is free to serve on any number. Putting 5601 in the vendor-blind reservation the other
// listeners use would refuse this deployment for a collision it cannot have.
func TestModelDeploymentWebhook_SideChannelPortIsOrdinaryOnTheSharedRow(t *testing.T) {
	md := epdDeployment(func(md *workercore.ModelDeployment) {
		for i := range md.Spec.Roles {
			md.Spec.Roles[i].Ports = []workercore.ModelDeploymentPort{
				{Port: inject.VLLMECSideChannelPort, Protocol: core.ProtocolTCP},
			}
		}
	})

	row, admitted := workerctrl.ModelDeploymentEPDRowFor(workercore.ModelDeploymentRouterLLMD,
		md.Spec.Engine.Name, nodefeature.ManufacturerAscend)
	require.True(t, admitted, "the shared row is admitted")

	assert.Empty(t, validateModelDeploymentEPDSideChannelPort(md, row),
		"a row that binds no side channel may serve on any number")
}

// TestModelDeploymentWebhook_EncoderKeysAreOwnedOnEveryRoleThatRendersThem is the refusal half of
// the encoder stage's key ownership.
//
// THE KEYS ARE NOT OPTIONAL ONCE THE LEG RENDERS. The side-channel variables are the only pointer
// to the socket a consumer is told to dial, and the transfer flag is the whole document, so a user
// value for either replaces what the operator rendered rather than adding to it -- and because the
// two halves read the same leg, one role accepting a value the other writes produces a leg whose
// ends disagree.
func TestModelDeploymentWebhook_EncoderKeysAreOwnedOnEveryRoleThatRendersThem(t *testing.T) {
	for _, tc := range []struct {
		name string
		key  string
	}{
		{name: "the transfer document", key: "--ec-transfer-config"},
		{name: "the side channel bind address", key: "VLLM_EC_SIDE_CHANNEL_HOST"},
		{name: "the side channel port", key: "VLLM_EC_SIDE_CHANNEL_PORT"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := newModelDeploymentWebhookWith(epdPlatformTypes())
			md := epdDeployment(func(md *workercore.ModelDeployment) {
				// THE CLAIM STAYS. This fixture is built for the shared-volume row, and dropping
				// the claim would fail the shared-volume rule as well -- so the object would be
				// invalid for a second reason and `require.Error` would be satisfied by the wrong
				// refusal. Only the owned key is meant to be wrong here, and the assertion below
				// says so rather than leaving it to the reader.
				for i := range md.Spec.Roles {
					if tc.key == "--ec-transfer-config" {
						md.Spec.Roles[i].ExtraArgs = []string{tc.key, "{}"}
						continue
					}
					md.Spec.Roles[i].Env = []workercore.ModelDeploymentEnvVar{
						{Name: tc.key, Value: "1"},
					}
				}
			})

			_, err := w.ValidateCreate(context.Background(), md)
			require.Error(t, err, "%s is set by the operator and must not be supplied", tc.key)
			assert.Contains(t, err.Error(), "is set by the operator")
			assert.NotContains(t, err.Error(), "shared claim",
				"the fixture must be valid for every rule except the one under test")
		})
	}
}
