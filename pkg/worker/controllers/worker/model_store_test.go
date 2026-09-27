package worker

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	core "k8s.io/api/core/v1"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	ctrlcli "sigs.k8s.io/controller-runtime/pkg/client"
	ctrlfake "sigs.k8s.io/controller-runtime/pkg/client/fake"

	gpustack "gpustack.ai/gpustack/api/v1"
	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	"gpustack.ai/gpustack/pkg/kubeclients/kubernetes/scheme"
	"gpustack.ai/gpustack/pkg/modelstore"
)

func testModelNodeLabeled(name string, nodeLabels map[string]string) *core.Node {
	return &core.Node{
		ObjectMeta: meta.ObjectMeta{Name: name, UID: types.UID("uid-" + name), Labels: nodeLabels},
	}
}

func testModelStore(name string, selector map[string]string, mutate func(*workercore.ModelStore)) *workercore.ModelStore {
	s := &workercore.ModelStore{
		ObjectMeta: meta.ObjectMeta{Name: name, Generation: 1},
		Spec: workercore.ModelStoreSpec{
			NodeSelector: meta.LabelSelector{MatchLabels: selector},
		},
	}
	if mutate != nil {
		mutate(s)
	}

	return s
}

func newTestModelStoreEnv(t *testing.T, objs ...ctrlcli.Object) (*ModelStoreReconciler, ctrlcli.Client) {
	t.Helper()
	cli := ctrlfake.NewClientBuilder().WithScheme(scheme.Scheme).
		WithStatusSubresource(&workercore.ModelStore{}).
		WithObjects(objs...).Build()

	r := &ModelStoreReconciler{Client: cli, Now: func() time.Time { return time.Unix(1700000000, 0) }}

	return r, cli
}

func reconcileModelStore(t *testing.T, r *ModelStoreReconciler, name string) {
	t.Helper()
	_, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: ctrlcli.ObjectKey{Name: name}})
	require.NoError(t, err)
}

// testCondition finds one condition by type, or nil when it is absent.
func testCondition(conds []gpustack.Condition, typ string) *gpustack.Condition {
	for i := range conds {
		if conds[i].Type == typ {
			return &conds[i]
		}
	}

	return nil
}

func TestSelectModelStoreOrdersByNameAndCountsOverlap(t *testing.T) {
	alpha := *testModelStore("alpha", map[string]string{"pool": "h100"}, nil)
	beta := *testModelStore("beta", map[string]string{"pool": "h100"}, nil)
	gamma := *testModelStore("gamma", map[string]string{"pool": "mi300"}, nil)
	node := testModelNodeLabeled("node-1", map[string]string{"pool": "h100"})

	cases := []struct {
		name      string
		stores    []workercore.ModelStore
		wantNames []string
	}{
		{name: "no store, no match", stores: []workercore.ModelStore{gamma}, wantNames: nil},
		{name: "one match", stores: []workercore.ModelStore{gamma, beta}, wantNames: []string{"beta"}},
		{
			name:      "an overlap orders the winner first",
			stores:    []workercore.ModelStore{beta, gamma, alpha},
			wantNames: []string{"alpha", "beta"},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			matched, err := selectModelStore(c.stores, node)
			require.NoError(t, err)
			var names []string
			for _, s := range matched {
				names = append(names, s.Name)
			}
			assert.Equal(t, c.wantNames, names)
		})
	}
}

