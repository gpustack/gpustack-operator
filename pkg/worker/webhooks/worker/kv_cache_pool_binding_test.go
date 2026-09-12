package worker

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	ctrlcli "sigs.k8s.io/controller-runtime/pkg/client"
	ctrlfake "sigs.k8s.io/controller-runtime/pkg/client/fake"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	"gpustack.ai/gpustack/pkg/kubeclients/kubernetes/scheme"
	workerctrl "gpustack.ai/gpustack/pkg/worker/controllers/worker"
)

func newKVCachePoolBindingWebhook(objs ...ctrlcli.Object) *KVCachePoolBindingWebhook {
	cli := ctrlfake.NewClientBuilder().
		WithScheme(scheme.Scheme).
		WithObjects(objs...).
		Build()
	return &KVCachePoolBindingWebhook{Client: cli, APIReader: cli}
}

type staleKVCachePoolReader struct {
	ctrlcli.Reader
}

func (r staleKVCachePoolReader) Get(
	ctx context.Context, key ctrlcli.ObjectKey, obj ctrlcli.Object, opts ...ctrlcli.GetOption,
) error {
	getOpts := (&ctrlcli.GetOptions{}).ApplyOptions(opts)
	if getOpts.Raw != nil && getOpts.Raw.ResourceVersion == "0" {
		return kerrors.NewNotFound(schema.GroupResource{
			Group: workercore.GroupVersion.Group, Resource: "kvcachepools",
		}, key.Name)
	}
	return r.Reader.Get(ctx, key, obj, opts...)
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

		// The domain registry, per master: one backend serving both pools refuses, two do not.
		{
			name:    "a domain another namespace already registered on the same master",
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

// TestKVCachePoolBindingWebhook_ADuplicateDomainIsTrueOfOneMasterOnly pins both halves of the
// per-master scope: refused where one master serves both Bindings, admitted where the colliding
// claim lives on a master that serves neither the same pool nor the same backend.
//
// The message is pinned clause by clause for the reason its predecessor was: a refusal that names
// the right objects with the wrong causality sends the operator investigating a collision that
// cannot occur. Naming the shared backend is the load-bearing part — the check read both pools to
// decide, and the message is the only place that reading surfaces.
func TestKVCachePoolBindingWebhook_ADuplicateDomainIsTrueOfOneMasterOnly(t *testing.T) {
	wh := newKVCachePoolBindingWebhook(newKVCachePool(), otherKVCachePoolBinding("team-a-chat"))

	_, err := wh.ValidateCreate(context.Background(), newKVCachePoolBinding())
	require.Error(t, err)

	msg := err.Error()
	assert.Contains(t, msg, "team-b/batch",
		"the refusal names the Binding holding the domain, which is where the operator looks first")
	assert.Contains(t, msg, "both Bindings' pools name mooncake-dram",
		"the shared backend is the fact the refusal turns on, so it is stated, not implied")
	assert.NotContains(t, msg, "pools are served by",
		"the intersection establishes that both pools NAME the backend; a pool naming several is "+
			"served by none of them, so \"served by\" would assert what was never read")
	assert.Contains(t, msg, "Two masters hold two ledgers",
		"the admitted case is stated too, so nobody reads the refusal as cluster-wide")
	assert.Contains(t, msg, "does not rescue a needed",
		"the advice to rename dead-ends on the one name an engine picks, so it says so: renaming "+
			"is admitted and the Pods that made the domain necessary still write elsewhere")
	assert.NotContains(t, msg, "exception",
		"calling \"default\" an exception reads as an exemption from the uniqueness rule this "+
			"very message is enforcing, which sends the reader back to retry the refused Binding")
}

func TestKVCachePoolBindingWebhook_AStalePoolMissDoesNotAdmitADuplicateDomain(t *testing.T) {
	holderPool := newKVCachePool()
	holderPool.Name = "other-pool"

	holder := otherKVCachePoolBinding("team-a-chat")
	holder.Spec.PoolRef.Name = holderPool.Name

	cached := ctrlfake.NewClientBuilder().
		WithScheme(scheme.Scheme).
		WithObjects(newKVCachePool(), holder).
		Build()
	apiReader := ctrlfake.NewClientBuilder().
		WithScheme(scheme.Scheme).
		WithObjects(holderPool).
		Build()
	wh := &KVCachePoolBindingWebhook{
		Client:    cached,
		APIReader: staleKVCachePoolReader{Reader: apiReader},
	}

	_, err := wh.ValidateCreate(context.Background(), newKVCachePoolBinding())
	require.Error(t, err)
	require.Contains(t, err.Error(), "already registered by team-b/batch")
}

// TestKVCachePoolBindingWebhook_TheSameDomainOnAnotherMasterIsAdmitted is the #166 case: two
// independent backends, each holding the one domain a no-tenant engine writes under. The second
// claim must be ADMITTED — the two masters hold two ledgers, and the refusal would be this check's
// own scope error rather than a fault between the Bindings.
func TestKVCachePoolBindingWebhook_TheSameDomainOnAnotherMasterIsAdmitted(t *testing.T) {
	otherPool := newKVCachePool()
	otherPool.Name = "other-pool"
	otherPool.Spec.Backends = []string{"mooncake-other"}

	holder := otherKVCachePoolBinding("default")
	holder.Spec.PoolRef.Name = "other-pool"

	candidate := newKVCachePoolBinding()
	candidate.Spec.Domain.Name = "default"

	wh := newKVCachePoolBindingWebhook(newKVCachePool(), otherPool, holder)
	_, err := wh.ValidateCreate(context.Background(), candidate)
	require.NoError(t, err,
		"a domain held on a DIFFERENT master collides with nothing: two ledgers, two key spaces")
}

// TestKVCachePoolBindingWebhook_APoolNamingNoBackendCollidesWithNothing pins the verdict when the
// holder's pool names NO master: an explicitly empty spec.backends, which the API server accepts
// because the field is required but carries no minItems. No master serves the holder, so its claim
// contests nothing and the candidate is admitted.
//
// This is a POSITIVE BASELINE, not a new rule. The verdict was already this before poolBackends
// normalized an empty list to nil — reached by intersecting against an empty set, one pool read
// later.
//
// WHAT IT DOES AND DOES NOT CATCH, measured by removing the normalization and re-running: it stays
// GREEN, because the normalization changes which path produces the verdict and not the verdict. So
// it fails on a regression that turns an empty list into a collision, and on one that dereferences
// it, and NOT on the normalization being dropped. Nothing here guards that; it is an efficiency
// and a readable equivalence, and it is documented as such rather than tested.
func TestKVCachePoolBindingWebhook_APoolNamingNoBackendCollidesWithNothing(t *testing.T) {
	emptyPool := newKVCachePool()
	emptyPool.Name = "empty-pool"
	emptyPool.Spec.Backends = []string{}

	holder := otherKVCachePoolBinding("default")
	holder.Spec.PoolRef.Name = "empty-pool"

	candidate := newKVCachePoolBinding()
	candidate.Spec.Domain.Name = "default"

	wh := newKVCachePoolBindingWebhook(newKVCachePool(), emptyPool, holder)
	_, err := wh.ValidateCreate(context.Background(), candidate)
	require.NoError(t, err,
		"a holder whose pool names no backend is served by no master, so it holds the domain "+
			"against nothing this candidate could collide with")
}

// TestKVCachePoolBindingWebhook_AClaimWhosePoolIsGoneCollidesWithNothing covers the holder whose
// pool no longer exists: no master serves it, so its registry entry bars nobody. The reconciler's
// per-master contested set agrees — it only ever sees Bindings through pools on its backend.
func TestKVCachePoolBindingWebhook_AClaimWhosePoolIsGoneCollidesWithNothing(t *testing.T) {
	holder := otherKVCachePoolBinding("team-a-chat")
	holder.Spec.PoolRef.Name = "deleted-pool"

	wh := newKVCachePoolBindingWebhook(newKVCachePool(), holder)
	_, err := wh.ValidateCreate(context.Background(), newKVCachePoolBinding())
	require.NoError(t, err)
}

// newExternalKVCachePoolBackend is the backend this operator did not start. It carries no managed
// branch, so there is no multiTenancy field to read and the only answer about its ledger is what a
// controller observed against the running master.
func newExternalKVCachePoolBackend() *workercore.KVCacheBackend {
	kvcb := newKVCacheBackend()
	kvcb.Spec.Connection.Managed = nil
	kvcb.Spec.Connection.External = &workercore.KVCacheBackendExternal{
		Endpoints: []workercore.KVCacheBackendEndpoint{
			{Name: workercore.KVCacheBackendEndpointNameClient, Address: "mooncake.invalid:50051"},
			{Name: workercore.KVCacheBackendEndpointNameAdmin, Address: "mooncake.invalid:9003"},
		},
	}
	return kvcb
}

// kvCachePoolWithLedgerVerdict is the pool as a controller left it after asking the master.
func kvCachePoolWithLedgerVerdict(reason string) *workercore.KVCachePool {
	kvcp := newKVCachePool()
	workerctrl.KVCachePoolConditionQuotaLedgerAvailable.False(kvcp, reason,
		"read from the master on the last pass")
	return kvcp
}

// TestKVCachePoolBindingWebhook_ASecondDistinctDomainNeedsAMasterThatSeparates covers both halves of
// the rule, and the pairing is what makes either half mean anything: a check that refused every second
// domain would pass the refusal cases alone, and one that refused none would pass the admitted cases
// alone.
func TestKVCachePoolBindingWebhook_ASecondDistinctDomainNeedsAMasterThatSeparates(t *testing.T) {
	for _, c := range []struct {
		name     string
		objs     []ctrlcli.Object
		wantMsg  string
		wantWarn string
	}{
		{
			// The positive half. A master holding a ledger keeps two domains apart, so the second one is
			// admitted -- and warned about, because the store is only one of the two keepers.
			name: "a master with a ledger takes a second domain",
			objs: []ctrlcli.Object{
				newKVCachePool(), newMultiTenantKVCacheBackend(),
				otherKVCachePoolBinding("team-b-batch"),
			},
			wantWarn: "needs both a master holding a tenant ledger and an engine that forwards",
		},
		{
			name: "a managed master with no ledger refuses the second domain",
			objs: []ctrlcli.Object{
				newKVCachePool(), newKVCacheBackend(), otherKVCachePoolBinding("team-b-batch"),
			},
			wantMsg: "that backend holds no tenant ledger",
		},
		{
			// The word SECOND is load-bearing: one domain on a ledger-less master is what such a master
			// serves correctly, so nothing is refused and nothing is warned about.
			name:    "a ledger-less master takes the first domain",
			objs:    []ctrlcli.Object{newKVCachePool(), newKVCacheBackend()},
			wantMsg: "",
		},
		{
			// The reachable shape. A pool's own webhook refuses a MANAGED backend with no ledger, and
			// declines to ask an external one at all, so this is the configuration nothing else sees.
			name: "an external master reported as running without multi-tenancy refuses it",
			objs: []ctrlcli.Object{
				kvCachePoolWithLedgerVerdict(workerctrl.KVCachePoolReasonMultiTenancyDisabled),
				newExternalKVCachePoolBackend(), otherKVCachePoolBinding("team-b-batch"),
			},
			wantMsg: "that backend holds no tenant ledger",
		},
		{
			// The same condition, False for the other reason. Refusing here would answer an outage with
			// a message telling the operator to reconfigure something that is already right.
			name: "an external master that is merely unreachable still takes it",
			objs: []ctrlcli.Object{
				kvCachePoolWithLedgerVerdict("LedgerUnreachable"),
				newExternalKVCachePoolBackend(), otherKVCachePoolBinding("team-b-batch"),
			},
			wantWarn: "needs both a master holding a tenant ledger and an engine that forwards",
		},
		{
			// Nobody has answered for this master: no backend object, no condition. Absence of a reading
			// is not a reading that the domains collapse.
			name:     "a master nothing has answered for still takes it",
			objs:     []ctrlcli.Object{newKVCachePool(), otherKVCachePoolBinding("team-b-batch")},
			wantWarn: "needs both a master holding a tenant ledger and an engine that forwards",
		},
		{
			// Scope. The other domain is on a master this pool does not name, so the two never meet and
			// there is nothing to separate -- no refusal and no warning, on a ledger-less backend.
			name: "a distinct domain on another master is not this rule's business",
			objs: func() []ctrlcli.Object {
				otherPool := newKVCachePool()
				otherPool.Name = "other-pool"
				otherPool.Spec.Backends = []string{"mooncake-other"}
				holder := otherKVCachePoolBinding("team-b-batch")
				holder.Spec.PoolRef.Name = "other-pool"
				return []ctrlcli.Object{newKVCachePool(), newKVCacheBackend(), otherPool, holder}
			}(),
			wantMsg: "",
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			wh := newKVCachePoolBindingWebhook(c.objs...)

			warnings, err := wh.ValidateCreate(context.Background(), newKVCachePoolBinding())
			if c.wantMsg != "" {
				require.Error(t, err)
				require.Contains(t, err.Error(), c.wantMsg)
				assert.Empty(t, warnings,
					"a warning describes what an admitted object leaves standing, and this one was refused")
				return
			}

			require.NoError(t, err)
			if c.wantWarn == "" {
				assert.Empty(t, warnings)
				return
			}
			require.Len(t, warnings, 1)
			assert.Contains(t, warnings[0], c.wantWarn)
		})
	}
}

// unreadableKVCachePoolClient answers reads of ONE named pool with a denial rather than a NotFound.
//
// The distinction is what the rules turn on: NotFound is an answer about the object, and a denial is
// the absence of one.
//
// It is scoped to a single pool deliberately. Denying every pool read makes the ceiling check fail
// first, and an assertion on the resulting error then passes whatever the domain rules do — measured,
// not assumed: a first version of this fixture denied all of them, and a mutation making the domain
// rules swallow the denial left the test green.
type unreadableKVCachePoolClient struct {
	ctrlcli.Client

	pool string
}

func (c unreadableKVCachePoolClient) Get(
	ctx context.Context, key ctrlcli.ObjectKey, obj ctrlcli.Object, opts ...ctrlcli.GetOption,
) error {
	if _, ok := obj.(*workercore.KVCachePool); ok && key.Name == c.pool {
		return kerrors.NewForbidden(schema.GroupResource{
			Group: workercore.GroupVersion.Group, Resource: "kvcachepools",
		}, key.Name, errors.New("no RBAC rule permits this read"))
	}
	return c.Client.Get(ctx, key, obj, opts...)
}

// TestKVCachePoolBindingWebhook_APoolThatCannotBeReadIsNotAPoolThatIsGone pins the difference these
// rules turn on, because both shapes end in no data and folding them together is the easy mistake.
//
// A pool that does not exist is an answer: no master serves that claim, so it collides with nothing
// and the Binding is admitted. A pool that cannot be READ is not an answer at all, and treating it as
// absent would let an RBAC denial or a timeout decide a domain question by silence — the claim would
// be admitted because nobody could look, which is the one outcome this whole rule exists to prevent.
//
// The denial is aimed at the HOLDER's pool, which only the domain scan reads: this Binding's own pool
// is read by the ceiling check too, so denying that one would prove nothing about these rules.
func TestKVCachePoolBindingWebhook_APoolThatCannotBeReadIsNotAPoolThatIsGone(t *testing.T) {
	holderPool := newKVCachePool()
	holderPool.Name = "holder-pool"

	holder := otherKVCachePoolBinding("team-b-batch")
	holder.Spec.PoolRef.Name = holderPool.Name

	base := newKVCachePoolBindingWebhook(
		newKVCachePool(), holderPool, holder, newKVCacheBackend())
	wh := &KVCachePoolBindingWebhook{
		Client:    unreadableKVCachePoolClient{Client: base.Client, pool: holderPool.Name},
		APIReader: unreadableKVCachePoolClient{Client: base.Client, pool: holderPool.Name},
	}

	_, err := wh.ValidateCreate(context.Background(), newKVCachePoolBinding())
	require.Error(t, err,
		"a pool read that was denied says nothing about the domains, and silence must not admit")
	assert.Contains(t, err.Error(), "no RBAC rule permits this read",
		"the cause travels up rather than being reported as a verdict about this object")
}

// TestKVCachePoolBindingWebhook_EverySharedMasterIsAsked is the case a first-match scan admits.
//
// The rule asks whether ANY master the two Bindings share fails to separate, so stopping at the first
// neighbor found answers a different question: a neighbor whose shared master holds a ledger says
// nothing about a second neighbor sharing a different master that does not. The pool under admission
// names two backends here, which its own webhook refuses at creation and the schema does not, so the
// shape reaches this rule whenever that webhook was absent when the pool was written.
func TestKVCachePoolBindingWebhook_EverySharedMasterIsAsked(t *testing.T) {
	ownPool := newKVCachePool()
	ownPool.Spec.Backends = []string{"mooncake-dram", "mooncake-second"}

	// The neighbor on the master that CAN separate, named to sort first.
	okPool := newKVCachePool()
	okPool.Name = "aaa-pool"
	okPool.Spec.Backends = []string{"mooncake-dram"}
	okHolder := otherKVCachePoolBinding("team-b-batch")
	okHolder.Namespace, okHolder.Name = "team-b", "aaa-batch"
	okHolder.Spec.PoolRef.Name = okPool.Name

	// The neighbor on the master that cannot, named to sort last.
	badPool := newKVCachePool()
	badPool.Name = "zzz-pool"
	badPool.Spec.Backends = []string{"mooncake-second"}
	badHolder := otherKVCachePoolBinding("team-c-rag")
	badHolder.Namespace, badHolder.Name = "team-c", "zzz-rag"
	badHolder.Spec.PoolRef.Name = badPool.Name

	ledgerless := newKVCacheBackend()
	ledgerless.Name = "mooncake-second"

	wh := newKVCachePoolBindingWebhook(
		ownPool, okPool, badPool, okHolder, badHolder,
		newMultiTenantKVCacheBackend(), ledgerless)

	_, err := wh.ValidateCreate(context.Background(), newKVCachePoolBinding())
	require.Error(t, err,
		"the second neighbour's master holds no ledger, and it is reached only by scanning past the "+
			"first neighbour whose master does")
	assert.Contains(t, err.Error(), "mooncake-second",
		"the refusal names the master that cannot separate, not the one that can")
}

// TestKVCachePoolBindingWebhook_TheObservationIsReadOffEitherPool covers a verdict that exists on the
// HOLDER's pool while this Binding's own pool has never been reconciled.
//
// One backend may be named by several pools, so which pool carries the observation is an accident of
// which one a controller reached first. A Binding created in the same breath as its pool is the
// ordinary case, and that pool is precisely the one with no conditions yet — so reading only it would
// miss an answer the cluster already holds.
func TestKVCachePoolBindingWebhook_TheObservationIsReadOffEitherPool(t *testing.T) {
	// This Binding's own pool: no conditions at all.
	ownPool := newKVCachePool()

	// The holder's pool, over the SAME backend, carrying the verdict.
	holderPool := kvCachePoolWithLedgerVerdict(workerctrl.KVCachePoolReasonMultiTenancyDisabled)
	holderPool.Name = "observed-pool"

	holder := otherKVCachePoolBinding("team-b-batch")
	holder.Spec.PoolRef.Name = holderPool.Name

	wh := newKVCachePoolBindingWebhook(
		ownPool, holderPool, holder, newExternalKVCachePoolBackend())

	_, err := wh.ValidateCreate(context.Background(), newKVCachePoolBinding())
	require.Error(t, err,
		"the master behind the shared backend was already observed as running without its ledger, "+
			"and the pool carrying that observation is the holder's rather than this one's")
	require.Contains(t, err.Error(), "holds no tenant ledger")
}

// TestKVCachePoolBindingWebhook_TheWarningClaimsNoSeparationItDidNotEstablish guards the one way an
// admission warning can be worse than none.
//
// Admitting means no master was observed OR declared to be ledger-less, which includes an external
// master nobody has scraped yet. A warning saying the store keeps the two domains apart would assert
// exactly what the fall-through did not establish, in the one place an operator reads at that moment.
func TestKVCachePoolBindingWebhook_TheWarningClaimsNoSeparationItDidNotEstablish(t *testing.T) {
	wh := newKVCachePoolBindingWebhook(
		newKVCachePool(), newExternalKVCachePoolBackend(), otherKVCachePoolBinding("team-b-batch"))

	warnings, err := wh.ValidateCreate(context.Background(), newKVCachePoolBinding())
	require.NoError(t, err)
	require.Len(t, warnings, 1)

	assert.NotContains(t, warnings[0], "The master keeps the two",
		"nothing here established that it does: an unscraped external master reaches this path")
	assert.Contains(t, warnings[0], "needs both",
		"the warning names both keepers, so neither reads as already satisfied")
	assert.Contains(t, warnings[0], "team-b/batch",
		"the other domain's holder is what an operator can act on")
}

// TestKVCachePoolBindingWebhook_TheSeparationRefusalSaysWhatToDo pins the clauses an operator acts on.
// The refusal names another namespace's object, so it has to say why that object bars this one and
// what can be changed here; a message naming only the collision leaves the reader with no next step.
func TestKVCachePoolBindingWebhook_TheSeparationRefusalSaysWhatToDo(t *testing.T) {
	wh := newKVCachePoolBindingWebhook(
		newKVCachePool(), newKVCacheBackend(), otherKVCachePoolBinding("team-b-batch"))

	_, err := wh.ValidateCreate(context.Background(), newKVCachePoolBinding())
	require.Error(t, err)

	msg := err.Error()
	assert.Contains(t, msg, "team-b/batch",
		"the Binding holding the other domain is where the operator looks first")
	assert.Contains(t, msg, "mooncake-dram",
		"the backend is the fact the refusal turns on, so it is named rather than implied")
	assert.Contains(t, msg, "whatever the engines do",
		"an operator who knows the engine forwards a tenant would otherwise read this as an engine "+
			"problem and go looking for a build that fixes it; no build does")
	assert.Contains(t, msg, "multiTenancy",
		"the field that resolves it is named, since the object to change is not the one refused")
	assert.NotContains(t, msg, "already registered by",
		"that is the duplicate-domain refusal's wording, and these two rules send the operator to "+
			"opposite actions: rename the domain there, give the master a ledger here")
}

// TestKVCachePoolBindingWebhook_SeparationIsNotRejudgedOnUpdate is the answer to what the check does
// about domains that already exist.
//
// spec.poolRef and spec.domain.name are both frozen, so an update cannot pair a domain with a master
// it was not created against. A copy of the rule on the update path would therefore never catch a new
// collision and would only fail an unrelated ceiling edit on an old one -- including the update that
// removes a finalizer, which would leave the Binding undeletable.
func TestKVCachePoolBindingWebhook_SeparationIsNotRejudgedOnUpdate(t *testing.T) {
	wh := newKVCachePoolBindingWebhook(
		newKVCachePool(), newKVCacheBackend(), otherKVCachePoolBinding("team-b-batch"))

	// The collision is real: the same cluster state refuses this object at CREATE.
	_, err := wh.ValidateCreate(context.Background(), newKVCachePoolBinding())
	require.Error(t, err, "the fixture must reproduce the collision, or this test proves nothing")

	oldKvcpb := newKVCachePoolBinding()
	newKvcpb := oldKvcpb.DeepCopy()
	newKvcpb.Spec.QuotaCeiling = resource.MustParse("30Ti")

	_, err = wh.ValidateUpdate(context.Background(), oldKvcpb, newKvcpb)
	require.NoError(t, err,
		"an edit that moves the ceiling must not be refused for a collision it did not create")
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
