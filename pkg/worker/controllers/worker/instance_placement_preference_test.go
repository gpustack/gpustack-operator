package worker

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	core "k8s.io/api/core/v1"
	storage "k8s.io/api/storage/v1"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrlcli "sigs.k8s.io/controller-runtime/pkg/client"

	worker "gpustack.ai/gpustack/api/worker/v1"
	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	"gpustack.ai/gpustack/pkg/modelstore"
)

func TestInstancePlacementPreference(t *testing.T) {
	otherDigest := "sha256:" + strings.Repeat("3", 64)
	llama := artifactFixture("", true, true)
	llama.Name = "llama"
	llama.Status.Resolved.ManifestDigest = otherDigest
	// n2 holds llama's digest, ready to mount.
	n2 := placementStore("n2", workercore.NodeModelStoreModelStateReady, meta.ConditionTrue)
	n2.Status.Models[0].Digest = otherDigest
	driver := &storage.CSIDriver{ObjectMeta: meta.ObjectMeta{Name: modelstore.DriverName}}
	// claimArtifact is a claim artifact named models, beside the hub ones.
	claimArtifact := func() *workercore.ModelArtifact {
		ma := artifactFixture("models", true, true)
		ma.Name = "models"
		return ma
	}
	volume := func(artifact string) workercore.InstanceAdditionalVolume {
		return workercore.InstanceAdditionalVolume{
			MountPath: "/models/" + artifact,
			Model:     &workercore.InstanceModelVolumeSource{ArtifactRef: core.LocalObjectReference{Name: artifact}},
		}
	}
	hosts := func(values ...string) core.PreferredSchedulingTerm {
		return core.PreferredSchedulingTerm{Weight: modelPlacementPreferenceWeight, Preference: core.NodeSelectorTerm{
			MatchExpressions: []core.NodeSelectorRequirement{{Key: core.LabelHostname, Operator: core.NodeSelectorOpIn, Values: values}},
		}}
	}

	cases := []struct {
		name    string
		volumes []workercore.InstanceAdditionalVolume
		objs    []ctrlcli.Object
		want    []core.PreferredSchedulingTerm
		// required is the required node affinity the Pod must carry beside its preference.
		required *core.NodeSelector
	}{
		{
			name: "a hub artifact on a hot node", volumes: []workercore.InstanceAdditionalVolume{volume("qwen")},
			objs: append(hotNode("n1"), artifactFixture("", true, true), driver),
			want: []core.PreferredSchedulingTerm{hosts("n1")},
		},
		{
			name: "one digest mounted twice is one term", volumes: []workercore.InstanceAdditionalVolume{volume("qwen"), volume("qwen")},
			objs: append(hotNode("n1"), artifactFixture("", true, true), driver),
			want: []core.PreferredSchedulingTerm{hosts("n1")},
		},
		{
			name:    "two digests are two terms, in volume order",
			volumes: []workercore.InstanceAdditionalVolume{volume("llama"), volume("qwen")},
			objs: append(append(hotNode("n1"), n2, placementCSINode("n2", true), placementNode("n2", "n2")),
				artifactFixture("", true, true), llama, driver),
			want: []core.PreferredSchedulingTerm{hosts("n2"), hosts("n1")},
		},
		{
			name: "a hub artifact no node holds", volumes: []workercore.InstanceAdditionalVolume{volume("qwen")},
			objs: []ctrlcli.Object{artifactFixture("", true, true), driver},
		},
		{
			name:    "a claim and a hub artifact carry the claim's affinity and the hub artifact's term",
			volumes: []workercore.InstanceAdditionalVolume{volume("models"), volume("llama")},
			objs: append([]ctrlcli.Object{
				n2, placementCSINode("n2", true), placementNode("n2", "n2"),
				claimArtifact(), claimFixture(core.ClaimBound, "local", core.ReadWriteOnce), volumeFixture("node-1"),
			},
				llama, driver),
			want:     []core.PreferredSchedulingTerm{hosts("n2")},
			required: volumeFixture("node-1").Spec.NodeAffinity.Required,
		},
		{name: "no volume", objs: append(hotNode("n1"), driver)},
		{
			name: "a claim", volumes: []workercore.InstanceAdditionalVolume{volume("qwen")},
			objs: append(hotNode("n1"), artifactFixture("models", true, true),
				claimFixture(core.ClaimBound, "local", core.ReadWriteOnce), volumeFixture("")),
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cli := buildInstanceClient(c.objs...)
			r := &InstanceReconciler{Client: cli, APIReader: cli}
			inst := modelVolumeInstance()
			inst.Spec.AdditionalVolumes = c.volumes

			models, wait, err := r.resolveInstanceModelVolumes(context.Background(), inst)
			require.NoError(t, err)
			require.Empty(t, wait)
			pod := r.convertPodFromInstance(context.Background(), inst,
				&worker.InstanceType{ObjectMeta: meta.ObjectMeta{Name: "generic-type"}}, models)

			assert.Equal(t, c.want, podPreference(pod))
			var required *core.NodeSelector
			if pod.Spec.Affinity != nil && pod.Spec.Affinity.NodeAffinity != nil {
				required = pod.Spec.Affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution
			}
			assert.Equal(t, c.required, required)
		})
	}
}

// TestInstancePlacementPreferenceBesidePersistentVolumeAffinity drives the reconciler, where the
// persistent claim's required node affinity is added after the Pod is built with its preference: a
// Pod whose workspace claim is bound to a node-pinned PV and whose hub artifact is hot elsewhere
// carries both, neither replacing the other.
func TestInstancePlacementPreferenceBesidePersistentVolumeAffinity(t *testing.T) {
	withInstancePersistentVolumePlacement(t, true)
	instType := &worker.InstanceType{
		ObjectMeta: meta.ObjectMeta{Name: "generic-type"},
		Status:     workercore.InstanceTypeStatus{Entrance: "queue-for-generic-type"},
	}
	inst := persistentVolumeInstance(false)
	inst.Spec.AdditionalVolumes = []workercore.InstanceAdditionalVolume{{
		MountPath: "/models/qwen",
		Model:     &workercore.InstanceModelVolumeSource{ArtifactRef: core.LocalObjectReference{Name: "qwen"}},
	}}
	objs := append(hotNode("n1"), inst, instType, artifactFixture("", true, true),
		&storage.CSIDriver{ObjectMeta: meta.ObjectMeta{Name: modelstore.DriverName}},
		claimFixture(core.ClaimBound, "local", core.ReadWriteOnce), volumeFixture("node-1"))
	cli := buildInstanceClient(objs...)

	_, err := reconcileInstance(t, cli, "team-a", "inst")
	require.NoError(t, err)

	pods := new(core.PodList)
	require.NoError(t, cli.List(context.Background(), pods, ctrlcli.InNamespace("team-a")))
	require.Len(t, pods.Items, 1)
	na := pods.Items[0].Spec.Affinity.NodeAffinity
	require.NotNil(t, na)
	assert.Equal(t, volumeFixture("node-1").Spec.NodeAffinity.Required, na.RequiredDuringSchedulingIgnoredDuringExecution)
	assert.Equal(t, []core.PreferredSchedulingTerm{{
		Weight: modelPlacementPreferenceWeight,
		Preference: core.NodeSelectorTerm{MatchExpressions: []core.NodeSelectorRequirement{
			{Key: core.LabelHostname, Operator: core.NodeSelectorOpIn, Values: []string{"n1"}},
		}},
	}}, na.PreferredDuringSchedulingIgnoredDuringExecution)
}
