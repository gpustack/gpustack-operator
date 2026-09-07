package worker

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	core "k8s.io/api/core/v1"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	ctrlcli "sigs.k8s.io/controller-runtime/pkg/client"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	"gpustack.ai/gpustack/pkg/kubeapistatus"
	"gpustack.ai/gpustack/pkg/systemmeta"
	"gpustack.ai/gpustack/pkg/worker/kuberess"
	"gpustack.ai/gpustack/pkg/worker/kvcache/mooncake"
)

func readBackend(t *testing.T, cli ctrlcli.Client, name string) *workercore.KVCacheBackend {
	t.Helper()

	kvcb := new(workercore.KVCacheBackend)
	require.NoError(t, cli.Get(context.Background(), ctrlcli.ObjectKey{Name: name}, kvcb))
	return kvcb
}

func readQuotaPolicyDocument(t *testing.T, cli ctrlcli.Client, backend string) string {
	t.Helper()

	cm := new(core.ConfigMap)
	require.NoError(t, cli.Get(context.Background(), ctrlcli.ObjectKey{
		Name:      backend + "-tenant-quota-policy",
		Namespace: kuberess.SystemNamespaceName,
	}, cm))
	return cm.Data[mooncake.QuotaPolicyFileName]
}

// bindingIsGone reports a Binding the API server has finished removing. The fake keeps an object with
// a finalizer on it, so "released" is only visible as the object disappearing after the release.
func bindingIsGone(t *testing.T, cli ctrlcli.Client, namespace, name string) bool {
	t.Helper()

	err := cli.Get(context.Background(),
		ctrlcli.ObjectKey{Namespace: namespace, Name: name}, new(workercore.KVCachePoolBinding))
	return err != nil
}

// holdBinding writes a workload into a Binding's usedBy, standing in for the controller of whatever
// consumes the pool through it.
//
// Nothing in this reconciler writes that list, and nothing else does YET: the kind that will,
// ModelDeployment, belongs to the model-deployment spec, which this repository has not built. So a
// test that expected the reconciler to fill it would be asserting against a writer that does not
// exist — and the finalizer below releases on every real pass until that writer arrives.
func holdBinding(t *testing.T, cli ctrlcli.Client, kvcpb *workercore.KVCachePoolBinding, workload string) {
	t.Helper()

	kvcpb.Status.UsedBy = []workercore.KVCacheObjectReference{
		{Kind: "ModelDeployment", Namespace: "", Name: workload},
	}
	require.NoError(t, cli.Status().Update(context.Background(), kvcpb))
}

func deleteObject(t *testing.T, cli ctrlcli.Client, obj ctrlcli.Object) {
	t.Helper()

	require.NoError(t, cli.Delete(context.Background(), obj))
}

// TestKVCachePoolTeardown_BothLevelsOfUsedBy is the readable half of T10: who holds what, in the one
// direction each object is allowed to look.
func TestKVCachePoolTeardown_BothLevelsOfUsedBy(t *testing.T) {
	master := newFakeMaster()
	address := master.start(t)

	r, cli := newReconciler(
		newReconcileBackend("mooncake-dram", address),
		newTestKVCachePool("shared", "mooncake-dram"),
		newBoundBinding("team-b", "batch", "shared", "team-b-batch", resource.MustParse("10Ti")),
		newBoundBinding("team-a", "chat", "shared", "team-a-chat", resource.MustParse("20Ti")),
	)

	reconcilePool(t, r, "shared")

	t.Run("the pool lists its bindings, from both namespaces", func(t *testing.T) {
		kvcp := readPool(t, cli, "shared")
		assert.Equal(t, []workercore.KVCacheObjectReference{
			{Kind: KVCachePoolBindingKind, Namespace: "team-a", Name: "chat"},
			{Kind: KVCachePoolBindingKind, Namespace: "team-b", Name: "batch"},
		}, kvcp.Status.UsedBy,
			"sorted, so a List that came back differently does not rewrite the status")
	})

	t.Run("the pool claims its backend with an empty-string namespace", func(t *testing.T) {
		kvcb := readBackend(t, cli, "mooncake-dram")
		assert.Equal(t, []workercore.KVCacheObjectReference{
			{Kind: KVCachePoolKind, Namespace: "", Name: "shared"},
		}, kvcb.Status.UsedBy,
			"both objects are cluster-scoped, so the empty namespace is a value rather than an absence")
	})

	t.Run("everything that registered something is locked", func(t *testing.T) {
		assert.True(t, systemmeta.IsLocked(readPool(t, cli, "shared")))
		assert.True(t, systemmeta.IsLocked(readBinding(t, cli, "team-a", "chat")))
		assert.True(t, systemmeta.IsLocked(readBinding(t, cli, "team-b", "batch")))
	})

	t.Run("a settled pass rewrites neither the claim nor the list", func(t *testing.T) {
		before := readBackend(t, cli, "mooncake-dram").ResourceVersion

		reconcilePool(t, r, "shared")

		assert.Equal(t, before, readBackend(t, cli, "mooncake-dram").ResourceVersion,
			"the backend belongs to another controller, and a claim rewritten every pass is churn "+
				"on its object rather than on this one")
	})
}

// TestKVCachePoolTeardown_ABindingHeldByAWorkloadIsNotReleased is criterion 5's unit half.
func TestKVCachePoolTeardown_ABindingHeldByAWorkloadIsNotReleased(t *testing.T) {
	master := newFakeMaster()
	address := master.start(t)

	r, cli := newReconciler(
		newReconcileBackend("mooncake-dram", address),
		newTestKVCachePool("shared", "mooncake-dram"),
		newBoundBinding("team-a", "chat", "shared", "team-a-chat", resource.MustParse("20Ti")),
	)

	reconcilePool(t, r, "shared")
	holdBinding(t, cli, readBinding(t, cli, "team-a", "chat"), "qwen-72b")
	deleteObject(t, cli, readBinding(t, cli, "team-a", "chat"))

	reconcilePool(t, r, "shared")

	kvcpb := readBinding(t, cli, "team-a", "chat")

	t.Run("the finalizer holds it", func(t *testing.T) {
		assert.True(t, systemmeta.IsLocked(kvcpb))
		assert.NotNil(t, kvcpb.DeletionTimestamp)
	})

	t.Run("the condition names the holder", func(t *testing.T) {
		assert.False(t, KVCachePoolBindingConditionReleasable.IsTrue(kvcpb))
		assert.Equal(t, KVCachePoolBindingReasonHeldByWorkloads,
			conditionReason(t, kvcpb, KVCachePoolBindingConditionReleasable))
		assert.Contains(t, KVCachePoolBindingConditionReleasable.GetMessage(kvcpb),
			"ModelDeployment/qwen-72b",
			"an operator whose delete is refused needs to know what to go and remove")
	})

	t.Run("the phase says deleting rather than the axis it was last serving", func(t *testing.T) {
		assert.Equal(t, KVCachePoolPhaseDeleting, kvcpb.Status.Phase)
	})

	t.Run("its quota stays on the master", func(t *testing.T) {
		assert.Equal(t, map[string]int64{"team-a-chat": quantityValue("20Ti")}, master.held(),
			"the workload is still writing, and withdrawing the quota under it would refuse writes "+
				"nothing has authorized stopping")
	})
}

