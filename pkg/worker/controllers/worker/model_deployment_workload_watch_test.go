package worker

import (
	"context"
	"slices"
	"testing"

	"github.com/stretchr/testify/assert"
	core "k8s.io/api/core/v1"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/workqueue"
	ctrlcli "sigs.k8s.io/controller-runtime/pkg/client"
	ctrlevent "sigs.k8s.io/controller-runtime/pkg/event"
	ctrlhandler "sigs.k8s.io/controller-runtime/pkg/handler"
	ctrlreconcile "sigs.k8s.io/controller-runtime/pkg/reconcile"
	kueue "sigs.k8s.io/kueue/apis/kueue/v1beta2"

	"gpustack.ai/gpustack/pkg/systemmeta"
)

// watchReplicaPod is a replica as the renderer leaves it: controlled by the named deployment and
// carrying the resource note the Pod watch filters on. An empty mdName leaves it without a
// controller.
func watchReplicaPod(name, mdName string) *core.Pod {
	pod := jointGroupPod(name, name, mdName)
	systemmeta.NoteResource(pod, ModelDeploymentResourceType,
		map[string]string{ModelDeploymentResourceNoteRole: "prefill"})

	return pod
}

// watchPodOwner is the owner reference Kueue gives a replica's Workload for one of its Pods.
func watchPodOwner(name string) meta.OwnerReference {
	return meta.OwnerReference{APIVersion: "v1", Kind: "Pod", Name: name, UID: types.UID("uid-" + name)}
}

// watchReplicaPods is every Pod the mapping cases can place in the cache, by name.
func watchReplicaPods() map[string]*core.Pod {
	return map[string]*core.Pod{
		"qwen-0":  watchReplicaPod("qwen-0", "qwen"),
		"qwen-1":  watchReplicaPod("qwen-1", "qwen"),
		"llama-0": watchReplicaPod("llama-0", "llama"),
		// Carries the note but no controller, so only the owner check can refuse it.
		"unowned-0": watchReplicaPod("unowned-0", ""),
		// Controlled by a deployment but without the note, so only the note check can refuse it.
		"unnoted-0": jointGroupPod("unnoted-0", "unnoted-0", "qwen"),
	}
}

// TestMapModelDeploymentWorkload covers the mapping from a replica's Workload to its deployment.
//
// Every refusing case names a Pod that exists or an owner that would map if the check under test
// were missing, so an empty answer there is the check's doing and not an empty fixture's.
func TestMapModelDeploymentWorkload(t *testing.T) {
	cases := []struct {
		name   string
		cached []string
		owners []meta.OwnerReference
		want   []string
	}{
		{
			name:   "a_replica_of_this_deployment_enqueues_it",
			cached: []string{"qwen-0"},
			owners: []meta.OwnerReference{watchPodOwner("qwen-0")},
			want:   []string{"team-a/qwen"},
		},
		{
			name:   "a_replica_of_another_deployment_enqueues_that_one_only",
			cached: []string{"qwen-0", "llama-0"},
			owners: []meta.OwnerReference{watchPodOwner("llama-0")},
			want:   []string{"team-a/llama"},
		},
		{
			name:   "a_pod_no_deployment_controls_maps_to_nothing",
			cached: []string{"unowned-0"},
			owners: []meta.OwnerReference{watchPodOwner("unowned-0")},
			want:   []string{},
		},
		{
			name:   "a_pod_without_the_deployment_resource_note_maps_to_nothing",
			cached: []string{"unnoted-0"},
			owners: []meta.OwnerReference{watchPodOwner("unnoted-0")},
			want:   []string{},
		},
		{
			name:   "a_pod_gone_from_the_cache_maps_to_nothing",
			cached: []string{"qwen-1"},
			owners: []meta.OwnerReference{watchPodOwner("qwen-0")},
			want:   []string{},
		},
		{
			name:   "an_owner_that_is_not_a_pod_maps_to_nothing",
			cached: []string{"qwen-0"},
			owners: []meta.OwnerReference{{
				APIVersion: "batch/v1", Kind: "Job", Name: "qwen-0", UID: "uid-qwen-0",
			}},
			want: []string{},
		},
		{
			name:   "every_member_of_one_group_enqueues_its_deployment_once",
			cached: []string{"qwen-0", "qwen-1"},
			owners: []meta.OwnerReference{watchPodOwner("qwen-0"), watchPodOwner("qwen-1")},
			want:   []string{"team-a/qwen"},
		},
		{
			name:   "owners_across_deployments_enqueue_each",
			cached: []string{"qwen-0", "llama-0"},
			owners: []meta.OwnerReference{watchPodOwner("qwen-0"), watchPodOwner("llama-0")},
			want:   []string{"team-a/llama", "team-a/qwen"},
		},
		{
			name:   "a_member_gone_from_the_cache_does_not_hide_the_others",
			cached: []string{"qwen-1"},
			owners: []meta.OwnerReference{watchPodOwner("qwen-0"), watchPodOwner("qwen-1")},
			want:   []string{"team-a/qwen"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			pods := watchReplicaPods()
			objs := make([]ctrlcli.Object, 0, len(tc.cached))
			for _, name := range tc.cached {
				objs = append(objs, pods[name])
			}
			cli := newModelDeploymentClient(objs...)
			r := &ModelDeploymentReconciler{Client: cli, APIReader: cli}

			wl := jointWorkload("wl", false)
			wl.OwnerReferences = tc.owners

			got := make([]string, 0)
			for _, req := range r.mapModelDeploymentWorkload(context.Background(), wl) {
				got = append(got, req.Namespace+"/"+req.Name)
			}
			slices.Sort(got)

			assert.Equal(t, tc.want, got)
		})
	}
}

