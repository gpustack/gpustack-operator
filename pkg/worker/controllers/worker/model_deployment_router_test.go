package worker

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"maps"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	app "k8s.io/api/apps/v1"
	core "k8s.io/api/core/v1"
	rbac "k8s.io/api/rbac/v1"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	labels "k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/utils/ptr"
	ctrlcli "sigs.k8s.io/controller-runtime/pkg/client"
	kueuectrlconst "sigs.k8s.io/kueue/pkg/controller/constants"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	"gpustack.ai/gpustack/pkg/systemmeta"
	"gpustack.ai/gpustack/pkg/worker/kvcache/router"
)

func routedModelDeployment(mutate ...func(*workercore.ModelDeployment)) *workercore.ModelDeployment {
	md := twoRoleDeployment(func(md *workercore.ModelDeployment) {
		md.Spec.Router = &workercore.ModelDeploymentRouter{Name: workercore.ModelDeploymentRouterLLMD}
		md.Spec.Roles[0].Kind = workercore.ModelDeploymentRoleKindPrefill
		md.Spec.Roles[1].Kind = workercore.ModelDeploymentRoleKindDecode
	})
	for _, m := range mutate {
		m(md)
	}

	return md
}

func routerObjectsAsClients(objects ModelDeploymentRouterObjects) []ctrlcli.Object {
	return []ctrlcli.Object{
		objects.Deployment,
		objects.ConfigMap,
		objects.Service,
		objects.ServiceAccount,
		objects.Role,
		objects.RoleBinding,
	}
}

func TestRenderModelDeploymentRouterObjects_OwnedAndDiscoverable(t *testing.T) {
	md := routedModelDeployment()
	objects, err := renderModelDeploymentRouterObjects(context.Background(), md, nil)
	require.NoError(t, err)

	for _, object := range routerObjectsAsClients(objects) {
		assert.True(t, modelDeploymentOwns(object, md), "%T must be owned", object)
		assert.True(t, systemmeta.MatchResource(object, ModelDeploymentResourceType),
			"%T must carry the resource note used by the watch predicate", object)
	}

	assert.IsType(t, &app.Deployment{}, objects.Deployment)
	assert.IsType(t, &core.ConfigMap{}, objects.ConfigMap)
	assert.IsType(t, &core.Service{}, objects.Service)
	assert.IsType(t, &core.ServiceAccount{}, objects.ServiceAccount)
	assert.IsType(t, &rbac.Role{}, objects.Role)
	assert.IsType(t, &rbac.RoleBinding{}, objects.RoleBinding)
	assert.Equal(t, []rbac.PolicyRule{{
		APIGroups: []string{""}, Resources: []string{"pods"}, Verbs: []string{"get", "list", "watch"},
	}}, objects.Role.Rules)
	assert.Equal(t, objects.ServiceAccount.Name, objects.Deployment.Spec.Template.Spec.ServiceAccountName)
	assert.NotContains(t, objects.Service.Spec.Selector, systemmeta.ResourceTypeLabel,
		"the ownership label belongs to the Service, not its backend selector")
	assert.Contains(t, objects.ConfigMap.Data[modelDeploymentRouterSelectorKey],
		modelDeploymentLabelKeyInstance+"=qwen")
	assert.Contains(t, objects.ConfigMap.Data[modelDeploymentRouterConfigKey],
		modelDeploymentLabelKeyRoleKind)
}

// TestRenderModelDeploymentRouterObjects_BothSelectorsSelectOnlyThePodsThatAnswer states the set
// every discovery path names: the members that answer the API, one per replica, at every role size.
//
// THE CASES ARE THE TWO SHAPES A CONJUNCTION CANNOT TELL APART, which is why the table holds both:
// server roles of different sizes share one discovery filter because the pickers filter by kind,
// and a prefill/decode pair mixes a multi-Member role with a single-Member one. Each case renders
// every member of every role and matches BOTH selectors with the machinery a consumer would use --
// the discovery selector is parsed, and the published one is turned into a selector from its own
// entries -- so what is asserted is the set a router would actually reach, not a restatement of the
// renderer's own strings.
//
// THE SINGLE-MEMBER ROWS FAIL OPEN BY DESIGN. An implementation that writes the member index only
// where a role has several members passes every other assertion in this suite: the multi-Member
// leaders still match, and only the single-Member Pods -- which carry no index at all -- fall out
// of the set. Naming member zero of a single-Member role here is what turns that silence into a
// failure. The published selector carries the mirror property: without its leader term it names
// every member of a multi-Member role, and the per-role set below is what goes red.
func TestRenderModelDeploymentRouterObjects_BothSelectorsSelectOnlyThePodsThatAnswer(t *testing.T) {
	type roleSpec struct {
		name string
		kind workercore.ModelDeploymentRoleKind
		size int32
	}

	testCases := []struct {
		name  string
		roles []roleSpec
	}{
		{
			name: "two server roles of different sizes",
			roles: []roleSpec{
				{name: "small", kind: workercore.ModelDeploymentRoleKindServer, size: 1},
				{name: "large", kind: workercore.ModelDeploymentRoleKindServer, size: 4},
			},
		},
		{
			name: "a multi-member prefiller beside a single-member decoder",
			roles: []roleSpec{
				{name: "prefill", kind: workercore.ModelDeploymentRoleKindPrefill, size: 2},
				{name: "decode", kind: workercore.ModelDeploymentRoleKindDecode, size: 1},
			},
		},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			md := newRenderDeployment(func(md *workercore.ModelDeployment) {
				md.Spec.Router = &workercore.ModelDeploymentRouter{Name: workercore.ModelDeploymentRouterLLMD}
				md.Spec.Roles[0].Name = tc.roles[0].name
				md.Spec.Roles[0].Kind = tc.roles[0].kind
				md.Spec.Roles[0].ReplicaSize = tc.roles[0].size
				for _, spec := range tc.roles[1:] {
					role := md.Spec.Roles[0]
					role.Name = spec.name
					role.Kind = spec.kind
					role.ReplicaSize = spec.size
					md.Spec.Roles = append(md.Spec.Roles, role)
				}
			})

			objects, err := renderModelDeploymentRouterObjects(context.Background(), md, nil)
			require.NoError(t, err)

			discovery, err := labels.Parse(objects.ConfigMap.Data[modelDeploymentRouterSelectorKey])
			require.NoError(t, err, "the discovery selector must parse with the machinery a router uses")

			published := make(map[string]labels.Selector, len(objects.Contract.Roles))
			for i := range objects.Contract.Roles {
				published[objects.Contract.Roles[i].Name] = labels.SelectorFromSet(objects.Contract.Roles[i].Selector)
			}

			// One member of ordinal zero per declared size, rendered and stamped the way the
			// converger renders them, so the selectors are matched against what would run rather
			// than against labels a test wrote by hand.
			renderMember := func(roleIndex, member int) *core.Pod {
				t.Helper()

				pod, err := renderModelDeploymentPodTemplate(context.Background(), ModelDeploymentRenderInput{
					Deployment: md, Role: &md.Spec.Roles[roleIndex], InstanceType: newRenderInstanceType(),
				})
				require.NoError(t, err)
				stampModelDeploymentPod(pod, md, &md.Spec.Roles[roleIndex], 0, member)

				return pod
			}

			var wantDiscovered, gotDiscovered []string
			for i := range tc.roles {
				spec := tc.roles[i]
				wantDiscovered = append(wantDiscovered, spec.name+"/member-0")

				var gotPublished []string
				for member := 0; member < int(spec.size); member++ {
					pod := renderMember(i, member)
					id := spec.name + "/member-" + pod.Labels[modelDeploymentMemberIndexLabel]

					if discovery.Matches(labels.Set(pod.Labels)) {
						gotDiscovered = append(gotDiscovered, id)
					}
					if published[spec.name].Matches(labels.Set(pod.Labels)) {
						gotPublished = append(gotPublished, id)
					}
				}

				assert.Equal(t, []string{spec.name + "/member-0"}, gotPublished,
					"the published selector of %s must name exactly the members that answer, "+
						"neither the ones that serve nothing nor fewer than its leader", spec.name)
			}

			assert.ElementsMatch(t, wantDiscovered, gotDiscovered,
				"discovery must reach exactly one member per replica at every size: a set missing "+
					"a single-Member role is the narrowed selector, and a set holding indexes above "+
					"zero is the widened one")
		})
	}
}

