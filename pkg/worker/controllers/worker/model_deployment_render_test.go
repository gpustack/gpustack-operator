package worker

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
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
	"gpustack.ai/gpustack/pkg/worker/kvcache/inject"
)

// newRenderDeployment builds a deployment whose single role every case then varies.
func newRenderDeployment(mutate ...func(*workercore.ModelDeployment)) *workercore.ModelDeployment {
	md := &workercore.ModelDeployment{
		ObjectMeta: meta.ObjectMeta{Name: "qwen", Namespace: "team-a", UID: "md-uid"},
		Spec: workercore.ModelDeploymentSpec{
			Model: workercore.ModelDeploymentModel{Name: "Qwen/Qwen2.5-72B-Instruct"},
			Engine: workercore.ModelDeploymentEngine{
				Name: workercore.ModelDeploymentEngineVLLM, Version: "0.25.1",
			},
			KVCache: &workercore.ModelDeploymentKVCache{PoolRef: core.LocalObjectReference{Name: "shared-kv"}},
			Roles: []workercore.ModelDeploymentRole{{
				Name:         "server",
				Replicas:     2,
				InstanceType: "h20-8x",
				Image:        "vllm/vllm-openai:v0.25.1",
			}},
		},
	}
	for _, m := range mutate {
		m(md)
	}

	return md
}

