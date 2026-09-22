package worker

import (
	"context"
	"errors"
	"maps"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	core "k8s.io/api/core/v1"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrlrecord "k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	ctrlcli "sigs.k8s.io/controller-runtime/pkg/client"
	ctrlfake "sigs.k8s.io/controller-runtime/pkg/client/fake"
	ctrlinterceptor "sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	ctrlreconcile "sigs.k8s.io/controller-runtime/pkg/reconcile"
	kueue "sigs.k8s.io/kueue/apis/kueue/v1beta2"
	kueuepodconst "sigs.k8s.io/kueue/pkg/controller/jobs/pod/constants"

	worker "gpustack.ai/gpustack/api/worker/v1"
	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	"gpustack.ai/gpustack/pkg/kubeapistatus"
	"gpustack.ai/gpustack/pkg/kubeclients/kubernetes/scheme"
	"gpustack.ai/gpustack/pkg/systemmeta"
	"gpustack.ai/gpustack/pkg/worker/kvcache/inject"
)

func newModelDeploymentClient(objs ...ctrlcli.Object) ctrlcli.Client {
	return ctrlfake.NewClientBuilder().
		WithScheme(scheme.Scheme).
		// The Binding is here because the deployment writes its usedBy through the status
		// subresource. Left out, the claim would be written as a whole-object update, and the
		// counting fixture would attribute the most frequent cross-object write to the wrong hook.
		WithStatusSubresource(&workercore.ModelDeployment{}, &workercore.KVCachePoolBinding{}).
		WithObjects(objs...).
		Build()
}

// modelDeploymentWrites counts every write a reconcile pass issues, so "issues no writes" can be
// asserted as the absence of a call rather than inferred from state that happens to look unchanged.
// A fake client renumbers a recreated object from one, so resource versions cannot tell a pass that
// wrote nothing from one that deleted and rebuilt everything.
type modelDeploymentWrites struct {
	creates, updates, deletes int
	// deleteGrace records the grace period each delete was issued with, nil meaning the object's
	// own default. Reading departures off the Pods only works while a departing Pod is still there
	// to read, so how a delete is issued is part of this controller's contract.
	deleteGrace []*int64
	// statusUpdates is counted separately because a status write goes through the subresource
	// client, which the Update hook never sees. Leaving it out would leave the most likely churn
	// uncounted: a status rebuilt from scratch every pass is one careless field away from
	// differing from itself forever.
	statusUpdates int
}

func newCountingModelDeploymentClient(w *modelDeploymentWrites, objs ...ctrlcli.Object) ctrlcli.Client {
	return ctrlfake.NewClientBuilder().
		WithScheme(scheme.Scheme).
		// The Binding is here because the deployment writes its usedBy through the status
		// subresource. Left out, the claim would be written as a whole-object update, and the
		// counting fixture would attribute the most frequent cross-object write to the wrong hook.
		WithStatusSubresource(&workercore.ModelDeployment{}, &workercore.KVCachePoolBinding{}).
		WithObjects(objs...).
		WithInterceptorFuncs(ctrlinterceptor.Funcs{
			Create: func(ctx context.Context, c ctrlcli.WithWatch, obj ctrlcli.Object, opts ...ctrlcli.CreateOption) error {
				w.creates++
				return c.Create(ctx, obj, opts...)
			},
			Update: func(ctx context.Context, c ctrlcli.WithWatch, obj ctrlcli.Object, opts ...ctrlcli.UpdateOption) error {
				w.updates++
				return c.Update(ctx, obj, opts...)
			},
			Delete: func(ctx context.Context, c ctrlcli.WithWatch, obj ctrlcli.Object, opts ...ctrlcli.DeleteOption) error {
				w.deletes++
				do := new(ctrlcli.DeleteOptions)
				do.ApplyOptions(opts)
				w.deleteGrace = append(w.deleteGrace, do.GracePeriodSeconds)

				return c.Delete(ctx, obj, opts...)
			},
			SubResourceUpdate: func(
				ctx context.Context, c ctrlcli.Client, sub string, obj ctrlcli.Object,
				opts ...ctrlcli.SubResourceUpdateOption,
			) error {
				w.statusUpdates++
				return c.Status().Update(ctx, obj, opts...)
			},
		}).
		Build()
}

func reconcileModelDeployment(t *testing.T, cli ctrlcli.Client) (ctrl.Result, error) {
	t.Helper()

	return reconcileModelDeploymentWith(t, &ModelDeploymentReconciler{
		Client: cli, APIReader: cli, Recorder: ctrlrecord.NewFakeRecorder(64),
	})
}

func reconcileModelDeploymentWith(t *testing.T, r *ModelDeploymentReconciler) (ctrl.Result, error) {
	t.Helper()

	return r.Reconcile(context.Background(), ctrlreconcile.Request{
		NamespacedName: ctrlcli.ObjectKey{Namespace: "team-a", Name: "qwen"},
	})
}

// drainEvents collects everything a fake recorder has buffered without blocking on an empty one.
func drainEvents(recorder *ctrlrecord.FakeRecorder) []string {
	var events []string
	for {
		select {
		case e := <-recorder.Events:
			events = append(events, e)
		default:
			return events
		}
	}
}

// replicaNames lists the replicas the deployment owns, sorted, so a case can state the whole set it
// expects rather than probing for the names it happens to think of.
func replicaNames(t *testing.T, cli ctrlcli.Client) []string {
	t.Helper()

	podList := new(core.PodList)
	require.NoError(t, cli.List(context.Background(), podList, ctrlcli.InNamespace("team-a")))

	names := make([]string, 0, len(podList.Items))
	for i := range podList.Items {
		names = append(names, podList.Items[i].Name)
	}
	slices.Sort(names)

	return names
}

func TestModelDeploymentOwnedResourceRequiresAResourceNote(t *testing.T) {
	role := new(core.Pod)
	systemmeta.NoteResource(role, ModelDeploymentResourceType,
		map[string]string{ModelDeploymentResourceNoteRole: "prefill"})
	router := new(core.Pod)
	systemmeta.NoteResource(router, ModelDeploymentResourceType,
		map[string]string{modelDeploymentResourceNoteRouter: "llm-d-router"})
	withoutNote := new(core.Pod)
	systemmeta.NoteResource(withoutNote, ModelDeploymentResourceType, nil)
	wrongType := role.DeepCopy()
	systemmeta.NoteResource(wrongType, "instances", nil)

	assert.True(t, modelDeploymentOwnedResource(role))
	assert.True(t, modelDeploymentOwnedResource(router))
	assert.False(t, modelDeploymentOwnedResource(withoutNote))
	assert.False(t, modelDeploymentOwnedResource(wrongType))
}

// replicaHashes reads back the fingerprint each running replica was built from, which is what a
// rollout actually moves.
func replicaHashes(t *testing.T, cli ctrlcli.Client) map[string]string {
	t.Helper()

	podList := new(core.PodList)
	require.NoError(t, cli.List(context.Background(), podList, ctrlcli.InNamespace("team-a")))

	hashes := make(map[string]string, len(podList.Items))
	for i := range podList.Items {
		hashes[podList.Items[i].Name] = podList.Items[i].Annotations[modelDeploymentPodSpecHashAnnotation]
	}

	return hashes
}

// replicaImages reads back the image each running replica was built from. It is what tells a replica
// rendered before a template edit from one rendered after it, which a hash cannot: a hash says two
// renders differ and never which spec either of them came from.
func replicaImages(t *testing.T, cli ctrlcli.Client) map[string]string {
	t.Helper()

	podList := new(core.PodList)
	require.NoError(t, cli.List(context.Background(), podList, ctrlcli.InNamespace("team-a")))

	images := make(map[string]string, len(podList.Items))
	for i := range podList.Items {
		images[podList.Items[i].Name] = podList.Items[i].Spec.Containers[0].Image
	}

	return images
}

func getModelDeployment(t *testing.T, cli ctrlcli.Client) *workercore.ModelDeployment {
	t.Helper()

	md := new(workercore.ModelDeployment)
	require.NoError(t, cli.Get(context.Background(),
		ctrlcli.ObjectKey{Namespace: "team-a", Name: "qwen"}, md))

	return md
}

// askingGroupWorkload builds the Workload Kueue composes for the deployment's group -- plain owner
// references to every replica, no controller reference -- with the replacement ask set or not. The
// UID is stamped by the fixture rather than the environment, so a same-object assertion cannot pass
// on two empty values.
func askingGroupWorkload(pods []core.Pod, asking bool) *kueue.Workload {
	wl := &kueue.Workload{}
	wl.Name, wl.Namespace, wl.UID = "qwen-wl", "team-a", types.UID("wl-uid")
	for i := range pods {
		wl.OwnerReferences = append(wl.OwnerReferences, meta.OwnerReference{
			APIVersion: "v1", Kind: "Pod", Name: pods[i].Name, UID: pods[i].UID,
		})
	}
	if asking {
		wl.Status.Conditions = []meta.Condition{{
			Type:               kueueWorkloadWaitingForReplacementPods,
			Status:             meta.ConditionTrue,
			Reason:             "PodsDeleted",
			LastTransitionTime: meta.Now(),
		}}
	}

	return wl
}

