package worker

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/api/resource"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrlcli "sigs.k8s.io/controller-runtime/pkg/client"
	ctrlfake "sigs.k8s.io/controller-runtime/pkg/client/fake"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	"gpustack.ai/gpustack/pkg/kubeclients/kubernetes/scheme"
)

func newKVCachePoolBindingWebhook(objs ...ctrlcli.Object) *KVCachePoolBindingWebhook {
	cli := ctrlfake.NewClientBuilder().
		WithScheme(scheme.Scheme).
		WithObjects(objs...).
		Build()
	return &KVCachePoolBindingWebhook{Client: cli, APIReader: cli}
}

// newKVCachePoolBinding builds a Binding that passes every rule against the fixture pool.
func newKVCachePoolBinding() *workercore.KVCachePoolBinding {
	return &workercore.KVCachePoolBinding{
		ObjectMeta: meta.ObjectMeta{Namespace: "team-a", Name: "chat"},
		Spec: workercore.KVCachePoolBindingSpec{
			PoolRef: workercore.KVCachePoolBindingPoolReference{Name: "shared"},
			Domain: workercore.KVCachePoolBindingDomain{
				Name:      "team-a-chat",
				BlockSize: 16,
				Dtype:     "bfloat16",
			},
			QuotaCeiling: resource.MustParse("20Ti"),
		},
	}
}

// otherKVCachePoolBinding is a Binding in a DIFFERENT namespace, for the uniqueness cases. Its own
// domain differs, so a case that wants a collision says so by setting one.
func otherKVCachePoolBinding(domain string) *workercore.KVCachePoolBinding {
	kvcpb := newKVCachePoolBinding()
	kvcpb.Namespace, kvcpb.Name = "team-b", "batch"
	kvcpb.Spec.Domain.Name = domain
	return kvcpb
}

type kvCachePoolBindingCase struct {
	name    string
	objs    []ctrlcli.Object
	mutate  func(kvcpb *workercore.KVCachePoolBinding)
	wantMsg string
}

func runKVCachePoolBindingCases(
	t *testing.T, cases []kvCachePoolBindingCase,
	validate func(wh *KVCachePoolBindingWebhook, oldKvcpb, newKvcpb *workercore.KVCachePoolBinding) error,
) {
	t.Helper()

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			objs := c.objs
			if objs == nil {
				objs = []ctrlcli.Object{newKVCachePool()}
			}
			oldKvcpb, newKvcpb := newKVCachePoolBinding(), newKVCachePoolBinding()
			c.mutate(newKvcpb)

			err := validate(newKVCachePoolBindingWebhook(objs...), oldKvcpb, newKvcpb)
			if c.wantMsg == "" {
				require.NoError(t, err)
				return
			}
			require.Error(t, err)
			require.Contains(t, err.Error(), c.wantMsg)
		})
	}
}

