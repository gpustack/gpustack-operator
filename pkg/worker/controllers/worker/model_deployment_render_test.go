package worker

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	core "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/utils/ptr"
	kueuectrlconst "sigs.k8s.io/kueue/pkg/controller/constants"

	worker "gpustack.ai/gpustack/api/worker/v1"
	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	"gpustack.ai/gpustack/pkg/nodefeature"
	"gpustack.ai/gpustack/pkg/setting/settingtest"
	"gpustack.ai/gpustack/pkg/systemmeta"
	"gpustack.ai/gpustack/pkg/utils/stringx"
	"gpustack.ai/gpustack/pkg/worker/kvcache/inject"
	"gpustack.ai/gpustack/pkg/worker/runtimecontrol"
)

// newRenderDeployment builds a deployment whose single role every case then varies.
func newRenderDeployment(mutate ...func(*workercore.ModelDeployment)) *workercore.ModelDeployment {
	md := &workercore.ModelDeployment{
		ObjectMeta: meta.ObjectMeta{Name: "qwen", Namespace: "team-a", UID: "md-uid"},
		Spec: workercore.ModelDeploymentSpec{
			Model: workercore.ModelDeploymentModel{Name: "Qwen/Qwen2.5-72B-Instruct"},
			Engine: workercore.ModelDeploymentEngine{
				Name:  workercore.ModelDeploymentEngineVLLM,
				Image: "vllm/vllm-openai:v0.25.1",
			},
			KVCache: &workercore.ModelDeploymentKVCache{PoolRef: core.LocalObjectReference{Name: "shared-kv"}},
			Roles: []workercore.ModelDeploymentRole{{
				Name:            "server",
				Replicas:        2,
				AcceleratorType: "h20-8x",
				Image:           "vllm/vllm-openai:v0.25.1",
			}},
		},
	}
	for _, m := range mutate {
		m(md)
	}

	return md
}

// newRenderAcceleratorType builds an AcceleratorType fixture for rendering tests.
func newRenderAcceleratorType(mutate ...func(*worker.AcceleratorType)) *worker.AcceleratorType {
	at := &worker.AcceleratorType{
		ObjectMeta: meta.ObjectMeta{Name: "h20-8x"},
		Status: workercore.AcceleratorTypeStatus{
			// DELIBERATELY NOT FormatLocalQueueName("h20-8x"). The entrance is read from this field,
			// and a fixture spelling it the way a name-derived render would spell it could not tell
			// the two apart -- the assertion in _Identity would pass either way. This value is one
			// no derivation produces.
			Entrance: "queue-for-h20-8x",
			// The observed detail carries the hardware the sizing reads. A role naming no image
			// renders the engine's image, whatever this detail reports.
			Detail: workercore.InstanceTypeDetail{
				Manufacturer: nodefeature.ManufacturerNVIDIA,
				InstanceTypeAcceleratorDetail: workercore.InstanceTypeAcceleratorDetail{
					RuntimeVersion:  "12.9",
					RuntimeVersions: []string{"12.9"},
				},
			},
		},
	}
	for _, m := range mutate {
		m(at)
	}

	return at
}

func renderOne(t *testing.T, md *workercore.ModelDeployment, at *worker.AcceleratorType) *core.Pod {
	t.Helper()

	pod, err := renderModelDeploymentPod(context.Background(), ModelDeploymentRenderInput{
		Deployment:      md,
		Role:            &md.Spec.Roles[0],
		AcceleratorType: at,
	})
	require.NoError(t, err)

	return pod
}

func envValue(pod *core.Pod, name string) (string, bool) {
	for _, e := range pod.Spec.Containers[0].Env {
		if e.Name == name {
			return e.Value, true
		}
	}

	return "", false
}

// TestRenderModelDeploymentPod_Identity pins what makes a rendered Pod findable and schedulable:
// its generated name prefix, the labels a Service selects on, the entrance label that routes it
// into the role's pool, the resource note a watch filters on, and the controller reference that
// makes it ours.
func TestRenderModelDeploymentPod_Identity(t *testing.T) {
	md := newRenderDeployment()
	pod, err := renderModelDeploymentPod(context.Background(), ModelDeploymentRenderInput{
		Deployment:      md,
		Role:            &md.Spec.Roles[0],
		AcceleratorType: newRenderAcceleratorType(),
	})
	require.NoError(t, err)

	// The replica carries no name of its own: the prefix names the deployment and the role, and
	// the suffix the API server appends to it is the only per-instance part.
	assert.Empty(t, pod.Name)
	assert.Equal(t, "qwen-server-", pod.GenerateName)
	assert.Equal(t, "team-a", pod.Namespace)

	assert.Equal(t, "model-deployment", pod.Labels[modelDeploymentLabelKeyName])
	assert.Equal(t, "qwen", pod.Labels[modelDeploymentLabelKeyInstance])
	assert.Equal(t, "server", pod.Labels[modelDeploymentLabelKeyComponent])

	// The InstanceType's PUBLISHED entrance, verbatim -- not FormatLocalQueueName of its name. The
	// fixture spells the two differently on purpose, so this asserts which one the render read.
	assert.Equal(t, "queue-for-h20-8x", pod.Labels[kueuectrlconst.QueueLabel],
		"the entrance label is read from the InstanceType's status, not derived from its name")
	assert.True(t, systemmeta.MatchResource(pod, ModelDeploymentResourceType))

	require.Len(t, pod.OwnerReferences, 1)
	assert.Equal(t, "ModelDeployment", pod.OwnerReferences[0].Kind)
	assert.Equal(t, md.UID, pod.OwnerReferences[0].UID)
	assert.True(t, ptr.Deref(pod.OwnerReferences[0].Controller, false),
		"the reference must be a CONTROLLER reference, or the owned-Pod watch never fires")
}

func TestRenderModelDeploymentPod_DecodeUsesRoutingSidecar(t *testing.T) {
	for _, tc := range []struct {
		name         string
		externalPort int32
	}{
		{name: "default serving port", externalPort: 8000},
		{name: "custom serving port", externalPort: 9000},
	} {
		t.Run(tc.name, func(t *testing.T) {
			md := newRenderDeployment(func(md *workercore.ModelDeployment) {
				md.Spec.Router = &workercore.ModelDeploymentRouter{Name: workercore.ModelDeploymentRouterLLMD}
				md.Spec.Roles[0].Kind = workercore.ModelDeploymentRoleKindDecode
				md.Spec.Roles[0].Ports = []workercore.ModelDeploymentPort{{
					Protocol: core.ProtocolTCP, Port: tc.externalPort,
				}}
			})
			pod, err := renderModelDeploymentPod(context.Background(), ModelDeploymentRenderInput{
				Deployment: md, Role: &md.Spec.Roles[0], AcceleratorType: newRenderAcceleratorType(),
				Connector: ModelDeploymentConnectorRender{
					Args: []string{"--kv-transfer-config", `{}`}, KVTransfer: true, RoutingSidecar: true,
				},
				NativeSidecar: true,
			})
			require.NoError(t, err)

			require.Len(t, pod.Spec.InitContainers, 1)
			sidecar := pod.Spec.InitContainers[0]
			assert.Equal(t, "routing-proxy", sidecar.Name)
			assert.Equal(t, "gpustack/mirrored-llm-d-router-disagg-sidecar:v1.0.0", sidecar.Image)
			require.NotNil(t, sidecar.RestartPolicy)
			assert.Equal(t, core.ContainerRestartPolicyAlways, *sidecar.RestartPolicy)
			assert.Contains(t, sidecar.Args, fmt.Sprintf("--port=%d", tc.externalPort))
			assert.Contains(t, sidecar.Args, "--model-server-port=8200")
			assert.Contains(t, sidecar.Args, "--kv-connector=mooncake")
			assert.Contains(t, sidecar.Args, "--mooncake-bootstrap-port=8998")
			assert.Contains(t, sidecar.Args, "--secure-proxy=false")
			require.Len(t, sidecar.Ports, 1)
			assert.Equal(t, tc.externalPort, sidecar.Ports[0].ContainerPort)

			main := pod.Spec.Containers[0]
			assert.Contains(t, main.Command, "8200")
			assert.NotEqual(t, tc.externalPort, main.Ports[0].ContainerPort)
			assert.Equal(t, int32(8200), main.Ports[0].ContainerPort)
			assert.Equal(t, "8200", pod.Annotations["prometheus.io/port"])
			assert.Equal(t, tc.externalPort, main.StartupProbe.HTTPGet.Port.IntVal)
			assert.Equal(t, tc.externalPort, main.ReadinessProbe.HTTPGet.Port.IntVal)
			assert.Equal(t, tc.externalPort, main.LivenessProbe.HTTPGet.Port.IntVal)
		})
	}
}

// TestRenderModelDeploymentPod_DecodeReadsTheEnginesPort follows the direct decoder's model-server
// port through every spelling the engine reads as --port: the proxy has to forward to the port the
// engine actually opens, which is the last entry naming it, however it is spelled.
func TestRenderModelDeploymentPod_DecodeReadsTheEnginesPort(t *testing.T) {
	testCases := []struct {
		name      string
		extraArgs []string
		wantPort  string
	}{
		{name: "the_last_spelling_wins", extraArgs: []string{"--port", "9100", "--por", "9200"}, wantPort: "9200"},
		{name: "the_last_spelling_wins_reversed", extraArgs: []string{"--por", "9200", "--port", "9100"}, wantPort: "9100"},
		{name: "a_lone_abbreviation_is_the_roles_own_port", extraArgs: []string{"--por=9200"}, wantPort: "9200"},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			md := newRenderDeployment(func(md *workercore.ModelDeployment) {
				md.Spec.Router = &workercore.ModelDeploymentRouter{Name: workercore.ModelDeploymentRouterLLMD}
				md.Spec.Roles[0].Kind = workercore.ModelDeploymentRoleKindDecode
				md.Spec.Roles[0].ExtraArgs = tc.extraArgs
			})
			pod, err := renderModelDeploymentPod(context.Background(), ModelDeploymentRenderInput{
				Deployment: md, Role: &md.Spec.Roles[0], AcceleratorType: newRenderAcceleratorType(),
				Connector: ModelDeploymentConnectorRender{
					Args: []string{"--kv-transfer-config", `{}`}, KVTransfer: true, RoutingSidecar: true,
				},
				NativeSidecar: true,
			})
			require.NoError(t, err)

			require.Len(t, pod.Spec.InitContainers, 1)
			assert.Contains(t, pod.Spec.InitContainers[0].Args, "--model-server-port="+tc.wantPort)
			main := pod.Spec.Containers[0]
			assert.Equal(t, tc.extraArgs, main.Command[len(main.Command)-len(tc.extraArgs)-2:len(main.Command)-2],
				"the role's own port is kept and nothing but the host is filled after it")
			assert.Equal(t, []string{"--host", "0.0.0.0"}, main.Command[len(main.Command)-2:])
		})
	}
}

// TestRenderModelDeploymentPod_ListenFlagSpellings reads the listen and TLS flags back the way the
// engine's own parser does: vLLM rewrites underscores, both engines resolve a unique prefix, and
// which prefixes are unique differs between the two. A spelling the operator misreads either sends
// a plaintext probe at a TLS listener, keeps a gate a client-certificate listener refuses, or fills
// a --port over the role's own.
func TestRenderModelDeploymentPod_ListenFlagSpellings(t *testing.T) {
	const (
		vllm   = workercore.ModelDeploymentEngineVLLM
		sglang = workercore.ModelDeploymentEngineSGLang
	)
	testCases := []struct {
		name      string
		engine    string
		extraArgs []string
		// wantFill is what the operator appends after the role's own arguments.
		wantFill []string
		// wantScheme is the probes' scheme; empty means the replica is not gated at all.
		wantScheme core.URIScheme
	}{
		{
			name: "vllm_underscore_certificate_turns_on_tls", engine: vllm,
			extraArgs: []string{"--ssl_certfile", "/c.pem"},
			wantFill:  []string{"--host", "0.0.0.0", "--port", "8000"}, wantScheme: core.URISchemeHTTPS,
		},
		{
			name: "vllm_abbreviated_certificate_turns_on_tls", engine: vllm,
			extraArgs: []string{"--ssl-certf=/c.pem"},
			wantFill:  []string{"--host", "0.0.0.0", "--port", "8000"}, wantScheme: core.URISchemeHTTPS,
		},
		{
			name: "vllm_underscore_key_turns_on_tls", engine: vllm,
			extraArgs: []string{"--ssl_keyfile", "/k.pem"},
			wantFill:  []string{"--host", "0.0.0.0", "--port", "8000"}, wantScheme: core.URISchemeHTTPS,
		},
		{
			// "--ssl-ce" is ambiguous on vLLM, which also registers --ssl-cert-reqs.
			name: "sglang_abbreviated_certificate_turns_on_tls", engine: sglang,
			extraArgs: []string{"--ssl-ce", "/c.pem"},
			wantFill:  []string{"--host", "0.0.0.0", "--port", "8000"}, wantScheme: core.URISchemeHTTPS,
		},
		{
			// SGLang registers no --ssl-cert-reqs, so this prefix has one flag to reach.
			name: "sglang_reads_ssl_cert_as_the_certificate", engine: sglang,
			extraArgs: []string{"--ssl-cert", "/c.pem"},
			wantFill:  []string{"--host", "0.0.0.0", "--port", "8000"}, wantScheme: core.URISchemeHTTPS,
		},
		{
			// The engine refuses this spelling at start; it is not read as TLS here either.
			name: "sglang_does_not_rewrite_underscores", engine: sglang,
			extraArgs: []string{"--ssl_certfile", "/c.pem"},
			wantFill:  []string{"--host", "0.0.0.0", "--port", "8000"}, wantScheme: core.URISchemeHTTP,
		},
		{
			name: "vllm_abbreviated_client_certificate_mode_withdraws_the_gates", engine: vllm,
			extraArgs: []string{"--ssl-certfile", "/c.pem", "--ssl-cert-r", "2"},
			wantFill:  []string{"--host", "0.0.0.0", "--port", "8000"},
		},
		{
			name: "vllm_underscore_client_certificate_mode_withdraws_the_gates", engine: vllm,
			extraArgs: []string{"--ssl-certfile", "/c.pem", "--ssl_cert_reqs=2"},
			wantFill:  []string{"--host", "0.0.0.0", "--port", "8000"},
		},
		{
			// FILLED, NOT OWNED, in every spelling: the role's own port stands and nothing is
			// appended after it, exactly as for --port itself.
			name: "an_abbreviated_port_is_the_roles_own", engine: vllm,
			extraArgs: []string{"--por", "9100"},
			wantFill:  []string{"--host", "0.0.0.0"}, wantScheme: core.URISchemeHTTP,
		},
		{
			name: "an_abbreviated_host_is_the_roles_own", engine: sglang,
			extraArgs: []string{"--ho=127.0.0.1"},
			wantFill:  []string{"--port", "8000"},
		},
		{
			// The baseline: a flag sharing the first letters of --port is not a listen flag.
			name: "a_flag_sharing_a_stem_moves_nothing", engine: vllm,
			extraArgs: []string{"--pooler-config", "{}"},
			wantFill:  []string{"--host", "0.0.0.0", "--port", "8000"}, wantScheme: core.URISchemeHTTP,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			md := newRenderDeployment(func(md *workercore.ModelDeployment) {
				md.Spec.Engine.Name = tc.engine
				md.Spec.Roles[0].ExtraArgs = tc.extraArgs
			})
			pod, err := renderModelDeploymentPod(context.Background(), ModelDeploymentRenderInput{
				Deployment: md, Role: &md.Spec.Roles[0], AcceleratorType: newRenderAcceleratorType(),
			})
			require.NoError(t, err)

			c := pod.Spec.Containers[0]
			require.GreaterOrEqual(t, len(c.Command), len(tc.extraArgs)+len(tc.wantFill))
			assert.Equal(t, append(slices.Clone(tc.extraArgs), tc.wantFill...),
				c.Command[len(c.Command)-len(tc.extraArgs)-len(tc.wantFill):])
			if tc.wantScheme == "" {
				assert.Nil(t, c.StartupProbe)
				assert.Nil(t, c.ReadinessProbe)
				assert.Nil(t, c.LivenessProbe)
				return
			}
			require.NotNil(t, c.StartupProbe)
			require.NotNil(t, c.ReadinessProbe)
			require.NotNil(t, c.LivenessProbe)
			assert.Equal(t, tc.wantScheme, c.StartupProbe.HTTPGet.Scheme)
			assert.Equal(t, tc.wantScheme, c.ReadinessProbe.HTTPGet.Scheme)
			assert.Equal(t, tc.wantScheme, c.LivenessProbe.HTTPGet.Scheme)
		})
	}
}

// TestRenderModelDeploymentPod_DecodeUsesClassicSidecarBelowTheFloor asserts the shape a cluster
// without initContainers[].restartPolicy gets: the SAME proxy, carried as a regular container
// rather than an init one. What changes is where it lands and that it carries no per-container
// restart policy; what must NOT change is the traffic gate -- the engine container's probes still
// target the proxy's port, so the Pod stays unready until the proxy listens, on either shape.
func TestRenderModelDeploymentPod_DecodeUsesClassicSidecarBelowTheFloor(t *testing.T) {
	md := newRenderDeployment(func(md *workercore.ModelDeployment) {
		md.Spec.Router = &workercore.ModelDeploymentRouter{Name: workercore.ModelDeploymentRouterLLMD}
		md.Spec.Roles[0].Kind = workercore.ModelDeploymentRoleKindDecode
		md.Spec.Roles[0].Ports = []workercore.ModelDeploymentPort{{
			Protocol: core.ProtocolTCP, Port: 8000,
		}}
	})
	pod, err := renderModelDeploymentPod(context.Background(), ModelDeploymentRenderInput{
		Deployment: md, Role: &md.Spec.Roles[0], AcceleratorType: newRenderAcceleratorType(),
		Connector: ModelDeploymentConnectorRender{
			Args: []string{"--kv-transfer-config", `{}`}, KVTransfer: true, RoutingSidecar: true,
		},
		NativeSidecar: false,
	})
	require.NoError(t, err)

	assert.Empty(t, pod.Spec.InitContainers, "a plain init container that never exits would wedge the Pod in Init")
	require.Len(t, pod.Spec.Containers, 2)

	sidecar := pod.Spec.Containers[0]
	assert.Equal(t, "routing-proxy", sidecar.Name, "the proxy is the FIRST regular container, the closest list order gets to starting first")
	assert.Equal(t, "gpustack/mirrored-llm-d-router-disagg-sidecar:v1.0.0", sidecar.Image)
	assert.Nil(t, sidecar.RestartPolicy,
		"a regular container leaves the field nil and lets the Pod's restartPolicy restart it")
	assert.Contains(t, sidecar.Args, "--port=8000")
	assert.Contains(t, sidecar.Args, "--model-server-port=8200")
	require.Len(t, sidecar.Ports, 1)
	assert.Equal(t, int32(8000), sidecar.Ports[0].ContainerPort)

	main := pod.Spec.Containers[1]
	assert.Contains(t, main.Command, "8200")
	assert.Equal(t, int32(8200), main.Ports[0].ContainerPort)
	assert.Equal(t, int32(8000), main.StartupProbe.HTTPGet.Port.IntVal)
	assert.Equal(t, int32(8000), main.ReadinessProbe.HTTPGet.Port.IntVal)
	assert.Equal(t, int32(8000), main.LivenessProbe.HTTPGet.Port.IntVal)
}

// sidecarRole is the minimal role the routing sidecar reads: a name and the port its Service
// fronts.
func sidecarRole() *workercore.ModelDeploymentRole {
	return &workercore.ModelDeploymentRole{
		Name:  "decode",
		Ports: []workercore.ModelDeploymentPort{{Protocol: core.ProtocolTCP, Port: 8000}},
	}
}

// argValue returns the value that follows a flag in an argument list, failing the test when the
// flag is absent, so a handshake argument that disappears cannot pass as one that was never
// asserted.
func argValue(t *testing.T, args []string, flag string) string {
	t.Helper()

	for i := 0; i+1 < len(args); i++ {
		if args[i] == flag {
			return args[i+1]
		}
	}
	require.Failf(t, "flag not rendered", "%q is not among %v", flag, args)

	return ""
}

// TestRenderModelDeploymentRoutingSidecar_HandshakeFollowsTheEngine is called directly rather
// than through the whole render so the connector vocabulary is pinned per engine and per vendor
// without standing up a pair.
func TestRenderModelDeploymentRoutingSidecar_HandshakeFollowsTheEngine(t *testing.T) {
	t.Run("vllm speaks the mooncake handshake", func(t *testing.T) {
		sidecar := renderModelDeploymentRoutingSidecar(
			context.Background(), sidecarRole(), 8200, core.URISchemeHTTP,
			workercore.ModelDeploymentEngineVLLM, true, nodefeature.ManufacturerNVIDIA, "", "",
		)

		assert.Contains(t, sidecar.Args, "--kv-connector=mooncake")
		assert.Contains(t, sidecar.Args,
			fmt.Sprintf("--mooncake-bootstrap-port=%d", inject.VLLMMooncakeBootstrapPort))
		assert.NotContains(t, sidecar.Args, "--kv-connector=sglang",
			"the SGLang connector would query a registry no vLLM prefiller serves")
		for _, e := range sidecar.Env {
			assert.NotEqual(t, "SGLANG_BOOTSTRAP_PORT", e.Name,
				"the variable is the SGLang port's only carrier and vLLM carries none of it")
		}
	})

	t.Run("vllm on ascend relays through nixlv2", func(t *testing.T) {
		sidecar := renderModelDeploymentRoutingSidecar(
			context.Background(), sidecarRole(), 8200, core.URISchemeHTTP,
			workercore.ModelDeploymentEngineVLLM, true, nodefeature.ManufacturerAscend, "", "",
		)

		assert.Contains(t, sidecar.Args, "--kv-connector=nixlv2",
			"the Ascend connector speaks the four-null-key handshake the nixlv2 mode relays")
		for _, arg := range sidecar.Args {
			assert.NotContains(t, arg, "mooncake",
				"an Ascend pair has no bootstrap registry, so no mooncake flag may render")
		}
	})

	t.Run("sglang speaks the sglang handshake", func(t *testing.T) {
		sidecar := renderModelDeploymentRoutingSidecar(
			context.Background(), sidecarRole(), 8200, core.URISchemeHTTP,
			workercore.ModelDeploymentEngineSGLang, true, nodefeature.ManufacturerNVIDIA, "", "",
		)

		assert.Contains(t, sidecar.Args, "--kv-connector=sglang")
		assert.NotContains(t, sidecar.Args, "mooncake",
			"a mooncake flag left beside the SGLang connector names a handshake this sidecar does not speak")
		assert.Equal(t, strconv.Itoa(int(inject.SGLangBootstrapPort)),
			argEnvValue(t, sidecar.Env, "SGLANG_BOOTSTRAP_PORT"))
	})
}

// argEnvValue returns a rendered variable's value, failing the test when the variable is absent.
func argEnvValue(t *testing.T, env []core.EnvVar, name string) string {
	t.Helper()

	for i := range env {
		if env[i].Name == name {
			return env[i].Value
		}
	}
	require.Failf(t, "variable not rendered", "%q is not among the sidecar's environment", name)

	return ""
}

// TestRenderModelDeploymentRoutingSidecar_SGLangBootstrapPortPairsWithThePrefiller asserts the
// pairing the whole disaggregation shape rests on: the prefiller's bootstrap-server argument, the
// annotation a discovering router reads, and the decode sidecar's environment variable are THREE
// WRITINGS OF ONE VALUE. The assertion compares the rendered values to each other and to neither
// implementation, so a literal substituted at any end -- the exact drift this test exists for --
// fails here regardless of which end moved.
func TestRenderModelDeploymentRoutingSidecar_SGLangBootstrapPortPairsWithThePrefiller(t *testing.T) {
	prefill, err := SynthesizeModelDeploymentConnector(ModelDeploymentConnectorInput{
		Engine:              workercore.ModelDeploymentEngineSGLang,
		Manufacturer:        nodefeature.ManufacturerNVIDIA,
		Domain:              "team-a-shared",
		MasterServerAddress: "shared-kv-master.gpustack-system.svc:50051",
		Protocols:           []string{"tcp"},
		Kind:                workercore.ModelDeploymentRoleKindPrefill,
		Disaggregated:       true,
		KVTransfer:          true,
	})
	require.NoError(t, err)

	engineArg := argValue(t, prefill.Args, "--disaggregation-bootstrap-port")
	annotation := prefill.PodAnnotations["sglang.ai/bootstrap-port"]
	require.NotEmpty(t, annotation, "the prefiller publishes no bootstrap port for discovery")

	sidecar := renderModelDeploymentRoutingSidecar(
		context.Background(), sidecarRole(), 8200, core.URISchemeHTTP,
		workercore.ModelDeploymentEngineSGLang, true, nodefeature.ManufacturerNVIDIA, "", "",
	)
	sidecarEnv := argEnvValue(t, sidecar.Env, "SGLANG_BOOTSTRAP_PORT")

	assert.Equal(t, engineArg, sidecarEnv,
		"the sidecar must dial the port the prefiller serves the registry on")
	assert.Equal(t, engineArg, annotation,
		"the annotation must publish the port the prefiller actually serves")
}

// TestRenderModelDeploymentPod_ProbeRouteFollowsTheServingShape asserts what route the three gates
// read in each shape, with LITERAL paths on both sides: comparing against the constants would hold
// however either of them drifted, which is exactly the drift the direct-decode shape introduced --
// the shared route stayed in place while the port underneath it changed owners.
func TestRenderModelDeploymentPod_ProbeRouteFollowsTheServingShape(t *testing.T) {
	t.Run("engine-only uses sentinel live and ready routes by default", func(t *testing.T) {
		md := newRenderDeployment()

		pod, err := renderModelDeploymentPod(context.Background(), ModelDeploymentRenderInput{
			Deployment: md, Role: &md.Spec.Roles[0], AcceleratorType: newRenderAcceleratorType(),
		})
		require.NoError(t, err)

		c := pod.Spec.Containers[0]
		require.NotNil(t, c.StartupProbe)
		require.NotNil(t, c.StartupProbe.HTTPGet)
		assert.Equal(t, "/gpustack/v1/live", c.StartupProbe.HTTPGet.Path)
		assert.Equal(t, modelDeploymentDefaultPort, c.StartupProbe.HTTPGet.Port.IntVal)

		require.NotNil(t, c.ReadinessProbe)
		require.NotNil(t, c.ReadinessProbe.HTTPGet)
		assert.Equal(t, "/gpustack/v1/ready", c.ReadinessProbe.HTTPGet.Path)
		assert.Equal(t, modelDeploymentDefaultPort, c.ReadinessProbe.HTTPGet.Port.IntVal)

		require.NotNil(t, c.LivenessProbe)
		require.NotNil(t, c.LivenessProbe.HTTPGet)
		assert.Equal(t, "/gpustack/v1/live", c.LivenessProbe.HTTPGet.Path)
		assert.Equal(t, modelDeploymentDefaultPort, c.LivenessProbe.HTTPGet.Port.IntVal)
	})

	t.Run("non-sentinel CPU-only role keeps the shared /health route", func(t *testing.T) {
		md := newRenderDeployment(func(md *workercore.ModelDeployment) {
			md.Spec.Roles[0].AcceleratorType = ""
		})

		pod, err := renderModelDeploymentPod(context.Background(), ModelDeploymentRenderInput{
			Deployment: md, Role: &md.Spec.Roles[0],
		})
		require.NoError(t, err)

		c := pod.Spec.Containers[0]
		for name, p := range map[string]*core.Probe{
			"startup": c.StartupProbe, "readiness": c.ReadinessProbe, "liveness": c.LivenessProbe,
		} {
			require.NotNil(t, p, "%s gate", name)
			require.NotNil(t, p.HTTPGet, "%s gate must read the engine's route, not accept a socket", name)
			assert.Equal(t, "/health", p.HTTPGet.Path,
				"%s gate keeps the route whose refusal grades the engine on this shape", name)
			assert.Equal(t, modelDeploymentDefaultPort, p.HTTPGet.Port.IntVal, "%s gate port", name)
		}
	})

	t.Run("direct decode uses sentinel routes on the sidecar port when active and /v1/models when held", func(t *testing.T) {
		// THE DEFECT THIS CASE GUARDS: the sidecar owns the service port and answers /health itself,
		// in every state, so a gate on that route grades the sidecar and reports Ready for the whole
		// model load. Both /gpustack/v1/{live,ready} and /v1/models are forwarded by the sidecar to
		// the engine, failing with 503 until the engine serves.
		md := newRenderDeployment(func(md *workercore.ModelDeployment) {
			md.Spec.Router = &workercore.ModelDeploymentRouter{Name: workercore.ModelDeploymentRouterLLMD}
			md.Spec.Roles[0].Kind = workercore.ModelDeploymentRoleKindDecode
			md.Spec.Roles[0].Ports = []workercore.ModelDeploymentPort{{
				Protocol: core.ProtocolTCP, Port: 8000,
			}}
		})

		activePod, err := renderModelDeploymentPod(context.Background(), ModelDeploymentRenderInput{
			Deployment: md, Role: &md.Spec.Roles[0], AcceleratorType: newRenderAcceleratorType(),
			Connector: ModelDeploymentConnectorRender{
				Args: []string{"--kv-transfer-config", `{}`}, KVTransfer: true, RoutingSidecar: true,
			},
			NativeSidecar: true,
		})
		require.NoError(t, err)

		activeCtr := activePod.Spec.Containers[0]
		assert.Equal(t, "/gpustack/v1/live", activeCtr.StartupProbe.HTTPGet.Path)
		assert.Equal(t, "/gpustack/v1/ready", activeCtr.ReadinessProbe.HTTPGet.Path)
		assert.Equal(t, "/gpustack/v1/live", activeCtr.LivenessProbe.HTTPGet.Path)
		for name, p := range map[string]*core.Probe{
			"startup": activeCtr.StartupProbe, "readiness": activeCtr.ReadinessProbe, "liveness": activeCtr.LivenessProbe,
		} {
			assert.Equal(t, int32(8000), p.HTTPGet.Port.IntVal,
				"%s gate still grades the address the Service fronts -- the sidecar's port, not the engine's", name)
		}

		heldPod, err := renderModelDeploymentPod(context.Background(), ModelDeploymentRenderInput{
			Deployment: md, Role: &md.Spec.Roles[0], AcceleratorType: newRenderAcceleratorType(),
			Connector: ModelDeploymentConnectorRender{
				Args: []string{"--kv-transfer-config", `{}`}, KVTransfer: true, RoutingSidecar: true,
			},
			NativeSidecar:         true,
			EngineSentinelEnabled: ptr.To(false),
		})
		require.NoError(t, err)

		heldCtr := heldPod.Spec.Containers[0]
		for name, p := range map[string]*core.Probe{
			"startup": heldCtr.StartupProbe, "readiness": heldCtr.ReadinessProbe, "liveness": heldCtr.LivenessProbe,
		} {
			require.NotNil(t, p, "%s gate", name)
			require.NotNil(t, p.HTTPGet, "%s gate must read the engine's route, not accept a socket", name)
			assert.Equal(t, "/v1/models", p.HTTPGet.Path,
				"%s gate reads the route whose answer turns on the engine, not the one the sidecar answers itself", name)
			assert.Equal(t, int32(8000), p.HTTPGet.Port.IntVal,
				"%s gate still grades the address the Service fronts -- the sidecar's port, not the engine's", name)
		}
	})
}

// TestRenderModelDeploymentPod_EntranceLabelIsNotInTheSelector states why the two label sets are
// different. The selector is what a Service is created with and cannot change; the entrance label
// follows the role's InstanceType, which a spec update can move. A selector carrying it would
// orphan every replica already running the moment the type changed.
func TestRenderModelDeploymentPod_EntranceLabelIsNotInTheSelector(t *testing.T) {
	md := newRenderDeployment()
	selector := modelDeploymentSelectorLabels(md, &md.Spec.Roles[0])

	assert.NotContains(t, selector, kueuectrlconst.QueueLabel)

	pod := renderOne(t, md, newRenderAcceleratorType())
	for k, v := range selector {
		assert.Equal(t, v, pod.Labels[k], "a replica must carry every label its Service selects on")
	}
}

// TestRenderModelDeploymentPod_RoleKindLabel pins the value of the role-kind label, which is what
// something in front of the replicas selects on to tell a prefiller from a decoder.
//
// EVERY CASE HERE SPELLS THE ROLE'S NAME DIFFERENTLY FROM ITS KIND, and that is the whole design of
// the table. The fixture's default role is named "server" and its unset kind resolves to "server",
// so a render that published role.Name instead of the effective kind would produce the same label
// on it -- the assertion would pass while reading the wrong field. The last case is the sharpest:
// a role NAMED decode whose kind is prefill, where the two answers are both legal kind words and
// only one of them is right.
func TestRenderModelDeploymentPod_RoleKindLabel(t *testing.T) {
	testCases := []struct {
		name     string
		roleName string
		kind     workercore.ModelDeploymentRoleKind
		want     string
	}{
		{
			name:     "an unset kind resolves to server rather than to the empty string",
			roleName: "runner",
			kind:     "",
			want:     "server",
		},
		{
			name:     "prefill",
			roleName: "first-half",
			kind:     workercore.ModelDeploymentRoleKindPrefill,
			want:     "prefill",
		},
		{
			name:     "decode",
			roleName: "second-half",
			kind:     workercore.ModelDeploymentRoleKindDecode,
			want:     "decode",
		},
		{
			name:     "a role named decode whose kind is prefill is labelled prefill",
			roleName: "decode",
			kind:     workercore.ModelDeploymentRoleKindPrefill,
			want:     "prefill",
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			md := newRenderDeployment(func(md *workercore.ModelDeployment) {
				md.Spec.Roles[0].Name = tc.roleName
				md.Spec.Roles[0].Kind = tc.kind
			})

			pod := renderOne(t, md, newRenderAcceleratorType())

			assert.Equal(t, tc.want, pod.Labels[modelDeploymentLabelKeyRoleKind])
			assert.Equal(t, tc.roleName, pod.Labels[modelDeploymentLabelKeyComponent],
				"the role's name keeps its own key, so the two are separately selectable")
		})
	}
}

// TestRenderModelDeploymentPod_RoleKindLabelIsNotInTheSelector keeps the role-kind label out of what
// a Service is created with, for the reason recorded on the constant.
//
// The absence assertion carries a positive baseline beside it on purpose: "the selector does not
// contain this key" is true of a render that never produces the key at all, so on its own it would
// pass against the tree as it stood before the key existed.
func TestRenderModelDeploymentPod_RoleKindLabelIsNotInTheSelector(t *testing.T) {
	md := newRenderDeployment(func(md *workercore.ModelDeployment) {
		md.Spec.Roles[0].Kind = workercore.ModelDeploymentRoleKindPrefill
	})

	selector := modelDeploymentSelectorLabels(md, &md.Spec.Roles[0])
	assert.NotContains(t, selector, modelDeploymentLabelKeyRoleKind)

	pod := renderOne(t, md, newRenderAcceleratorType())
	require.Contains(t, pod.Labels, modelDeploymentLabelKeyRoleKind,
		"the baseline for the absence above: the replica does carry the key the selector omits")
}

// TestRenderModelDeploymentPod_RoleKindLabelSeparatesAPair asserts the property the label exists
// for: within ONE deployment, a selector on the kind reaches one half of a pair and not the other.
//
// Rendering each role and comparing the two values is what makes this more than the per-role case
// above. A render that published a constant, or that read the deployment rather than the role, gives
// every replica of both roles the same value and passes every single-role assertion in this file.
func TestRenderModelDeploymentPod_RoleKindLabelSeparatesAPair(t *testing.T) {
	md := newRenderDeployment(func(md *workercore.ModelDeployment) {
		md.Spec.Roles[0].Name = "p"
		md.Spec.Roles[0].Kind = workercore.ModelDeploymentRoleKindPrefill
		md.Spec.Roles = append(md.Spec.Roles, workercore.ModelDeploymentRole{
			Name:            "d",
			Replicas:        2,
			AcceleratorType: "h20-8x",
			Kind:            workercore.ModelDeploymentRoleKindDecode,
			Image:           "vllm/vllm-openai:v0.25.1",
		})
	})

	it := newRenderAcceleratorType()
	labelOf := func(i int) string {
		pod, err := renderModelDeploymentPod(context.Background(), ModelDeploymentRenderInput{
			Deployment:      md,
			Role:            &md.Spec.Roles[i],
			AcceleratorType: it,
		})
		require.NoError(t, err)

		return pod.Labels[modelDeploymentLabelKeyRoleKind]
	}

	assert.Equal(t, "prefill", labelOf(0))
	assert.Equal(t, "decode", labelOf(1))
}

