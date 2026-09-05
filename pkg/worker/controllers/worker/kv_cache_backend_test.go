package worker

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apps "k8s.io/api/apps/v1"
	core "k8s.io/api/core/v1"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/utils/ptr"
	ctrlcli "sigs.k8s.io/controller-runtime/pkg/client"
	ctrlfake "sigs.k8s.io/controller-runtime/pkg/client/fake"
	ctrlinterceptor "sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	ctrlevent "sigs.k8s.io/controller-runtime/pkg/event"
	ctrlreconcile "sigs.k8s.io/controller-runtime/pkg/reconcile"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	"gpustack.ai/gpustack/pkg/kubeclients/kubernetes/scheme"
	"gpustack.ai/gpustack/pkg/systemmeta"
	"gpustack.ai/gpustack/pkg/worker/kuberess"
	"gpustack.ai/gpustack/pkg/worker/kvcache"
	"gpustack.ai/gpustack/pkg/worker/kvcache/mooncake"
)

// newKVCacheBackendObject builds a managed backend with the given consumers already recorded in
// status, which is the only input this task's status derivation reads.
// withReconcilerDiskTier declares a complete local disk tier, both halves.
//
// One helper rather than two, because a tier declared on one side only is refused at admission — a
// fixture that set one half would put the reconciler a question the API never puts to it.
func withReconcilerDiskTier(kvcb *workercore.KVCacheBackend) {
	kvcb.Spec.Connection.Managed.Members[0].LocalDisk = &workercore.KVCacheBackendMemberLocalDisk{
		Path:     "/var/lib/kvcache",
		Capacity: resource.MustParse("4Ti"),
	}
	kvcb.Spec.Connection.Managed.Leader.Offload = &workercore.KVCacheBackendLeaderOffload{Enabled: true}
}

func newKVCacheBackendObject(usedBy ...workercore.KVCacheObjectReference) *workercore.KVCacheBackend {
	kvcb := &workercore.KVCacheBackend{
		ObjectMeta: meta.ObjectMeta{Name: "mooncake-dram"},
		Spec: workercore.KVCacheBackendSpec{
			Type:  "Mooncake",
			Image: "example.com/mooncake:v0",
			Connection: workercore.KVCacheBackendConnection{
				Managed: &workercore.KVCacheBackendManaged{
					Leader: workercore.KVCacheBackendLeader{Replicas: ptr.To[int32](1)},
					Members: []workercore.KVCacheBackendMember{{
						NodeSelector:      map[string]string{"kvcache-dram": "true"},
						Medium:            "DRAM",
						CapacityPerMember: resource.MustParse("500Gi"),
					}},
				},
			},
		},
	}
	kvcb.Status.UsedBy = usedBy
	return kvcb
}

// kvCachePoolsNamedBy materializes the pools a usedBy list names.
//
// The backend refuses on the claims that still RESOLVE, not on the raw list, so a fixture that names
// a pool without creating it is not describing a claimed backend — it is describing one whose
// claimant has already gone, which is a different test.
func kvCachePoolsNamedBy(refs ...workercore.KVCacheObjectReference) []ctrlcli.Object {
	objs := make([]ctrlcli.Object, 0, len(refs))
	for _, ref := range refs {
		if ref.Kind != KVCachePoolKind {
			continue
		}
		objs = append(objs, &workercore.KVCachePool{ObjectMeta: meta.ObjectMeta{Name: ref.Name}})
	}
	return objs
}

func newKVCacheBackendClient(objs ...ctrlcli.Object) ctrlcli.Client {
	return ctrlfake.NewClientBuilder().
		WithScheme(scheme.Scheme).
		WithStatusSubresource(&workercore.KVCacheBackend{}).
		WithObjects(objs...).
		Build()
}

func reconcileKVCacheBackend(t *testing.T, cli ctrlcli.Client, name string) *workercore.KVCacheBackend {
	t.Helper()

	// Every reconciler under test is handed an admin transport, including the cases that are not
	// about the admin surface. Without one the reconciler falls back to a real HTTP client and
	// resolves the leader's Service DNS name, so the suite would make live network calls whose
	// timing and outcome depend on the machine it runs on. The default here answers nothing, which
	// is what a backend whose leader has not started yet looks like.
	r := &KVCacheBackendReconciler{
		Client: cli,
		AdminHTTP: &http.Client{Transport: &adminRoundTripper{byPath: map[string]adminResponse{
			"/health": {err: errors.New("connect: connection refused")},
		}}},
	}
	_, err := r.Reconcile(context.Background(), ctrlreconcile.Request{
		NamespacedName: ctrlcli.ObjectKey{Name: name},
	})
	require.NoError(t, err)

	got := new(workercore.KVCacheBackend)
	if err := cli.Get(context.Background(), ctrlcli.ObjectKey{Name: name}, got); err != nil {
		return nil
	}
	return got
}

// TestKVCacheBackendReconciler_LocksAndReportsProvisioning pins the live path: the object is locked
// so nothing it renders can be orphaned, and it reports Provisioning because nothing has been
// observed SERVING yet. Rendering a leader and observing one are different things, and the three
// workload conditions stay absent until something can actually ask the leader how it is.
func TestKVCacheBackendReconciler_LocksAndReportsProvisioning(t *testing.T) {
	kvcb := newKVCacheBackendObject()
	cli := newKVCacheBackendClient(kvcb)

	got := reconcileKVCacheBackend(t, cli, kvcb.Name)
	require.NotNil(t, got)

	assert.True(t, systemmeta.IsLocked(got), "a live backend must be locked")
	assert.Equal(t, KVCacheBackendPhaseProvisioning, got.Status.Phase)
	assert.True(t, KVCacheBackendConditionDeletable.IsTrue(got),
		"a backend nothing claims is deletable")

	// All three observed axes are asked on every pass, and the leader here does not answer. So each
	// condition exists and is False — a measurement, not an assumption.
	assert.True(t, KVCacheBackendConditionLeaderAvailable.IsFalse(got))
	assert.True(t, KVCacheBackendConditionMembersMounted.IsFalse(got))
	assert.True(t, KVCacheBackendConditionCapacityObserved.IsFalse(got))
	assert.Nil(t, got.Status.Capacity, "an unreachable leader reports no capacity, not zero")

	// And the phase is Provisioning rather than Error: the leader's Deployment has no ready
	// replica here, so not being able to read it is exactly what a backend coming up looks like.
	// Reporting Error would make every fresh install look broken for its first minute.
	assert.Equal(t, KVCacheBackendPhaseProvisioning, got.Status.Phase)
	assert.Equal(t, "LeaderStarting",
		KVCacheBackendConditionLeaderAvailable.GetReason(got))
}

// TestKVCacheBackendReconciler_IsIdempotent pins that a settled backend produces no write. It is
// asserted through the resourceVersion rather than by counting calls, because that is what the API
// server would see: a write here would wake this reconciler again and the loop would not settle.
func TestKVCacheBackendReconciler_IsIdempotent(t *testing.T) {
	kvcb := newKVCacheBackendObject()
	cli := newKVCacheBackendClient(kvcb)

	initial := new(workercore.KVCacheBackend)
	require.NoError(t, cli.Get(context.Background(), ctrlcli.ObjectKey{Name: kvcb.Name}, initial))

	first := reconcileKVCacheBackend(t, cli, kvcb.Name)
	require.NotNil(t, first)
	// Without this the test would pass on a reconciler that writes nothing at all, which is a
	// different thing from one that settles.
	require.NotEqual(t, initial.ResourceVersion, first.ResourceVersion,
		"the first pass must actually write: it locks the object and fills its status")

	second := reconcileKVCacheBackend(t, cli, kvcb.Name)
	require.NotNil(t, second)

	assert.Equal(t, first.ResourceVersion, second.ResourceVersion,
		"a second pass over settled state must write nothing")
	assert.Equal(t, first.Status, second.Status)
}

// TestKVCacheBackendReconciler_RefusesAUsedDelete pins the finalizer contract: while something
// claims the backend the lock is held, the object stays present, and the refusal says which object
// to go and remove. Clearing the last claim then lets the teardown complete.
func TestKVCacheBackendReconciler_RefusesAUsedDelete(t *testing.T) {
	claim := workercore.KVCacheObjectReference{
		Kind: "KVCachePool",
		Name: "team-a-pool",
	}
	kvcb := newKVCacheBackendObject(claim)
	systemmeta.Lock(kvcb)
	now := meta.Now()
	kvcb.DeletionTimestamp = &now

	cli := newKVCacheBackendClient(append([]ctrlcli.Object{kvcb}, kvCachePoolsNamedBy(claim)...)...)

	got := reconcileKVCacheBackend(t, cli, kvcb.Name)
	require.NotNil(t, got, "a claimed backend must not be released")

	assert.True(t, systemmeta.IsLocked(got), "the lock is held while the backend is claimed")
	assert.Equal(t, KVCacheBackendPhaseDeleting, got.Status.Phase)
	assert.True(t, KVCacheBackendConditionDeletable.IsFalse(got))
	assert.Contains(t, KVCacheBackendConditionDeletable.GetMessage(got), "KVCachePool/team-a-pool",
		"the refusal must name what to go and remove")
	assert.Contains(t, got.Status.PhaseMessage, "KVCachePool/team-a-pool")

	// Clearing the claim is what resumes the teardown — no timer, no requeue.
	got.Status.UsedBy = nil
	require.NoError(t, cli.Status().Update(context.Background(), got))

	after := reconcileKVCacheBackend(t, cli, kvcb.Name)
	require.Nil(t, after, "releasing the last claim lets the object go")

	// Assert WHY it is gone: the finalizer was released and the object was then collected, not
	// that some other error made the read fail.
	err := cli.Get(context.Background(), ctrlcli.ObjectKey{Name: kvcb.Name},
		new(workercore.KVCacheBackend))
	assert.True(t, kerrors.IsNotFound(err), "expected NotFound, got %v", err)
}

// TestKVCacheBackendReconciler_ReadsUsedByAsClaimsThatStillExist pins the READ side of usedBy, which
// is the side that has to work when the write side never happens.
//
// A consumer writes its own entry and clears it on the way out, so nothing else is in a position to
// clear one it left behind — and an operator forcing a wedged pool's finalizer off leaves exactly
// that. Refusing on the raw list would then hold this backend's teardown with no event able to end
// it, and the only symptom would be a Deletable=False naming an object nobody can find.
//
// The last case is the boundary, and it is deliberately the opposite of convenient: an entry this
// reconciler cannot resolve is KEPT. Dropping it would turn "cannot verify" into "does not exist" on
// a claim whose whole purpose is to hold a deletion.
func TestKVCacheBackendReconciler_ReadsUsedByAsClaimsThatStillExist(t *testing.T) {
	poolRef := func(name string) workercore.KVCacheObjectReference {
		return workercore.KVCacheObjectReference{Kind: KVCachePoolKind, Name: name}
	}

	testCases := []struct {
		name string
		// usedBy is what the status carries; livePools is which of those objects the cluster still
		// holds. A name in the first and not the second is the claimant that went away without
		// clearing up after itself.
		usedBy    []workercore.KVCacheObjectReference
		livePools []string
		deleting  bool

		wantReleased     bool
		wantDeletable    bool
		wantInMessage    []string
		wantNotInMessage []string
	}{
		{
			name:          "a claim whose pool still exists holds the teardown",
			usedBy:        []workercore.KVCacheObjectReference{poolRef("team-a")},
			livePools:     []string{"team-a"},
			deleting:      true,
			wantDeletable: false,
			wantInMessage: []string{"in use by", "KVCachePool/team-a"},
		},
		{
			name:         "a claim whose pool is gone does not",
			usedBy:       []workercore.KVCacheObjectReference{poolRef("team-a")},
			deleting:     true,
			wantReleased: true,
		},
		{
			name:             "one live claim among stale ones holds, and names only the live one",
			usedBy:           []workercore.KVCacheObjectReference{poolRef("gone"), poolRef("team-b")},
			livePools:        []string{"team-b"},
			deleting:         true,
			wantDeletable:    false,
			wantInMessage:    []string{"KVCachePool/team-b"},
			wantNotInMessage: []string{"KVCachePool/gone"},
		},
		{
			name:          "an entry of a kind this reconciler cannot resolve is kept",
			usedBy:        []workercore.KVCacheObjectReference{{Kind: "SomethingElse", Name: "x"}},
			deleting:      true,
			wantDeletable: false,
			wantInMessage: []string{"SomethingElse/x"},
		},
		{
			name:          "a live backend reports Deletable and says why the list disagrees",
			usedBy:        []workercore.KVCacheObjectReference{poolRef("gone")},
			wantDeletable: true,
			wantInMessage: []string{
				"no object claims this backend",
				"KVCachePool/gone",
				"no longer exists",
			},
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			kvcb := newKVCacheBackendObject(tc.usedBy...)
			systemmeta.Lock(kvcb)
			if tc.deleting {
				now := meta.Now()
				kvcb.DeletionTimestamp = &now
			}

			objs := []ctrlcli.Object{kvcb}
			for _, name := range tc.livePools {
				objs = append(objs, &workercore.KVCachePool{ObjectMeta: meta.ObjectMeta{Name: name}})
			}

			got := reconcileKVCacheBackend(t, newKVCacheBackendClient(objs...), kvcb.Name)
			if tc.wantReleased {
				require.Nil(t, got,
					"a backend whose every claim names something gone has nothing left to hold it")
				return
			}
			require.NotNil(t, got)

			assert.Equal(t, tc.wantDeletable, KVCacheBackendConditionDeletable.IsTrue(got))

			message := KVCacheBackendConditionDeletable.GetMessage(got)
			for _, want := range tc.wantInMessage {
				assert.Contains(t, message, want)
			}
			for _, notWant := range tc.wantNotInMessage {
				assert.NotContains(t, message, notWant,
					"a claim that no longer resolves must not be named as one that does")
			}
		})
	}
}

// TestKVCacheBackendReconciler_EnqueuesEveryBackendAPoolNames pins the watch that makes the read
// above level-based rather than lucky.
//
// Without it the only thing that wakes this reconciler on a claim change is the consumer's own write
// onto this status — and a consumer that vanished writes nothing. The pool's own disappearance has to
// be the event, and a delete carries the object's last known state, which still names its backends.
func TestKVCacheBackendReconciler_EnqueuesEveryBackendAPoolNames(t *testing.T) {
	r := &KVCacheBackendReconciler{}

	got := r.enqueueKVCacheBackendWhenPoolChanged(context.Background(), &workercore.KVCachePool{
		ObjectMeta: meta.ObjectMeta{Name: "team-a"},
		Spec:       workercore.KVCachePoolSpec{Backends: []string{"mooncake-dram", "mooncake-ssd"}},
	})

	assert.Equal(t, []ctrlreconcile.Request{
		{NamespacedName: ctrlcli.ObjectKey{Name: "mooncake-dram"}},
		{NamespacedName: ctrlcli.ObjectKey{Name: "mooncake-ssd"}},
	}, got)

	assert.Empty(t, r.enqueueKVCacheBackendWhenPoolChanged(context.Background(), &workercore.KVCacheBackend{}),
		"an object of another kind enqueues nothing rather than panicking")
}

// TestKVCacheBackendReconciler_DeletesTheWorkloadsBeforeReleasingTheLock pins the ORDER, which is
// the only thing that separates a teardown from a promise of one.
//
// The rendered objects are namespaced dependents of a cluster-scoped owner, so the collector would
// reach them on its own — but between the finalizer coming off and that happening, the leader is
// still serving on an address nothing accounts for and the members still hold the node memory they
// claimed. A fake client runs no garbage collector, which is what makes this assertable: anything
// still present after the pass is something the reconciler did not delete.
func TestKVCacheBackendReconciler_DeletesTheWorkloadsBeforeReleasingTheLock(t *testing.T) {
	ctx := context.Background()
	kvcb := newKVCacheBackendObject()
	cli := newKVCacheBackendClient(kvcb)
	r := &KVCacheBackendReconciler{
		Client:    cli,
		AdminHTTP: &http.Client{Transport: &adminRoundTripper{byPath: map[string]adminResponse{}}},
	}
	req := ctrlreconcile.Request{NamespacedName: ctrlcli.ObjectKey{Name: kvcb.Name}}

	// One pass to render them, so the delete has something to find.
	_, err := r.Reconcile(ctx, req)
	require.NoError(t, err)

	leaderKey := ctrlcli.ObjectKey{
		Name:      mooncake.LeaderObjectName(kvcb),
		Namespace: kuberess.SystemNamespaceName,
	}
	require.NoError(t, cli.Get(ctx, leaderKey, new(apps.Deployment)))
	require.NoError(t, cli.Get(ctx, leaderKey, new(core.Service)))
	daemons := new(apps.DaemonSetList)
	require.NoError(t, cli.List(ctx, daemons, ctrlcli.InNamespace(kuberess.SystemNamespaceName)))
	require.Len(t, daemons.Items, 1, "the first pass must actually render a member group")

	// Deleted for real rather than stamped by hand: the lock is on, so the API server marks the
	// object deleting and leaves it, which is the state the teardown actually meets.
	live := new(workercore.KVCacheBackend)
	require.NoError(t, cli.Get(ctx, ctrlcli.ObjectKey{Name: kvcb.Name}, live))
	require.True(t, systemmeta.IsLocked(live))
	require.NoError(t, cli.Delete(ctx, live))
	require.NoError(t, cli.Get(ctx, ctrlcli.ObjectKey{Name: kvcb.Name}, live))
	require.NotNil(t, live.DeletionTimestamp, "the finalizer must have held it")

	// The pass that deletes them does NOT release the lock. A delete call returns as soon as the
	// API server records the intent, so releasing here would publish "gone" over workloads that
	// are still terminating — the exact claim the ordering exists to make true.
	res, err := r.Reconcile(ctx, req)
	require.NoError(t, err)
	assert.Positive(t, res.RequeueAfter, "the pass that deletes them must come back to check")

	assert.True(t, kerrors.IsNotFound(cli.Get(ctx, leaderKey, new(apps.Deployment))),
		"the leader Deployment is gone by the time the lock is released")
	assert.True(t, kerrors.IsNotFound(cli.Get(ctx, leaderKey, new(core.Service))),
		"and so is the Service in front of it")
	require.NoError(t, cli.List(ctx, daemons, ctrlcli.InNamespace(kuberess.SystemNamespaceName)))
	assert.Empty(t, daemons.Items, "every member group goes with them")

	held := new(workercore.KVCacheBackend)
	require.NoError(t, cli.Get(ctx, ctrlcli.ObjectKey{Name: kvcb.Name}, held),
		"the object is still held while that check is outstanding")
	// And it SAYS so. Teardown lasts as long as the workloads take to terminate, and a pass that
	// returns without writing leaves whatever phase was there before — usually Ready, on an object
	// the API server has already marked for deletion.
	assert.Equal(t, KVCacheBackendPhaseDeleting, held.Status.Phase,
		"the phase says Deleting for the whole teardown, not just at the end of it")

	// The next pass finds nothing left and lets go.
	_, err = r.Reconcile(ctx, req)
	require.NoError(t, err)

	assert.True(t, kerrors.IsNotFound(
		cli.Get(ctx, ctrlcli.ObjectKey{Name: kvcb.Name}, new(workercore.KVCacheBackend))),
		"and only then does the object itself go")
}

// TestKVCacheBackendReconciler_TeardownSparesObjectsItDidNotRender is the collision case, and it is
// about a DELETE, which nothing undoes.
//
// The names are derived, so an object of that name can predate the backend, and the align path
// cannot be relied on to have adopted it: that path never writes spec.selector, because the field is
// immutable, so a same-name workload whose selector differs has its every update rejected and never
// acquires the note. Teardown meets exactly that object, and the only thing separating "ours" from
// "somebody else's who happened to pick this name" is the note.
func TestKVCacheBackendReconciler_TeardownSparesObjectsItDidNotRender(t *testing.T) {
	ctx := context.Background()
	kvcb := newKVCacheBackendObject()
	systemmeta.Lock(kvcb)
	kvcb.DeletionTimestamp = ptr.To(meta.Now())

	leaderKey := ctrlcli.ObjectKey{
		Name:      mooncake.LeaderObjectName(kvcb),
		Namespace: kuberess.SystemNamespaceName,
	}
	// Three strangers: two holding the derived names, one carrying the labels the member sweep
	// selects on. None of them carries the resource note, which is the whole difference.
	stranger := &apps.Deployment{
		ObjectMeta: meta.ObjectMeta{Name: leaderKey.Name, Namespace: leaderKey.Namespace},
		Spec: apps.DeploymentSpec{
			Selector: &meta.LabelSelector{MatchLabels: map[string]string{"app": "someone-else"}},
			Template: core.PodTemplateSpec{
				ObjectMeta: meta.ObjectMeta{Labels: map[string]string{"app": "someone-else"}},
				Spec:       core.PodSpec{Containers: []core.Container{{Name: "c", Image: "busybox"}}},
			},
		},
	}
	strangerSvc := &core.Service{
		ObjectMeta: meta.ObjectMeta{Name: leaderKey.Name, Namespace: leaderKey.Namespace},
		Spec:       core.ServiceSpec{Selector: map[string]string{"app": "someone-else"}},
	}
	strangerDs := &apps.DaemonSet{
		ObjectMeta: meta.ObjectMeta{
			Name:      "not-ours",
			Namespace: kuberess.SystemNamespaceName,
			Labels:    mooncake.BackendLabels(kvcb),
		},
	}

	cli := newKVCacheBackendClient(kvcb, stranger, strangerSvc, strangerDs)
	r := &KVCacheBackendReconciler{
		Client:    cli,
		AdminHTTP: &http.Client{Transport: &adminRoundTripper{byPath: map[string]adminResponse{}}},
	}
	_, err := r.Reconcile(ctx, ctrlreconcile.Request{
		NamespacedName: ctrlcli.ObjectKey{Name: kvcb.Name},
	})
	require.NoError(t, err)

	assert.NoError(t, cli.Get(ctx, leaderKey, new(apps.Deployment)),
		"a Deployment holding the derived name and no note is not this backend's to delete")
	assert.NoError(t, cli.Get(ctx, leaderKey, new(core.Service)),
		"nor is a Service holding it")
	assert.NoError(t, cli.Get(ctx, ctrlcli.ObjectKey{
		Name: "not-ours", Namespace: kuberess.SystemNamespaceName,
	}, new(apps.DaemonSet)), "and labels are a query, not a proof of ownership")

	// And the backend still lets go: the strangers are not this backend's to wait for either, so
	// counting them as outstanding would wedge the teardown forever.
	assert.True(t, kerrors.IsNotFound(
		cli.Get(ctx, ctrlcli.ObjectKey{Name: kvcb.Name}, new(workercore.KVCacheBackend))),
		"an object it will never delete must not hold the finalizer")
}

// TestKVCacheBackendReconciler_TeardownIsIdempotent covers the repeat: a NotFound is the state the
// teardown is trying to reach, so a partially deleted backend converges rather than failing on what
// somebody else already removed.
func TestKVCacheBackendReconciler_TeardownIsIdempotent(t *testing.T) {
	ctx := context.Background()
	kvcb := newKVCacheBackendObject()
	systemmeta.Lock(kvcb)
	now := meta.Now()
	kvcb.DeletionTimestamp = &now

	cli := newKVCacheBackendClient(kvcb)
	r := &KVCacheBackendReconciler{
		Client:    cli,
		AdminHTTP: &http.Client{Transport: &adminRoundTripper{byPath: map[string]adminResponse{}}},
	}

	// Nothing was ever rendered, so every delete below finds nothing at all.
	_, err := r.Reconcile(ctx, ctrlreconcile.Request{
		NamespacedName: ctrlcli.ObjectKey{Name: kvcb.Name},
	})
	require.NoError(t, err, "a teardown with nothing to tear down is not a failure")
}

