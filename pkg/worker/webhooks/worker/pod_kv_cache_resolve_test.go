package worker

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	core "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrlcli "sigs.k8s.io/controller-runtime/pkg/client"
	ctrlfake "sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	"gpustack.ai/gpustack/pkg/kubeclients/kubernetes/scheme"
	workerctrl "gpustack.ai/gpustack/pkg/worker/controllers/worker"
	"gpustack.ai/gpustack/pkg/worker/kvcache/inject"
)

func newPodKVCacheWebhook(objs ...ctrlcli.Object) *PodKVCacheWebhook {
	cli := ctrlfake.NewClientBuilder().
		WithScheme(scheme.Scheme).
		WithObjects(objs...).
		Build()
	return &PodKVCacheWebhook{Client: cli, APIReader: cli}
}

// kvCacheFixture is the whole resolvable chain: a Binding in the Pod's namespace, the pool it points
// at with its ledger reported available, and the backend supplying the transport.
func kvCacheFixture() []ctrlcli.Object {
	binding := &workercore.KVCachePoolBinding{
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
	pool := &workercore.KVCachePool{
		ObjectMeta: meta.ObjectMeta{Name: "shared"},
		Spec:       workercore.KVCachePoolSpec{Backends: []string{"mc"}},
		Status:     workercore.KVCachePoolStatus{ClientEndpoint: "mc-leader.gpustack-system.svc:50051"},
	}
	workerctrl.KVCachePoolConditionQuotaLedgerAvailable.True(pool, "Available", "ledger present")
	backend := &workercore.KVCacheBackend{
		ObjectMeta: meta.ObjectMeta{Name: "mc"},
		Spec: workercore.KVCacheBackendSpec{
			Transport: workercore.KVCacheBackendTransport{Protocol: "TCP"},
		},
	}
	return []ctrlcli.Object{binding, pool, backend}
}

// kvCachePod is an opted-in Pod naming the fixture Binding.
func kvCachePod() *core.Pod {
	return &core.Pod{
		ObjectMeta: meta.ObjectMeta{
			Namespace: "team-a",
			Name:      "chat-0",
			Labels:    map[string]string{KVCacheInjectLabelKey: KVCacheInjectLabelValue},
			Annotations: map[string]string{
				KVCacheBindingAnnotationKey: "chat",
				KVCacheEngineAnnotationKey:  "vllm",
			},
		},
		Spec: core.PodSpec{Containers: []core.Container{{Name: "server", Args: []string{"serve"}}}},
	}
}

// TestPodKVCacheResolve_HappyPath pins what a fully resolvable chain produces.
func TestPodKVCacheResolve_HappyPath(t *testing.T) {
	r := newPodKVCacheWebhook(kvCacheFixture()...)

	got, err := r.resolve(context.Background(), kvCachePod())
	require.NoError(t, err)

	assert.Equal(t, inject.EngineVLLM, got.Input.Engine)
	assert.Equal(t, inject.RoleNone, got.Input.Role)
	assert.Equal(t, "mc-leader.gpustack-system.svc:50051", got.Input.Connection.MasterAddress,
		"the master address is the pool's published CLIENT endpoint, never the backend's admin address")
	assert.Equal(t, "tcp", got.Input.Connection.Protocol,
		"the API spelling TCP is mapped to the artifact's own through mooncake.MemberProtocol")
}

// TestPodKVCacheResolve_CarriesTheDomainAndVersion. Both fields exist for the stamp and nothing else,
// so this pins where each comes from.
//
// There is no longer a verdict here about whether isolation is enforced. That field, and the
// substitutable seam that fed it, were removed when the decision moved into the renderer: whether a
// tenant is emitted is the facts table's answer read inside the inject package, and the control that
// substitutes it now lives beside it, in TestRender_TenantFollowsTheFactsTable. A seam one layer
// further from the behavior it guards can stay green while that behavior changes.
func TestPodKVCacheResolve_CarriesTheDomainAndVersion(t *testing.T) {
	got, err := newPodKVCacheWebhook(kvCacheFixture()...).resolve(context.Background(), kvCachePod())
	require.NoError(t, err)

	assert.Equal(t, "team-a-chat", got.Isolation.Domain,
		"the domain comes from the Binding, never from a Pod annotation")
	assert.Equal(t, "v0.25.1", got.Isolation.EngineVersion,
		"the version is carried so the stamp says why, not only what")
}

// TestPodKVCacheResolve_Refusals covers every input this webhook declines to serve.
func TestPodKVCacheResolve_Refusals(t *testing.T) {
	testCases := []struct {
		name    string
		mutate  func(pod *core.Pod, objs []ctrlcli.Object) []ctrlcli.Object
		wantMsg string
	}{
		{
			name: "domain annotation is refused, never read",
			mutate: func(pod *core.Pod, objs []ctrlcli.Object) []ctrlcli.Object {
				pod.Annotations[KVCacheDomainAnnotationKey] = "team-b-chat"
				return objs
			},
			wantMsg: KVCacheDomainAnnotationKey,
		},
		{
			name: "engine annotation missing",
			mutate: func(pod *core.Pod, objs []ctrlcli.Object) []ctrlcli.Object {
				delete(pod.Annotations, KVCacheEngineAnnotationKey)
				return objs
			},
			wantMsg: KVCacheEngineAnnotationKey,
		},
		{
			name: "engine annotation unknown",
			mutate: func(pod *core.Pod, objs []ctrlcli.Object) []ctrlcli.Object {
				pod.Annotations[KVCacheEngineAnnotationKey] = "tensorrt"
				return objs
			},
			wantMsg: "tensorrt",
		},
		{
			name: "role annotation unknown",
			mutate: func(pod *core.Pod, objs []ctrlcli.Object) []ctrlcli.Object {
				pod.Annotations[KVCacheRoleAnnotationKey] = "both"
				return objs
			},
			wantMsg: KVCacheRoleAnnotationKey,
		},
		{
			name: "binding annotation missing",
			mutate: func(pod *core.Pod, objs []ctrlcli.Object) []ctrlcli.Object {
				delete(pod.Annotations, KVCacheBindingAnnotationKey)
				return objs
			},
			wantMsg: KVCacheBindingAnnotationKey,
		},
		{
			name: "binding annotation carries a namespace",
			mutate: func(pod *core.Pod, objs []ctrlcli.Object) []ctrlcli.Object {
				pod.Annotations[KVCacheBindingAnnotationKey] = "team-b/chat"
				return objs
			},
			wantMsg: "no cross-namespace form",
		},
		{
			name: "binding absent from the namespace",
			mutate: func(pod *core.Pod, objs []ctrlcli.Object) []ctrlcli.Object {
				pod.Annotations[KVCacheBindingAnnotationKey] = "nope"
				return objs
			},
			wantMsg: `annotation "kvcache.gpustack.ai/binding" names KVCachePoolBinding "nope"`,
		},
		{
			name: "binding is in another namespace",
			mutate: func(pod *core.Pod, objs []ctrlcli.Object) []ctrlcli.Object {
				pod.Namespace = "team-b"
				return objs
			},
			wantMsg: `in namespace "team-b"`,
		},
		{
			name: "pool absent",
			mutate: func(pod *core.Pod, objs []ctrlcli.Object) []ctrlcli.Object {
				return objs[:1] // the Binding only
			},
			wantMsg: `references pool "shared", which does not exist`,
		},
		{
			name: "backend absent",
			mutate: func(pod *core.Pod, objs []ctrlcli.Object) []ctrlcli.Object {
				return objs[:2] // Binding and pool
			},
			wantMsg: `references backend "mc", which does not exist`,
		},
		{
			name: "pool names no backend",
			mutate: func(pod *core.Pod, objs []ctrlcli.Object) []ctrlcli.Object {
				objs[1].(*workercore.KVCachePool).Spec.Backends = nil
				return objs
			},
			wantMsg: "names no backend",
		},
		{
			name: "pool has published no client endpoint yet",
			mutate: func(pod *core.Pod, objs []ctrlcli.Object) []ctrlcli.Object {
				objs[1].(*workercore.KVCachePool).Status.ClientEndpoint = ""
				return objs
			},
			wantMsg: "has not published a client endpoint yet",
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			pod, objs := kvCachePod(), kvCacheFixture()
			objs = tc.mutate(pod, objs)

			got, err := newPodKVCacheWebhook(objs...).resolve(context.Background(), pod)
			assert.Nil(t, got, "a refusal resolves nothing; a partial result would be rendered")
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.wantMsg,
				"a refusal a reader cannot act on is barely better than a silent one")
		})
	}
}

