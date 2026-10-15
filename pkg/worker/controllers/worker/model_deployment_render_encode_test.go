package worker

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	core "k8s.io/api/core/v1"

	worker "gpustack.ai/gpustack/api/worker/v1"
	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	"gpustack.ai/gpustack/pkg/nodefeature"
)

// epdRenderDecodePod renders the decoder of an encoder/prefill/decode deployment on the given
// platform. The connector is shaped so the decoder is a direct decode fronted by the routing
// sidecar, which is the only shape the sidecar is ever rendered in.
func epdRenderDecodePod(
	t *testing.T, manufacturer string,
) *core.Pod {
	t.Helper()

	md := epdDeployment(t, []workercore.ModelDeploymentRoleKind{
		workercore.ModelDeploymentRoleKindEncode,
		workercore.ModelDeploymentRoleKindPrefill,
		workercore.ModelDeploymentRoleKindDecode,
	})
	md.Spec.Router = &workercore.ModelDeploymentRouter{Name: workercore.ModelDeploymentRouterLLMD}

	// The renderer no longer re-derives the transport from the node's manufacturer. It is handed
	// the row's connector already in the sidecar's vocabulary, which is what makes the row the
	// single source for the flag rather than one of two places that decide it.
	connector := ModelDeploymentEPDConnector(md.Spec.Router.Name, md.Spec.Engine.Name, manufacturer)
	require.NotEmpty(t, connector, "%s+%s is not an admitted row", md.Spec.Engine.Name, manufacturer)

	pod, err := renderModelDeploymentPod(context.Background(), ModelDeploymentRenderInput{
		Deployment: md, Role: &md.Spec.Roles[2], InstanceType: newRenderInstanceType(
			func(it *worker.InstanceType) { it.Status.Detail.Manufacturer = manufacturer }),
		Connector: ModelDeploymentConnectorRender{
			Args: []string{"--kv-transfer-config", `{}`}, KVTransfer: true, RoutingSidecar: true,
		},
		ECConnector:   ModelDeploymentECSidecarConnector(connector),
		NativeSidecar: true,
	})
	require.NoError(t, err)
	require.Len(t, pod.Spec.InitContainers, 1, "the decoder runs the routing sidecar")

	return pod
}

// TestRenderModelDeploymentRoutingSidecar_CarriesTheEncoderConnector pins which sidecar handshake the
// encoder leg runs, per platform.
//
// The value is the sidecar's own vocabulary rather than the engine connector's, and the two are not
// the same words for the same thing: the engine runs ECCPUConnector or ECExampleConnector, while the
// sidecar pulls the embeddings out of the encoder's reply over ec-nixl or reads them from the
// directory both roles mount under ec-example
// (llm-d/llm-d-router@v0.11.0 `pkg/sidecar/constants/constants.go:35-39`, registered at
// `pkg/sidecar/proxy/options.go:62,180-183,277-278`).
//
// A value outside that pair does not degrade: the sidecar refuses to start
// (`pkg/sidecar/proxy/options.go:671-673`), so a decoder rendered with a third spelling never
// becomes ready and never answers.
//
// The table lists exactly the platforms the matrix admits, one case per admitted row.
func TestRenderModelDeploymentRoutingSidecar_CarriesTheEncoderConnector(t *testing.T) {
	for _, tc := range []struct {
		name         string
		manufacturer string
		want         string
	}{
		{
			name: "rocm pulls over nixl", manufacturer: nodefeature.ManufacturerAMD,
			want: "--ec-connector=ec-nixl",
		},
		{
			name: "nvidia pulls over nixl", manufacturer: nodefeature.ManufacturerNVIDIA,
			want: "--ec-connector=ec-nixl",
		},
		{
			name: "ascend reads the shared directory", manufacturer: nodefeature.ManufacturerAscend,
			want: "--ec-connector=ec-example",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sidecar := epdRenderDecodePod(t, tc.manufacturer).Spec.InitContainers[0]

			assert.Contains(t, sidecar.Args, tc.want)

			// The same platform twice must not answer differently: the flag is the row's connector
			// and nothing else, so reading it back through the row is an equality, not a lookup
			// that could happen to agree.
			row := ModelDeploymentEPDConnector(workercore.ModelDeploymentRouterLLMD,
				workercore.ModelDeploymentEngineVLLM, tc.manufacturer)
			assert.Contains(t, sidecar.Args,
				"--ec-connector="+ModelDeploymentECSidecarConnector(row),
				"the rendered flag is the admitted row's connector, translated once")
		})
	}
}

