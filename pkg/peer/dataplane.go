package peer

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"path"
	"time"

	"github.com/go-logr/logr"
	libp2p "github.com/libp2p/go-libp2p"
	"github.com/libp2p/go-libp2p/core/crypto"
	libp2pnetwork "github.com/libp2p/go-libp2p/core/network"
	libp2ppeer "github.com/libp2p/go-libp2p/core/peer"
	libp2pquic "github.com/libp2p/go-libp2p/p2p/transport/quic"
	libp2ptcp "github.com/libp2p/go-libp2p/p2p/transport/tcp"
	libp2pwebsocket "github.com/libp2p/go-libp2p/p2p/transport/websocket"
	maddr "github.com/multiformats/go-multiaddr"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/rest"
	klog "k8s.io/klog/v2"
	ctrlcli "sigs.k8s.io/controller-runtime/pkg/client"

	"gpustack.ai/gpustack/pkg/kubeclients/kubernetes"
	"gpustack.ai/gpustack/pkg/kubeconfig"
	"gpustack.ai/gpustack/pkg/kubediscovery"
	"gpustack.ai/gpustack/pkg/utils/gox"
	"gpustack.ai/gpustack/pkg/utils/httpx"
	"gpustack.ai/gpustack/pkg/utils/osx"
	"gpustack.ai/gpustack/pkg/utils/version"
	"gpustack.ai/gpustack/pkg/utils/waitx"
)

type DataPlaneConfig struct {
	ServerURL              string
	BindAddress            net.IP
	BindPort               int
	Token                  string
	Team                   string
	Cluster                string
	LoopbackKubeRestConfig rest.Config
	LoopbackKubeClient     kubernetes.Interface
}

// DataPlane defines the data plane peer.
type DataPlane struct {
	config         *DataPlaneConfig
	selfPrivateKey crypto.PrivKey
	selfPeerID     string
}

// NewDataPlane creates a new DataPlane instance.
func NewDataPlane(config DataPlaneConfig) (*DataPlane, error) {
	privateKey := GeneratePrivateKey(config.Token)
	peerID := GenerateID(privateKey).String()

	return &DataPlane{
		config:         &config,
		selfPrivateKey: privateKey,
		selfPeerID:     peerID,
	}, nil
}