// newRenderInstanceType builds an accelerated InstanceType whose per-card unit resources every
// sizing case reads: 16 CPU and 64Gi of RAM for one whole card.
func newRenderInstanceType(mutate ...func(*worker.InstanceType)) *worker.InstanceType {
	it := &worker.InstanceType{
		ObjectMeta: meta.ObjectMeta{Name: "h20-8x"},
		Spec: workercore.InstanceTypeSpec{
			Acceleratable: true,
			UnitResources: workercore.InstanceTypeUnitResources{CPU: "16", RAM: "64Gi"},
			LocalStorage:  "512Gi",
		},
		Status: workercore.InstanceTypeStatus{
			// DELIBERATELY NOT FormatLocalQueueName("h20-8x"). The entrance is read from this field,
			// and a fixture spelling it the way a name-derived render would spell it could not tell
			// the two apart -- the assertion in _Identity would pass either way. This value is one
			// no derivation produces.
			Entrance: "queue-for-h20-8x",
			// The observed detail carries what image synthesis needs, so a role naming no image
			// still renders. A case that wants the unsynthesizable path clears one of these.
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
		m(it)
	}

	return it
}

func renderOne(t *testing.T, md *workercore.ModelDeployment, it *worker.InstanceType) *core.Pod {
	t.Helper()

	pod, err := renderModelDeploymentPod(context.Background(), ModelDeploymentRenderInput{
		Deployment:   md,
		Role:         &md.Spec.Roles[0],
		InstanceType: it,
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
		Deployment:   md,
		Role:         &md.Spec.Roles[0],
		InstanceType: newRenderInstanceType(),
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
				Deployment: md, Role: &md.Spec.Roles[0], InstanceType: newRenderInstanceType(),
				Connector: ModelDeploymentConnectorRender{
					Args: []string{"--kv-transfer-config", `{}`}, KVTransfer: true, RoutingSidecar: true,
				},
				NativeSidecar: true,
			})
			require.NoError(t, err)

			require.Len(t, pod.Spec.InitContainers, 1)
			sidecar := pod.Spec.InitContainers[0]
			assert.Equal(t, "routing-proxy", sidecar.Name)
			assert.Equal(t, "gpustack/mirrored-llm-d-router-disagg-sidecar:v0.10.0", sidecar.Image)
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
				Deployment: md, Role: &md.Spec.Roles[0], InstanceType: newRenderInstanceType(),
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
			wantFill:  []string{"--host", "0.0.0.0"},
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
				Deployment: md, Role: &md.Spec.Roles[0], InstanceType: newRenderInstanceType(),
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
		Deployment: md, Role: &md.Spec.Roles[0], InstanceType: newRenderInstanceType(),
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
	assert.Equal(t, "gpustack/mirrored-llm-d-router-disagg-sidecar:v0.10.0", sidecar.Image)
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
			workercore.ModelDeploymentEngineVLLM, true, nodefeature.ManufacturerNVIDIA)

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
			workercore.ModelDeploymentEngineVLLM, true, nodefeature.ManufacturerAscend)

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
			workercore.ModelDeploymentEngineSGLang, true, nodefeature.ManufacturerNVIDIA)

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
		workercore.ModelDeploymentEngineSGLang, true, nodefeature.ManufacturerNVIDIA)
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
	t.Run("engine-only keeps the shared route", func(t *testing.T) {
		md := newRenderDeployment()

		pod, err := renderModelDeploymentPod(context.Background(), ModelDeploymentRenderInput{
			Deployment: md, Role: &md.Spec.Roles[0], InstanceType: newRenderInstanceType(),
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

	t.Run("direct decode reads the route the sidecar forwards to the engine", func(t *testing.T) {
		// THE DEFECT THIS CASE GUARDS: the sidecar owns the service port and answers /health itself,
		// in every state, so a gate on that route grades the sidecar and reports Ready for the whole
		// model load. /v1/models is the route the sidecar forwards, and the forward fails until the
		// engine serves -- which is what makes the gate below an engine gate again.
		md := newRenderDeployment(func(md *workercore.ModelDeployment) {
			md.Spec.Router = &workercore.ModelDeploymentRouter{Name: workercore.ModelDeploymentRouterLLMD}
			md.Spec.Roles[0].Kind = workercore.ModelDeploymentRoleKindDecode
			md.Spec.Roles[0].Ports = []workercore.ModelDeploymentPort{{
				Protocol: core.ProtocolTCP, Port: 8000,
			}}
		})

		pod, err := renderModelDeploymentPod(context.Background(), ModelDeploymentRenderInput{
			Deployment: md, Role: &md.Spec.Roles[0], InstanceType: newRenderInstanceType(),
			Connector: ModelDeploymentConnectorRender{
				Args: []string{"--kv-transfer-config", `{}`}, KVTransfer: true, RoutingSidecar: true,
			},
			NativeSidecar: true,
		})
		require.NoError(t, err)

		c := pod.Spec.Containers[0]
		for name, p := range map[string]*core.Probe{
			"startup": c.StartupProbe, "readiness": c.ReadinessProbe, "liveness": c.LivenessProbe,
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

	pod := renderOne(t, md, newRenderInstanceType())
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
			want:     string(workercore.ModelDeploymentRoleKindServer),
		},
		{
			name:     "prefill",
			roleName: "first-half",
			kind:     workercore.ModelDeploymentRoleKindPrefill,
			want:     string(workercore.ModelDeploymentRoleKindPrefill),
		},
		{
			name:     "decode",
			roleName: "second-half",
			kind:     workercore.ModelDeploymentRoleKindDecode,
			want:     string(workercore.ModelDeploymentRoleKindDecode),
		},
		{
			name:     "a role named decode whose kind is prefill is labelled prefill",
			roleName: "decode",
			kind:     workercore.ModelDeploymentRoleKindPrefill,
			want:     string(workercore.ModelDeploymentRoleKindPrefill),
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			md := newRenderDeployment(func(md *workercore.ModelDeployment) {
				md.Spec.Roles[0].Name = tc.roleName
				md.Spec.Roles[0].Kind = tc.kind
			})

			pod := renderOne(t, md, newRenderInstanceType())

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

	pod := renderOne(t, md, newRenderInstanceType())
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
			Name:         "d",
			Replicas:     2,
			InstanceType: "h20-8x",
			Kind:         workercore.ModelDeploymentRoleKindDecode,
			Image:        "vllm/vllm-openai:v0.25.1",
		})
	})

	it := newRenderInstanceType()
	labelOf := func(i int) string {
		pod, err := renderModelDeploymentPod(context.Background(), ModelDeploymentRenderInput{
			Deployment:   md,
			Role:         &md.Spec.Roles[i],
			InstanceType: it,
		})
		require.NoError(t, err)

		return pod.Labels[modelDeploymentLabelKeyRoleKind]
	}

	assert.Equal(t, string(workercore.ModelDeploymentRoleKindPrefill), labelOf(0))
	assert.Equal(t, string(workercore.ModelDeploymentRoleKindDecode), labelOf(1))
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
				Deployment:   md,
				Role:         &md.Spec.Roles[0],
				InstanceType: newRenderInstanceType(),
				Connector:    tc.connector,
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
		Deployment:   md,
		Role:         &md.Spec.Roles[0],
		InstanceType: newRenderInstanceType(),
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

	assert.Empty(t, pod.Spec.Volumes, "and the file itself is not mounted either")
	assert.Empty(t, pod.Spec.Containers[0].VolumeMounts, "nor is it mounted into the container")
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
		Deployment:   md,
		Role:         &md.Spec.Roles[0],
		InstanceType: newRenderInstanceType(),
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
			}, connector, false)
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
			Deployment: md, Role: &md.Spec.Roles[0], InstanceType: newRenderInstanceType(),
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
			Deployment: md, Role: &md.Spec.Roles[0], InstanceType: newRenderInstanceType(),
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
				Deployment: md, Role: &md.Spec.Roles[0], InstanceType: newRenderInstanceType(),
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
	} {
		t.Run(tc.name, func(t *testing.T) {
			md := newRenderDeployment(func(md *workercore.ModelDeployment) {
				md.Spec.Engine.Name = workercore.ModelDeploymentEngineSGLang
				md.Spec.Roles[0].Kind = tc.kind
				md.Spec.Roles[0].ExtraArgs = tc.extraArgs
			})
			in := ModelDeploymentConnectorInput{
				Engine: workercore.ModelDeploymentEngineSGLang, Kind: tc.kind,
				Disaggregated: true, KVTransfer: true,
			}
			if tc.store {
				in.MasterServerAddress = "shared-kv-master.gpustack-system.svc:50051"
				in.Protocols = []string{"tcp"}
			}
			connector, err := SynthesizeModelDeploymentConnector(in)
			require.NoError(t, err)

			pod, err := renderModelDeploymentPod(context.Background(), ModelDeploymentRenderInput{
				Deployment: md, Role: &md.Spec.Roles[0], InstanceType: newRenderInstanceType(),
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
				Deployment:   md,
				Role:         &md.Spec.Roles[0],
				InstanceType: newRenderInstanceType(),
				Connector:    connector,
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

// TestDeriveModelDeploymentResources covers the half of a replica's request that is DERIVED rather
// than declared. CPU and memory are inexpressible on a role, so getting this wrong charges quota
// for something other than what runs — and nothing in the API would show it.
func TestDeriveModelDeploymentResources(t *testing.T) {
	testCases := []struct {
		name string

		resources *workercore.ModelDeploymentRoleResources
		instType  func(*worker.InstanceType)

		wantCPU, wantRAM string
		wantErr          string
	}{
		{
			name:      "whole cards scale the unit resources by the card count",
			resources: &workercore.ModelDeploymentRoleResources{Accelerator: ptr.To(resource.MustParse("4"))},
			wantCPU:   "64", wantRAM: "256Gi",
		},
		{
			name:    "no accelerator at all still sizes one unit's worth of host",
			wantCPU: "16", wantRAM: "64Gi",
		},
		{
			name: "a memory slice takes that percentage of one card's worth",
			resources: &workercore.ModelDeploymentRoleResources{
				Accelerator:                       ptr.To(resource.MustParse("1")),
				AcceleratorSlicedMemoryPercentage: 50,
			},
			instType: func(it *worker.InstanceType) { it.Status.Detail.SlicedDetail.Logical.Count = 128 },
			wantCPU:  "8", wantRAM: "32Gi",
		},
		{
			name: "a bare compute slice copies across, so the host follows the same fraction",
			resources: &workercore.ModelDeploymentRoleResources{
				Accelerator:                      ptr.To(resource.MustParse("1")),
				AcceleratorSlicedCoresPercentage: 25,
			},
			instType: func(it *worker.InstanceType) { it.Status.Detail.SlicedDetail.Logical.Count = 128 },
			wantCPU:  "4", wantRAM: "16Gi",
		},
		{
			name: "a partition profile is anchored on the share of VRAM it occupies",
			resources: &workercore.ModelDeploymentRoleResources{
				Accelerator:                   ptr.To(resource.MustParse("1")),
				AcceleratorPartitionedProfile: "3g.40gb",
			},
			instType: func(it *worker.InstanceType) {
				it.Status.Detail.Memory = "80Gi"
				it.Status.Detail.SlicedDetail.Physical.Profiles = []workercore.AcceleratorSlicedPhysicalDetailProfile{
					{Name: "3g.40gb", MemoryMib: 40 << 10, Count: 2},
				}
			},
			wantCPU: "8", wantRAM: "32Gi",
		},
		{
			name: "a slice against an uncomputed detail is retryable, never a whole card",
			resources: &workercore.ModelDeploymentRoleResources{
				Accelerator:                       ptr.To(resource.MustParse("1")),
				AcceleratorSlicedMemoryPercentage: 50,
			},
			instType: func(it *worker.InstanceType) { it.Status.Detail.Manufacturer = "" },
			wantErr:  "not ready yet",
		},
		{
			name: "a partition the detail cannot size yet is retryable too",
			resources: &workercore.ModelDeploymentRoleResources{
				Accelerator:                   ptr.To(resource.MustParse("1")),
				AcceleratorPartitionedProfile: "3g.40gb",
			},
			instType: func(it *worker.InstanceType) {
				it.Status.Detail.SlicedDetail.Physical.Profiles = []workercore.AcceleratorSlicedPhysicalDetailProfile{
					{Name: "3g.40gb", MemoryMib: 0},
				}
			},
			wantErr: "cannot be sized",
		},
		{
			name:      "an unparseable unit is reported rather than rendered as zero",
			resources: &workercore.ModelDeploymentRoleResources{Accelerator: ptr.To(resource.MustParse("1"))},
			instType:  func(it *worker.InstanceType) { it.Spec.UnitResources.CPU = "not-a-quantity" },
			wantErr:   "invalid CPU unit",
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			it := newRenderInstanceType()
			if tc.instType != nil {
				tc.instType(it)
			}
			role := &workercore.ModelDeploymentRole{Name: "server", Resources: tc.resources}

			ress, err := deriveModelDeploymentResources(role, it)
			if tc.wantErr != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tc.wantErr)

				return
			}

			require.NoError(t, err)
			qtyEqual(t, qty(tc.wantCPU), ress.CPU, "cpu")
			qtyEqual(t, qty(tc.wantRAM), ress.RAM, "ram")
			qtyEqual(t, qty("15Gi"), ress.LocalStorage, "local storage")
		})
	}
}

// TestDeriveModelDeploymentResources_LocalStorage pins both sides of the ephemeral-storage default,
// which is a cap and not a constant: 15Gi is what a replica asks for, unless the InstanceType offers
// less, in which case asking for the default would make every replica unschedulable.
func TestDeriveModelDeploymentResources_LocalStorage(t *testing.T) {
	testCases := []struct {
		name        string
		offered     string
		wantStorage string
	}{
		{name: "the type offers more than the default", offered: "512Gi", wantStorage: "15Gi"},
		{name: "the type offers nothing at all", offered: "", wantStorage: "15Gi"},
		{name: "the type offers less than the default", offered: "8Gi", wantStorage: "8Gi"},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			it := newRenderInstanceType(func(it *worker.InstanceType) { it.Spec.LocalStorage = tc.offered })
			role := &workercore.ModelDeploymentRole{Name: "server"}

			ress, err := deriveModelDeploymentResources(role, it)
			require.NoError(t, err)
			qtyEqual(t, qty(tc.wantStorage), ress.LocalStorage, "local storage")
		})
	}
}

