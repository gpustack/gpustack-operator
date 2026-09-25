package worker

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	core "k8s.io/api/core/v1"
	storage "k8s.io/api/storage/v1"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrlcli "sigs.k8s.io/controller-runtime/pkg/client"

	worker "gpustack.ai/gpustack/api/worker/v1"
	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
)

// persistentVolumeInstance is an Instance mounting the claim "models" as its workspace, or, with
// additional, as an additional volume beside an ephemeral workspace.
func persistentVolumeInstance(additional bool) *workercore.Instance {
	inst := modelVolumeInstance()
	inst.Spec.AdditionalVolumes = nil
	if !additional {
		inst.Spec.Volume = workercore.InstanceVolume{Persistent: &core.LocalObjectReference{Name: "models"}}
		return inst
	}
	inst.Spec.AdditionalVolumes = []workercore.InstanceAdditionalVolume{
		{MountPath: "/mnt/models", Persistent: &core.LocalObjectReference{Name: "models"}},
	}

	return inst
}

// withInstancePersistentVolumePlacement sets the escape Setting for one test.
func withInstancePersistentVolumePlacement(t *testing.T, on bool) {
	t.Helper()
	orig := instancePersistentVolumePlacement
	t.Cleanup(func() { instancePersistentVolumePlacement = orig })
	instancePersistentVolumePlacement = func(context.Context) bool { return on }
}

func TestInstancePersistentVolumeResolution(t *testing.T) {
	cases := []struct {
		name         string
		additional   bool
		both         bool
		ephemeral    bool
		placementOff bool
		objs         []ctrlcli.Object
		wantWait     string
		wantAffinity bool
	}{
		{
			name:         "a bound workspace claim carries its PV's node affinity",
			objs:         []ctrlcli.Object{claimFixture(core.ClaimBound, "local", core.ReadWriteOnce), volumeFixture("node-1")},
			wantAffinity: true,
		},
		{
			name:         "a bound additional claim carries its PV's node affinity",
			additional:   true,
			objs:         []ctrlcli.Object{claimFixture(core.ClaimBound, "local", core.ReadWriteOnce), volumeFixture("node-1")},
			wantAffinity: true,
		},
		{
			name:         "a claim mounted as the workspace and an additional volume is read once",
			both:         true,
			objs:         []ctrlcli.Object{claimFixture(core.ClaimBound, "local", core.ReadWriteOnce), volumeFixture("node-1")},
			wantAffinity: true,
		},
		{
			name: "a bound claim on a PV reachable from every node adds nothing",
			objs: []ctrlcli.Object{claimFixture(core.ClaimBound, "nfs", core.ReadWriteMany), volumeFixture("")},
		},
		{
			name: "an unbound claim on an immediate class waits",
			objs: []ctrlcli.Object{
				claimFixture(core.ClaimPending, "fast", core.ReadWriteOnce),
				classFixture("fast", "csi.example.com", storage.VolumeBindingImmediate),
			},
			wantWait: `PersistentVolumeClaim "models" is Pending`,
		},
		{
			name:       "an unbound additional claim on an immediate class waits",
			additional: true,
			objs: []ctrlcli.Object{
				claimFixture(core.ClaimPending, "fast", core.ReadWriteOnce),
				classFixture("fast", "csi.example.com", storage.VolumeBindingImmediate),
			},
			wantWait: `PersistentVolumeClaim "models" is Pending`,
		},
		{
			name: "an unbound claim provisioned on the first Pod's node adds nothing",
			objs: []ctrlcli.Object{
				claimFixture(core.ClaimPending, "local-path", core.ReadWriteOnce),
				classFixture("local-path", "rancher.io/local-path", storage.VolumeBindingWaitForFirstConsumer),
			},
		},
		{
			// The static local-volume class: TAS would pick the node before the binder looks
			// for a PV there, so the claim must be bound first.
			name: "an unbound claim on a class without a provisioner waits",
			objs: []ctrlcli.Object{
				claimFixture(core.ClaimPending, "local-storage", core.ReadWriteOnce),
				classFixture("local-storage", modelArtifactNoProvisioner, storage.VolumeBindingWaitForFirstConsumer),
			},
			wantWait: `PersistentVolumeClaim "models" is Pending`,
		},
		{
			name:     "a missing claim waits",
			wantWait: `PersistentVolumeClaim "models" does not exist`,
		},
		{
			name:      "an ephemeral workspace reads no claim",
			ephemeral: true,
		},
		{
			name:         "with the Setting off an unbound claim does not wait",
			placementOff: true,
			objs: []ctrlcli.Object{
				claimFixture(core.ClaimPending, "fast", core.ReadWriteOnce),
				classFixture("fast", "csi.example.com", storage.VolumeBindingImmediate),
			},
		},
		{
			name:         "with the Setting off a bound claim adds nothing",
			placementOff: true,
			objs:         []ctrlcli.Object{claimFixture(core.ClaimBound, "local", core.ReadWriteOnce), volumeFixture("node-1")},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			withInstancePersistentVolumePlacement(t, !c.placementOff)
			inst := persistentVolumeInstance(c.additional)
			if c.ephemeral {
				inst = modelVolumeInstance()
				inst.Spec.AdditionalVolumes = nil
			}
			if c.both {
				inst.Spec.AdditionalVolumes = persistentVolumeInstance(true).Spec.AdditionalVolumes
			}
			cli := buildInstanceClient(c.objs...)
			r := &InstanceReconciler{Client: cli, APIReader: cli}

			affinities, wait, err := r.resolveInstancePersistentVolumes(context.Background(), inst)
			require.NoError(t, err)
			if c.wantWait != "" {
				assert.Contains(t, wait, c.wantWait)
				assert.Empty(t, affinities)
				return
			}
			assert.Empty(t, wait)
			if !c.wantAffinity {
				assert.Empty(t, affinities)
				return
			}
			require.Len(t, affinities, 1)
			assert.Equal(t, volumeFixture("node-1").Spec.NodeAffinity.Required, affinities[0])
		})
	}
}

