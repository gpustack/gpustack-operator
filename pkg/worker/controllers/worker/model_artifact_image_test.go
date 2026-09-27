package worker

import (
	"context"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	core "k8s.io/api/core/v1"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrlcli "sigs.k8s.io/controller-runtime/pkg/client"
	ctrlfake "sigs.k8s.io/controller-runtime/pkg/client/fake"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	"gpustack.ai/gpustack/pkg/kubeclients/kubernetes/scheme"
)

var testImageReference = "registry.example.com/team/qwen@sha256:" + strings.Repeat("a", 64)

func imageArtifactFixture(uid types.UID) *workercore.ModelArtifact {
	ma := artifactFixture("models", true, true)
	ma.Spec.Source = workercore.ModelArtifactSource{Image: &workercore.ModelArtifactImageSource{Reference: testImageReference}}
	ma.UID = uid

	return ma
}

func imageModelInstanceFixture(nodeName string) *workercore.Instance {
	return &workercore.Instance{
		ObjectMeta: meta.ObjectMeta{Namespace: "team-a", Name: "inst"},
		Spec: workercore.InstanceSpec{
			NodeName: nodeName,
			InstanceTemplate: workercore.InstanceTemplate{AdditionalVolumes: []workercore.InstanceAdditionalVolume{
				{MountPath: "/models", Model: &workercore.InstanceModelVolumeSource{ArtifactRef: core.LocalObjectReference{Name: "qwen"}}},
			}},
		},
	}
}

func imageRuntimeNode(kubelet, containerd string) *core.Node {
	return &core.Node{
		ObjectMeta: meta.ObjectMeta{Name: "node-1", Labels: map[string]string{core.LabelHostname: "host-1"}},
		Status:     core.NodeStatus{NodeInfo: core.NodeSystemInfo{KubeletVersion: kubelet, ContainerRuntimeVersion: containerd}},
	}
}

// TestModelImageRuntimeUnsupported walks the node floors an image volume needs.
func TestModelImageRuntimeUnsupported(t *testing.T) {
	cases := []struct {
		name         string
		kubelet      string
		containerd   string
		wantFragment string
	}{
		{name: "current versions are supported", kubelet: "v1.35.5", containerd: "containerd://2.3.1"},
		{name: "an old kubelet is refused", kubelet: "v1.34.9", containerd: "containerd://2.3.1", wantFragment: "kubelet"},
		{name: "an old containerd is refused", kubelet: "v1.35.5", containerd: "containerd://2.0.0", wantFragment: "containerd"},
		{name: "a non-containerd runtime is refused", kubelet: "v1.35.5", containerd: "docker://27.0", wantFragment: "not containerd"},
		{name: "an unreadable kubelet is refused", kubelet: "", containerd: "containerd://2.3.1", wantFragment: "kubelet"},
		{name: "an unreadable containerd is refused", kubelet: "v1.35.5", containerd: "containerd://", wantFragment: "containerd"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			unsupported := modelImageRuntimeUnsupported(c.kubelet, c.containerd)
			if c.wantFragment == "" {
				assert.Empty(t, unsupported)
				return
			}
			assert.Contains(t, unsupported, c.wantFragment)
		})
	}
}

// TestInstanceModelImageDelivery walks the pinned Instance's node pre-check through the resolver.
func TestInstanceModelImageDelivery(t *testing.T) {
	t.Run("a supported node lets the pod build", func(t *testing.T) {
		cli := ctrlfake.NewClientBuilder().WithScheme(scheme.Scheme).
			WithObjects(imageArtifactFixture("uid-qwen"), imageRuntimeNode("v1.35.5", "containerd://2.3.1")).
			Build()
		models, blocked, err := (&InstanceReconciler{Client: cli}).
			resolveInstanceModelVolumes(context.Background(), imageModelInstanceFixture("node-1"))
		require.NoError(t, err)
		assert.Empty(t, blocked)
		require.Contains(t, models, 0)
		assert.Equal(t, workercore.ModelDeploymentModelDeliveryImage, models[0].Render.Delivery)
		assert.Equal(t, testImageReference, models[0].Render.ImageReference)
	})

	t.Run("an old kubelet blocks with the node and the floor named", func(t *testing.T) {
		cli := ctrlfake.NewClientBuilder().WithScheme(scheme.Scheme).
			WithObjects(imageArtifactFixture("uid-qwen"), imageRuntimeNode("v1.32.9", "containerd://2.3.1")).
			Build()
		_, blocked, err := (&InstanceReconciler{Client: cli}).
			resolveInstanceModelVolumes(context.Background(), imageModelInstanceFixture("node-1"))
		require.NoError(t, err)
		assert.Contains(t, blocked, "node-1")
		assert.Contains(t, blocked, "cannot run one")
	})

	t.Run("a missing node blocks", func(t *testing.T) {
		cli := ctrlfake.NewClientBuilder().WithScheme(scheme.Scheme).
			WithObjects(imageArtifactFixture("uid-qwen")).
			Build()
		_, blocked, err := (&InstanceReconciler{Client: cli}).
			resolveInstanceModelVolumes(context.Background(), imageModelInstanceFixture("node-1"))
		require.NoError(t, err)
		assert.Contains(t, blocked, "does not exist")
	})

	t.Run("an unpinned instance skips the node check", func(t *testing.T) {
		cli := ctrlfake.NewClientBuilder().WithScheme(scheme.Scheme).
			WithObjects(imageArtifactFixture("uid-qwen"), imageRuntimeNode("v1.32.9", "containerd://1.7.0")).
			Build()
		models, blocked, err := (&InstanceReconciler{Client: cli}).
			resolveInstanceModelVolumes(context.Background(), imageModelInstanceFixture(""))
		require.NoError(t, err)
		assert.Empty(t, blocked, "no node is known, so the pod is left to the scheduler's fate")
		require.Contains(t, models, 0)
	})
}