// TestKVCachePoolTeardown_AnUnheldBindingReleasesAfterItsEntryIsGone pins the ORDER T10 is about.
func TestKVCachePoolTeardown_AnUnheldBindingReleasesAfterItsEntryIsGone(t *testing.T) {
	master := newFakeMaster()
	address := master.start(t)

	r, cli := newReconciler(
		newReconcileBackend("mooncake-dram", address),
		newTestKVCachePool("shared", "mooncake-dram"),
		newBoundBinding("team-a", "chat", "shared", "team-a-chat", resource.MustParse("20Ti")),
		newBoundBinding("team-b", "batch", "shared", "team-b-batch", resource.MustParse("10Ti")),
	)

	reconcilePool(t, r, "shared")
	deleteObject(t, cli, readBinding(t, cli, "team-a", "chat"))

	reconcilePool(t, r, "shared")

	t.Run("the entry it owned is deleted and no other", func(t *testing.T) {
		assert.Equal(t, map[string]int64{"team-b-batch": quantityValue("10Ti")}, master.held())
	})

	t.Run("the binding is released and gone", func(t *testing.T) {
		assert.True(t, bindingIsGone(t, cli, "team-a", "chat"))
	})

	t.Run("the pool stops listing it", func(t *testing.T) {
		assert.Equal(t, []workercore.KVCacheObjectReference{
			{Kind: KVCachePoolBindingKind, Namespace: "team-b", Name: "batch"},
		}, readPool(t, cli, "shared").Status.UsedBy)
	})

	t.Run("the rendered policy drops it too", func(t *testing.T) {
		document := readQuotaPolicyDocument(t, cli, "mooncake-dram")
		assert.NotContains(t, document, "team-a-chat")
		assert.Contains(t, document, "team-b-batch")
	})
}

// TestKVCachePoolTeardown_AnUnconvergedLedgerHoldsTheRelease is the ordering asserted from the other
// side: without it, the release would run on a pass that never reached the master.
func TestKVCachePoolTeardown_AnUnconvergedLedgerHoldsTheRelease(t *testing.T) {
	master := newFakeMaster()
	address := master.start(t)

	r, cli := newReconciler(
		newReconcileBackend("mooncake-dram", address),
		newTestKVCachePool("shared", "mooncake-dram"),
		newBoundBinding("team-a", "chat", "shared", "team-a-chat", resource.MustParse("20Ti")),
	)

	reconcilePool(t, r, "shared")
	deleteObject(t, cli, readBinding(t, cli, "team-a", "chat"))

	// The master stops answering the ledger. The exposition is untouched, so this is specifically a
	// ledger that did not converge rather than a pass that saw nothing at all.
	master.refuse(503, `{"success":false,"error_code":-1011}`)
	reconcilePool(t, r, "shared")

	assert.True(t, systemmeta.IsLocked(readBinding(t, cli, "team-a", "chat")),
		"released over an entry still on the master, the capacity it holds becomes unreclaimable: "+
			"the ledger records no owner and the object that knew is gone")

	master.refuse(0, "")
	reconcilePool(t, r, "shared")

	assert.True(t, bindingIsGone(t, cli, "team-a", "chat"),
		"and the hold is not a latch: the pass that does converge releases it")
}

// TestKVCachePoolTeardown_APoolIsHeldByItsBindings is the pool's half of the refusal.
func TestKVCachePoolTeardown_APoolIsHeldByItsBindings(t *testing.T) {
	master := newFakeMaster()
	address := master.start(t)

	r, cli := newReconciler(
		newReconcileBackend("mooncake-dram", address),
		newTestKVCachePool("shared", "mooncake-dram"),
		newBoundBinding("team-a", "chat", "shared", "team-a-chat", resource.MustParse("20Ti")),
	)

	reconcilePool(t, r, "shared")
	deleteObject(t, cli, readPool(t, cli, "shared"))

	reconcilePool(t, r, "shared")

	kvcp := readPool(t, cli, "shared")

	t.Run("the finalizer holds it and the condition names the binding", func(t *testing.T) {
		assert.True(t, systemmeta.IsLocked(kvcp))
		assert.Equal(t, KVCachePoolReasonHeldByBindings,
			conditionReason(t, kvcp, KVCachePoolConditionReleasable))
		assert.Contains(t, KVCachePoolConditionReleasable.GetMessage(kvcp), "team-a/chat")
		assert.Equal(t, KVCachePoolPhaseDeleting, kvcp.Status.Phase)
	})

	t.Run("the ledger is untouched while it is held", func(t *testing.T) {
		assert.Equal(t, map[string]int64{"team-a-chat": quantityValue("20Ti")}, master.held())
	})

	t.Run("the backend is still claimed", func(t *testing.T) {
		assert.NotEmpty(t, readBackend(t, cli, "mooncake-dram").Status.UsedBy,
			"a pool that dropped its claim while still holding entries would let the backend go "+
				"with them still on it")
	})
}

// TestKVCachePoolTeardown_APoolReleasesOnlyItsOwnEntries is the F7 rule on the delete path: the
// ledger is shared, so what a pool may remove is what it registered.
func TestKVCachePoolTeardown_APoolReleasesOnlyItsOwnEntries(t *testing.T) {
	master := newFakeMaster()
	address := master.start(t)

	r, cli := newReconciler(
		newReconcileBackend("mooncake-dram", address),
		newTestKVCachePool("shared", "mooncake-dram"),
		newTestKVCachePool("research", "mooncake-dram"),
		newBoundBinding("team-a", "chat", "shared", "team-a-chat", resource.MustParse("20Ti")),
		newBoundBinding("lab", "sweep", "research", "lab-sweep", resource.MustParse("5Ti")),
	)

	reconcilePool(t, r, "shared")
	reconcilePool(t, r, "research")

	// The Binding is force-deleted: its lock comes off without the pass that would have removed its
	// entry. That is the state the pool's own finalizer exists to repair.
	kvcpb := readBinding(t, cli, "team-a", "chat")
	systemmeta.Unlock(kvcpb)
	require.NoError(t, cli.Update(context.Background(), kvcpb))
	deleteObject(t, cli, kvcpb)

	deleteObject(t, cli, readPool(t, cli, "shared"))
	reconcilePool(t, r, "shared")

	t.Run("its own entry is gone", func(t *testing.T) {
		assert.NotContains(t, master.held(), "team-a-chat")
	})

	t.Run("the sibling pool's entry survives", func(t *testing.T) {
		assert.Equal(t, map[string]int64{"lab-sweep": quantityValue("5Ti")}, master.held(),
			"one master serves several pools, and its ledger says nothing about whose an entry is")
	})

	t.Run("the rendered policy loses this pool's tenant and keeps the sibling's", func(t *testing.T) {
		document := readQuotaPolicyDocument(t, cli, "mooncake-dram")
		assert.NotContains(t, document, "team-a-chat",
			"the ledger is not the only place a tenant is written: this document is rendered by "+
				"this controller but owned by the backend, so an entry left in it is one the seed "+
				"container copies back over the master's file on the next container start")
		assert.Contains(t, document, "lab-sweep",
			"and the sibling's tenant has to survive that re-render, for the same reason its "+
				"ledger entry does")
	})

	t.Run("the claim on the backend is dropped", func(t *testing.T) {
		assert.Equal(t, []workercore.KVCacheObjectReference{
			{Kind: KVCachePoolKind, Namespace: "", Name: "research"},
		}, readBackend(t, cli, "mooncake-dram").Status.UsedBy,
			"the sibling pool still holds the backend, and only this pool's claim goes")
	})

	t.Run("the pool is released", func(t *testing.T) {
		err := cli.Get(context.Background(), ctrlcli.ObjectKey{Name: "shared"}, new(workercore.KVCachePool))
		assert.Error(t, err)
	})
}

