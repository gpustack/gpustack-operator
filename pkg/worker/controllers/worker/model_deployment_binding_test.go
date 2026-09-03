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
	ctrlinterceptor "sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	"gpustack.ai/gpustack/pkg/kubeclients/kubernetes/scheme"
	"gpustack.ai/gpustack/pkg/systemmeta"
)

// newRenderBinding builds the Binding newRenderDeployment's poolRef names, usable by default.
//
// Usable means BOTH halves the resolution reads: the phase its own controller derives, and the
// QuotaGranted axis. A fixture that set only the phase would let a regression on the axis pass.
func newRenderBinding(mutate ...func(*workercore.KVCachePoolBinding)) *workercore.KVCachePoolBinding {
	kvcpb := &workercore.KVCachePoolBinding{
		ObjectMeta: meta.ObjectMeta{Name: "shared-kv", Namespace: "team-a"},
		Spec: workercore.KVCachePoolBindingSpec{
			PoolRef:      workercore.KVCachePoolBindingPoolReference{Name: "shared"},
			Domain:       workercore.KVCachePoolBindingDomain{Name: "chat", BlockSize: 256, Dtype: "bfloat16"},
			QuotaCeiling: resource.MustParse("100Gi"),
		},
		Status: workercore.KVCachePoolBindingStatus{
			Phase:        KVCachePoolPhaseReady,
			PhaseMessage: "the master reports this domain's figures",
		},
	}
	KVCachePoolBindingConditionQuotaGranted.True(kvcpb, "Granted", "the master granted 100Gi")

	for _, m := range mutate {
		m(kvcpb)
	}

	return kvcpb
}

func getModelDeploymentBinding(t *testing.T, cli ctrlcli.Client) *workercore.KVCachePoolBinding {
	t.Helper()

	kvcpb := new(workercore.KVCachePoolBinding)
	require.NoError(t, cli.Get(context.Background(),
		ctrlcli.ObjectKey{Namespace: "team-a", Name: "shared-kv"}, kvcpb))

	return kvcpb
}

// modelDeploymentClaim is the entry a deployment is expected to write into a Binding's usedBy.
func modelDeploymentClaim() workercore.KVCacheObjectReference {
	return workercore.KVCacheObjectReference{Kind: ModelDeploymentKind, Namespace: "", Name: "qwen"}
}

// TestModelDeploymentBinding_ResolvesTheDomainInItsOwnNamespace is the projection F3 exists for: the
// domain is read off the Binding an admin owns, and a reader of the deployment alone can tell which
// cache its replicas are on.
func TestModelDeploymentBinding_ResolvesTheDomainInItsOwnNamespace(t *testing.T) {
	md := newRenderDeployment()
	cli := newModelDeploymentClient(md, newRenderInstanceType(), newRenderBinding())
	r := &ModelDeploymentReconciler{Client: cli, APIReader: cli}

	observed, err := r.resolveModelDeploymentDomain(context.Background(), md)
	require.NoError(t, err)

	assert.True(t, observed.Ready)
	assert.Equal(t, modelDeploymentReasonRegistered, observed.Reason)
	assert.Equal(t, &workercore.ModelDeploymentKVCacheStatus{
		Binding: "shared-kv",
		Pool:    "shared",
		Domain: workercore.ModelDeploymentKVCacheDomain{
			Name: "chat", BlockSize: 256, Dtype: "bfloat16",
		},
	}, observed.KVCache)
}

// TestModelDeploymentBinding_NeverReadsAnotherNamespace states the security property F2 rests on,
// as a refusal by the client rather than as a claim in a comment. poolRef is a
// LocalObjectReference, so a name it carries can only mean an object in this namespace; a read that
// went looking elsewhere would be the bypass the type was chosen to make unrepresentable.
func TestModelDeploymentBinding_NeverReadsAnotherNamespace(t *testing.T) {
	md := newRenderDeployment()
	cli := ctrlfake.NewClientBuilder().
		WithScheme(scheme.Scheme).
		WithStatusSubresource(&workercore.ModelDeployment{}, &workercore.KVCachePoolBinding{}).
		WithObjects(md, newRenderInstanceType(), newRenderBinding()).
		WithInterceptorFuncs(ctrlinterceptor.Funcs{
			Get: func(
				ctx context.Context, c ctrlcli.WithWatch, key ctrlcli.ObjectKey,
				obj ctrlcli.Object, opts ...ctrlcli.GetOption,
			) error {
				if _, ok := obj.(*workercore.KVCachePoolBinding); ok && key.Namespace != "team-a" {
					t.Errorf("a binding was read from namespace %q, which poolRef cannot name", key.Namespace)
				}

				return c.Get(ctx, key, obj, opts...)
			},
		}).
		Build()
	r := &ModelDeploymentReconciler{Client: cli, APIReader: cli}

	observed, err := r.resolveModelDeploymentDomain(context.Background(), md)
	require.NoError(t, err)
	require.True(t, observed.Ready, "the control: the binding in the OWN namespace was found")
}