// TestKVCacheBackendReconciler_ExternalTeardownDeletesNothing is the other half: an external backend
// renders nothing, so its teardown has nothing to remove and must not go looking.
func TestKVCacheBackendReconciler_ExternalTeardownDeletesNothing(t *testing.T) {
	ctx := context.Background()
	kvcb := newExternalKVCacheBackendObject()
	systemmeta.Lock(kvcb)
	now := meta.Now()
	kvcb.DeletionTimestamp = &now

	cli := newClientRefusingCreates(t, kvcb)
	r := &KVCacheBackendReconciler{
		Client:    cli,
		AdminHTTP: &http.Client{Transport: &adminRoundTripper{byPath: map[string]adminResponse{}}},
	}

	_, err := r.Reconcile(ctx, ctrlreconcile.Request{
		NamespacedName: ctrlcli.ObjectKey{Name: kvcb.Name},
	})
	require.NoError(t, err)

	assert.True(t, kerrors.IsNotFound(
		cli.Get(ctx, ctrlcli.ObjectKey{Name: kvcb.Name}, new(workercore.KVCacheBackend))),
		"the lock is released on the same pass, since there is nothing to wait for")
}

// leaderObjectKeys is where the rendered objects land: one namespace shared by every backend, named
// after the backend that owns them.
func leaderObjectKey(kvcb *workercore.KVCacheBackend) ctrlcli.ObjectKey {
	return ctrlcli.ObjectKey{
		Name:      kvcb.Name + "-leader",
		Namespace: kuberess.SystemNamespaceName,
	}
}

func memberObjectKey(kvcb *workercore.KVCacheBackend, group int) ctrlcli.ObjectKey {
	return ctrlcli.ObjectKey{
		Name:      fmt.Sprintf("%s-member-%d", kvcb.Name, group),
		Namespace: kuberess.SystemNamespaceName,
	}
}

// TestKVCacheBackendReconciler_RendersTheLeaderWorkload pins that a managed backend gets a leader
// Deployment, a Service in front of it, and the two addresses published into status.
func TestKVCacheBackendReconciler_RendersTheLeaderWorkload(t *testing.T) {
	kvcb := newKVCacheBackendObject()
	cli := newKVCacheBackendClient(kvcb)

	got := reconcileKVCacheBackend(t, cli, kvcb.Name)
	require.NotNil(t, got)

	ctx := context.Background()

	deploy := new(apps.Deployment)
	require.NoError(t, cli.Get(ctx, leaderObjectKey(kvcb), deploy))
	require.NotNil(t, deploy.Spec.Replicas)
	assert.Equal(t, int32(1), *deploy.Spec.Replicas)
	require.Len(t, deploy.Spec.Template.Spec.Containers, 1)
	assert.Equal(t, kvcb.Spec.Image, deploy.Spec.Template.Spec.Containers[0].Image,
		"the object's own image wins over the setting")

	svc := new(core.Service)
	require.NoError(t, cli.Get(ctx, leaderObjectKey(kvcb), svc))
	assert.Equal(t, core.ServiceTypeClusterIP, svc.Spec.Type)
	assert.Equal(t, deploy.Spec.Selector.MatchLabels, svc.Spec.Selector,
		"the Service must select what the Deployment runs")

	ds := new(apps.DaemonSet)
	require.NoError(t, cli.Get(ctx, memberObjectKey(kvcb, 0), ds),
		"the member group is rendered too; a leader with nothing to allocate against holds no cache")
	assert.Equal(t, kvcb.Spec.Connection.Managed.Members[0].NodeSelector,
		ds.Spec.Template.Spec.NodeSelector)
	assert.NotEqual(t, deploy.Spec.Selector.MatchLabels, ds.Spec.Selector.MatchLabels,
		"the member's selector must not collide with the leader's, or each would front the other's Pods")

	endpoints := make(map[string]string, len(got.Status.Endpoints))
	for _, e := range got.Status.Endpoints {
		endpoints[e.Name] = e.Address
	}
	assert.Equal(t, map[string]string{
		workercore.KVCacheBackendEndpointNameClient: "mooncake-dram-leader.gpustack-system.svc:50051",
		workercore.KVCacheBackendEndpointNameAdmin:  "mooncake-dram-leader.gpustack-system.svc:9003",
	}, endpoints, "both roles are published; a consumer handed one address cannot reach the other")
}

// TestKVCacheBackendReconciler_SettlesAgainstServerDefaults is the case a fake client alone cannot
// prove, and it is the reason this test defaults the objects by hand.
//
// A real API server fills a Pod template and a Service on write — terminationMessagePath,
// imagePullPolicy, dnsPolicy, schedulerName, clusterIP, sessionAffinity and more. A reconciler that
// compared the whole object against what it rendered would therefore differ on EVERY pass and rewrite
// the Deployment forever, rolling the leader each time. The fake client defaults nothing, so it would
// report that reconciler as settled. This test writes those defaults in before the second pass, which
// is the only way the assertion means anything.
func TestKVCacheBackendReconciler_SettlesAgainstServerDefaults(t *testing.T) {
	kvcb := newKVCacheBackendObject()
	cli := newKVCacheBackendClient(kvcb)
	ctx := context.Background()

	require.NotNil(t, reconcileKVCacheBackend(t, cli, kvcb.Name))

	deploy := new(apps.Deployment)
	require.NoError(t, cli.Get(ctx, leaderObjectKey(kvcb), deploy))
	podSpec := &deploy.Spec.Template.Spec
	podSpec.RestartPolicy = core.RestartPolicyAlways
	podSpec.DNSPolicy = core.DNSClusterFirst
	podSpec.SchedulerName = "default-scheduler"
	podSpec.TerminationGracePeriodSeconds = ptr.To[int64](30)
	podSpec.SecurityContext = &core.PodSecurityContext{}
	podSpec.Containers[0].TerminationMessagePath = "/dev/termination-log"
	podSpec.Containers[0].ImagePullPolicy = core.PullIfNotPresent
	deploy.Spec.RevisionHistoryLimit = ptr.To[int32](10)
	deploy.Spec.ProgressDeadlineSeconds = ptr.To[int32](600)
	// Neither the strategy nor the termination message policy is the server's: it defaults each
	// only where the object names none, and this renderer names both. Writing a server default
	// over either would model a write the server never makes, and the pass would correctly undo it.
	require.NoError(t, cli.Update(ctx, deploy))

	svc := new(core.Service)
	require.NoError(t, cli.Get(ctx, leaderObjectKey(kvcb), svc))
	svc.Spec.ClusterIP = "10.43.0.17"
	svc.Spec.ClusterIPs = []string{"10.43.0.17"}
	svc.Spec.SessionAffinity = core.ServiceAffinityNone
	svc.Spec.IPFamilies = []core.IPFamily{core.IPv4Protocol}
	svc.Spec.IPFamilyPolicy = ptr.To(core.IPFamilyPolicySingleStack)
	svc.Spec.InternalTrafficPolicy = ptr.To(core.ServiceInternalTrafficPolicyCluster)
	require.NoError(t, cli.Update(ctx, svc))

	ds := new(apps.DaemonSet)
	require.NoError(t, cli.Get(ctx, memberObjectKey(kvcb, 0), ds))
	dsPod := &ds.Spec.Template.Spec
	dsPod.RestartPolicy = core.RestartPolicyAlways
	dsPod.DNSPolicy = core.DNSClusterFirst
	dsPod.SchedulerName = "default-scheduler"
	// No terminationGracePeriodSeconds here: the member renderer sets it explicitly, so the server
	// never defaults it. Writing the server's 30 in would be testing a field this operator owns.
	dsPod.SecurityContext = &core.PodSecurityContext{}
	dsPod.Containers[0].TerminationMessagePath = "/dev/termination-log"
	dsPod.Containers[0].ImagePullPolicy = core.PullIfNotPresent
	ds.Spec.RevisionHistoryLimit = ptr.To[int32](10)
	require.NoError(t, cli.Update(ctx, ds))

	defaultedDeploy, defaultedSvc := new(apps.Deployment), new(core.Service)
	defaultedDs := new(apps.DaemonSet)
	require.NoError(t, cli.Get(ctx, leaderObjectKey(kvcb), defaultedDeploy))
	require.NoError(t, cli.Get(ctx, leaderObjectKey(kvcb), defaultedSvc))
	require.NoError(t, cli.Get(ctx, memberObjectKey(kvcb, 0), defaultedDs))

	require.NotNil(t, reconcileKVCacheBackend(t, cli, kvcb.Name))

	afterDeploy, afterSvc := new(apps.Deployment), new(core.Service)
	afterDs := new(apps.DaemonSet)
	require.NoError(t, cli.Get(ctx, leaderObjectKey(kvcb), afterDeploy))
	require.NoError(t, cli.Get(ctx, leaderObjectKey(kvcb), afterSvc))
	require.NoError(t, cli.Get(ctx, memberObjectKey(kvcb, 0), afterDs))
	assert.Equal(t, defaultedDs.ResourceVersion, afterDs.ResourceVersion,
		"a defaulted member DaemonSet must read as settled; otherwise every pass rolls every member")

	assert.Equal(t, defaultedDeploy.ResourceVersion, afterDeploy.ResourceVersion,
		"a defaulted Deployment must read as settled; otherwise every pass rolls the leader")
	assert.Equal(t, defaultedSvc.ResourceVersion, afterSvc.ResourceVersion,
		"a defaulted Service must read as settled; clusterIP is assigned, not rendered")
	assert.Equal(t, "10.43.0.17", afterSvc.Spec.ClusterIP,
		"the assigned clusterIP survives: it is immutable, and rendering over it would fail the update")
}

// TestKVCacheBackendReconciler_ConvergesADriftedLeader pins the other half of the same contract: a
// field this operator DOES render is put back when something else changes it. Settling and
// converging are different properties, and a reconciler that only settled would be inert.
func TestKVCacheBackendReconciler_ConvergesADriftedLeader(t *testing.T) {
	kvcb := newKVCacheBackendObject()
	cli := newKVCacheBackendClient(kvcb)
	ctx := context.Background()

	require.NotNil(t, reconcileKVCacheBackend(t, cli, kvcb.Name))

	deploy := new(apps.Deployment)
	require.NoError(t, cli.Get(ctx, leaderObjectKey(kvcb), deploy))
	deploy.Spec.Template.Spec.Containers[0].Image = "someone-elses:image"
	deploy.Spec.Template.Spec.Containers[0].Args = []string{"-rpc_port=1"}
	deploy.Spec.Template.Spec.Containers[0].TerminationMessagePolicy = core.TerminationMessageReadFile
	require.NoError(t, cli.Update(ctx, deploy))

	require.NotNil(t, reconcileKVCacheBackend(t, cli, kvcb.Name))

	after := new(apps.Deployment)
	require.NoError(t, cli.Get(ctx, leaderObjectKey(kvcb), after))
	assert.Equal(t, kvcb.Spec.Image, after.Spec.Template.Spec.Containers[0].Image,
		"a hand-edited image is put back")
	assert.Equal(t, mooncake.RenderLeaderFlags(kvcb.Spec.Connection.Managed.Leader),
		after.Spec.Template.Spec.Containers[0].Args,
		"a hand-edited argv is put back; extraArgs is the supported way to change it")
	assert.Equal(t, core.TerminationMessageFallbackToLogsOnError,
		after.Spec.Template.Spec.Containers[0].TerminationMessagePolicy,
		"and so is the policy: edited back to the default, every later failure would report an "+
			"empty message, and nothing else here would ever look at it")
}

// TestKVCacheBackendReconciler_WithoutAnImage pins the gap admission cannot close, and the two
// shapes it takes.
//
// The webhook refuses an object naming no image while the setting names none either — but the
// setting is editable and can be blanked AFTER that object is admitted, and this loop runs again
// every time. Nothing is rendered in either case, because rendering an empty image would produce an
// API-server rejection pointing at a container instead of at the setting somebody cleared.
//
// What must NOT happen is returning. The workloads an earlier pass created keep running and keep
// serving, so bailing out froze status on a stale reading — with an exponential backoff behind it
// and nothing on the object saying why.
//
// The two cases differ in whether anything was EVER rendered, and they must not report the same
// thing. A backend that has a Service is alive and merely unmanageable; one that has none can never
// start, and saying "the leader is still coming up" about it would leave it at Provisioning forever.
func TestKVCacheBackendReconciler_WithoutAnImage(t *testing.T) {
	cases := []struct {
		name         string
		rendered     bool
		wasPublished bool
		foreignSvc   bool
		wantPhrase   string
	}{
		{
			name:       "never rendered, so it can never start",
			rendered:   false,
			wantPhrase: "nothing has been rendered",
		},
		{
			name:       "already rendered, so it runs on and stops converging",
			rendered:   true,
			wantPhrase: "can no longer be rendered",
		},
		{
			// The status this pass starts from is the one the previous pass wrote, so an address
			// that is no longer derivable has to be actively withdrawn. Left standing it would name
			// a Service that has been deleted, and — because "was anything ever rendered" is read
			// off that same field — it would also report a backend that can never start as one that
			// merely stopped converging.
			name:         "the Service is gone, so the address it published goes with it",
			rendered:     false,
			wasPublished: true,
			wantPhrase:   "nothing has been rendered",
		},
		{
			// The name is DERIVED, so a Service can hold it without this backend having made it —
			// and this branch runs exactly when nothing was rendered to overwrite one. Publishing on
			// existence alone would point consumers at a stranger's workload and then read this
			// backend's health, capacity and membership out of it.
			name:       "a Service of the same name that this backend did not render",
			foreignSvc: true,
			wantPhrase: "nothing has been rendered",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ctx := context.Background()
			kvcb := newKVCacheBackendObject()
			kvcb.Spec.Image = ""
			if c.wasPublished {
				kvcb.Status.Endpoints = mooncake.LeaderEndpoints(kvcb)
				require.NotEmpty(t, kvcb.Status.Endpoints)
			}

			objs := []ctrlcli.Object{kvcb}
			if c.rendered {
				// What an earlier pass left behind, before the setting was cleared.
				objs = append(objs, mooncake.RenderLeaderService(kvcb))
			}
			if c.foreignSvc {
				// Same name, none of the marks: no resource note, no owner. The rendered one is
				// stripped rather than hand-built, so this stays a Service the aligner would
				// otherwise accept and differs from ours in exactly the judged field.
				foreign := mooncake.RenderLeaderService(kvcb)
				foreign.Annotations = nil
				foreign.OwnerReferences = nil
				objs = append(objs, foreign)
			}
			cli := newKVCacheBackendClient(objs...)

			r := &KVCacheBackendReconciler{
				Client: cli,
				AdminHTTP: &http.Client{Transport: &adminRoundTripper{byPath: map[string]adminResponse{
					"/health":              {body: healthServing},
					"/metrics":             {body: metricsPopulated},
					"/get_segments_detail": {body: segmentsEmpty},
				}}},
			}
			res, err := r.Reconcile(ctx, ctrlreconcile.Request{
				NamespacedName: ctrlcli.ObjectKey{Name: kvcb.Name},
			})

			require.NoError(t, err,
				"an editable setting going blank is not this loop's error to retry on")
			assert.Positive(t, res.RequeueAfter, "and the observation timer keeps running")

			assert.True(t, kerrors.IsNotFound(
				cli.Get(ctx, leaderObjectKey(kvcb), new(apps.Deployment))),
				"nothing is rendered from an unresolved image, in either case")

			got := new(workercore.KVCacheBackend)
			require.NoError(t, cli.Get(ctx, ctrlcli.ObjectKey{Name: kvcb.Name}, got))
			assert.Equal(t, KVCacheBackendPhaseDegraded, got.Status.Phase,
				"not Ready and not Provisioning: one is a lie, the other never resolves")
			assert.Contains(t, got.Status.PhaseMessage, c.wantPhrase)
			assert.Contains(t, got.Status.PhaseMessage, "kv-cache-backend-image",
				"and the message names the setting to go and fix, not a container that failed")

			assert.Equal(t, c.rendered, len(got.Status.Endpoints) > 0,
				"an address is published only when the Service behind it exists — deriving one from "+
					"the object's name would advertise something that was never created")
		})
	}
}

// TestKVCacheBackendReconciler_EnqueuesFromTheResourceNote pins the pair the watches rest on: the
// predicate admits an object, and the map function then finds the backend to re-enqueue on it. They
// read the same note, so this asserts them together — a predicate that admitted an object the mapper
// cannot resolve would wake the controller with nothing to do, and the reverse would silently drop
// the event.
func TestKVCacheBackendReconciler_EnqueuesFromTheResourceNote(t *testing.T) {
	kvcb := newKVCacheBackendObject()
	r := &KVCacheBackendReconciler{}
	predicate := kvCacheBackendWorkloadPredicate()

	for name, obj := range map[string]ctrlcli.Object{
		"deployment": mooncake.RenderLeaderDeployment(kvcb, "example.com/mooncake:v0"),
		"service":    mooncake.RenderLeaderService(kvcb),
		"daemonset":  mooncake.RenderMemberDaemonSet(kvcb, 0, "example.com/mooncake:v0"),
	} {
		assert.True(t, predicate.Create(ctrlevent.CreateEvent{Object: obj}),
			"%s: a rendered object must pass the watch predicate", name)

		got := r.enqueueKVCacheBackendWhenWorkloadChanged(context.Background(), obj)
		assert.Equal(t, []ctrlreconcile.Request{
			{NamespacedName: ctrlcli.ObjectKey{Name: kvcb.Name}},
		}, got, "%s: must map back to the cluster-scoped backend, with no namespace", name)
	}

	// Something else's Deployment in the same namespace: neither admitted nor mapped.
	foreign := &apps.Deployment{ObjectMeta: meta.ObjectMeta{
		Name:      "someone-elses",
		Namespace: kuberess.SystemNamespaceName,
	}}
	assert.False(t, predicate.Create(ctrlevent.CreateEvent{Object: foreign}),
		"an unrelated Deployment must not wake this reconciler")
	assert.Empty(t, r.enqueueKVCacheBackendWhenWorkloadChanged(context.Background(), foreign))

	// A rendered object copied verbatim into another namespace. The note still says what it says —
	// an annotation is writable by anyone who can create an object anywhere — and the mapper still
	// resolves it, because the mapper only ever reads that note. The predicate is what has to refuse
	// it: nothing outside the system namespace is ever rendered, so a match there is somebody else's
	// claim, and honoring it would spend a reconcile and three admin reads per event.
	impostor := mooncake.RenderLeaderDeployment(kvcb, "example.com/mooncake:v0")
	impostor.Namespace = "default"
	assert.False(t, predicate.Create(ctrlevent.CreateEvent{Object: impostor}),
		"a resource note written outside the system namespace must not wake this reconciler")
}

// TestKVCacheBackendReconciler_ConvergesAFabricSwitch pins that the transport is converged in BOTH
// directions. Admission permits an update to the transport block, so a backend can gain RDMA and
// lose it again, and each of the four things RDMA brings — hostNetwork, the DNS policy, the device
// volume and the two capabilities — has to come back off.
//
// A renderer that set a field on one path and left it to the server on the other would pass the
// forward half of this and fail the return half, which is how the DNS policy was caught.
func TestKVCacheBackendReconciler_ConvergesAFabricSwitch(t *testing.T) {
	kvcb := newKVCacheBackendObject()
	kvcb.Spec.Transport.Protocol = "TCP"
	cli := newKVCacheBackendClient(kvcb)
	ctx := context.Background()

	require.NotNil(t, reconcileKVCacheBackend(t, cli, kvcb.Name))

	setProtocol := func(protocol string) {
		got := new(workercore.KVCacheBackend)
		require.NoError(t, cli.Get(ctx, ctrlcli.ObjectKey{Name: kvcb.Name}, got))
		got.Spec.Transport.Protocol = protocol
		require.NoError(t, cli.Update(ctx, got))
		require.NotNil(t, reconcileKVCacheBackend(t, cli, kvcb.Name))
	}

	memberPod := func() core.PodSpec {
		ds := new(apps.DaemonSet)
		require.NoError(t, cli.Get(ctx, memberObjectKey(kvcb, 0), ds))
		return ds.Spec.Template.Spec
	}

	setProtocol("RDMA")
	rdma := memberPod()
	assert.True(t, rdma.HostNetwork, "switching to RDMA takes the host network")
	assert.Equal(t, core.DNSClusterFirstWithHostNet, rdma.DNSPolicy)
	require.Len(t, rdma.Volumes, 1, "and the device tree")
	require.NotNil(t, rdma.Containers[0].SecurityContext)
	assert.Len(t, rdma.Containers[0].SecurityContext.Capabilities.Add, 2)

	setProtocol("TCP")
	tcp := memberPod()
	assert.False(t, tcp.HostNetwork, "switching back gives the host network up")
	assert.Equal(t, core.DNSClusterFirst, tcp.DNSPolicy,
		"and the DNS policy with it, rather than leaving ClusterFirstWithHostNet on a Pod "+
			"that no longer has the host's network")
	assert.Empty(t, tcp.Volumes, "and unmounts the device tree")
	assert.Nil(t, tcp.Containers[0].SecurityContext,
		"and drops the capabilities entirely, rather than leaving an empty context behind")
}

// TestKVCacheBackendReconciler_ConvergesAMultiTenancySwitch pins the edit an operator is TOLD to
// make: the pool webhook refuses a pool whose backend runs without multi-tenancy and names the field
// to set, so turning it on for a backend that already has a leader is the ordinary path, not a
// corner. Turning it on adds a volume pair and a seed container beside the mount the container pass
// already moves, and a pass that moved one without the other would be refused by the API server on
// every reconcile — while this object went on reporting Ready.
func TestKVCacheBackendReconciler_ConvergesAMultiTenancySwitch(t *testing.T) {
	kvcb := newKVCacheBackendObject()
	cli := newKVCacheBackendClient(kvcb)
	ctx := context.Background()

	require.NotNil(t, reconcileKVCacheBackend(t, cli, kvcb.Name))

	setMultiTenancy := func(on bool) {
		got := new(workercore.KVCacheBackend)
		require.NoError(t, cli.Get(ctx, ctrlcli.ObjectKey{Name: kvcb.Name}, got))
		got.Spec.Connection.Managed.Leader.MultiTenancy = on
		require.NoError(t, cli.Update(ctx, got))
		require.NotNil(t, reconcileKVCacheBackend(t, cli, kvcb.Name))
	}

	leaderPod := func() core.PodSpec {
		deploy := new(apps.Deployment)
		require.NoError(t, cli.Get(ctx, leaderObjectKey(kvcb), deploy))
		return deploy.Spec.Template.Spec
	}

	// Every mount names a volume the pod carries. The API server enforces this and answers
	// `volumeMounts[0].name: Not found` when it does not, rejecting the whole update — but the fake
	// client runs NO pod-spec validation, so without asserting it here the suite would happily
	// accept a template a real cluster refuses forever.
	mountsResolve := func(t *testing.T, pod core.PodSpec) {
		t.Helper()
		carried := make(map[string]struct{}, len(pod.Volumes))
		for _, v := range pod.Volumes {
			carried[v.Name] = struct{}{}
		}
		containers := append(append([]core.Container{}, pod.InitContainers...), pod.Containers...)
		for _, c := range containers {
			for _, m := range c.VolumeMounts {
				assert.Contains(t, carried, m.Name,
					"container %q mounts %q and the pod carries no such volume", c.Name, m.Name)
			}
		}
	}

	off := leaderPod()
	mountsResolve(t, off)
	assert.Empty(t, off.Volumes, "multi-tenancy off carries no policy volume")
	assert.Empty(t, off.InitContainers, "and nothing to seed one")

	setMultiTenancy(true)
	on := leaderPod()
	mountsResolve(t, on)
	assert.Len(t, on.Volumes, 2, "turning it on brings the writable volume and the seed beside it")
	assert.Len(t, on.InitContainers, 1, "and the container that seeds one before the master looks")
	assert.Contains(t, on.Containers[0].Args, "-enable_multi_tenants=true",
		"and the flag, which is the whole reason the edit was made")

	setMultiTenancy(false)
	back := leaderPod()
	mountsResolve(t, back)
	assert.Empty(t, back.Volumes, "turning it back off takes the volumes away")
	assert.Empty(t, back.InitContainers,
		"and the seed container with them, rather than leaving one that runs on every start")
	assert.NotContains(t, back.Containers[0].Args, "-enable_multi_tenants=true")
}

// adminResponse is one canned reply. A nil err with a status and a body is a reply that arrived; a
// non-nil err is nothing arriving at all, which is a different outcome the reconciler must not
// confuse with a bad reply.
type adminResponse struct {
	status int
	body   string
	err    error
	// location is the redirect target a leader's address can send this operator to, which is only
	// ever set by the case that pins what happens when one does.
	location string
}

// adminRoundTripper answers per path, so one case can pair a healthy /health with a failing
// /metrics — which is exactly the combination the service_ready gate exists for.
type adminRoundTripper struct {
	byPath map[string]adminResponse
	asked  []string
	// hosts records where each request went, which is the only way to tell reading a backend's own
	// address from reading a Service this operator happened to render.
	hosts []string
}

