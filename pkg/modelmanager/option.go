package modelmanager

import (
	"context"
	"errors"
	"path/filepath"

	"github.com/spf13/pflag"
	"k8s.io/apimachinery/pkg/runtime"

	"gpustack.ai/gpustack/pkg/manager"
	"gpustack.ai/gpustack/pkg/modelmanager/peer"
	"gpustack.ai/gpustack/pkg/modelstore"
	"gpustack.ai/gpustack/pkg/systemname"
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

	// Peer sync.
	PeerSyncPort              int
	PeerSyncAuth              string
	PeerSyncServiceAccount    string
	PeerSyncMaxServingStreams int
	PeerSyncStreamsPerSource  int
}

// NewOptions returns the options with their defaults: the kubelet directory and cache root of a
// standard node, and the plugin's secure port.
func NewOptions() *Options {
	opts := &Options{
		ServerOptions:  webserver.NewOptions(),
		ManagerOptions: manager.NewOptions(),
		KubeletDir:     "/var/lib/kubelet",
		CacheRoot:      "/var/lib/gpustack/models",

		PeerSyncAuth:              string(peer.AuthToken),
		PeerSyncMaxServingStreams: 8,
		PeerSyncStreamsPerSource:  peer.DefaultStreamsPerSource,
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

	// Peer sync.
	fs.IntVar(&o.PeerSyncPort, "peer-sync-port", o.PeerSyncPort,
		"the TCP port the node's published trees are served to the other nodes' plugins on; 0 is off.")
	fs.StringVar(&o.PeerSyncAuth, "peer-sync-auth", o.PeerSyncAuth,
		"how peers authenticate each other: token, the plugins' projected ServiceAccount tokens reviewed")
	fs.StringVar(&o.PeerSyncServiceAccount, "peer-sync-service-account", o.PeerSyncServiceAccount,
		"the plugins' ServiceAccount name, which the token authentication admits.")
	fs.IntVar(&o.PeerSyncMaxServingStreams, "peer-sync-max-serving-streams", o.PeerSyncMaxServingStreams,
		"how many peer file answers this node serves at once.")
	fs.IntVar(&o.PeerSyncStreamsPerSource, "peer-sync-streams-per-source", o.PeerSyncStreamsPerSource,
		"how many requests this node opens to one peer at once.")
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

	_, err := o.PeerSync()

	return err
}

// PeerSync is the peer sync's configuration, or nil while the port is off.
func (o *Options) PeerSync() (*peer.Sync, error) {
	if o.PeerSyncPort == 0 {
		return nil, nil
	}
	auth := peer.AuthOptions{
		Mode:           peer.AuthMode(o.PeerSyncAuth),
		Audience:       peer.PeerAudience,
		Namespace:      systemname.NamespaceName,
		ServiceAccount: o.PeerSyncServiceAccount,
	}
	switch auth.Mode {
	case peer.AuthToken:
		if auth.ServiceAccount == "" {
			return nil, errors.New("--peer-sync-service-account: required by --peer-sync-auth=token")
		}
	case peer.AuthNone:
	default:
		return nil, errors.New("--peer-sync-auth: token is the only mode this release serves")
	}
	switch {
	case o.PeerSyncPort < 1 || o.PeerSyncPort > 65535:
		return nil, errors.New("--peer-sync-port: out of range")
	case o.PeerSyncMaxServingStreams < 1 || o.PeerSyncMaxServingStreams > 1024:
		return nil, errors.New("--peer-sync-max-serving-streams: want 1 to 1024")
	case o.PeerSyncStreamsPerSource < 1 || o.PeerSyncStreamsPerSource > 64:
		return nil, errors.New("--peer-sync-streams-per-source: want 1 to 64")
	}

	return &peer.Sync{
		Port:              o.PeerSyncPort,
		Auth:              auth,
		MaxServingStreams: o.PeerSyncMaxServingStreams,
		StreamsPerSource:  o.PeerSyncStreamsPerSource,
	}, nil
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
	peerSync, err := o.PeerSync()
	if err != nil {
		return nil, err
	}

	return &Config{
		ServerConfig:  srvConfig,
		ManagerConfig: mgrConfig,
		NodeName:      o.NodeName,
		KubeletDir:    filepath.Clean(o.KubeletDir),
		CacheRoot:     filepath.Clean(o.CacheRoot),
		CSISocket:     socket,
		PeerSync:      peerSync,
	}, nil
}