// replicaGroupWorkload composes the Workload Kueue builds for ONE replica's group: named after the
// group verbatim, and owned by that replica alone.
//
// IT IS THE SHAPE EVERY GROUP THIS OPERATOR RENDERS HAS, and askingGroupWorkload cannot express it:
// that one pools every Pod it is given under a single name, so a case built on it always leaves a
// member standing beside a departing one. A group of one has no such sibling, which is the whole of
// what makes a departure from it unrecoverable.
func replicaGroupWorkload(name string, pods ...core.Pod) *kueue.Workload {
	wl := &kueue.Workload{}
	wl.Name, wl.Namespace, wl.UID = name, "team-a", types.UID("wl-"+name)
	for i := range pods {
		wl.OwnerReferences = append(wl.OwnerReferences, meta.OwnerReference{
			APIVersion: "v1", Kind: "Pod", Name: pods[i].Name, UID: pods[i].UID,
		})
	}

	return wl
}

// setGroupAsk flips the replacement ask on the stored Workload, standing in for the Kueue pass that
// would write it: nothing in this tree runs Kueue, and the ask is its answer to a departure.
func setGroupAsk(t *testing.T, cli ctrlcli.Client, asking bool) {
	t.Helper()

	wl := new(kueue.Workload)
	require.NoError(t, cli.Get(context.Background(),
		ctrlcli.ObjectKey{Namespace: "team-a", Name: "qwen-wl"}, wl))
	if asking {
		wl.Status.Conditions = []meta.Condition{{
			Type:               kueueWorkloadWaitingForReplacementPods,
			Status:             meta.ConditionTrue,
			Reason:             "PodsDeleted",
			LastTransitionTime: meta.Now(),
		}}
	} else {
		wl.Status.Conditions = nil
	}
	require.NoError(t, cli.Update(context.Background(), wl))
}

// admittedReplicaWorkload builds the Workload Kueue composes for ONE replica's group: named after
// the group the Pod's own membership label carries -- which is Kueue's own rule, the group name
// verbatim -- owning that Pod by a plain reference, admitted or still pending as the case needs.
//
// The UIDs are stamped by the fixture rather than the environment, so an ownership match between a
// Pod and a Workload cannot pass on two empty values.
func admittedReplicaWorkload(pod *core.Pod, admitted bool) *kueue.Workload {
	group := pod.Labels[kueuepodconst.GroupNameLabel]
	wl := &kueue.Workload{}
	wl.Name, wl.Namespace, wl.UID = group, pod.Namespace, types.UID("wl-"+group)
	wl.OwnerReferences = []meta.OwnerReference{{
		APIVersion: "v1", Kind: "Pod", Name: pod.Name, UID: pod.UID,
	}}
	if admitted {
		wl.Status.Conditions = []meta.Condition{{
			Type:               kueue.WorkloadAdmitted,
			Status:             meta.ConditionTrue,
			Reason:             "Admitted",
			LastTransitionTime: meta.Now(),
		}}
	}

	return wl
}

// standInForKueue plays the half of the handshake this tree cannot run: after a pass, it finishes
// the departures the pass issued -- the drain completes and the finalizer releases -- and composes
// a Workload for every live replica whose group has none, admitted or pending as the case asks.
//
// THE PODS ARE STAMPED WITH UIDs BEFORE ANYTHING OWNS THEM, because the fake client assigns none
// to a GenerateName create and an empty UID on both sides of an ownership match matches
// everything: without the stamp, the workload the guard counts could not tell one replica from
// another.
func standInForKueue(t *testing.T, cli ctrlcli.Client, admit bool) {
	t.Helper()
	ctx := context.Background()

	for _, pod := range replicaPods(t, cli) {
		if pod.DeletionTimestamp == nil || !slices.Contains(pod.Finalizers, kueuepodconst.PodFinalizer) {
			continue
		}
		released := pod.DeepCopy()
		released.Finalizers = nil
		require.NoError(t, cli.Update(ctx, released))
	}

	for _, pod := range replicaPods(t, cli) {
		if pod.UID != "" {
			continue
		}
		live := pod.DeepCopy()
		live.UID = types.UID("pod-" + pod.Name)
		require.NoError(t, cli.Update(ctx, live))
	}

	composed := make(map[string]*kueue.Workload)
	wlList := new(kueue.WorkloadList)
	require.NoError(t, cli.List(ctx, wlList, ctrlcli.InNamespace("team-a")))
	for i := range wlList.Items {
		wl := new(kueue.Workload)
		wlList.Items[i].DeepCopyInto(wl)
		composed[wl.Name] = wl
	}

	for _, pod := range replicaPods(t, cli) {
		if pod.DeletionTimestamp != nil {
			continue
		}
		group := pod.Labels[kueuepodconst.GroupNameLabel]
		if composed[group] == nil {
			// RECORDED AS COMPOSED, so the rest of the group adopts it below rather than trying to
			// create a second object under the same name. A group holds one Workload however many
			// members it has, and without this the stand-in could only ever serve groups of one.
			wl := admittedReplicaWorkload(&pod, admit)
			require.NoError(t, cli.Create(ctx, wl))
			composed[group] = wl

			continue
		}

		// A live member of a group whose Workload already stands is ADOPTED by it, which is
		// Kueue's own move for a replacement whose predecessor vanished with the Workload left
		// standing: the group's Workload is the group name verbatim, and the newcomer joins the
		// reservation rather than composing a second object under a taken name.
		wl := composed[group].DeepCopy()
		owned := false
		for _, ref := range wl.OwnerReferences {
			owned = owned || ref.UID == pod.UID
		}
		if owned {
			continue
		}
		wl.OwnerReferences = append(wl.OwnerReferences, meta.OwnerReference{
			APIVersion: "v1", Kind: "Pod", Name: pod.Name, UID: pod.UID,
		})
		require.NoError(t, cli.Update(ctx, wl))
	}
}

// TestModelDeploymentReconciler_CreatesOneReplicaPerDeclaredCount is the shape of the whole feature:
// N replicas of one role, each an ordinary Pod the existing admission chain already knows how to
// handle, and no Instance anywhere.
func TestModelDeploymentReconciler_CreatesOneReplicaPerDeclaredCount(t *testing.T) {
	md := newRenderDeployment(func(md *workercore.ModelDeployment) {
		md.Spec.Roles[0].Replicas = 4
	})
	cli := newModelDeploymentClient(md, newRenderInstanceType())

	_, err := reconcileModelDeployment(t, cli)
	require.NoError(t, err)

	names := replicaNames(t, cli)
	require.Len(t, names, 4)
	seen := make(map[string]bool, len(names))
	for _, name := range names {
		assert.True(t, strings.HasPrefix(name, "qwen-server-"),
			"a replica's name is the server-assigned completion of the rendered prefix: %s", name)
		assert.False(t, seen[name], "each replica is a distinct instance: %s", name)
		seen[name] = true
	}

	instList := new(workercore.InstanceList)
	require.NoError(t, cli.List(context.Background(), instList))
	assert.Empty(t, instList.Items, "a ModelDeployment renders Pods directly and creates no Instance")
}

// TestModelDeploymentReconciler_CreatedReplicasCarryNoName pins the create request itself: the
// replica reaches the API server nameless, and the server completes the rendered prefix. A
// reconciler that quietly minted slot names client-side would pass every name-shaped assertion
// above while reintroducing the very blockage the generated names exist to remove -- a minted name
// is held by the departed Pod until Kueue releases its finalizer, and the replacement could never
// be created under it.
func TestModelDeploymentReconciler_CreatedReplicasCarryNoName(t *testing.T) {
	var created []*core.Pod
	cli := ctrlfake.NewClientBuilder().
		WithScheme(scheme.Scheme).
		WithStatusSubresource(&workercore.ModelDeployment{}, &workercore.KVCachePoolBinding{}).
		WithObjects(newRenderDeployment(), newRenderInstanceType()).
		WithInterceptorFuncs(ctrlinterceptor.Funcs{
			Create: func(
				ctx context.Context, c ctrlcli.WithWatch, obj ctrlcli.Object, opts ...ctrlcli.CreateOption,
			) error {
				if pod, ok := obj.(*core.Pod); ok {
					created = append(created, pod.DeepCopy())
				}

				return c.Create(ctx, obj, opts...)
			},
		}).
		Build()

	_, err := reconcileModelDeployment(t, cli)
	require.NoError(t, err)

	require.Len(t, created, 2)
	for _, pod := range created {
		assert.Empty(t, pod.Name, "the API server names the replica; the create carries no name")
		assert.Equal(t, "qwen-server-", pod.GenerateName,
			"and the prefix names the deployment and the role, rendered")
	}
}

// TestModelDeploymentReconciler_SecondPassWritesNothing is what makes a level-based controller safe
// to run on every Pod event. A pass that rewrote its own output would restart every replica each
// time any of them changed.
func TestModelDeploymentReconciler_SecondPassWritesNothing(t *testing.T) {
	writes := new(modelDeploymentWrites)
	cli := newCountingModelDeploymentClient(writes, newRenderDeployment(), newRenderInstanceType())

	_, err := reconcileModelDeployment(t, cli)
	require.NoError(t, err)
	require.Len(t, replicaNames(t, cli), 2)
	require.Equal(t, 4, writes.creates,
		"the first pass creates two replicas, the deployment-wide Service and the role's own")
	require.Equal(t, 1, writes.updates, "and adds the finalizer")
	require.Equal(t, 1, writes.statusUpdates, "and reports the status once")

	*writes = modelDeploymentWrites{}
	_, err = reconcileModelDeployment(t, cli)
	require.NoError(t, err)

	assert.Equal(t, modelDeploymentWrites{}, *writes,
		"an unchanged spec must issue no create, no update, no delete and no status write at all")
}

