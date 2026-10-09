package worker

import (
	"context"
	"slices"
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	core "k8s.io/api/core/v1"
	ctrlcli "sigs.k8s.io/controller-runtime/pkg/client"
	kueuectrlconst "sigs.k8s.io/kueue/pkg/controller/constants"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	"gpustack.ai/gpustack/pkg/systemmeta"
	"gpustack.ai/gpustack/pkg/worker/kvcache/inject"
)

func getModelDeploymentService(t *testing.T, cli ctrlcli.Client) *core.Service {
	t.Helper()

	svc := new(core.Service)
	require.NoError(t, cli.Get(context.Background(),
		ctrlcli.ObjectKey{Namespace: "team-a", Name: "qwen"}, svc))

	return svc
}

// serviceNames lists every Service in the namespace, sorted, so a case states the whole set it
// expects rather than probing for the names it happens to think of. It does NOT filter by owner:
// a Service the deployment failed to clean up is exactly what several of these cases are about.
func serviceNames(t *testing.T, cli ctrlcli.Client) []string {
	t.Helper()

	svcList := new(core.ServiceList)
	require.NoError(t, cli.List(context.Background(), svcList, ctrlcli.InNamespace("team-a")))

	names := make([]string, 0, len(svcList.Items))
	for i := range svcList.Items {
		names = append(names, svcList.Items[i].Name)
	}
	slices.Sort(names)

	return names
}

// TestRenderModelDeploymentService_IsOneClusterIPForEveryReplica states the shape, and states it
// against the alternative this repository already implements. instance.go renders one NodePort
// Service PER POD, which is right for a single addressable box; N interchangeable replicas behind
// one address is a different shape, and per-Pod NodePort would publish N addresses with no load
// balancing and burn one node port per replica.
func TestRenderModelDeploymentService_IsOneClusterIPForEveryReplica(t *testing.T) {
	md := newRenderDeployment(func(md *workercore.ModelDeployment) { md.Spec.Roles[0].Replicas = 4 })
	cli := newModelDeploymentClient(md, newRenderInstanceType())

	_, err := reconcileModelDeployment(t, cli)
	require.NoError(t, err)
	require.Len(t, replicaNames(t, cli), 4)

	// TWO Services for ONE role, and the count is per ROLE rather than per replica: the
	// deployment-wide address and that role's own. Four replicas still produce no fourth Service,
	// which is the property this case is about.
	assert.Equal(t, []string{"qwen", "qwen-server"}, serviceNames(t, cli),
		"four replicas are fronted per ROLE, not one Service each")

	svc := getModelDeploymentService(t, cli)
	assert.Equal(t, core.ServiceTypeClusterIP, svc.Spec.Type)
	assert.Equal(t, "qwen", svc.Name)
	assert.True(t, systemmeta.MatchResource(svc, ModelDeploymentResourceType))
	assert.True(t, modelDeploymentOwns(svc, getModelDeployment(t, cli)))
}

// TestModelDeploymentService_OnePerRoleBesideTheDeploymentWide covers which Services exist and
// what each of them fronts.
//
// WHY A P/D DEPLOYMENT NEEDS THIS AT ALL: the two roles are deliberately different configurations,
// so an address resolving to "whichever replica" is an address for neither of them — a decoder has
// to be reachable AS a decoder. Nothing in this spec calls these addresses, because no router
// exists yet; they are rendered now so the pairing spec finds them rather than having to change
// this object's shape to get them.
func TestModelDeploymentService_OnePerRoleBesideTheDeploymentWide(t *testing.T) {
	cli := newModelDeploymentClient(twoRoleDeployment(), newRenderInstanceType())

	_, err := reconcileModelDeployment(t, cli)
	require.NoError(t, err)

	assert.Equal(t, []string{"qwen", "qwen-decode", "qwen-prefill"}, serviceNames(t, cli))

	svcList := new(core.ServiceList)
	require.NoError(t, cli.List(context.Background(), svcList, ctrlcli.InNamespace("team-a")))
	byName := map[string]*core.Service{}
	for i := range svcList.Items {
		byName[svcList.Items[i].Name] = &svcList.Items[i]
	}

	md := getModelDeployment(t, cli)
	for _, name := range []string{"qwen-prefill", "qwen-decode"} {
		svc := byName[name]
		assert.Equal(t, core.ServiceTypeClusterIP, svc.Spec.Type, "%s", name)
		assert.True(t, modelDeploymentOwns(svc, md), "%s must be owned by the deployment", name)
	}

	// The selector names the DEPLOYMENT as well as the role. Without that, two deployments in one
	// namespace each running a role called "decode" would share endpoints -- and the symptom is a
	// request served by another team.s model, not an error. The eligibility term rides beside
	// them: an ordinary Service answers only for members the reconciler has marked eligible.
	assert.Equal(t, map[string]string{
		modelDeploymentLabelKeyName:      modelDeploymentLabelValueName,
		modelDeploymentLabelKeyInstance:  "qwen",
		modelDeploymentLabelKeyComponent: "decode",
	}, byName["qwen-decode"].Spec.Selector)

	// The deployment-wide one still fronts the FIRST role, unchanged. It is not a router and must
	// not become one: a Service selecting every role would round-robin a request onto a process
	// configured as a producer and one configured as a consumer.
	assert.Equal(t, byName["qwen-prefill"].Spec.Selector, byName["qwen"].Spec.Selector)
}

func TestRenderModelDeploymentService_KVEventPorts(t *testing.T) {
	md := routedModelDeployment()
	services := renderModelDeploymentServices(md, nil, nil)
	require.Len(t, services, 3)

	portsByName := func(service *core.Service) map[string]int32 {
		ports := make(map[string]int32, len(service.Spec.Ports))
		for _, port := range service.Spec.Ports {
			ports[port.Name] = port.Port
		}
		return ports
	}

	assert.Equal(t, map[string]int32{"http": 8000, "kv-events": 5557, "kv-replay": 5558},
		portsByName(services[1]), "the prefill role Service makes both published addresses dialable")
	assert.Equal(t, map[string]int32{"http": 8000}, portsByName(services[2]),
		"a decode-only role is not required to publish cache events")
}

