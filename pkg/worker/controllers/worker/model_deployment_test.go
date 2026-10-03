package worker

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	core "k8s.io/api/core/v1"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	labels "k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime/schema"
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
	"gpustack.ai/gpustack/pkg/worker/settings"
)

func newModelDeploymentClient(objs ...ctrlcli.Object) ctrlcli.Client {
	// An object read from a server always carries a resource version, and the release record's
	// write is a real optimistic lock that needs one to compare against. A fixture that omits it
	// describes a state no server hands out, so it is stamped here rather than in every case.
	for _, obj := range objs {
		if md, ok := obj.(*workercore.ModelDeployment); ok && md.ResourceVersion == "" {
			md.ResourceVersion = "1"
		}
	}

	return ctrlfake.NewClientBuilder().
		WithScheme(scheme.Scheme).
		// The release observation lists a node's pods with the spec.nodeName field selector, which
		// a real API server answers directly. The fake client can only answer it when the field is
		// indexed, so the index is registered here rather than in the one case that uses it.
		WithIndex(&core.Pod{}, "spec.nodeName", func(obj ctrlcli.Object) []string {
			pod, ok := obj.(*core.Pod)
			if !ok {
				return nil
			}
			if pod.Spec.NodeName == "" {
				return nil
			}

			return []string{pod.Spec.NodeName}
		}).
		// The Binding is here because the deployment writes its usedBy through the status
		// subresource. Left out, the claim would be written as a whole-object update, and the
		// counting fixture would attribute the most frequent cross-object write to the wrong hook.
		// The release observation compares the node's own records against the ledger the device
		// manager published, and a published ledger is a status write on a cluster-scoped object.
		WithStatusSubresource(&workercore.ModelDeployment{}, &workercore.KVCachePoolBinding{},
			&workercore.Devices{}).
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
	// patches is counted separately from updates because a label convergence goes through the
	// patch path: a pass that re-derived what it already wrote must issue no patch at all, and an
	// Update-counting fixture cannot see that.
	patches int
	// ordered writes each counted write as "kind name action" in issue order, which is what turns
	// "the labels land before the selectors narrow" from an inference into an observation.
	ordered []string
}

func (w *modelDeploymentWrites) record(action string, obj ctrlcli.Object) {
	// A fake client hands the interceptor the object as it was passed, with no GVK filled in, so
	// the kind is derived from the Go type rather than read off the object.
	var kind string
	switch obj.(type) {
	case *core.Pod:
		kind = "Pod"
	case *core.Service:
		kind = "Service"
	case *workercore.ModelDeployment:
		kind = "ModelDeployment"
	default:
		kind = fmt.Sprintf("%T", obj)
	}
	w.ordered = append(w.ordered, kind+" "+obj.GetName()+" "+action)
}

