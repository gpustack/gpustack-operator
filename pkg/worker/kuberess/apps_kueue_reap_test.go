package kuberess

import (
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	admreg "k8s.io/api/admissionregistration/v1"
	apiext "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	k8stesting "k8s.io/client-go/testing"

	kubefake "gpustack.ai/gpustack/pkg/kubeclients/kubernetes/fake"
)

var fixedDeletionTime = meta.Date(2026, 7, 15, 9, 0, 0, 0, time.UTC)

// clusterQueuesGVR is the GVR the reaper derives from a Terminating ClusterQueue CRD.
var clusterQueuesGVR = schema.GroupVersionResource{
	Group:    kueueAPIGroup,
	Version:  "v1beta1",
	Resource: "clusterqueues",
}

// kueueCRD builds a Kueue CRD, optionally stuck Terminating.
func kueueCRD(plural, kind string, terminating bool) *apiext.CustomResourceDefinition {
	crd := &apiext.CustomResourceDefinition{
		ObjectMeta: meta.ObjectMeta{Name: plural + "." + kueueAPIGroup},
		Spec: apiext.CustomResourceDefinitionSpec{
			Group: kueueAPIGroup,
			Names: apiext.CustomResourceDefinitionNames{Plural: plural, Kind: kind},
			Versions: []apiext.CustomResourceDefinitionVersion{
				{Name: "v1beta1", Served: true},
			},
		},
	}
	if terminating {
		crd.DeletionTimestamp = fixedDeletionTime.DeepCopy()
	}
	return crd
}

// kueueWebhookConfigs returns the validating+mutating webhook configs the chart ships,
// labeled with the Helm release instance the reaper selects on.
func kueueWebhookConfigs() []runtime.Object {
	labels := map[string]string{"app.kubernetes.io/instance": gpustackOperatorReleaseName}
	return []runtime.Object{
		&admreg.ValidatingWebhookConfiguration{
			ObjectMeta: meta.ObjectMeta{Name: "kueue-validating-webhook-configuration", Labels: labels},
		},
		&admreg.MutatingWebhookConfiguration{
			ObjectMeta: meta.ObjectMeta{Name: "kueue-mutating-webhook-configuration", Labels: labels},
		},
	}
}

// terminatingClusterQueue is an unstructured ClusterQueue stuck Terminating with the
// resource-in-use finalizer.
func terminatingClusterQueue(name string) *unstructured.Unstructured {
	cq := &unstructured.Unstructured{}
	cq.SetGroupVersionKind(schema.GroupVersionKind{Group: kueueAPIGroup, Version: "v1beta1", Kind: "ClusterQueue"})
	cq.SetName(name)
	cq.SetFinalizers([]string{"kueue.x-k8s.io/resource-in-use"})
	cq.SetDeletionTimestamp(fixedDeletionTime.DeepCopy())
	return cq
}

// newDynamicClient builds a fake dynamic client that knows the ClusterQueue list kind.
func newDynamicClient(objs ...runtime.Object) *dynamicfake.FakeDynamicClient {
	gvrToListKind := map[schema.GroupVersionResource]string{
		clusterQueuesGVR: "ClusterQueueList",
	}
	return dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), gvrToListKind, objs...)
}

// Test_reapOrphanedKueueWith_noopWhenHealthy asserts the reaper does nothing when no
// Kueue CRD is Terminating: no webhook deletion, no finalizer strip, reports not acted.
func Test_reapOrphanedKueueWith_noopWhenHealthy(t *testing.T) {
	objs := append([]runtime.Object{kueueCRD("clusterqueues", "ClusterQueue", false)}, kueueWebhookConfigs()...)
	cli := kubefake.NewSimpleClientset(objs...)
	dynCli := newDynamicClient(terminatingClusterQueue("poc-cq"))

	acted, err := reapOrphanedKueueWith(t.Context(), cli, dynCli)
	require.NoError(t, err)
	assert.False(t, acted, "must not act on a healthy cluster")

	for _, a := range cli.Actions() {
		assert.NotEqual(t, "delete-collection", a.GetVerb(), "must not delete webhook configs when healthy")
	}
	for _, a := range dynCli.Actions() {
		assert.NotEqual(t, "patch", a.GetVerb(), "must not strip finalizers when healthy")
	}
}