// TestModelDeploymentService_RemovingARoleRemovesItsService covers what an owner reference does NOT
// collect.
//
// The deployment still exists, so nothing about the reference is stale and Kubernetes leaves the
// Service behind. A leftover keeps RESOLVING, to a selector no Pod matches, so a caller wired to a
// decoder that was removed gets connection refused rather than NXDOMAIN and reads it as the decoder
// being down.
func TestModelDeploymentService_RemovingARoleRemovesItsService(t *testing.T) {
	cli := newModelDeploymentClient(twoRoleDeployment(), newRenderInstanceType())

	_, err := reconcileModelDeployment(t, cli)
	require.NoError(t, err)
	require.Equal(t, []string{"qwen", "qwen-decode", "qwen-prefill"}, serviceNames(t, cli))

	shrunk := getModelDeployment(t, cli)
	shrunk.Spec.Roles = shrunk.Spec.Roles[:1]
	require.NoError(t, cli.Update(context.Background(), shrunk))

	_, err = reconcileModelDeployment(t, cli)
	require.NoError(t, err)

	assert.Equal(t, []string{"qwen", "qwen-prefill"}, serviceNames(t, cli),
		"the removed role's Service goes with it")
}

// TestModelDeploymentService_AReplicaServiceIsCreatedAndReclaimedWithItsReplica runs the whole
// convergence rather than the renderer, because the question here is about the prune path: a
// headless Service is derived from an ordinal. Scaling down keeps its peer records until the
// ordinal's last member leaves, then reclaims the Service.
//
// THE SCALE-UP HALF IS THE CONTROL. Without it "the Services went away" is satisfied by a
// convergence that never created them, which is the failure this case would otherwise report as a
// pass.
func TestModelDeploymentService_AReplicaServiceIsCreatedAndReclaimedWithItsReplica(t *testing.T) {
	md := newRenderDeployment(func(md *workercore.ModelDeployment) {
		md.Spec.Roles[0].ReplicaSize = 2
		md.Spec.Roles[0].Replicas = 3
	})
	cli := newModelDeploymentClient(md, newRenderInstanceType())

	_, err := reconcileModelDeployment(t, cli)
	require.NoError(t, err)
	require.Equal(t, []string{
		"qwen", "qwen-server", "qwen-server-r0", "qwen-server-r1", "qwen-server-r2",
	}, serviceNames(t, cli), "each replica is published behind one headless Service of its own")

	scaled := getModelDeployment(t, cli)
	scaled.Spec.Roles[0].Replicas = 1
	require.NoError(t, cli.Update(context.Background(), scaled))

	_, err = reconcileModelDeployment(t, cli)
	require.NoError(t, err)

	require.Len(t, replicaPods(t, cli), 6, "the old members have not left")
	require.Equal(t, []string{
		"qwen", "qwen-server", "qwen-server-r0", "qwen-server-r1", "qwen-server-r2",
	}, serviceNames(t, cli), "old members keep their peer records")
	for _, pod := range replicaPods(t, cli) {
		if pod.Labels[modelDeploymentReplicaOrdinalLabel] != "0" {
			releaseLifecyclePod(t, cli, &pod)
		}
	}
	require.Len(t, replicaPods(t, cli), 2, "only the desired replica remains")
	_, err = reconcileModelDeployment(t, cli)
	require.NoError(t, err)

	assert.Equal(t, []string{"qwen", "qwen-server", "qwen-server-r0"}, serviceNames(t, cli),
		"the departed replicas take their peer records with them")
}

// TestModelDeploymentService_SurvivesAScale pins the one interaction between the Service and a
// replicas change.
//
// A replicas change now trims or grows the affected role's ordinals and touches nothing else, but
// the Service's obligation is unchanged: a scale must move its endpoints and leave the object
// alone. A Service rebuilt alongside the replicas would drop its allocated ClusterIP, so every
// client that resolved the name would be talking to an address nothing answers on -- for a change
// that was only ever about how many replicas there are.
func TestModelDeploymentService_SurvivesAScale(t *testing.T) {
	cli := newModelDeploymentClient(twoRoleDeployment(), newRenderInstanceType())

	_, err := reconcileModelDeployment(t, cli)
	require.NoError(t, err)
	before := getModelDeploymentService(t, cli)

	grown := getModelDeployment(t, cli)
	grown.Spec.Roles[0].Replicas = 3
	require.NoError(t, cli.Update(context.Background(), grown))

	// The scaled role gains its third ordinal beside the two that stay, and the sibling role's Pods
	// are nobody's cost -- a deployment is never left without replicas at all.
	_, err = reconcileModelDeployment(t, cli)
	require.NoError(t, err)
	surviving := 0
	for _, pod := range replicaPods(t, cli) {
		if modelDeploymentPodRole(&pod) == "decode" {
			surviving++
		}
	}
	require.Equal(t, 2, surviving, "the sibling role's Pods stay exactly where they were")

	assert.Equal(t, []string{"qwen", "qwen-decode", "qwen-prefill"}, serviceNames(t, cli),
		"a deployment mid-scale still has its addresses")
	after := getModelDeploymentService(t, cli)
	assert.Equal(t, before.UID, after.UID)
	assert.Equal(t, before.ResourceVersion, after.ResourceVersion)
}

// TestRenderModelDeploymentService_SelectsExactlyTheRolesPods is the property that decides whether
// traffic reaches anything: the selector has to match what the renderer actually stamps on a
// replica, and must not carry the entrance label, which a spec update can move.
func TestRenderModelDeploymentService_SelectsExactlyTheRolesPods(t *testing.T) {
	md := newRenderDeployment()
	svc := renderModelDeploymentService(md, nil)
	pod := renderOne(t, md, newRenderInstanceType())
	// The eligibility term is written at runtime by the reconciler, so a member the Service may
	// select is one that carries it; the rendered template never does.
	pod.Labels[modelDeploymentLabelKeyEndpointEligible] = modelDeploymentEndpointEligibleValue

	require.NotEmpty(t, svc.Spec.Selector)
	for k, v := range svc.Spec.Selector {
		assert.Equal(t, v, pod.Labels[k], "the replica must carry selector label %s", k)
	}
	assert.NotContains(t, svc.Spec.Selector, kueuectrlconst.QueueLabel,
		"a selector that followed the InstanceType would orphan every replica already running")
}

