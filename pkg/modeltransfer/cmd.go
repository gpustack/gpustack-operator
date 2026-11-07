package modeltransfer

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"time"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
	klog "k8s.io/klog/v2"

	"gpustack.ai/gpustack/pkg/modeltransfer/broker"
)

// NewCommand returns the cobra command of the model-transfer source runtime: the
// node's broker and its worker control service, in a process of their own.
func NewCommand() *cobra.Command {
	o := NewOptions()

	c := &cobra.Command{
		Use:     "model-transfer",
		Aliases: []string{"mx"},
		Short:   "serves authorized source generations for remote weight loading.",
		PreRunE: func(c *cobra.Command, args []string) error {
			return o.Validate()
		},
		RunE: func(c *cobra.Command, args []string) error {
			ctx := c.Context()

			cfg, err := o.Complete()
			if err != nil {
				return fmt.Errorf("complete config: %w", err)
			}
			defer func() {
				if err := cfg.Close(); err != nil {
					klog.ErrorS(err, "close model-transfer runtime")
				}
			}()
			if err := cfg.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
				return err
			}
			return nil
		},
	}

	o.AddFlags(c.Flags())

	return c
}

// AddFlags registers the options on fs.
func (o *Options) AddFlags(fs *pflag.FlagSet) {
	fs.DurationVar(&o.KubeConnTimeout, "kube-conn-timeout", o.KubeConnTimeout, "the Kubernetes request timeout.")
	fs.Float64Var(&o.KubeConnQPS, "kube-conn-qps", o.KubeConnQPS, "the Kubernetes client queries per second.")
	fs.IntVar(&o.KubeConnBurst, "kube-conn-burst", o.KubeConnBurst, "the Kubernetes client burst limit.")
	fs.StringVar(&o.KubeContentType, "kube-content-type", o.KubeContentType, "the Kubernetes request content type.")
	fs.StringVar(&o.NodeName, "node-name", o.NodeName,
		"the name of the node this broker serves.")
	fs.StringVar(&o.CacheRoot, "cache-root", o.CacheRoot,
		"the model cache root whose published trees sources fill from.")
	fs.StringVar(&o.BrokerRoot, "broker-root", o.BrokerRoot,
		"the broker's durable ledger root.")
	fs.StringVar(&o.ControlAddr, "control-addr", o.ControlAddr,
		"where the worker control service listens, on this node.")
	fs.StringVar(&o.WorkerNamespace, "worker-namespace", o.WorkerNamespace,
		"the namespace the admitted source-worker Pods run in; empty lists across all namespaces.")
	fs.Int64Var(&o.PhysicalBytes, "physical-bytes", o.PhysicalBytes,
		"the node's registered-memory ceiling for source generations; zero derives one sixteenth of the node's total RAM.")
	fs.Int64Var(&o.PhysicalBytes, "max-memory-bytes", o.PhysicalBytes,
		"alias of --physical-bytes: the source transfer buffer ceiling in bytes.")
	fs.Int64Var(&o.MinMemoryBytes, "min-memory-bytes", o.MinMemoryBytes,
		"the floor under the source buffer ceiling; a value below 1 GiB resolves up to 1 GiB.")
	fs.Int32Var(&o.MaxSessions, "max-sessions", o.MaxSessions,
		"the node's concurrent source-session cap; a value below one resolves to 64.")
	fs.IntVar(&o.MaxGenerations, "max-generations", o.MaxGenerations,
		"how many live source generations this node holds at once.")
	fs.IntVar(&o.MaxQuarantined, "max-quarantined", o.MaxQuarantined,
		"how many quarantined generations this node retains at most.")
	fs.Int64Var(&o.QuarantineBytes, "quarantine-bytes", o.QuarantineBytes,
		"the reserved bytes quarantined generations may retain at most.")
	fs.DurationVar(&o.LeaseTTL, "lease-ttl", o.LeaseTTL,
		"how long a source worker's health lease stands without a heartbeat.")
	fs.StringVar(&o.SessionAddr, "session-addr", o.SessionAddr,
		"where the engine session listener serves, over TLS.")
	fs.StringVar(&o.SessionTLSCert, "session-tls-cert", o.SessionTLSCert,
		"the required session serving certificate.")
	fs.StringVar(&o.SessionTLSKey, "session-tls-key", o.SessionTLSKey,
		"the session serving certificate key.")
	fs.StringVar(&o.BootstrapFile, "bootstrap-file", o.BootstrapFile,
		"the Pod-local file the worker's bootstrap credential is written to.")
	fs.StringVar(&o.WorkerID, "worker-id", o.WorkerID,
		"this node's transport worker identity in admissions.")
	fs.StringVar(&o.VendorScope, "vendor-scope", o.VendorScope,
		"this broker's vendor identity in generation tokens and cache refs.")
}

