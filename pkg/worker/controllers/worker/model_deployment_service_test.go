package worker

import (
	"context"
	"slices"
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	core "k8s.io/api/core/v1"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	ctrlrecord "k8s.io/client-go/tools/record"
	ctrlcli "sigs.k8s.io/controller-runtime/pkg/client"
	kueuectrlconst "sigs.k8s.io/kueue/pkg/controller/constants"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	"gpustack.ai/gpustack/pkg/kubemeta"
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

// epdDeployment is the three-role shape an encoder deployment has: an encoder, a prefiller and a
// decoder, in the order the case gives them.
//
// THE ENCODER DECLARES ITS OWN PORT AND ITS OWN TRANSPORT. That is what makes "the front door read
// the encoder" observable rather than a matter of taste: with every role answering on the same port
// over the same transport, a Service pointed at an encoder would be indistinguishable from one
// pointed at a prefiller. As shaped here the two disagree on both figures the front door publishes --
// the selector names the role, the port says 8000 against the encoder's 9100, and the address says
// http against the encoder's https.
func epdDeployment(
	t *testing.T, order []workercore.ModelDeploymentRoleKind, mutate ...func(*workercore.ModelDeployment),
) *workercore.ModelDeployment {
	t.Helper()

	shapes := map[workercore.ModelDeploymentRoleKind]workercore.ModelDeploymentRole{
		workercore.ModelDeploymentRoleKindEncode: {
			Name:         "encode",
			Kind:         workercore.ModelDeploymentRoleKindEncode,
			InstanceType: "h20-8x",
			Image:        "vllm/vllm-openai:v0.30.0",
			Ports: []workercore.ModelDeploymentPort{
				{Port: 9100, Protocol: core.ProtocolTCP},
			},
			ExtraArgs: []string{"--port", "9100", "--ssl-certfile", "/etc/tls/tls.crt"},
		},
		workercore.ModelDeploymentRoleKindPrefill: {
			Name:         "prefill",
			Kind:         workercore.ModelDeploymentRoleKindPrefill,
			InstanceType: "h20-8x",
			Image:        "vllm/vllm-openai:v0.30.0",
		},
		workercore.ModelDeploymentRoleKindDecode: {
			Name:         "decode",
			Kind:         workercore.ModelDeploymentRoleKindDecode,
			InstanceType: "h20-8x",
			Image:        "vllm/vllm-openai:v0.30.0",
		},
	}

	roles := make([]workercore.ModelDeploymentRole, 0, len(order))
	for _, kind := range order {
		role, known := shapes[kind]
		require.Truef(t, known, "no encoder-deployment shape for role kind %q", kind)
		roles = append(roles, role)
	}

	return newRenderDeployment(append([]func(*workercore.ModelDeployment){
		func(md *workercore.ModelDeployment) {
			md.Spec.Roles = roles
		},
	}, mutate...)...)
}

// TestRenderModelDeploymentServices_AnEncodeRoleGetsItsOwnService covers what the position of the
// encoder changes and what it does not.
//
// AN ENCODER IS A ROLE LIKE ANY OTHER to this renderer, so it is served like one. What must not
// follow from it is the front door: the deployment-wide Service answers the OpenAI API, and an
// encoder answers none of it.
func TestRenderModelDeploymentServices_AnEncodeRoleGetsItsOwnService(t *testing.T) {
	md := epdDeployment(t, []workercore.ModelDeploymentRoleKind{
		workercore.ModelDeploymentRoleKindEncode,
		workercore.ModelDeploymentRoleKindPrefill,
		workercore.ModelDeploymentRoleKindDecode,
	})

	services := renderModelDeploymentServices(md, nil, nil)
	byName := map[string]*core.Service{}
	for _, svc := range services {
		byName[svc.Name] = svc
	}

	encode := byName["qwen-encode"]
	require.NotNil(t, encode, "the encoder role is served by a Service of its own, %v",
		serviceNamesOf(services))
	assert.Equal(t, map[string]string{
		modelDeploymentLabelKeyName:      modelDeploymentLabelValueName,
		modelDeploymentLabelKeyInstance:  "qwen",
		modelDeploymentLabelKeyComponent: "encode",
	}, encode.Spec.Selector)
	assert.Equal(t, md.Name, services[0].Name,
		"the deployment-wide Service comes first, and the role Services follow it")
}

// serviceNamesOf names the rendered Services in the order they were rendered.
func serviceNamesOf(services []*core.Service) []string {
	names := make([]string, 0, len(services))
	for _, svc := range services {
		names = append(names, svc.Name)
	}

	return names
}

// TestRenderModelDeploymentService_NeverFrontsAnEncodeRole pins where an encoder deployment's front
// door lands.
//
// AN ENCODER IS NOT A FRONT DOOR. It serves embeddings for a prefiller, not completions, so a Service
// selecting it round-robins requests onto a process that answers none of them -- and the failure is
// silent, because the Service has endpoints and the router's health check is the only thing that
// notices. The rule is therefore POSITIONAL AND TOTAL: the front role is the first role that can
// answer, wherever the encoder sits in the list. A deployment with no encoder keeps today's rule,
// which is also the first role.
func TestRenderModelDeploymentService_NeverFrontsAnEncodeRole(t *testing.T) {
	testCases := []struct {
		name  string
		order []workercore.ModelDeploymentRoleKind
	}{
		{
			// The order a reader would write an encoder deployment in, and the one that breaks a
			// front door still reading the first role.
			name: "the_encoder_first",
			order: []workercore.ModelDeploymentRoleKind{
				workercore.ModelDeploymentRoleKindEncode,
				workercore.ModelDeploymentRoleKindPrefill,
				workercore.ModelDeploymentRoleKindDecode,
			},
		},
		{
			name: "the_encoder_in_the_middle",
			order: []workercore.ModelDeploymentRoleKind{
				workercore.ModelDeploymentRoleKindPrefill,
				workercore.ModelDeploymentRoleKindEncode,
				workercore.ModelDeploymentRoleKindDecode,
			},
		},
		{
			// The two orders above are what a first-role rule and a skip rule disagree about. This
			// one both already answer for, and exists so the table cannot pass against a rule that
			// skips the encoder only in some positions.
			name: "the_encoder_last",
			order: []workercore.ModelDeploymentRoleKind{
				workercore.ModelDeploymentRoleKindPrefill,
				workercore.ModelDeploymentRoleKindDecode,
				workercore.ModelDeploymentRoleKindEncode,
			},
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			md := epdDeployment(t, tc.order)

			svc := renderModelDeploymentService(md, nil)

			assert.Equal(t, md.Name, svc.Name)
			assert.Equal(t, "prefill",
				svc.Spec.Selector[modelDeploymentLabelKeyComponent],
				"the front door routes the first role that answers, not the first one listed")
			require.Len(t, svc.Spec.Ports, 1)
			assert.Equal(t, int32(8000), svc.Spec.Ports[0].Port,
				"and publishes the address that role listens on, which the encoder's 9100 is not")
		})
	}
}