func TestNodeModelStoreAppliesTheStoreLayer(t *testing.T) {
	cases := []struct {
		name     string
		objs     []ctrlcli.Object
		wantSpec workercore.NodeModelStoreSpec
	}{
		{
			name: "a store's stated fields override and its silent fields keep the defaults",
			objs: []ctrlcli.Object{
				testModelCSIDriver(), testModelNodeLabeled("node-1", map[string]string{"pool": "h100"}),
				testCSINode("node-1", modelstore.DriverName),
				testModelStore("h100", map[string]string{"pool": "h100"}, func(s *workercore.ModelStore) {
					concurrency := int32(16)
					s.Spec.Watermarks = &workercore.NodeModelStoreWatermarks{HighPercent: 85, LowPercent: 75}
					s.Spec.Download = &workercore.ModelStoreDownload{Concurrency: &concurrency}
				}),
			},
			wantSpec: workercore.NodeModelStoreSpec{
				Watermarks: workercore.NodeModelStoreWatermarks{HighPercent: 85, LowPercent: 75},
				Download:   workercore.NodeModelStoreDownload{Concurrency: 16},
				Hub:        workercore.NodeModelStoreHub{HuggingFaceEndpoint: "https://huggingface.co"},
				Store:      "h100",
			},
		},
		{
			name: "an overlap resolves to the alphabetically first store",
			objs: []ctrlcli.Object{
				testModelCSIDriver(), testModelNodeLabeled("node-1", map[string]string{"pool": "h100"}),
				testCSINode("node-1", modelstore.DriverName),
				testModelStore("zulu", map[string]string{"pool": "h100"}, func(s *workercore.ModelStore) {
					high := 90
					s.Spec.Watermarks = &workercore.NodeModelStoreWatermarks{HighPercent: int32(high), LowPercent: 80}
				}),
				testModelStore("alpha", map[string]string{"pool": "h100"}, func(s *workercore.ModelStore) {
					high := 84
					s.Spec.Watermarks = &workercore.NodeModelStoreWatermarks{HighPercent: int32(high), LowPercent: 74}
				}),
			},
			wantSpec: workercore.NodeModelStoreSpec{
				Watermarks: workercore.NodeModelStoreWatermarks{HighPercent: 84, LowPercent: 74},
				Download:   workercore.NodeModelStoreDownload{Concurrency: 8},
				Hub:        workercore.NodeModelStoreHub{HuggingFaceEndpoint: "https://huggingface.co"},
				Store:      "alpha",
			},
		},
		{
			name: "no matching store leaves the cluster defaults and no name",
			objs: []ctrlcli.Object{
				testModelCSIDriver(), testModelNodeLabeled("node-1", map[string]string{"pool": "mi300"}),
				testCSINode("node-1", modelstore.DriverName),
				testModelStore("h100", map[string]string{"pool": "h100"}, nil),
			},
			wantSpec: defaultNodeModelStoreSpec(),
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r, _ := newTestNodeModelStoreEnv(t, c.objs...)
			reconcileNodeModelStore(t, r, "node-1")

			nms := new(workercore.NodeModelStore)
			require.NoError(t, r.Client.Get(context.Background(), ctrlcli.ObjectKey{Name: "node-1"}, nms))
			assert.Equal(t, c.wantSpec, nms.Spec)
		})
	}
}

func TestNodeModelStoreReconcileKeepsThePinnedField(t *testing.T) {
	pinned := []string{"sha256:aa", "sha256:bb"}
	objs := []ctrlcli.Object{
		testModelCSIDriver(), testModelNode("node-1"), testCSINode("node-1", modelstore.DriverName),
		testSettingsSecret(map[string]string{"model-store-download-concurrency": "4"}),
		&workercore.NodeModelStore{
			ObjectMeta: meta.ObjectMeta{Name: "node-1"},
			Spec: workercore.NodeModelStoreSpec{
				Download: workercore.NodeModelStoreDownload{Concurrency: 8},
				Pinned:   pinned,
			},
		},
	}
	r, _ := newTestNodeModelStoreEnv(t, objs...)
	reconcileNodeModelStore(t, r, "node-1")

	nms := new(workercore.NodeModelStore)
	require.NoError(t, r.Client.Get(context.Background(), ctrlcli.ObjectKey{Name: "node-1"}, nms))
	assert.Equal(t, int32(4), nms.Spec.Download.Concurrency, "the settings change applies")
	assert.Equal(t, pinned, nms.Spec.Pinned, "the configuration write carries the stored pinned list")
}