// TestPodKVCacheResolve_QuotaLedgerGate is F4b, and its three failing shapes get three messages
// because they call for three different actions: change a configuration, or wait, or wait.
//
// TestPodKVCacheResolve_RefusesATerminatingBinding. A Binding under deletion is still returned by
// Get, so nothing about the read distinguishes it. The domain it names is on its way out of the
// pool's ledger, and a Pod admitted against it is injected, stamped, and then fails every cache
// write with TENANT_NOT_REGISTERED - permanently, since no amount of waiting brings the domain back.
// Nothing walks it back either: a plain Pod never appears in the Binding's status.usedBy, so the
// finalizer that protects declared consumers does not know this one exists.
func TestPodKVCacheResolve_RefusesATerminatingBinding(t *testing.T) {
	objs := kvCacheFixture()
	binding := objs[0].(*workercore.KVCachePoolBinding)
	now := meta.Now()
	binding.DeletionTimestamp = &now
	// A deletion timestamp without a finalizer is not a state the API server produces, and the fake
	// client rejects it. The finalizer is what makes the terminating state observable at all.
	binding.Finalizers = []string{"kvcache.gpustack.ai/test"}

	_, err := newPodKVCacheWebhook(objs...).resolve(context.Background(), kvCachePod())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "being deleted",
		"the message names the terminating state, not a generic lookup failure")

	// The control that gives the assertion above its teeth: the same fixture, same Pod, no deletion
	// timestamp. Without it a refusal coming from anything else the fixture carries would read as a
	// pass for this check.
	_, err = newPodKVCacheWebhook(kvCacheFixture()...).resolve(context.Background(), kvCachePod())
	require.NoError(t, err, "the identical fixture resolves when the Binding is not terminating")
}

