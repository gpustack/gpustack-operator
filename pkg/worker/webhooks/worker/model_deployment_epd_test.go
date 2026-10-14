package worker

import (
	"context"
	"fmt"
	"strings"
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
				Name: workercore.ModelDeploymentEngineVLLM, Version: "0.23.0",
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
			name:       "an Encode role without a Prefill role is refused",
			mutate:     func(md *workercore.ModelDeployment) { epdDropRole(md, "prefill") },
			wantRefuse: `an "Encode" role needs a "Prefill" role beside it`,
		},
		{
			name:       "an Encode role without a Decode role is refused",
			mutate:     func(md *workercore.ModelDeployment) { epdDropRole(md, "decode") },
			wantRefuse: `an "Encode" role needs a "Decode" role beside it`,
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
			mutate:     func(md *workercore.ModelDeployment) { md.Spec.Engine.Version = "0.22.0" },
			wantRefuse: "ECExampleConnector carries ascend from vLLM 0.23.0",
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
			name: "an engine version that only pre-releases the floor is refused",
			mutate: func(md *workercore.ModelDeployment) {
				md.Spec.Engine.Version = "0.23.0rc1"
			},
			wantRefuse: "0.23.0rc1 is below it",
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
			name: "the two roles mounting different claims is refused",
			mutate: func(md *workercore.ModelDeployment) {
				epdRoleByName(md, "prefill").AdditionalVolumes[0].PersistentVolumeClaim.ClaimName = "other"
			},
			wantRefuse: "must name the same claimName",
		},
		{
			name: "the two roles mounting different sub-paths is refused",
			mutate: func(md *workercore.ModelDeployment) {
				epdRoleByName(md, "prefill").AdditionalVolumes[0].SubPath = "other"
			},
			wantRefuse: "must name the same subPath",
		},
		{
			name: "the two roles mounting at different paths is refused",
			mutate: func(md *workercore.ModelDeployment) {
				epdRoleByName(md, "prefill").AdditionalVolumes[0].MountPath = "/var/other"
			},
			wantRefuse: "must name the same mountPath",
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
				assertModelDeploymentEPDAdmission(t, tc.mutate, nil, tc.wantRefuse)
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

				assertModelDeploymentEPDAdmission(t, tc.mutate, old, tc.wantRefuse)
			})
		})
	}
}

// assertModelDeploymentEPDAdmission runs one case against both entry points' handlers.
func assertModelDeploymentEPDAdmission(
	t *testing.T, mutate func(*workercore.ModelDeployment), old *workercore.ModelDeployment, wantRefuse string,
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

// TestModelDeploymentEPDMatrix pins the table's own shape, so a row cannot be added or edited without
// something here saying so. The rows are the whole admission answer, and the message a refusal
// carries is built from them.
func TestModelDeploymentEPDMatrix(t *testing.T) {
	t.Run("every row names a router, an engine and a platform the API has", func(t *testing.T) {
		routers := map[string]bool{
			workercore.ModelDeploymentRouterLLMD:   true,
			workercore.ModelDeploymentRouterVLLM:   true,
			workercore.ModelDeploymentRouterSGLang: true,
		}
		engines := map[string]bool{
			workercore.ModelDeploymentEngineVLLM:   true,
			workercore.ModelDeploymentEngineSGLang: true,
		}

		for _, row := range modelDeploymentEPDMatrix {
			assert.True(t, routers[row.router], "router %q", row.router)
			assert.True(t, engines[row.engine], "engine %q", row.engine)
			assert.NotEmpty(t, row.platform, "row %+v names no platform", row)
			assert.NotEmpty(t, row.connector, "row %+v names no connector", row)
			assert.NotEmpty(t, row.engineFloor, "row %+v names no floor", row)
		}
	})

	t.Run("no row names the same combination twice", func(t *testing.T) {
		seen := map[string]bool{}
		for _, row := range modelDeploymentEPDMatrix {
			key := strings.Join([]string{row.router, row.engine, row.platform}, "+")
			assert.False(t, seen[key], "%s is admitted twice", key)
			seen[key] = true
		}
	})

	t.Run("no NVIDIA row is admitted", func(t *testing.T) {
		assert.Nil(t, modelDeploymentEPDRowFor(
			workercore.ModelDeploymentRouterLLMD, workercore.ModelDeploymentEngineVLLM,
			nodefeature.ManufacturerNVIDIA),
			"an NVIDIA combination is admitted only after its proof of concept passes")
	})

	t.Run("the two admitted rows are the ones the design names", func(t *testing.T) {
		assert.Equal(t, modelDeploymentEPDRowFor(
			workercore.ModelDeploymentRouterLLMD, workercore.ModelDeploymentEngineVLLM,
			nodefeature.ManufacturerAscend), modelDeploymentEPDRowFor(
			workercore.ModelDeploymentRouterLLMD, workercore.ModelDeploymentEngineVLLM,
			nodefeature.ManufacturerAscend))

		ascend := modelDeploymentEPDRowFor(
			workercore.ModelDeploymentRouterLLMD, workercore.ModelDeploymentEngineVLLM,
			nodefeature.ManufacturerAscend)
		require.NotNil(t, ascend)
		assert.Equal(t, modelDeploymentECConnectorExample, ascend.connector)
		assert.True(t, ascend.sharedVolume, "the Ascend row carries the encoder leg through a claim")

		rocm := modelDeploymentEPDRowFor(
			workercore.ModelDeploymentRouterLLMD, workercore.ModelDeploymentEngineVLLM,
			nodefeature.ManufacturerAMD)
		require.NotNil(t, rocm)
		assert.Equal(t, modelDeploymentECConnectorCPU, rocm.connector)
		assert.False(t, rocm.sharedVolume, "the ROCm row carries the encoder leg over the network")
	})
}

// TestModelDeploymentMeetsFloor covers the version comparison the matrix floors rest on, including
// the two directions that refuse rather than admit.
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
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, modelDeploymentMeetsFloor(tc.version, tc.floor))
		})
	}
}
