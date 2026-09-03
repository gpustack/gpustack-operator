package worker

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/api/resource"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrlcli "sigs.k8s.io/controller-runtime/pkg/client"
	ctrlfake "sigs.k8s.io/controller-runtime/pkg/client/fake"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	"gpustack.ai/gpustack/pkg/kubeclients/kubernetes/scheme"
)

func newKVCachePoolWebhook(objs ...ctrlcli.Object) *KVCachePoolWebhook {
	cli := ctrlfake.NewClientBuilder().
		WithScheme(scheme.Scheme).
		WithObjects(objs...).
		Build()
	return &KVCachePoolWebhook{Client: cli, APIReader: cli}
}

// newKVCachePool builds a pool that passes every rule against the fixture backend, so a case mutates
// exactly the one thing it is about.
func newKVCachePool() *workercore.KVCachePool {
	return &workercore.KVCachePool{
		ObjectMeta: meta.ObjectMeta{Name: "shared"},
		Spec: workercore.KVCachePoolSpec{
			Backends: []string{"mooncake-dram"},
			Quota:    workercore.KVCachePoolQuota{Total: resource.MustParse("100Ti")},
		},
	}
}

// newMultiTenantKVCacheBackend is the fixture backend a pool may be admitted against: managed, with
// the ledger F5 requires.
func newMultiTenantKVCacheBackend() *workercore.KVCacheBackend {
	kvcb := newKVCacheBackend()
	kvcb.Spec.Connection.Managed.Leader.MultiTenancy = true
	return kvcb
}

// kvCachePoolCase is one admission case. wantMsg empty means the object must be ACCEPTED; otherwise
// it is the substring the refusal must carry.
//
// A refusal is asserted by its message rather than merely by being an error: these rules sit in a
// webhook and not in the schema precisely so the message can say what to do about them.
type kvCachePoolCase struct {
	name    string
	objs    []ctrlcli.Object
	mutate  func(kvcp *workercore.KVCachePool)
	wantMsg string
}

func runKVCachePoolCases(
	t *testing.T, cases []kvCachePoolCase,
	validate func(wh *KVCachePoolWebhook, oldKvcp, newKvcp *workercore.KVCachePool) error,
) {
	t.Helper()

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			objs := c.objs
			if objs == nil {
				objs = []ctrlcli.Object{newMultiTenantKVCacheBackend()}
			}
			oldKvcp, newKvcp := newKVCachePool(), newKVCachePool()
			c.mutate(newKvcp)

			err := validate(newKVCachePoolWebhook(objs...), oldKvcp, newKvcp)
			if c.wantMsg == "" {
				require.NoError(t, err)
				return
			}
			require.Error(t, err)
			require.Contains(t, err.Error(), c.wantMsg)
		})
	}
}

// TestKVCachePoolWebhook_ValidateCreate pins the rules a CRD schema cannot express: a count whose
// refusal has to carry a reason, the two questions asked of the backend this pool names, and a
// quantity the schema types as a string.
func TestKVCachePoolWebhook_ValidateCreate(t *testing.T) {
	runKVCachePoolCases(t, []kvCachePoolCase{
		{name: "the canonical pool", mutate: func(*workercore.KVCachePool) {}},

		// The count. A bound the schema could have carried, held here so the message can say why one
		// is the answer rather than a limit somebody will ask to raise.
		{
			name:    "no backend",
			mutate:  func(p *workercore.KVCachePool) { p.Spec.Backends = nil },
			wantMsg: "exactly 1 backend is supported, and 0 were given",
		},
		{
			name: "two backends",
			objs: []ctrlcli.Object{newMultiTenantKVCacheBackend()},
			mutate: func(p *workercore.KVCachePool) {
				p.Spec.Backends = []string{"mooncake-dram", "mooncake-ssd"}
			},
			wantMsg: "no master can account for bytes held in another",
		},
		{
			name:    "one backend named by the empty string",
			mutate:  func(p *workercore.KVCachePool) { p.Spec.Backends = []string{""} },
			wantMsg: "a backend is named by the name of a KVCacheBackend",
		},

		// The backend itself.
		{
			name:    "a backend nobody created",
			mutate:  func(p *workercore.KVCachePool) { p.Spec.Backends = []string{"absent"} },
			wantMsg: `spec.backends[0]: Not found: "absent"`,
		},
		{
			name:    "a backend running without its tenant ledger",
			objs:    []ctrlcli.Object{newKVCacheBackend()},
			mutate:  func(*workercore.KVCachePool) {},
			wantMsg: "runs without multi-tenancy",
		},
		{
			// An external backend is somebody else's master, started by somebody else's command
			// line. Admission cannot know whether the ledger is on, so it does not pretend to: the
			// reconciler reads the master's own 409 and reports it, which is the check that holds
			// for a managed backend too once it is edited after admission.
			name: "an external backend, whose command line this operator never saw",
			objs: []ctrlcli.Object{&workercore.KVCacheBackend{
				ObjectMeta: meta.ObjectMeta{Name: "mooncake-dram"},
				Spec:       newExternalKVCacheBackendSpec(),
			}},
			mutate: func(*workercore.KVCachePool) {},
		},

		// The ceiling. Its rules are the policy file's own, so a number this pool declares and a
		// number written into a tenant's entry are refused by one validator.
		{
			name:    "a zero ceiling",
			mutate:  func(p *workercore.KVCachePool) { p.Spec.Quota.Total = resource.MustParse("0") },
			wantMsg: "must be greater than 0",
		},
		{
			name:    "a negative ceiling",
			mutate:  func(p *workercore.KVCachePool) { p.Spec.Quota.Total = resource.MustParse("-1") },
			wantMsg: "must be greater than 0",
		},
		{
			// Above the int64 maximum. Quantity.Value() answers this WRONGLY without failing —
			// measured, 10E comes back as 0 — so nothing downstream would report it.
			//
			// The suffix is DECIMAL on purpose. A binary one is already gone by the time admission
			// runs: ParseQuantity saturates 10Ei at the maximum, so the field holds a number that
			// fits and there is nothing left to refuse. A case written with 10Ei would pass while
			// asserting nothing.
			name:    "a ceiling past what a byte count can hold",
			mutate:  func(p *workercore.KVCachePool) { p.Spec.Quota.Total = resource.MustParse("10E") },
			wantMsg: "must not exceed 9223372036854775807",
		},
	}, func(wh *KVCachePoolWebhook, _, newKvcp *workercore.KVCachePool) error {
		_, err := wh.ValidateCreate(context.Background(), newKvcp)
		return err
	})
}