// TestRenderModelDeploymentPod_Command covers the whole argv, which the operator owns end to end
// because Command replaces it and there is no Args: there is nowhere to put arguments beside an
// image's own entrypoint, so the append tier can only append to a command line the operator built.
func TestRenderModelDeploymentPod_Command(t *testing.T) {
	testCases := []struct {
		name        string
		engine      string
		extraArgs   []string
		connector   ModelDeploymentConnectorRender
		wantCommand []string
	}{
		{
			// THE LISTEN ADDRESS IS RENDERED, not left to the engine. vLLM would have opened every
			// interface on this port by itself; writing it makes the argv say so.
			name:   "vllm base command, model positional, listen address filled",
			engine: workercore.ModelDeploymentEngineVLLM,
			wantCommand: []string{
				"vllm", "serve", "Qwen/Qwen2.5-72B-Instruct",
				"--host", "0.0.0.0", "--port", "8000",
			},
		},
		{
			// THE CASE THE RENDERING EXISTS FOR. Left alone this engine opens 127.0.0.1:30000,
			// which no Service endpoint and no kubelet probe can reach.
			name:   "sglang base command names the model through a flag, and is moved off loopback",
			engine: workercore.ModelDeploymentEngineSGLang,
			wantCommand: []string{
				"python3", "-m", "sglang.launch_server", "--model-path", "Qwen/Qwen2.5-72B-Instruct",
				"--enable-metrics",
				"--host", "0.0.0.0", "--port", "8000",
			},
		},
		{
			name:      "connector arguments land before the user's, and the address after both",
			engine:    workercore.ModelDeploymentEngineVLLM,
			connector: ModelDeploymentConnectorRender{Args: []string{`--kv-transfer-config={}`}},
			extraArgs: []string{"--max-model-len=32768"},
			wantCommand: []string{
				"vllm", "serve", "Qwen/Qwen2.5-72B-Instruct",
				`--kv-transfer-config={}`,
				"--max-model-len=32768",
				"--host", "0.0.0.0", "--port", "8000",
			},
		},
		{
			// FILLED, NOT OWNED. A role that names either flag keeps its own value, and the
			// operator adds only the one that is missing.
			name:      "a role's own port is kept and only the host is filled",
			engine:    workercore.ModelDeploymentEngineVLLM,
			extraArgs: []string{"--port", "9100"},
			wantCommand: []string{
				"vllm", "serve", "Qwen/Qwen2.5-72B-Instruct",
				"--port", "9100",
				"--host", "0.0.0.0",
			},
		},
		{
			// The other spelling has to answer alike, or the operator would append a second value
			// for a flag the role already set.
			name:      "the equals spelling counts as already set",
			engine:    workercore.ModelDeploymentEngineSGLang,
			extraArgs: []string{"--host=127.0.0.1"},
			wantCommand: []string{
				"python3", "-m", "sglang.launch_server", "--model-path", "Qwen/Qwen2.5-72B-Instruct",
				"--enable-metrics",
				"--host=127.0.0.1",
				"--port", "8000",
			},
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			md := newRenderDeployment(func(md *workercore.ModelDeployment) {
				md.Spec.Engine.Name = tc.engine
				md.Spec.Roles[0].ExtraArgs = tc.extraArgs
			})

			pod, err := renderModelDeploymentPod(context.Background(), ModelDeploymentRenderInput{
				Deployment:      md,
				Role:            &md.Spec.Roles[0],
				AcceleratorType: newRenderAcceleratorType(),
				Connector:       tc.connector,
			})
			require.NoError(t, err)

			assert.Equal(t, tc.wantCommand, pod.Spec.Containers[0].Command)
		})
	}
}

// TestRenderModelDeploymentPod_TakeOver pins the third tier. A role that replaces the command owns
// the argv, so the operator contributes no engine argument, no client environment and no mounted
// configuration — every one of which describes a file the replaced argv never names.
func TestRenderModelDeploymentPod_TakeOver(t *testing.T) {
	md := newRenderDeployment(func(md *workercore.ModelDeployment) {
		md.Spec.Roles[0].ExtraArgs = []string{"--max-model-len=32768"}
		md.Spec.Roles[0].Command = []string{"/bin/my-server", "--flag"}
	})

	pod, err := renderModelDeploymentPod(context.Background(), ModelDeploymentRenderInput{
		Deployment:      md,
		Role:            &md.Spec.Roles[0],
		AcceleratorType: newRenderAcceleratorType(),
		Connector: ModelDeploymentConnectorRender{
			Args: []string{`--kv-transfer-config={}`},
			Env:  []core.EnvVar{{Name: "MOONCAKE_CONFIG_PATH", Value: inject.ConfigFilePath}},
			Ports: []core.ContainerPort{
				{Name: "kv-events", ContainerPort: 5557},
				{Name: "kv-replay", ContainerPort: 5558},
			},
			Volumes:      []core.Volume{{Name: inject.ConfigVolumeName}},
			VolumeMounts: []core.VolumeMount{{Name: inject.ConfigVolumeName, MountPath: inject.ConfigMountPath}},
			PodAnnotations: map[string]string{
				inject.ClientConfigAnnotationKey: `{"master_server_address":"master:50051"}`,
			},
		},
	})
	require.NoError(t, err)

	assert.Equal(t, []string{"/bin/my-server", "--flag"}, pod.Spec.Containers[0].Command,
		"a replaced command is used verbatim; the operator appends nothing to it")

	_, ok := envValue(pod, "MOONCAKE_CONFIG_PATH")
	assert.False(t, ok, "the operator's client environment points at a file this argv never reads")

	for _, volume := range pod.Spec.Volumes {
		assert.NotEqual(t, inject.ConfigVolumeName, volume.Name, "the client file is not mounted")
	}
	for _, mount := range pod.Spec.Containers[0].VolumeMounts {
		assert.NotEqual(t, inject.ConfigMountPath, mount.MountPath, "the client file is not mounted")
	}
	for _, port := range pod.Spec.Containers[0].Ports {
		assert.NotContains(t, []int32{5557, 5558}, port.ContainerPort,
			"a take-over role gets no publisher port")
	}

	// THE ANNOTATION IS WITHHELD TOO, and it is the piece most likely to be forgotten: it is not
	// part of the PodSpec, so every assertion above passes while it lands. On a take-over role it
	// would put the pool's address and the whole client document on a Pod the operator did not
	// configure -- a record of a wiring that is not there.
	assert.NotContains(t, pod.Annotations, inject.ClientConfigAnnotationKey,
		"a take-over role gets no part of the connector, including the annotation carrying it")

	// AND NO PROBES, for the same reason. The operator did not write this argv, so it cannot claim
	// the container answers the engine's health route on the Service's port. A gate rendered against
	// a command it did not build would leave a working replica permanently unready, which is worse
	// than the late-Ready the gates exist to fix.
	assert.Nil(t, pod.Spec.Containers[0].StartupProbe,
		"the operator cannot gate a command line it did not build")
	assert.Nil(t, pod.Spec.Containers[0].ReadinessProbe,
		"nor grade its readiness on a route it cannot know the container serves")
}

func TestRenderModelDeploymentPod_KVEventPorts(t *testing.T) {
	md := newRenderDeployment()
	pod, err := renderModelDeploymentPod(context.Background(), ModelDeploymentRenderInput{
		Deployment:      md,
		Role:            &md.Spec.Roles[0],
		AcceleratorType: newRenderAcceleratorType(),
		Connector: ModelDeploymentConnectorRender{Ports: []core.ContainerPort{
			{Name: "kv-events", Protocol: core.ProtocolTCP, ContainerPort: 5557},
			{Name: "kv-replay", Protocol: core.ProtocolTCP, ContainerPort: 5558},
		}},
	})
	require.NoError(t, err)

	assert.Contains(t, pod.Spec.Containers[0].Ports,
		core.ContainerPort{Name: "kv-events", Protocol: core.ProtocolTCP, ContainerPort: 5557})
	assert.Contains(t, pod.Spec.Containers[0].Ports,
		core.ContainerPort{Name: "kv-replay", Protocol: core.ProtocolTCP, ContainerPort: 5558})
}

// TestRenderModelDeploymentPod_ConnectorPortCollision covers the merge between the connector's
// fixed synthesized ports and the ports already on the container: one endpoint declared twice
// renders once, and the same number under a different name is refused.
func TestRenderModelDeploymentPod_ConnectorPortCollision(t *testing.T) {
	connector := ModelDeploymentConnectorRender{Ports: []core.ContainerPort{
		{Name: "kv-events", Protocol: core.ProtocolTCP, ContainerPort: 5557},
		{Name: "kv-replay", Protocol: core.ProtocolTCP, ContainerPort: 5558},
	}}

	t.Run("a port already carrying the same number and name is not duplicated", func(t *testing.T) {
		ports, err := appendModelDeploymentConnectorPorts(
			[]core.ContainerPort{
				{Name: "http", Protocol: core.ProtocolTCP, ContainerPort: 8000},
				{Name: "kv-events", Protocol: core.ProtocolTCP, ContainerPort: 5557},
			}, connector, false,
		)
		require.NoError(t, err)

		var declared int
		for _, p := range ports {
			if p.ContainerPort == 5557 {
				declared++
			}
		}
		assert.Equal(t, 1, declared, "one endpoint declared twice renders once")
		assert.Contains(t, ports,
			core.ContainerPort{Name: "kv-replay", Protocol: core.ProtocolTCP, ContainerPort: 5558})
	})

	t.Run("the same number under a different name is refused", func(t *testing.T) {
		// A template's port name is synthesized from its number, so a user-declared port on the
		// publisher's number always lands here rather than in the dedupe arm above.
		md := newRenderDeployment(func(md *workercore.ModelDeployment) {
			md.Spec.Roles[0].Ports = []workercore.ModelDeploymentPort{
				{Protocol: core.ProtocolTCP, Port: 8000},
				{Protocol: core.ProtocolTCP, Port: 5557},
			}
		})
		_, err := renderModelDeploymentPod(context.Background(), ModelDeploymentRenderInput{
			Deployment: md, Role: &md.Spec.Roles[0], AcceleratorType: newRenderAcceleratorType(),
			Connector: connector,
		})
		require.Error(t, err, "two endpoints claiming one socket cannot be merged silently")
		assert.Contains(t, err.Error(), "5557")
	})

	t.Run("a direct decoder's engine port colliding with a declared port is refused", func(t *testing.T) {
		md := newRenderDeployment(func(md *workercore.ModelDeployment) {
			md.Spec.Router = &workercore.ModelDeploymentRouter{Name: workercore.ModelDeploymentRouterLLMD}
			md.Spec.Roles[0].Kind = workercore.ModelDeploymentRoleKindDecode
			md.Spec.Roles[0].ExtraArgs = []string{"--port", "9100"}
			md.Spec.Roles[0].Ports = []workercore.ModelDeploymentPort{
				{Protocol: core.ProtocolTCP, Port: 8000},
				{Protocol: core.ProtocolTCP, Port: 9100},
			}
		})
		_, err := renderModelDeploymentPod(context.Background(), ModelDeploymentRenderInput{
			Deployment: md, Role: &md.Spec.Roles[0], AcceleratorType: newRenderAcceleratorType(),
			Connector: ModelDeploymentConnectorRender{
				Args: []string{"--kv-transfer-config", `{}`}, KVTransfer: true, RoutingSidecar: true,
			},
		})
		require.Error(t, err, "the engine port must clear every declared port, not only the served one")
		assert.Contains(t, err.Error(), "9100")
	})
}

// TestRenderModelDeploymentPod_TCPPinYieldsToTheRole follows the TCP pin from synthesis onto the
// Pod: the operator's value lands when the role sets none, and a role's own entry -- any value, the
// transfer engine reads it for presence -- stands alone. The first row is the second's baseline,
// so a render that dropped the variable outright fails there rather than passing both.
func TestRenderModelDeploymentPod_TCPPinYieldsToTheRole(t *testing.T) {
	for _, tc := range []struct {
		name    string
		roleEnv []workercore.ModelDeploymentEnvVar
		want    string
	}{
		{name: "the operator's pin lands", want: "1"},
		{
			name:    "the role's own value stands",
			roleEnv: []workercore.ModelDeploymentEnvVar{{Name: "MC_FORCE_TCP", Value: "true"}},
			want:    "true",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			md := newRenderDeployment(func(md *workercore.ModelDeployment) {
				md.Spec.Roles[0].Kind = workercore.ModelDeploymentRoleKindPrefill
				md.Spec.Roles[0].Env = tc.roleEnv
			})
			connector, err := SynthesizeModelDeploymentConnector(ModelDeploymentConnectorInput{
				Engine: md.Spec.Engine.Name, Kind: workercore.ModelDeploymentRoleKindPrefill,
				Disaggregated: true, KVTransfer: true,
			})
			require.NoError(t, err)

			pod, err := renderModelDeploymentPod(context.Background(), ModelDeploymentRenderInput{
				Deployment: md, Role: &md.Spec.Roles[0], AcceleratorType: newRenderAcceleratorType(),
				Connector: connector,
			})
			require.NoError(t, err)

			count := 0
			for _, e := range pod.Spec.Containers[0].Env {
				if e.Name == "MC_FORCE_TCP" {
					count++
					assert.Equal(t, tc.want, e.Value)
				}
			}
			assert.Equal(t, 1, count, "exactly one MC_FORCE_TCP entry reaches the container")
		})
	}
}

// TestRenderModelDeploymentPod_DefaultedArgsYieldToTheRole follows the SGLang defaulted switches
// from synthesis onto the argv. A role naming the same flag, in either spelling, keeps its own
// entry and the operator's whole group is dropped; the rows without one are the baseline that
// shows the group lands exactly once otherwise.
func TestRenderModelDeploymentPod_DefaultedArgsYieldToTheRole(t *testing.T) {
	for _, tc := range []struct {
		name      string
		engine    string
		kind      workercore.ModelDeploymentRoleKind
		store     bool
		extraArgs []string
		flag      string
		want      []string
	}{
		{
			name: "a decode half gets the retraction backup", kind: workercore.ModelDeploymentRoleKindDecode,
			flag: "--disaggregation-decode-retraction-backup",
			want: []string{"--disaggregation-decode-retraction-backup", "cpu_tensor"},
		},
		{
			name: "a decode half's own backup stands", kind: workercore.ModelDeploymentRoleKindDecode,
			extraArgs: []string{"--disaggregation-decode-retraction-backup=host_pool"},
			flag:      "--disaggregation-decode-retraction-backup",
			want:      []string{"--disaggregation-decode-retraction-backup=host_pool"},
		},
		{
			name: "a decode half's own backup stands in two tokens", kind: workercore.ModelDeploymentRoleKindDecode,
			extraArgs: []string{"--disaggregation-decode-retraction-backup", "host_pool"},
			flag:      "--disaggregation-decode-retraction-backup",
			want:      []string{"--disaggregation-decode-retraction-backup", "host_pool"},
		},
		{
			name: "a prefill half with a store gets the hierarchical cache", kind: workercore.ModelDeploymentRoleKindPrefill,
			store: true, flag: "--enable-hierarchical-cache",
			want: []string{"--enable-hierarchical-cache"},
		},
		{
			name: "a prefill half's own hierarchical cache is not repeated", kind: workercore.ModelDeploymentRoleKindPrefill,
			store: true, extraArgs: []string{"--enable-hierarchical-cache"}, flag: "--enable-hierarchical-cache",
			want: []string{"--enable-hierarchical-cache"},
		},
		{
			name: "sglang over tcp gets defaulted page size", kind: workercore.ModelDeploymentRoleKindPrefill,
			store: true, flag: "--page-size",
			want: []string{"--page-size", "64"},
		},
		{
			name: "sglang role's own page size overrides the default", kind: workercore.ModelDeploymentRoleKindPrefill,
			store: true, extraArgs: []string{"--page-size", "16"}, flag: "--page-size",
			want: []string{"--page-size", "16"},
		},
		{
			name: "vllm over tcp gets defaulted block size", engine: workercore.ModelDeploymentEngineVLLM,
			kind: workercore.ModelDeploymentRoleKindPrefill, store: true, flag: "--block-size",
			want: []string{"--block-size", "128"},
		},
		{
			name: "vllm role's own block size overrides the default", engine: workercore.ModelDeploymentEngineVLLM,
			kind: workercore.ModelDeploymentRoleKindPrefill, store: true,
			extraArgs: []string{"--block-size", "64"}, flag: "--block-size",
			want: []string{"--block-size", "64"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			engine := tc.engine
			if engine == "" {
				engine = workercore.ModelDeploymentEngineSGLang
			}
			md := newRenderDeployment(func(md *workercore.ModelDeployment) {
				md.Spec.Engine.Name = engine
				md.Spec.Roles[0].Kind = tc.kind
				md.Spec.Roles[0].ExtraArgs = tc.extraArgs
			})
			in := ModelDeploymentConnectorInput{
				Engine: engine, Kind: tc.kind,
				Disaggregated: true, KVTransfer: true,
			}
			if tc.store {
				in.MasterServerAddress = "shared-kv-master.gpustack-system.svc:50051"
				in.Protocols = []string{"tcp"}
			}
			connector, err := SynthesizeModelDeploymentConnector(in)
			require.NoError(t, err)

			pod, err := renderModelDeploymentPod(context.Background(), ModelDeploymentRenderInput{
				Deployment: md, Role: &md.Spec.Roles[0], AcceleratorType: newRenderAcceleratorType(),
				Connector: connector,
			})
			require.NoError(t, err)

			command := pod.Spec.Containers[0].Command
			var got []string
			for i, arg := range command {
				if ModelDeploymentArgName(arg) != tc.flag {
					continue
				}
				got = append(got, arg)
				if arg == tc.flag && i+1 < len(command) && !strings.HasPrefix(command[i+1], "--") {
					got = append(got, command[i+1])
				}
			}
			assert.Equal(t, tc.want, got, "the argv carries exactly one answer for %s", tc.flag)
		})
	}
}

// TestRenderModelDeploymentPod_Env covers the merge of the two sources: what the operator owns is
// rendered first and cannot be replaced, what it defaults yields to a user's value, and what the
// user appended lands after both.
func TestRenderModelDeploymentPod_Env(t *testing.T) {
	connector := ModelDeploymentConnectorRender{
		Env:          []core.EnvVar{{Name: "MOONCAKE_CONFIG_PATH", Value: inject.ConfigFilePath}},
		DefaultedEnv: []core.EnvVar{{Name: "MC_TE_METRIC", Value: "1"}},
	}

	testCases := []struct {
		name     string
		roleEnv  []workercore.ModelDeploymentEnvVar
		wantEnv  map[string]string
		wantGone []string
	}{
		{
			name:    "nothing supplied — owned and defaulted both render",
			wantEnv: map[string]string{"MOONCAKE_CONFIG_PATH": inject.ConfigFilePath, "MC_TE_METRIC": "1"},
		},
		{
			name:    "a defaulted key yields to the user's value",
			roleEnv: []workercore.ModelDeploymentEnvVar{{Name: "MC_TE_METRIC", Value: "0"}},
			wantEnv: map[string]string{"MC_TE_METRIC": "0"},
		},
		{
			name:    "an owned key supplied anyway never displaces the operator's",
			roleEnv: []workercore.ModelDeploymentEnvVar{{Name: "MOONCAKE_CONFIG_PATH", Value: "/tmp/mine.json"}},
			wantEnv: map[string]string{"MOONCAKE_CONFIG_PATH": inject.ConfigFilePath},
		},
		{
			name:    "an ordinary user variable is passed through untouched",
			roleEnv: []workercore.ModelDeploymentEnvVar{{Name: "VLLM_LOGGING_LEVEL", Value: "DEBUG"}},
			wantEnv: map[string]string{"VLLM_LOGGING_LEVEL": "DEBUG"},
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			md := newRenderDeployment(func(md *workercore.ModelDeployment) {
				md.Spec.Roles[0].Env = tc.roleEnv
			})

			pod, err := renderModelDeploymentPod(context.Background(), ModelDeploymentRenderInput{
				Deployment:      md,
				Role:            &md.Spec.Roles[0],
				AcceleratorType: newRenderAcceleratorType(),
				Connector:       connector,
			})
			require.NoError(t, err)

			for name, want := range tc.wantEnv {
				got, ok := envValue(pod, name)
				if assert.Truef(t, ok, "%s must be rendered", name) {
					assert.Equal(t, want, got, "value of %s", name)
				}
			}

			seen := make(map[string]int)
			for _, e := range pod.Spec.Containers[0].Env {
				seen[e.Name]++
			}
			for name, count := range seen {
				assert.Equalf(t, 1, count,
					"%s rendered %d times; a duplicate leaves no way to tell which value won", name, count)
			}
		})
	}
}

// TestRenderModelDeploymentResources covers explicit host resource rendering, general overcommit,
// and accelerator slicing/partitioning readiness checks.
func TestRenderModelDeploymentResources(t *testing.T) {
	testCases := []struct {
		name string

		resources             *workercore.ModelDeploymentRoleResources
		accType               func(*worker.AcceleratorType)
		cpuOnly               bool
		withGeneralOvercommit bool

		wantLimits   core.ResourceList
		wantRequests core.ResourceList
		wantErr      string
	}{
		{
			name:      "host resources omitted when nil on role",
			resources: &workercore.ModelDeploymentRoleResources{Accelerator: ptr.To(resource.MustParse("2"))},
			wantLimits: core.ResourceList{
				"nvidia.com/gpu": resource.MustParse("2"),
			},
			wantRequests: core.ResourceList{
				"nvidia.com/gpu": resource.MustParse("2"),
			},
		},
		{
			name: "explicit CPU, RAM, and LocalStorage render when set",
			resources: &workercore.ModelDeploymentRoleResources{
				Accelerator:  ptr.To(resource.MustParse("4")),
				CPU:          ptr.To(resource.MustParse("64")),
				RAM:          ptr.To(resource.MustParse("256Gi")),
				LocalStorage: ptr.To(resource.MustParse("15Gi")),
			},
			wantLimits: core.ResourceList{
				"nvidia.com/gpu":              resource.MustParse("4"),
				core.ResourceCPU:              resource.MustParse("64"),
				core.ResourceMemory:           resource.MustParse("256Gi"),
				core.ResourceEphemeralStorage: resource.MustParse("15Gi"),
			},
			wantRequests: core.ResourceList{
				"nvidia.com/gpu":              resource.MustParse("4"),
				core.ResourceCPU:              resource.MustParse("64"),
				core.ResourceMemory:           resource.MustParse("256Gi"),
				core.ResourceEphemeralStorage: resource.MustParse("15Gi"),
			},
		},
		{
			name: "CPU-only role renders only declared host resources",
			resources: &workercore.ModelDeploymentRoleResources{
				CPU: ptr.To(resource.MustParse("8")),
				RAM: ptr.To(resource.MustParse("32Gi")),
			},
			cpuOnly: true,
			wantLimits: core.ResourceList{
				core.ResourceCPU:    resource.MustParse("8"),
				core.ResourceMemory: resource.MustParse("32Gi"),
			},
			wantRequests: core.ResourceList{
				core.ResourceCPU:    resource.MustParse("8"),
				core.ResourceMemory: resource.MustParse("32Gi"),
			},
		},
		{
			name: "general overcommit scales host requests on accelerated role",
			resources: &workercore.ModelDeploymentRoleResources{
				Accelerator:  ptr.To(resource.MustParse("1")),
				CPU:          ptr.To(resource.MustParse("16")),
				RAM:          ptr.To(resource.MustParse("64Gi")),
				LocalStorage: ptr.To(resource.MustParse("15Gi")),
			},
			withGeneralOvercommit: true,
			wantLimits: core.ResourceList{
				"nvidia.com/gpu":              resource.MustParse("1"),
				core.ResourceCPU:              resource.MustParse("16"),
				core.ResourceMemory:           resource.MustParse("64Gi"),
				core.ResourceEphemeralStorage: resource.MustParse("15Gi"),
			},
			wantRequests: core.ResourceList{
				"nvidia.com/gpu":              resource.MustParse("1"),
				core.ResourceCPU:              resource.MustParse("1600m"),
				core.ResourceMemory:           resource.MustParse("8Gi"),
				core.ResourceEphemeralStorage: resource.MustParse("1920Mi"),
			},
		},
		{
			name: "a memory slice copies across to cores when cores percentage is unset",
			resources: &workercore.ModelDeploymentRoleResources{
				Accelerator:                       ptr.To(resource.MustParse("1")),
				AcceleratorSlicedMemoryPercentage: 50,
			},
			accType: func(at *worker.AcceleratorType) { at.Status.Detail.SlicedDetail.Logical.Count = 128 },
			wantLimits: core.ResourceList{
				"nvidia.com/gpu.sliced":                   resource.MustParse("1"),
				"nvidia.com/gpu.sliced.memory-percentage": resource.MustParse("50"),
				"nvidia.com/gpu.sliced.cores-percentage":  resource.MustParse("50"),
			},
			wantRequests: core.ResourceList{
				"nvidia.com/gpu.sliced":                   resource.MustParse("1"),
				"nvidia.com/gpu.sliced.memory-percentage": resource.MustParse("50"),
				"nvidia.com/gpu.sliced.cores-percentage":  resource.MustParse("50"),
			},
		},
		{
			name: "a bare compute slice copies across to memory when memory percentage is unset",
			resources: &workercore.ModelDeploymentRoleResources{
				Accelerator:                      ptr.To(resource.MustParse("1")),
				AcceleratorSlicedCoresPercentage: 25,
			},
			accType: func(at *worker.AcceleratorType) { at.Status.Detail.SlicedDetail.Logical.Count = 128 },
			wantLimits: core.ResourceList{
				"nvidia.com/gpu.sliced":                   resource.MustParse("1"),
				"nvidia.com/gpu.sliced.memory-percentage": resource.MustParse("25"),
				"nvidia.com/gpu.sliced.cores-percentage":  resource.MustParse("25"),
			},
			wantRequests: core.ResourceList{
				"nvidia.com/gpu.sliced":                   resource.MustParse("1"),
				"nvidia.com/gpu.sliced.memory-percentage": resource.MustParse("25"),
				"nvidia.com/gpu.sliced.cores-percentage":  resource.MustParse("25"),
			},
		},
		{
			name: "a partition profile renders when the detail can size it",
			resources: &workercore.ModelDeploymentRoleResources{
				Accelerator:                   ptr.To(resource.MustParse("1")),
				AcceleratorPartitionedProfile: "3g.40gb",
			},
			accType: func(at *worker.AcceleratorType) {
				at.Status.Detail.Memory = "80Gi"
				at.Status.Detail.SlicedDetail.Physical.Profiles = []workercore.AcceleratorSlicedPhysicalDetailProfile{
					{Name: "3g.40gb", MemoryMib: 40 << 10, Count: 2},
				}
			},
			wantLimits: core.ResourceList{
				"nvidia.com/gpu.partitioned":             resource.MustParse("1"),
				"nvidia.com/gpu.partitioned.mig-3g.40gb": resource.MustParse("1"),
			},
			wantRequests: core.ResourceList{
				"nvidia.com/gpu.partitioned":             resource.MustParse("1"),
				"nvidia.com/gpu.partitioned.mig-3g.40gb": resource.MustParse("1"),
			},
		},
		{
			name: "a slice against an uncomputed detail is retryable, never a whole card",
			resources: &workercore.ModelDeploymentRoleResources{
				Accelerator:                       ptr.To(resource.MustParse("1")),
				AcceleratorSlicedMemoryPercentage: 50,
			},
			accType: func(at *worker.AcceleratorType) { at.Status.Detail.Manufacturer = "" },
			wantErr: "not ready yet",
		},
		{
			name: "a whole-card request against an uncomputed manufacturer is retryable, never an empty resource key",
			resources: &workercore.ModelDeploymentRoleResources{
				Accelerator: ptr.To(resource.MustParse("1")),
			},
			accType: func(at *worker.AcceleratorType) { at.Status.Detail.Manufacturer = "" },
			wantErr: "yields no acceleratable resource name",
		},
		{
			name: "a slice against an unknown manufacturer is refused before emitting malformed sliced keys",
			resources: &workercore.ModelDeploymentRoleResources{
				Accelerator:                       ptr.To(resource.MustParse("1")),
				AcceleratorSlicedMemoryPercentage: 50,
			},
			accType: func(at *worker.AcceleratorType) {
				at.Status.Detail.Manufacturer = "unknown-vendor"
				at.Status.Detail.Memory = "80Gi"
				at.Status.Detail.SlicedDetail.Logical.Count = 4
			},
			wantErr: "yields no acceleratable resource name",
		},
		{
			name: "a partition profile on a manufacturer without hardware partitioning is refused rather than falling through to a whole card",
			resources: &workercore.ModelDeploymentRoleResources{
				Accelerator:                   ptr.To(resource.MustParse("1")),
				AcceleratorPartitionedProfile: "3g.40gb",
			},
			accType: func(at *worker.AcceleratorType) {
				at.Status.Detail.Manufacturer = nodefeature.ManufacturerAMD
				at.Status.Detail.Memory = "64Gi"
				at.Status.Detail.SlicedDetail.Physical.Profiles = []workercore.AcceleratorSlicedPhysicalDetailProfile{
					{Name: "3g.40gb", MemoryMib: 32 << 10, Count: 2},
				}
			},
			wantErr: "yields no partitioned resource name",
		},
		{
			name: "a partition the detail cannot size yet is retryable too",
			resources: &workercore.ModelDeploymentRoleResources{
				Accelerator:                   ptr.To(resource.MustParse("1")),
				AcceleratorPartitionedProfile: "3g.40gb",
			},
			accType: func(at *worker.AcceleratorType) {
				at.Status.Detail.SlicedDetail.Physical.Profiles = []workercore.AcceleratorSlicedPhysicalDetailProfile{
					{Name: "3g.40gb", MemoryMib: 0},
				}
			},
			wantErr: "cannot be sized",
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			var at *worker.AcceleratorType
			if !tc.cpuOnly {
				at = newRenderAcceleratorType()
				if tc.accType != nil {
					tc.accType(at)
				}
			}
			role := &workercore.ModelDeploymentRole{Name: "server", Resources: tc.resources}

			reqs, _, err := renderModelDeploymentResources(role, at, tc.withGeneralOvercommit)
			if tc.wantErr != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tc.wantErr)

				return
			}

			require.NoError(t, err)
			assert.Equal(t, len(tc.wantLimits), len(reqs.Limits))
			for k, want := range tc.wantLimits {
				got, ok := reqs.Limits[k]
				require.True(t, ok, "missing limit %s", k)
				qtyEqual(t, want, got, "limit "+string(k))
			}
			assert.Equal(t, len(tc.wantRequests), len(reqs.Requests))
			for k, want := range tc.wantRequests {
				got, ok := reqs.Requests[k]
				require.True(t, ok, "missing request %s", k)
				qtyEqual(t, want, got, "request "+string(k))
			}
		})
	}
}

// TestRenderModelDeploymentPod_AcceleratorRequest checks that declared host and accelerator
// resources reach the container and omitted host resources stay unset.
func TestRenderModelDeploymentPod_AcceleratorRequest(t *testing.T) {
	md := newRenderDeployment(func(md *workercore.ModelDeployment) {
		md.Spec.Roles[0].Resources = &workercore.ModelDeploymentRoleResources{
			Accelerator: ptr.To(resource.MustParse("2")),
			CPU:         ptr.To(resource.MustParse("32")),
			RAM:         ptr.To(resource.MustParse("128Gi")),
		}
	})

	pod := renderOne(t, md, newRenderAcceleratorType())

	accName := nodefeature.GetAcceleratableResourceName(
		nodefeature.ManufacturerNVIDIA, workercore.DeviceAllocationModeExclusive,
	)
	limits := pod.Spec.Containers[0].Resources.Limits
	qtyEqual(t, qty("2"), limits[accName], "the declared card count reaches the container")
	qtyEqual(t, qty("32"), limits[core.ResourceCPU], "the declared host CPU reaches the container")
	qtyEqual(t, qty("128Gi"), limits[core.ResourceMemory], "the declared host memory reaches the container")
	assert.NotContains(t, limits, core.ResourceEphemeralStorage, "unset LocalStorage is omitted")
}

func TestRenderModelDeploymentPod_InterfaceRequest(t *testing.T) {
	tests := []struct {
		name     string
		count    string
		resource core.ResourceName
	}{
		{name: "absent"},
		{name: "one RDMA interface", count: "1", resource: "device.gpustack.ai/rdma.shared"},
		{name: "two RDMA interfaces", count: "2", resource: "device.gpustack.ai/rdma"},
		{name: "two EFA interfaces", count: "2", resource: "vpc.amazonaws.com/efa"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			md := newRenderDeployment()
			if tc.count != "" {
				md.Spec.Roles[0].Resources = &workercore.ModelDeploymentRoleResources{
					Interface: ptr.To(resource.MustParse(tc.count)),
				}
			}
			pod, err := renderModelDeploymentPod(context.Background(), ModelDeploymentRenderInput{
				Deployment: md, Role: &md.Spec.Roles[0], AcceleratorType: newRenderAcceleratorType(),
				InterfaceResource: tc.resource,
			})
			require.NoError(t, err)
			limits := pod.Spec.Containers[0].Resources.Limits
			if tc.resource == "" {
				assert.NotContains(t, limits, core.ResourceName("device.gpustack.ai/rdma.shared"))
				assert.NotContains(t, limits, core.ResourceName("device.gpustack.ai/rdma"))
				assert.NotContains(t, limits, core.ResourceName("vpc.amazonaws.com/efa"))
				return
			}
			qtyEqual(t, qty(tc.count), limits[tc.resource], "the requested interface count reaches the engine")
		})
	}
}