func TestInstancePersistentVolumeReconcile(t *testing.T) {
	instType := &worker.InstanceType{
		ObjectMeta: meta.ObjectMeta{Name: "generic-type"},
		Status:     workercore.InstanceTypeStatus{Entrance: "queue-for-generic-type"},
	}
	bound := []ctrlcli.Object{claimFixture(core.ClaimBound, "local", core.ReadWriteOnce), volumeFixture("node-1")}
	immediate := []ctrlcli.Object{
		claimFixture(core.ClaimPending, "fast", core.ReadWriteOnce),
		classFixture("fast", "csi.example.com", storage.VolumeBindingImmediate),
	}
	cases := []struct {
		name         string
		placementOff bool
		objs         []ctrlcli.Object
		wantPod      bool
		wantAffinity bool
		wantMessage  string
	}{
		{name: "a bound claim's Pod is placed on its PV's node", objs: bound, wantPod: true, wantAffinity: true},
		{
			name:        "an unbound claim on an immediate class builds no Pod and says why",
			objs:        immediate,
			wantMessage: `PersistentVolumeClaim "models" is Pending`,
		},
		// The escape Setting restores the render that knew only the claim names.
		{name: "with the Setting off an unbound claim still builds the Pod", placementOff: true, objs: immediate, wantPod: true},
		{name: "with the Setting off a bound claim's Pod carries no affinity", placementOff: true, objs: bound, wantPod: true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			withInstancePersistentVolumePlacement(t, !c.placementOff)
			cli := buildInstanceClient(append([]ctrlcli.Object{persistentVolumeInstance(false), instType.DeepCopy()}, c.objs...)...)
			_, err := reconcileInstance(t, cli, "team-a", "inst")
			require.NoError(t, err)

			pods := new(core.PodList)
			require.NoError(t, cli.List(context.Background(), pods, ctrlcli.InNamespace("team-a")))
			if !c.wantPod {
				assert.Empty(t, pods.Items)
				got := new(workercore.Instance)
				require.NoError(t, cli.Get(context.Background(), ctrlcli.ObjectKey{Namespace: "team-a", Name: "inst"}, got))
				assert.Equal(t, InstancePhaseStarting, got.Status.Phase)
				assert.Contains(t, got.Status.PhaseMessage, c.wantMessage)
				return
			}
			require.Len(t, pods.Items, 1)
			pod := &pods.Items[0]
			vol := findVolume(pod, "workspace")
			require.NotNil(t, vol)
			require.NotNil(t, vol.PersistentVolumeClaim)
			assert.Equal(t, "models", vol.PersistentVolumeClaim.ClaimName)
			if !c.wantAffinity {
				assert.Nil(t, pod.Spec.Affinity)
				return
			}
			require.NotNil(t, pod.Spec.Affinity)
			require.NotNil(t, pod.Spec.Affinity.NodeAffinity)
			assert.Equal(t, volumeFixture("node-1").Spec.NodeAffinity.Required,
				pod.Spec.Affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution)
		})
	}
}