func TestModelStoreStatusObservesThePool(t *testing.T) {
	labeledNMS := func(name, store string, total, stored int64) *workercore.NodeModelStore {
		nms := &workercore.NodeModelStore{
			ObjectMeta: meta.ObjectMeta{Name: name},
			Spec:       workercore.NodeModelStoreSpec{Store: store},
		}
		if total > 0 {
			nms.Status.Capacity = &workercore.NodeModelStoreCapacity{TotalBytes: total, StoredBytes: stored}
		}

		return nms
	}

	cases := []struct {
		name        string
		store       *workercore.ModelStore
		objs        []ctrlcli.Object
		wantNodes   int32
		wantReady   meta.ConditionStatus
		wantReason  string
		wantOverlap meta.ConditionStatus
		wantCap     *workercore.ModelStoreCapacity
	}{
		{
			name:  "matched nodes with the store applied read ready and sum their capacity",
			store: testModelStore("h100", map[string]string{"pool": "h100"}, nil),
			objs: []ctrlcli.Object{
				testModelNodeLabeled("node-1", map[string]string{"pool": "h100"}),
				testModelNodeLabeled("node-2", map[string]string{"pool": "h100"}),
				testModelNodeLabeled("node-3", map[string]string{"pool": "mi300"}),
				labeledNMS("node-1", "h100", 100, 40),
				labeledNMS("node-2", "h100", 200, 60),
			},
			wantNodes: 2, wantReady: meta.ConditionTrue, wantReason: "Applied",
			wantOverlap: meta.ConditionFalse,
			wantCap:     &workercore.ModelStoreCapacity{TotalBytes: 300, StoredBytes: 100},
		},
		{
			name:  "a matched node with no plugin object keeps the store applying",
			store: testModelStore("h100", map[string]string{"pool": "h100"}, nil),
			objs: []ctrlcli.Object{
				testModelNodeLabeled("node-1", map[string]string{"pool": "h100"}),
				labeledNMS("node-2", "h100", 0, 0),
				testModelNodeLabeled("node-2", map[string]string{"pool": "h100"}),
			},
			wantNodes: 2, wantReady: meta.ConditionFalse, wantReason: "Applying",
			wantOverlap: meta.ConditionFalse, wantCap: nil,
		},
		{
			name:  "an overlap is reported on the store whose nodes are shared",
			store: testModelStore("beta", map[string]string{"pool": "h100"}, nil),
			objs: []ctrlcli.Object{
				testModelNodeLabeled("node-1", map[string]string{"pool": "h100"}),
				testModelStore("alpha", map[string]string{"pool": "h100"}, nil),
			},
			wantNodes: 1, wantReady: meta.ConditionFalse, wantReason: "Overridden",
			wantOverlap: meta.ConditionTrue, wantCap: nil,
		},
		{
			name:      "a selector matching nothing reads vacuously ready",
			store:     testModelStore("h100", map[string]string{"pool": "h100"}, nil),
			objs:      []ctrlcli.Object{testModelNodeLabeled("node-1", map[string]string{"pool": "mi300"})},
			wantNodes: 0,
			wantReady: meta.ConditionTrue, wantReason: "NoNodes",
			wantOverlap: meta.ConditionFalse, wantCap: nil,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r, cli := newTestModelStoreEnv(t, append(c.objs, c.store)...)
			reconcileModelStore(t, r, c.store.Name)

			got := new(workercore.ModelStore)
			require.NoError(t, cli.Get(context.Background(), ctrlcli.ObjectKey{Name: c.store.Name}, got))
			assert.Equal(t, c.wantNodes, got.Status.Nodes)
			assert.Equal(t, c.wantCap, got.Status.Capacity)

			ready := testCondition(got.Status.Conditions, ModelStoreConditionReady)
			require.NotNil(t, ready)
			assert.Equal(t, c.wantReady, ready.Status)
			assert.Equal(t, c.wantReason, ready.Reason)

			overlap := testCondition(got.Status.Conditions, ModelStoreConditionSelectorOverlap)
			require.NotNil(t, overlap)
			assert.Equal(t, c.wantOverlap, overlap.Status)
		})
	}
}

func TestModelStoreStatusWritesNothingWhenNothingChanged(t *testing.T) {
	r, cli := newTestModelStoreEnv(t,
		testModelNodeLabeled("node-1", map[string]string{"pool": "h100"}),
		testModelStore("h100", map[string]string{"pool": "h100"}, nil),
		&workercore.NodeModelStore{
			ObjectMeta: meta.ObjectMeta{Name: "node-1"},
			Spec:       workercore.NodeModelStoreSpec{Store: "h100"},
		},
	)
	reconcileModelStore(t, r, "h100")
	before := new(workercore.ModelStore)
	require.NoError(t, cli.Get(context.Background(), ctrlcli.ObjectKey{Name: "h100"}, before))

	reconcileModelStore(t, r, "h100")
	after := new(workercore.ModelStore)
	require.NoError(t, cli.Get(context.Background(), ctrlcli.ObjectKey{Name: "h100"}, after))

	assert.Equal(t, before.ResourceVersion, after.ResourceVersion,
		"an unchanged pool must not write its status again")
}

func TestModelStoreOverlapIsVisibleOnBothStores(t *testing.T) {
	r, cli := newTestModelStoreEnv(t,
		testModelNodeLabeled("node-1", map[string]string{"pool": "h100"}),
		testModelStore("alpha", map[string]string{"pool": "h100"}, nil),
		testModelStore("beta", map[string]string{"pool": "h100"}, nil),
	)
	reconcileModelStore(t, r, "alpha")
	reconcileModelStore(t, r, "beta")

	for _, name := range []string{"alpha", "beta"} {
		got := new(workercore.ModelStore)
		require.NoError(t, cli.Get(context.Background(), ctrlcli.ObjectKey{Name: name}, got))
		overlap := testCondition(got.Status.Conditions, ModelStoreConditionSelectorOverlap)
		require.NotNil(t, overlap, "%s carries no overlap condition", name)
		assert.Equal(t, meta.ConditionTrue, overlap.Status, "%s must see the overlap", name)
	}
}