// TestModelDeploymentReconciler_TwoPassesAssignTheSameOrdinalToTheSameReplica pins the identity a
// replica's ordinal owes: two passes over an unchanged spec leave every Pod on the ordinal it was
// rendered at, and a converged role occupies zero through count-minus-one with no gap. The ordinal
// is the only per-replica key the converger reads -- the group name derives from it and the create
// gate selects on it -- so a pass that moved one would move the replica's whole identity.
func TestModelDeploymentReconciler_TwoPassesAssignTheSameOrdinalToTheSameReplica(t *testing.T) {
	md := newRenderDeployment(func(md *workercore.ModelDeployment) { md.Spec.Roles[0].Replicas = 3 })
	cli := newModelDeploymentClient(md, newRenderInstanceType())

	ordinalsOf := func() map[string]string {
		assigned := make(map[string]string, 3)
		for _, pod := range replicaPods(t, cli) {
			assigned[pod.Name] = pod.Labels[modelDeploymentReplicaOrdinalLabel]
		}

		return assigned
	}

	_, err := reconcileModelDeployment(t, cli)
	require.NoError(t, err)
	first := ordinalsOf()
	require.Len(t, first, 3)
	require.ElementsMatch(t, []string{"0", "1", "2"}, slices.Collect(maps.Values(first)),
		"a converged role occupies every ordinal from zero, with no gap")

	_, err = reconcileModelDeployment(t, cli)
	require.NoError(t, err)

	assert.Equal(t, first, ordinalsOf(),
		"an unchanged spec reassigns no replica's ordinal: the second pass found every slot as the first left it")
}

// TestModelDeploymentReconciler_ReplacesADeletedReplicaUnderAFreshName states the difference
// between converging and executing a workflow: the desired state is re-derived from the spec on
// every pass, so a replica removed by anything at all comes back -- under a name the API server
// assigns fresh, never the name the departed replica held.
//
// THE WORKLOAD IS THE CREATE GATE, so it is in the fixture asking for a replacement, exactly as
// Kueue does once it has read the departure. Its survival is half of what this case asserts: a
// departure replaced in place costs the group nothing, where the old design took the whole group
// down for one missing member.
func TestModelDeploymentReconciler_ReplacesADeletedReplicaUnderAFreshName(t *testing.T) {
	ctx := context.Background()
	cli := newModelDeploymentClient(newRenderDeployment(), newRenderInstanceType())

	_, err := reconcileModelDeployment(t, cli)
	require.NoError(t, err)
	names := replicaNames(t, cli)
	require.Len(t, names, 2)

	gone := new(core.Pod)
	require.NoError(t, cli.Get(ctx, ctrlcli.ObjectKey{Namespace: "team-a", Name: names[0]}, gone))
	require.NoError(t, cli.Delete(ctx, gone))
	require.NoError(t, cli.Create(ctx, askingGroupWorkload(replicaPods(t, cli), true)))

	_, err = reconcileModelDeployment(t, cli)
	require.NoError(t, err)

	after := replicaNames(t, cli)
	require.Len(t, after, 2)
	assert.Contains(t, after, names[1], "the survivor keeps its name and everything it holds")
	var replacement string
	for _, name := range after {
		if name != names[1] {
			replacement = name
		}
	}
	require.NotEmpty(t, replacement)
	assert.True(t, strings.HasPrefix(replacement, "qwen-server-"),
		"the replacement carries the rendered prefix: %s", replacement)
	assert.NotEqual(t, names[0], replacement,
		"and never the departed replica's name, which is the whole point of generated names")

	survivor := new(kueue.Workload)
	require.NoError(t, cli.Get(ctx, ctrlcli.ObjectKey{Namespace: "team-a", Name: "qwen-wl"}, survivor),
		"a departure replaced in place does not delete the group's Workload")
}

// TestModelDeploymentReconciler_ALostCreateResponseLeavesOnePodPerOrdinal is the create path's own
// failure window: a create the API server persisted whose response never came back. The name a
// GenerateName create receives exists only in the response that was lost, so nothing the reconciler
// holds names the Pod -- the ordinal label the reconciler itself wrote does, and the create gate
// reads it on the API server before creating again.
//
// THE FIXTURE SPLITS THE TWO VIEWS the window lives between: the reconciler's client stands in for
// the informer cache and never sees the persisted Pod, while the API reader stands in for the API
// server and holds it. A reconciler reading its cache would call both ordinals free and create a
// second Pod for each -- two members of a one-member group, which Kueue answers by deleting the
// newer one.
//
// THE CRITERION IS THE OBJECT COUNT on the server after the retry -- one Pod per ordinal -- and
// deliberately not the absence of an AlreadyExists error: the create this case stages fails with a
// lost connection, and "the retry errors differently" is a different proposition from "the retry
// creates nothing".
func TestModelDeploymentReconciler_ALostCreateResponseLeavesOnePodPerOrdinal(t *testing.T) {
	md := newRenderDeployment(func(md *workercore.ModelDeployment) { md.Spec.Roles[0].Replicas = 2 })
	server := newModelDeploymentClient(md, newRenderInstanceType())

	var lostCreates int
	cacheView := ctrlfake.NewClientBuilder().
		WithScheme(scheme.Scheme).
		WithStatusSubresource(&workercore.ModelDeployment{}, &workercore.KVCachePoolBinding{}).
		WithObjects(md, newRenderInstanceType()).
		WithInterceptorFuncs(ctrlinterceptor.Funcs{
			Create: func(
				ctx context.Context, c ctrlcli.WithWatch, obj ctrlcli.Object, opts ...ctrlcli.CreateOption,
			) error {
				pod, ok := obj.(*core.Pod)
				if !ok {
					return c.Create(ctx, obj, opts...)
				}

				// The server persists the replica; the response never arrives.
				lostCreates++
				if err := server.Create(ctx, pod); err != nil {
					return err
				}

				return errors.New("connection lost: the create's response never arrived")
			},
		}).
		Build()

	r := &ModelDeploymentReconciler{
		Client: cacheView, APIReader: server, Recorder: ctrlrecord.NewFakeRecorder(64),
	}

	_, err := reconcileModelDeploymentWith(t, r)
	require.Error(t, err, "the pass reports the create that never answered")

	// The window, as the cluster would hold it: the server has one Pod per ordinal, the cache
	// view has neither, and no response ever named either object.
	serverPods := replicaPods(t, server)
	require.Len(t, serverPods, 2)
	require.Empty(t, replicaPods(t, cacheView),
		"the cached view never saw the creates: this is the state the gate has to catch")

	lostBefore := lostCreates
	_, err = reconcileModelDeploymentWith(t, r)
	require.NoError(t, err,
		"the retry neither fails nor treats the loss as unwritten: it finds what the server holds")

	assert.Equal(t, lostBefore, lostCreates,
		"no create is issued for an ordinal the API server already holds a Pod for")
	assert.Empty(t, replicaPods(t, cacheView),
		"and nothing was created into the cached view either: the gate decided, not the cache")

	counts := make(map[string]int, 2)
	for _, pod := range replicaPods(t, server) {
		counts[pod.Labels[modelDeploymentReplicaOrdinalLabel]]++
	}
	assert.Equal(t, map[string]int{"0": 1, "1": 1}, counts,
		"exactly one Pod per ordinal: the retry left the lost create's work standing, once")
}

// TestModelDeploymentReconciler_CountChangeTrimsTheHighestOrdinals pins that a replicas edit IS a
// trim: every member's group is its own one-member group, so a change to the declared count moves
// no total any running Pod carries, and the ordinals the new count no longer names go while the
// ones it still names stand exactly where they were.
//
// IT TAKES ONE PASS. The trim removes the departing ordinals and touches nothing else; asserting an
// empty deployment in between is what a whole-group rebuild would produce and this one must not.
// The survivors are told apart from fresh renders with a marker the renderer never writes, because
// the names say nothing: a rebuild that deleted and recreated everyone would leave the same names.
func TestModelDeploymentReconciler_CountChangeTrimsTheHighestOrdinals(t *testing.T) {
	ctx := context.Background()
	md := newRenderDeployment(func(md *workercore.ModelDeployment) { md.Spec.Roles[0].Replicas = 4 })
	cli := newModelDeploymentClient(md, newRenderInstanceType())

	_, err := reconcileModelDeployment(t, cli)
	require.NoError(t, err)
	require.Len(t, replicaNames(t, cli), 4)

	const stayed = "test.gpustack.ai/stayed"
	for _, pod := range replicaPods(t, cli) {
		live := pod.DeepCopy()
		live.Annotations[stayed] = "yes"
		require.NoError(t, cli.Update(ctx, live))
	}

	scaled := getModelDeployment(t, cli)
	scaled.Spec.Roles[0].Replicas = 2
	require.NoError(t, cli.Update(ctx, scaled))

	_, err = reconcileModelDeployment(t, cli)
	require.NoError(t, err)
	names := replicaNames(t, cli)
	require.Len(t, names, 2, "the trim lands in one pass: two ordinals go, two stay")
	for _, name := range names {
		assert.True(t, strings.HasPrefix(name, "qwen-server-"),
			"the kept replicas carry the rendered prefix: %s", name)
	}
	marked := 0
	for _, pod := range replicaPods(t, cli) {
		if pod.Annotations[stayed] == "yes" {
			marked++
		}
	}
	assert.Equal(t, 2, marked,
		"the survivors are the very objects the edit found running, not fresh renders beside them")
}

