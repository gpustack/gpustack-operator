package webserver

import (
	"context"
	"net"

	"sigs.k8s.io/controller-runtime/pkg/manager"
)

type Runner = manager.Runnable

type Config struct {
	Listener net.Listener
	Runners  []Runner
}

func (c *Config) Apply(_ context.Context) (Server, error) {
	return New(c)
}