func (rt *adminRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	rt.asked = append(rt.asked, req.URL.Path)
	rt.hosts = append(rt.hosts, req.URL.Host)

	resp, ok := rt.byPath[req.URL.Path]
	if !ok {
		return &http.Response{
			StatusCode: http.StatusNotFound,
			Body:       io.NopCloser(strings.NewReader("")),
			Header:     make(http.Header),
			Request:    req,
		}, nil
	}
	if resp.err != nil {
		return nil, resp.err
	}

	status := resp.status
	if status == 0 {
		status = http.StatusOK
	}
	header := make(http.Header)
	if resp.location != "" {
		header.Set("Location", resp.location)
	}
	return &http.Response{
		StatusCode: status,
		Body:       io.NopCloser(strings.NewReader(resp.body)),
		Header:     header,
		Request:    req,
	}, nil
}

const (
	healthServing    = `{"status":"ok","role":"leader","ha_state":"serving","service_ready":true}`
	healthNotServing = `{"status":"ok","role":"leader","ha_state":"starting","service_ready":false}`

	metricsPopulated = "master_total_capacity_bytes 1082331758592\n" +
		"master_allocated_bytes 5476083302\n"
	// What a leader that is UP but not serving answers: 200, well-formed, and all zeroes, because
	// no segment has mounted yet.
	metricsZeroed = "master_total_capacity_bytes 0\nmaster_allocated_bytes 0\n"
	metricsFile   = "master_total_file_capacity_bytes 2164663517184\n" +
		"master_allocated_file_size_bytes 10952166604\n"
	metricsWithoutOurFamilies = "master_key_count 1284\nmaster_active_clients 2\n"
)

func reconcileWithAdmin(
	t *testing.T, kvcb *workercore.KVCacheBackend, byPath map[string]adminResponse,
) *workercore.KVCacheBackend {
	t.Helper()

	cli := newKVCacheBackendClient(kvcb)
	r := &KVCacheBackendReconciler{
		Client:    cli,
		AdminHTTP: &http.Client{Transport: &adminRoundTripper{byPath: byPath}},
	}
	_, err := r.Reconcile(context.Background(), ctrlreconcile.Request{
		NamespacedName: ctrlcli.ObjectKey{Name: kvcb.Name},
	})
	require.NoError(t, err)

	got := new(workercore.KVCacheBackend)
	require.NoError(t, cli.Get(context.Background(), ctrlcli.ObjectKey{Name: kvcb.Name}, got))
	return got
}

func TestKVCacheBackendCapacity_PublishesWhatTheLeaderReports(t *testing.T) {
	got := reconcileWithAdmin(t, newKVCacheBackendObject(), map[string]adminResponse{
		"/health":  {body: healthServing},
		"/metrics": {body: metricsPopulated},
	})

	require.NotNil(t, got.Status.Capacity.Total)
	require.NotNil(t, got.Status.Capacity.Used)
	assert.Equal(t, int64(1082331758592), got.Status.Capacity.Total.Value())
	assert.Equal(t, int64(5476083302), got.Status.Capacity.Used.Value())
	assert.True(t, KVCacheBackendConditionCapacityObserved.IsTrue(got))
}

// TestKVCacheBackendCapacity_FollowsTheScrapeAndNotTheSpec pins that nothing sums the spec. The
// object below asks for 500Gi per member; the leader reports a total that agrees with no arithmetic
// over that number, and the reported figure is what is published.
func TestKVCacheBackendCapacity_FollowsTheScrapeAndNotTheSpec(t *testing.T) {
	kvcb := newKVCacheBackendObject()
	kvcb.Spec.Connection.Managed.Members[0].CapacityPerMember = resource.MustParse("500Gi")

	got := reconcileWithAdmin(t, kvcb, map[string]adminResponse{
		"/health":  {body: healthServing},
		"/metrics": {body: "master_total_capacity_bytes 777\nmaster_allocated_bytes 13\n"},
	})

	require.NotNil(t, got.Status.Capacity.Total)
	assert.Equal(t, int64(777), got.Status.Capacity.Total.Value(),
		"the leader's number is published; the spec is not a source of capacity")
}

// TestKVCacheBackendCapacity_NotServingIsAbsentNotZero is the case this whole gate exists for.
//
// The scrape SUCCEEDS here. /metrics is not gated by the leader, so a leader that is up but not
// serving answers 200 with a well-formed exposition whose gauges read zero. A reconciler that gated
// on the scrape's success — which is what "a failed scrape leaves it absent" alone amounts to —
// would publish that zero as an observation, and a zero reads as an empty cache.
func TestKVCacheBackendCapacity_NotServingIsAbsentNotZero(t *testing.T) {
	got := reconcileWithAdmin(t, newKVCacheBackendObject(), map[string]adminResponse{
		"/health":  {body: healthNotServing},
		"/metrics": {body: metricsZeroed},
	})

	assert.Nil(t, got.Status.Capacity,
		"absent, not the zero the leader would have handed over quite successfully")
	assert.True(t, KVCacheBackendConditionCapacityObserved.IsFalse(got))
	assert.Equal(t, "ServicePlaneNotActive",
		KVCacheBackendConditionCapacityObserved.GetReason(got))
	assert.Contains(t, KVCacheBackendConditionCapacityObserved.GetMessage(got),
		"service plane is not active",
		"the message says the leader is starting, not that something failed")
}

func TestKVCacheBackendCapacity_DistinguishesEveryFailure(t *testing.T) {
	cases := []struct {
		name       string
		byPath     map[string]adminResponse
		wantReason string
		wantIn     string
	}{
		{
			name: "nothing answers",
			byPath: map[string]adminResponse{
				"/health": {err: errors.New("connect: connection refused")},
			},
			wantReason: "LeaderStarting",
			wantIn:     "connection refused",
		},
		{
			name: "the health document does not parse",
			byPath: map[string]adminResponse{
				"/health": {body: `{"status":"ok","service_ready":`},
			},
			wantReason: "LeaderStarting",
			wantIn:     "does not parse",
		},
		{
			name: "the leader is serving but the scrape fails",
			byPath: map[string]adminResponse{
				"/health":  {body: healthServing},
				"/metrics": {status: http.StatusInternalServerError, body: "boom"},
			},
			wantReason: "ScrapeFailed",
			wantIn:     "500",
		},
		{
			name: "the exposition does not carry our families",
			byPath: map[string]adminResponse{
				"/health":  {body: healthServing},
				"/metrics": {body: metricsWithoutOurFamilies},
			},
			wantReason: "FamilyMissing",
			wantIn:     "medium",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := reconcileWithAdmin(t, newKVCacheBackendObject(), c.byPath)

			assert.Nil(t, got.Status.Capacity, "never a zero, whatever went wrong")
			assert.True(t, KVCacheBackendConditionCapacityObserved.IsFalse(got))
			assert.Equal(t, c.wantReason,
				KVCacheBackendConditionCapacityObserved.GetReason(got),
				"each failure has its own reason, because each needs a different thing done about it")
			assert.Contains(t, KVCacheBackendConditionCapacityObserved.GetMessage(got), c.wantIn)
		})
	}
}

// TestKVCacheBackendCapacity_DiskTierIsStillAbsentNotZero re-runs the two absence rules against a
// backend that HAS a disk tier.
//
// Both rules were already covered, but only on the path that reads the memory pair alone. A disk
// tier goes through a different branch — the one that ADDS two gauges — and adding is exactly where
// an absence rule is easiest to lose: a nil plus a nil is a very natural zero, and a published zero
// reads as an empty cache rather than as one nobody has looked at.
func TestKVCacheBackendCapacity_DiskTierIsStillAbsentNotZero(t *testing.T) {
	t.Run("a scrape that fails leaves capacity absent", func(t *testing.T) {
		kvcb := newKVCacheBackendObject()
		withReconcilerDiskTier(kvcb)

		got := reconcileWithAdmin(t, kvcb, map[string]adminResponse{
			"/health":  {body: healthServing},
			"/metrics": {err: errors.New("connect: connection refused")},
		})

		assert.Nil(t, got.Status.Capacity, "absent, never a zero and never the previous value")
		assert.True(t, KVCacheBackendConditionCapacityObserved.IsFalse(got))
	})

	t.Run("a clean all-zero exposition while not serving stays absent", func(t *testing.T) {
		kvcb := newKVCacheBackendObject()
		withReconcilerDiskTier(kvcb)

		got := reconcileWithAdmin(t, kvcb, map[string]adminResponse{
			"/health":  {body: healthNotServing},
			"/metrics": {body: metricsZeroed},
		})

		assert.Nil(t, got.Status.Capacity,
			"the scrape SUCCEEDED and both gauges read zero; the gate is service_ready, not the scrape")
		assert.True(t, KVCacheBackendConditionCapacityObserved.IsFalse(got))
	})
}

// TestKVCacheBackendCapacity_DescribesWhatTheBackendIsMadeOf pins which pair of gauges a backend is
// read from, which is decided by whether it has a disk tier rather than by any medium name.
//
// It replaces a case that ran the same exposition through five media and asserted the file pair for
// three of them. That case agreed with a classification which put NoF in the file family — the
// leader keeps a THIRD pair for NoF (master_total_nof_capacity_bytes) that nothing here reads — so
// it was green against a rule that was wrong. Both the classification and that case are gone: the
// wrong rule left with the enum values it keyed on, and deleting the defect without its instrument
// would have left a test passing against a medium that no longer exists.
//
// ONE exposition is used for both cases on purpose. The rule under test is a property of the SPEC,
// so giving each case its own body would let a wrong rule pass on the strength of a differently
// shaped input.
func TestKVCacheBackendCapacity_DescribesWhatTheBackendIsMadeOf(t *testing.T) {
	cases := []struct {
		name      string
		mutate    func(*workercore.KVCacheBackend)
		wantTotal int64
	}{
		{
			// The file pair is present in the exposition and reads non-zero, so this case fails if
			// the sum is taken unconditionally.
			name:      "no disk tier reads the memory pair alone",
			wantTotal: 1082331758592,
		},
		{
			// The figure is the SUM and is deliberately not either pair on its own: the memory pair
			// alone is 1082331758592 and the file pair alone is 2164663517184, so an expectation
			// equal to one of them could not tell "both pairs" from "the file pair", which is the
			// distinction under test.
			name:      "a disk tier is both pairs, because the backend is made of both",
			mutate:    withReconcilerDiskTier,
			wantTotal: 1082331758592 + 2164663517184,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			kvcb := newKVCacheBackendObject()
			if c.mutate != nil {
				c.mutate(kvcb)
			}

			got := reconcileWithAdmin(t, kvcb, map[string]adminResponse{
				"/health":  {body: healthServing},
				"/metrics": {body: metricsPopulated + metricsFile},
			})

			require.NotNil(t, got.Status.Capacity.Total,
				"the exposition carries both pairs; this backend must read the right ones")
			assert.Equal(t, c.wantTotal, got.Status.Capacity.Total.Value())
		})
	}
}

// TestKVCacheBackendCapacity_DoesNotRepublishAStaleFigure pins that a figure observed once does not
// survive an observation that failed. A retained value would read as current, which is the same
// falsehood as a zero and harder to notice.
func TestKVCacheBackendCapacity_DoesNotRepublishAStaleFigure(t *testing.T) {
	kvcb := newKVCacheBackendObject()
	cli := newKVCacheBackendClient(kvcb)
	ctx := context.Background()

	rt := &adminRoundTripper{byPath: map[string]adminResponse{
		"/health":  {body: healthServing},
		"/metrics": {body: metricsPopulated},
	}}
	r := &KVCacheBackendReconciler{
		Client:    cli,
		AdminHTTP: &http.Client{Transport: rt},
	}
	reconcile := func() *workercore.KVCacheBackend {
		_, err := r.Reconcile(ctx, ctrlreconcile.Request{
			NamespacedName: ctrlcli.ObjectKey{Name: kvcb.Name},
		})
		require.NoError(t, err)
		got := new(workercore.KVCacheBackend)
		require.NoError(t, cli.Get(ctx, ctrlcli.ObjectKey{Name: kvcb.Name}, got))
		return got
	}

	observed := reconcile()
	require.NotNil(t, observed.Status.Capacity.Total, "the first pass must actually observe")

	// The leader stops answering.
	rt.byPath["/health"] = adminResponse{err: errors.New("connect: connection refused")}

	after := reconcile()
	assert.Nil(t, after.Status.Capacity,
		"the figure from the last successful pass must not be republished as current")
	assert.True(t, KVCacheBackendConditionCapacityObserved.IsFalse(after))
}

// memberPodOn fabricates the Pod a DaemonSet would have created on a node, carrying the fingerprint
// the template had at the time. The fake client runs no DaemonSet controller, so the Pods a real
// cluster would create are built here — which is also what lets a case age one deliberately.
func memberPodOn(
	t *testing.T, kvcb *workercore.KVCacheBackend, node, fingerprint string,
) *core.Pod {
	t.Helper()

	ds := mooncake.RenderMemberDaemonSet(kvcb, 0, "example.com/mooncake:v0")
	labels := make(map[string]string, len(ds.Spec.Selector.MatchLabels))
	for k, v := range ds.Spec.Selector.MatchLabels {
		labels[k] = v
	}

	return &core.Pod{
		ObjectMeta: meta.ObjectMeta{
			Name:        "member-" + node,
			Namespace:   kuberess.SystemNamespaceName,
			Labels:      labels,
			Annotations: map[string]string{mooncake.MemberPodSpecHashAnnotation: fingerprint},
			// The DaemonSet controller stamps this on every Pod it makes, and the restart path
			// reads it: a selector says a Pod LOOKS like ours, this says it IS. A fixture without
			// it would agree with a restart path that deleted on the selector alone.
			OwnerReferences: []meta.OwnerReference{{
				APIVersion: "apps/v1",
				Kind:       "DaemonSet",
				Name:       ds.Name,
				Controller: ptr.To(true),
			}},
		},
		Spec: core.PodSpec{NodeName: node},
	}
}

func currentMemberFingerprint(t *testing.T, kvcb *workercore.KVCacheBackend) string {
	t.Helper()
	ds := mooncake.RenderMemberDaemonSet(kvcb, 0, "example.com/mooncake:v0")
	return ds.Spec.Template.Annotations[mooncake.MemberPodSpecHashAnnotation]
}

func livingMemberPods(t *testing.T, cli ctrlcli.Client) []string {
	t.Helper()

	pods := new(core.PodList)
	require.NoError(t, cli.List(context.Background(), pods,
		ctrlcli.InNamespace(kuberess.SystemNamespaceName)))

	names := make([]string, 0, len(pods.Items))
	for i := range pods.Items {
		names = append(names, pods.Items[i].Name)
	}
	return names
}

// TestKVCacheBackendScale_WideningRestartsNothing is the growth half of the contract: adding a node
// to a group must not disturb the members already holding cache.
func TestKVCacheBackendScale_WideningRestartsNothing(t *testing.T) {
	kvcb := newKVCacheBackendObject()
	// Two constraints to start with, because widening means REMOVING one: a nodeSelector's entries
	// are ANDed, so adding a key narrows the group and would test the opposite of the name.
	kvcb.Spec.Connection.Managed.Members[0].NodeSelector = map[string]string{
		"kvcache-dram": "true", "zone": "b",
	}
	current := currentMemberFingerprint(t, kvcb)

	cli := newKVCacheBackendClient(kvcb,
		memberPodOn(t, kvcb, "n7", current),
		memberPodOn(t, kvcb, "n8", current))
	ctx := context.Background()

	// Settle first, so the workloads exist and the comparison below is against a real object.
	require.NotNil(t, reconcileKVCacheBackend(t, cli, kvcb.Name))
	require.ElementsMatch(t, []string{"member-n7", "member-n8"}, livingMemberPods(t, cli),
		"the settled pass must not restart anything either")

	before := new(apps.Deployment)
	require.NoError(t, cli.Get(ctx, leaderObjectKey(kvcb), before))

	// Widen the group, exactly as an operator taking in a zone would: one constraint fewer.
	live := new(workercore.KVCacheBackend)
	require.NoError(t, cli.Get(ctx, ctrlcli.ObjectKey{Name: kvcb.Name}, live))
	live.Spec.Connection.Managed.Members[0].NodeSelector = map[string]string{
		"kvcache-dram": "true",
	}
	require.NoError(t, cli.Update(ctx, live))

	require.NotNil(t, reconcileKVCacheBackend(t, cli, kvcb.Name))

	// What this asserts is that THIS operator deletes nothing. Whether the DaemonSet's controller
	// then places a Pod on a newly eligible node is its own business and is not exercised here —
	// the fake client runs no such controller.
	assert.ElementsMatch(t, []string{"member-n7", "member-n8"}, livingMemberPods(t, cli),
		"a widening deletes no member Pod; the new node's is the DaemonSet controller's to add")

	ds := new(apps.DaemonSet)
	require.NoError(t, cli.Get(ctx, memberObjectKey(kvcb, 0), ds))
	assert.Equal(t, map[string]string{"kvcache-dram": "true"},
		ds.Spec.Template.Spec.NodeSelector, "the widening did reach the DaemonSet")

	after := new(apps.Deployment)
	require.NoError(t, cli.Get(ctx, leaderObjectKey(kvcb), after))
	assert.Equal(t, before.ResourceVersion, after.ResourceVersion,
		"and the leader is untouched by a member-group change")
}

// TestKVCacheBackendScale_ASpecChangeRestartsEveryMember is the other half. OnDelete alone would
// leave a changed image written and never applied; the fingerprint is what turns it back into a
// change that takes effect.
func TestKVCacheBackendScale_ASpecChangeRestartsEveryMember(t *testing.T) {
	kvcb := newKVCacheBackendObject()

	cli := newKVCacheBackendClient(kvcb,
		memberPodOn(t, kvcb, "n7", "a-fingerprint-from-before-the-change"),
		memberPodOn(t, kvcb, "n8", "a-fingerprint-from-before-the-change"))

	require.NotNil(t, reconcileKVCacheBackend(t, cli, kvcb.Name))

	assert.Empty(t, livingMemberPods(t, cli),
		"every member built from the old template is deleted, so the DaemonSet recreates it "+
			"from the new one")
}

// TestKVCacheBackendScale_LeavesCurrentMembersAlone pins that the restart is targeted. A pass that
// deleted Pods already carrying the current fingerprint would restart the group on every reconcile.
func TestKVCacheBackendScale_LeavesCurrentMembersAlone(t *testing.T) {
	kvcb := newKVCacheBackendObject()
	current := currentMemberFingerprint(t, kvcb)

	cli := newKVCacheBackendClient(kvcb,
		memberPodOn(t, kvcb, "n7", current),
		memberPodOn(t, kvcb, "n8", "stale"))

	require.NotNil(t, reconcileKVCacheBackend(t, cli, kvcb.Name))

	assert.Equal(t, []string{"member-n7"}, livingMemberPods(t, cli),
		"only the outdated member is restarted; the current one keeps its cache")

	// And a second pass over the settled group deletes nothing more.
	require.NotNil(t, reconcileKVCacheBackend(t, cli, kvcb.Name))
	assert.Equal(t, []string{"member-n7"}, livingMemberPods(t, cli),
		"reconciling again is not a restart")
}

// TestKVCacheBackendScale_ADiskTierCapacityChangeRestartsThatGroupOnly pins the one edit the disk
// tier deliberately leaves open.
//
// Admission freezes whether a group has a tier and where it lives, because both strand what the
// members already wrote. The ceiling is editable instead, and this is what that costs: the members
// of that group restart, since the ceiling is an environment variable and nothing short of a
// restart re-reads one. The tier's CONTENTS survive that restart, which is the whole reason it is a
// hostPath.
//
// The leader is asserted untouched by resourceVersion, because a member-side edit that rewrote the
// leader Deployment would restart the metadata service the entire backend depends on to raise one
// member's disk ceiling.
func TestKVCacheBackendScale_ADiskTierCapacityChangeRestartsThatGroupOnly(t *testing.T) {
	ctx := context.Background()

	kvcb := newKVCacheBackendObject()
	withReconcilerDiskTier(kvcb)
	current := currentMemberFingerprint(t, kvcb)

	cli := newKVCacheBackendClient(kvcb,
		memberPodOn(t, kvcb, "n7", current),
		memberPodOn(t, kvcb, "n8", current))

	require.NotNil(t, reconcileKVCacheBackend(t, cli, kvcb.Name))
	assert.ElementsMatch(t, []string{"member-n7", "member-n8"}, livingMemberPods(t, cli),
		"a settled group is not restarted by a pass that changes nothing")

	before := new(apps.Deployment)
	require.NoError(t, cli.Get(ctx, leaderObjectKey(kvcb), before))

	// Raise the ceiling, which admission permits and the fingerprint therefore has to notice.
	live := new(workercore.KVCacheBackend)
	require.NoError(t, cli.Get(ctx, ctrlcli.ObjectKey{Name: kvcb.Name}, live))
	live.Spec.Connection.Managed.Members[0].LocalDisk.Capacity = resource.MustParse("8Ti")
	require.NoError(t, cli.Update(ctx, live))

	require.NotNil(t, reconcileKVCacheBackend(t, cli, kvcb.Name))

	assert.Empty(t, livingMemberPods(t, cli),
		"every member of that group is deleted, so the DaemonSet recreates it reading the new ceiling")

	after := new(apps.Deployment)
	require.NoError(t, cli.Get(ctx, leaderObjectKey(kvcb), after))
	assert.Equal(t, before.ResourceVersion, after.ResourceVersion,
		"and the leader is untouched: a member's disk ceiling is not the metadata service's business")
}

// TestKVCacheBackendScale_ARemovedGroupIsTornDown covers the lifecycle that allowing several member
// groups opened up.
//
// The sync loop walks the CURRENT groups, so on its own it can only create and update: a group
// dropped from the spec left a DaemonSet nothing addresses, with members still holding node memory
// the backend no longer declares. It was unreachable while exactly one group was allowed — dropping
// the only group means deleting the object — and became reachable the moment several were.
func TestKVCacheBackendScale_ARemovedGroupIsTornDown(t *testing.T) {
	ctx := context.Background()

	kvcb := newKVCacheBackendObject()
	kvcb.Spec.Connection.Managed.Members = append(kvcb.Spec.Connection.Managed.Members,
		workercore.KVCacheBackendMember{
			NodeSelector:      map[string]string{"kvcache-cold": "true"},
			Medium:            "DRAM",
			CapacityPerMember: resource.MustParse("10Ti"),
		})

	cli := newKVCacheBackendClient(kvcb)
	require.NotNil(t, reconcileKVCacheBackend(t, cli, kvcb.Name))

	for _, group := range []int{0, 1} {
		require.NoError(t, cli.Get(ctx, memberObjectKey(kvcb, group), new(apps.DaemonSet)),
			"both groups render a DaemonSet before the removal")
	}

	// Drop the second group.
	live := new(workercore.KVCacheBackend)
	require.NoError(t, cli.Get(ctx, ctrlcli.ObjectKey{Name: kvcb.Name}, live))
	live.Spec.Connection.Managed.Members = live.Spec.Connection.Managed.Members[:1]
	require.NoError(t, cli.Update(ctx, live))

	require.NotNil(t, reconcileKVCacheBackend(t, cli, kvcb.Name))

	require.NoError(t, cli.Get(ctx, memberObjectKey(kvcb, 0), new(apps.DaemonSet)),
		"the surviving group is untouched")
	err := cli.Get(ctx, memberObjectKey(kvcb, 1), new(apps.DaemonSet))
	assert.True(t, kerrors.IsNotFound(err),
		"the removed group's DaemonSet is deleted; left standing, its members go on serving "+
			"segments the backend no longer declares, got err=%v", err)
}

