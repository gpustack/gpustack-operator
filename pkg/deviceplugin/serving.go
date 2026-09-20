package deviceplugin

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sync"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	core "k8s.io/api/core/v1"
	klog "k8s.io/klog/v2"
	deviceplugin "k8s.io/kubelet/pkg/apis/deviceplugin/v1beta1"

	"gpustack.ai/gpustack/pkg/utils/gox"
	"gpustack.ai/gpustack/pkg/utils/osx"
)

// servingOptions names what one serving loop serves: the plugin answering the gRPC calls, the
// resource name it registers with kubelet, and the base name of the socket it owns beside kubelet's
// own.
type servingOptions struct {
	Plugin       deviceplugin.DevicePluginServer
	ResourceName core.ResourceName
	SocketName   string
}

// serving owns the generation lifecycle of one device-plugin server: a socket beside kubelet's, a
// gRPC server on it, and the registration naming the two to kubelet. Its zero value is a server
// that was never started. It is embedded in ResourceServer and reusable by any server that can
// name its socket, its resource, and its plugin.
type serving struct {
	// mu guards the serving state below. Start swaps it as it hands over from one generation of
	// service to the next, while Stop and the registration readout reach it from other goroutines.
	mu   sync.Mutex
	stop context.CancelFunc
	// done is closed once the serving loop has returned, which is what Stop waits on: a Stop that
	// returned while a generation was still being set up would leave that generation free to take
	// a socket path the next allocator has already claimed.
	done       chan struct{}
	server     *grpc.Server
	registered bool
}

// pluginSocketPeriod is how often a serving generation re-checks the two things that decide whether
// it is still doing its job: that its own socket is still on disk, and that kubelet has accepted
// its registration. It doubles as the retry interval for the registration.
const pluginSocketPeriod = time.Second

// registerTimeout bounds one Register call. kubelet dials the plugin's endpoint back from inside its
// own handler, blocking, under a 10 s timeout of its own, so a legitimate call can take that long.
// Past it kubelet is not answering, and an unbounded call would hold the serving loop — and with it
// the socket probe that ends the generation — for as long as kubelet stayed wedged.
const registerTimeout = 15 * time.Second

// isRegistered reports whether kubelet has accepted the registration of the generation currently
// serving. It is false while nothing is serving, and false again from the moment a generation is
// retired until its successor's registration is accepted.
func (s *serving) isRegistered() bool {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.registered
}

// Start serves the plugin, socket and resource opts names to kubelet for as long as ctx allows,
// re-establishing the service whenever kubelet takes it away.
//
// One generation of service is a socket beside kubelet's, a gRPC server on it, and a registration
// naming the two to kubelet. A generation does not last the lifetime of the process: kubelet
// unlinks every socket in the plugin directory each time its own registration server starts, which
// retires the generation without this process being touched. So Start loops, serving a new
// generation as soon as it observes the current one over — its socket gone, or its server no longer
// serving on a socket still there. Registration is not optional and not one-shot — a generation
// kubelet does not know about leaves the node advertising the resource with nothing allocatable.
//
// It blocks until ctx is canceled or Stop is called, and reports an error only when a generation
// could not be established at all. Calling it on a serving that is already started starts nothing
// and simply blocks until ctx is done, so a supervisor that retries cannot end up with two servers
// on one socket.
func (s *serving) Start(ctx context.Context, kubeSocket string, opts servingOptions, logger klog.Logger) error {
	caller := ctx
	ctx, stop := context.WithCancel(caller)
	defer stop()

	if !s.begin(stop) {
		logger.Error(nil, "server already started")
		<-ctx.Done()
		return ctx.Err()
	}
	defer s.end()

	socket := filepath.Join(filepath.Dir(kubeSocket), opts.SocketName)

	for {
		if err := s.serve(ctx, kubeSocket, socket, opts, logger); err != nil {
			return err
		}
		if ctx.Err() != nil {
			// Only the caller giving up is an outcome to report: being stopped on request is what
			// Stop is for, and a supervisor that treats it as a failure would take the rest of the
			// allocator down with a server it asked to retire.
			return caller.Err()
		}
		logger.Info("generation is over, serving again", "socket", socket)
	}
}