// Unknown is deliberately grouped with the waits rather than with the passes. Reading an admission of
// ignorance as a "yes" is how a pool that never established its ledger ends up serving engines that
// believe they are isolated.
func TestPodKVCacheResolve_QuotaLedgerGate(t *testing.T) {
	condition := workerctrl.KVCachePoolConditionQuotaLedgerAvailable

	testCases := []struct {
		name    string
		set     func(pool *workercore.KVCachePool)
		wantErr bool
		wantMsg string
	}{
		{
			name:    "reported available",
			set:     func(pool *workercore.KVCachePool) { condition.True(pool, "Available", "ledger present") },
			wantErr: false,
		},
		{
			name: "reported off",
			set: func(pool *workercore.KVCachePool) {
				condition.False(pool, workerctrl.KVCachePoolReasonMultiTenancyDisabled, "off")
			},
			wantErr: true,
			wantMsg: workerctrl.KVCachePoolReasonMultiTenancyDisabled,
		},
		{
			name:    "reported unknown",
			set:     func(pool *workercore.KVCachePool) { condition.Unknown(pool, "Probing", "not settled") },
			wantErr: true,
			wantMsg: "a wait rather than an answer",
		},
		{
			name:    "not reported at all",
			set:     func(pool *workercore.KVCachePool) {},
			wantErr: true,
			wantMsg: "does not report",
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			objs := kvCacheFixture()
			pool := objs[1].(*workercore.KVCachePool)
			pool.Status.Conditions = nil
			tc.set(pool)

			_, err := newPodKVCacheWebhook(objs...).resolve(context.Background(), kvCachePod())
			if !tc.wantErr {
				require.NoError(t, err)
				return
			}
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.wantMsg)
		})
	}
}

