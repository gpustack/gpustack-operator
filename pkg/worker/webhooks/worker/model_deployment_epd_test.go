package worker

import (
	"context"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/mod/semver"
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
				Name: workercore.ModelDeploymentEngineVLLM,
				// The floor itself, so the fixture stays valid as the row's floor moves and every
				// case below is refused by the one rule it names rather than by the version.
				Version: workerctrl.ModelDeploymentECExampleConnectorFirstAscend,
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
// differ in what the roles mount, in what their engine version must be, and in what a case is
// allowed to set -- a helper that took either would have to clear three fields to answer a
// question about one.
func epdNetworkRowDeployment(mutate func(*workercore.ModelDeployment)) *workercore.ModelDeployment {
	md := epdDeployment(func(md *workercore.ModelDeployment) {
		md.Spec.Engine.Version = "0.30.0"
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

// TestModelDeploymentWebhook_EncoderAdmission covers every rule that bears on a deployment declaring
// an Encode role.
//
// EACH CASE RUNS TWICE: once as a create and once as an update whose previous state is a valid
// admitted deployment. The second run is what the rules are for on an update -- the platform, the
// engine version and the router are all read from the object as submitted, so an edit cannot carry
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
				md.Spec.Engine.Version = "0.30.0"
				for i := range md.Spec.Roles {
					md.Spec.Roles[i].InstanceType = "amd-8x"
					// The ROCm row carries the encoder's output over the network, so no role mounts
					// the shared claim.
					md.Spec.Roles[i].AdditionalVolumes = nil
				}
			},
		},
		{
			name: "a platform with no admitted row is refused and names the admitted rows",
			mutate: func(md *workercore.ModelDeployment) {
				md.Spec.Engine.Version = "0.30.0"
				for i := range md.Spec.Roles {
					md.Spec.Roles[i].InstanceType = "nvidia-8x"
					md.Spec.Roles[i].AdditionalVolumes = nil
				}
			},
			wantRefuse: `platform "nvidia" is not admitted for encoder roles under router "llm-d-router" and engine "vLLM"`,
		},
		{
			name: "a router that drives no encoder role is refused",
			mutate: func(md *workercore.ModelDeployment) {
				md.Spec.Engine.Version = "0.30.0"
				md.Spec.Router.Name = workercore.ModelDeploymentRouterSGLang
				for i := range md.Spec.Roles {
					md.Spec.Roles[i].InstanceType = "amd-8x"
					md.Spec.Roles[i].AdditionalVolumes = nil
				}
			},
			wantRefuse: `router "sglang-gateway" is not admitted for an encoder role`,
		},
		{
			name: "a router and engine the matrix does not pair is refused",
			mutate: func(md *workercore.ModelDeployment) {
				md.Spec.Engine.Name = workercore.ModelDeploymentEngineSGLang
				md.Spec.Engine.Version = "0.30.0"
				for i := range md.Spec.Roles {
					md.Spec.Roles[i].InstanceType = "amd-8x"
					md.Spec.Roles[i].AdditionalVolumes = nil
				}
			},
			wantRefuse: `router "llm-d-router" is not admitted with engine "SGLang"`,
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
			name:       "an engine without a version is refused",
			mutate:     func(md *workercore.ModelDeployment) { md.Spec.Engine.Version = "" },
			wantRefuse: "an encoder role needs the engine version to be set",
		},
		{
			name:       "an engine version below the row floor is refused",
			mutate:     func(md *workercore.ModelDeployment) { md.Spec.Engine.Version = "0.23.0" },
			wantRefuse: "ECExampleConnector carries ascend from vllm-ascend 0.30.0rc1",
		},
		{
			name: "an engine version below the network connector floor is refused",
			mutate: func(md *workercore.ModelDeployment) {
				md.Spec.Engine.Version = "0.29.0"
				for i := range md.Spec.Roles {
					md.Spec.Roles[i].InstanceType = "amd-8x"
					md.Spec.Roles[i].AdditionalVolumes = nil
				}
			},
			wantRefuse: "ECCPUConnector carries amd from vLLM 0.30.0",
		},
		{
			name: "a version spelling a pre-release the way the project publishes it is read",
			mutate: func(md *workercore.ModelDeployment) {
				md.Spec.Engine.Version = "0.28.0.rc1"
			},
			wantRefuse: "0.28.0.rc1 is below it",
		},
		{
			// The same tag refused for the network row, which is the direction that has to survive
			// the rewrite: the connector exists only in the release, not in its candidates.
			name: "a pre-release of the network connector's floor is refused",
			mutate: func(md *workercore.ModelDeployment) {
				md.Spec.Engine.Version = "0.30.0rc1"
				for i := range md.Spec.Roles {
					md.Spec.Roles[i].InstanceType = "amd-8x"
					md.Spec.Roles[i].AdditionalVolumes = nil
				}
			},
			wantRefuse: "ECCPUConnector carries amd from vLLM 0.30.0",
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
				md.Spec.Engine.Version = "0.25.1"
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
				md.Spec.Engine.Version = "0.25.1"
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
				md.Spec.Engine.Version = "0.25.1"
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
				md.Spec.Engine.Version = "0.30.0"
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

// TestModelDeploymentSemver covers the rewrite the floor comparison reads, including the two
// spellings a project publishes and the versions it leaves unreadable.
//
// A rewritten spelling must end up VALID, because an invalid one sorts below every valid version
// and would be refused by every floor. A version nothing could be read out of must stay invalid,
// which is what keeps it on the refusing side of every comparison.
func TestModelDeploymentSemver(t *testing.T) {
	testCases := []struct {
		name      string
		version   string
		want      string
		wantValid bool
	}{
		{name: "a release is only given its leading v", version: "0.30.0", want: "v0.30.0", wantValid: true},
		{name: "a release carrying one is left alone", version: "v0.30.0", want: "v0.30.0", wantValid: true},
		// THE TWO SPELLINGS VLLM-ASCEND PUBLISHES, neither of which semver reads as written.
		{name: "a candidate spelled rcN", version: "0.30.0rc1", want: "v0.30.0-rc.1", wantValid: true},
		{name: "a candidate spelled rcN and carrying a v", version: "v0.30.0rc1", want: "v0.30.0-rc.1", wantValid: true},
		{name: "a candidate spelled .rcN", version: "0.28.0.rc1", want: "v0.28.0-rc.1", wantValid: true},
		// The number is separated so that a two-digit candidate orders above a one-digit one:
		// semver compares an alphanumeric pre-release identifier lexically, and rc10 sorts below rc9.
		{name: "a two-digit candidate keeps its order", version: "0.30.0rc10", want: "v0.30.0-rc.10", wantValid: true},
		// A post-release is newer than the release it follows, and the only slot above a release
		// that semver has is the next patch release. Its number rides along as a second identifier
		// so two posts of one release keep the order the project publishes them in.
		{name: "a post-release sorts above its release", version: "0.23.0.post1", want: "v0.23.1-0.1", wantValid: true},
		{name: "a post-release carrying a v", version: "v0.23.0.post1", want: "v0.23.1-0.1", wantValid: true},
		{name: "a later post-release keeps its order", version: "0.30.0.post2", want: "v0.30.1-0.2", wantValid: true},
		// The unreadable cases are what keep a version on the refusing side of every comparison
		// rather than making it comparable.
		{name: "an unreadable version is left alone", version: "latest", want: "vlatest"},
		{name: "a candidate naming no release stays unreadable", version: "rc1", want: "v-rc.1"},
		// A leading zero is a number semver will not read as a numeric identifier, so a tail
		// carrying one is not recognized and the whole version stays unreadable.
		{name: "a candidate number with a leading zero stays unreadable", version: "0.30.0rc01", want: "v0.30.0rc01"},
		{name: "a post number with a leading zero stays unreadable", version: "0.30.0.post01", want: "v0.30.0.post01"},
		// A patch too large to count leaves no next patch to build the post on, so the version is
		// left alone and reads as the tail it still carries.
		{
			name:    "a release too large to count stays unreadable",
			version: "0.30.123456789012345678901234567890.post1",
			want:    "v0.30.123456789012345678901234567890.post1",
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			got := modelDeploymentSemver(tc.version)
			assert.Equal(t, tc.want, got)
			assert.Equal(t, tc.wantValid, semver.IsValid(got),
				"a rewritten spelling must be readable to the comparison")
		})
	}
}

// TestModelDeploymentMeetsFloor covers the version comparison the matrix floors rest on, including
// the two directions that refuse rather than admit.
//
// THE TWO FLOORS ARE THE TWO PROJECTS THAT PUBLISH A DIFFERENT SPELLING, so each row below reads
// the versions its own project writes: the Ascend row the candidate and post tails vllm-ascend
// tags carry, the network row the plain releases vLLM tags carry.
func TestModelDeploymentMeetsFloor(t *testing.T) {
	testCases := []struct {
		name           string
		version, floor string
		want           bool
	}{
		{name: "the floor itself meets it", version: "0.30.0", floor: "0.30.0", want: true},
		{name: "a later minor meets it", version: "0.31.2", floor: "0.30.0", want: true},
		{name: "an earlier minor does not", version: "0.29.9", floor: "0.30.0"},
		{name: "a later major meets it", version: "1.0.0", floor: "0.30.0", want: true},
		{name: "a v-prefixed version is read the same", version: "v0.30.0", floor: "v0.30.0", want: true},
		{name: "a pre-release sorts below its release", version: "0.30.0rc1", floor: "0.30.0"},
		{name: "an unreadable version sorts below a valid one", version: "latest", floor: "0.30.0"},
		{name: "an empty version sorts below a valid one", version: "", floor: "0.30.0"},

		// THE ASCEND FLOOR IS ITSELF A CANDIDATE, so these cases read it as a version rather than
		// as a literal: a floor this comparison could not read would refuse its own tag.
		{name: "a release below the candidate floor does not meet it", version: "0.23.0", floor: "0.30.0rc1"},
		{name: "a candidate below the floor does not meet it", version: "0.28.0.rc1", floor: "0.30.0rc1"},
		{name: "the candidate floor meets itself", version: "0.30.0rc1", floor: "0.30.0rc1", want: true},
		{name: "a later candidate meets the floor", version: "0.30.0rc2", floor: "0.30.0rc1", want: true},
		{name: "the release the floor leads to meets it", version: "0.30.0", floor: "0.30.0rc1", want: true},
		{name: "a post-release of that release meets it", version: "0.30.0.post1", floor: "0.30.0rc1", want: true},
		{name: "a post-release sorts above the release it follows", version: "0.30.0.post1", floor: "0.30.0", want: true},
		{name: "a post-release does not meet a floor above it", version: "0.23.0.post1", floor: "0.30.0rc1"},
		{name: "an unreadable version does not meet a candidate floor", version: "latest", floor: "0.30.0rc1"},

		// TWO POST-RELEASES OF ONE RELEASE KEEP THEIR OWN ORDER, on either side of the two releases
		// they sit between.
		{name: "a later post-release meets an earlier one's floor", version: "0.30.0.post2", floor: "0.30.0.post1", want: true},
		{name: "an earlier post-release does not meet a later one's floor", version: "0.30.0.post2", floor: "0.30.0.post3"},
		{name: "a post-release does not meet a patch release above it", version: "0.30.0.post1", floor: "0.30.1"},
		{name: "a version whose number carries a leading zero does not meet the floor", version: "0.30.0rc01", floor: "0.30.0rc1"},
		{name: "a version too large to count does not meet a floor", version: "0.30.123456789012345678901234567890.post1", floor: "0.30.0.post1"},

		// The network row keeps refusing a candidate: the connector is in the release alone.
		{name: "a candidate does not meet the release floor", version: "0.30.0rc2", floor: "0.30.0"},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, modelDeploymentMeetsFloor(tc.version, tc.floor))
		})
	}
}

// TestModelDeploymentMeetsFloor_AdmitsEveryRowFloor reads the admitted rows rather than a hand-written
// pair, because the failure this guards against is a FLOOR THAT REFUSES ITSELF: a row naming its
// floor in a spelling the comparison cannot read would refuse every deployment carrying exactly
// the version it requires.
func TestModelDeploymentMeetsFloor_AdmitsEveryRowFloor(t *testing.T) {
	for _, platform := range []string{nodefeature.ManufacturerAscend, nodefeature.ManufacturerAMD} {
		row, admitted := workerctrl.ModelDeploymentEPDRowFor(
			workercore.ModelDeploymentRouterLLMD, workercore.ModelDeploymentEngineVLLM, platform)
		require.True(t, admitted, "the %s row is admitted", platform)

		assert.True(t, modelDeploymentMeetsFloor(row.EngineFloor, row.EngineFloor),
			"the %s row refuses the very floor it names", platform)
	}
}