// TestRenderModelDeploymentServices_AboveOneMemberAddsAHeadlessServicePerReplica covers what a role
// of multi-Member replicas is published as, and each assertion is aimed at a different way it could
// be wrong.
func TestRenderModelDeploymentServices_AboveOneMemberAddsAHeadlessServicePerReplica(t *testing.T) {
	md := newRenderDeployment(func(md *workercore.ModelDeployment) {
		md.Spec.Roles[0].ReplicaSize = 2
		md.Spec.Roles[0].Replicas = 3
	})

	svcs := renderModelDeploymentServices(md, nil, nil)
	names := make([]string, 0, len(svcs))
	byName := make(map[string]*core.Service, len(svcs))
	for _, svc := range svcs {
		names = append(names, svc.Name)
		byName[svc.Name] = svc
	}

	// ONE PER REPLICA, not one per role and not one per member.
	assert.Equal(t, []string{
		"qwen", "qwen-server", "qwen-server-r0", "qwen-server-r1", "qwen-server-r2",
	}, names)

	replica := byName["qwen-server-r0"]
	require.NotNil(t, replica)
	assert.Equal(t, core.ClusterIPNone, replica.Spec.ClusterIP,
		"a ClusterIP would load-balance between members, which is the one thing a rank must not get")
	assert.True(t, replica.Spec.PublishNotReadyAddresses,
		"a member cannot become ready until it can reach the peers this record publishes")

	// IT SELECTS ITS OWN REPLICA. Without the ordinal term every replica's Service fronts every
	// member of the role, and a collective forms across instance boundaries -- which serves wrong
	// answers rather than failing.
	assert.Equal(t, "0", replica.Spec.Selector[modelDeploymentReplicaOrdinalLabel])
	assert.Equal(t, "1", byName["qwen-server-r1"].Spec.Selector[modelDeploymentReplicaOrdinalLabel])
	assert.NotContains(t, replica.Spec.Selector, modelDeploymentMemberIndexLabel,
		"a replica's Service publishes ALL its members; narrowing to the leader would hide the peers")

	// AND THE ROLE'S OWN SERVICE FRONTS ONLY LEADERS, because the other members serve no API.
	assert.Equal(t, "0", byName["qwen-server"].Spec.Selector[modelDeploymentMemberIndexLabel])
	assert.Equal(t, "0", byName["qwen"].Spec.Selector[modelDeploymentMemberIndexLabel])
}

// TestRenderModelDeploymentServices_SelectEligibleEndpoints pins the eligibility term in every
// ordinary Service's selector: an ordinary Service selects only endpoints the reconciler has
// marked eligible, whatever the role's shape, while a replica's headless Service stays
// eligibility-blind and keeps publishing every member unready-addresses and all.
func TestRenderModelDeploymentServices_SelectEligibleEndpoints(t *testing.T) {
	testCases := []struct {
		name string
		md   *workercore.ModelDeployment
	}{
		{
			name: "at size one",
			md:   newRenderDeployment(func(md *workercore.ModelDeployment) { md.Spec.Roles[0].Replicas = 3 }),
		},
		{
			name: "above size one",
			md: newRenderDeployment(func(md *workercore.ModelDeployment) {
				md.Spec.Roles[0].ReplicaSize = 2
				md.Spec.Roles[0].Replicas = 3
			}),
		},
		{
			name: "a p/d pair",
			md:   twoRoleDeployment(),
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			// The ordinary Services narrow only once a real pass has qualified the group: the
			// activatable fixtures are qualified here through the real probe and recorded through
			// the real status observation. A p/d pair can never qualify, so it renders term-less.
			if modelDeploymentGroupForwardActivatable(tc.md, &tc.md.Spec.Roles[0]) {
				pods := []core.Pod{healthPod(tc.md.Spec.Roles[0].Name, 0, 0, healthBool(true), "uid-a")}
				qs := qualifyModelDeploymentInstances(context.Background(), tc.md, pods,
					modelDeploymentPendingReplacement{}, boundProbeFetch)
				require.NotEmpty(t, qs)
				observeModelDeploymentEndpointEligibility(tc.md, qs)
			}
			svcs := renderModelDeploymentServices(tc.md, nil, nil)
			require.NotEmpty(t, svcs)

			ordinary := 0
			for _, svc := range svcs {
				if svc.Spec.ClusterIP == core.ClusterIPNone {
					assert.NotContains(t, svc.Spec.Selector, modelDeploymentLabelKeyEndpointEligible,
						"%s publishes every member for its replica; eligibility narrows only the ordinary Services", svc.Name)
					assert.True(t, svc.Spec.PublishNotReadyAddresses,
						"%s must keep publishing unready addresses", svc.Name)

					continue
				}
				ordinary++
				assert.Equal(t, modelDeploymentEndpointEligibleValue,
					svc.Spec.Selector[modelDeploymentLabelKeyEndpointEligible],
					"%s selects only endpoints the reconciler marked eligible", svc.Name)
			}
			assert.NotZero(t, ordinary, "the deployment always has at least one ordinary Service")
		})
	}
}

// TestRenderModelDeploymentServices_AtSizeOneIsUnchanged is the control for the case above: the
// shape that existed before multi-Member replicas must be untouched, asserted by comparison rather
// than by reading the new code's intent.
func TestRenderModelDeploymentServices_AtSizeOneIsUnchanged(t *testing.T) {
	md := newRenderDeployment(func(md *workercore.ModelDeployment) { md.Spec.Roles[0].Replicas = 3 })

	svcs := renderModelDeploymentServices(md, nil, nil)
	names := make([]string, 0, len(svcs))
	for _, svc := range svcs {
		names = append(names, svc.Name)
	}
	assert.Equal(t, []string{"qwen", "qwen-server"}, names,
		"a role of single-Member replicas gets no headless Service: there is nobody to address")

	for _, svc := range svcs {
		assert.NotContains(t, svc.Spec.Selector, modelDeploymentMemberIndexLabel,
			"%s: single-Member replicas carry no member-index label, so a term on it selects nothing",
			svc.Name)
	}

	// The endpoints still reach a rendered replica, which is what makes the absences above mean
	// "unchanged" rather than "empty".
	pod := renderOne(t, md, newRenderInstanceType())
	// A member a Service selects carries the eligibility label the reconciler writes at runtime;
	// the template it renders with does not, which is why the term above is absent from selectors
	// on the headless replica Services that reach every member regardless.
	pod.Labels[modelDeploymentLabelKeyEndpointEligible] = modelDeploymentEndpointEligibleValue
	for k, v := range svcs[0].Spec.Selector {
		assert.Equal(t, v, pod.Labels[k], "the replica must carry selector label %s", k)
	}
}

