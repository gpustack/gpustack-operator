package modelmanager

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	ctrlcache "sigs.k8s.io/controller-runtime/pkg/cache"
	ctrlcli "sigs.k8s.io/controller-runtime/pkg/client"
	ctrlfake "sigs.k8s.io/controller-runtime/pkg/client/fake"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	"gpustack.ai/gpustack/pkg/kubeclients/kubernetes/scheme"
	"gpustack.ai/gpustack/pkg/manager"
	"gpustack.ai/gpustack/pkg/modelartifact"
	"gpustack.ai/gpustack/pkg/modelmanager/download"
	"gpustack.ai/gpustack/pkg/modelmanager/gc"
	"gpustack.ai/gpustack/pkg/modelmanager/materialize"
	"gpustack.ai/gpustack/pkg/modelmanager/store"
)

// staleCache serves one pinned copy of the node's object forever, the way a wedged informer does.
type staleCache struct {
	ctrlcache.Cache
	nms *workercore.NodeModelStore
}

func (c staleCache) Get(_ context.Context, _ ctrlcli.ObjectKey, obj ctrlcli.Object, _ ...ctrlcli.GetOption) error {
	c.nms.DeepCopyInto(obj.(*workercore.NodeModelStore))
	return nil
}

// fakeCtrlManager answers the three getters newReporter reads; nothing else is called.
type fakeCtrlManager struct {
	ctrl.Manager
	cache  ctrlcache.Cache
	reader ctrlcli.Reader
	client ctrlcli.Client
}

func (f fakeCtrlManager) GetCache() ctrlcache.Cache    { return f.cache }
func (f fakeCtrlManager) GetAPIReader() ctrlcli.Reader { return f.reader }
func (f fakeCtrlManager) GetClient() ctrlcli.Client    { return f.client }

// TestReporterReadsPastAStalledInformer wires the reporter the way Start does, with the informer
// cache serving the node's previous spec forever — a stalled watch — and the API server holding the
// new one: a report must converge the plugin's environment to the new spec regardless.
func TestReporterReadsPastAStalledInformer(t *testing.T) {
	ctx := context.Background()
	spec := func(endpoint string) workercore.NodeModelStoreSpec {
		return workercore.NodeModelStoreSpec{
			Watermarks: workercore.NodeModelStoreWatermarks{HighPercent: 80, LowPercent: 70},
			Download:   workercore.NodeModelStoreDownload{Concurrency: 8},
			Hub:        workercore.NodeModelStoreHub{HuggingFaceEndpoint: endpoint},
		}
	}

	root := t.TempDir()
	t.Cleanup(func() {
		_ = filepath.WalkDir(root, func(p string, e os.DirEntry, err error) error {
			if err == nil && e.IsDir() {
				_ = os.Chmod(p, 0o755)
			}
			return nil
		})
	})
	st, err := store.Open(root)
	require.NoError(t, err)

	// The API server holds the new endpoint; the cache is frozen at the old one.
	cli := ctrlfake.NewClientBuilder().WithScheme(scheme.Scheme).
		WithStatusSubresource(&workercore.NodeModelStore{}).
		WithObjects(&workercore.NodeModelStore{
			ObjectMeta: meta.ObjectMeta{Name: "node-1"},
			Spec:       spec("https://new.hf.example.com"),
		}).
		Build()
	current := new(workercore.NodeModelStore)
	require.NoError(t, cli.Get(ctx, ctrlcli.ObjectKey{Name: "node-1"}, current))
	stale := current.DeepCopy()
	stale.Spec = spec("https://old.hf.example.com")

	m := &Manager{Store: st, NodeName: "node-1"}
	collector := &gc.Collector{
		Store:       st,
		Usage:       func() (gc.Usage, error) { return gc.Usage{Total: 1000, Used: 420}, nil },
		Referenced:  func() (map[string]bool, error) { return map[string]bool{}, nil },
		Downloading: func() map[string]bool { return nil },
		Now:         time.Now,
	}
	materializer := &materialize.Materializer{Store: st, Now: time.Now, Base: ctx}
	cm := manager.CtrlManager{Manager: fakeCtrlManager{cache: staleCache{nms: stale}, reader: cli, client: cli}}
	reporter := m.newReporter(cm, collector, materializer, download.New(nil, 1, 0), nil)
	collector.Watermarks = reporter.Watermarks
	collector.Pinned = reporter.Pinned

	require.NoError(t, reporter.Report(ctx))
	hub, _, err := reporter.Environment(ctx, materialize.HubHuggingFace)
	require.NoError(t, err)
	require.IsType(t, new(modelartifact.HuggingFace), hub)
	assert.Equal(t, "https://new.hf.example.com", hub.(*modelartifact.HuggingFace).Endpoint,
		"a report converges the applied configuration past a stalled informer")
}
