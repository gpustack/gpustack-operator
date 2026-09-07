package worker

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	admreg "k8s.io/api/admissionregistration/v1"
	core "k8s.io/api/core/v1"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"

	"gpustack.ai/gpustack/pkg/webhook"
	"gpustack.ai/gpustack/pkg/worker/kvcache/inject"
)

// injectedKVCachePod is an opted-in Pod that has been through the mutating half, so the two frozen
// annotations hold the values production writes rather than fixture constants. A test that froze a
// hand-written stamp would pass while the webhook wrote something else.
func injectedKVCachePod(t *testing.T) *core.Pod {
	t.Helper()

	pod := kvCachePod()
	require.NoError(t, admit(t, pod))
	require.Contains(t, pod.Annotations, KVCacheInjectedAnnotationKey,
		"the fixture must be an INJECTED Pod, or every case below freezes an absent key")
	require.Contains(t, pod.Annotations, inject.ClientConfigAnnotationKey,
		"the fixture must carry the projected configuration, or case 3 of the report is unreachable")
	return pod
}

// neverInjectedKVCachePod is a Pod that never opted in, so it carries neither the trigger label nor
// anything the mutating half writes. It is the subject of the report's first consequence - the Pod
// somebody adds the label to after the fact - and using an injected Pod with the label stripped
// instead would test a state nothing can produce.
func neverInjectedKVCachePod(t *testing.T) *core.Pod {
	t.Helper()

	pod := kvCachePod()
	delete(pod.Labels, KVCacheInjectLabelKey)
	require.NotContains(t, pod.Annotations, KVCacheInjectedAnnotationKey,
		"a Pod that never opted in carries no record, or the label case would report the record too")
	require.NotContains(t, pod.Annotations, inject.ClientConfigAnnotationKey)
	return pod
}

type podKVCacheUpdateCase struct {
	name string
	// base builds the stored Pod. Defaults to an injected one.
	base func(t *testing.T) *core.Pod
	// mutateOld shapes what the Pod looked like BEFORE the edit, for the cases whose subject is that.
	mutateOld func(pod *core.Pod)
	mutateNew func(pod *core.Pod)
	// wantMsg empty means the update must be ADMITTED.
	wantMsg string
}

func runPodKVCacheUpdateCases(t *testing.T, cases []podKVCacheUpdateCase) {
	t.Helper()

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			build := c.base
			if build == nil {
				build = injectedKVCachePod
			}
			// One build and a copy, rather than two builds: a second pass through the mutating half
			// would compare two independent renders instead of an edit to one Pod.
			oldPod := build(t)
			newPod := oldPod.DeepCopy()
			if c.mutateOld != nil {
				c.mutateOld(oldPod)
			}
			c.mutateNew(newPod)

			_, err := (&PodKVCacheWebhook{}).ValidateUpdate(context.Background(), oldPod, newPod)
			if c.wantMsg == "" {
				require.NoError(t, err)
				return
			}
			require.Error(t, err)
			require.Contains(t, err.Error(), c.wantMsg)
		})
	}
}