// surplusReplicaAt renders one replica AS THE PASS ITSELF WOULD FOR THAT SLOT, gives it an
// explicit identity, and hands it to the fixture: the surplus cases need a member whose
// fingerprint is under the test's control and whose slot is under the test's control, which no
// reconciler-issued create gives. A non-empty hash overrides the rendered fingerprint, which is
// how a stale member is placed. The rendered prefix it still carries is inert once a name is
// assigned; nothing in the reconciler reads it.
func surplusReplicaAt(
	t *testing.T, md *workercore.ModelDeployment, name string, ordinal int, hash string,
) *core.Pod {
	t.Helper()

	pod, err := renderModelDeploymentPod(context.Background(), ModelDeploymentRenderInput{
		Deployment: md, Role: &md.Spec.Roles[0],
		InstanceType: newRenderInstanceType(), Ordinal: ordinal,
	})
	require.NoError(t, err)
	pod.Name = name
	if hash != "" {
		pod.Annotations[modelDeploymentPodSpecHashAnnotation] = hash
	}

	return pod
}

// TestModelDeploymentReconciler_ScaleDownShedsTheSurplusBySlot pins the choice the surplus rule
// makes. Three live replicas against a role declaring two, every one of them on a slot the spec
// keeps -- the duplicates-on-one-ordinal state a replacement created beside a departing holder
// leaves behind -- and the pass sheds exactly one: the member the slot's current render does NOT
// describe. The choice is asserted, not observed: the names say which replica went.
//
// IT TAKES A HAND-BUILT SET, because a spec edit cannot produce this state: a replicas change
// removes ordinals the new count no longer names, and this state is duplicates ON one ordinal --
// members no ordinal rule can tell apart, which only a hand places.
func TestModelDeploymentReconciler_ScaleDownShedsTheSurplusBySlot(t *testing.T) {
	t.Run("the seat keeps the member its current render describes", func(t *testing.T) {
		md := newRenderDeployment() // declares two
		stale := surplusReplicaAt(t, md, "qwen-server-dup-z", 0, "a-hash-no-render-produces")
		keeper := surplusReplicaAt(t, md, "qwen-server-dup-a", 0, "")
		seated := surplusReplicaAt(t, md, "qwen-server-one", 1, "")

		cli := newModelDeploymentClient(md, newRenderInstanceType(), stale, keeper, seated)

		res, err := reconcileModelDeployment(t, cli)
		require.NoError(t, err)

		assert.Equal(t, []string{"qwen-server-dup-a", "qwen-server-one"}, replicaNames(t, cli),
			"the stale duplicate goes even though its name alone would have kept it: the seat "+
				"belongs to the member the slot's current render describes")
		assert.Positive(t, res.RequeueAfter, "a pass that removed a replica comes back for the ask")
	})

	t.Run("the greatest name keeps the seat when nothing separates the members", func(t *testing.T) {
		md := newRenderDeployment() // declares two
		lesser := surplusReplicaAt(t, md, "qwen-server-dup-a", 0, "")
		greater := surplusReplicaAt(t, md, "qwen-server-dup-z", 0, "")
		seated := surplusReplicaAt(t, md, "qwen-server-one", 1, "")

		cli := newModelDeploymentClient(md, newRenderInstanceType(), lesser, greater, seated)

		_, err := reconcileModelDeployment(t, cli)
		require.NoError(t, err)

		assert.Equal(t, []string{"qwen-server-dup-z", "qwen-server-one"}, replicaNames(t, cli),
			"two members the render cannot tell apart are settled by name, which is determinism "+
				"rather than meaning")
	})
}

// TestModelDeploymentReconciler_RecreatesOnASpecChange covers the rollout policy: recreate, no
// surge, ONE REPLICA AT A TIME. A pass deletes at most one outdated replica and its own Workload,
// and creates nothing beside the deletion; the next pass creates the replacement once the ordinal
// reads empty, and the two-step repeats until every replica was built from the current spec.
//
// THE FIXTURE'S WORKLOADS ARE ADMITTED, because the rollout's currency is an admitted replica: the
// stand-in plays the Kueue half -- composing and admitting a Workload per live replica, releasing
// the finalizer of a departing one -- and without it the guard holds the rollout, which is the
// behavior its own case below pins.
func TestModelDeploymentReconciler_RecreatesOnASpecChange(t *testing.T) {
	cli := newModelDeploymentClient(newRenderDeployment(), newRenderInstanceType())

	_, err := reconcileModelDeployment(t, cli)
	require.NoError(t, err)
	before := replicaNames(t, cli)
	require.Len(t, before, 2)
	standInForKueue(t, cli, true)

	changed := getModelDeployment(t, cli)
	changed.Spec.Roles[0].Image = "vllm/vllm-openai:v0.26.0"
	require.NoError(t, cli.Update(context.Background(), changed))

	// One outdated replica goes with its Workload; nothing is created beside the deletion.
	_, err = reconcileModelDeployment(t, cli)
	require.NoError(t, err)
	mid := replicaNames(t, cli)
	require.Len(t, mid, 1, "the rollout deletes at most one replica per pass")
	assert.Contains(t, before, mid[0], "and it is one of the replicas that existed before the edit")

	// The missing ordinal comes back once it reads empty on the API server, built from the new
	// spec, and the stand-in admits it -- which is what releases the guard for the next departure.
	_, err = reconcileModelDeployment(t, cli)
	require.NoError(t, err)
	require.Len(t, replicaNames(t, cli), 2)
	standInForKueue(t, cli, true)

	// The last outdated replica goes in its own turn; the pass before's replacement survives it.
	_, err = reconcileModelDeployment(t, cli)
	require.NoError(t, err)
	last := replicaNames(t, cli)
	require.Len(t, last, 1)
	assert.NotContains(t, before, last[0],
		"the survivor of the second delete pass is the replica the second replace pass created")

	// And the count is restored; every replica now carries the edit.
	_, err = reconcileModelDeployment(t, cli)
	require.NoError(t, err)

	podList := new(core.PodList)
	require.NoError(t, cli.List(context.Background(), podList, ctrlcli.InNamespace("team-a")))
	require.Len(t, podList.Items, 2)
	for i := range podList.Items {
		assert.Equal(t, "vllm/vllm-openai:v0.26.0", podList.Items[i].Spec.Containers[0].Image,
			"%s is built from the new spec, not the old one", podList.Items[i].Name)
	}
}

// TestModelDeploymentReconciler_CountHealsBeforeTheHashRolls pins the order the convergence works
// in. A role short of its declared count does not roll its outdated members in the same breath:
// deleting from an already-short set widens exactly the gap the pass is trying to close, so the
// missing replica is created first and the outdated survivor is rolled by the pass after.
//
// THE FIXTURE'S WORKLOADS ARE ADMITTED, with the stand-in keeping them so as the count moves: the
// guard that turns a replica over counts admitted Workloads, and this case stages the heal
// happening while the replacement's admission has not landed -- the survivor's turnover waits for
// it, which is the heal-first order asserted from the other side.
func TestModelDeploymentReconciler_CountHealsBeforeTheHashRolls(t *testing.T) {
	ctx := context.Background()
	cli := newModelDeploymentClient(newRenderDeployment(), newRenderInstanceType())

	_, err := reconcileModelDeployment(t, cli)
	require.NoError(t, err)
	names := replicaNames(t, cli)
	require.Len(t, names, 2)
	standInForKueue(t, cli, true)

	// One replica vanishes outright, the way a finished eviction leaves it: no DeletionTimestamp
	// for the pass to observe, and the role merely short of its count.
	gone := new(core.Pod)
	require.NoError(t, cli.Get(ctx, ctrlcli.ObjectKey{Namespace: "team-a", Name: names[0]}, gone))
	require.NoError(t, cli.Delete(ctx, gone))

	// And the spec moves, so the surviving replica's fingerprint no longer matches.
	changed := getModelDeployment(t, cli)
	changed.Spec.Roles[0].Image = "vllm/vllm-openai:v0.26.0"
	require.NoError(t, cli.Update(ctx, changed))

	_, err = reconcileModelDeployment(t, cli)
	require.NoError(t, err)

	// The count is healed in this pass -- one new replica built from the current spec -- and the
	// outdated survivor is left standing until the count is right.
	images := replicaImages(t, cli)
	require.Len(t, images, 2)
	var byImage []string
	for name, image := range images {
		switch image {
		case "vllm/vllm-openai:v0.25.1":
			assert.Equal(t, names[1], name, "the survivor is the replica the edit found running")
		case "vllm/vllm-openai:v0.26.0":
			byImage = append(byImage, name)
		default:
			t.Fatalf("unexpected image %q on %s", image, name)
		}
	}
	require.Len(t, byImage, 1, "the missing replica came back built from the current spec")

	// The replacement's admission is what releases the guard, and the pass after rolls the
	// survivor now that the count and the admitted count are both whole.
	standInForKueue(t, cli, true)
	_, err = reconcileModelDeployment(t, cli)
	require.NoError(t, err)
	assert.Len(t, replicaNames(t, cli), 1, "the outdated survivor goes once the count is right")

	_, err = reconcileModelDeployment(t, cli)
	require.NoError(t, err)
	for _, image := range replicaImages(t, cli) {
		assert.Equal(t, "vllm/vllm-openai:v0.26.0", image)
	}
	assert.Len(t, replicaImages(t, cli), 2)
}

