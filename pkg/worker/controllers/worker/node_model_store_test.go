package worker

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	core "k8s.io/api/core/v1"
	storage "k8s.io/api/storage/v1"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	ctrlcli "sigs.k8s.io/controller-runtime/pkg/client"
	ctrlfake "sigs.k8s.io/controller-runtime/pkg/client/fake"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	"gpustack.ai/gpustack/pkg/kubeclients/kubernetes/scheme"
	"gpustack.ai/gpustack/pkg/modelstore"
	"gpustack.ai/gpustack/pkg/setting"
)

func testModelNode(name string) *core.Node {
	return &core.Node{ObjectMeta: meta.ObjectMeta{Name: name, UID: types.UID("uid-" + name)}}
}

func testCSINode(name string, drivers ...string) *storage.CSINode {
	cn := &storage.CSINode{ObjectMeta: meta.ObjectMeta{Name: name}}
	for _, d := range drivers {
		cn.Spec.Drivers = append(cn.Spec.Drivers, storage.CSINodeDriver{Name: d, NodeID: name})
	}
	return cn
}

func testModelCSIDriver() *storage.CSIDriver {
	return &storage.CSIDriver{ObjectMeta: meta.ObjectMeta{Name: modelstore.DriverName}}
}

func testSettingsSecret(values map[string]string) *core.Secret {
	sec := &core.Secret{
		ObjectMeta: meta.ObjectMeta{Namespace: setting.DelegatedSecretNamespace, Name: setting.DelegatedSecretName},
		Data:       map[string][]byte{},
	}
	for k, v := range values {
		sec.Data[k] = []byte(v)
	}
	return sec
}

// defaultNodeModelStoreSpec is the spec the Settings' shipped defaults produce.
func defaultNodeModelStoreSpec() workercore.NodeModelStoreSpec {
	return workercore.NodeModelStoreSpec{
		Watermarks: workercore.NodeModelStoreWatermarks{HighPercent: 80, LowPercent: 70},
		Download:   workercore.NodeModelStoreDownload{Concurrency: 8},
		Hub:        workercore.NodeModelStoreHub{HuggingFaceEndpoint: "https://huggingface.co"},
	}
}

func newTestNodeModelStoreEnv(t *testing.T, objs ...ctrlcli.Object) (*NodeModelStoreReconciler, ctrlcli.Client) {
	t.Helper()
	cli := ctrlfake.NewClientBuilder().WithScheme(scheme.Scheme).
		WithStatusSubresource(&workercore.NodeModelStore{}).
		WithObjects(objs...).Build()

	// No kubelet answers in a unit test: the node keeps kubelet's defaults unless a case sets a reader.
	return &NodeModelStoreReconciler{Client: cli, ReadKubelet: func(context.Context, string) (*workercore.NodeModelStoreKubelet, error) {
		return nil, errors.New("no kubelet in a unit test")
	}}, cli
}

func reconcileNodeModelStore(t *testing.T, r *NodeModelStoreReconciler, node string) ctrl.Result {
	t.Helper()
	res, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: ctrlcli.ObjectKey{Name: node}})
	require.NoError(t, err)
	return res
}

func TestNodeModelStoreReconcile(t *testing.T) {
	existing := func(spec workercore.NodeModelStoreSpec) *workercore.NodeModelStore {
		return &workercore.NodeModelStore{ObjectMeta: meta.ObjectMeta{Name: "node-1"}, Spec: spec}
	}
	edited := defaultNodeModelStoreSpec()
	edited.Download.Concurrency = 64

	cases := []struct {
		name       string
		objs       []ctrlcli.Object
		wantExists bool
		wantSpec   workercore.NodeModelStoreSpec
	}{
		{
			name:       "a node whose CSINode lists the driver gets its object with the defaults",
			objs:       []ctrlcli.Object{testModelCSIDriver(), testModelNode("node-1"), testCSINode("node-1", modelstore.DriverName)},
			wantExists: true,
			wantSpec:   defaultNodeModelStoreSpec(),
		},
		{
			name: "the Settings store's values are the spec",
			objs: []ctrlcli.Object{
				testModelCSIDriver(), testModelNode("node-1"), testCSINode("node-1", modelstore.DriverName),
				testSettingsSecret(map[string]string{
					"model-store-high-watermark":          "85",
					"model-store-download-bandwidth":      "100Mi",
					"model-artifact-https-proxy":          "http://proxy:3128",
					"model-artifact-huggingface-endpoint": "http://hub.local",
				}),
			},
			wantExists: true,
			wantSpec: workercore.NodeModelStoreSpec{
				Watermarks: workercore.NodeModelStoreWatermarks{HighPercent: 85, LowPercent: 70},
				Download:   workercore.NodeModelStoreDownload{Concurrency: 8, BytesPerSecond: 100 << 20},
				Hub: workercore.NodeModelStoreHub{
					HuggingFaceEndpoint: "http://hub.local", HTTPSProxy: "http://proxy:3128",
				},
			},
		},
		{
			name: "a node whose CSINode lists another driver gets nothing",
			objs: []ctrlcli.Object{testModelCSIDriver(), testModelNode("node-1"), testCSINode("node-1", "nfs.csi.k8s.io")},
		},
		{
			name: "a node without a CSINode gets nothing",
			objs: []ctrlcli.Object{testModelCSIDriver(), testModelNode("node-1")},
		},
		{
			name: "an object whose driver left CSINode for a moment is kept, and realigned",
			objs: []ctrlcli.Object{
				testModelCSIDriver(), testModelNode("node-1"), testCSINode("node-1"), existing(edited),
			},
			wantExists: true,
			wantSpec:   defaultNodeModelStoreSpec(),
		},
		{
			name:       "a hand edit of the spec is overwritten",
			objs:       []ctrlcli.Object{testModelCSIDriver(), testModelNode("node-1"), testCSINode("node-1", modelstore.DriverName), existing(edited)},
			wantExists: true,
			wantSpec:   defaultNodeModelStoreSpec(),
		},
		{
			name: "without the CSIDriver the object is deleted",
			objs: []ctrlcli.Object{testModelNode("node-1"), testCSINode("node-1", modelstore.DriverName), existing(edited)},
		},
		{
			name: "a node that is gone gets nothing",
			objs: []ctrlcli.Object{testModelCSIDriver(), testCSINode("node-1", modelstore.DriverName)},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r, cli := newTestNodeModelStoreEnv(t, c.objs...)
			reconcileNodeModelStore(t, r, "node-1")

			got := new(workercore.NodeModelStore)
			err := cli.Get(context.Background(), ctrlcli.ObjectKey{Name: "node-1"}, got)
			if !c.wantExists {
				assert.True(t, kerrors.IsNotFound(err), "want no object, got %v", err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, c.wantSpec, got.Spec)
			owner := meta.GetControllerOf(got)
			require.NotNil(t, owner, "the Node owns it, so it goes with the Node")
			assert.Equal(t, "Node", owner.Kind)
			assert.Equal(t, "node-1", owner.Name)
		})
	}
}