func TestRenderModelDeploymentRouterObjects_PodStaysOutsideAdmission(t *testing.T) {
	objects, err := renderModelDeploymentRouterObjects(context.Background(), routedModelDeployment(), nil)
	require.NoError(t, err)

	pod := objects.Deployment.Spec.Template
	assert.NotContains(t, pod.Labels, kueuectrlconst.QueueLabel)
	for _, container := range pod.Spec.Containers {
		assert.Empty(t, container.Resources.Requests, container.Name)
		assert.Empty(t, container.Resources.Limits, container.Name)
	}
}

func TestRenderModelDeploymentRouterObjects_ConfigHashIsDeterministic(t *testing.T) {
	md := routedModelDeployment()
	first, err := renderModelDeploymentRouterObjects(context.Background(), md, nil)
	require.NoError(t, err)
	second, err := renderModelDeploymentRouterObjects(context.Background(), md.DeepCopy(), nil)
	require.NoError(t, err)

	assert.Equal(t, first.ConfigMap.Data, second.ConfigMap.Data)
	assert.Equal(t, first.Deployment.Spec.Template.Annotations, second.Deployment.Spec.Template.Annotations)
}

func TestRenderModelDeploymentRouterObjects_RoleOrderDoesNotChangeTokenizer(t *testing.T) {
	md := routedModelDeployment()
	before, err := renderModelDeploymentRouterObjects(context.Background(), md, nil)
	require.NoError(t, err)

	md.Spec.Roles[0], md.Spec.Roles[1] = md.Spec.Roles[1], md.Spec.Roles[0]
	after, err := renderModelDeploymentRouterObjects(context.Background(), md, nil)
	require.NoError(t, err)

	assert.Equal(t, before.ConfigMap.Data, after.ConfigMap.Data)
	assert.Contains(t, after.ConfigMap.Data[modelDeploymentRouterConfigKey],
		"url: http://qwen-prefill.team-a.svc:8000")
}

func TestHashRouterConfig_IgnoresMapInsertionOrder(t *testing.T) {
	left := map[string]string{"a": "one", "b": "two"}
	right := make(map[string]string, 2)
	right["b"] = "two"
	right["a"] = "one"

	assert.Equal(t, hashRouterConfig(left), hashRouterConfig(right))
}

func TestRenderModelDeploymentRouterObjects_ScalingDoesNotRoll(t *testing.T) {
	md := routedModelDeployment()
	before, err := renderModelDeploymentRouterObjects(context.Background(), md, nil)
	require.NoError(t, err)

	md.Spec.Roles[0].Replicas++
	md.Spec.Roles[1].Replicas += 2
	after, err := renderModelDeploymentRouterObjects(context.Background(), md, nil)
	require.NoError(t, err)

	assert.Equal(t, before.ConfigMap.Data, after.ConfigMap.Data)
	assert.Equal(t, before.Deployment.Spec.Template, after.Deployment.Spec.Template)
	assert.NotContains(t, objectsConfig(before), "replicas")
	assert.NotContains(t, objectsConfig(before), "qwen-prefill-0")
}

func TestRenderModelDeploymentRouterObjects_ConfigChangeMovesTheHash(t *testing.T) {
	md := routedModelDeployment()
	before, err := renderModelDeploymentRouterObjects(context.Background(), md, nil)
	require.NoError(t, err)

	md.Spec.Router.ExtraArgs = []string{"--zap-log-level=debug"}
	after, err := renderModelDeploymentRouterObjects(context.Background(), md, nil)
	require.NoError(t, err)

	assert.NotEqual(t,
		before.Deployment.Spec.Template.Annotations[modelDeploymentRouterConfigHash],
		after.Deployment.Spec.Template.Annotations[modelDeploymentRouterConfigHash])
	assert.NotEqual(t, before.ConfigMap.Data, after.ConfigMap.Data)
}

func TestRenderModelDeploymentRouterObjects_CarriesEngineContracts(t *testing.T) {
	objects, err := renderModelDeploymentRouterObjects(context.Background(), routedModelDeployment(), nil)
	require.NoError(t, err)

	config := objects.ConfigMap.Data[modelDeploymentRouterConfigKey]
	for _, value := range []string{
		"vllm:num_requests_waiting",
		"vllm:num_requests_running",
		"vllm:kv_cache_usage_perc",
		"port: 8000",
		"topicFilter: kv@",
		"socketPort: 5557",
		"replaySocketPort: 5558",
	} {
		assert.Contains(t, config, value)
	}
}