func TestModelDeploymentInterfaceResource(t *testing.T) {
	tests := []struct {
		name    string
		store   []string
		direct  string
		count   int64
		want    core.ResourceName
		wantErr string
	}{
		{name: "store RDMA shared", store: []string{"rdma"}, count: 1, want: "device.gpustack.ai/rdma.shared"},
		{name: "store RDMA exclusive", store: []string{"rdma"}, count: 2, want: "device.gpustack.ai/rdma"},
		{name: "pure direct EFA", direct: "efa", count: 2, want: "vpc.amazonaws.com/efa"},
		{name: "same fabric once", store: []string{"efa"}, direct: "efa", count: 1, want: "vpc.amazonaws.com/efa"},
		{name: "TCP group with direct fabric", store: []string{"tcp"}, direct: "rdma", count: 1, want: "device.gpustack.ai/rdma.shared"},
		{name: "mixed groups", store: []string{"tcp", "rdma"}, direct: "rdma", count: 1, wantErr: "tcp, rdma"},
		{name: "mixed legs", store: []string{"rdma"}, direct: "efa", count: 1, wantErr: "rdma and efa"},
		{name: "no fabric", store: []string{"tcp"}, direct: "tcp", count: 1, wantErr: "no effective RDMA or EFA"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ModelDeploymentInterfaceResource(tc.store, tc.direct, tc.count)
			if tc.wantErr != "" {
				require.ErrorContains(t, err, tc.wantErr)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}
}

func TestModelDeploymentDirectInterfaceProtocol(t *testing.T) {
	tests := []struct {
		name            string
		engine          string
		manufacturer    string
		command         []string
		serverSize      int32
		elasticWidth    int32
		awsEFAEnabled   bool
		omitDecodeIface bool
		includeEncode   bool
		want            string
	}{
		{name: "native vLLM with EFA", engine: workercore.ModelDeploymentEngineVLLM, manufacturer: nodefeature.ManufacturerNVIDIA, awsEFAEnabled: true, want: "efa"},
		{name: "native vLLM defaults to RDMA", engine: workercore.ModelDeploymentEngineVLLM, manufacturer: nodefeature.ManufacturerNVIDIA, want: "rdma"},
		{name: "Ascend ignores direct interface", engine: workercore.ModelDeploymentEngineVLLM, manufacturer: nodefeature.ManufacturerAscend, awsEFAEnabled: true},
		{name: "unobserved manufacturer cannot select device", engine: workercore.ModelDeploymentEngineVLLM, awsEFAEnabled: true},
		{name: "SGLang pair resolves EFA", engine: workercore.ModelDeploymentEngineSGLang, manufacturer: nodefeature.ManufacturerNVIDIA, awsEFAEnabled: true, want: "efa"},
		{name: "one half without interface disables pair direct fabric", engine: workercore.ModelDeploymentEngineVLLM, manufacturer: nodefeature.ManufacturerNVIDIA, omitDecodeIface: true},
		{name: "encode without interface does not block P/D pair direct fabric", engine: workercore.ModelDeploymentEngineVLLM, manufacturer: nodefeature.ManufacturerNVIDIA, includeEncode: true, want: "rdma"},
		{name: "user command owns transfer", engine: workercore.ModelDeploymentEngineVLLM, manufacturer: nodefeature.ManufacturerNVIDIA, command: []string{"custom"}},
		{name: "multi-member Server role selects RDMA", engine: workercore.ModelDeploymentEngineVLLM, manufacturer: nodefeature.ManufacturerNVIDIA, serverSize: 2, want: "rdma"},
		{name: "multi-member Server role selects EFA when enabled", engine: workercore.ModelDeploymentEngineVLLM, manufacturer: nodefeature.ManufacturerNVIDIA, serverSize: 2, awsEFAEnabled: true, want: "efa"},
		{name: "elastic EP Server role selects RDMA", engine: workercore.ModelDeploymentEngineVLLM, manufacturer: nodefeature.ManufacturerNVIDIA, serverSize: 1, elasticWidth: 2, want: "rdma"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			md := newRenderDeployment(func(md *workercore.ModelDeployment) {
				md.Spec.Engine.Name = tc.engine
				md.Spec.KVCache = nil
				if tc.serverSize > 0 {
					md.Spec.Roles[0].Kind = workercore.ModelDeploymentRoleKindServer
					md.Spec.Roles[0].ReplicaSize = tc.serverSize
					if tc.elasticWidth > 0 {
						runtimecontrol.SetElasticWidth(&md.Spec.Roles[0], tc.elasticWidth)
					}
					md.Spec.Roles[0].Command = tc.command
					return
				}
				md.Spec.Router = &workercore.ModelDeploymentRouter{Name: workercore.ModelDeploymentRouterLLMD}
				md.Spec.Roles[0].Kind = workercore.ModelDeploymentRoleKindPrefill
				md.Spec.Roles[0].Command = tc.command
				md.Spec.Roles[0].Resources = &workercore.ModelDeploymentRoleResources{
					Interface: ptr.To(resource.MustParse("1")),
				}
				decodeRole := workercore.ModelDeploymentRole{
					Name: "decode",
					Kind: workercore.ModelDeploymentRoleKindDecode,
				}
				if !tc.omitDecodeIface {
					decodeRole.Resources = &workercore.ModelDeploymentRoleResources{
						Interface: ptr.To(resource.MustParse("1")),
					}
				}
				md.Spec.Roles = append(md.Spec.Roles, decodeRole)
				if tc.includeEncode {
					md.Spec.Roles = append(md.Spec.Roles, workercore.ModelDeploymentRole{
						Name: "encode",
						Kind: workercore.ModelDeploymentRoleKindEncode,
					})
				}
			})
			assert.Equal(t, tc.want, ModelDeploymentDirectInterfaceProtocol(
				md, &md.Spec.Roles[0], tc.manufacturer, "", false, tc.awsEFAEnabled,
			))
		})
	}
}

func TestModelDeploymentDirectInterfaceProtocol_AscendA5(t *testing.T) {
	qty1 := resource.MustParse("1")
	routedPair := func(engine, kvBackend string, prefillIface, decodeIface, withKVCache, withEncode bool) *workercore.ModelDeployment {
		return newRenderDeployment(func(md *workercore.ModelDeployment) {
			md.Spec.Engine.Name = engine
			if !withKVCache {
				md.Spec.KVCache = nil
			}
			md.Spec.Router = &workercore.ModelDeploymentRouter{Name: workercore.ModelDeploymentRouterLLMD}
			if kvBackend != "" {
				md.Spec.KVTransfer = &workercore.ModelDeploymentKVTransfer{Backend: kvBackend}
			}
			md.Spec.Roles = []workercore.ModelDeploymentRole{
				{
					Name: "prefill",
					Kind: workercore.ModelDeploymentRoleKindPrefill,
				},
				{
					Name: "decode",
					Kind: workercore.ModelDeploymentRoleKindDecode,
				},
			}
			if prefillIface {
				md.Spec.Roles[0].Resources = &workercore.ModelDeploymentRoleResources{Interface: ptr.To(qty1)}
			}
			if decodeIface {
				md.Spec.Roles[1].Resources = &workercore.ModelDeploymentRoleResources{Interface: ptr.To(qty1)}
			}
			if withEncode {
				md.Spec.Roles = append(md.Spec.Roles, workercore.ModelDeploymentRole{
					Name:      "encode",
					Kind:      workercore.ModelDeploymentRoleKindEncode,
					Resources: &workercore.ModelDeploymentRoleResources{Interface: ptr.To(qty1)},
				})
			}
		})
	}

	cases := []struct {
		name           string
		md             *workercore.ModelDeployment
		roleIndex      int
		family         string
		remoteDelivery bool
		awsEFAEnabled  bool
		want           string
	}{
		{
			name:   "A5 vLLM routed P/D default HIXL grants rdma",
			md:     routedPair(workercore.ModelDeploymentEngineVLLM, "", true, true, false, false),
			family: nodefeature.AcceleratorFamilyAscend950,
			want:   "rdma",
		},
		{
			name:          "A5 vLLM routed P/D explicit HIXL grants rdma even when awsEFAEnabled is true",
			md:            routedPair(workercore.ModelDeploymentEngineVLLM, workercore.ModelDeploymentKVTransferBackendHIXL, true, true, false, false),
			family:        nodefeature.AcceleratorFamilyAscend950,
			awsEFAEnabled: true,
			want:          "rdma",
		},
		{
			name:          "A5 vLLM routed P/D explicit Mooncake grants rdma even when awsEFAEnabled is true",
			md:            routedPair(workercore.ModelDeploymentEngineVLLM, workercore.ModelDeploymentKVTransferBackendMooncake, true, true, true, false),
			family:        nodefeature.AcceleratorFamilyAscend950,
			awsEFAEnabled: true,
			want:          "rdma",
		},
		{
			name:      "A5 vLLM routed P/D decode half also grants rdma",
			md:        routedPair(workercore.ModelDeploymentEngineVLLM, workercore.ModelDeploymentKVTransferBackendHIXL, true, true, true, true),
			roleIndex: 1,
			family:    nodefeature.AcceleratorFamilyAscend950,
			want:      "rdma",
		},
		{
			name:      "A5 vLLM Encode role returns empty even with KVCache and P/D siblings",
			md:        routedPair(workercore.ModelDeploymentEngineVLLM, workercore.ModelDeploymentKVTransferBackendHIXL, true, true, true, true),
			roleIndex: 2,
			family:    nodefeature.AcceleratorFamilyAscend950,
			want:      "",
		},
		{
			name:   "A5 vLLM asymmetric P/D without KVCache rejected",
			md:     routedPair(workercore.ModelDeploymentEngineVLLM, workercore.ModelDeploymentKVTransferBackendHIXL, true, false, false, false),
			family: nodefeature.AcceleratorFamilyAscend950,
			want:   "",
		},
		{
			name:   "A5 vLLM asymmetric P/D with KVCache still rejected",
			md:     routedPair(workercore.ModelDeploymentEngineVLLM, workercore.ModelDeploymentKVTransferBackendHIXL, true, false, true, false),
			family: nodefeature.AcceleratorFamilyAscend950,
			want:   "",
		},
		{
			name: "A5 vLLM unrouted pair with KVCache returns empty",
			md: func() *workercore.ModelDeployment {
				md := routedPair(workercore.ModelDeploymentEngineVLLM, "", true, true, true, false)
				md.Spec.Router = nil
				return md
			}(),
			family: nodefeature.AcceleratorFamilyAscend950,
			want:   "",
		},
		{
			name: "A5 vLLM lone Prefill with KVCache grants rdma",
			md: newRenderDeployment(func(md *workercore.ModelDeployment) {
				md.Spec.Roles[0].Kind = workercore.ModelDeploymentRoleKindPrefill
				md.Spec.Roles[0].Resources = &workercore.ModelDeploymentRoleResources{Interface: ptr.To(qty1)}
			}),
			family: nodefeature.AcceleratorFamilyAscend950,
			want:   "rdma",
		},
		{
			name: "A5 vLLM lone Decode without KVCache returns empty",
			md: newRenderDeployment(func(md *workercore.ModelDeployment) {
				md.Spec.KVCache = nil
				md.Spec.Roles[0].Kind = workercore.ModelDeploymentRoleKindDecode
				md.Spec.Roles[0].Resources = &workercore.ModelDeploymentRoleResources{Interface: ptr.To(qty1)}
			}),
			family: nodefeature.AcceleratorFamilyAscend950,
			want:   "",
		},
		{
			name: "A5 vLLM single-Pod Server with KVCache grants rdma even when awsEFAEnabled is true",
			md: newRenderDeployment(func(md *workercore.ModelDeployment) {
				md.Spec.Roles[0].Kind = workercore.ModelDeploymentRoleKindServer
				md.Spec.Roles[0].ReplicaSize = 1
				md.Spec.Roles[0].Resources = &workercore.ModelDeploymentRoleResources{Interface: ptr.To(qty1)}
			}),
			family:        nodefeature.AcceleratorFamilyAscend950,
			awsEFAEnabled: true,
			want:          "rdma",
		},
		{
			name: "A5 vLLM single-Pod Server without KVCache grants rdma on remote delivery even when awsEFAEnabled is true",
			md: newRenderDeployment(func(md *workercore.ModelDeployment) {
				md.Spec.KVCache = nil
				md.Spec.Roles[0].Kind = workercore.ModelDeploymentRoleKindServer
				md.Spec.Roles[0].ReplicaSize = 1
				md.Spec.Roles[0].Resources = &workercore.ModelDeploymentRoleResources{Interface: ptr.To(qty1)}
			}),
			family:         nodefeature.AcceleratorFamilyAscend950,
			remoteDelivery: true,
			awsEFAEnabled:  true,
			want:           "rdma",
		},
		{
			name: "A5 vLLM single-Pod Server without KVCache returns empty without remote delivery",
			md: newRenderDeployment(func(md *workercore.ModelDeployment) {
				md.Spec.KVCache = nil
				md.Spec.Roles[0].Kind = workercore.ModelDeploymentRoleKindServer
				md.Spec.Roles[0].ReplicaSize = 1
				md.Spec.Roles[0].Resources = &workercore.ModelDeploymentRoleResources{Interface: ptr.To(qty1)}
			}),
			family:        nodefeature.AcceleratorFamilyAscend950,
			awsEFAEnabled: true,
			want:          "",
		},
		{
			name: "A5 vLLM single-Pod Server with conflicting load-format returns empty on remote delivery",
			md: newRenderDeployment(func(md *workercore.ModelDeployment) {
				md.Spec.KVCache = nil
				md.Spec.Roles[0].Kind = workercore.ModelDeploymentRoleKindServer
				md.Spec.Roles[0].ReplicaSize = 1
				md.Spec.Roles[0].ExtraArgs = []string{"--load-format=safetensors"}
				md.Spec.Roles[0].Resources = &workercore.ModelDeploymentRoleResources{Interface: ptr.To(qty1)}
			}),
			family:         nodefeature.AcceleratorFamilyAscend950,
			remoteDelivery: true,
			want:           "",
		},
		{
			name: "A5 SGLang single-Pod Server grants rdma on remote delivery even when awsEFAEnabled is true",
			md: newRenderDeployment(func(md *workercore.ModelDeployment) {
				md.Spec.Engine.Name = workercore.ModelDeploymentEngineSGLang
				md.Spec.KVCache = nil
				md.Spec.Roles[0].Kind = workercore.ModelDeploymentRoleKindServer
				md.Spec.Roles[0].ReplicaSize = 1
				md.Spec.Roles[0].Resources = &workercore.ModelDeploymentRoleResources{Interface: ptr.To(qty1)}
			}),
			family:         nodefeature.AcceleratorFamilyAscend950,
			remoteDelivery: true,
			awsEFAEnabled:  true,
			want:           "rdma",
		},
		{
			name:           "A5 SGLang routed symmetric P/D grants rdma on remote delivery",
			md:             routedPair(workercore.ModelDeploymentEngineSGLang, workercore.ModelDeploymentKVTransferBackendHIXL, true, true, true, false),
			family:         nodefeature.AcceleratorFamilyAscend950,
			remoteDelivery: true,
			awsEFAEnabled:  true,
			want:           "rdma",
		},
		{
			name:           "A5 vLLM asymmetric P/D returns empty even on remote delivery",
			md:             routedPair(workercore.ModelDeploymentEngineVLLM, workercore.ModelDeploymentKVTransferBackendHIXL, true, false, false, false),
			family:         nodefeature.AcceleratorFamilyAscend950,
			remoteDelivery: true,
			want:           "",
		},
		{
			name:           "A5 SGLang asymmetric P/D returns empty even on remote delivery",
			md:             routedPair(workercore.ModelDeploymentEngineSGLang, workercore.ModelDeploymentKVTransferBackendHIXL, true, false, false, false),
			family:         nodefeature.AcceleratorFamilyAscend950,
			remoteDelivery: true,
			want:           "",
		},
		{
			name: "A5 vLLM Encode role returns empty even on remote delivery",
			md: func() *workercore.ModelDeployment {
				md := routedPair(workercore.ModelDeploymentEngineVLLM, workercore.ModelDeploymentKVTransferBackendHIXL, true, true, true, true)
				md.Spec.Roles = append([]workercore.ModelDeploymentRole{{
					Name: "encode", Kind: workercore.ModelDeploymentRoleKindEncode,
					Resources: &workercore.ModelDeploymentRoleResources{Interface: ptr.To(qty1)},
				}}, md.Spec.Roles...)
				return md
			}(),
			family:         nodefeature.AcceleratorFamilyAscend950,
			remoteDelivery: true,
			want:           "",
		},
		{
			name:           "A2 910B Server returns empty even on remote delivery",
			md:             newRenderDeployment(),
			family:         "910B",
			remoteDelivery: true,
			want:           "",
		},
		{
			name:           "A3 910C routed P/D returns empty even on remote delivery",
			md:             routedPair(workercore.ModelDeploymentEngineVLLM, workercore.ModelDeploymentKVTransferBackendHIXL, true, true, true, false),
			family:         "910C",
			remoteDelivery: true,
			want:           "",
		},
		{
			name: "A5 vLLM multi-Pod Server without KVCache grants rdma",
			md: newRenderDeployment(func(md *workercore.ModelDeployment) {
				md.Spec.KVCache = nil
				md.Spec.Roles[0].Kind = workercore.ModelDeploymentRoleKindServer
				md.Spec.Roles[0].ReplicaSize = 2
				md.Spec.Roles[0].Resources = &workercore.ModelDeploymentRoleResources{Interface: ptr.To(qty1)}
			}),
			family:        nodefeature.AcceleratorFamilyAscend950,
			awsEFAEnabled: true,
			want:          "rdma",
		},
		{
			name: "A5 vLLM elastic-EP Server without KVCache grants rdma",
			md: newRenderDeployment(func(md *workercore.ModelDeployment) {
				md.Spec.KVCache = nil
				md.Spec.Roles[0].Kind = workercore.ModelDeploymentRoleKindServer
				md.Spec.Roles[0].ReplicaSize = 1
				runtimecontrol.SetElasticWidth(&md.Spec.Roles[0], 2)
				md.Spec.Roles[0].Resources = &workercore.ModelDeploymentRoleResources{Interface: ptr.To(qty1)}
			}),
			family:        nodefeature.AcceleratorFamilyAscend950,
			awsEFAEnabled: true,
			want:          "rdma",
		},
		{
			name: "A5 vLLM command take-over returns empty",
			md: func() *workercore.ModelDeployment {
				md := routedPair(workercore.ModelDeploymentEngineVLLM, workercore.ModelDeploymentKVTransferBackendHIXL, true, true, true, false)
				md.Spec.Roles[0].Command = []string{"vllm", "serve", "m"}
				return md
			}(),
			family: nodefeature.AcceleratorFamilyAscend950,
			want:   "",
		},
		{
			name:   "A5 SGLang routed P/D returns empty",
			md:     routedPair(workercore.ModelDeploymentEngineSGLang, workercore.ModelDeploymentKVTransferBackendHIXL, true, true, true, false),
			family: nodefeature.AcceleratorFamilyAscend950,
			want:   "",
		},
		{
			name: "A5 SGLang single-Pod Server with KVCache returns empty",
			md: newRenderDeployment(func(md *workercore.ModelDeployment) {
				md.Spec.Engine.Name = workercore.ModelDeploymentEngineSGLang
				md.Spec.Roles[0].Kind = workercore.ModelDeploymentRoleKindServer
				md.Spec.Roles[0].Resources = &workercore.ModelDeploymentRoleResources{Interface: ptr.To(qty1)}
			}),
			family: nodefeature.AcceleratorFamilyAscend950,
			want:   "",
		},
		{
			name:   "A2 910B routed P/D returns empty",
			md:     routedPair(workercore.ModelDeploymentEngineVLLM, workercore.ModelDeploymentKVTransferBackendHIXL, true, true, true, false),
			family: "910B",
			want:   "",
		},
		{
			name:   "A3 910C Server with KVCache returns empty",
			md:     newRenderDeployment(),
			family: "910C",
			want:   "",
		},
		{
			name:   "unconverged empty family Ascend returns empty",
			md:     routedPair(workercore.ModelDeploymentEngineVLLM, workercore.ModelDeploymentKVTransferBackendHIXL, true, true, true, false),
			family: "",
			want:   "",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ModelDeploymentDirectInterfaceProtocol(
				tc.md, &tc.md.Spec.Roles[tc.roleIndex], nodefeature.ManufacturerAscend, tc.family,
				tc.remoteDelivery, tc.awsEFAEnabled,
			)
			assert.Equal(t, tc.want, got)
		})
	}
}

// TestRenderModelDeploymentPod_Ports pins that a replica is always reachable: a role naming no port
// still gets one, because the Service fronting the deployment needs a target.
func TestRenderModelDeploymentPod_Ports(t *testing.T) {
	md := newRenderDeployment()
	pod := renderOne(t, md, newRenderAcceleratorType())
	require.Len(t, pod.Spec.Containers[0].Ports, 1)
	assert.Equal(t, int32(8000), pod.Spec.Containers[0].Ports[0].ContainerPort)
	assert.Equal(t, "http", pod.Spec.Containers[0].Ports[0].Name)

	md = newRenderDeployment(func(md *workercore.ModelDeployment) {
		md.Spec.Roles[0].Ports = []workercore.ModelDeploymentPort{
			{Port: 9000, Protocol: core.ProtocolTCP},
		}
	})
	pod = renderOne(t, md, newRenderAcceleratorType())
	require.Len(t, pod.Spec.Containers[0].Ports, 1)
	assert.Equal(t, int32(9000), pod.Spec.Containers[0].Ports[0].ContainerPort)
}

func TestRenderModelDeploymentPod_MetricsScrape(t *testing.T) {
	for _, tc := range []struct {
		name, scheme string
		port         int32
		tls          bool
	}{
		{name: "default", port: 8000, scheme: "http"},
		{name: "custom", port: 9000, scheme: "http"},
		{name: "TLS", port: 8000, scheme: "https", tls: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			md := newRenderDeployment()
			if tc.tls {
				md.Spec.Roles[0].ExtraArgs = []string{"--ssl-certfile", "/etc/tls/tls.crt"}
			}
			if tc.port != 8000 {
				md.Spec.Roles[0].Ports = []workercore.ModelDeploymentPort{{Port: tc.port}}
			}
			pod := renderOne(t, md, newRenderAcceleratorType())
			assert.Equal(t, "true", pod.Annotations["prometheus.io/scrape"])
			assert.Equal(t, "/metrics", pod.Annotations["prometheus.io/path"])
			assert.Equal(t, strconv.Itoa(int(tc.port)), pod.Annotations["prometheus.io/port"])
			assert.Equal(t, tc.scheme, pod.Annotations["prometheus.io/scheme"])
			assert.Equal(t, tc.port, pod.Spec.Containers[0].Ports[0].ContainerPort)
		})
	}
}

// TestRenderModelDeploymentPod_ResolvesTheImageAtTwoLevels pins the rule that replaced synthesis: a
// role's own image wins, a role naming none takes the engine's, and a pair naming none at all is
// refused.
//
// THE LAST CASE IS UNREACHABLE THROUGH ADMISSION, because the schema requires spec.engine.image. It
// stays because the renderer must not depend on that: if the obligation ever moves, the failure has
// to be a loud error rather than a Pod with an empty image that the kubelet rejects minutes later.
func TestRenderModelDeploymentPod_ResolvesTheImageAtTwoLevels(t *testing.T) {
	for _, tc := range []struct {
		name    string
		engine  string
		role    string
		want    string
		wantErr string
	}{
		{
			name:   "the role's own image wins over the engine's",
			engine: "vllm/vllm-openai:v0.25.1", role: "registry.example/private:v0.25.1",
			want: "registry.example/private:v0.25.1",
		},
		{
			name:   "a role naming none runs the engine's image",
			engine: "vllm/vllm-openai:v0.25.1",
			want:   "vllm/vllm-openai:v0.25.1",
		},
		{
			name:    "neither naming one is refused rather than rendered empty",
			wantErr: `role "server" names no image`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			md := newRenderDeployment(func(md *workercore.ModelDeployment) {
				md.Spec.Engine.Image = tc.engine
				md.Spec.Roles[0].Image = tc.role
			})

			pod, err := renderModelDeploymentPod(context.Background(), ModelDeploymentRenderInput{
				Deployment:      md,
				Role:            &md.Spec.Roles[0],
				AcceleratorType: newRenderAcceleratorType(),
			})
			if tc.wantErr != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tc.wantErr)
				assert.Nil(t, pod, "no Pod may be rendered with an empty image")
				return
			}

			require.NoError(t, err)
			assert.Equal(t, tc.want, pod.Spec.Containers[0].Image)
		})
	}
}

// TestRenderModelDeploymentPod_ClientConfigMount pins where the rendered client configuration is
// mounted, because the owned MOONCAKE_CONFIG_PATH variable is the only pointer to it: a mount path
// that drifted from the constant the connector renders would leave the engine reading nothing.
//
// THE CONNECTOR COMES FROM SYNTHESIS RATHER THAN A LITERAL, and that is the point of this test now.
// What it pins is that the three pieces ARRIVE TOGETHER: the annotation holding the document, the
// projection whose fieldRef reads that annotation, and the mount. A literal here would satisfy every
// assertion below while synthesis produced a projection naming an annotation nobody sets -- which
// mounts an EMPTY FILE and starts an engine that uses no cache, with every symptom inside the
// container.
func TestRenderModelDeploymentPod_ClientConfigMount(t *testing.T) {
	md := newRenderDeployment()
	conn, err := SynthesizeModelDeploymentConnector(
		connectorInput(workercore.ModelDeploymentEngineVLLM, nodefeature.ManufacturerNVIDIA),
	)
	require.NoError(t, err)

	pod, err := renderModelDeploymentPod(context.Background(), ModelDeploymentRenderInput{
		Deployment:      md,
		Role:            &md.Spec.Roles[0],
		AcceleratorType: newRenderAcceleratorType(),
		Connector:       conn,
	})
	require.NoError(t, err)

	// Four volumes: the client configuration projection below, the compilation cache, shared
	// memory, and the default read-only gpustack-engine-sentinel package.
	require.Len(t, pod.Spec.Volumes, 4)
	require.NotNil(t, pod.Spec.Volumes[0].DownwardAPI,
		"the file is a projection of an annotation, not a ConfigMap: there is no second object")
	require.Len(t, pod.Spec.Volumes[0].DownwardAPI.Items, 1)

	ref := pod.Spec.Volumes[0].DownwardAPI.Items[0].FieldRef
	require.NotNil(t, ref)
	assert.Contains(t, ref.FieldPath, inject.ClientConfigAnnotationKey)
	assert.NotEmpty(t, pod.Annotations[inject.ClientConfigAnnotationKey],
		"the projection reads this annotation, so an absent or empty one mounts an empty file")

	assert.NotNil(t, findVolume(pod, ModelDeploymentCompileCacheVolumeName),
		"a managed replica carries the compilation cache")
	assert.NotNil(t, findMount(&pod.Spec.Containers[0], ModelDeploymentCompileCacheVolumeName))
	assert.NotNil(t, findVolume(pod, modelDeploymentEngineSentinelVolumeName),
		"a managed accelerated replica carries gpustack-engine-sentinel")
	assert.NotNil(t, findMount(&pod.Spec.Containers[0], modelDeploymentEngineSentinelVolumeName))

	mounts := pod.Spec.Containers[0].VolumeMounts
	require.Len(t, mounts, 4)
	assert.Equal(t, inject.ConfigMountPath, mounts[0].MountPath)
	assert.True(t, mounts[0].ReadOnly)
}

// TestRenderModelDeploymentPod_StartupGateScalesWithTheDeclaredBudget pins that the kubelet is told
// the budget the role asked for, in the only unit it counts in.
//
// The two directions are the ones that can each be got wrong alone: a threshold held at the default
// restarts a role that declared an hour of loading after thirty minutes, and a threshold derived
// without rounding up cuts the last period off the budget it was meant to honor. The other two
// gates are asserted alongside because they must NOT move -- a startup budget is about the load
// window, and a replica that has served once is governed by the failure it just had.
func TestRenderModelDeploymentPod_StartupGateScalesWithTheDeclaredBudget(t *testing.T) {
	for _, tc := range []struct {
		name                  string
		startupTimeoutSeconds *int32
		wantFailureThreshold  int32
	}{
		{
			name:                 "no declared budget renders the gate this operator has always rendered",
			wantFailureThreshold: modelDeploymentStartupFailureThreshold,
		},
		{
			name:                  "a budget shorter than one period still gets a gate rather than none",
			startupTimeoutSeconds: ptr.To(int32(10)),
			wantFailureThreshold:  1,
		},
		{
			name:                  "a budget between periods rounds up rather than short",
			startupTimeoutSeconds: ptr.To(int32(25)),
			wantFailureThreshold:  3,
		},
		{
			name:                  "an hour is three hundred and sixty periods of ten seconds",
			startupTimeoutSeconds: ptr.To(int32(3600)),
			wantFailureThreshold:  360,
		},
		{
			name:                  "the largest budget the schema admits is a whole day of periods",
			startupTimeoutSeconds: ptr.To(int32(86400)),
			wantFailureThreshold:  8640,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			md := newRenderDeployment(func(md *workercore.ModelDeployment) {
				md.Spec.Roles[0].StartupTimeoutSeconds = tc.startupTimeoutSeconds
			})

			c := renderOne(t, md, newRenderAcceleratorType()).Spec.Containers[0]

			require.NotNil(t, c.StartupProbe)
			assert.Equal(t, tc.wantFailureThreshold, c.StartupProbe.FailureThreshold)
			assert.Equal(t, modelDeploymentProbePeriodSeconds, c.StartupProbe.PeriodSeconds)
			assert.Equal(t, modelDeploymentProbeTimeoutSeconds, c.StartupProbe.TimeoutSeconds)

			require.NotNil(t, c.ReadinessProbe)
			assert.Equal(t, modelDeploymentReadinessFailureThreshold, c.ReadinessProbe.FailureThreshold)
			require.NotNil(t, c.LivenessProbe)
			assert.Equal(t, modelDeploymentLivenessFailureThreshold, c.LivenessProbe.FailureThreshold)
			assert.Equal(t, modelDeploymentProbeTimeoutSeconds, c.ReadinessProbe.TimeoutSeconds)
			assert.Equal(t, modelDeploymentProbeTimeoutSeconds, c.LivenessProbe.TimeoutSeconds)
		})
	}
}

// TestRenderModelDeploymentPod_CompileCacheMountsOnManagedRoles pins the cache's whole contract on
// a rendered Pod: a managed role carries it on both engines and under every way the weights can
// arrive, and a role that took over its command line carries none of it.
//
// The delivery modes are enumerated rather than sampled because the cache path is keyed on the
// weights, so a mode this table skips is a mode whose identity derivation nothing here would cover.
// The take-over case is the other half of the same rule: the operator did not build that command
// line, so it cannot claim the container reads the cache, and a volume it mounted anyway would be
// a host directory nothing ever writes.
func TestRenderModelDeploymentPod_CompileCacheMountsOnManagedRoles(t *testing.T) {
	for _, engine := range []string{
		workercore.ModelDeploymentEngineVLLM, workercore.ModelDeploymentEngineSGLang,
	} {
		for _, tc := range []struct {
			name     string
			artifact *ModelDeploymentArtifactRender
		}{
			{name: "no artifact, the engine fetches the weights itself", artifact: nil},
			{name: "hub artifact delivered by the node plugin", artifact: testNodeArtifactRender()},
			{name: "claim artifact mounted read-only", artifact: testPvcArtifactRender()},
			{name: "image artifact mounted through an image volume", artifact: testImageArtifactRender()},
			{name: "hub artifact downloaded by the engine", artifact: testEngineArtifactRender()},
		} {
			t.Run(engine+"/"+tc.name, func(t *testing.T) {
				md := newRenderDeployment(func(md *workercore.ModelDeployment) {
					md.Spec.Engine.Name = engine
					md.Spec.KVCache = nil
				})

				pod := renderWithArtifact(t, md, tc.artifact)

				volume := findVolume(pod, ModelDeploymentCompileCacheVolumeName)
				require.NotNil(t, volume, "the managed role carries the compilation cache")
				require.NotNil(t, volume.HostPath)
				assert.Equal(t, core.HostPathDirectoryOrCreate, *volume.HostPath.Type,
					"the node creates the directory rather than refusing a cold start")
				// The host side is this role's own subdirectory, not the shared root: two roles
				// that compiled for different shapes must not write over each other's kernels.
				assert.True(t,
					strings.HasPrefix(volume.HostPath.Path, DefaultModelDeploymentCompileCacheHostDir+"/"),
					"%s is not a subdirectory of the cache root", volume.HostPath.Path)
				assert.NotEqual(t, DefaultModelDeploymentCompileCacheHostDir, volume.HostPath.Path)

				mount := findMount(&pod.Spec.Containers[0], ModelDeploymentCompileCacheVolumeName)
				require.NotNil(t, mount, "the cache is mounted into the engine's container")
				assert.Equal(t, ModelDeploymentCompileCacheMountPath, mount.MountPath)
			})
		}
	}

	t.Run("a take-over role is given no part of the cache", func(t *testing.T) {
		md := newRenderDeployment(func(md *workercore.ModelDeployment) {
			md.Spec.KVCache = nil
			md.Spec.Roles[0].Command = []string{"/bin/my-server", "--flag"}
		})

		pod := renderWithArtifact(t, md, testNodeArtifactRender())

		assert.Nil(t, findVolume(pod, ModelDeploymentCompileCacheVolumeName))
		assert.Nil(t, findMount(&pod.Spec.Containers[0], ModelDeploymentCompileCacheVolumeName))
	})

	for _, tc := range []struct {
		name      string
		mountPath string
	}{
		{name: "exact compilation cache mount path", mountPath: ModelDeploymentCompileCacheMountPath},
		{name: "parent directory covering compilation cache", mountPath: "/var/lib/gpustack"},
		{name: "subdirectory inside compilation cache", mountPath: ModelDeploymentCompileCacheMountPath + "/vllm"},
	} {
		t.Run("user additionalVolumes yields default hostPath at "+tc.name, func(t *testing.T) {
			md := newRenderDeployment(func(md *workercore.ModelDeployment) {
				md.Spec.KVCache = nil
				md.Spec.Roles[0].AdditionalVolumes = []workercore.ModelDeploymentAdditionalVolume{
					{
						MountPath:             tc.mountPath,
						PersistentVolumeClaim: &core.PersistentVolumeClaimVolumeSource{ClaimName: "shared-cache"},
					},
				}
			})

			pod := renderOne(t, md, newRenderAcceleratorType())

			assert.Nil(t, findVolume(pod, ModelDeploymentCompileCacheVolumeName),
				"default hostPath compilation cache volume must yield when user mounts an overlapping path")
			assert.Nil(t, findMount(&pod.Spec.Containers[0], ModelDeploymentCompileCacheVolumeName))

			userMount := findMount(&pod.Spec.Containers[0], "additional-0")
			require.NotNil(t, userMount, "user's additionalVolume mount must remain intact")
			assert.Equal(t, tc.mountPath, userMount.MountPath)

			value, found := envValue(pod, "VLLM_CACHE_ROOT")
			require.True(t, found, "defaulted VLLM_CACHE_ROOT remains set")
			assert.Equal(t, ModelDeploymentCompileCacheMountPath, value)
		})
	}
}

// TestRenderModelDeploymentPod_CompileCacheRootIsDefaultedNotOwned pins who decides where an
// engine's cache lives: the operator defaults it to the path it mounted, and a user entry replaces
// it rather than colliding with it.
//
// Defaulted is the load-bearing half. An owned name would be refused at admission, so a user who
// mounts the cache elsewhere, or runs an image whose engine spells the variable differently, could
// not say so at all -- and the whole point of mounting the cache rather than hard-coding a path
// into an argument is that the path stays negotiable.
func TestRenderModelDeploymentPod_CompileCacheRootIsDefaultedNotOwned(t *testing.T) {
	for _, tc := range []struct {
		name      string
		engine    string
		variable  string
		userValue string
		want      string
	}{
		{
			name:   "vllm is pointed at the mounted cache",
			engine: workercore.ModelDeploymentEngineVLLM, variable: "VLLM_CACHE_ROOT",
			want: ModelDeploymentCompileCacheMountPath,
		},
		{
			name:   "a user entry replaces the vllm default",
			engine: workercore.ModelDeploymentEngineVLLM, variable: "VLLM_CACHE_ROOT",
			userValue: "/mnt/scratch/vllm", want: "/mnt/scratch/vllm",
		},
		{
			name:   "sglang is pointed at the mounted cache",
			engine: workercore.ModelDeploymentEngineSGLang, variable: "SGLANG_CACHE_DIR",
			want: ModelDeploymentCompileCacheMountPath,
		},
		{
			name:   "a user entry replaces the sglang default",
			engine: workercore.ModelDeploymentEngineSGLang, variable: "SGLANG_CACHE_DIR",
			userValue: "/mnt/scratch/sglang", want: "/mnt/scratch/sglang",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			md := newRenderDeployment(func(md *workercore.ModelDeployment) {
				md.Spec.Engine.Name = tc.engine
				md.Spec.KVCache = nil
				if tc.userValue != "" {
					md.Spec.Roles[0].Env = []workercore.ModelDeploymentEnvVar{
						{Name: tc.variable, Value: tc.userValue},
					}
				}
			})

			value, found := envValue(renderOne(t, md, newRenderAcceleratorType()), tc.variable)

			require.True(t, found, "the engine is told where its cache lives")
			assert.Equal(t, tc.want, value)
		})
	}

	t.Run("a take-over role is told nothing about a cache it does not have", func(t *testing.T) {
		md := newRenderDeployment(func(md *workercore.ModelDeployment) {
			md.Spec.KVCache = nil
			md.Spec.Roles[0].Command = []string{"/bin/my-server", "--flag"}
		})

		pod := renderOne(t, md, newRenderAcceleratorType())

		_, found := envValue(pod, "VLLM_CACHE_ROOT")
		assert.False(t, found)
	})
}

// TestRenderModelDeploymentPod_NoClientConfigWithoutOne pins the state a deployment is in before its
// Binding has been resolved: a replica renders and runs without a client configuration mount,
// rather than referencing a ConfigMap nothing has written.
func TestRenderModelDeploymentPod_NoClientConfigWithoutOne(t *testing.T) {
	md := newRenderDeployment()
	pod := renderOne(t, md, newRenderAcceleratorType())

	for _, volume := range pod.Spec.Volumes {
		assert.NotEqual(t, inject.ConfigVolumeName, volume.Name)
	}
	for _, mount := range pod.Spec.Containers[0].VolumeMounts {
		assert.NotEqual(t, inject.ConfigMountPath, mount.MountPath)
	}
}