// TestRenderModelDeploymentPod_AcceleratorRequest checks that the derived values and the declared
// accelerator reach the container through the one resource renderer both CRDs share.
func TestRenderModelDeploymentPod_AcceleratorRequest(t *testing.T) {
	md := newRenderDeployment(func(md *workercore.ModelDeployment) {
		md.Spec.Roles[0].Resources = &workercore.ModelDeploymentRoleResources{
			Accelerator: ptr.To(resource.MustParse("2")),
		}
	})

	pod := renderOne(t, md, newRenderInstanceType())

	accName := nodefeature.GetAcceleratableResourceName(
		nodefeature.ManufacturerNVIDIA, workercore.DeviceAllocationModeExclusive)
	limits := pod.Spec.Containers[0].Resources.Limits
	qtyEqual(t, qty("2"), limits[accName], "the declared card count reaches the container")
	qtyEqual(t, qty("32"), limits[core.ResourceCPU], "the host CPU is derived, not declared")
	qtyEqual(t, qty("128Gi"), limits[core.ResourceMemory], "the host memory is derived, not declared")
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
				Deployment: md, Role: &md.Spec.Roles[0], InstanceType: newRenderInstanceType(),
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
		name         string
		engine       string
		manufacturer string
		command      []string
		want         string
	}{
		{name: "native vLLM", engine: workercore.ModelDeploymentEngineVLLM, manufacturer: nodefeature.ManufacturerNVIDIA, want: "efa"},
		{name: "Ascend ignores declaration", engine: workercore.ModelDeploymentEngineVLLM, manufacturer: nodefeature.ManufacturerAscend},
		{name: "unobserved manufacturer cannot select device", engine: workercore.ModelDeploymentEngineVLLM},
		{name: "SGLang ignores declaration", engine: workercore.ModelDeploymentEngineSGLang, manufacturer: nodefeature.ManufacturerNVIDIA},
		{name: "user command owns transfer", engine: workercore.ModelDeploymentEngineVLLM, manufacturer: nodefeature.ManufacturerNVIDIA, command: []string{"custom"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			md := newRenderDeployment(func(md *workercore.ModelDeployment) {
				md.Spec.Engine.Name = tc.engine
				md.Spec.KVCache = nil
				md.Spec.KVTransfer = &workercore.ModelDeploymentKVTransfer{Protocol: "efa"}
				md.Spec.Router = &workercore.ModelDeploymentRouter{Name: workercore.ModelDeploymentRouterLLMD}
				md.Spec.Roles[0].Kind = workercore.ModelDeploymentRoleKindPrefill
				md.Spec.Roles[0].Command = tc.command
				md.Spec.Roles = append(md.Spec.Roles, workercore.ModelDeploymentRole{Name: "decode", Kind: workercore.ModelDeploymentRoleKindDecode})
			})
			assert.Equal(t, tc.want, ModelDeploymentDirectInterfaceProtocol(md, &md.Spec.Roles[0], tc.manufacturer))
		})
	}
}

