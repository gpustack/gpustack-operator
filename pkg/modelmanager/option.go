package modelmanager

import (
	"context"
	"errors"
	"path/filepath"

	"github.com/spf13/pflag"
	"k8s.io/apimachinery/pkg/runtime"

	"gpustack.ai/gpustack/pkg/manager"
	"gpustack.ai/gpustack/pkg/modelstore"
	"gpustack.ai/gpustack/pkg/webserver"
)

// Options are the model-manager command's flags.
type Options struct {
	// Server.
	ServerOptions *webserver.Options

	// Manager.
	ManagerOptions *manager.Options

	// Node.
	NodeName   string
	KubeletDir string
	CacheRoot  string
	CSISocket  string
}

// NewOptions returns the options with their defaults: the kubelet directory and cache root of a
// standard node, and the plugin's secure port.
func NewOptions() *Options {
	opts := &Options{
		ServerOptions:  webserver.NewOptions(),
		ManagerOptions: manager.NewOptions(),
		KubeletDir:     "/var/lib/kubelet",
		CacheRoot:      "/var/lib/gpustack/models",
	}
	opts.ServerOptions.BindPort = 32444
	opts.ManagerOptions.KubeContentType = runtime.ContentTypeJSON

	return opts
}

// AddFlags registers the options on fs.
func (o *Options) AddFlags(fs *pflag.FlagSet) {
	// Server.
	o.ServerOptions.AddFlags(fs, webserver.WithoutBindUnixPath())

	// Manager.
	o.ManagerOptions.AddFlags(fs, manager.WithoutKubeElectionOptions())

	// Node.
	fs.StringVar(&o.NodeName, "node-name", o.NodeName,
		"the name of the node this plugin runs on, also its NodeModelStore's name.")
	fs.StringVar(&o.KubeletDir, "kubelet-dir", o.KubeletDir,
		"the kubelet's root directory, whose pods directory holds the volume targets.")
	fs.StringVar(&o.CacheRoot, "cache-root", o.CacheRoot,
		"the node's model cache directory.")
	fs.StringVar(&o.CSISocket, "csi-socket", o.CSISocket,
		"the Unix socket the CSI services listen on; empty is <kubelet-dir>/plugins/"+modelstore.DriverName+"/csi.sock.")
}

// Validate checks the options: a node name is required, and every path must be absolute.
func (o *Options) Validate(ctx context.Context) error {
	if err := o.ServerOptions.Validate(ctx); err != nil {
		return err
	}
	if err := o.ManagerOptions.Validate(ctx); err != nil {
		return err
	}
	switch {
	case o.NodeName == "":
		return errors.New("--node-name: required")
	case !filepath.IsAbs(o.KubeletDir):
		return errors.New("--kubelet-dir: must be absolute")
	case !filepath.IsAbs(o.CacheRoot):
		return errors.New("--cache-root: must be absolute")
	case o.CSISocket != "" && !filepath.IsAbs(o.CSISocket):
		return errors.New("--csi-socket: must be absolute")
	}

	return nil
}

// Complete turns the options into the configuration the plugin runs with.
func (o *Options) Complete(ctx context.Context) (*Config, error) {
	srvConfig, err := o.ServerOptions.Complete(ctx)
	if err != nil {
		return nil, err
	}
	mgrConfig, err := o.ManagerOptions.Complete(ctx)
	if err != nil {
		return nil, err
	}
	socket := o.CSISocket
	if socket == "" {
		socket = filepath.Join(o.KubeletDir, "plugins", modelstore.DriverName, "csi.sock")
	}

	return &Config{
		ServerConfig:  srvConfig,
		ManagerConfig: mgrConfig,
		NodeName:      o.NodeName,
		KubeletDir:    filepath.Clean(o.KubeletDir),
		CacheRoot:     filepath.Clean(o.CacheRoot),
		CSISocket:     socket,
	}, nil
}