func TestRenderModelDeploymentRouterObjects_MetricsUseServingPort(t *testing.T) {
	md := routedModelDeployment(func(md *workercore.ModelDeployment) {
		for i := range md.Spec.Roles {
			md.Spec.Roles[i].Ports = []workercore.ModelDeploymentPort{{Port: 8100}}
		}
	})
	objects, err := renderModelDeploymentRouterObjects(context.Background(), md, nil)
	require.NoError(t, err)

	assert.Contains(t, objects.ConfigMap.Data[modelDeploymentRouterConfigKey], "port: 8100")
	assert.Equal(t, int32(8100), objects.Contract.Metrics.Port)
}

func TestRenderModelDeploymentRouterObjects_MetricsScrape(t *testing.T) {
	for _, name := range []string{
		workercore.ModelDeploymentRouterLLMD,
		workercore.ModelDeploymentRouterVLLM,
		workercore.ModelDeploymentRouterSGLang,
	} {
		t.Run(name, func(t *testing.T) {
			md := routedModelDeployment()
			md.Spec.Router.Name = name
			objects, err := renderModelDeploymentRouterObjects(context.Background(), md, nil)
			require.NoError(t, err)
			annotations := objects.Deployment.Spec.Template.Annotations
			assert.Equal(t, "true", annotations["prometheus.io/scrape"])
			assert.Equal(t, "/metrics", annotations["prometheus.io/path"])
			assert.Equal(t, "9090", annotations["prometheus.io/port"])
			assert.Equal(t, "http", annotations["prometheus.io/scheme"])
			found := false
			for _, container := range objects.Deployment.Spec.Template.Spec.Containers {
				for _, port := range container.Ports {
					if port.Name == "metrics" && port.ContainerPort == 9090 {
						found = true
					}
				}
			}
			assert.True(t, found, "the advertised port belongs to a router container")
		})
	}
}

func TestRenderModelDeploymentRouterObjects_RefusesDifferentServingPorts(t *testing.T) {
	md := routedModelDeployment(func(md *workercore.ModelDeployment) {
		md.Spec.Roles[1].Ports = []workercore.ModelDeploymentPort{{Port: 8100}}
	})

	_, err := renderModelDeploymentRouterObjects(context.Background(), md, nil)
	require.EqualError(t, err, "router requires every role to use the same serving port; got [8000 8100]")
}

func TestRenderModelDeploymentRouterObjects_ProbesBothRouterContainers(t *testing.T) {
	objects, err := renderModelDeploymentRouterObjects(context.Background(), routedModelDeployment(), nil)
	require.NoError(t, err)

	envoy := objects.Deployment.Spec.Template.Spec.Containers[0]
	require.NotNil(t, envoy.ReadinessProbe)
	assert.Equal(t, intstr.FromInt32(modelDeploymentRouterHTTPPort), envoy.ReadinessProbe.TCPSocket.Port)

	epp := objects.Deployment.Spec.Template.Spec.Containers[1]
	require.NotNil(t, epp.ReadinessProbe)
	require.NotNil(t, epp.ReadinessProbe.GRPC)
	assert.Equal(t, int32(9003), epp.ReadinessProbe.GRPC.Port)
}

// TestRenderModelDeploymentRouterObjects_PullPolicyReachesBothContainers pins the policy onto BOTH
// router containers rather than one.
//
// Both images are resolved through the same redirect, so both are as replaceable as each other, and
// a mutable tag left on the default policy is served from whatever the node already has. The field
// is the deployment's one answer for its router; a container it did not reach was a gap rather than
// an exemption.
func TestRenderModelDeploymentRouterObjects_PullPolicyReachesBothContainers(t *testing.T) {
	md := routedModelDeployment()
	md.Spec.Router.ImagePullPolicy = core.PullAlways

	objects, err := renderModelDeploymentRouterObjects(context.Background(), md, nil)
	require.NoError(t, err)

	containers := objects.Deployment.Spec.Template.Spec.Containers
	require.Len(t, containers, 2)
	for i := range containers {
		assert.Equal(t, core.PullAlways, containers[i].ImagePullPolicy,
			"%s carries the policy the router declared", containers[i].Name)
	}
}

// TestRenderModelDeploymentRouterObjects_PullPolicyIsResolvedWhenUndeclared pins the case the test
// above cannot see: the field is optional and nothing defaults it.
//
// An empty policy rendered here is one the API server fills in on write, and alignRenderedContainer
// compares the field unconditionally — so the next pass reads the server's default as a difference,
// writes the empty value back, and the Deployment updates forever. Asserting a non-empty value is
// what makes that loop impossible rather than merely unobserved.
func TestRenderModelDeploymentRouterObjects_PullPolicyIsResolvedWhenUndeclared(t *testing.T) {
	md := routedModelDeployment()
	md.Spec.Router.ImagePullPolicy = ""

	objects, err := renderModelDeploymentRouterObjects(context.Background(), md, nil)
	require.NoError(t, err)

	containers := objects.Deployment.Spec.Template.Spec.Containers
	require.Len(t, containers, 2)
	for i := range containers {
		assert.NotEmpty(t, containers[i].ImagePullPolicy,
			"%s renders a policy the API server has no reason to default", containers[i].Name)
	}
}