// TestModelDeploymentEndpoint_NeverReadsAnEncodeRole pins the address a user is handed.
//
// THE PUBLISHED ADDRESS IS THE SAME FRONT DOOR IN A DIFFERENT GUISE: it is what a client is told to
// dial. A front door reading the encoder would publish the encoder's port and the encoder's transport
// at the same time, which is the clearest available statement that it did.
func TestModelDeploymentEndpoint_NeverReadsAnEncodeRole(t *testing.T) {
	testCases := []struct {
		name  string
		order []workercore.ModelDeploymentRoleKind
	}{
		{
			name: "the_encoder_first",
			order: []workercore.ModelDeploymentRoleKind{
				workercore.ModelDeploymentRoleKindEncode,
				workercore.ModelDeploymentRoleKindPrefill,
				workercore.ModelDeploymentRoleKindDecode,
			},
		},
		{
			name: "the_encoder_in_the_middle",
			order: []workercore.ModelDeploymentRoleKind{
				workercore.ModelDeploymentRoleKindPrefill,
				workercore.ModelDeploymentRoleKindEncode,
				workercore.ModelDeploymentRoleKindDecode,
			},
		},
		{
			name: "the_encoder_last",
			order: []workercore.ModelDeploymentRoleKind{
				workercore.ModelDeploymentRoleKindPrefill,
				workercore.ModelDeploymentRoleKindDecode,
				workercore.ModelDeploymentRoleKindEncode,
			},
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			md := epdDeployment(t, tc.order)

			assert.Equal(t, "http://qwen.team-a.svc:8000", modelDeploymentEndpoint(md),
				"https://...:9100 is the encoder's address, and no client can take a completion from it")
		})
	}
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
// surfaces.
//
// TWO REASONS WITHHOLD THE TERM, AND EACH IS ASSERTED ON ITS OWN. A shape that can never qualify
// -- a role whose command line was replaced with no managed replica of it still standing -- renders
// no eligibility term whatever any pass says. A shape that CAN qualify does not narrow until a real
// pass records the predicate satisfied, so a supported pair with nothing decided renders no term
// either, for a different reason.
//
// The condition here is computed by the real qualifier and the real status observation, never set
// by hand. Once a selector carries the term, a quiet pass retains it, and a definite fault still
// withdraws.
func TestEndpointEligibleSelectorActivation(t *testing.T) {
	prefill := healthDeployment(2)
	prefill.Spec.Router = &workercore.ModelDeploymentRouter{Name: workercore.ModelDeploymentRouterLLMD}
	for i := range prefill.Spec.Roles {
		prefill.Spec.Roles[i].Kind = workercore.ModelDeploymentRoleKindPrefill
	}

	// THE SAME PAIR WITH ONE ROLE TAKING ITS COMMAND LINE OVER: a shape the support check refuses,
	// so no pass can ever qualify it and its surfaces never narrow.
	prefillTakeOver := healthDeployment(2)
	prefillTakeOver.Spec.Router = &workercore.ModelDeploymentRouter{Name: workercore.ModelDeploymentRouterLLMD}
	for i := range prefillTakeOver.Spec.Roles {
		prefillTakeOver.Spec.Roles[i].Kind = workercore.ModelDeploymentRoleKindPrefill
	}
	prefillTakeOver.Spec.Roles[0].Command = []string{"custom-server"}

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

	for _, tc := range []struct {
		name   string
		md     *workercore.ModelDeployment
		reason string
	}{
		{
			name:   "supported pair with nothing decided",
			md:     prefill,
			reason: "nothing has decided the predicate yet",
		},
		{
			name:   "pair that took its command line over",
			md:     prefillTakeOver,
			reason: "the shape can never activate",
		},
		{
			name:   "replaced command",
			md:     takeOver,
			reason: "the shape can never activate",
		},
	} {
		t.Run(tc.name+" renders no narrowing", func(t *testing.T) {
			front := renderModelDeploymentService(tc.md, nil)
			_, has := front.Spec.Selector[modelDeploymentLabelKeyEndpointEligible]
			assert.False(t, has, "front Service must not narrow because "+tc.reason)
			roleSvc := renderModelDeploymentRoleService(tc.md, &tc.md.Spec.Roles[0], nil, nil, false)
			_, has = roleSvc.Spec.Selector[modelDeploymentLabelKeyEndpointEligible]
			assert.False(t, has, "role Service must not narrow because "+tc.reason)
			published, err := renderModelDeploymentRouterObjects(context.Background(), tc.md,
				map[string]string{tc.md.Spec.Roles[0].Name: "manufacturer"}, false)
			require.NoError(t, err)
			_, has = published.Contract.Roles[0].Selector[modelDeploymentLabelKeyEndpointEligible]
			assert.False(t, has, "router discovery must not narrow because "+tc.reason)
			assert.NotContains(t, published.ConfigMap.Data[modelDeploymentRouterConfigKey],
				modelDeploymentLabelKeyEndpointEligible+"="+modelDeploymentEndpointEligibleValue,
				"router discovery config must not narrow because "+tc.reason)
		})
	}

	t.Run("a take-over pair stays unnarrowed beside a qualified ordinary role", func(t *testing.T) {
		// THE PREDICATE IS DECIDED HERE, and that is the whole point of this case. With nothing
		// decided the ACTIVATION half would withhold the term on its own, so a take-over shape
		// beside an ordinary one would be unnarrowed for a reason that has nothing to do with the
		// take-over. Here the ordinary role's real qualification decides the predicate, so the
		// ACTIVATION half WOULD narrow this deployment, and the take-over role's own surfaces stay
		// unnarrowed because the shape can never activate -- which is then the only reason left.
		md := healthDeployment(2, "prefill")
		md.Spec.Router = &workercore.ModelDeploymentRouter{Name: workercore.ModelDeploymentRouterLLMD}
		// THE TAKE-OVER ROLE IS THE FRONT ONE, because the deployment-wide Service routes the front
		// role alone and the case is about that role's surfaces.
		md.Spec.Roles[0], md.Spec.Roles[1] = md.Spec.Roles[1], md.Spec.Roles[0]
		md.Spec.Roles[0].Kind = workercore.ModelDeploymentRoleKindPrefill
		md.Spec.Roles[0].Command = []string{"custom-server"}
		pods := stampHealthGroupTotals(md, []core.Pod{
			healthPod("server", 0, 0, healthBool(true), "uid-a"),
			healthPod("server", 0, 1, healthBool(true), "uid-b"),
		})
		qs := qualifyModelDeploymentInstances(context.Background(), md, pods,
			modelDeploymentPendingReplacement{}, boundProbeFetch)
		require.Len(t, qs, 1, "only the ordinary role has members")
		observeModelDeploymentEndpointEligibility(md, qs)
		require.True(t, modelDeploymentEligibilityDecided(md),
			"setup requires a decided predicate, so the take-over role's unnarrowed surfaces name the static half")

		ordinary := renderModelDeploymentRoleService(md, &md.Spec.Roles[1], nil, nil, false)
		assert.Equal(t, modelDeploymentEndpointEligibleValue,
			ordinary.Spec.Selector[modelDeploymentLabelKeyEndpointEligible],
			"the ordinary role beside it narrows, so the predicate really is decided")

		front := renderModelDeploymentService(md, nil)
		_, has := front.Spec.Selector[modelDeploymentLabelKeyEndpointEligible]
		assert.False(t, has, "a shape that can never activate never narrows, whatever any pass says")
		roleSvc := renderModelDeploymentRoleService(md, &md.Spec.Roles[0], nil, nil, false)
		_, has = roleSvc.Spec.Selector[modelDeploymentLabelKeyEndpointEligible]
		assert.False(t, has, "its own role Service does not narrow either")
		published, err := renderModelDeploymentRouterObjects(context.Background(), md,
			map[string]string{md.Spec.Roles[0].Name: "manufacturer"}, false)
		require.NoError(t, err)
		_, has = published.Contract.Roles[0].Selector[modelDeploymentLabelKeyEndpointEligible]
		assert.False(t, has, "the router's published selector for it does not narrow either")
	})

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

// The front Service routes the first role alone. A managed Pod of a sibling role, or one already
// leaving, must not lift the take-over refusal for a first role that supplies its own command.
func TestRenderModelDeploymentService_FrontGateReadsOnlyTheFrontRoleMembers(t *testing.T) {
	newDeployment := func() *workercore.ModelDeployment {
		md := healthDeployment(1, "sibling")
		md.Spec.Roles[0].Command = []string{"vllm", "serve", "qwen"}
		ModelDeploymentConditionEndpointEligibility.True(md,
			modelDeploymentReasonEndpointsQualified, "every group qualified")

		return md
	}
	// siblingPod is a managed Pod, which is what would lift the refusal if it were counted.
	siblingPod := func(md *workercore.ModelDeployment, mutate func(*core.Pod)) core.Pod {
		pod := healthPod("sibling", 0, 0, healthBool(true), "uid-sibling")
		pod.Spec.Containers = []core.Container{{Name: modelDeploymentMainContainerName}}
		kubemeta.ControlOnWithoutBlock(&pod, md, workercore.SchemeGroupVersionKind("ModelDeployment"))
		if mutate != nil {
			mutate(&pod)
		}

		return pod
	}

	testCases := []struct {
		name string
		pod  func(md *workercore.ModelDeployment) core.Pod
	}{
		{
			name: "a managed Pod of a sibling role",
			pod:  func(md *workercore.ModelDeployment) core.Pod { return siblingPod(md, nil) },
		},
		{
			name: "a managed Pod of the front role that is already leaving",
			pod: func(md *workercore.ModelDeployment) core.Pod {
				return siblingPod(md, func(pod *core.Pod) {
					pod.Labels[modelDeploymentLabelKeyComponent] = md.Spec.Roles[0].Name
					now := meta.Now()
					pod.DeletionTimestamp = &now
				})
			},
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			md := newDeployment()
			pod := tc.pod(md)
			require.True(t, modelDeploymentReplicaRunsCommand([]*core.Pod{&pod}),
				"setup requires the Pod to be one this operator built")

			svc := renderModelDeploymentService(md, []core.Pod{pod})

			_, narrowed := svc.Spec.Selector[modelDeploymentLabelKeyEndpointEligible]
			assert.False(t, narrowed, "a take-over front role is never narrowed by endpoint eligibility")
		})
	}
}