// Run reconstructs the broker's state, then serves the worker control
// service and sweeps leases until the context ends. Reconstruction keeps
// recorded fills when the worker set cannot be read: releasing them on a
// control-plane hiccup would drop reservations workers are filling under.
func (c *Config) Run(ctx context.Context) error {
	if c.Intents == nil {
		return errors.New("no Kubernetes intent source is configured")
	}
	ctx, cancelRun := context.WithCancel(ctx)
	defer cancelRun()
	if starter, ok := c.snapshot.(interface{ Start(context.Context) }); ok {
		starter.Start(ctx)
	}
	live, err := c.LiveWorkers(ctx)
	switch err {
	case nil:
		if err := c.Broker.Reconstruct(func() (map[string]bool, error) { return live, nil }); err != nil {
			return fmt.Errorf("reconstruct the source generations: %w", err)
		}
	default:
		// Not fatal when the worker set cannot be listed on start: keep
		// recorded fills so a control-plane hiccup never drops a reservation,
		// while immediately invalidating serving transfer readiness.
		if err := c.Broker.InvalidateServing(); err != nil {
			return fmt.Errorf("invalidate serving generations on start: %w", err)
		}
	}

	listener, err := net.Listen("tcp", c.ControlAddr)
	if err != nil {
		return fmt.Errorf("listen on the control address %s: %w", c.ControlAddr, err)
	}
	defer listener.Close()
	server := &http.Server{
		Handler:  broker.NewControlHandlerWithInstanceHook(c.Broker, c.recordWorkerInstance),
		ErrorLog: klog.NewStandardLogger("WARNING"),
	}

	// The worker's bootstrap credential: a Pod-local random token the
	// broker writes into the emptyDir file both containers share, and the
	// only bearer the /v1/assignments poll accepts.
	bootstrap, err := c.writeBootstrapCredential()
	if err != nil {
		return err
	}
	c.Broker.SetBootstrapCredential(bootstrap)
	c.Broker.SetPrivateSource(broker.NewPrivateSource(bootstrap))
	c.bootstrapToken = bootstrap
	if err := c.reconcileSource(ctx); err != nil {
		klog.ErrorS(err, "initial source-intent reconciliation failed")
	}
	intentDone := make(chan struct{})
	go func() {
		defer close(intentDone)
		ticker := time.NewTicker(max(c.TickEvery, time.Second))
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if err := c.reconcileSource(ctx); err != nil {
					klog.ErrorS(err, "source-intent reconciliation failed")
				}
			}
		}
	}()
	defer func() { cancelRun(); <-intentDone }()

	// The engine session listener: TLS only, and only with an authorizer.
	// A deployment that cannot authenticate its engines yet serves no
	// sessions at all rather than unauthenticated ones.
	if c.Authorizer == nil || c.SessionTLSCert == "" || c.SessionTLSKey == "" {
		return errors.New("the session listener requires an authorizer and a TLS certificate pair")
	}
	sessionMux := http.NewServeMux()
	sessionMux.Handle("/v1/metadata", c.metadataHandler())
	sessionMux.Handle("/", broker.NewSessionHandler(c.Broker, c.Authorizer))
	sessionServer, err := broker.NewSessionTLSServer(c.SessionAddr, sessionMux, c.SessionTLSCert, c.SessionTLSKey)
	if err != nil {
		return err
	}
	sessionListener, err := net.Listen("tcp", c.SessionAddr)
	if err != nil {
		return fmt.Errorf("listen on the session address %s: %w", c.SessionAddr, err)
	}
	defer sessionListener.Close()
	sessionErr := make(chan error, 1)
	go func() { sessionErr <- sessionServer.ServeTLS(sessionListener, "", "") }()
	defer func() { _ = sessionServer.Close() }()

	sweeps := make(chan error, 1)
	go c.sweepLeases(ctx, sweeps)

	serveErr := make(chan error, 1)
	go func() { serveErr <- server.Serve(listener) }()

	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdownCtx)
		if err := <-serveErr; err != nil && !errors.Is(err, http.ErrServerClosed) {
			return fmt.Errorf("serve the control service: %w", err)
		}

		return context.Canceled
	case err := <-serveErr:
		return fmt.Errorf("serve the control service: %w", err)
	case err := <-sessionErr:
		return fmt.Errorf("serve the TLS session service: %w", err)
	case err := <-sweeps:
		_ = server.Close()
		return err
	}
}

// sweepLeases reports lifecycle failures; cancellation only ends the monitor.
func (c *Config) sweepLeases(ctx context.Context, failures chan<- error) {
	ticker := time.NewTicker(max(c.TickEvery, time.Second))
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := c.Broker.Tick(); err != nil {
				failures <- fmt.Errorf("sweep the leases: %w", err)
				return
			}
		}
	}
}