// TestKVCachePoolBindingWebhook_ValidateCreate pins the shape rules and the two cross-object reads.
func TestKVCachePoolBindingWebhook_ValidateCreate(t *testing.T) {
	runKVCachePoolBindingCases(t, []kvCachePoolBindingCase{
		{name: "the canonical binding", mutate: func(*workercore.KVCachePoolBinding) {}},

		// The pool.
		{
			name:    "no pool named",
			mutate:  func(b *workercore.KVCachePoolBinding) { b.Spec.PoolRef.Name = "" },
			wantMsg: "a Binding grants exactly one pool",
		},
		{
			name:    "a pool nobody created",
			mutate:  func(b *workercore.KVCachePoolBinding) { b.Spec.PoolRef.Name = "absent" },
			wantMsg: `spec.poolRef.name: Not found: "absent"`,
		},

		// The reuse domain's shape.
		{
			name:    "a domain with no name",
			mutate:  func(b *workercore.KVCachePoolBinding) { b.Spec.Domain.Name = "" },
			wantMsg: "a reuse domain with no name",
		},
		{
			name:    "a domain named like a Kubernetes object",
			mutate:  func(b *workercore.KVCachePoolBinding) { b.Spec.Domain.Name = "team-a-chat-v2" },
			wantMsg: "",
		},
		{
			name:    "a domain with an underscore, which a DNS label refuses",
			mutate:  func(b *workercore.KVCachePoolBinding) { b.Spec.Domain.Name = "team_a" },
			wantMsg: "a reuse domain is named like a Kubernetes object",
		},
		{
			// The master's own reserved prefix. It is refused by the DNS-1123 rule too, and the case
			// is here because the two rules answer for different reasons: one is this API's shape,
			// the other is what the leader would do with the name.
			name:    "a domain on the leader's reserved prefix",
			mutate:  func(b *workercore.KVCachePoolBinding) { b.Spec.Domain.Name = "_internal" },
			wantMsg: `must not start with "_"`,
		},
		{
			name:    "a domain in capitals",
			mutate:  func(b *workercore.KVCachePoolBinding) { b.Spec.Domain.Name = "Team-A" },
			wantMsg: "a reuse domain is named like a Kubernetes object",
		},
		{
			name:    "a block size of zero",
			mutate:  func(b *workercore.KVCachePoolBinding) { b.Spec.Domain.BlockSize = 0 },
			wantMsg: "the number of tokens one cache block holds",
		},
		{
			name:    "a negative block size",
			mutate:  func(b *workercore.KVCachePoolBinding) { b.Spec.Domain.BlockSize = -16 },
			wantMsg: "must be greater than 0",
		},
		{
			name:    "no dtype",
			mutate:  func(b *workercore.KVCachePoolBinding) { b.Spec.Domain.Dtype = "" },
			wantMsg: "the element type the cached tensors carry",
		},
		{
			name:    "a dtype in capitals",
			mutate:  func(b *workercore.KVCachePoolBinding) { b.Spec.Domain.Dtype = "BFloat16" },
			wantMsg: "two spellings of one type would read as two types",
		},
		{
			// The set is deliberately not enumerated: it belongs to whatever spec owns workloads, and
			// enumerating it here would make a new engine dtype an API change to this group.
			name:    "a dtype this API has never heard of",
			mutate:  func(b *workercore.KVCachePoolBinding) { b.Spec.Domain.Dtype = "fp4_e2m1" },
			wantMsg: "",
		},

		// The ceiling. There is no case for omitting it: the field is required, so the schema refuses
		// such an object before any webhook is consulted, and a case here would be asserting against
		// a path admission never takes.
		{
			name: "a ceiling of zero",
			mutate: func(b *workercore.KVCachePoolBinding) {
				b.Spec.QuotaCeiling = resource.MustParse("0")
			},
			wantMsg: "must be greater than 0",
		},
		{
			name: "a ceiling exactly the pool's own",
			mutate: func(b *workercore.KVCachePoolBinding) {
				b.Spec.QuotaCeiling = resource.MustParse("100Ti")
			},
			wantMsg: "",
		},
		{
			name: "a ceiling larger than the whole pool",
			mutate: func(b *workercore.KVCachePoolBinding) {
				b.Spec.QuotaCeiling = resource.MustParse("200Ti")
			},
			wantMsg: "must not exceed the pool's own ceiling of 100Ti",
		},

		// The domain registry, cluster-wide.
		{
			name:    "a domain another namespace already registered",
			objs:    []ctrlcli.Object{newKVCachePool(), otherKVCachePoolBinding("team-a-chat")},
			mutate:  func(*workercore.KVCachePoolBinding) {},
			wantMsg: `already registered by team-b/batch`,
		},
		{
			name:    "a domain nobody else holds",
			objs:    []ctrlcli.Object{newKVCachePool(), otherKVCachePoolBinding("team-b-batch")},
			mutate:  func(*workercore.KVCachePoolBinding) {},
			wantMsg: "",
		},
	}, func(wh *KVCachePoolBindingWebhook, _, newKvcpb *workercore.KVCachePoolBinding) error {
		_, err := wh.ValidateCreate(context.Background(), newKvcpb)
		return err
	})
}

