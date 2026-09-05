package worker

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	core "k8s.io/api/core/v1"
	ctrlcli "sigs.k8s.io/controller-runtime/pkg/client"
	kueuectrlconst "sigs.k8s.io/kueue/pkg/controller/constants"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	"gpustack.ai/gpustack/pkg/systemmeta"
)

func getModelDeploymentService(t *testing.T, cli ctrlcli.Client) *core.Service {
	t.Helper()

	svc := new(core.Service)
	require.NoError(t, cli.Get(context.Background(),
		ctrlcli.ObjectKey{Namespace: "team-a", Name: "qwen"}, svc))

	return svc
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

	svcList := new(core.ServiceList)
	require.NoError(t, cli.List(context.Background(), svcList, ctrlcli.InNamespace("team-a")))
	require.Len(t, svcList.Items, 1, "four replicas are fronted by ONE Service, not one each")

	svc := &svcList.Items[0]
	assert.Equal(t, core.ServiceTypeClusterIP, svc.Spec.Type)
	assert.Equal(t, "qwen", svc.Name)
	assert.True(t, systemmeta.MatchResource(svc, ModelDeploymentResourceType))
	assert.True(t, modelDeploymentOwns(svc, getModelDeployment(t, cli)))
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

	*writes = modelDeploymentWrites{}
	_, err = reconcileModelDeployment(t, cli)
	require.NoError(t, err)

	require.Len(t, replicaNames(t, cli), 2, "the replicas moved")
	after := getModelDeploymentService(t, cli)
	assert.Equal(t, before.UID, after.UID)
	assert.Equal(t, before.ResourceVersion, after.ResourceVersion,
		"and the Service was not touched at all")
	assert.Zero(t, writes.creates, "no Service was recreated")
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