func TestRenderModelDeploymentRouterObjects_CarriesRuntimeIdentityAndStreamingTrailers(t *testing.T) {
	objects, err := renderModelDeploymentRouterObjects(context.Background(), routedModelDeployment(), nil)
	require.NoError(t, err)

	epp := objects.Deployment.Spec.Template.Spec.Containers[1]
	for _, env := range []core.EnvVar{
		{Name: "NAMESPACE", ValueFrom: &core.EnvVarSource{FieldRef: &core.ObjectFieldSelector{FieldPath: "metadata.namespace"}}},
		{Name: "POD_NAME", ValueFrom: &core.EnvVarSource{FieldRef: &core.ObjectFieldSelector{FieldPath: "metadata.name"}}},
	} {
		assert.Contains(t, epp.Env, env)
	}

	envoy := objects.ConfigMap.Data[modelDeploymentRouterEnvoyConfigKey]
	assert.Contains(t, envoy, "request_trailer_mode: SEND")
	assert.Contains(t, envoy, "response_trailer_mode: SEND")
	assert.Contains(t, envoy, "typed_extension_protocol_options:")
	assert.Contains(t, envoy, "explicit_http_config:")
	// The original-destination cluster follows x-gateway-destination-endpoint, so a client-supplied
	// copy of that header is what would pick the upstream. Only the EPP may set it.
	//
	// Three assertions rather than one, because the removal has to happen in a place Envoy accepts
	// AND at a moment that leaves the EPP's own copy standing, and a single "the text is present"
	// check reports neither. An earlier version of this file asserted the removal as a
	// connection-manager field, which Envoy has never declared, so the assertion passed against a
	// configuration that stopped the proxy from starting.
	// The trailing colon matters: it matches the YAML key and not the prose in the comment beside
	// it, which names the same field in order to say why it is not used.
	assert.NotContains(t, envoy, "request_headers_to_remove:",
		"HttpConnectionManager declares no such field, and a route-level removal would run after ext_proc")
	assert.Contains(t, envoy, "- remove: x-gateway-destination-endpoint")
	assert.Less(t,
		strings.Index(envoy, "envoy.filters.http.header_mutation"),
		strings.Index(envoy, "envoy.filters.http.ext_proc"),
		"the client's copy must be dropped before the EPP is consulted, and filters run in order")
}

// TestRenderModelDeploymentRouterObjects_KeepsTrafficManagementEastWest pins the two flags this
// operator renders OFF while upstream defaults them ON.
//
// IT GUARDS A BOUNDARY, NOT A VALUE. A router picks which replica serves a request that is already
// inside the cluster; TLS and caller authentication are the gateway's, at the edge. The assertion
// exists because the inverted default is exactly what a reader comparing against upstream would
// "fix", and because neither flag is reachable through extraArgs -- so flipping one here would be
// the only way it could happen, and it would happen silently.
func TestRenderModelDeploymentRouterObjects_KeepsTrafficManagementEastWest(t *testing.T) {
	objects, err := renderModelDeploymentRouterObjects(context.Background(), routedModelDeployment(), nil)
	require.NoError(t, err)

	epp := objects.Deployment.Spec.Template.Spec.Containers[1]
	assert.Contains(t, epp.Args, "--secure-serving=false")
	assert.Contains(t, epp.Args, "--metrics-endpoint-auth=false")
}

func TestRenderModelDeploymentRouterObjects_UnmanagedProducerPublishesNoKVEvents(t *testing.T) {
	md := routedModelDeployment(func(md *workercore.ModelDeployment) {
		md.Spec.Roles[0].Command = []string{"vllm", "serve", "custom-model"}
	})

	objects, err := renderModelDeploymentRouterObjects(context.Background(), md, nil)
	require.NoError(t, err)

	assert.Nil(t, objects.Contract.Roles[0].KVEvents)
}

func TestRenderModelDeploymentRouterObjects_EndpointUsesRouterTransport(t *testing.T) {
	md := routedModelDeployment(func(md *workercore.ModelDeployment) {
		md.Spec.Roles[0].ExtraArgs = []string{"--ssl-keyfile=/tls/key.pem"}
	})
	objects, err := renderModelDeploymentRouterObjects(context.Background(), md, nil)
	require.NoError(t, err)

	assert.Equal(t, "http://qwen-router.team-a.svc:8081", objects.Contract.Endpoint)
	assert.Equal(t, "https://qwen-prefill.team-a.svc:8000", objects.Contract.Roles[0].Endpoint)
}

func objectsConfig(objects ModelDeploymentRouterObjects) string {
	return objects.ConfigMap.Data[modelDeploymentRouterConfigKey] +
		objects.ConfigMap.Data[modelDeploymentRouterEnvoyConfigKey]
}

func TestModelDeploymentRouter_ConvergesAndRepairsConfig(t *testing.T) {
	cli := newModelDeploymentClient(routedModelDeployment(), newRenderInstanceType())

	_, err := reconcileModelDeployment(t, cli)
	require.NoError(t, err)
	config := new(core.ConfigMap)
	key := ctrlcli.ObjectKey{Namespace: "team-a", Name: "qwen-router"}
	require.NoError(t, cli.Get(context.Background(), key, config))
	want := config.Data[modelDeploymentRouterConfigKey]
	config.Data[modelDeploymentRouterConfigKey] = "edited out of band"
	require.NoError(t, cli.Update(context.Background(), config))

	_, err = reconcileModelDeployment(t, cli)
	require.NoError(t, err)
	require.NoError(t, cli.Get(context.Background(), key, config))
	assert.Equal(t, want, config.Data[modelDeploymentRouterConfigKey])
}

func TestModelDeploymentRouter_RemovalPrunesEveryObject(t *testing.T) {
	cli := newModelDeploymentClient(routedModelDeployment(), newRenderInstanceType())
	_, err := reconcileModelDeployment(t, cli)
	require.NoError(t, err)
	key := ctrlcli.ObjectKey{Namespace: "team-a", Name: "qwen-router"}
	objects := []ctrlcli.Object{
		new(app.Deployment), new(core.ConfigMap), new(core.Service),
		new(core.ServiceAccount), new(rbac.Role), new(rbac.RoleBinding),
	}
	for _, object := range objects {
		require.NoError(t, cli.Get(context.Background(), key, object), "%T must exist before removal", object)
	}

	md := getModelDeployment(t, cli)
	md.Spec.Router = nil
	require.NoError(t, cli.Update(context.Background(), md))
	_, err = reconcileModelDeployment(t, cli)
	require.NoError(t, err)

	for _, object := range objects {
		assert.True(t, kerrors.IsNotFound(cli.Get(context.Background(), key, object)), "%T", object)
	}
	md = getModelDeployment(t, cli)
	assert.Nil(t, md.Status.Router)
	assert.Equal(t, "http://qwen.team-a.svc:8000", md.Status.Endpoint)

	md.Spec.Router = &workercore.ModelDeploymentRouter{Name: workercore.ModelDeploymentRouterLLMD}
	require.NoError(t, cli.Update(context.Background(), md))
	_, err = reconcileModelDeployment(t, cli)
	require.NoError(t, err)
	for _, object := range objects {
		require.NoError(t, cli.Get(context.Background(), key, object), "%T must be recreated", object)
	}
}

