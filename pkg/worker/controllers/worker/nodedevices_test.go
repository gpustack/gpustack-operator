package worker

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	core "k8s.io/api/core/v1"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrlcli "sigs.k8s.io/controller-runtime/pkg/client"
	ctrlfake "sigs.k8s.io/controller-runtime/pkg/client/fake"
	ctrlreconcile "sigs.k8s.io/controller-runtime/pkg/reconcile"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	"gpustack.ai/gpustack/pkg/kubeclients/kubernetes/scheme"
	"gpustack.ai/gpustack/pkg/systemname"
)

func TestNodeDevicesReconciler_SyncsManagedLabel(t *testing.T) {
	const node = "node-a"
	cases := []struct {
		name       string
		nodeLabels map[string]string
		devLabels  map[string]string
		want       string // expected managed value on the Devices; "" → label absent
	}{
		{
			name:       "stamps managed when the node is managed",
			nodeLabels: map[string]string{systemname.ManagedLabelKey: "true"},
			devLabels:  map[string]string{"feature.gpustack.ai/x": "true"},
			want:       "true",
		},
		{
			name:       "removes managed when the node carries none",
			nodeLabels: map[string]string{},
			devLabels:  map[string]string{systemname.ManagedLabelKey: "true"},
			want:       "",
		},
		{
			name:       "mirrors the node's explicit value",
			nodeLabels: map[string]string{systemname.ManagedLabelKey: "false"},
			devLabels:  map[string]string{systemname.ManagedLabelKey: "true"},
			want:       "false",
		},
	}

	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			nd := &core.Node{ObjectMeta: meta.ObjectMeta{Name: node, Labels: c.nodeLabels}}
			devs := &workercore.Devices{ObjectMeta: meta.ObjectMeta{Name: node, Labels: c.devLabels}}
			cli := ctrlfake.NewClientBuilder().
				WithScheme(scheme.Scheme).
				WithObjects(nd, devs).
				Build()
			r := &NodeDevicesReconciler{Client: cli}

			_, err := r.Reconcile(context.Background(),
				ctrlreconcile.Request{NamespacedName: ctrlcli.ObjectKey{Name: node}})
			require.NoError(t, err)

			got := new(workercore.Devices)
			require.NoError(t, cli.Get(context.Background(), ctrlcli.ObjectKey{Name: node}, got))
			assert.Equal(t, c.want, got.Labels[systemname.ManagedLabelKey])
		})
	}
}

// TestNodeDevicesReconciler_MissingDevicesIsNoop pins that a Node with no Devices
// object (e.g. not yet reported by the DeviceManager) reconciles without error.
func TestNodeDevicesReconciler_MissingDevicesIsNoop(t *testing.T) {
	nd := &core.Node{ObjectMeta: meta.ObjectMeta{
		Name:   "node-a",
		Labels: map[string]string{systemname.ManagedLabelKey: "true"},
	}}
	cli := ctrlfake.NewClientBuilder().WithScheme(scheme.Scheme).WithObjects(nd).Build()
	r := &NodeDevicesReconciler{Client: cli}

	_, err := r.Reconcile(context.Background(),
		ctrlreconcile.Request{NamespacedName: ctrlcli.ObjectKey{Name: "node-a"}})
	assert.NoError(t, err)
}