// TestKVCachePoolBindingWebhook_ValidateUpdate covers what an update may and may not move. The three
// frozen domain fields are the reason this webhook exists rather than a note in a document: two of
// them fail as wrong tensors rather than as errors.
func TestKVCachePoolBindingWebhook_ValidateUpdate(t *testing.T) {
	runKVCachePoolBindingCases(t, []kvCachePoolBindingCase{
		{name: "an update that moves nothing", mutate: func(*workercore.KVCachePoolBinding) {}},
		{
			name:    "the pool repointed",
			mutate:  func(b *workercore.KVCachePoolBinding) { b.Spec.PoolRef.Name = "other" },
			wantMsg: "poolRef is immutable",
		},
		{
			name:    "the domain renamed",
			mutate:  func(b *workercore.KVCachePoolBinding) { b.Spec.Domain.Name = "team-a-chat-v2" },
			wantMsg: "name is immutable",
		},
		{
			name:    "the block size changed",
			mutate:  func(b *workercore.KVCachePoolBinding) { b.Spec.Domain.BlockSize = 32 },
			wantMsg: "reading them back at a new one is silent corruption",
		},
		{
			name:    "the dtype changed",
			mutate:  func(b *workercore.KVCachePoolBinding) { b.Spec.Domain.Dtype = "float16" },
			wantMsg: "reading them back as another one is silent corruption",
		},
		{
			name: "the ceiling lowered",
			mutate: func(b *workercore.KVCachePoolBinding) {
				b.Spec.QuotaCeiling = resource.MustParse("10Ti")
			},
			wantMsg: "",
		},
		{
			name: "the ceiling raised past the pool's own",
			mutate: func(b *workercore.KVCachePoolBinding) {
				b.Spec.QuotaCeiling = resource.MustParse("200Ti")
			},
			wantMsg: "must not exceed the pool's own ceiling",
		},
	}, func(wh *KVCachePoolBindingWebhook, oldKvcpb, newKvcpb *workercore.KVCachePoolBinding) error {
		_, err := wh.ValidateUpdate(context.Background(), oldKvcpb, newKvcpb)
		return err
	})
}

// TestKVCachePoolBindingWebhook_UpdateReadsThePoolOnlyForAMovedCeiling is what keeps a Binding
// deletable.
//
// Removing a finalizer is an UPDATE, and a pool deleted before its Bindings is the ordinary teardown
// order. An update that leaves the ceiling where it was must therefore not need the pool at all —
// otherwise the Binding would be undeletable for as long as the pool stayed gone.
func TestKVCachePoolBindingWebhook_UpdateReadsThePoolOnlyForAMovedCeiling(t *testing.T) {
	// No pool anywhere in the cluster.
	wh := newKVCachePoolBindingWebhook()

	t.Run("an update that leaves the ceiling alone needs no pool", func(t *testing.T) {
		for _, c := range []struct {
			name   string
			mutate func(kvcpb *workercore.KVCachePoolBinding)
		}{
			{"a finalizer being removed", func(b *workercore.KVCachePoolBinding) { b.Finalizers = nil }},
			{"a status write", func(b *workercore.KVCachePoolBinding) { b.Status.Phase = "Degraded" }},
			{"the ceiling rewritten in another spelling", func(b *workercore.KVCachePoolBinding) {
				b.Spec.QuotaCeiling = resource.MustParse("21990232555520")
			}},
		} {
			t.Run(c.name, func(t *testing.T) {
				oldKvcpb := newKVCachePoolBinding()
				oldKvcpb.Finalizers = []string{"worker.gpustack.ai/kv-cache-pool-binding"}
				newKvcpb := oldKvcpb.DeepCopy()
				c.mutate(newKvcpb)

				_, err := wh.ValidateUpdate(context.Background(), oldKvcpb, newKvcpb)
				require.NoError(t, err)
			})
		}
	})

	t.Run("an update that moves the ceiling does need it", func(t *testing.T) {
		oldKvcpb := newKVCachePoolBinding()
		newKvcpb := oldKvcpb.DeepCopy()
		newKvcpb.Spec.QuotaCeiling = resource.MustParse("30Ti")

		_, err := wh.ValidateUpdate(context.Background(), oldKvcpb, newKvcpb)
		require.Error(t, err)
		require.Contains(t, err.Error(), `spec.poolRef.name: Not found: "shared"`)
	})
}