// TestRenderModelDeploymentRoutingSidecar_LeavesTheFlagOffWithoutAnEncoder is the other half: the
// flag is absent by default upstream, where an absent value is what tells the sidecar to skip the
// encoder stage entirely. Rendering it onto a deployment that declares no encoder would put an
// encoder stage in a shape with no encoder to run, on a Pod whose configuration is hashed -- so this
// is a compatibility rule as much as a correctness one.
func TestRenderModelDeploymentRoutingSidecar_LeavesTheFlagOffWithoutAnEncoder(t *testing.T) {
	md := newRenderDeployment(func(md *workercore.ModelDeployment) {
		md.Spec.Router = &workercore.ModelDeploymentRouter{Name: workercore.ModelDeploymentRouterLLMD}
		md.Spec.Engine.Version = "0.30.0"
		md.Spec.Roles = []workercore.ModelDeploymentRole{
			{
				Name: "prefill", Kind: workercore.ModelDeploymentRoleKindPrefill,
				InstanceType: "h20-8x", Image: "vllm/vllm-openai:v0.30.0",
			},
			{
				Name: "decode", Kind: workercore.ModelDeploymentRoleKindDecode,
				InstanceType: "h20-8x", Image: "vllm/vllm-openai:v0.30.0",
			},
		}
	})
	pod, err := renderModelDeploymentPod(context.Background(), ModelDeploymentRenderInput{
		Deployment: md, Role: &md.Spec.Roles[1], InstanceType: newRenderInstanceType(),
		Connector: ModelDeploymentConnectorRender{
			Args: []string{"--kv-transfer-config", `{}`}, KVTransfer: true, RoutingSidecar: true,
		},
		NativeSidecar: true,
	})
	require.NoError(t, err)
	require.Len(t, pod.Spec.InitContainers, 1)

	for _, arg := range pod.Spec.InitContainers[0].Args {
		assert.NotContains(t, arg, "--ec-connector",
			"an absent value is what tells the sidecar to skip the encoder stage")
	}
}

// TestRenderModelDeploymentRoutingSidecar_PutsTheConnectorOnlyOnTheDecoder holds the flag to the one
// Pod that runs the sidecar. The encoder and the prefiller carry no sidecar at all -- the decode
// Pod's proxy is the only coordinator in the shape
// (llm-d/llm-d-router@v0.11.0 `docs/disaggregation.md`, "Sidecar Responsibilities (Decode Only)") --
// so a flag rendered anywhere else would name a handshake a process that is not there never reads.
func TestRenderModelDeploymentRoutingSidecar_PutsTheConnectorOnlyOnTheDecoder(t *testing.T) {
	md := epdDeployment(t, []workercore.ModelDeploymentRoleKind{
		workercore.ModelDeploymentRoleKindEncode,
		workercore.ModelDeploymentRoleKindPrefill,
		workercore.ModelDeploymentRoleKindDecode,
	})
	md.Spec.Router = &workercore.ModelDeploymentRouter{Name: workercore.ModelDeploymentRouterLLMD}

	for i := range md.Spec.Roles {
		role := &md.Spec.Roles[i]
		pod, err := renderModelDeploymentPod(context.Background(), ModelDeploymentRenderInput{
			Deployment: md, Role: role, InstanceType: newRenderInstanceType(),
			Connector: ModelDeploymentConnectorRender{
				Args: []string{"--kv-transfer-config", `{}`}, KVTransfer: true,
			},
		})
		require.NoError(t, err)

		for _, container := range append(pod.Spec.InitContainers, pod.Spec.Containers...) {
			if container.Name != "routing-proxy" {
				continue
			}
			for _, arg := range container.Args {
				assert.NotContains(t, arg, "--ec-connector",
					"only the decode sidecar reads the encoder connector")
			}
		}
	}
}