// TestConvertModelDeploymentAdditionalVolumes asserts how a role's spec.additionalVolumes reach the
// rendered Pod: one Pod volume and one mount per entry, named from the entry's index, with the
// mount path, sub-path and read-only carried from the entry. A claim's reference is the entry's own
// claim name and its own read-only, copied as they arrive: a Pod volume reaches a claim in the
// Pod's namespace only, so nothing may add a namespace to the reference, and the mount's read-only
// stays the entry's rather than the source's.
func TestConvertModelDeploymentAdditionalVolumes(t *testing.T) {
	for _, tc := range []struct {
		name       string
		entry      workercore.ModelDeploymentAdditionalVolume
		wantVolume core.Volume
		wantMount  core.VolumeMount
	}{
		{
			name: "a claim",
			entry: workercore.ModelDeploymentAdditionalVolume{
				MountPath:             "/cache",
				PersistentVolumeClaim: &core.PersistentVolumeClaimVolumeSource{ClaimName: "shared-cache"},
			},
			wantVolume: core.Volume{
				Name: "additional-0",
				VolumeSource: core.VolumeSource{
					PersistentVolumeClaim: &core.PersistentVolumeClaimVolumeSource{ClaimName: "shared-cache"},
				},
			},
			wantMount: core.VolumeMount{Name: "additional-0", MountPath: "/cache"},
		},
		{
			name: "a claim with a sub-path",
			entry: workercore.ModelDeploymentAdditionalVolume{
				MountPath:             "/cache",
				SubPath:               "qwen",
				PersistentVolumeClaim: &core.PersistentVolumeClaimVolumeSource{ClaimName: "shared-cache"},
			},
			wantVolume: core.Volume{
				Name: "additional-0",
				VolumeSource: core.VolumeSource{
					PersistentVolumeClaim: &core.PersistentVolumeClaimVolumeSource{ClaimName: "shared-cache"},
				},
			},
			wantMount: core.VolumeMount{Name: "additional-0", MountPath: "/cache", SubPath: "qwen"},
		},
		{
			name: "a claim with a read-only entry",
			entry: workercore.ModelDeploymentAdditionalVolume{
				MountPath:             "/cache",
				ReadOnly:              true,
				PersistentVolumeClaim: &core.PersistentVolumeClaimVolumeSource{ClaimName: "shared-cache"},
			},
			wantVolume: core.Volume{
				Name: "additional-0",
				VolumeSource: core.VolumeSource{
					PersistentVolumeClaim: &core.PersistentVolumeClaimVolumeSource{ClaimName: "shared-cache"},
				},
			},
			wantMount: core.VolumeMount{Name: "additional-0", MountPath: "/cache", ReadOnly: true},
		},
		{
			name: "a read-only claim source is copied and the mount read-only stays the entry's own",
			entry: workercore.ModelDeploymentAdditionalVolume{
				MountPath:             "/cache",
				PersistentVolumeClaim: &core.PersistentVolumeClaimVolumeSource{ClaimName: "shared-cache", ReadOnly: true},
			},
			wantVolume: core.Volume{
				Name: "additional-0",
				VolumeSource: core.VolumeSource{
					PersistentVolumeClaim: &core.PersistentVolumeClaimVolumeSource{
						ClaimName: "shared-cache", ReadOnly: true,
					},
				},
			},
			wantMount: core.VolumeMount{Name: "additional-0", MountPath: "/cache"},
		},
		{
			name: "a config map",
			entry: workercore.ModelDeploymentAdditionalVolume{
				MountPath: "/etc/app",
				ReadOnly:  true,
				SubPath:   "conf",
				ConfigMap: &core.LocalObjectReference{Name: "app-config"},
			},
			wantVolume: core.Volume{
				Name: "additional-0",
				VolumeSource: core.VolumeSource{
					ConfigMap: &core.ConfigMapVolumeSource{LocalObjectReference: core.LocalObjectReference{Name: "app-config"}},
				},
			},
			wantMount: core.VolumeMount{Name: "additional-0", MountPath: "/etc/app", ReadOnly: true, SubPath: "conf"},
		},
		{
			name: "a secret",
			entry: workercore.ModelDeploymentAdditionalVolume{
				MountPath: "/var/run/creds",
				ReadOnly:  true,
				Secret:    &core.LocalObjectReference{Name: "app-creds"},
			},
			wantVolume: core.Volume{
				Name: "additional-0",
				VolumeSource: core.VolumeSource{
					Secret: &core.SecretVolumeSource{SecretName: "app-creds"},
				},
			},
			wantMount: core.VolumeMount{Name: "additional-0", MountPath: "/var/run/creds", ReadOnly: true},
		},
		{
			name: "a host path",
			entry: workercore.ModelDeploymentAdditionalVolume{
				MountPath: "/host/models",
				HostPath:  &core.HostPathVolumeSource{Path: "/mnt/models"},
			},
			wantVolume: core.Volume{
				Name:         "additional-0",
				VolumeSource: core.VolumeSource{HostPath: &core.HostPathVolumeSource{Path: "/mnt/models"}},
			},
			wantMount: core.VolumeMount{Name: "additional-0", MountPath: "/host/models"},
		},
		{
			name: "an entry with every source renders the first",
			entry: workercore.ModelDeploymentAdditionalVolume{
				MountPath:             "/data",
				ConfigMap:             &core.LocalObjectReference{Name: "app-config"},
				Secret:                &core.LocalObjectReference{Name: "app-creds"},
				HostPath:              &core.HostPathVolumeSource{Path: "/mnt/models"},
				PersistentVolumeClaim: &core.PersistentVolumeClaimVolumeSource{ClaimName: "shared-cache"},
			},
			wantVolume: core.Volume{
				Name: "additional-0",
				VolumeSource: core.VolumeSource{
					ConfigMap: &core.ConfigMapVolumeSource{LocalObjectReference: core.LocalObjectReference{Name: "app-config"}},
				},
			},
			wantMount: core.VolumeMount{Name: "additional-0", MountPath: "/data"},
		},
		{
			name: "a claim yields to a host path",
			entry: workercore.ModelDeploymentAdditionalVolume{
				MountPath:             "/data",
				HostPath:              &core.HostPathVolumeSource{Path: "/mnt/models"},
				PersistentVolumeClaim: &core.PersistentVolumeClaimVolumeSource{ClaimName: "shared-cache"},
			},
			wantVolume: core.Volume{
				Name:         "additional-0",
				VolumeSource: core.VolumeSource{HostPath: &core.HostPathVolumeSource{Path: "/mnt/models"}},
			},
			wantMount: core.VolumeMount{Name: "additional-0", MountPath: "/data"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			vols, mounts := convertModelDeploymentAdditionalVolumes(
				[]workercore.ModelDeploymentAdditionalVolume{tc.entry},
			)

			assert.Equal(t, []core.Volume{tc.wantVolume}, vols)
			assert.Equal(t, []core.VolumeMount{tc.wantMount}, mounts)
		})
	}

	// Two roles mount the same claim through their own entries: each Pod volume carries the claim
	// name as the entry spelled it, so both reference one claim in the Pods' own namespace.
	t.Run("the same claim name reaches both roles of a pair", func(t *testing.T) {
		entry := workercore.ModelDeploymentAdditionalVolume{
			MountPath:             "/cache",
			PersistentVolumeClaim: &core.PersistentVolumeClaimVolumeSource{ClaimName: "shared-cache"},
		}

		prefillVols, _ := convertModelDeploymentAdditionalVolumes(
			[]workercore.ModelDeploymentAdditionalVolume{entry},
		)
		decodeVols, _ := convertModelDeploymentAdditionalVolumes(
			[]workercore.ModelDeploymentAdditionalVolume{entry},
		)

		require.Len(t, prefillVols, 1)
		require.Len(t, decodeVols, 1)
		require.NotNil(t, prefillVols[0].PersistentVolumeClaim)
		require.NotNil(t, decodeVols[0].PersistentVolumeClaim)
		assert.Equal(t, "shared-cache", prefillVols[0].PersistentVolumeClaim.ClaimName)
		assert.Equal(t, "shared-cache", decodeVols[0].PersistentVolumeClaim.ClaimName)
	})
}

// TestModelDeploymentPodSpecHash_IsDeterministic states the property the whole rollout rests on: two
// renders of one spec must fingerprint identically, or the deployment would recreate every replica
// on every pass forever.
func TestModelDeploymentPodSpecHash_IsDeterministic(t *testing.T) {
	md := newRenderDeployment(func(md *workercore.ModelDeployment) {
		md.Spec.Roles[0].Env = []workercore.ModelDeploymentEnvVar{
			{Name: "B", Value: "2"}, {Name: "A", Value: "1"},
		}
	})

	first := renderOne(t, md, newRenderAcceleratorType())
	second := renderOne(t, md, newRenderAcceleratorType())

	assert.Equal(t,
		first.Annotations[modelDeploymentPodSpecHashAnnotation],
		second.Annotations[modelDeploymentPodSpecHashAnnotation])
}

// TestModelDeploymentPodSpecHash_DoesNotCoverItself verifies that stamping a rendered template
// strips the internal binding-hash carrier and stamps both the explicit spec hash and the current
// render-version annotation deterministically.
func TestModelDeploymentPodSpecHash_DoesNotCoverItself(t *testing.T) {
	md := newRenderDeployment()
	it := newRenderAcceleratorType()
	pod := renderOne(t, md, it)

	stored := pod.Annotations[modelDeploymentPodSpecHashAnnotation]
	assert.NotEmpty(t, stored)
	assert.Equal(t, strconv.Itoa(modelDeploymentRenderVersion),
		pod.Annotations[modelDeploymentRenderVersionAnnotation])
	assert.NotContains(t, pod.Annotations, modelDeploymentTemplateBindingHashAnnotation)

	again := renderOne(t, md, it)
	assert.Equal(t, stored, again.Annotations[modelDeploymentPodSpecHashAnnotation],
		"re-rendering and stamping a Pod must reproduce the value it already carries")
}

// TestModelDeploymentPodSpecHash_DoesNotCoverTheName verifies that the explicit spec hash covers
// the user spec and resolved bindings rather than a Pod's assigned name, so a replica named by an
// earlier scheme is not stale merely for being old.
func TestModelDeploymentPodSpecHash_DoesNotCoverTheName(t *testing.T) {
	base := renderOne(t, newRenderDeployment(), newRenderAcceleratorType())
	stored := base.Annotations[modelDeploymentPodSpecHashAnnotation]

	testCases := []struct {
		name      string
		mutateMD  func(*workercore.ModelDeployment)
		mutatePod func(*core.Pod)
		moves     bool
	}{
		{
			name: "a name assigned by an earlier scheme leaves the fingerprint where it was",
			mutatePod: func(pod *core.Pod) {
				pod.Name = "qwen-server-3"
			},
		},
		{
			name: "a spec change moves the fingerprint",
			mutateMD: func(md *workercore.ModelDeployment) {
				md.Spec.Roles[0].Image = "vllm/vllm-openai:v0.26.0"
			},
			moves: true,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			md := newRenderDeployment()
			if tc.mutateMD != nil {
				tc.mutateMD(md)
			}
			tmpl, err := renderModelDeploymentPodTemplate(context.Background(), ModelDeploymentRenderInput{
				Deployment:      md,
				Role:            &md.Spec.Roles[0],
				AcceleratorType: newRenderAcceleratorType(),
			})
			require.NoError(t, err)
			if tc.mutatePod != nil {
				tc.mutatePod(tmpl)
			}
			stampModelDeploymentPod(tmpl, md, &md.Spec.Roles[0], 0, 0)

			if tc.moves {
				assert.NotEqual(t, stored, tmpl.Annotations[modelDeploymentPodSpecHashAnnotation])
				return
			}
			assert.Equal(t, stored, tmpl.Annotations[modelDeploymentPodSpecHashAnnotation])
		})
	}
}

// TestModelDeploymentPodSpecHash_IgnoresImplicitOperatorDefaultsAndSettings verifies that implicit
// operator rendering knobs, replica counts, and hub settings do not move pod-spec-hash, while
// explicit spec and artifact binding changes do.
func TestModelDeploymentPodSpecHash_IgnoresImplicitOperatorDefaultsAndSettings(t *testing.T) {
	newArtifact := func() *ModelDeploymentArtifactRender {
		return &ModelDeploymentArtifactRender{
			Delivery:   workercore.ModelDeploymentModelDeliveryEngine,
			Repository: "Qwen/Qwen3-8B",
			Revision:   "abc1234",
			Hub:        modelDeploymentHubHuggingFace,
			SizeBytes:  1 << 30,
		}
	}

	baseMD := newRenderDeployment()
	baseIn := ModelDeploymentRenderInput{
		Deployment:      baseMD,
		Role:            &baseMD.Spec.Roles[0],
		AcceleratorType: newRenderAcceleratorType(),
		Artifact:        newArtifact(),
	}
	basePod, err := renderModelDeploymentPod(context.Background(), baseIn)
	require.NoError(t, err)
	baseHash := basePod.Annotations[modelDeploymentPodSpecHashAnnotation]
	assert.Equal(t, strconv.Itoa(modelDeploymentRenderVersion),
		basePod.Annotations[modelDeploymentRenderVersionAnnotation])

	testCases := []struct {
		name   string
		mutate func(*workercore.ModelDeployment, *ModelDeploymentRenderInput)
		moves  bool
	}{
		{
			name: "general resources overcommit knob does not move the explicit spec hash",
			mutate: func(_ *workercore.ModelDeployment, in *ModelDeploymentRenderInput) {
				in.GeneralResourcesOvercommit = true
			},
		},
		{
			name: "native sidecar shape knob does not move the explicit spec hash",
			mutate: func(_ *workercore.ModelDeployment, in *ModelDeploymentRenderInput) {
				in.NativeSidecar = true
			},
		},
		{
			name: "tcp tw_reuse knob does not move the explicit spec hash",
			mutate: func(_ *workercore.ModelDeployment, in *ModelDeploymentRenderInput) {
				in.TCPTWReuse = true
			},
		},
		{
			name: "hub endpoint and proxy settings do not move the explicit spec hash",
			mutate: func(_ *workercore.ModelDeployment, in *ModelDeploymentRenderInput) {
				in.Artifact.Endpoint = "https://hf-mirror.example.com"
				in.Artifact.HTTPSProxy = "http://proxy.example.com:8080"
				in.Artifact.NoProxy = "10.0.0.0/8"
			},
		},
		{
			name: "replica count scaling does not move the explicit spec hash",
			mutate: func(md *workercore.ModelDeployment, _ *ModelDeploymentRenderInput) {
				md.Spec.Roles[0].Replicas = 5
			},
		},
		{
			name: "artifact revision change moves the explicit spec hash",
			mutate: func(_ *workercore.ModelDeployment, in *ModelDeploymentRenderInput) {
				in.Artifact.Revision = "def5678"
			},
			moves: true,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			md := newRenderDeployment()
			in := ModelDeploymentRenderInput{
				Deployment:      md,
				Role:            &md.Spec.Roles[0],
				AcceleratorType: newRenderAcceleratorType(),
				Artifact:        newArtifact(),
			}
			tc.mutate(md, &in)
			pod, err := renderModelDeploymentPod(context.Background(), in)
			require.NoError(t, err)
			if tc.moves {
				assert.NotEqual(t, baseHash, pod.Annotations[modelDeploymentPodSpecHashAnnotation])
				return
			}
			assert.Equal(t, baseHash, pod.Annotations[modelDeploymentPodSpecHashAnnotation])
		})
	}
}

// TestModelDeploymentPodSpecHash_CoversEverySpecField uses reflection to ensure every field on
// ModelDeploymentSpec, ModelDeploymentModel, ModelDeploymentRouter, ModelDeploymentRole, and
// ModelDeploymentRoleRuntimeControl is explicitly accounted for in modelDeploymentExplicitSpecHash.
func TestModelDeploymentPodSpecHash_CoversEverySpecField(t *testing.T) {
	fieldNames := func(typ reflect.Type) []string {
		got := make([]string, 0, typ.NumField())
		for i := 0; i < typ.NumField(); i++ {
			got = append(got, typ.Field(i).Name)
		}
		return got
	}

	assert.Equal(t, []string{
		"Model",
		"Engine",
		"KVCache",
		"Roles",
		"Router",
		"KVTransfer",
	}, fieldNames(reflect.TypeFor[workercore.ModelDeploymentSpec]()),
		"new ModelDeploymentSpec fields must be wired into modelDeploymentExplicitSpecHash")

	assert.Equal(t, []string{
		"Name",
		"ArtifactRef",
		"LoRAAdapters",
	}, fieldNames(reflect.TypeFor[workercore.ModelDeploymentModel]()),
		"new ModelDeploymentModel fields must be wired into modelDeploymentExplicitSpecHash")

	assert.Equal(t, []string{
		"Name",
		"Replicas",
		"Image",
		"ExtraArgs",
		"ImagePullPolicy",
		"ImagePullSecrets",
		"RequestTimeoutSeconds",
		"DisaggregationThresholdTokens",
	}, fieldNames(reflect.TypeFor[workercore.ModelDeploymentRouter]()),
		"new ModelDeploymentRouter fields must be classified as projected or router-only in modelDeploymentExplicitSpecHash")

	assert.Equal(t, []string{
		"Name",
		"Kind",
		"Replicas",
		"ReplicaSize",
		"AcceleratorType",
		"Resources",
		"Image",
		"EngineVersion",
		"ImagePullPolicy",
		"ImagePullSecrets",
		"Privileged",
		"Ports",
		"AdditionalVolumes",
		"ShmSize",
		"Command",
		"ExtraArgs",
		"Env",
		"Topology",
		"TerminationGracePeriodSeconds",
		"StartupTimeoutSeconds",
		"Pause",
		"RuntimeControl",
		"KernelArtifactRef",
	}, fieldNames(reflect.TypeFor[workercore.ModelDeploymentRole]()),
		"new ModelDeploymentRole fields must be wired into modelDeploymentExplicitSpecHash")

	assert.Equal(t, []string{
		"Parameters",
	}, fieldNames(reflect.TypeFor[workercore.ModelDeploymentRoleRuntimeControl]()),
		"new ModelDeploymentRoleRuntimeControl fields must be wired into modelDeploymentExplicitSpecHash")
}

// TestModelDeploymentPodSpecHash_MovesWithEveryRenderedInput walks the inputs a rollout must react
// to and asserts each one moves the fingerprint. A change that did not would leave replicas running
// a spec nobody asked for, with the deployment reporting itself Ready.
func TestModelDeploymentPodSpecHash_MovesWithEveryRenderedInput(t *testing.T) {
	base := renderOne(t, newRenderDeployment(), newRenderAcceleratorType())
	baseHash := base.Annotations[modelDeploymentPodSpecHashAnnotation]

	testCases := []struct {
		name    string
		mutate  func(*workercore.ModelDeployment)
		acctype func(*worker.AcceleratorType)
		input   func(*ModelDeploymentRenderInput)
	}{
		{
			name:   "image",
			mutate: func(md *workercore.ModelDeployment) { md.Spec.Roles[0].Image = "vllm/vllm-openai:v0.26.0" },
		},
		{
			name: "shared memory size",
			mutate: func(md *workercore.ModelDeployment) {
				md.Spec.Roles[0].ShmSize = ptr.To(resource.MustParse("32Gi"))
			},
		},
		{
			name:   "model",
			mutate: func(md *workercore.ModelDeployment) { md.Spec.Model.Name = "Qwen/Qwen3-32B" },
		},
		{
			name:   "extra arguments",
			mutate: func(md *workercore.ModelDeployment) { md.Spec.Roles[0].ExtraArgs = []string{"--max-model-len=1024"} },
		},
		{
			name: "environment",
			mutate: func(md *workercore.ModelDeployment) {
				md.Spec.Roles[0].Env = []workercore.ModelDeploymentEnvVar{{Name: "A", Value: "1"}}
			},
		},
		{
			name: "accelerator request",
			mutate: func(md *workercore.ModelDeployment) {
				md.Spec.Roles[0].Resources = &workercore.ModelDeploymentRoleResources{
					Accelerator: ptr.To(resource.MustParse("2")),
				}
			},
		},
		{
			name:    "accelerator type — the entrance label moves with it",
			mutate:  func(md *workercore.ModelDeployment) { md.Spec.Roles[0].AcceleratorType = "h20-4x" },
			acctype: func(at *worker.AcceleratorType) { at.Name, at.Status.Entrance = "h20-4x", "queue-for-h20-4x" },
		},
		{
			name:    "the published entrance, with the accelerator type name unchanged",
			acctype: func(at *worker.AcceleratorType) { at.Status.Entrance = "queue-renamed" },
		},
		{
			name: "declared host CPU resource",
			mutate: func(md *workercore.ModelDeployment) {
				md.Spec.Roles[0].Resources = &workercore.ModelDeploymentRoleResources{
					CPU: ptr.To(resource.MustParse("32")),
				}
			},
		},
		{
			name:  "the resolved connector arguments",
			input: func(in *ModelDeploymentRenderInput) { in.Connector.Args = []string{`--kv-transfer-config={}`} },
		},
		{
			name:  "the runtime class",
			input: func(in *ModelDeploymentRenderInput) { in.RuntimeClassName = "nvidia" },
		},
		{
			name: "the termination grace",
			mutate: func(md *workercore.ModelDeployment) {
				md.Spec.Roles[0].TerminationGracePeriodSeconds = ptr.To[int64](45)
			},
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			md := newRenderDeployment()
			if tc.mutate != nil {
				tc.mutate(md)
			}
			at := newRenderAcceleratorType()
			if tc.acctype != nil {
				tc.acctype(at)
			}

			in := ModelDeploymentRenderInput{
				Deployment:      md,
				Role:            &md.Spec.Roles[0],
				AcceleratorType: at,
			}
			if tc.input != nil {
				tc.input(&in)
			}

			pod, err := renderModelDeploymentPod(context.Background(), in)
			require.NoError(t, err)
			assert.NotEqual(t, baseHash, pod.Annotations[modelDeploymentPodSpecHashAnnotation])
		})
	}
}

// TestRenderModelDeploymentPod_ConfigChangeMovesTheSpecHash is the rollout property a ConfigMap
// carrier could not have delivered.
//
// A ConfigMap reaches a Pod as a NAME, so re-rendering its contents leaves core.PodSpec
// byte-identical while the hash's subject is {Labels, Annotations, PodSpec}. The hash would not
// move, no recreate would follow, the replicas would keep a stale client configuration -- and a
// check asserting the hash moved would have gone GREEN over exactly that. The annotation carrier
// puts the document itself inside the hash's subject, so this test is the difference between the
// two designs rather than a restatement of either.
func TestRenderModelDeploymentPod_ConfigChangeMovesTheSpecHash(t *testing.T) {
	md := newRenderDeployment()

	render := func(endpoint string) *core.Pod {
		cin := connectorInput(workercore.ModelDeploymentEngineVLLM, nodefeature.ManufacturerNVIDIA)
		cin.MasterServerAddress = endpoint
		conn, err := SynthesizeModelDeploymentConnector(cin)
		require.NoError(t, err)

		pod, err := renderModelDeploymentPod(context.Background(), ModelDeploymentRenderInput{
			Deployment:      md,
			Role:            &md.Spec.Roles[0],
			AcceleratorType: newRenderAcceleratorType(),
			Connector:       conn,
		})
		require.NoError(t, err)

		return pod
	}

	before := render("master-a.gpustack-system.svc:50051")
	after := render("master-b.gpustack-system.svc:50051")

	assert.NotEqual(t,
		before.Annotations[modelDeploymentPodSpecHashAnnotation],
		after.Annotations[modelDeploymentPodSpecHashAnnotation],
		"a pool endpoint change has to move the hash, or the replicas keep a stale client config")

	// AND THE POD SPEC IS IDENTICAL ACROSS THE TWO, which is what makes the assertion above mean
	// something. The volume names an annotation rather than carrying the document, so the spec
	// cannot see this change at all: had the hash's subject been the spec alone, these two renders
	// would be indistinguishable and no recreate would ever follow a configuration change.
	assert.Equal(t, before.Spec, after.Spec,
		"the projection names an annotation, so the spec is blind to the content change")
}

// TestRenderModelDeploymentPod_Probes pins the gates that make "ready" mean "the engine answers".
//
// Without them the kubelet reports Ready as soon as the process starts, which on a measured run
// preceded the engine serving by more than a minute and a half. Every assertion here is about the
// rendered shape; whether the gate actually tracks the engine needs an accelerator and is not
// covered by any test in this package.
func TestRenderModelDeploymentPod_Probes(t *testing.T) {
	t.Run("a role declaring its own ports is gated on that port, which the engine is told to open", func(t *testing.T) {
		// THE DECLARED PORT REACHES THE ENGINE, so the address the Service targets and the address
		// the engine opens are one figure. That is what makes gating on it correct rather than a way
		// to strand a replica, and it is why this case no longer withholds the gates.
		md := newRenderDeployment(func(md *workercore.ModelDeployment) {
			md.Spec.Roles[0].Ports = []workercore.ModelDeploymentPort{
				{Port: 9100, Protocol: core.ProtocolTCP},
			}
		})

		pod, err := renderModelDeploymentPod(context.Background(), ModelDeploymentRenderInput{
			Deployment:      md,
			Role:            &md.Spec.Roles[0],
			AcceleratorType: newRenderAcceleratorType(),
		})
		require.NoError(t, err)

		c := pod.Spec.Containers[0]
		assert.Equal(t, []string{"--host", "0.0.0.0", "--port", "9100"}, c.Command[len(c.Command)-4:],
			"the declared port is what the engine is told to open")
		require.NotNil(t, c.StartupProbe, "and the gate reads it")
		require.NotNil(t, c.ReadinessProbe)
		require.NotNil(t, c.LivenessProbe)
		assert.Equal(t, intstr.FromInt32(9100), c.StartupProbe.HTTPGet.Port)
		assert.Equal(t, intstr.FromInt32(9100), c.ReadinessProbe.HTTPGet.Port)
		assert.Equal(t, intstr.FromInt32(9100), c.LivenessProbe.HTTPGet.Port,
			"all three gates read the port the engine was told to open")
		assert.Equal(t, int32(9100), c.Ports[0].ContainerPort)
	})

	t.Run("a port declared as UDP is not gated, and this one fails the other way", func(t *testing.T) {
		// EVERY OTHER GUARD HERE AVOIDS CALLING A WORKING REPLICA BROKEN. This one avoids the
		// reverse: an HTTP engine speaks TCP whatever the declaration says, so the gate would
		// SUCCEED while the published endpoint forwards a protocol nothing answers.
		for _, proto := range []core.Protocol{core.ProtocolUDP, core.ProtocolSCTP} {
			md := newRenderDeployment(func(md *workercore.ModelDeployment) {
				md.Spec.Roles[0].Ports = []workercore.ModelDeploymentPort{
					{Port: 8000, Protocol: proto},
				}
			})

			pod, err := renderModelDeploymentPod(context.Background(), ModelDeploymentRenderInput{
				Deployment:      md,
				Role:            &md.Spec.Roles[0],
				AcceleratorType: newRenderAcceleratorType(),
			})
			require.NoError(t, err)

			assert.Nil(t, pod.Spec.Containers[0].StartupProbe,
				"%s: an HTTP gate would pass while the endpoint forwards %s", proto, proto)
			assert.Nil(t, pod.Spec.Containers[0].ReadinessProbe, "%s", proto)
			assert.Nil(t, pod.Spec.Containers[0].LivenessProbe,
				"%s: and a liveness gate on an address nothing answers would restart it forever", proto)
		}
	})

	t.Run("a role moving its port alone is gated on the port the engine opens", func(t *testing.T) {
		// THE CONTAINER PORT FOLLOWS THE ENGINE'S, so the Service's targetPort and the gate both
		// read the port the role named. Withdrawing the gates here would leave a replica that
		// answers nowhere graded as serving.
		for _, extraArgs := range [][]string{
			{"--port", "9100"},
			{"--port=9100"},
			{"--por", "9100"},
			{"--ssl-certfile", "/etc/tls/tls.crt", "--port", "9100"},
		} {
			md := newRenderDeployment(func(md *workercore.ModelDeployment) {
				md.Spec.Roles[0].ExtraArgs = extraArgs
			})

			c := renderOne(t, md, newRenderAcceleratorType()).Spec.Containers[0]
			assert.Equal(t, int32(9100), c.Ports[0].ContainerPort, "%v", extraArgs)
			for _, probe := range []*core.Probe{c.StartupProbe, c.ReadinessProbe, c.LivenessProbe} {
				require.NotNil(t, probe, "%v", extraArgs)
				assert.Equal(t, intstr.FromInt32(9100), probe.HTTPGet.Port, "%v", extraArgs)
			}
		}
	})

	t.Run("a declared port the engine was moved off is not gated", func(t *testing.T) {
		// ADMISSION REFUSES THIS SHAPE, but only for what it admits from now on: an object stored
		// before the rule still renders, and its Service keeps the declared port while the engine
		// listens on the other one. A gate on either port would grade the other fact.
		md := newRenderDeployment(func(md *workercore.ModelDeployment) {
			md.Spec.Roles[0].Ports = []workercore.ModelDeploymentPort{{Port: 9000, Protocol: core.ProtocolTCP}}
			md.Spec.Roles[0].ExtraArgs = []string{"--port", "9100"}
		})

		c := renderOne(t, md, newRenderAcceleratorType()).Spec.Containers[0]
		assert.Equal(t, int32(9000), c.Ports[0].ContainerPort, "the declared port is the role's own")
		assert.Nil(t, c.StartupProbe)
		assert.Nil(t, c.ReadinessProbe)
		assert.Nil(t, c.LivenessProbe)
	})

	t.Run("a role moving its host or demanding a client certificate is not gated", func(t *testing.T) {
		// TWO WAYS TO DO IT, and both strand a replica that is serving. --host moves WHERE the
		// engine listens, and the operator then leaves it alone, so it cannot know whether a probe
		// reaches the address. Demanding a CLIENT certificate is the other: the kubelet has none
		// to present, so the gate would fail against a listener serving every real client correctly.
		//
		// THAT SECOND ONE NEEDS TLS ACTUALLY TURNED ON to mean anything, which is why the case below
		// carries a certificate. --ssl-cert-reqs by itself is covered as a GATED case further down.
		for _, extraArgs := range [][]string{
			{"--host", "127.0.0.1"},
			{"--host=127.0.0.1"},
			{"--ssl-certfile", "/etc/tls/tls.crt", "--ssl-cert-reqs", "2"},
			{"--ssl-cert-reqs", "2", "--ssl-keyfile=/etc/tls/tls.key"},
			{"--ssl-certfile", "/etc/tls/tls.crt", "--ssl-cert-reqs=2"},
			// "02" is CERT_REQUIRED to the engine's integer parser, so a check comparing the value
			// as text would gate a listener that refuses the probe.
			{"--ssl-certfile", "/etc/tls/tls.crt", "--ssl-cert-reqs", "02"},
			// A value this cannot read is treated as the refusal: withdrawing a gate costs a signal,
			// while fitting one to a listener that rejects it restarts a replica that is serving.
			{"--ssl-certfile", "/etc/tls/tls.crt", "--ssl-cert-reqs", "required"},
			{"--ssl-certfile", "/etc/tls/tls.crt", "--ssl-cert-reqs"},
			// A NUMBER OUTSIDE THE ENUM IS NOT A PERMISSIVE ONE. ssl defines 0, 1 and 2 and nothing
			// else, so these describe a listener whose behavior this operator cannot reason about.
			// A check naming only the rejecting value would read them as safe and gate it.
			{"--ssl-certfile", "/etc/tls/tls.crt", "--ssl-cert-reqs", "3"},
			{"--ssl-certfile", "/etc/tls/tls.crt", "--ssl-cert-reqs=-1"},
			// The last occurrence is the one the engine's parser keeps.
			{"--ssl-certfile", "/etc/tls/tls.crt", "--ssl-cert-reqs=0", "--ssl-cert-reqs=2"},
		} {
			md := newRenderDeployment(func(md *workercore.ModelDeployment) {
				md.Spec.Roles[0].ExtraArgs = extraArgs
			})

			pod, err := renderModelDeploymentPod(context.Background(), ModelDeploymentRenderInput{
				Deployment:      md,
				Role:            &md.Spec.Roles[0],
				AcceleratorType: newRenderAcceleratorType(),
			})
			require.NoError(t, err)

			assert.Nil(t, pod.Spec.Containers[0].StartupProbe,
				"%v decides the endpoint for itself, so the gate cannot claim to read it", extraArgs)
			assert.Nil(t, pod.Spec.Containers[0].ReadinessProbe,
				"%v decides the endpoint for itself, so the gate cannot claim to read it", extraArgs)
			assert.Nil(t, pod.Spec.Containers[0].LivenessProbe,
				"%v: a liveness gate on an address the operator does not know would restart a "+
					"replica that is serving, which is the worst of the three outcomes", extraArgs)
		}
	})

	t.Run("a role enabling TLS stays gated, over HTTPS", func(t *testing.T) {
		// LOSING READINESS TO ONE ORDINARY FLAG would hand back the defect these gates remove. The
		// address is still the operator's own -- only the transport moved -- and the kubelet does
		// not verify the server certificate on an HTTPS probe.
		//
		// EITHER FLAG ALONE IS ENOUGH, because uvicorn -- which both engines hand their ssl
		// arguments to -- enables TLS on `ssl_keyfile or ssl_certfile`. Reading it as "both" would
		// send a plaintext probe at a TLS listener, which never completes.
		for _, extraArgs := range [][]string{
			{"--ssl-certfile", "/etc/tls/tls.crt"},
			{"--ssl-keyfile=/etc/tls/tls.key"},
			{"--ssl-certfile", "/etc/tls/tls.crt", "--ssl-keyfile", "/etc/tls/tls.key"},
			// A CLIENT-CERTIFICATE MODE THAT ACCEPTS A CERTIFICATE-LESS CLIENT KEEPS ITS GATES.
			// CERT_NONE asks for nothing and CERT_OPTIONAL verifies only what is offered, so the
			// kubelet's probe completes against both; only CERT_REQUIRED turns it away. Withdrawing
			// the gates on the flag's presence alone would cost these roles all three for nothing.
			{"--ssl-certfile", "/etc/tls/tls.crt", "--ssl-cert-reqs", "0"},
			{"--ssl-certfile", "/etc/tls/tls.crt", "--ssl-cert-reqs=1"},
			{"--ssl-certfile", "/etc/tls/tls.crt", "--ssl-cert-reqs=2", "--ssl-cert-reqs=0"},
		} {
			md := newRenderDeployment(func(md *workercore.ModelDeployment) {
				md.Spec.Roles[0].ExtraArgs = extraArgs
			})

			pod, err := renderModelDeploymentPod(context.Background(), ModelDeploymentRenderInput{
				Deployment:      md,
				Role:            &md.Spec.Roles[0],
				AcceleratorType: newRenderAcceleratorType(),
			})
			require.NoError(t, err)

			c := pod.Spec.Containers[0]
			require.NotNil(t, c.StartupProbe, "%v", extraArgs)
			require.NotNil(t, c.ReadinessProbe, "%v", extraArgs)
			require.NotNil(t, c.LivenessProbe, "%v", extraArgs)
			assert.Equal(t, core.URISchemeHTTPS, c.StartupProbe.HTTPGet.Scheme,
				"%v moved the transport, so the gate has to follow it", extraArgs)
			assert.Equal(t, core.URISchemeHTTPS, c.ReadinessProbe.HTTPGet.Scheme, "%v", extraArgs)
			assert.Equal(t, core.URISchemeHTTPS, c.LivenessProbe.HTTPGet.Scheme, "%v", extraArgs)
		}
	})

	t.Run("an --ssl-* flag that does not turn TLS on leaves the replica gated over HTTP", func(t *testing.T) {
		// THE WHOLE FAMILY IS NOT THE CRITERION, only the two flags that carry a certificate or a
		// key. Both supported engines hand every ssl argument to uvicorn and decide nothing
		// themselves, and uvicorn enables TLS on `ssl_keyfile or ssl_certfile` alone. Each of these
		// passed BY ITSELF leaves an ordinary HTTP server.
		//
		// GETTING THIS WRONG FAILS TWICE: the gate speaks TLS to a plaintext listener, which never
		// completes and restarts the replica at the startup threshold, and status.endpoint publishes
		// an https:// address no client can use. --ssl-cert-reqs additionally lost its gate for a
		// client-certificate rule that uvicorn never applied, having built no TLS context.
		for _, extraArgs := range [][]string{
			{"--ssl-ca-certs", "/etc/tls/ca.crt"},
			{"--ssl-cert-reqs", "2"},
			{"--ssl-ciphers", "HIGH"},
			{"--ssl-keyfile-password", "hunter2"},
		} {
			md := newRenderDeployment(func(md *workercore.ModelDeployment) {
				md.Spec.Roles[0].ExtraArgs = extraArgs
			})

			pod, err := renderModelDeploymentPod(context.Background(), ModelDeploymentRenderInput{
				Deployment:      md,
				Role:            &md.Spec.Roles[0],
				AcceleratorType: newRenderAcceleratorType(),
			})
			require.NoError(t, err)

			c := pod.Spec.Containers[0]
			require.NotNil(t, c.StartupProbe, "%v does not move the listener, so it keeps its gates", extraArgs)
			require.NotNil(t, c.ReadinessProbe, "%v", extraArgs)
			require.NotNil(t, c.LivenessProbe, "%v", extraArgs)
			assert.Equal(t, core.URISchemeHTTP, c.StartupProbe.HTTPGet.Scheme,
				"%v leaves an ordinary HTTP server, and a TLS probe against it never completes", extraArgs)
			assert.Equal(t, core.URISchemeHTTP, c.ReadinessProbe.HTTPGet.Scheme, "%v", extraArgs)
			assert.Equal(t, core.URISchemeHTTP, c.LivenessProbe.HTTPGet.Scheme, "%v", extraArgs)
		}
	})

	t.Run("a gated replica carries a liveness gate whose threshold outlives the readiness one", func(t *testing.T) {
		// A REPLICA WHOSE ENGINE STOPS ANSWERING HAS NO OTHER WAY BACK. Losing readiness only takes
		// it out of the Service; the Pod keeps running and keeps the accelerators it was admitted
		// with, and this controller deletes a replica only for a rebuild, a scale-down or a spec
		// change -- never because it went quiet.
		//
		// THE ORDER OF THE TWO THRESHOLDS IS THE POINT, not either figure. Whatever they are, a
		// stall has to cost readiness before it costs a restart, because readiness is recovered by
		// answering again and a restart throws away a model that took the startup budget to load.
		// Equal thresholds would restart on the same failure that withdrew the replica.
		md := newRenderDeployment()

		pod, err := renderModelDeploymentPod(context.Background(), ModelDeploymentRenderInput{
			Deployment:      md,
			Role:            &md.Spec.Roles[0],
			AcceleratorType: newRenderAcceleratorType(),
		})
		require.NoError(t, err)

		c := pod.Spec.Containers[0]
		require.NotNil(t, c.LivenessProbe)
		require.NotNil(t, c.ReadinessProbe)
		require.NotNil(t, c.StartupProbe)

		assert.Equal(t, modelDeploymentEngineSentinelLivePath, c.StartupProbe.HTTPGet.Path,
			"startup gate reads sentinel live route on managed accelerated roles")
		assert.Equal(t, modelDeploymentEngineSentinelLivePath, c.LivenessProbe.HTTPGet.Path,
			"liveness gate reads sentinel live route once startup succeeds")
		assert.Equal(t, modelDeploymentEngineSentinelReadyPath, c.ReadinessProbe.HTTPGet.Path,
			"readiness gate reads sentinel ready route")
		assert.Greater(t, c.LivenessProbe.FailureThreshold, c.ReadinessProbe.FailureThreshold,
			"a stall must cost readiness before it costs a restart")
		assert.Greater(t, c.StartupProbe.FailureThreshold, c.LivenessProbe.FailureThreshold,
			"and the load window must outlast both, or a slow start becomes a restart loop")
		assert.Equal(t, c.ReadinessProbe.PeriodSeconds, c.LivenessProbe.PeriodSeconds,
			"the thresholds are only comparable while the periods agree")
	})

	t.Run("a connector argument carrying an endpoint flag is seen too", func(t *testing.T) {
		// THE TWO CHECKS READ THE SAME LIST. The fill skips a flag anywhere on the rendered command
		// line, so a gate scanning only role.ExtraArgs would honor a connector-supplied port and
		// then grade the operator's own -- the asymmetry this case exists to keep closed. No
		// connector renders one of these today, which is exactly why nothing else would catch it.
		md := newRenderDeployment()

		pod, err := renderModelDeploymentPod(context.Background(), ModelDeploymentRenderInput{
			Deployment:      md,
			Role:            &md.Spec.Roles[0],
			AcceleratorType: newRenderAcceleratorType(),
			Connector:       ModelDeploymentConnectorRender{Args: []string{"--port", "9100"}},
		})
		require.NoError(t, err)

		assert.Nil(t, pod.Spec.Containers[0].StartupProbe,
			"a port the connector supplied is still not a port the operator chose")
		assert.Nil(t, pod.Spec.Containers[0].ReadinessProbe)
		assert.Nil(t, pod.Spec.Containers[0].LivenessProbe)
		assert.NotContains(t, pod.Spec.Containers[0].Command[len(pod.Spec.Containers[0].Command)-2:],
			"--port", "and the fill must not add a second one")
	})

	t.Run("an extra argument that is not an endpoint flag leaves all three gates in place", func(t *testing.T) {
		// THE BASELINE THE CASE ABOVE NEEDS. Without it, a guard that withheld the gates whenever a
		// role carried ANY extra argument would pass that case just as well. The second argument is
		// deliberately one whose name contains "ssl" without being part of the family, so a guard
		// matching on a substring rather than the flag prefix fails here.
		md := newRenderDeployment(func(md *workercore.ModelDeployment) {
			md.Spec.Roles[0].ExtraArgs = []string{"--max-model-len", "4096", "--served-model-name", "ssl-demo"}
		})

		pod, err := renderModelDeploymentPod(context.Background(), ModelDeploymentRenderInput{
			Deployment:      md,
			Role:            &md.Spec.Roles[0],
			AcceleratorType: newRenderAcceleratorType(),
		})
		require.NoError(t, err)

		require.NotNil(t, pod.Spec.Containers[0].StartupProbe,
			"an argument that does not move the port leaves the engine on the operator's own")
		require.NotNil(t, pod.Spec.Containers[0].ReadinessProbe,
			"and the readiness gate with it")
		assert.Equal(t, core.URISchemeHTTP, pod.Spec.Containers[0].StartupProbe.HTTPGet.Scheme,
			"and nothing moved the transport, so the gate stays plaintext")
	})

	testCases := []struct {
		name     string
		wantPort int32
	}{
		{
			name:     "a role naming no port is gated on the default every engine serves on",
			wantPort: modelDeploymentDefaultPort,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			md := newRenderDeployment()

			pod, err := renderModelDeploymentPod(context.Background(), ModelDeploymentRenderInput{
				Deployment:      md,
				Role:            &md.Spec.Roles[0],
				AcceleratorType: newRenderAcceleratorType(),
			})
			require.NoError(t, err)

			c := pod.Spec.Containers[0]
			require.NotNil(t, c.StartupProbe, "a slow loader needs a startup gate")
			require.NotNil(t, c.ReadinessProbe, "ready must not precede the engine serving")

			// A LIVENESS GATE ON THIS ROUTE IS SAFE ONLY BESIDE THE STARTUP GATE ABOVE. One
			// supported engine answers 503 for the whole load, and a liveness gate reading it alone
			// would restart a replica that is loading normally -- but the kubelet suppresses
			// liveness until startup succeeds, so it never sees that window.
			require.NotNil(t, c.LivenessProbe,
				"an engine that answered once and then stopped has no other way back")

			for name, entry := range map[string]struct {
				probe *core.Probe
				path  string
			}{
				"startup":   {c.StartupProbe, modelDeploymentEngineSentinelLivePath},
				"readiness": {c.ReadinessProbe, modelDeploymentEngineSentinelReadyPath},
				"liveness":  {c.LivenessProbe, modelDeploymentEngineSentinelLivePath},
			} {
				require.NotNil(t, entry.probe.HTTPGet, "%s gate must read the engine's route, not accept a socket", name)
				assert.Equal(t, entry.path, entry.probe.HTTPGet.Path, "%s gate path", name)
				assert.Equal(t, tc.wantPort, entry.probe.HTTPGet.Port.IntVal, "%s gate port", name)
			}

			// THE GATE AND THE SERVICE MUST NAME ONE PORT, and the comparison is against the
			// rendered Service rather than against the helper the render used -- comparing a value
			// with the function that produced it would hold however either side drifted.
			svc := renderModelDeploymentService(md, nil)
			require.Len(t, svc.Spec.Ports, 1)
			assert.Equal(t, svc.Spec.Ports[0].TargetPort.IntVal, c.ReadinessProbe.HTTPGet.Port.IntVal,
				"the port the gate grades and the port the Service sends traffic to are one fact")

			// The three gates are not one gate three times: the startup budget carries the load
			// window, readiness stays tight so a replica that has served once is taken out quickly,
			// and liveness sits between them so a stall costs readiness before it costs a restart.
			assert.Greater(t, c.StartupProbe.FailureThreshold, c.ReadinessProbe.FailureThreshold,
				"the startup gate carries the load window that readiness must not have to tolerate")
			assert.Greater(t, c.LivenessProbe.FailureThreshold, c.ReadinessProbe.FailureThreshold,
				"a restart throws away a loaded model, so it must cost more than losing readiness")
		})
	}
}