// serve runs one generation of service: it listens on its own socket, serves the device-plugin gRPC
// API there, and registers the resource with kubelet, retrying the registration for as long as the
// generation lives — right after kubelet wiped the plugin directory its registration server may not
// be listening yet, and giving up would leave the resource unknown to the node.
//
// It returns nil once the generation is over — its socket gone from disk, or its server no longer
// serving, either of which is the caller's signal to serve a new one, or ctx done — and an error only
// when the generation could not be established at all. Removing the socket belongs to the start of a
// generation and not the end of one: a retiring generation that unlinked the path would unlink
// whatever holds it by then, which once a replacement allocator has taken over is the replacement's
// socket. What a generation leaves behind is a path with nothing listening, and both the next
// generation and kubelet's own startup wipe remove it.
func (s *serving) serve(ctx context.Context, kubeSocket, socket string, opts servingOptions, logger klog.Logger) error {
	if err := os.Remove(socket); err != nil && !os.IsNotExist(err) {
		logger.Error(err, "clean up socket", "socket", socket)
		return err
	}

	lis, err := net.Listen("unix", socket)
	if err != nil {
		logger.Error(err, "listen on socket", "socket", socket)
		return err
	}
	// Unlinking the path is never the listener's job. A generation retired before its serving
	// goroutine was scheduled leaves that goroutine to close the listener itself, and a unix listener
	// unlinks its path on close — by then the successor generation's socket, whose disappearance
	// kubelet would notice and this process would not.
	if ul, ok := lis.(*net.UnixListener); ok {
		ul.SetUnlinkOnClose(false)
	}

	srv := grpc.NewServer(
		grpc.Creds(insecure.NewCredentials()),
	)
	deviceplugin.RegisterDevicePluginServer(srv, opts.Plugin)
	s.publish(srv)
	defer s.retire(srv, logger)

	// Serving is not waited on here: grpc.Server.Serve returns only once its listener is closed,
	// which is what retiring the generation above does, so waiting on it would wait on this
	// function's own teardown. Its end is still watched, because a server that stops serving on its
	// own leaves the socket path in place, and the path is all the check below can see.
	served := make(chan struct{})
	gox.Go(func() {
		defer close(served)

		logger.Info("serving on socket", "socket", socket)
		// A generation retired before this goroutine got to run reports its server as already
		// stopped, which is the teardown working, not a failure to serve.
		if err := srv.Serve(lis); err != nil && !errors.Is(err, grpc.ErrServerStopped) {
			logger.Error(err, "serve on socket", "socket", socket)
		}
	})

	// Both halves of keeping the generation alive are level-based, so one ticker drives them: hand
	// the registration to kubelet until it takes it, and retire once the socket is gone. The socket
	// is checked after the tick rather than before it, so every generation lives at least one
	// period and a path that cannot be read cannot spin the caller's loop.
	tick := time.NewTicker(pluginSocketPeriod)
	defer tick.Stop()
	for {
		// Asked before the registration below, not only in the select: handing kubelet an endpoint
		// this generation is already tearing down would be edge-triggered work in a level-based
		// loop, and it would report a failure that is really a shutdown.
		if ctx.Err() != nil {
			return nil
		}
		if !s.isRegistered() {
			logger.Info("registering to kubelet")
			if err := s.register(ctx, srv, kubeSocket, socket, opts); err != nil {
				logger.Error(err, "registering to kubelet, retrying", "socket", kubeSocket)
			}
		}
		select {
		case <-ctx.Done():
			return nil
		case <-served:
			// Reported rather than returned: a generation that stopped serving after it was
			// established is the caller's to replace, not its cue to give up on the resource. The
			// report is worth making because nothing else here can make it — the socket check below
			// still finds the path, so this generation would otherwise look alive forever.
			if ctx.Err() == nil {
				logger.Error(nil, "stopped serving with its socket still in place", "socket", socket)
			}

			return nil
		case <-tick.C:
		}
		if _, err := os.Stat(socket); err != nil {
			logger.Info("socket is gone", "socket", socket)
			return nil
		}
	}
}