// TestRenderModelDeploymentPod_Ports pins that a replica is always reachable: a role naming no port
// still gets one, because the Service fronting the deployment needs a target.
func TestRenderModelDeploymentPod_Ports(t *testing.T) {
	md := newRenderDeployment()
	pod := renderOne(t, md, newRenderInstanceType())
	require.Len(t, pod.Spec.Containers[0].Ports, 1)
	assert.Equal(t, int32(8000), pod.Spec.Containers[0].Ports[0].ContainerPort)
	assert.Equal(t, "http", pod.Spec.Containers[0].Ports[0].Name)

	md = newRenderDeployment(func(md *workercore.ModelDeployment) {
		md.Spec.Roles[0].Ports = []workercore.ModelDeploymentPort{
			{Port: 9000, Protocol: core.ProtocolTCP},
		}
	})
	pod = renderOne(t, md, newRenderInstanceType())
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
			pod := renderOne(t, md, newRenderInstanceType())
			assert.Equal(t, "true", pod.Annotations["prometheus.io/scrape"])
			assert.Equal(t, "/metrics", pod.Annotations["prometheus.io/path"])
			assert.Equal(t, strconv.Itoa(int(tc.port)), pod.Annotations["prometheus.io/port"])
			assert.Equal(t, tc.scheme, pod.Annotations["prometheus.io/scheme"])
			assert.Equal(t, tc.port, pod.Spec.Containers[0].Ports[0].ContainerPort)
		})
	}
}

