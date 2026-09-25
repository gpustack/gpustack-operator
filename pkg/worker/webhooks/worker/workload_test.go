package worker

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	admissionv1 "k8s.io/api/admission/v1"
	admreg "k8s.io/api/admissionregistration/v1"
	core "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrlcli "sigs.k8s.io/controller-runtime/pkg/client"
	ctrlfake "sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	ctrladmission "sigs.k8s.io/controller-runtime/pkg/webhook/admission"
	kueue "sigs.k8s.io/kueue/apis/kueue/v1beta2"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	"gpustack.ai/gpustack/pkg/kubeclients/kubernetes/scheme"
	"gpustack.ai/gpustack/pkg/nodefeature"
	"gpustack.ai/gpustack/pkg/setting/settingtest"
	"gpustack.ai/gpustack/pkg/systemmeta"
)

const (
	_fitInstanceType = "gpustack--nvidia-t4-linux-amd64"
	_fitGroup        = "nvidia-t4"
	_fitNamespace    = "team-a"
)

var (
	_fitQueue      = nodefeature.FormatLocalQueueName(_fitInstanceType)
	_fitSlicedKey  = nodefeature.FitSlicedMaxFreeUnitsLabelKey(_fitGroup)
	_fitSharedKey  = nodefeature.FitSharedFreeCardsLabelKey(_fitGroup)
	_fitCountLabel = nodefeature.AcceleratableFeatureLabelPrefix + _fitGroup + ".count"
)

// seedFitAffinity stores the workload-fit-affinity setting in the delegated Secret the setting is
// read from. Every case seeds it, so none depends on what a read without it yields.
func seedFitAffinity(t *testing.T, value string) {
	t.Helper()
	settingtest.MergeDelegatedSettings(t, map[string]string{"workload-fit-affinity": value})
}

// fitChain is the operator's queue chain for one InstanceType: its LocalQueue in the Workload's
// namespace, its ClusterQueue carrying the InstanceType mark, and the InstanceType naming the group.
func fitChain() []ctrlcli.Object {
	lq := &kueue.LocalQueue{
		ObjectMeta: meta.ObjectMeta{Namespace: _fitNamespace, Name: _fitQueue},
		Spec:       kueue.LocalQueueSpec{ClusterQueue: _fitInstanceType},
	}
	cq := &kueue.ClusterQueue{ObjectMeta: meta.ObjectMeta{Name: _fitInstanceType}}
	systemmeta.NoteResource(cq, "instancetypes", nil)
	it := &workercore.InstanceType{
		ObjectMeta: meta.ObjectMeta{Name: _fitInstanceType},
		Spec:       workercore.InstanceTypeSpec{AcceleratorGroup: _fitGroup},
	}
	return []ctrlcli.Object{lq, cq, it}
}

func newWorkloadWebhook(objs ...ctrlcli.Object) *WorkloadWebhook {
	return &WorkloadWebhook{Client: ctrlfake.NewClientBuilder().WithScheme(scheme.Scheme).WithObjects(objs...).Build()}
}

// fitPodSet is one PodSet whose single container requests the given resources.
func fitPodSet(name string, reqs map[core.ResourceName]string) kueue.PodSet {
	rl := core.ResourceList{}
	for n, v := range reqs {
		rl[n] = resource.MustParse(v)
	}
	ps := kueue.PodSet{Name: kueue.NewPodSetReference(name), Count: 1}
	ps.Template.Spec.Containers = []core.Container{{
		Name: "main", Resources: core.ResourceRequirements{Requests: rl, Limits: rl.DeepCopy()},
	}}
	return ps
}

func fitWorkload(queue string, podSets ...kueue.PodSet) *kueue.Workload {
	return &kueue.Workload{
		ObjectMeta: meta.ObjectMeta{Namespace: _fitNamespace, Name: "wl"},
		Spec:       kueue.WorkloadSpec{QueueName: kueue.LocalQueueName(queue), PodSets: podSets},
	}
}

