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

	return nil
}

func (m *Manager) Start(ctx context.Context) error {
	cm := m.Manager.CtrlManager
	ms := cm.GetWebhookServer()

	// Register the accelerator monitor snapshot readout.
	ms.Register(MonitorSnapshotPath, newMonitorSnapshotHandler(m.Detector.MonitorSnapshot))

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
		err := controllers.Setup(ctx, cm)
		if err != nil {
			return fmt.Errorf("setup controllers: %w", err)
		}
		return m.Manager.Start(ctx)
	})
	return gp.Wait()
}
