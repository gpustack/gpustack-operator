package worker

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	core "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrlcli "sigs.k8s.io/controller-runtime/pkg/client"

	worker "gpustack.ai/gpustack/api/worker/v1"
	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
)

func modelVolumeInstance() *workercore.Instance {
	q := resource.MustParse
	cpu, ram, ls := q("1"), q("2Gi"), q("10Gi")

	return &workercore.Instance{
		ObjectMeta: meta.ObjectMeta{Namespace: "team-a", Name: "inst"},
		Spec: workercore.InstanceSpec{
			Type: "generic-type",
			InstanceTemplate: workercore.InstanceTemplate{
				Image:     "img",
				Resources: &workercore.InstanceResources{CPU: cpu, RAM: ram, LocalStorage: ls},
				AdditionalVolumes: []workercore.InstanceAdditionalVolume{
					{MountPath: "/models/qwen", Model: &workercore.InstanceModelVolumeSource{ArtifactRef: core.LocalObjectReference{Name: "qwen"}}},
				},
			},
			Volume: workercore.InstanceVolume{Ephemeral: &workercore.InstanceEphemeralVolume{Capacity: q("10Gi")}},
		},
	}
}

func TestInstanceModelVolumeResolution(t *testing.T) {
	cases := []struct {
		name     string
		objs     []ctrlcli.Object
		wantWait string
	}{
		{name: "a missing artifact waits", wantWait: `ModelArtifact "qwen" does not exist`},
		{name: "a hub artifact is reported", objs: []ctrlcli.Object{artifactFixture("", true, true)}, wantWait: "not on a PersistentVolumeClaim"},
		{
			name:     "a claim that cannot bind waits",
			objs:     []ctrlcli.Object{artifactFixture("models", true, true), claimFixture(core.ClaimPending, "", core.ReadWriteOnce)},
			wantWait: "is Pending",
		},
		{
			name: "a bound claim that is not shared serves one Instance",
			objs: []ctrlcli.Object{artifactFixture("models", true, true), claimFixture(core.ClaimBound, "local", core.ReadWriteOnce), volumeFixture("node-1")},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cli := buildInstanceClient(c.objs...)
			r := &InstanceReconciler{Client: cli, APIReader: cli}

			models, wait, err := r.resolveInstanceModelVolumes(context.Background(), modelVolumeInstance())
			require.NoError(t, err)
			if c.wantWait != "" {
				assert.Contains(t, wait, c.wantWait)
				return
			}
			assert.Empty(t, wait)
			require.Contains(t, models, 0)
		})
	}
}

func TestInstanceModelVolumeRender(t *testing.T) {
	cli := buildInstanceClient(artifactFixture("models", true, true),
		claimFixture(core.ClaimBound, "local", core.ReadWriteOnce), volumeFixture("node-1"))
	r := &InstanceReconciler{Client: cli, APIReader: cli}
	inst := modelVolumeInstance()
	inst.Spec.AdditionalVolumes[0].ReadOnly = false

	models, wait, err := r.resolveInstanceModelVolumes(context.Background(), inst)
	require.NoError(t, err)
	require.Empty(t, wait)
	pod := r.convertPodFromInstance(context.Background(), inst,
		&worker.InstanceType{ObjectMeta: meta.ObjectMeta{Name: "generic-type"}}, models)

	vol := findVolume(pod, additionalVolumeName(0))
	require.NotNil(t, vol)
	require.NotNil(t, vol.PersistentVolumeClaim)
	assert.Equal(t, "models", vol.PersistentVolumeClaim.ClaimName)
	assert.True(t, vol.PersistentVolumeClaim.ReadOnly)
	mount := findMount(&pod.Spec.Containers[0], additionalVolumeName(0))
	require.NotNil(t, mount)
	assert.Equal(t, "/models/qwen", mount.MountPath)
	assert.Equal(t, "qwen", mount.SubPath, "the artifact's own path")
	assert.True(t, mount.ReadOnly, "read-only whatever the entry says")
	require.NotNil(t, pod.Spec.Affinity)
	assert.Equal(t, volumeFixture("node-1").Spec.NodeAffinity.Required,
		pod.Spec.Affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution)
}

func TestInstanceModelVolumeReconcileWaits(t *testing.T) {
	instType := &worker.InstanceType{
		ObjectMeta: meta.ObjectMeta{Name: "generic-type"},
		Status:     workercore.InstanceTypeStatus{Entrance: "queue-for-generic-type"},
	}
	cases := []struct {
		name     string
		volumes  bool
		wantPods int
	}{
		// The baseline shows this fixture reaches the create at all, so the wait below is the
		// model volume's and not an earlier guard's.
		{name: "without a model volume the Pod is built", wantPods: 1},
		{name: "a model volume on a missing artifact builds nothing", volumes: true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			inst := modelVolumeInstance()
			if !c.volumes {
				inst.Spec.AdditionalVolumes = nil
			}
			cli := buildInstanceClient(inst, instType.DeepCopy())
			_, err := reconcileInstance(t, cli, "team-a", "inst")
			require.NoError(t, err)

			pods := new(core.PodList)
			require.NoError(t, cli.List(context.Background(), pods, ctrlcli.InNamespace("team-a")))
			assert.Len(t, pods.Items, c.wantPods)
			if c.volumes {
				got := new(workercore.Instance)
				require.NoError(t, cli.Get(context.Background(), ctrlcli.ObjectKey{Namespace: "team-a", Name: "inst"}, got))
				assert.Contains(t, got.Status.PhaseMessage, `ModelArtifact "qwen" does not exist`)
			}
		})
	}
}