// begin marks the serving as started and records how to stop it, reporting false when it is
// already running — in which case the caller must not serve.
func (s *serving) begin(stop context.CancelFunc) bool {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.stop != nil {
		return false
	}
	s.stop, s.done = stop, make(chan struct{})

	return true
}

// end clears the running state and releases whatever Stop is waiting on it, so a later Start may
// serve again.
func (s *serving) end() {
	s.mu.Lock()
	done := s.done
	s.stop, s.done = nil, nil
	s.mu.Unlock()

	close(done)
}

// publish hands the generation's gRPC server over to Stop, and clears the registration the new
// generation has yet to make.
func (s *serving) publish(srv *grpc.Server) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.server, s.registered = srv, false
}

// retire stops srv, which closes its listener with it, and drops the registration that named it to
// kubelet. It is a no-op unless srv is still the generation being served — so a Stop slow enough to
// resume after a later Start has published its own generation retires nothing, rather than taking
// down a server it never meant to.
//
// The server is stopped after the lock is released, never under it: grpc's Stop waits for Serve to
// return, and holding a mutex that anything on the serving path might read across that wait is how
// a shutdown deadlocks.
func (s *serving) retire(srv *grpc.Server, logger klog.Logger) {
	s.mu.Lock()
	if srv == nil || s.server != srv {
		s.mu.Unlock()

		return
	}
	s.server, s.registered = nil, false
	s.mu.Unlock()

	logger.Info("stopping")
	srv.Stop()
}

// Stop stops the gRPC server and ends the serving loop, and does not return until Start has: past
// it, no generation is left to touch the socket. It is a no-op, logged, on a serving that was
// never started.
//
// A later Start may serve again once this one has returned — the single-Start guard is released by
// Start on its way out, so a Start racing its own Stop is refused rather than allowed to put a second
// server on one socket.
func (s *serving) Stop(logger klog.Logger) {
	// The generation is read together with the cancel function, so what gets retired below is the
	// generation this Stop was asked about and not whatever is being served by the time it gets there.
	s.mu.Lock()
	stop, srv, done := s.stop, s.server, s.done
	s.mu.Unlock()

	if stop == nil {
		logger.Errorf(nil, "server not started")
		return
	}

	stop()
	s.retire(srv, logger)
	// Waited on rather than left to finish on its own: a generation caught between being started and
	// being published owns no server for the retire above to reach, yet it will still listen on — and
	// first remove — the socket. Returning before it is done would let it do that to the path a
	// replacement allocator has already taken over.
	<-done
}

// register hands kubelet the endpoint of srv, the generation being served, and records the acceptance
// against it. srv identifies the generation so that a registration accepted just as its generation
// retires is not recorded against its successor.
//
// Failures are reported and not logged: this runs once per period for as long as kubelet stays away, so
// a line here and another at the caller would double a per-second error stream, and it is the caller
// that knows the attempt will be retried. Each error names the step instead.
func (s *serving) register(ctx context.Context, srv *grpc.Server, kubeSocket, socket string, opts servingOptions) error {
	regOpts, err := opts.Plugin.GetDevicePluginOptions(ctx, &Empty{})
	if err != nil {
		return fmt.Errorf("getting device plugin options: %w", err)
	}

	cli, err := grpc.NewClient("unix://"+kubeSocket,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		return fmt.Errorf("dialing kubelet socket: %w", err)
	}
	defer osx.Close(cli)

	regCli := deviceplugin.NewRegistrationClient(cli)
	regReq := &deviceplugin.RegisterRequest{
		Version:      deviceplugin.Version,
		Endpoint:     filepath.Base(socket),
		ResourceName: string(opts.ResourceName),
		Options:      regOpts,
	}
	regCtx, cancel := context.WithTimeout(ctx, registerTimeout)
	defer cancel()
	if _, err = regCli.Register(regCtx, regReq); err != nil {
		return fmt.Errorf("registering %s: %w", opts.ResourceName, err)
	}

	// Recorded against the generation that was registered, never whatever is current by now: a retire
	// landing between the call returning and this lock would otherwise have its cleared flag put back,
	// leaving a stopped server reporting itself registered to kubelet.
	s.mu.Lock()
	if s.server == srv {
		s.registered = true
	}
	s.mu.Unlock()

	return nil
}
