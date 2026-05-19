package webserver

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"path/filepath"
	"strconv"

	"github.com/spf13/pflag"
	"sigs.k8s.io/controller-runtime/pkg/certwatcher"

	certcache "gpustack.ai/gpustack/pkg/utils/certs/cache"
	"gpustack.ai/gpustack/pkg/utils/certs/fakecert"
	"gpustack.ai/gpustack/pkg/utils/osx"
)

type Options struct {
	FlagOptions

	// Establish.
	BindUnixPath string
	BindAddress  net.IP
	BindPort     int
	CertDir      string
}

func NewOptions() *Options {
	return &Options{
		// Establish.
		BindAddress: net.ParseIP("0.0.0.0"),
		BindPort:    9443,
	}
}

type (
	FlagOptions struct {
		noBindUnixPath bool
	}

	FlagOption func(opts *FlagOptions)
)

func WithoutBindUnixPath() FlagOption {
	return func(opts *FlagOptions) {
		opts.noBindUnixPath = true
	}
}

func (o *Options) AddFlags(fs *pflag.FlagSet, opts ...FlagOption) {
	for i := range opts {
		opts[i](&o.FlagOptions)
	}

	// Establish.
	if !o.noBindUnixPath {
		fs.StringVar(&o.BindUnixPath, "bind-unix-path", o.BindUnixPath,
			"the unix socket path on which to serve. "+
				"if specified, --bind-address and --secure-port will be ignored.")
	}
	fs.IPVar(&o.BindAddress, "bind-address", o.BindAddress,
		"the IP address(without port) on which to serve.")
	fs.IntVar(&o.BindPort, "secure-port", o.BindPort,
		"the port on which to serve HTTPS.")
	fs.StringVar(&o.CertDir, "cert-dir", o.CertDir,
		"the directory where the TLS certs are located. "+
			"if provided, must place tls.crt and tls.key under --cert-dir.")
}

func (o *Options) Validate(_ context.Context) error {
	// Establish.
	if !o.noBindUnixPath && o.BindUnixPath != "" {
		if !osx.ExistsParentDir(o.BindUnixPath) {
			return errors.New("--bind-unix-path: no found parent directory")
		}
		return nil
	}
	if o.BindPort < 1 || o.BindPort > 65535 {
		return errors.New("--secure-port: out of range")
	}
	if o.CertDir != "" {
		if !osx.ExistsDir(o.CertDir) {
			return errors.New("--cert-dir: no found directory")
		}
		if !osx.Exists(filepath.Join(o.CertDir, "tls.crt")) {
			return errors.New("--cert-dir: no found tls.crt")
		}
		if !osx.Exists(filepath.Join(o.CertDir, "tls.key")) {
			return errors.New("--cert-dir: no found tls.key")
		}
	}

	return nil
}

func (o *Options) Complete(_ context.Context) (*Config, error) {
	var cfg Config

	tlsCfg := &tls.Config{
		NextProtos: []string{"http2", "http/1.1"},
		CipherSuites: []uint16{
			tls.TLS_ECDHE_RSA_WITH_CHACHA20_POLY1305_SHA256,
			tls.TLS_ECDHE_ECDSA_WITH_CHACHA20_POLY1305_SHA256,
		},
		MinVersion: tls.VersionTLS12,
	}
	if dir := o.CertDir; dir != "" {
		certPath := filepath.Join(dir, "tls.crt")
		keyPath := filepath.Join(dir, "tls.key")
		certWatcher, err := certwatcher.New(certPath, keyPath)
		if err != nil {
			return nil, fmt.Errorf("create cert watcher: %w", err)
		}
		tlsCfg.GetCertificate = certWatcher.GetCertificate
		cfg.Runners = append(cfg.Runners, certWatcher)
	} else {
		certCache := certcache.NewDirCache(osx.TempDir("tls"))
		certMgr := &fakecert.DynamicManager{
			Cache: certCache,
		}
		tlsCfg.GetCertificate = certMgr.GetCertificate
	}

	var (
		lis net.Listener
		err error
	)
	if o.BindUnixPath != "" {
		err = osx.RemoveSocket(o.BindUnixPath)
		if err != nil {
			return nil, fmt.Errorf("remove existing unix socket: %w", err)
		}
		lis, err = tls.Listen("unix", o.BindUnixPath, tlsCfg)
	} else {
		lis, err = tls.Listen("tcp",
			net.JoinHostPort(o.BindAddress.String(), strconv.Itoa(o.BindPort)), tlsCfg)
	}
	if err != nil {
		return nil, fmt.Errorf("create listener: %w", err)
	}

	cfg.Listener = lis
	return &cfg, nil
}
