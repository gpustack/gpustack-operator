package devicemanager

import (
	"context"

	core "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/fields"
	ctrlcache "sigs.k8s.io/controller-runtime/pkg/cache"
	ctrlcli "sigs.k8s.io/controller-runtime/pkg/client"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	"gpustack.ai/gpustack/pkg/devicemanager/allocator"
	"gpustack.ai/gpustack/pkg/devicemanager/detector"
	"gpustack.ai/gpustack/pkg/devicemanager/exporter"
	"gpustack.ai/gpustack/pkg/manager"
	"gpustack.ai/gpustack/pkg/utils/osx"
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
	// The device manager reconciles only its own Devices and Node — and the RDMA selection
	// observer reads the local Node through the cache — so the cache watches exactly those two
	// objects for this node: one informer each instead of N nodes each watching N Nodes and
	// N Devices across the cluster. The policy kind stays unscoped on purpose: it is
	// cluster-wide configuration every node must see whole.
	if nodeName := osx.Getenv("KUBERNETES_NODE_NAME"); nodeName != "" && c.ManagerConfig.CacheByObject == nil {
		c.ManagerConfig.CacheByObject = map[ctrlcli.Object]ctrlcache.ByObject{
			&workercore.Devices{}: {Field: fields.OneTermEqualSelector("metadata.name", nodeName)},
			&core.Node{}:          {Field: fields.OneTermEqualSelector("metadata.name", nodeName)},
		}
	}
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