// TestConvertAdditionalVolumesImageDelivery renders an Instance model volume for an image artifact.
func TestConvertAdditionalVolumesImageDelivery(t *testing.T) {
	w := &modelArtifactWeights{Render: &ModelDeploymentArtifactRender{
		Delivery: workercore.ModelDeploymentModelDeliveryImage, ImageReference: testImageReference,
	}}
	avs := []workercore.InstanceAdditionalVolume{
		{MountPath: "/models", ReadOnly: false, Model: &workercore.InstanceModelVolumeSource{ArtifactRef: core.LocalObjectReference{Name: "qwen"}}},
	}

	vols, mounts := convertAdditionalVolumes(avs, map[int]*modelArtifactWeights{0: w})
	require.Len(t, vols, 1)
	require.NotNil(t, vols[0].Image, "the model volume is an image volume")
	assert.Equal(t, testImageReference, vols[0].Image.Reference)
	require.Len(t, mounts, 1)
	assert.True(t, mounts[0].ReadOnly, "a model image is read-only whatever the entry says")
	assert.Empty(t, mounts[0].SubPath, "the image root is mounted whole")
}

// TestModelPlacementImagePreference walks the soft preference built from Node.status.images.
func TestModelPlacementImagePreference(t *testing.T) {
	other := "registry.example.com/team/other@sha256:" + strings.Repeat("b", 64)
	node := func(name, hostname string, images ...string) *core.Node {
		n := &core.Node{ObjectMeta: meta.ObjectMeta{Name: name}}
		if hostname != "" {
			n.Labels = map[string]string{core.LabelHostname: hostname}
		}
		for _, img := range images {
			n.Status.Images = append(n.Status.Images, core.ContainerImage{Names: []string{img}})
		}

		return n
	}
	t.Run("nodes reporting the image are named, sorted and capped", func(t *testing.T) {
		var objs []ctrlcli.Object
		for i := 0; i < modelPlacementMaxNodes+2; i++ {
			objs = append(objs, node(
				strings.Repeat("n", 1)+strconv.Itoa(i), "host-"+strconv.Itoa(modelPlacementMaxNodes+1-i), testImageReference))
		}
		objs = append(objs, node("unrelated", "host-x", other))
		cli := ctrlfake.NewClientBuilder().WithScheme(scheme.Scheme).WithObjects(objs...).Build()

		term := modelPlacementImagePreference(context.Background(), cli, testImageReference)
		require.NotNil(t, term)
		assert.Equal(t, int32(modelPlacementPreferenceWeight), term.Weight)
		values := term.Preference.MatchExpressions[0].Values
		assert.Len(t, values, modelPlacementMaxNodes, "the list is capped")
		assert.Equal(t, "host-0", values[0], "the order is hostname-sorted, not listing order")
	})

	t.Run("a node not reporting the image and a node without a hostname are left out", func(t *testing.T) {
		cli := ctrlfake.NewClientBuilder().WithScheme(scheme.Scheme).WithObjects(
			node("holder", "host-1", other, testImageReference),
			node("other-image", "host-2", other),
			node("no-hostname", "", testImageReference),
		).Build()

		term := modelPlacementImagePreference(context.Background(), cli, testImageReference)
		require.NotNil(t, term)
		assert.Equal(t, []string{"host-1"}, term.Preference.MatchExpressions[0].Values)
	})

	t.Run("no node reporting yields no term", func(t *testing.T) {
		cli := ctrlfake.NewClientBuilder().WithScheme(scheme.Scheme).WithObjects(
			node("other-image", "host-2", other),
		).Build()

		assert.Nil(t, modelPlacementImagePreference(context.Background(), cli, testImageReference))
	})

	t.Run("another delivery yields no term", func(t *testing.T) {
		pvc := &modelArtifactWeights{Render: &ModelDeploymentArtifactRender{
			Delivery: workercore.ModelDeploymentModelDeliveryPvc, ClaimName: "models",
		}}
		assert.Nil(t, pvc.placementPreference(context.Background(),
			ctrlfake.NewClientBuilder().WithScheme(scheme.Scheme).Build()))
	})
}