var (
	_slice800k = map[core.ResourceName]string{"nvidia.com/gpu.sliced": "1", "nvidia.com/gpu.sliced.units": "800000"}
	_shared2   = map[core.ResourceName]string{"nvidia.com/gpu.shared": "2"}
)

// requiredTerms returns a PodSet's required node-selector terms, nil when it has none.
func requiredTerms(ps kueue.PodSet) []core.NodeSelectorTerm {
	a := ps.Template.Spec.Affinity
	if a == nil || a.NodeAffinity == nil || a.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution == nil {
		return nil
	}
	return a.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution.NodeSelectorTerms
}

func gt(key, v string) core.NodeSelectorRequirement {
	return core.NodeSelectorRequirement{Key: key, Operator: core.NodeSelectorOpGt, Values: []string{v}}
}

func TestWorkloadWebhook_Default(t *testing.T) {
	seedFitAffinity(t, "true")

	withTerms := func(ps kueue.PodSet, terms ...core.NodeSelectorTerm) kueue.PodSet {
		ps.Template.Spec.Affinity = &core.Affinity{NodeAffinity: &core.NodeAffinity{
			RequiredDuringSchedulingIgnoredDuringExecution: &core.NodeSelector{NodeSelectorTerms: terms},
		}}
		return ps
	}

	cases := []struct {
		name  string
		wl    *kueue.Workload
		terms [][]core.NodeSelectorTerm
	}{
		{
			name:  "a slice is pinned to a card with its units free",
			wl:    fitWorkload(_fitQueue, fitPodSet("main", _slice800k)),
			terms: [][]core.NodeSelectorTerm{{{MatchExpressions: []core.NodeSelectorRequirement{gt(_fitSlicedKey, "799999")}}}},
		},
		{
			name:  "a shared request of two is pinned to two cards with a free share",
			wl:    fitWorkload(_fitQueue, fitPodSet("main", _shared2)),
			terms: [][]core.NodeSelectorTerm{{{MatchExpressions: []core.NodeSelectorRequirement{gt(_fitSharedKey, "1")}}}},
		},
		{
			name:  "a shared request of one is not pinned",
			wl:    fitWorkload(_fitQueue, fitPodSet("main", map[core.ResourceName]string{"nvidia.com/gpu.shared": "1"})),
			terms: [][]core.NodeSelectorTerm{nil},
		},
		{
			name:  "exclusive is not pinned",
			wl:    fitWorkload(_fitQueue, fitPodSet("main", map[core.ResourceName]string{"nvidia.com/gpu": "2"})),
			terms: [][]core.NodeSelectorTerm{nil},
		},
		{
			name: "a partition is not pinned",
			wl: fitWorkload(_fitQueue, fitPodSet("main", map[core.ResourceName]string{
				"nvidia.com/gpu.partitioned": "1", "nvidia.com/gpu.partitioned.mig-1g.10gb": "1",
			})),
			terms: [][]core.NodeSelectorTerm{nil},
		},
		{
			name:  "a PodSet without accelerators is not pinned",
			wl:    fitWorkload(_fitQueue, fitPodSet("main", map[core.ResourceName]string{core.ResourceCPU: "1"})),
			terms: [][]core.NodeSelectorTerm{nil},
		},
		{
			name: "each PodSet is pinned with its own demand",
			wl: fitWorkload(_fitQueue,
				fitPodSet("prefill", _slice800k),
				fitPodSet("router", map[core.ResourceName]string{core.ResourceCPU: "1"}),
				fitPodSet("decode", map[core.ResourceName]string{"nvidia.com/gpu.sliced": "1", "nvidia.com/gpu.sliced.units": "480000"}),
			),
			terms: [][]core.NodeSelectorTerm{
				{{MatchExpressions: []core.NodeSelectorRequirement{gt(_fitSlicedKey, "799999")}}},
				nil,
				{{MatchExpressions: []core.NodeSelectorRequirement{gt(_fitSlicedKey, "479999")}}},
			},
		},
		{
			name: "the pin is ANDed into every existing term, next to the static card-count pin",
			wl: fitWorkload(_fitQueue, withTerms(fitPodSet("main", _shared2),
				core.NodeSelectorTerm{MatchExpressions: []core.NodeSelectorRequirement{gt(_fitCountLabel, "1")}},
				core.NodeSelectorTerm{MatchExpressions: []core.NodeSelectorRequirement{{
					Key: core.LabelTopologyZone, Operator: core.NodeSelectorOpIn, Values: []string{"a"},
				}}},
			)),
			terms: [][]core.NodeSelectorTerm{{
				{MatchExpressions: []core.NodeSelectorRequirement{gt(_fitCountLabel, "1"), gt(_fitSharedKey, "1")}},
				{MatchExpressions: []core.NodeSelectorRequirement{
					{Key: core.LabelTopologyZone, Operator: core.NodeSelectorOpIn, Values: []string{"a"}},
					gt(_fitSharedKey, "1"),
				}},
			}},
		},
		{
			name: "a stricter requirement on the same key is kept beside the pin",
			wl: fitWorkload(_fitQueue, withTerms(fitPodSet("main", _slice800k),
				core.NodeSelectorTerm{MatchExpressions: []core.NodeSelectorRequirement{gt(_fitSlicedKey, "999999")}},
			)),
			terms: [][]core.NodeSelectorTerm{{{MatchExpressions: []core.NodeSelectorRequirement{
				gt(_fitSlicedKey, "999999"), gt(_fitSlicedKey, "799999"),
			}}}},
		},
		{
			name: "a required selector with no term still matches no node",
			wl: func() *kueue.Workload {
				wl := fitWorkload(_fitQueue, fitPodSet("main", _slice800k))
				wl.Spec.PodSets[0].Template.Spec.Affinity = &core.Affinity{NodeAffinity: &core.NodeAffinity{
					RequiredDuringSchedulingIgnoredDuringExecution: &core.NodeSelector{},
				}}
				return wl
			}(),
			terms: [][]core.NodeSelectorTerm{nil},
		},
		{
			name: "an empty term still matches no node",
			wl: fitWorkload(_fitQueue, withTerms(fitPodSet("main", _slice800k),
				core.NodeSelectorTerm{},
				core.NodeSelectorTerm{MatchExpressions: []core.NodeSelectorRequirement{{
					Key: core.LabelTopologyZone, Operator: core.NodeSelectorOpIn, Values: []string{"a"},
				}}},
			)),
			terms: [][]core.NodeSelectorTerm{{
				{},
				{MatchExpressions: []core.NodeSelectorRequirement{
					{Key: core.LabelTopologyZone, Operator: core.NodeSelectorOpIn, Values: []string{"a"}},
					gt(_fitSlicedKey, "799999"),
				}},
			}},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := newWorkloadWebhook(fitChain()...)
			require.NoError(t, r.Default(context.Background(), c.wl))
			require.Len(t, c.wl.Spec.PodSets, len(c.terms))
			for i := range c.terms {
				assert.Equal(t, c.terms[i], requiredTerms(c.wl.Spec.PodSets[i]), "podset %d", i)
			}

			// A second pass adds nothing.
			again := c.wl.DeepCopy()
			require.NoError(t, r.Default(context.Background(), again))
			assert.Equal(t, c.wl, again)
		})
	}
}