// TestKVCachePoolTeardown_TheLastPoolLeavesNoPolicyBehind pins the worse half of what the rendered
// document can strand.
//
// With a sibling pool on the master, a tenant left in the document survives only until that sibling's
// next pass re-renders. With NO sibling, nothing renders it ever again — and the seed container goes
// on copying it over the master's file at every container start, restoring quotas for reuse domains
// no object in the cluster claims. The master rebuilds usage from an empty index on load, so each one
// comes back as a quota with zero usage: the shape of a healthy tenant, which no reading of the
// master afterwards can tell from a real one. The last pool out is the only thing that can prevent it.
func TestKVCachePoolTeardown_TheLastPoolLeavesNoPolicyBehind(t *testing.T) {
	master := newFakeMaster()
	address := master.start(t)

	r, cli := newReconciler(
		newReconcileBackend("mooncake-dram", address),
		newTestKVCachePool("shared", "mooncake-dram"),
		newBoundBinding("team-a", "chat", "shared", "team-a-chat", resource.MustParse("20Ti")),
	)

	reconcilePool(t, r, "shared")
	require.Contains(t, readQuotaPolicyDocument(t, cli, "mooncake-dram"), "team-a-chat",
		"the tenant has to be in the document before its removal from it means anything")

	// Force-deleted, so the Binding's lock comes off without the pass that removes its entry — the
	// same repair path the sibling case exercises, here with nobody left to repair it afterwards.
	kvcpb := readBinding(t, cli, "team-a", "chat")
	systemmeta.Unlock(kvcpb)
	require.NoError(t, cli.Update(context.Background(), kvcpb))
	deleteObject(t, cli, kvcpb)

	deleteObject(t, cli, readPool(t, cli, "shared"))
	reconcilePool(t, r, "shared")

	assert.NotContains(t, readQuotaPolicyDocument(t, cli, "mooncake-dram"), "team-a-chat",
		"nothing renders this document again once the last pool is gone, so this pass is the last "+
			"chance to take its tenants out of it")
}

// TestKVCachePoolTeardown_ADomainThatStillHoldsObjectsHoldsThePool pins the 409 that is not F5's.
func TestKVCachePoolTeardown_ADomainThatStillHoldsObjectsHoldsThePool(t *testing.T) {
	master := newFakeMaster()
	address := master.start(t)

	r, cli := newReconciler(
		newReconcileBackend("mooncake-dram", address),
		newTestKVCachePool("shared", "mooncake-dram"),
		newBoundBinding("team-a", "chat", "shared", "team-a-chat", resource.MustParse("20Ti")),
	)

	reconcilePool(t, r, "shared")

	kvcpb := readBinding(t, cli, "team-a", "chat")
	systemmeta.Unlock(kvcpb)
	require.NoError(t, cli.Update(context.Background(), kvcpb))
	deleteObject(t, cli, kvcpb)

	// -1702 is TENANT_NOT_EMPTY, the third of the three meanings 409 carries on this surface. Only the
	// REMOVAL is refused: the teardown reads the ledger first, to apply the same explicit-policy gate
	// convergence uses, and a master that refused the listing too would hold for that reason instead.
	master.refuseDeletes(409, `{"success":false,"error_code":-1702,"error_message":"TENANT_NOT_EMPTY"}`)

	deleteObject(t, cli, readPool(t, cli, "shared"))
	reconcilePool(t, r, "shared")

	kvcp := readPool(t, cli, "shared")

	assert.True(t, systemmeta.IsLocked(kvcp))
	assert.Equal(t, KVCachePoolReasonLedgerNotReleased,
		conditionReason(t, kvcp, KVCachePoolConditionReleasable))
	assert.Contains(t, KVCachePoolConditionReleasable.GetMessage(kvcp), "still holds objects",
		"the third meaning of 409 is answered here, and reading it as multi-tenancy-off would put a "+
			"false condition on the one call that decides whether an object can be released")
	assert.NotEmpty(t, readBackend(t, cli, "mooncake-dram").Status.UsedBy,
		"and the claim outlives the entry it could not remove: a backend released with one of this "+
			"pool's entries still on its master could be deleted with the entry on it")
}

// TestKVCachePoolTeardown_APoolAndItsBindingsGoTogether is the ordinary way a stack comes down, and
// the one shape that deadlocks if the teardown only ever refuses.
//
// A Binding's lock comes off in the SERVING pass, and a pool marked for deletion never reaches one.
// Without the teardown releasing what it can before it refuses, the pool would wait for Bindings
// whose only release path it has just stopped taking, and neither object would ever go.
func TestKVCachePoolTeardown_APoolAndItsBindingsGoTogether(t *testing.T) {
	master := newFakeMaster()
	address := master.start(t)

	r, cli := newReconciler(
		newReconcileBackend("mooncake-dram", address),
		newTestKVCachePool("shared", "mooncake-dram"),
		newBoundBinding("team-a", "chat", "shared", "team-a-chat", resource.MustParse("20Ti")),
		newBoundBinding("team-b", "batch", "shared", "team-b-batch", resource.MustParse("10Ti")),
	)

	reconcilePool(t, r, "shared")

	// Everything at once, which is what `kubectl delete -f` does.
	deleteObject(t, cli, readPool(t, cli, "shared"))
	deleteObject(t, cli, readBinding(t, cli, "team-a", "chat"))
	deleteObject(t, cli, readBinding(t, cli, "team-b", "batch"))

	reconcilePool(t, r, "shared")

	t.Run("every binding is released", func(t *testing.T) {
		assert.True(t, bindingIsGone(t, cli, "team-a", "chat"))
		assert.True(t, bindingIsGone(t, cli, "team-b", "batch"))
	})

	t.Run("their entries are gone from the master", func(t *testing.T) {
		assert.Empty(t, master.held())
	})

	t.Run("and the pool goes with them", func(t *testing.T) {
		err := cli.Get(context.Background(), ctrlcli.ObjectKey{Name: "shared"},
			new(workercore.KVCachePool))
		assert.Error(t, err, "one pass, because nothing was ever waiting on anything else")
	})
}