func newCountingModelDeploymentClient(w *modelDeploymentWrites, objs ...ctrlcli.Object) ctrlcli.Client {
	return ctrlfake.NewClientBuilder().
		WithScheme(scheme.Scheme).
		// The Binding is here because the deployment writes its usedBy through the status
		// subresource. Left out, the claim would be written as a whole-object update, and the
		// counting fixture would attribute the most frequent cross-object write to the wrong hook.
		// The release observation compares the node's own records against the ledger the device
		// manager published, and a published ledger is a status write on a cluster-scoped object.
		WithStatusSubresource(&workercore.ModelDeployment{}, &workercore.KVCachePoolBinding{},
			&workercore.Devices{}).
		WithObjects(objs...).
		WithInterceptorFuncs(ctrlinterceptor.Funcs{
			Create: func(ctx context.Context, c ctrlcli.WithWatch, obj ctrlcli.Object, opts ...ctrlcli.CreateOption) error {
				w.creates++
				w.record("create", obj)
				return c.Create(ctx, obj, opts...)
			},
			Update: func(ctx context.Context, c ctrlcli.WithWatch, obj ctrlcli.Object, opts ...ctrlcli.UpdateOption) error {
				w.updates++
				w.record("update", obj)
				return c.Update(ctx, obj, opts...)
			},
			Delete: func(ctx context.Context, c ctrlcli.WithWatch, obj ctrlcli.Object, opts ...ctrlcli.DeleteOption) error {
				w.deletes++
				do := new(ctrlcli.DeleteOptions)
				do.ApplyOptions(opts)
				w.deleteGrace = append(w.deleteGrace, do.GracePeriodSeconds)
				w.record("delete", obj)

				return c.Delete(ctx, obj, opts...)
			},
			Patch: func(
				ctx context.Context, c ctrlcli.WithWatch, obj ctrlcli.Object,
				p ctrlcli.Patch, opts ...ctrlcli.PatchOption,
			) error {
				w.patches++
				w.record("patch", obj)
				return c.Patch(ctx, obj, p, opts...)
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

// reconcileModelDeploymentActivated runs a pass whose group-forward probe answers bound: the
// qualification activates for real, through the same collector the reconciler uses, so the
// eligibility condition and the selector narrowing below it are observed facts of this pass.
func reconcileModelDeploymentActivated(t *testing.T, cli ctrlcli.Client) (ctrl.Result, error) {
	t.Helper()

	return reconcileModelDeploymentWith(t, &ModelDeploymentReconciler{
		Client: cli, APIReader: cli, Recorder: ctrlrecord.NewFakeRecorder(64),
		groupForwardFetch: boundProbeFetch,
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
// realSurplusReplicaAt builds a Pod from the reconciling renderer's OWN output, not from the
// single-pod helper above.
//
// The single-pod helper renders one replica and stops, but the reconciler synthesizes the KV
// connector into the role as well -- the event config, its two ports and the transfer engine's
// metrics environment. A Pod built without that describes a spec the reconciler never intended to
// produce, so its fingerprint matches nothing the reconciler renders and every comparison against
// the current render falls through to whatever breaks the tie afterwards. A fixture for a rule
// about currency has to be built from the same instrument the rule reads.
func realSurplusReplicaAt(
	t *testing.T, md *workercore.ModelDeployment, name string, ordinal int, staleHash string,
) *core.Pod {
	t.Helper()

	r := &ModelDeploymentReconciler{Client: newModelDeploymentClient(md, newRenderInstanceType())}
	desired, err := r.renderModelDeploymentPods(context.Background(), md, nil, nil, nil)
	require.NoError(t, err)
	require.Contains(t, desired, "server")

	pod := desired["server"][ordinal][0].DeepCopy()
	pod.Name = name
	if staleHash != "" {
		pod.Annotations[modelDeploymentPodSpecHashAnnotation] = staleHash
	}

	return pod
}

// realRenderHashOf is the fingerprint the reconciler renders for one seat, read the same way the
// currency rule reads it.
func realRenderHashOf(
	t *testing.T, md *workercore.ModelDeployment, ordinal int,
) string {
	t.Helper()

	r := &ModelDeploymentReconciler{Client: newModelDeploymentClient(md, newRenderInstanceType())}
	desired, err := r.renderModelDeploymentPods(context.Background(), md, nil, nil, nil)
	require.NoError(t, err)

	return desired["server"][ordinal][0].Annotations[modelDeploymentPodSpecHashAnnotation]
}

// replicaNames lists the deployment's serving replica pods by name.
//
// It reads replicaPods, so a Router pod is left out: the Router is discovered in the same namespace
// but is not a seat on a replica, and a case asserting how many replicas remain must not count it.
func replicaNames(t *testing.T, cli ctrlcli.Client) []string {
	t.Helper()

	pods := replicaPods(t, cli)
	names := make([]string, 0, len(pods))
	for _, pod := range pods {
		names = append(names, pod.Name)
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
		// The release observation compares the node's own records against the ledger the device
		// manager published, and a published ledger is a status write on a cluster-scoped object.
		WithStatusSubresource(&workercore.ModelDeployment{}, &workercore.KVCachePoolBinding{},
			&workercore.Devices{}).
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
		// The release observation compares the node's own records against the ledger the device
		// manager published, and a published ledger is a status write on a cluster-scoped object.
		WithStatusSubresource(&workercore.ModelDeployment{}, &workercore.KVCachePoolBinding{},
			&workercore.Devices{}).
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
	// Router-backed: this case is about which ordinals a trim keeps, and a trim only completes when
	// the withdrawal can be observed. Without a router the surplus would be held, not removed.
	md, router := retirementRouterFixture(md)
	cli := newModelDeploymentClient(md, newRenderInstanceType(), router)

	_, err := reconcileModelDeploymentRetiring(t, cli)
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

	_, err = reconcileModelDeploymentRetiring(t, cli)
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

	// The reconciler renders with the overcommit setting it reads, so the fixture reads it too.
	pod, err := renderModelDeploymentPod(context.Background(), ModelDeploymentRenderInput{
		Deployment: md, Role: &md.Spec.Roles[0],
		InstanceType: newRenderInstanceType(), Ordinal: ordinal,
		GeneralResourcesOvercommit: settings.InstanceGeneralResourcesOvercommit.ShouldValueBool(context.Background()),
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
		// The router is declared BEFORE the replicas are rendered, because the render the seat rule
		// compares against is taken from the spec as it stands. Declaring the router afterwards
		// would leave the seeded members describing a spec that no longer exists.
		md, router := retirementRouterFixture(md)
		// BUILT FROM THE REAL RENDER, and asserted current before the rule is tested at all: if the
		// intended keeper did not describe the seat the reconciler renders, this case would be
		// asserting a tie-break's outcome rather than the currency rule that is supposed to decide.
		stale := realSurplusReplicaAt(t, md, "qwen-server-dup-z", 0, "a-hash-no-render-produces")
		keeper := realSurplusReplicaAt(t, md, "qwen-server-dup-a", 0, "")
		require.Equal(t, realRenderHashOf(t, md, 0), keeper.Annotations[modelDeploymentPodSpecHashAnnotation],
			"the intended keeper is built from the reconciler's own render and is therefore current")
		require.NotEqual(t, keeper.Annotations[modelDeploymentPodSpecHashAnnotation],
			stale.Annotations[modelDeploymentPodSpecHashAnnotation],
			"and the stale occupant's fingerprint is distinct from it")
		seated := realSurplusReplicaAt(t, md, "qwen-server-one", 1, "")

		cli := newModelDeploymentClient(md, newRenderInstanceType(), stale, keeper, seated, router)

		res, err := reconcileModelDeploymentRetiring(t, cli)
		require.NoError(t, err)

		assert.Equal(t, []string{"qwen-server-dup-a", "qwen-server-one"}, replicaNames(t, cli),
			"the stale duplicate goes even though its name alone would have kept it: the seat "+
				"belongs to the member the slot's current render describes")
		assert.Positive(t, res.RequeueAfter, "a pass that removed a replica comes back for the ask")
	})

	t.Run("the greatest name keeps the seat when nothing separates the members", func(t *testing.T) {
		md := newRenderDeployment() // declares two
		md, router := retirementRouterFixture(md)
		lesser := realSurplusReplicaAt(t, md, "qwen-server-dup-a", 0, "")
		greater := realSurplusReplicaAt(t, md, "qwen-server-dup-z", 0, "")
		seated := realSurplusReplicaAt(t, md, "qwen-server-one", 1, "")

		cli := newModelDeploymentClient(md, newRenderInstanceType(), lesser, greater, seated, router)

		_, err := reconcileModelDeploymentRetiring(t, cli)
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
		assert.Zero(t, res.RequeueAfter,
			"the held rollout waits for the admission's own event rather than polling for it")
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

// TestModelDeploymentReconciler_ExpectedBindingClaimFailuresAreQuiet pins how a pass ends when the
// Binding it claims changed or was deleted after it was read. Neither is logged as an error or
// returned, and neither requeues: the Binding watch passes every change to it, so the change behind
// the conflict wakes the deployment again. Any other failure is still returned and logged, and the
// next pass writes the claim in every case.
func TestModelDeploymentReconciler_ExpectedBindingClaimFailuresAreQuiet(t *testing.T) {
	gr := schema.GroupResource{Group: workercore.GroupVersion.Group, Resource: "kvcachepoolbindings"}
	testCases := []struct {
		name       string
		err        error
		wantErr    bool
		wantLogged int
	}{
		{
			name: "a conflicting claim ends quietly, for the binding watch to retry",
			err:  kerrors.NewConflict(gr, "shared-kv", fmt.Errorf("the object has been modified")),
		},
		{
			name: "a claim on a deleted binding ends quietly",
			err:  kerrors.NewNotFound(gr, "shared-kv"),
		},
		{
			name:       "any other claim failure is returned and logged",
			err:        kerrors.NewInternalError(fmt.Errorf("etcd unavailable")),
			wantErr:    true,
			wantLogged: 1,
		},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			var injected bool
			fail := failOnce(tc.err)
			cli := ctrlinterceptor.NewClient(
				newModelDeploymentClient(newRenderDeployment(), newRenderInstanceType(), newRenderBinding()).(ctrlcli.WithWatch),
				ctrlinterceptor.Funcs{
					SubResourceUpdate: func(ctx context.Context, c ctrlcli.Client, sub string, obj ctrlcli.Object, opts ...ctrlcli.SubResourceUpdateOption) error {
						if _, ok := obj.(*workercore.KVCachePoolBinding); ok {
							if err := fail(); err != nil {
								injected = true
								return err
							}
						}
						return c.SubResource(sub).Update(ctx, obj, opts...)
					},
				})
			r := &ModelDeploymentReconciler{Client: cli, APIReader: cli, Recorder: ctrlrecord.NewFakeRecorder(64)}
			req := ctrlreconcile.Request{NamespacedName: ctrlcli.ObjectKey{Namespace: "team-a", Name: "qwen"}}

			out := reconcileUntilInjected(t, r, req, &injected)
			if tc.wantErr {
				assert.Error(t, out.err)
			} else {
				assert.NoError(t, out.err)
			}
			assert.Equal(t, ctrlreconcile.Result{}, out.res)
			assert.Equal(t, tc.wantLogged, out.logged, "error log lines")

			// The next pass, which the Binding event or the returned error triggers, writes it.
			_, err := r.Reconcile(context.Background(), req)
			require.NoError(t, err)
			assert.Equal(t, []workercore.KVCacheObjectReference{modelDeploymentClaim()},
				getModelDeploymentBinding(t, cli).Status.UsedBy)
		})
	}
}

// TestModelDeploymentReconciler_ExpectedStatusWriteFailuresAreQuiet pins how a pass ends when the
// deployment changed or was deleted after its status was read. Neither is logged as an error or
// returned. A conflict requeues: the predicate passes only generation changes, so the change behind
// it may deliver no event. Any other failure is still returned and logged, and the next pass writes
// the status in every case.
func TestModelDeploymentReconciler_ExpectedStatusWriteFailuresAreQuiet(t *testing.T) {
	gr := schema.GroupResource{Group: workercore.GroupVersion.Group, Resource: "modeldeployments"}
	testCases := []struct {
		name       string
		err        error
		want       ctrlreconcile.Result
		wantErr    bool
		wantLogged int
	}{
		{
			name: "a conflicting status update requeues quietly",
			err:  kerrors.NewConflict(gr, "qwen", fmt.Errorf("the object has been modified")),
			want: _requeueAfterConflict,
		},
		{
			name: "a status update on a deleted deployment ends quietly",
			err:  kerrors.NewNotFound(gr, "qwen"),
		},
		{
			name:       "any other status update failure is returned and logged",
			err:        kerrors.NewInternalError(fmt.Errorf("etcd unavailable")),
			wantErr:    true,
			wantLogged: 1,
		},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			var injected bool
			fail := failOnce(tc.err)
			cli := ctrlinterceptor.NewClient(
				newModelDeploymentClient(newRenderDeployment(), newRenderInstanceType(), newRenderBinding()).(ctrlcli.WithWatch),
				ctrlinterceptor.Funcs{
					SubResourceUpdate: func(ctx context.Context, c ctrlcli.Client, sub string, obj ctrlcli.Object, opts ...ctrlcli.SubResourceUpdateOption) error {
						if _, ok := obj.(*workercore.ModelDeployment); ok {
							if err := fail(); err != nil {
								injected = true
								return err
							}
						}
						return c.SubResource(sub).Update(ctx, obj, opts...)
					},
				})
			r := &ModelDeploymentReconciler{Client: cli, APIReader: cli, Recorder: ctrlrecord.NewFakeRecorder(64)}
			req := ctrlreconcile.Request{NamespacedName: ctrlcli.ObjectKey{Namespace: "team-a", Name: "qwen"}}

			out := reconcileUntilInjected(t, r, req, &injected)
			if tc.wantErr {
				assert.Error(t, out.err)
			} else {
				assert.NoError(t, out.err)
			}
			assert.Equal(t, tc.want, out.res)
			assert.Equal(t, tc.wantLogged, out.logged, "error log lines")

			// The next pass, which the requeue or the returned error triggers, writes it.
			_, err := r.Reconcile(context.Background(), req)
			require.NoError(t, err)
			assert.NotEmpty(t, getModelDeployment(t, cli).Status.Conditions)
		})
	}
}

// endpointEligibilityTestSeed runs the pass that creates the replicas and their Services, then
// leaves the tree in the state a scenario names: every Pod Ready or not, the ordinary Services'
// selectors carrying the eligibility term or not. Everything a later pass under test sees is
// observed state of a real pass, not a hand-built clone of a render.
func endpointEligibilityTestSeed(t *testing.T, cli ctrlcli.Client, md *workercore.ModelDeployment) []*core.Pod {
	t.Helper()

	_, err := reconcileModelDeployment(t, cli)
	require.NoError(t, err)
	assignEndpointEligibilityUIDs(t, cli)

	return endpointEligibilityPods(t, cli)
}

// assignEndpointEligibilityUIDs gives every seeded Pod a UID.
//
// THE FAKE CLIENT ASSIGNS NONE, and the health predicate keys each member's group by UID, so every
// member of a multi-role deployment would share one key and whichever group was read last would
// answer for all of them -- a single-member role's answer would then eligibles a held replica's
// members. A real cluster gives every Pod a UID, so the fixture gives them one here rather than
// leaving a case to depend on a key the real world never leaves empty.
func assignEndpointEligibilityUIDs(t *testing.T, cli ctrlcli.Client) {
	t.Helper()

	for i, pod := range endpointEligibilityPods(t, cli) {
		if pod.UID != "" {
			continue
		}
		pod.UID = types.UID("uid-" + strconv.Itoa(i) + "-" + pod.Name)
		require.NoError(t, cli.Update(context.Background(), pod))
	}
}

func endpointEligibilityPods(t *testing.T, cli ctrlcli.Client) []*core.Pod {
	t.Helper()

	list := new(core.PodList)
	require.NoError(t, cli.List(context.Background(), list, ctrlcli.InNamespace("team-a")))
	pods := make([]*core.Pod, 0, len(list.Items))
	for i := range list.Items {
		pods = append(pods, &list.Items[i])
	}
	slices.SortFunc(pods, func(a, b *core.Pod) int { return strings.Compare(a.Name, b.Name) })

	return pods
}

func setEndpointEligibilityPodsReady(t *testing.T, cli ctrlcli.Client, ready bool) {
	t.Helper()

	for _, pod := range endpointEligibilityPods(t, cli) {
		if ready {
			pod.Status.Conditions = []core.PodCondition{{Type: core.PodReady, Status: core.ConditionTrue}}
		} else {
			pod.Status.Conditions = nil
		}
		require.NoError(t, cli.Status().Update(context.Background(), pod))
	}
}

func setEndpointEligibilityTermOnServices(t *testing.T, cli ctrlcli.Client, present bool) {
	t.Helper()

	list := new(core.ServiceList)
	require.NoError(t, cli.List(context.Background(), list, ctrlcli.InNamespace("team-a")))
	for i := range list.Items {
		svc := &list.Items[i]
		if svc.Spec.ClusterIP == core.ClusterIPNone {
			continue
		}
		if present {
			svc.Spec.Selector[modelDeploymentLabelKeyEndpointEligible] = modelDeploymentEndpointEligibleValue
		} else {
			delete(svc.Spec.Selector, modelDeploymentLabelKeyEndpointEligible)
		}
		require.NoError(t, cli.Update(context.Background(), svc))
	}
}

func endpointEligibilityOrdinarySelector(t *testing.T, cli ctrlcli.Client) labels.Selector {
	t.Helper()

	list := new(core.ServiceList)
	require.NoError(t, cli.List(context.Background(), list, ctrlcli.InNamespace("team-a")))
	for i := range list.Items {
		if list.Items[i].Spec.ClusterIP == core.ClusterIPNone {
			continue
		}
		sel, err := meta.LabelSelectorAsSelector(&meta.LabelSelector{MatchLabels: list.Items[i].Spec.Selector})
		require.NoError(t, err)

		return sel
	}
	t.Fatal("the deployment has no ordinary Service")

	return nil
}

// TestModelDeploymentReconciler_BackfillsEligibilityBeforeTheSelectorsNarrow is the upgrade
// negative. First enable over an existing deployment meets Pods that are already serving and
// Services whose selectors predate the term; the pass must land the labels on the healthy
// qualifying members BEFORE any Service selector gains the term, or the first enable zeroes every
// ordinary Service's endpoints.
func TestModelDeploymentReconciler_BackfillsEligibilityBeforeTheSelectorsNarrow(t *testing.T) {
	md := newRenderDeployment(func(md *workercore.ModelDeployment) { md.Spec.Roles[0].Replicas = 2 })
	cli := newModelDeploymentClient(md.DeepCopy(), newRenderInstanceType())

	// The world the upgrade arrives at: replicas exist and serve, Services are term-less.
	endpointEligibilityTestSeed(t, cli, md)
	setEndpointEligibilityPodsReady(t, cli, true)
	setEndpointEligibilityTermOnServices(t, cli, false)

	writes := new(modelDeploymentWrites)
	cli = endpointEligibilityCountingClient(t, writes, cli)
	_, err := reconcileModelDeploymentActivated(t, cli)
	require.NoError(t, err)

	for _, pod := range endpointEligibilityPods(t, cli) {
		assert.Equal(t, modelDeploymentEndpointEligibleValue,
			pod.Labels[modelDeploymentLabelKeyEndpointEligible],
			"%s is a healthy member and was backfilled", pod.Name)
	}

	firstPatch := slices.IndexFunc(writes.ordered, func(entry string) bool {
		return strings.Contains(entry, " patch") && strings.HasPrefix(entry, "Pod ")
	})
	firstNarrow := slices.IndexFunc(writes.ordered, func(entry string) bool {
		return strings.Contains(entry, " update") && strings.HasPrefix(entry, "Service ")
	})
	require.NotEqual(t, -1, firstPatch, "the pass patched the Pods: %v", writes.ordered)
	require.NotEqual(t, -1, firstNarrow, "the pass narrowed the Services: %v", writes.ordered)
	assert.Less(t, firstPatch, firstNarrow,
		"the labels landed before any selector narrowed: %v", writes.ordered)
}

// TestModelDeploymentReconciler_EligibilityWritesChangeNoPodIdentity pins the runtime boundary:
// the eligibility key is written and removed by patching metadata, and no write of it moves a
// Pod's identity — generation, UID, or the spec hash the rollout reads.
func TestModelDeploymentReconciler_EligibilityWritesChangeNoPodIdentity(t *testing.T) {
	md := newRenderDeployment(func(md *workercore.ModelDeployment) { md.Spec.Roles[0].Replicas = 2 })
	cli := newModelDeploymentClient(md.DeepCopy(), newRenderInstanceType())
	endpointEligibilityTestSeed(t, cli, md)
	setEndpointEligibilityPodsReady(t, cli, true)

	_, err := reconcileModelDeployment(t, cli)
	require.NoError(t, err)

	snapshot := map[string]struct {
		uid          types.UID
		gen          int64
		resourceHash string
	}{}
	for _, pod := range endpointEligibilityPods(t, cli) {
		require.Equal(t, modelDeploymentEndpointEligibleValue,
			pod.Labels[modelDeploymentLabelKeyEndpointEligible], "%s", pod.Name)
		snapshot[pod.Name] = struct {
			uid          types.UID
			gen          int64
			resourceHash string
		}{pod.UID, pod.Generation, pod.Annotations[modelDeploymentPodSpecHashAnnotation]}
	}

	// The key leaves again — the member stopped qualifying — and identity still does not move.
	setEndpointEligibilityPodsReady(t, cli, false)
	_, err = reconcileModelDeployment(t, cli)
	require.NoError(t, err)

	for _, pod := range endpointEligibilityPods(t, cli) {
		assert.NotContains(t, pod.Labels, modelDeploymentLabelKeyEndpointEligible,
			"%s stopped qualifying and the key is gone", pod.Name)
		before := snapshot[pod.Name]
		assert.Equal(t, before.uid, pod.UID, "%s kept its UID", pod.Name)
		assert.Equal(t, before.gen, pod.Generation, "%s kept its generation", pod.Name)
		assert.Equal(t, before.resourceHash, pod.Annotations[modelDeploymentPodSpecHashAnnotation],
			"%s kept the stored hash", pod.Name)
	}
}

// TestModelDeploymentReconciler_TheKeyRestoresThroughDerivationWithoutFlapping pins the
// controller half of AC-1.2 with the level-based behavior of AC-1.4: while the key is off a
// member the ordinary Service selector matches nothing of it, one pass re-derives the key with a
// single patch, and a converged tree is written by no pass at all.
func TestModelDeploymentReconciler_TheKeyRestoresThroughDerivationWithoutFlapping(t *testing.T) {
	md := newRenderDeployment(func(md *workercore.ModelDeployment) { md.Spec.Roles[0].Replicas = 1 })
	cli := newModelDeploymentClient(md.DeepCopy(), newRenderInstanceType())
	endpointEligibilityTestSeed(t, cli, md)
	setEndpointEligibilityPodsReady(t, cli, true)
	_, err := reconcileModelDeploymentActivated(t, cli)
	require.NoError(t, err)

	selector := endpointEligibilityOrdinarySelector(t, cli)
	pods := endpointEligibilityPods(t, cli)
	require.Len(t, pods, 1)
	require.True(t, selector.Matches(labels.Set(pods[0].Labels)),
		"a labeled, healthy member is selected")

	// While the key is off — here by drift, in production by a disqualification — the selector
	// matches nothing of this member, before any reconcile runs.
	drifted := pods[0].DeepCopy()
	delete(drifted.Labels, modelDeploymentLabelKeyEndpointEligible)
	require.NoError(t, cli.Update(context.Background(), drifted))
	refetched := endpointEligibilityPods(t, cli)[0]
	assert.False(t, selector.Matches(labels.Set(refetched.Labels)),
		"with the key removed the ordinary Service selects nothing of the member")

	// One pass re-derives the key, with exactly one patch for exactly one drifted Pod.
	writes := new(modelDeploymentWrites)
	cli = endpointEligibilityCountingClient(t, writes, cli)
	_, err = reconcileModelDeployment(t, cli)
	require.NoError(t, err)
	assert.Equal(t, 1, writes.patches, "the re-derivation is one patch: %v", writes.ordered)
	restored := endpointEligibilityPods(t, cli)[0]
	assert.Equal(t, modelDeploymentEndpointEligibleValue,
		restored.Labels[modelDeploymentLabelKeyEndpointEligible])
	assert.True(t, selector.Matches(labels.Set(restored.Labels)),
		"with the key restored the ordinary Service selects the member again")

	// A converged tree is written by no pass at all — the level-based re-derivation flaps never.
	writes = new(modelDeploymentWrites)
	cli = endpointEligibilityCountingClient(t, writes, cli)
	_, err = reconcileModelDeployment(t, cli)
	require.NoError(t, err)
	assert.Zero(t, writes.patches, "a converged pass patches nothing: %v", writes.ordered)
}

// TestModelDeploymentReconciler_AReplacementMemberIsEligibleOnlyThroughItsOwnVerification pins
// AC-1.5: a member that takes a departed member's name starts ineligible whatever its predecessor
// was, and becomes eligible only through its own readiness.
func TestModelDeploymentReconciler_AReplacementMemberIsEligibleOnlyThroughItsOwnVerification(t *testing.T) {
	md := newRenderDeployment(func(md *workercore.ModelDeployment) { md.Spec.Roles[0].Replicas = 1 })
	cli := newModelDeploymentClient(md.DeepCopy(), newRenderInstanceType())
	endpointEligibilityTestSeed(t, cli, md)
	setEndpointEligibilityPodsReady(t, cli, true)
	_, err := reconcileModelDeployment(t, cli)
	require.NoError(t, err)

	departed := endpointEligibilityPods(t, cli)[0]
	require.Equal(t, modelDeploymentEndpointEligibleValue,
		departed.Labels[modelDeploymentLabelKeyEndpointEligible])

	// The replacement takes the NAME and the whole rendered identity — same replica, same ordinal,
	// same spec hash — and nothing that was written at runtime: a fresh UID, no readiness, and no
	// eligibility key, because that key was this predecessor.s own verification, not the slot.s.
	replacement := departed.DeepCopy()
	replacement.UID = departed.UID + "-next"
	replacement.ResourceVersion = ""
	replacement.Status = core.PodStatus{}
	delete(replacement.Labels, modelDeploymentLabelKeyEndpointEligible)
	require.NoError(t, cli.Delete(context.Background(), departed))
	require.NoError(t, cli.Create(context.Background(), replacement))

	_, err = reconcileModelDeployment(t, cli)
	require.NoError(t, err)

	after := endpointEligibilityPods(t, cli)
	require.Len(t, after, 1)
	assert.Equal(t, replacement.UID, after[0].UID, "the surviving Pod is the replacement")
	assert.NotContains(t, after[0].Labels, modelDeploymentLabelKeyEndpointEligible,
		"the predecessor's eligibility never carried over: not ready, not eligible")

	setEndpointEligibilityPodsReady(t, cli, true)
	_, err = reconcileModelDeployment(t, cli)
	require.NoError(t, err)

	after = endpointEligibilityPods(t, cli)
	assert.Equal(t, modelDeploymentEndpointEligibleValue,
		after[0].Labels[modelDeploymentLabelKeyEndpointEligible],
		"the replacement is eligible through its own readiness, nothing else")
}

// TestModelDeploymentReconciler_ReadinessAloneNeverEligiblesANonLeader pins the member rule for
// a leader-served role: above size one the members that serve no API stay ineligible however
// ready they are, so PodReady alone never produces a selectable-but-unqualified endpoint.
//
// A MULTI-MEMBER REPLICA IS NOW HELD ENTIRELY, so this asserts the stronger statement: readiness
// alone eligibles NOTHING here, the leader included. The leader stops carrying the label because
// the whole-group predicate cannot verify a replica of two members, which is what the
// qualification work is for; the follower never carried it for the shape reason it always had.
// The shape terms themselves are unchanged and are pinned directly in the health table, where a
// single-member replica makes them observable.
func TestModelDeploymentReconciler_ReadinessAloneNeverEligiblesANonLeader(t *testing.T) {
	md := newRenderDeployment(func(md *workercore.ModelDeployment) {
		md.Spec.Roles[0].Replicas = 1
		md.Spec.Roles[0].ReplicaSize = 2
	})
	cli := newModelDeploymentClient(md.DeepCopy(), newRenderInstanceType())
	endpointEligibilityTestSeed(t, cli, md)
	setEndpointEligibilityPodsReady(t, cli, true)
	_, err := reconcileModelDeployment(t, cli)
	require.NoError(t, err)

	members := endpointEligibilityPods(t, cli)
	require.Len(t, members, 2)
	byIndex := map[string]*core.Pod{}
	for _, pod := range members {
		byIndex[pod.Labels[modelDeploymentMemberIndexLabel]] = pod
	}
	leader, follower := byIndex["0"], byIndex["1"]
	require.NotNil(t, leader)
	require.NotNil(t, follower)

	assert.NotContains(t, leader.Labels, modelDeploymentLabelKeyEndpointEligible,
		"readiness alone eligibles nothing: a replica of two members is held unverifiable")
	assert.NotContains(t, follower.Labels, modelDeploymentLabelKeyEndpointEligible,
		"a ready member that serves no API is not: readiness alone eligibles nothing")
}

// TestModelDeploymentReconciler_AMultiMemberReplicaIsHeldUnverified is the activation negative
// read through the reconciler. Every member of a multi-member replica is ready and the shape terms
// would select some of them, yet none carries the label, because the group-forward predicate has
// no observation to rest on and restore is gated on it.
//
// The two shapes are held IDENTICALLY, which is the point: the group gate stands in front of the
// shape gate, so a shape difference cannot produce an eligibility the group predicate never
// verified.
func TestModelDeploymentReconciler_AMultiMemberReplicaIsHeldUnverified(t *testing.T) {
	testCases := []struct {
		name      string
		extraArgs []string
	}{
		{name: "leader served", extraArgs: nil},
		{
			name:      "external data parallel",
			extraArgs: []string{"--data-parallel-size", "2", "--data-parallel-external-lb"},
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			md := newRenderDeployment(func(md *workercore.ModelDeployment) {
				md.Spec.Roles[0].Replicas = 1
				md.Spec.Roles[0].ReplicaSize = 2
				md.Spec.Roles[0].ExtraArgs = tc.extraArgs
			})
			cli := newModelDeploymentClient(md.DeepCopy(), newRenderInstanceType())
			endpointEligibilityTestSeed(t, cli, md)
			setEndpointEligibilityPodsReady(t, cli, true)
			_, err := reconcileModelDeployment(t, cli)
			require.NoError(t, err)

			for _, pod := range endpointEligibilityPods(t, cli) {
				assert.NotContains(t, pod.Labels, modelDeploymentLabelKeyEndpointEligible,
					"a multi-member replica is held regardless of the shape, because the group "+
						"forward predicate has no observation to verify it against")
			}
		})
	}
}

// TestModelDeploymentReconciler_ANotReadyMemberRevokesTheWholeReplica is the withdrawal half read
// through the reconciler, and the one that must work even where restore is gated. A single-member
// replica is activated -- there is no cross-member path to verify -- so it is restored here, and
// then the one member going NotReady takes the key away again.
func TestModelDeploymentReconciler_ANotReadyMemberRevokesTheWholeReplica(t *testing.T) {
	md := newRenderDeployment(func(md *workercore.ModelDeployment) {
		md.Spec.Roles[0].Replicas = 1
	})
	cli := newModelDeploymentClient(md.DeepCopy(), newRenderInstanceType())
	endpointEligibilityTestSeed(t, cli, md)
	setEndpointEligibilityPodsReady(t, cli, true)
	_, err := reconcileModelDeployment(t, cli)
	require.NoError(t, err)

	members := endpointEligibilityPods(t, cli)
	require.Len(t, members, 1)
	require.Equal(t, modelDeploymentEndpointEligibleValue,
		members[0].Labels[modelDeploymentLabelKeyEndpointEligible],
		"a single-member replica has no group predicate to hold it, so it qualifies")

	// The member is no longer ready, and the key must go whatever else is true of the group.
	setEndpointEligibilityPodsReady(t, cli, false)
	_, err = reconcileModelDeployment(t, cli)
	require.NoError(t, err)

	for _, pod := range endpointEligibilityPods(t, cli) {
		assert.NotContains(t, pod.Labels, modelDeploymentLabelKeyEndpointEligible,
			"a definite health fault withdraws the key, and withdrawal is never gated")
	}
}

// setEndpointEligibilityPodsNotReadyAt takes the member at one index of every replica NotReady and
// leaves the rest alone, so a case can fail exactly one member of a group.
//
// The condition is set FALSE rather than dropped, and the difference is the whole point: a Pod
// carrying PodReady=False is a definite fault the predicate revokes on, while a Pod with no
// readiness condition at all is a member nothing has reported on yet, which is an open question
// and holds rather than revokes.
func setEndpointEligibilityPodsNotReadyAt(t *testing.T, cli ctrlcli.Client, member string) {
	t.Helper()

	for _, pod := range endpointEligibilityPods(t, cli) {
		if pod.Labels[modelDeploymentMemberIndexLabel] != member {
			continue
		}
		pod.Status.Conditions = []core.PodCondition{{Type: core.PodReady, Status: core.ConditionFalse}}
		require.NoError(t, cli.Status().Update(context.Background(), pod))
	}
}

// setEndpointEligibilityLabelOnPods writes or removes the eligibility key on every Pod directly,
// standing in for a key a pass before the group gate existed, or for one that drifted. The two
// activation negatives that carry the most weight are about a key that is ALREADY on a Pod: a gate
// that refuses to touch one, and a fault that has to take one away, are both invisible to a
// fixture that starts with no key at all.
func setEndpointEligibilityLabelOnPods(t *testing.T, cli ctrlcli.Client, present bool) {
	t.Helper()

	for _, pod := range endpointEligibilityPods(t, cli) {
		if present {
			pod.Labels[modelDeploymentLabelKeyEndpointEligible] = modelDeploymentEndpointEligibleValue
		} else {
			delete(pod.Labels, modelDeploymentLabelKeyEndpointEligible)
		}
		require.NoError(t, cli.Update(context.Background(), pod))
	}
}

// endpointEligibilitySelectorForRole returns the ordinary Service selector that carries a named
// role, so a case can ask what a SPECIFIC role's healthy endpoints look like rather than whichever
// Service happened to be listed first.
func endpointEligibilitySelectorForRole(t *testing.T, cli ctrlcli.Client, role string) labels.Selector {
	t.Helper()

	list := new(core.ServiceList)
	require.NoError(t, cli.List(context.Background(), list, ctrlcli.InNamespace("team-a")))
	for i := range list.Items {
		svc := &list.Items[i]
		if svc.Spec.ClusterIP == core.ClusterIPNone {
			continue
		}
		if svc.Spec.Selector[modelDeploymentLabelKeyComponent] != role {
			continue
		}
		sel, err := meta.LabelSelectorAsSelector(&meta.LabelSelector{MatchLabels: svc.Spec.Selector})
		require.NoError(t, err)

		return sel
	}
	t.Fatalf("the deployment has no ordinary Service for role %q", role)

	return nil
}

// TestModelDeploymentReconciler_AHeldReplicaLeavesAnExistingLabelUntouched is the byte-unchanged
// clause of AC-3.5. A member of a replica the group predicate cannot verify is not granted
// eligibility, but a key that is already on it is also not taken away: the reconciler is not
// allowed to grant on a weaker signal than the spec requires, and it is equally not allowed to
// revoke on one, because revoking here would be a verdict the predicate never made.
func TestModelDeploymentReconciler_AHeldReplicaLeavesAnExistingLabelUntouched(t *testing.T) {
	md := newRenderDeployment(func(md *workercore.ModelDeployment) {
		md.Spec.Roles[0].Replicas = 1
		md.Spec.Roles[0].ReplicaSize = 2
	})
	cli := newModelDeploymentClient(md.DeepCopy(), newRenderInstanceType())
	endpointEligibilityTestSeed(t, cli, md)
	setEndpointEligibilityPodsReady(t, cli, true)
	setEndpointEligibilityLabelOnPods(t, cli, true)

	before := map[string]map[string]string{}
	for _, pod := range endpointEligibilityPods(t, cli) {
		before[pod.Name] = maps.Clone(pod.Labels)
	}

	writes := new(modelDeploymentWrites)
	cli = endpointEligibilityCountingClient(t, writes, cli)
	_, err := reconcileModelDeploymentActivated(t, cli)
	require.NoError(t, err)

	assert.Zero(t, writes.patches,
		"a held replica writes no Pod at all, so the pre-existing label is preserved byte for byte: %v",
		writes.ordered)
	for _, pod := range endpointEligibilityPods(t, cli) {
		assert.Equal(t, before[pod.Name], pod.Labels,
			"%s kept every label it had, because the predicate neither granted nor revoked", pod.Name)
	}
}

// TestModelDeploymentReconciler_ADefaultHealthFaultWithdrawsThroughTheGate is the half of the gate
// that must not exist. Restore is held for a replica whose group predicate cannot be verified, but
// a member of it going NotReady is a definite fault, and a definite fault takes the key away even
// while the group predicate is unsupported. The label is put on by hand first, because otherwise
// there would be nothing to withdraw and the case would pass for the wrong reason.
func TestModelDeploymentReconciler_ADefaultHealthFaultWithdrawsThroughTheGate(t *testing.T) {
	md := newRenderDeployment(func(md *workercore.ModelDeployment) {
		md.Spec.Roles[0].Replicas = 1
		md.Spec.Roles[0].ReplicaSize = 2
	})
	cli := newModelDeploymentClient(md.DeepCopy(), newRenderInstanceType())
	endpointEligibilityTestSeed(t, cli, md)
	setEndpointEligibilityPodsReady(t, cli, true)
	setEndpointEligibilityLabelOnPods(t, cli, true)

	// One member of the two goes NotReady. The group is still unsupported, and the healthy member
	// is still healthy, but the replica as a whole has a definite fault and loses the key.
	setEndpointEligibilityPodsNotReadyAt(t, cli, "1")

	_, err := reconcileModelDeployment(t, cli)
	require.NoError(t, err)

	members := endpointEligibilityPods(t, cli)
	require.Len(t, members, 2)
	for _, pod := range members {
		assert.NotContains(t, pod.Labels, modelDeploymentLabelKeyEndpointEligible,
			"%s lost the key: a definite health fault withdraws eligibility even where restore is "+
				"gated on an unverifiable group predicate", pod.Name)
	}
}

// TestModelDeploymentReconciler_AReservedReplicaIsNotRestored is the "not yet eligible" half of the
// restore rule, on a path where NOTHING ELSE could be doing the holding.
//
// A multi-member replica is held twice over, once by the activation gate and once by the
// unverified group-forward leg, so no single defect puts its label back and the case cannot say
// which of the two did the work. This replica has a single member, so the group-forward leg is
// NotApplicable and the gate stands open: the one thing keeping the label off a healthy, ready
// member is the retirement leg, and dropping that leg from the conjunction would put an endpoint
// that is on its way out back into selection.
func TestModelDeploymentReconciler_AReservedReplicaIsNotRestored(t *testing.T) {
	md := newRenderDeployment(func(md *workercore.ModelDeployment) {
		md.Spec.Roles[0].Replicas = 1
	})
	cli := newModelDeploymentClient(md.DeepCopy(), newRenderInstanceType())
	endpointEligibilityTestSeed(t, cli, md)
	setEndpointEligibilityPodsReady(t, cli, true)

	// An external actor reserves the replica for retirement. Its member is healthy and its group
	// has nothing left to verify, and it is still not eligible, because it is going away.
	live := getModelDeployment(t, cli)
	live.Status.Retirement = &workercore.ModelDeploymentRetirementStatus{
		RoleName: "server", ReplicaOrdinal: 0,
		State: workercore.ModelDeploymentRetirementStateAdmitted,
	}
	require.NoError(t, cli.Status().Update(context.Background(), live))

	_, err := reconcileModelDeployment(t, cli)
	require.NoError(t, err)

	members := endpointEligibilityPods(t, cli)
	require.Len(t, members, 1)
	assert.NotContains(t, members[0].Labels, modelDeploymentLabelKeyEndpointEligible,
		"a reserved replica is not restored, even with a ready member and nothing to verify")
}

// TestModelDeploymentReconciler_TheGateIsPerReplicaAndDoesNotStopBackfill is the activation negative
// that matters most, because its failure mode looks like a healthy deployment. The gate is decided
// per replica, not per deployment: a deployment holding a multi-member role the predicate cannot
// verify must still backfill the label onto the healthy members of its single-member role. Were
// the gate ever lifted to the deployment, first enable would zero every ordinary Service of every
// healthy deployment, which is exactly what the backfill exists to prevent.
func TestModelDeploymentReconciler_TheGateIsPerReplicaAndDoesNotStopBackfill(t *testing.T) {
	md := newRenderDeployment(func(md *workercore.ModelDeployment) {
		md.Spec.Roles = []workercore.ModelDeploymentRole{
			{
				Name: "paired", Replicas: 1, ReplicaSize: 2,
				InstanceType: "h20-8x", Image: "vllm/vllm-openai:v0.25.1",
			},
			{
				Name: "solo", Replicas: 1,
				InstanceType: "h20-8x", Image: "vllm/vllm-openai:v0.25.1",
			},
		}
	})
	cli := newModelDeploymentClient(md.DeepCopy(), newRenderInstanceType())
	endpointEligibilityTestSeed(t, cli, md)
	setEndpointEligibilityPodsReady(t, cli, true)
	setEndpointEligibilityTermOnServices(t, cli, false)

	_, err := reconcileModelDeployment(t, cli)
	require.NoError(t, err)

	byRole := map[string][]*core.Pod{}
	for _, pod := range endpointEligibilityPods(t, cli) {
		byRole[pod.Labels[modelDeploymentLabelKeyComponent]] = append(
			byRole[pod.Labels[modelDeploymentLabelKeyComponent]], pod)
	}
	require.Len(t, byRole["paired"], 2)
	require.Len(t, byRole["solo"], 1)

	for _, pod := range byRole["paired"] {
		assert.NotContains(t, pod.Labels, modelDeploymentLabelKeyEndpointEligible,
			"the unverifiable replica is held")
	}
	require.Equal(t, modelDeploymentEndpointEligibleValue,
		byRole["solo"][0].Labels[modelDeploymentLabelKeyEndpointEligible],
		"the activated replica beside it is still backfilled, so the hold does not zero healthy endpoints")

	// And the Service that serves the healthy role still selects it after its selector narrows.
	selector := endpointEligibilitySelectorForRole(t, cli, "solo")
	assert.True(t, selector.Matches(labels.Set(byRole["solo"][0].Labels)),
		"the ordinary Service of the activated role still selects its healthy member")
}

// endpointEligibilityCountingClient rebuilds a counting client over the tree an earlier pass left
// behind: the counting fixture takes objects, so the live ones are listed back and replanted,
// UIDs and all.
func endpointEligibilityCountingClient(
	t *testing.T, w *modelDeploymentWrites, previous ctrlcli.Client,
) ctrlcli.Client {
	t.Helper()

	instList := new(worker.InstanceTypeList)
	require.NoError(t, previous.List(context.Background(), instList))
	pods := endpointEligibilityPods(t, previous)
	svcList := new(core.ServiceList)
	require.NoError(t, previous.List(context.Background(), svcList, ctrlcli.InNamespace("team-a")))

	objs := make([]ctrlcli.Object, 0, 1+len(instList.Items)+len(pods)+len(svcList.Items))
	objs = append(objs, getModelDeployment(t, previous))
	for i := range instList.Items {
		objs = append(objs, &instList.Items[i])
	}
	for _, pod := range pods {
		objs = append(objs, pod)
	}
	for i := range svcList.Items {
		objs = append(objs, &svcList.Items[i])
	}

	return newCountingModelDeploymentClient(w, objs...)
}
