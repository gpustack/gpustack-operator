package worker

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	core "k8s.io/api/core/v1"
	"sigs.k8s.io/yaml"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
)

// epdRouterObjects renders the managed router of an encoder/prefill/decode deployment whose roles
// appear in the order the case gives them.
//
// THE ENCODER IS PUT ON THE SHARED SERVING PORT. The shared encoder fixture gives it a port of its
// own so a Service pointed at an encoder is distinguishable from one pointed at a prefiller, and a
// router renders nothing at all for a shape whose roles disagree about that port. What the router
// needs is the opposite: one port every role serves on, which is what the picker discovers them by.
func epdRouterObjects(
	t *testing.T, order []workercore.ModelDeploymentRoleKind,
) ModelDeploymentRouterObjects {
	t.Helper()

	md := epdDeployment(t, order)
	md.Spec.Router = &workercore.ModelDeploymentRouter{Name: workercore.ModelDeploymentRouterLLMD}
	for i := range md.Spec.Roles {
		md.Spec.Roles[i].Ports = []workercore.ModelDeploymentPort{{Protocol: core.ProtocolTCP, Port: 8000}}
		md.Spec.Roles[i].ExtraArgs = nil
	}

	objects, err := renderModelDeploymentRouterObjects(context.Background(), md, nil, false)
	require.NoError(t, err)
	require.NotNil(t, objects.ConfigMap, "this router is configured by a document")

	return objects
}

// epdPickerDocument parses the rendered picker document as a tree.
func epdPickerDocument(t *testing.T, objects ModelDeploymentRouterObjects) map[string]any {
	t.Helper()

	var parsed map[string]any
	require.NoError(t, yaml.Unmarshal([]byte(objects.ConfigMap.Data[modelDeploymentRouterConfigKey]), &parsed))

	return parsed
}

// epdDocumentNames returns every plugin name and every scheduling profile name in the document, in
// the order the document lists them.
func epdDocumentNames(t *testing.T, document map[string]any) (plugins, profiles []string) {
	t.Helper()

	for _, plugin := range document["plugins"].([]any) {
		entry := plugin.(map[string]any)
		name, _ := entry["name"].(string)
		plugins = append(plugins, name)
	}
	for _, profile := range document["schedulingProfiles"].([]any) {
		profiles = append(profiles, profile.(map[string]any)["name"].(string))
	}

	return plugins, profiles
}

// TestRenderModelDeploymentRouterObjects_DiscoversTheEncoderStage is the controller-side half of the
// picker document: the roles reach the renderer with the encoder among them, and the document that
// comes back has a filter and a profile for it.
//
// IT IS ABOUT THE POOL, NOT THE ANSWER. The router's --endpoint-selector is deliberately unchanged
// by an encoder role, because that selector is the POOL the picker draws candidates from
// (llm-d/llm-d-router@v0.11.0 `cmd/epp/runner/runner.go:540`, applied at
// `pkg/epp/datastore/datastore.go:220-227`) and the encoder filter runs against the pool. Excluding
// an encoder from it would leave the encoder profile with no candidate at all, and upstream refuses
// a profile that ends with no endpoint (`pkg/epp/scheduling/scheduler_profile.go:130-136`).
// Stage discovery is therefore the opposite requirement to the answering set, and only the
// answering set is narrowed.
func TestRenderModelDeploymentRouterObjects_DiscoversTheEncoderStage(t *testing.T) {
	objects := epdRouterObjects(t, []workercore.ModelDeploymentRoleKind{
		workercore.ModelDeploymentRoleKindEncode,
		workercore.ModelDeploymentRoleKindPrefill,
		workercore.ModelDeploymentRoleKindDecode,
	})

	plugins, profiles := epdDocumentNames(t, epdPickerDocument(t, objects))
	assert.Contains(t, plugins, "encode-pods", "the pool filter names the encoder")
	assert.Contains(t, profiles, "encode")

	// The pool the picker draws from still holds every role, encoder included.
	selector := objects.ConfigMap.Data[modelDeploymentRouterSelectorKey]
	assert.NotContains(t, selector, "!=",
		"the router's pool selector excludes no role, or the encoder filter would select nothing")
	assert.Contains(t, selector, "app.kubernetes.io/instance=qwen")
}

// TestRenderModelDeploymentRouterObjects_TakesTheEncoderOutOfTheKVEventContract pins the one set
// the encoder is kept out of on the router's side. An encoder keeps no KV cache, so it publishes no
// events, and the published contract is what tells the picker which Pods publish.
func TestRenderModelDeploymentRouterObjects_TakesTheEncoderOutOfTheKVEventContract(t *testing.T) {
	objects := epdRouterObjects(t, []workercore.ModelDeploymentRoleKind{
		workercore.ModelDeploymentRoleKindEncode,
		workercore.ModelDeploymentRoleKindPrefill,
		workercore.ModelDeploymentRoleKindDecode,
	})

	byName := map[string]workercore.ModelDeploymentRouterRoleStatus{}
	for _, role := range objects.Contract.Roles {
		byName[role.Name] = role
	}
	require.Contains(t, byName, "encode")
	assert.Nil(t, byName["encode"].KVEvents, "an encoder publishes no KV events")
	assert.NotNil(t, byName["prefill"].KVEvents, "the prefiller still does")
	assert.Nil(t, byName["decode"].KVEvents, "and the decoder consumes them rather than publishing")
}

