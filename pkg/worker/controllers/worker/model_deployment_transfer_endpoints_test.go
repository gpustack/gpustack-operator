package worker

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apps "k8s.io/api/apps/v1"
	core "k8s.io/api/core/v1"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrlcli "sigs.k8s.io/controller-runtime/pkg/client"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	"gpustack.ai/gpustack/pkg/setting"
)

func TestModelTransferDiscoveryIsolatesInvalidTrustAnchors(t *testing.T) {
	_, ca, _ := newClientCertificate(t)
	for _, tc := range []struct {
		name  string
		badCA []byte
	}{
		{name: "invalid PEM", badCA: []byte("invalid")},
		{name: "oversized", badCA: []byte(strings.Repeat("x", (256<<10)+1))},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ns := setting.DelegatedSecretNamespace
			md := &workercore.ModelDeployment{ObjectMeta: meta.ObjectMeta{Name: "consumer", Namespace: "tenant", UID: "consumer-uid"}}
			objects := []ctrlcli.Object{md}
			for _, source := range []struct {
				name, vendor string
				ca           []byte
			}{
				{name: "good", vendor: "nvidia", ca: ca},
				{name: "bad", vendor: "ascend", ca: tc.badCA},
			} {
				labels := map[string]string{"app.kubernetes.io/component": "model-transfer", "gpustack.ai/manufacturer": source.vendor}
				ds := &apps.DaemonSet{ObjectMeta: meta.ObjectMeta{Name: source.name, Namespace: ns, UID: types.UID(source.name), Labels: labels}}
				svc := &core.Service{ObjectMeta: meta.ObjectMeta{Name: source.name, Namespace: ns, Labels: labels}, Spec: core.ServiceSpec{
					ClusterIP: core.ClusterIPNone, Selector: labels, Ports: []core.ServicePort{{Name: "sessions", Port: 9251}},
				}}
				secret := &core.Secret{ObjectMeta: meta.ObjectMeta{Name: source.name + "-cert", Namespace: ns}, Data: map[string][]byte{"ca.crt": source.ca}}
				pod := &core.Pod{
					ObjectMeta: meta.ObjectMeta{
						Name: source.name, Namespace: ns, UID: types.UID(source.name + "-pod"), Labels: labels,
						OwnerReferences: []meta.OwnerReference{*meta.NewControllerRef(ds, apps.SchemeGroupVersion.WithKind("DaemonSet"))},
					},
					Spec: core.PodSpec{NodeName: "node"}, Status: core.PodStatus{Phase: core.PodRunning, PodIP: "192.0.2.7"},
				}
				objects = append(objects, ds, svc, secret, pod)
			}
			cli := modelTransferTestEnv(t, objects...)
			r := &ModelDeploymentReconciler{Client: cli}
			weights := &modelArtifactWeights{Render: modelTransferRemoteRender()}
			require.NoError(t, r.reconcileModelTransferClientConfig(context.Background(), md, weights))
			cm := new(core.ConfigMap)
			require.NoError(t, cli.Get(context.Background(), ctrlcli.ObjectKey{Namespace: md.Namespace, Name: weights.Render.ModelTransferConfigMap}, cm))
			var discovery struct {
				Endpoints []modelTransferEndpoint `json:"endpoints"`
			}
			require.NoError(t, json.Unmarshal([]byte(cm.Data["endpoints.json"]), &discovery))
			require.Len(t, discovery.Endpoints, 1)
			require.Equal(t, "good-pod", discovery.Endpoints[0].PodUID)
		})
	}
}