// serviceReadyUnknown is the readiness a member reports while kubelet has not made up its mind.
//
// THE GROUP IS HELD RATHER THAN REVOKED, and the replica is not part of the deployment-wide answer
// at all: a member that has reported nothing is a member the operator knows nothing about.
var serviceReadyUnknown *bool

// serviceRoleOf returns the named role of a deployment, or nil when the deployment does not
// declare one.
func serviceRoleOf(md *workercore.ModelDeployment, name string) *workercore.ModelDeploymentRole {
	for i := range md.Spec.Roles {
		if md.Spec.Roles[i].Name == name {
			return &md.Spec.Roles[i]
		}
	}

	return nil
}

// serviceManagedMember is a member this operator built, which is what makes a role's effective
// command line empty while such a member stands.
func serviceManagedMember(role string, ordinal int) core.Pod {
	member := healthPod(role, ordinal, 0, healthBool(true), "uid-managed")
	member.Spec.Containers = []core.Container{{Name: modelDeploymentMainContainerName}}

	return member
}

// serviceExternalDPMember is one member of an External-DP replica, carrying the command line the
// render would have written.
//
// THE DEPLOYED SHAPE IS READ FROM THE MEMBERS and not from the role, so a member that carries no
// command line reads as leader-served and the role's own arguments would never be consulted.
func serviceExternalDPMember(role string, ordinal, member int, ready *bool, uid string) core.Pod {
	pod := healthPod(role, ordinal, member, ready, uid)
	pod.Spec.Containers = []core.Container{{
		Name: modelDeploymentMainContainerName,
		Args: []string{"--data-parallel-size", "2", "--data-parallel-rank", strconv.Itoa(member)},
	}}

	return pod
}