// TestPodKVCacheValidateUpdate covers the three consequences the report names and, in the same table,
// the edits that must keep passing.
//
// The admitted half is not decoration. Every refusal below would also be produced by a handler that
// refused unconditionally, so without those rows a tautological guard is indistinguishable from this
// one - and the cost of the tautology is the worse failure: an opted-in Pod is a live workload whose
// finalizers, labels and status belong to whatever is managing it.
func TestPodKVCacheValidateUpdate(t *testing.T) {
	runPodKVCacheUpdateCases(t, []podKVCacheUpdateCase{
		// Consequence 1: an opted-in Pod that was never injected.
		{
			name: "the opt-in label added to a Pod that already exists",
			base: neverInjectedKVCachePod,
			mutateNew: func(pod *core.Pod) {
				pod.Labels[KVCacheInjectLabelKey] = KVCacheInjectLabelValue
			},
			wantMsg: "injection runs at CREATE and only there",
		},
		{
			name:      "the opt-in label dropped from an injected Pod",
			mutateNew: func(pod *core.Pod) { delete(pod.Labels, KVCacheInjectLabelKey) },
			wantMsg:   "nothing un-injects a Pod",
		},
		{
			name:      "the opt-in label flipped to false on an injected Pod",
			mutateNew: func(pod *core.Pod) { pod.Labels[KVCacheInjectLabelKey] = "false" },
			wantMsg:   "nothing un-injects a Pod",
		},

		// Consequence 2: a forged record.
		{
			name: "the injection record edited",
			mutateNew: func(pod *core.Pod) {
				pod.Annotations[KVCacheInjectedAnnotationKey] = `{"binding":"someone-elses"}`
			},
			wantMsg: "a record of a decision nobody made",
		},
		{
			name:      "the injection record removed",
			mutateNew: func(pod *core.Pod) { delete(pod.Annotations, KVCacheInjectedAnnotationKey) },
			wantMsg:   "a record of a decision nobody made",
		},

		// Consequence 3: a live configuration swap, the sharpest of the three.
		{
			name: "the client configuration repointed at another master",
			mutateNew: func(pod *core.Pod) {
				pod.Annotations[inject.ClientConfigAnnotationKey] = `{"master_server_address":"attacker:50051"}`
			},
			wantMsg: "swaps the master address, transport and segment sizes under a running container",
		},
		{
			name:      "the client configuration removed",
			mutateNew: func(pod *core.Pod) { delete(pod.Annotations, inject.ClientConfigAnnotationKey) },
			wantMsg:   "swaps the master address, transport and segment sizes under a running container",
		},

		// The positive baseline: what an opted-in Pod must still be able to do.
		{
			name:      "an update that moves nothing",
			mutateNew: func(*core.Pod) {},
		},
		{
			name:      "a finalizer removed",
			mutateOld: func(pod *core.Pod) { pod.Finalizers = []string{"kueue.x-k8s.io/managed"} },
			mutateNew: func(*core.Pod) {},
		},
		{
			name:      "an unrelated label added",
			mutateNew: func(pod *core.Pod) { pod.Labels["team"] = "a" },
		},
		{
			name:      "an unrelated annotation added",
			mutateNew: func(pod *core.Pod) { pod.Annotations["example.com/note"] = "hello" },
		},
		{
			name:      "a status write",
			mutateNew: func(pod *core.Pod) { pod.Status.Phase = core.PodRunning },
		},
		{
			name:      "the container image changed",
			mutateNew: func(pod *core.Pod) { pod.Spec.Containers[0].Image = "vllm:v0.25.2" },
		},
		{
			// Deliberately allowed. The engine annotation is an INPUT, read once at CREATE, and
			// nothing re-renders from it, so a later edit changes no behavior. The record an
			// operator is told to read is the frozen one, and the reference page points there.
			name:      "an input annotation edited after the fact",
			mutateNew: func(pod *core.Pod) { pod.Annotations[KVCacheEngineAnnotationKey] = "sglang" },
		},
		{
			// Defense in depth for a widened objectSelector, matching the mutating half's own guard.
			// A forged record on such a Pod is inert: nothing projects it and no container reads it.
			name: "a Pod that never opted in on either side",
			base: neverInjectedKVCachePod,
			mutateNew: func(pod *core.Pod) {
				pod.Annotations[KVCacheInjectedAnnotationKey] = `{"binding":"forged"}`
			},
		},
	})
}

// TestPodKVCacheValidateUpdate_RefusesEveryOwnedKeyAtOnce. The handler accumulates rather than
// returning on the first fault, so one strip-everything edit reports all three keys instead of
// sending its author back one round per key.
func TestPodKVCacheValidateUpdate_RefusesEveryOwnedKeyAtOnce(t *testing.T) {
	oldPod := injectedKVCachePod(t)
	newPod := oldPod.DeepCopy()
	delete(newPod.Labels, KVCacheInjectLabelKey)
	delete(newPod.Annotations, KVCacheInjectedAnnotationKey)
	delete(newPod.Annotations, inject.ClientConfigAnnotationKey)

	_, err := (&PodKVCacheWebhook{}).ValidateUpdate(context.Background(), oldPod, newPod)
	require.Error(t, err)

	msg := err.Error()
	assert.Contains(t, msg, KVCacheInjectLabelKey)
	assert.Contains(t, msg, KVCacheInjectedAnnotationKey)
	assert.Contains(t, msg, inject.ClientConfigAnnotationKey)
}

