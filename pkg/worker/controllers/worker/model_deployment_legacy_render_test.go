package worker

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	core "k8s.io/api/core/v1"
	node "k8s.io/api/node/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	ctrlcli "sigs.k8s.io/controller-runtime/pkg/client"

	worker "gpustack.ai/gpustack/api/worker/v1"
	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	"gpustack.ai/gpustack/pkg/nodefeature"
	"gpustack.ai/gpustack/pkg/setting/settingtest"
)

// Every fixture below freezes the COMPLETE rendered Pods of a deployment that declares no Encode
// role: labels, annotations including the pod-spec-hash fingerprint, and the whole spec, the
// routing sidecar included where the decode role carries one. The renderer of those deployments is
// about to change, and these files are the proof it changes nothing: a byte that moves without the
// change saying so fails here, exactly as a Pod whose hash moved rolls a running replica.
//
// HOW IMAGES ARE PINNED, so a moved image default breaks no fixture. The engine image is STATED ON
// EVERY ROLE: a stated image always wins over synthesis, so the synthesis matrix cannot move a
// file. The routing sidecar image and the registry and namespace redirection are SETTINGS with
// defaults, so the test pins all three through the delegated settings Secret for its own duration.
//
// The fixtures render against the environment TestMain pins (cluster 1.31, so a direct decoder
// gets the native sidecar shape) and the remaining settings defaults. Re-baseline deliberately,
// never silently: go test ./pkg/worker/controllers/worker/ -run LegacyRender -update, then read the
// diff, then say in the change what rolled.
var legacyRenderUpdate = flag.Bool(
	"update", false, "rewrite the golden fixture files under testdata/legacy-render")

const (
	legacyRenderDir = "testdata/legacy-render"

	legacyRenderPinnedEngineImage         = "vllm/vllm-openai:v0.25.1"
	legacyRenderPinnedAscendEngineImage   = "vllm/vllm-ascend:v0.23.0"
	legacyRenderPinnedSidecarImage        = "gpustack.example/disagg-sidecar:v0.10.0-fixture"
	legacyRenderAscendInstanceType        = "atlas-910b"
	legacyRenderAscendInstanceTypeEntrant = "entrance-for-atlas-910b"
)

// newLegacyRenderDeployment is the stored-object base every shape varies: engine and model named,
// no cache and no router until a shape adds the one it exercises, one replica per role, and the
// image stated so the synthesis matrix cannot move a fixture.
func newLegacyRenderDeployment() *workercore.ModelDeployment {
	return newRenderDeployment(func(md *workercore.ModelDeployment) {
		md.Spec.KVCache = nil
		md.Spec.Roles = []workercore.ModelDeploymentRole{legacyRenderServerRole("")}
	})
}

// legacyRenderServerRole is the single-role shape a deployment written before disaggregation has.
// A kind of "" is deliberate in the defaulted case: in-memory objects carry the zero value, and
// the render resolves it, which is half of what that fixture freezes.
func legacyRenderServerRole(kind workercore.ModelDeploymentRoleKind) workercore.ModelDeploymentRole {
	return workercore.ModelDeploymentRole{
		Name:         "server",
		Kind:         kind,
		Replicas:     1,
		InstanceType: "h20-8x",
		Resources: &workercore.ModelDeploymentRoleResources{
			Accelerator: ptr.To(resource.MustParse("1")),
		},
		Image: legacyRenderPinnedEngineImage,
	}
}

// legacyRenderPairRoles is a prefill/decode pair on one pool.
func legacyRenderPairRoles(instanceType, image string) []workercore.ModelDeploymentRole {
	return []workercore.ModelDeploymentRole{
		{
			Name:         "prefill",
			Kind:         workercore.ModelDeploymentRoleKindPrefill,
			Replicas:     1,
			InstanceType: instanceType,
			Resources: &workercore.ModelDeploymentRoleResources{
				Accelerator: ptr.To(resource.MustParse("1")),
			},
			Image: image,
		},
		{
			Name:         "decode",
			Kind:         workercore.ModelDeploymentRoleKindDecode,
			Replicas:     1,
			InstanceType: instanceType,
			Resources: &workercore.ModelDeploymentRoleResources{
				Accelerator: ptr.To(resource.MustParse("1")),
			},
			Image: image,
		},
	}
}

