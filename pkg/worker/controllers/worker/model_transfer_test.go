package worker

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apps "k8s.io/api/apps/v1"
	core "k8s.io/api/core/v1"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	ctrlcli "sigs.k8s.io/controller-runtime/pkg/client"
	ctrlfake "sigs.k8s.io/controller-runtime/pkg/client/fake"

	"gpustack.ai/gpustack/pkg/kubeclients/kubernetes/scheme"
	"gpustack.ai/gpustack/pkg/setting"
)

func transferFleetFixture() (*apps.DaemonSet, *core.Pod) {
	labels := map[string]string{
		"app.kubernetes.io/part-of":    "gpustack-operator",
		"app.kubernetes.io/component":  "model-transfer",
		"app.kubernetes.io/managed-by": "Helm",
		"app.kubernetes.io/instance":   "test",
		"gpustack.ai/manufacturer":     "nvidia",
	}
	ds := &apps.DaemonSet{
		ObjectMeta: meta.ObjectMeta{
			Name:       "source-nvidia",
			Namespace:  setting.DelegatedSecretNamespace,
			UID:        types.UID("fleet"),
			Generation: 1,
			Labels:     labels,
		},
		Spec: apps.DaemonSetSpec{
			Selector:       &meta.LabelSelector{MatchLabels: labels},
			UpdateStrategy: apps.DaemonSetUpdateStrategy{Type: apps.OnDeleteDaemonSetStrategyType},
			Template: core.PodTemplateSpec{
				ObjectMeta: meta.ObjectMeta{Labels: labels},
				Spec: core.PodSpec{
					Containers: []core.Container{
						{Name: "broker", Image: "broker:v1"},
						{Name: "worker", Image: "registry:5000/worker:new", Env: []core.EnvVar{{Name: "EXTRA", Value: "kept"}}},
						{Name: "sidecar", Image: "sidecar:v1"},
					},
				},
			},
		},
		Status: apps.DaemonSetStatus{
			ObservedGeneration:     1,
			DesiredNumberScheduled: 1,
			CurrentNumberScheduled: 1,
		},
	}
	pod := &core.Pod{
		ObjectMeta: meta.ObjectMeta{
			Name:      "source-pod",
			Labels:    labels,
			Namespace: ds.Namespace,
			UID:       types.UID("pod-1"),
			OwnerReferences: []meta.OwnerReference{{
				APIVersion: "apps/v1",
				Kind:       "DaemonSet",
				Name:       ds.Name,
				UID:        ds.UID,
				Controller: ptr.To(true),
			}},
			Annotations: map[string]string{transferWorkerInstance: "instance-1"},
		},
		Spec: core.PodSpec{
			NodeName:   "node-1",
			Containers: []core.Container{{Name: "worker", Image: "registry:5000/worker:old"}},
		},
	}
	return ds, pod
}

func TestTransferImageWaitsForExactInstanceDrain(t *testing.T) {
	for _, tc := range []struct {
		name, ack, instance string
		permit              bool
	}{
		{"missing", "", "", false},
		{"old-target", "registry:5000/worker:old", "instance-1", false},
		{"stale-instance", "registry:5000/worker:new", "instance-old", false},
		{"exact", "registry:5000/worker:new", "instance-1", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ds, pod := transferFleetFixture()
			cli := ctrlfake.NewClientBuilder().WithScheme(scheme.Scheme).WithObjects(ds, pod).Build()
			r := &ModelTransferImageReconciler{Client: cli}
			_, err := r.Reconcile(context.Background(), ctrl.Request{})
			require.NoError(t, err)
			fresh := new(core.Pod)
			require.NoError(t, cli.Get(context.Background(), ctrlcli.ObjectKeyFromObject(pod), fresh))
			assert.Equal(t, "registry:5000/worker:new", fresh.Annotations[transferDrainFor])
			fresh.Annotations[transferDrainedFor] = tc.ack
			fresh.Annotations[transferDrainedInstance] = tc.instance
			require.NoError(t, cli.Update(context.Background(), fresh))
			_, err = r.Reconcile(context.Background(), ctrl.Request{})
			require.NoError(t, err)
			err = cli.Get(context.Background(), ctrlcli.ObjectKeyFromObject(pod), new(core.Pod))
			if tc.permit {
				assert.True(t, kerrors.IsNotFound(err))
			} else {
				require.NoError(t, err)
			}
		})
	}
}

func TestTransferReplacementRequiresNewPodAcknowledgement(t *testing.T) {
	ds, pod := transferFleetFixture()
	pod.UID = types.UID("recreated")
	pod.Annotations[transferWorkerInstance] = "new-instance"
	pod.Annotations[transferDrainFor] = "registry:5000/worker:new"
	pod.Annotations[transferDrainedFor] = "registry:5000/worker:new"
	pod.Annotations[transferDrainedInstance] = "old-instance"
	cli := ctrlfake.NewClientBuilder().WithScheme(scheme.Scheme).WithObjects(ds, pod).Build()
	_, err := (&ModelTransferImageReconciler{Client: cli}).Reconcile(context.Background(), ctrl.Request{})
	require.NoError(t, err)
	require.NoError(t, cli.Get(context.Background(), ctrlcli.ObjectKeyFromObject(pod), new(core.Pod)))
}

func TestTransferMatchingImageWithPriorDrainRequiresAck(t *testing.T) {
	for _, tc := range []struct {
		name, desired, ack, instance string
		wantDrain                    string
		wantDeleted                  bool
	}{
		{"same image without ack", "registry:5000/worker:old", "", "", "prior-target", false},
		{"same image with stale instance", "registry:5000/worker:old", "prior-target", "stale", "prior-target", false},
		{"same image with exact ack", "registry:5000/worker:old", "prior-target", "instance-1", "", true},
		{"different image requests another drain", "registry:5000/worker:new", "prior-target", "instance-1", "registry:5000/worker:new", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ds, pod := transferFleetFixture()
			ds.Spec.Template.Spec.Containers[1].Image = tc.desired
			pod.Annotations[transferDrainFor] = "prior-target"
			pod.Annotations[transferDrainedFor] = tc.ack
			pod.Annotations[transferDrainedInstance] = tc.instance
			cli := ctrlfake.NewClientBuilder().WithScheme(scheme.Scheme).WithObjects(ds, pod).Build()
			_, err := (&ModelTransferImageReconciler{Client: cli}).Reconcile(context.Background(), ctrl.Request{})
			require.NoError(t, err)
			got := new(core.Pod)
			err = cli.Get(context.Background(), ctrlcli.ObjectKeyFromObject(pod), got)
			if tc.wantDeleted {
				require.True(t, kerrors.IsNotFound(err))
			} else {
				require.NoError(t, err)
				assert.Equal(t, tc.wantDrain, got.Annotations[transferDrainFor])
			}
		})
	}
}