func TestModelTransferClientDiscoveryAndTrust(t *testing.T) {
	_, ca, _ := newClientCertificate(t)
	ns := setting.DelegatedSecretNamespace
	labels := map[string]string{"app.kubernetes.io/component": "model-transfer", "gpustack.ai/manufacturer": "nvidia"}
	ds := &apps.DaemonSet{ObjectMeta: meta.ObjectMeta{Name: "source", Namespace: ns, UID: "daemon-uid", Labels: labels}}
	svc := &core.Service{
		ObjectMeta: meta.ObjectMeta{Name: "source", Namespace: ns, Labels: labels},
		Spec: core.ServiceSpec{
			ClusterIP: core.ClusterIPNone, Selector: labels,
			Ports: []core.ServicePort{{Name: "sessions", Port: 9251}},
		},
	}
	secret := &core.Secret{
		ObjectMeta: meta.ObjectMeta{Name: "source-cert", Namespace: ns}, Type: core.SecretTypeTLS,
		Data: map[string][]byte{"ca.crt": ca, "tls.key": []byte("PRIVATE-KEY-NEVER-MIRRORED")},
	}
	owner := meta.NewControllerRef(ds, apps.SchemeGroupVersion.WithKind("DaemonSet"))
	pod := &core.Pod{
		ObjectMeta: meta.ObjectMeta{
			Name: "source-1", Namespace: ns, UID: "source-pod-uid", Labels: labels,
			OwnerReferences: []meta.OwnerReference{*owner},
		}, Spec: core.PodSpec{NodeName: "source-node"},
		Status: core.PodStatus{Phase: core.PodRunning, PodIP: "192.0.2.7"},
	}
	md := &workercore.ModelDeployment{ObjectMeta: meta.ObjectMeta{Name: "consumer", Namespace: "tenant", UID: "consumer-uid"}}
	cli := modelTransferTestEnv(t, svc, secret, ds, pod, md)
	r := &ModelDeploymentReconciler{Client: cli}
	weights := &modelArtifactWeights{Render: modelTransferRemoteRender()}
	require.NoError(t, r.reconcileModelTransferClientConfig(context.Background(), md, weights))
	name := weights.Render.ModelTransferConfigMap
	require.NotEmpty(t, name)
	cm := new(core.ConfigMap)
	require.NoError(t, cli.Get(context.Background(), ctrlcli.ObjectKey{Namespace: "tenant", Name: name}, cm))
	assert.Equal(t, md.UID, meta.GetControllerOf(cm).UID)
	encoded, err := json.Marshal(cm.Data)
	require.NoError(t, err)
	assert.NotContains(t, string(encoded), "PRIVATE-KEY")
	var config struct {
		Version   int                     `json:"version"`
		Endpoints []modelTransferEndpoint `json:"endpoints"`
	}
	require.NoError(t, json.Unmarshal([]byte(cm.Data["endpoints.json"]), &config))
	require.Len(t, config.Endpoints, 1)
	assert.Equal(t, 1, config.Version)
	endpoint := config.Endpoints[0]
	assert.Equal(t, "192.0.2.7:9251", endpoint.Address)
	assert.Equal(t, "source."+ns+".svc", endpoint.ServerName)
	assert.Equal(t, "source-pod-uid", endpoint.PodUID)
	assert.Equal(t, string(ca), cm.Data[strings.TrimPrefix(endpoint.CAFile, modelTransferClientMount+"/")])

	// Recreated source Pods change discovery data without changing the volume name.
	current := new(core.Pod)
	require.NoError(t, cli.Get(context.Background(), ctrlcli.ObjectKeyFromObject(pod), current))
	current.Status.PodIP = "192.0.2.8"
	require.NoError(t, cli.Status().Update(context.Background(), current))
	require.NoError(t, r.reconcileModelTransferClientConfig(context.Background(), md, weights))
	assert.Equal(t, name, weights.Render.ModelTransferConfigMap)
	require.NoError(t, cli.Get(context.Background(), ctrlcli.ObjectKey{Namespace: "tenant", Name: name}, cm))
	assert.Contains(t, cm.Data["endpoints.json"], "192.0.2.8:9251")

	volumes, mounts := weights.Render.modelTransferVolumes()
	assert.Equal(t, name, volumes[len(volumes)-1].ConfigMap.Name)
	assert.Equal(t, modelTransferClientMount, mounts[len(mounts)-1].MountPath)
	assert.True(t, mounts[len(mounts)-1].ReadOnly)
	container := new(core.Container)
	weights.Render.applyModelTransfer(container)
	identities := map[string]string{}
	for _, env := range container.Env {
		if env.ValueFrom != nil && env.ValueFrom.FieldRef != nil {
			identities[env.Name] = env.ValueFrom.FieldRef.FieldPath
		}
	}
	assert.Equal(t, "metadata.uid", identities["KUBERNETES_POD_UID"])
	assert.Equal(t, "metadata.namespace", identities["KUBERNETES_POD_NAMESPACE"])
}