// newLegacyRenderAscendInstanceType is the Ascend pool the Ascend pair runs on: the same unit
// sizing as the NVIDIA fixture, a manufacturer that switches the vLLM renderer to its Ascend
// variant and the sidecar to the nixlv2 handshake, and an entrance no derivation from the name
// produces, for the same reason the NVIDIA fixture spells its own that way.
func newLegacyRenderAscendInstanceType() *worker.InstanceType {
	return newRenderInstanceType(func(it *worker.InstanceType) {
		it.Name = legacyRenderAscendInstanceType
		it.Status.Entrance = legacyRenderAscendInstanceTypeEntrant
		it.Status.Detail.Manufacturer = nodefeature.ManufacturerAscend
		it.Status.Detail.RuntimeVersion = "8.2"
		it.Status.Detail.RuntimeVersions = []string{"8.2"}
	})
}

// legacySharedPoolConnection is the connection a resolved shared-pool binding produces, on the
// same literals the connector tests use, so the fixtures and those tests describe one pool.
func legacySharedPoolConnection() *ModelDeploymentConnectorInput {
	connection := connectorInput(workercore.ModelDeploymentEngineVLLM, nodefeature.ManufacturerNVIDIA)

	return &connection
}

// renderLegacyDeploymentPods drives the reconciler's own render pass over one deployment, so the
// fixtures freeze what that pass produces for a stored object: the per-role connector synthesis,
// the template, and the stamp that writes the Kueue group metadata and the spec-hash fingerprint.
// One replica of one member per role is rendered: what separates two replicas is stamped per
// ordinal and covered by the pod-group tests, not by these files.
func renderLegacyDeploymentPods(
	t *testing.T, md *workercore.ModelDeployment, objs []ctrlcli.Object,
	connection *ModelDeploymentConnectorInput,
) []*core.Pod {
	t.Helper()

	cli := newModelDeploymentClient(append([]ctrlcli.Object{md}, objs...)...)
	r := &ModelDeploymentReconciler{Client: cli, APIReader: cli}

	desired, err := r.renderModelDeploymentPods(context.Background(), md, connection, nil, nil)
	require.NoError(t, err)

	pods := make([]*core.Pod, 0, len(md.Spec.Roles))
	for i := range md.Spec.Roles {
		ordinals := desired[md.Spec.Roles[i].Name]
		require.NotNil(t, ordinals, "role %q rendered no replicas", md.Spec.Roles[i].Name)
		require.NotEmpty(t, ordinals[0], "role %q rendered no members", md.Spec.Roles[i].Name)
		pods = append(pods, ordinals[0][0])
	}

	return pods
}

// checkLegacyRenderFixture compares a shape's rendered Pods against its recorded fixture, byte for
// byte, and names the file and a line diff when they disagree. With -update it records instead.
func checkLegacyRenderFixture(t *testing.T, name string, pods []*core.Pod) {
	t.Helper()

	var rendered bytes.Buffer
	encoder := json.NewEncoder(&rendered)
	encoder.SetIndent("", "  ")
	require.NoError(t, encoder.Encode(pods))

	path := filepath.Join(legacyRenderDir, name+".json")
	if *legacyRenderUpdate {
		require.NoError(t, os.MkdirAll(legacyRenderDir, 0o755))
		require.NoError(t, os.WriteFile(path, rendered.Bytes(), 0o644))
		return
	}

	recorded, err := os.ReadFile(path)
	require.NoError(t, err, "the recording is the contract; regenerate it with -update")
	require.Equal(t, string(recorded), rendered.String(),
		"the render of %s moved; re-baseline deliberately with -update", path)
}