// TestModelDeploymentReconciler_LocksBeforeRendering pins the ordering the teardown depends on. The
// finalizer has to be on the object before the first replica exists, or a deployment deleted
// moments after creation would leave replicas holding accelerators nothing accounts for.
func TestModelDeploymentReconciler_LocksBeforeRendering(t *testing.T) {
	cli := newModelDeploymentClient(newRenderDeployment(), newRenderInstanceType())

	_, err := reconcileModelDeployment(t, cli)
	require.NoError(t, err)

	assert.True(t, systemmeta.IsLocked(getModelDeployment(t, cli)))
}

// TestModelDeploymentReconciler_TeardownHoldsTheFinalizerUntilTheReplicasAreGone is the whole reason
// the finalizer exists. Releasing it as soon as the deletes are issued would let the object vanish
// while its replicas still run.
func TestModelDeploymentReconciler_TeardownHoldsTheFinalizerUntilTheReplicasAreGone(t *testing.T) {
	md := newRenderDeployment()
	md.Finalizers = []string{systemmeta.LockedResourceFinalizer}
	now := meta.Now()
	md.DeletionTimestamp = &now

	pod := &core.Pod{ObjectMeta: meta.ObjectMeta{
		Namespace: "team-a",
		Name:      "qwen-server-0",
		Labels: map[string]string{
			modelDeploymentLabelKeyName:     modelDeploymentLabelValueName,
			modelDeploymentLabelKeyInstance: "qwen",
		},
		OwnerReferences: []meta.OwnerReference{{
			APIVersion: workercore.SchemeGroupVersion.String(),
			Kind:       "ModelDeployment",
			Name:       "qwen",
			UID:        md.UID,
			Controller: boolPtr(true),
		}},
		Finalizers: []string{"test.gpustack.ai/hold"},
	}}
	systemmeta.NoteResource(pod, ModelDeploymentResourceType, nil)

	cli := newModelDeploymentClient(md, pod, newRenderInstanceType())

	res, err := reconcileModelDeployment(t, cli)
	require.NoError(t, err)
	assert.Positive(t, res.RequeueAfter, "a teardown with replicas still present must come back")
	assert.True(t, systemmeta.IsLocked(getModelDeployment(t, cli)),
		"the finalizer is held while a replica is still terminating")

	held := new(core.Pod)
	require.NoError(t, cli.Get(context.Background(),
		ctrlcli.ObjectKey{Namespace: "team-a", Name: "qwen-server-0"}, held))
	assert.NotNil(t, held.DeletionTimestamp, "and the replica has been asked to go")

	held.Finalizers = nil
	require.NoError(t, cli.Update(context.Background(), held))

	_, err = reconcileModelDeployment(t, cli)
	require.NoError(t, err)

	remaining := new(workercore.ModelDeployment)
	err = cli.Get(context.Background(), ctrlcli.ObjectKey{Namespace: "team-a", Name: "qwen"}, remaining)
	assert.True(t, err != nil || !systemmeta.IsLocked(remaining),
		"once the replicas are gone the finalizer is released")
}

// TestModelDeploymentReconciler_IgnoresAPodItDoesNotOwn is the guard against adopting the replicas
// of a deployment that carried the same name and has since been recreated. Their controller
// reference names a UID this object does not have, so they are neither counted nor deleted.
//
// UNDER GENERATED NAMES THE STRAY BLOCKS NOTHING: the deployment's replicas arrive under
// server-assigned names, so the one slot the stray occupies is not one of them. What is left to
// assert is that the stray is invisible to the convergence -- neither counted toward the role nor
// corrected -- while the deployment's own replicas are created around it.
func TestModelDeploymentReconciler_IgnoresAPodItDoesNotOwn(t *testing.T) {
	stray := &core.Pod{ObjectMeta: meta.ObjectMeta{
		Namespace: "team-a",
		Name:      "qwen-server-0",
		Labels: map[string]string{
			modelDeploymentLabelKeyName:     modelDeploymentLabelValueName,
			modelDeploymentLabelKeyInstance: "qwen",
		},
		OwnerReferences: []meta.OwnerReference{{
			APIVersion: workercore.SchemeGroupVersion.String(),
			Kind:       "ModelDeployment",
			Name:       "qwen",
			UID:        "a-previous-incarnation",
			Controller: boolPtr(true),
		}},
	}}

	cli := newModelDeploymentClient(newRenderDeployment(), newRenderInstanceType(), stray)

	_, err := reconcileModelDeployment(t, cli)
	require.NoError(t, err)

	ours := make([]string, 0, 2)
	for _, name := range replicaNames(t, cli) {
		if name != "qwen-server-0" {
			ours = append(ours, name)
		}
	}
	require.Len(t, ours, 2, "the deployment creates its replicas under names of their own")

	kept := new(core.Pod)
	require.NoError(t, cli.Get(context.Background(),
		ctrlcli.ObjectKey{Namespace: "team-a", Name: "qwen-server-0"}, kept))
	assert.Equal(t, "a-previous-incarnation", string(kept.OwnerReferences[0].UID),
		"a Pod this deployment does not own is left exactly as it was")
}

// TestModelDeploymentReconciler_MissingInstanceTypeIsRetried states that an unresolvable type is an
// error rather than a Pod rendered without one. The type supplies the accelerator spelling and the
// per-card resources the host request is derived from, so a replica rendered without it would ask
// for something other than what the role declared.
func TestModelDeploymentReconciler_MissingInstanceTypeIsRetried(t *testing.T) {
	cli := newModelDeploymentClient(newRenderDeployment())

	_, err := reconcileModelDeployment(t, cli)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "instance type")
	assert.Empty(t, replicaNames(t, cli), "and nothing is rendered in the meantime")
}

// TestModelDeploymentReconciler_RenderFailureIsEventedNotOnlyLogged covers the one reader-visible
// home a render failure has.
//
// Rendering aborts the pass before any status is written, so the object keeps saying what it said
// last -- Phase=Starting, "no replica has been created yet" -- and none of its conditions names the
// cause. That description fits a slow start and a PERMANENT failure identically, and some of these
// are permanent: a manufacturer with no runner backend never resolves however long the controller
// retries. Without the Event the cause exists only in the controller's own logs.
func TestModelDeploymentReconciler_RenderFailureIsEventedNotOnlyLogged(t *testing.T) {
	// A role naming no image, against an InstanceType whose detail cannot synthesize one.
	md := newRenderDeployment(func(md *workercore.ModelDeployment) {
		md.Spec.Roles[0].Image = ""
	})
	// The PERMANENT shape deliberately, not the transient one: a manufacturer with no runner backend
	// never resolves, so this is the case a retry cannot fix and a reader has to be told about.
	it := newRenderInstanceType(func(it *worker.InstanceType) {
		it.Status.Detail.Manufacturer = "cambricon" // no runner backend, and never will resolve
	})
	recorder := ctrlrecord.NewFakeRecorder(64)

	_, err := reconcileModelDeploymentWith(t, &ModelDeploymentReconciler{
		Client:    newModelDeploymentClient(md, it),
		APIReader: newModelDeploymentClient(md, it),
		Recorder:  recorder,
	})
	require.Error(t, err)

	events := drainEvents(recorder)
	require.Len(t, events, 1, "exactly one Event, so repeats aggregate rather than stream")
	assert.Contains(t, events[0], "Warning "+modelDeploymentEventRenderFailed)
	assert.Contains(t, events[0], "has no runner backend",
		"the Event carries the renderer's own message: a reason without the cause sends nobody anywhere")
}

// TestModelDeploymentReconciler_RenderFailureWithoutARecorderDoesNotPanic exercises the DEFENDED
// path rather than the defense.
//
// The guard beside this Event exists because `Recorder` is populated only by `SetupController`, so
// a reconciler built directly carries none — which is every reconciler in this package's tests. The
// sibling test above sets one, so it proves the Event is emitted and proves nothing about the nil
// case: a guard whose absence panics is only tested by a caller that would have panicked.
func TestModelDeploymentReconciler_RenderFailureWithoutARecorderDoesNotPanic(t *testing.T) {
	md := newRenderDeployment(func(md *workercore.ModelDeployment) {
		md.Spec.Roles[0].Image = ""
	})
	it := newRenderInstanceType(func(it *worker.InstanceType) {
		it.Status.Detail.Manufacturer = "cambricon" // no runner backend, and never will resolve
	})

	_, err := reconcileModelDeploymentWith(t, &ModelDeploymentReconciler{
		Client:    newModelDeploymentClient(md, it),
		APIReader: newModelDeploymentClient(md, it),
		// Recorder deliberately absent.
	})

	require.Error(t, err, "the render failure is still reported to the caller")
	assert.Contains(t, err.Error(), "has no runner backend",
		"and it is the renderer's own message, not one the missing Recorder replaced")
}

func boolPtr(b bool) *bool { return &b }

