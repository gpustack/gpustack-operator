package worker

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apps "k8s.io/api/apps/v1"
	core "k8s.io/api/core/v1"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/record"
	"k8s.io/utils/ptr"
	ctrlcli "sigs.k8s.io/controller-runtime/pkg/client"
	ctrlfake "sigs.k8s.io/controller-runtime/pkg/client/fake"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	"gpustack.ai/gpustack/pkg/kubeclients/kubernetes/scheme"
	"gpustack.ai/gpustack/pkg/systemmeta"
	"gpustack.ai/gpustack/pkg/worker/kuberess"
	"gpustack.ai/gpustack/pkg/worker/kvcache"
	"gpustack.ai/gpustack/pkg/worker/kvcache/mooncake"
)

// teardownBackend is a managed backend with nothing on it but the fields the teardown reads.
func teardownBackend() *workercore.KVCacheBackend {
	return &workercore.KVCacheBackend{
		ObjectMeta: meta.ObjectMeta{Name: "store", UID: "11111111-2222-3333-4444-555555555555"},
		Spec: workercore.KVCacheBackendSpec{
			Connection: workercore.KVCacheBackendConnection{
				Managed: &workercore.KVCacheBackendManaged{
					Members: []workercore.KVCacheBackendMember{{}},
				},
			},
		},
	}
}

// teardownMemberSelector is what the member DaemonSet selects its pods on. Any set will do; what the
// test needs is that the DaemonSet and its pods agree on one.
var teardownMemberSelector = map[string]string{"kvcache-member": "store-0"}

// terminatingMemberDaemonSet builds a member DaemonSet that has been terminating for age, declaring
// the grace its pods get.
//
// The note is written with the same call the renderer uses, because the teardown finds its workloads
// through it: a fixture that set the labels by hand would be invisible to the code under test for a
// reason that has nothing to do with what is being asserted.
//
// A finalizer rides along because the fake client refuses an object carrying a deletion timestamp
// without one, which is also what a real foreground deletion leaves behind.
func terminatingMemberDaemonSet(
	kvcb *workercore.KVCacheBackend, age time.Duration, grace *int64,
) *apps.DaemonSet {
	ds := &apps.DaemonSet{
		ObjectMeta: meta.ObjectMeta{
			Name:              mooncake.MemberObjectName(kvcb, 0),
			Namespace:         kuberess.SystemNamespaceName,
			DeletionTimestamp: ptr.To(meta.NewTime(time.Now().Add(-age))),
			Finalizers:        []string{meta.FinalizerDeleteDependents},
		},
		Spec: apps.DaemonSetSpec{
			Selector: &meta.LabelSelector{MatchLabels: teardownMemberSelector},
			Template: core.PodTemplateSpec{
				ObjectMeta: meta.ObjectMeta{Labels: teardownMemberSelector},
				Spec:       core.PodSpec{TerminationGracePeriodSeconds: grace},
			},
		},
	}
	systemmeta.NoteResource(ds, kvcache.ResourceType, map[string]string{
		kvcache.ResourceNoteBackend: kvcb.Name,
	})

	return ds
}

// terminatingMemberPod is one of that DaemonSet's pods, stuck terminating on a node, carrying the
// grace it was CREATED with rather than whatever its DaemonSet's template says now.
func terminatingMemberPod(name, node string, age time.Duration, grace *int64) *core.Pod {
	return &core.Pod{
		ObjectMeta: meta.ObjectMeta{
			Name:              name,
			Namespace:         kuberess.SystemNamespaceName,
			Labels:            teardownMemberSelector,
			DeletionTimestamp: ptr.To(meta.NewTime(time.Now().Add(-age))),
			Finalizers:        []string{meta.FinalizerDeleteDependents},
		},
		Spec: core.PodSpec{NodeName: node, TerminationGracePeriodSeconds: grace},
	}
}