// TestRenderModelDeploymentService_Port covers both readings of the port rule, including that the
// Service and the container behind it can never name different ports — they read one render.
func TestRenderModelDeploymentService_Port(t *testing.T) {
	testCases := []struct {
		name     string
		ports    []workercore.ModelDeploymentPort
		wantPort int32
		wantName string
	}{
		{name: "no port declared falls back to the engines' default", wantPort: 8000, wantName: "http"},
		{
			name:     "a declared port is used as it stands",
			ports:    []workercore.ModelDeploymentPort{{Port: 9000, Protocol: core.ProtocolTCP}},
			wantPort: 9000,
		},
		{
			name: "the FIRST declared port is the one the deployment is reached on",
			ports: []workercore.ModelDeploymentPort{
				{Port: 9000, Protocol: core.ProtocolTCP},
				{Port: 9001, Protocol: core.ProtocolTCP},
			},
			wantPort: 9000,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			md := newRenderDeployment(func(md *workercore.ModelDeployment) {
				md.Spec.Roles[0].Ports = tc.ports
			})

			svc := renderModelDeploymentService(md, nil)
			require.Len(t, svc.Spec.Ports, 1)
			assert.Equal(t, tc.wantPort, svc.Spec.Ports[0].Port)
			assert.Equal(t, tc.wantPort, svc.Spec.Ports[0].TargetPort.IntVal)
			if tc.wantName != "" {
				assert.Equal(t, tc.wantName, svc.Spec.Ports[0].Name)
			}

			pod := renderOne(t, md, newRenderInstanceType())
			assert.Equal(t, svc.Spec.Ports[0].TargetPort.IntVal,
				pod.Spec.Containers[0].Ports[0].ContainerPort,
				"the Service must target the port the container actually opens")
		})
	}
}

// TestRenderModelDeploymentService_TargetFollowsTheEnginesPort covers a role that names its
// engine's port in its own arguments and declares no ports: the engine listens where the role said,
// so the container port, the Service's targetPort and the probes follow it there, while the
// Service's own port and the published endpoint stay on the declared or default port a caller
// already dials.
func TestRenderModelDeploymentService_TargetFollowsTheEnginesPort(t *testing.T) {
	testCases := []struct {
		name      string
		engine    string
		extraArgs []string
		command   []string
		ports     []workercore.ModelDeploymentPort
		// directDecode renders the role as an llm-d decoder fronted by its routing proxy.
		directDecode bool
		// wantPort is the Service's own port and the published endpoint's.
		wantPort int32
		// wantTargetPort is where the Service sends traffic, and where the probes grade.
		wantTargetPort int32
		// wantEnginePort is the main container's first port, the one its engine opens.
		wantEnginePort int32
		// wantProbes is false for a role the operator renders no probes for.
		wantProbes bool
	}{
		{
			// THE POSITIVE BASELINE: without it, an implementation that moved every Service to
			// 9100 would pass the cases below.
			name:     "no_port_flag_leaves_the_default",
			wantPort: 8000, wantTargetPort: 8000, wantEnginePort: 8000, wantProbes: true,
		},
		{
			name:      "a_flag_sharing_a_stem_moves_nothing",
			extraArgs: []string{"--pooler-config", "{}"},
			wantPort:  8000, wantTargetPort: 8000, wantEnginePort: 8000, wantProbes: true,
		},
		{
			name:      "an_explicit_port_is_followed",
			extraArgs: []string{"--port", "9100"},
			wantPort:  8000, wantTargetPort: 9100, wantEnginePort: 9100, wantProbes: true,
		},
		{
			name:      "an_inline_port_is_followed",
			extraArgs: []string{"--port=9100"},
			wantPort:  8000, wantTargetPort: 9100, wantEnginePort: 9100, wantProbes: true,
		},
		{
			name:      "an_abbreviated_port_is_followed",
			extraArgs: []string{"--por", "9100"},
			wantPort:  8000, wantTargetPort: 9100, wantEnginePort: 9100, wantProbes: true,
		},
		{
			name:      "an_abbreviated_inline_port_is_followed",
			extraArgs: []string{"--por=9100"},
			wantPort:  8000, wantTargetPort: 9100, wantEnginePort: 9100, wantProbes: true,
		},
		{
			name:      "the_last_spelling_wins",
			extraArgs: []string{"--port", "9000", "--por", "9100"},
			wantPort:  8000, wantTargetPort: 9100, wantEnginePort: 9100, wantProbes: true,
		},
		{
			// "--po" is ambiguous on vLLM, which also registers --pooler-config; SGLang reads it.
			name: "sglang_reads_its_own_abbreviation", engine: workercore.ModelDeploymentEngineSGLang,
			extraArgs: []string{"--po", "9100"},
			wantPort:  8000, wantTargetPort: 9100, wantEnginePort: 9100, wantProbes: true,
		},
		{
			name:      "a_declared_port_that_agrees_is_gated_on_it",
			ports:     []workercore.ModelDeploymentPort{{Port: 9100, Protocol: core.ProtocolTCP}},
			extraArgs: []string{"--port", "9100"},
			wantPort:  9100, wantTargetPort: 9100, wantEnginePort: 9100, wantProbes: true,
		},
		{
			// A take-over role gets no probes, but its Service still has to reach the engine.
			name:     "a_take_over_command_is_followed",
			command:  []string{"vllm", "serve", "m", "--port", "9100"},
			wantPort: 8000, wantTargetPort: 9100, wantEnginePort: 9100,
		},
		{
			// THE PROXY OWNS THE SERVING PORT, and the role's --port names the engine behind it.
			name:         "a_direct_decoder_keeps_the_proxy_in_front",
			extraArgs:    []string{"--port", "9100"},
			directDecode: true,
			wantPort:     8000, wantTargetPort: 8000, wantEnginePort: 9100, wantProbes: true,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			md := newRenderDeployment(func(md *workercore.ModelDeployment) {
				if tc.engine != "" {
					md.Spec.Engine.Name = tc.engine
				}
				md.Spec.Roles[0].ExtraArgs = tc.extraArgs
				md.Spec.Roles[0].Command = tc.command
				md.Spec.Roles[0].Ports = tc.ports
				if tc.directDecode {
					md.Spec.Router = &workercore.ModelDeploymentRouter{Name: workercore.ModelDeploymentRouterLLMD}
					md.Spec.Roles[0].Kind = workercore.ModelDeploymentRoleKindDecode
				}
			})
			in := ModelDeploymentRenderInput{
				Deployment: md, Role: &md.Spec.Roles[0], InstanceType: newRenderInstanceType(),
				NativeSidecar: true,
			}
			if tc.directDecode {
				in.Connector = ModelDeploymentConnectorRender{
					Args: []string{"--kv-transfer-config", `{}`}, KVTransfer: true, RoutingSidecar: true,
				}
			}
			pod, err := renderModelDeploymentPod(context.Background(), in)
			require.NoError(t, err)

			svcs := renderModelDeploymentServices(md, nil, nil)
			require.Len(t, svcs, 2)
			for _, svc := range svcs {
				require.Len(t, svc.Spec.Ports, 1, svc.Name)
				assert.Equal(t, tc.wantPort, svc.Spec.Ports[0].Port, svc.Name)
				assert.Equal(t, tc.wantTargetPort, svc.Spec.Ports[0].TargetPort.IntVal, svc.Name)
			}
			assert.Equal(t, "http://qwen.team-a.svc:"+strconv.Itoa(int(tc.wantPort)), modelDeploymentEndpoint(md))

			main := pod.Spec.Containers[len(pod.Spec.Containers)-1]
			require.Equal(t, modelDeploymentMainContainerName, main.Name)
			assert.Equal(t, tc.wantEnginePort, main.Ports[0].ContainerPort)
			served := false
			for _, c := range append(slices.Clone(pod.Spec.InitContainers), pod.Spec.Containers...) {
				for _, p := range c.Ports {
					served = served || p.ContainerPort == tc.wantTargetPort
				}
			}
			assert.True(t, served, "some container in the Pod opens the port the Service targets")

			if !tc.wantProbes {
				assert.Nil(t, main.StartupProbe)
				assert.Nil(t, main.ReadinessProbe)
				assert.Nil(t, main.LivenessProbe)

				return
			}
			for _, probe := range []*core.Probe{main.StartupProbe, main.ReadinessProbe, main.LivenessProbe} {
				require.NotNil(t, probe)
				assert.Equal(t, tc.wantTargetPort, probe.HTTPGet.Port.IntVal,
					"the gate grades the address the Service sends traffic to")
			}
		})
	}
}

