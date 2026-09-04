package worker

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	admreg "k8s.io/api/admissionregistration/v1"
	core "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	kueuectrlconst "sigs.k8s.io/kueue/pkg/controller/constants"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	"gpustack.ai/gpustack/pkg/nodefeature"
)

// This file holds the invariants that would otherwise rot without anything failing. Each one is a
// property of the whole arrangement rather than of a function: which Pods this webhook is asked
// about, how it coexists with the other Pod webhook, and what it is allowed to change. None of them
// would break a unit test of the code they constrain, which is why they are written down separately.

// mutatingWebhookNamed finds one entry of the generated mutating configuration.
func mutatingWebhookNamed(t *testing.T, name string) admreg.MutatingWebhook {
	t.Helper()

	cfg := GetMutatingWebhookConfiguration("gpustack-worker-mutation", admreg.WebhookClientConfig{})
	for i := range cfg.Webhooks {
		if cfg.Webhooks[i].Name == name {
			return cfg.Webhooks[i]
		}
	}
	require.Failf(t, "webhook not registered", "no entry named %q", name)
	return admreg.MutatingWebhook{}
}

// TestKVCacheWebhookConfig_SelectorIsTheInjectLabel is the assertion Risks asks for by name.
//
// The selector reverting to the queue-name label would quietly reduce this webhook to serving only
// this project's own Kueue chain, which is the exact failure the spec exists to avoid - and it would
// break nothing else, because every Pod that still matched would still be injected correctly.
func TestKVCacheWebhookConfig_SelectorIsTheInjectLabel(t *testing.T) {
	wh := mutatingWebhookNamed(t, "mutate.gpustack-worker-kvcache.core.v1.pod")

	require.NotNil(t, wh.ObjectSelector)
	require.Len(t, wh.ObjectSelector.MatchExpressions, 1)

	expr := wh.ObjectSelector.MatchExpressions[0]
	assert.Equal(t, "kvcache.gpustack.ai/inject", expr.Key,
		"a Pod opts in with this label; selecting on anything else changes which Pods are served")
	assert.NotEqual(t, kueuectrlconst.QueueLabel, expr.Key,
		"the queue-name label is the OTHER Pod webhook's trigger, and using it here would restrict "+
			"this one to Pods already in this project's scheduling chain")
	assert.Equal(t, meta.LabelSelectorOperator("In"), expr.Operator)
	assert.Equal(t, []string{"true"}, expr.Values)
}

// TestKVCacheWebhookConfig_TwoPodEntriesAreDistinct. Both Pod mutating webhooks live in one
// configuration object, so a shared name would be rejected by the API server at install time and a
// shared path would put two handlers on one mux route.
func TestKVCacheWebhookConfig_TwoPodEntriesAreDistinct(t *testing.T) {
	kvcache := mutatingWebhookNamed(t, "mutate.gpustack-worker-kvcache.core.v1.pod")
	accelerator := mutatingWebhookNamed(t, "mutate.gpustack-worker.core.v1.pod")

	assert.NotEqual(t, accelerator.Name, kvcache.Name)
	assert.NotEqual(t, (&PodWebhook{}).DefaultPath(), (&PodKVCacheWebhook{}).DefaultPath())
	assert.Equal(t, "/mutate-gpustack-worker-kvcache-core-v1-pod", (&PodKVCacheWebhook{}).DefaultPath())
}

// TestKVCacheWebhookConfig_SingleConfigurationObject. A configuration object of its own would enter
// the API server's ordering by its own name, and the prefix chosen here sorts before Kueue's
// deliberately. Both entries must therefore stay in the one object.
func TestKVCacheWebhookConfig_SingleConfigurationObject(t *testing.T) {
	cfg := GetMutatingWebhookConfiguration("gpustack-worker-mutation", admreg.WebhookClientConfig{})

	var podEntries int
	for i := range cfg.Webhooks {
		for _, rule := range cfg.Webhooks[i].Rules {
			if len(rule.Resources) == 1 && rule.Resources[0] == "pods" {
				podEntries++
			}
		}
	}
	assert.Equal(t, 2, podEntries, "both Pod mutating entries live in %q", cfg.Name)
}

// TestKVCacheWebhookConfig_FailsClosedAndIsNotReinvoked pins the two policies whose wrong value is
// silent. Ignore would let an opted-in Pod start with no cache while its owner believed otherwise;
// IfNeeded would re-enter Default on this webhook's own output, which the conflict rule then refuses.
func TestKVCacheWebhookConfig_FailsClosedAndIsNotReinvoked(t *testing.T) {
	wh := mutatingWebhookNamed(t, "mutate.gpustack-worker-kvcache.core.v1.pod")

	require.NotNil(t, wh.FailurePolicy)
	assert.Equal(t, admreg.Fail, *wh.FailurePolicy)
	require.NotNil(t, wh.ReinvocationPolicy)
	assert.Equal(t, admreg.NeverReinvocationPolicy, *wh.ReinvocationPolicy)
	require.NotNil(t, wh.SideEffects)
	assert.Equal(t, admreg.SideEffectClassNone, *wh.SideEffects,
		"the webhook only reads cluster state; it creates no ConfigMap and no object of any kind")
}