// TestKVCachePoolWebhook_ValidateUpdate covers what an update may and may not move.
func TestKVCachePoolWebhook_ValidateUpdate(t *testing.T) {
	runKVCachePoolCases(t, []kvCachePoolCase{
		{name: "an update that moves nothing", mutate: func(*workercore.KVCachePool) {}},
		{
			name:    "the ceiling raised",
			mutate:  func(p *workercore.KVCachePool) { p.Spec.Quota.Total = resource.MustParse("200Ti") },
			wantMsg: "",
		},
		{
			name:    "the ceiling zeroed",
			mutate:  func(p *workercore.KVCachePool) { p.Spec.Quota.Total = resource.MustParse("0") },
			wantMsg: "must be greater than 0",
		},
		{
			name:    "the backend repointed",
			mutate:  func(p *workercore.KVCachePool) { p.Spec.Backends = []string{"mooncake-ssd"} },
			wantMsg: "backends is immutable",
		},
	}, func(wh *KVCachePoolWebhook, oldKvcp, newKvcp *workercore.KVCachePool) error {
		_, err := wh.ValidateUpdate(context.Background(), oldKvcp, newKvcp)
		return err
	})
}

// TestKVCachePoolWebhook_UpdateDoesNotReReadTheBackend is the rule that keeps a pool deletable.
//
// Removing a finalizer is an UPDATE, and it is the operator's own. A pool whose backend has been
// deleted — the ordinary order when an admin tears a stack down — would be refused by a rule that
// re-asked whether that backend exists, and the pool would then be undeletable for as long as the
// backend stayed gone. The same reasoning covers a backend edited to turn multi-tenancy off: the
// reconciler's Condition is what reports that, level-based, on an object that can still be changed.
func TestKVCachePoolWebhook_UpdateDoesNotReReadTheBackend(t *testing.T) {
	// No backend at all in the cluster, and none of these updates may care.
	wh := newKVCachePoolWebhook()

	for _, c := range []struct {
		name   string
		mutate func(kvcp *workercore.KVCachePool)
	}{
		{"a finalizer being removed", func(p *workercore.KVCachePool) { p.Finalizers = nil }},
		{"the ceiling being raised", func(p *workercore.KVCachePool) {
			p.Spec.Quota.Total = resource.MustParse("200Ti")
		}},
		{"a status write", func(p *workercore.KVCachePool) { p.Status.Phase = "Degraded" }},
	} {
		t.Run(c.name, func(t *testing.T) {
			oldKvcp := newKVCachePool()
			oldKvcp.Finalizers = []string{"worker.gpustack.ai/kv-cache-pool"}
			newKvcp := oldKvcp.DeepCopy()
			c.mutate(newKvcp)

			_, err := wh.ValidateUpdate(context.Background(), oldKvcp, newKvcp)
			require.NoError(t, err)
		})
	}
}

// TestKVCachePoolWebhook_DeleteIsTheFinalizersDecision states where the refusal lives. This handler
// is given the object and nothing else, while the question — is anything still holding this pool —
// is answered from the Bindings that name it.
func TestKVCachePoolWebhook_DeleteIsTheFinalizersDecision(t *testing.T) {
	kvcp := newKVCachePool()
	kvcp.Status.UsedBy = []workercore.KVCacheObjectReference{
		{Kind: "KVCachePoolBinding", Namespace: "team-a", Name: "chat"},
	}

	_, err := newKVCachePoolWebhook().ValidateDelete(context.Background(), kvcp)
	require.NoError(t, err)
}