func TestWorkloadWebhook_ForeignWorkloadsUntouched(t *testing.T) {
	seedFitAffinity(t, "true")

	chain := fitChain()
	lq, cq, it := chain[0], chain[1], chain[2]
	unmarkedCQ := &kueue.ClusterQueue{ObjectMeta: meta.ObjectMeta{Name: _fitInstanceType}}
	grouplessIT := &workercore.InstanceType{ObjectMeta: meta.ObjectMeta{Name: _fitInstanceType}}
	malformedIT := &workercore.InstanceType{
		ObjectMeta: meta.ObjectMeta{Name: _fitInstanceType},
		Spec:       workercore.InstanceTypeSpec{AcceleratorGroup: "Bad/Group"},
	}

	cases := []struct {
		name  string
		queue string
		objs  []ctrlcli.Object
	}{
		{name: "a queue name the operator does not use", queue: "research", objs: chain},
		{name: "an operator-shaped queue name without a LocalQueue", queue: _fitQueue, objs: []ctrlcli.Object{cq, it}},
		{name: "a LocalQueue on a ClusterQueue without the InstanceType mark", queue: _fitQueue, objs: []ctrlcli.Object{lq, unmarkedCQ, it}},
		{name: "a marked ClusterQueue without an InstanceType", queue: _fitQueue, objs: []ctrlcli.Object{lq, cq}},
		{name: "an InstanceType without an accelerator group", queue: _fitQueue, objs: []ctrlcli.Object{lq, cq, grouplessIT}},
		{name: "an InstanceType whose group is not label grammar", queue: _fitQueue, objs: []ctrlcli.Object{lq, cq, malformedIT}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			wl := fitWorkload(c.queue, fitPodSet("main", _slice800k))
			before := wl.DeepCopy()
			require.NoError(t, newWorkloadWebhook(c.objs...).Default(context.Background(), wl))
			assert.Equal(t, before, wl)
		})
	}
}