// Test_reapOrphanedKueueWith_reapsInOrder is the core case: with a Terminating Kueue
// CRD, the reaper deletes the webhook configs (by the correct label) BEFORE stripping
// the ClusterQueue finalizer, and leaves non-Kueue webhooks untouched.
func Test_reapOrphanedKueueWith_reapsInOrder(t *testing.T) {
	otherWebhook := &admreg.ValidatingWebhookConfiguration{
		ObjectMeta: meta.ObjectMeta{Name: "someone-elses-webhook"},
	}
	objs := append([]runtime.Object{kueueCRD("clusterqueues", "ClusterQueue", true), otherWebhook}, kueueWebhookConfigs()...)
	cli := kubefake.NewSimpleClientset(objs...)
	dynCli := newDynamicClient(terminatingClusterQueue("poc-cq"))

	// Record the interleaving of webhook deletion and finalizer strip. Reactors return
	// false so the default tracker still performs the action.
	var order []string
	recordWebhook := func(a k8stesting.Action) (bool, runtime.Object, error) {
		order = append(order, "webhook")
		return false, nil, nil
	}
	cli.PrependReactor("delete-collection", "validatingwebhookconfigurations", recordWebhook)
	cli.PrependReactor("delete-collection", "mutatingwebhookconfigurations", recordWebhook)
	dynCli.PrependReactor("patch", "clusterqueues", func(a k8stesting.Action) (bool, runtime.Object, error) {
		order = append(order, "strip")
		return false, nil, nil
	})

	acted, err := reapOrphanedKueueWith(t.Context(), cli, dynCli)
	require.NoError(t, err)
	assert.True(t, acted, "must act when a kueue CRD is Terminating")

	// Ordering is load-bearing: every webhook deletion precedes the finalizer strip.
	require.Contains(t, order, "webhook")
	require.Contains(t, order, "strip")
	assert.Less(t, lastIndex(order, "webhook"), firstIndex(order, "strip"),
		"webhook configs must be deleted before finalizers are stripped, got %v", order)

	// The delete-collection targeted the Kueue release label.
	assertWebhookDeleteSelector(t, cli.Actions())

	// The ClusterQueue finalizer was stripped (object either cleared or drained away).
	got, err := dynCli.Resource(clusterQueuesGVR).Get(t.Context(), "poc-cq", meta.GetOptions{})
	if err == nil {
		assert.Empty(t, got.GetFinalizers(), "finalizer must be stripped")
	}

	// The unrelated webhook survives.
	_, err = cli.AdmissionregistrationV1().ValidatingWebhookConfigurations().
		Get(t.Context(), "someone-elses-webhook", meta.GetOptions{})
	assert.NoError(t, err, "non-kueue webhook must not be deleted")
}

// Test_reapOrphanedKueueWith_idempotent asserts a second run over already-reaped state
// still succeeds (webhooks gone, finalizers already cleared).
func Test_reapOrphanedKueueWith_idempotent(t *testing.T) {
	objs := append([]runtime.Object{kueueCRD("clusterqueues", "ClusterQueue", true)}, kueueWebhookConfigs()...)
	cli := kubefake.NewSimpleClientset(objs...)
	dynCli := newDynamicClient(terminatingClusterQueue("poc-cq"))

	_, err := reapOrphanedKueueWith(t.Context(), cli, dynCli)
	require.NoError(t, err)

	acted, err := reapOrphanedKueueWith(t.Context(), cli, dynCli)
	require.NoError(t, err, "re-run must be safe")
	assert.True(t, acted, "the fake keeps the Terminating CRD, so the second run still acts")
}

// Test_reapOrphanedKueueWith_webhookDeleteErrorHalts asserts a real (non-NotFound)
// webhook deletion failure is propagated and the reaper does NOT go on to strip
// finalizers — stripping while the webhook still stands would be rejected anyway.
func Test_reapOrphanedKueueWith_webhookDeleteErrorHalts(t *testing.T) {
	objs := append([]runtime.Object{kueueCRD("clusterqueues", "ClusterQueue", true)}, kueueWebhookConfigs()...)
	cli := kubefake.NewSimpleClientset(objs...)
	dynCli := newDynamicClient(terminatingClusterQueue("poc-cq"))
	cli.PrependReactor("delete-collection", "validatingwebhookconfigurations",
		func(a k8stesting.Action) (bool, runtime.Object, error) {
			return true, nil, apierrors.NewInternalError(errors.New("boom"))
		})

	acted, err := reapOrphanedKueueWith(t.Context(), cli, dynCli)
	assert.True(t, acted, "it acted (found a stuck CRD) before failing")
	require.Error(t, err)
	for _, a := range dynCli.Actions() {
		assert.NotEqual(t, "patch", a.GetVerb(), "must not strip finalizers when webhook deletion failed")
	}
}