// TestKVCacheBackendScale_PruningProvesOwnership pins that the sweep deletes on this backend's own
// note rather than on a derived name.
//
// The per-group names are built from the backend's name, so an unrelated object can already carry
// one. Deleting by name alone would delete somebody else's workload the first time a group is
// dropped.
func TestKVCacheBackendScale_PruningProvesOwnership(t *testing.T) {
	ctx := context.Background()

	kvcb := newKVCacheBackendObject()

	// A DaemonSet wearing the resource-type label and the name group 1 WOULD have, but carrying no
	// note of this backend. The spec declares one group, so the sweep sees it as surplus by name.
	stranger := &apps.DaemonSet{
		ObjectMeta: meta.ObjectMeta{
			Name:      mooncake.MemberObjectName(kvcb, 1),
			Namespace: kuberess.SystemNamespaceName,
			Labels: systemmeta.GetResourcesLabelSetOfType[map[string]string](
				kvcache.ResourceType),
		},
	}

	cli := newKVCacheBackendClient(kvcb, stranger)
	require.NotNil(t, reconcileKVCacheBackend(t, cli, kvcb.Name))

	assert.NoError(t, cli.Get(ctx, ctrlcli.ObjectKeyFromObject(stranger), new(apps.DaemonSet)),
		"a workload this backend never rendered must survive the sweep, however its name reads")
}

// TestKVCacheBackendScale_AGraceChangeReachesTheHook covers the convergence half of an editable
// grace.
//
// The grace lives INSIDE the hook's argv. The fingerprint moves with it, so the members are deleted
// and recreated — but from the DaemonSet's template, so if the aligner does not carry Lifecycle
// across, they come back with the new hash and the old hook, permanently. That is worse than not
// converging: the restart makes it look like the change took.
func TestKVCacheBackendScale_AGraceChangeReachesTheHook(t *testing.T) {
	ctx := context.Background()

	kvcb := newKVCacheBackendObject()
	withReconcilerDiskTier(kvcb)
	kvcb.Spec.Connection.Managed.ScaleIn = &workercore.KVCacheBackendScaleIn{GracePeriodSeconds: 30}

	cli := newKVCacheBackendClient(kvcb)
	require.NotNil(t, reconcileKVCacheBackend(t, cli, kvcb.Name))

	ds := new(apps.DaemonSet)
	require.NoError(t, cli.Get(ctx, memberObjectKey(kvcb, 0), ds))
	require.NotNil(t, ds.Spec.Template.Spec.Containers[0].Lifecycle)
	assert.Contains(t, ds.Spec.Template.Spec.Containers[0].Lifecycle.PreStop.Exec.Command[2],
		`"grace_period_seconds":30`)

	live := new(workercore.KVCacheBackend)
	require.NoError(t, cli.Get(ctx, ctrlcli.ObjectKey{Name: kvcb.Name}, live))
	live.Spec.Connection.Managed.ScaleIn.GracePeriodSeconds = 45
	require.NoError(t, cli.Update(ctx, live))

	require.NotNil(t, reconcileKVCacheBackend(t, cli, kvcb.Name))

	require.NoError(t, cli.Get(ctx, memberObjectKey(kvcb, 0), ds))
	command := ds.Spec.Template.Spec.Containers[0].Lifecycle.PreStop.Exec.Command[2]
	assert.Contains(t, command, `"grace_period_seconds":45`,
		"the hook carries the new grace, not the one it was first written with")
	assert.NotContains(t, command, `"grace_period_seconds":30`)
	assert.Equal(t, int64(105), *ds.Spec.Template.Spec.TerminationGracePeriodSeconds,
		"and the window moved with it")
}

// TestKVCacheBackendScale_IgnoresForeignPods pins that the label selector bounds the blast radius. A
// Pod in the same namespace that this backend does not own must survive.
func TestKVCacheBackendScale_IgnoresForeignPods(t *testing.T) {
	kvcb := newKVCacheBackendObject()
	foreign := &core.Pod{
		ObjectMeta: meta.ObjectMeta{
			Name:        "somebody-elses",
			Namespace:   kuberess.SystemNamespaceName,
			Labels:      map[string]string{"app.kubernetes.io/name": "something-else"},
			Annotations: map[string]string{mooncake.MemberPodSpecHashAnnotation: "stale"},
		},
	}

	cli := newKVCacheBackendClient(kvcb, foreign,
		memberPodOn(t, kvcb, "n7", "stale"))

	require.NotNil(t, reconcileKVCacheBackend(t, cli, kvcb.Name))

	assert.Equal(t, []string{"somebody-elses"}, livingMemberPods(t, cli),
		"an unrelated Pod carrying a stale annotation is not this backend's to restart")
}

// TestKVCacheBackendScale_ASelectorIsNotOwnership is the case the label bound above does NOT cover.
//
// A Pod carrying all three identity labels matches the selector exactly, whoever built it — labels
// are a query and anybody can write them. This is a delete, so matching is not enough: the Pod's own
// controller reference has to name this DaemonSet.
func TestKVCacheBackendScale_ASelectorIsNotOwnership(t *testing.T) {
	kvcb := newKVCacheBackendObject()

	// Identical to a member Pod in every way the selector can see, and controlled by something else.
	impostor := memberPodOn(t, kvcb, "n9", "stale")
	impostor.Name = "impostor"
	impostor.OwnerReferences = []meta.OwnerReference{{
		APIVersion: "apps/v1",
		Kind:       "StatefulSet",
		Name:       "somebody-elses-store",
		Controller: ptr.To(true),
	}}

	cli := newKVCacheBackendClient(kvcb, impostor, memberPodOn(t, kvcb, "n7", "stale"))

	require.NotNil(t, reconcileKVCacheBackend(t, cli, kvcb.Name))

	assert.Equal(t, []string{"impostor"}, livingMemberPods(t, cli),
		"the operator's own stale Pod is restarted and the impostor is left alone, although the "+
			"selector cannot tell them apart")
}

const (
	// These carry the shape a REAL leader reports, recorded from a live cluster rather than assumed.
	// A segment's te_endpoint host is the member's POD IP, because the renderer sets
	// MOONCAKE_LOCAL_HOSTNAME from the downward API's status.podIP — and the name the leader builds
	// appends a port of its own, which is why segment_name and te_endpoint carry different ones.
	//
	// The IPs match what runningMemberPod gives its Pods, so these join through the address key the
	// way a real listing does. A fixture on the node name would exercise only the compatibility
	// path — see segmentsOneOKLegacyNodeName — and quietly leave the live one uncovered.
	segmentsTwoOK = `{"total_segments":2,"segments":[
		{"segment_name":"10.42.0.11:13775","te_endpoint":"10.42.0.11:15380","protocol":"tcp","status":"OK"},
		{"segment_name":"10.42.0.12:13887","te_endpoint":"10.42.0.12:16006","protocol":"rdma","status":"OK"}]}`
	segmentsOneOK = `{"total_segments":1,"segments":[
		{"segment_name":"10.42.0.11:13775","te_endpoint":"10.42.0.11:15380","protocol":"tcp","status":"OK"}]}`
	// The shape a member rendered before the address moved still reports. The join keeps a node-name
	// key for exactly this, and without a fixture nothing would notice its removal.
	segmentsOneOKLegacyNodeName = `{"total_segments":1,"segments":[
		{"segment_name":"n7:13775","te_endpoint":"n7:15380","protocol":"tcp","status":"OK"}]}`
	segmentsDraining = `{"total_segments":1,"segments":[
		{"segment_name":"10.42.0.12:13887","te_endpoint":"10.42.0.12:16006","protocol":"tcp","status":"DRAINING"}]}`
	segmentsUnknownState = `{"total_segments":1,"segments":[
		{"segment_name":"10.42.0.13:13991","te_endpoint":"10.42.0.13:16112","protocol":"tcp","status":"QUIESCING"}]}`
	segmentsEmpty = `{"total_segments":0,"segments":[]}`
	// What a member whose local_hostname was overridden through extraArgs produces: an address
	// where the rendered default would have put a node name. Both have to join.
	segmentsByAddress = `{"total_segments":1,"segments":[
		{"segment_name":"overridden","te_endpoint":"10.42.0.11:15002","protocol":"tcp","status":"OK"}]}`
	// What two member groups that both selected ONE node produce on the RDMA path: each Pod holds
	// the host's network namespace, so both segments carry the node's own address and differ only
	// in the transfer port — which is exactly the part the join strips before looking a Pod up.
	segmentsSharedHost = `{"total_segments":2,"segments":[
		{"segment_name":"10.42.0.11:13720","te_endpoint":"10.42.0.11:15002","protocol":"rdma","status":"OK"},
		{"segment_name":"10.42.0.11:14071","te_endpoint":"10.42.0.11:16566","protocol":"rdma","status":"OK"}]}`
	// The same shared address with ONE segment on it: two members are ready and only one of them
	// mounted. This is the case that separates counting from flagging.
	segmentsSharedHostOne = `{"total_segments":1,"segments":[
		{"segment_name":"10.42.0.11:13720","te_endpoint":"10.42.0.11:15002","protocol":"rdma","status":"OK"}]}`
	// What two TCP groups on ONE node produce: each member has its own pod IP and advertises it, so
	// the two segments carry different addresses even though both Pods answer to the node's name.
	// The shared key exists and no segment uses it.
	segmentsTwoAddressesOneNode = `{"total_segments":2,"segments":[
		{"segment_name":"10.42.0.11:13720","te_endpoint":"10.42.0.11:15002","protocol":"tcp","status":"OK"},
		{"segment_name":"10.42.0.12:14071","te_endpoint":"10.42.0.12:16566","protocol":"tcp","status":"OK"}]}`
)

// memberPodOfGroup is runningMemberPod for a backend with several groups: labels, owner and name all
// come from the group, so two Pods on one node are distinct OBJECTS even when every key the status
// join uses — node name and address — is identical between them.
func memberPodOfGroup(
	t *testing.T, kvcb *workercore.KVCacheBackend, group int, node, ip string,
) *core.Pod {
	t.Helper()

	ds := mooncake.RenderMemberDaemonSet(kvcb, group, "example.com/mooncake:v0")
	labels := make(map[string]string, len(ds.Spec.Selector.MatchLabels))
	for k, v := range ds.Spec.Selector.MatchLabels {
		labels[k] = v
	}

	return &core.Pod{
		ObjectMeta: meta.ObjectMeta{
			Name:      fmt.Sprintf("member-%d-%s", group, node),
			Namespace: kuberess.SystemNamespaceName,
			Labels:    labels,
			Annotations: map[string]string{
				mooncake.MemberPodSpecHashAnnotation: mooncake.MemberPodSpecHash(ds.Spec.Template),
			},
			OwnerReferences: []meta.OwnerReference{{
				APIVersion: "apps/v1",
				Kind:       "DaemonSet",
				Name:       ds.Name,
				Controller: ptr.To(true),
			}},
		},
		Spec: core.PodSpec{NodeName: node},
		Status: core.PodStatus{
			PodIP:      ip,
			Conditions: []core.PodCondition{{Type: core.PodReady, Status: core.ConditionTrue}},
		},
	}
}

// runningMemberPod is a member Pod as the DaemonSet would have left it: on a node, with an address,
// and READY.
//
// The readiness is not decoration. A Pod that has scheduled and has an address is not yet a member
// that mounted anything, and the shortfall comparison counts only ready Pods — so a fixture without
// it would be a Pod no real cluster produces in this state and would quietly stop counting.
func runningMemberPod(
	t *testing.T, kvcb *workercore.KVCacheBackend, node, ip string,
) *core.Pod {
	t.Helper()

	pod := memberPodOn(t, kvcb, node, currentMemberFingerprint(t, kvcb))
	pod.Status.PodIP = ip
	pod.Status.Conditions = []core.PodCondition{
		{Type: core.PodReady, Status: core.ConditionTrue},
	}
	return pod
}

// startingMemberPod is the same Pod before its container came up: scheduled, addressed, not ready.
func startingMemberPod(
	t *testing.T, kvcb *workercore.KVCacheBackend, node, ip string,
) *core.Pod {
	t.Helper()

	pod := memberPodOn(t, kvcb, node, currentMemberFingerprint(t, kvcb))
	pod.Status.PodIP = ip
	pod.Status.Conditions = []core.PodCondition{
		{Type: core.PodReady, Status: core.ConditionFalse, Reason: "ContainersNotReady"},
	}
	return pod
}

func reconcileWithAdminAndPods(
	t *testing.T,
	kvcb *workercore.KVCacheBackend,
	byPath map[string]adminResponse,
	pods ...ctrlcli.Object,
) *workercore.KVCacheBackend {
	t.Helper()

	objs := append([]ctrlcli.Object{kvcb}, pods...)
	cli := newKVCacheBackendClient(objs...)
	r := &KVCacheBackendReconciler{
		Client:    cli,
		AdminHTTP: &http.Client{Transport: &adminRoundTripper{byPath: byPath}},
	}
	_, err := r.Reconcile(context.Background(), ctrlreconcile.Request{
		NamespacedName: ctrlcli.ObjectKey{Name: kvcb.Name},
	})
	require.NoError(t, err)

	got := new(workercore.KVCacheBackend)
	require.NoError(t, cli.Get(context.Background(), ctrlcli.ObjectKey{Name: kvcb.Name}, got))
	return got
}

// TestKVCacheBackendStatus_MembersComeFromTheListing pins that membership is READ. The node name and
// the medium are joined in from the Pod whose address the leader reported.
func TestKVCacheBackendStatus_MembersComeFromTheListing(t *testing.T) {
	kvcb := newKVCacheBackendObject()

	got := reconcileWithAdminAndPods(t, kvcb, map[string]adminResponse{
		"/health":              {body: healthServing},
		"/metrics":             {body: metricsPopulated},
		"/get_segments_detail": {body: segmentsTwoOK},
	},
		runningMemberPod(t, kvcb, "n7", "10.42.0.11"),
		runningMemberPod(t, kvcb, "n8", "10.42.0.12"))

	assert.Equal(t, []workercore.KVCacheBackendMemberStatus{
		{SegmentName: "10.42.0.11:13775", NodeName: "n7", Medium: "DRAM", Protocol: "tcp", State: "OK"},
		{SegmentName: "10.42.0.12:13887", NodeName: "n8", Medium: "DRAM", Protocol: "rdma", State: "OK"},
	}, got.Status.Members)

	assert.Equal(t, KVCacheBackendPhaseReady, got.Status.Phase)
	assert.True(t, KVCacheBackendConditionMembersMounted.IsTrue(got))
}

// TestKVCacheBackendStatus_ProtocolIsTheLeadersAndNotTheRenderers is the case the B-plan correction
// exists for. The object below asks for TCP; the leader reports one segment on rdma. What the data
// plane is ACTUALLY doing is what status carries.
func TestKVCacheBackendStatus_ProtocolIsTheLeadersAndNotTheRenderers(t *testing.T) {
	kvcb := newKVCacheBackendObject()
	kvcb.Spec.Transport.Protocol = "TCP"

	got := reconcileWithAdminAndPods(t, kvcb, map[string]adminResponse{
		"/health":              {body: healthServing},
		"/metrics":             {body: metricsPopulated},
		"/get_segments_detail": {body: segmentsTwoOK},
	},
		runningMemberPod(t, kvcb, "n8", "10.42.0.12"))

	require.Len(t, got.Status.Members, 2)
	assert.Equal(t, "rdma", got.Status.Members[1].Protocol,
		"the renderer asked for tcp; the leader says this segment came up on rdma, and the "+
			"leader is the one that knows")
}

func TestKVCacheBackendStatus_StatePassesThrough(t *testing.T) {
	cases := []struct {
		name    string
		listing string
		want    string
	}{
		{name: "a draining segment is still listed", listing: segmentsDraining, want: "Draining"},
		{
			name:    "a state this version does not know is published verbatim",
			listing: segmentsUnknownState,
			want:    "QUIESCING",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			kvcb := newKVCacheBackendObject()

			got := reconcileWithAdminAndPods(t, kvcb, map[string]adminResponse{
				"/health":              {body: healthServing},
				"/metrics":             {body: metricsPopulated},
				"/get_segments_detail": {body: c.listing},
			})

			require.Len(t, got.Status.Members, 1)
			assert.Equal(t, c.want, got.Status.Members[0].State)
		})
	}
}

// TestKVCacheBackendStatus_JoinsOnEitherKeyTheListingCarries is the regression a cluster run found
// and no fixture could have — now covering both keys, because the renderer's answer moved.
//
// The failure it guards is SILENT: an unjoined segment is published with an empty node and medium,
// which is indistinguishable from the legitimate "no Pod matched" case this API deliberately leaves
// blank. So each case gives its Pod a value for the OTHER key that appears NOWHERE in the listing —
// a join that still succeeds can only have gone through the key under test.
func TestKVCacheBackendStatus_JoinsOnEitherKeyTheListingCarries(t *testing.T) {
	cases := []struct {
		name    string
		listing string
		node    string
		podIP   string
	}{
		{
			// te_endpoint is 10.42.0.12:16006, and this Pod's node name appears in no entry.
			name:    "the pod IP, which is what a member advertises today",
			listing: segmentsDraining,
			node:    "n8",
			podIP:   "10.42.0.12",
		},
		{
			// te_endpoint is n7:15380, and this Pod's address appears in no entry. Removing the
			// node-name key from the join would red exactly this case and nothing else.
			name:    "the node name, from a member rendered before the address moved",
			listing: segmentsOneOKLegacyNodeName,
			node:    "n7",
			podIP:   "10.99.99.99",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			kvcb := newKVCacheBackendObject()

			got := reconcileWithAdminAndPods(t, kvcb, map[string]adminResponse{
				"/health":              {body: healthServing},
				"/metrics":             {body: metricsPopulated},
				"/get_segments_detail": {body: c.listing},
			},
				runningMemberPod(t, kvcb, c.node, c.podIP))

			require.Len(t, got.Status.Members, 1)
			assert.Equal(t, c.node, got.Status.Members[0].NodeName,
				"joined, or the segment publishes a blank node that reads as no Pod matching")
			assert.Equal(t, "DRAM", got.Status.Members[0].Medium)
		})
	}
}

// TestKVCacheBackendStatus_JoinsAnOverriddenHostnameToo pins the other half. local_hostname is
// reachable through extraArgs, so a deployment can make the leader report an address instead — and
// the index carries both keys precisely so that one still joins.
func TestKVCacheBackendStatus_JoinsAnOverriddenHostnameToo(t *testing.T) {
	kvcb := newKVCacheBackendObject()

	got := reconcileWithAdminAndPods(t, kvcb, map[string]adminResponse{
		"/health":              {body: healthServing},
		"/metrics":             {body: metricsPopulated},
		"/get_segments_detail": {body: segmentsByAddress},
	},
		runningMemberPod(t, kvcb, "n7", "10.42.0.11"))

	require.Len(t, got.Status.Members, 1)
	assert.Equal(t, "n7", got.Status.Members[0].NodeName,
		"the listing names an address this time, and the same Pod is behind it")
	assert.Equal(t, "DRAM", got.Status.Members[0].Medium)
}

// TestKVCacheBackendStatus_CountsPodsNotIndexKeys pins that indexing one Pod under two keys did not
// double the number of members this operator believes are running. Getting that wrong reports a
// shortfall against a count nothing has — every fully mounted backend would read as Degraded.
func TestKVCacheBackendStatus_CountsPodsNotIndexKeys(t *testing.T) {
	kvcb := newKVCacheBackendObject()

	got := reconcileWithAdminAndPods(t, kvcb, map[string]adminResponse{
		"/health":              {body: healthServing},
		"/metrics":             {body: metricsPopulated},
		"/get_segments_detail": {body: segmentsTwoOK},
	},
		runningMemberPod(t, kvcb, "n7", "10.42.0.11"),
		runningMemberPod(t, kvcb, "n8", "10.42.0.12"))

	assert.True(t, KVCacheBackendConditionMembersMounted.IsTrue(got),
		"two segments against two Pods is fully mounted, however many keys index them")
	assert.Equal(t, KVCacheBackendPhaseReady, got.Status.Phase)
}

// TestKVCacheBackendStatus_JoinsWhatItCanAndLeavesTheRest pins that an unmatched segment is
// published with the fields the listing DOES carry, and the two it cannot are left empty rather than
// filled with the only group's values — which would be a guess that is right until it is not.
func TestKVCacheBackendStatus_JoinsWhatItCanAndLeavesTheRest(t *testing.T) {
	kvcb := newKVCacheBackendObject()

	got := reconcileWithAdminAndPods(t, kvcb, map[string]adminResponse{
		"/health":              {body: healthServing},
		"/metrics":             {body: metricsPopulated},
		"/get_segments_detail": {body: segmentsDraining},
	})

	require.Len(t, got.Status.Members, 1)
	assert.Equal(t, "10.42.0.12:13887", got.Status.Members[0].SegmentName)
	assert.Empty(t, got.Status.Members[0].NodeName,
		"no pod carries that address, so the node is unknown and says so")
	assert.Empty(t, got.Status.Members[0].Medium)
}

// TestKVCacheBackendStatus_APodTheLeaderDoesNotListIsShort pins the honest rendering: the leader is
// what allocation goes through, so a running Pod it does not know about holds nothing.
func TestKVCacheBackendStatus_APodTheLeaderDoesNotListIsShort(t *testing.T) {
	kvcb := newKVCacheBackendObject()

	got := reconcileWithAdminAndPods(t, kvcb, map[string]adminResponse{
		"/health":              {body: healthServing},
		"/metrics":             {body: metricsPopulated},
		"/get_segments_detail": {body: segmentsDraining},
	},
		runningMemberPod(t, kvcb, "n7", "10.42.0.11"),
		runningMemberPod(t, kvcb, "n8", "10.42.0.12"))

	assert.Len(t, got.Status.Members, 1, "only what the leader lists is a member")
	assert.True(t, KVCacheBackendConditionMembersMounted.IsFalse(got))
	message := KVCacheBackendConditionMembersMounted.GetMessage(got)
	assert.Contains(t, message, "1 of 2 ready member pod(s) match none of them",
		"the shortfall is in the message, so an operator sees the gap rather than inferring it")
	assert.Contains(t, message, "n7",
		"and the pod is NAMED, which is the difference between a number and something to go and look at")
	assert.Equal(t, KVCacheBackendPhaseDegraded, got.Status.Phase,
		"the leader serves, so the backend exists; it just holds less than it should")
}

// TestKVCacheBackendStatus_AShortfallNamesASampleAndNotEveryPod bounds the one part of a condition
// message that grows with the cluster rather than with this operator.
//
// A leader that has lost its listing accounts every member pod as short at once, so the names run
// as long as the DaemonSet does. A condition message is capped at 32768 characters by the schema and
// the API server rejects the WHOLE status write when one exceeds it — which would lose the status on
// exactly the pass that had a shortfall to report. The counts carry the magnitude and are never
// sampled; the names are what gets bounded.
func TestKVCacheBackendStatus_AShortfallNamesASampleAndNotEveryPod(t *testing.T) {
	kvcb := newKVCacheBackendObject()

	// One pod the leader does list, so this is a shortfall rather than an empty listing.
	pods := make([]ctrlcli.Object, 0, 30)
	pods = append(pods, runningMemberPod(t, kvcb, "n11", "10.42.0.11"))
	for i := range 29 {
		pods = append(pods, runningMemberPod(t, kvcb,
			fmt.Sprintf("n%d", 100+i), fmt.Sprintf("10.42.1.%d", i+1)))
	}

	got := reconcileWithAdminAndPods(t, kvcb, map[string]adminResponse{
		"/health":              {body: healthServing},
		"/metrics":             {body: metricsPopulated},
		"/get_segments_detail": {body: segmentsOneOK},
	}, pods...)

	message := KVCacheBackendConditionMembersMounted.GetMessage(got)
	assert.Contains(t, message, "29 of 30 ready member pod(s) match none of them",
		"the counts are the actionable half and they describe all of it")
	assert.Equal(t, 20, strings.Count(message, "member-n"),
		"and the names stop at a sample, whatever the cluster's size")
	assert.Contains(t, message, "and 9 more",
		"what was left out is stated, so the sample does not read as the whole list")
}

// TestKVCacheBackendStatus_AStaleSegmentDoesNotStandInForAMissingMember is why the shortfall is a
// set difference and not a difference of counts.
//
// One ready Pod the leader does not list, plus one listed segment whose Pod is already gone, is one
// against one. Comparing cardinalities called that Mounted — a backend that has lost a member,
// reported healthy, because the arithmetic balanced.
func TestKVCacheBackendStatus_AStaleSegmentDoesNotStandInForAMissingMember(t *testing.T) {
	kvcb := newKVCacheBackendObject()

	// The listing's only segment sits on n8, and n8 has no Pod here — it is the member that left.
	// The one Pod that IS running, on n7, appears nowhere in the listing.
	got := reconcileWithAdminAndPods(t, kvcb, map[string]adminResponse{
		"/health":              {body: healthServing},
		"/metrics":             {body: metricsPopulated},
		"/get_segments_detail": {body: segmentsDraining},
	},
		runningMemberPod(t, kvcb, "n7", "10.42.0.11"))

	require.Len(t, got.Status.Members, 1, "the stale segment is still published, as read")
	assert.True(t, KVCacheBackendConditionMembersMounted.IsFalse(got),
		"one segment against one ready pod, and neither is the other")
	assert.Contains(t, KVCacheBackendConditionMembersMounted.GetMessage(got),
		"1 of 1 ready member pod(s) match none of them")
	assert.Equal(t, KVCacheBackendPhaseDegraded, got.Status.Phase)
}

