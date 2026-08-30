package kvcache

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apps "k8s.io/api/apps/v1"
	core "k8s.io/api/core/v1"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	"gpustack.ai/gpustack/pkg/worker/kuberess"
)

// testBackend is the canonical managed backend, as admission leaves it. Every case starts from this
// and mutates the one field it is about, so a case says what it changes and nothing else.
func testBackend(mutate ...func(*workercore.KVCacheBackend)) *workercore.KVCacheBackend {
	kvcb := &workercore.KVCacheBackend{
		ObjectMeta: meta.ObjectMeta{
			Name: "mooncake-dram",
			UID:  "1c1b3a0e-0000-4000-8000-000000000001",
		},
		Spec: workercore.KVCacheBackendSpec{
			Type: "Mooncake",
			Connection: workercore.KVCacheBackendConnection{
				Managed: &workercore.KVCacheBackendManaged{
					Leader: workercore.KVCacheBackendLeader{
						Replicas:           ptr.To[int32](1),
						AllocationStrategy: "FreeRatioFirst",
					},
					Members: []workercore.KVCacheBackendMember{
						{Medium: "DRAM"},
					},
				},
			},
		},
	}
	for _, m := range mutate {
		m(kvcb)
	}
	return kvcb
}

// leaderContainer returns the one container the Deployment runs, failing the test rather than
// panicking when the render produced a shape no assertion below would make sense against.
func leaderContainer(t *testing.T, kvcb *workercore.KVCacheBackend, image string) core.Container {
	t.Helper()
	deploy := RenderLeaderDeployment(kvcb, image)
	require.Len(t, deploy.Spec.Template.Spec.Containers, 1,
		"the leader runs exactly one container")
	return deploy.Spec.Template.Spec.Containers[0]
}

// TestLeaderWorkload_Shape pins the parts of the Deployment that are not about a single
// field: where it lands, how many of it there are, and what it runs.
func TestLeaderWorkload_Shape(t *testing.T) {
	deploy := RenderLeaderDeployment(testBackend(), "mooncake:v0.3.13")

	assert.Equal(t, "mooncake-dram-leader", deploy.Name)
	assert.Equal(t, kuberess.SystemNamespaceName, deploy.Namespace,
		"the leader runs in the operator's own namespace, not in one derived from a cluster-scoped object")
	require.NotNil(t, deploy.Spec.Replicas)
	assert.Equal(t, int32(1), *deploy.Spec.Replicas)

	container := leaderContainer(t, testBackend(), "mooncake:v0.3.13")
	assert.Equal(t, "mooncake:v0.3.13", container.Image)
	assert.Equal(t, []string{"mooncake_master"}, container.Command,
		"the entrypoint is named rather than inherited, so the argv is what kubectl shows")
	assert.Equal(t, RenderLeaderFlags(testBackend().Spec.Connection.Managed.Leader), container.Args,
		"the args are T4's renderer verbatim; a second flag source is what makes them ambiguous")
}

// TestLeaderWorkload_ImageIsWhatItIsGiven asserts the renderer does not resolve the image
// itself. Whether the value came from spec.image or from the Setting is the caller's decision, and a
// renderer that reached for a default would make the fallback untestable from here.
func TestLeaderWorkload_ImageIsWhatItIsGiven(t *testing.T) {
	kvcb := testBackend(func(k *workercore.KVCacheBackend) {
		k.Spec.Image = "from-the-spec:v1"
	})

	container := leaderContainer(t, kvcb, "from-the-setting:v2")
	assert.Equal(t, "from-the-setting:v2", container.Image,
		"the renderer publishes the image it was handed, having no opinion about where it came from")
}