func TestModelDeploymentRouter_ReadyReplicaPublishesRouterEndpoint(t *testing.T) {
	cli := newModelDeploymentClient(routedModelDeployment(), newRenderInstanceType())
	_, err := reconcileModelDeployment(t, cli)
	require.NoError(t, err)
	md := getModelDeployment(t, cli)
	assert.Empty(t, md.Status.Endpoint)

	deployment := new(app.Deployment)
	key := ctrlcli.ObjectKey{Namespace: "team-a", Name: "qwen-router"}
	require.NoError(t, cli.Get(context.Background(), key, deployment))
	deployment.Status.ReadyReplicas = 1
	require.NoError(t, cli.Status().Update(context.Background(), deployment))
	_, err = reconcileModelDeployment(t, cli)
	require.NoError(t, err)

	md = getModelDeployment(t, cli)
	assert.Equal(t, "http://qwen-router.team-a.svc:8081", md.Status.Endpoint)
}

func TestModelDeploymentRouter_RefusesToAdopt(t *testing.T) {
	foreign := &core.ConfigMap{ObjectMeta: meta.ObjectMeta{Name: "qwen-router", Namespace: "team-a"}}
	cli := newModelDeploymentClient(routedModelDeployment(), newRenderInstanceType(), foreign)

	_, err := reconcileModelDeployment(t, cli)
	require.EqualError(t, err, "configmap team-a/qwen-router is not owned by this deployment")
}

func TestModelDeploymentRouter_SecondPassWritesNothing(t *testing.T) {
	writes := new(modelDeploymentWrites)
	cli := newCountingModelDeploymentClient(writes, routedModelDeployment(), newRenderInstanceType())
	_, err := reconcileModelDeployment(t, cli)
	require.NoError(t, err)
	require.Equal(t, 13, writes.creates,
		"the pass creates four replicas, three role Services and six router objects")

	*writes = modelDeploymentWrites{}
	_, err = reconcileModelDeployment(t, cli)
	require.NoError(t, err)
	assert.Equal(t, modelDeploymentWrites{}, *writes)
}

func TestModelDeploymentRouter_ConfigChangeRollsOnce(t *testing.T) {
	writes := new(modelDeploymentWrites)
	cli := newCountingModelDeploymentClient(writes, routedModelDeployment(), newRenderInstanceType())
	_, err := reconcileModelDeployment(t, cli)
	require.NoError(t, err)

	md := getModelDeployment(t, cli)
	md.Spec.Router.ExtraArgs = []string{"--zap-log-level=debug"}
	require.NoError(t, cli.Update(context.Background(), md))
	*writes = modelDeploymentWrites{}
	_, err = reconcileModelDeployment(t, cli)
	require.NoError(t, err)
	assert.Equal(t, 2, writes.updates, "only the ConfigMap and Deployment move")

	*writes = modelDeploymentWrites{}
	_, err = reconcileModelDeployment(t, cli)
	require.NoError(t, err)
	assert.Equal(t, modelDeploymentWrites{}, *writes)
}

// TestRenderModelDeploymentRouterObjects_NamesTheProgramToRun pins the command on the container
// that runs out of the shared router image, for every router that renders one.
//
// IT IS ASSERTED ON THE CONTAINER rather than on the render, because the defect it exists for lived
// in the gap between them: the render named the binary and the container did not carry it. The
// image declares no entrypoint on purpose -- it holds three programs -- so a container given
// arguments and no command puts the first flag where the program name goes and dies as "executable
// file not found", naming the flag. Found by running it on a cluster, not by this suite, which is
// why the assertion is on the field rather than on a message.
func TestRenderModelDeploymentRouterObjects_NamesTheProgramToRun(t *testing.T) {
	cases := []struct {
		name      string
		md        *workercore.ModelDeployment
		container string
		want      []string
	}{
		{
			name:      "the picker runs the endpoint picker",
			md:        routedModelDeployment(),
			container: "epp",
			want:      []string{"epp"},
		},
		{
			name: "the vllm router runs its own binary",
			md: routedModelDeployment(func(md *workercore.ModelDeployment) {
				md.Spec.Router.Name = workercore.ModelDeploymentRouterVLLM
			}),
			container: "router",
			want:      []string{"vllm-router"},
		},
		{
			name: "the sglang gateway runs its own binary",
			md: routedModelDeployment(func(md *workercore.ModelDeployment) {
				md.Spec.Router.Name = workercore.ModelDeploymentRouterSGLang
				md.Spec.Engine.Name = workercore.ModelDeploymentEngineSGLang
			}),
			container: "router",
			want:      []string{"sgl-model-gateway"},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			objects, err := renderModelDeploymentRouterObjects(context.Background(), c.md, nil)
			require.NoError(t, err)

			commands := map[string][]string{}
			for _, ctr := range objects.Deployment.Spec.Template.Spec.Containers {
				commands[ctr.Name] = ctr.Command
			}
			require.Contains(t, commands, c.container, "the router must render this container")
			assert.Equal(t, c.want, commands[c.container],
				"the container must name which of the image's three programs it runs")
		})
	}
}

// TestRenderModelDeploymentRouterObjects_ImageSources pins where each of a managed router's two
// container images comes from. Both defaults are settings rather than constants, so a change to
// either is one every cluster inherits at once, and this is the only place that shows up.
//
// The picker's row also pins the precedence: an image named on the object wins, and it is taken
// verbatim rather than redirected, because it is the user's own reference.
func TestRenderModelDeploymentRouterObjects_ImageSources(t *testing.T) {
	cases := []struct {
		name      string
		md        *workercore.ModelDeployment
		container string
		want      string
		why       string
	}{
		{
			name:      "proxy runs the mirrored image, pinned to a release",
			md:        routedModelDeployment(),
			container: "envoy",
			want:      "gpustack/mirrored-envoy:distroless-v1.33.2",
			why:       "the proxy has no field to override it; only the setting redirects it",
		},
		{
			// One image carries every router, and which binary runs is the rendered command's
			// decision rather than the image's. It is this project's own build because the three
			// routers come from three sources and one of them carries a patch shipped here, so no
			// upstream image holds them together.
			name:      "picker defaults to the image that carries every router",
			md:        routedModelDeployment(),
			container: "epp",
			want:      "gpustack/llm-router:v0.1.0",
			why:       "the default names a release of this project's own build, not a moving upstream tag",
		},
		{
			name: "and so does a router configured by argv, from the same setting",
			md: routedModelDeployment(func(md *workercore.ModelDeployment) {
				md.Spec.Router.Name = workercore.ModelDeploymentRouterVLLM
			}),
			container: "router",
			want:      "gpustack/llm-router:v0.1.0",
			why:       "one setting says where the binaries come from; the command picks which one runs",
		},
		{
			name: "an image named on the object is used verbatim",
			md: routedModelDeployment(func(md *workercore.ModelDeployment) {
				md.Spec.Router.Image = "registry.example/team/picker:v9"
			}),
			container: "epp",
			want:      "registry.example/team/picker:v9",
			why:       "the user's own reference must not be rewritten to the cluster mirror",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			objects, err := renderModelDeploymentRouterObjects(context.Background(), c.md, nil)
			require.NoError(t, err)

			images := map[string]string{}
			for _, ctr := range objects.Deployment.Spec.Template.Spec.Containers {
				images[ctr.Name] = ctr.Image
			}
			require.Contains(t, images, c.container, "the router must render this container")
			assert.Equal(t, c.want, images[c.container], c.why)
		})
	}
}

