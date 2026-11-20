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

func transferConfigHashFixture(image, dsHash, podHash string) (*apps.DaemonSet, *core.Pod) {
	ds, pod := transferFleetFixture()
	ds.Name = "source-ascend"
	ds.Labels["gpustack.ai/manufacturer"] = "ascend"
	ds.Spec.Selector.MatchLabels["gpustack.ai/manufacturer"] = "ascend"
	ds.Spec.Template.Labels["gpustack.ai/manufacturer"] = "ascend"
	ds.Spec.Template.Spec.Containers[1].Image = image
	if dsHash != "" {
		ds.Spec.Template.Annotations = map[string]string{
			transferConfigHash: dsHash,
		}
	}

	pod.Name = "source-ascend-pod"
	pod.Labels["gpustack.ai/manufacturer"] = "ascend"
	pod.OwnerReferences[0].Name = ds.Name
	pod.Spec.Containers[0].Image = image
	if podHash != "" {
		pod.Annotations[transferConfigHash] = podHash
	}
	return ds, pod
}

func TestTransferSameImageConfigHashRolloutAndRollback(t *testing.T) {
	const (
		image     = "registry:5000/worker:cann9.2"
		hashFalse = "aaaa1111bbbb2222cccc3333dddd4444"
		hashTrue  = "eeee5555ffff66660000777711118888"
	)

	ds, pod := transferConfigHashFixture(image, hashFalse, hashFalse)
	cli := ctrlfake.NewClientBuilder().WithScheme(scheme.Scheme).WithObjects(ds, pod).Build()
	r := &ModelTransferImageReconciler{Client: cli}

	// Steady state with matching image and config-hash makes no changes.
	res, err := r.Reconcile(context.Background(), ctrl.Request{})
	require.NoError(t, err)
	assert.Zero(t, res.RequeueAfter)
	got := new(core.Pod)
	require.NoError(t, cli.Get(context.Background(), ctrlcli.ObjectKeyFromObject(pod), got))
	assert.Empty(t, got.Annotations[transferDrainFor])

	// Toggle false -> true at the exact same worker image.
	require.NoError(t, cli.Get(context.Background(), ctrlcli.ObjectKeyFromObject(ds), ds))
	ds.Generation++
	ds.Spec.Template.Annotations[transferConfigHash] = hashTrue
	require.NoError(t, cli.Update(context.Background(), ds))
	ds.Status.ObservedGeneration = ds.Generation
	require.NoError(t, cli.Status().Update(context.Background(), ds))

	res, err = r.Reconcile(context.Background(), ctrl.Request{})
	require.NoError(t, err)
	assert.NotZero(t, res.RequeueAfter)
	require.NoError(t, cli.Get(context.Background(), ctrlcli.ObjectKeyFromObject(pod), got))
	wantTrueTarget := transferRevision(image, hashTrue)
	assert.Equal(t, wantTrueTarget, got.Annotations[transferDrainFor])

	// Stale or incomplete drain acknowledgements must not permit deletion.
	for _, stale := range []struct {
		name, ack, instance string
	}{
		{"missing ack", "", ""},
		{"old hash ack", transferRevision(image, hashFalse), "instance-1"},
		{"image only ack", image, "instance-1"},
		{"stale instance ack", wantTrueTarget, "instance-stale"},
	} {
		t.Run("enable_"+stale.name, func(t *testing.T) {
			require.NoError(t, cli.Get(context.Background(), ctrlcli.ObjectKeyFromObject(pod), got))
			got.Annotations[transferDrainedFor] = stale.ack
			got.Annotations[transferDrainedInstance] = stale.instance
			require.NoError(t, cli.Update(context.Background(), got))

			_, err := r.Reconcile(context.Background(), ctrl.Request{})
			require.NoError(t, err)
			require.NoError(t, cli.Get(context.Background(), ctrlcli.ObjectKeyFromObject(pod), new(core.Pod)))
		})
	}

	// Exact drain acknowledgement deletes the Pod.
	require.NoError(t, cli.Get(context.Background(), ctrlcli.ObjectKeyFromObject(pod), got))
	got.Annotations[transferDrainedFor] = wantTrueTarget
	got.Annotations[transferDrainedInstance] = "instance-1"
	require.NoError(t, cli.Update(context.Background(), got))
	_, err = r.Reconcile(context.Background(), ctrl.Request{})
	require.NoError(t, err)
	assert.True(t, kerrors.IsNotFound(cli.Get(context.Background(), ctrlcli.ObjectKeyFromObject(pod), new(core.Pod))))

	// Simulate DaemonSet creating replacement Pod with hashTrue and a fresh instance.
	_, pod2 := transferConfigHashFixture(image, hashTrue, hashTrue)
	pod2.Name = "source-ascend-pod-2"
	pod2.UID = types.UID("pod-2")
	pod2.Annotations[transferWorkerInstance] = "instance-2"
	require.NoError(t, cli.Create(context.Background(), pod2))

	res, err = r.Reconcile(context.Background(), ctrl.Request{})
	require.NoError(t, err)
	assert.Zero(t, res.RequeueAfter)

	// Rollback true -> false at the exact same worker image.
	require.NoError(t, cli.Get(context.Background(), ctrlcli.ObjectKeyFromObject(ds), ds))
	ds.Generation++
	ds.Spec.Template.Annotations[transferConfigHash] = hashFalse
	require.NoError(t, cli.Update(context.Background(), ds))
	ds.Status.ObservedGeneration = ds.Generation
	require.NoError(t, cli.Status().Update(context.Background(), ds))

	res, err = r.Reconcile(context.Background(), ctrl.Request{})
	require.NoError(t, err)
	assert.NotZero(t, res.RequeueAfter)
	require.NoError(t, cli.Get(context.Background(), ctrlcli.ObjectKeyFromObject(pod2), got))
	wantFalseTarget := transferRevision(image, hashFalse)
	assert.Equal(t, wantFalseTarget, got.Annotations[transferDrainFor])

	// Stale ack from prior enable target or prior pod instance must be fenced.
	for _, stale := range []struct {
		name, ack, instance string
	}{
		{"prior target", wantTrueTarget, "instance-2"},
		{"prior pod instance", wantFalseTarget, "instance-1"},
	} {
		t.Run("rollback_"+stale.name, func(t *testing.T) {
			require.NoError(t, cli.Get(context.Background(), ctrlcli.ObjectKeyFromObject(pod2), got))
			got.Annotations[transferDrainedFor] = stale.ack
			got.Annotations[transferDrainedInstance] = stale.instance
			require.NoError(t, cli.Update(context.Background(), got))

			_, err := r.Reconcile(context.Background(), ctrl.Request{})
			require.NoError(t, err)
			require.NoError(t, cli.Get(context.Background(), ctrlcli.ObjectKeyFromObject(pod2), new(core.Pod)))
		})
	}

	// Exact rollback acknowledgement deletes pod2.
	require.NoError(t, cli.Get(context.Background(), ctrlcli.ObjectKeyFromObject(pod2), got))
	got.Annotations[transferDrainedFor] = wantFalseTarget
	got.Annotations[transferDrainedInstance] = "instance-2"
	require.NoError(t, cli.Update(context.Background(), got))
	_, err = r.Reconcile(context.Background(), ctrl.Request{})
	require.NoError(t, err)
	assert.True(t, kerrors.IsNotFound(cli.Get(context.Background(), ctrlcli.ObjectKeyFromObject(pod2), new(core.Pod))))
}