// Start starts the data plane peer.
func (dp *DataPlane) Start(ctx context.Context) error {
	logger := klog.Background().WithName("peer")

	var (
		serverURL       = dp.config.ServerURL
		address         = dp.config.BindAddress
		port            = dp.config.BindPort
		token           = dp.config.Token
		team            = dp.config.Team
		cluster         = dp.config.Cluster
		serverHost      string
		isServerHostDNS bool
		isServerHostIP6 bool
	)
	{
		serverURL, err := url.Parse(serverURL)
		if err != nil {
			return fmt.Errorf("parse server URL: %w", err)
		}

		serverHost = serverURL.Hostname()
		serverIP := net.ParseIP(serverHost)
		isServerHostDNS = serverIP == nil
		if !isServerHostDNS {
			isServerHostIP6 = serverIP.To4() == nil
		}
	}

	// Request /.well-knwon/peer to get the bootstrap peer IDs and port.
	peerURL := path.Join(serverURL, "/.well-known/peer")
	peerResp, err := httpx.DefaultInsecureClient.Get(peerURL)
	if err != nil {
		return fmt.Errorf("get peer info from server: %w", err)
	}
	defer osx.Close(peerResp.Body)
	var peerInfo struct {
		BootstrapPeerIDs []string `json:"bootstrapPeerIDs"`
		BootstrapPort    int      `json:"bootstrapPort"`
	}
	if err := json.NewDecoder(peerResp.Body).Decode(&peerInfo); err != nil {
		return fmt.Errorf("decode peer info response: %w", err)
	}
	if len(peerInfo.BootstrapPeerIDs) == 0 {
		return fmt.Errorf("bootstrap peers no found")
	}

	logger.Info("get bootstrap peer info", "ids", peerInfo.BootstrapPeerIDs, "port", peerInfo.BootstrapPort)

	// Dail to bootstrap peers to join the cluster.
	var p2pOpts []libp2p.Option
	switch {
	case address.IsUnspecified():
		p2pOpts = append(p2pOpts, libp2p.ListenAddrStrings(
			fmt.Sprintf("/ip4/0.0.0.0/tcp/%d", port),
			fmt.Sprintf("/ip4/0.0.0.0/udp/%d/quic-v1", port),
			fmt.Sprintf("/ip4/0.0.0.0/tcp/%d/ws", port),
			fmt.Sprintf("/ip6/::/tcp/%d", port),
			fmt.Sprintf("/ip6/::/udp/%d/quic-v1", port),
			fmt.Sprintf("/ip6/::/tcp/%d/ws", port),
		))
	case address.To4() == nil:
		p2pOpts = append(p2pOpts, libp2p.ListenAddrStrings(
			fmt.Sprintf("/ip6/%s/tcp/%d", address, port),
			fmt.Sprintf("/ip6/%s/udp/%d/quic-v1", address, port),
			fmt.Sprintf("/ip6/%s/tcp/%d/ws", address, port),
		))
	default:
		p2pOpts = append(p2pOpts, libp2p.ListenAddrStrings(
			fmt.Sprintf("/ip4/%s/tcp/%d", address, port),
			fmt.Sprintf("/ip4/%s/udp/%d/quic-v1", address, port),
			fmt.Sprintf("/ip4/%s/tcp/%d/ws", address, port),
		))
	}
	p2pOpts = append(p2pOpts,
		libp2p.ChainOptions(
			libp2p.Transport(libp2ptcp.NewTCPTransport),
			libp2p.Transport(libp2pquic.NewTransport),
			libp2p.Transport(libp2pwebsocket.New),
		),
		libp2p.Identity(dp.selfPrivateKey),
		libp2p.UserAgent(version.GetUserAgent()),
		libp2p.DisableMetrics(),
		libp2p.EnableNATService(),
		libp2p.EnableAutoNATv2(),
	)
	p2pHost, err := libp2p.New(p2pOpts...)
	if err != nil {
		return fmt.Errorf("create libp2p host: %w", err)
	}
	defer osx.Close(p2pHost)

	logger.Info("created p2p host", "id", p2pHost.ID(), "addrs", p2pHost.Addrs())

	// Configure stream handler for reverse proxy.
	p2pHost.SetStreamHandler(reverseProxyProtocolID, dp.handleReverseProxy(ctx, logger))

	var p2pBootstrapPeerAddrInfos []libp2ppeer.AddrInfo
	{
		var p2pBootstrapAddrStrs []string
		switch {
		case isServerHostDNS:
			p2pBootstrapAddrStrs = []string{
				fmt.Sprintf("/dns4/%s/tcp/%d", serverHost, peerInfo.BootstrapPort),
				fmt.Sprintf("/dns4/%s/udp/%d/quic-v1", serverHost, peerInfo.BootstrapPort),
				fmt.Sprintf("/dns4/%s/tcp/%d/ws", serverHost, peerInfo.BootstrapPort),
			}
		case isServerHostIP6:
			p2pBootstrapAddrStrs = []string{
				fmt.Sprintf("/ip6/%s/tcp/%d", serverHost, peerInfo.BootstrapPort),
				fmt.Sprintf("/ip6/%s/udp/%d/quic-v1", serverHost, peerInfo.BootstrapPort),
				fmt.Sprintf("/ip6/%s/tcp/%d/ws", serverHost, peerInfo.BootstrapPort),
			}
		default:
			p2pBootstrapAddrStrs = []string{
				fmt.Sprintf("/ip4/%s/tcp/%d", serverHost, peerInfo.BootstrapPort),
				fmt.Sprintf("/ip4/%s/udp/%d/quic-v1", serverHost, peerInfo.BootstrapPort),
				fmt.Sprintf("/ip4/%s/tcp/%d/ws", serverHost, peerInfo.BootstrapPort),
			}
		}
		p2pBootstrapAddrs := make([]maddr.Multiaddr, len(p2pBootstrapAddrStrs))
		for i := range p2pBootstrapAddrStrs {
			p2pBootstrapAddrs[i] = maddr.StringCast(p2pBootstrapAddrStrs[i])
		}
		p2pBootstrapPeerAddrInfos = make([]libp2ppeer.AddrInfo, len(peerInfo.BootstrapPeerIDs))
		for i := range peerInfo.BootstrapPeerIDs {
			id, err := libp2ppeer.Decode(peerInfo.BootstrapPeerIDs[i])
			if err != nil {
				return fmt.Errorf("invalid bootstrap peer ID %s: %w", peerInfo.BootstrapPeerIDs[i], err)
			}
			p2pBootstrapPeerAddrInfos[i] = libp2ppeer.AddrInfo{
				ID:    id,
				Addrs: p2pBootstrapAddrs,
			}
		}
	}

	logger.Info("connecting to bootstrap peers", "peerIDs", peerInfo.BootstrapPeerIDs)

	// Connect to bootstrap peers with retry.
	var p2pConnectedBootstrapPeerID libp2ppeer.ID
	err = waitx.PollUntilContextTimeout(ctx, time.Second, 30*time.Second, true, func(ctx context.Context) error {
		var err error
		for i := range p2pBootstrapPeerAddrInfos {
			err = p2pHost.Connect(ctx, p2pBootstrapPeerAddrInfos[i])
			if err != nil {
				logger.Error(err, "connect to bootstrap peer", "peerID", p2pBootstrapPeerAddrInfos[i].ID)
				continue
			}
			logger.Info("connected to bootstrap peer", "peerID", p2pBootstrapPeerAddrInfos[i].ID)
			p2pConnectedBootstrapPeerID = p2pBootstrapPeerAddrInfos[i].ID
		}
		return err
	})
	if err != nil {
		return fmt.Errorf("connect to bootstrap peers: %w", err)
	}

	logger.Info("registering to bootstrap peer", "peerID", p2pConnectedBootstrapPeerID)

	// Send registration request to bootstrap peer.
	_, restCfg, _, err := kubeconfig.LoadRestConfigNonInteractive()
	if err != nil {
		return fmt.Errorf("load kubeconfig: %w", err)
	}
	apiCfg, err := kubeconfig.ConvertRestConfigToApiConfigString(restCfg)
	if err != nil {
		return fmt.Errorf("convert rest config to api config: %w", err)
	}
	req := requestRegistration{
		Token:     token,
		Team:      team,
		Cluster:   cluster,
		ApiConfig: apiCfg,
	}
	p2pStream, err := p2pHost.NewStream(ctx, p2pConnectedBootstrapPeerID, registrationProtocolID)
	if err != nil {
		return fmt.Errorf("create p2p stream: %w", err)
	}
	if err = json.NewEncoder(p2pStream).Encode(req); err != nil {
		return fmt.Errorf("send registration request: %w", err)
	}
	var resp responseRegistration
	if err = json.NewDecoder(p2pStream).Decode(&resp); err != nil {
		return fmt.Errorf("receive registration response: %w", err)
	}
	if !resp.OK {
		return errors.New("failed to start data plane peer")
	}

	logger.Info("started data plane peer")

	<-ctx.Done()
	return ctx.Err()
}