// TestRenderModelDeploymentPod_SerializedOutputIsPinnedToThePreSplitRender pins the WHOLE rendered
// Pod -- serialized and digested, not field-picked -- for one input off every branch of the
// renderer. The other cases in this file assert the fields they exist for, so a refactor of the
// render path could drop a field no case reads and every one of them would stay green; this is the
// case that cannot, because it compares the bytes of everything at once.
//
// THE ORIGINAL DIGESTS WERE CAPTURED FROM THE RENDERER AS IT STOOD BEFORE IT WAS SPLIT into a
// template half and a stamp half, and that split had to reproduce every one of them exactly. The
// table has been RE-BASELINED nine times since, each time for a rendering change that was
// intended: once for the per-replica groups, where the stamp began naming a (role, ordinal) group,
// declaring a total of one and writing the ordinal label; once when the ordinal label's key took the
// modeldeployment prefix the role-kind label and the spec-hash annotation already carry, so that one
// reader looking for this deployment's own keys finds all of them under one prefix; once when
// the member index became unconditional on every member, so that one equality term in a discovery
// selector names the Pods that answer the API at every role size; once when each managed Pod
// began declaring its metrics listener scheme; and once when every Pod whose command line the
// operator builds began carrying the drain hook and the termination grace it is budgeted against,
// which left the take-over case's digest where it was. The shared-memory volume then changed
// every role's Pod, including take-over roles. Every digest here moved by intent
// rather than by drift. The two direct-decoder cases then moved when the default routing sidecar
// image advanced from v0.10.0 to v0.11.0, which is the only thing in those two Pods that carries
// the setting; the seven cases rendering no sidecar stayed where they were, naming the move to
// the setting's value the same way the router table does. The sixth re-baselining is the
// compilation-cache and startup-probe update: every managed Pod gained the compilation cache
// volume, mount and cache-root entry alongside a wider probe timeout, leaving the take-over case's
// digest unchanged. The seventh is the runtime-cache update: every managed Pod gained the four
// cache-root entries that keep an engine's startup caches on the mounted cache instead of on the
// container's writable layer, and the take-over case's digest stayed where it was for the same
// reason the sixth left it: a role that brings its own command line is told nothing about a cache
// it does not have. The eighth re-baselining moves every digest, the take-over case's included: the
// runtime-cache update was narrowed from one XDG root to the scoped cache names huggingface_hub,
// hf_xet, pip and torch read, so the Hub token and the datasets cache stay on the container's own
// filesystem instead of a host-backed mount, and the render-version annotation every Pod carries
// advanced to 4 with that change. The ninth re-baselining advances render-version to 6 and injects
// the read-only gpustack-engine-sentinel package, unified sentinel probes and sentinel PreStop drain
// module across managed accelerated roles. Each case renders ORDINAL ZERO -- the
// composite's zero value -- which is the
// one ordinal a digest can name without the table growing a dimension. To re-baseline after an
// intended rendering change: empty the table, run this case, and pin the digests the failures
// print.
func TestRenderModelDeploymentPod_SerializedOutputIsPinnedToThePreSplitRender(t *testing.T) {
	pinned := map[string]string{
		"a sole server role": "7ad675fa1d3551c0843ec690f1c0e994bf6df78e192e3341b619eced6cc9f09f",
		"a sole server role with a synthesized cache connector":                              "41ec47248abc38a498e5821795203467fa339bb30634854fe98dbd9fb05a47b5",
		"a take-over role carrying a connector it must be given no part of":                  "f3f26080d4924c1a2c7304f8560197b2cc0683b45751552e4c895fabe6376725",
		"a direct decoder with a native routing sidecar":                                     "1d166efa56e21a15d7b53c2998e6f257b730b0ad6d63fa1eb0da8ba456ec4b50",
		"a direct decoder with a classic routing sidecar":                                    "b15217ef40a6ce84c30f061f84698b610616e962610a578e9f536a208f61447a",
		"a role naming no image, which runs the engine's image":                              "7ad675fa1d3551c0843ec690f1c0e994bf6df78e192e3341b619eced6cc9f09f",
		"a TLS-listening role with declared ports, privileges, a runtime class and a volume": "ad7ceeefffbc4cf541bd6babc80004d8a72d1a4ed8dc981fa6690b5aa02470d7",
		"the prefill role of a two-role deployment":                                          "3df99cb5b902a54725480a6b4e2530878a9a17d1feed1c4f1efb70f27c59171f",
		"the decode role of a two-role deployment":                                           "cbadbdca81a66b14e7f4ace397324680dae9b99aa0de122c13c864aa14a3c4c7",
	}

	// newPinnedInput builds the render input the way the reconciler does: the deployment and its
	// role as one object, the InstanceType resolved beside it. Every case calls its builder again
	// for a second render rather than reusing one Pod, so a digest also states that two renders of
	// one input agree -- reusing an object would state nothing a copy had not already said.
	newPinnedInput := func(
		t *testing.T, roleIndex int, mutate ...func(*workercore.ModelDeployment),
	) ModelDeploymentRenderInput {
		t.Helper()

		md := newRenderDeployment(mutate...)

		return ModelDeploymentRenderInput{
			Deployment:      md,
			Role:            &md.Spec.Roles[roleIndex],
			AcceleratorType: newRenderAcceleratorType(),
		}
	}

	synthesizedConnector := func(t *testing.T) ModelDeploymentConnectorRender {
		t.Helper()

		conn, err := SynthesizeModelDeploymentConnector(
			connectorInput(workercore.ModelDeploymentEngineVLLM, nodefeature.ManufacturerNVIDIA),
		)
		require.NoError(t, err)

		return conn
	}

	testCases := []struct {
		name  string
		input func(*testing.T) ModelDeploymentRenderInput
	}{
		{
			name:  "a sole server role",
			input: func(t *testing.T) ModelDeploymentRenderInput { return newPinnedInput(t, 0) },
		},
		{
			name: "a sole server role with a synthesized cache connector",
			input: func(t *testing.T) ModelDeploymentRenderInput {
				in := newPinnedInput(t, 0)
				in.Connector = synthesizedConnector(t)

				return in
			},
		},
		{
			name: "a take-over role carrying a connector it must be given no part of",
			input: func(t *testing.T) ModelDeploymentRenderInput {
				in := newPinnedInput(t, 0, func(md *workercore.ModelDeployment) {
					md.Spec.Roles[0].Command = []string{"/bin/my-server", "--flag"}
				})
				in.Connector = synthesizedConnector(t)

				return in
			},
		},
		{
			name: "a direct decoder with a native routing sidecar",
			input: func(t *testing.T) ModelDeploymentRenderInput {
				in := newPinnedInput(t, 0, func(md *workercore.ModelDeployment) {
					md.Spec.Router = &workercore.ModelDeploymentRouter{Name: workercore.ModelDeploymentRouterLLMD}
					md.Spec.Roles[0].Kind = workercore.ModelDeploymentRoleKindDecode
					md.Spec.Roles[0].Ports = []workercore.ModelDeploymentPort{{
						Protocol: core.ProtocolTCP, Port: 8000,
					}}
				})
				in.Connector = ModelDeploymentConnectorRender{
					Args: []string{"--kv-transfer-config", `{}`}, KVTransfer: true, RoutingSidecar: true,
				}
				in.NativeSidecar = true

				return in
			},
		},
		{
			name: "a direct decoder with a classic routing sidecar",
			input: func(t *testing.T) ModelDeploymentRenderInput {
				in := newPinnedInput(t, 0, func(md *workercore.ModelDeployment) {
					md.Spec.Router = &workercore.ModelDeploymentRouter{Name: workercore.ModelDeploymentRouterLLMD}
					md.Spec.Roles[0].Kind = workercore.ModelDeploymentRoleKindDecode
					md.Spec.Roles[0].Ports = []workercore.ModelDeploymentPort{{
						Protocol: core.ProtocolTCP, Port: 8000,
					}}
				})
				in.Connector = ModelDeploymentConnectorRender{
					Args: []string{"--kv-transfer-config", `{}`}, KVTransfer: true, RoutingSidecar: true,
				}
				in.NativeSidecar = false

				return in
			},
		},
		{
			name: "a role naming no image, which runs the engine's image",
			input: func(t *testing.T) ModelDeploymentRenderInput {
				return newPinnedInput(t, 0, func(md *workercore.ModelDeployment) {
					md.Spec.Roles[0].Image = ""
				})
			},
		},
		{
			name: "a TLS-listening role with declared ports, privileges, a runtime class and a volume",
			input: func(t *testing.T) ModelDeploymentRenderInput {
				in := newPinnedInput(t, 0, func(md *workercore.ModelDeployment) {
					md.Spec.Roles[0].Ports = []workercore.ModelDeploymentPort{{
						Protocol: core.ProtocolTCP, Port: 9000,
					}}
					md.Spec.Roles[0].ExtraArgs = []string{"--ssl-certfile", "/etc/tls/tls.crt"}
					md.Spec.Roles[0].Privileged = true
					md.Spec.Roles[0].AdditionalVolumes = []workercore.ModelDeploymentAdditionalVolume{{
						MountPath: "/models",
						HostPath:  &core.HostPathVolumeSource{Path: "/mnt/models"},
					}}
				})
				in.RuntimeClassName = "nvidia"

				return in
			},
		},
		{
			name: "the prefill role of a two-role deployment",
			input: func(t *testing.T) ModelDeploymentRenderInput {
				return newPinnedInput(t, 0, twoRoleDeploymentMutations()...)
			},
		},
		{
			name: "the decode role of a two-role deployment",
			input: func(t *testing.T) ModelDeploymentRenderInput {
				return newPinnedInput(t, 1, twoRoleDeploymentMutations()...)
			},
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			pod, err := renderModelDeploymentPod(context.Background(), tc.input(t))
			require.NoError(t, err)

			encoded, err := json.Marshal(pod)
			require.NoError(t, err)
			digest := stringx.SumBytesBySHA256(encoded)

			want, ok := pinned[tc.name]
			if !ok {
				t.Fatalf("no pinned digest for %q; the digest this build produces is %s -- pin it in the table", tc.name, digest)
			}
			assert.Equal(t, want, digest)
		})
	}
}

// twoRoleDeploymentMutations turns the single-role fixture into a prefill/decode pair, keeping the
// second role on the same InstanceType so both halves render rather than one failing to size.
func twoRoleDeploymentMutations() []func(*workercore.ModelDeployment) {
	return []func(*workercore.ModelDeployment){
		func(md *workercore.ModelDeployment) {
			md.Spec.Roles[0].Name = "prefill"
			md.Spec.Roles[0].Kind = workercore.ModelDeploymentRoleKindPrefill
		},
		func(md *workercore.ModelDeployment) {
			md.Spec.Roles = append(md.Spec.Roles, workercore.ModelDeploymentRole{
				Name:            "decode",
				Replicas:        2,
				AcceleratorType: "h20-8x",
				Kind:            workercore.ModelDeploymentRoleKindDecode,
				Image:           "vllm/vllm-openai:v0.25.1",
			})
		},
	}
}

// TestRenderModelDeploymentPod_TemplateCarriesNoGroupMetadataOrHash states the boundary the split
// exists to enforce: the template half renders what a role's spec states and NOTHING that names one
// replica's group membership or fingerprint, and the stamp half is what puts those on.
//
// BOTH HALVES ARE NEEDED OR THE CASE PROVES NOTHING. "The template lacks these keys" is true of a
// template that renders nothing at all, so beside the absences stand presences: the template's own
// output carries the metadata it IS responsible for, and the same Pod -- stamped -- carries all
// four keys it is not. A per-replica value leaking into the template is also silent: it renders a
// wrong Pod rather than erroring, which is why the boundary is asserted here rather than left to
// placement.
//
// The Kueue keys are spelled literally rather than read off the constants because a constant that
// drifted would satisfy an assertion built from it while the real key rode through the template.
func TestRenderModelDeploymentPod_TemplateCarriesNoGroupMetadataOrHash(t *testing.T) {
	md := newRenderDeployment()
	role := &md.Spec.Roles[0]

	template, err := renderModelDeploymentPodTemplate(context.Background(), ModelDeploymentRenderInput{
		Deployment:      md,
		Role:            role,
		AcceleratorType: newRenderAcceleratorType(),
	})
	require.NoError(t, err)

	assert.NotContains(t, template.Labels, "kueue.x-k8s.io/pod-group-name")
	assert.NotContains(t, template.Labels, modelDeploymentReplicaOrdinalLabel)
	assert.NotContains(t, template.Annotations, "kueue.x-k8s.io/pod-group-total-count")
	assert.NotContains(t, template.Annotations, "kueue.x-k8s.io/role-hash")
	assert.NotContains(t, template.Annotations, modelDeploymentPodSpecHashAnnotation)

	// The positive baseline for the absences above: this is a rendered template, not an empty one,
	// and it carries the metadata of its own -- the identity labels, the resource note a watch
	// filters on, the controller reference that makes the Pod ours.
	assert.Equal(t, "qwen-server-", template.GenerateName)
	require.Len(t, template.Spec.Containers, 1)
	require.Len(t, template.OwnerReferences, 1)
	assert.True(t, systemmeta.MatchResource(template, ModelDeploymentResourceType))

	stampModelDeploymentPod(template, md, role, 0, 0)

	assert.Equal(t, modelDeploymentReplicaGroupName(md, role.Name, 0),
		template.Labels["kueue.x-k8s.io/pod-group-name"],
		"a replica's group is its own: the name is derived for its (role, ordinal) alone")
	assert.Equal(t, "1", template.Annotations["kueue.x-k8s.io/pod-group-total-count"],
		"the group declares one member: the replica the stamp names")
	assert.Equal(t, "0", template.Labels[modelDeploymentReplicaOrdinalLabel],
		"the ordinal is stamped beside the membership it derives")
	assert.Equal(t, "server", template.Annotations["kueue.x-k8s.io/role-hash"],
		"the role hash stays the role's own name")
	assert.Contains(t, template.Annotations, modelDeploymentPodSpecHashAnnotation)
}

// TestStampModelDeploymentPod_AboveOneMemberNamesAndAddressesEachMember covers what the stamp does
// once a replica is more than one Pod, and the assertions are chosen so that each one fails for a
// different reason.
//
// THE MEMBERS MUST SHARE A GROUP AND DIFFER IN NOTHING ELSE THE RENDER CONTROLS. Sharing the group
// is what makes them one admission; differing only in name, hostname and member index is what keeps
// the template comparable, which is the Boundaries invariant at this size.
func TestStampModelDeploymentPod_AboveOneMemberNamesAndAddressesEachMember(t *testing.T) {
	md := newRenderDeployment(func(md *workercore.ModelDeployment) {
		md.Spec.Roles[0].ReplicaSize = 3
	})
	role := &md.Spec.Roles[0]

	stamp := func(ordinal, member int) *core.Pod {
		t.Helper()

		pod, err := renderModelDeploymentPodTemplate(context.Background(), ModelDeploymentRenderInput{
			Deployment: md, Role: role, AcceleratorType: newRenderAcceleratorType(),
		})
		require.NoError(t, err)
		stampModelDeploymentPod(pod, md, role, ordinal, member)

		return pod
	}

	leader, worker := stamp(0, 0), stamp(0, 1)

	// The name is DERIVED, which is what lets a sibling address it before it exists. GenerateName
	// is cleared with it: a Pod carrying both would be named by the API server and the derived name
	// would be the one nobody could reach.
	assert.Equal(t, "qwen-server-r0-m0", leader.Name)
	assert.Equal(t, "qwen-server-r0-m1", worker.Name)
	assert.Empty(t, leader.GenerateName)

	// hostname and subdomain are what make the name resolvable, and the subdomain is the SAME for
	// both -- it names the replica's headless Service, which is the thing they are published behind.
	assert.Equal(t, "qwen-server-r0-m0", leader.Spec.Hostname)
	assert.Equal(t, "qwen-server-r0-m1", worker.Spec.Hostname)
	assert.Equal(t, "qwen-server-r0", leader.Spec.Subdomain)
	assert.Equal(t, leader.Spec.Subdomain, worker.Spec.Subdomain)

	// ONE GROUP, DECLARING ALL THREE. This is the assertion that makes them one admission rather
	// than three, and the total is the role's size rather than the count of members stamped so far.
	assert.Equal(t, leader.Labels["kueue.x-k8s.io/pod-group-name"],
		worker.Labels["kueue.x-k8s.io/pod-group-name"],
		"members of one replica share its group: they are admitted together or not at all")
	assert.Equal(t, "3", leader.Annotations["kueue.x-k8s.io/pod-group-total-count"])
	assert.Equal(t, "3", worker.Annotations["kueue.x-k8s.io/pod-group-total-count"])
	assert.Equal(t, "server", leader.Annotations["kueue.x-k8s.io/role-hash"],
		"the role hash names the PodSet, so it stays the role's -- the member index is not in it")
	assert.Equal(t, leader.Annotations["kueue.x-k8s.io/role-hash"],
		worker.Annotations["kueue.x-k8s.io/role-hash"])

	// The index is a LABEL, because a Service selector has to be able to match the leader and a
	// selector cannot express "the Pod whose name ends in -m0".
	assert.Equal(t, "0", leader.Labels[modelDeploymentMemberIndexLabel])
	assert.Equal(t, "1", worker.Labels[modelDeploymentMemberIndexLabel])

	// A SECOND REPLICA SHARES NOTHING OF THE FIRST'S IDENTITY, which is Goal 1 at this size: its
	// members are named apart and its group is its own, so neither replica's admission touches the
	// other's.
	second := stamp(1, 0)
	assert.Equal(t, "qwen-server-r1-m0", second.Name)
	assert.Equal(t, "qwen-server-r1", second.Spec.Subdomain)
	assert.NotEqual(t, leader.Labels["kueue.x-k8s.io/pod-group-name"],
		second.Labels["kueue.x-k8s.io/pod-group-name"])
}

// TestStampModelDeploymentPod_MembersOfAReplicaDifferOnlyInTheirIdentity is the Boundaries invariant
// stated at the size where it is least obvious.
//
// THE CONTAINER SPEC IS WHERE A RANK LAYOUT WOULD LEAK IN, and the whole reason the member index
// travels as a label is that a renderer writing it into env instead would make two members of one
// replica hash differently -- and every member would then read as a pending rollout, forever, with
// nothing erroring. So the comparison is on the serialized PodSpec with the two per-member fields
// blanked: anything else that differs is a defect this case exists to name.
func TestStampModelDeploymentPod_MembersOfAReplicaDifferOnlyInTheirIdentity(t *testing.T) {
	renderMember := func(md *workercore.ModelDeployment, member int) *core.Pod {
		t.Helper()

		role := &md.Spec.Roles[0]
		pod, err := renderModelDeploymentPodTemplate(context.Background(), ModelDeploymentRenderInput{
			Deployment: md, Role: role, AcceleratorType: newRenderAcceleratorType(),
		})
		require.NoError(t, err)
		stampModelDeploymentPod(pod, md, role, 0, member)
		return pod
	}

	t.Run("leader-served multi-member replica clears follower probes and matches everywhere else", func(t *testing.T) {
		md := newRenderDeployment(func(md *workercore.ModelDeployment) {
			md.Spec.Roles[0].ReplicaSize = 4
		})

		leader := renderMember(md, 0)
		require.NotNil(t, leader.Spec.Containers[0].StartupProbe)
		require.NotNil(t, leader.Spec.Containers[0].ReadinessProbe)
		require.NotNil(t, leader.Spec.Containers[0].LivenessProbe)

		normalize := func(pod *core.Pod, clearProbes bool) string {
			cp := pod.DeepCopy()
			cp.Spec.Hostname = ""
			if clearProbes {
				clearModelDeploymentMainContainerProbes(cp)
			}
			encoded, err := json.Marshal(cp.Spec)
			require.NoError(t, err)
			return string(encoded)
		}

		leaderNormalized := normalize(leader, true)
		var firstFollower string
		for member := 1; member < 4; member++ {
			follower := renderMember(md, member)
			assert.Nil(t, follower.Spec.Containers[0].StartupProbe)
			assert.Nil(t, follower.Spec.Containers[0].ReadinessProbe)
			assert.Nil(t, follower.Spec.Containers[0].LivenessProbe)

			followerSpec := normalize(follower, false)
			if member == 1 {
				firstFollower = followerSpec
			} else {
				assert.Equal(t, firstFollower, followerSpec,
					"follower %d's PodSpec differs from follower 1's beyond its hostname", member)
			}
			assert.Equal(t, leaderNormalized, followerSpec,
				"member %d's PodSpec differs from the leader's beyond hostname and leader-only probes", member)
		}
	})

	t.Run("external-DP multi-member replica keeps probes on every member and differs only in hostname", func(t *testing.T) {
		md := newRenderDeployment(func(md *workercore.ModelDeployment) {
			md.Spec.Roles[0].ReplicaSize = 4
			md.Spec.Roles[0].ExtraArgs = []string{
				"--data-parallel-size=4",
				"--data-parallel-size-local=1",
				"--data-parallel-external-lb",
			}
		})

		specOf := func(member int) string {
			pod := renderMember(md, member)
			require.NotNil(t, pod.Spec.Containers[0].ReadinessProbe)
			pod.Spec.Hostname = ""
			encoded, err := json.Marshal(pod.Spec)
			require.NoError(t, err)
			return string(encoded)
		}

		first := specOf(0)
		for member := 1; member < 4; member++ {
			assert.Equal(t, first, specOf(member),
				"member %d's PodSpec differs from member 0's beyond its hostname", member)
		}
	})
}

// renderStampedMember is one member of one replica, rendered and stamped the way the converger does.
func renderStampedMember(
	t *testing.T, md *workercore.ModelDeployment, ordinal, member int,
) *core.Pod {
	t.Helper()

	pod, err := renderModelDeploymentPodTemplate(context.Background(), ModelDeploymentRenderInput{
		Deployment: md, Role: &md.Spec.Roles[0], AcceleratorType: newRenderAcceleratorType(),
	})
	require.NoError(t, err)
	stampModelDeploymentPod(pod, md, &md.Spec.Roles[0], ordinal, member)

	return pod
}

// mainContainerEnv is the main container's environment, keyed by name. It fails rather than returns
// empty when there is no main container: an assertion over a map nobody filled passes for "the
// variable is absent" and for "the container this test is about was renamed".
func mainContainerEnv(t *testing.T, pod *core.Pod) map[string]core.EnvVar {
	t.Helper()

	for i := range pod.Spec.Containers {
		if pod.Spec.Containers[i].Name != modelDeploymentMainContainerName {
			continue
		}
		env := make(map[string]core.EnvVar, len(pod.Spec.Containers[i].Env))
		for _, e := range pod.Spec.Containers[i].Env {
			env[e.Name] = e
		}

		return env
	}
	require.FailNow(t, "the rendered Pod carries no main container")

	return nil
}

// TestStampModelDeploymentPod_AMultiMemberInstancePublishesItsRankLayout is the rank layout an
// instance of several Pods hands its engine: who to talk to, how many there are, and which one this
// is.
//
// THE INDEX IS ASSERTED AS A fieldRef AND NEVER AS A VALUE, which is the half a test reading only
// the resolved rank would miss. A literal index would satisfy "the container knows its rank" while
// making the container spec a per-member document -- and the invariant that one template describes a
// whole replica is what the sibling case above measures.
func TestStampModelDeploymentPod_AMultiMemberInstancePublishesItsRankLayout(t *testing.T) {
	md := newRenderDeployment(func(md *workercore.ModelDeployment) {
		md.Spec.Roles[0].ReplicaSize = 3
	})

	env := mainContainerEnv(t, renderStampedMember(t, md, 2, 1))

	assert.Equal(t, "qwen-server-r2-m0.qwen-server-r2",
		env[modelDeploymentLeaderAddressEnv].Value,
		"the leader address names member zero of THIS replica under its own headless Service")
	assert.Equal(t, "3", env[modelDeploymentReplicaSizeEnv].Value,
		"the size is the role's, not the deployment's replica count")

	index := env[modelDeploymentMemberIndexEnv]
	assert.Empty(t, index.Value,
		"the index is read from the label rather than written in, so no literal belongs here")
	require.NotNil(t, index.ValueFrom, "the index carries no source at all")
	require.NotNil(t, index.ValueFrom.FieldRef, "the index's source is not a downward-API fieldRef")
	assert.Equal(t, "metadata.labels['"+modelDeploymentMemberIndexLabel+"']",
		index.ValueFrom.FieldRef.FieldPath,
		"the fieldRef names a different key than the one the stamp writes, so it resolves to nothing")

	// THE LABEL THE fieldRef POINTS AT HAS TO BE THERE. The kubelet fails the Pod on a fieldRef to a
	// label that does not exist, so the two halves are one fact and a test of either alone passes
	// while the Pod cannot start.
	assert.Equal(t, "1", renderStampedMember(t, md, 2, 1).Labels[modelDeploymentMemberIndexLabel],
		"the member index label the fieldRef reads is missing or holds the wrong member")
}

// TestStampModelDeploymentPod_ASingleMemberInstancePublishesNoRankLayout states the rule the pinned
// digest enforces but does not explain: at size one there is no collective, so there is no rank.
//
// It is not redundant with that digest. The digest fails on ANY difference and names none of them,
// so it reports "the render moved" where this reports which promise was broken.
func TestStampModelDeploymentPod_ASingleMemberInstancePublishesNoRankLayout(t *testing.T) {
	env := mainContainerEnv(t, renderStampedMember(t, newRenderDeployment(), 0, 0))

	for _, name := range modelDeploymentRankEnvNames {
		assert.NotContains(t, env, name,
			"a single-Pod instance carries %s, which no engine can act on and which moves the "+
				"fingerprint of every deployment that never asked for a multi-Pod instance", name)
	}
}

// TestRenderModelDeploymentPodTemplate_RendersTwoReplicasOfOneRoleIdentical is the invariant that
// keeps replica identity out of the render path: two replicas of one role receive the SAME
// template.
//
// THE INPUT IS THE ONLY ROAD A REPLICA'S IDENTITY COULD TAKE, and it carries none today -- the
// render takes no ordinal and no name -- so "two replicas of one role" is the same fixture rendered
// twice, from freshly built inputs the way two reconcile passes would build them. The comparison is
// on serialized bytes rather than picked fields, because a difference in a field no assertion reads
// is exactly the difference this case exists to catch.
func TestRenderModelDeploymentPodTemplate_RendersTwoReplicasOfOneRoleIdentical(t *testing.T) {
	testCases := []struct {
		name  string
		input func(*testing.T) ModelDeploymentRenderInput
	}{
		{
			name: "a server role",
			input: func(t *testing.T) ModelDeploymentRenderInput {
				md := newRenderDeployment()

				return ModelDeploymentRenderInput{
					Deployment: md, Role: &md.Spec.Roles[0], AcceleratorType: newRenderAcceleratorType(),
				}
			},
		},
		{
			name: "a direct decoder with a native routing sidecar",
			input: func(t *testing.T) ModelDeploymentRenderInput {
				md := newRenderDeployment(func(md *workercore.ModelDeployment) {
					md.Spec.Router = &workercore.ModelDeploymentRouter{Name: workercore.ModelDeploymentRouterLLMD}
					md.Spec.Roles[0].Kind = workercore.ModelDeploymentRoleKindDecode
					md.Spec.Roles[0].Ports = []workercore.ModelDeploymentPort{{
						Protocol: core.ProtocolTCP, Port: 8000,
					}}
				})

				return ModelDeploymentRenderInput{
					Deployment: md, Role: &md.Spec.Roles[0], AcceleratorType: newRenderAcceleratorType(),
					Connector: ModelDeploymentConnectorRender{
						Args: []string{"--kv-transfer-config", `{}`}, KVTransfer: true, RoutingSidecar: true,
					},
					NativeSidecar: true,
				}
			},
		},
		{
			name: "a take-over role",
			input: func(t *testing.T) ModelDeploymentRenderInput {
				md := newRenderDeployment(func(md *workercore.ModelDeployment) {
					md.Spec.Roles[0].Command = []string{"/bin/my-server", "--flag"}
				})

				return ModelDeploymentRenderInput{
					Deployment: md, Role: &md.Spec.Roles[0], AcceleratorType: newRenderAcceleratorType(),
				}
			},
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			first, err := renderModelDeploymentPodTemplate(context.Background(), tc.input(t))
			require.NoError(t, err)
			second, err := renderModelDeploymentPodTemplate(context.Background(), tc.input(t))
			require.NoError(t, err)

			// The baseline the comparison needs: the template rendered a Pod, or equality below
			// would hold between two empty objects and prove nothing.
			require.Len(t, first.Spec.Containers, 1)

			firstJSON, err := json.Marshal(first)
			require.NoError(t, err)
			secondJSON, err := json.Marshal(second)
			require.NoError(t, err)

			assert.Equal(t, string(firstJSON), string(secondJSON))
		})
	}
}