// twoGroupsOnOneNode is a backend whose two groups both landed on n7, holding the host's network
// namespace and therefore one address. Every key the status join indexes by — node name and pod IP —
// is shared between them.
func twoGroupsOnOneNode(t *testing.T, segments string) *workercore.KVCacheBackend {
	t.Helper()

	kvcb := twoGroupBackend(t)

	return reconcileTwoGroups(t, kvcb, segments,
		memberPodOfGroup(t, kvcb, 0, "n7", "10.42.0.11"),
		memberPodOfGroup(t, kvcb, 1, "n7", "10.42.0.11"))
}

// twoGroupBackend is the two-group spec on its own, for the cases that turn on what the member Pods
// look like rather than on the listing and therefore build their own.
func twoGroupBackend(t *testing.T) *workercore.KVCacheBackend {
	t.Helper()

	kvcb := newKVCacheBackendObject()
	second := *kvcb.Spec.Connection.Managed.Members[0].DeepCopy()
	second.NodeSelector = map[string]string{"kvcache-dram-cold": "true"}
	kvcb.Spec.Connection.Managed.Members = append(kvcb.Spec.Connection.Managed.Members, second)

	return kvcb
}

// reconcileTwoGroups runs one reconcile against a serving leader with the given listing and Pods.
func reconcileTwoGroups(
	t *testing.T, kvcb *workercore.KVCacheBackend, segments string, pods ...ctrlcli.Object,
) *workercore.KVCacheBackend {
	t.Helper()

	return reconcileWithAdminAndPods(t, kvcb, map[string]adminResponse{
		"/health":              {body: healthServing},
		"/metrics":             {body: metricsPopulated},
		"/get_segments_detail": {body: segments},
	}, pods...)
}

// TestKVCacheBackendStatus_SharedIdentityIsReportedNotGuessed pins what the status does when two
// ready member Pods answer to one key — and what it must NOT do.
//
// The identity is unrecoverable, and that is a property of the data: the leader reports a segment by
// address, both of the fields it offers carry a transfer port bound at random, and no Pod carries
// that port. Two Pods behind one host are indistinguishable in every observable field.
//
// Three earlier versions of this code assigned anyway — credit the surviving Pod, credit the whole
// set, credit by multiplicity — and each had a defect the next review found. The assertions below
// are written against the ABSENCE of an assignment rather than against the condition being False,
// so that restoring any of those three (or adding a "credit at least one" fallback to make the
// status look better) turns this red rather than passing on a nicer-looking guess.
func TestKVCacheBackendStatus_SharedIdentityIsReportedNotGuessed(t *testing.T) {
	got := twoGroupsOnOneNode(t, segmentsSharedHost)

	assert.Equal(t, "AmbiguousMemberIdentity",
		KVCacheBackendConditionMembersMounted.GetReason(got))

	message := KVCacheBackendConditionMembersMounted.GetMessage(got)
	assert.Contains(t, message, `answer to "10.42.0.11"`,
		"the message names the key, because the operator's next step is to stop the groups sharing it")
	assert.Contains(t, message, "member-0-n7, member-1-n7",
		"and names the pods, so which groups collided is not left to be worked out")
	assert.Contains(t, message, "node selectors",
		"an operator reading this needs the action, not only the diagnosis")

	// The heart of it: nothing was guessed. A segment on an ambiguous key is published as read, with
	// no node and no medium attached, rather than attributed to whichever Pod a map happened to keep.
	require.Len(t, got.Status.Members, 2)
	for _, member := range got.Status.Members {
		assert.Empty(t, member.NodeName,
			"segment %s must carry no node: which pod produced it cannot be known", member.SegmentName)
		assert.Empty(t, member.Medium,
			"segment %s must carry no medium, for the same reason", member.SegmentName)
	}

	assert.NotContains(t, message, "match none of them",
		"and it is not reported as a shortfall: the pods are unaccounted for by construction here, "+
			"so a count of them would describe this index rather than the cluster")
}

// TestKVCacheBackendStatus_SharedIdentityIsNotReportedHealthy is the other direction, and it is the
// one that matters more.
//
// A permanent false shortfall is noisy; a missing member reported as healthy is silent. Two ready
// members with ONE segment between them is exactly that case: an implementation that credited the
// whole collision set on a single match would read fully mounted here.
func TestKVCacheBackendStatus_SharedIdentityIsNotReportedHealthy(t *testing.T) {
	got := twoGroupsOnOneNode(t, segmentsSharedHostOne)

	assert.False(t, KVCacheBackendConditionMembersMounted.IsTrue(got),
		"one segment behind a shared key says nothing about the second member, and 'nothing known' "+
			"must never render as Mounted")
	assert.Equal(t, "AmbiguousMemberIdentity",
		KVCacheBackendConditionMembersMounted.GetReason(got))
	assert.Equal(t, KVCacheBackendPhaseDegraded, got.Status.Phase)
}

// TestKVCacheBackendStatus_ASharedKeyNoSegmentUsesIsNotAmbiguous is the noisy direction of the
// ambiguity rule, and it is the common configuration rather than an edge case.
//
// Two TCP groups on one node share that node's NAME, because both Pods are scheduled there — but a
// TCP member advertises its own pod IP, so the leader reports each segment under a distinct key and
// every one of them resolves. The shared key is real and no segment ever arrives on it.
//
// Judging the index instead of the listing marks this healthy backend Degraded and tells the operator
// to split two groups that never collided. Since `Auto` resolves to TCP, this is what most backends
// with two groups on a node look like.
func TestKVCacheBackendStatus_ASharedKeyNoSegmentUsesIsNotAmbiguous(t *testing.T) {
	kvcb := twoGroupBackend(t)
	got := reconcileTwoGroups(t, kvcb, segmentsTwoAddressesOneNode,
		memberPodOfGroup(t, kvcb, 0, "n7", "10.42.0.11"),
		memberPodOfGroup(t, kvcb, 1, "n7", "10.42.0.12"))

	assert.True(t, KVCacheBackendConditionMembersMounted.IsTrue(got),
		"both segments resolve by address, so nothing about this backend is ambiguous: %s",
		KVCacheBackendConditionMembersMounted.GetMessage(got))
	assert.Equal(t, KVCacheBackendPhaseReady, got.Status.Phase)

	require.Len(t, got.Status.Members, 2)
	for _, member := range got.Status.Members {
		assert.Equal(t, "n7", member.NodeName,
			"segment %s resolves to exactly one ready pod and must carry its node", member.SegmentName)
	}
}

// TestKVCacheBackendStatus_APodIndexedTwiceDoesNotEraseACollision covers a cluster whose nodes are
// named by their addresses, which is legal and is what --hostname-override and several managed
// platforms produce.
//
// Such a Pod is filed under one key TWICE, since its node name and its address are the same string.
// The second filing must not displace what the first left there: with two groups on that node, the
// entry it would overwrite is the one recording that they collide, and erasing it turns a reported
// ambiguity back into a silent guess.
//
// The count in the message is the assertion that matters — three filings for two Pods must still
// describe two.
func TestKVCacheBackendStatus_APodIndexedTwiceDoesNotEraseACollision(t *testing.T) {
	kvcb := twoGroupBackend(t)
	got := reconcileTwoGroups(t, kvcb, segmentsSharedHost,
		memberPodOfGroup(t, kvcb, 0, "10.42.0.11", "10.42.0.11"),
		memberPodOfGroup(t, kvcb, 1, "10.42.0.11", "10.42.0.11"))

	assert.Equal(t, "AmbiguousMemberIdentity",
		KVCacheBackendConditionMembersMounted.GetReason(got),
		"two ready pods answer to this key; a pod filed under it twice is still one pod")

	message := KVCacheBackendConditionMembersMounted.GetMessage(got)
	assert.Contains(t, message, "2 ready member pod(s)",
		"three filings for two pods must not read as three members")
	assert.Contains(t, message, "member-0-10.42.0.11, member-1-10.42.0.11")
}

// TestKVCacheBackendStatus_AnUnreadyPodDoesNotTakeAReadyOnesSegment pins which Pod a segment resolves
// to when a key is shared by one ready member and one that is not.
//
// This is not an ambiguity: only a ready member can hold a segment, so the ready one is the only
// candidate. Reading whichever Pod was filed last instead credits the unready one, and the ready
// member — the only one that could have produced the segment — is then reported as a shortfall on a
// backend that is fully mounted. The Pods are ordered so the unready one is filed LAST, which is the
// arrangement that makes the difference observable.
func TestKVCacheBackendStatus_AnUnreadyPodDoesNotTakeAReadyOnesSegment(t *testing.T) {
	kvcb := twoGroupBackend(t)
	starting := memberPodOfGroup(t, kvcb, 1, "n7", "10.42.0.11")
	starting.Status.Conditions = []core.PodCondition{
		{Type: core.PodReady, Status: core.ConditionFalse, Reason: "ContainersNotReady"},
	}

	got := reconcileTwoGroups(t, kvcb, segmentsSharedHostOne,
		memberPodOfGroup(t, kvcb, 0, "n7", "10.42.0.11"),
		starting)

	assert.True(t, KVCacheBackendConditionMembersMounted.IsTrue(got),
		"the one ready member holds the one segment, so nothing is short: %s",
		KVCacheBackendConditionMembersMounted.GetMessage(got))

	require.Len(t, got.Status.Members, 1)
	assert.Equal(t, "n7", got.Status.Members[0].NodeName)
}

// TestKVCacheBackendStatus_TheAmbiguityMessageIsBounded pins the one property that decides whether
// this condition can be published at all.
//
// A two-group RDMA backend produces one ambiguous key PER NODE, so the size of this message answers to
// the cluster rather than to anything in this package. `Condition.message` is capped at 32768
// characters by the schema, and past it every status write is rejected: the reconcile then retries
// forever without ever publishing the ambiguity it was reporting. A fault report that grows with the
// fault fails exactly when it is needed.
//
// Both axes are asserted, because bounding one and not the other still grows without limit.
func TestKVCacheBackendStatus_TheAmbiguityMessageIsBounded(t *testing.T) {
	// The schema's own limit on meta.Condition.message. Named here rather than compared to a
	// constant in this package, because the bound this test defends is the API's, not ours.
	const conditionMessageMax = 32768

	t.Run("many shared keys", func(t *testing.T) {
		ambiguous := map[string][]string{}
		for i := range 1000 {
			key := fmt.Sprintf("10.42.%d.%d", i/256, i%256)
			ambiguous[key] = []string{
				fmt.Sprintf("member-0-node-%d", i), fmt.Sprintf("member-1-node-%d", i),
			}
		}

		message := describeAmbiguousKeys(ambiguous)
		assert.Less(t, len(message), conditionMessageMax,
			"a thousand-node backend must still render a message the api server accepts")
		assert.Contains(t, message, "and 980 more shared key(s)",
			"the count of what was left out is the actionable half; a truncated list without it "+
				"reads like the whole answer")
	})

	t.Run("many pods on one key", func(t *testing.T) {
		sharing := make([]string, 0, 500)
		for i := range 500 {
			sharing = append(sharing, fmt.Sprintf("member-%d-n7", i))
		}

		message := describeAmbiguousKeys(map[string][]string{"10.42.0.11": sharing})
		assert.Less(t, len(message), conditionMessageMax)
		assert.Contains(t, message, "and 480 more",
			"the names within one key are bounded by the same helper the shortfall uses")
		assert.Contains(t, message, "500 ready member pod(s)",
			"and the COUNT is not truncated with the list: it is what says how bad this is")
	})
}

// crashingMemberPod is a member whose image lacks a library its binary links against: it started,
// the loader failed it, and the kubelet is backing off. The loader's own words are in the last
// TERMINATION, which is what the rendered FallbackToLogsOnError policy puts there; the waiting state
// beside it carries only the kubelet's backoff boilerplate.
func crashingMemberPod(
	t *testing.T, kvcb *workercore.KVCacheBackend, node string,
) *core.Pod {
	t.Helper()

	pod := memberPodOn(t, kvcb, node, currentMemberFingerprint(t, kvcb))
	pod.Status.Conditions = []core.PodCondition{
		{Type: core.PodReady, Status: core.ConditionFalse, Reason: "ContainersNotReady"},
	}
	pod.Status.ContainerStatuses = []core.ContainerStatus{{
		Name:         "member",
		RestartCount: 3,
		State: core.ContainerState{
			Waiting: &core.ContainerStateWaiting{
				Reason:  "CrashLoopBackOff",
				Message: "back-off 5m0s restarting failed container=member pod=member-" + node,
			},
		},
		LastTerminationState: core.ContainerState{
			Terminated: &core.ContainerStateTerminated{
				ExitCode: 127,
				Reason:   "Error",
				Message: "mc_store_rest_server: error while loading shared libraries: " +
					"libascendcl.so: cannot open shared object file",
			},
		},
	}}
	return pod
}

// unschedulableMemberPod is a member no node will take — a taint, a node that went away, a request
// nothing can satisfy. It never becomes ready and never stops trying.
func unschedulableMemberPod(
	t *testing.T, kvcb *workercore.KVCacheBackend, node string,
) *core.Pod {
	t.Helper()

	pod := memberPodOn(t, kvcb, node, currentMemberFingerprint(t, kvcb))
	pod.Spec.NodeName = ""
	pod.Status.Conditions = []core.PodCondition{{
		Type:    core.PodScheduled,
		Status:  core.ConditionFalse,
		Reason:  core.PodReasonUnschedulable,
		Message: "0/3 nodes are available: 1 Insufficient memory.",
	}}
	return pod
}

// TestKVCacheBackendStatus_AStuckMemberIsAShortfallEvenWithSegments is the other side of
// TestKVCacheBackendStatus_AStartingPodIsNotAShortfall, and the pair is the contract: a member on
// its way is not a shortfall, a member that has stopped is one.
//
// Without the second half a group that has lost a node reads exactly like a healthy one. Only ready
// Pods are held to the leader's listing, so a member that never became ready produces no shortfall,
// and the remaining segment is enough for Mounted — a backend at half its capacity reporting Ready,
// with nothing anywhere saying otherwise.
func TestKVCacheBackendStatus_AStuckMemberIsAShortfallEvenWithSegments(t *testing.T) {
	kvcb := newKVCacheBackendObject()

	cases := []struct {
		name   string
		pod    *core.Pod
		reason string
		says   string
	}{
		{
			name:   "a container that will not start",
			pod:    crashingMemberPod(t, kvcb, "n8"),
			reason: "MemberCrashLooping",
			says:   "libascendcl.so",
		},
		{
			name:   "a pod no node will take",
			pod:    unschedulableMemberPod(t, kvcb, "n8"),
			reason: core.PodReasonUnschedulable,
			says:   "no node to run on",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			// n7 is ready and holds the one segment the leader lists, so the shortfall finds
			// nothing to say. Everything reported here comes from n8.
			got := reconcileWithAdminAndPods(t, kvcb, map[string]adminResponse{
				"/health":              {body: healthServing},
				"/metrics":             {body: metricsPopulated},
				"/get_segments_detail": {body: segmentsOneOK},
			},
				runningMemberPod(t, kvcb, "n7", "10.42.0.11"),
				c.pod)

			assert.True(t, KVCacheBackendConditionMembersMounted.IsFalse(got),
				"a selected node holding nothing is a shortfall, whatever the other members hold")
			assert.Equal(t, c.reason, KVCacheBackendConditionMembersMounted.GetReason(got))
			assert.Contains(t, got.Status.PhaseMessage, c.says,
				"the message names the pod's own reason, not just that a member is missing")
			assert.Equal(t, KVCacheBackendPhaseDegraded, got.Status.Phase)
		})
	}
}

// TestKVCacheBackendStatus_AListingTooLargeToPublishIsWithheld covers the failure that looks like
// success: the read works, the decode works, and it is the WRITE that cannot happen.
//
// Every entry is republished on every pass, so a listing past what an object can hold does not fail
// once — it makes every status write fail from then on, with nothing in the observation path
// reporting a problem. Truncating instead would be worse than refusing: a silently shortened list is
// indistinguishable from a backend that lost members, which is a condition this API acts on.
func TestKVCacheBackendStatus_AListingTooLargeToPublishIsWithheld(t *testing.T) {
	entries := make([]string, 0, kvCacheBackendMaxMembers+1)
	for i := range kvCacheBackendMaxMembers + 1 {
		entries = append(entries, fmt.Sprintf(
			`{"segment_name":"n%d-dram","te_endpoint":"10.42.%d.%d:15002","protocol":"tcp","status":"OK"}`,
			i, i/256, i%256))
	}
	oversized := fmt.Sprintf(`{"total_segments":%d,"segments":[%s]}`,
		len(entries), strings.Join(entries, ","))

	// A handful of entries whose identifiers are long is the case a count-only guard admits: the
	// admin endpoint chooses these strings, and for an external backend it is somebody else's.
	long := strings.Repeat("s", kvCacheBackendMaxMembersBytes/4)
	heavy := fmt.Sprintf(`{"total_segments":4,"segments":[%s]}`, strings.Join([]string{
		fmt.Sprintf(`{"segment_name":%q,"te_endpoint":"10.42.0.1:1","protocol":"tcp","status":"OK"}`, long+"1"),
		fmt.Sprintf(`{"segment_name":%q,"te_endpoint":"10.42.0.2:1","protocol":"tcp","status":"OK"}`, long+"2"),
		fmt.Sprintf(`{"segment_name":%q,"te_endpoint":"10.42.0.3:1","protocol":"tcp","status":"OK"}`, long+"3"),
		fmt.Sprintf(`{"segment_name":%q,"te_endpoint":"10.42.0.4:1","protocol":"tcp","status":"OK"}`, long+"4"),
	}, ","))

	// Under the budget as bytes in memory, over it as bytes on the wire. JSON escapes `<` as
	// `<`, six bytes for one, and which characters an identifier is made of is the admin
	// endpoint's choice — somebody else's, for an external backend. Sized to sit between the two:
	// 120000 raw against a 524288 budget, 720008 once encoded.
	escaping := strings.Repeat("<", 30000)
	inflating := fmt.Sprintf(`{"total_segments":4,"segments":[%s]}`, strings.Join([]string{
		fmt.Sprintf(`{"segment_name":"%s1","te_endpoint":"10.42.0.1:1","protocol":"tcp","status":"OK"}`, escaping),
		fmt.Sprintf(`{"segment_name":"%s2","te_endpoint":"10.42.0.2:1","protocol":"tcp","status":"OK"}`, escaping),
		fmt.Sprintf(`{"segment_name":"%s3","te_endpoint":"10.42.0.3:1","protocol":"tcp","status":"OK"}`, escaping),
		fmt.Sprintf(`{"segment_name":"%s4","te_endpoint":"10.42.0.4:1","protocol":"tcp","status":"OK"}`, escaping),
	}, ","))
	require.Less(t, 4*len(escaping), kvCacheBackendMaxMembersBytes,
		"the fixture has to be admissible by the byte count it is meant to defeat")

	for _, c := range []struct {
		name string
		body string
	}{
		{"too many entries", oversized},
		{"few entries, too many bytes", heavy},
		{"few entries, within budget until they are encoded", inflating},
	} {
		t.Run(c.name, func(t *testing.T) {
			kvcb := newKVCacheBackendObject()
			got := reconcileWithAdminAndPods(t, kvcb, map[string]adminResponse{
				"/health":              {body: healthServing},
				"/metrics":             {body: metricsPopulated},
				"/get_segments_detail": {body: c.body},
			})

			assert.True(t, KVCacheBackendConditionMembersMounted.IsFalse(got))
			assert.Equal(t, "ListingTooLarge", KVCacheBackendConditionMembersMounted.GetReason(got))
			assert.Empty(t, got.Status.Members,
				"nothing is published, rather than a prefix that would read as a shrunken backend")
			assert.Equal(t, KVCacheBackendPhaseDegraded, got.Status.Phase)
		})
	}
}

// disown keeps a Pod's identity labels and takes away its membership: it becomes a Pod that LOOKS
// like this group's and is somebody else's.
func disown(pod *core.Pod) *core.Pod {
	pod.OwnerReferences = []meta.OwnerReference{{
		APIVersion: "apps/v1",
		Kind:       "DaemonSet",
		Name:       "somebody-elses-daemonset",
		Controller: ptr.To(true),
	}}
	return pod
}

// TestKVCacheBackendStatus_AStrangerPodIsNotAMember is the exact dual of the rows above — the same
// Pods, disowned — and the member-side counterpart of the leader's ownership check.
//
// The three identity labels are DERIVED from the backend's name, so anything can carry them: a
// neighboring controller, a hand-applied manifest, a leftover from a same-named backend that was
// deleted. Each use of a member Pod publishes a decision, and on the selector alone a stranger can
// invent a shortfall, put a fault on a healthy backend, or join a segment to the wrong node.
func TestKVCacheBackendStatus_AStrangerPodIsNotAMember(t *testing.T) {
	kvcb := newKVCacheBackendObject()

	cases := []struct {
		name string
		pod  *core.Pod
	}{
		{
			name: "a ready one, which would be counted and invent a shortfall",
			pod:  disown(runningMemberPod(t, kvcb, "n8", "10.42.0.12")),
		},
		{
			name: "a crash-looping one, which would publish its reason as this backend's",
			pod:  disown(crashingMemberPod(t, kvcb, "n8")),
		},
		{
			name: "one no node will take",
			pod:  disown(unschedulableMemberPod(t, kvcb, "n8")),
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			// Same fixture as the shortfall rows: n7 is ours, ready, and holds the one segment the
			// leader lists. Everything that could go wrong here comes from the disowned n8.
			got := reconcileWithAdminAndPods(t, kvcb, map[string]adminResponse{
				"/health":              {body: healthServing},
				"/metrics":             {body: metricsPopulated},
				"/get_segments_detail": {body: segmentsOneOK},
			},
				runningMemberPod(t, kvcb, "n7", "10.42.0.11"),
				c.pod)

			assert.True(t, KVCacheBackendConditionMembersMounted.IsTrue(got),
				"a pod this backend does not own says nothing about it, in any state")
			assert.Equal(t, KVCacheBackendPhaseReady, got.Status.Phase)
		})
	}
}

// TestKVCacheBackendStatus_AMemberContainerFaultReachesThePhaseMessage pins the promise this API
// makes about transports: a member needs its transport's vendor runtime inside the image, the
// webhook cannot see inside an image, so the failure belongs to runtime and runtime has to name it.
//
// Without this the whole report is "the leader lists no segment, with 0 member pod(s) running" — a
// member that never becomes ready is not in that count, because only a ready Pod can be held to the
// leader's listing. The container is the only thing that knows why.
func TestKVCacheBackendStatus_AMemberContainerFaultReachesThePhaseMessage(t *testing.T) {
	kvcb := newKVCacheBackendObject()

	got := reconcileWithAdminAndPods(t, kvcb, map[string]adminResponse{
		"/health":              {body: healthServing},
		"/metrics":             {body: metricsPopulated},
		"/get_segments_detail": {body: segmentsEmpty},
	},
		crashingMemberPod(t, kvcb, "n7"))

	assert.True(t, KVCacheBackendConditionMembersMounted.IsFalse(got))
	assert.Equal(t, "MemberCrashLooping", KVCacheBackendConditionMembersMounted.GetReason(got),
		"the reason names the fault, not the absence it produced")

	// The loader error itself, not the backoff boilerplate that shares the Pod with it.
	assert.Contains(t, got.Status.PhaseMessage, "libascendcl.so",
		"the container's own message is what makes this diagnosable")
	assert.Contains(t, got.Status.PhaseMessage, "exited with code 127")
	assert.NotContains(t, got.Status.PhaseMessage, "back-off",
		"the waiting state's boilerplate says nothing and must not win over the termination")
	assert.Equal(t, KVCacheBackendPhaseDegraded, got.Status.Phase)
}

