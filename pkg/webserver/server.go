package webserver

import (
	"context"
	"errors"
	"net"
	"net/http"
	"strconv"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	ctrlwebhook "sigs.k8s.io/controller-runtime/pkg/webhook"

	"gpustack.ai/gpustack/pkg/utils/gox"
	"gpustack.ai/gpustack/pkg/utils/netx"
)

type Server interface {
	ctrlwebhook.Server

	HostPort() (string, int, error)
}

// New creates a Server with the given configuration.
func New(c *Config) (Server, error) {
	if c.Listener == nil {
		return nil, errors.New("listener is required")
	}
	tcpAddr, ok := c.Listener.Addr().(*net.TCPAddr)
	if !ok {
		return nil, errors.New("listener must be a TCP listener")
	}

	return server{
		listener: c.Listener,
		host:     tcpAddr.IP.String(),
		port:     tcpAddr.Port,
		runners:  c.Runners,
		mux:      http.NewServeMux(),
	}, nil
}

type server struct {
	listener net.Listener
	host     string
	port     int
	runners  []manager.Runnable
	mux      *http.ServeMux
}

func (server) NeedLeaderElection() bool {
	return false
}

func (s server) Register(path string, hook http.Handler) {
	s.mux.Handle(path, hook)
}

func (s server) Start(ctx context.Context) error {
	srv := newHttpServer(s.mux)

	gp := gox.GroupWithContextIn(ctx)
	for i := range s.runners {
		r := s.runners[i]
		gp.Go(func(ctx context.Context) error {
			return r.Start(ctx)
		})
	}
	gp.Go(func(ctx context.Context) error {
		<-ctx.Done()
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
		return nil
	})
	gp.Go(func(_ context.Context) error {
		err := srv.Serve(s.listener)
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			return err
		}
		return nil
	})
	return gp.Wait()
}

func (s server) StartedChecker() healthz.Checker {
	addr := net.JoinHostPort(s.host, strconv.Itoa(s.port))
	return func(req *http.Request) error {
		return netx.IsConnected(req.Context(), "tls", addr, 10*time.Second)
	}
}

func (s server) WebhookMux() *http.ServeMux {
	return s.mux
}

func (s server) HostPort() (string, int, error) {
	return s.host, s.port, nil
}
