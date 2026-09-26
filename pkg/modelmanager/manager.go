package modelmanager

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"sync/atomic"
	"time"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"google.golang.org/grpc"
	core "k8s.io/api/core/v1"
	toolscache "k8s.io/client-go/tools/cache"
	klog "k8s.io/klog/v2"
	ctrlcache "sigs.k8s.io/controller-runtime/pkg/cache"
	ctrlcli "sigs.k8s.io/controller-runtime/pkg/client"
	ctrlmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	"gpustack.ai/gpustack/pkg/manager"
	"gpustack.ai/gpustack/pkg/modelmanager/download"
	"gpustack.ai/gpustack/pkg/modelmanager/driver"
	"gpustack.ai/gpustack/pkg/modelmanager/gc"
	"gpustack.ai/gpustack/pkg/modelmanager/materialize"
	"gpustack.ai/gpustack/pkg/modelmanager/metrics"
	"gpustack.ai/gpustack/pkg/modelmanager/report"
	"gpustack.ai/gpustack/pkg/modelmanager/store"
	"gpustack.ai/gpustack/pkg/modelstore"
	"gpustack.ai/gpustack/pkg/systemname"
	"gpustack.ai/gpustack/pkg/utils/gox"
	"gpustack.ai/gpustack/pkg/utils/version"
)

// Manager is the model-manager plugin on one node: the CSI services, the materializer, the collector
// and the reporter over the node's cache.
type Manager struct {
	Manager *manager.Manager
	Store   *store.Store

	NodeName   string
	KubeletDir string
	CSISocket  string

	ready atomic.Bool
}

// Prepare registers the metrics.
func (m *Manager) Prepare(ctx context.Context) error {
	if err := m.Manager.Prepare(ctx); err != nil {
		return err
	}
	for _, c := range metrics.Collectors() {
		if err := ctrlmetrics.Registry.Register(c); err != nil {
			return fmt.Errorf("register metric: %w", err)
		}
	}

	return nil
}

// Start serves the CSI services and runs the manager. Mount calls answer Unavailable until the
// caches synced and the references were rebuilt from the node's mounts.
func (m *Manager) Start(ctx context.Context) error {
	cm := m.Manager.CtrlManager

	downloader := download.New(nil, 1, 0)
	downloader.Received = func(n int64) { metrics.DownloadBytes.WithLabelValues("hub").Add(float64(n)) }
	materializer := &materialize.Materializer{Store: m.Store, Now: time.Now, Base: ctx}
	collector := &gc.Collector{
		Store:       m.Store,
		Usage:       func() (gc.Usage, error) { return gc.Statfs(m.Store.Root()) },
		Referenced:  m.referenced,
		Downloading: materializer.Downloading,
		Now:         time.Now,
	}
	kubeletCap, err := m.kubeletCap()
	if err != nil {
		return err
	}
	reporter := &report.Reporter{
		Reader: cm.GetCache(), Client: cm.GetClient(), NodeName: m.NodeName, Namespace: systemname.NamespaceName,
		Store: m.Store, Collector: collector, Downloading: materializer.Downloading, Progress: materializer.Progress, Downloader: downloader,
		KubeletCap: kubeletCap, Now: time.Now,
	}
	collector.Watermarks = reporter.Watermarks
	materializer.Environment = reporter.Environment
	materializer.Reserve = collector.Reserve
	materializer.Changed = reporter.Trigger

	cm.GetWebhookServer().Register(modelstore.DownloadsPath, newDownloadsHandler(materializer.Progress))

	d := &driver.Driver{
		Name:         modelstore.DriverName,
		Version:      version.Get(),
		NodeID:       m.NodeName,
		KubeletDir:   m.KubeletDir,
		Store:        m.Store,
		Artifacts:    cm.GetCache(),
		Ready:        m.ready.Load,
		Mounter:      driver.HostMounter{},
		Materializer: materializer,
		Events:       reporter,
	}

	gp := gox.GroupWithContextIn(ctx)
	gp.Go(func(ctx context.Context) error {
		klog.Info("starting controller manager")
		return m.Manager.Start(ctx)
	})
	gp.Go(func(ctx context.Context) error {
		if err := m.Manager.WaitForReady(ctx); err != nil {
			return fmt.Errorf("wait for manager ready: %w", err)
		}
		if err := m.rebuild(); err != nil {
			return fmt.Errorf("rebuild references: %w", err)
		}
		// Every mount reads its artifact from this informer: started and synced here, before the
		// plugin serves, a mount never waits on a first sync.
		if _, err := cm.GetCache().GetInformer(ctx, &workercore.ModelArtifact{}, ctrlcache.BlockUntilSynced(true)); err != nil {
			return fmt.Errorf("watch ModelArtifacts: %w", err)
		}
		// The node's object is created by the worker after this plugin registers, and its spec and
		// the CA bundle change at runtime: each is a reason to apply and report again. The object's
		// own status updates are not: they are this plugin's writes coming back.
		for _, w := range []struct {
			obj     ctrlcli.Object
			changed func(oldObj, newObj any) bool
		}{
			{&workercore.NodeModelStore{}, ownObjectChanged},
			{&core.ConfigMap{}, func(any, any) bool { return true }},
		} {
			inf, err := cm.GetCache().GetInformer(ctx, w.obj)
			if err != nil {
				return fmt.Errorf("watch %T: %w", w.obj, err)
			}
			trigger := func(any) { reporter.Trigger("") }
			if _, err := inf.AddEventHandler(toolscache.ResourceEventHandlerFuncs{
				AddFunc: trigger,
				UpdateFunc: func(o, n any) {
					if w.changed(o, n) {
						trigger(n)
					}
				},
				DeleteFunc: trigger,
			}); err != nil {
				return fmt.Errorf("watch %T: %w", w.obj, err)
			}
		}
		m.ready.Store(true)
		klog.Info("serving mounts")
		return reporter.Run(ctx)
	})
	gp.Go(func(ctx context.Context) error {
		return serveCSI(ctx, m.CSISocket, d)
	})

	return gp.Wait()
}