// GetPeerID returns the peer ID.
func (dp *DataPlane) GetPeerID() string {
	return dp.selfPeerID
}

// GetClusterKubeRestConfig returns the kube rest config of the current cluster.
func (dp *DataPlane) GetClusterKubeRestConfig(ctx context.Context, cls types.NamespacedName, opts ...func(*rest.Config)) (*rest.Config, error) { // nolint:lll
	lpRestCfg := dp.config.LoopbackKubeRestConfig
	return &lpRestCfg, nil
}

// GetClusterKubeClient returns the kube client of the current cluster.
func (dp *DataPlane) GetClusterKubeClient(ctx context.Context, cls types.NamespacedName, opts ...func(*rest.Config)) (kubernetes.Interface, error) {
	cliCfg, err := dp.GetClusterKubeRestConfig(ctx, cls, opts...)
	if err != nil {
		return nil, fmt.Errorf("get kube rest config: %w", err)
	}

	return kubernetes.NewForConfig(cliCfg)
}

// GetClusterCtrlClient returns the controller-runtime client of current cluster.
func (dp *DataPlane) GetClusterCtrlClient(ctx context.Context, cls types.NamespacedName, opts ...func(*rest.Config)) (ctrlcli.Client, error) {
	cliCfg, err := dp.GetClusterKubeRestConfig(ctx, cls, opts...)
	if err != nil {
		return nil, fmt.Errorf("get kube rest config: %w", err)
	}

	return ctrlcli.NewWithWatch(cliCfg, ctrlcli.Options{})
}