// TestKVCacheBackendStatus_AFaultMessageIsBoundedBeforeItIsPublished pins the other half of the
// condition-message limit: a string this operator did not write.
//
// The kubelet's and the scheduler's messages are passed through because they are the actionable part
// of a fault, and their length answers to the cluster rather than to anything here — a container
// writes up to its termination-log limit, a scheduler enumerates what it tried. One of them is
// enough to push a message past the schema's 32768 characters, and the API server then rejects the
// whole status write.
func TestKVCacheBackendStatus_AFaultMessageIsBoundedBeforeItIsPublished(t *testing.T) {
	kvcb := newKVCacheBackendObject()

	pod := crashingMemberPod(t, kvcb, "n7")
	pod.Status.ContainerStatuses[0].LastTerminationState.Terminated.Message = "panic: " + strings.Repeat("goroutine stack ", 4000)

	got := reconcileWithAdminAndPods(t, kvcb, map[string]adminResponse{
		"/health":              {body: healthServing},
		"/metrics":             {body: metricsPopulated},
		"/get_segments_detail": {body: segmentsEmpty},
	}, pod)

	message := KVCacheBackendConditionMembersMounted.GetMessage(got)
	assert.Less(t, utf8.RuneCountInString(message), 32768,
		"a message over the schema's limit is not truncated by the API server, it is refused")
	assert.Contains(t, message, "panic: goroutine stack",
		"and the start of it survives, which is the part that says what happened")
	assert.Contains(t, message, "truncated",
		"the cut is stated, so nobody reads the tail as the end of the output")
}

// TestKVCacheBackendStatus_AnOversizedHealthFieldIsBounded covers the same limit reached through the
// admin surface rather than through Kubernetes.
//
// `role` and `ha_state` are quoted back because they are what an operator reads to tell a starting
// leader from a wedged one, and they arrive as JSON strings off an address the spec named. The
// response as a whole is bounded at 8 MiB and nothing says how that is spread across its fields.
func TestKVCacheBackendStatus_AnOversizedHealthFieldIsBounded(t *testing.T) {
	huge := strings.Repeat("l", 1<<20)

	cases := []struct {
		name         string
		serviceReady bool
	}{
		{name: "a leader that is up but not serving names both fields", serviceReady: false},
		{name: "a serving leader names its role", serviceReady: true},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := reconcileWithAdmin(t, newKVCacheBackendObject(), map[string]adminResponse{
				"/health": {body: fmt.Sprintf(
					`{"status":"ok","role":%q,"ha_state":%q,"service_ready":%t}`,
					huge, huge, c.serviceReady)},
				"/metrics":             {body: metricsPopulated},
				"/get_segments_detail": {body: segmentsEmpty},
			})

			message := KVCacheBackendConditionLeaderAvailable.GetMessage(got)
			assert.Less(t, utf8.RuneCountInString(message), 32768,
				"an over-limit message is refused outright, taking the whole status write with it")
			assert.Contains(t, message, "(truncated)")
		})
	}
}

// TestKVCacheBackendStatus_ARefusalNamesASampleOfItsConsumers bounds the claimant list.
//
// usedBy has no item bound — it is written by whoever consumes the backend — and it is joined into
// the message that carries Deletable=False and the Deleting phase. Unbounded, a backend with enough
// consumers would have its delete refused by a status write that could not be persisted, so the
// object would refuse and not say why.
func TestKVCacheBackendStatus_ARefusalNamesASampleOfItsConsumers(t *testing.T) {
	refs := make([]workercore.KVCacheObjectReference, 0, 30)
	for i := range 30 {
		refs = append(refs, workercore.KVCacheObjectReference{
			Kind: "KVCachePool",
			Name: fmt.Sprintf("pool-%d", i),
		})
	}

	kvcb := newKVCacheBackendObject(refs...)
	systemmeta.Lock(kvcb)
	now := meta.Now()
	kvcb.DeletionTimestamp = &now

	cli := newKVCacheBackendClient(append([]ctrlcli.Object{kvcb}, kvCachePoolsNamedBy(refs...)...)...)
	got := reconcileKVCacheBackend(t, cli, kvcb.Name)
	require.NotNil(t, got, "a claimed backend must not be released")

	message := KVCacheBackendConditionDeletable.GetMessage(got)
	assert.Equal(t, 20, strings.Count(message, "KVCachePool/"),
		"the names stop at a sample, whatever the number of consumers")
	assert.Contains(t, message, "and 10 more",
		"and what was left out is stated rather than silently dropped")
	assert.Equal(t, KVCacheBackendPhaseDeleting, got.Status.Phase)
}

// TestKVCacheBackendStatus_KeepsTheLastListingOnAFailedRead pins the difference from capacity, and
// the reason is in the types: capacity has an absent that means "not observed", a list does not —
// an empty list is a legible value meaning "no segments", so clearing it would publish a falsehood.
func TestKVCacheBackendStatus_KeepsTheLastListingOnAFailedRead(t *testing.T) {
	kvcb := newKVCacheBackendObject()
	cli := newKVCacheBackendClient(kvcb, runningMemberPod(t, kvcb, "n7", "10.42.0.11"))
	ctx := context.Background()

	rt := &adminRoundTripper{byPath: map[string]adminResponse{
		"/health":              {body: healthServing},
		"/metrics":             {body: metricsPopulated},
		"/get_segments_detail": {body: segmentsTwoOK},
	}}
	r := &KVCacheBackendReconciler{
		Client:    cli,
		AdminHTTP: &http.Client{Transport: rt},
	}
	reconcile := func() *workercore.KVCacheBackend {
		_, err := r.Reconcile(ctx, ctrlreconcile.Request{
			NamespacedName: ctrlcli.ObjectKey{Name: kvcb.Name},
		})
		require.NoError(t, err)
		got := new(workercore.KVCacheBackend)
		require.NoError(t, cli.Get(ctx, ctrlcli.ObjectKey{Name: kvcb.Name}, got))
		return got
	}

	observed := reconcile()
	require.Len(t, observed.Status.Members, 2, "the first pass must actually observe")

	// The listing route starts refusing, while the leader keeps answering /health.
	rt.byPath["/get_segments_detail"] = adminResponse{
		status: http.StatusServiceUnavailable, body: "service plane is not active",
	}

	after := reconcile()
	assert.Len(t, after.Status.Members, 2,
		"membership stays at its last read: an empty list would claim every member is gone")
	assert.True(t, KVCacheBackendConditionMembersMounted.IsFalse(after))
	assert.Contains(t, KVCacheBackendConditionMembersMounted.GetMessage(after),
		"as of the last successful read",
		"and the condition says the view is stale, which is what makes the stale list readable")
}

// TestKVCacheBackendStatus_Phases walks the five phases over the documents that produce them.
func TestKVCacheBackendStatus_Phases(t *testing.T) {
	cases := []struct {
		name      string
		byPath    map[string]adminResponse
		podReady  bool
		wantPhase string
	}{
		{
			name: "a leader that is still coming up",
			byPath: map[string]adminResponse{
				"/health": {err: errors.New("connect: connection refused")},
			},
			wantPhase: KVCacheBackendPhaseProvisioning,
		},
		{
			name: "a leader answering that it is not serving",
			byPath: map[string]adminResponse{
				"/health": {body: healthNotServing},
			},
			wantPhase: KVCacheBackendPhaseProvisioning,
		},
		{
			name: "a leader serving with nothing mounted",
			byPath: map[string]adminResponse{
				"/health":              {body: healthServing},
				"/metrics":             {body: metricsPopulated},
				"/get_segments_detail": {body: segmentsEmpty},
			},
			wantPhase: KVCacheBackendPhaseDegraded,
		},
		{
			name: "a leader serving with a segment",
			byPath: map[string]adminResponse{
				"/health":              {body: healthServing},
				"/metrics":             {body: metricsPopulated},
				"/get_segments_detail": {body: segmentsDraining},
			},
			wantPhase: KVCacheBackendPhaseReady,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := reconcileWithAdminAndPods(t, newKVCacheBackendObject(), c.byPath)
			assert.Equal(t, c.wantPhase, got.Status.Phase)

			if c.wantPhase == KVCacheBackendPhaseReady {
				assert.Empty(t, got.Status.PhaseMessage,
					"a Ready backend carries no phase message: there is nothing to explain")
			} else {
				assert.NotEmpty(t, got.Status.PhaseMessage,
					"every phase short of Ready must say why, or an operator has nowhere to start")
			}
		})
	}
}

// TestKVCacheBackendStatus_ServiceReadyFalseIsNeverReady is asserted on its own because it is the
// one rule that cannot be weakened. The leader's /health says ok — a constant — while service_ready
// says otherwise, and a reader that took the constant for a verdict would publish Ready over a
// leader that serves nothing.
func TestKVCacheBackendStatus_ServiceReadyFalseIsNeverReady(t *testing.T) {
	got := reconcileWithAdminAndPods(t, newKVCacheBackendObject(), map[string]adminResponse{
		"/health":              {body: healthNotServing},
		"/metrics":             {body: metricsPopulated},
		"/get_segments_detail": {body: segmentsTwoOK},
	})

	assert.NotEqual(t, KVCacheBackendPhaseReady, got.Status.Phase)
	assert.Empty(t, got.Status.Members,
		"a leader that is not serving is not asked for its listing, so nothing is published from one")
	assert.Nil(t, got.Status.Capacity)
}

// TestKVCacheBackendStatus_IsIdempotent pins that a settled, fully observed backend writes nothing on
// a second pass — including the member list, whose order the leader does not promise.
func TestKVCacheBackendStatus_IsIdempotent(t *testing.T) {
	kvcb := newKVCacheBackendObject()
	cli := newKVCacheBackendClient(kvcb,
		runningMemberPod(t, kvcb, "n7", "10.42.0.11"),
		runningMemberPod(t, kvcb, "n8", "10.42.0.12"))
	ctx := context.Background()

	r := &KVCacheBackendReconciler{
		Client: cli,
		AdminHTTP: &http.Client{Transport: &adminRoundTripper{byPath: map[string]adminResponse{
			"/health":              {body: healthServing},
			"/metrics":             {body: metricsPopulated},
			"/get_segments_detail": {body: segmentsTwoOK},
		}}},
	}
	reconcile := func() *workercore.KVCacheBackend {
		_, err := r.Reconcile(ctx, ctrlreconcile.Request{
			NamespacedName: ctrlcli.ObjectKey{Name: kvcb.Name},
		})
		require.NoError(t, err)
		got := new(workercore.KVCacheBackend)
		require.NoError(t, cli.Get(ctx, ctrlcli.ObjectKey{Name: kvcb.Name}, got))
		return got
	}

	first := reconcile()
	require.Equal(t, KVCacheBackendPhaseReady, first.Status.Phase,
		"the first pass must reach a fully observed state, or this proves nothing")

	second := reconcile()
	assert.Equal(t, first.ResourceVersion, second.ResourceVersion,
		"a fully observed backend produces no status write on the next pass")
}

// externalAdminAddress is the address the fixtures below name, and every external case asserts the
// reads went here. A managed backend's address is a Service name this operator renders; this one
// resolves to nothing in the cluster, which is the point.
const externalAdminAddress = "mooncake.corp.example:9003"

// newExternalKVCacheBackendObject builds a backend somebody else runs.
//
// It carries an image because admission requires one of every object — the field is validated before
// the connection branch is looked at — and this mode never reads it. That is a fair thing for a test
// to encode: the fixture is the object an admin can actually create.
func newExternalKVCacheBackendObject(
	usedBy ...workercore.KVCacheObjectReference,
) *workercore.KVCacheBackend {
	kvcb := &workercore.KVCacheBackend{
		ObjectMeta: meta.ObjectMeta{Name: "mooncake-shared"},
		Spec: workercore.KVCacheBackendSpec{
			Type:  "Mooncake",
			Image: "example.com/mooncake:v0",
			Connection: workercore.KVCacheBackendConnection{
				External: &workercore.KVCacheBackendExternal{
					Endpoints: []workercore.KVCacheBackendEndpoint{
						{
							Name:    workercore.KVCacheBackendEndpointNameClient,
							Address: "mooncake.corp.example:50051",
						},
						{
							Name:    workercore.KVCacheBackendEndpointNameAdmin,
							Address: externalAdminAddress,
						},
					},
				},
			},
		},
	}
	kvcb.Status.UsedBy = usedBy
	return kvcb
}

// newClientRefusingCreates is the assertion behind "the external mode renders nothing", and it is
// written as a client rather than as a lookup afterwards on purpose: a test that goes looking for a
// Deployment and finds none cannot tell a reconciler that skipped the render from one that rendered
// under a name or a namespace the test did not think to check. A create that never happens is the
// claim, so a create is what fails.
func newClientRefusingCreates(t *testing.T, objs ...ctrlcli.Object) ctrlcli.Client {
	t.Helper()

	return ctrlfake.NewClientBuilder().
		WithScheme(scheme.Scheme).
		WithStatusSubresource(&workercore.KVCacheBackend{}).
		WithObjects(objs...).
		WithInterceptorFuncs(ctrlinterceptor.Funcs{
			Create: func(
				_ context.Context, _ ctrlcli.WithWatch, obj ctrlcli.Object, _ ...ctrlcli.CreateOption,
			) error {
				err := fmt.Errorf("an external backend created a %T named %q", obj, obj.GetName())
				t.Error(err)
				return err
			},
		}).
		Build()
}

// reconcileExternal runs one pass over a backend somebody else runs, against a client that refuses
// every create. It hands back the transport as well as the object, so a case can assert WHERE the
// operator went and not only what it concluded.
func reconcileExternal(
	t *testing.T, kvcb *workercore.KVCacheBackend, byPath map[string]adminResponse,
) (*workercore.KVCacheBackend, *adminRoundTripper) {
	t.Helper()

	rt := &adminRoundTripper{byPath: byPath}
	cli := newClientRefusingCreates(t, kvcb)
	r := &KVCacheBackendReconciler{
		Client:    cli,
		AdminHTTP: &http.Client{Transport: rt},
	}
	_, err := r.Reconcile(context.Background(), ctrlreconcile.Request{
		NamespacedName: ctrlcli.ObjectKey{Name: kvcb.Name},
	})
	require.NoError(t, err)

	got := new(workercore.KVCacheBackend)
	if err := cli.Get(context.Background(), ctrlcli.ObjectKey{Name: kvcb.Name}, got); err != nil {
		return nil, rt
	}
	return got, rt
}

// TestKVCacheBackendExternal_RendersNothingAndReadsTheSpecsAddress pins the two halves of the mode:
// nothing is created, and the reads go to the address the spec named rather than to a Service this
// operator would have rendered for a managed backend.
func TestKVCacheBackendExternal_RendersNothingAndReadsTheSpecsAddress(t *testing.T) {
	kvcb := newExternalKVCacheBackendObject()

	got, rt := reconcileExternal(t, kvcb, map[string]adminResponse{
		"/health":              {body: healthServing},
		"/metrics":             {body: metricsPopulated},
		"/get_segments_detail": {body: segmentsTwoOK},
	})
	require.NotNil(t, got)

	assert.Equal(t, kvcb.Spec.Connection.External.Endpoints, got.Status.Endpoints,
		"status publishes the addresses the spec named, unchanged")

	require.NotEmpty(t, rt.hosts, "the backend must be read, not assumed healthy")
	for _, host := range rt.hosts {
		assert.Equal(t, externalAdminAddress, host,
			"every read goes to the spec's admin address")
	}
	assert.ElementsMatch(t, []string{"/health", "/metrics", "/get_segments_detail"}, rt.asked,
		"the same three routes a managed backend is read through")
}

// TestKVCacheBackendExternal_ObservesTheSameWayAManagedOneDoes pins that the observation path is
// shared. The members come from the leader's listing, and the two fields the listing cannot supply
// stay EMPTY: the Pods behind an external backend are somebody else's, so there is nothing to join.
func TestKVCacheBackendExternal_ObservesTheSameWayAManagedOneDoes(t *testing.T) {
	got, _ := reconcileExternal(t, newExternalKVCacheBackendObject(), map[string]adminResponse{
		"/health":              {body: healthServing},
		"/metrics":             {body: metricsPopulated},
		"/get_segments_detail": {body: segmentsTwoOK},
	})
	require.NotNil(t, got)

	assert.Equal(t, []workercore.KVCacheBackendMemberStatus{
		{SegmentName: "10.42.0.11:13775", Protocol: "tcp", State: "OK"},
		{SegmentName: "10.42.0.12:13887", Protocol: "rdma", State: "OK"},
	}, got.Status.Members, "no node and no medium are guessed at for members this operator does not run")

	assert.Equal(t, KVCacheBackendPhaseReady, got.Status.Phase)
	assert.True(t, KVCacheBackendConditionMembersMounted.IsTrue(got))
}

// TestKVCacheBackendExternal_ReadyOnlyWhenTheServicePlaneIsActive pins the gate on the external side.
// The /health status field is a constant, so an external backend that answers is not thereby usable.
func TestKVCacheBackendExternal_ReadyOnlyWhenTheServicePlaneIsActive(t *testing.T) {
	cases := []struct {
		name      string
		health    string
		wantPhase string
		wantReady bool
	}{
		{
			name:      "the backend reports its service plane active",
			health:    healthServing,
			wantPhase: KVCacheBackendPhaseReady,
			wantReady: true,
		},
		{
			name:      "the backend answers and says it is not serving",
			health:    healthNotServing,
			wantPhase: KVCacheBackendPhaseProvisioning,
			wantReady: false,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, _ := reconcileExternal(t, newExternalKVCacheBackendObject(),
				map[string]adminResponse{
					"/health":              {body: c.health},
					"/metrics":             {body: metricsPopulated},
					"/get_segments_detail": {body: segmentsTwoOK},
				})
			require.NotNil(t, got)

			assert.Equal(t, c.wantPhase, got.Status.Phase)
			assert.Equal(t, c.wantReady, KVCacheBackendConditionLeaderAvailable.IsTrue(got))
		})
	}
}

// TestKVCacheBackendExternal_CapacityFollowsReachability pins the fourth acceptance clause: a
// reachable exposition is published and an unreachable one leaves the figures absent rather than
// zero. The health read succeeds in both cases, so what is being measured is the scrape alone.
func TestKVCacheBackendExternal_CapacityFollowsReachability(t *testing.T) {
	reachable, _ := reconcileExternal(t, newExternalKVCacheBackendObject(),
		map[string]adminResponse{
			"/health":              {body: healthServing},
			"/metrics":             {body: metricsPopulated},
			"/get_segments_detail": {body: segmentsTwoOK},
		})
	require.NotNil(t, reachable)
	require.NotNil(t, reachable.Status.Capacity.Total)
	assert.Equal(t, int64(1082331758592), reachable.Status.Capacity.Total.Value())
	assert.True(t, KVCacheBackendConditionCapacityObserved.IsTrue(reachable))

	unreachable, _ := reconcileExternal(t, newExternalKVCacheBackendObject(),
		map[string]adminResponse{
			"/health":              {body: healthServing},
			"/metrics":             {err: errors.New("connect: connection refused")},
			"/get_segments_detail": {body: segmentsTwoOK},
		})
	require.NotNil(t, unreachable)
	assert.Nil(t, unreachable.Status.Capacity, "absent, never zero")
	assert.True(t, KVCacheBackendConditionCapacityObserved.IsFalse(unreachable))
	assert.Equal(t, "ScrapeFailed", KVCacheBackendConditionCapacityObserved.GetReason(unreachable))

	// And the backend is still Ready. Capacity is one axis of three, and losing the scrape of a
	// backend that says it is serving does not make it unusable.
	assert.Equal(t, KVCacheBackendPhaseReady, unreachable.Status.Phase)
}

// TestKVCacheBackendExternal_CapacityAddsBothPools pins the one place the external mode reads the
// exposition differently, and the reason is that it has less to go on.
//
// A managed backend names its medium, so one pair of gauges is read. An external one names none —
// this API says how to reach a backend, not what it is made of — so both pools are added. The leader
// serializes both families whatever it uses, so a single-tier backend reads the same either way and
// only a tiered one, like the body below, tells the two rules apart.
func TestKVCacheBackendExternal_CapacityAddsBothPools(t *testing.T) {
	got, _ := reconcileExternal(t, newExternalKVCacheBackendObject(), map[string]adminResponse{
		"/health":              {body: healthServing},
		"/metrics":             {body: metricsPopulated + metricsFile},
		"/get_segments_detail": {body: segmentsTwoOK},
	})
	require.NotNil(t, got)

	require.NotNil(t, got.Status.Capacity.Total)
	require.NotNil(t, got.Status.Capacity.Used)
	assert.Equal(t, int64(1082331758592+2164663517184), got.Status.Capacity.Total.Value())
	assert.Equal(t, int64(5476083302+10952166604), got.Status.Capacity.Used.Value())
}

// TestKVCacheBackendExternal_CapacitySaturatesRatherThanWrapping is the edge of the rule above.
//
// Each gauge is separately bounded to a non-negative int64 by the decoder, and their SUM need not
// fit one. A wrapped sum is negative, and a negative capacity is published as a capacity — reported
// with CapacityObserved true, because nothing failed. Only the external mode adds, and an external
// backend's /metrics is whatever address an administrator wrote.
func TestKVCacheBackendExternal_CapacitySaturatesRatherThanWrapping(t *testing.T) {
	const nearMax = "9223372036854775000"
	huge := "master_total_capacity_bytes " + nearMax + "\n" +
		"master_allocated_bytes 1\n" +
		"master_total_file_capacity_bytes " + nearMax + "\n" +
		"master_allocated_file_size_bytes 1\n"

	got, _ := reconcileExternal(t, newExternalKVCacheBackendObject(), map[string]adminResponse{
		"/health":              {body: healthServing},
		"/metrics":             {body: huge},
		"/get_segments_detail": {body: segmentsTwoOK},
	})
	require.NotNil(t, got)

	require.NotNil(t, got.Status.Capacity.Total)
	assert.Positive(t, got.Status.Capacity.Total.Value(),
		"a wrapped sum is negative, and a negative capacity reads as an observed one")
	assert.Equal(t, int64(math.MaxInt64), got.Status.Capacity.Total.Value())
}

// TestKVCacheBackendExternal_AnAddressThatDoesNotAnswerIsAFault is the one verdict that differs from
// the managed side, and it differs because the excuse does not carry over.
//
// A managed leader that cannot be read is excused while its own Pod is not ready — this operator
// created that Pod moments ago. An external address was declared by an admin to be a backend that
// already runs, and there is no Pod of ours to wait on. Reporting Provisioning would leave a typo
// waiting forever for a start that is never going to come.
func TestKVCacheBackendExternal_AnAddressThatDoesNotAnswerIsAFault(t *testing.T) {
	got, _ := reconcileExternal(t, newExternalKVCacheBackendObject(), map[string]adminResponse{
		"/health": {err: errors.New("connect: connection refused")},
	})
	require.NotNil(t, got)

	assert.Equal(t, KVCacheBackendPhaseError, got.Status.Phase)
	assert.Equal(t, "LeaderUnreachable", KVCacheBackendConditionLeaderAvailable.GetReason(got))
	assert.Contains(t, KVCacheBackendConditionLeaderAvailable.GetMessage(got), externalAdminAddress,
		"the message names the address that did not answer, because that is what an operator fixes")
	assert.Nil(t, got.Status.Capacity)

	// The endpoints are published regardless. They are what the spec says, not what was observed,
	// and blanking them would hide the very address the message is complaining about.
	assert.Len(t, got.Status.Endpoints, 2)
}