// TestLeaderWorkload_NeverSurgesASecondMaster is the other half of "one replica, and only one".
//
// The replica count alone does not say it. A Deployment naming no strategy gets RollingUpdate, whose
// maxSurge defaults to 25% and ROUNDS UP — to one, against one desired replica — so every image or
// flag change would start the new master before stopping the old one and run two at once. That is
// the split brain the replica count exists to prevent, and it would happen on every update.
func TestLeaderWorkload_NeverSurgesASecondMaster(t *testing.T) {
	deploy := RenderLeaderDeployment(testBackend(), "mooncake:v0.3.13")

	require.NotNil(t, deploy.Spec.Replicas)
	assert.Equal(t, int32(1), *deploy.Spec.Replicas)
	assert.Equal(t, apps.RecreateDeploymentStrategyType, deploy.Spec.Strategy.Type,
		"an unset strategy is RollingUpdate, which surges a second master past a single replica")
	assert.Nil(t, deploy.Spec.Strategy.RollingUpdate,
		"and the rolling fields go with the type, or they describe a strategy not in use")
}

// TestLeaderWorkload_PullPolicyAndSecrets covers the two fields that decide whether the image can be
// fetched at all.
//
// Neither is inherited from the cluster-wide "image-pull-policy" / "image-pull-secrets" Settings:
// those are values of the bundled-application chart install and reach nothing a controller renders.
// So without these fields no role of this backend could ever run an image from a private registry —
// there is no service account of ours carrying credentials either.
func TestLeaderWorkload_PullPolicyAndSecrets(t *testing.T) {
	t.Run("unset resolves to the default the tag implies", func(t *testing.T) {
		deploy := RenderLeaderDeployment(testBackend(), "mooncake:v1")

		assert.Equal(t, core.PullIfNotPresent, deploy.Spec.Template.Spec.Containers[0].ImagePullPolicy,
			"resolved here rather than left empty: an empty policy is filled in by the API server, "+
				"and an aligner cannot both converge that default and correct a stale value")
		assert.Empty(t, deploy.Spec.Template.Spec.ImagePullSecrets)
	})

	t.Run("both reach the rendered pod", func(t *testing.T) {
		kvcb := testBackend(func(k *workercore.KVCacheBackend) {
			k.Spec.ImagePullPolicy = core.PullAlways
			k.Spec.ImagePullSecrets = []core.LocalObjectReference{{Name: "registry-creds"}}
		})

		deploy := RenderLeaderDeployment(kvcb, "private.example.com/mooncake:v1")

		assert.Equal(t, core.PullAlways, deploy.Spec.Template.Spec.Containers[0].ImagePullPolicy)
		assert.Equal(t, []core.LocalObjectReference{{Name: "registry-creds"}},
			deploy.Spec.Template.Spec.ImagePullSecrets)
	})
}

// TestLeaderWorkload_Probes is the case the F6 correction exists for.
//
// /health is in the master's UNGATED route class: it answers 200 in every state, including before
// the service plane is up. An httpGet readinessProbe pointed at it is not a weak probe, it is an
// inert one — the Pod goes Ready as soon as the HTTP server binds, and the Service publishes an
// endpoint that refuses RPC. So the two probes take different paths, and this test asserts the
// difference rather than each path in isolation: a render that pointed both at /health would satisfy
// any per-probe assertion and still be the bug.
func TestLeaderWorkload_Probes(t *testing.T) {
	container := leaderContainer(t, testBackend(), "mooncake:v0.3.13")

	require.NotNil(t, container.ReadinessProbe)
	require.NotNil(t, container.ReadinessProbe.HTTPGet)
	require.NotNil(t, container.LivenessProbe)
	require.NotNil(t, container.LivenessProbe.HTTPGet)

	readiness, liveness := container.ReadinessProbe.HTTPGet, container.LivenessProbe.HTTPGet

	assert.Equal(t, "/get_all_segments", readiness.Path,
		"readiness asks a gated route, which 503s until the service plane is up")
	assert.Equal(t, "/health", liveness.Path,
		"liveness asks an ungated route, which answers while the process answers at all")
	assert.NotEqual(t, readiness.Path, liveness.Path,
		"the two probes ask different questions; one path for both makes readiness inert")

	assert.Equal(t, int32(LeaderMetricsPort), readiness.Port.IntVal,
		"both probes go to the admin port, which is where every HTTP route lives")
	assert.Equal(t, int32(LeaderMetricsPort), liveness.Port.IntVal)

	assert.Greater(t, liveness.Port.IntVal, int32(0))
	assert.Greater(t, container.LivenessProbe.FailureThreshold, container.ReadinessProbe.FailureThreshold,
		"liveness tolerates more failures than readiness: withholding traffic is cheap, killing a "+
			"master that is merely slow to activate is not")
}