func TestWorkloadWebhook_LookupErrorLeavesTheWorkloadUnpinned(t *testing.T) {
	seedFitAffinity(t, "true")

	cli := ctrlfake.NewClientBuilder().WithScheme(scheme.Scheme).WithObjects(fitChain()...).
		WithInterceptorFuncs(interceptor.Funcs{
			Get: func(ctx context.Context, c ctrlcli.WithWatch, key ctrlcli.ObjectKey, obj ctrlcli.Object, opts ...ctrlcli.GetOption) error {
				if _, ok := obj.(*kueue.ClusterQueue); ok {
					return errors.New("the API server is unavailable")
				}
				return c.Get(ctx, key, obj, opts...)
			},
		}).Build()
	wl := fitWorkload(_fitQueue, fitPodSet("main", _slice800k))
	before := wl.DeepCopy()
	require.NoError(t, (&WorkloadWebhook{Client: cli}).Default(context.Background(), wl), "the webhook never denies")
	assert.Equal(t, before, wl)
}

func updateContext(t *testing.T, old *kueue.Workload) context.Context {
	t.Helper()
	raw, err := json.Marshal(old)
	require.NoError(t, err)
	return ctrladmission.NewContextWithRequest(context.Background(), ctrladmission.Request{
		AdmissionRequest: admissionv1.AdmissionRequest{
			Operation: admissionv1.Update,
			Namespace: _fitNamespace,
			OldObject: runtime.RawExtension{Raw: raw},
		},
	})
}

func TestWorkloadWebhook_Update(t *testing.T) {
	seedFitAffinity(t, "true")

	unreserved := fitWorkload(_fitQueue, fitPodSet("main", _slice800k))
	reserved := unreserved.DeepCopy()
	reserved.Status.Conditions = []meta.Condition{{
		Type: kueue.WorkloadQuotaReserved, Status: meta.ConditionTrue, Reason: "QuotaReserved",
	}}

	cases := []struct {
		name  string
		old   *kueue.Workload
		terms []core.NodeSelectorTerm
	}{
		{
			name:  "a spec rebuilt before reservation is pinned again",
			old:   unreserved,
			terms: []core.NodeSelectorTerm{{MatchExpressions: []core.NodeSelectorRequirement{gt(_fitSlicedKey, "799999")}}},
		},
		{
			name: "an update of a Workload holding quota is left alone",
			old:  reserved,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			wl := c.old.DeepCopy()
			require.NoError(t, newWorkloadWebhook(fitChain()...).Default(updateContext(t, c.old), wl))
			assert.Equal(t, c.terms, requiredTerms(wl.Spec.PodSets[0]))
		})
	}
}