// TestModelDeploymentEndpoint pins the address a user is handed. It is derived from the Service's
// name and namespace rather than read back off the object, because a ClusterIP is neither what
// callers use nor stable across a recreate.
func TestModelDeploymentEndpoint(t *testing.T) {
	assert.Equal(t, "http://qwen.team-a.svc:8000", modelDeploymentEndpoint(newRenderDeployment()))

	md := newRenderDeployment(func(md *workercore.ModelDeployment) {
		md.Spec.Roles[0].Ports = []workercore.ModelDeploymentPort{{Port: 9000, Protocol: core.ProtocolTCP}}
	})
	assert.Equal(t, "http://qwen.team-a.svc:9000", modelDeploymentEndpoint(md))
}

// TestModelDeploymentEndpoint_Scheme pins the transport the published address claims.
//
// A ROLE SERVING OVER TLS PUBLISHED AS http:// IS AN ADDRESS NO CLIENT CAN USE, and the replica
// behind it is graded Ready by a probe that did speak TLS -- so "ready" and "the endpoint answers"
// would be two facts again, which is what the gates exist to prevent.
func TestModelDeploymentEndpoint_Scheme(t *testing.T) {
	testCases := []struct {
		name      string
		extraArgs []string
		command   []string
		expected  string
	}{
		{
			// THE POSITIVE BASELINE. Without it, an implementation that answered https:// to
			// everything would pass every other case here.
			name:     "a role adding no arguments is published over http",
			expected: "http://qwen.team-a.svc:8000",
		},
		{
			name:      "an argument whose value merely contains ssl does not move the transport",
			extraArgs: []string{"--served-model-name", "ssl-demo"},
			expected:  "http://qwen.team-a.svc:8000",
		},
		{
			// A CERTIFICATE ALONE IS ENOUGH, and reading the criterion as "certificate AND key"
			// would publish http:// for a listener uvicorn really does put on TLS -- it enables it
			// on `ssl_keyfile or ssl_certfile`. Each engine separately logs its own idea of "SSL
			// enabled" from a stricter expression; those lines decide nothing.
			name:      "a certificate alone moves it",
			extraArgs: []string{"--ssl-certfile", "/etc/tls/tls.crt"},
			expected:  "https://qwen.team-a.svc:8000",
		},
		{
			// vLLM rewrites the underscores before it matches the flag, so the address has to as well.
			name:      "a certificate spelled with underscores moves it on vLLM",
			extraArgs: []string{"--ssl_certfile", "/etc/tls/tls.crt"},
			expected:  "https://qwen.team-a.svc:8000",
		},
		{
			name:      "a key alone moves it too, spelled with an equals sign",
			extraArgs: []string{"--ssl-keyfile=/etc/tls/tls.key"},
			expected:  "https://qwen.team-a.svc:8000",
		},
		{
			// THE REST OF THE FAMILY DOES NOT MOVE IT. uvicorn builds no TLS context for a CA
			// bundle, a cipher list, a key password or a client-certificate mode on their own, so
			// the server stays plaintext and an https:// address would be unusable.
			name:      "a CA bundle alone leaves it plaintext",
			extraArgs: []string{"--ssl-ca-certs", "/etc/tls/ca.crt"},
			expected:  "http://qwen.team-a.svc:8000",
		},
		{
			name:      "and so does a client-certificate mode with nothing to apply it to",
			extraArgs: []string{"--ssl-cert-reqs", "2"},
			expected:  "http://qwen.team-a.svc:8000",
		},
		{
			// THE CASE THE PROBE DECLINES AND THE ADDRESS MUST NOT. Once a certificate is present
			// the client-certificate mode bites: the replica is ungradable, because a kubelet probe
			// has no certificate to present -- and it is serving TLS, so an address saying otherwise
			// is wrong for every real client.
			name:      "a client certificate demanded over real TLS is ungradable and still https",
			extraArgs: []string{"--ssl-certfile", "/etc/tls/tls.crt", "--ssl-cert-reqs", "2"},
			expected:  "https://qwen.team-a.svc:8000",
		},
		{
			// Moving WHERE it listens moves the Service's targetPort, not the published address, and
			// leaves the transport as it was.
			name:      "moving the port alone leaves the published address and its transport as they were",
			extraArgs: []string{"--port", "9100"},
			expected:  "http://qwen.team-a.svc:8000",
		},
		{
			name:      "a take-over role is read from the argv it replaced the command with",
			command:   []string{"python", "-m", "vllm.entrypoints.openai.api_server", "--ssl-certfile", "/x"},
			extraArgs: []string{"--ssl-certfile", "/ignored"},
			expected:  "https://qwen.team-a.svc:8000",
		},
		{
			name:     "a take-over role's command line is read by the engine's own spelling rules",
			command:  []string{"vllm", "serve", "m", "--ssl-certf", "/x"},
			expected: "https://qwen.team-a.svc:8000",
		},
		{
			// THE EXTRA ARGUMENTS OF A TAKE-OVER ROLE ARE NOT APPENDED to the command it replaced,
			// so reading them would describe a command line that is not being run.
			name:      "and not from the extra arguments that command line never receives",
			command:   []string{"python", "-m", "vllm.entrypoints.openai.api_server"},
			extraArgs: []string{"--ssl-certfile", "/never-reaches-the-engine"},
			expected:  "http://qwen.team-a.svc:8000",
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			md := newRenderDeployment(func(md *workercore.ModelDeployment) {
				md.Spec.Roles[0].ExtraArgs = tc.extraArgs
				md.Spec.Roles[0].Command = tc.command
			})

			assert.Equal(t, tc.expected, modelDeploymentEndpoint(md))
		})
	}
}

