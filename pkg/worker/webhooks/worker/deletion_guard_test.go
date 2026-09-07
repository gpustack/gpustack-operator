package worker

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	admreg "k8s.io/api/admissionregistration/v1"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	"gpustack.ai/gpustack/pkg/webhook"
)

// TestDeletionGuardClassification classifies every handler this package registers for UPDATE by
// whether its update validation survives the shared deletion guard, and it is the substance of the
// question rather than a restatement of it.
//
// The assertion is the same type assertion ExecuteSetup performs on each handler, which is the only
// expression deciding whether that handler is wrapped in production. Calling ValidateUpdate directly
// cannot answer it: the wrapper is not in that path, so a handler being skipped in production refuses
// identically when called by hand.
//
// Each row's reason is the evidence, and it is what has to be disproved before a handler moves
// between the two classes. The rule is one question: can this handler reject an update whose only
// change is metadata.finalizers? A handler that cannot keeps receiving update validation while the
// object drains; one that can keeps the guard.
func TestDeletionGuardClassification(t *testing.T) {
	testCases := []struct {
		name    string
		handler any
		want    bool
		reason  string
	}{
		{
			name:    "KVCachePoolBinding freezes the reuse identity a warm cache was written under",
			handler: &KVCachePoolBindingWebhook{},
			want:    true,
			reason: "validateKVCachePoolBindingSpec and validateKVCachePoolBindingImmutable are " +
				"answered from the two objects; the only external read, " +
				"validateKVCachePoolBindingCeilingFitsPool, is gated behind ceilingMoved, and " +
				"clearing a finalizer moves no ceiling",
		},
		{
			name:    "KVCacheBackend freezes the connection branch and the disk tier",
			handler: &KVCacheBackendWebhook{},
			want:    true,
			reason: "the handler holds no client; the only external read is the fallback-image " +
				"setting in validateKVCacheBackendImage, gated behind checkFallback, which is " +
				"false for an update leaving spec.image where it was",
		},
		{
			name:    "KVCachePool freezes the backend its whole identity rests on",
			handler: &KVCachePoolWebhook{},
			want:    true,
			reason: "ValidateUpdate takes no context and calls validateKVCachePoolSpec and " +
				"validateKVCachePoolImmutable only; validateKVCachePoolBackend is reached from " +
				"ValidateCreate alone",
		},
		{
			name:    "ModelDeployment holds every rule against the object alone",
			handler: &ModelDeploymentWebhook{},
			want:    true,
			reason: "the handler holds no client at all and ValidateUpdate takes no context, so " +
				"there is no state it could read that deletion could have taken away",
		},
		{
			name:    "InstanceType freezes its spec except three admin-editable fields",
			handler: &InstanceTypeWebhook{},
			want:    true,
			reason: "ValidateUpdate takes no context and calls validateInstanceTypeSpecImmutable " +
				"only, a masked equality on the two specs; Default reads one setting and returns " +
				"nil on every path, so opting the mutating half out with it refuses nothing",
		},
		{
			name:    "PodKVCache freezes what admission decided for the Pod's whole life",
			handler: &PodKVCacheWebhook{},
			want:    true,
			reason: "validatePodKVCacheOwnedMetadata compares the two objects' metadata and reads " +
				"nothing else; this marker predates this classification and is asserted in full " +
				"by TestPodKVCacheValidateUpdate_SurvivesTheDeletionGuard",
		},
		{
			name:    "Instance is the one handler the guard is load-bearing for",
			handler: &InstanceWebhook{},
			want:    false,
			reason: "one marker releases the defaulter as well as the validator, and this " +
				"handler's Default reads the referenced InstanceType and refuses when it is gone. " +
				"Opting in would make a running Instance undeletable whenever its InstanceType " +
				"was deleted first. See TestInstanceWebhookKeepsTheDeletionGuard",
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			_, got := tc.handler.(webhook.ReceiveDeletionUpdate)
			assert.Equal(t, tc.want, got, tc.reason)
		})
	}
}

// TestPodWebhookIsOutsideTheDeletionGuard records why the accelerator Pod handler is absent from the
// table above: the guard cannot fire on it. Both of its halves are registered for CREATE alone, and a
// create carries no deletion timestamp, so whether it implements the marker changes nothing
// observable — an assertion on the marker there would be one no edit could make fail.
//
// What is assertable is the registration that makes the guard inert, so that is what is asserted. The
// validating half's operations are pinned by TestKVCacheWebhookConfig_TheOtherPodValidatorIsUntouched;
// this covers the mutating half, whose Default does read the cluster and would refuse if it ever ran
// on an UPDATE.
func TestPodWebhookIsOutsideTheDeletionGuard(t *testing.T) {
	cfg := GetMutatingWebhookConfiguration("gpustack-worker-mutation", admreg.WebhookClientConfig{})

	var found bool
	for i := range cfg.Webhooks {
		if cfg.Webhooks[i].Name != "mutate.gpustack-worker.core.v1.pod" {
			continue
		}
		found = true
		require.Len(t, cfg.Webhooks[i].Rules, 1)
		assert.Equal(t, []admreg.OperationType{admreg.Create}, cfg.Webhooks[i].Rules[0].Operations,
			"this handler's Default reads the InstanceType fronting the Pod's queue and returns an "+
				"error when it cannot; widening it to UPDATE would put that read in the path of "+
				"every edit to a terminating Pod")
	}
	require.True(t, found, "no mutating entry named mutate.gpustack-worker.core.v1.pod")
}