// TestKVCachePoolBindingWebhook_ADomainIsNotClaimedByTheObjectUnderAdmission guards the obvious way
// the uniqueness check breaks: a re-admitted update finds the stored copy of the very object being
// admitted and refuses it against itself.
func TestKVCachePoolBindingWebhook_ADomainIsNotClaimedByTheObjectUnderAdmission(t *testing.T) {
	stored := newKVCachePoolBinding()
	wh := newKVCachePoolBindingWebhook(newKVCachePool(), stored)

	_, err := wh.ValidateCreate(context.Background(), newKVCachePoolBinding())
	require.NoError(t, err)
}

// TestKVCachePoolBindingWebhook_ADuplicateDomainIsTrueOfOneMasterAndOfTwo pins the refusal's message
// against the one way it can be wrong while naming the right objects.
//
// The refusal holds whether or not the two Bindings are served by the same master, and its REASON does
// not: on one master the two would share cache and one ledger entry, on two independent backends they
// share neither. A message stating only the first sends an operator on two backends to investigate a
// collision that cannot occur between two ledgers, which is worse than a vague message — it is a
// correct object with a wrong causality.
func TestKVCachePoolBindingWebhook_ADuplicateDomainIsTrueOfOneMasterAndOfTwo(t *testing.T) {
	wh := newKVCachePoolBindingWebhook(newKVCachePool(), otherKVCachePoolBinding("team-a-chat"))

	_, err := wh.ValidateCreate(context.Background(), newKVCachePoolBinding())
	require.Error(t, err)

	msg := err.Error()
	assert.Contains(t, msg, "team-b/batch",
		"the refusal names the Binding holding the domain, which is where the operator looks first")
	assert.Contains(t, msg, "registered once cluster-wide",
		"the scope of the registry is what the refusal actually turns on")
	assert.Contains(t, msg, "Served by one master, the two would share cache",
		"the collision is stated WITH the condition that produces it, never on its own")
	assert.Contains(t, msg, "Served by two independent backends, they share nothing",
		"the case with no sharing at all is stated too, so nobody goes looking for a collision")
	assert.Contains(t, msg, "does not rescue a needed",
		"the advice to rename dead-ends on the one name an engine picks, so it says so: renaming "+
			"is admitted and the Pods that made the domain necessary still write elsewhere")
	assert.NotContains(t, msg, "exception",
		"calling \"default\" an exception reads as an exemption from the uniqueness rule this "+
			"very message is enforcing, which sends the reader back to retry the refused Binding")
}

// TestKVCachePoolBindingWebhook_DeleteIsTheFinalizersDecision states where the refusal lives: this
// handler sees the object, while the questions — is a workload still holding it, has the tenant
// drained — are answered from elsewhere.
func TestKVCachePoolBindingWebhook_DeleteIsTheFinalizersDecision(t *testing.T) {
	kvcpb := newKVCachePoolBinding()
	kvcpb.Status.UsedBy = []workercore.KVCacheObjectReference{
		{Kind: "Deployment", Namespace: "", Name: "chat"},
	}

	_, err := newKVCachePoolBindingWebhook().ValidateDelete(context.Background(), kvcpb)
	require.NoError(t, err)
}
