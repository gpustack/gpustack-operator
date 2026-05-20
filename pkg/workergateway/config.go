package workergateway

import (
	"context"

	"gpustack.ai/gpustack/pkg/webserver"
	"gpustack.ai/gpustack/pkg/workergateway/manager"
)

type Config struct {
	// Manager.
	ManagerConfig *manager.Config

	// Server.
	ServerConfig *webserver.Config
}

func (c *Config) Apply(ctx context.Context) (*WorkerGateway, error) {
	mgr, err := c.ManagerConfig.Apply(ctx)
	if err != nil {
		return nil, err
	}

	srv, err := c.ServerConfig.Apply(ctx)
	if err != nil {
		return nil, err
	}

	return &WorkerGateway{
		Manager: mgr,
		Server:  srv,
	}, nil
}