// serviceLabelPods runs the real qualifier and the real label writers over the members, and
// returns the members with the eligibility and answering labels written plus the qualifications
// the pass produced.
//
// THE WRITERS ARE THE REAL ONES: the question this file asks is which endpoints a Service selector
// reaches, and that depends on the labels the writers put on the member. The live selectors are
// the ones the live Services carry, because the writers read them to decide whether a member
// answers.
func serviceLabelPods(
	t *testing.T, md *workercore.ModelDeployment, pods []core.Pod,
	liveSelectors map[string]map[string]string,
	pairFetch ...modelDeploymentGroupForwardPairFetch,
) ([]core.Pod, []modelDeploymentInstanceQualification) {
	t.Helper()

	qualifications := qualifyModelDeploymentInstances(context.Background(), md, pods,
		modelDeploymentPendingReplacement{}, boundProbeFetch, pairFetch...)
	byMember := modelDeploymentQualificationsByMember(qualifications)

	labelled := make([]core.Pod, len(pods))
	copy(labelled, pods)
	for _, view := range modelDeploymentGroupPodsByReplica(labelled) {
		role := serviceRoleOf(md, view.Role)
		if role == nil {
			continue
		}
		for _, member := range view.Members {
			qualification, evaluated := byMember[member.UID]
			eligible := modelDeploymentPodEligible(md, role, member, view, qualification, evaluated)
			// THE GATE IS ASKED OF THE MODE THIS REPLICA IS RUNNING, as the writer asks it.
			effective := *role
			if modelDeploymentReplicaRunsCommand(view.Members) {
				effective.Command = nil
			}
			gated := modelDeploymentEligibilitySelectorActive(md, &effective,
				liveSelectors[role.Name], modelDeploymentEligibilityDecided(md))
			answering := modelDeploymentPodAnswersAPI(member, view, md.Spec.Engine.Name,
				eligible, gated, map[int]bool{})
			modelDeploymentApplyLabel(member, modelDeploymentLabelKeyEndpointEligible, eligible)
			modelDeploymentApplyLabel(member, modelDeploymentLabelKeyAPIAnswering, answering)
		}
	}

	return labelled, qualifications
}

// serviceReached names the members a Service's selector reaches, sorted, so a case states the
// whole set it expects rather than probing for one member of it.
func serviceReached(svc *core.Service, pods []core.Pod) []string {
	selector := labels.SelectorFromSet(svc.Spec.Selector)
	reached := make([]string, 0, len(pods))
	for i := range pods {
		if selector.Matches(labels.Set(pods[i].Labels)) {
			reached = append(reached, pods[i].Name)
		}
	}
	slices.Sort(reached)

	return reached
}

