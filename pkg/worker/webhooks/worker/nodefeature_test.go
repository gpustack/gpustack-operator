package worker

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrlfake "sigs.k8s.io/controller-runtime/pkg/client/fake"
	nfd "sigs.k8s.io/node-feature-discovery/api/nfd/v1alpha1"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	"gpustack.ai/gpustack/pkg/kubeclients/kubernetes/scheme"
	"gpustack.ai/gpustack/pkg/nodefeature"
)

const _a10gPartitionsKey = nodefeature.AcceleratableFeatureLabelPrefix + "nvidia-a10g" + nodefeature.SlicedPartitionsLabelSuffix

// newWorkerNodeFeature builds a "${node}-gpustack-worker" NodeFeature carrying the
// given spec labels, mirroring what NodeFeatureReconciler produces.
func newWorkerNodeFeature(node string, specLabels map[string]string) *nfd.NodeFeature {
	return &nfd.NodeFeature{
		ObjectMeta: meta.ObjectMeta{
			Name: node + "-gpustack-worker",
			Labels: map[string]string{
				nfd.NodeFeatureObjNodeNameLabel: node,
			},
		},
		Spec: nfd.NodeFeatureSpec{Labels: specLabels},
	}
}

// devicesWithMaxPartitions builds a Devices CR for node with a single nvidia group
// of the given id whose accelerators report the given MaxPartitions.
func devicesWithMaxPartitions(node, id string, maxPartitions int32) *workercore.Devices {
	return &workercore.Devices{
		ObjectMeta: meta.ObjectMeta{Name: node},
		Spec: workercore.DevicesSpec{
			Groups: []workercore.DevicesGroup{{
				ID:           id,
				Manufacturer: "nvidia",
				Name:         id,
				Accelerators: []workercore.Accelerator{
					{Features: workercore.AcceleratorFeatures{MaxPartitions: maxPartitions}},
				},
			}},
		},
	}
}

func TestNodeFeatureWebhook_validate(t *testing.T) {
	cases := []struct {
		name    string
		nf      *nfd.NodeFeature
		devices *workercore.Devices // optional Devices CR seeded into the client
		wantErr bool
	}{
		{
			name:    "non-worker node feature is not validated",
			nf:      &nfd.NodeFeature{ObjectMeta: meta.ObjectMeta{Name: "node-5-gpustack-device-manager"}},
			wantErr: false,
		},
		{
			name: "worker node feature without node-name label is not validated",
			nf: &nfd.NodeFeature{
				ObjectMeta: meta.ObjectMeta{Name: "node-5-gpustack-worker"},
				Spec:       nfd.NodeFeatureSpec{Labels: map[string]string{_a10gPartitionsKey: "3"}},
			},
			wantErr: false,
		},
		{
			name:    "no sliced.partitions labels",
			nf:      newWorkerNodeFeature("node-5", map[string]string{"acceleratable.feature.gpustack.ai/nvidia-a10g.count": "4"}),
			wantErr: false,
		},
		{
			name:    "valid power of two without devices is allowed",
			nf:      newWorkerNodeFeature("node-5", map[string]string{_a10gPartitionsKey: "8"}),
			wantErr: false,
		},
		{
			name:    "non power of two is rejected",
			nf:      newWorkerNodeFeature("node-5", map[string]string{_a10gPartitionsKey: "3"}),
			wantErr: true,
		},
		{
			name:    "above max size is rejected",
			nf:      newWorkerNodeFeature("node-5", map[string]string{_a10gPartitionsKey: "1024"}),
			wantErr: true,
		},
		{
			name:    "non integer is rejected",
			nf:      newWorkerNodeFeature("node-5", map[string]string{_a10gPartitionsKey: "eight"}),
			wantErr: true,
		},
		{
			name:    "exceeds device max partitions is rejected",
			nf:      newWorkerNodeFeature("node-5", map[string]string{_a10gPartitionsKey: "16"}),
			devices: devicesWithMaxPartitions("node-5", "a10g", 8),
			wantErr: true,
		},
		{
			name:    "within device max partitions is allowed",
			nf:      newWorkerNodeFeature("node-5", map[string]string{_a10gPartitionsKey: "8"}),
			devices: devicesWithMaxPartitions("node-5", "a10g", 8),
			wantErr: false,
		},
		{
			name:    "power of two beyond device max but device unknown is allowed (best-effort)",
			nf:      newWorkerNodeFeature("node-5", map[string]string{_a10gPartitionsKey: "16"}),
			wantErr: false,
		},
		{
			// Pins the aKey split convention: an id with an embedded hyphen
			// (tesla-t4) must still match its Devices group via Cut on the first
			// hyphen (manufacturer=nvidia, id=tesla-t4).
			name: "embedded-hyphen id matches its device group",
			nf: newWorkerNodeFeature("node-5", map[string]string{
				nodefeature.AcceleratableFeatureLabelPrefix + "nvidia-tesla-t4" + nodefeature.SlicedPartitionsLabelSuffix: "16",
			}),
			devices: devicesWithMaxPartitions("node-5", "tesla-t4", 8),
			wantErr: true,
		},
		{
			// A group reporting MaxPartitions=0 means "no partition limit" and must
			// degrade to the power-of-two check — distinct from the device-unknown path.
			name:    "device present with zero max partitions degrades to power of two",
			nf:      newWorkerNodeFeature("node-5", map[string]string{_a10gPartitionsKey: "16"}),
			devices: devicesWithMaxPartitions("node-5", "a10g", 0),
			wantErr: false,
		},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			b := ctrlfake.NewClientBuilder().WithScheme(scheme.Scheme)
			if c.devices != nil {
				b = b.WithObjects(c.devices)
			}
			cli := b.Build()
			r := &NodeFeatureWebhook{Client: cli, APIReader: cli}

			_, err := r.ValidateCreate(context.Background(), c.nf)
			if c.wantErr {
				assert.Error(t, err)
				return
			}
			assert.NoError(t, err)

			// ValidateUpdate must behave identically on the new object.
			_, err = r.ValidateUpdate(context.Background(), c.nf, c.nf)
			assert.NoError(t, err)
		})
	}
}