// TestRenderModelDeploymentPod_DecodeWithoutTheProxyFlagRunsAlone is the row the sidecar cases
// cannot carry, because every one of them sets both flags at once.
//
// A decoder under a router configured by argv HAS the engine-side transfer leg and NOT the proxy:
// it learns where to pull from out of the request body, which the engine reads for itself. A
// renderer deriving the proxy from the leg -- which is what it used to do -- passes every other
// case in this file and puts that proxy on this Pod, where it would wait for a header nothing
// sends and the replica would never answer.
func TestRenderModelDeploymentPod_DecodeWithoutTheProxyFlagRunsAlone(t *testing.T) {
	md := newRenderDeployment(func(md *workercore.ModelDeployment) {
		md.Spec.Router = &workercore.ModelDeploymentRouter{
			Name: workercore.ModelDeploymentRouterVLLM,
		}
		md.Spec.Roles[0].Kind = workercore.ModelDeploymentRoleKindDecode
	})

	pod, err := renderModelDeploymentPod(context.Background(), ModelDeploymentRenderInput{
		Deployment: md, Role: &md.Spec.Roles[0], AcceleratorType: newRenderAcceleratorType(),
		Connector: ModelDeploymentConnectorRender{
			Args: []string{"--kv-transfer-config", `{}`}, KVTransfer: true,
		},
		NativeSidecar: true,
	})
	require.NoError(t, err)

	assert.Empty(t, pod.Spec.InitContainers, "no proxy is rendered for this router")
	require.Len(t, pod.Spec.Containers, 1, "the engine runs alone")
	assert.Equal(t, "main", pod.Spec.Containers[0].Name)
	// And the engine keeps the port its Service publishes, because nothing took it.
	assert.Contains(t, pod.Spec.Containers[0].Command, "--port")
	at := slices.Index(pod.Spec.Containers[0].Command, "--port")
	require.Less(t, at+1, len(pod.Spec.Containers[0].Command))
	assert.Equal(t, "8000", pod.Spec.Containers[0].Command[at+1],
		"the proxy is what moves the engine off the published port, and there is none")
}

// newTCPTWReusePair builds a deployment declaring both halves of an SGLang pair behind the managed
// router, which is the shape whose prefill half the tcp_tw_reuse setting targets.
func newTCPTWReusePair(mutate ...func(*workercore.ModelDeployment)) *workercore.ModelDeployment {
	md := newRenderDeployment(func(md *workercore.ModelDeployment) {
		md.Spec.Engine = workercore.ModelDeploymentEngine{
			Name: workercore.ModelDeploymentEngineSGLang, Image: "lmsysorg/sglang:v0.5.18",
		}
		md.Spec.Router = &workercore.ModelDeploymentRouter{Name: workercore.ModelDeploymentRouterLLMD}
		md.Spec.Roles = []workercore.ModelDeploymentRole{
			{
				Name: "prefill", Kind: workercore.ModelDeploymentRoleKindPrefill,
				Replicas: 1, AcceleratorType: "h20-8x", Image: "lmsysorg/sglang:v0.5.18",
			},
			{
				Name: "decode", Kind: workercore.ModelDeploymentRoleKindDecode,
				Replicas: 1, AcceleratorType: "h20-8x", Image: "lmsysorg/sglang:v0.5.18",
			},
		}
	})
	for _, m := range mutate {
		m(md)
	}

	return md
}

// TestRenderModelDeploymentPod_TCPTWReuse pins where the tcp_tw_reuse setting lands: on the
// prefill half of an SGLang pair, the side that opens a new TCP connection for every transfer and
// so holds the TIME-WAIT sockets, and on no other Pod. Every row renders twice, with the setting on
// and off, so each target row is its own baseline: a render that never wrote the sysctl fails
// there, and a render that wrote it everywhere fails on the rows that must stay untouched.
//
// The spec hash is compared across the two renders as well, because it is how flipping the setting
// reaches a running replica: a target Pod must move so the recreate rollout replaces it, and every
// other Pod must keep its fingerprint so the flip rolls nothing else.
func TestRenderModelDeploymentPod_TCPTWReuse(t *testing.T) {
	store := ModelDeploymentConnectorInput{MasterServerAddress: "master:50051", Protocols: []string{"tcp"}}

	for _, tc := range []struct {
		name      string
		md        *workercore.ModelDeployment
		role      string
		kvStore   bool
		kvDirect  bool
		wantReuse bool
	}{
		{
			name: "an SGLang prefill half with a store over TCP",
			md:   newTCPTWReusePair(), role: "prefill", kvStore: true, kvDirect: true, wantReuse: true,
		},
		{
			name: "an SGLang prefill half with no store, which runs out of ports as well",
			md:   newTCPTWReusePair(), role: "prefill", kvDirect: true, wantReuse: true,
		},
		{
			name: "an SGLang decode half, which accepts transfers rather than opening them",
			md:   newTCPTWReusePair(), role: "decode", kvStore: true, kvDirect: true,
		},
		{
			name: "a vLLM prefill half, whose transfer connections persist",
			md: newTCPTWReusePair(func(md *workercore.ModelDeployment) {
				md.Spec.Engine = workercore.ModelDeploymentEngine{
					Name: workercore.ModelDeploymentEngineVLLM, Image: "vllm/vllm-openai:v0.29.0",
				}
			}),
			role: "prefill", kvStore: true, kvDirect: true,
		},
		{
			name: "an SGLang server with a store, which is not one half of a pair",
			md: newTCPTWReusePair(func(md *workercore.ModelDeployment) {
				md.Spec.Router = nil
				md.Spec.Roles = md.Spec.Roles[:1]
				md.Spec.Roles[0].Name = "server"
				md.Spec.Roles[0].Kind = workercore.ModelDeploymentRoleKindServer
			}),
			role: "server", kvStore: true,
		},
		{
			name: "a lone SGLang prefill role with a store, which runs undivided",
			md: newTCPTWReusePair(func(md *workercore.ModelDeployment) {
				md.Spec.Roles = md.Spec.Roles[:1]
			}),
			role: "prefill", kvStore: true,
		},
		{
			name: "an SGLang prefill half that replaced its command line",
			md: newTCPTWReusePair(func(md *workercore.ModelDeployment) {
				md.Spec.Roles[0].Command = []string{"/bin/my-prefill"}
			}),
			role: "prefill", kvStore: true, kvDirect: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var role *workercore.ModelDeploymentRole
			for i := range tc.md.Spec.Roles {
				if tc.md.Spec.Roles[i].Name == tc.role {
					role = &tc.md.Spec.Roles[i]
				}
			}
			require.NotNil(t, role)

			in := ModelDeploymentConnectorInput{}
			if tc.kvStore {
				in = store
			}
			in.Engine = tc.md.Spec.Engine.Name
			in.Kind = role.Kind
			in.Disaggregated = ModelDeploymentDeclaresBothHalves(tc.md)
			in.KVTransfer = tc.kvDirect
			connector, err := SynthesizeModelDeploymentConnector(in)
			require.NoError(t, err)

			render := func(reuse bool) *core.Pod {
				pod, err := renderModelDeploymentPod(context.Background(), ModelDeploymentRenderInput{
					Deployment: tc.md, Role: role, AcceleratorType: newRenderAcceleratorType(),
					Connector: connector, TCPTWReuse: reuse,
				})
				require.NoError(t, err)

				return pod
			}
			on, off := render(true), render(false)

			assert.Nil(t, off.Spec.SecurityContext,
				"with the setting off the Pod renders exactly as it did before the setting existed")
			onHash := on.Annotations[modelDeploymentPodSpecHashAnnotation]
			offHash := off.Annotations[modelDeploymentPodSpecHashAnnotation]
			if !tc.wantReuse {
				assert.Nil(t, on.Spec.SecurityContext, "the setting reaches no Pod outside its target")
				assert.Equal(t, offHash, onHash, "so flipping it rolls nothing here")

				return
			}
			require.NotNil(t, on.Spec.SecurityContext)
			assert.Equal(t, []core.Sysctl{{Name: "net.ipv4.tcp_tw_reuse", Value: "1"}},
				on.Spec.SecurityContext.Sysctls)
			assert.Equal(t, offHash, onHash,
				"the setting is an implicit operator knob and does not move the explicit spec hash")
		})
	}
}

// TestRenderModelDeploymentPod_TCPTWReuseKeepsPrivileged pins the merge with what a role states
// itself. A role has no Pod-level security context of its own -- `privileged` is the only security
// field it carries, and it lands on the main container -- so the sysctl is written beside it and
// takes nothing from it.
func TestRenderModelDeploymentPod_TCPTWReuseKeepsPrivileged(t *testing.T) {
	md := newTCPTWReusePair(func(md *workercore.ModelDeployment) { md.Spec.Roles[0].Privileged = true })
	connector, err := SynthesizeModelDeploymentConnector(ModelDeploymentConnectorInput{
		Engine: md.Spec.Engine.Name, Kind: workercore.ModelDeploymentRoleKindPrefill,
		Disaggregated: true, KVTransfer: true,
	})
	require.NoError(t, err)

	pod, err := renderModelDeploymentPod(context.Background(), ModelDeploymentRenderInput{
		Deployment: md, Role: &md.Spec.Roles[0], AcceleratorType: newRenderAcceleratorType(),
		Connector: connector, TCPTWReuse: true,
	})
	require.NoError(t, err)

	require.NotNil(t, pod.Spec.SecurityContext)
	assert.Equal(t, []core.Sysctl{{Name: "net.ipv4.tcp_tw_reuse", Value: "1"}}, pod.Spec.SecurityContext.Sysctls)
	require.NotNil(t, pod.Spec.Containers[0].SecurityContext)
	assert.Equal(t, ptr.To(true), pod.Spec.Containers[0].SecurityContext.Privileged,
		"the role's own privileged mode stays on its container")
}

// TestRenderModelDeploymentPods_TCPTWReuseFollowsTheSetting pins the one read of the setting: the
// reconciler takes it once per pass and hands it to every role, and only the prefill half turns it
// into a sysctl. The default-off pass is the baseline that shows the read is what moves the render.
func TestRenderModelDeploymentPods_TCPTWReuseFollowsTheSetting(t *testing.T) {
	sysctls := func(t *testing.T) map[string][]core.Sysctl {
		t.Helper()

		md := newTCPTWReusePair()
		r := &ModelDeploymentReconciler{Client: newModelDeploymentClient(md, newRenderAcceleratorType())}
		desired, err := r.renderModelDeploymentPods(context.Background(), md, nil, nil, nil)
		require.NoError(t, err)

		got := map[string][]core.Sysctl{}
		for role, replicas := range desired {
			require.Len(t, replicas[0], 1)
			if sc := replicas[0][0].Spec.SecurityContext; sc != nil {
				got[role] = sc.Sysctls
			}
		}

		return got
	}

	t.Run("default off renders no sysctl", func(t *testing.T) {
		assert.Empty(t, sysctls(t))
	})

	t.Run("on renders the sysctl on the prefill half alone", func(t *testing.T) {
		settingtest.MergeDelegatedSettings(t, map[string]string{"model-deployment-tcp-tw-reuse": "true"})
		assert.Equal(t, map[string][]core.Sysctl{
			"prefill": {{Name: "net.ipv4.tcp_tw_reuse", Value: "1"}},
		}, sysctls(t))
	})
}

// TestRenderModelDeploymentPods_AWSEFAEnabledPodSpecHash verifies that flipping aws-efa-enabled
// changes pod-spec-hash on roles whose rendered PodSpec changes (NIXL P/D, Mooncake P/D with
// interfaces, multi-Pod vLLM Server with interfaces) and leaves pod-spec-hash untouched on
// unaffected roles (Mooncake TCP/HIP/Ascend P/D without interfaces, Ascend HIXL P/D, and
// non-Remote/non-ModelExpress artifactRef deployments).
func TestRenderModelDeploymentPods_AWSEFAEnabledPodSpecHash(t *testing.T) {
	origQualified := modelDeploymentModelTransferQualified
	modelDeploymentModelTransferQualified = func(modelTransferTuple) bool { return true }
	t.Cleanup(func() { modelDeploymentModelTransferQualified = origQualified })

	renderHash := func(
		t *testing.T, md *workercore.ModelDeployment, it *worker.AcceleratorType,
		artifact *ModelDeploymentArtifactRender, efa bool,
	) string {
		t.Helper()
		val := "false"
		if efa {
			val = "true"
		}
		settingtest.MergeDelegatedSettings(t, map[string]string{
			"aws-efa-enabled": val,
		})
		var weights *modelArtifactWeights
		if artifact != nil {
			weights = &modelArtifactWeights{Render: artifact}
		}
		r := &ModelDeploymentReconciler{Client: newModelDeploymentClient(md, it)}
		desired, err := r.renderModelDeploymentPods(t.Context(), md, nil, nil, weights)
		require.NoError(t, err)
		roleName := md.Spec.Roles[0].Name
		require.NotEmpty(t, desired[roleName])
		require.NotEmpty(t, desired[roleName][0])
		return desired[roleName][0][0].Annotations[modelDeploymentPodSpecHashAnnotation]
	}

	nvidiaIT := newRenderAcceleratorType()
	amdIT := newRenderAcceleratorType(func(it *worker.AcceleratorType) {
		it.Status.Detail.Manufacturer = nodefeature.ManufacturerAMD
	})
	ascendIT := newRenderAcceleratorType(func(it *worker.AcceleratorType) {
		it.Status.Detail.Manufacturer = nodefeature.ManufacturerAscend
	})

	cases := []struct {
		name      string
		md        *workercore.ModelDeployment
		it        *worker.AcceleratorType
		artifact  *ModelDeploymentArtifactRender
		wantMoves bool
	}{
		{
			name: "NIXL P/D pair moves pod-spec-hash when aws-efa-enabled flips",
			md: newTCPTWReusePair(func(md *workercore.ModelDeployment) {
				md.Spec.KVTransfer = &workercore.ModelDeploymentKVTransfer{
					Backend: workercore.ModelDeploymentKVTransferBackendNIXL,
				}
			}),
			it:        nvidiaIT,
			wantMoves: true,
		},
		{
			name: "Mooncake P/D pair requesting interfaces moves pod-spec-hash when aws-efa-enabled flips",
			md: newTCPTWReusePair(func(md *workercore.ModelDeployment) {
				for i := range md.Spec.Roles {
					md.Spec.Roles[i].Resources = &workercore.ModelDeploymentRoleResources{
						Interface: ptr.To(resource.MustParse("1")),
					}
				}
			}),
			it:        nvidiaIT,
			wantMoves: true,
		},
		{
			name: "Remote NIXL artifact role moves pod-spec-hash when aws-efa-enabled flips",
			md:   newRenderDeployment(),
			it:   nvidiaIT,
			artifact: &ModelDeploymentArtifactRender{
				Delivery:             workercore.ModelDeploymentModelDeliveryRemote,
				ModelTransferPolicy:  "Require",
				SourceReady:          true,
				ModelTransferBackend: "nixl",
				ArtifactName:         "qwen3-8b",
				ArtifactUID:          "uid-1",
				ManifestDigest:       "sha256:fea7da0c5227e595df7f4fa5822011e7a33cabe1dff13e3ffa7be6898dd1c363",
			},
			wantMoves: true,
		},
		{
			name:      "Mooncake TCP P/D pair without interfaces keeps pod-spec-hash unchanged",
			md:        newTCPTWReusePair(),
			it:        nvidiaIT,
			wantMoves: false,
		},
		{
			name:      "Mooncake AMD ROCm P/D pair without interfaces keeps pod-spec-hash unchanged",
			md:        newTCPTWReusePair(),
			it:        amdIT,
			wantMoves: false,
		},
		{
			name:      "Ascend HIXL P/D pair keeps pod-spec-hash unchanged",
			md:        newTCPTWReusePair(),
			it:        ascendIT,
			wantMoves: false,
		},
		{
			name: "Node CSI artifactRef deployment without ModelExpress keeps pod-spec-hash unchanged",
			md:   newRenderDeployment(),
			it:   nvidiaIT,
			artifact: &ModelDeploymentArtifactRender{
				Delivery:     workercore.ModelDeploymentModelDeliveryNode,
				ArtifactName: "qwen3-8b",
				Repository:   "Qwen/Qwen3-8B",
				Revision:     "abc1234",
				Hub:          modelDeploymentHubHuggingFace,
			},
			wantMoves: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			offHash := renderHash(t, tc.md, tc.it, tc.artifact, false)
			onHash := renderHash(t, tc.md, tc.it, tc.artifact, true)
			if tc.wantMoves {
				assert.NotEqual(t, offHash, onHash)
			} else {
				assert.Equal(t, offHash, onHash)
			}
		})
	}
}

func TestRenderModelDeploymentPod_Capabilities(t *testing.T) {
	engramCaps := &core.SecurityContext{
		Capabilities: &core.Capabilities{
			Add: []core.Capability{"IPC_LOCK", "SYS_RESOURCE"},
		},
	}
	privilegedSC := &core.SecurityContext{Privileged: ptr.To(true)}

	cases := []struct {
		name              string
		md                *workercore.ModelDeployment
		interfaceResource core.ResourceName
		artifact          *ModelDeploymentArtifactRender
		want              *core.SecurityContext
	}{
		{
			name: "plain vLLM role renders no securityContext",
			md:   newRenderDeployment(),
			want: nil,
		},
		{
			name: "role requesting RDMA interfaces grants IPC_LOCK and SYS_RESOURCE",
			md: newRenderDeployment(func(md *workercore.ModelDeployment) {
				md.Spec.Roles[0].Resources = &workercore.ModelDeploymentRoleResources{
					Interface: ptr.To(resource.MustParse("2")),
				}
			}),
			interfaceResource: "rdma.gpustack.ai/hca",
			want:              engramCaps,
		},
		{
			name: "role with remote model-transfer delivery grants IPC_LOCK and SYS_RESOURCE",
			md:   newRenderDeployment(),
			artifact: &ModelDeploymentArtifactRender{
				Delivery:       workercore.ModelDeploymentModelDeliveryRemote,
				ArtifactName:   "qwen3-8b",
				ArtifactUID:    "uid-1",
				ManifestDigest: "sha256:fea7da0c5227e595df7f4fa5822011e7a33cabe1dff13e3ffa7be6898dd1c363",
			},
			want: engramCaps,
		},
		{
			name: "vLLM with --engram-config cpu_offload true grants IPC_LOCK and SYS_RESOURCE",
			md: newRenderDeployment(func(md *workercore.ModelDeployment) {
				md.Spec.Roles[0].ExtraArgs = []string{"--engram-config", `{"cpu_offload":true,"use_thp":true}`}
			}),
			want: engramCaps,
		},
		{
			name: "vLLM with --engram-config omitting cpu_offload defaults to true and grants capabilities",
			md: newRenderDeployment(func(md *workercore.ModelDeployment) {
				md.Spec.Roles[0].ExtraArgs = []string{"--engram-config={\"use_thp\":true}"}
			}),
			want: engramCaps,
		},
		{
			name: "vLLM with --engram-config cpu_offload false renders no securityContext",
			md: newRenderDeployment(func(md *workercore.ModelDeployment) {
				md.Spec.Roles[0].ExtraArgs = []string{"--engram-config", `{"cpu_offload":false,"embedding_across_dp":true}`}
			}),
			want: nil,
		},
		{
			name: "vLLM with dotted --engram-config.cpu_offload=true grants capabilities",
			md: newRenderDeployment(func(md *workercore.ModelDeployment) {
				md.Spec.Roles[0].ExtraArgs = []string{"--engram_config.cpu_offload=true"}
			}),
			want: engramCaps,
		},
		{
			name: "vLLM with dotted --engram-config.cpu_offload=false renders no securityContext",
			md: newRenderDeployment(func(md *workercore.ModelDeployment) {
				md.Spec.Roles[0].ExtraArgs = []string{"--engram-config", `{"cpu_offload":true}`, "--engram-config.cpu_offload=false"}
			}),
			want: nil,
		},
		{
			name: "vLLM take-over command with --engram-config grants capabilities",
			md: newRenderDeployment(func(md *workercore.ModelDeployment) {
				md.Spec.Roles[0].Command = []string{"vllm", "serve", "deepseek-ai/DeepSeek-V4.1-Flash", "--engram-config", `{"cpu_offload":true}`}
			}),
			want: engramCaps,
		},
		{
			name: "SGLang with SGLANG_ENABLE_DSV41_ENGRAM_HOST_TABLE=1 grants capabilities",
			md: newRenderDeployment(func(md *workercore.ModelDeployment) {
				md.Spec.Engine.Name = workercore.ModelDeploymentEngineSGLang
				md.Spec.Roles[0].Env = []workercore.ModelDeploymentEnvVar{
					{Name: "SGLANG_ENABLE_DSV41_ENGRAM_HOST_TABLE", Value: "1"},
				}
			}),
			want: engramCaps,
		},
		{
			name: "SGLang with SGLANG_ENABLE_DSV41_ENGRAM_HOST_TABLE=0 renders no securityContext",
			md: newRenderDeployment(func(md *workercore.ModelDeployment) {
				md.Spec.Engine.Name = workercore.ModelDeploymentEngineSGLang
				md.Spec.Roles[0].Env = []workercore.ModelDeploymentEnvVar{
					{Name: "SGLANG_ENABLE_DSV41_ENGRAM_HOST_TABLE", Value: "0"},
				}
			}),
			want: nil,
		},
		{
			name: "privileged role keeps Privileged without adding capabilities",
			md: newRenderDeployment(func(md *workercore.ModelDeployment) {
				md.Spec.Roles[0].Privileged = true
				md.Spec.Roles[0].ExtraArgs = []string{"--engram-config", `{"cpu_offload":true}`}
			}),
			want: privilegedSC,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			pod, err := renderModelDeploymentPod(t.Context(), ModelDeploymentRenderInput{
				Deployment:        tc.md,
				Role:              &tc.md.Spec.Roles[0],
				AcceleratorType:   newRenderAcceleratorType(),
				InterfaceResource: tc.interfaceResource,
				Artifact:          tc.artifact,
			})
			require.NoError(t, err)
			assert.Equal(t, tc.want, pod.Spec.Containers[0].SecurityContext)
		})
	}
}

func TestRenderModelDeploymentPod_CPUOnlyUnqueuedRole(t *testing.T) {
	md := newRenderDeployment(func(md *workercore.ModelDeployment) {
		md.Spec.Roles[0].AcceleratorType = ""
		md.Spec.Roles[0].ReplicaSize = 2
		md.Spec.Roles[0].Resources = &workercore.ModelDeploymentRoleResources{
			CPU: ptr.To(resource.MustParse("8")),
			RAM: ptr.To(resource.MustParse("32Gi")),
		}
	})

	t.Run("default mixed-on-node false adds cpu-only nodeSelector and catch-all toleration", func(t *testing.T) {
		pod, err := renderModelDeploymentPod(t.Context(), ModelDeploymentRenderInput{
			Deployment:  md,
			Role:        &md.Spec.Roles[0],
			MixedOnNode: false,
		})
		require.NoError(t, err)

		assert.NotContains(t, pod.Labels, kueuectrlconst.QueueLabel,
			"CPU-only role must omit kueue.x-k8s.io/queue-name")
		assert.NotContains(t, pod.Labels, "kueue.x-k8s.io/pod-group-name",
			"CPU-only role must omit Kueue pod-group-name label")
		assert.NotContains(t, pod.Annotations, "kueue.x-k8s.io/pod-group-total-count")
		assert.NotContains(t, pod.Annotations, "kueue.x-k8s.io/role-hash")
		assert.NotContains(t, pod.Annotations, "kueue.x-k8s.io/pod-group-serving")

		assert.Equal(t, "0", pod.Labels[modelDeploymentReplicaOrdinalLabel])
		assert.Equal(t, "0", pod.Labels[modelDeploymentMemberIndexLabel])
		assert.Equal(t, "2", pod.Annotations[modelDeploymentReplicaSizeAnnotation])

		assert.Equal(t, "true", pod.Spec.NodeSelector["gpustack.ai/managed"])
		assert.Equal(t, "true", pod.Spec.NodeSelector[nodefeature.NodeCPUOnlyLabelKey])
		assert.Contains(t, pod.Spec.Tolerations, core.Toleration{Operator: core.TolerationOpExists})

		cacheVol := findVolume(pod, ModelDeploymentCompileCacheVolumeName)
		require.NotNil(t, cacheVol, "CPU-only role must mount an emptyDir runtime cache at /var/lib/gpustack/cache")
		assert.NotNil(t, cacheVol.EmptyDir, "CPU-only role uses EmptyDir rather than a GPU HostPath compilation cache")
		assert.Nil(t, cacheVol.HostPath)
		cacheMount := findMount(&pod.Spec.Containers[0], ModelDeploymentCompileCacheVolumeName)
		require.NotNil(t, cacheMount)
		assert.Equal(t, ModelDeploymentCompileCacheMountPath, cacheMount.MountPath)
	})

	t.Run("mixed-on-node true omits cpu-only nodeSelector while keeping managed selector and toleration", func(t *testing.T) {
		pod, err := renderModelDeploymentPod(t.Context(), ModelDeploymentRenderInput{
			Deployment:  md,
			Role:        &md.Spec.Roles[0],
			MixedOnNode: true,
		})
		require.NoError(t, err)

		assert.Equal(t, "true", pod.Spec.NodeSelector["gpustack.ai/managed"])
		assert.NotContains(t, pod.Spec.NodeSelector, nodefeature.NodeCPUOnlyLabelKey)
		assert.Contains(t, pod.Spec.Tolerations, core.Toleration{Operator: core.TolerationOpExists})
	})
}

func TestRenderModelDeploymentPod_UpgradeHashStabilityWithFamily(t *testing.T) {
	require.Equal(t, 7, modelDeploymentRenderVersion, "render version must match current baseline")

	storeConnection := func(protocol string) *ModelDeploymentConnectorInput {
		return &ModelDeploymentConnectorInput{
			Domain:              "chat",
			Dtype:               "bfloat16",
			MasterServerAddress: "shared-kv-master.gpustack-system.svc:50051",
			Protocols:           []string{protocol},
		}
	}

	nonA5Cases := []struct {
		name          string
		manufacturer  string
		family        string
		engine        string
		router        string
		kvBackend     string
		storeProtocol string
		disaggregated bool
	}{
		{
			name:         "NVIDIA Hopper aggregated Server",
			manufacturer: nodefeature.ManufacturerNVIDIA,
			family:       "hopper",
			engine:       workercore.ModelDeploymentEngineVLLM,
		},
		{
			name:          "NVIDIA Hopper Server with KVCache",
			manufacturer:  nodefeature.ManufacturerNVIDIA,
			family:        "hopper",
			engine:        workercore.ModelDeploymentEngineVLLM,
			storeProtocol: "rdma",
		},
		{
			name:          "NVIDIA Hopper vLLM P/D NIXL",
			manufacturer:  nodefeature.ManufacturerNVIDIA,
			family:        "hopper",
			engine:        workercore.ModelDeploymentEngineVLLM,
			router:        workercore.ModelDeploymentRouterLLMD,
			kvBackend:     workercore.ModelDeploymentKVTransferBackendNIXL,
			disaggregated: true,
		},
		{
			name:          "AMD CDNA3 vLLM P/D NIXL",
			manufacturer:  nodefeature.ManufacturerAMD,
			family:        "cdna3",
			engine:        workercore.ModelDeploymentEngineVLLM,
			router:        workercore.ModelDeploymentRouterLLMD,
			kvBackend:     workercore.ModelDeploymentKVTransferBackendNIXL,
			disaggregated: true,
		},
		{
			name:          "Ascend A2 910B Server with CANN KVCache",
			manufacturer:  nodefeature.ManufacturerAscend,
			family:        "910B",
			engine:        workercore.ModelDeploymentEngineVLLM,
			storeProtocol: "ascend",
		},
		{
			name:          "Ascend A2 910B vLLM P/D HIXL",
			manufacturer:  nodefeature.ManufacturerAscend,
			family:        "910B",
			engine:        workercore.ModelDeploymentEngineVLLM,
			router:        workercore.ModelDeploymentRouterLLMD,
			kvBackend:     workercore.ModelDeploymentKVTransferBackendHIXL,
			disaggregated: true,
		},
		{
			name:          "Ascend A3 910C vLLM P/D HIXL with CANN KVCache",
			manufacturer:  nodefeature.ManufacturerAscend,
			family:        "910C",
			engine:        workercore.ModelDeploymentEngineVLLM,
			router:        workercore.ModelDeploymentRouterLLMD,
			kvBackend:     workercore.ModelDeploymentKVTransferBackendHIXL,
			storeProtocol: "ascend",
			disaggregated: true,
		},
		{
			name:          "Ascend A3 910C SGLang P/D HIXL",
			manufacturer:  nodefeature.ManufacturerAscend,
			family:        "910C",
			engine:        workercore.ModelDeploymentEngineSGLang,
			router:        workercore.ModelDeploymentRouterSGLang,
			kvBackend:     workercore.ModelDeploymentKVTransferBackendHIXL,
			disaggregated: true,
		},
	}

	renderForFamily := func(t *testing.T, md *workercore.ModelDeployment, manufacturer, family, storeProtocol string) map[string]map[int][]*core.Pod {
		t.Helper()
		at := newRenderAcceleratorType(func(at *worker.AcceleratorType) {
			at.Name = "pool-type"
			at.Status.Entrance = "queue-for-pool-type"
			at.Status.Detail.Manufacturer = manufacturer
			at.Status.Detail.Family = family
		})
		cli := newModelDeploymentClient(md, at)
		r := &ModelDeploymentReconciler{Client: cli, APIReader: cli}
		var conn *ModelDeploymentConnectorInput
		var ifaceProtocols []string
		if storeProtocol != "" {
			conn = storeConnection(storeProtocol)
			ifaceProtocols = []string{storeProtocol}
		}
		pods, err := r.renderModelDeploymentRolePods(t.Context(), md, conn, ifaceProtocols, nil, false)
		require.NoError(t, err)
		return pods
	}

	for _, tc := range nonA5Cases {
		t.Run(tc.name, func(t *testing.T) {
			qty1 := resource.MustParse("1")
			md := newRenderDeployment(func(md *workercore.ModelDeployment) {
				md.Spec.Engine.Name = tc.engine
				if tc.storeProtocol == "" {
					md.Spec.KVCache = nil
				}
				if tc.router != "" {
					md.Spec.Router = &workercore.ModelDeploymentRouter{Name: tc.router}
				}
				if tc.kvBackend != "" {
					md.Spec.KVTransfer = &workercore.ModelDeploymentKVTransfer{Backend: tc.kvBackend}
				}
				if tc.disaggregated {
					md.Spec.Roles = []workercore.ModelDeploymentRole{
						{
							Name:            "prefill",
							Kind:            workercore.ModelDeploymentRoleKindPrefill,
							AcceleratorType: "pool-type",
							Image:           md.Spec.Engine.Image,
							Replicas:        1,
							ReplicaSize:     1,
							Resources:       &workercore.ModelDeploymentRoleResources{Accelerator: ptr.To(qty1)},
						},
						{
							Name:            "decode",
							Kind:            workercore.ModelDeploymentRoleKindDecode,
							AcceleratorType: "pool-type",
							Image:           md.Spec.Engine.Image,
							Replicas:        1,
							ReplicaSize:     1,
							Resources:       &workercore.ModelDeploymentRoleResources{Accelerator: ptr.To(qty1)},
						},
					}
				} else {
					md.Spec.Roles = []workercore.ModelDeploymentRole{{
						Name:            "server",
						Kind:            workercore.ModelDeploymentRoleKindServer,
						AcceleratorType: "pool-type",
						Image:           md.Spec.Engine.Image,
						Replicas:        1,
						ReplicaSize:     1,
						Resources:       &workercore.ModelDeploymentRoleResources{Accelerator: ptr.To(qty1)},
					}}
				}
			})

			withoutFamily := renderForFamily(t, md, tc.manufacturer, "", tc.storeProtocol)
			withFamily := renderForFamily(t, md, tc.manufacturer, tc.family, tc.storeProtocol)

			for roleName, byOrdinal := range withoutFamily {
				for ord, wantMembers := range byOrdinal {
					gotMembers := withFamily[roleName][ord]
					require.Len(t, gotMembers, len(wantMembers))
					for m := range wantMembers {
						assert.Equal(t,
							wantMembers[m].Annotations[modelDeploymentPodSpecHashAnnotation],
							gotMembers[m].Annotations[modelDeploymentPodSpecHashAnnotation],
							"role %s must produce identical pod-spec-hash when Family is %q vs empty",
							roleName, tc.family,
						)
						wantJSON, err := json.Marshal(wantMembers[m])
						require.NoError(t, err)
						gotJSON, err := json.Marshal(gotMembers[m])
						require.NoError(t, err)
						assert.Equal(t, string(wantJSON), string(gotJSON))
					}
				}
			}
		})
	}

	t.Run("Ascend A5 convergence moves pod-spec-hash when A5 alters connector output", func(t *testing.T) {
		qty1 := resource.MustParse("1")
		vllmPD := newRenderDeployment(func(md *workercore.ModelDeployment) {
			md.Spec.KVCache = nil
			md.Spec.Router = &workercore.ModelDeploymentRouter{Name: workercore.ModelDeploymentRouterLLMD}
			md.Spec.KVTransfer = &workercore.ModelDeploymentKVTransfer{Backend: workercore.ModelDeploymentKVTransferBackendHIXL}
			md.Spec.Roles = []workercore.ModelDeploymentRole{
				{
					Name: "prefill", Kind: workercore.ModelDeploymentRoleKindPrefill,
					AcceleratorType: "pool-type", Image: md.Spec.Engine.Image, Replicas: 1, ReplicaSize: 1,
					Resources: &workercore.ModelDeploymentRoleResources{Accelerator: ptr.To(qty1)},
				},
				{
					Name: "decode", Kind: workercore.ModelDeploymentRoleKindDecode,
					AcceleratorType: "pool-type", Image: md.Spec.Engine.Image, Replicas: 1, ReplicaSize: 1,
					Resources: &workercore.ModelDeploymentRoleResources{Accelerator: ptr.To(qty1)},
				},
			}
		})
		unconverged := renderForFamily(t, vllmPD, nodefeature.ManufacturerAscend, "", "")
		convergedA5 := renderForFamily(t, vllmPD, nodefeature.ManufacturerAscend, nodefeature.AcceleratorFamilyAscend950, "")
		assert.NotEqual(t,
			unconverged["prefill"][0][0].Annotations[modelDeploymentPodSpecHashAnnotation],
			convergedA5["prefill"][0][0].Annotations[modelDeploymentPodSpecHashAnnotation],
		)

		sglangPD := vllmPD.DeepCopy()
		sglangPD.Spec.Engine.Name = workercore.ModelDeploymentEngineSGLang
		sglangPD.Spec.Router.Name = workercore.ModelDeploymentRouterSGLang
		unconvergedSG := renderForFamily(t, sglangPD, nodefeature.ManufacturerAscend, "", "")
		convergedSGA5 := renderForFamily(t, sglangPD, nodefeature.ManufacturerAscend, nodefeature.AcceleratorFamilyAscend950, "")
		assert.NotEqual(t,
			unconvergedSG["prefill"][0][0].Annotations[modelDeploymentPodSpecHashAnnotation],
			convergedSGA5["prefill"][0][0].Annotations[modelDeploymentPodSpecHashAnnotation],
		)
	})
}