// TestKVCachePoolTeardown_AHeldBindingStillLetsItsSiblingGo is the same shape with one holder left.
// The pool has to refuse and still not strand the sibling behind that refusal.
func TestKVCachePoolTeardown_AHeldBindingStillLetsItsSiblingGo(t *testing.T) {
	master := newFakeMaster()
	address := master.start(t)

	r, cli := newReconciler(
		newReconcileBackend("mooncake-dram", address),
		newTestKVCachePool("shared", "mooncake-dram"),
		newBoundBinding("team-a", "chat", "shared", "team-a-chat", resource.MustParse("20Ti")),
		newBoundBinding("team-b", "batch", "shared", "team-b-batch", resource.MustParse("10Ti")),
	)

	reconcilePool(t, r, "shared")
	holdBinding(t, cli, readBinding(t, cli, "team-a", "chat"), "qwen-72b")

	deleteObject(t, cli, readPool(t, cli, "shared"))
	deleteObject(t, cli, readBinding(t, cli, "team-a", "chat"))
	deleteObject(t, cli, readBinding(t, cli, "team-b", "batch"))

	reconcilePool(t, r, "shared")

	t.Run("the unheld sibling is released anyway", func(t *testing.T) {
		assert.True(t, bindingIsGone(t, cli, "team-b", "batch"))
		assert.NotContains(t, master.held(), "team-b-batch")
	})

	t.Run("the held one keeps its lock and its quota", func(t *testing.T) {
		assert.True(t, systemmeta.IsLocked(readBinding(t, cli, "team-a", "chat")))
		assert.Contains(t, master.held(), "team-a-chat")
	})

	t.Run("the pool is held, and says by which binding", func(t *testing.T) {
		kvcp := readPool(t, cli, "shared")
		assert.True(t, systemmeta.IsLocked(kvcp))
		assert.Equal(t, KVCachePoolReasonHeldByBindings,
			conditionReason(t, kvcp, KVCachePoolConditionReleasable))
		assert.Contains(t, KVCachePoolConditionReleasable.GetMessage(kvcp), "team-a/chat")
		assert.NotContains(t, KVCachePoolConditionReleasable.GetMessage(kvcp), "team-b/batch",
			"the one this pass let go of is not a holder any more")
	})
}

// TestKVCachePoolTeardown_ABackendThatIsGoneDoesNotStrandThePool covers the ordinary teardown order.
func TestKVCachePoolTeardown_ABackendThatIsGoneDoesNotStrandThePool(t *testing.T) {
	master := newFakeMaster()
	address := master.start(t)

	r, cli := newReconciler(
		newReconcileBackend("mooncake-dram", address),
		newTestKVCachePool("shared", "mooncake-dram"),
	)

	reconcilePool(t, r, "shared")
	require.NoError(t, cli.Delete(context.Background(), readBackend(t, cli, "mooncake-dram")))

	deleteObject(t, cli, readPool(t, cli, "shared"))
	reconcilePool(t, r, "shared")

	err := cli.Get(context.Background(), ctrlcli.ObjectKey{Name: "shared"}, new(workercore.KVCachePool))
	assert.Error(t, err,
		"a stack is torn down backend-first, and a pool that waited for one would be undeletable "+
			"for as long as it stayed gone")
}

// TestKVCachePoolTeardown_ASiblingsContestedEntryStaysInTheLedger is the third surface of one rule,
// and the one that had no guard.
//
// The serving pass will not converge a contested domain, and the teardown's SEED render keeps a
// sibling's tenant in the document — both tested above. The teardown's LEDGER removal did neither: it
// deleted by name, so whichever pool was deleted first took the entry away from the claimant that
// keeps it, and the cache under that quota with it. Nothing observable afterwards says a pool that no
// longer exists is why a live domain lost its ceiling.
func TestKVCachePoolTeardown_ASiblingsContestedEntryStaysInTheLedger(t *testing.T) {
	master := newFakeMaster()
	address := master.start(t)

	r, cli := newReconciler(
		newReconcileBackend("mooncake-dram", address),
		newTestKVCachePool("shared", "mooncake-dram"),
		newTestKVCachePool("other", "mooncake-dram"),
		newBoundBinding("team-a", "chat", "shared", "one-domain", resource.MustParse("20Ti")),
	)

	reconcilePool(t, r, "shared")
	require.Contains(t, master.held(), "one-domain",
		"precondition: the first claimant registered it, so there is an entry to take away")

	// A second Binding, under a DIFFERENT pool, claims the same domain. Admission refuses this on
	// create — reaching it means two of them raced one cache, which is the state the contested rule
	// exists for.
	require.NoError(t, cli.Create(context.Background(),
		newBoundBinding("team-b", "batch", "other", "one-domain", resource.MustParse("10Ti"))))

	deleteObject(t, cli, readBinding(t, cli, "team-a", "chat"))
	deleteObject(t, cli, readPool(t, cli, "shared"))
	reconcilePool(t, r, "shared")

	assert.Contains(t, master.held(), "one-domain",
		"the domain is contested, so this pool's teardown is not the event that decides it belongs "+
			"to nobody — the other claimant is still live and still holding the cache")
}

// TestKVCachePoolTeardown_AnAmbiguousBackendHoldsRatherThanLeaks is the counterpart to the test
// above, and the two only differ in WHY the backend did not resolve.
//
// resolveKVCachePoolBackend fails in two ways. BackendNotFound is gone: there is no master, nothing
// is owed to it, and holding the pool would make it undeletable for as long as a backend torn down
// first stayed torn down. BackendNotSingular is NOT gone — the pool names two backends, and one of
// them can be a live master still holding the entries this pool registered while the field named one.
// Collapsed onto the same nil client, the teardown calls that ledger settled and takes the finalizer
// off, and the entries stay on a master that does not record which pool created them: capacity
// nothing can ever reclaim.
func TestKVCachePoolTeardown_AnAmbiguousBackendHoldsRatherThanLeaks(t *testing.T) {
	master := newFakeMaster()
	address := master.start(t)

	r, cli := newReconciler(
		newReconcileBackend("mooncake-dram", address),
		newReconcileBackend("mooncake-ssd", address),
		newTestKVCachePool("shared", "mooncake-dram"),
		newBoundBinding("team-a", "chat", "shared", "team-a-chat", resource.MustParse("20Ti")),
	)

	reconcilePool(t, r, "shared")
	require.Contains(t, master.held(), "team-a-chat",
		"precondition: the entry this teardown must not abandon is on the master")

	deleteObject(t, cli, readBinding(t, cli, "team-a", "chat"))

	// spec.backends becomes ambiguous — an object written before admission took exactly one, or a
	// write that went around the webhook. Nothing happened to the master; only the pool's ability to
	// name it changed.
	kvcp := readPool(t, cli, "shared")
	kvcp.Spec.Backends = []string{"mooncake-dram", "mooncake-ssd"}
	require.NoError(t, cli.Update(context.Background(), kvcp))

	deleteObject(t, cli, readPool(t, cli, "shared"))
	reconcilePool(t, r, "shared")

	assert.Contains(t, master.held(), "team-a-chat",
		"the entry is still owed: a pool that cannot say which master it wrote to has not settled")
	assert.True(t, systemmeta.IsLocked(readBinding(t, cli, "team-a", "chat")),
		"and the Binding is not released over it either")

	held := readPool(t, cli, "shared")
	assert.Equal(t, KVCachePoolReasonLedgerNotReleased,
		conditionReason(t, held, KVCachePoolConditionReleasable),
		"held for a reason that names the ambiguity, not silently")

	// Not a latch. Naming one backend again is all it takes for the same pass to settle.
	held.Spec.Backends = []string{"mooncake-dram"}
	require.NoError(t, cli.Update(context.Background(), held))
	reconcilePool(t, r, "shared")

	assert.NotContains(t, master.held(), "team-a-chat",
		"the pass that CAN resolve its master removes the entry")
	assert.Error(t,
		cli.Get(context.Background(), ctrlcli.ObjectKey{Name: "shared"}, new(workercore.KVCachePool)),
		"and the pool goes with it")
}

