package devicemanager

import (
	"context"
	"fmt"

	"github.com/prometheus/client_golang/prometheus"
	"k8s.io/component-base/metrics/legacyregistry"
	klog "k8s.io/klog/v2"
	ctrlmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"

	"gpustack.ai/gpustack/pkg/devicemanager/allocator"
	"gpustack.ai/gpustack/pkg/devicemanager/controllers"
	"gpustack.ai/gpustack/pkg/devicemanager/detector"
	"gpustack.ai/gpustack/pkg/devicemanager/exporter"
	"gpustack.ai/gpustack/pkg/deviceplugin"
	"gpustack.ai/gpustack/pkg/manager"
	"gpustack.ai/gpustack/pkg/utils/gox"
)

func init() {
	ctrlmetrics.Registry = struct {
		prometheus.Registerer
		prometheus.Gatherer
	}{
		Registerer: legacyregistry.Registerer(),
		Gatherer:   legacyregistry.DefaultGatherer,
	}
}

type Manager struct {
	Manager   *manager.Manager
	Detector  *detector.Detector
	Allocator *allocator.Allocator
	Exporter  *exporter.Poller
}

// Prepare prepares the runtime for the server,
// including installing system resources, etc.
func (m *Manager) Prepare(ctx context.Context) error {
	err := m.Manager.Prepare(ctx)
	if err != nil {
		return err
	}

	// Publish this node's Instance figures on the /metrics the shared manager already serves:
	// no second route, no second port, no Prometheus of our own.
	err = ctrlmetrics.Registry.Register(
		exporter.NewCollector(m.Exporter, m.Detector.MonitorSnapshot),
	)
	if err != nil {
		return fmt.Errorf("register instance metrics collector: %w", err)
	}

	return nil
}

func (m *Manager) Start(ctx context.Context) error {
	cm := m.Manager.CtrlManager
	ms := cm.GetWebhookServer()

	// Register the accelerator monitor snapshot readout.
	ms.Register(MonitorSnapshotPath, newMonitorSnapshotHandler(m.Detector.MonitorSnapshot))

	// The RDMA selection observer and devices reconciler read this process's verbs observation
	// through the detector, which no controller setup can reach, so the sources and the
	// observer-to-reconciler activation link are attached and the controllers are set up before
	// any subsystem runs: the detector's first pass reads OnRDMAInterfacesChanged and can fire it
	// while this wiring would still be running beside it, and a fired Notify only lands once
	// SetupController has armed the observer's event channel.
	obs := controllers.Get[*deviceplugin.RDMASelectionObserver]()
	rec := controllers.Get[*deviceplugin.DevicesReconciler]()
	obs.RDMAInterfaceInventory = m.Detector.RDMAInterfaceSnapshot
	obs.Reconciler = rec
	rec.RDMAInterfaceInventory = m.Detector.RDMAInterfaceSnapshot
	m.Detector.OnRDMAInterfacesChanged = obs.Notify
	err := controllers.Setup(ctx, cm)
	if err != nil {
		return fmt.Errorf("setup controllers: %w", err)
	}

	// Start.
	gp := gox.GroupWithContextIn(ctx)
	gp.Go(func(ctx context.Context) error {
		klog.Info("starting detector")
		return m.Detector.Start(ctx)
	})
	gp.Go(func(ctx context.Context) error {
		// NB(thxCode): Allocator needs manager to be ready before starting,
		// otherwise it may fail to create the necessary resources.
		klog.Info("waiting for manager ready")
		err := m.Manager.WaitForReady(ctx)
		if err != nil {
			return fmt.Errorf("wait for manager ready: %w", err)
		}
		klog.Info("starting allocator")
		return m.Allocator.Start(ctx)
	})
	gp.Go(func(ctx context.Context) error {
		// NB(thxCode): the poller lists Pods through the field index the controllers register,
		// and through the informer cache, so it must not run before either exists.
		klog.Info("waiting for manager ready")
		err := m.Manager.WaitForReady(ctx)
		if err != nil {
			return fmt.Errorf("wait for manager ready: %w", err)
		}
		klog.Info("starting instance metrics exporter")
		return m.Exporter.Start(ctx)
	})
	gp.Go(func(ctx context.Context) error {
		klog.Info("starting controller manager")
		return m.Manager.Start(ctx)
	})
	return gp.Wait()
}
