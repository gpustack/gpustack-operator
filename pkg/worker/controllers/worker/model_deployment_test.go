package worker

import (
	"context"
	"slices"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	core "k8s.io/api/core/v1"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrlrecord "k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	ctrlcli "sigs.k8s.io/controller-runtime/pkg/client"
	ctrlfake "sigs.k8s.io/controller-runtime/pkg/client/fake"
	ctrlinterceptor "sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	ctrlreconcile "sigs.k8s.io/controller-runtime/pkg/reconcile"

	worker "gpustack.ai/gpustack/api/worker/v1"
	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	"gpustack.ai/gpustack/pkg/kubeclients/kubernetes/scheme"
	"gpustack.ai/gpustack/pkg/systemmeta"
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

func getModelDeployment(t *testing.T, cli ctrlcli.Client) *workercore.ModelDeployment {
	t.Helper()

	md := new(workercore.ModelDeployment)
	require.NoError(t, cli.Get(context.Background(),
		ctrlcli.ObjectKey{Namespace: "team-a", Name: "qwen"}, md))

	return md
}

// TestModelDeploymentReconciler_RendersOnePodPerReplica is the shape of the whole feature: N
// replicas of one role, each an ordinary Pod the existing admission chain already knows how to
// handle, and no Instance anywhere.
func TestModelDeploymentReconciler_RendersOnePodPerReplica(t *testing.T) {
	md := newRenderDeployment(func(md *workercore.ModelDeployment) {
		md.Spec.Roles[0].Replicas = 4
	})
	cli := newModelDeploymentClient(md, newRenderInstanceType())

	_, err := reconcileModelDeployment(t, cli)
	require.NoError(t, err)

	assert.Equal(t,
		[]string{"qwen-server-0", "qwen-server-1", "qwen-server-2", "qwen-server-3"},
		replicaNames(t, cli))

	instList := new(workercore.InstanceList)
	require.NoError(t, cli.List(context.Background(), instList))
	assert.Empty(t, instList.Items, "a ModelDeployment renders Pods directly and creates no Instance")
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
	require.Equal(t, 3, writes.creates, "the first pass creates two replicas and one Service")
	require.Equal(t, 1, writes.updates, "and adds the finalizer")
	require.Equal(t, 1, writes.statusUpdates, "and reports the status once")

	*writes = modelDeploymentWrites{}
	_, err = reconcileModelDeployment(t, cli)
	require.NoError(t, err)

	assert.Equal(t, modelDeploymentWrites{}, *writes,
		"an unchanged spec must issue no create, no update, no delete and no status write at all")
}

// TestModelDeploymentReconciler_RecreatesADeletedReplica states the difference between converging
// and executing a workflow: the desired state is re-derived from the spec on every pass, so a
// replica removed by anything at all comes back under the name it had.
func TestModelDeploymentReconciler_RecreatesADeletedReplica(t *testing.T) {
	cli := newModelDeploymentClient(newRenderDeployment(), newRenderInstanceType())

	_, err := reconcileModelDeployment(t, cli)
	require.NoError(t, err)

	gone := &core.Pod{ObjectMeta: meta.ObjectMeta{Namespace: "team-a", Name: "qwen-server-1"}}
	require.NoError(t, cli.Delete(context.Background(), gone))
	require.Equal(t, []string{"qwen-server-0"}, replicaNames(t, cli))

	_, err = reconcileModelDeployment(t, cli)
	require.NoError(t, err)
	assert.Equal(t, []string{"qwen-server-0", "qwen-server-1"}, replicaNames(t, cli))
}

// TestModelDeploymentReconciler_ScaleDownRemovesTheHighestOrdinals pins why the ordinal is in the
// name. Which replicas to remove is decidable from the spec alone, so a scale down does not have to
// choose between Pods that look alike.
func TestModelDeploymentReconciler_ScaleDownRemovesTheHighestOrdinals(t *testing.T) {
	md := newRenderDeployment(func(md *workercore.ModelDeployment) { md.Spec.Roles[0].Replicas = 4 })
	cli := newModelDeploymentClient(md, newRenderInstanceType())

	_, err := reconcileModelDeployment(t, cli)
	require.NoError(t, err)
	require.Len(t, replicaNames(t, cli), 4)

	scaled := getModelDeployment(t, cli)
	scaled.Spec.Roles[0].Replicas = 2
	require.NoError(t, cli.Update(context.Background(), scaled))

	_, err = reconcileModelDeployment(t, cli)
	require.NoError(t, err)
	assert.Equal(t, []string{"qwen-server-0", "qwen-server-1"}, replicaNames(t, cli))
}

// TestModelDeploymentReconciler_RecreatesOnASpecChange covers the rollout policy: recreate, no
// surge. The replica is deleted on the pass that notices the difference and rebuilt by the pass that
// notices its absence, so the fingerprint on the new one is the current spec's.
func TestModelDeploymentReconciler_RecreatesOnASpecChange(t *testing.T) {
	cli := newModelDeploymentClient(newRenderDeployment(), newRenderInstanceType())

	_, err := reconcileModelDeployment(t, cli)
	require.NoError(t, err)
	before := replicaHashes(t, cli)
	require.Len(t, before, 2)

	changed := getModelDeployment(t, cli)
	changed.Spec.Roles[0].Template.Image = "vllm/vllm-openai:v0.26.0"
	require.NoError(t, cli.Update(context.Background(), changed))

	_, err = reconcileModelDeployment(t, cli)
	require.NoError(t, err)
	assert.Empty(t, replicaNames(t, cli), "an outdated replica is deleted rather than patched in place")

	_, err = reconcileModelDeployment(t, cli)
	require.NoError(t, err)
	assert.Len(t, replicaNames(t, cli), 2)

	after := replicaHashes(t, cli)
	for name := range before {
		assert.NotEqual(t, before[name], after[name],
			"%s must be built from the new spec, not carry the old fingerprint", name)
	}

	podList := new(core.PodList)
	require.NoError(t, cli.List(context.Background(), podList, ctrlcli.InNamespace("team-a")))
	for i := range podList.Items {
		assert.Equal(t, "vllm/vllm-openai:v0.26.0", podList.Items[i].Spec.Containers[0].Image)
	}
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

	// The name it occupies is one this deployment wants, so the create collides and the pass asks to
	// come back rather than adopting it or reporting success.
	res, err := reconcileModelDeployment(t, cli)
	require.NoError(t, err)
	assert.Positive(t, res.RequeueAfter)

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
		md.Spec.Roles[0].Template.Image = ""
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
		md.Spec.Roles[0].Template.Image = ""
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