// TestPodKVCacheResolve_UnreportedAndOffAreDistinguishable. Both refuse, and a single shared message
// would tell an operator to go and enable something that may already be enabled.
func TestPodKVCacheResolve_UnreportedAndOffAreDistinguishable(t *testing.T) {
	condition := workerctrl.KVCachePoolConditionQuotaLedgerAvailable

	off := kvCacheFixture()
	offPool := off[1].(*workercore.KVCachePool)
	offPool.Status.Conditions = nil
	condition.False(offPool, workerctrl.KVCachePoolReasonMultiTenancyDisabled, "off")

	unreported := kvCacheFixture()
	unreported[1].(*workercore.KVCachePool).Status.Conditions = nil

	_, offErr := newPodKVCacheWebhook(off...).resolve(context.Background(), kvCachePod())
	_, unreportedErr := newPodKVCacheWebhook(unreported...).resolve(context.Background(), kvCachePod())

	require.Error(t, offErr)
	require.Error(t, unreportedErr)
	assert.NotEqual(t, offErr.Error(), unreportedErr.Error(),
		"reported-off is a configuration to change and not-yet-reported is a wait")
}

// TestPodKVCacheResolve_InfrastructureErrorIsNotReportedAsNotFound is the reason every read here
// distinguishes NotFound from everything else, and it is checked at each of the three objects because
// each has its own "does not exist" message to be mistaken for.
//
// This webhook runs with failurePolicy Fail, so a timeout, an RBAC denial or a 5xx already stops the
// Pod being created. Reporting one of them as "no such Binding in this namespace" would send the author
// looking for a typo in a manifest that is correct, while the actual fault is a cluster the message
// never mentions.
func TestPodKVCacheResolve_InfrastructureErrorIsNotReportedAsNotFound(t *testing.T) {
	boom := errors.New("etcdserver: request timed out")

	testCases := []struct {
		name       string
		failOn     string
		notMissing string
	}{
		{name: "reading the binding", failOn: "chat", notMissing: "no KVCachePoolBinding"},
		{name: "reading the pool", failOn: "shared", notMissing: "does not exist"},
		{name: "reading the backend", failOn: "mc", notMissing: "does not exist"},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			fail := interceptor.Funcs{
				Get: func(
					ctx context.Context, cli ctrlcli.WithWatch, key ctrlcli.ObjectKey,
					obj ctrlcli.Object, opts ...ctrlcli.GetOption,
				) error {
					if key.Name == tc.failOn {
						return boom
					}
					return cli.Get(ctx, key, obj, opts...)
				},
			}
			cli := ctrlfake.NewClientBuilder().
				WithScheme(scheme.Scheme).
				WithObjects(kvCacheFixture()...).
				WithInterceptorFuncs(fail).
				Build()

			_, err := (&PodKVCacheWebhook{Client: cli, APIReader: cli}).
				resolve(context.Background(), kvCachePod())

			require.Error(t, err)
			assert.ErrorIs(t, err, boom, "the underlying failure is wrapped, not replaced")
			assert.NotContains(t, err.Error(), tc.notMissing,
				"an unreachable API server must not read as a missing object")
		})
	}
}

// TestPodKVCacheResolve_ColdCacheStillResolves covers the ordinary race this webhook is built for: a
// Pod created in the same breath as its Binding, before the informer holds any of it.
//
// Without a live read the first Pod of a new deployment is refused for an object that exists, and the
// refusal names the Binding the author just created - the least believable message the webhook could
// produce.
//
// The cache is left EMPTY rather than missing only the Binding, because the three objects reach the
// API server by two different routes and one test should exercise both: the Binding is read straight
// through the APIReader, while the pool and the backend go through r.get, which consults the cache
// first and falls back only on NotFound. Seeding the cache with the pool and the backend would leave
// that fallback with no test at all.
func TestPodKVCacheResolve_ColdCacheStillResolves(t *testing.T) {
	objs := kvCacheFixture()

	cold := ctrlfake.NewClientBuilder().WithScheme(scheme.Scheme).Build()
	warm := ctrlfake.NewClientBuilder().WithScheme(scheme.Scheme).WithObjects(objs...).Build()
	r := &PodKVCacheWebhook{Client: cold, APIReader: warm}

	got, err := r.resolve(context.Background(), kvCachePod())

	require.NoError(t, err, "a cache miss is a retry against the API server, not a refusal")
	assert.Equal(t, "team-a-chat", got.Isolation.Domain)
}
