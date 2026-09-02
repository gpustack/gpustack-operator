package worker

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/api/resource"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	ctrlcli "sigs.k8s.io/controller-runtime/pkg/client"
	ctrlfake "sigs.k8s.io/controller-runtime/pkg/client/fake"
	ctrlevent "sigs.k8s.io/controller-runtime/pkg/event"
	ctrlreconcile "sigs.k8s.io/controller-runtime/pkg/reconcile"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	"gpustack.ai/gpustack/pkg/kubeclients/kubernetes/scheme"
)

func newTestKVCachePool(name string, backends ...string) *workercore.KVCachePool {
	return &workercore.KVCachePool{
		ObjectMeta: meta.ObjectMeta{Name: name},
		Spec: workercore.KVCachePoolSpec{
			Backends: backends,
			Quota:    workercore.KVCachePoolQuota{Total: resource.MustParse("100Ti")},
		},
	}
}

func newTestKVCachePoolBinding(namespace, name, pool string) *workercore.KVCachePoolBinding {
	return &workercore.KVCachePoolBinding{
		ObjectMeta: meta.ObjectMeta{Namespace: namespace, Name: name},
		Spec: workercore.KVCachePoolBindingSpec{
			PoolRef: workercore.KVCachePoolBindingPoolReference{Name: pool},
			Domain: workercore.KVCachePoolBindingDomain{
				Name:      name,
				BlockSize: 16,
				Dtype:     "bfloat16",
			},
			// Set, because the field is required and its zero value is one the webhook refuses. A
			// fixture that left it at zero would be an object no cluster would have accepted.
			QuotaCeiling: resource.MustParse("1Ti"),
		},
	}
}

// TestIndexKVCachePoolBindingByPool pins what carries a Binding into its pool's pass.
func TestIndexKVCachePoolBindingByPool(t *testing.T) {
	cases := []struct {
		name string
		obj  ctrlcli.Object
		want []string
	}{
		{
			name: "a binding names its pool",
			obj:  newTestKVCachePoolBinding("team-a", "chat", "shared"),
			want: []string{"shared"},
		},
		{
			// The pool may be created a moment later, and the index is the only thing that carries
			// the Binding into the pass that follows. Dropping it here would make a Binding that
			// named its pool early permanently invisible to it.
			name: "a binding naming a pool that does not exist is still indexed",
			obj:  newTestKVCachePoolBinding("team-a", "early", "not-yet"),
			want: []string{"not-yet"},
		},
		{
			name: "a binding naming nothing indexes nothing",
			obj:  newTestKVCachePoolBinding("team-a", "chat", ""),
			want: nil,
		},
		{
			name: "an object of another kind indexes nothing",
			obj:  newTestKVCachePool("shared", "mooncake-dram"),
			want: nil,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			assert.Equal(t, c.want, indexKVCachePoolBindingByPool(c.obj))
		})
	}
}

// TestIndexKVCachePoolByBackend pins the query one pool uses to find its siblings on a shared master.
func TestIndexKVCachePoolByBackend(t *testing.T) {
	cases := []struct {
		name string
		obj  ctrlcli.Object
		want []string
	}{
		{
			name: "the admitted shape, one backend",
			obj:  newTestKVCachePool("shared", "mooncake-dram"),
			want: []string{"mooncake-dram"},
		},
		{
			// Admission takes exactly one, and the index still describes what is STORED: an object
			// written before that rule, or by a client that went around the webhook, has to be
			// findable through the very query its siblings use to find each other.
			name: "a stored pool naming two backends is indexed under both",
			obj:  newTestKVCachePool("legacy", "mooncake-dram", "mooncake-ssd"),
			want: []string{"mooncake-dram", "mooncake-ssd"},
		},
		{
			name: "an empty entry indexes nothing",
			obj:  newTestKVCachePool("blank", ""),
			want: []string{},
		},
		{
			name: "a pool naming no backend indexes nothing",
			obj:  newTestKVCachePool("bare"),
			want: []string{},
		},
		{
			name: "an object of another kind indexes nothing",
			obj:  newTestKVCachePoolBinding("team-a", "chat", "shared"),
			want: nil,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			assert.Equal(t, c.want, indexKVCachePoolByBackend(c.obj))
		})
	}
}