// TestWorkloadTerminationBudgetFollowsTheWorkload pins that the wait is sized by what the workload
// itself declares.
//
// A constant would have to be one of two wrong numbers. Short enough to bound an unreachable node, it
// abandons a member group draining on the hour its own spec asked for; long enough for that group, it
// is no bound for anything else. The cases below are the two ends and the kind that runs no pod at
// all, so a change back to a constant fails on whichever end it does not happen to match.
func TestWorkloadTerminationBudgetFollowsTheWorkload(t *testing.T) {
	for _, tc := range []struct {
		name string
		obj  ctrlcli.Object
		want time.Duration
	}{
		{
			name: "a member group draining on the longest grace its spec can ask for",
			obj: &apps.DaemonSet{Spec: apps.DaemonSetSpec{Template: core.PodTemplateSpec{
				Spec: core.PodSpec{TerminationGracePeriodSeconds: ptr.To[int64](3660)},
			}}},
			want: 3660*time.Second + kvCacheBackendWorkloadTerminationMargin,
		},
		{
			name: "a member group with no local disk, whose grace is the shutdown alone",
			obj: &apps.DaemonSet{Spec: apps.DaemonSetSpec{Template: core.PodTemplateSpec{
				Spec: core.PodSpec{TerminationGracePeriodSeconds: ptr.To[int64](60)},
			}}},
			want: 60*time.Second + kvCacheBackendWorkloadTerminationMargin,
		},
		{
			name: "the leader, whose template names no grace and whose pods get the kubelet default",
			obj:  &apps.Deployment{},
			want: core.DefaultTerminationGracePeriodSeconds*time.Second +
				kvCacheBackendWorkloadTerminationMargin,
		},
		{
			name: "a Service, which runs no pod and waits on nothing but the collection itself",
			obj:  &core.Service{},
			want: kvCacheBackendWorkloadTerminationMargin,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, kvCacheBackendWorkloadTerminationBudget(tc.obj))
		})
	}
}

// TestTeardownStopsWaitingOnAWorkloadPastItsOwnBudget is the bound itself, asserted through the count
// the finalizer is held on.
//
// The three cases are one behavior read at three points, and the third is the one that distinguishes
// this from any fixed timeout: the same age that abandons a member with a short grace must NOT abandon
// one whose spec asked for a long one. Without it a constant of any value passes the other two.
func TestTeardownStopsWaitingOnAWorkloadPastItsOwnBudget(t *testing.T) {
	for _, tc := range []struct {
		name  string
		age   time.Duration
		grace *int64
		want  int
	}{
		{
			name:  "still inside its grace, so the teardown keeps waiting",
			age:   30 * time.Second,
			grace: ptr.To[int64](60),
			want:  1,
		},
		{
			name:  "past its grace and the margin, so the teardown stops waiting",
			age:   time.Hour,
			grace: ptr.To[int64](60),
			want:  0,
		},
		{
			name:  "the same age, on a group whose spec asked to drain for an hour",
			age:   time.Hour,
			grace: ptr.To[int64](3600),
			want:  1,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			kvcb := teardownBackend()
			r := &KVCacheBackendReconciler{
				Client: ctrlfake.NewClientBuilder().
					WithScheme(scheme.Scheme).
					WithObjects(terminatingMemberDaemonSet(kvcb, tc.age, tc.grace)).
					Build(),
				Recorder: record.NewFakeRecorder(10),
			}

			remaining, err := r.deleteRenderedWorkloads(context.Background(), kvcb)
			require.NoError(t, err)
			assert.Equal(t, tc.want, remaining,
				"remaining is what the finalizer is held on, so a workload nothing can finish "+
					"terminating must stop counting toward it")
		})
	}
}