// TestRenderModelDeploymentPod_SynthesizesTheImage covers the role that names none.
//
// The operator used to refuse this outright, on the grounds that it builds the argv but never the
// image. It now assembles one from the engine and the observed hardware -- but only when the
// hardware HAS been observed, which is why the second case matters as much as the first: an
// InstanceType whose detail has not converged must not produce a tag with a hole in it.
func TestRenderModelDeploymentPod_SynthesizesTheImage(t *testing.T) {
	t.Run("no image at all still renders", func(t *testing.T) {
		md := newRenderDeployment(func(md *workercore.ModelDeployment) {
			md.Spec.Roles[0].Image = ""
		})

		pod, err := renderModelDeploymentPod(context.Background(), ModelDeploymentRenderInput{
			Deployment:   md,
			Role:         &md.Spec.Roles[0],
			InstanceType: newRenderInstanceType(),
		})
		require.NoError(t, err, "an empty image is not an error once one can be synthesized")
		assert.Equal(t, "gpustack/runner:cuda12.9-vllm0.25.1", pod.Spec.Containers[0].Image)
	})

	t.Run("a stated image wins over synthesis", func(t *testing.T) {
		md := newRenderDeployment()
		pod := renderOne(t, md, newRenderInstanceType())
		assert.NotEqual(t, "gpustack/runner:cuda12.9-vllm0.25.1", pod.Spec.Containers[0].Image,
			"synthesis must not overwrite a value the user stated")
	})

	t.Run("an unobserved runtime version refuses rather than rendering a hole", func(t *testing.T) {
		md := newRenderDeployment(func(md *workercore.ModelDeployment) {
			md.Spec.Roles[0].Image = ""
		})
		it := newRenderInstanceType(func(it *worker.InstanceType) {
			it.Status.Detail.RuntimeVersion = ""
			it.Status.Detail.RuntimeVersions = nil
		})

		_, err := renderModelDeploymentPod(context.Background(), ModelDeploymentRenderInput{
			Deployment:   md,
			Role:         &md.Spec.Roles[0],
			InstanceType: it,
		})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "names no image and none could be synthesized")
		assert.Contains(t, err.Error(), "has not observed a runtime version yet",
			"the reason has to distinguish a wait from a refusal, because only one of them resolves")
	})

	t.Run("a manufacturer with no runner backend refuses", func(t *testing.T) {
		md := newRenderDeployment(func(md *workercore.ModelDeployment) {
			md.Spec.Roles[0].Image = ""
		})
		it := newRenderInstanceType(func(it *worker.InstanceType) {
			it.Status.Detail.Manufacturer = nodefeature.ManufacturerCambricon
		})

		_, err := renderModelDeploymentPod(context.Background(), ModelDeploymentRenderInput{
			Deployment:   md,
			Role:         &md.Spec.Roles[0],
			InstanceType: it,
		})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "has no runner backend",
			"this one never resolves, so the message must name the manufacturer")
	})

	// THE FALLBACK IS WHAT MAKES THIS AN ERROR PATH RATHER THAN A DEFAULT. A Pod carrying no
	// queue-name label is not queued by Kueue at all -- the kubelet runs it directly, charging no
	// quota and passing none of the gates the chain applies. So an InstanceType that has not
	// published its entrance yet has to stop the render, exactly as an uncomputed accelerator detail
	// does, rather than yield a Pod that would run.
	t.Run("an instance type with no published entrance refuses", func(t *testing.T) {
		md := newRenderDeployment()
		it := newRenderInstanceType(func(it *worker.InstanceType) {
			it.Status.Entrance = ""
		})

		pod, err := renderModelDeploymentPod(context.Background(), ModelDeploymentRenderInput{
			Deployment:   md,
			Role:         &md.Spec.Roles[0],
			InstanceType: it,
		})
		require.Error(t, err)
		assert.Nil(t, pod, "no Pod may be returned, or a caller could create one with no queue")
		assert.Contains(t, err.Error(), "publishes no queue entrance yet")
		assert.Contains(t, err.Error(), "h20-8x",
			"the message must name the instance type, since the field is two objects away")
	})
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
		connectorInput(workercore.ModelDeploymentEngineVLLM, nodefeature.ManufacturerNVIDIA))
	require.NoError(t, err)

	pod, err := renderModelDeploymentPod(context.Background(), ModelDeploymentRenderInput{
		Deployment:   md,
		Role:         &md.Spec.Roles[0],
		InstanceType: newRenderInstanceType(),
		Connector:    conn,
	})
	require.NoError(t, err)

	require.Len(t, pod.Spec.Volumes, 1)
	require.NotNil(t, pod.Spec.Volumes[0].DownwardAPI,
		"the file is a projection of an annotation, not a ConfigMap: there is no second object")
	require.Len(t, pod.Spec.Volumes[0].DownwardAPI.Items, 1)

	ref := pod.Spec.Volumes[0].DownwardAPI.Items[0].FieldRef
	require.NotNil(t, ref)
	assert.Contains(t, ref.FieldPath, inject.ClientConfigAnnotationKey)
	assert.NotEmpty(t, pod.Annotations[inject.ClientConfigAnnotationKey],
		"the projection reads this annotation, so an absent or empty one mounts an empty file")

	mounts := pod.Spec.Containers[0].VolumeMounts
	require.Len(t, mounts, 1)
	assert.Equal(t, inject.ConfigMountPath, mounts[0].MountPath)
	assert.True(t, mounts[0].ReadOnly)
}

// TestRenderModelDeploymentPod_NoClientConfigWithoutOne pins the state a deployment is in before its
// Binding has been resolved: a replica renders and runs, with no mount and no volume, rather than
// referencing a ConfigMap nothing has written.
func TestRenderModelDeploymentPod_NoClientConfigWithoutOne(t *testing.T) {
	md := newRenderDeployment()
	pod := renderOne(t, md, newRenderInstanceType())

	assert.Empty(t, pod.Spec.Volumes)
	assert.Empty(t, pod.Spec.Containers[0].VolumeMounts)
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

	first := renderOne(t, md, newRenderInstanceType())
	second := renderOne(t, md, newRenderInstanceType())

	assert.Equal(t,
		first.Annotations[modelDeploymentPodSpecHashAnnotation],
		second.Annotations[modelDeploymentPodSpecHashAnnotation])
}