// TestEnqueueKVCachePoolWhenBindingChanged pins the mapping that makes the pool the only reconciled
// object of the pair.
func TestEnqueueKVCachePoolWhenBindingChanged(t *testing.T) {
	r := &KVCachePoolReconciler{}
	ctx := context.Background()

	t.Run("a binding enqueues the pool it names, by bare name", func(t *testing.T) {
		got := r.enqueueKVCachePoolWhenBindingChanged(ctx,
			newTestKVCachePoolBinding("team-a", "chat", "shared"))

		// No namespace: the pool is cluster-scoped, so a Binding from any namespace enqueues the
		// same object. That is what lets one pass see every domain claimed against it.
		assert.Equal(t, []ctrlreconcile.Request{
			{NamespacedName: ctrlcli.ObjectKey{Name: "shared"}},
		}, got)
	})

	t.Run("bindings from different namespaces enqueue one pool", func(t *testing.T) {
		first := r.enqueueKVCachePoolWhenBindingChanged(ctx,
			newTestKVCachePoolBinding("team-a", "chat", "shared"))
		second := r.enqueueKVCachePoolWhenBindingChanged(ctx,
			newTestKVCachePoolBinding("team-b", "batch", "shared"))

		assert.Equal(t, first, second)
	})

	t.Run("a binding naming nothing enqueues nothing", func(t *testing.T) {
		assert.Empty(t, r.enqueueKVCachePoolWhenBindingChanged(ctx,
			newTestKVCachePoolBinding("team-a", "chat", "")))
	})
}

// TestKVCachePoolPredicate is the guard against the loop that never settles.
//
// Every write this operator makes to a pool is a status or a finalizer write, and neither moves the
// generation or the deletion timestamp. A predicate that let those through would have each pass
// schedule the next one, forever, against a pool that had already converged.
func TestKVCachePoolPredicate(t *testing.T) {
	predicate := kvCachePoolPredicate()

	settled := newTestKVCachePool("shared", "mooncake-dram")
	settled.Generation = 3

	cases := []struct {
		name    string
		mutate  func(kvcp *workercore.KVCachePool)
		wantRun bool
	}{
		{
			name:    "our own status write",
			mutate:  func(p *workercore.KVCachePool) { p.Status.Phase = "Ready" },
			wantRun: false,
		},
		{
			name: "our own finalizer write",
			mutate: func(p *workercore.KVCachePool) {
				p.Finalizers = []string{"worker.gpustack.ai/kv-cache-pool"}
			},
			wantRun: false,
		},
		{
			name: "a resync that moved nothing at all",
			mutate: func(p *workercore.KVCachePool) {
				p.ResourceVersion = "999"
			},
			wantRun: false,
		},
		{
			name: "a spec edit, which the api server records as a new generation",
			mutate: func(p *workercore.KVCachePool) {
				p.Spec.Quota.Total = resource.MustParse("200Ti")
				p.Generation = 4
			},
			wantRun: true,
		},
		{
			name: "the object being marked for deletion",
			mutate: func(p *workercore.KVCachePool) {
				p.DeletionTimestamp = &meta.Time{Time: time.Unix(1756684800, 0)}
			},
			wantRun: true,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			updated := settled.DeepCopy()
			c.mutate(updated)

			assert.Equal(t, c.wantRun, predicate.Update(ctrlevent.UpdateEvent{
				ObjectOld: settled,
				ObjectNew: updated,
			}))
		})
	}

	t.Run("the final removal is not a pass", func(t *testing.T) {
		// Nothing is left to converge, and what this pool held outside the cluster was released by
		// the finalizer while the object still existed.
		assert.False(t, predicate.Delete(ctrlevent.DeleteEvent{Object: settled}))
	})

	t.Run("a create is always a pass", func(t *testing.T) {
		assert.True(t, predicate.Create(ctrlevent.CreateEvent{Object: settled}))
	})
}