// TestModelDeploymentGroupForwardActivatable states when a role's group-forward capability can
// activate the eligibility gate.
//
// THREE FACTS ARE KEPT APART HERE. An engine the matrix does not carry has no observation at
// all. A role that took its command line over serves a route the operator does not know, so it
// cannot activate -- unless a managed member of it still stands, because the mode the standing
// members run is the mode the gate asks about. A P/D role activates when the operator has an
// observation for its pair, and a partner that took its own command line over does not take this
// role's barrier away.
func TestModelDeploymentGroupForwardActivatable(t *testing.T) {
	pairDeployment := func(engine string, fronted bool) *workercore.ModelDeployment {
		md := healthPairDeployment(engine, 1, 1)
		if fronted {
			md.Spec.Router = &workercore.ModelDeploymentRouter{Name: workercore.ModelDeploymentRouterLLMD}
		}

		return md
	}
	takenOver := func(md *workercore.ModelDeployment, role string) *workercore.ModelDeployment {
		for i := range md.Spec.Roles {
			if md.Spec.Roles[i].Name == role {
				md.Spec.Roles[i].Command = []string{"custom-server"}
			}
		}

		return md
	}

	testCases := []struct {
		name string
		md   *workercore.ModelDeployment
		role string
		// standing says a managed member of the role still stands, which is what makes the
		// role's effective command line empty.
		standing bool
		want     bool
	}{
		{
			name: "an ordinary role of a supported engine",
			md:   healthDeployment(1),
			role: "server",
			want: true,
		},
		{
			name: "an ordinary role of an engine outside the matrix",
			md: func() *workercore.ModelDeployment {
				md := healthDeployment(1)
				md.Spec.Engine.Name = "no-such-engine"

				return md
			}(),
			role: "server",
			want: false,
		},
		{
			name: "a role that took its command line over",
			md: func() *workercore.ModelDeployment {
				md := healthDeployment(1)
				md.Spec.Roles[0].Command = []string{"custom-server"}

				return md
			}(),
			role: "server",
			want: false,
		},
		{
			name:     "a role that took its command line over while a managed member stands",
			md:       takenOver(healthDeployment(1), "server"),
			role:     "server",
			standing: true,
			want:     true,
		},
		{
			name: "a prefill role of a supported pair",
			md:   pairDeployment(workercore.ModelDeploymentEngineSGLang, false),
			role: "prefill",
			want: true,
		},
		{
			name: "a decode role of a supported pair",
			md:   pairDeployment(workercore.ModelDeploymentEngineSGLang, false),
			role: "decode",
			want: true,
		},
		{
			name: "a prefill role of an engine the pair matrix does not carry",
			md:   pairDeployment("no-such-engine", false),
			role: "prefill",
			want: false,
		},
		{
			name: "a vllm decode role with no routing proxy in front of it",
			md:   pairDeployment(workercore.ModelDeploymentEngineVLLM, false),
			role: "decode",
			want: false,
		},
		{
			name: "a vllm decode role with the routing proxy in front of it",
			md:   pairDeployment(workercore.ModelDeploymentEngineVLLM, true),
			role: "decode",
			want: true,
		},
		{
			name: "a prefill role that took its command line over",
			md:   takenOver(pairDeployment(workercore.ModelDeploymentEngineSGLang, false), "prefill"),
			role: "prefill",
			want: false,
		},
		{
			name:     "a prefill role that took its command line over while a managed member stands",
			md:       takenOver(pairDeployment(workercore.ModelDeploymentEngineSGLang, false), "prefill"),
			role:     "prefill",
			standing: true,
			want:     true,
		},
		{
			name: "a decode role whose prefill partner took its command line over",
			md:   takenOver(pairDeployment(workercore.ModelDeploymentEngineSGLang, false), "prefill"),
			role: "decode",
			want: true,
		},
		{
			name: "a decode role that took its own command line over",
			md:   takenOver(pairDeployment(workercore.ModelDeploymentEngineSGLang, false), "decode"),
			role: "decode",
			want: false,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			role := serviceRoleOf(tc.md, tc.role)
			require.NotNil(t, role, "setup requires the named role")
			if tc.standing {
				member := serviceManagedMember(tc.role, 0)
				role = modelDeploymentEffectiveRoleForEligibility(role, []*core.Pod{&member})
			}

			assert.Equal(t, tc.want, modelDeploymentGroupForwardActivatable(tc.md, role),
				"the %s role of a %s deployment", tc.role, tc.md.Spec.Engine.Name)
		})
	}
}

// serviceExternalDPDeployment is a deployment whose role this operator serves with a data-parallel
// replica, so every rank of the replica answers for itself.
func serviceExternalDPDeployment() *workercore.ModelDeployment {
	return newRenderDeployment(func(md *workercore.ModelDeployment) {
		md.Spec.Roles[0].ReplicaSize = 2
		md.Spec.Roles[0].Replicas = 2
		md.Spec.Roles[0].ExtraArgs = []string{
			"--data-parallel-size", "2",
			"--data-parallel-rank", "$(GPUSTACK_MEMBER_INDEX)",
		}
	})
}

// serviceLeaderMember is one member of a replica this operator serves with a leader, so the
// member carries no parallelism and the replica answers for its leader alone.
func serviceLeaderMember(role string, ordinal, member int, ready *bool, uid string) core.Pod {
	pod := healthPod(role, ordinal, member, ready, uid)
	pod.Spec.Containers = []core.Container{{Name: modelDeploymentMainContainerName}}

	return pod
}

// TestRenderModelDeploymentRoleService_PrefillAndDecodeSelectIndependently states that the two
// roles of a P/D deployment are selected by their own qualification, and that the deployment-wide
// Service follows the front role alone.
//
// THE PAIR IS OBSERVED AS ONE THING, so a fault on one side is expected to reach the other side.
// What this pins is the SELECTION: each role's Service reaches the members its own role
// qualified, and the deployment-wide Service reaches exactly what the front role's Service
// reaches.
func TestRenderModelDeploymentRoleService_PrefillAndDecodeSelectIndependently(t *testing.T) {
	testCases := []struct {
		name string
		// unsafe names the group that is not safe, by role and ordinal.
		unsafeRole    string
		unsafeOrdinal int
		wantPrefill   []string
		wantDecode    []string
	}{
		{
			name:          "a prefill group that is not safe leaves the decode role selectable",
			unsafeRole:    "prefill",
			unsafeOrdinal: 0,
			wantPrefill:   []string{"prefill-1-0"},
			wantDecode:    []string{"decode-0-0", "decode-1-0"},
		},
		{
			name:          "a decode group that is not safe leaves the prefill role selectable",
			unsafeRole:    "decode",
			unsafeOrdinal: 0,
			wantPrefill:   []string{"prefill-0-0", "prefill-1-0"},
			wantDecode:    []string{"decode-1-0"},
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			md := healthPairDeployment(workercore.ModelDeploymentEngineSGLang, 1, 1)
			for i := range md.Spec.Roles {
				md.Spec.Roles[i].Replicas = 2
			}
			ready := func(role string, ordinal int) *bool {
				if role == tc.unsafeRole && ordinal == tc.unsafeOrdinal {
					return healthBool(false)
				}

				return healthBool(true)
			}
			pods := []core.Pod{
				healthPairPod("prefill", 0, 0, ready("prefill", 0), "uid-p0", "8998"),
				healthPairPod("prefill", 1, 0, ready("prefill", 1), "uid-p1", "8999"),
				healthPairPod("decode", 0, 0, ready("decode", 0), "uid-d0", ""),
				healthPairPod("decode", 1, 0, ready("decode", 1), "uid-d1", ""),
			}
			pods = stampHealthGroupTotals(md, pods)
			fetch := &healthPairFetch{prefillHosts: map[string]bool{
				healthPairHost("prefill", 0, 0): true,
				healthPairHost("prefill", 1, 0): true,
			}}

			qualified, qualifications := serviceLabelPods(t, md, pods, nil, fetch.fetch)
			observeModelDeploymentEndpointEligibility(md, qualifications)

			prefillSvc := renderModelDeploymentRoleService(md, &md.Spec.Roles[0], nil, nil, false)
			decodeSvc := renderModelDeploymentRoleService(md, &md.Spec.Roles[1], nil, nil, false)
			deploymentSvc := renderModelDeploymentService(md, qualified)

			assert.Equal(t, tc.wantPrefill, serviceReached(prefillSvc, qualified),
				"the prefill role's Service reaches the members its own role qualified")
			assert.Equal(t, tc.wantDecode, serviceReached(decodeSvc, qualified),
				"the decode role's Service reaches the members its own role qualified")
			assert.Equal(t, serviceReached(prefillSvc, qualified),
				serviceReached(deploymentSvc, qualified),
				"the deployment-wide Service follows the front role alone")
		})
	}
}