// TestModelDeploymentEndpointReadsEveryFlagTheEngineGets is the gate under the one assumption
// modelDeploymentEndpoint makes that it cannot check for itself.
//
// The published address is derived from the spec, which cannot show it a rendered command line, so
// it reads the ROLE's arguments alone. That is sound only while the operator's own contributions --
// the engine's base argv and every connector's arguments -- carry no listen or TLS flag. Nothing in
// the type system says so, and a connector that grew one would render an HTTPS probe for a replica
// whose published address still said http://, for the same replica. This fails on that day.
func TestModelDeploymentEndpointReadsEveryFlagTheEngineGets(t *testing.T) {
	operatorArgs := []string{}

	for _, engine := range []string{
		workercore.ModelDeploymentEngineVLLM,
		workercore.ModelDeploymentEngineSGLang,
	} {
		command, err := ModelDeploymentEngineCommand(engine, "Qwen/Qwen2.5-72B-Instruct")
		require.NoError(t, err, "engine %q", engine)
		operatorArgs = append(operatorArgs, command...)
	}

	// THE CONNECTOR'S ARGUMENTS ARE TAKEN FROM THE RENDERER THAT PRODUCES THEM, not from
	// modelDeploymentOwnedKeys. That catalog is what the validating webhook REFUSES a user for; what
	// actually reaches a command line is inject.Render's output, and nothing makes the two equal. A
	// check reading the catalog would pass while the renderer emitted a flag the catalog never
	// listed -- which is the failure this test exists to catch, so it would be checking the one
	// thing that cannot break.
	//
	// inject.Engines() is enumerated rather than restated, because it is wider than the set a user
	// may name: the operator derives vllm-ascend from the pool's accelerator, so a check over the
	// nameable engines alone would miss a third renderer. The roles are the three ParseRole accepts,
	// filtered by SupportsRole so an unsupported pairing is skipped rather than asserted on.
	rendered := 0
	for _, engine := range inject.Engines() {
		for _, role := range []inject.Role{inject.RoleNone, inject.RolePrefill, inject.RoleDecode} {
			if !inject.SupportsRole(engine, role) {
				continue
			}

			// AN ENGINE MAY REFUSE A TRANSPORT -- vllm-ascend takes only "ascend" -- and the table
			// stating which is not exported. Rather than restate it here, where it would go stale
			// silently, each candidate is tried and the first that renders is used. An engine none
			// of them satisfies fails LOUDLY below rather than being skipped, which is the whole
			// difference between this and a check that quietly measures nothing.
			var res *inject.Result
			for _, protocol := range []string{"tcp", "ascend"} {
				r, err := inject.Render(inject.Input{
					Engine: engine,
					Role:   role,
					Domain: "tenant-a",
					Connection: inject.Connection{
						MasterAddress: "kvcache.gpustack-system.svc:50051",
						Protocol:      protocol,
					},
				})
				if err == nil {
					res = r

					break
				}
			}
			require.NotNil(t, res,
				"engine %q role %q rendered under no candidate transport; add the one it takes",
				engine, role)
			require.NotEmpty(t, res.Args, "engine %q role %q renders no argument at all", engine, role)

			operatorArgs = append(operatorArgs, res.Args...)
			for _, group := range res.DefaultedArgs {
				operatorArgs = append(operatorArgs, group...)
			}
			rendered++
		}
	}

	require.NotEmpty(t, operatorArgs, "a check over an empty list is vacuously true")
	// The count is stated so that a renderer which stopped emitting arguments, or an engine dropped
	// from Engines(), shows up as a smaller denominator rather than as a check that still passes.
	require.Equal(t, 9, rendered,
		"three engines with three roles each, sglang included since it renders the prefill and "+
			"decode halves as its own disaggregation arguments; update this figure deliberately "+
			"when that changes")

	// Read under each engine's own spelling rules, which differ in what a prefix reaches.
	for _, engine := range []string{
		workercore.ModelDeploymentEngineVLLM,
		workercore.ModelDeploymentEngineSGLang,
	} {
		scheme, gradable := modelDeploymentEngineTransport(engine, operatorArgs)
		assert.Equal(t, core.URISchemeHTTP, scheme,
			"%s: an operator-supplied TLS flag would move the transport where the published "+
				"address cannot see it", engine)
		assert.True(t, gradable,
			"%s: an operator-supplied listen flag would move the engine where the published "+
				"address cannot see it", engine)
		_, err := modelDeploymentCommandPort(engine, operatorArgs)
		assert.Error(t, err,
			"%s: an operator-supplied --port would move the engine where the Service's target, read "+
				"from the role's arguments alone, cannot see it", engine)
	}
}

// TestModelDeploymentReconciler_ScalingDoesNotRecreateTheService pins what a scale owes it. The
// Service holds an allocated ClusterIP that every client which resolved the name is still using, so
// a scale must move its endpoints and leave the object alone.
func TestModelDeploymentReconciler_ScalingDoesNotRecreateTheService(t *testing.T) {
	md := newRenderDeployment(func(md *workercore.ModelDeployment) { md.Spec.Roles[0].Replicas = 4 })
	// Router-backed so the trim completes; the Router is seeded BEFORE the measured pass below, so
	// the write counter still sees only the two departing replicas being deleted and no Service
	// being created.
	md, router := retirementRouterFixture(md)
	writes := new(modelDeploymentWrites)
	cli := newCountingModelDeploymentClient(writes, md, newRenderInstanceType(), router)

	_, err := reconcileModelDeploymentRetiring(t, cli)
	require.NoError(t, err)
	before := getModelDeploymentService(t, cli)

	scaled := getModelDeployment(t, cli)
	scaled.Spec.Roles[0].Replicas = 2
	require.NoError(t, cli.Update(context.Background(), scaled))

	// A replicas change trims the two highest ordinals in ONE pass -- no Pod that stays is deleted,
	// no total any Pod declares moves, and nothing about the Service's Selector changes hands. The
	// pass that removes replicas is the one most likely to decide it has nothing to front, which is
	// what the assertions below are about.
	*writes = modelDeploymentWrites{}
	_, err = reconcileModelDeploymentRetiring(t, cli)
	require.NoError(t, err)

	require.Len(t, replicaNames(t, cli), 2, "the trim is done in the same pass")
	assert.Zero(t, writes.creates, "a trim creates nothing -- and no Service either")
	assert.Equal(t, 2, writes.deletes, "the only deletes are the two departing replicas")

	after := getModelDeploymentService(t, cli)
	assert.Equal(t, before.UID, after.UID)
	assert.Equal(t, before.ResourceVersion, after.ResourceVersion,
		"and the Service was not touched at all")
}