// TestKVCachePoolIndexesAnswerTheQueriesTheyExistFor drives both indexes the way a pass does.
//
// The extractor tests above say what a key WOULD be; this says the query works — which is the part
// that fails at runtime rather than at compile time, because a List against an index nobody
// registered is an error and not an empty result.
func TestKVCachePoolIndexesAnswerTheQueriesTheyExistFor(t *testing.T) {
	cli := ctrlfake.NewClientBuilder().
		WithScheme(scheme.Scheme).
		WithIndex(&workercore.KVCachePoolBinding{},
			IndexingKVCachePoolBindingByPool, indexKVCachePoolBindingByPool).
		WithIndex(&workercore.KVCachePool{},
			IndexingKVCachePoolByBackend, indexKVCachePoolByBackend).
		WithObjects(
			newTestKVCachePool("shared", "mooncake-dram"),
			// A second pool on the SAME master. Finding it is F7: the ledger and the policy file
			// converge per master, so a pass that saw only its own pool would erase this one's
			// tenants on every reconcile.
			newTestKVCachePool("research", "mooncake-dram"),
			newTestKVCachePool("cold", "mooncake-ssd"),
			newTestKVCachePoolBinding("team-a", "chat", "shared"),
			newTestKVCachePoolBinding("team-b", "batch", "shared"),
			newTestKVCachePoolBinding("team-c", "eval", "research"),
		).
		Build()
	ctx := context.Background()

	t.Run("one query returns a pool's bindings, across namespaces", func(t *testing.T) {
		list := &workercore.KVCachePoolBindingList{}
		require.NoError(t, cli.List(ctx, list,
			ctrlcli.MatchingFields{IndexingKVCachePoolBindingByPool: "shared"}))

		names := make([]string, 0, len(list.Items))
		for i := range list.Items {
			names = append(names, list.Items[i].Namespace+"/"+list.Items[i].Name)
		}
		assert.ElementsMatch(t, []string{"team-a/chat", "team-b/batch"}, names)
	})

	t.Run("one query returns every pool on one master", func(t *testing.T) {
		list := &workercore.KVCachePoolList{}
		require.NoError(t, cli.List(ctx, list,
			ctrlcli.MatchingFields{IndexingKVCachePoolByBackend: "mooncake-dram"}))

		names := make([]string, 0, len(list.Items))
		for i := range list.Items {
			names = append(names, list.Items[i].Name)
		}
		assert.ElementsMatch(t, []string{"shared", "research"}, names,
			"the pool on another backend is not a sibling and must not appear")
	})

	t.Run("a pool nobody bound answers empty rather than failing", func(t *testing.T) {
		list := &workercore.KVCachePoolBindingList{}
		require.NoError(t, cli.List(ctx, list,
			ctrlcli.MatchingFields{IndexingKVCachePoolBindingByPool: "cold"}))
		assert.Empty(t, list.Items)
	})
}

// TestKVCachePoolReconcile_AbsentPoolIsNotAFailure covers the request a Binding enqueues for a pool
// nobody created. The mapping deliberately lets it through, so the pass has to answer it quietly.
func TestKVCachePoolReconcile_AbsentPoolIsNotAFailure(t *testing.T) {
	r := &KVCachePoolReconciler{
		Client: ctrlfake.NewClientBuilder().
			WithScheme(scheme.Scheme).
			// The absent-pool pass looks for orphan Bindings through this index, so a fake without it
			// would fail on the index rather than on the behavior under test.
			WithIndex(&workercore.KVCachePoolBinding{},
				IndexingKVCachePoolBindingByPool, indexKVCachePoolBindingByPool).
			Build(),
	}

	got, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: ctrlcli.ObjectKey{Name: "not-yet"},
	})

	require.NoError(t, err)
	assert.Equal(t, ctrl.Result{}, got, "and it is not requeued: the create will bring it back")
}