// Test_deleteKueueWebhookConfigs_toleratesNotFound asserts an already-absent webhook
// config is not an error (the reaper runs on partially torn-down clusters).
func Test_deleteKueueWebhookConfigs_toleratesNotFound(t *testing.T) {
	cli := kubefake.NewSimpleClientset()
	cli.PrependReactor("delete-collection", "*",
		func(a k8stesting.Action) (bool, runtime.Object, error) {
			return true, nil, apierrors.NewNotFound(schema.GroupResource{Resource: "webhooks"}, "kueue")
		})

	require.NoError(t, deleteKueueWebhookConfigs(t.Context(), cli))
}

// Test_stripKueueCRFinalizers_propagatesPatchError asserts a failed finalizer patch is
// surfaced rather than silently leaving a CR pinned.
func Test_stripKueueCRFinalizers_propagatesPatchError(t *testing.T) {
	dynCli := newDynamicClient(terminatingClusterQueue("poc-cq"))
	dynCli.PrependReactor("patch", "clusterqueues",
		func(a k8stesting.Action) (bool, runtime.Object, error) {
			return true, nil, apierrors.NewInternalError(errors.New("boom"))
		})

	err := stripKueueCRFinalizers(t.Context(), dynCli, kueueCRD("clusterqueues", "ClusterQueue", true))
	require.Error(t, err)
}

// Test_waitKueueCRDsDrained asserts the wait returns once the Terminating CRD is gone.
func Test_waitKueueCRDsDrained(t *testing.T) {
	cli := kubefake.NewSimpleClientset(kueueCRD("clusterqueues", "ClusterQueue", true))

	calls := 0
	cli.PrependReactor("list", "customresourcedefinitions", func(a k8stesting.Action) (bool, runtime.Object, error) {
		calls++
		if calls >= 2 {
			return true, &apiext.CustomResourceDefinitionList{}, nil // drained
		}
		return false, nil, nil // first poll: still Terminating
	})

	err := waitKueueCRDsDrained(t.Context(), cli, 2*time.Second, 5*time.Millisecond)
	require.NoError(t, err)
	assert.GreaterOrEqual(t, calls, 2, "must poll until drained")
}

// Test_storageCRDVersion pins version selection: the storage version wins (it needs no
// conversion), then the first served, then the first declared version, then empty.
func Test_storageCRDVersion(t *testing.T) {
	cases := []struct {
		name     string
		versions []apiext.CustomResourceDefinitionVersion
		want     string
	}{
		{
			name:     "prefers the storage version over a served non-storage one",
			versions: []apiext.CustomResourceDefinitionVersion{{Name: "v1beta1", Served: true, Storage: false}, {Name: "v1beta2", Served: true, Storage: true}},
			want:     "v1beta2",
		},
		{
			name:     "falls back to first served when none is storage",
			versions: []apiext.CustomResourceDefinitionVersion{{Name: "v1beta1", Served: false}, {Name: "v1beta2", Served: true}},
			want:     "v1beta2",
		},
		{
			name:     "falls back to first when none served or stored",
			versions: []apiext.CustomResourceDefinitionVersion{{Name: "v1alpha1", Served: false}},
			want:     "v1alpha1",
		},
		{
			name:     "empty versions yields empty",
			versions: nil,
			want:     "",
		},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			crd := &apiext.CustomResourceDefinition{Spec: apiext.CustomResourceDefinitionSpec{Versions: c.versions}}
			assert.Equal(t, c.want, storageCRDVersion(crd))
		})
	}
}

func assertWebhookDeleteSelector(t *testing.T, actions []k8stesting.Action) {
	t.Helper()
	found := false
	for _, a := range actions {
		// A List action also satisfies DeleteCollectionAction (both expose
		// GetListRestrictions), so gate on the verb first.
		if a.GetVerb() != "delete-collection" {
			continue
		}
		found = true
		dca := a.(k8stesting.DeleteCollectionAction)
		// A parsed selector renders its set values sorted, whatever order they were given in.
		want := slices.Sorted(slices.Values(kueueReleaseNames))
		assert.Equal(t,
			"app.kubernetes.io/instance in ("+strings.Join(want, ",")+")",
			dca.GetListRestrictions().Labels.String(),
			"webhook delete must select only the releases this operator installs kueue under")
	}
	assert.True(t, found, "expected a delete-collection action")
}

func firstIndex(s []string, v string) int {
	for i := range s {
		if s[i] == v {
			return i
		}
	}
	return -1
}

func lastIndex(s []string, v string) int {
	for i := len(s) - 1; i >= 0; i-- {
		if s[i] == v {
			return i
		}
	}
	return -1
}