// TestMapModelDeploymentInstanceType covers the watch that keeps a synthesized image current.
//
// NOTE ON WHAT THE CROSS-NAMESPACE CASE DOES NOT PROVE: an InstanceType is cluster-scoped, so
// obj.GetNamespace() is empty, and a List scoped to that empty namespace is a List over all of
// them. An implementation that copied the Binding mapper's InNamespace(obj.GetNamespace()) would
// therefore pass a cross-namespace assertion by coincidence. The assertions that do discriminate
// are the name filter and the per-deployment de-duplication.
func TestMapModelDeploymentInstanceType(t *testing.T) {
	matching := newRenderDeployment(func(md *workercore.ModelDeployment) {
		md.Name = "matching"
		md.Spec.Roles[0].InstanceType = "h20-8x"
	})
	otherNamespace := newRenderDeployment(func(md *workercore.ModelDeployment) {
		md.Name = "elsewhere"
		md.Namespace = "team-b"
		md.Spec.Roles[0].InstanceType = "h20-8x"
	})
	otherType := newRenderDeployment(func(md *workercore.ModelDeployment) {
		md.Name = "other-type"
		md.Spec.Roles[0].InstanceType = "l4-1x"
	})
	// Two roles on the SAME type. The mapper must still enqueue one request: a duplicate is not
	// wrong so much as it is a second full reconcile for nothing, and the loop that produces it is
	// the easy thing to write.
	twoRoles := newRenderDeployment(func(md *workercore.ModelDeployment) {
		md.Name = "two-roles"
		md.Spec.Roles[0].InstanceType = "h20-8x"
		second := md.Spec.Roles[0]
		second.Name = "decode"
		md.Spec.Roles = append(md.Spec.Roles, second)
	})

	cli := newModelDeploymentClient(matching, otherNamespace, otherType, twoRoles)
	r := &ModelDeploymentReconciler{Client: cli, APIReader: cli}

	it := &worker.InstanceType{ObjectMeta: meta.ObjectMeta{Name: "h20-8x"}}
	reqs := r.mapModelDeploymentInstanceType(context.Background(), it)

	got := make([]string, 0, len(reqs))
	for _, req := range reqs {
		got = append(got, req.Namespace+"/"+req.Name)
	}
	slices.Sort(got)

	assert.Equal(t, []string{"team-a/matching", "team-a/two-roles", "team-b/elsewhere"}, got,
		"every deployment with a role on this type, once each, and none on another type")
}

// TestModelDeploymentReconciler_ReplacementWaitsForTheOrdinalToReadEmpty walks the create gate
// end to end. What a replacement waits out is the departing Pod's EXISTENCE on the API server --
// not the delete being issued, and not any ask about it: a per-replica group declares one member,
// a replacement created while the departing holder is still listed makes two, and Kueue's answer
// to that excess is to delete the newest gated Pod, the replacement itself.
//
// THE DEPARTED POD IS HELD BY KUEUE'S FINALIZER, which is the real shape of this window: measured
// on a live cluster, a deleted Pod of a serving group stays on the books -- Running through the
// drain, then Succeeded, still counted active -- for as long as the finalizer holds it, and its
// Workload being deleted is what eventually releases it. A Pod deleted bare is simply GONE, its
// ordinal has nothing to wait for, and the gate creates at once.
//
// THE ASK IS STAGED AND SHOWN NOT TO GATE. Kueue's WaitingForReplacementPods verdict is set to
// True in the middle of the wait, exactly when the old gate would have created -- and the pass
// still creates nothing, because the member still reads on the server. The ask stopped being an
// input the moment freeing an ordinal began deleting the Workload the ask lives on: by the time
// the Pod is gone there is nothing left to ask.
func TestModelDeploymentReconciler_ReplacementWaitsForTheOrdinalToReadEmpty(t *testing.T) {
	ctx := context.Background()
	cli := newModelDeploymentClient(newRenderDeployment(), newRenderInstanceType())

	_, err := reconcileModelDeployment(t, cli)
	require.NoError(t, err)
	names := replicaNames(t, cli)
	require.Len(t, names, 2)

	// The departure the way a live cluster shapes it: the delete lands, Kueue's finalizer holds
	// the Pod, and the Workload this operator's own rollout delete would have taken is gone.
	gone := new(core.Pod)
	require.NoError(t, cli.Get(ctx, ctrlcli.ObjectKey{Namespace: "team-a", Name: names[0]}, gone))
	gone.Finalizers = []string{kueuepodconst.PodFinalizer}
	require.NoError(t, cli.Update(ctx, gone))
	require.NoError(t, cli.Delete(ctx, gone))

	// While the departing member still reads on the API server, nothing is created beside it.
	res, err := reconcileModelDeployment(t, cli)
	require.NoError(t, err)
	assert.ElementsMatch(t, replicaNames(t, cli), names,
		"with the departing member still on the books, creating a replacement is the harmful move")
	assert.Positive(t, res.RequeueAfter, "the pass polls rather than sleeping forever")

	// Kueue notices the departure and asks -- the verdict the old gate waited on -- and the pass
	// still creates nothing: the member's existence is the whole of the gate, and it still reads.
	require.NoError(t, cli.Create(ctx, askingGroupWorkload(replicaPods(t, cli), true)))
	_, err = reconcileModelDeployment(t, cli)
	require.NoError(t, err)
	assert.ElementsMatch(t, replicaNames(t, cli), names,
		"the ask reading True changes nothing while the departing member is still listed: the "+
			"replacement would be the excess member Kueue deletes first")

	// The drain completes -- the workload is gone, so the finalizer releases -- and the object
	// leaves; the pass that reads the ordinal empty creates the replacement.
	released := new(core.Pod)
	require.NoError(t, cli.Get(ctx, ctrlcli.ObjectKey{Namespace: "team-a", Name: names[0]}, released))
	released.Finalizers = nil
	require.NoError(t, cli.Update(ctx, released))

	_, err = reconcileModelDeployment(t, cli)
	require.NoError(t, err)
	after := replicaNames(t, cli)
	require.Len(t, after, 2, "the survivor and the replacement for the vacated ordinal")
	var replacement string
	for _, name := range after {
		if name != names[1] {
			replacement = name
		}
	}
	require.NotEmpty(t, replacement)
	assert.True(t, strings.HasPrefix(replacement, "qwen-server-"),
		"the replacement carries the rendered prefix: %s", replacement)
	assert.NotEqual(t, names[0], replacement,
		"and never the departed replica's name, which is the whole point of generated names")
}

// TestModelDeploymentReconciler_ATemplateEditRollsOneReplicaAtATime is the whole rollout cadence on
// a role of three. The edit must take at least three passes -- one departure at a time -- the role
// must never be more than one replica away from its declared count, and each departure must take
// that replica's own Workload with it, because a replacement is a fresh admission rather than a
// rider on the departed one's reservation.
//
// THE FIXTURE STANDS IN FOR KUEUE'S HALF of the handshake: nothing in this tree runs it, so the
// stand-in finishes every drain the pass issued and composes an admitted Workload for each live
// replica lacking one -- exactly when the real controller would.
func TestModelDeploymentReconciler_ATemplateEditRollsOneReplicaAtATime(t *testing.T) {
	ctx := context.Background()
	md := newRenderDeployment(func(md *workercore.ModelDeployment) { md.Spec.Roles[0].Replicas = 3 })
	cli := newModelDeploymentClient(md, newRenderInstanceType())

	_, err := reconcileModelDeployment(t, cli)
	require.NoError(t, err)
	require.Len(t, replicaNames(t, cli), 3)
	standInForKueue(t, cli, true)

	// The marker the stand-in never writes again, so "the Workload that admitted the group" can
	// be told from the composition that followed a departure: names collide per ordinal -- a
	// group's Workload is the group name verbatim -- and UIDs are the fixture's own stamps, so
	// neither names identity here.
	const composedBeforeTheEdit = "test.gpustack.ai/composed-before-the-edit"
	wlList := new(kueue.WorkloadList)
	require.NoError(t, cli.List(ctx, wlList, ctrlcli.InNamespace("team-a")))
	for i := range wlList.Items {
		wl := wlList.Items[i].DeepCopy()
		if wl.Annotations == nil {
			wl.Annotations = make(map[string]string, 1)
		}
		wl.Annotations[composedBeforeTheEdit] = "yes"
		require.NoError(t, cli.Update(ctx, wl))
	}

	changed := getModelDeployment(t, cli)
	changed.Spec.Roles[0].Image = "vllm/vllm-openai:v0.26.0"
	require.NoError(t, cli.Update(ctx, changed))

	var deletePasses int
	for pass := 0; pass < 24; pass++ {
		before := len(replicaNames(t, cli))
		_, err = reconcileModelDeployment(t, cli)
		require.NoError(t, err, "pass %d", pass)
		after := len(replicaNames(t, cli))
		require.GreaterOrEqual(t, after, 2,
			"pass %d: never more than one replica away from the declared count", pass)
		if after < before {
			deletePasses++
		}
		standInForKueue(t, cli, true)

		current := true
		for _, image := range replicaImages(t, cli) {
			current = current && image == "vllm/vllm-openai:v0.26.0"
		}
		if current && after == 3 {
			break
		}
	}
	require.GreaterOrEqual(t, deletePasses, 3,
		"a three-replica rollout takes at least three passes: one departure at a time")

	require.Len(t, replicaImages(t, cli), 3)
	for name, image := range replicaImages(t, cli) {
		assert.Equal(t, "vllm/vllm-openai:v0.26.0", image,
			"%s is built from the edited spec", name)
	}

	// Each departure took its own Workload: every original composition is gone, and the three
	// standing are objects the stand-in composed after a departure, admitted by a fresh grant.
	wlList = new(kueue.WorkloadList)
	require.NoError(t, cli.List(ctx, wlList, ctrlcli.InNamespace("team-a")))
	require.Len(t, wlList.Items, 3, "one Workload per replica, as one group per replica leaves")
	for i := range wlList.Items {
		assert.NotContains(t, wlList.Items[i].Annotations, composedBeforeTheEdit,
			"the Workload that admitted a replaced replica went with it: the replacement was "+
				"admitted on its own, not onto the departed one's reservation")
		assert.True(t,
			kubeapistatus.ConditionType(kueue.WorkloadAdmitted).IsTrue(&wlList.Items[i]),
			"the standing Workload holds an admission")
	}
}

