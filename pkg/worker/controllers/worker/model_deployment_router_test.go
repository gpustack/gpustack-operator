package worker

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	app "k8s.io/api/apps/v1"
	core "k8s.io/api/core/v1"
	rbac "k8s.io/api/rbac/v1"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	ctrlcli "sigs.k8s.io/controller-runtime/pkg/client"
	kueuectrlconst "sigs.k8s.io/kueue/pkg/controller/constants"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	"gpustack.ai/gpustack/pkg/systemmeta"
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
	objects, err := renderModelDeploymentRouterObjects(context.Background(), md)
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

func TestRenderModelDeploymentRouterObjects_PodStaysOutsideAdmission(t *testing.T) {
	objects, err := renderModelDeploymentRouterObjects(context.Background(), routedModelDeployment())
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
	first, err := renderModelDeploymentRouterObjects(context.Background(), md)
	require.NoError(t, err)
	second, err := renderModelDeploymentRouterObjects(context.Background(), md.DeepCopy())
	require.NoError(t, err)

	assert.Equal(t, first.ConfigMap.Data, second.ConfigMap.Data)
	assert.Equal(t, first.Deployment.Spec.Template.Annotations, second.Deployment.Spec.Template.Annotations)
}

func TestRenderModelDeploymentRouterObjects_RoleOrderDoesNotChangeTokenizer(t *testing.T) {
	md := routedModelDeployment()
	before, err := renderModelDeploymentRouterObjects(context.Background(), md)
	require.NoError(t, err)

	md.Spec.Roles[0], md.Spec.Roles[1] = md.Spec.Roles[1], md.Spec.Roles[0]
	after, err := renderModelDeploymentRouterObjects(context.Background(), md)
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
	before, err := renderModelDeploymentRouterObjects(context.Background(), md)
	require.NoError(t, err)

	md.Spec.Roles[0].Replicas++
	md.Spec.Roles[1].Replicas += 2
	after, err := renderModelDeploymentRouterObjects(context.Background(), md)
	require.NoError(t, err)

	assert.Equal(t, before.ConfigMap.Data, after.ConfigMap.Data)
	assert.Equal(t, before.Deployment.Spec.Template, after.Deployment.Spec.Template)
	assert.NotContains(t, objectsConfig(before), "replicas")
	assert.NotContains(t, objectsConfig(before), "qwen-prefill-0")
}

func TestRenderModelDeploymentRouterObjects_ConfigChangeMovesTheHash(t *testing.T) {
	md := routedModelDeployment()
	before, err := renderModelDeploymentRouterObjects(context.Background(), md)
	require.NoError(t, err)

	md.Spec.Router.ExtraArgs = []string{"--zap-log-level=debug"}
	after, err := renderModelDeploymentRouterObjects(context.Background(), md)
	require.NoError(t, err)

	assert.NotEqual(t,
		before.Deployment.Spec.Template.Annotations[modelDeploymentRouterConfigHash],
		after.Deployment.Spec.Template.Annotations[modelDeploymentRouterConfigHash])
	assert.NotEqual(t, before.ConfigMap.Data, after.ConfigMap.Data)
}

func TestRenderModelDeploymentRouterObjects_CarriesEngineContracts(t *testing.T) {
	objects, err := renderModelDeploymentRouterObjects(context.Background(), routedModelDeployment())
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
			md.Spec.Roles[i].Template.Ports = []workercore.InstancePort{{Port: 8100}}
		}
	})
	objects, err := renderModelDeploymentRouterObjects(context.Background(), md)
	require.NoError(t, err)

	assert.Contains(t, objects.ConfigMap.Data[modelDeploymentRouterConfigKey], "port: 8100")
	assert.Equal(t, int32(8100), objects.Contract.Metrics.Port)
}

func TestRenderModelDeploymentRouterObjects_RefusesDifferentServingPorts(t *testing.T) {
	md := routedModelDeployment(func(md *workercore.ModelDeployment) {
		template := *md.Spec.Roles[1].Template
		template.Ports = []workercore.InstancePort{{Port: 8100}}
		md.Spec.Roles[1].Template = &template
	})

	_, err := renderModelDeploymentRouterObjects(context.Background(), md)
	require.EqualError(t, err, "router requires every role to use the same serving port; got [8000 8100]")
}

func TestRenderModelDeploymentRouterObjects_ProbesBothRouterContainers(t *testing.T) {
	objects, err := renderModelDeploymentRouterObjects(context.Background(), routedModelDeployment())
	require.NoError(t, err)

	envoy := objects.Deployment.Spec.Template.Spec.Containers[0]
	require.NotNil(t, envoy.ReadinessProbe)
	assert.Equal(t, intstr.FromInt32(modelDeploymentRouterHTTPPort), envoy.ReadinessProbe.TCPSocket.Port)

	epp := objects.Deployment.Spec.Template.Spec.Containers[1]
	require.NotNil(t, epp.ReadinessProbe)
	require.NotNil(t, epp.ReadinessProbe.GRPC)
	assert.Equal(t, int32(9003), epp.ReadinessProbe.GRPC.Port)
}

func TestRenderModelDeploymentRouterObjects_CarriesRuntimeIdentityAndStreamingTrailers(t *testing.T) {
	objects, err := renderModelDeploymentRouterObjects(context.Background(), routedModelDeployment())
	require.NoError(t, err)

	epp := objects.Deployment.Spec.Template.Spec.Containers[1]
	assert.Contains(t, epp.Args, "--secure-serving=false")
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
}

func TestRenderModelDeploymentRouterObjects_UnmanagedProducerPublishesNoKVEvents(t *testing.T) {
	md := routedModelDeployment(func(md *workercore.ModelDeployment) {
		md.Spec.Roles[0].Template.Command = []string{"vllm", "serve", "custom-model"}
	})

	objects, err := renderModelDeploymentRouterObjects(context.Background(), md)
	require.NoError(t, err)

	assert.Nil(t, objects.Contract.Roles[0].KVEvents)
}

func TestRenderModelDeploymentRouterObjects_EndpointUsesRouterTransport(t *testing.T) {
	md := routedModelDeployment(func(md *workercore.ModelDeployment) {
		md.Spec.Roles[0].ExtraArgs = []string{"--ssl-keyfile=/tls/key.pem"}
	})
	objects, err := renderModelDeploymentRouterObjects(context.Background(), md)
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
			name:      "picker defaults to the mirrored image, pinned rather than following main",
			md:        routedModelDeployment(),
			container: "epp",
			want:      "gpustack/mirrored-llm-d-router-endpoint-picker:v0.10.0",
			why:       "upstream publishes this under a moving tag, so the default names a release",
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
			objects, err := renderModelDeploymentRouterObjects(context.Background(), c.md)
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