func TestTransferConfigHashTransitionsAndPriorDrainFencing(t *testing.T) {
	const (
		image     = "registry:5000/worker:cann9.2"
		hashFalse = "1111aaaa"
		hashTrue  = "2222bbbb"
		hashOther = "3333cccc"
	)

	testCases := []struct {
		name        string
		dsHash      string
		podHash     string
		priorDrain  string
		ack         string
		instance    string
		wantDrain   string
		wantDeleted bool
		wantCleared bool
	}{
		{
			name:        "unannotated pod upgrades to hashed template",
			dsHash:      hashTrue,
			podHash:     "",
			wantDrain:   transferRevision(image, hashTrue),
			wantCleared: true,
		},
		{
			name:        "hashed pod downgrades to unannotated template after exact ack",
			dsHash:      "",
			podHash:     hashTrue,
			priorDrain:  image,
			ack:         image,
			instance:    "instance-1",
			wantDeleted: true,
		},
		{
			name:       "cancelled toggle with prior drain waits when ack is stale",
			dsHash:     hashFalse,
			podHash:    hashFalse,
			priorDrain: transferRevision(image, hashTrue),
			ack:        transferRevision(image, hashTrue),
			instance:   "stale-instance",
			wantDrain:  transferRevision(image, hashTrue),
		},
		{
			name:        "cancelled toggle with prior drain deletes on exact ack",
			dsHash:      hashFalse,
			podHash:     hashFalse,
			priorDrain:  transferRevision(image, hashTrue),
			ack:         transferRevision(image, hashTrue),
			instance:    "instance-1",
			wantDeleted: true,
		},
		{
			name:        "retargeted config-hash replaces prior drain and clears stale ack",
			dsHash:      hashOther,
			podHash:     hashFalse,
			priorDrain:  transferRevision(image, hashTrue),
			ack:         transferRevision(image, hashTrue),
			instance:    "instance-1",
			wantDrain:   transferRevision(image, hashOther),
			wantCleared: true,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			ds, pod := transferConfigHashFixture(image, tc.dsHash, tc.podHash)
			if tc.priorDrain != "" {
				pod.Annotations[transferDrainFor] = tc.priorDrain
			}
			if tc.ack != "" {
				pod.Annotations[transferDrainedFor] = tc.ack
			}
			if tc.instance != "" {
				pod.Annotations[transferDrainedInstance] = tc.instance
			}

			cli := ctrlfake.NewClientBuilder().WithScheme(scheme.Scheme).WithObjects(ds, pod).Build()
			_, err := (&ModelTransferImageReconciler{Client: cli}).Reconcile(context.Background(), ctrl.Request{})
			require.NoError(t, err)

			got := new(core.Pod)
			err = cli.Get(context.Background(), ctrlcli.ObjectKeyFromObject(pod), got)
			if tc.wantDeleted {
				require.True(t, kerrors.IsNotFound(err))
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.wantDrain, got.Annotations[transferDrainFor])
			if tc.wantCleared {
				assert.Empty(t, got.Annotations[transferDrainedFor])
				assert.Empty(t, got.Annotations[transferDrainedInstance])
			}
		})
	}
}
