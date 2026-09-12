package worker

import (
	"context"
	"slices"
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

// TestModelDeploymentService_OnePerRoleBesideTheDeploymentWide is T7's shape.
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
	// request served by another team's model, not an error.
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

// TestModelDeploymentService_SurvivesTheGroupRebuild pins the one interaction between T4 and T7.
//
// A replicas change deletes every Pod of the group and creates none until they are gone. A Service
// rebuilt alongside them would drop its allocated ClusterIP, so every client that resolved the name
// would be talking to an address nothing answers on -- for a change that was only ever about how
// many replicas there are.
func TestModelDeploymentService_SurvivesTheGroupRebuild(t *testing.T) {
	cli := newModelDeploymentClient(twoRoleDeployment(), newRenderInstanceType())

	_, err := reconcileModelDeployment(t, cli)
	require.NoError(t, err)
	before := getModelDeploymentService(t, cli)

	grown := getModelDeployment(t, cli)
	grown.Spec.Roles[0].Replicas = 3
	require.NoError(t, cli.Update(context.Background(), grown))

	// The rebuild pass, the one that leaves the deployment with no replicas at all.
	_, err = reconcileModelDeployment(t, cli)
	require.NoError(t, err)
	require.Empty(t, replicaNames(t, cli))

	assert.Equal(t, []string{"qwen", "qwen-decode", "qwen-prefill"}, serviceNames(t, cli),
		"a deployment with no replicas still has its addresses")
	after := getModelDeploymentService(t, cli)
	assert.Equal(t, before.UID, after.UID)
	assert.Equal(t, before.ResourceVersion, after.ResourceVersion)
}

// TestRenderModelDeploymentService_SelectsExactlyTheRolesPods is the property that decides whether
// traffic reaches anything: the selector has to match what the renderer actually stamps on a
// replica, and must not carry the entrance label, which a spec update can move.
func TestRenderModelDeploymentService_SelectsExactlyTheRolesPods(t *testing.T) {
	md := newRenderDeployment()
	svc := renderModelDeploymentService(md)
	pod := renderOne(t, md, newRenderInstanceType())

	require.NotEmpty(t, svc.Spec.Selector)
	for k, v := range svc.Spec.Selector {
		assert.Equal(t, v, pod.Labels[k], "the replica must carry selector label %s", k)
	}
	assert.NotContains(t, svc.Spec.Selector, kueuectrlconst.QueueLabel,
		"a selector that followed the InstanceType would orphan every replica already running")
}

// TestRenderModelDeploymentService_Port covers both readings of F9's rule, including that the
// Service and the container behind it can never name different ports — they read one render.
func TestRenderModelDeploymentService_Port(t *testing.T) {
	testCases := []struct {
		name     string
		ports    []workercore.InstancePort
		wantPort int32
		wantName string
	}{
		{name: "no port declared falls back to the engines' default", wantPort: 8000, wantName: "http"},
		{
			name:     "a declared port is used as it stands",
			ports:    []workercore.InstancePort{{Port: 9000, Protocol: core.ProtocolTCP}},
			wantPort: 9000,
		},
		{
			name: "the FIRST declared port is the one the deployment is reached on",
			ports: []workercore.InstancePort{
				{Port: 9000, Protocol: core.ProtocolTCP},
				{Port: 9001, Protocol: core.ProtocolTCP},
			},
			wantPort: 9000,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			md := newRenderDeployment(func(md *workercore.ModelDeployment) {
				md.Spec.Roles[0].Template.Ports = tc.ports
			})

			svc := renderModelDeploymentService(md)
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

// TestModelDeploymentEndpoint pins the address a user is handed. It is derived from the Service's
// name and namespace rather than read back off the object, because a ClusterIP is neither what
// callers use nor stable across a recreate.
func TestModelDeploymentEndpoint(t *testing.T) {
	assert.Equal(t, "http://qwen.team-a.svc:8000", modelDeploymentEndpoint(newRenderDeployment()))

	md := newRenderDeployment(func(md *workercore.ModelDeployment) {
		md.Spec.Roles[0].Template.Ports = []workercore.InstancePort{{Port: 9000, Protocol: core.ProtocolTCP}}
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
			// Moving WHERE it listens withdraws the gate without touching the transport, which is
			// the other half of the pair above.
			name:      "moving the port alone leaves the transport as it was",
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
				md.Spec.Roles[0].Template.Command = tc.command
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
			rendered++
		}
	}

	require.NotEmpty(t, operatorArgs, "a check over an empty list is vacuously true")
	// The count is stated so that a renderer which stopped emitting arguments, or an engine dropped
	// from Engines(), shows up as a smaller denominator rather than as a check that still passes.
	require.Equal(t, 7, rendered,
		"three engines, two of them with three roles and sglang with one; update this figure "+
			"deliberately when that changes")

	scheme, gradable := modelDeploymentEngineTransport(operatorArgs)
	assert.Equal(t, core.URISchemeHTTP, scheme,
		"an operator-supplied TLS flag would move the transport where the published address "+
			"cannot see it")
	assert.True(t, gradable,
		"an operator-supplied listen flag would move the engine where the published address "+
			"cannot see it")
}

// TestModelDeploymentReconciler_ScalingDoesNotRecreateTheService is F9's third acceptance. The
// Service holds an allocated ClusterIP that every client which resolved the name is still using, so
// a scale must move its endpoints and leave the object alone.
func TestModelDeploymentReconciler_ScalingDoesNotRecreateTheService(t *testing.T) {
	md := newRenderDeployment(func(md *workercore.ModelDeployment) { md.Spec.Roles[0].Replicas = 4 })
	writes := new(modelDeploymentWrites)
	cli := newCountingModelDeploymentClient(writes, md, newRenderInstanceType())

	_, err := reconcileModelDeployment(t, cli)
	require.NoError(t, err)
	before := getModelDeploymentService(t, cli)

	scaled := getModelDeployment(t, cli)
	scaled.Spec.Roles[0].Replicas = 2
	require.NoError(t, cli.Update(context.Background(), scaled))

	// A replicas change rebuilds the group, so the scale takes two passes: the first deletes every
	// Pod, the second creates the new set. The Service must survive BOTH -- the pass that leaves the
	// deployment with no replicas at all is the one most likely to decide it has nothing to front.
	*writes = modelDeploymentWrites{}
	_, err = reconcileModelDeployment(t, cli)
	require.NoError(t, err)

	require.Empty(t, replicaNames(t, cli), "the rebuild pass creates nothing")
	assert.Zero(t, writes.creates, "and it recreates no Service either")

	*writes = modelDeploymentWrites{}
	_, err = reconcileModelDeployment(t, cli)
	require.NoError(t, err)

	require.Len(t, replicaNames(t, cli), 2, "the replicas moved")
	after := getModelDeploymentService(t, cli)
	assert.Equal(t, before.UID, after.UID)
	assert.Equal(t, before.ResourceVersion, after.ResourceVersion,
		"and the Service was not touched at all")
	assert.Equal(t, 2, writes.creates, "the only creates are the two replicas")
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
			expected := renderModelDeploymentService(md)
			actual := renderModelDeploymentService(md)
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