// TestRenderModelDeploymentRouterObjects_IsDeterministicAcrossEncoderRoleOrder holds the encoder's
// position in the role list to be the author's accident. The picker document is hashed into the
// router Pod, so a document that followed the declared order would roll the router when a user
// reordered roles that changed nothing else.
func TestRenderModelDeploymentRouterObjects_IsDeterministicAcrossEncoderRoleOrder(t *testing.T) {
	orders := map[string][]workercore.ModelDeploymentRoleKind{
		"encode first": {
			workercore.ModelDeploymentRoleKindEncode,
			workercore.ModelDeploymentRoleKindPrefill,
			workercore.ModelDeploymentRoleKindDecode,
		},
		"encode in the middle": {
			workercore.ModelDeploymentRoleKindPrefill,
			workercore.ModelDeploymentRoleKindEncode,
			workercore.ModelDeploymentRoleKindDecode,
		},
		"encode last": {
			workercore.ModelDeploymentRoleKindPrefill,
			workercore.ModelDeploymentRoleKindDecode,
			workercore.ModelDeploymentRoleKindEncode,
		},
	}

	documents := make(map[string]string, len(orders))
	hashes := make(map[string]string, len(orders))
	for name, order := range orders {
		objects := epdRouterObjects(t, order)
		documents[name] = objects.ConfigMap.Data[modelDeploymentRouterConfigKey]
		hashes[name] = objects.Deployment.Spec.Template.Annotations[modelDeploymentRouterConfigHash]
	}

	assert.Equal(t, documents["encode first"], documents["encode in the middle"])
	assert.Equal(t, documents["encode first"], documents["encode last"])
	assert.Equal(t, hashes["encode first"], hashes["encode in the middle"])
	assert.Equal(t, hashes["encode first"], hashes["encode last"],
		"the same document must hash the same, or the router rolls for a reordering")
}

// TestModelDeploymentTokenizerRole_PrefersThePrefillerOverTheEncoder holds the encoder out of the
// one role the router reads the tokenizer from. The router tokenizes every request against that
// endpoint, and the embeddings an encoder produces are its own model's output for its own requests
// rather than a tokenizer: pointing the router at one puts every prefix measurement on the wrong
// vocabulary.
//
// The prefiller wins by kind, not by position, which is the same rule a pair already ran under; the
// case here is that an encoder whose NAME sorts before "prefill" must not take the seat.
func TestModelDeploymentTokenizerRole_PrefersThePrefillerOverTheEncoder(t *testing.T) {
	orders := map[string][]workercore.ModelDeploymentRoleKind{
		"encode first": {
			workercore.ModelDeploymentRoleKindEncode,
			workercore.ModelDeploymentRoleKindPrefill,
			workercore.ModelDeploymentRoleKindDecode,
		},
		"encode in the middle": {
			workercore.ModelDeploymentRoleKindPrefill,
			workercore.ModelDeploymentRoleKindEncode,
			workercore.ModelDeploymentRoleKindDecode,
		},
		"encode last": {
			workercore.ModelDeploymentRoleKindPrefill,
			workercore.ModelDeploymentRoleKindDecode,
			workercore.ModelDeploymentRoleKindEncode,
		},
	}

	for name, order := range orders {
		t.Run(name, func(t *testing.T) {
			md := epdDeployment(t, order)

			role := modelDeploymentTokenizerRole(md)
			require.NotNil(t, role)
			assert.Equal(t, workercore.ModelDeploymentRoleKindPrefill,
				ModelDeploymentEffectiveRoleKind(role))
		})
	}
}

// TestRenderModelDeploymentRouterObjects_LeavesAPairWithoutTheEncoderStage is the controller-side
// compatibility rule. Every term this work adds is keyed on the encoder role, so a pair renders a
// document with no encoder filter, no encoder profile and no encoder decider.
func TestRenderModelDeploymentRouterObjects_LeavesAPairWithoutTheEncoderStage(t *testing.T) {
	objects := epdRouterObjects(t, []workercore.ModelDeploymentRoleKind{
		workercore.ModelDeploymentRoleKindPrefill,
		workercore.ModelDeploymentRoleKindDecode,
	})

	plugins, profiles := epdDocumentNames(t, epdPickerDocument(t, objects))
	assert.NotContains(t, plugins, "encode-pods")
	assert.NotContains(t, profiles, "encode")

	document := objects.ConfigMap.Data[modelDeploymentRouterConfigKey]
	assert.NotContains(t, document, "always-disagg-multimodal-decider")
	assert.NotContains(t, document, "role-kind!=encode")
}

// TestRenderModelDeploymentRouterObjects_ReservesNoPortForTheEncoder is here because the encoder
// role is otherwise invisible to the router: it holds the renderer out of the KV publisher set, and
// this is the check that it is still a role that renders. The render refuses roles on different
// serving ports, and an encoder declares its own, so a shape that quietly dropped the role would
// render a router no encoder ever appears in.
func TestRenderModelDeploymentRouterObjects_ReservesNoPortForTheEncoder(t *testing.T) {
	md := epdDeployment(t, []workercore.ModelDeploymentRoleKind{
		workercore.ModelDeploymentRoleKindEncode,
		workercore.ModelDeploymentRoleKindPrefill,
		workercore.ModelDeploymentRoleKindDecode,
	})
	md.Spec.Router = &workercore.ModelDeploymentRouter{Name: workercore.ModelDeploymentRouterLLMD}
	for i := range md.Spec.Roles {
		md.Spec.Roles[i].Ports = []workercore.ModelDeploymentPort{{Protocol: core.ProtocolTCP, Port: 8000}}
		md.Spec.Roles[i].ExtraArgs = nil
	}

	objects, err := renderModelDeploymentRouterObjects(context.Background(), md, nil, false)
	require.NoError(t, err)

	kinds := map[workercore.ModelDeploymentRoleKind]bool{}
	for _, role := range objects.Contract.Roles {
		kinds[role.Kind] = true
	}
	assert.True(t, kinds[workercore.ModelDeploymentRoleKindEncode],
		"the encoder is published as a role the router may address")
}
