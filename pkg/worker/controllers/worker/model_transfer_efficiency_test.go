package worker

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apps "k8s.io/api/apps/v1"
	core "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	ctrlcli "sigs.k8s.io/controller-runtime/pkg/client"
	ctrlfake "sigs.k8s.io/controller-runtime/pkg/client/fake"
	ctrlevent "sigs.k8s.io/controller-runtime/pkg/event"

	"gpustack.ai/gpustack/pkg/kubeclients/kubernetes/scheme"
)

func TestSourceDaemonSetWatchIgnoresStatusChurn(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change string
		want   bool
	}{
		{"status", "status", false},
		{"new image", "image", true},
		{"manufacturer", "manufacturer", true},
		{"leaves source selection", "component", true},
		{"replacement", "uid", true},
		{"unrelated fleet", "unrelated", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			old, _ := transferFleetFixture()
			if tc.change == "unrelated" {
				old.Labels["app.kubernetes.io/component"] = "device-manager"
			}
			next := old.DeepCopy()
			switch tc.change {
			case "status", "unrelated":
				next.Status.NumberReady++
			case "image":
				next.Spec.Template.Spec.Containers[1].Image = "worker:newer"
			case "manufacturer":
				next.Labels["gpustack.ai/manufacturer"] = "ascend"
			case "component":
				next.Labels["app.kubernetes.io/component"] = "other"
			case "uid":
				next.UID = types.UID("replacement")
			}
			got := modelTransferSourceDaemonSetChanged().Update(ctrlevent.UpdateEvent{ObjectOld: old, ObjectNew: next})
			assert.Equal(t, tc.want, got)
		})
	}
}

func TestAmbiguousSourceVendorDoesNotBlockOtherFleet(t *testing.T) {
	ds, pod := transferFleetFixture()
	ambiguous := ds.DeepCopy()
	ambiguous.Name = "ascend-one"
	ambiguous.UID = "ascend-one"
	ambiguous.Labels["gpustack.ai/manufacturer"] = "ascend"
	duplicate := ambiguous.DeepCopy()
	duplicate.Name = "ascend-two"
	duplicate.UID = "ascend-two"
	cli := ctrlfake.NewClientBuilder().WithScheme(scheme.Scheme).WithObjects(ds, pod, ambiguous, duplicate).Build()
	_, err := (&ModelTransferImageReconciler{Client: cli}).Reconcile(context.Background(), ctrl.Request{})
	require.ErrorContains(t, err, "ambiguous model-transfer DaemonSets for ascend")
	current := new(core.Pod)
	require.NoError(t, cli.Get(context.Background(), ctrlcli.ObjectKeyFromObject(pod), current))
	assert.Equal(t, ds.Spec.Template.Spec.Containers[1].Image, current.Annotations[transferDrainFor])
	retained := new(apps.DaemonSet)
	require.NoError(t, cli.Get(context.Background(), ctrlcli.ObjectKeyFromObject(ambiguous), retained))
}