// TestLeaderWorkload_PodIdentityEnv pins that the argv's $(VAR) references resolve. T4
// renders -pod_name=$(KUBERNETES_POD_NAME); without the matching downward-API entry here the flag
// reaches the process as the literal string, and the master would take that for a pod name.
func TestLeaderWorkload_PodIdentityEnv(t *testing.T) {
	container := leaderContainer(t, testBackend(), "mooncake:v0.3.13")

	byName := make(map[string]core.EnvVar, len(container.Env))
	for _, env := range container.Env {
		byName[env.Name] = env
	}

	for name, field := range map[string]string{
		LeaderPodNameEnv:      "metadata.name",
		LeaderPodNamespaceEnv: "metadata.namespace",
	} {
		env, ok := byName[name]
		require.True(t, ok, "%s must be defined; the rendered argv refers to it", name)
		require.NotNil(t, env.ValueFrom, "%s comes from the downward API, not a literal", name)
		require.NotNil(t, env.ValueFrom.FieldRef)
		assert.Equal(t, field, env.ValueFrom.FieldRef.FieldPath)
	}
}

// TestLeaderWorkload_ClaimsNoHost pins the absences. The leader is a metadata service: it
// holds no cache bytes, so it needs neither the host's network namespace nor any device. The member
// is the side that needs those (F9), and a leader that acquired them would be a privilege nobody
// asked for.
func TestLeaderWorkload_ClaimsNoHost(t *testing.T) {
	deploy := RenderLeaderDeployment(testBackend(), "mooncake:v0.3.13")
	podSpec := deploy.Spec.Template.Spec

	assert.False(t, podSpec.HostNetwork, "the leader is not hostNetwork")
	assert.Empty(t, podSpec.Volumes, "the leader mounts nothing")

	container := podSpec.Containers[0]
	assert.Empty(t, container.VolumeMounts)
	if container.SecurityContext != nil {
		assert.NotEqual(t, ptr.To(true), container.SecurityContext.Privileged,
			"never privileged")
	}
}

// TestLeaderWorkload_SelectorSurvivesASpecChange is about an immutable field.
//
// A Deployment's spec.selector cannot be changed after creation: an update carrying a different one
// is rejected by the API server, and the object is then stuck until somebody deletes it by hand. So
// the selector must be built only from things that do not change over a backend's life. This test
// mutates every spec field the webhook permits an update to and asserts the selector does not move.
func TestLeaderWorkload_SelectorSurvivesASpecChange(t *testing.T) {
	before := RenderLeaderDeployment(testBackend(), "mooncake:v0.3.13")

	after := RenderLeaderDeployment(testBackend(func(k *workercore.KVCacheBackend) {
		k.Spec.Image = "mooncake:v0.4.0"
		k.Spec.Transport.Protocol = "RDMA"
		leader := &k.Spec.Connection.Managed.Leader
		leader.AllocationStrategy = "Random"
		leader.ExtraArgs = map[string]string{"client_ttl": "30"}
	}), "mooncake:v0.4.0")

	require.NotNil(t, before.Spec.Selector)
	require.NotNil(t, after.Spec.Selector)
	assert.Equal(t, before.Spec.Selector, after.Spec.Selector,
		"the selector is immutable in the API, so it may not carry anything a spec update can move")

	assert.NotEqual(t, before.Spec.Template.Spec.Containers[0].Image,
		after.Spec.Template.Spec.Containers[0].Image,
		"the mutation did reach the template — otherwise the assertion above proves nothing")

	for key, value := range before.Spec.Selector.MatchLabels {
		assert.Equal(t, value, before.Spec.Template.Labels[key],
			"the template must carry every selector label, or the Deployment matches no Pod")
	}
}

