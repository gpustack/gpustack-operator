package devicemanager

import (
	"context"

	"gpustack.ai/gpustack/pkg/devicemanager/allocator"
	"gpustack.ai/gpustack/pkg/devicemanager/detector"
	"gpustack.ai/gpustack/pkg/devicemanager/exporter"
	"gpustack.ai/gpustack/pkg/manager"
	"gpustack.ai/gpustack/pkg/webserver"
)

type Config struct {
	ServerConfig    *webserver.Config
	ManagerConfig   *manager.Config
	DetectorConfig  *detector.Config
	AllocatorConfig *allocator.Config
	ExporterConfig  *exporter.Config
}

func (c *Config) Apply(ctx context.Context) (*Manager, error) {
	srv, err := c.ServerConfig.Apply(ctx)
	if err != nil {
		return nil, err
	}
	c.ManagerConfig.WebhookServer = srv
	mgr, err := c.ManagerConfig.Apply(ctx)
	if err != nil {
		return nil, err
	}
	det, err := c.DetectorConfig.Apply(ctx)
	if err != nil {
		return nil, err
	}
	alc, err := c.AllocatorConfig.Apply(ctx)
	if err != nil {
		return nil, err
	}

	c.ExporterConfig.MonitorPeriod = c.DetectorConfig.MonitorPeriod
	exp, err := c.ExporterConfig.Apply(ctx)
	if err != nil {
		return nil, err
	}

	return &Manager{
		Manager:   mgr,
		Detector:  det,
		Allocator: alc,
		Exporter:  exp,
	}, nil
}