func TestNodeModelStoreReconcileFollowsTheSettings(t *testing.T) {
	sec := testSettingsSecret(map[string]string{"model-store-download-concurrency": "4"})
	r, cli := newTestNodeModelStoreEnv(t,
		testModelCSIDriver(), testModelNode("node-1"), testCSINode("node-1", modelstore.DriverName), sec)
	reconcileNodeModelStore(t, r, "node-1")

	got := new(workercore.NodeModelStore)
	require.NoError(t, cli.Get(context.Background(), ctrlcli.ObjectKey{Name: "node-1"}, got))
	require.Equal(t, int32(4), got.Spec.Download.Concurrency)

	// A change of the store is read on its own event, not after the Settings package's read cache.
	sec.Data["model-store-download-concurrency"] = []byte("12")
	require.NoError(t, cli.Update(context.Background(), sec))
	reconcileNodeModelStore(t, r, "node-1")
	require.NoError(t, cli.Get(context.Background(), ctrlcli.ObjectKey{Name: "node-1"}, got))
	assert.Equal(t, int32(12), got.Spec.Download.Concurrency)

	// A pass with nothing changed writes nothing.
	rv := got.ResourceVersion
	reconcileNodeModelStore(t, r, "node-1")
	require.NoError(t, cli.Get(context.Background(), ctrlcli.ObjectKey{Name: "node-1"}, got))
	assert.Equal(t, rv, got.ResourceVersion)
}

func TestNodeModelStoreReconcileNeverWritesAnInvalidSpec(t *testing.T) {
	sec := testSettingsSecret(map[string]string{"model-store-download-concurrency": "4"})
	r, cli := newTestNodeModelStoreEnv(t,
		testModelCSIDriver(), testModelNode("node-1"), testCSINode("node-1", modelstore.DriverName), sec)
	reconcileNodeModelStore(t, r, "node-1")

	// Two racing writes can leave the low watermark above the high one; admission judged each alone.
	sec.Data["model-store-low-watermark"] = []byte("90")
	require.NoError(t, cli.Update(context.Background(), sec))
	res := reconcileNodeModelStore(t, r, "node-1")
	assert.Equal(t, nodeModelStoreInvalidRetry, res.RequeueAfter)

	got := new(workercore.NodeModelStore)
	require.NoError(t, cli.Get(context.Background(), ctrlcli.ObjectKey{Name: "node-1"}, got))
	assert.Equal(t, int32(70), got.Spec.Watermarks.LowPercent, "the last valid spec stays")
	assert.Equal(t, int32(4), got.Spec.Download.Concurrency)
}

func TestNodeModelStoreReconcileWritesNoStatus(t *testing.T) {
	r, cli := newTestNodeModelStoreEnv(t,
		testModelCSIDriver(), testModelNode("node-1"), testCSINode("node-1", modelstore.DriverName),
		&workercore.NodeModelStore{
			ObjectMeta: meta.ObjectMeta{Name: "node-1"},
			Spec:       defaultNodeModelStoreSpec(),
			Status:     workercore.NodeModelStoreStatus{ObservedGeneration: 7},
		})
	reconcileNodeModelStore(t, r, "node-1")

	got := new(workercore.NodeModelStore)
	require.NoError(t, cli.Get(context.Background(), ctrlcli.ObjectKey{Name: "node-1"}, got))
	assert.Equal(t, int64(7), got.Status.ObservedGeneration)
}

func TestNodeModelStoreEnqueuesEveryNode(t *testing.T) {
	r, _ := newTestNodeModelStoreEnv(t,
		testCSINode("node-1", modelstore.DriverName),
		testCSINode("node-2", "nfs.csi.k8s.io"),
		testCSINode("node-3", modelstore.DriverName),
		&workercore.NodeModelStore{ObjectMeta: meta.ObjectMeta{Name: "node-2"}},
		&workercore.NodeModelStore{ObjectMeta: meta.ObjectMeta{Name: "node-4"}},
	)

	reqs := r.enqueueEveryNode(context.Background(), nil)
	got := make([]string, 0, len(reqs))
	for _, req := range reqs {
		got = append(got, req.Name)
	}
	assert.ElementsMatch(t, []string{"node-1", "node-2", "node-3", "node-4"}, got)
}