// TestTeardownStopsWaitingOnALeaderPastItsBudget pins the bound on the OTHER loop.
//
// The leader objects are reached by name and the member groups by label, so the two are separate
// pieces of code that happen to want the same rule. A bound wired into one of them leaves a leader
// pod on a node that stopped answering holding the backend exactly as before.
func TestTeardownStopsWaitingOnALeaderPastItsBudget(t *testing.T) {
	kvcb := teardownBackend()
	deploy := &apps.Deployment{
		ObjectMeta: meta.ObjectMeta{
			Name:              mooncake.LeaderObjectName(kvcb),
			Namespace:         kuberess.SystemNamespaceName,
			DeletionTimestamp: ptr.To(meta.NewTime(time.Now().Add(-time.Hour))),
			Finalizers:        []string{meta.FinalizerDeleteDependents},
		},
	}
	systemmeta.NoteResource(deploy, kvcache.ResourceType, map[string]string{
		kvcache.ResourceNoteBackend: kvcb.Name,
	})

	r := &KVCacheBackendReconciler{
		Client: ctrlfake.NewClientBuilder().
			WithScheme(scheme.Scheme).
			WithObjects(deploy).
			Build(),
		Recorder: record.NewFakeRecorder(10),
	}

	remaining, err := r.deleteRenderedWorkloads(context.Background(), kvcb)
	require.NoError(t, err)
	assert.Equal(t, 0, remaining)
}

// TestTeardownCountsAWorkloadItJustDeleted covers the one path that reaches the bound with no
// deletion timestamp to measure from.
//
// The leader objects are read and then deleted in the same pass, so the copy the bound is handed is
// the one from BEFORE the delete: it carries no timestamp at all. Read as an age that is a very long
// time, a workload would be abandoned on the pass that asked it to go, and the teardown would release
// the finalizer with the leader still serving.
func TestTeardownCountsAWorkloadItJustDeleted(t *testing.T) {
	kvcb := teardownBackend()
	deploy := &apps.Deployment{
		ObjectMeta: meta.ObjectMeta{
			Name:      mooncake.LeaderObjectName(kvcb),
			Namespace: kuberess.SystemNamespaceName,
		},
	}
	systemmeta.NoteResource(deploy, kvcache.ResourceType, map[string]string{
		kvcache.ResourceNoteBackend: kvcb.Name,
	})

	recorder := record.NewFakeRecorder(10)
	r := &KVCacheBackendReconciler{
		Client: ctrlfake.NewClientBuilder().
			WithScheme(scheme.Scheme).
			WithObjects(deploy).
			Build(),
		Recorder: recorder,
	}

	remaining, err := r.deleteRenderedWorkloads(context.Background(), kvcb)
	require.NoError(t, err)
	assert.Equal(t, 1, remaining,
		"a workload deleted on this very pass has not been terminating for any time at all")

	select {
	case event := <-recorder.Events:
		require.Failf(t, "nothing was given up on", "recorded: %s", event)
	default:
	}
}

// TestAbandonedWorkloadNamesTheNodeHoldingIt pins the pointer the event exists to hand over.
//
// An operator reading a backend stuck in Deleting has had neither half: not which workload would not
// go, and not where. The workload is in the event because it is what the event is recorded on; the
// node can only come from the pods, and is the half that says what to go and look at.
func TestAbandonedWorkloadNamesTheNodeHoldingIt(t *testing.T) {
	kvcb := teardownBackend()
	recorder := record.NewFakeRecorder(10)
	r := &KVCacheBackendReconciler{
		Client: ctrlfake.NewClientBuilder().
			WithScheme(scheme.Scheme).
			WithObjects(
				terminatingMemberDaemonSet(kvcb, time.Hour, ptr.To[int64](60)),
				terminatingMemberPod("store-0-abcde", "node-b", time.Hour, nil),
				terminatingMemberPod("store-0-fghij", "node-a", time.Hour, nil),
			).
			Build(),
		Recorder: recorder,
	}

	remaining, err := r.deleteRenderedWorkloads(context.Background(), kvcb)
	require.NoError(t, err)
	require.Equal(t, 0, remaining)

	var event string
	select {
	case event = <-recorder.Events:
	default:
		require.Fail(t, "giving up has to be loud: no event was recorded")
	}

	assert.Contains(t, event, kvCacheBackendEventWorkloadAbandoned)
	assert.Contains(t, event, "node-a, node-b",
		"the nodes are sorted, so the same stall produces the same message on every pass and "+
			"client-go folds the repeats into one event instead of a hundred")
	assert.NotContains(t, event, "1h0m0s",
		"the elapsed age would make every repeat a distinct event; the budget that ran out is "+
			"constant and is what the message carries")
}