// TestKVCachePoolTeardown_AnOrphanBindingIsReleased covers the request a Binding enqueues for a pool
// that was force-deleted out from under it.
func TestKVCachePoolTeardown_AnOrphanBindingIsReleased(t *testing.T) {
	master := newFakeMaster()
	address := master.start(t)

	r, cli := newReconciler(
		newReconcileBackend("mooncake-dram", address),
		newTestKVCachePool("shared", "mooncake-dram"),
		newBoundBinding("team-a", "chat", "shared", "team-a-chat", resource.MustParse("20Ti")),
		newBoundBinding("team-b", "batch", "shared", "team-b-batch", resource.MustParse("10Ti")),
	)

	reconcilePool(t, r, "shared")

	kvcp := readPool(t, cli, "shared")
	systemmeta.Unlock(kvcp)
	require.NoError(t, cli.Update(context.Background(), kvcp))
	deleteObject(t, cli, kvcp)

	deleteObject(t, cli, readBinding(t, cli, "team-a", "chat"))
	reconcilePool(t, r, "shared")

	t.Run("the one being deleted is released", func(t *testing.T) {
		assert.True(t, bindingIsGone(t, cli, "team-a", "chat"))
	})

	t.Run("the one that is not is left alone", func(t *testing.T) {
		assert.True(t, systemmeta.IsLocked(readBinding(t, cli, "team-b", "batch")),
			"the pool it names may be created a moment later, and this lock is what will make that "+
				"pool's own teardown wait for it")
	})
}

// TestKVCachePoolTeardown_AnInterruptedReleaseIsRepairedByTheNextPass is the backstop the acceptance
// names: an entry left on the ledger after its Binding is gone is invisible capacity.
func TestKVCachePoolTeardown_AnInterruptedReleaseIsRepairedByTheNextPass(t *testing.T) {
	master := newFakeMaster()
	address := master.start(t)

	r, cli := newReconciler(
		newReconcileBackend("mooncake-dram", address),
		newTestKVCachePool("shared", "mooncake-dram"),
		newBoundBinding("team-a", "chat", "shared", "team-a-chat", resource.MustParse("20Ti")),
		newBoundBinding("team-b", "batch", "shared", "team-b-batch", resource.MustParse("10Ti")),
	)

	reconcilePool(t, r, "shared")

	// The release is interrupted after the lock came off and before the entry went: the Binding is
	// gone and its tenant is still on the master.
	kvcpb := readBinding(t, cli, "team-a", "chat")
	systemmeta.Unlock(kvcpb)
	require.NoError(t, cli.Update(context.Background(), kvcpb))
	deleteObject(t, cli, kvcpb)
	require.Contains(t, master.held(), "team-a-chat", "the fixture has to start from the leak")

	reconcilePool(t, r, "shared")

	assert.Equal(t, map[string]int64{"team-b-batch": quantityValue("10Ti")}, master.held(),
		"the next pass over that master deletes any entry no Binding of any pool on it claims")
}

// TestKVCachePoolTeardown_AnEntryThisOperatorNeverRegisteredSurvivesTheRepair is the other half of
// that backstop, and the one that keeps it from being data loss.
func TestKVCachePoolTeardown_AnEntryThisOperatorNeverRegisteredSurvivesTheRepair(t *testing.T) {
	master := newFakeMaster()
	address := master.start(t)
	master.ledger["somebody-elses"] = quantityValue("1Ti")
	master.explicit["somebody-elses"] = true

	r, _ := newReconciler(
		newReconcileBackend("mooncake-dram", address),
		newTestKVCachePool("shared", "mooncake-dram"),
		newBoundBinding("team-a", "chat", "shared", "team-a-chat", resource.MustParse("20Ti")),
	)

	reconcilePool(t, r, "shared")

	assert.Contains(t, master.held(), "somebody-elses",
		"an external master may well be serving tenants nobody here created, and the ledger carries "+
			"no label saying whose an entry is")
}

// TestKVCachePoolTeardown_ADomainIsRegisteredBeforeTheStatusNamesIt guards the one gap the pool's
// status cannot cover on its own.
//
// What the delete path may act on is what this operator REGISTERED, and the pool's status is one pass
// behind the ledger: a pass that wrote the entry and then failed its status write leaves a tenant on
// the master that status.domains has never named. Read from that status alone, such an entry would be
// invisible to the delete path while its Binding existed — and permanently once it was gone.
func TestKVCachePoolTeardown_ADomainIsRegisteredBeforeTheStatusNamesIt(t *testing.T) {
	// Marked for deletion and held by nothing, which is the state that drops it from the claims. The
	// pool's status carries no domains at all, standing in for the status write that never landed.
	kvcpb := newBoundBinding("team-a", "chat", "shared", "team-a-chat",
		resource.MustParse("20Ti"))
	systemmeta.Lock(kvcpb)
	kvcpb.DeletionTimestamp = ptr.To(meta.Now())

	backend := newReconcileBackend("mooncake-dram", "unused.example:9003")
	r, _ := newReconciler(backend, newTestKVCachePool("shared", "mooncake-dram"), kvcpb)

	master, err := r.observeKVCachePoolMaster(context.Background(), backend)
	require.NoError(t, err)

	assert.Contains(t, master.registered, "team-a-chat",
		"registered is what may be deleted, and it has to cover an entry no status has named yet")
	assert.NotContains(t, master.domains, "team-a-chat",
		"and it is not a claim: a released binding asks for nothing, which is what deletes the entry")
	assert.Empty(t, master.tenants,
		"nor a tenant, which is what drops it from the rendered policy")
}

// reconcilePoolReturning runs one pass and hands back what it returned, which is the half
// reconcilePool asserts away. A teardown that FAILS and one that HOLDS both leave the object in
// place, and only the returned error tells them apart.
func reconcilePoolReturning(t *testing.T, r *KVCachePoolReconciler, name string) error {
	t.Helper()

	_, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: ctrlcli.ObjectKey{Name: name},
	})
	return err
}