// TestKVCacheBackendExternal_DoesNotFollowARedirect pins that the address the spec names is the only
// one this operator ever reads.
//
// An external backend's address belongs to whoever wrote the spec, and a default client follows a
// redirect from it to anywhere this operator can reach — with the operator's network identity, and
// with an excerpt of whatever answers copied into a status that is readable by everyone who can read
// the object. The redirect is reported as the answer it is instead.
func TestKVCacheBackendExternal_DoesNotFollowARedirect(t *testing.T) {
	kvcb := newExternalKVCacheBackendObject()
	rt := &adminRoundTripper{byPath: map[string]adminResponse{
		"/health": {
			status:   http.StatusFound,
			location: "http://10.0.0.1:8080/private",
			body:     "moved",
		},
	}}

	// The client the controller builds for itself. Assembling one here would test whatever this
	// line happened to configure, and the thing under test is a property of that client.
	admin := newAdminHTTPClient()
	admin.Transport = rt

	r := &KVCacheBackendReconciler{
		Client:    newClientRefusingCreates(t, kvcb),
		AdminHTTP: admin,
	}
	_, err := r.Reconcile(context.Background(), ctrlreconcile.Request{
		NamespacedName: ctrlcli.ObjectKey{Name: kvcb.Name},
	})
	require.NoError(t, err)

	require.NotEmpty(t, rt.hosts, "the address was read at all")
	for _, host := range rt.hosts {
		assert.Equal(t, externalAdminAddress, host,
			"every read goes to the address in the spec and to nothing the answer points at")
	}
}

// TestKVCacheBackendExternal_RefusesAUsedDelete pins that the finalizer contract does not depend on
// this operator having rendered anything. Nothing here is ours to tear down, and the refusal is
// about the CONSUMERS rather than about the workloads.
func TestKVCacheBackendExternal_RefusesAUsedDelete(t *testing.T) {
	claim := workercore.KVCacheObjectReference{
		Kind: "KVCachePool",
		Name: "team-a-pool",
	}
	kvcb := newExternalKVCacheBackendObject(claim)
	systemmeta.Lock(kvcb)
	now := meta.Now()
	kvcb.DeletionTimestamp = &now

	cli := newClientRefusingCreates(t, append([]ctrlcli.Object{kvcb}, kvCachePoolsNamedBy(claim)...)...)
	r := &KVCacheBackendReconciler{
		Client: cli,
		AdminHTTP: &http.Client{Transport: &adminRoundTripper{byPath: map[string]adminResponse{
			"/health": {body: healthServing},
		}}},
	}
	_, err := r.Reconcile(context.Background(), ctrlreconcile.Request{
		NamespacedName: ctrlcli.ObjectKey{Name: kvcb.Name},
	})
	require.NoError(t, err)

	got := new(workercore.KVCacheBackend)
	require.NoError(t, cli.Get(context.Background(), ctrlcli.ObjectKey{Name: kvcb.Name}, got),
		"a claimed backend must not be released")
	assert.True(t, systemmeta.IsLocked(got))
	assert.Equal(t, KVCacheBackendPhaseDeleting, got.Status.Phase)
	assert.True(t, KVCacheBackendConditionDeletable.IsFalse(got))
	assert.Contains(t, KVCacheBackendConditionDeletable.GetMessage(got), "KVCachePool/team-a-pool")
}

// TestKVCacheBackendExternal_IsIdempotent pins that a settled external backend writes nothing on a
// second pass. It matters more here than on the managed side: the endpoints are copied out of the
// spec on every pass, and a copy that produced a new value each time would write forever.
func TestKVCacheBackendExternal_IsIdempotent(t *testing.T) {
	kvcb := newExternalKVCacheBackendObject()
	cli := newClientRefusingCreates(t, kvcb)
	ctx := context.Background()

	r := &KVCacheBackendReconciler{
		Client: cli,
		AdminHTTP: &http.Client{Transport: &adminRoundTripper{byPath: map[string]adminResponse{
			"/health":              {body: healthServing},
			"/metrics":             {body: metricsPopulated},
			"/get_segments_detail": {body: segmentsTwoOK},
		}}},
	}
	reconcile := func() *workercore.KVCacheBackend {
		_, err := r.Reconcile(ctx, ctrlreconcile.Request{
			NamespacedName: ctrlcli.ObjectKey{Name: kvcb.Name},
		})
		require.NoError(t, err)
		got := new(workercore.KVCacheBackend)
		require.NoError(t, cli.Get(ctx, ctrlcli.ObjectKey{Name: kvcb.Name}, got))
		return got
	}

	first := reconcile()
	require.Equal(t, KVCacheBackendPhaseReady, first.Status.Phase,
		"the first pass must reach a fully observed state, or this proves nothing")

	second := reconcile()
	assert.Equal(t, first.ResourceVersion, second.ResourceVersion,
		"a fully observed external backend produces no status write on the next pass")
}

// TestKVCacheBackendStatus_AnUnschedulableLeaderIsAFaultNotAStart separates the two states that look
// identical through readyReplicas alone.
//
// A leader Pod still pulling its image and a leader Pod no node will accept are both "0 ready". The
// first resolves itself and the second never does — a taint, a node that went away, a request no
// node can satisfy — so reporting them the same leaves an operator watching Provisioning for a start
// that cannot happen. Both halves are asserted here, because the fault only means anything against
// the start it is being told apart from.
func TestKVCacheBackendStatus_AnUnschedulableLeaderIsAFaultNotAStart(t *testing.T) {
	ctx := context.Background()
	kvcb := newKVCacheBackendObject()
	cli := newKVCacheBackendClient(kvcb)
	r := &KVCacheBackendReconciler{
		Client: cli,
		AdminHTTP: &http.Client{Transport: &adminRoundTripper{byPath: map[string]adminResponse{
			"/health": {err: errors.New("connect: connection refused")},
		}}},
	}
	req := ctrlreconcile.Request{NamespacedName: ctrlcli.ObjectKey{Name: kvcb.Name}}

	read := func() *workercore.KVCacheBackend {
		t.Helper()
		_, err := r.Reconcile(ctx, req)
		require.NoError(t, err)
		got := new(workercore.KVCacheBackend)
		require.NoError(t, cli.Get(ctx, ctrlcli.ObjectKey{Name: kvcb.Name}, got))
		return got
	}

	starting := read()
	require.Equal(t, KVCacheBackendPhaseProvisioning, starting.Status.Phase,
		"a leader whose Pod has not appeared yet is starting, which is what the fault below has to "+
			"be distinguishable from")
	require.Equal(t, "LeaderStarting",
		KVCacheBackendConditionLeaderAvailable.GetReason(starting))

	// The refused Pod is labeled from the Deployment's OWN selector rather than from labels
	// restated here, so this test cannot pass against a renderer that labels its Pods differently.
	deploy := new(apps.Deployment)
	require.NoError(t, cli.Get(ctx, ctrlcli.ObjectKey{
		Name:      mooncake.LeaderObjectName(kvcb),
		Namespace: kuberess.SystemNamespaceName,
	}, deploy))
	require.NotNil(t, deploy.Spec.Selector)
	require.NotEmpty(t, deploy.Spec.Selector.MatchLabels)

	placeLeaderPod(t, cli, kvcb, &core.Pod{
		ObjectMeta: meta.ObjectMeta{
			Name:      mooncake.LeaderObjectName(kvcb) + "-refused",
			Namespace: kuberess.SystemNamespaceName,
			Labels:    deploy.Spec.Selector.MatchLabels,
		},
		Status: core.PodStatus{Conditions: []core.PodCondition{{
			Type:   core.PodScheduled,
			Status: core.ConditionFalse,
			Reason: core.PodReasonUnschedulable,
		}}},
	})

	got := read()
	assert.Equal(t, KVCacheBackendPhaseError, got.Status.Phase)
	assert.Equal(t, "LeaderUnschedulable",
		KVCacheBackendConditionLeaderAvailable.GetReason(got))
	assert.Contains(t, got.Status.PhaseMessage, "cannot be scheduled onto any node",
		"the message says what an operator has to change, not that a read failed")
	assert.Nil(t, got.Status.Capacity, "a leader that never started holds nothing")
}

// TestKVCacheBackendStatus_ASettledBackendComesBackOnATimer pins the one thing the watches cannot
// give this reconciler.
//
// Every figure status reports about a running store is read over HTTP from a process whose contents
// move without Kubernetes hearing about it. The watches wake on a workload change, which a leader
// whose Pods are steady while its cache fills does not produce — and which an external backend can
// never produce, because this operator owns no workload for it. Both branches are asserted, since a
// timer on only one of them would leave exactly the case that has no other trigger.
func TestKVCacheBackendStatus_ASettledBackendComesBackOnATimer(t *testing.T) {
	ctx := context.Background()
	serving := map[string]adminResponse{
		"/health":              {body: healthServing},
		"/metrics":             {body: metricsPopulated},
		"/get_segments_detail": {body: segmentsTwoOK},
	}

	t.Run("managed", func(t *testing.T) {
		kvcb := newKVCacheBackendObject()
		cli := newKVCacheBackendClient(kvcb)
		r := &KVCacheBackendReconciler{
			Client:    cli,
			AdminHTTP: &http.Client{Transport: &adminRoundTripper{byPath: serving}},
		}
		res, err := r.Reconcile(ctx, ctrlreconcile.Request{
			NamespacedName: ctrlcli.ObjectKey{Name: kvcb.Name},
		})
		require.NoError(t, err)
		assert.Equal(t, kvCacheBackendObserveInterval, res.RequeueAfter)
	})

	t.Run("external", func(t *testing.T) {
		kvcb := newExternalKVCacheBackendObject()
		cli := newClientRefusingCreates(t, kvcb)
		r := &KVCacheBackendReconciler{
			Client:    cli,
			AdminHTTP: &http.Client{Transport: &adminRoundTripper{byPath: serving}},
		}
		res, err := r.Reconcile(ctx, ctrlreconcile.Request{
			NamespacedName: ctrlcli.ObjectKey{Name: kvcb.Name},
		})
		require.NoError(t, err)
		assert.Equal(t, kvCacheBackendObserveInterval, res.RequeueAfter,
			"an external backend owns no workload to watch, so the timer is its only trigger")
	})
}

// TestKVCacheBackendWatch_OnlyALeadersPodMapsBack pins the Pod watch's filter.
//
// The leader's Pods carry no resource note — the Deployment's own controller makes them from a
// template, and the note is on the Deployment rather than inside it — so this watch matches on the
// identity labels instead. The labels come from the RENDERER here rather than being restated, so the
// test cannot pass against a renderer that labels its Pods differently.
func TestKVCacheBackendWatch_OnlyALeadersPodMapsBack(t *testing.T) {
	ctx := context.Background()
	kvcb := newKVCacheBackendObject()
	cli := newKVCacheBackendClient(kvcb)
	r := &KVCacheBackendReconciler{
		Client:    cli,
		AdminHTTP: &http.Client{Transport: &adminRoundTripper{byPath: map[string]adminResponse{}}},
	}
	_, err := r.Reconcile(ctx, ctrlreconcile.Request{
		NamespacedName: ctrlcli.ObjectKey{Name: kvcb.Name},
	})
	require.NoError(t, err)

	deploy := new(apps.Deployment)
	require.NoError(t, cli.Get(ctx, ctrlcli.ObjectKey{
		Name:      mooncake.LeaderObjectName(kvcb),
		Namespace: kuberess.SystemNamespaceName,
	}, deploy))
	leaderPodLabels := deploy.Spec.Template.Labels
	require.NotEmpty(t, leaderPodLabels)

	ds := new(apps.DaemonSet)
	require.NoError(t, cli.Get(ctx, ctrlcli.ObjectKey{
		Name:      mooncake.MemberObjectName(kvcb, 0),
		Namespace: kuberess.SystemNamespaceName,
	}, ds))
	memberPodLabels := ds.Spec.Template.Labels
	require.NotEmpty(t, memberPodLabels)

	cases := []struct {
		name      string
		namespace string
		labels    map[string]string
		want      []ctrlreconcile.Request
		watched   bool
	}{
		{
			name:    "the leader's own Pod",
			labels:  leaderPodLabels,
			want:    []ctrlreconcile.Request{{NamespacedName: ctrlcli.ObjectKey{Name: kvcb.Name}}},
			watched: true,
		},
		{
			name:   "a member's Pod, which the DaemonSet watch already covers",
			labels: memberPodLabels,
		},
		{
			name:   "some other operator's Pod wearing the same instance label",
			labels: map[string]string{"app.kubernetes.io/instance": kvcb.Name},
		},
		{
			name:   "a Pod with no labels at all",
			labels: nil,
		},
		{
			// Labels are the one thing this watch can filter on, and any user who can create a Pod
			// in their own namespace can copy them. The mapper still resolves this one, because all
			// it ever reads is the label — which is why the namespace has to be the predicate's
			// business, and why the two are asserted apart here.
			name:      "the leader's labels, worn by a Pod in somebody else's namespace",
			namespace: "default",
			labels:    leaderPodLabels,
			want:      []ctrlreconcile.Request{{NamespacedName: ctrlcli.ObjectKey{Name: kvcb.Name}}},
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			namespace := c.namespace
			if namespace == "" {
				namespace = kuberess.SystemNamespaceName
			}
			pod := &core.Pod{ObjectMeta: meta.ObjectMeta{
				Name:      "p",
				Namespace: namespace,
				Labels:    c.labels,
			}}
			assert.Equal(t, c.want, r.enqueueKVCacheBackendWhenLeaderPodChanged(ctx, pod))
			assert.Equal(t, c.watched, kvCacheBackendLeaderPodPredicate().Create(
				ctrlevent.CreateEvent{Object: pod}),
				"the predicate runs first, so what it admits is what the mapper is ever asked about")
		})
	}
}

// TestKVCacheBackendStatus_AStartingPodIsNotAShortfall separates "should have mounted" from "exists".
//
// The shortfall compares the leader's segments against the member Pods that ought to be holding one,
// and a Pod still pulling its image is not one of those. Counting it would invent a gap against a
// Pod that has not been asked to fill anything yet, and hold an otherwise healthy backend at
// Degraded for as long as the rollout takes.
func TestKVCacheBackendStatus_AStartingPodIsNotAShortfall(t *testing.T) {
	kvcb := newKVCacheBackendObject()

	// The ready Pod on n7 holds the segment the leader lists for n7, so it is accounted for. The
	// Pod on n8 is still starting; the leader already lists its segment, and nothing about that
	// Pod may be held against the backend either way.
	got := reconcileWithAdminAndPods(t, kvcb, map[string]adminResponse{
		"/health":              {body: healthServing},
		"/metrics":             {body: metricsPopulated},
		"/get_segments_detail": {body: segmentsTwoOK},
	},
		runningMemberPod(t, kvcb, "n7", "10.42.0.11"),
		startingMemberPod(t, kvcb, "n8", "10.42.0.12"))

	assert.True(t, KVCacheBackendConditionMembersMounted.IsTrue(got),
		"the one ready pod is listed, and a pod still starting is not held to the shortfall")
	assert.Equal(t, KVCacheBackendPhaseReady, got.Status.Phase)

	// The starting Pod is still INDEXED, or the segment it eventually mounts could not be joined.
	require.Len(t, got.Status.Members, 2)
	assert.Equal(t, "n8", got.Status.Members[1].NodeName,
		"the join reads the index, which carries every pod; only the accounting is gated on readiness")
}

// TestKVCacheBackendStatus_UnreadablePodsAreNotAMountedVerdict pins what happens when the Kubernetes
// half of the join fails.
//
// The leader's listing is still published — it was read successfully and is the better half of the
// answer — but with no Pods to compare it against there is no verdict to give. Reporting Mounted
// would claim every member is accounted for on the strength of a comparison that never ran.
//
// Only the SECOND pod listing fails, and that is the shape the case has to have. A pass lists this
// backend's member Pods twice — once to decide restarts, once to join the leader's segments — and a
// failure of the first returns from the reconcile long before any of this. The window being pinned
// is the transient one where the first read succeeded and the second did not.
func TestKVCacheBackendStatus_UnreadablePodsAreNotAMountedVerdict(t *testing.T) {
	kvcb := newKVCacheBackendObject()

	podLists := 0
	cli := ctrlfake.NewClientBuilder().
		WithScheme(scheme.Scheme).
		WithStatusSubresource(&workercore.KVCacheBackend{}).
		WithObjects(kvcb).
		WithInterceptorFuncs(ctrlinterceptor.Funcs{
			List: func(ctx context.Context, c ctrlcli.WithWatch, list ctrlcli.ObjectList,
				opts ...ctrlcli.ListOption,
			) error {
				if _, ok := list.(*core.PodList); ok {
					podLists++
					if podLists > 1 {
						return errors.New("etcd is having a moment")
					}
				}
				return c.List(ctx, list, opts...)
			},
		}).Build()

	r := &KVCacheBackendReconciler{
		Client: cli,
		AdminHTTP: &http.Client{Transport: &adminRoundTripper{byPath: map[string]adminResponse{
			"/health":              {body: healthServing},
			"/metrics":             {body: metricsPopulated},
			"/get_segments_detail": {body: segmentsTwoOK},
		}}},
	}
	_, err := r.Reconcile(context.Background(), ctrlreconcile.Request{
		NamespacedName: ctrlcli.ObjectKey{Name: kvcb.Name},
	})
	require.NoError(t, err, "an unreadable pod list is reported, not returned as a reconcile error")

	got := new(workercore.KVCacheBackend)
	require.NoError(t, cli.Get(context.Background(), ctrlcli.ObjectKey{Name: kvcb.Name}, got))

	assert.Len(t, got.Status.Members, 2, "the listing was read, so it is published")
	assert.True(t, KVCacheBackendConditionMembersMounted.IsFalse(got),
		"with nothing to compare against, Mounted would be a claim this pass cannot make")
	assert.Equal(t, "PodsUnreadable", KVCacheBackendConditionMembersMounted.GetReason(got))
	assert.Contains(t, KVCacheBackendConditionMembersMounted.GetMessage(got), "etcd is having a moment")
}

// TestKVCacheBackendStatus_AContainerThatWillNotRunIsAFault covers the second fault readyReplicas
// cannot see.
//
// A leader Pod that was scheduled and whose container will not start — an image that cannot be
// pulled, a process exiting as fast as it starts — is neither ready nor unschedulable. Without
// reading the container's own state it falls through to "still starting" and the backend reports
// Provisioning for as long as it exists, which is the same defect the unschedulable case had.
func TestKVCacheBackendStatus_AContainerThatWillNotRunIsAFault(t *testing.T) {
	cases := []struct {
		name   string
		status core.ContainerStatus
		reason string
		says   string
	}{
		{
			name: "an image that cannot be pulled",
			status: core.ContainerStatus{
				Name: "leader",
				State: core.ContainerState{Waiting: &core.ContainerStateWaiting{
					Reason:  "ImagePullBackOff",
					Message: `Back-off pulling image "example.com/mooncake:v0"`,
				}},
			},
			reason: "ImagePullBackOff",
			says:   "Back-off pulling image",
		},
		{
			name: "a container that keeps exiting",
			status: core.ContainerStatus{
				Name:         "leader",
				RestartCount: 7,
				State: core.ContainerState{Waiting: &core.ContainerStateWaiting{
					Reason: "CrashLoopBackOff", Message: "back-off 5m0s restarting failed container",
				}},
			},
			reason: "CrashLoopBackOff",
			says:   "back-off 5m0s",
		},
		{
			name: "a config the kubelet cannot turn into a container",
			status: core.ContainerStatus{
				Name: "leader",
				State: core.ContainerState{Waiting: &core.ContainerStateWaiting{
					Reason: "CreateContainerConfigError", Message: `secret "registry-creds" not found`,
				}},
			},
			reason: "CreateContainerConfigError",
			says:   "registry-creds",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			kvcb := newKVCacheBackendObject()
			cli := newKVCacheBackendClient(kvcb)
			r := &KVCacheBackendReconciler{
				Client:    cli,
				AdminHTTP: &http.Client{Transport: &adminRoundTripper{byPath: map[string]adminResponse{}}},
			}
			req := ctrlreconcile.Request{NamespacedName: ctrlcli.ObjectKey{Name: kvcb.Name}}
			_, err := r.Reconcile(context.Background(), req)
			require.NoError(t, err)

			// The Pod the Deployment's controller would have made, in the state the kubelet left
			// it: scheduled, not ready, and not going anywhere.
			placeLeaderPod(t, cli, kvcb, leaderPodInState(t, kvcb, c.status))

			_, err = r.Reconcile(context.Background(), req)
			require.NoError(t, err)

			got := new(workercore.KVCacheBackend)
			require.NoError(t, cli.Get(context.Background(),
				ctrlcli.ObjectKey{Name: kvcb.Name}, got))

			assert.Equal(t, c.reason, KVCacheBackendConditionLeaderAvailable.GetReason(got),
				"the kubelet's own reason is passed through, not restated")
			assert.Contains(t, KVCacheBackendConditionLeaderAvailable.GetMessage(got), c.says,
				"and its message with it, because it names the thing to go and fix")
			assert.Equal(t, KVCacheBackendPhaseError, got.Status.Phase,
				"waiting does not resolve any of these, so it is not a start in progress")
		})
	}
}

// TestKVCacheBackendStatus_AStartingContainerIsNotAFault is the other side of the same rule: the
// kubelet's own progress states must NOT read as faults, or every install reports Error for the
// seconds its container takes to be created.
func TestKVCacheBackendStatus_AStartingContainerIsNotAFault(t *testing.T) {
	for _, reason := range []string{"ContainerCreating", "PodInitializing"} {
		t.Run(reason, func(t *testing.T) {
			kvcb := newKVCacheBackendObject()
			cli := newKVCacheBackendClient(kvcb)
			r := &KVCacheBackendReconciler{
				Client:    cli,
				AdminHTTP: &http.Client{Transport: &adminRoundTripper{byPath: map[string]adminResponse{}}},
			}
			req := ctrlreconcile.Request{NamespacedName: ctrlcli.ObjectKey{Name: kvcb.Name}}
			_, err := r.Reconcile(context.Background(), req)
			require.NoError(t, err)

			placeLeaderPod(t, cli, kvcb, leaderPodInState(t, kvcb,
				core.ContainerStatus{
					Name:  "leader",
					State: core.ContainerState{Waiting: &core.ContainerStateWaiting{Reason: reason}},
				}))

			_, err = r.Reconcile(context.Background(), req)
			require.NoError(t, err)

			got := new(workercore.KVCacheBackend)
			require.NoError(t, cli.Get(context.Background(),
				ctrlcli.ObjectKey{Name: kvcb.Name}, got))

			assert.Equal(t, "LeaderStarting", KVCacheBackendConditionLeaderAvailable.GetReason(got))
			assert.Equal(t, KVCacheBackendPhaseProvisioning, got.Status.Phase)
		})
	}
}

// TestHostOf covers the join key the whole membership listing turns on. It fails silently — a miss
// leaves an empty node and medium rather than an error — so the shapes are pinned rather than
// trusted.
func TestHostOf(t *testing.T) {
	cases := []struct {
		name    string
		address string
		want    string
	}{
		{"a pod IP and a port, which is what a rendered member reports", "10.42.0.11:15380", "10.42.0.11"},
		{"an IPv4 address and a port", "10.42.0.11:15380", "10.42.0.11"},
		{"a bracketed IPv6 address and a port", "[2001:db8::1]:15002", "2001:db8::1"},
		{"a bare host with no port", "n7", "n7"},
		{"a bare IPv6 address, which has colons but no port", "2001:db8::1", "2001:db8::1"},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			assert.Equal(t, c.want, hostOf(c.address))
		})
	}
}

// TestKVCacheBackendStatus_AStrangerPodIsNotTheLeaders pins that the leader's Pods are proven and
// not merely matched.
//
// Every reader of that list turns what it finds into a fault published against the backend, and the
// labels it lists on are derived — so an unrelated Pod carrying them would make this operator report
// a healthy leader as broken. The Pod here is unschedulable, which is the loudest of those faults.
func TestKVCacheBackendStatus_AStrangerPodIsNotTheLeaders(t *testing.T) {
	ctx := context.Background()
	kvcb := newKVCacheBackendObject()
	cli := newKVCacheBackendClient(kvcb)
	r := &KVCacheBackendReconciler{
		Client: cli,
		AdminHTTP: &http.Client{Transport: &adminRoundTripper{byPath: map[string]adminResponse{
			"/health": {err: errors.New("connect: connection refused")},
		}}},
	}
	req := ctrlreconcile.Request{NamespacedName: ctrlcli.ObjectKey{Name: kvcb.Name}}
	_, err := r.Reconcile(ctx, req)
	require.NoError(t, err)

	deploy := new(apps.Deployment)
	require.NoError(t, cli.Get(ctx, leaderObjectKey(kvcb), deploy))

	// The leader's OWN Pod, still starting. It has to be here, or the reader would find no
	// ReplicaSet at all and decline for that reason instead of the one under test.
	placeLeaderPod(t, cli, kvcb, leaderPodInState(t, kvcb, core.ContainerStatus{
		Name:  "leader",
		State: core.ContainerState{Waiting: &core.ContainerStateWaiting{Reason: "ContainerCreating"}},
	}))

	// And beside it: same labels, no ownership. Nothing links this one back to the Deployment.
	require.NoError(t, cli.Create(ctx, &core.Pod{
		ObjectMeta: meta.ObjectMeta{
			Name:      "somebody-elses-pod",
			Namespace: kuberess.SystemNamespaceName,
			Labels:    deploy.Spec.Selector.MatchLabels,
		},
		Status: core.PodStatus{Conditions: []core.PodCondition{{
			Type:   core.PodScheduled,
			Status: core.ConditionFalse,
			Reason: core.PodReasonUnschedulable,
		}}},
	}))

	_, err = r.Reconcile(ctx, req)
	require.NoError(t, err)

	got := new(workercore.KVCacheBackend)
	require.NoError(t, cli.Get(ctx, ctrlcli.ObjectKey{Name: kvcb.Name}, got))
	assert.Equal(t, "LeaderStarting", KVCacheBackendConditionLeaderAvailable.GetReason(got),
		"the leader's own Pod is creating its container, which is a start and not a fault")
	assert.Equal(t, KVCacheBackendPhaseProvisioning, got.Status.Phase,
		"a stranger's scheduling problem is not this backend's fault to report")
}