// TestModelDeploymentReconciler_TheCreateGateReadsTheAPIServer pins where the create gate's
// existence read goes, AND HOW MANY OF THEM IT COSTS. The read decides whether this pass creates, it
// answers about an object the informer cache may not have seen -- a persisted create whose response
// was lost -- and the pass is woken by Pod events, so the read goes to the API server rather than a
// cache that would just repeat the list the gate is checking against.
//
// THE COUNT IS PART OF THE CONTRACT because these reads bypass the cache: one per role, whatever
// number of ordinals that role is short. A read per ordinal is the same answer at N times the cost,
// billed again on every requeue for as long as a departure takes to drain.
type countingAPIReader struct {
	ctrlcli.Reader
	lists int
}

func (r *countingAPIReader) List(
	ctx context.Context, list ctrlcli.ObjectList, opts ...ctrlcli.ListOption,
) error {
	r.lists++

	return r.Reader.List(ctx, list, opts...)
}

func TestModelDeploymentReconciler_TheCreateGateReadsTheAPIServer(t *testing.T) {
	ctx := context.Background()
	cli := newModelDeploymentClient(newRenderDeployment(), newRenderInstanceType())
	reader := &countingAPIReader{Reader: cli}
	r := &ModelDeploymentReconciler{Client: cli, APIReader: reader}

	// The first pass reads the role ONCE on the API server before creating for either of its two
	// missing ordinals: the groups a first create would double-populate are exactly the ones this
	// read has to find empty, and one list of the role answers for every one of them.
	_, err := reconcileModelDeploymentWith(t, r)
	require.NoError(t, err)
	assert.Equal(t, 1, reader.lists,
		"one API-server read per role, not one per missing ordinal: this role is short two")

	// A settled pass reads nothing through the API server: every ordinal is accounted for and
	// no role is rolling.
	reader.lists = 0
	_, err = reconcileModelDeploymentWith(t, r)
	require.NoError(t, err)
	assert.Zero(t, reader.lists, "a settled pass issues no API-server read at all")

	// A short role reads the missing ordinal through the API server, and the replacement lands
	// once the ordinal reads empty.
	names := replicaNames(t, cli)
	require.Len(t, names, 2)
	require.NoError(t, cli.Delete(ctx, &core.Pod{ObjectMeta: meta.ObjectMeta{
		Namespace: "team-a", Name: names[0],
	}}))

	reader.lists = 0
	_, err = reconcileModelDeploymentWith(t, r)
	require.NoError(t, err)
	assert.Positive(t, reader.lists,
		"the short ordinal is read on the API server, not against the cache the gate checks")
	assert.Len(t, replicaNames(t, cli), 2,
		"a bare-deleted replica leaves an empty ordinal, and the gate creates into it")
}

// TestModelDeploymentReconciler_TheRolloutGuardCountsAdmittedReplicas is the full-pool half of the
// rollout: a role whose replicas are live but hold no admission yet does not turn any of them
// over. A replacement is a fresh admission -- the departed replica's Workload is deleted with it,
// reservation and all -- so a live-but-queued replacement counts as no progress at all, and a
// guard that counted live replicas would keep deleting outdated ones behind it until the
// deployment served nothing, a deficit no declared count covers.
//
// THE POSITIVE CASE IS THE PAIR THE NEGATIVE ONE NEEDS: a guard that never rolled anything would
// pass the held case unread, so beside it stands the same fixture with admissions in place and
// exactly one replica turned over.
func TestModelDeploymentReconciler_TheRolloutGuardCountsAdmittedReplicas(t *testing.T) {
	prepare := func(t *testing.T, admit bool) ctrlcli.Client {
		t.Helper()

		cli := newModelDeploymentClient(newRenderDeployment(), newRenderInstanceType())
		_, err := reconcileModelDeployment(t, cli)
		require.NoError(t, err)
		require.Len(t, replicaNames(t, cli), 2)
		standInForKueue(t, cli, admit)

		changed := getModelDeployment(t, cli)
		changed.Spec.Roles[0].Image = "vllm/vllm-openai:v0.26.0"
		require.NoError(t, cli.Update(context.Background(), changed))

		return cli
	}

	t.Run("live but not admitted holds the rollout", func(t *testing.T) {
		cli := prepare(t, false)

		res, err := reconcileModelDeployment(t, cli)
		require.NoError(t, err)

		assert.Len(t, replicaNames(t, cli), 2,
			"both replicas still stand: a queued replacement is no admission to trade on")
		wlList := new(kueue.WorkloadList)
		require.NoError(t, cli.List(context.Background(), wlList, ctrlcli.InNamespace("team-a")))
		assert.Len(t, wlList.Items, 2, "and neither replica's Workload was taken")
		assert.Positive(t, res.RequeueAfter,
			"the held rollout polls for the admission that releases it")
	})

	t.Run("admitted lets it proceed one replica at a time", func(t *testing.T) {
		cli := prepare(t, true)

		res, err := reconcileModelDeployment(t, cli)
		require.NoError(t, err)

		assert.Len(t, replicaNames(t, cli), 1,
			"with every declared replica admitted, exactly one outdated replica turns over")
		wlList := new(kueue.WorkloadList)
		require.NoError(t, cli.List(context.Background(), wlList, ctrlcli.InNamespace("team-a")))
		assert.Len(t, wlList.Items, 1,
			"and the departed replica's Workload went with it, freeing the slot the hard way")
		assert.Positive(t, res.RequeueAfter, "the pass comes back for the replacement")
	})
}

// TestModelDeploymentReconciler_ARolloutReplacesTheHighestOrdinalFirst pins which replica a rollout
// turns over first: the highest ordinal, the same end a scale-down sheds from, so the ordinals a
// rollout keeps current stay dense from zero and two passes over the same state pick the same
// victim. The choice is by slot and not by creation timestamp -- a timestamp says when an object
// was made, not which seat it holds, and the seat is what the replacement renders against.
func TestModelDeploymentReconciler_ARolloutReplacesTheHighestOrdinalFirst(t *testing.T) {
	ctx := context.Background()
	md := newRenderDeployment(func(md *workercore.ModelDeployment) { md.Spec.Roles[0].Replicas = 3 })
	cli := newModelDeploymentClient(md, newRenderInstanceType())

	_, err := reconcileModelDeployment(t, cli)
	require.NoError(t, err)
	ordinals := make(map[string]string, 3)
	for _, pod := range replicaPods(t, cli) {
		ordinals[pod.Name] = pod.Labels[modelDeploymentReplicaOrdinalLabel]
	}
	require.Len(t, ordinals, 3)
	standInForKueue(t, cli, true)

	changed := getModelDeployment(t, cli)
	changed.Spec.Roles[0].Image = "vllm/vllm-openai:v0.26.0"
	require.NoError(t, cli.Update(ctx, changed))

	_, err = reconcileModelDeployment(t, cli)
	require.NoError(t, err)

	remaining := replicaNames(t, cli)
	require.Len(t, remaining, 2, "one departure per pass, as every case above pins")
	for _, name := range remaining {
		assert.Contains(t, []string{"0", "1"}, ordinals[name],
			"%s survives the first departure: the turnover starts at the top", name)
	}
	var departed string
	for name := range ordinals {
		if slices.Contains(remaining, name) {
			continue
		}
		departed = name
	}
	require.NotEmpty(t, departed)
	assert.Equal(t, "2", ordinals[departed],
		"the replica that went is ordinal 2, the highest -- not whichever object was made first")
}