// TestRenderModelDeploymentService_NeverReachesAnotherRolesProcess states that the
// deployment-wide Service of a P/D deployment names the front role and reaches no process of the
// other role.
func TestRenderModelDeploymentService_NeverReachesAnotherRolesProcess(t *testing.T) {
	md := healthPairDeployment(workercore.ModelDeploymentEngineSGLang, 1, 1)
	pods := []core.Pod{
		healthPairPod("prefill", 0, 0, healthBool(true), "uid-p0", "8998"),
		healthPairPod("decode", 0, 0, healthBool(true), "uid-d0", ""),
	}
	pods = stampHealthGroupTotals(md, pods)
	fetch := &healthPairFetch{prefillHosts: map[string]bool{healthPairHost("prefill", 0, 0): true}}

	qualified, qualifications := serviceLabelPods(t, md, pods, nil, fetch.fetch)
	observeModelDeploymentEndpointEligibility(md, qualifications)

	front := modelDeploymentFrontRole(md)
	require.Equal(t, "prefill", front.Name, "setup requires the prefill role to be the front")
	deploymentSvc := renderModelDeploymentService(md, qualified)

	assert.Equal(t, front.Name, deploymentSvc.Spec.Selector[modelDeploymentLabelKeyComponent],
		"the deployment-wide Service names the front role")
	assert.Equal(t, []string{"prefill-0-0"}, serviceReached(deploymentSvc, qualified),
		"the deployment-wide Service reaches the front role and nothing else")
}

// TestRenderModelDeploymentRoleService_ExternalDPPublishesEveryRankOfAQualifiedGroup states that
// an External-DP role's Service publishes every rank of a qualified group.
func TestRenderModelDeploymentRoleService_ExternalDPPublishesEveryRankOfAQualifiedGroup(t *testing.T) {
	md := serviceExternalDPDeployment()
	pods := []core.Pod{
		serviceExternalDPMember("server", 0, 0, healthBool(true), "uid-a0"),
		serviceExternalDPMember("server", 0, 1, healthBool(true), "uid-a1"),
		serviceExternalDPMember("server", 1, 0, healthBool(true), "uid-b0"),
		serviceExternalDPMember("server", 1, 1, healthBool(true), "uid-b1"),
	}
	pods = stampHealthGroupTotals(md, pods)

	qualified, qualifications := serviceLabelPods(t, md, pods, nil)
	observeModelDeploymentEndpointEligibility(md, qualifications)

	svc := renderModelDeploymentRoleService(md, &md.Spec.Roles[0], nil, nil, false)

	assert.Equal(t, modelDeploymentEndpointEligibleValue,
		svc.Spec.Selector[modelDeploymentLabelKeyEndpointEligible],
		"the Service names the endpoints the operator qualified")
	assert.NotContains(t, svc.Spec.Selector, modelDeploymentMemberIndexLabel,
		"an External-DP role publishes every rank and not only its leader")
	assert.Equal(t, []string{"server-0-0", "server-0-1", "server-1-0", "server-1-1"},
		serviceReached(svc, qualified),
		"every rank of every qualified group is published")
}

// TestRenderModelDeploymentRoleService_AnOrdinaryGroupKeepsLeaderOnlySelection states that a role
// this operator serves with a leader keeps its leader-only selection.
func TestRenderModelDeploymentRoleService_AnOrdinaryGroupKeepsLeaderOnlySelection(t *testing.T) {
	md := newRenderDeployment(func(md *workercore.ModelDeployment) {
		md.Spec.Roles[0].ReplicaSize = 2
		md.Spec.Roles[0].Replicas = 2
	})
	pods := []core.Pod{
		serviceLeaderMember("server", 0, 0, healthBool(true), "uid-a0"),
		serviceLeaderMember("server", 0, 1, healthBool(true), "uid-a1"),
		serviceLeaderMember("server", 1, 0, healthBool(true), "uid-b0"),
		serviceLeaderMember("server", 1, 1, healthBool(true), "uid-b1"),
	}
	pods = stampHealthGroupTotals(md, pods)

	qualified, qualifications := serviceLabelPods(t, md, pods, nil)
	observeModelDeploymentEndpointEligibility(md, qualifications)

	svc := renderModelDeploymentRoleService(md, &md.Spec.Roles[0], nil, nil, false)

	assert.Equal(t, modelDeploymentEndpointEligibleValue,
		svc.Spec.Selector[modelDeploymentLabelKeyEndpointEligible],
		"the Service names the endpoints the operator qualified")
	assert.Equal(t, strconv.Itoa(modelDeploymentLeaderMemberIndex),
		svc.Spec.Selector[modelDeploymentMemberIndexLabel],
		"an ordinary role keeps its leader-only selection")
	assert.Equal(t, []string{"server-0-0", "server-1-0"}, serviceReached(svc, qualified),
		"only the leader of every group answers")
}

