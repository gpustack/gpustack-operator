package worker

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

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

func TestModelDeploymentOwnedResourceRequiresAResourceNote(t *testing.T) {
	role := new(core.Pod)
	systemmeta.NoteResource(role, ModelDeploymentResourceType,
		map[string]string{ModelDeploymentResourceNoteRole: "prefill"})
	router := new(core.Pod)
	systemmeta.NoteResource(router, ModelDeploymentResourceType,
		map[string]string{modelDeploymentResourceNoteRouter: "llm-d"})
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

// surplusReplica renders one replica as the pass itself would, gives it an explicit identity, and
// hands it to the fixture: the surplus cases need a member whose fingerprint matches the template
// and whose creationTimestamp is under the test's control, which no reconciler-issued create gives.
// The rendered prefix it still carries is inert once a name is assigned; nothing in the reconciler
// reads it.
func surplusReplica(t *testing.T, md *workercore.ModelDeployment, name string, created meta.Time) *core.Pod {
	t.Helper()

	pod := renderOne(t, md, newRenderInstanceType())
	pod.Name = name
	pod.CreationTimestamp = created

	return pod
}

// TestModelDeploymentReconciler_ScaleDownRemovesTheNewestReplica pins the choice the surplus rule
// makes. Three live replicas against a role declaring two -- every member agreeing on the current
// total, which is the state a replacement created while the departed Pod was still active leaves
// behind -- and the pass sheds exactly one: the NEWEST, so the longest-serving replicas survive a
// scale-down. The choice is asserted, not observed: the names say which replica went.
//
// IT TAKES A HAND-BUILT SET, because a spec edit cannot produce this state: a replicas change
// removes ordinals the new count no longer names, and this state is duplicates ON one ordinal --
// members no ordinal rule can tell apart, which only a hand places.
func TestModelDeploymentReconciler_ScaleDownRemovesTheNewestReplica(t *testing.T) {
	t.Run("by creation timestamp", func(t *testing.T) {
		md := newRenderDeployment() // declares two
		moment := time.Date(2026, 9, 18, 10, 0, 0, 0, time.UTC)

		oldest := surplusReplica(t, md, "qwen-server-old", meta.NewTime(moment))
		middle := surplusReplica(t, md, "qwen-server-mid", meta.NewTime(moment.Add(1*time.Minute)))
		newest := surplusReplica(t, md, "qwen-server-new", meta.NewTime(moment.Add(2*time.Minute)))

		cli := newModelDeploymentClient(md, newRenderInstanceType(), oldest, middle, newest)

		res, err := reconcileModelDeployment(t, cli)
		require.NoError(t, err)

		assert.Equal(t, []string{"qwen-server-mid", "qwen-server-old"}, replicaNames(t, cli),
			"the newest replica is the one shed, and the choice is the assertion")
		assert.Positive(t, res.RequeueAfter, "a pass that removed a replica comes back for the ask")
	})

	t.Run("by name when the timestamps tie", func(t *testing.T) {
		md := newRenderDeployment() // declares two
		moment := meta.NewTime(time.Date(2026, 9, 18, 10, 0, 0, 0, time.UTC))

		// One create pass, one second of granularity: every replica of a group can carry the same
		// timestamp, so the rule has to stay total without it.
		aaa := surplusReplica(t, md, "qwen-server-aaa", moment)
		mmm := surplusReplica(t, md, "qwen-server-mmm", moment)
		zzz := surplusReplica(t, md, "qwen-server-zzz", moment)

		cli := newModelDeploymentClient(md, newRenderInstanceType(), aaa, mmm, zzz)

		_, err := reconcileModelDeployment(t, cli)
		require.NoError(t, err)

		assert.Equal(t, []string{"qwen-server-aaa", "qwen-server-mmm"}, replicaNames(t, cli),
			"the greater name reads as the newer replica when nothing else separates them")
	})
}

// TestModelDeploymentReconciler_RecreatesOnASpecChange covers the rollout policy: recreate, no
// surge, ONE REPLICA AT A TIME. A pass deletes at most one outdated replica and creates nothing
// beside the deletion; the next pass creates the replacement, and the two-step repeats until every
// replica was built from the current spec. There is no Workload in this fixture, so the create
// gate's no-Workload branch applies and each replace pass creates freely.
func TestModelDeploymentReconciler_RecreatesOnASpecChange(t *testing.T) {
	cli := newModelDeploymentClient(newRenderDeployment(), newRenderInstanceType())

	_, err := reconcileModelDeployment(t, cli)
	require.NoError(t, err)
	before := replicaNames(t, cli)
	require.Len(t, before, 2)

	changed := getModelDeployment(t, cli)
	changed.Spec.Roles[0].Image = "vllm/vllm-openai:v0.26.0"
	require.NoError(t, cli.Update(context.Background(), changed))

	// One outdated replica goes; nothing is created beside the deletion.
	_, err = reconcileModelDeployment(t, cli)
	require.NoError(t, err)
	mid := replicaNames(t, cli)
	require.Len(t, mid, 1, "the rollout deletes at most one replica per pass")
	assert.Contains(t, before, mid[0], "and it is one of the replicas that existed before the edit")

	// The missing count comes back, built from the new spec.
	_, err = reconcileModelDeployment(t, cli)
	require.NoError(t, err)
	require.Len(t, replicaNames(t, cli), 2)

	// The last outdated replica goes; the pass before's replacement survives it.
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
// deleting from an already-short group widens exactly the gap the pass is trying to close, so the
// missing replica is created first and the outdated survivor is rolled by the pass after.
func TestModelDeploymentReconciler_CountHealsBeforeTheHashRolls(t *testing.T) {
	ctx := context.Background()
	cli := newModelDeploymentClient(newRenderDeployment(), newRenderInstanceType())

	_, err := reconcileModelDeployment(t, cli)
	require.NoError(t, err)
	names := replicaNames(t, cli)
	require.Len(t, names, 2)

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

	// The pass after rolls the survivor now that the count is whole.
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

// TestModelDeploymentReconciler_ReplacementWaitsForTheWorkloadsAsk walks the create gate's three
// answers end to end. The ask is Kueue's own verdict on whether the departed Pod has stopped
// counting, and each phase below is one answer: not asking yet -- where creating would put one more
// active Pod in the group than its PodSet declares, and Kueue's response to that excess is to evict
// the replacement itself; asking -- where the pass creates exactly the absent count; and no
// Workload at all -- where waiting would deadlock a deployment whose group composes none.
//
// THE DEPARTED POD IS HELD BY KUEUE'S FINALIZER, which is the real shape of this window: measured
// on a live cluster, a deleted Pod of a serving group stays on the books -- its ordinal's group
// still has a member -- for as long as the finalizer holds it. A Pod deleted bare is simply GONE,
// its group has nothing to ask and the gate creates freely, so the wait this case walks needs the
// held member to be observable at all.
func TestModelDeploymentReconciler_ReplacementWaitsForTheWorkloadsAsk(t *testing.T) {
	ctx := context.Background()
	cli := newModelDeploymentClient(newRenderDeployment(), newRenderInstanceType())

	_, err := reconcileModelDeployment(t, cli)
	require.NoError(t, err)
	names := replicaNames(t, cli)
	require.Len(t, names, 2)

	gone := new(core.Pod)
	require.NoError(t, cli.Get(ctx, ctrlcli.ObjectKey{Namespace: "team-a", Name: names[0]}, gone))
	gone.Finalizers = []string{kueuepodconst.PodFinalizer}
	require.NoError(t, cli.Update(ctx, gone))
	require.NoError(t, cli.Delete(ctx, gone))
	require.NoError(t, cli.Create(ctx, askingGroupWorkload(replicaPods(t, cli), false)))

	// Not asking yet: the departed member still counts in its ordinal's group, so the pass creates
	// nothing beside it and comes back for the ask.
	res, err := reconcileModelDeployment(t, cli)
	require.NoError(t, err)
	assert.ElementsMatch(t, replicaNames(t, cli), names,
		"with the departed member still counting, creating a replacement is the harmful move")
	assert.Positive(t, res.RequeueAfter, "and the pass polls rather than sleeping forever")

	// Asking: the pass creates the replacement beside the held member, which is what releases it.
	setGroupAsk(t, cli, true)
	_, err = reconcileModelDeployment(t, cli)
	require.NoError(t, err)
	after := replicaNames(t, cli)
	require.Len(t, after, 3, "the survivor, the held member, and the replacement beside them")
	var replacement string
	for _, name := range after {
		if name != names[0] && name != names[1] {
			replacement = name
		}
	}
	require.NotEmpty(t, replacement, "the ask answers for the departed member with a fresh replica")

	// No Workload at all: the group composes none, so the gate's wait has no object to watch and
	// the pass creates freely. The replacement from the phase above leaves for good -- no
	// finalizer, so it is GONE rather than departing -- and the Workload goes with the question it
	// carried.
	require.NoError(t, cli.Delete(ctx, &kueue.Workload{
		ObjectMeta: meta.ObjectMeta{Namespace: "team-a", Name: "qwen-wl"},
	}))
	require.NoError(t, cli.Delete(ctx, &core.Pod{ObjectMeta: meta.ObjectMeta{
		Namespace: "team-a", Name: replacement,
	}}))

	_, err = reconcileModelDeployment(t, cli)
	require.NoError(t, err)
	assert.Len(t, replicaNames(t, cli), 3,
		"with no Workload the pass creates freely, or a deployment's first pass would never start")
}

// TestModelDeploymentReconciler_ATemplateEditRollsOneReplicaAtATime is the whole rollout cadence on
// a role of three. The edit must take at least three passes -- one departure at a time -- the role
// must never be more than one replica away from its declared count, and the Workload that admitted
// the group must be the same object at the end, which is what a departure no longer costs.
//
// THE FIXTURE STANDS IN FOR KUEUE'S HALF of the handshake: nothing in this tree runs it, so the
// test writes the ask whenever the group is short, exactly when the real controller would.
func TestModelDeploymentReconciler_ATemplateEditRollsOneReplicaAtATime(t *testing.T) {
	ctx := context.Background()
	md := newRenderDeployment(func(md *workercore.ModelDeployment) { md.Spec.Roles[0].Replicas = 3 })
	cli := newModelDeploymentClient(md, newRenderInstanceType())

	_, err := reconcileModelDeployment(t, cli)
	require.NoError(t, err)
	require.Len(t, replicaNames(t, cli), 3)

	wl := askingGroupWorkload(replicaPods(t, cli), false)
	require.NoError(t, cli.Create(ctx, wl))
	require.NotEmpty(t, wl.UID, "the UID is the fixture's own stamp, not the environment's")

	changed := getModelDeployment(t, cli)
	changed.Spec.Roles[0].Image = "vllm/vllm-openai:v0.26.0"
	require.NoError(t, cli.Update(ctx, changed))

	var deletePasses int
	for pass := 0; pass < 12; pass++ {
		if len(replicaNames(t, cli)) < 3 {
			setGroupAsk(t, cli, true)
		}

		before := len(replicaNames(t, cli))
		_, err = reconcileModelDeployment(t, cli)
		require.NoError(t, err, "pass %d", pass)
		after := len(replicaNames(t, cli))
		require.GreaterOrEqual(t, after, 2,
			"pass %d: never more than one replica away from the declared count", pass)
		if after < before {
			deletePasses++
		}

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

	survivor := new(kueue.Workload)
	require.NoError(t, cli.Get(ctx, ctrlcli.ObjectKey{Namespace: "team-a", Name: "qwen-wl"}, survivor))
	require.Len(t, replicaImages(t, cli), 3)
	assert.Equal(t, wl.UID, survivor.UID,
		"the Workload that admitted the group served the whole rollout: same object, asserted by UID")
	for name, image := range replicaImages(t, cli) {
		assert.Equal(t, "vllm/vllm-openai:v0.26.0", image,
			"%s is built from the edited spec", name)
	}
}

// TestModelDeploymentReconciler_TheAskIsReadAgainstTheAPIServer pins where the gate's Workload read
// goes. The ask decides whether this pass creates, it is written by a controller nothing here
// watches, and the pass is woken by Pod events -- so the read goes to the API server rather than a
// cache that may not have seen the write, and a settled pass issues none at all.
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

func TestModelDeploymentReconciler_TheAskIsReadAgainstTheAPIServer(t *testing.T) {
	ctx := context.Background()
	cli := newModelDeploymentClient(newRenderDeployment(), newRenderInstanceType())
	reader := &countingAPIReader{Reader: cli}
	r := &ModelDeploymentReconciler{Client: cli, APIReader: reader}

	// The first pass and a settled pass read nothing through the API server: the group with no
	// members has no Workload to ask, and a role at its count has no question.
	for pass := 0; pass < 2; pass++ {
		_, err := reconcileModelDeploymentWith(t, r)
		require.NoError(t, err, "pass %d", pass)
	}
	assert.Zero(t, reader.lists,
		"a settled pass issues no API-server read: the gate reads only when a role is short")

	// A short role reads the missing ordinal's Workload through the API server. The member that
	// holds the ordinal open is a departing Pod -- held by Kueue's finalizer, the shape a live
	// cluster keeps through this window -- and a workload owns it, so the gate has something to
	// ask and pays the read.
	names := replicaNames(t, cli)
	require.Len(t, names, 2)
	gone := new(core.Pod)
	require.NoError(t, cli.Get(ctx, ctrlcli.ObjectKey{Namespace: "team-a", Name: names[0]}, gone))
	gone.Finalizers = []string{kueuepodconst.PodFinalizer}
	require.NoError(t, cli.Update(ctx, gone))
	require.NoError(t, cli.Delete(ctx, gone))
	require.NoError(t, cli.Create(ctx, askingGroupWorkload(replicaPods(t, cli), false)))

	_, err := reconcileModelDeploymentWith(t, r)
	require.NoError(t, err)
	assert.Positive(t, reader.lists,
		"the short-ordinal gate reads the Workload against the API server, not the cache")
}