// TestModelDeploymentBinding_Usability walks every reading of the Binding, including the one this
// spec is the first consumer of.
func TestModelDeploymentBinding_Usability(t *testing.T) {
	testCases := []struct {
		name       string
		mutate     func(*workercore.KVCachePoolBinding)
		wantReady  bool
		wantReason string
	}{
		{
			name:       "ready with a granted quota",
			wantReady:  true,
			wantReason: modelDeploymentReasonRegistered,
		},
		{
			// The regression the QuotaGranted axis was added for: readiness once reported True both
			// when the grant was zero and when there was no ledger entry, so a Binding could read
			// Ready while no write could succeed. The phase alone would pass this case.
			name: "the phase says ready but nothing was granted",
			mutate: func(kvcpb *workercore.KVCachePoolBinding) {
				KVCachePoolBindingConditionQuotaGranted.False(kvcpb, "ZeroGranted",
					"the master granted zero bytes")
			},
			wantReady:  false,
			wantReason: modelDeploymentReasonBindingNotReady,
		},
		{
			// nil and zero are separate answers: "not exported" is not "granted nothing", and both
			// are unusable.
			name: "no ledger entry at all",
			mutate: func(kvcpb *workercore.KVCachePoolBinding) {
				KVCachePoolBindingConditionQuotaGranted.False(kvcpb, "NoLedgerEntry",
					"the master carries no entry for this domain")
			},
			wantReady:  false,
			wantReason: modelDeploymentReasonBindingNotReady,
		},
		{
			// An axis this file does not know about is still honored, because the phase is read
			// rather than a copy of the axis list. A copied list would report this usable.
			name: "an axis this deployment does not read drove the phase to Error",
			mutate: func(kvcpb *workercore.KVCachePoolBinding) {
				kvcpb.Status.Phase = KVCachePoolPhaseError
				kvcpb.Status.PhaseMessage = "reuse domain \"chat\" is also claimed by team-b/batch"
			},
			wantReady:  false,
			wantReason: modelDeploymentReasonBindingNotReady,
		},
		{
			name: "the binding's own controller has not reached it yet",
			mutate: func(kvcpb *workercore.KVCachePoolBinding) {
				kvcpb.Status = workercore.KVCachePoolBindingStatus{}
			},
			wantReady:  false,
			wantReason: modelDeploymentReasonBindingNotReady,
		},
		{
			// Its own reason, because it sends a reader somewhere else entirely: find who deleted
			// the authorization, rather than wait for it or look at the pool.
			name: "being deleted under a running deployment",
			mutate: func(kvcpb *workercore.KVCachePoolBinding) {
				now := meta.Now()
				kvcpb.DeletionTimestamp = &now
				kvcpb.Finalizers = []string{systemmeta.LockedResourceFinalizer}
			},
			wantReady:  false,
			wantReason: modelDeploymentReasonBindingDeleting,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			kvcpb := newRenderBinding()
			if tc.mutate != nil {
				tc.mutate(kvcpb)
			}

			ready, reason, message := modelDeploymentBindingUsable(kvcpb)

			assert.Equal(t, tc.wantReady, ready)
			assert.Equal(t, tc.wantReason, reason)
			assert.Contains(t, message, "shared-kv",
				"the message must name the binding, or a reader has nowhere to go")
		})
	}
}

// TestModelDeploymentBinding_MissingBindingKeepsTheReplicasServing is F2's fourth acceptance. An
// admin object vanishing is not a reason to stop serving; the condition is the signal.
func TestModelDeploymentBinding_MissingBindingKeepsTheReplicasServing(t *testing.T) {
	md := newRenderDeployment()
	cli := newModelDeploymentClient(md, newRenderInstanceType(), newRenderBinding())

	_, err := reconcileModelDeployment(t, cli)
	require.NoError(t, err)
	require.Len(t, replicaNames(t, cli), 2)
	require.True(t, ModelDeploymentConditionDomainRegistered.IsTrue(getModelDeployment(t, cli)))

	require.NoError(t, cli.Delete(context.Background(), newRenderBinding()))

	_, err = reconcileModelDeployment(t, cli)
	require.NoError(t, err)

	assert.Len(t, replicaNames(t, cli), 2, "the replicas keep serving")

	got := getModelDeployment(t, cli)
	assert.True(t, ModelDeploymentConditionDomainRegistered.IsFalse(got))
	assert.Equal(t, modelDeploymentReasonBindingNotFound,
		ModelDeploymentConditionDomainRegistered.GetReason(got))
	assert.Equal(t, &workercore.ModelDeploymentKVCacheStatus{
		Binding: "shared-kv",
		Pool:    "shared",
		Domain: workercore.ModelDeploymentKVCacheDomain{
			Name: "chat", BlockSize: 256, Dtype: "bfloat16",
		},
	}, got.Status.KVCache,
		"the domain the replicas are still writing into is kept; the condition is what says it is stale")
}