func TestModelTransferClientConfigRefusesForeignOwner(t *testing.T) {
	md := &workercore.ModelDeployment{ObjectMeta: meta.ObjectMeta{Name: "consumer", Namespace: "tenant", UID: "consumer-uid"}}
	cli := modelTransferTestEnv(t, md)
	r := &ModelDeploymentReconciler{Client: cli}
	weights := &modelArtifactWeights{Render: modelTransferRemoteRender()}
	require.NoError(t, r.reconcileModelTransferClientConfig(context.Background(), md, weights))
	cm := new(core.ConfigMap)
	require.NoError(t, cli.Get(context.Background(), ctrlcli.ObjectKey{Namespace: md.Namespace, Name: weights.Render.ModelTransferConfigMap}, cm))
	cm.OwnerReferences[0].UID = "somebody-else"
	require.NoError(t, cli.Update(context.Background(), cm))
	require.ErrorContains(t, r.reconcileModelTransferClientConfig(context.Background(), md, weights), "owned by another")
}

func TestModelTransferDiscoveryReusesPodInventory(t *testing.T) {
	_, ca, _ := newClientCertificate(t)
	ns := setting.DelegatedSecretNamespace
	objects := make([]ctrlcli.Object, 0, 8)
	for _, name := range []string{"first", "second"} {
		labels := map[string]string{"app.kubernetes.io/component": "model-transfer", "gpustack.ai/manufacturer": "nvidia", "source": name}
		ds := &apps.DaemonSet{ObjectMeta: meta.ObjectMeta{Name: name, Namespace: ns, UID: types.UID(name), Labels: labels}}
		service := &core.Service{ObjectMeta: meta.ObjectMeta{Name: name, Namespace: ns, Labels: labels}, Spec: core.ServiceSpec{
			ClusterIP: core.ClusterIPNone, Selector: labels, Ports: []core.ServicePort{{Name: "sessions", Port: 9251}},
		}}
		secret := &core.Secret{ObjectMeta: meta.ObjectMeta{Name: name + "-cert", Namespace: ns}, Data: map[string][]byte{"ca.crt": ca}}
		pod := &core.Pod{
			ObjectMeta: meta.ObjectMeta{
				Name: name, Namespace: ns, UID: types.UID(name + "-pod"), Labels: labels,
				OwnerReferences: []meta.OwnerReference{*meta.NewControllerRef(ds, apps.SchemeGroupVersion.WithKind("DaemonSet"))},
			},
			Spec: core.PodSpec{NodeName: "node"}, Status: core.PodStatus{Phase: core.PodRunning, PodIP: "192.0.2.7"},
		}
		objects = append(objects, ds, service, secret, pod)
	}
	cli := modelTransferTestEnv(t, objects...)
	counter := &sourceInventoryClient{Client: cli, lists: map[string]int{}}
	r := &ModelDeploymentReconciler{Client: counter}
	md := &workercore.ModelDeployment{ObjectMeta: meta.ObjectMeta{Name: "consumer", Namespace: "team-a", UID: "consumer"}}
	weights := &modelArtifactWeights{Render: modelTransferRemoteRender()}
	require.NoError(t, r.reconcileModelTransferClientConfig(t.Context(), md, weights))
	require.Equal(t, 1, counter.lists["*v1.PodList"])
	cm := new(core.ConfigMap)
	require.NoError(t, cli.Get(t.Context(), ctrlcli.ObjectKey{Namespace: md.Namespace, Name: weights.Render.ModelTransferConfigMap}, cm))
	var discovery struct {
		Endpoints []modelTransferEndpoint `json:"endpoints"`
	}
	require.NoError(t, json.Unmarshal([]byte(cm.Data["endpoints.json"]), &discovery))
	require.Len(t, discovery.Endpoints, 2)
	require.Equal(t, "first-pod", discovery.Endpoints[0].PodUID)
	require.Equal(t, "second-pod", discovery.Endpoints[1].PodUID)
}