// TestPodKVCacheValidateUpdate_EveryRefusalNamesTheWayForward is the assertion behind a property the
// message text carries rather than the code.
//
// The subject of an UPDATE refusal is a Pod that is already running, so "you may not edit this" alone
// leaves its author retrying the same patch. Each refusal has to name the operation that does work,
// and it is not deleting this Pod: an owned Pod comes back from the same template, identical. The
// edit belongs where the Pod is defined, and every message says so in the same words.
func TestPodKVCacheValidateUpdate_EveryRefusalNamesTheWayForward(t *testing.T) {
	for _, c := range []podKVCacheUpdateCase{
		{
			name: "the opt-in label added",
			base: neverInjectedKVCachePod,
			mutateNew: func(pod *core.Pod) {
				pod.Labels[KVCacheInjectLabelKey] = KVCacheInjectLabelValue
			},
		},
		{
			name:      "the opt-in label dropped",
			mutateNew: func(pod *core.Pod) { delete(pod.Labels, KVCacheInjectLabelKey) },
		},
		{
			name: "the record edited",
			mutateNew: func(pod *core.Pod) {
				pod.Annotations[KVCacheInjectedAnnotationKey] = `{}`
			},
		},
		{
			name: "the configuration edited",
			mutateNew: func(pod *core.Pod) {
				pod.Annotations[inject.ClientConfigAnnotationKey] = `{}`
			},
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			build := c.base
			if build == nil {
				build = injectedKVCachePod
			}
			oldPod := build(t)
			newPod := oldPod.DeepCopy()
			c.mutateNew(newPod)

			_, err := (&PodKVCacheWebhook{}).ValidateUpdate(context.Background(), oldPod, newPod)
			require.Error(t, err)
			assert.Contains(t, err.Error(), "let the Pod be replaced",
				"an UPDATE refusal whose subject is a running Pod must name the operation that "+
					"works; \"delete this Pod\" is not it, since an owned Pod returns identical")
		})
	}
}

// TestPodKVCacheValidateUpdate_SurvivesTheDeletionGuard is the only assertion here that the table
// above cannot make, and it is deliberately shaped differently.
//
// `ExecuteSetup` wraps every validator in a guard that returns success as soon as the new object
// carries a deletion timestamp, UNLESS the handler implements `webhook.ReceiveDeletionUpdate`
// (`pkg/webhook/helper.go:67-80`, `:123-128`). The decision is one type assertion on the handler, and
// the first assertion below is that same expression - so this tests the input the framework branches
// on, not a restatement of it.
//
// The second assertion cannot stand alone. Calling ValidateUpdate directly bypasses the wrapper
// entirely, so a handler that was being skipped in production would still refuse here: measured, this
// call refuses identically for a live Pod and a terminating one. Every row of the table above shares
// that blindness. Only the marker decides whether any of them describe production.
func TestPodKVCacheValidateUpdate_SurvivesTheDeletionGuard(t *testing.T) {
	var handler any = &PodKVCacheWebhook{}
	_, keepsDeletionUpdate := handler.(webhook.ReceiveDeletionUpdate)
	require.True(t, keepsDeletionUpdate,
		"without this marker the shared guard skips update validation for a terminating Pod, and "+
			"all three keys become editable for the whole grace period - while the container still "+
			"runs and the kubelet still reprojects the client configuration from the annotation")

	// A terminating Pod is where the freeze is worth most: the container is still serving through its
	// grace period, and the projected file follows the annotation.
	oldPod := injectedKVCachePod(t)
	deletedAt := meta.NewTime(oldPod.CreationTimestamp.Time)
	oldPod.DeletionTimestamp = &deletedAt
	oldPod.Finalizers = []string{"kueue.x-k8s.io/managed"}

	swapped := oldPod.DeepCopy()
	swapped.Annotations[inject.ClientConfigAnnotationKey] = `{"master_server_address":"attacker:50051"}`
	_, err := (&PodKVCacheWebhook{}).ValidateUpdate(context.Background(), oldPod, swapped)
	require.Error(t, err, "the master address may not be swapped under a container that is still running")

	// The control, on the same terminating Pod: the update that has to keep working while it drains.
	drained := oldPod.DeepCopy()
	drained.Finalizers = nil
	_, err = (&PodKVCacheWebhook{}).ValidateUpdate(context.Background(), oldPod, drained)
	require.NoError(t, err,
		"clearing a finalizer touches none of the three keys, which is why opting out of the guard "+
			"cannot strand a terminating Pod")
}