// rebuild reconstructs the references from the node's mounts and the ledger, and removes what an
// interrupted removal left.
func (m *Manager) rebuild() error {
	if err := m.Store.EmptyTrash(); err != nil {
		return err
	}
	device, err := store.Device(m.Store.Root())
	if err != nil {
		return err
	}
	f, err := os.Open("/proc/self/mountinfo")
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	mounts, err := store.ParseMountInfo(f)
	if err != nil {
		return err
	}
	rebuilt, err := m.Store.Rebuild(mounts, device)
	if err != nil {
		return err
	}
	klog.InfoS("rebuilt references", "kept", len(rebuilt.Kept), "corrected", len(rebuilt.Corrected),
		"adopted", len(rebuilt.Adopted), "dropped", len(rebuilt.Dropped))

	return nil
}

// referenced is the set of digests some target mounts, from the ledger the rebuild keeps true. A
// ledger it cannot read is an error, never an empty set, which would leave every mounted tree
// removable.
func (m *Manager) referenced() (map[string]bool, error) {
	refs, err := m.Store.Refs()
	if err != nil {
		return nil, fmt.Errorf("read the references: %w", err)
	}
	set := make(map[string]bool, len(refs))
	for _, r := range refs {
		set[r.Hex] = true
	}

	return set, nil
}

// kubeletCap returns the high watermark's cap when the cache shares kubelet's filesystem, and nil
// when the cache has a filesystem of its own, where the Setting applies as written. The thresholds
// come from the node's spec; the plugin reads no kubelet file, since the path kubelet reads is its
// --config flag and nothing on the node says what that is.
func (m *Manager) kubeletCap() (func(*workercore.NodeModelStoreKubelet, uint64) *gc.Cap, error) {
	cache, err := store.Device(m.Store.Root())
	if err != nil {
		return nil, err
	}
	kubelet, err := store.Device(m.KubeletDir)
	if err != nil {
		return nil, err
	}
	if cache != kubelet {
		return nil, nil
	}
	return func(k *workercore.NodeModelStoreKubelet, total uint64) *gc.Cap {
		c := gc.KubeletCap(k, total)
		return &c
	}, nil
}

// serveCSI serves the Identity and Node services on socket until ctx is done.
func serveCSI(ctx context.Context, socket string, d *driver.Driver) error {
	if err := os.MkdirAll(filepath.Dir(socket), 0o750); err != nil {
		return err
	}
	if err := os.Remove(socket); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("remove the stale socket: %w", err)
	}
	lis, err := net.Listen("unix", socket)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", socket, err)
	}

	srv := grpc.NewServer()
	csi.RegisterIdentityServer(srv, d)
	csi.RegisterNodeServer(srv, d)
	go func() {
		<-ctx.Done()
		srv.GracefulStop()
	}()
	klog.InfoS("serving CSI", "socket", socket)

	return srv.Serve(lis)
}