func TestRenderModelDeploymentPod_AscendA5HIXLEPYield(t *testing.T) {
	qty1 := resource.MustParse("1")
	atlas950 := newRenderAcceleratorType(func(at *worker.AcceleratorType) {
		at.Name = "atlas-950"
		at.Status.Entrance = "queue-for-atlas-950"
		at.Status.Detail.Manufacturer = nodefeature.ManufacturerAscend
		at.Status.Detail.Family = nodefeature.AcceleratorFamilyAscend950
	})
	storeConn := &ModelDeploymentConnectorInput{
		Domain:              "chat",
		Dtype:               "bfloat16",
		MasterServerAddress: "shared-kv-master.gpustack-system.svc:50051",
		Protocols:           []string{"ascend"},
	}

	for _, mountPath := range []string{"", "/etc/hixlep", "/etc/hixlep/"} {
		name := "default_operator_mount"
		if mountPath != "" {
			name = "yields_to_user_mount_" + strings.Trim(mountPath, "/")
		}
		t.Run(name, func(t *testing.T) {
			md := newRenderDeployment(func(md *workercore.ModelDeployment) {
				md.Spec.Router = &workercore.ModelDeploymentRouter{Name: workercore.ModelDeploymentRouterLLMD}
				md.Spec.KVTransfer = &workercore.ModelDeploymentKVTransfer{Backend: workercore.ModelDeploymentKVTransferBackendHIXL}
				var avs []workercore.ModelDeploymentAdditionalVolume
				if mountPath != "" {
					avs = []workercore.ModelDeploymentAdditionalVolume{{
						MountPath: mountPath,
						ReadOnly:  true,
						ConfigMap: &core.LocalObjectReference{Name: "custom-hixlep"},
					}}
				}
				md.Spec.Roles = []workercore.ModelDeploymentRole{
					{
						Name: "prefill", Kind: workercore.ModelDeploymentRoleKindPrefill,
						AcceleratorType: "atlas-950", Image: md.Spec.Engine.Image, Replicas: 1, ReplicaSize: 1,
						AdditionalVolumes: avs,
						Resources: &workercore.ModelDeploymentRoleResources{
							Accelerator: ptr.To(qty1),
							Interface:   ptr.To(qty1),
						},
					},
					{
						Name: "decode", Kind: workercore.ModelDeploymentRoleKindDecode,
						AcceleratorType: "atlas-950", Image: md.Spec.Engine.Image, Replicas: 1, ReplicaSize: 1,
						AdditionalVolumes: avs,
						Resources: &workercore.ModelDeploymentRoleResources{
							Accelerator: ptr.To(qty1),
							Interface:   ptr.To(qty1),
						},
					},
				}
			})

			cli := newModelDeploymentClient(md, atlas950)
			r := &ModelDeploymentReconciler{Client: cli, APIReader: cli}
			pods, err := r.renderModelDeploymentRolePods(t.Context(), md, storeConn, []string{"ascend"}, nil, false)
			require.NoError(t, err)

			for _, roleName := range []string{"prefill", "decode"} {
				pod := pods[roleName][0][0]
				ctr := &pod.Spec.Containers[0]
				assert.Contains(t, strings.Join(ctr.Command, " "), `"ascend_local_comm_res_path":"/etc/hixlep"`)
				assert.Nil(t, envByName(ctr.Env, inject.AscendLocalCommResEnv))
				assert.NotNil(t, findVolume(pod, "gpustack-ascend-driver"))
				if roleName == "prefill" {
					assert.NotNil(t, findVolume(pod, inject.ConfigVolumeName))
				}

				if mountPath == "" {
					vol := findVolume(pod, inject.AscendHIXLEPVolumeName)
					require.NotNil(t, vol)
					require.NotNil(t, vol.HostPath)
					assert.Equal(t, inject.AscendHIXLEPPath, vol.HostPath.Path)
					mnt := findMount(ctr, inject.AscendHIXLEPVolumeName)
					require.NotNil(t, mnt)
					assert.Equal(t, inject.AscendHIXLEPPath, mnt.MountPath)
					assert.True(t, mnt.ReadOnly)
				} else {
					assert.Nil(t, findVolume(pod, inject.AscendHIXLEPVolumeName),
						"operator must omit gpustack-ascend-hixlep volume when user mounts %q", mountPath)
					assert.Nil(t, findMount(ctr, inject.AscendHIXLEPVolumeName),
						"operator must omit gpustack-ascend-hixlep volumeMount when user mounts %q", mountPath)
					userVol := findVolume(pod, "additional-0")
					require.NotNil(t, userVol)
					require.NotNil(t, userVol.ConfigMap)
					assert.Equal(t, "custom-hixlep", userVol.ConfigMap.Name)
				}
			}
		})
	}

	t.Run("multi-Pod A5 Server without KVCache gets rdma without hixlep or ASCEND_LOCAL_COMM_RES", func(t *testing.T) {
		md := newRenderDeployment(func(md *workercore.ModelDeployment) {
			md.Spec.KVCache = nil
			md.Spec.Roles = []workercore.ModelDeploymentRole{{
				Name: "server", Kind: workercore.ModelDeploymentRoleKindServer,
				AcceleratorType: "atlas-950", Image: md.Spec.Engine.Image, Replicas: 1, ReplicaSize: 2,
				Resources: &workercore.ModelDeploymentRoleResources{
					Accelerator: ptr.To(qty1),
					Interface:   ptr.To(qty1),
				},
			}}
		})
		cli := newModelDeploymentClient(md, atlas950)
		r := &ModelDeploymentReconciler{Client: cli, APIReader: cli}
		pods, err := r.renderModelDeploymentRolePods(t.Context(), md, nil, nil, nil, true)
		require.NoError(t, err)
		pod := pods["server"][0][0]
		ctr := &pod.Spec.Containers[0]
		rdmaShared := nodefeature.GetRDMAResourceName(workercore.DeviceAllocationModeShared)
		assert.Equal(t, qty1, ctr.Resources.Limits[rdmaShared])
		assert.Nil(t, findVolume(pod, inject.AscendHIXLEPVolumeName))
		assert.Nil(t, envByName(ctr.Env, inject.AscendLocalCommResEnv))
		assert.NotContains(t, strings.Join(ctr.Command, " "), "ascend_local_comm_res_path")
	})

	t.Run("legacy KV-only sub-mount under /etc/hixlep keeps operator mount while A5 remote Host RoCE yields", func(t *testing.T) {
		subMount := []workercore.ModelDeploymentAdditionalVolume{{
			MountPath: "/etc/hixlep/custom.json",
			ReadOnly:  true,
			ConfigMap: &core.LocalObjectReference{Name: "sub-hixlep"},
		}}
		md := newRenderDeployment(func(md *workercore.ModelDeployment) {
			md.Spec.Model.ArtifactRef = &core.LocalObjectReference{Name: "weights"}
			md.Spec.Router = &workercore.ModelDeploymentRouter{Name: workercore.ModelDeploymentRouterLLMD}
			md.Spec.KVTransfer = &workercore.ModelDeploymentKVTransfer{Backend: workercore.ModelDeploymentKVTransferBackendHIXL}
			md.Spec.Roles = []workercore.ModelDeploymentRole{
				{
					Name: "prefill", Kind: workercore.ModelDeploymentRoleKindPrefill,
					AcceleratorType: "atlas-950", Image: md.Spec.Engine.Image, Replicas: 1, ReplicaSize: 1,
					AdditionalVolumes: subMount,
					Resources: &workercore.ModelDeploymentRoleResources{
						Accelerator: ptr.To(qty1),
						Interface:   ptr.To(qty1),
					},
				},
				{
					Name: "decode", Kind: workercore.ModelDeploymentRoleKindDecode,
					AcceleratorType: "atlas-950", Image: md.Spec.Engine.Image, Replicas: 1, ReplicaSize: 1,
					AdditionalVolumes: subMount,
					Resources: &workercore.ModelDeploymentRoleResources{
						Accelerator: ptr.To(qty1),
						Interface:   ptr.To(qty1),
					},
				},
			}
		})
		cli := newModelDeploymentClient(md, atlas950)
		r := &ModelDeploymentReconciler{Client: cli, APIReader: cli}

		kvOnlyPods, err := r.renderModelDeploymentRolePods(t.Context(), md, storeConn, []string{"ascend"}, nil, false)
		require.NoError(t, err)
		kvPod := kvOnlyPods["prefill"][0][0]
		assert.NotNil(t, findVolume(kvPod, inject.AscendHIXLEPVolumeName),
			"KV-only render must preserve exact-path yielding so sub-mounts keep gpustack-ascend-hixlep")
		assert.NotNil(t, findMount(&kvPod.Spec.Containers[0], inject.AscendHIXLEPVolumeName))
		assert.NotNil(t, findVolume(kvPod, "additional-0"))

		remoteRender := modelTransferRemoteRender()
		remoteRender.SourceReady = true
		remotePods, err := r.renderModelDeploymentRolePods(
			t.Context(), md, storeConn, []string{"ascend"}, &modelArtifactWeights{Render: remoteRender}, false,
		)
		require.NoError(t, err)
		remotePod := remotePods["prefill"][0][0]
		assert.Nil(t, findVolume(remotePod, inject.AscendHIXLEPVolumeName),
			"A5 remote Host RoCE must yield gpustack-ascend-hixlep on sub-mount overlap")
		assert.Nil(t, findMount(&remotePod.Spec.Containers[0], inject.AscendHIXLEPVolumeName))
		assert.NotNil(t, findVolume(remotePod, "additional-0"))
	})
}

func TestModelDeploymentPodSpecHash_RouterProjectionAndRuntimeControlDecoupling(t *testing.T) {
	at := newRenderAcceleratorType()
	baseNoRouter := newRenderDeployment()
	noRouterHash := renderOne(t, baseNoRouter, at).Annotations[modelDeploymentPodSpecHashAnnotation]

	baseLLMD := newRenderDeployment(func(md *workercore.ModelDeployment) {
		md.Spec.Router = &workercore.ModelDeploymentRouter{Name: workercore.ModelDeploymentRouterLLMD}
	})
	llmdPod := renderOne(t, baseLLMD, at)
	llmdHash := llmdPod.Annotations[modelDeploymentPodSpecHashAnnotation]
	require.NotEqual(t, noRouterHash, llmdHash, "attaching a router must move pod-spec-hash")

	baseVLLMRouter := newRenderDeployment(func(md *workercore.ModelDeployment) {
		md.Spec.Router = &workercore.ModelDeploymentRouter{Name: workercore.ModelDeploymentRouterVLLM}
	})
	vllmRouterHash := renderOne(t, baseVLLMRouter, at).Annotations[modelDeploymentPodSpecHashAnnotation]
	require.NotEqual(t, llmdHash, vllmRouterHash, "changing router.name must move pod-spec-hash")

	decoupledRouterCases := []struct {
		name   string
		mutate func(*workercore.ModelDeploymentRouter)
	}{
		{
			name:   "replicas",
			mutate: func(r *workercore.ModelDeploymentRouter) { r.Replicas = ptr.To[int32](3) },
		},
		{
			name:   "image",
			mutate: func(r *workercore.ModelDeploymentRouter) { r.Image = "registry.example/router:v2" },
		},
		{
			name:   "imagePullPolicy",
			mutate: func(r *workercore.ModelDeploymentRouter) { r.ImagePullPolicy = core.PullAlways },
		},
		{
			name: "imagePullSecrets",
			mutate: func(r *workercore.ModelDeploymentRouter) {
				r.ImagePullSecrets = []core.LocalObjectReference{{Name: "pull-sec"}}
			},
		},
		{
			name:   "extraArgs",
			mutate: func(r *workercore.ModelDeploymentRouter) { r.ExtraArgs = []string{"--log-level=debug"} },
		},
		{
			name:   "requestTimeoutSeconds",
			mutate: func(r *workercore.ModelDeploymentRouter) { r.RequestTimeoutSeconds = ptr.To[int32](120) },
		},
		{
			name: "disaggregationThresholdTokens",
			mutate: func(r *workercore.ModelDeploymentRouter) {
				r.DisaggregationThresholdTokens = ptr.To[int32](512)
			},
		},
	}

	for _, tc := range decoupledRouterCases {
		t.Run("router."+tc.name+" does not move projected pod-spec-hash", func(t *testing.T) {
			md := baseLLMD.DeepCopy()
			tc.mutate(md.Spec.Router)
			pod := renderOne(t, md, at)
			assert.Equal(t, llmdHash, pod.Annotations[modelDeploymentPodSpecHashAnnotation])

			legacyHashes := modelDeploymentDesiredPodLegacyHashes(md, &md.Spec.Roles[0], 0, 0, pod)
			require.NotEmpty(t, legacyHashes)
			assert.NotEqual(t, llmdHash, legacyHashes[0],
				"legacy full-router hash must retain router.%s for pre-upgrade currency", tc.name)
		})
	}

	t.Run("role.replicas, role.pause, spec.model.loraAdapters, and non-elastic runtimeControl.parameters do not move pod-spec-hash", func(t *testing.T) {
		mutated := baseNoRouter.DeepCopy()
		mutated.Spec.Roles[0].Replicas = 8
		mutated.Spec.Roles[0].Pause = true
		mutated.Spec.Model.LoRAAdapters = []workercore.ModelDeploymentLoRAAdapter{
			{
				Name:        "tenant-a",
				ArtifactRef: core.LocalObjectReference{Name: "lora-artifact-a"},
				SubPath:     "adapters/a",
				Pinned:      true,
			},
		}
		mutated.Spec.Roles[0].RuntimeControl = &workercore.ModelDeploymentRoleRuntimeControl{
			Parameters: map[string]string{
				runtimecontrol.ParamSchedulingMaxActiveRequests:         "16",
				runtimecontrol.ParamSchedulingMaxBatchedTokens:          "2048",
				runtimecontrol.ParamSchedulingLongPrefillTokenThreshold: "512",
				runtimecontrol.ParamSchedulingCacheAwareWindow:          "4",
				runtimecontrol.ParamSchedulingCacheAwareThreshold:       "0.5",
			},
		}
		pod := renderOne(t, mutated, at)
		assert.Equal(t, noRouterHash, pod.Annotations[modelDeploymentPodSpecHashAnnotation])

		tuned := mutated.DeepCopy()
		tuned.Spec.Roles[0].Pause = false
		tuned.Spec.Model.LoRAAdapters = nil
		tuned.Spec.Roles[0].RuntimeControl.Parameters = map[string]string{
			runtimecontrol.ParamSchedulingMaxActiveRequests: "32",
			runtimecontrol.ParamSchedulingMaxBatchedTokens:  "4096",
		}
		assert.Equal(t, noRouterHash, renderOne(t, tuned, at).Annotations[modelDeploymentPodSpecHashAnnotation])

		emptyCtrl := baseNoRouter.DeepCopy()
		emptyCtrl.Spec.Roles[0].RuntimeControl = &workercore.ModelDeploymentRoleRuntimeControl{}
		assert.Equal(t, noRouterHash, renderOne(t, emptyCtrl, at).Annotations[modelDeploymentPodSpecHashAnnotation])
	})

	t.Run("elastic.dataParallelWidth presence moves pod-spec-hash while scaling its value keeps pod-spec-hash identical", func(t *testing.T) {
		elasticWidth2 := baseNoRouter.DeepCopy()
		runtimecontrol.SetElasticWidth(&elasticWidth2.Spec.Roles[0], 2)
		width2Hash := renderOne(t, elasticWidth2, at).Annotations[modelDeploymentPodSpecHashAnnotation]
		assert.NotEqual(t, noRouterHash, width2Hash,
			"enabling elastic.dataParallelWidth changes Pod topology and must move pod-spec-hash")

		elasticWidth4 := baseNoRouter.DeepCopy()
		runtimecontrol.SetElasticWidth(&elasticWidth4.Spec.Roles[0], 4)
		assert.Equal(t, width2Hash, renderOne(t, elasticWidth4, at).Annotations[modelDeploymentPodSpecHashAnnotation],
			"scaling elastic.dataParallelWidth from 2 to 4 must keep pod-spec-hash identical")

		elasticWidth4WithTuningAndPause := elasticWidth4.DeepCopy()
		elasticWidth4WithTuningAndPause.Spec.Roles[0].Pause = true
		elasticWidth4WithTuningAndPause.Spec.Model.LoRAAdapters = []workercore.ModelDeploymentLoRAAdapter{
			{Name: "adapter-b", ArtifactRef: core.LocalObjectReference{Name: "art-b"}},
		}
		elasticWidth4WithTuningAndPause.Spec.Roles[0].RuntimeControl.Parameters[runtimecontrol.ParamSchedulingMaxActiveRequests] = "24"
		assert.Equal(t, width2Hash,
			renderOne(t, elasticWidth4WithTuningAndPause, at).Annotations[modelDeploymentPodSpecHashAnnotation],
			"co-present live parameters, pause, and loraAdapters must keep elastic pod-spec-hash identical")
	})
}

func TestModelDeploymentPodMatchesCurrencyWithLegacy(t *testing.T) {
	desired := &core.Pod{
		ObjectMeta: meta.ObjectMeta{
			Annotations: map[string]string{
				modelDeploymentPodSpecHashAnnotation:   "projected-hash",
				modelDeploymentRenderVersionAnnotation: strconv.Itoa(modelDeploymentRenderVersion),
			},
		},
	}
	podWith := func(hash, rv string) *core.Pod {
		ann := map[string]string{}
		if hash != "" {
			ann[modelDeploymentPodSpecHashAnnotation] = hash
		}
		if rv != "" {
			ann[modelDeploymentRenderVersionAnnotation] = rv
		}
		return &core.Pod{ObjectMeta: meta.ObjectMeta{Annotations: ann}}
	}

	cases := []struct {
		name       string
		actual     *core.Pod
		desired    *core.Pod
		legacyHash string
		minVersion int
		want       bool
	}{
		{
			name:       "projected hash matches at current render version",
			actual:     podWith("projected-hash", "6"),
			desired:    desired,
			legacyHash: "legacy-hash",
			minVersion: 1,
			want:       true,
		},
		{
			name:       "legacy full-router hash matches at render version 6",
			actual:     podWith("legacy-hash", "6"),
			desired:    desired,
			legacyHash: "legacy-hash",
			minVersion: 1,
			want:       true,
		},
		{
			name:       "legacy full-router hash matches at render version 5",
			actual:     podWith("legacy-hash", "5"),
			desired:    desired,
			legacyHash: "legacy-hash",
			minVersion: 1,
			want:       true,
		},
		{
			name:       "legacy full-router hash matches at render version 1 when floor is 1",
			actual:     podWith("legacy-hash", "1"),
			desired:    desired,
			legacyHash: "legacy-hash",
			minVersion: 1,
			want:       true,
		},
		{
			name:       "legacy full-router hash is rejected above current render version",
			actual:     podWith("legacy-hash", strconv.Itoa(modelDeploymentRenderVersion+1)),
			desired:    desired,
			legacyHash: "legacy-hash",
			minVersion: 1,
			want:       false,
		},
		{
			name:       "legacy full-router hash is rejected below minRenderVersion floor",
			actual:     podWith("legacy-hash", "3"),
			desired:    desired,
			legacyHash: "legacy-hash",
			minVersion: 4,
			want:       false,
		},
		{
			name:       "stale hash matching neither projected nor legacy is rejected",
			actual:     podWith("other-hash", "6"),
			desired:    desired,
			legacyHash: "legacy-hash",
			minVersion: 1,
			want:       false,
		},
		{
			name:       "missing render-version annotation is rejected",
			actual:     podWith("legacy-hash", ""),
			desired:    desired,
			legacyHash: "legacy-hash",
			minVersion: 1,
			want:       false,
		},
		{
			name:       "nil actual or desired is rejected",
			actual:     nil,
			desired:    desired,
			legacyHash: "legacy-hash",
			minVersion: 1,
			want:       false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want,
				modelDeploymentPodMatchesCurrencyWithLegacy(tc.actual, tc.desired, tc.legacyHash, tc.minVersion))
		})
	}
}

func TestRenderModelDeploymentPod_EngineSentinelMountPathMatchesPythonLayout(t *testing.T) {
	assert.Equal(t, "/var/lib/gpustack/operator/lib/engine-sentinel", modelDeploymentEngineSentinelHostPath)
	assert.Equal(t, modelDeploymentEngineSentinelHostPath, modelDeploymentEngineSentinelMountPath)

	pyRoot := filepath.Join("..", "..", "..", "..", "engine-sentinel", "python")
	_, err := os.Stat(filepath.Join(pyRoot, "sitecustomize.py"))
	require.NoError(t, err)
	_, err = os.Stat(filepath.Join(pyRoot, "hatch.toml"))
	require.NoError(t, err)
}

func TestRenderModelDeploymentPod_EngineSentinel(t *testing.T) {
	stageMountPath := "/var/lib/gpustack/operator/lib/engine-sentinel"
	nvidiaAT := newRenderAcceleratorType()
	ascendAT := newRenderAcceleratorType(func(at *worker.AcceleratorType) {
		at.Name = "atlas-910b"
		at.Status.Entrance = "queue-for-atlas-910b"
		at.Status.Detail.Manufacturer = nodefeature.ManufacturerAscend
	})

	t.Run("take-over, encode, cpu-only, and SGLang Ray roles render zero sentinel additions", func(t *testing.T) {
		plainMD := newRenderDeployment()
		assert.True(t, ModelDeploymentRoleSupportsEngineSentinel(plainMD, &plainMD.Spec.Roles[0]))
		assert.False(t, ModelDeploymentRoleSupportsEngineSentinel(nil, &plainMD.Spec.Roles[0]))
		assert.False(t, ModelDeploymentRoleSupportsEngineSentinel(plainMD, nil))

		takeOverMD := newRenderDeployment(func(md *workercore.ModelDeployment) {
			md.Spec.Roles[0].Command = []string{"/bin/custom-engine"}
		})
		assert.False(t, ModelDeploymentRoleSupportsEngineSentinel(takeOverMD, &takeOverMD.Spec.Roles[0]))
		takeOverPod := renderOne(t, takeOverMD, nvidiaAT)
		assert.Equal(t, []string{"/bin/custom-engine"}, takeOverPod.Spec.Containers[0].Command)
		assert.Nil(t, findVolume(takeOverPod, modelDeploymentEngineSentinelVolumeName))
		assert.Nil(t, envByName(takeOverPod.Spec.Containers[0].Env, "PYTHONPATH"))
		assert.Nil(t, envByName(takeOverPod.Spec.Containers[0].Env, modelDeploymentEngineSentinelEnableEnv))

		encodeMD := newRenderDeployment(func(md *workercore.ModelDeployment) {
			md.Spec.Roles[0].Kind = workercore.ModelDeploymentRoleKindEncode
		})
		assert.False(t, ModelDeploymentRoleSupportsEngineSentinel(encodeMD, &encodeMD.Spec.Roles[0]))
		encodePod := renderOne(t, encodeMD, nvidiaAT)
		assert.Nil(t, findVolume(encodePod, modelDeploymentEngineSentinelVolumeName))
		assert.Nil(t, envByName(encodePod.Spec.Containers[0].Env, modelDeploymentEngineSentinelEnableEnv))
		assert.Equal(t, "/health", encodePod.Spec.Containers[0].StartupProbe.HTTPGet.Path)
		assert.Equal(t, "/health", encodePod.Spec.Containers[0].ReadinessProbe.HTTPGet.Path)
		assert.Equal(t, "/health", encodePod.Spec.Containers[0].LivenessProbe.HTTPGet.Path)

		cpuMD := newRenderDeployment(func(md *workercore.ModelDeployment) {
			md.Spec.Roles[0].AcceleratorType = ""
		})
		assert.False(t, ModelDeploymentRoleSupportsEngineSentinel(cpuMD, &cpuMD.Spec.Roles[0]))
		cpuPod, err := renderModelDeploymentPod(t.Context(), ModelDeploymentRenderInput{
			Deployment: cpuMD,
			Role:       &cpuMD.Spec.Roles[0],
		})
		require.NoError(t, err)
		assert.Nil(t, findVolume(cpuPod, modelDeploymentEngineSentinelVolumeName))
		assert.Nil(t, envByName(cpuPod.Spec.Containers[0].Env, modelDeploymentEngineSentinelEnableEnv))
		assert.Equal(t, "/health", cpuPod.Spec.Containers[0].ReadinessProbe.HTTPGet.Path)

		rayCases := []struct {
			name      string
			extraArgs []string
			env       []workercore.ModelDeploymentEnvVar
		}{
			{name: "distributed-executor-backend=ray", extraArgs: []string{"--distributed-executor-backend=ray"}},
			{name: "distributed-executor-backend separate value", extraArgs: []string{"--distributed-executor-backend", "ray"}},
			{name: "enable-ray", extraArgs: []string{"--enable-ray"}},
			{name: "use-ray", extraArgs: []string{"--use-ray"}},
			{name: "ray-address", extraArgs: []string{"--ray-address=auto"}},
			{name: "ray-prefix flag", extraArgs: []string{"--ray-port=6379"}},
			{name: "RAY_ADDRESS env", env: []workercore.ModelDeploymentEnvVar{{Name: "RAY_ADDRESS", Value: "127.0.0.1:6379"}}},
			{name: "SGLANG_USE_RAY env", env: []workercore.ModelDeploymentEnvVar{{Name: "SGLANG_USE_RAY", Value: "1"}}},
		}
		for _, rc := range rayCases {
			sglangRayMD := newRenderDeployment(func(md *workercore.ModelDeployment) {
				md.Spec.Engine.Name = workercore.ModelDeploymentEngineSGLang
				md.Spec.Roles[0].ExtraArgs = rc.extraArgs
				md.Spec.Roles[0].Env = rc.env
			})
			assert.False(t, ModelDeploymentRoleSupportsEngineSentinel(sglangRayMD, &sglangRayMD.Spec.Roles[0]), rc.name)
			rayPod := renderOne(t, sglangRayMD, nvidiaAT)
			assert.Nil(t, findVolume(rayPod, modelDeploymentEngineSentinelVolumeName), rc.name)
			assert.Nil(t, envByName(rayPod.Spec.Containers[0].Env, modelDeploymentEngineSentinelEnableEnv), rc.name)
			assert.Equal(t, "/health", rayPod.Spec.Containers[0].ReadinessProbe.HTTPGet.Path, rc.name)
		}

		// SGLang with non-ray backend and vLLM with Ray backend still support engine-sentinel.
		sglangMP := newRenderDeployment(func(md *workercore.ModelDeployment) {
			md.Spec.Engine.Name = workercore.ModelDeploymentEngineSGLang
			md.Spec.Roles[0].ExtraArgs = []string{"--distributed-executor-backend=mp", "--enable-ray=false"}
		})
		assert.True(t, ModelDeploymentRoleSupportsEngineSentinel(sglangMP, &sglangMP.Spec.Roles[0]))

		// SGLANG_RUST_SERVER=1/true runs the Rust sglang_server binary instead of Python uvicorn,
		// so engine-sentinel must not be injected and probes fall back to /health.
		rustCases := []struct {
			name         string
			engine       string
			env          []workercore.ModelDeploymentEnvVar
			wantSentinel bool
		}{
			{
				name:         "SGLang with SGLANG_RUST_SERVER=1 skips engine-sentinel",
				engine:       workercore.ModelDeploymentEngineSGLang,
				env:          []workercore.ModelDeploymentEnvVar{{Name: modelDeploymentSGLangRustServerEnv, Value: "1"}},
				wantSentinel: false,
			},
			{
				name:         "SGLang with SGLANG_RUST_SERVER=true skips engine-sentinel",
				engine:       workercore.ModelDeploymentEngineSGLang,
				env:          []workercore.ModelDeploymentEnvVar{{Name: modelDeploymentSGLangRustServerEnv, Value: "true"}},
				wantSentinel: false,
			},
			{
				name:         "SGLang with SGLANG_RUST_SERVER=0 keeps engine-sentinel",
				engine:       workercore.ModelDeploymentEngineSGLang,
				env:          []workercore.ModelDeploymentEnvVar{{Name: modelDeploymentSGLangRustServerEnv, Value: "0"}},
				wantSentinel: true,
			},
			{
				name:         "SGLang with normal Python frontend keeps engine-sentinel",
				engine:       workercore.ModelDeploymentEngineSGLang,
				wantSentinel: true,
			},
			{
				name:         "vLLM ignores SGLANG_RUST_SERVER and keeps engine-sentinel",
				engine:       workercore.ModelDeploymentEngineVLLM,
				env:          []workercore.ModelDeploymentEnvVar{{Name: modelDeploymentSGLangRustServerEnv, Value: "1"}},
				wantSentinel: true,
			},
		}
		for _, rc := range rustCases {
			rustMD := newRenderDeployment(func(md *workercore.ModelDeployment) {
				md.Spec.Engine.Name = rc.engine
				md.Spec.Roles[0].Env = rc.env
			})
			assert.Equal(t, rc.wantSentinel, ModelDeploymentRoleSupportsEngineSentinel(rustMD, &rustMD.Spec.Roles[0]), rc.name)
			pod := renderOne(t, rustMD, nvidiaAT)
			if rc.wantSentinel {
				assert.NotNil(t, findVolume(pod, modelDeploymentEngineSentinelVolumeName), rc.name)
				assert.NotNil(t, envByName(pod.Spec.Containers[0].Env, modelDeploymentEngineSentinelEnableEnv), rc.name)
				assert.Equal(t, "/gpustack/v1/live", pod.Spec.Containers[0].StartupProbe.HTTPGet.Path, rc.name)
				assert.Equal(t, "/gpustack/v1/ready", pod.Spec.Containers[0].ReadinessProbe.HTTPGet.Path, rc.name)
				assert.Equal(t, "/gpustack/v1/live", pod.Spec.Containers[0].LivenessProbe.HTTPGet.Path, rc.name)
			} else {
				assert.Nil(t, findVolume(pod, modelDeploymentEngineSentinelVolumeName), rc.name)
				assert.Nil(t, envByName(pod.Spec.Containers[0].Env, modelDeploymentEngineSentinelEnableEnv), rc.name)
				assert.Equal(t, "/health", pod.Spec.Containers[0].StartupProbe.HTTPGet.Path, rc.name)
				assert.Equal(t, "/health", pod.Spec.Containers[0].ReadinessProbe.HTTPGet.Path, rc.name)
				assert.Equal(t, "/health", pod.Spec.Containers[0].LivenessProbe.HTTPGet.Path, rc.name)
			}
		}

		vllmRay := newRenderDeployment(func(md *workercore.ModelDeployment) {
			md.Spec.Engine.Name = workercore.ModelDeploymentEngineVLLM
			md.Spec.Roles[0].ExtraArgs = []string{"--distributed-executor-backend=ray"}
		})
		assert.True(t, ModelDeploymentRoleSupportsEngineSentinel(vllmRay, &vllmRay.Spec.Roles[0]))
	})

	for _, profile := range []struct {
		name string
		at   *worker.AcceleratorType
	}{
		{name: "vLLM NVIDIA", at: nvidiaAT},
		{name: "vLLM Ascend", at: ascendAT},
	} {
		t.Run(profile.name+" injects read-only PYTHONPATH, sentinel env, untouched argv, and HTTPS probes by default", func(t *testing.T) {
			md := newRenderDeployment(func(md *workercore.ModelDeployment) {
				md.Spec.Roles[0].AcceleratorType = profile.at.Name
				md.Spec.Roles[0].Ports = []workercore.ModelDeploymentPort{
					{Port: 9443, Protocol: core.ProtocolTCP},
				}
				md.Spec.Roles[0].ExtraArgs = []string{
					"--max-num-seqs=64",
					"--ssl-certfile=/etc/tls/tls.crt",
					"--ssl-keyfile=/etc/tls/tls.key",
				}
				md.Spec.Roles[0].Env = []workercore.ModelDeploymentEnvVar{
					{Name: "PYTHONPATH", Value: "/opt/user/py"},
				}
			})

			pod := renderOne(t, md, profile.at)
			ctr := &pod.Spec.Containers[0]

			assert.Equal(t, []string{
				"vllm", "serve", "Qwen/Qwen2.5-72B-Instruct",
				"--max-num-seqs=64",
				"--ssl-certfile=/etc/tls/tls.crt",
				"--ssl-keyfile=/etc/tls/tls.key",
				"--host", "0.0.0.0",
				"--port", "9443",
			}, ctr.Command, "sentinel must never wrap container argv")
			assert.Nil(t, envByName(ctr.Env, "VLLM_PLUGINS"),
				"renderer must never mutate VLLM_PLUGINS")
			for _, e := range ctr.Env {
				if e.ValueFrom != nil {
					assert.Nil(t, e.ValueFrom.SecretKeyRef, "sentinel must never mount SecretKeyRef auth tokens")
				}
			}

			vol := findVolume(pod, modelDeploymentEngineSentinelVolumeName)
			require.NotNil(t, vol)
			require.NotNil(t, vol.HostPath)
			assert.Equal(t, stageMountPath, vol.HostPath.Path)
			assert.Equal(t, ptr.To(core.HostPathDirectoryOrCreate), vol.HostPath.Type)

			mnt := findMount(ctr, modelDeploymentEngineSentinelVolumeName)
			require.NotNil(t, mnt)
			assert.Equal(t, stageMountPath, mnt.MountPath)
			assert.True(t, mnt.ReadOnly)

			pyPath := envByName(ctr.Env, "PYTHONPATH")
			require.NotNil(t, pyPath)
			assert.Equal(t, stageMountPath+":/opt/user/py", pyPath.Value)

			enableEnv := envByName(ctr.Env, modelDeploymentEngineSentinelEnableEnv)
			require.NotNil(t, enableEnv)
			assert.Equal(t, "1", enableEnv.Value)

			engineEnv := envByName(ctr.Env, modelDeploymentEngineSentinelEngineEnv)
			require.NotNil(t, engineEnv)
			assert.Equal(t, "vllm", engineEnv.Value)

			require.NotNil(t, ctr.StartupProbe)
			assert.Equal(t, "/gpustack/v1/live", ctr.StartupProbe.HTTPGet.Path)
			assert.Equal(t, intstr.FromInt32(9443), ctr.StartupProbe.HTTPGet.Port)
			assert.Equal(t, core.URISchemeHTTPS, ctr.StartupProbe.HTTPGet.Scheme)

			require.NotNil(t, ctr.ReadinessProbe)
			assert.Equal(t, "/gpustack/v1/ready", ctr.ReadinessProbe.HTTPGet.Path)
			assert.Equal(t, intstr.FromInt32(9443), ctr.ReadinessProbe.HTTPGet.Port)
			assert.Equal(t, core.URISchemeHTTPS, ctr.ReadinessProbe.HTTPGet.Scheme)

			require.NotNil(t, ctr.LivenessProbe)
			assert.Equal(t, "/gpustack/v1/live", ctr.LivenessProbe.HTTPGet.Path)
			assert.Equal(t, intstr.FromInt32(9443), ctr.LivenessProbe.HTTPGet.Port)
			assert.Equal(t, core.URISchemeHTTPS, ctr.LivenessProbe.HTTPGet.Scheme)
		})
	}

	t.Run("SGLang injects read-only PYTHONPATH, sglang sentinel env, untouched argv, and unified probes", func(t *testing.T) {
		md := newRenderDeployment(func(md *workercore.ModelDeployment) {
			md.UID = ""
			md.Spec.Engine = workercore.ModelDeploymentEngine{
				Name:  workercore.ModelDeploymentEngineSGLang,
				Image: "lmsysorg/sglang:v0.5.9",
			}
			md.Spec.Roles[0].Image = ""
			md.Spec.Roles[0].ExtraArgs = []string{"--max-running-requests=128"}
		})

		pod := renderOne(t, md, nvidiaAT)
		ctr := &pod.Spec.Containers[0]

		assert.Equal(t, []string{
			"python3", "-m", "sglang.launch_server",
			"--model-path", "Qwen/Qwen2.5-72B-Instruct",
			"--enable-metrics",
			"--max-running-requests=128",
			"--host", "0.0.0.0",
			"--port", "8000",
		}, ctr.Command)

		pyPath := envByName(ctr.Env, "PYTHONPATH")
		require.NotNil(t, pyPath)
		assert.Equal(t, stageMountPath, pyPath.Value)

		enableEnv := envByName(ctr.Env, modelDeploymentEngineSentinelEnableEnv)
		require.NotNil(t, enableEnv)
		assert.Equal(t, "1", enableEnv.Value)

		engineEnv := envByName(ctr.Env, modelDeploymentEngineSentinelEngineEnv)
		require.NotNil(t, engineEnv)
		assert.Equal(t, "sglang", engineEnv.Value)

		assert.Equal(t, "/gpustack/v1/live", ctr.StartupProbe.HTTPGet.Path)
		assert.Equal(t, "/gpustack/v1/ready", ctr.ReadinessProbe.HTTPGet.Path)
		assert.Equal(t, "/gpustack/v1/live", ctr.LivenessProbe.HTTPGet.Path)
	})

	t.Run("vLLM composes cleanly with model-transfer without wrapping argv or altering load-format", func(t *testing.T) {
		md := newRenderDeployment()
		artifact := &ModelDeploymentArtifactRender{
			Delivery:             workercore.ModelDeploymentModelDeliveryRemote,
			ModelTransferPolicy:  "Require",
			SourceReady:          true,
			ModelTransferBackend: "nixl",
			ArtifactName:         "qwen-weights",
			ArtifactUID:          "art-uid-1",
			ManifestDigest:       "sha256:fea7da0c5227e595df7f4fa5822011e7a33cabe1dff13e3ffa7be6898dd1c363",
		}
		pod, err := renderModelDeploymentPod(t.Context(), ModelDeploymentRenderInput{
			Deployment:      md,
			Role:            &md.Spec.Roles[0],
			AcceleratorType: nvidiaAT,
			Artifact:        artifact,
		})
		require.NoError(t, err)
		ctr := &pod.Spec.Containers[0]

		require.GreaterOrEqual(t, len(ctr.Command), 6)
		assert.Equal(t, "sh", ctr.Command[0])
		assert.Equal(t, "-c", ctr.Command[1])
		assert.Contains(t, ctr.Command[2], "gpustack-model-transfer")
		assert.Equal(t, []string{"sh", "vllm", "serve"}, ctr.Command[3:6],
			"model-transfer shell invokes vllm serve directly with zero sentinel argv wrapper")
		assert.Equal(t, []string{"--load-format", "model-transfer"}, ctr.Args,
			"sentinel must never alter --load-format")
		assert.NotNil(t, findVolume(pod, modelDeploymentModelTransferWheelVolumeName))
		assert.NotNil(t, findVolume(pod, modelDeploymentEngineSentinelVolumeName))
	})

	t.Run("router rollout sentinel gate mounts PYTHONPATH while holding activation until router isolation converges", func(t *testing.T) {
		md := newRenderDeployment(func(md *workercore.ModelDeployment) {
			md.Spec.Router = &workercore.ModelDeploymentRouter{Name: workercore.ModelDeploymentRouterVLLM}
		})

		heldPod, err := renderModelDeploymentPod(t.Context(), ModelDeploymentRenderInput{
			Deployment:            md,
			Role:                  &md.Spec.Roles[0],
			AcceleratorType:       nvidiaAT,
			EngineSentinelEnabled: ptr.To(false),
		})
		require.NoError(t, err)
		heldCtr := &heldPod.Spec.Containers[0]
		assert.Equal(t, "vllm", heldCtr.Command[0])
		assert.NotNil(t, findVolume(heldPod, modelDeploymentEngineSentinelVolumeName),
			"read-only sentinel volume stays mounted even while activation is held")
		pyPath := envByName(heldCtr.Env, "PYTHONPATH")
		require.NotNil(t, pyPath)
		assert.Equal(t, stageMountPath, pyPath.Value)
		assert.Nil(t, envByName(heldCtr.Env, modelDeploymentEngineSentinelEnableEnv))
		assert.Nil(t, envByName(heldCtr.Env, modelDeploymentEngineSentinelEngineEnv))
		assert.Equal(t, "/health", heldCtr.StartupProbe.HTTPGet.Path)
		assert.Equal(t, "/health", heldCtr.ReadinessProbe.HTTPGet.Path)
		assert.Equal(t, "/health", heldCtr.LivenessProbe.HTTPGet.Path)

		releasedPod, err := renderModelDeploymentPod(t.Context(), ModelDeploymentRenderInput{
			Deployment:            md,
			Role:                  &md.Spec.Roles[0],
			AcceleratorType:       nvidiaAT,
			EngineSentinelEnabled: ptr.To(true),
		})
		require.NoError(t, err)
		releasedCtr := &releasedPod.Spec.Containers[0]
		assert.Equal(t, "vllm", releasedCtr.Command[0])
		require.NotNil(t, envByName(releasedCtr.Env, modelDeploymentEngineSentinelEnableEnv))
		require.NotNil(t, envByName(releasedCtr.Env, modelDeploymentEngineSentinelEngineEnv))
		assert.Equal(t, "/gpustack/v1/live", releasedCtr.StartupProbe.HTTPGet.Path)
		assert.Equal(t, "/gpustack/v1/ready", releasedCtr.ReadinessProbe.HTTPGet.Path)
		assert.Equal(t, "/gpustack/v1/live", releasedCtr.LivenessProbe.HTTPGet.Path)
		assert.Equal(t,
			heldPod.Annotations[modelDeploymentPodSpecHashAnnotation],
			releasedPod.Annotations[modelDeploymentPodSpecHashAnnotation],
		)
	})

	t.Run("multi-member leader-served follower omits probes while External-DP follower keeps probes", func(t *testing.T) {
		leaderServed := newRenderDeployment(func(md *workercore.ModelDeployment) {
			md.Spec.Roles[0].ReplicaSize = 2
		})
		leaderPod, err := renderModelDeploymentPod(t.Context(), ModelDeploymentRenderInput{
			Deployment: leaderServed, Role: &leaderServed.Spec.Roles[0], AcceleratorType: nvidiaAT, Member: 0,
		})
		require.NoError(t, err)
		followerPod, err := renderModelDeploymentPod(t.Context(), ModelDeploymentRenderInput{
			Deployment: leaderServed, Role: &leaderServed.Spec.Roles[0], AcceleratorType: nvidiaAT, Member: 1,
		})
		require.NoError(t, err)
		require.NotNil(t, leaderPod.Spec.Containers[0].StartupProbe)
		require.NotNil(t, leaderPod.Spec.Containers[0].ReadinessProbe)
		require.NotNil(t, leaderPod.Spec.Containers[0].LivenessProbe)
		assert.Equal(t, "/gpustack/v1/live", leaderPod.Spec.Containers[0].StartupProbe.HTTPGet.Path)
		assert.Equal(t, "/gpustack/v1/ready", leaderPod.Spec.Containers[0].ReadinessProbe.HTTPGet.Path)
		assert.Equal(t, "/gpustack/v1/live", leaderPod.Spec.Containers[0].LivenessProbe.HTTPGet.Path)
		assert.Nil(t, followerPod.Spec.Containers[0].StartupProbe)
		assert.Nil(t, followerPod.Spec.Containers[0].ReadinessProbe)
		assert.Nil(t, followerPod.Spec.Containers[0].LivenessProbe)

		externalDP := newRenderDeployment(func(md *workercore.ModelDeployment) {
			md.Spec.Roles[0].ReplicaSize = 2
			md.Spec.Roles[0].ExtraArgs = []string{
				"--data-parallel-size=2",
				"--data-parallel-size-local=1",
				"--data-parallel-external-lb",
			}
		})
		dpFollowerPod, err := renderModelDeploymentPod(t.Context(), ModelDeploymentRenderInput{
			Deployment: externalDP, Role: &externalDP.Spec.Roles[0], AcceleratorType: nvidiaAT, Member: 1,
		})
		require.NoError(t, err)
		require.NotNil(t, dpFollowerPod.Spec.Containers[0].StartupProbe)
		require.NotNil(t, dpFollowerPod.Spec.Containers[0].ReadinessProbe)
		require.NotNil(t, dpFollowerPod.Spec.Containers[0].LivenessProbe)
		assert.Equal(t, "/gpustack/v1/live", dpFollowerPod.Spec.Containers[0].StartupProbe.HTTPGet.Path)
		assert.Equal(t, "/gpustack/v1/ready", dpFollowerPod.Spec.Containers[0].ReadinessProbe.HTTPGet.Path)
		assert.Equal(t, "/gpustack/v1/live", dpFollowerPod.Spec.Containers[0].LivenessProbe.HTTPGet.Path)
	})
}