// TestAbandonedWorkloadSurvivesANilRecorder pins that the give-up path degrades to silence rather
// than taking the teardown down with it.
//
// The recorder is assigned in SetupController and nowhere else, so a reconciler built with a client
// and nothing else reaches this branch with a nil one. A panic here would be the object nobody can
// delete arriving by a different route than the one this bound was added to close.
func TestAbandonedWorkloadSurvivesANilRecorder(t *testing.T) {
	kvcb := teardownBackend()
	r := &KVCacheBackendReconciler{
		Client: ctrlfake.NewClientBuilder().
			WithScheme(scheme.Scheme).
			WithObjects(terminatingMemberDaemonSet(kvcb, time.Hour, ptr.To[int64](60))).
			Build(),
	}

	require.NotPanics(t, func() {
		remaining, err := r.deleteRenderedWorkloads(context.Background(), kvcb)
		assert.NoError(t, err)
		assert.Equal(t, 0, remaining)
	})
}

// TestTeardownReadsTheGraceTheStuckPodActuallyHas pins that the deciding grace comes from the pod and
// not from the template above it.
//
// A pod runs the template it was CREATED from, and the two drift for as long as a rolling restart
// takes: restartOutdatedMemberPods replaces a generation one pod at a time, so a member whose group
// had scaleIn.gracePeriodSeconds lowered is still draining on the old, longer one. Reading the
// template would abandon it while its kubelet is doing exactly what it was told, and the disk tier
// cleanup that runs next would then empty a directory under a store still writing to it.
//
// The mirror case is what stops this from being a wait with no bound again: the same pod, past the
// grace it really has, is abandoned.
func TestTeardownReadsTheGraceTheStuckPodActuallyHas(t *testing.T) {
	for _, tc := range []struct {
		name     string
		podAge   time.Duration
		podGrace *int64
		want     int
	}{
		{
			name:     "the pod was created on the longer grace its group used to declare",
			podAge:   time.Hour,
			podGrace: ptr.To[int64](3600),
			want:     1,
		},
		{
			name:     "the pod carries the same short grace the template does",
			podAge:   time.Hour,
			podGrace: ptr.To[int64](60),
			want:     0,
		},
		{
			// The workload and its pods do not get their deletion timestamps at the same instant: a
			// foreground deletion reaches the pods through the levels between them, and an eviction
			// can delete one long after. Timed from the workload, this pod is charged with an hour it
			// has not spent and is abandoned ten seconds into a grace it is honoring.
			name:     "the pod started terminating long after the workload did",
			podAge:   10 * time.Second,
			podGrace: ptr.To[int64](60),
			want:     1,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			kvcb := teardownBackend()
			r := &KVCacheBackendReconciler{
				Client: ctrlfake.NewClientBuilder().
					WithScheme(scheme.Scheme).
					WithObjects(
						// The template has since been lowered to a minute; the pod has not caught up.
						terminatingMemberDaemonSet(kvcb, time.Hour, ptr.To[int64](60)),
						terminatingMemberPod("store-0-abcde", "node-a", tc.podAge, tc.podGrace),
					).
					Build(),
				Recorder: record.NewFakeRecorder(10),
			}

			remaining, err := r.deleteRenderedWorkloads(context.Background(), kvcb)
			require.NoError(t, err)
			assert.Equal(t, tc.want, remaining,
				"the kubelet honors the grace the pod was created with, so that is the only one "+
					"that says whether this pod is late")
		})
	}
}