func TestWorkloadWebhook_SettingOff(t *testing.T) {
	seedFitAffinity(t, "false")

	wl := fitWorkload(_fitQueue, fitPodSet("main", _slice800k))
	before := wl.DeepCopy()
	require.NoError(t, newWorkloadWebhook(fitChain()...).Default(context.Background(), wl))
	assert.Equal(t, before, wl)
}

// TestWorkloadWebhook_PatchTouchesOnlyAffinity runs the webhook through controller-runtime's
// defaulting handler on raw Workload JSON, as the API server sends it, including a field the vendored
// Kueue type does not know. The patch an operator Workload gets must only add affinity, and a foreign
// Workload must get no patch at all.
func TestWorkloadWebhook_PatchTouchesOnlyAffinity(t *testing.T) {
	seedFitAffinity(t, "true")

	handler := ctrladmission.WithCustomDefaulter(scheme.Scheme, &kueue.Workload{}, newWorkloadWebhook(fitChain()...))
	rawOf := func(queue string) []byte {
		raw, err := json.Marshal(fitWorkload(queue, fitPodSet("main", _slice800k)))
		require.NoError(t, err)
		var obj map[string]any
		require.NoError(t, json.Unmarshal(raw, &obj))
		obj["apiVersion"], obj["kind"] = kueue.GroupVersion.String(), "Workload"
		obj["spec"].(map[string]any)["fieldFromANewerKueue"] = "kept"
		raw, err = json.Marshal(obj)
		require.NoError(t, err)
		return raw
	}
	handle := func(queue string) ctrladmission.Response {
		return handler.Handle(context.Background(), ctrladmission.Request{AdmissionRequest: admissionv1.AdmissionRequest{
			Operation: admissionv1.Create,
			Namespace: _fitNamespace,
			Object:    runtime.RawExtension{Raw: rawOf(queue)},
		}})
	}

	pinned := handle(_fitQueue)
	require.True(t, pinned.Allowed)
	require.NotEmpty(t, pinned.Patches)
	for _, p := range pinned.Patches {
		assert.True(t, strings.HasPrefix(p.Path, "/spec/podSets/0/template/spec/affinity"), "patch %s %s", p.Operation, p.Path)
	}

	foreign := handle("research")
	require.True(t, foreign.Allowed)
	assert.Empty(t, foreign.Patches)
	assert.Nil(t, foreign.PatchType)
}

func TestWorkloadWebhookRegistration(t *testing.T) {
	_, mwc := GetWebhookConfigurations("test", admreg.WebhookClientConfig{})
	var wh *admreg.MutatingWebhook
	for i := range mwc.Webhooks {
		if rulesCoverResource(mwc.Webhooks[i].Rules, "workloads") {
			wh = &mwc.Webhooks[i]
		}
	}
	require.NotNil(t, wh, "a mutating webhook on workloads is registered")

	require.Len(t, wh.Rules, 1)
	assert.Equal(t, []string{"kueue.x-k8s.io"}, wh.Rules[0].APIGroups)
	assert.ElementsMatch(t, []admreg.OperationType{admreg.Create, admreg.Update}, wh.Rules[0].Operations)
	require.NotNil(t, wh.FailurePolicy)
	assert.Equal(t, admreg.Ignore, *wh.FailurePolicy, "the pin is optional; Kueue must always be able to create a Workload")
	require.Len(t, wh.MatchConditions, 1)
	assert.Equal(t,
		"has(object.spec.queueName) && object.spec.queueName.startsWith('"+nodefeature.LocalQueueNamePrefix+"')",
		wh.MatchConditions[0].Expression)
}

func rulesCoverResource(rules []admreg.RuleWithOperations, resource string) bool {
	return slices.ContainsFunc(rules, func(r admreg.RuleWithOperations) bool {
		return slices.Contains(r.Resources, resource)
	})
}