// serviceConvergeServices runs the production Service convergence and returns the Services it
// wrote, by name.
//
// THE PRODUCTION PATH IS USED because the term a previous pass narrowed onto a live Service is
// cluster state a from-scratch render does not carry, and the retention of that term across a
// quiet observation is what this file is about.
func serviceConvergeServices(
	t *testing.T, md *workercore.ModelDeployment, pods []core.Pod,
	qualifications []modelDeploymentInstanceQualification, cli ctrlcli.Client,
) map[string]*core.Service {
	t.Helper()

	reconciler := &ModelDeploymentReconciler{
		Client:    cli,
		APIReader: cli,
		Recorder:  ctrlrecord.NewFakeRecorder(16),
	}
	require.NoError(t, reconciler.syncModelDeploymentService(context.Background(), md,
		modelDeploymentQualificationsDecided(qualifications), pods))

	services := map[string]*core.Service{}
	for _, name := range []string{
		md.Name, md.Name + "-prefill", md.Name + "-decode",
		md.Name + "-" + md.Spec.Roles[0].Name,
	} {
		service := &core.Service{}
		if err := cli.Get(context.Background(), ctrlcli.ObjectKey{Namespace: md.Namespace, Name: name},
			service); err == nil {
			services[name] = service
		}
	}

	return services
}

// TestRenderModelDeploymentRoleService_AnUnsafeMemberWithdrawsItsWholeGroup states that one
// member that is not safe takes every rank of its group out of the Service while every rank of
// the healthy group stays.
//
// THE SERVICES ARE CONVERGED RATHER THAN RENDERED, because a term that was narrowed onto a live
// Service has to survive a quiet observation: the deployment-wide condition is Unknown while a
// group is held, and a term dropped then would hand the old routing back the group this operator
// withdrew.
func TestRenderModelDeploymentRoleService_AnUnsafeMemberWithdrawsItsWholeGroup(t *testing.T) {
	testCases := []struct {
		name string
		// absent says the member of the second group is gone rather than unsafe.
		absent bool
		// unsafe says how the member of the second group reports itself.
		unsafe func(pod *core.Pod)
	}{
		{
			name: "a member that reports it is not ready",
			unsafe: func(pod *core.Pod) {
				pod.Status.Conditions = []core.PodCondition{
					{Type: core.PodReady, Status: core.ConditionFalse},
				}
			},
		},
		{
			name: "a member that reports nothing about its readiness",
			unsafe: func(pod *core.Pod) {
				pod.Status.Conditions = []core.PodCondition{
					{Type: core.PodReady, Status: core.ConditionUnknown},
				}
			},
		},
		{
			name:   "a member that is missing",
			absent: true,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			md := serviceExternalDPDeployment()
			md.UID = "uid-qwen"
			members := []core.Pod{
				serviceExternalDPMember("server", 0, 0, healthBool(true), "uid-a0"),
				serviceExternalDPMember("server", 0, 1, healthBool(true), "uid-a1"),
				serviceExternalDPMember("server", 1, 0, healthBool(true), "uid-b0"),
				serviceExternalDPMember("server", 1, 1, healthBool(true), "uid-b1"),
			}
			for i := range members {
				kubemeta.ControlOnWithoutBlock(&members[i], md,
					workercore.SchemeGroupVersionKind("ModelDeployment"))
			}

			cli := newModelDeploymentClient(md, newRenderInstanceType())

			// the first pass: every group is healthy, so the Services narrow onto the members
			// this operator qualified.
			qualified, qualifications := serviceLabelPods(t, md, stampHealthGroupTotals(md, members), nil)
			observeModelDeploymentEndpointEligibility(md, qualifications)
			services := serviceConvergeServices(t, md, qualified, qualifications, cli)
			require.NotNil(t, services[md.Name+"-"+md.Spec.Roles[0].Name], "setup requires the role Service")
			assert.Equal(t, []string{"server-0-0", "server-0-1", "server-1-0", "server-1-1"},
				serviceReached(services[md.Name+"-"+md.Spec.Roles[0].Name], qualified),
				"the first pass publishes every rank of every healthy group")

			// the second pass: one member of the second group is no longer safe.
			faulted := make([]core.Pod, len(qualified))
			copy(faulted, qualified)
			if tc.absent {
				faulted = append(faulted[:3], faulted[4:]...)
			} else {
				tc.unsafe(&faulted[2])
			}
			qualified, qualifications = serviceLabelPods(t, md, faulted, nil)
			observeModelDeploymentEndpointEligibility(md, qualifications)
			services = serviceConvergeServices(t, md, qualified, qualifications, cli)

			service := services[md.Name+"-"+md.Spec.Roles[0].Name]
			require.NotNil(t, service, "setup requires the role Service")
			assert.Equal(t, modelDeploymentEndpointEligibleValue,
				service.Spec.Selector[modelDeploymentLabelKeyEndpointEligible],
				"the Service keeps naming the endpoints this operator qualified")
			assert.Equal(t, []string{"server-0-0", "server-0-1"},
				serviceReached(service, qualified),
				"every rank of the healthy group stays selected and no rank of the unsafe group does")
		})
	}
}