// TestKVCacheBackendReconciler_RestoresDiscoveryLabels covers the labels on the OBJECT rather than
// on its Pod template. They are how the member sweep finds a group, so losing one would hide the
// DaemonSet from the reconciler that owns it.
func TestKVCacheBackendReconciler_RestoresDiscoveryLabels(t *testing.T) {
	ctx := context.Background()
	kvcb := newKVCacheBackendObject()
	cli := newKVCacheBackendClient(kvcb)

	require.NotNil(t, reconcileKVCacheBackend(t, cli, kvcb.Name))

	ds := new(apps.DaemonSet)
	require.NoError(t, cli.Get(ctx, memberObjectKey(kvcb, 0), ds))
	require.Contains(t, ds.Labels, "app.kubernetes.io/instance")
	delete(ds.Labels, "app.kubernetes.io/instance")
	ds.Labels["someone-elses/tracking-id"] = "keep-me"
	require.NoError(t, cli.Update(ctx, ds))

	require.NotNil(t, reconcileKVCacheBackend(t, cli, kvcb.Name))

	after := new(apps.DaemonSet)
	require.NoError(t, cli.Get(ctx, memberObjectKey(kvcb, 0), after))
	assert.Equal(t, kvcb.Name, after.Labels["app.kubernetes.io/instance"],
		"a dropped discovery key is put back")
	assert.Equal(t, "keep-me", after.Labels["someone-elses/tracking-id"],
		"and labels this operator did not put there are left alone")
}

// TestKVCacheBackendReconciler_TeardownFindsAMemberMissingItsIdentityLabels is why the sweep lists on
// the resource-type label rather than on the backend's own two.
//
// Restoring them, as the test above does, is not enough on its own: a delete runs the teardown branch
// and never reaches the aligner, so a label dropped before the delete is never put back. Discovering
// on the same key the ownership check reads is what closes that window.
func TestKVCacheBackendReconciler_TeardownFindsAMemberMissingItsIdentityLabels(t *testing.T) {
	ctx := context.Background()
	kvcb := newKVCacheBackendObject()
	cli := newKVCacheBackendClient(kvcb)
	r := &KVCacheBackendReconciler{
		Client:    cli,
		AdminHTTP: &http.Client{Transport: &adminRoundTripper{byPath: map[string]adminResponse{}}},
	}
	req := ctrlreconcile.Request{NamespacedName: ctrlcli.ObjectKey{Name: kvcb.Name}}
	_, err := r.Reconcile(ctx, req)
	require.NoError(t, err)

	ds := new(apps.DaemonSet)
	require.NoError(t, cli.Get(ctx, memberObjectKey(kvcb, 0), ds))
	delete(ds.Labels, "app.kubernetes.io/name")
	delete(ds.Labels, "app.kubernetes.io/instance")
	require.NoError(t, cli.Update(ctx, ds))

	live := new(workercore.KVCacheBackend)
	require.NoError(t, cli.Get(ctx, ctrlcli.ObjectKey{Name: kvcb.Name}, live))
	require.NoError(t, cli.Delete(ctx, live))

	_, err = r.Reconcile(ctx, req)
	require.NoError(t, err)

	assert.True(t, kerrors.IsNotFound(cli.Get(ctx, memberObjectKey(kvcb, 0), new(apps.DaemonSet))),
		"the member is still found and deleted without its identity labels")
}

// placeLeaderPod puts a leader Pod into the cluster the way the built-in controllers would: a
// ReplicaSet owned by the Deployment, and the Pod owned by that ReplicaSet.
//
// The chain is the whole point of the helper. A Deployment does not own its Pods — its ReplicaSets
// do — and the reader refuses a Pod it cannot walk back to this Deployment, because every fault it
// finds is published against the backend. A fixture that made a bare Pod would agree with a reader
// that trusted the selector labels alone, which is exactly the reading being guarded against.
func placeLeaderPod(
	t *testing.T, cli ctrlcli.Client, kvcb *workercore.KVCacheBackend, pod *core.Pod,
) {
	t.Helper()

	deploy := mooncake.RenderLeaderDeployment(kvcb, "example.com/mooncake:v0")
	rs := &apps.ReplicaSet{
		ObjectMeta: meta.ObjectMeta{
			Name:      mooncake.LeaderObjectName(kvcb) + "-7d9cf6b8c4",
			Namespace: kuberess.SystemNamespaceName,
			Labels:    deploy.Spec.Selector.MatchLabels,
			OwnerReferences: []meta.OwnerReference{{
				APIVersion: "apps/v1",
				Kind:       "Deployment",
				Name:       deploy.Name,
				Controller: ptr.To(true),
			}},
		},
	}
	if err := cli.Create(context.Background(), rs); err != nil && !kerrors.IsAlreadyExists(err) {
		require.NoError(t, err)
	}

	pod.OwnerReferences = []meta.OwnerReference{{
		APIVersion: "apps/v1",
		Kind:       "ReplicaSet",
		Name:       rs.Name,
		Controller: ptr.To(true),
	}}
	require.NoError(t, cli.Create(context.Background(), pod))
}

// leaderPodInState fabricates the Pod the leader's Deployment controller would have created, with
// one container in the state a case is about. It carries the Deployment's own selector labels,
// because that is how the reconciler finds it — the same immutable labels the Pod watch matches.
// Hand it to placeLeaderPod, which supplies the ownership chain.
func leaderPodInState(
	t *testing.T, kvcb *workercore.KVCacheBackend, status core.ContainerStatus,
) *core.Pod {
	t.Helper()

	deploy := mooncake.RenderLeaderDeployment(kvcb, "example.com/mooncake:v0")
	labels := make(map[string]string, len(deploy.Spec.Selector.MatchLabels))
	for k, v := range deploy.Spec.Selector.MatchLabels {
		labels[k] = v
	}

	return &core.Pod{
		ObjectMeta: meta.ObjectMeta{
			Name:      mooncake.LeaderObjectName(kvcb) + "-abcde",
			Namespace: kuberess.SystemNamespaceName,
			Labels:    labels,
		},
		Spec: core.PodSpec{NodeName: "n7"},
		Status: core.PodStatus{
			Conditions: []core.PodCondition{
				{Type: core.PodScheduled, Status: core.ConditionTrue},
				{Type: core.PodReady, Status: core.ConditionFalse},
			},
			ContainerStatuses: []core.ContainerStatus{status},
		},
	}
}

// TestKVCacheBackendConverge_TakesBackWhatWasGrantedByHand pins the two privileges a hand edit could
// grant and nothing else here would ever look at again.
//
// Both are fields the renderer only ever leaves at their zero value, which is exactly why they need
// converging: a comparison that only looks at what the renderer sets to something interesting will
// never notice one of them being turned on, and the grant then stands for the life of the object.
func TestKVCacheBackendConverge_TakesBackWhatWasGrantedByHand(t *testing.T) {
	ctx := context.Background()
	kvcb := newKVCacheBackendObject()
	cli := newKVCacheBackendClient(kvcb)
	r := &KVCacheBackendReconciler{
		Client:    cli,
		AdminHTTP: &http.Client{Transport: &adminRoundTripper{byPath: map[string]adminResponse{}}},
	}
	req := ctrlreconcile.Request{NamespacedName: ctrlcli.ObjectKey{Name: kvcb.Name}}
	_, err := r.Reconcile(ctx, req)
	require.NoError(t, err)

	deployKey := ctrlcli.ObjectKey{
		Name:      mooncake.LeaderObjectName(kvcb),
		Namespace: kuberess.SystemNamespaceName,
	}

	// The leader is designed without the host's network namespace. Nothing else in the aligner
	// reads this field, so an edit that sets it would otherwise never be looked at again.
	deploy := new(apps.Deployment)
	require.NoError(t, cli.Get(ctx, deployKey, deploy))
	require.False(t, deploy.Spec.Template.Spec.HostNetwork, "the renderer never asks for it")
	deploy.Spec.Template.Spec.HostNetwork = true
	require.NoError(t, cli.Update(ctx, deploy))

	// The leader's admin API answers on 9003 with no authentication of its own; it is private
	// because the Service is a ClusterIP. Flipped to NodePort it is published to every node — and
	// the port comparison deliberately ignores the assigned nodePort, so the ports keep comparing
	// equal while the exposure stands.
	// The third: an edit back to RollingUpdate. The replica count still reads 1 afterwards, so
	// nothing about the object looks wrong — and the next update surges a second master anyway,
	// because maxSurge rounds up.
	require.Equal(t, apps.RecreateDeploymentStrategyType, deploy.Spec.Strategy.Type)
	deploy.Spec.Strategy = apps.DeploymentStrategy{
		Type: apps.RollingUpdateDeploymentStrategyType,
		RollingUpdate: &apps.RollingUpdateDeployment{
			MaxSurge:       ptr.To(intstr.FromString("25%")),
			MaxUnavailable: ptr.To(intstr.FromString("25%")),
		},
	}
	require.NoError(t, cli.Update(ctx, deploy))

	// A limit, which the renderers never set. The backend's own claim is a REQUEST on purpose — a
	// member sizes its segment from the same figure — so a limit is a field left at its zero value,
	// and one injected under it OOM-kills the container on a loop with nothing here saying why.
	require.Empty(t, deploy.Spec.Template.Spec.Containers[0].Resources.Limits)
	deploy.Spec.Template.Spec.Containers[0].Resources.Limits = core.ResourceList{
		core.ResourceMemory: resource.MustParse("64Mi"),
	}
	// And the token mount, which is the same shape once more: an edit to true is a privilege granted
	// by hand to a third-party image that never calls the API server.
	deploy.Spec.Template.Spec.AutomountServiceAccountToken = ptr.To(true)
	require.NoError(t, cli.Update(ctx, deploy))

	svc := new(core.Service)
	require.NoError(t, cli.Get(ctx, deployKey, svc))
	require.Equal(t, core.ServiceTypeClusterIP, svc.Spec.Type)
	svc.Spec.Type = core.ServiceTypeNodePort
	svc.Spec.Ports[0].NodePort = 31234
	// And the quieter one: externalIPs publishes every port this Service declares — 9003 among
	// them — while the type stays ClusterIP, so the comparison that catches a NodePort flip sees
	// nothing at all.
	require.Empty(t, svc.Spec.ExternalIPs)
	svc.Spec.ExternalIPs = []string{"203.0.113.7"}
	// And the third route to the same place, which publishes what the other two cannot: an address
	// the readiness gate is withholding on purpose. The leader's probe is gated on its service
	// plane, so this makes an engine's first connection land on a leader that answers and cannot
	// serve.
	require.False(t, svc.Spec.PublishNotReadyAddresses)
	svc.Spec.PublishNotReadyAddresses = true
	require.NoError(t, cli.Update(ctx, svc))

	_, err = r.Reconcile(ctx, req)
	require.NoError(t, err)

	require.NoError(t, cli.Get(ctx, deployKey, deploy))
	assert.False(t, deploy.Spec.Template.Spec.HostNetwork,
		"a privilege the renderer never grants is taken back on the next pass")
	assert.Equal(t, apps.RecreateDeploymentStrategyType, deploy.Spec.Strategy.Type,
		"and so is a strategy that would surge a second master past the single replica")
	assert.Nil(t, deploy.Spec.Strategy.RollingUpdate,
		"with the rolling fields cleared, or they describe a strategy no longer in use")
	assert.Empty(t, deploy.Spec.Template.Spec.Containers[0].Resources.Limits,
		"an injected limit is taken back, or it throttles or OOM-kills a backend the spec never capped")
	require.NotNil(t, deploy.Spec.Template.Spec.AutomountServiceAccountToken)
	assert.False(t, *deploy.Spec.Template.Spec.AutomountServiceAccountToken,
		"and so is a token mounted by hand into an image that never calls the API server")

	require.NoError(t, cli.Get(ctx, deployKey, svc))
	assert.Equal(t, core.ServiceTypeClusterIP, svc.Spec.Type)
	assert.Zero(t, svc.Spec.Ports[0].NodePort,
		"returning to ClusterIP releases the node port with it, or the exposure survives the type")
	assert.Empty(t, svc.Spec.ExternalIPs,
		"and the external addresses go independently of the type, or an unauthenticated admin API "+
			"stays published behind a Service that still reads ClusterIP")
	assert.False(t, svc.Spec.PublishNotReadyAddresses,
		"and the readiness gate is put back, or the Service resolves to a leader that has not "+
			"reported its service plane active")
}

// TestKVCacheBackendConverge_RestoresThePullCredentials is the same contract in the other direction:
// what a hand edit REMOVED has to come back. Dropping the secret leaves a Deployment that cannot
// pull, and since nothing else in the aligner reads the field the object would sit there failing to
// start with its spec still saying which secret to use.
func TestKVCacheBackendConverge_RestoresThePullCredentials(t *testing.T) {
	ctx := context.Background()
	kvcb := newKVCacheBackendObject()
	kvcb.Spec.ImagePullPolicy = core.PullAlways
	kvcb.Spec.ImagePullSecrets = []core.LocalObjectReference{{Name: "registry-creds"}}

	cli := newKVCacheBackendClient(kvcb)
	r := &KVCacheBackendReconciler{
		Client:    cli,
		AdminHTTP: &http.Client{Transport: &adminRoundTripper{byPath: map[string]adminResponse{}}},
	}
	req := ctrlreconcile.Request{NamespacedName: ctrlcli.ObjectKey{Name: kvcb.Name}}
	_, err := r.Reconcile(ctx, req)
	require.NoError(t, err)

	deployKey := ctrlcli.ObjectKey{
		Name:      mooncake.LeaderObjectName(kvcb),
		Namespace: kuberess.SystemNamespaceName,
	}

	deploy := new(apps.Deployment)
	require.NoError(t, cli.Get(ctx, deployKey, deploy))
	require.Len(t, deploy.Spec.Template.Spec.ImagePullSecrets, 1, "the first pass must render them")
	deploy.Spec.Template.Spec.ImagePullSecrets = nil
	deploy.Spec.Template.Spec.Containers[0].ImagePullPolicy = core.PullNever
	require.NoError(t, cli.Update(ctx, deploy))

	_, err = r.Reconcile(ctx, req)
	require.NoError(t, err)

	require.NoError(t, cli.Get(ctx, deployKey, deploy))
	assert.Equal(t, []core.LocalObjectReference{{Name: "registry-creds"}},
		deploy.Spec.Template.Spec.ImagePullSecrets)
	assert.Equal(t, core.PullAlways, deploy.Spec.Template.Spec.Containers[0].ImagePullPolicy)
}

// TestKVCacheBackendConverge_RestoresTheReadinessProbes covers both roles, because each probe
// carries a verdict nothing else in the object supplies. The leader's asks a GATED route that
// answers 503 until the service plane is up; the member's connects to a port the entrypoint serves
// only after it has mounted its segment.
//
// Strip either and the kubelet reports Ready for a process that has merely started — and for a
// member that is the difference between a rollout and a shortfall, since every ready member Pod is
// held to the leader's listing.
func TestKVCacheBackendConverge_RestoresTheReadinessProbes(t *testing.T) {
	ctx := context.Background()
	kvcb := newKVCacheBackendObject()
	cli := newKVCacheBackendClient(kvcb)
	require.NotNil(t, reconcileKVCacheBackend(t, cli, kvcb.Name))

	deployKey := ctrlcli.ObjectKey{
		Name:      mooncake.LeaderObjectName(kvcb),
		Namespace: kuberess.SystemNamespaceName,
	}

	deploy := new(apps.Deployment)
	require.NoError(t, cli.Get(ctx, deployKey, deploy))
	require.NotNil(t, deploy.Spec.Template.Spec.Containers[0].ReadinessProbe)
	deploy.Spec.Template.Spec.Containers[0].ReadinessProbe = nil
	require.NoError(t, cli.Update(ctx, deploy))

	ds := new(apps.DaemonSet)
	require.NoError(t, cli.Get(ctx, memberObjectKey(kvcb, 0), ds))
	require.NotNil(t, ds.Spec.Template.Spec.Containers[0].ReadinessProbe)
	ds.Spec.Template.Spec.Containers[0].ReadinessProbe = nil
	require.NoError(t, cli.Update(ctx, ds))

	require.NotNil(t, reconcileKVCacheBackend(t, cli, kvcb.Name))

	require.NoError(t, cli.Get(ctx, deployKey, deploy))
	assert.NotNil(t, deploy.Spec.Template.Spec.Containers[0].ReadinessProbe,
		"the leader's probe comes back, or a leader takes traffic before its service plane serves it")

	require.NoError(t, cli.Get(ctx, memberObjectKey(kvcb, 0), ds))
	probe := ds.Spec.Template.Spec.Containers[0].ReadinessProbe
	require.NotNil(t, probe, "and so does the member's, or Ready stops meaning mounted")
	require.NotNil(t, probe.TCPSocket)
	assert.Equal(t, int32(8080), probe.TCPSocket.Port.IntVal)
}

// TestKVCacheBackendConverge_DoesNotFightTheServersPullPolicyDefault is the case that made the
// policy resolved rather than left empty.
//
// The API server defaults an empty ImagePullPolicy on write, so a rendered "" would face a stored
// "IfNotPresent" forever and an unconditional comparison would call that drift on every pass — a
// leader rolled for the life of the object. The renderer resolves the same value the server would
// have written, so the two agree and the comparison can stay unconditional.
func TestKVCacheBackendConverge_DoesNotFightTheServersPullPolicyDefault(t *testing.T) {
	ctx := context.Background()
	kvcb := newKVCacheBackendObject()
	cli := newKVCacheBackendClient(kvcb)
	r := &KVCacheBackendReconciler{
		Client:    cli,
		AdminHTTP: &http.Client{Transport: &adminRoundTripper{byPath: map[string]adminResponse{}}},
	}
	req := ctrlreconcile.Request{NamespacedName: ctrlcli.ObjectKey{Name: kvcb.Name}}
	_, err := r.Reconcile(ctx, req)
	require.NoError(t, err)

	deployKey := ctrlcli.ObjectKey{
		Name:      mooncake.LeaderObjectName(kvcb),
		Namespace: kuberess.SystemNamespaceName,
	}

	// A fake client defaults nothing, so the value the API server would have written is applied by
	// hand here. Without it the test would agree with the bug it is meant to catch.
	deploy := new(apps.Deployment)
	require.NoError(t, cli.Get(ctx, deployKey, deploy))
	deploy.Spec.Template.Spec.Containers[0].ImagePullPolicy = core.PullIfNotPresent
	require.NoError(t, cli.Update(ctx, deploy))
	settled := deploy.ResourceVersion

	_, err = r.Reconcile(ctx, req)
	require.NoError(t, err)

	require.NoError(t, cli.Get(ctx, deployKey, deploy))
	assert.Equal(t, core.PullIfNotPresent, deploy.Spec.Template.Spec.Containers[0].ImagePullPolicy,
		"what the renderer resolves is what the server would have defaulted, so they agree")
	assert.Equal(t, settled, deploy.ResourceVersion,
		"and the pass writes nothing at all, which is what makes it settled rather than quiet")
}

// TestKVCacheBackendConverge_ClearingThePullPolicyRestoresTheDefault is the first of the two states
// the old guard could not reach.
//
// A policy that was set and is then REMOVED from the spec has to go back to the default. The
// comparison used to be skipped whenever the spec named nothing, so the value it had been set to
// stood for the life of the object — the user deleted the field and kept the behavior anyway.
func TestKVCacheBackendConverge_ClearingThePullPolicyRestoresTheDefault(t *testing.T) {
	ctx := context.Background()
	kvcb := newKVCacheBackendObject()
	kvcb.Spec.ImagePullPolicy = core.PullAlways

	cli := newKVCacheBackendClient(kvcb)
	r := &KVCacheBackendReconciler{
		Client:    cli,
		AdminHTTP: &http.Client{Transport: &adminRoundTripper{byPath: map[string]adminResponse{}}},
	}
	req := ctrlreconcile.Request{NamespacedName: ctrlcli.ObjectKey{Name: kvcb.Name}}
	_, err := r.Reconcile(ctx, req)
	require.NoError(t, err)

	deployKey := ctrlcli.ObjectKey{
		Name:      mooncake.LeaderObjectName(kvcb),
		Namespace: kuberess.SystemNamespaceName,
	}
	deploy := new(apps.Deployment)
	require.NoError(t, cli.Get(ctx, deployKey, deploy))
	require.Equal(t, core.PullAlways, deploy.Spec.Template.Spec.Containers[0].ImagePullPolicy)

	// The field is removed from the spec.
	live := new(workercore.KVCacheBackend)
	require.NoError(t, cli.Get(ctx, ctrlcli.ObjectKey{Name: kvcb.Name}, live))
	live.Spec.ImagePullPolicy = ""
	require.NoError(t, cli.Update(ctx, live))

	_, err = r.Reconcile(ctx, req)
	require.NoError(t, err)

	require.NoError(t, cli.Get(ctx, deployKey, deploy))
	assert.Equal(t, core.PullIfNotPresent, deploy.Spec.Template.Spec.Containers[0].ImagePullPolicy,
		"removing the field asks for the default back, and the image is a pinned tag")
}

// TestKVCacheBackendConverge_MovingToLatestMovesThePullPolicy is the second state, and it needs no
// user to have touched the policy at all.
//
// With the policy never set, the stored value is whatever was defaulted from the image the backend
// had at the time. Moving the image to :latest changes what that default should be — and this is
// exactly the move where re-pulling is the point, so keeping IfNotPresent is the wrong way round.
func TestKVCacheBackendConverge_MovingToLatestMovesThePullPolicy(t *testing.T) {
	ctx := context.Background()
	kvcb := newKVCacheBackendObject()

	cli := newKVCacheBackendClient(kvcb)
	r := &KVCacheBackendReconciler{
		Client:    cli,
		AdminHTTP: &http.Client{Transport: &adminRoundTripper{byPath: map[string]adminResponse{}}},
	}
	req := ctrlreconcile.Request{NamespacedName: ctrlcli.ObjectKey{Name: kvcb.Name}}
	_, err := r.Reconcile(ctx, req)
	require.NoError(t, err)

	deployKey := ctrlcli.ObjectKey{
		Name:      mooncake.LeaderObjectName(kvcb),
		Namespace: kuberess.SystemNamespaceName,
	}
	deploy := new(apps.Deployment)
	require.NoError(t, cli.Get(ctx, deployKey, deploy))
	require.Equal(t, core.PullIfNotPresent, deploy.Spec.Template.Spec.Containers[0].ImagePullPolicy,
		"the pinned tag it started on")

	live := new(workercore.KVCacheBackend)
	require.NoError(t, cli.Get(ctx, ctrlcli.ObjectKey{Name: kvcb.Name}, live))
	live.Spec.Image = "example.com/mooncake:latest"
	require.NoError(t, cli.Update(ctx, live))

	_, err = r.Reconcile(ctx, req)
	require.NoError(t, err)

	require.NoError(t, cli.Get(ctx, deployKey, deploy))
	assert.Equal(t, core.PullAlways, deploy.Spec.Template.Spec.Containers[0].ImagePullPolicy,
		"the tag moved, so the default derived from it moves with it")
}