// TestModelDeploymentPodSpecHash_DoesNotCoverItself is the other half of that property. A
// fingerprint that covered the annotation holding it would differ from the one already stored on
// every pass, which reads exactly like a spec that keeps changing.
func TestModelDeploymentPodSpecHash_DoesNotCoverItself(t *testing.T) {
	md := newRenderDeployment()
	pod := renderOne(t, md, newRenderInstanceType())

	stored := pod.Annotations[modelDeploymentPodSpecHashAnnotation]
	assert.Equal(t, stored, modelDeploymentPodSpecHash(pod),
		"rehashing a rendered Pod must reproduce the value it already carries")
}

// TestModelDeploymentPodSpecHash_DoesNotCoverTheName is the migration half of the fingerprint's
// subject: the hash covers {Labels, Annotations, Spec} and no part of the naming, so a replica
// named by an earlier scheme is not stale merely for being old. The rollout compares fingerprints,
// and a hash that read the name would read every surviving replica as outdated the moment the
// naming scheme changed.
//
// THE POSITIVE BASELINE RUNS IN THE SAME TABLE on purpose. "The fingerprint ignores the name" alone
// would pass against a fingerprint that ignored everything, so beside the case that must not move
// it sits one that must.
func TestModelDeploymentPodSpecHash_DoesNotCoverTheName(t *testing.T) {
	base := renderOne(t, newRenderDeployment(), newRenderInstanceType())
	stored := base.Annotations[modelDeploymentPodSpecHashAnnotation]

	testCases := []struct {
		name   string
		mutate func(*core.Pod)
		moves  bool
	}{
		{
			// The name an earlier scheme would have given this replica. Neither the rendered prefix
			// nor this assigned name is part of the fingerprint's subject -- which is what lets a
			// replica named before the generated naming exist beside ones named after it without
			// reading as stale.
			name: "a name assigned by an earlier scheme leaves the fingerprint where it was",
			mutate: func(pod *core.Pod) {
				pod.Name = "qwen-server-3"
			},
		},
		{
			name: "a spec change moves the fingerprint",
			mutate: func(pod *core.Pod) {
				pod.Spec.Containers[0].Image = "vllm/vllm-openai:v0.26.0"
			},
			moves: true,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			pod := base.DeepCopy()
			tc.mutate(pod)

			if tc.moves {
				assert.NotEqual(t, stored, modelDeploymentPodSpecHash(pod))
				return
			}
			assert.Equal(t, stored, modelDeploymentPodSpecHash(pod))
		})
	}
}