// TestPodKVCacheDefault_UnoptedPodIsUntouched covers the two shapes that must come out identical.
//
// The objectSelector already keeps both away in production, so this is defense in depth. It is worth
// asserting because widening that selector is a one-line edit in a generated file, and a webhook that
// fails closed would then stop every Pod in the cluster rather than ignore it.
func TestPodKVCacheDefault_UnoptedPodIsUntouched(t *testing.T) {
	testCases := []struct {
		name  string
		label func(pod *core.Pod)
	}{
		{name: "no label at all", label: func(pod *core.Pod) { delete(pod.Labels, KVCacheInjectLabelKey) }},
		{name: "the label set to false", label: func(pod *core.Pod) { pod.Labels[KVCacheInjectLabelKey] = "false" }},
		{name: "the label set to something else", label: func(pod *core.Pod) { pod.Labels[KVCacheInjectLabelKey] = "yes" }},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			pod := kvCachePod()
			tc.label(pod)
			before := pod.DeepCopy()

			require.NoError(t, admit(t, pod))
			assert.Equal(t, before, pod, "a Pod that did not opt in is byte-identical after admission")
		})
	}
}

// TestPodWebhooks_AreOrderIndependent runs both Pod mutating webhooks over one Pod in both orders.
//
// A Pod may legitimately carry both labels: an accelerator workload that also wants a cache. The two
// touch disjoint fields today, and the generator decides their order by Go type name, so a rename
// would silently reverse it. This test is what makes that reversal harmless rather than unnoticed.
func TestPodWebhooks_AreOrderIndependent(t *testing.T) {
	nvidiaBase := nodefeature.GetAcceleratableResourceName(
		nodefeature.ManufacturerNVIDIA, workercore.DeviceAllocationModeExclusive)
	names := logicalResourceNamesForBase(nvidiaBase)

	// A Pod in both worlds: routed to a GPUStack queue with a sliced request, and opted into a cache.
	build := func() *core.Pod {
		pod := slicedPod(map[core.ResourceName]string{
			names.card:   "1",
			names.memPct: "50",
		})
		pod.Namespace = "team-a"
		pod.Spec.Containers[0].Args = []string{"serve"}
		pod.Labels[KVCacheInjectLabelKey] = KVCacheInjectLabelValue
		pod.Annotations = map[string]string{
			KVCacheBindingAnnotationKey: "chat",
			KVCacheEngineAnnotationKey:  "vllm",
		}
		return pod
	}

	accelerator := newPodWebhook(instanceTypeWithEntrance("80Gi"))
	kvcache := newPodKVCacheWebhook(kvCacheFixture()...)

	acceleratorFirst := build()
	require.NoError(t, accelerator.Default(context.Background(), acceleratorFirst))
	require.NoError(t, kvcache.Default(context.Background(), acceleratorFirst))

	kvcacheFirst := build()
	require.NoError(t, kvcache.Default(context.Background(), kvcacheFirst))
	require.NoError(t, accelerator.Default(context.Background(), kvcacheFirst))

	assert.Equal(t, acceleratorFirst, kvcacheFirst,
		"the two webhooks mutate disjoint fields, so neither order can produce a different object")

	// The control: both webhooks actually did something, or the equality above would be the trivial
	// one between two untouched Pods.
	require.Contains(t, containerEnv(&acceleratorFirst.Spec.Containers[0]), "MOONCAKE_CONFIG_PATH",
		"the cache webhook ran")
	_, folded := containerResource(&acceleratorFirst.Spec.Containers[0], names.units)
	require.True(t, folded, "the accelerator webhook ran and folded the sliced units")
}

// TestPodKVCacheDefault_NeverTouchesResources is the disjointness invariant from the other side, and
// it is asserted on the object rather than on the code: a future edit that reached for a resource
// would be an edit into the other webhook's territory, where the two would then disagree about one
// field with nothing reporting it.
func TestPodKVCacheDefault_NeverTouchesResources(t *testing.T) {
	pod := kvCachePod()
	pod.Spec.Containers[0].Resources = core.ResourceRequirements{
		Requests: core.ResourceList{core.ResourceMemory: resource.MustParse("8Gi")},
		Limits:   core.ResourceList{core.ResourceMemory: resource.MustParse("8Gi")},
	}
	before := pod.Spec.Containers[0].Resources.DeepCopy()

	require.NoError(t, admit(t, pod))
	assert.Equal(t, *before, pod.Spec.Containers[0].Resources,
		"the injected client buffer is host memory the container's own limit must already cover; "+
			"this webhook states that in documentation and never raises the limit itself")
}