// TestKVCachePoolTeardown_AMissingLedgerIsAlreadyReleased is the first of the three exits from the
// deadlock issue 164 reports: a master running without multi-tenancy holds NO tenant ledger, so a
// teardown that reads one is not waiting for an answer — it has one, and there is nothing to release.
//
// Every release is paired with the read failure that must still HOLD, on the same shape. A rule that
// let any failed ledger read through would release a pool over entries still sitting on a master
// nobody could reach — and would satisfy every "the object was deleted" assertion while doing it.
//
// The two shapes are not decoration either: a pool with nothing of its own registered reaches only the
// policy re-render, and a pool that registered a reuse domain reaches the ledger REMOVAL in front of
// it. The two read the ledger through different calls, so one fixed without the other still deadlocks.
func TestKVCachePoolTeardown_AMissingLedgerIsAlreadyReleased(t *testing.T) {
	const (
		// 409 UNAVAILABLE_IN_CURRENT_MODE: the ledger does not exist, because the master was not
		// started with multi-tenancy.
		multiTenancyOff = `{"success":false,"error_code":-1011,"error_message":"UNAVAILABLE_IN_CURRENT_MODE"}`
		// 503 on the SAME code: a leader whose service plane has not come up. The ledger may well be
		// there and this pass simply could not read it, which is the difference the rule turns on.
		ledgerNotAnswering = `{"success":false,"error_code":-1011}`
	)

	testCases := []struct {
		name string
		// registered puts a reuse domain of this pool's on the master before the teardown runs, which
		// is what routes the pass through the ledger removal as well as through the re-render.
		registered   bool
		refuseStatus int
		refuseBody   string

		wantReleased bool
		// wantDeletes is how many removals the teardown may issue. It takes both values on purpose:
		// asserting only that a missing ledger issues none would pass on a teardown that never
		// removes anything at all.
		wantDeletes int
		// wantHeldBy is the Releasable reason a pool that is not released must carry. Empty, with
		// wantReleased false, means the pass is expected to FAIL outright instead.
		wantHeldBy string
	}{
		{
			name:         "a master answering its ledger releases a pool that registered nothing",
			wantReleased: true,
		},
		{
			name:         "and so does one that holds no ledger at all",
			refuseStatus: 409,
			refuseBody:   multiTenancyOff,
			wantReleased: true,
		},
		{
			name:         "while a ledger that merely did not answer fails the re-render",
			refuseStatus: 503,
			refuseBody:   ledgerNotAnswering,
		},
		{
			name:         "a pool with a domain of its own has its entry removed",
			registered:   true,
			wantReleased: true,
			wantDeletes:  1,
		},
		{
			name:         "and is released by a master with no ledger to remove it from",
			registered:   true,
			refuseStatus: 409,
			refuseBody:   multiTenancyOff,
			wantReleased: true,
		},
		{
			name:         "while a ledger that did not answer holds it",
			registered:   true,
			refuseStatus: 503,
			refuseBody:   ledgerNotAnswering,
			wantHeldBy:   KVCachePoolReasonLedgerNotReleased,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			master := newFakeMaster()
			address := master.start(t)

			objs := []ctrlcli.Object{
				newReconcileBackend("mooncake-dram", address),
				newTestKVCachePool("shared", "mooncake-dram"),
			}
			if tc.registered {
				objs = append(objs, newBoundBinding("team-a", "chat", "shared", "team-a-chat",
					resource.MustParse("20Ti")))
			}
			r, cli := newReconciler(objs...)

			// One serving pass first, so the teardown meets state this controller actually built
			// against a master that DID hold a ledger — which is the order the issue reports and the
			// only one that leaves an entry to reason about.
			reconcilePool(t, r, "shared")
			if tc.registered {
				require.Contains(t, master.held(), "team-a-chat",
					"the entry has to be on the master before its removal means anything")
				require.NotEmpty(t, readPool(t, cli, "shared").Status.Domains)
				deleteObject(t, cli, readBinding(t, cli, "team-a", "chat"))
			}
			deleteObject(t, cli, readPool(t, cli, "shared"))

			master.refuse(tc.refuseStatus, tc.refuseBody)
			err := reconcilePoolReturning(t, r, "shared")

			_, deletes, _, _ := master.counts()
			assert.Equal(t, tc.wantDeletes, deletes,
				"a ledger that does not exist has nothing to remove, and a pass that removed "+
					"something anyway would be acting on an answer it never got")

			kvcp := new(workercore.KVCachePool)
			getErr := cli.Get(context.Background(), ctrlcli.ObjectKey{Name: "shared"}, kvcp)

			if tc.wantReleased {
				require.NoError(t, err)
				assert.True(t, kerrors.IsNotFound(getErr),
					"expected the pool to be released and collected, got %v", getErr)
				if tc.registered {
					assert.NotContains(t, readQuotaPolicyDocument(t, cli, "mooncake-dram"),
						"team-a-chat",
						"the seed is the OTHER place a tenant is written, and the master copies it "+
							"back over its own policy on every container start: skipping the "+
							"re-render would release the pool and leave its tenant to come back")
				}
				return
			}

			require.NoError(t, getErr, "a pool that is not released is still there")
			if tc.wantHeldBy == "" {
				require.Error(t, err,
					"a read that says nothing about whether the entries are still there must not "+
						"be taken as permission to let go")
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.wantHeldBy, conditionReason(t, kvcp, KVCachePoolConditionReleasable))
			assert.Equal(t, KVCachePoolPhaseDeleting, kvcp.Status.Phase)
		})
	}
}

// newManagedReconcileBackend is the backend issue 164 reports on: a MANAGED one running with
// multi-tenancy, publishing an admin address a test's fake master answers on.
//
// The status is written by hand rather than by a pass of the backend's own reconciler, because that
// pass derives the address from the object's name — it would publish the leader Service's DNS name,
// and every read against it would leave this machine looking for a host that does not exist.
func newManagedReconcileBackend(name, admin string) *workercore.KVCacheBackend {
	kvcb := &workercore.KVCacheBackend{
		ObjectMeta: meta.ObjectMeta{Name: name},
		Spec: workercore.KVCacheBackendSpec{
			Type:  "Mooncake",
			Image: "example.com/mooncake:v0",
			Connection: workercore.KVCacheBackendConnection{
				Managed: &workercore.KVCacheBackendManaged{
					Leader: workercore.KVCacheBackendLeader{
						Replicas:     ptr.To[int32](1),
						MultiTenancy: true,
					},
					Members: []workercore.KVCacheBackendMember{{
						NodeSelector:      map[string]string{"kvcache-dram": "true"},
						Medium:            "DRAM",
						CapacityPerMember: resource.MustParse("500Gi"),
					}},
				},
			},
		},
	}
	kvcb.Status.Endpoints = []workercore.KVCacheBackendEndpoint{
		{Name: workercore.KVCacheBackendEndpointNameClient, Address: "mc.example:50051"},
		{Name: workercore.KVCacheBackendEndpointNameAdmin, Address: admin},
	}
	systemmeta.Lock(kvcb)
	return kvcb
}