// TestModelDeploymentPodSpecHash_MovesWithEveryRenderedInput walks the inputs a rollout must react
// to and asserts each one moves the fingerprint. A change that did not would leave replicas running
// a spec nobody asked for, with the deployment reporting itself Ready.
func TestModelDeploymentPodSpecHash_MovesWithEveryRenderedInput(t *testing.T) {
	base := renderOne(t, newRenderDeployment(), newRenderInstanceType())
	baseHash := base.Annotations[modelDeploymentPodSpecHashAnnotation]

	testCases := []struct {
		name    string
		mutate  func(*workercore.ModelDeployment)
		instype func(*worker.InstanceType)
		input   func(*ModelDeploymentRenderInput)
	}{
		{
			name:   "image",
			mutate: func(md *workercore.ModelDeployment) { md.Spec.Roles[0].Image = "vllm/vllm-openai:v0.26.0" },
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
			// BOTH SIDES MOVE, because that is what naming another pool actually does: the
			// reconciler reads the InstanceType by the name the role carries
			// (getModelDeploymentInstanceType), so a different name is a different object and a
			// different published entrance. Mutating the name alone would render the SAME entrance
			// and assert nothing -- the entrance is no longer derived from the name.
			name:    "instance type — the entrance label moves with it",
			mutate:  func(md *workercore.ModelDeployment) { md.Spec.Roles[0].InstanceType = "h20-4x" },
			instype: func(it *worker.InstanceType) { it.Name, it.Status.Entrance = "h20-4x", "queue-for-h20-4x" },
		},
		{
			// The entrance alone, with the role's instanceType untouched: a pool whose LocalQueue
			// was renamed under a deployment that never changed. The replicas must be recreated
			// carrying the new one, or they stay routed at a queue that is gone.
			name:    "the published entrance, with the instance type name unchanged",
			instype: func(it *worker.InstanceType) { it.Status.Entrance = "queue-renamed" },
		},
		{
			name:    "unit resources — the derived host request moves without the spec moving",
			instype: func(it *worker.InstanceType) { it.Spec.UnitResources.CPU = "32" },
		},
		{
			name:  "the resolved connector arguments",
			input: func(in *ModelDeploymentRenderInput) { in.Connector.Args = []string{`--kv-transfer-config={}`} },
		},
		{
			name:  "the runtime class",
			input: func(in *ModelDeploymentRenderInput) { in.RuntimeClassName = "nvidia" },
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			md := newRenderDeployment()
			if tc.mutate != nil {
				tc.mutate(md)
			}
			it := newRenderInstanceType()
			if tc.instype != nil {
				tc.instype(it)
			}

			in := ModelDeploymentRenderInput{
				Deployment:   md,
				Role:         &md.Spec.Roles[0],
				InstanceType: it,
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
			Deployment:   md,
			Role:         &md.Spec.Roles[0],
			InstanceType: newRenderInstanceType(),
			Connector:    conn,
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
			Deployment:   md,
			Role:         &md.Spec.Roles[0],
			InstanceType: newRenderInstanceType(),
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
				Deployment:   md,
				Role:         &md.Spec.Roles[0],
				InstanceType: newRenderInstanceType(),
			})
			require.NoError(t, err)

			assert.Nil(t, pod.Spec.Containers[0].StartupProbe,
				"%s: an HTTP gate would pass while the endpoint forwards %s", proto, proto)
			assert.Nil(t, pod.Spec.Containers[0].ReadinessProbe, "%s", proto)
			assert.Nil(t, pod.Spec.Containers[0].LivenessProbe,
				"%s: and a liveness gate on an address nothing answers would restart it forever", proto)
		}
	})

	t.Run("a role configuring its own listening endpoint is not gated", func(t *testing.T) {
		// TWO WAYS TO DO IT, and both strand a replica that is serving. --host and --port move WHERE
		// the engine listens, and the operator then leaves them alone, so the engine moves without
		// the Service following. Demanding a CLIENT certificate is the other: the kubelet has none
		// to present, so the gate would fail against a listener serving every real client correctly.
		//
		// THAT SECOND ONE NEEDS TLS ACTUALLY TURNED ON to mean anything, which is why the case below
		// carries a certificate. --ssl-cert-reqs by itself is covered as a GATED case further down.
		for _, extraArgs := range [][]string{
			{"--port", "9100"},
			{"--port=9100"},
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
			{"--ssl-certfile", "/etc/tls/tls.crt", "--port", "9100"},
		} {
			md := newRenderDeployment(func(md *workercore.ModelDeployment) {
				md.Spec.Roles[0].ExtraArgs = extraArgs
			})

			pod, err := renderModelDeploymentPod(context.Background(), ModelDeploymentRenderInput{
				Deployment:   md,
				Role:         &md.Spec.Roles[0],
				InstanceType: newRenderInstanceType(),
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
				Deployment:   md,
				Role:         &md.Spec.Roles[0],
				InstanceType: newRenderInstanceType(),
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
				Deployment:   md,
				Role:         &md.Spec.Roles[0],
				InstanceType: newRenderInstanceType(),
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
			Deployment:   md,
			Role:         &md.Spec.Roles[0],
			InstanceType: newRenderInstanceType(),
		})
		require.NoError(t, err)

		c := pod.Spec.Containers[0]
		require.NotNil(t, c.LivenessProbe)
		require.NotNil(t, c.ReadinessProbe)
		require.NotNil(t, c.StartupProbe)

		assert.Equal(t, modelDeploymentProbePath, c.LivenessProbe.HTTPGet.Path,
			"the liveness gate reads the same route, which is safe only because the startup gate "+
				"suppresses it for the whole load")
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
			Deployment:   md,
			Role:         &md.Spec.Roles[0],
			InstanceType: newRenderInstanceType(),
			Connector:    ModelDeploymentConnectorRender{Args: []string{"--port", "9100"}},
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
			Deployment:   md,
			Role:         &md.Spec.Roles[0],
			InstanceType: newRenderInstanceType(),
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
				Deployment:   md,
				Role:         &md.Spec.Roles[0],
				InstanceType: newRenderInstanceType(),
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

			for name, p := range map[string]*core.Probe{
				"startup": c.StartupProbe, "readiness": c.ReadinessProbe, "liveness": c.LivenessProbe,
			} {
				require.NotNil(t, p.HTTPGet, "%s gate must read the engine's route, not accept a socket", name)
				assert.Equal(t, modelDeploymentProbePath, p.HTTPGet.Path, "%s gate path", name)
				assert.Equal(t, tc.wantPort, p.HTTPGet.Port.IntVal, "%s gate port", name)
			}

			// THE GATE AND THE SERVICE MUST NAME ONE PORT, and the comparison is against the
			// rendered Service rather than against the helper the render used -- comparing a value
			// with the function that produced it would hold however either side drifted.
			svc := renderModelDeploymentService(md)
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
// table has been RE-BASELINED five times since, each time for a rendering change that was
// intended: once for the per-replica groups, where the stamp began naming a (role, ordinal) group,
// declaring a total of one and writing the ordinal label; once when the ordinal label's key took the
// modeldeployment prefix the role-kind label and the spec-hash annotation already carry, so that one
// reader looking for this deployment's own keys finds all of them under one prefix; once when
// the member index became unconditional on every member, so that one equality term in a discovery
// selector names the Pods that answer the API at every role size; once when each managed Pod
// began declaring its metrics listener scheme; and once when every Pod whose command line the
// operator builds began carrying the drain hook and the termination grace it is budgeted against,
// which left the take-over case's digest where it was. Every digest here moved by intent
// rather than by drift. Each case renders ORDINAL ZERO -- the composite's zero value -- which is the
// one ordinal a digest can name without the table growing a dimension. To re-baseline after an
// intended rendering change: empty the table, run this case, and pin the digests the failures
// print.
func TestRenderModelDeploymentPod_SerializedOutputIsPinnedToThePreSplitRender(t *testing.T) {
	pinned := map[string]string{
		"a sole server role": "bbd82155b1d685e6906f885fdbebf266890117cafa9d1f58e089e6d678c27ec4",
		"a sole server role with a synthesized cache connector":                              "42565b50fb7e640fa5d6a1a23c7a6d1920fa98370a3ca2d2080c2bb7e7a0a7d8",
		"a take-over role carrying a connector it must be given no part of":                  "1984284b5fbc1e7245e71bb5ea8c3ed1daccef316d724e398952fa637c4c67cd",
		"a direct decoder with a native routing sidecar":                                     "f7c36762d7ae4034f5a3642e0670d75a5fde2d05a151126ee82fa121fc11de2f",
		"a direct decoder with a classic routing sidecar":                                    "4c6b12bd46308005d3d1fccad37d7523525c2993500a7a40d1822c2ee84b6c83",
		"a role naming no image, synthesized from the observed hardware":                     "c6fbaa790a7663c4d9f601ba5d2183e8cb049b410e168ba05acc62b633b2d5f8",
		"a TLS-listening role with declared ports, privileges, a runtime class and a volume": "a576d65292f1abd652bb02aa5670707da9d6acdc122d5bc990de42e00387cd1c",
		"the prefill role of a two-role deployment":                                          "108810fcd9c5b7b417bf9fdb3b91af0d8f6031458c45eeffa22083b105cdc5aa",
		"the decode role of a two-role deployment":                                           "6d5e95c247bf376093f8d2263fc2469983bae32ad30c2f23422a3971f8454dc2",
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
			Deployment:   md,
			Role:         &md.Spec.Roles[roleIndex],
			InstanceType: newRenderInstanceType(),
		}
	}

	synthesizedConnector := func(t *testing.T) ModelDeploymentConnectorRender {
		t.Helper()

		conn, err := SynthesizeModelDeploymentConnector(
			connectorInput(workercore.ModelDeploymentEngineVLLM, nodefeature.ManufacturerNVIDIA))
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
			name: "a role naming no image, synthesized from the observed hardware",
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
			sum := sha256.Sum256(encoded)
			digest := hex.EncodeToString(sum[:])

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
				Name:         "decode",
				Replicas:     2,
				InstanceType: "h20-8x",
				Kind:         workercore.ModelDeploymentRoleKindDecode,
				Image:        "vllm/vllm-openai:v0.25.1",
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
		Deployment:   md,
		Role:         role,
		InstanceType: newRenderInstanceType(),
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
			Deployment: md, Role: role, InstanceType: newRenderInstanceType(),
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
	md := newRenderDeployment(func(md *workercore.ModelDeployment) {
		md.Spec.Roles[0].ReplicaSize = 4
	})
	role := &md.Spec.Roles[0]

	specOf := func(member int) string {
		t.Helper()

		pod, err := renderModelDeploymentPodTemplate(context.Background(), ModelDeploymentRenderInput{
			Deployment: md, Role: role, InstanceType: newRenderInstanceType(),
		})
		require.NoError(t, err)
		stampModelDeploymentPod(pod, md, role, 0, member)

		// The two fields the stamp is ALLOWED to vary per member. Blanking them is what makes the
		// comparison discriminating rather than vacuous: without it the test would be asserting
		// that two different Pods are different.
		pod.Spec.Hostname = ""

		encoded, err := json.Marshal(pod.Spec)
		require.NoError(t, err)

		return string(encoded)
	}

	first := specOf(0)
	for member := 1; member < 4; member++ {
		assert.Equal(t, first, specOf(member),
			"member %d's PodSpec differs from the leader's beyond its hostname: "+
				"a per-member value has leaked into the render", member)
	}
}

// renderStampedMember is one member of one replica, rendered and stamped the way the converger does.
func renderStampedMember(
	t *testing.T, md *workercore.ModelDeployment, ordinal, member int,
) *core.Pod {
	t.Helper()

	pod, err := renderModelDeploymentPodTemplate(context.Background(), ModelDeploymentRenderInput{
		Deployment: md, Role: &md.Spec.Roles[0], InstanceType: newRenderInstanceType(),
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
					Deployment: md, Role: &md.Spec.Roles[0], InstanceType: newRenderInstanceType(),
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
					Deployment: md, Role: &md.Spec.Roles[0], InstanceType: newRenderInstanceType(),
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
					Deployment: md, Role: &md.Spec.Roles[0], InstanceType: newRenderInstanceType(),
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
		Deployment: md, Role: &md.Spec.Roles[0], InstanceType: newRenderInstanceType(),
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
			Name: workercore.ModelDeploymentEngineSGLang, Version: "0.5.18",
		}
		md.Spec.Router = &workercore.ModelDeploymentRouter{Name: workercore.ModelDeploymentRouterLLMD}
		md.Spec.Roles = []workercore.ModelDeploymentRole{
			{
				Name: "prefill", Kind: workercore.ModelDeploymentRoleKindPrefill,
				Replicas: 1, InstanceType: "h20-8x", Image: "lmsysorg/sglang:v0.5.18",
			},
			{
				Name: "decode", Kind: workercore.ModelDeploymentRoleKindDecode,
				Replicas: 1, InstanceType: "h20-8x", Image: "lmsysorg/sglang:v0.5.18",
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
					Name: workercore.ModelDeploymentEngineVLLM, Version: "0.29.0",
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
					Deployment: tc.md, Role: role, InstanceType: newRenderInstanceType(),
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
			assert.NotEqual(t, offHash, onHash, "flipping the setting must roll the target replica")
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
		Deployment: md, Role: &md.Spec.Roles[0], InstanceType: newRenderInstanceType(),
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
		r := &ModelDeploymentReconciler{Client: newModelDeploymentClient(md, newRenderInstanceType())}
		desired, err := r.renderModelDeploymentPods(context.Background(), md, nil, nil)
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