// TestModelDeploymentBinding_ConvergenceIsNotGatedOnTheBinding states that the resolution reports
// rather than decides.
//
// Gating the convergence on it would make a Binding that is a few seconds behind — or one an admin
// has not created yet — into a deployment with no replicas at all, and would turn every routine
// store upgrade into an outage of every deployment on it. The replicas exist; DomainRegistered is
// what says whether they are attached.
func TestModelDeploymentBinding_ConvergenceIsNotGatedOnTheBinding(t *testing.T) {
	md := newRenderDeployment()
	cli := newModelDeploymentClient(md, newRenderInstanceType())

	_, err := reconcileModelDeployment(t, cli)
	require.NoError(t, err)

	assert.Equal(t, []string{"qwen-server-0", "qwen-server-1"}, replicaNames(t, cli),
		"the replicas are rendered although no binding could be resolved")

	got := getModelDeployment(t, cli)
	assert.True(t, ModelDeploymentConditionDomainRegistered.IsFalse(got))
	assert.Equal(t, modelDeploymentReasonBindingNotFound,
		ModelDeploymentConditionDomainRegistered.GetReason(got))
	assert.Nil(t, got.Status.KVCache,
		"and nothing is invented: a deployment that never resolved has no domain to report")
}

// TestModelDeploymentBinding_ALeaderRestartDropsNothing is the measured window, asserted. A store
// leader restart makes every Binding not-Ready for tens of seconds, which is an ordinary operation:
// the pass must report it and change nothing else.
func TestModelDeploymentBinding_ALeaderRestartDropsNothing(t *testing.T) {
	md := newRenderDeployment()
	writes := new(modelDeploymentWrites)
	cli := newCountingModelDeploymentClient(writes, md, newRenderInstanceType(), newRenderBinding())

	_, err := reconcileModelDeployment(t, cli)
	require.NoError(t, err)
	before := replicaHashes(t, cli)
	require.Len(t, before, 2)

	// The leader restarts: its readiness probe reads its segment list rather than its quota ledger,
	// so the Pod is Ready while the ledger still answers zero.
	restarting := getModelDeploymentBinding(t, cli)
	restarting.Status.Phase = KVCachePoolPhaseError
	restarting.Status.PhaseMessage = "the master reports nothing to allocate"
	KVCachePoolBindingConditionQuotaGranted.False(restarting, "ZeroGranted",
		"the master granted zero bytes")
	require.NoError(t, cli.Status().Update(context.Background(), restarting))

	*writes = modelDeploymentWrites{}
	_, err = reconcileModelDeployment(t, cli)
	require.NoError(t, err)

	assert.Zero(t, writes.deletes, "no replica is torn down for a routine store upgrade")
	assert.Zero(t, writes.creates)
	assert.Equal(t, before, replicaHashes(t, cli), "and none is rebuilt")
	assert.Equal(t, []workercore.KVCacheObjectReference{modelDeploymentClaim()},
		getModelDeploymentBinding(t, cli).Status.UsedBy,
		"the claim is held through the window: dropped here, an admin's delete would go through "+
			"under replicas that are still writing")

	got := getModelDeployment(t, cli)
	assert.True(t, ModelDeploymentConditionDomainRegistered.IsFalse(got))

	// And it recovers on the next pass, with nothing to undo.
	recovered := getModelDeploymentBinding(t, cli)
	recovered.Status.Phase = KVCachePoolPhaseReady
	KVCachePoolBindingConditionQuotaGranted.True(recovered, "Granted", "the master granted 100Gi")
	require.NoError(t, cli.Status().Update(context.Background(), recovered))

	*writes = modelDeploymentWrites{}
	_, err = reconcileModelDeployment(t, cli)
	require.NoError(t, err)

	assert.Zero(t, writes.deletes)
	assert.Zero(t, writes.creates)
	assert.Equal(t, before, replicaHashes(t, cli))
	assert.True(t, ModelDeploymentConditionDomainRegistered.IsTrue(getModelDeployment(t, cli)))
}