// GetClusterMetadata returns the metadata of current cluster, including API endpoint, CA and Kubernetes version.
func (dp *DataPlane) GetClusterMetadata(ctx context.Context, cls types.NamespacedName) (*ClusterMetadata, error) {
	cliCfg, err := dp.GetClusterKubeRestConfig(ctx, cls)
	if err != nil {
		return nil, fmt.Errorf("get kube rest config: %w", err)
	}

	ver, err := kubediscovery.GetVersionWithRestConfig(ctx, cliCfg)
	if err != nil {
		return nil, fmt.Errorf("get kubernetes version: %w", err)
	}

	var m ClusterMetadata
	m.Endpoint = cliCfg.Host
	if cliCfg.APIPath != "" {
		m.Endpoint += cliCfg.APIPath
	}
	m.Version = ver.String()
	if cliCfg.CAData != nil {
		m.CA = string(cliCfg.CAData)
	}

	return &m, nil
}

func (dp *DataPlane) handleReverseProxy(ctx context.Context, logger logr.Logger) func(p2pStream libp2pnetwork.Stream) {
	handle := func(p2pStream libp2pnetwork.Stream, logger logr.Logger) error {
		logger.Info("received reverse proxy stream")

		dialer := net.Dialer{
			Timeout:   5 * time.Second,
			KeepAlive: 30 * time.Second,
		}

		host, port := osx.Getenv("KUBE_API_SERVER_PROXY_HOST", "localhost"), osx.Getenv("KUBE_API_SERVER_PROXY_PORT", "443")
		if host == "" || port == "" {
			return fmt.Errorf("invalid proxy host or port: host=%s, port=%s", host, port)
		}

		conn, err := dialer.DialContext(ctx, "tcp", net.JoinHostPort(host, port))
		if err != nil {
			return fmt.Errorf("dial local service: %w", err)
		}
		defer osx.Close(conn)

		g := gox.GroupWithContext(ctx)
		g.Go(func() error {
			_, err := io.Copy(conn, p2pStream)
			return err
		})
		g.Go(func() error {
			_, err := io.Copy(p2pStream, conn)
			return err
		})
		if err = g.Wait(); err != nil {
			return fmt.Errorf("proxy io error: %w", err)
		}
		return nil
	}

	return func(p2pStream libp2pnetwork.Stream) {
		defer osx.Close(p2pStream)

		logger := logger.
			WithValues("stream", stringifyStream(p2pStream))

		if err := handle(p2pStream, logger); err != nil {
			logger.Error(err, "handle reverse proxy request")
			_ = p2pStream.Reset()
		}
	}
}