// validatingWebhookNamed finds one entry of the generated validating configuration.
func validatingWebhookNamed(t *testing.T, name string) admreg.ValidatingWebhook {
	t.Helper()

	cfg := GetValidatingWebhookConfiguration("gpustack-worker-validation", admreg.WebhookClientConfig{})
	for i := range cfg.Webhooks {
		if cfg.Webhooks[i].Name == name {
			return cfg.Webhooks[i]
		}
	}
	require.Failf(t, "webhook not registered", "no entry named %q", name)
	return admreg.ValidatingWebhook{}
}

// TestKVCacheWebhookConfig_UpdateGuardIsRegisteredAndFailsOpen pins the registration this guard needs
// and the one policy on it whose value is a decision rather than a default.
//
// failurePolicy is Ignore here against Fail on the mutating half, and the asymmetry is the point. This
// entry matches UPDATE on Pods, so under Fail an unreachable webhook would block finalizer removal on
// every opted-in Pod - and this feature's own teardown waits on exactly that, since a Binding's
// finalizer holds while status.usedBy is non-empty and that cannot empty while its Pods cannot finish
// deleting. A missed refusal costs one wrong record; a wedged deletion costs the pool.
func TestKVCacheWebhookConfig_UpdateGuardIsRegisteredAndFailsOpen(t *testing.T) {
	wh := validatingWebhookNamed(t, "validate.gpustack-worker-kvcache.core.v1.pod")

	require.Len(t, wh.Rules, 1)
	assert.Equal(t, []admreg.OperationType{admreg.Update}, wh.Rules[0].Operations,
		"CREATE belongs to the mutating half, which refuses from Default; DELETE must never be "+
			"refused here, since that is the one thing that could leave a workload unable to finish")
	assert.Equal(t, []string{"pods"}, wh.Rules[0].Resources,
		"the main resource only: pods/status is the kubelet's write path and must not be gated")

	require.NotNil(t, wh.FailurePolicy)
	assert.Equal(t, admreg.Ignore, *wh.FailurePolicy,
		"Fail here would put an unreachable webhook in the path of finalizer removal on every "+
			"opted-in Pod, which this feature's own teardown then waits behind")

	require.NotNil(t, wh.SideEffects)
	assert.Equal(t, admreg.SideEffectClassNone, *wh.SideEffects)

	require.NotNil(t, wh.ObjectSelector)
	require.Len(t, wh.ObjectSelector.MatchExpressions, 1)
	expr := wh.ObjectSelector.MatchExpressions[0]
	assert.Equal(t, KVCacheInjectLabelKey, expr.Key,
		"the selector is evaluated against BOTH objects and matches if either does, which is what "+
			"makes a label transition in either direction reach this handler")
	assert.Equal(t, meta.LabelSelectorOperator("In"), expr.Operator)
	assert.Equal(t, []string{KVCacheInjectLabelValue}, expr.Values)
}

// TestKVCacheWebhookConfig_TheOtherPodValidatorIsUntouched. Both Pod validating entries live in one
// configuration object, so a shared name would be rejected at install time and a shared path would put
// two handlers on one mux route. The accelerator validator stays CREATE-only.
func TestKVCacheWebhookConfig_TheOtherPodValidatorIsUntouched(t *testing.T) {
	kvcache := validatingWebhookNamed(t, "validate.gpustack-worker-kvcache.core.v1.pod")
	accelerator := validatingWebhookNamed(t, "validate.gpustack-worker.core.v1.pod")

	assert.NotEqual(t, accelerator.Name, kvcache.Name)
	assert.NotEqual(t, (&PodWebhook{}).ValidatePath(), (&PodKVCacheWebhook{}).ValidatePath())
	assert.Equal(t, "/validate-gpustack-worker-kvcache-core-v1-pod",
		(&PodKVCacheWebhook{}).ValidatePath())

	require.Len(t, accelerator.Rules, 1)
	assert.Equal(t, []admreg.OperationType{admreg.Create}, accelerator.Rules[0].Operations,
		"the accelerator validator's own contract is create-time; this change must not widen it")
}