// TestModelDeploymentLegacyRender_GoldenPods records and then freezes the rendered Pods of the
// seven legacy shapes: the deployment with no Encode role, in every configuration that reaches a
// distinct branch of the renderer.
func TestModelDeploymentLegacyRender_GoldenPods(t *testing.T) {
	settingtest.MergeDelegatedSettings(t, map[string]string{
		"model-deployment-routing-sidecar-image": legacyRenderPinnedSidecarImage,
		"container-registry":                     "",
		"container-namespace":                    "",
	})

	cases := []struct {
		name string
		// mutate builds the deployment of this shape out of the plain base.
		mutate func(md *workercore.ModelDeployment)
		// instypes adds InstanceTypes beyond the NVIDIA default every shape already gets.
		instypes []*worker.InstanceType
		// connection is the resolved shared-pool binding, nil for shapes with no cache.
		connection *ModelDeploymentConnectorInput
	}{
		{
			// The zero kind is what an in-memory object and a pre-defaulting API carry; the
			// fixture freezes that the render resolves it to the server shape, not to nothing.
			name:   "server-defaulted",
			mutate: func(md *workercore.ModelDeployment) {},
		},
		{
			// The same role with the kind spelled out. The fixture must be byte-identical to
			// server-defaulted: the two spellings are one kind, and any render that treats them
			// differently has invented a fourth kind.
			name: "server-explicit",
			mutate: func(md *workercore.ModelDeployment) {
				md.Spec.Roles[0].Kind = workercore.ModelDeploymentRoleKindServer
			},
		},
		{
			// A pair with neither a router nor a cache renders no connector at all: each half
			// runs undivided, and that plainness is what this fixture freezes.
			name: "pair-lone",
			mutate: func(md *workercore.ModelDeployment) {
				md.Spec.Roles = legacyRenderPairRoles("h20-8x", legacyRenderPinnedEngineImage)
			},
		},
		{
			// The managed pair: the transfer leg and the KV event publisher on the prefiller,
			// the routing sidecar on the decoder.
			name: "pair-llm-d",
			mutate: func(md *workercore.ModelDeployment) {
				md.Spec.Router = &workercore.ModelDeploymentRouter{Name: workercore.ModelDeploymentRouterLLMD}
				md.Spec.Roles = legacyRenderPairRoles("h20-8x", legacyRenderPinnedEngineImage)
			},
		},
		{
			// The shared pool: both halves carry the store client, its projected configuration
			// file and the annotation it is projected from, with no router and therefore no
			// transfer leg, no sidecar and no event publisher.
			name: "pair-shared-pool",
			mutate: func(md *workercore.ModelDeployment) {
				md.Spec.KVCache = &workercore.ModelDeploymentKVCache{
					PoolRef: core.LocalObjectReference{Name: "shared-kv"},
				}
				md.Spec.Roles = legacyRenderPairRoles("h20-8x", legacyRenderPinnedEngineImage)
			},
			connection: legacySharedPoolConnection(),
		},
		{
			// The Ascend pair: the renderer's Ascend variant, the nixlv2 sidecar handshake and
			// no KV event publisher, because that router's data layer is off Ascend.
			name: "pair-ascend-llm-d",
			mutate: func(md *workercore.ModelDeployment) {
				md.Spec.Router = &workercore.ModelDeploymentRouter{Name: workercore.ModelDeploymentRouterLLMD}
				md.Spec.Roles = legacyRenderPairRoles(
					legacyRenderAscendInstanceType, legacyRenderPinnedAscendEngineImage)
			},
			instypes: []*worker.InstanceType{newLegacyRenderAscendInstanceType()},
		},
		{
			// A role that replaces the command line: no engine arguments of the operator's, no
			// connector, no probes, no drain hook and no metrics annotations.
			name: "server-command",
			mutate: func(md *workercore.ModelDeployment) {
				md.Spec.Roles[0].Command = []string{"vllm", "serve", "Qwen/Qwen2.5-72B-Instruct"}
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			md := newLegacyRenderDeployment()
			tc.mutate(md)

			objs := legacyRenderCluster(append(tc.instypes, newRenderInstanceType())...)
			pods := renderLegacyDeploymentPods(t, md, objs, tc.connection)
			require.NotEmpty(t, pods)

			checkLegacyRenderFixture(t, tc.name, pods)
		})
	}
}

// legacyRenderCluster returns the cluster objects every shape renders against: the pool's
// InstanceTypes and the accelerator RuntimeClasses the reconciler looks up, present as they are on
// a cluster this operator installed, so the rendered Pods carry their runtime class names.
func legacyRenderCluster(instTypes ...*worker.InstanceType) []ctrlcli.Object {
	objs := make([]ctrlcli.Object, 0, len(instTypes)+2)
	for _, it := range instTypes {
		objs = append(objs, it)
	}

	return append(objs,
		&node.RuntimeClass{ObjectMeta: meta.ObjectMeta{Name: "nvidia"}},
		&node.RuntimeClass{ObjectMeta: meta.ObjectMeta{Name: "ascend"}},
	)
}