// TestModelDeploymentDeclaredParallelismPair pins the resolution rules off the deployment's own
// role list: the first role of a kind in declaration order wins whichever tier it runs on (a rule
// this function owns rather than borrows from admission), a take-over half's books are its command
// while its inert extra arguments are read by nobody, a server kind's books are read by nobody,
// and vLLM's environment-carried DP width reaches the pair.
func TestModelDeploymentDeclaredParallelismPair(t *testing.T) {
	testCases := []struct {
		name    string
		md      *workercore.ModelDeployment
		want    inject.ParallelismPair
		wantErr string
	}{
		{
			name: "both halves read off their own books",
			md: routedModelDeployment(func(md *workercore.ModelDeployment) {
				md.Spec.Roles[0].ExtraArgs = []string{"--tensor-parallel-size", "2"}
				md.Spec.Roles[1].ExtraArgs = []string{"--data-parallel-size", "3"}
			}),
			want: inject.ParallelismPair{
				Prefill: inject.Parallelism{TensorParallel: 2, DataParallel: 1},
				Decode:  inject.Parallelism{TensorParallel: 1, DataParallel: 3},
			},
		},
		{
			name: "the first role of a kind in declaration order wins",
			md: routedModelDeployment(func(md *workercore.ModelDeployment) {
				md.Spec.Roles[0].ExtraArgs = []string{"--tensor-parallel-size", "2"}
				md.Spec.Roles = append(md.Spec.Roles, workercore.ModelDeploymentRole{
					Name:      "prefill-again",
					Kind:      workercore.ModelDeploymentRoleKindPrefill,
					ExtraArgs: []string{"--tensor-parallel-size", "4"},
				})
			}),
			want: inject.ParallelismPair{
				Prefill: inject.Parallelism{TensorParallel: 2, DataParallel: 1},
				Decode:  inject.Parallelism{TensorParallel: 1, DataParallel: 1},
			},
		},
		{
			// The command is the half's books and declares nothing, so the half is all ones;
			// the inert extra arguments beside it are read by nobody -- the broken degree there
			// would fail the parse if anyone did.
			name: "a take-over half reads its command, its inert extra arguments parsed by nobody",
			md: routedModelDeployment(func(md *workercore.ModelDeployment) {
				md.Spec.Roles[0].ExtraArgs = []string{"--tensor-parallel-size", "2"}
				md.Spec.Roles[1].Command = []string{"/bin/my-server", "--flag"}
				md.Spec.Roles[1].ExtraArgs = []string{"--tensor-parallel-size", "banana"}
			}),
			want: inject.ParallelismPair{
				Prefill: inject.Parallelism{TensorParallel: 2, DataParallel: 1},
				Decode:  inject.Parallelism{TensorParallel: 1, DataParallel: 1},
			},
		},
		{
			// First of the kind in declaration order wins, whatever tier it runs on: the
			// take-over role's command is the half's books, and the later managed role's degree
			// is never read.
			name: "the first role of a kind wins whichever tier it runs on",
			md: routedModelDeployment(func(md *workercore.ModelDeployment) {
				md.Spec.Roles[0].Command = []string{"/bin/my-server", "--flag"}
				md.Spec.Roles = append(md.Spec.Roles, workercore.ModelDeploymentRole{
					Name:      "prefill-managed",
					Kind:      workercore.ModelDeploymentRoleKindPrefill,
					ExtraArgs: []string{"--tensor-parallel-size", "2"},
				})
			}),
			want: inject.ParallelismPair{
				Prefill: inject.Parallelism{TensorParallel: 1, DataParallel: 1},
				Decode:  inject.Parallelism{TensorParallel: 1, DataParallel: 1},
			},
		},
		{
			name: "a take-over command carries the half's declared degrees",
			md: routedModelDeployment(func(md *workercore.ModelDeployment) {
				md.Spec.Roles[1].Command = []string{"/bin/my-server", "--data-parallel-size", "3"}
			}),
			want: inject.ParallelismPair{
				Prefill: inject.Parallelism{TensorParallel: 1, DataParallel: 1},
				Decode:  inject.Parallelism{TensorParallel: 1, DataParallel: 3},
			},
		},
		{
			name: "an unreadable degree in a take-over command names the role",
			md: routedModelDeployment(func(md *workercore.ModelDeployment) {
				md.Spec.Roles[1].Command = []string{"/bin/my-server", "--tensor-parallel-size"}
			}),
			wantErr: `role "decode"`,
		},
		{
			name: "a server kind's books are read by nobody, not even to refuse them",
			md: twoRoleDeployment(func(md *workercore.ModelDeployment) {
				md.Spec.Roles[0].ExtraArgs = []string{"--tensor-parallel-size", "banana"}
			}),
			want: inject.ParallelismPair{},
		},
		{
			name: "vLLM's environment-carried DP width reaches the pair",
			md: routedModelDeployment(func(md *workercore.ModelDeployment) {
				md.Spec.Roles[0].Env = []workercore.ModelDeploymentEnvVar{
					{Name: "VLLM_DP_SIZE", Value: "3"},
				}
			}),
			want: inject.ParallelismPair{
				Prefill: inject.Parallelism{TensorParallel: 1, DataParallel: 3},
				Decode:  inject.Parallelism{TensorParallel: 1, DataParallel: 1},
			},
		},
		{
			name: "an unreadable degree names the role",
			md: routedModelDeployment(func(md *workercore.ModelDeployment) {
				md.Spec.Roles[1].ExtraArgs = []string{"--tensor-parallel-size"}
			}),
			wantErr: `role "decode"`,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := modelDeploymentDeclaredParallelismPair(tc.md)
			if tc.wantErr != "" {
				require.ErrorContains(t, err, tc.wantErr)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}
}

// TestModelDeployment_UnreadableDeclaredParallelismFailsTheRender pins the loud direction: a
// degree declaration nobody can read never becomes a silent 1/1 in a document the engine then
// trusts. The same books would keep the engine itself from starting, so the render fails naming
// the role -- exactly the refusal admission would have issued had the object passed through it.
func TestModelDeployment_UnreadableDeclaredParallelismFailsTheRender(t *testing.T) {
	md := routedModelDeployment(func(md *workercore.ModelDeployment) {
		md.Spec.Roles[0].ExtraArgs = []string{"--tensor-parallel-size", "banana"}
	})
	cli := newModelDeploymentClient(md, newRenderInstanceType())

	_, err := reconcileModelDeployment(t, cli)
	require.ErrorContains(t, err, `role "prefill"`)
	require.ErrorContains(t, err, "is not an integer")
}

// TestModelDeployment_UnreadableDeclaredParallelismOffThePairStaysTheEngines pins the gate the
// loud direction wears: a deployment holding ONE half renders no transfer document, so the
// resolution has no consumer and an unreadable degree on its books stays the engine's own
// startup refusal rather than failing the reconcile of every unrelated field. Admission still
// refuses the same declaration on a new object -- this path exists for objects written before
// the webhook learned the check.
func TestModelDeployment_UnreadableDeclaredParallelismOffThePairStaysTheEngines(t *testing.T) {
	md := newRenderDeployment(func(md *workercore.ModelDeployment) {
		md.Spec.Roles[0].Kind = workercore.ModelDeploymentRoleKindDecode
		md.Spec.Roles[0].ExtraArgs = []string{"--tensor-parallel-size", "banana"}
	})
	cli := newModelDeploymentClient(md, newRenderInstanceType())

	_, err := reconcileModelDeployment(t, cli)
	require.NoError(t, err)
}

// TestModelDeployment_DegreeEditRollsThePair pins the rollout shape a degree edit has: the
// parallel blocks are one document both roles carry identically, so editing one role's declared
// degree -- a flag or vLLM's env-carried DP width -- rewrites BOTH roles' Pods and every spec
// hash of the pair moves -- while an ordinary extraArgs edit moves only the hashes of the role
// whose argv changed. The pair-wide move is the one exception the role's API comment documents
// to a container-field edit rolling its own role.
func TestModelDeployment_DegreeEditRollsThePair(t *testing.T) {
	// Keyed by role and ordinal rather than by name: the name carries a random suffix per
	// render, while the slot is the identity a replacement keeps.
	renderHashes := func(t *testing.T, decodeArgs []string, decodeEnv []workercore.ModelDeploymentEnvVar) map[string]string {
		md := routedModelDeployment(func(md *workercore.ModelDeployment) {
			md.Spec.KVCache = nil
			md.Spec.Roles[0].ExtraArgs = []string{"--tensor-parallel-size", "2"}
			md.Spec.Roles[1].ExtraArgs = decodeArgs
			md.Spec.Roles[1].Env = decodeEnv
		})
		cli := newModelDeploymentClient(md, ascendRenderInstanceType())
		_, err := reconcileModelDeployment(t, cli)
		require.NoError(t, err)

		hashes := map[string]string{}
		for _, pod := range replicaPods(t, cli) {
			ordinal, ok := modelDeploymentPodOrdinal(&pod)
			require.True(t, ok, "%s carries no ordinal", pod.Name)
			key := modelDeploymentPodRole(&pod) + "/" + strconv.Itoa(ordinal)
			hash := pod.Annotations[modelDeploymentPodSpecHashAnnotation]
			require.NotEmpty(t, hash, "%s carries no spec hash", pod.Name)
			hashes[key] = hash
		}

		return hashes
	}

	base := renderHashes(t, []string{"--tensor-parallel-size", "1"}, nil)
	degreeEdited := renderHashes(t, []string{"--tensor-parallel-size", "2"}, nil)
	envEdited := renderHashes(t, []string{"--tensor-parallel-size", "1"},
		[]workercore.ModelDeploymentEnvVar{{Name: "VLLM_DP_SIZE", Value: "2"}})
	argEdited := renderHashes(t, []string{"--tensor-parallel-size", "1", "--max-log-len=100"}, nil)

	require.Len(t, base, 4)
	for slot, hash := range base {
		assert.NotEqual(t, hash, degreeEdited[slot],
			"a degree edit on the decode role rewrites the document both roles carry: %s", slot)
		assert.NotEqual(t, hash, envEdited[slot],
			"and so does an env-carried DP width, the other declared-degree source: %s", slot)

		edited := argEdited[slot]
		if strings.HasPrefix(slot, "prefill/") {
			assert.Equal(t, hash, edited,
				"an ordinary extraArgs edit on the decode role leaves the prefill Pods: %s", slot)
		} else {
			assert.NotEqual(t, hash, edited,
				"and lands on the role whose argv changed: %s", slot)
		}
	}
}