// TestLeaderWorkload_IsDeterministic pins that one backend renders identically every time.
// The reconciler converges this object on every pass, so a render that wandered — a map range in the
// argv, say — would rewrite the Deployment forever and restart the master with it.
func TestLeaderWorkload_IsDeterministic(t *testing.T) {
	kvcb := testBackend(func(k *workercore.KVCacheBackend) {
		k.Spec.Connection.Managed.Leader.ExtraArgs = map[string]string{
			"a": "1", "b": "2", "c": "3", "d": "4", "e": "5",
		}
	})

	first := RenderLeaderDeployment(kvcb, "mooncake:v0.3.13")
	for range 20 {
		assert.Equal(t, first, RenderLeaderDeployment(kvcb, "mooncake:v0.3.13"))
	}
}

// TestLeaderWorkload_IsOwnedByTheBackend asserts the garbage-collection path. The rendered
// objects are namespaced dependents of a cluster-scoped owner, which is the direction that works.
func TestLeaderWorkload_IsOwnedByTheBackend(t *testing.T) {
	kvcb := testBackend()

	for name, obj := range map[string]meta.Object{
		"deployment": RenderLeaderDeployment(kvcb, "mooncake:v0.3.13"),
		"service":    RenderLeaderService(kvcb),
	} {
		refs := obj.GetOwnerReferences()
		require.Len(t, refs, 1, "%s carries exactly one owner", name)
		assert.Equal(t, "KVCacheBackend", refs[0].Kind, "%s", name)
		assert.Equal(t, kvcb.Name, refs[0].Name, "%s", name)
		assert.Equal(t, kvcb.UID, refs[0].UID, "%s", name)
	}
}

func TestLeaderWorkload_Service(t *testing.T) {
	kvcb := testBackend()
	svc := RenderLeaderService(kvcb)
	deploy := RenderLeaderDeployment(kvcb, "mooncake:v0.3.13")

	assert.Equal(t, "mooncake-dram-leader", svc.Name)
	assert.Equal(t, kuberess.SystemNamespaceName, svc.Namespace)
	assert.Equal(t, core.ServiceTypeClusterIP, svc.Spec.Type,
		"the leader is reached from inside the cluster; nothing here publishes it outward")

	assert.Equal(t, deploy.Spec.Selector.MatchLabels, svc.Spec.Selector,
		"the Service selects exactly what the Deployment does, or it fronts nothing")

	ports := make(map[string]int32, len(svc.Spec.Ports))
	for _, p := range svc.Spec.Ports {
		ports[p.Name] = p.Port
	}
	assert.Equal(t, map[string]int32{
		"rpc":   LeaderRPCPort,
		"admin": LeaderMetricsPort,
	}, ports, "both ports are published: one is what engines connect to, one is what this operator reads")
}

// TestLeaderWorkload_Endpoints pins what status.endpoints publishes for a managed backend: a Client address
// an engine dials and an Admin address this operator scrapes. They are the same host on two ports,
// and the roles are named because a consumer handed the wrong one fails at connect time with nothing
// to point at.
func TestLeaderWorkload_Endpoints(t *testing.T) {
	got := LeaderEndpoints(testBackend())

	byName := make(map[string]string, len(got))
	for _, e := range got {
		byName[e.Name] = e.Address
	}

	host := "mooncake-dram-leader." + kuberess.SystemNamespaceName + ".svc"
	assert.Equal(t, map[string]string{
		workercore.KVCacheBackendEndpointNameClient: host + ":50051",
		workercore.KVCacheBackendEndpointNameAdmin:  host + ":9003",
	}, byName)
}