// TestAlignModelDeploymentService covers what convergence corrects and what it must leave alone.
// Reporting a difference that is not one would rewrite the Service on every pass.
func TestAlignModelDeploymentService(t *testing.T) {
	testCases := []struct {
		name        string
		mutate      func(*core.Service)
		wantChanged bool
	}{
		{name: "nothing drifted", wantChanged: false},
		{
			name:        "a port was edited by hand",
			mutate:      func(svc *core.Service) { svc.Spec.Ports[0].Port = 9999 },
			wantChanged: true,
		},
		{
			name:        "the selector was edited by hand",
			mutate:      func(svc *core.Service) { svc.Spec.Selector["app.kubernetes.io/instance"] = "other" },
			wantChanged: true,
		},
		{
			// REACHABLE ONLY SINCE THIS CONTROLLER WATCHES THE SERVICE: before that, an edit to the
			// type sat until something else woke the deployment. Now the edit wakes it, and a pass
			// that corrected the selector and the ports while leaving the type would be a wake-up
			// that observed the drift and fixed everything except it.
			name: "the type was edited by hand",
			mutate: func(svc *core.Service) {
				svc.Spec.Type = core.ServiceTypeNodePort
				svc.Spec.Ports[0].NodePort = 31234
			},
			wantChanged: true,
		},
		{
			// The API server fills these in and this operator never renders them, so noticing them
			// would rewrite the Service forever.
			name: "the fields the API server owns",
			mutate: func(svc *core.Service) {
				svc.Spec.ClusterIP = "10.0.0.7"
				svc.Spec.ClusterIPs = []string{"10.0.0.7"}
				svc.Spec.SessionAffinity = core.ServiceAffinityNone
				svc.Spec.IPFamilyPolicy = nil
				svc.Spec.Ports[0].NodePort = 0
			},
			wantChanged: false,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			md := newRenderDeployment()
			expected := renderModelDeploymentService(md, nil)
			actual := renderModelDeploymentService(md, nil)
			if tc.mutate != nil {
				tc.mutate(actual)
			}

			assert.Equal(t, tc.wantChanged, alignModelDeploymentService(actual, expected))
			if tc.wantChanged {
				assert.Equal(t, expected.Spec.Ports, actual.Spec.Ports)
				assert.Equal(t, expected.Spec.Selector, actual.Spec.Selector)
				// Asserted with the rest: a corrected type that kept the assigned nodePort is a
				// Service the API server refuses, so the two have to move together.
				assert.Equal(t, expected.Spec.Type, actual.Spec.Type)
				assert.Zero(t, actual.Spec.Ports[0].NodePort)
			}
		})
	}
}

// TestModelDeploymentReconciler_RefusesAServiceItDoesNotOwn states that a name collision is
// reported rather than resolved. Taking over a Service that belongs to something else would
// redirect whatever already points at it, which is the one failure here nobody would look for in
// this operator's logs.
func TestModelDeploymentReconciler_RefusesAServiceItDoesNotOwn(t *testing.T) {
	stray := &core.Service{}
	stray.Name, stray.Namespace = "qwen", "team-a"
	stray.Spec.Selector = map[string]string{"app": "something-else"}
	stray.Spec.Ports = []core.ServicePort{{Name: "http", Port: 80}}

	cli := newModelDeploymentClient(newRenderDeployment(), newRenderInstanceType(), stray)

	_, err := reconcileModelDeployment(t, cli)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not owned by this deployment")

	kept := getModelDeploymentService(t, cli)
	assert.Equal(t, map[string]string{"app": "something-else"}, kept.Spec.Selector)
}