// TestRenderModelDeploymentRouterObjects_SerializedOutputIsPinnedToThePreSplitRender pins the WHOLE
// object set -- serialized and digested, not field-picked -- for one input off every branch of the
// renderer. The other cases in this file assert the fields they exist for, so a refactor of the
// render path could drop a field no case reads and every one of them would stay green; this is the
// case that cannot, because it compares the bytes of everything at once.
//
// THE ORIGINAL DIGESTS WERE CAPTURED FROM THE RENDERER AS IT STOOD BEFORE IT WAS SPLIT into a
// shared half and a per-router half, and that split had to reproduce every one of them exactly. The
// table has been RE-BASELINED three times since, each time for a rendering change that was intended:
// once when the proxy gained its access log and a route timeout filled from a field rather than
// fixed in the template, once when the default router image became the one build that carries all
// three routers, and once when the containers running out of that image gained the command naming
// which of its three programs to run. That last one was found by running it: the image declares no
// entrypoint, so arguments without a command land in the CMD position and the first flag is read as
// the program name, which every field-picked case in this file passed over. The metrics listener
// scheme annotation was then added to each managed router Pod.
// To re-baseline after an intended rendering change: empty the table, run this case, and pin the
// digests the failures print.
func TestRenderModelDeploymentRouterObjects_SerializedOutputIsPinnedToThePreSplitRender(t *testing.T) {
	pinned := map[string]string{
		"a prefill and decode pair": "4e62baa7a1935f66946a863ee730e551cd23a4f8ffe677051b47e2bf58f8b9a4",
		"a sole server role":        "052c5cc61b6c6b2c313c8cc743f070dcc9e5eaa2f8cf197ad52affa3fa8dc0cc",
		"a router declaring its own image, policy, replicas and extra arguments": "a3eb0fce558ed09aa0f41e6b57cf7e9777bc169ed48709a0f2647064532c43cc",
		"a role whose command the user took over, so it publishes no events":     "cb3bbff686dad17a56b2f26dc9ef12cf3a8fbefb139b31361b76d0b6a92b72d6",
		"an sglang engine, whose metrics contract differs":                       "c2034efbe3aba469e9bba1f7a8249f61c94976e89f958dc719a1e29ce7ce42cc",
	}

	soleServerRole := func() *workercore.ModelDeployment {
		md := newRenderDeployment()
		md.Spec.Router = &workercore.ModelDeploymentRouter{Name: workercore.ModelDeploymentRouterLLMD}

		return md
	}

	testCases := []struct {
		name          string
		md            *workercore.ModelDeployment
		manufacturers map[string]string
	}{
		{
			name: "a prefill and decode pair",
			md:   routedModelDeployment(),
		},
		{
			name: "a sole server role",
			md:   soleServerRole(),
		},
		{
			name: "a router declaring its own image, policy, replicas and extra arguments",
			md: routedModelDeployment(func(md *workercore.ModelDeployment) {
				md.Spec.Router.Image = "registry.example/team/picker:v9"
				md.Spec.Router.ImagePullPolicy = core.PullAlways
				md.Spec.Router.Replicas = ptr.To(int32(3))
				md.Spec.Router.ExtraArgs = []string{"--v=4"}
			}),
		},
		{
			name: "a role whose command the user took over, so it publishes no events",
			md: routedModelDeployment(func(md *workercore.ModelDeployment) {
				md.Spec.Roles[0].Command = []string{"vllm", "serve", "custom-model"}
			}),
		},
		{
			name: "an sglang engine, whose metrics contract differs",
			md: routedModelDeployment(func(md *workercore.ModelDeployment) {
				md.Spec.Engine.Name = workercore.ModelDeploymentEngineSGLang
			}),
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			objects, err := renderModelDeploymentRouterObjects(
				context.Background(), tc.md, tc.manufacturers)
			require.NoError(t, err)

			encoded, err := json.Marshal(objects)
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

// TestRenderModelDeploymentRouterWorkload_RefusesARouterWithNoWorkload pins the refusal on the shape
// a newly added router arrives in.
//
// The caller renders the router's configuration first and is refused there for a name nothing
// registered, so this branch is not reachable through it today. It is asserted anyway because this
// is the function a fourth router is added to, and the failure it prevents is silent: a branch that
// returns the zero value renders a Deployment carrying no containers at all.
func TestRenderModelDeploymentRouterWorkload_RefusesARouterWithNoWorkload(t *testing.T) {
	md := routedModelDeployment(func(md *workercore.ModelDeployment) {
		md.Spec.Router.Name = "a-router-with-no-branch"
	})

	_, err := renderModelDeploymentRouterWorkload(context.Background(), md,
		modelDeploymentRouterDerived{Name: "qwen-router", Rendered: router.Output{Document: "a: b"}})
	require.EqualError(t, err, `router "a-router-with-no-branch" renders no workload`)
}

// TestRenderModelDeploymentRouterWorkload_ConfigurationBelongsToTheBranchThatReadsIt states the
// boundary the split draws, from the side that would silently stop being true.
//
// EVERY ConfigMap key belongs to a branch, including the three that look router-independent. The
// selector, the target ports and the user's arguments reach the picker as a mounted file and as
// environment entries, and they reach a router configured by argv as arguments -- so writing them
// in the shared half would put a second copy of one derivation where nothing reads it, and leave a
// reader unable to tell which copy the process is using. The digests cannot state this: both halves
// write into one object, so a key rendered from the wrong side produces identical bytes.
func TestRenderModelDeploymentRouterWorkload_ConfigurationBelongsToTheBranchThatReadsIt(t *testing.T) {
	derived := modelDeploymentRouterDerived{
		Name:             "qwen-router",
		Rendered:         router.Output{Document: "a: b"},
		EndpointSelector: "app.kubernetes.io/name=model-deployment",
		TargetPorts:      "8000",
		ExtraArgs:        "[]",
	}

	picker, err := renderModelDeploymentRouterWorkload(
		context.Background(), routedModelDeployment(), derived)
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{
		modelDeploymentRouterConfigKey,
		modelDeploymentRouterEnvoyConfigKey,
		modelDeploymentRouterExtraArgsKey,
		modelDeploymentRouterSelectorKey,
		modelDeploymentRouterTargetPortsKey,
	}, slices.Collect(maps.Keys(picker.Config)),
		"the picker reads all five, so its branch writes all five")
	assert.Len(t, picker.Containers, 2, "the picker and the proxy that fronts it")
	assert.Len(t, picker.Volumes, 1, "the mounted document, and nothing else")

	argv, err := renderModelDeploymentRouterWorkload(context.Background(),
		routedModelDeployment(func(md *workercore.ModelDeployment) {
			md.Spec.Router.Name = workercore.ModelDeploymentRouterVLLM
		}), derived)
	require.NoError(t, err)
	assert.Empty(t, argv.Config, "a router configured by argv reads no configuration object")
	assert.Len(t, argv.Containers, 1, "one process, no proxy")
	assert.Empty(t, argv.Volumes, "nothing to mount")
}

// TestRenderModelDeploymentRouterObjects_ArgvRouterRendersNoConfigMap states what the case above
// implies about the object set: no configuration means no object, rather than an empty one.
//
// An empty ConfigMap would be indistinguishable from one whose content was dropped, and the Pod
// would carry a configuration hash taken over nothing -- a value that never moves, on an annotation
// whose whole purpose is to move.
func TestRenderModelDeploymentRouterObjects_ArgvRouterRendersNoConfigMap(t *testing.T) {
	md := routedModelDeployment(func(md *workercore.ModelDeployment) {
		md.Spec.Router.Name = workercore.ModelDeploymentRouterVLLM
	})

	objects, err := renderModelDeploymentRouterObjects(context.Background(), md, nil)
	require.NoError(t, err)

	assert.Nil(t, objects.ConfigMap)
	assert.NotContains(t, objects.Deployment.Spec.Template.Annotations, modelDeploymentRouterConfigHash)
	require.Len(t, objects.Deployment.Spec.Template.Spec.Containers, 1)
	assert.Empty(t, objects.Deployment.Spec.Template.Spec.Volumes)
	assert.Empty(t, objects.Deployment.Spec.Template.Spec.Containers[0].VolumeMounts)
	assert.NotNil(t, objects.Service, "the shared half still fronts it")
	assert.NotNil(t, objects.Role, "and still grants it the discovery it needs")
}

// TestRenderModelDeploymentRouterObjects_TheGatewayAndTheEngineReadOneField pins the property the
// SGLang gateway's routing rests on.
//
// That gateway registers a discovered worker under the id the WORKER reports for itself, not one
// discovery assigns, and it refuses a request naming a model no registered worker answers to. So
// the name a caller asks for and the name the engine registers under have to be one field, and this
// asserts they are -- by comparing the two derivations WITH EACH OTHER rather than each with a
// literal, which is what the cases already in this repository do and what would keep passing if one
// side started reading somewhere else.
//
// The cluster half of the same claim -- that the gateway does register the worker under this name
// -- is an end-to-end case; nothing rendered here can observe a registry.
func TestRenderModelDeploymentRouterObjects_TheGatewayAndTheEngineReadOneField(t *testing.T) {
	// Deliberately not the fixture's own model, so a renderer that hardcoded that string fails.
	const model = "org/a-model-the-fixture-does-not-name"

	md := routedModelDeployment(func(md *workercore.ModelDeployment) {
		md.Spec.Engine.Name = workercore.ModelDeploymentEngineSGLang
		md.Spec.Router.Name = workercore.ModelDeploymentRouterSGLang
		md.Spec.Model.Name = model
	})
	require.NotEqual(t, model, newRenderDeployment().Spec.Model.Name,
		"the fixture must not already carry this name, or the comparison proves nothing")

	pod, err := renderModelDeploymentPodTemplate(context.Background(), ModelDeploymentRenderInput{
		Deployment: md, Role: &md.Spec.Roles[0], InstanceType: newRenderInstanceType(),
	})
	require.NoError(t, err)

	argv := pod.Spec.Containers[0].Command
	at := slices.Index(argv, "--model-path")
	require.GreaterOrEqual(t, at, 0, "the engine is launched with the model named; got %v", argv)
	require.Less(t, at+1, len(argv))

	assert.Equal(t, md.Spec.Model.Name, argv[at+1],
		"the id the worker registers under is the deployment's own model name")

	// And the other end names nothing, which is what makes the engine's value the only one: the
	// gateway routes by what workers registered, so a name rendered here would be a second source
	// for one answer.
	objects, err := renderModelDeploymentRouterObjects(context.Background(), md, nil)
	require.NoError(t, err)
	require.Len(t, objects.Deployment.Spec.Template.Spec.Containers, 1)
	for _, arg := range objects.Deployment.Spec.Template.Spec.Containers[0].Args {
		assert.NotContains(t, arg, model,
			"the gateway is told no model name; it routes by the id each worker reports")
	}
}

// TestRenderModelDeploymentRouterObjects_MetricsContractIsThePickersAlone is the renderer's half of
// the same gate the admission handler carries.
//
// The engine metrics table exists to feed one router's metrics extractor. The other two score on
// their own observations and scrape none of it, so a published contract naming engine metrics under
// them would describe a router that is not running — and the call itself would refuse, in a third
// router's name, on an engine whose row happened to be missing.
func TestRenderModelDeploymentRouterObjects_MetricsContractIsThePickersAlone(t *testing.T) {
	cases := []struct {
		router, engine string
		want           bool
	}{
		{router: workercore.ModelDeploymentRouterLLMD, engine: workercore.ModelDeploymentEngineVLLM, want: true},
		{router: workercore.ModelDeploymentRouterVLLM, engine: workercore.ModelDeploymentEngineVLLM},
		{router: workercore.ModelDeploymentRouterSGLang, engine: workercore.ModelDeploymentEngineSGLang},
	}
	for _, c := range cases {
		t.Run(c.router, func(t *testing.T) {
			md := routedModelDeployment(func(md *workercore.ModelDeployment) {
				md.Spec.Router.Name = c.router
				md.Spec.Engine.Name = c.engine
			})

			objects, err := renderModelDeploymentRouterObjects(context.Background(), md, nil)
			require.NoError(t, err)

			if c.want {
				require.NotNil(t, objects.Contract.Metrics)
				assert.NotEmpty(t, objects.Contract.Metrics.QueuedRequests,
					"the picker is configured from these strings, so they are published")

				return
			}
			assert.Nil(t, objects.Contract.Metrics,
				"this router scrapes no engine metrics, so it publishes none")
		})
	}
}

// TestRenderModelDeploymentRouterObjects_ProxyTemplateIsAssertedOnItsRenderedText reads the proxy
// configuration as the proxy would, rather than as the constant it came from.
//
// NO PLACEHOLDER MAY SURVIVE, and that is the assertion this case exists for. The template is
// substituted rather than formatted, because it is full of Envoy's own percent-delimited operators
// and a format verb would either mangle them or need every one escaped. Substitution fails quietly:
// a renamed placeholder leaves its own text in the rendered file, Envoy reads it as a duration it
// cannot parse, and the proxy does not start -- with nothing here to say why.
func TestRenderModelDeploymentRouterObjects_ProxyTemplateIsAssertedOnItsRenderedText(t *testing.T) {
	t.Run("the field renders as the route timeout", func(t *testing.T) {
		md := routedModelDeployment(func(md *workercore.ModelDeployment) {
			md.Spec.Router.RequestTimeoutSeconds = ptr.To(int32(90))
		})

		objects, err := renderModelDeploymentRouterObjects(context.Background(), md, nil)
		require.NoError(t, err)

		envoy := objects.ConfigMap.Data[modelDeploymentRouterEnvoyConfigKey]
		assert.Contains(t, envoy, "timeout: 90s")
		assert.NotContains(t, envoy, modelDeploymentRouterEnvoyTimeoutPlaceholder,
			"a surviving placeholder is a duration Envoy cannot parse")
		assert.NotContains(t, envoy, "<", "no placeholder of any spelling survives")
	})

	t.Run("unset keeps the proxy's own day", func(t *testing.T) {
		objects, err := renderModelDeploymentRouterObjects(
			context.Background(), routedModelDeployment(), nil)
		require.NoError(t, err)

		envoy := objects.ConfigMap.Data[modelDeploymentRouterEnvoyConfigKey]
		assert.Contains(t, envoy, "timeout: 86400s",
			"absence renders the value this proxy has always carried, not a second field's default")
		assert.NotContains(t, envoy, modelDeploymentRouterEnvoyTimeoutPlaceholder)
	})

	t.Run("the access log names the endpoint beside the outcome", func(t *testing.T) {
		objects, err := renderModelDeploymentRouterObjects(
			context.Background(), routedModelDeployment(), nil)
		require.NoError(t, err)

		envoy := objects.ConfigMap.Data[modelDeploymentRouterEnvoyConfigKey]
		assert.Contains(t, envoy, "envoy.access_loggers.stdout")
		for _, operator := range []string{
			"%RESPONSE_CODE%", "%RESPONSE_FLAGS%", "%RESPONSE_CODE_DETAILS%", "%DURATION%",
			"%UPSTREAM_HOST%", "%REQ(:METHOD)%", "%REQ(X-REQUEST-ID)%",
			"%BYTES_RECEIVED%", "%BYTES_SENT%",
		} {
			assert.Contains(t, envoy, operator,
				"the log carries the pair no other log on this Pod does: the endpoint and the outcome")
		}
		assert.NotContains(t, envoy, "admin:",
			"an admin listener is an unauthenticated control surface, not an access log")
	})
}

// TestRenderModelDeploymentRouterObjects_TimeoutAndThresholdReachTheirOwnRouter pins where each of
// the two fields lands, which is not the same place for both.
//
// The timeout reaches all three: the proxy's route under the picker, a flag under the other two.
// The threshold reaches one, because only the picker has a per-request decision to threshold —
// admission refuses it on the others, so this asserts what the accepted value does rather than
// restating that refusal.
func TestRenderModelDeploymentRouterObjects_TimeoutAndThresholdReachTheirOwnRouter(t *testing.T) {
	t.Run("the timeout becomes a flag on a router configured by argv", func(t *testing.T) {
		md := routedModelDeployment(func(md *workercore.ModelDeployment) {
			md.Spec.Router.Name = workercore.ModelDeploymentRouterVLLM
			md.Spec.Router.RequestTimeoutSeconds = ptr.To(int32(90))
		})

		objects, err := renderModelDeploymentRouterObjects(context.Background(), md, nil)
		require.NoError(t, err)
		require.Len(t, objects.Deployment.Spec.Template.Spec.Containers, 1)
		assert.Contains(t, objects.Deployment.Spec.Template.Spec.Containers[0].Args,
			"--request-timeout-secs=90")
	})

	t.Run("unset renders no flag at all", func(t *testing.T) {
		md := routedModelDeployment(func(md *workercore.ModelDeployment) {
			md.Spec.Router.Name = workercore.ModelDeploymentRouterVLLM
		})

		objects, err := renderModelDeploymentRouterObjects(context.Background(), md, nil)
		require.NoError(t, err)
		for _, arg := range objects.Deployment.Spec.Template.Spec.Containers[0].Args {
			assert.NotContains(t, arg, "--request-timeout-secs",
				"absence is what leaves this router on its own default, which differs from the proxy's")
		}
	})

	for _, tc := range []struct {
		name      string
		threshold *int32
		want      string
	}{
		{name: "a declared threshold", threshold: ptr.To(int32(512)), want: "nonCachedTokens: 512"},
		{name: "an explicit zero, which disables splitting", threshold: ptr.To(int32(0)), want: "nonCachedTokens: 0"},
		{name: "unset, which renders the value it always did", want: "nonCachedTokens: 8"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			md := routedModelDeployment(func(md *workercore.ModelDeployment) {
				md.Spec.Router.DisaggregationThresholdTokens = tc.threshold
			})

			objects, err := renderModelDeploymentRouterObjects(context.Background(), md, nil)
			require.NoError(t, err)
			assert.Contains(t, objects.ConfigMap.Data[modelDeploymentRouterConfigKey], tc.want)
		})
	}
}