// TestModelDeploymentBinding_ClaimsTheBinding is the contract the Binding's API pins on this kind:
// it declares ModelDeployment the ONLY writer of usedBy, and refuses a deletion the list is not
// empty on. Until something writes it, that refusal enforces over a list that is always empty.
func TestModelDeploymentBinding_ClaimsTheBinding(t *testing.T) {
	md := newRenderDeployment()
	writes := new(modelDeploymentWrites)
	cli := newCountingModelDeploymentClient(writes, md, newRenderInstanceType(), newRenderBinding())

	_, err := reconcileModelDeployment(t, cli)
	require.NoError(t, err)

	assert.Equal(t, []workercore.KVCacheObjectReference{modelDeploymentClaim()},
		getModelDeploymentBinding(t, cli).Status.UsedBy)

	// A settled pass must not rewrite another controller's object: the Binding's own reconciler
	// writes this status too, so a claim re-issued every pass would make every pass a conflict.
	*writes = modelDeploymentWrites{}
	_, err = reconcileModelDeployment(t, cli)
	require.NoError(t, err)
	assert.Zero(t, writes.statusUpdates,
		"the claim already says what it should, so nothing is written")
}

// TestModelDeploymentBinding_ReleasesOnlyAfterTheLastReplicaIsGone is the ordering the claim exists
// for. Released while a replica is still up, the authorization could be deleted from under a
// process that is still writing through it.
func TestModelDeploymentBinding_ReleasesOnlyAfterTheLastReplicaIsGone(t *testing.T) {
	md := newRenderDeployment()
	cli := newModelDeploymentClient(md, newRenderInstanceType(), newRenderBinding())

	_, err := reconcileModelDeployment(t, cli)
	require.NoError(t, err)
	require.Len(t, replicaNames(t, cli), 2)
	require.NotEmpty(t, getModelDeploymentBinding(t, cli).Status.UsedBy)

	deleting := getModelDeployment(t, cli)
	require.NoError(t, cli.Delete(context.Background(), deleting))

	// First teardown pass: the replicas are still there, so the claim must still be there too.
	_, err = reconcileModelDeployment(t, cli)
	require.NoError(t, err)
	assert.Equal(t, []workercore.KVCacheObjectReference{modelDeploymentClaim()},
		getModelDeploymentBinding(t, cli).Status.UsedBy,
		"a replica is still writing, so the authorization must not become deletable")

	// That pass issued the deletes, so the next one observes no replicas and releases.
	require.Empty(t, replicaNames(t, cli))

	_, err = reconcileModelDeployment(t, cli)
	require.NoError(t, err)
	assert.Empty(t, getModelDeploymentBinding(t, cli).Status.UsedBy,
		"the last replica has left, so a Binding deleted after its deployments can finish")
}

// TestModelDeploymentBinding_TeardownDoesNotReportTheDomainAsGone pins why a teardown pass passes no
// reading at all. It does not look at the Binding — it has no question to ask it — and a pass that
// wrote "not observed" would report the domain as having vanished at exactly the moment an operator
// is reading the object to find out what is being torn down.
func TestModelDeploymentBinding_TeardownDoesNotReportTheDomainAsGone(t *testing.T) {
	md := newRenderDeployment()
	cli := newModelDeploymentClient(md, newRenderInstanceType(), newRenderBinding())

	_, err := reconcileModelDeployment(t, cli)
	require.NoError(t, err)
	before := getModelDeployment(t, cli).Status.KVCache
	require.NotNil(t, before)

	require.NoError(t, cli.Delete(context.Background(), getModelDeployment(t, cli)))
	_, err = reconcileModelDeployment(t, cli)
	require.NoError(t, err)

	got := getModelDeployment(t, cli)
	assert.Equal(t, ModelDeploymentPhaseDeleting, got.Status.Phase)
	assert.Equal(t, before, got.Status.KVCache)
	assert.True(t, ModelDeploymentConditionDomainRegistered.IsTrue(got),
		"the last reading stands; the phase is what says the deployment is going away")
}

// TestObserveModelDeploymentDomain_NilLeavesEverythingAlone is the same rule one level down, where
// the decision actually lives.
func TestObserveModelDeploymentDomain_NilLeavesEverythingAlone(t *testing.T) {
	holder := new(workercore.ModelDeployment)
	holder.Status.KVCache = &workercore.ModelDeploymentKVCacheStatus{Binding: "shared-kv"}
	ModelDeploymentConditionDomainRegistered.True(holder, modelDeploymentReasonRegistered, "registered")

	observeModelDeploymentDomain(holder, nil)

	assert.Equal(t, "shared-kv", holder.Status.KVCache.Binding)
	assert.True(t, ModelDeploymentConditionDomainRegistered.IsTrue(holder))

	// And a reading with no projection — the Binding was not found — moves the condition without
	// erasing the record.
	observeModelDeploymentDomain(holder, &modelDeploymentDomain{
		Reason: modelDeploymentReasonBindingNotFound, Message: "gone",
	})

	assert.Equal(t, "shared-kv", holder.Status.KVCache.Binding)
	assert.True(t, ModelDeploymentConditionDomainRegistered.IsFalse(holder))
	assert.Equal(t, modelDeploymentReasonBindingNotFound,
		ModelDeploymentConditionDomainRegistered.GetReason(holder))
}