// TestRenderModelDeploymentRoleService_ATakeOverKeepsTheNarrowedSelector states that a role that
// takes its command line over while a managed member of it still stands keeps the Services
// narrowed.
//
// THE PAIR CANNOT BE ASKED ANY MORE ONCE A ROLE TOOK ITS COMMAND LINE OVER, so the
// deployment-wide condition is Unknown while the qualified groups keep the eligibility they have.
// The Services then select by membership, and the group qualification has to reach that answer:
// an unsafe member has to stay out, a healthy carried member has to stay in, and a member the
// take-over brought and nothing labeled has to stay out too.
func TestRenderModelDeploymentRoleService_ATakeOverKeepsTheNarrowedSelector(t *testing.T) {
	md := healthPairDeployment(workercore.ModelDeploymentEngineSGLang, 1, 1)
	md.UID = "uid-qwen"
	md.Spec.Roles[1].Replicas = 2
	members := []core.Pod{
		healthPairPod("prefill", 0, 0, healthBool(true), "uid-p0", "8998"),
		healthPairPod("decode", 0, 0, serviceReadyUnknown, "uid-d0", ""),
		healthPairPod("decode", 1, 0, healthBool(true), "uid-d1", ""),
	}
	for i := range members {
		members[i].Spec.Containers = []core.Container{{Name: modelDeploymentMainContainerName}}
		kubemeta.ControlOnWithoutBlock(&members[i], md,
			workercore.SchemeGroupVersionKind("ModelDeployment"))
	}
	pods := stampHealthGroupTotals(md, members)
	fetch := &healthPairFetch{prefillHosts: map[string]bool{healthPairHost("prefill", 0, 0): true}}

	cli := newModelDeploymentClient(md, newRenderInstanceType())

	// the first pass: the pair is verified, so the healthy groups qualify and the Services
	// narrow onto them.
	qualified, qualifications := serviceLabelPods(t, md, pods, nil, fetch.fetch)
	observeModelDeploymentEndpointEligibility(md, qualifications)
	services := serviceConvergeServices(t, md, qualified, qualifications, cli)
	for _, name := range []string{md.Name, md.Name + "-prefill", md.Name + "-decode"} {
		require.NotNil(t, services[name], "setup requires the %s Service", name)
		assert.Equal(t, modelDeploymentEndpointEligibleValue,
			services[name].Spec.Selector[modelDeploymentLabelKeyEndpointEligible],
			"the %s Service narrows onto the qualified members", name)
	}
	assert.Equal(t, []string{"decode-1-0"},
		serviceReached(services[md.Name+"-decode"], qualified),
		"the unsafe decode group is out of the decode role's Service")

	// the prefill role takes its command line over while its managed member still stands, and
	// the member the take-over brought carries no label.
	md.Spec.Roles[0].Command = []string{"custom-server"}
	takeover := healthPairPod("prefill", 1, 0, healthBool(true), "uid-p1", "")
	takeover.Spec.Containers = []core.Container{{Name: modelDeploymentMainContainerName}}
	kubemeta.ControlOnWithoutBlock(&takeover, md,
		workercore.SchemeGroupVersionKind("ModelDeployment"))

	// the second pass reads the live selectors the first pass left behind, the way the writer
	// reads them, and the pair cannot be asked any more: nothing is decided, and the qualified
	// groups keep the eligibility they have.
	liveSelectors := map[string]map[string]string{}
	for _, role := range md.Spec.Roles {
		service := services[md.Name+"-"+role.Name]
		require.NotNil(t, service, "setup requires the %s Service", role.Name)
		liveSelectors[role.Name] = service.Spec.Selector
	}
	carried := append(append([]core.Pod{}, qualified...), takeover)
	qualified, qualifications = serviceLabelPods(t, md, stampHealthGroupTotals(md, carried),
		liveSelectors, fetch.fetch)
	observeModelDeploymentEndpointEligibility(md, qualifications)
	require.False(t, modelDeploymentEligibilityDecided(md),
		"the pair cannot be asked, so the predicate is not decided")
	services = serviceConvergeServices(t, md, qualified, qualifications, cli)

	// THE REQUIREMENT STAYS IN FORCE. The take-over did not decide the predicate, so a pass that
	// re-read the shape alone would let the requirement lapse and the Service would go back to
	// naming whatever it finds. The eligibility is carried by the membership of the answering
	// members here, so the requirement shows up as the note that says it is active.
	assert.Equal(t, modelDeploymentEndpointEligibleValue,
		services[md.Name+"-prefill"].Annotations[modelDeploymentEligibilityActiveAnnotation],
		"the prefill role's Service keeps the requirement in force through the take-over")
	assert.Equal(t, modelDeploymentEndpointEligibleValue,
		services[md.Name].Annotations[modelDeploymentEligibilityActiveAnnotation],
		"and so does the deployment-wide Service")
	// THE DECODE ROLE'S SERVICE KEEPS THE TERM, because the retention reads it back from the live
	// object: nothing has decided the predicate, so the term the first pass published is the one
	// that holds.
	assert.Equal(t, modelDeploymentEndpointEligibleValue,
		services[md.Name+"-decode"].Spec.Selector[modelDeploymentLabelKeyEndpointEligible],
		"the decode role's Service keeps the term the first pass published")

	assert.Equal(t, []string{"prefill-0-0"},
		serviceReached(services[md.Name+"-prefill"], qualified),
		"the prefill role's Service keeps serving the managed member and not the take-over member")
	assert.Equal(t, []string{"decode-1-0"},
		serviceReached(services[md.Name+"-decode"], qualified),
		"the decode role's Service keeps serving the healthy group and not the unsafe one")
	assert.Equal(t, []string{"prefill-0-0"},
		serviceReached(services[md.Name], qualified),
		"the deployment-wide Service keeps serving the front role alone")
}
