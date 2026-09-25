package modelmanager

import (
	"context"
	"fmt"

	core "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/fields"
	ctrlcache "sigs.k8s.io/controller-runtime/pkg/cache"
	ctrlcli "sigs.k8s.io/controller-runtime/pkg/client"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	"gpustack.ai/gpustack/pkg/manager"
	"gpustack.ai/gpustack/pkg/modelmanager/store"
	"gpustack.ai/gpustack/pkg/systemname"
	"gpustack.ai/gpustack/pkg/webserver"
)

// Config is the completed configuration of the model-manager plugin on one node.
type Config struct {
	ServerConfig  *webserver.Config
	ManagerConfig *manager.Config

	NodeName   string
	KubeletDir string
	CacheRoot  string
	CSISocket  string
}

// Apply builds the plugin: its server, a controller manager whose cache watches only what the
// plugin reads, and the store over the cache root.
func (c *Config) Apply(ctx context.Context) (*Manager, error) {
	srv, err := c.ServerConfig.Apply(ctx)
	if err != nil {
		return nil, err
	}
	c.ManagerConfig.WebhookServer = srv
	// The plugin's role reads its own NodeModelStore and ConfigMaps in the operator's namespace
	// only, so the cache watches exactly those; ModelArtifacts are read cluster-wide.
	c.ManagerConfig.CacheByObject = map[ctrlcli.Object]ctrlcache.ByObject{
		&workercore.NodeModelStore{}: {Field: fields.OneTermEqualSelector("metadata.name", c.NodeName)},
		&core.ConfigMap{}:            {Namespaces: map[string]ctrlcache.Config{systemname.NamespaceName: {}}},
	}
	mgr, err := c.ManagerConfig.Apply(ctx)
	if err != nil {
		return nil, err
	}

	st, err := store.Open(c.CacheRoot)
	if err != nil {
		return nil, fmt.Errorf("open the cache root %s: %w", c.CacheRoot, err)
	}

	return &Manager{
		Manager:    mgr,
		Store:      st,
		NodeName:   c.NodeName,
		KubeletDir: c.KubeletDir,
		CSISocket:  c.CSISocket,
	}, nil
}