func TestRenderModelDeploymentPod_AscendA5RemoteModelTransfer(t *testing.T) {
	qty1 := resource.MustParse("1")
	rdmaShared := nodefeature.GetRDMAResourceName(workercore.DeviceAllocationModeShared)
	atlas950 := newRenderAcceleratorType(func(at *worker.AcceleratorType) {
		at.Name = "atlas-950"
		at.Status.Entrance = "queue-for-atlas-950"
		at.Status.Detail.Manufacturer = nodefeature.ManufacturerAscend
		at.Status.Detail.Family = nodefeature.AcceleratorFamilyAscend950
	})
	atlas910b := newRenderAcceleratorType(func(at *worker.AcceleratorType) {
		at.Name = "atlas-910b"
		at.Status.Entrance = "queue-for-atlas-910b"
		at.Status.Detail.Manufacturer = nodefeature.ManufacturerAscend
		at.Status.Detail.Family = "910B"
	})
	remoteWeights := func() *modelArtifactWeights {
		r := modelTransferRemoteRender()
		r.SourceReady = true
		return &modelArtifactWeights{Render: r}
	}
	storeConn := &ModelDeploymentConnectorInput{
		Domain:              "chat",
		Dtype:               "bfloat16",
		MasterServerAddress: "shared-kv-master.gpustack-system.svc:50051",
		Protocols:           []string{"ascend"},
	}

	assertNoDuplicateEnvNames := func(t *testing.T, env []core.EnvVar) {
		t.Helper()
		seen := make(map[string]int, len(env))
		for _, e := range env {
			seen[e.Name]++
		}
		for name, count := range seen {
			assert.Equalf(t, 1, count, "duplicate env var %s", name)
		}
	}
	countHIXLEP := func(pod *core.Pod) (int, int) {
		var vols, mounts int
		for _, v := range pod.Spec.Volumes {
			if v.Name == inject.AscendHIXLEPVolumeName {
				vols++
			}
		}
		for _, m := range pod.Spec.Containers[0].VolumeMounts {
			if m.Name == inject.AscendHIXLEPVolumeName {
				mounts++
			}
		}
		return vols, mounts
	}

	t.Run("A5 UB remote defaults ASCEND_LOCAL_COMM_RES deduplicated with KV connector and omits hixlep", func(t *testing.T) {
		for _, engine := range []string{workercore.ModelDeploymentEngineVLLM, workercore.ModelDeploymentEngineSGLang} {
			md := newRenderDeployment(func(md *workercore.ModelDeployment) {
				md.Spec.Engine.Name = engine
				md.Spec.Model.ArtifactRef = &core.LocalObjectReference{Name: "weights"}
				md.Spec.Roles = []workercore.ModelDeploymentRole{{
					Name: "server", Kind: workercore.ModelDeploymentRoleKindServer,
					AcceleratorType: "atlas-950", Image: md.Spec.Engine.Image, Replicas: 1, ReplicaSize: 1,
					Resources: &workercore.ModelDeploymentRoleResources{Accelerator: ptr.To(qty1)},
				}}
			})
			cli := newModelDeploymentClient(md, atlas950)
			r := &ModelDeploymentReconciler{Client: cli, APIReader: cli}
			pods, err := r.renderModelDeploymentRolePods(t.Context(), md, storeConn, []string{"ascend"}, remoteWeights(), false)
			require.NoError(t, err)
			pod := pods["server"][0][0]
			ctr := &pod.Spec.Containers[0]
			assertNoDuplicateEnvNames(t, ctr.Env)

			localEnv := envByName(ctr.Env, inject.AscendLocalCommResEnv)
			require.NotNil(t, localEnv)
			assert.Equal(t, inject.AscendLocalCommResAutoGenValue, localEnv.Value)
			assert.Nil(t, envByName(ctr.Env, inject.AscendGlobalResourceConfigEnv))
			volCount, mountCount := countHIXLEP(pod)
			assert.Zero(t, volCount)
			assert.Zero(t, mountCount)
		}
	})

	t.Run("A5 Host RoCE weights-only vLLM Server mounts hixlep and sets flat ASCEND_GLOBAL_RESOURCE_CONFIG without KV args", func(t *testing.T) {
		md := newRenderDeployment(func(md *workercore.ModelDeployment) {
			md.Spec.KVCache = nil
			md.Spec.Model.ArtifactRef = &core.LocalObjectReference{Name: "weights"}
			md.Spec.Roles = []workercore.ModelDeploymentRole{{
				Name: "server", Kind: workercore.ModelDeploymentRoleKindServer,
				AcceleratorType: "atlas-950", Image: md.Spec.Engine.Image, Replicas: 1, ReplicaSize: 1,
				Resources: &workercore.ModelDeploymentRoleResources{
					Accelerator: ptr.To(qty1),
					Interface:   ptr.To(qty1),
				},
			}}
		})
		cli := newModelDeploymentClient(md, atlas950)
		r := &ModelDeploymentReconciler{Client: cli, APIReader: cli}
		pods, err := r.renderModelDeploymentRolePods(t.Context(), md, nil, nil, remoteWeights(), true)
		require.NoError(t, err)
		pod := pods["server"][0][0]
		ctr := &pod.Spec.Containers[0]
		assertNoDuplicateEnvNames(t, ctr.Env)

		assert.Equal(t, qty1, ctr.Resources.Limits[rdmaShared])
		assert.NotContains(t, strings.Join(ctr.Command, " "), "ascend_local_comm_res_path",
			"weights-only Host RoCE must not alter vLLM KV connector args")
		volCount, mountCount := countHIXLEP(pod)
		assert.Equal(t, 1, volCount)
		assert.Equal(t, 1, mountCount)
		vol := findVolume(pod, inject.AscendHIXLEPVolumeName)
		require.NotNil(t, vol)
		require.NotNil(t, vol.HostPath)
		require.NotNil(t, vol.HostPath.Type)
		assert.Equal(t, core.HostPathDirectory, *vol.HostPath.Type)
		mnt := findMount(ctr, inject.AscendHIXLEPVolumeName)
		require.NotNil(t, mnt)
		assert.True(t, mnt.ReadOnly)

		globalEnv := envByName(ctr.Env, inject.AscendGlobalResourceConfigEnv)
		require.NotNil(t, globalEnv)
		assert.Equal(t, `{"comm_resource_config.protocol_desc":["roce:host"],"local_comm_res_path":"/etc/hixlep"}`, globalEnv.Value)
		assert.Nil(t, envByName(ctr.Env, inject.AscendLocalCommResEnv))
	})

	t.Run("A5 Host RoCE remote SGLang symmetric P/D mounts hixlep and preserves device_urma KV env", func(t *testing.T) {
		md := newRenderDeployment(func(md *workercore.ModelDeployment) {
			md.Spec.Engine.Name = workercore.ModelDeploymentEngineSGLang
			md.Spec.KVCache = nil
			md.Spec.Model.ArtifactRef = &core.LocalObjectReference{Name: "weights"}
			md.Spec.Router = &workercore.ModelDeploymentRouter{Name: workercore.ModelDeploymentRouterSGLang}
			md.Spec.KVTransfer = &workercore.ModelDeploymentKVTransfer{Backend: workercore.ModelDeploymentKVTransferBackendHIXL}
			md.Spec.Roles = []workercore.ModelDeploymentRole{
				{
					Name: "prefill", Kind: workercore.ModelDeploymentRoleKindPrefill,
					AcceleratorType: "atlas-950", Image: md.Spec.Engine.Image, Replicas: 1, ReplicaSize: 1,
					Resources: &workercore.ModelDeploymentRoleResources{
						Accelerator: ptr.To(qty1),
						Interface:   ptr.To(qty1),
					},
				},
				{
					Name: "decode", Kind: workercore.ModelDeploymentRoleKindDecode,
					AcceleratorType: "atlas-950", Image: md.Spec.Engine.Image, Replicas: 1, ReplicaSize: 1,
					Resources: &workercore.ModelDeploymentRoleResources{
						Accelerator: ptr.To(qty1),
						Interface:   ptr.To(qty1),
					},
				},
			}
		})
		cli := newModelDeploymentClient(md, atlas950)
		r := &ModelDeploymentReconciler{Client: cli, APIReader: cli}
		pods, err := r.renderModelDeploymentRolePods(t.Context(), md, nil, nil, remoteWeights(), true)
		require.NoError(t, err)

		for _, roleName := range []string{"prefill", "decode"} {
			pod := pods[roleName][0][0]
			ctr := &pod.Spec.Containers[0]
			assertNoDuplicateEnvNames(t, ctr.Env)
			assert.Equal(t, qty1, ctr.Resources.Limits[rdmaShared])
			volCount, mountCount := countHIXLEP(pod)
			assert.Equal(t, 1, volCount)
			assert.Equal(t, 1, mountCount)
			mfEnv := envByName(ctr.Env, inject.AscendMFTransferProtocolEnv)
			require.NotNil(t, mfEnv)
			assert.Equal(t, inject.AscendMFTransferProtocolDeviceURMA, mfEnv.Value)
			globalEnv := envByName(ctr.Env, inject.AscendGlobalResourceConfigEnv)
			require.NotNil(t, globalEnv)
			assert.Equal(t, `{"comm_resource_config.protocol_desc":["roce:host"],"local_comm_res_path":"/etc/hixlep"}`, globalEnv.Value)
			assert.Nil(t, envByName(ctr.Env, inject.AscendLocalCommResEnv))
		}
	})

	t.Run("A5 Host RoCE KV plus weights mounts /etc/hixlep once and yields on exact ancestor and descendant overlaps", func(t *testing.T) {
		overlapCases := []struct {
			name        string
			userPath    string
			wantYielded bool
		}{
			{name: "no_user_mount", userPath: "", wantYielded: false},
			{name: "exact_hixlep", userPath: "/etc/hixlep", wantYielded: true},
			{name: "trailing_slash_hixlep", userPath: "/etc/hixlep/", wantYielded: true},
			{name: "ancestor_etc", userPath: "/etc", wantYielded: true},
			{name: "ancestor_root", userPath: "/", wantYielded: true},
			{name: "descendant_file", userPath: "/etc/hixlep/ub_endpoint_npu_0.json", wantYielded: true},
			{name: "sibling_non_overlapping", userPath: "/etc/hixlep2", wantYielded: false},
		}
		for _, oc := range overlapCases {
			t.Run(oc.name, func(t *testing.T) {
				var avs []workercore.ModelDeploymentAdditionalVolume
				if oc.userPath != "" {
					avs = []workercore.ModelDeploymentAdditionalVolume{{
						MountPath: oc.userPath,
						ReadOnly:  true,
						ConfigMap: &core.LocalObjectReference{Name: "user-hixlep"},
					}}
				}
				md := newRenderDeployment(func(md *workercore.ModelDeployment) {
					md.Spec.Model.ArtifactRef = &core.LocalObjectReference{Name: "weights"}
					md.Spec.Router = &workercore.ModelDeploymentRouter{Name: workercore.ModelDeploymentRouterLLMD}
					md.Spec.KVTransfer = &workercore.ModelDeploymentKVTransfer{Backend: workercore.ModelDeploymentKVTransferBackendHIXL}
					md.Spec.Roles = []workercore.ModelDeploymentRole{
						{
							Name: "prefill", Kind: workercore.ModelDeploymentRoleKindPrefill,
							AcceleratorType: "atlas-950", Image: md.Spec.Engine.Image, Replicas: 1, ReplicaSize: 1,
							AdditionalVolumes: avs,
							Resources: &workercore.ModelDeploymentRoleResources{
								Accelerator: ptr.To(qty1),
								Interface:   ptr.To(qty1),
							},
						},
						{
							Name: "decode", Kind: workercore.ModelDeploymentRoleKindDecode,
							AcceleratorType: "atlas-950", Image: md.Spec.Engine.Image, Replicas: 1, ReplicaSize: 1,
							AdditionalVolumes: avs,
							Resources: &workercore.ModelDeploymentRoleResources{
								Accelerator: ptr.To(qty1),
								Interface:   ptr.To(qty1),
							},
						},
					}
				})
				cli := newModelDeploymentClient(md, atlas950)
				r := &ModelDeploymentReconciler{Client: cli, APIReader: cli}
				pods, err := r.renderModelDeploymentRolePods(t.Context(), md, storeConn, []string{"ascend"}, remoteWeights(), false)
				require.NoError(t, err)

				for _, roleName := range []string{"prefill", "decode"} {
					pod := pods[roleName][0][0]
					ctr := &pod.Spec.Containers[0]
					assertNoDuplicateEnvNames(t, ctr.Env)
					assert.Contains(t, strings.Join(ctr.Command, " "), `"ascend_local_comm_res_path":"/etc/hixlep"`)
					globalEnv := envByName(ctr.Env, inject.AscendGlobalResourceConfigEnv)
					require.NotNil(t, globalEnv)
					assert.Equal(t, `{"comm_resource_config.protocol_desc":["roce:host"],"local_comm_res_path":"/etc/hixlep"}`, globalEnv.Value)
					assert.Nil(t, envByName(ctr.Env, inject.AscendLocalCommResEnv))

					volCount, mountCount := countHIXLEP(pod)
					if oc.wantYielded {
						assert.Zero(t, volCount, "must yield gpustack-ascend-hixlep volume when user mounts %q", oc.userPath)
						assert.Zero(t, mountCount, "must yield gpustack-ascend-hixlep mount when user mounts %q", oc.userPath)
						assert.NotNil(t, findVolume(pod, "additional-0"))
					} else {
						assert.Equal(t, 1, volCount, "must mount gpustack-ascend-hixlep once")
						assert.Equal(t, 1, mountCount, "must mount gpustack-ascend-hixlep once")
					}
				}
			})
		}
	})

	t.Run("user role.Env overrides defaulted A5 remote env without duplicates", func(t *testing.T) {
		customGlobal := `{"comm_resource_config.protocol_desc":["roce:host"],"local_comm_res_path":"/custom/ep.json"}`
		md := newRenderDeployment(func(md *workercore.ModelDeployment) {
			md.Spec.KVCache = nil
			md.Spec.Model.ArtifactRef = &core.LocalObjectReference{Name: "weights"}
			md.Spec.Roles = []workercore.ModelDeploymentRole{{
				Name: "server", Kind: workercore.ModelDeploymentRoleKindServer,
				AcceleratorType: "atlas-950", Image: md.Spec.Engine.Image, Replicas: 1, ReplicaSize: 1,
				Env: []workercore.ModelDeploymentEnvVar{
					{Name: inject.AscendGlobalResourceConfigEnv, Value: customGlobal},
					{Name: inject.AscendLocalCommResEnv, Value: ""},
				},
				Resources: &workercore.ModelDeploymentRoleResources{
					Accelerator: ptr.To(qty1),
					Interface:   ptr.To(qty1),
				},
			}}
		})
		cli := newModelDeploymentClient(md, atlas950)
		r := &ModelDeploymentReconciler{Client: cli, APIReader: cli}
		pods, err := r.renderModelDeploymentRolePods(t.Context(), md, nil, nil, remoteWeights(), false)
		require.NoError(t, err)
		ctr := &pods["server"][0][0].Spec.Containers[0]
		assertNoDuplicateEnvNames(t, ctr.Env)
		assert.Equal(t, customGlobal, envByName(ctr.Env, inject.AscendGlobalResourceConfigEnv).Value)
		assert.Equal(t, "", envByName(ctr.Env, inject.AscendLocalCommResEnv).Value)
	})

	t.Run("pod-spec-hash stays stable on non-A5 and non-Remote renders and moves across A5 Remote modes", func(t *testing.T) {
		assert.Equal(t, 7, modelDeploymentRenderVersion)
		md910 := newRenderDeployment(func(md *workercore.ModelDeployment) {
			md.Spec.KVCache = nil
			md.Spec.Roles[0].AcceleratorType = "atlas-910b"
		})
		artBase := modelTransferRemoteRender()
		artBase.ModelTransferBackend = "hixl"
		pod910Clean, err := renderModelDeploymentPod(t.Context(), ModelDeploymentRenderInput{
			Deployment: md910, Role: &md910.Spec.Roles[0], AcceleratorType: atlas910b, Artifact: artBase,
		})
		require.NoError(t, err)

		artDirty := *artBase
		artDirty.ModelTransferAscendA5 = true
		artDirty.ModelTransferAscendHostRoCE = true
		pod910Dirty, err := renderModelDeploymentPod(t.Context(), ModelDeploymentRenderInput{
			Deployment: md910, Role: &md910.Spec.Roles[0], AcceleratorType: atlas910b, Artifact: &artDirty,
		})
		require.NoError(t, err)
		assert.Equal(t,
			pod910Clean.Annotations[modelDeploymentPodSpecHashAnnotation],
			pod910Dirty.Annotations[modelDeploymentPodSpecHashAnnotation],
			"A2/A3 pod-spec-hash must zero A5 remote fields",
		)

		mdA5 := newRenderDeployment(func(md *workercore.ModelDeployment) {
			md.Spec.KVCache = nil
			md.Spec.Roles[0].AcceleratorType = "atlas-950"
		})
		nodeClean := testNodeArtifactRender()
		nodeDirty := *nodeClean
		nodeDirty.ModelTransferAscendA5 = true
		nodeDirty.ModelTransferAscendHostRoCE = true
		podNodeClean, err := renderModelDeploymentPod(t.Context(), ModelDeploymentRenderInput{
			Deployment: mdA5, Role: &mdA5.Spec.Roles[0], AcceleratorType: atlas950, Artifact: nodeClean,
		})
		require.NoError(t, err)
		podNodeDirty, err := renderModelDeploymentPod(t.Context(), ModelDeploymentRenderInput{
			Deployment: mdA5, Role: &mdA5.Spec.Roles[0], AcceleratorType: atlas950, Artifact: &nodeDirty,
		})
		require.NoError(t, err)
		assert.Equal(t,
			podNodeClean.Annotations[modelDeploymentPodSpecHashAnnotation],
			podNodeDirty.Annotations[modelDeploymentPodSpecHashAnnotation],
			"non-Remote A5 pod-spec-hash must zero A5 remote fields",
		)

		podA5UB, err := renderModelDeploymentPod(t.Context(), ModelDeploymentRenderInput{
			Deployment: mdA5, Role: &mdA5.Spec.Roles[0], AcceleratorType: atlas950, Artifact: artBase,
		})
		require.NoError(t, err)
		mdA5RoCE := mdA5.DeepCopy()
		mdA5RoCE.Spec.Roles[0].Resources = &workercore.ModelDeploymentRoleResources{
			Accelerator: ptr.To(qty1),
			Interface:   ptr.To(qty1),
		}
		podA5RoCE, err := renderModelDeploymentPod(t.Context(), ModelDeploymentRenderInput{
			Deployment: mdA5RoCE, Role: &mdA5RoCE.Spec.Roles[0], AcceleratorType: atlas950,
			InterfaceResource: rdmaShared, Artifact: artBase,
		})
		require.NoError(t, err)
		assert.NotEqual(t,
			podA5UB.Annotations[modelDeploymentPodSpecHashAnnotation],
			podA5RoCE.Annotations[modelDeploymentPodSpecHashAnnotation],
		)
	})

	t.Run("role requesting interface with empty InterfaceResource never injects hixlep or ASCEND_GLOBAL_RESOURCE_CONFIG", func(t *testing.T) {
		mdNoGrant := newRenderDeployment(func(md *workercore.ModelDeployment) {
			md.Spec.KVCache = nil
			md.Spec.Roles[0].AcceleratorType = "atlas-950"
			md.Spec.Roles[0].Resources = &workercore.ModelDeploymentRoleResources{
				Accelerator: ptr.To(qty1),
				Interface:   ptr.To(qty1),
			}
		})
		artPreSet := modelTransferRemoteRender()
		artPreSet.ModelTransferBackend = "hixl"
		artPreSet.ModelTransferAscendA5 = true
		artPreSet.ModelTransferAscendHostRoCE = true

		inNoGrant := ModelDeploymentRenderInput{
			Deployment:      mdNoGrant,
			Role:            &mdNoGrant.Spec.Roles[0],
			AcceleratorType: atlas950,
			Artifact:        artPreSet,
		}
		norm := normalizeModelDeploymentArtifactForRole(inNoGrant, false)
		require.NotNil(t, norm)
		assert.True(t, norm.ModelTransferAscendA5)
		assert.False(t, norm.ModelTransferAscendHostRoCE,
			"Host RoCE must stay false when InterfaceResource is empty even if role.Resources.Interface is 1")

		vols, mounts := appendModelDeploymentConnectorVolumes(nil, nil, ModelDeploymentConnectorRender{}, norm)
		assert.Empty(t, vols)
		assert.Empty(t, mounts)
		ctr := core.Container{
			Name:    modelDeploymentMainContainerName,
			Command: []string{"vllm", "serve", "/var/lib/gpustack/model"},
		}
		norm.applyModelTransfer(&ctr, workercore.ModelDeploymentEngineVLLM)
		assert.Nil(t, envByName(ctr.Env, inject.AscendGlobalResourceConfigEnv))
		localEnv := envByName(ctr.Env, inject.AscendLocalCommResEnv)
		require.NotNil(t, localEnv)
		assert.Equal(t, inject.AscendLocalCommResAutoGenValue, localEnv.Value)

		// Fallback branch when AcceleratorType is nil must also clear Host RoCE without InterfaceResource.
		inNilAcc := inNoGrant
		inNilAcc.AcceleratorType = nil
		normNil := normalizeModelDeploymentArtifactForRole(inNilAcc, false)
		require.NotNil(t, normNil)
		assert.False(t, normNil.ModelTransferAscendHostRoCE)

		// Direct Pod render with pre-set ModelTransferAscendHostRoCE=true and empty InterfaceResource.
		mdUB := newRenderDeployment(func(md *workercore.ModelDeployment) {
			md.Spec.KVCache = nil
			md.Spec.Roles[0].AcceleratorType = "atlas-950"
		})
		podUB, err := renderModelDeploymentPod(t.Context(), ModelDeploymentRenderInput{
			Deployment: mdUB, Role: &mdUB.Spec.Roles[0], AcceleratorType: atlas950, Artifact: artPreSet,
		})
		require.NoError(t, err)
		volCount, mountCount := countHIXLEP(podUB)
		assert.Zero(t, volCount)
		assert.Zero(t, mountCount)
		assert.Nil(t, envByName(podUB.Spec.Containers[0].Env, inject.AscendGlobalResourceConfigEnv))
	})
}

func TestModelDeploymentScaler_AscendA5ConnectorIgnoresRemoteWeightLeg(t *testing.T) {
	qty1 := resource.MustParse("1")

	t.Run("single-Pod A5 Server with ArtifactRef keeps kvDirectProtocol empty while directProtocol is rdma", func(t *testing.T) {
		md := newRenderDeployment(func(md *workercore.ModelDeployment) {
			md.Spec.KVCache = nil
			md.Spec.Model.ArtifactRef = &core.LocalObjectReference{Name: "weights"}
			md.Spec.Router = &workercore.ModelDeploymentRouter{Name: workercore.ModelDeploymentRouterVLLM}
			md.Spec.Roles = []workercore.ModelDeploymentRole{{
				Name:            "server",
				Kind:            workercore.ModelDeploymentRoleKindServer,
				AcceleratorType: "atlas-950",
				Image:           md.Spec.Engine.Image,
				Replicas:        1,
				ReplicaSize:     1,
				Resources: &workercore.ModelDeploymentRoleResources{
					Accelerator: ptr.To(qty1),
					Interface:   ptr.To(qty1),
				},
			}}
		})
		role := &md.Spec.Roles[0]
		assert.Equal(t, "rdma", ModelDeploymentDirectInterfaceProtocol(
			md, role, nodefeature.ManufacturerAscend, nodefeature.AcceleratorFamilyAscend950, true, false,
		))
		kvDirect := modelDeploymentAscendKVDirectProtocol(
			md, role, nodefeature.ManufacturerAscend, nodefeature.AcceleratorFamilyAscend950,
		)
		assert.Empty(t, kvDirect)
		assert.False(t, modelDeploymentAscendHostRoCE(
			workercore.ModelDeploymentEngineVLLM, nodefeature.ManufacturerAscend,
			nodefeature.AcceleratorFamilyAscend950, kvDirect, role,
		))

		conn, err := modelDeploymentScalerConnectorRender(
			workercore.ModelDeploymentEngineVLLM, md, role,
			nodefeature.ManufacturerAscend, nodefeature.AcceleratorFamilyAscend950,
		)
		require.NoError(t, err)
		assert.False(t, slices.ContainsFunc(conn.Volumes, func(v core.Volume) bool {
			return v.Name == inject.AscendHIXLEPVolumeName
		}))
		require.NoError(t, modelDeploymentScalerOperatorEnvGateWithManufacturer(
			workercore.ModelDeploymentEngineVLLM, md, role,
			nodefeature.ManufacturerAscend, nodefeature.AcceleratorFamilyAscend950,
		))
	})

	t.Run("asymmetric vs symmetric A5 P/D with ArtifactRef follows KV direct leg", func(t *testing.T) {
		md := newRenderDeployment(func(md *workercore.ModelDeployment) {
			md.Spec.KVCache = nil
			md.Spec.Model.ArtifactRef = &core.LocalObjectReference{Name: "weights"}
			md.Spec.Router = &workercore.ModelDeploymentRouter{Name: workercore.ModelDeploymentRouterLLMD}
			md.Spec.KVTransfer = &workercore.ModelDeploymentKVTransfer{Backend: workercore.ModelDeploymentKVTransferBackendHIXL}
			md.Spec.Roles = []workercore.ModelDeploymentRole{
				{
					Name: "prefill", Kind: workercore.ModelDeploymentRoleKindPrefill,
					AcceleratorType: "atlas-950", Image: md.Spec.Engine.Image, Replicas: 1, ReplicaSize: 1,
					Resources: &workercore.ModelDeploymentRoleResources{
						Accelerator: ptr.To(qty1),
						Interface:   ptr.To(qty1),
					},
				},
				{
					Name: "decode", Kind: workercore.ModelDeploymentRoleKindDecode,
					AcceleratorType: "atlas-950", Image: md.Spec.Engine.Image, Replicas: 1, ReplicaSize: 1,
					Resources: &workercore.ModelDeploymentRoleResources{Accelerator: ptr.To(qty1)},
				},
			}
		})

		asymConn, err := modelDeploymentScalerConnectorRender(
			workercore.ModelDeploymentEngineVLLM, md, &md.Spec.Roles[0],
			nodefeature.ManufacturerAscend, nodefeature.AcceleratorFamilyAscend950,
		)
		require.NoError(t, err)
		assert.False(t, slices.ContainsFunc(asymConn.Volumes, func(v core.Volume) bool {
			return v.Name == inject.AscendHIXLEPVolumeName
		}))
		assert.NotNil(t, envByName(asymConn.DefaultedEnv, inject.AscendLocalCommResEnv))
		renderedEnv, err := modelDeploymentScalerRenderedOperatorEnv(
			workercore.ModelDeploymentEngineVLLM, md, &md.Spec.Roles[0],
			nodefeature.ManufacturerAscend, nodefeature.AcceleratorFamilyAscend950,
		)
		require.NoError(t, err)
		assert.Contains(t, renderedEnv, inject.AscendLocalCommResEnv)

		md.Spec.Roles[1].Resources.Interface = ptr.To(qty1)
		symConn, err := modelDeploymentScalerConnectorRender(
			workercore.ModelDeploymentEngineVLLM, md, &md.Spec.Roles[0],
			nodefeature.ManufacturerAscend, nodefeature.AcceleratorFamilyAscend950,
		)
		require.NoError(t, err)
		assert.True(t, slices.ContainsFunc(symConn.Volumes, func(v core.Volume) bool {
			return v.Name == inject.AscendHIXLEPVolumeName
		}))
		assert.Nil(t, envByName(symConn.DefaultedEnv, inject.AscendLocalCommResEnv))
	})
}