// TestModelDeploymentWorkloadWatch_EnqueuesTheDeployment drives the watch's predicate and handler
// together, the way the controller composes them, for a replica whose Pod exists before its
// Workload does.
//
// KUEUE DOES NOT TOUCH THE Pod WHILE IT DECIDES, so the Pod watch sees nothing between the Pod
// being created and its gates being lifted. The Workload appearing, reserving quota and being
// admitted are events on the Workload alone, and a deployment whose status reads them has to be
// woken by them.
//
// WHAT THIS DOES NOT COVER IS THE BUILDER REGISTRATION, which a fake client cannot observe; that the
// controller watches with this pair is asserted in a cluster by the e2e case that reads the
// deployment's QuotaReserved condition.
func TestModelDeploymentWorkloadWatch_EnqueuesTheDeployment(t *testing.T) {
	type eventKind int
	const (
		created eventKind = iota
		updated
		deleted
	)

	cases := []struct {
		name   string
		kind   eventKind
		mutate func(*kueue.Workload)
		want   bool
	}{
		{name: "the_workload_appearing", kind: created, want: true},
		{name: "the_workload_being_deleted", kind: deleted, want: true},
		{
			name: "quota_being_reserved",
			kind: updated,
			mutate: func(wl *kueue.Workload) {
				wl.Status.Conditions[0].Status = meta.ConditionTrue
				wl.Status.Conditions[0].Reason = "QuotaReserved"
			},
			want: true,
		},
		{
			name: "the_scheduler_rewording_why_it_is_pending",
			kind: updated,
			mutate: func(wl *kueue.Workload) {
				wl.Status.Conditions[0].Message = "couldn't assign flavors to pod set prefill"
			},
			want: true,
		},
		{
			name: "an_admission_being_assigned",
			kind: updated,
			mutate: func(wl *kueue.Workload) {
				wl.Status.Admission = &kueue.Admission{ClusterQueue: "pool"}
			},
			want: true,
		},
		{
			name: "an_admission_check_moving",
			kind: updated,
			mutate: func(wl *kueue.Workload) {
				wl.Status.AdmissionChecks[0].State = kueue.CheckStateReady
			},
			want: true,
		},
		{
			name:   "the_workload_being_deactivated",
			kind:   updated,
			mutate: func(wl *kueue.Workload) { wl.Spec.Active = ptrTo(false) },
			want:   true,
		},
		{
			name: "a_member_being_added",
			kind: updated,
			mutate: func(wl *kueue.Workload) {
				wl.OwnerReferences = append(wl.OwnerReferences, watchPodOwner("qwen-1"))
			},
			want: true,
		},
		{
			name: "the_scheduler_recording_a_requeue",
			kind: updated,
			mutate: func(wl *kueue.Workload) {
				wl.Status.RequeueState = &kueue.RequeueState{Count: ptrTo[int32](1)}
			},
			want: false,
		},
		{
			name: "a_condition_timestamp_moving_alone",
			kind: updated,
			mutate: func(wl *kueue.Workload) {
				wl.Status.Conditions[0].LastTransitionTime = meta.Unix(1756684800, 0)
			},
			want: false,
		},
		{
			name:   "a_resync_that_moved_nothing",
			kind:   updated,
			mutate: func(wl *kueue.Workload) { wl.ResourceVersion = "999" },
			want:   false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cli := newModelDeploymentClient(watchReplicaPod("qwen-0", "qwen"))
			r := &ModelDeploymentReconciler{Client: cli, APIReader: cli}
			predicate := modelDeploymentWorkloadPredicate()
			handler := ctrlhandler.EnqueueRequestsFromMapFunc(r.mapModelDeploymentWorkload)
			queue := workqueue.NewTypedRateLimitingQueue(
				workqueue.DefaultTypedControllerRateLimiter[ctrlreconcile.Request]())
			defer queue.ShutDown()

			ctx := context.Background()
			wl := jointWorkload("wl", false, watchReplicaPod("qwen-0", "qwen"))
			switch tc.kind {
			case created:
				e := ctrlevent.CreateEvent{Object: wl}
				if predicate.Create(e) {
					handler.Create(ctx, e, queue)
				}
			case deleted:
				e := ctrlevent.DeleteEvent{Object: wl}
				if predicate.Delete(e) {
					handler.Delete(ctx, e, queue)
				}
			case updated:
				next := wl.DeepCopy()
				tc.mutate(next)
				e := ctrlevent.UpdateEvent{ObjectOld: wl, ObjectNew: next}
				if predicate.Update(e) {
					handler.Update(ctx, e, queue)
				}
			}

			if !tc.want {
				assert.Zero(t, queue.Len(), "a change the deployment's status does not read wakes nothing")

				return
			}
			if assert.Equal(t, 1, queue.Len()) {
				req, _ := queue.Get()
				assert.Equal(t, types.NamespacedName{Namespace: "team-a", Name: "qwen"}, req.NamespacedName)
			}
		})
	}
}