// TestKVCachePoolTeardown_MultiTenancyWithdrawnMidFlightStillDeletesBothObjects walks issue 164's own
// reproduction to its end, with BOTH reconcilers on one cluster.
//
// The per-rule tests above each pin one exit. This one asserts the property the reproduction is
// written to check and none of them covers: the two teardowns COMPOSE. The pool releases, which is
// what clears the claim, which is what lets the backend's own teardown past the refusal it was
// holding on.
func TestKVCachePoolTeardown_MultiTenancyWithdrawnMidFlightStillDeletesBothObjects(t *testing.T) {
	ctx := context.Background()
	master := newFakeMaster()
	address := master.start(t)

	kvcb := newManagedReconcileBackend("mooncake-dram", address)
	poolReconciler, cli := newReconciler(
		kvcb,
		newTestKVCachePool("shared", "mooncake-dram"),
		newBoundBinding("team-a", "chat", "shared", "team-a-chat", resource.MustParse("20Ti")),
	)
	backendReconciler := &KVCacheBackendReconciler{Client: cli, AdminHTTP: newAdminHTTPClient()}
	reconcileBackend := func(t *testing.T) {
		t.Helper()
		_, err := backendReconciler.Reconcile(ctx, ctrl.Request{
			NamespacedName: ctrlcli.ObjectKey{Name: "mooncake-dram"},
		})
		require.NoError(t, err)
	}

	// Both reach the state the reproduction starts from: the pool holds a ceiling on the master and
	// the backend records the claim.
	reconcilePool(t, poolReconciler, "shared")
	require.Contains(t, master.held(), "team-a-chat")
	require.NotEmpty(t, readBackend(t, cli, "mooncake-dram").Status.UsedBy)

	// Multi-tenancy is turned off on the live backend. The webhook refuses this edit now, so what is
	// written here is the object an operator ALREADY has — the state the refusal cannot reach back
	// into — and the master answers its ledger accordingly.
	live := readBackend(t, cli, "mooncake-dram")
	live.Spec.Connection.Managed.Leader.MultiTenancy = false
	require.NoError(t, cli.Update(ctx, live))
	master.refuse(409, `{"success":false,"error_code":-1011,"error_message":"UNAVAILABLE_IN_CURRENT_MODE"}`)

	deleteObject(t, cli, readBinding(t, cli, "team-a", "chat"))
	deleteObject(t, cli, readPool(t, cli, "shared"))
	require.NoError(t, cli.Delete(ctx, readBackend(t, cli, "mooncake-dram")))

	// The backend goes first and is refused, naming what to go and remove. That refusal is step 2 of
	// the reproduction, and it is correct: what was wrong is that it never ended.
	reconcileBackend(t)
	got := readBackend(t, cli, "mooncake-dram")
	require.True(t, KVCacheBackendConditionDeletable.IsFalse(got))
	require.Contains(t, KVCacheBackendConditionDeletable.GetMessage(got), "KVCachePool/shared")

	reconcilePool(t, poolReconciler, "shared")
	assert.True(t, kerrors.IsNotFound(
		cli.Get(ctx, ctrlcli.ObjectKey{Name: "shared"}, new(workercore.KVCachePool))),
		"the pool's finalizer completes against a master that holds no ledger")
	assert.True(t, bindingIsGone(t, cli, "team-a", "chat"),
		"and takes the binding it was holding with it")

	// Two passes, because the backend releases only once the workloads it rendered are gone rather
	// than once their deletes were accepted.
	reconcileBackend(t)
	reconcileBackend(t)
	assert.True(t, kerrors.IsNotFound(
		cli.Get(ctx, ctrlcli.ObjectKey{Name: "mooncake-dram"}, new(workercore.KVCacheBackend))),
		"and the backend drains on its own once nothing claims it")
}

// conditionReason reads the reason off a condition, which the accessor does not expose.
func conditionReason(t *testing.T, obj any, ct kubeapistatus.ConditionType) string {
	t.Helper()

	switch o := obj.(type) {
	case *workercore.KVCachePool:
		for i := range o.Status.Conditions {
			if o.Status.Conditions[i].Type == string(ct) {
				return o.Status.Conditions[i].Reason
			}
		}
	case *workercore.KVCachePoolBinding:
		for i := range o.Status.Conditions {
			if o.Status.Conditions[i].Type == string(ct) {
				return o.Status.Conditions[i].Reason
			}
		}
	}
	t.Fatalf("condition %s is not on the object", ct)
	return ""
}

// TestKVCachePoolTeardown_AGoneBackendDoesNotPanicOnAReleasingBinding covers the one state in which
// the ledger-removal branches get a nil client.
//
// resolveKVCachePoolAdmin answers nil with unreachable FALSE when the backend is gone, and both
// branches guard on unreachable alone — so a gone backend walked straight into a method call on a
// nil *AdminClient and dereferenced its address.
func TestKVCachePoolTeardown_AGoneBackendDoesNotPanicOnAReleasingBinding(t *testing.T) {
	master := newFakeMaster()
	address := master.start(t)

	r, cli := newReconciler(
		newReconcileBackend("mooncake-dram", address),
		newTestKVCachePool("shared", "mooncake-dram"),
		newBoundBinding("team-a", "chat", "shared", "team-a-chat", resource.MustParse("20Ti")),
	)

	reconcilePool(t, r, "shared")

	// The Binding goes first, and nothing holds it, so it is RELEASING when the teardown runs.
	deleteObject(t, cli, readBinding(t, cli, "team-a", "chat"))
	// Then the backend, which is what makes the resolved client nil with unreachable FALSE.
	require.NoError(t, cli.Delete(context.Background(), readBackend(t, cli, "mooncake-dram")))

	deleteObject(t, cli, readPool(t, cli, "shared"))
	reconcilePool(t, r, "shared")

	err := cli.Get(context.Background(), ctrlcli.ObjectKey{Name: "shared"}, new(workercore.KVCachePool))
	assert.Error(t, err,
		"nothing is owed to a ledger that no longer exists, so the pool goes rather than panicking "+
			"the controller on the way out")
}

// TestKVCachePoolTeardown_AnUndrainedDomainHoldsItsBindingsFinalizer is the SERVING path's version of
// the refusal, and the one that orphans capacity if it is read as success.
//
// The pool is not being deleted here — only the Binding is — so the removal goes through the ledger
// convergence rather than the teardown. A domain that still holds objects is refused there too, and
// treating that pass as converged takes the finalizer off the only object that knows the entry is
// this operator's.
func TestKVCachePoolTeardown_AnUndrainedDomainHoldsItsBindingsFinalizer(t *testing.T) {
	master := newFakeMaster()
	address := master.start(t)

	r, cli := newReconciler(
		newReconcileBackend("mooncake-dram", address),
		newTestKVCachePool("shared", "mooncake-dram"),
		newBoundBinding("team-a", "chat", "shared", "team-a-chat", resource.MustParse("20Ti")),
	)

	reconcilePool(t, r, "shared")
	deleteObject(t, cli, readBinding(t, cli, "team-a", "chat"))

	// -1702 is TENANT_NOT_EMPTY: the domain has objects, so the master keeps the quota. Only the
	// REMOVAL is refused — the ledger still lists and still accepts writes, which is what makes this
	// the serving path rather than a master that stopped answering.
	master.refuseDeletes(409, `{"success":false,"error_code":-1702,"error_message":"TENANT_NOT_EMPTY"}`)
	reconcilePool(t, r, "shared")

	kvcpb := readBinding(t, cli, "team-a", "chat")
	assert.True(t, systemmeta.IsLocked(kvcpb),
		"the entry is still on the master, so the object that owns it may not stop existing")
	assert.Equal(t, KVCachePoolBindingReasonLedgerNotReleased,
		conditionReason(t, kvcpb, KVCachePoolBindingConditionReleasable),
		"and the hold says why, because nothing else in the cluster explains this one")
}