// TestKVCachePoolBindingSurvivesTheDeletionGuard is the positive half of the classification, on the
// instance the report traced: the reuse identity a warm cache was written under stays frozen while
// the Binding drains, and the update that lets it go is still admitted.
//
// The second case seeds NO pool, and that is the assertion rather than the fixture being lazy. If
// the ceiling check were reachable on a finalizer-clearing update it would answer "pool not found"
// and refuse — which is the deadlock the guard exists to prevent, and the reason the pool read is
// gated behind ceilingMoved instead of being asked on every update.
//
// Neither case can stand alone. Calling ValidateUpdate directly bypasses the wrapper, so both would
// read the same before this handler carried the marker: they establish that the RULES hold on a
// terminating object, and the row in the table above is what establishes that they are reached.
func TestKVCachePoolBindingSurvivesTheDeletionGuard(t *testing.T) {
	terminating := func() *workercore.KVCachePoolBinding {
		kvcpb := newKVCachePoolBinding()
		deletedAt := meta.Now()
		kvcpb.DeletionTimestamp = &deletedAt
		kvcpb.Finalizers = []string{"gpustack.ai/locked"}
		return kvcpb
	}

	t.Run("the block size may not be changed while the Binding drains", func(t *testing.T) {
		oldKvcpb := terminating()
		newKvcpb := oldKvcpb.DeepCopy()
		newKvcpb.Spec.Domain.BlockSize = 32

		_, err := newKVCachePoolBindingWebhook(newKVCachePool()).
			ValidateUpdate(context.Background(), oldKvcpb, newKvcpb)
		require.Error(t, err,
			"the blocks already in the cache were written at the old size, and the pool publishes "+
				"this value as authoritative for the whole span before the last finalizer comes off")
		assert.Contains(t, err.Error(), "blockSize is immutable")
	})

	t.Run("the finalizer still comes off with the pool already gone", func(t *testing.T) {
		oldKvcpb := terminating()
		draining := oldKvcpb.DeepCopy()
		draining.Finalizers = nil

		_, err := newKVCachePoolBindingWebhook().
			ValidateUpdate(context.Background(), oldKvcpb, draining)
		require.NoError(t, err,
			"a pool deleted before its Bindings is the ordinary teardown order, so no rule reached "+
				"by an update that moves no ceiling may depend on the pool being there")
	})
}

// TestInstanceWebhookKeepsTheDeletionGuard is the negative half of the classification, and it is the
// one with teeth: it demonstrates the refusal that the guard's absence would let through.
//
// The Instance carries the system finalizer, so the reconciler releases it with a full-object update
// that leaves the spec untouched. That update passes through the mutating half, whose failurePolicy
// is Fail. With no InstanceType to read, Default refuses it — so without the guard an Instance whose
// InstanceType was deleted first could never finish deleting.
//
// FORBIDDEN: giving this handler the marker. The deadlock above is the reason the guard exists, and
// it is the deadlock a single marker cannot avoid while it releases the defaulter too.
func TestInstanceWebhookKeepsTheDeletionGuard(t *testing.T) {
	// The finalizer-clearing update: the spec is untouched and only metadata moves. Each case gets
	// its own pair, because Default mutates the object it is handed before it reaches the read that
	// fails — harmless in production, where the refusal discards the patch, and misleading here if
	// the two calls shared one object.
	draining := func() (oldInst, newInst *workercore.Instance) {
		oldInst = webhookInstance("chat", "a10g")
		deletedAt := meta.Now()
		oldInst.DeletionTimestamp = &deletedAt
		oldInst.Finalizers = []string{"gpustack.ai/locked"}

		newInst = oldInst.DeepCopy()
		newInst.Finalizers = nil
		return oldInst, newInst
	}

	t.Run("its defaulter refuses the update that releases the object", func(t *testing.T) {
		// No InstanceType is seeded: this is the state after an administrator deleted it.
		_, newInst := draining()

		err := newInstanceWebhook().Default(context.Background(), newInst)
		require.Error(t, err,
			"Default reads the referenced InstanceType and has none, so the finalizer-clearing "+
				"update is refused — which is exactly what the deletion guard suppresses")
		assert.Contains(t, err.Error(), "spec.type")
	})

	t.Run("its update validation would have survived deletion", func(t *testing.T) {
		oldInst, newInst := draining()

		_, err := newInstanceWebhook().ValidateUpdate(context.Background(), oldInst, newInst)
		require.NoError(t, err,
			"update validation is not what keeps this handler guarded; the defaulter is, and one "+
				"marker cannot release one half without the other")
	})
}