// TestEndpointEligibleSelectorActivation pins the first-enable repair at the three rendered
// surfaces. A shape that can never qualify -- an unverified engine version, a prefill role, a
// replaced command -- renders no eligibility term whatever any pass says. A shape that can
// qualify does not narrow until a real pass records the predicate satisfied; the condition here
// is computed by the real qualifier and the real status observation, never set by hand. Once a
// selector carries the term, a quiet pass retains it, and a definite fault still withdraws.
func TestEndpointEligibleSelectorActivation(t *testing.T) {
	unverified := healthDeployment(2)
	unverified.Spec.Router = &workercore.ModelDeploymentRouter{Name: workercore.ModelDeploymentRouterLLMD}
	unverified.Spec.Engine.Version = "0.0.0-not-verified"

	prefill := healthDeployment(2)
	prefill.Spec.Router = &workercore.ModelDeploymentRouter{Name: workercore.ModelDeploymentRouterLLMD}
	for i := range prefill.Spec.Roles {
		prefill.Spec.Roles[i].Kind = workercore.ModelDeploymentRoleKindPrefill
	}

	takeOver := healthDeployment(2)
	takeOver.Spec.Router = &workercore.ModelDeploymentRouter{Name: workercore.ModelDeploymentRouterLLMD}
	takeOver.Spec.Roles[0].Command = []string{"custom-server"}

	healthy := healthDeployment(2)
	healthy.Spec.Router = &workercore.ModelDeploymentRouter{Name: workercore.ModelDeploymentRouterLLMD}
	// Stamped with the group total the render always writes, because completeness is measured
	// against the replica's OWN total now. Without it this pair reads as two replicas of one, and a
	// member claiming seat one of a group declaring one is a group this operator cannot classify.
	healthyPods := stampHealthGroupTotals(healthy, []core.Pod{
		healthPod("server", 0, 0, healthBool(true), "uid-a"),
		healthPod("server", 0, 1, healthBool(true), "uid-b"),
	})

	for name, md := range map[string]*workercore.ModelDeployment{
		"unverified engine version": unverified,
		"prefill role":              prefill,
		"replaced command":          takeOver,
	} {
		t.Run(name+" renders no narrowing", func(t *testing.T) {
			front := renderModelDeploymentService(md, nil)
			_, has := front.Spec.Selector[modelDeploymentLabelKeyEndpointEligible]
			assert.False(t, has, "front Service must not narrow a never-activating shape")
			roleSvc := renderModelDeploymentRoleService(md, &md.Spec.Roles[0], nil, nil, false)
			_, has = roleSvc.Spec.Selector[modelDeploymentLabelKeyEndpointEligible]
			assert.False(t, has, "role Service must not narrow a never-activating shape")
			published, err := renderModelDeploymentRouterObjects(context.Background(), md,
				map[string]string{md.Spec.Roles[0].Name: "manufacturer"}, false)
			require.NoError(t, err)
			_, has = published.Contract.Roles[0].Selector[modelDeploymentLabelKeyEndpointEligible]
			assert.False(t, has, "router discovery must not narrow a never-activating shape")
			assert.NotContains(t, published.ConfigMap.Data[modelDeploymentRouterConfigKey],
				modelDeploymentLabelKeyEndpointEligible+"="+modelDeploymentEndpointEligibleValue,
				"router discovery config must not narrow a never-activating shape")
		})
	}

	t.Run("quiet first enable keeps routing, activation narrows, quiet passes retain", func(t *testing.T) {
		quiet := func(context.Context, string, []byte) ([]byte, error) {
			return nil, context.DeadlineExceeded
		}

		first := qualifyModelDeploymentInstances(context.Background(), healthy, healthyPods,
			modelDeploymentPendingReplacement{}, quiet)
		require.Len(t, first, 1)
		require.False(t, first[0].Activated(), "setup requires an unavailable first observation")
		observeModelDeploymentEndpointEligibility(healthy, first)
		assert.False(t, modelDeploymentEligibilityDecided(healthy), "no activation fact exists yet")
		front := renderModelDeploymentService(healthy, nil)
		_, has := front.Spec.Selector[modelDeploymentLabelKeyEndpointEligible]
		assert.False(t, has, "first enable with an unavailable observation must not narrow")

		working := qualifyModelDeploymentInstances(context.Background(), healthy, healthyPods,
			modelDeploymentPendingReplacement{}, boundProbeFetch)
		require.Len(t, working, 1)
		require.True(t, working[0].Activated(), "setup requires an available observation")
		observeModelDeploymentEndpointEligibility(healthy, working)
		assert.True(t, modelDeploymentEligibilityDecided(healthy), "a real pass recorded the predicate")
		front = renderModelDeploymentService(healthy, nil)
		assert.Equal(t, modelDeploymentEndpointEligibleValue,
			front.Spec.Selector[modelDeploymentLabelKeyEndpointEligible], "activation narrows")

		again := qualifyModelDeploymentInstances(context.Background(), healthy, healthyPods,
			modelDeploymentPendingReplacement{}, quiet)
		observeModelDeploymentEndpointEligibility(healthy, again)
		assert.False(t, modelDeploymentEligibilityDecided(healthy), "a quiet pass only holds")
		retained := modelDeploymentEligibilitySelectorActive(healthy, &healthy.Spec.Roles[0],
			map[string]string{modelDeploymentLabelKeyEndpointEligible: modelDeploymentEndpointEligibleValue}, false)
		assert.True(t, retained, "an already-narrowed selector survives a quiet pass")
		fresh := renderModelDeploymentService(healthy, nil)
		_, has = fresh.Spec.Selector[modelDeploymentLabelKeyEndpointEligible]
		assert.False(t, has, "a from-scratch render never fabricates the term")
	})

	t.Run("a definite fault still withdraws the replica", func(t *testing.T) {
		broken := []core.Pod{
			healthPod("server", 0, 0, healthBool(true), "uid-a"),
			healthPod("server", 0, 1, healthBool(false), "uid-b"),
		}
		qs := qualifyModelDeploymentInstances(context.Background(), healthy, broken,
			modelDeploymentPendingReplacement{}, boundProbeFetch)
		require.Len(t, qs, 1)
		assert.True(t, qs[0].HasFailure(), "the readiness fault is definite")
		assert.False(t, qs[0].Eligible(), "a definite fault withdraws regardless of the probe")
	})
}

func TestAdmissionFirstEnableUnknownPreservesExistingRouting(t *testing.T) {
	for _, tc := range []struct {
		name   string
		custom bool
	}{
		{name: "supported engine without an available native observation"},
		{name: "custom command without its required serving annotation", custom: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			md := healthDeployment(2)
			md.Spec.Router = &workercore.ModelDeploymentRouter{Name: workercore.ModelDeploymentRouterLLMD}
			pods := []core.Pod{
				healthPod("server", 0, 0, healthBool(true), "uid-a"),
				healthPod("server", 0, 1, healthBool(true), "uid-b"),
			}
			if tc.custom {
				md.Spec.Roles[0].Command = []string{"custom-server"}
				for i := range pods {
					delete(pods[i].Annotations, "prometheus.io/port")
				}
			}
			qs := qualifyModelDeploymentInstances(context.Background(), md, pods,
				modelDeploymentPendingReplacement{},
				func(context.Context, string, []byte) ([]byte, error) { return nil, context.DeadlineExceeded })
			require.Len(t, qs, 1)
			require.False(t, qs[0].Activated(), "setup requires unavailable first-enable observation")
			require.False(t, qs[0].HasFailure(), "setup requires healthy existing members")
			for i := range pods {
				view := modelDeploymentReplicaView{Members: []*core.Pod{&pods[i]}}
				require.False(t, modelDeploymentPodEligible(md, &md.Spec.Roles[0], &pods[i], view, qs[0], true),
					"first enable must not fabricate eligibility")
			}
			for _, svc := range []*core.Service{
				renderModelDeploymentService(md, nil), renderModelDeploymentRoleService(md, &md.Spec.Roles[0], nil, nil, false),
			} {
				_, narrowed := svc.Spec.Selector[modelDeploymentLabelKeyEndpointEligible]
				assert.False(t, narrowed, "unavailable first-enable observation must preserve Service selection")
			}
			router, err := renderModelDeploymentRouterObjects(context.Background(), md, map[string]string{"server": "manufacturer"}, false)
			require.NoError(t, err)
			require.NotEmpty(t, router.Contract.Roles)
			_, narrowed := router.Contract.Roles[0].Selector[modelDeploymentLabelKeyEndpointEligible]
			assert.False(t, narrowed, "unavailable first-enable observation must preserve router discovery")
		})
	}
}