// TestKVCachePoolTeardown_AContestedDomainKeepsItsLedgerEntry holds the line the contested branch
// draws: a domain two Bindings claim is managed for NEITHER, and its entry is left exactly as it is.
//
// Dropping the name from the desired tenants is only half of that. Convergence deletes what is
// registered and not desired, so a name left in registered while taken out of desired is deleted BY
// the pass that claims to leave it alone — and the cache under it goes with the quota, for both
// claimants, on a race admission already refused.
func TestKVCachePoolTeardown_AContestedDomainKeepsItsLedgerEntry(t *testing.T) {
	master := newFakeMaster()
	address := master.start(t)

	r, cli := newReconciler(
		newReconcileBackend("mooncake-dram", address),
		newTestKVCachePool("shared", "mooncake-dram"),
		newBoundBinding("team-a", "chat", "shared", "one-domain", resource.MustParse("20Ti")),
	)

	reconcilePool(t, r, "shared")
	require.Contains(t, master.held(), "one-domain",
		"the first claimant registers it, which is what gives the next pass something to delete")

	// The second create admission refuses; reaching this state means two of them raced one cache.
	require.NoError(t, cli.Create(context.Background(),
		newBoundBinding("team-b", "batch", "shared", "one-domain", resource.MustParse("10Ti"))))

	reconcilePool(t, r, "shared")

	assert.Contains(t, master.held(), "one-domain",
		"neither claimant owns it, so neither may mutate or remove it until the conflict resolves")
}

// TestKVCachePoolTeardown_ASiblingsContestedEntryStaysInTheSeed is the teardown half of a rule the
// serving path already keeps, and the half that was missing.
//
// observeKVCachePoolMaster deliberately drops two kinds of tenant from master.tenants: a contested
// domain, and one whose Binding is releasing. The serving pass puts them back with
// withContestedTenants/withRetainedTenants before it writes the seed, because that document is copied
// over the master's own policy at every leader container start — omitting a live domain takes its
// quota away on the next restart.
//
// The teardown pass renders the same document from the same snapshot, and it is not this pool's own
// tenants that make the omission dangerous: the dropped entries belong to SIBLING pools on the same
// backend, whose reconcile is not running. Deleting any pool would publish a whole document built
// without them, with nothing to notice.
func TestKVCachePoolTeardown_ASiblingsContestedEntryStaysInTheSeed(t *testing.T) {
	master := newFakeMaster()
	address := master.start(t)

	r, cli := newReconciler(
		newReconcileBackend("mooncake-dram", address),
		newTestKVCachePool("going", "mooncake-dram"),
		newTestKVCachePool("staying", "mooncake-dram"),
		newBoundBinding("team-a", "chat", "going", "going-domain", resource.MustParse("20Ti")),
		newBoundBinding("lab", "sweep", "staying", "argued-over", resource.MustParse("5Ti")),
	)

	reconcilePool(t, r, "going")
	reconcilePool(t, r, "staying")

	// The second claimant is refused at admission; reaching this state means two creates raced one
	// cache. From here the domain is managed for NEITHER of them, which is exactly why the entry and
	// the document line have to be left alone rather than rewritten by whoever passes next.
	require.NoError(t, cli.Create(context.Background(),
		newBoundBinding("team-b", "batch", "staying", "argued-over", resource.MustParse("9Ti"))))
	reconcilePool(t, r, "staying")

	require.Contains(t, readQuotaPolicyDocument(t, cli, "mooncake-dram"), "argued-over",
		"the serving path keeps a contested domain in the seed; without that this test would be "+
			"asserting the teardown preserves something that was never there")

	// Force-deleted, so the departing pool reaches its own teardown re-render: a Binding still
	// holding its finalizer stops the pass at HeldByBindings, which never writes the document.
	kvcpb := readBinding(t, cli, "team-a", "chat")
	systemmeta.Unlock(kvcpb)
	require.NoError(t, cli.Update(context.Background(), kvcpb))
	deleteObject(t, cli, kvcpb)

	deleteObject(t, cli, readPool(t, cli, "going"))
	reconcilePool(t, r, "going")

	document := readQuotaPolicyDocument(t, cli, "mooncake-dram")

	assert.Contains(t, document, "argued-over",
		"the contested domain belongs to a pool that is staying, and its ledger entry is still there: "+
			"a seed written without it takes its quota away on the next leader restart")
	assert.NotContains(t, document, "going-domain",
		"and the departing pool's own tenant still has to go, which is what this pass is for")
}

// TestKVCachePoolTeardown_ASiblingsUndrainedEntryStaysInTheSeed is the other kind observeKVCachePool-
// Master drops: a releasing Binding contributes no tenant, so a sibling pool's domain that the master
// refuses to release — because it still holds objects — is absent from the snapshot while being very
// much alive on the master.
func TestKVCachePoolTeardown_ASiblingsUndrainedEntryStaysInTheSeed(t *testing.T) {
	master := newFakeMaster()
	address := master.start(t)

	r, cli := newReconciler(
		newReconcileBackend("mooncake-dram", address),
		newTestKVCachePool("going", "mooncake-dram"),
		newTestKVCachePool("staying", "mooncake-dram"),
		newBoundBinding("team-a", "chat", "going", "going-domain", resource.MustParse("20Ti")),
		newBoundBinding("lab", "sweep", "staying", "still-full", resource.MustParse("5Ti")),
	)

	reconcilePool(t, r, "going")
	reconcilePool(t, r, "staying")
	require.Contains(t, master.held(), "still-full")

	// The sibling's Binding starts releasing, and the master refuses to drop a domain that still
	// holds objects (-1702 is TENANT_NOT_EMPTY). The entry stays; the snapshot stops carrying it.
	master.refuseDeletes(409, `{"success":false,"error_code":-1702,"error_message":"TENANT_NOT_EMPTY"}`)
	deleteObject(t, cli, readBinding(t, cli, "lab", "sweep"))
	reconcilePool(t, r, "staying")

	require.Contains(t, master.held(), "still-full",
		"the master refused the removal, so the domain is still live and still needs its policy")
	require.Contains(t, readQuotaPolicyDocument(t, cli, "mooncake-dram"), "still-full",
		"the serving path puts a retained domain back into the seed; the teardown path is what this "+
			"test is about, so that precondition has to hold first")

	// Force-deleted, for the reason the contested case gives: a pool held by its own Binding never
	// reaches the re-render.
	kvcpb := readBinding(t, cli, "team-a", "chat")
	systemmeta.Unlock(kvcpb)
	require.NoError(t, cli.Update(context.Background(), kvcpb))
	deleteObject(t, cli, kvcpb)

	// The refusal is lifted before the departing pool runs, and it has to be: the fake refuses EVERY
	// removal, so leaving it on stops this teardown at its own domain and the re-render below never
	// happens — the assertion would then pass on a document nobody rewrote. The sibling's entry stays
	// live because only `staying` would ever remove it, and `staying` does not reconcile again here.
	master.refuseDeletes(0, "")

	deleteObject(t, cli, readPool(t, cli, "going"))
	reconcilePool(t, r, "going")

	require.NotContains(t, readQuotaPolicyDocument(t, cli, "mooncake-dram"), "going-domain",
		"the re-render has to have happened for the assertion below to mean anything")

	assert.Contains(t, readQuotaPolicyDocument(t, cli, "mooncake-dram"), "still-full",
		"a domain the master would not release is one a restart must not find unquotaed, and the "+
			"pool re-rendering the document here is not the pool that owns it")
}
