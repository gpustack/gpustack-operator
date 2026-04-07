package peer

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/go-logr/logr"
	libp2p "github.com/libp2p/go-libp2p"
	libp2ppubsub "github.com/libp2p/go-libp2p-pubsub"
	"github.com/libp2p/go-libp2p/core/crypto"
	libp2phost "github.com/libp2p/go-libp2p/core/host"
	libp2pnetwork "github.com/libp2p/go-libp2p/core/network"
	libp2ppeer "github.com/libp2p/go-libp2p/core/peer"
	libp2pquic "github.com/libp2p/go-libp2p/p2p/transport/quic"
	libp2ptcp "github.com/libp2p/go-libp2p/p2p/transport/tcp"
	libp2pwebsocket "github.com/libp2p/go-libp2p/p2p/transport/websocket"
	maddr "github.com/multiformats/go-multiaddr"
	core "k8s.io/api/core/v1"
	discovery "k8s.io/api/discovery/v1"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/rest"
	klog "k8s.io/klog/v2"
	ctrlcli "sigs.k8s.io/controller-runtime/pkg/client"

	server "gpustack.ai/gpustack/api/server/v1"
	servercore "gpustack.ai/gpustack/api/server/v1alpha1"
	"gpustack.ai/gpustack/pkg/kubeclients/kubernetes"
	"gpustack.ai/gpustack/pkg/kubeconfig"
	"gpustack.ai/gpustack/pkg/kubediscovery"
	"gpustack.ai/gpustack/pkg/utils/funcx"
	"gpustack.ai/gpustack/pkg/utils/gox"
	"gpustack.ai/gpustack/pkg/utils/osx"
	"gpustack.ai/gpustack/pkg/utils/stringx"
	"gpustack.ai/gpustack/pkg/utils/varx"
	"gpustack.ai/gpustack/pkg/utils/version"
	"gpustack.ai/gpustack/pkg/utils/waitx"
)

const (
	registrationProtocolID = "/anystack/registration/1.0.0"
	reverseProxyProtocolID = "/anystack/reverseproxy/1.0.0"
)

type ControlPlaneConfig struct {
	BindAddress            net.IP
	BindPort               int
	ClusterID              string
	SelfIP                 string
	LoopbackKubeRestConfig rest.Config
	LoopbackKubeClient     kubernetes.Interface
}

// ControlPlane defines the control plane peer.
type ControlPlane struct {
	config         *ControlPlaneConfig
	selfPrivateKey crypto.PrivKey
	selfPeerID     string
	peerRoute      types.NamespacedName
	peerEpCh       chan peerEpInfo
	p2pHost        varx.Notify[libp2phost.Host]
	p2pPub         varx.Notify[*libp2ppubsub.Topic]
	dps            sync.Map
}

// NewControlPlane creates a new ControlPlane instance.
func NewControlPlane(config ControlPlaneConfig, route types.NamespacedName) (*ControlPlane, error) {
	privateKey := GeneratePrivateKey(config.ClusterID, config.SelfIP)
	peerID := GenerateID(privateKey).String()

	return &ControlPlane{
		config:         &config,
		selfPrivateKey: privateKey,
		selfPeerID:     peerID,
		peerRoute:      route,
		peerEpCh:       make(chan peerEpInfo, 1),
		p2pHost:        varx.NewNotify[libp2phost.Host](),
		p2pPub:         varx.NewNotify[*libp2ppubsub.Topic](),
	}, nil
}

// Start starts the control plane.
func (cp *ControlPlane) Start(ctx context.Context) error {
	logger := klog.Background().WithName("peer")

	var (
		address       = cp.config.BindAddress
		port          = cp.config.BindPort
		peerClusterID = cp.config.ClusterID
		selfIP        = cp.config.SelfIP
	)

	// Launch.
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
		libp2p.Identity(cp.selfPrivateKey),
		libp2p.UserAgent(version.GetUserAgent()),
		libp2p.DisableMetrics(),
	)
	p2pHost, err := libp2p.New(p2pOpts...)
	if err != nil {
		return fmt.Errorf("create p2p host: %w", err)
	}
	defer osx.Close(p2pHost)

	logger.Info("created p2p host", "id", p2pHost.ID(), "addrs", p2pHost.Addrs())

	// Create pubsub topic for cluster route broadcasting.
	p2pPubsub, err := libp2ppubsub.NewGossipSub(ctx, p2pHost)
	if err != nil {
		return fmt.Errorf("create p2p pubsub: %w", err)
	}
	p2pPub, err := p2pPubsub.Join(peerClusterID)
	if err != nil {
		return fmt.Errorf("join p2p pubsub topic: %w", err)
	}
	defer osx.Close(p2pPub)
	p2pSub, err := p2pPub.Subscribe()
	if err != nil {
		return fmt.Errorf("subscribe p2p pubsub topic: %w", err)
	}
	defer p2pSub.Cancel()

	// Configure stream handler for cluster registration.
	p2pHost.SetStreamHandler(registrationProtocolID, cp.handleRegistration(ctx, p2pPub, logger))

	cp.p2pHost.Configure(p2pHost)
	cp.p2pPub.Configure(p2pPub)

	gp := gox.GroupWithContextIn(ctx)
	gp.Go(func(ctx context.Context) error {
		// List/watch peer endpoints in the cluster and send produce peer endpoint events.
		return waitx.PollUntilContextCancel(ctx, 1*time.Second, true, func(ctx context.Context) error {
			err = cp.listWatchPeerEndpoints(ctx)
			if err != nil {
				logger.Error(err, "list/watch peer endpoints, retrying...")
			}
			return nil
		})
	})
	gp.Go(func(ctx context.Context) error {
		// Listen for peer endpoint events and connect/disconnect to peers accordingly.
		for {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case ep := <-cp.peerEpCh:
				if ep.IP == selfIP {
					continue
				}
				// For deleted event, we don't care about the IP address, just remove the peer from peerstore.
				if ep.Type == watch.Deleted {
					logger.V(4).Info("deleted peer endpoint", "endpoint", ep)
					peerID := GenerateIDFromSeed(peerClusterID, ep.IP)
					p2pHost.Peerstore().ClearAddrs(peerID)
					continue
				}

				// For added event, we need to connect to the peer.
				logger.V(4).Info("ingested peer endpoint", "endpoint", ep)
				var peerInfo libp2ppeer.AddrInfo
				peerInfo.ID = GenerateIDFromSeed(peerClusterID, ep.IP)
				if address.To4() == nil {
					peerInfo.Addrs = []maddr.Multiaddr{
						maddr.StringCast(fmt.Sprintf("/ip6/%s/tcp/%d", ep.IP, port)),
						maddr.StringCast(fmt.Sprintf("/ip6/%s/udp/%d/quic-v1", ep.IP, port)),
						maddr.StringCast(fmt.Sprintf("/ip6/%s/tcp/%d/ws", ep.IP, port)),
					}
				} else {
					peerInfo.Addrs = []maddr.Multiaddr{
						maddr.StringCast(fmt.Sprintf("/ip4/%s/tcp/%d", ep.IP, port)),
						maddr.StringCast(fmt.Sprintf("/ip4/%s/udp/%d/quic-v1", ep.IP, port)),
						maddr.StringCast(fmt.Sprintf("/ip4/%s/tcp/%d/ws", ep.IP, port)),
					}
				}
				err = p2pHost.Connect(ctx, peerInfo)
				if err != nil {
					logger.Error(err, "connect to peer failed", "ID", peerInfo.ID, "addrs", peerInfo.Addrs)
				}
			}
		}
	})
	gp.Go(func(ctx context.Context) error {
		// Listen for cluster(data plane) route broadcasts and update the cluster-peer mapping.
		for {
			select {
			case <-ctx.Done():
				return ctx.Err()
			default:
				dpRoute, err := p2pSub.Next(ctx)
				if err != nil {
					logger.Error(err, "get next pubsub message")
					continue
				}
				dpKey, peerID, err := parseDataPlaneData(dpRoute.Data)
				if err != nil {
					logger.Error(err, "parse dataplane route from pubsub message", "message", string(dpRoute.Data))
					continue
				}
				cp.dps.Store(dpKey, peerID)
			}
		}
	})

	return gp.Wait()
}

// Dialer defines the function type for dialing a connection to the data plane.
type Dialer = func(context.Context, string, string) (net.Conn, error)

// GetDataPlaneDialer returns a Dialer function for the given dataplane(cluster) key,
// which can be used to dial a connection to the data plane through the p2p network.
func (cp *ControlPlane) GetDataPlaneDialer(dpKey types.NamespacedName) Dialer {
	p2pHost := cp.p2pHost.Get()

	return func(ctx context.Context, _, _ string) (net.Conn, error) {
		p2pPeerID, ok := cp.dps.Load(dpKey)
		if !ok {
			return nil, fmt.Errorf("cluster %s not found", dpKey)
		}
		p2pStream, err := p2pHost.NewStream(ctx, p2pPeerID.(libp2ppeer.ID), reverseProxyProtocolID)
		if err != nil {
			return nil, fmt.Errorf("create p2p stream: %w", err)
		}
		return &p2pStreamConn{Stream: p2pStream}, nil
	}
}

// GetBootstrapPeerIDs returns the peer IDs of the control plane's p2p host,
// which can be used by the data plane to connect to the control plane for bootstrapping.
func (cp *ControlPlane) GetBootstrapPeerIDs() []string {
	p2pPub := cp.p2pPub.Get()

	peers := p2pPub.ListPeers()
	peerIDs := make([]string, 0, len(peers))
	for _, peerID := range peers {
		peerIDs = append(peerIDs, peerID.String())
	}
	return peerIDs
}

// GetBootstrapPort returns the port number that the control plane is listening on for p2p connections.
func (cp *ControlPlane) GetBootstrapPort() int {
	return cp.config.BindPort
}

// GetPeerID returns the peer ID.
func (cp *ControlPlane) GetPeerID() string {
	return cp.selfPeerID
}

// GetClusterKubeRestConfig returns the kube rest config of the cluster,
// which can be used to create kube client to access the cluster.
func (cp *ControlPlane) GetClusterKubeRestConfig(ctx context.Context, cls types.NamespacedName, opts ...func(*rest.Config)) (*rest.Config, error) {
	var restCfg *rest.Config

	lpCli := cp.config.LoopbackKubeClient
	clsCfg, err := lpCli.ServerV1().Clusters(cls.Namespace).GetConfig(ctx, cls.Name, meta.GetOptions{ResourceVersion: "0"})
	if err != nil {
		return nil, fmt.Errorf("get cluster config: %w", err)
	}

	if clsCfg.Status.Type == servercore.ClusterTypeLoopback {
		lpRestCfg := cp.config.LoopbackKubeRestConfig
		restCfg = &lpRestCfg
	} else {
		if clsCfg.Status.Config == "" {
			return nil, fmt.Errorf("cluster api config is empty")
		}
		restCfg, err = kubeconfig.LoadRestConfigFromApiConfigContent(
			stringx.ToBytes(&clsCfg.Status.Config),
			clsCfg.Status.Type == servercore.ClusterTypeProxy)
		if err != nil {
			return nil, fmt.Errorf("load rest config from cluster api config: %w", err)
		}
		if clsCfg.Status.Type == servercore.ClusterTypeReverseProxy {
			restCfg.Dial = cp.GetDataPlaneDialer(cls)
		}
	}

	restCfg.Timeout = 15 * time.Second
	restCfg.QPS = 100
	restCfg.Burst = 200
	restCfg.UserAgent = version.GetUserAgent()
	for _, opt := range opts {
		opt(restCfg)
	}

	return restCfg, nil
}

// GetClusterKubeClient returns the kube client of the cluster.
func (cp *ControlPlane) GetClusterKubeClient(ctx context.Context, cls types.NamespacedName, opts ...func(*rest.Config)) (kubernetes.Interface, error) { // nolint:lll
	cliCfg, err := cp.GetClusterKubeRestConfig(ctx, cls, opts...)
	if err != nil {
		return nil, fmt.Errorf("get kube rest config: %w", err)
	}

	return kubernetes.NewForConfig(cliCfg)
}

// GetClusterCtrlClient returns the controller-runtime client of the cluster.
func (cp *ControlPlane) GetClusterCtrlClient(ctx context.Context, cls types.NamespacedName, opts ...func(*rest.Config)) (ctrlcli.Client, error) {
	cliCfg, err := cp.GetClusterKubeRestConfig(ctx, cls, opts...)
	if err != nil {
		return nil, fmt.Errorf("get kube rest config: %w", err)
	}

	return ctrlcli.NewWithWatch(cliCfg, ctrlcli.Options{})
}

// GetClusterMetadata returns the metadata of the cluster, including API endpoint, CA and Kubernetes version.
func (cp *ControlPlane) GetClusterMetadata(ctx context.Context, cls types.NamespacedName) (*ClusterMetadata, error) {
	cliCfg, err := cp.GetClusterKubeRestConfig(ctx, cls)
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

type (
	requestRegistration struct {
		Token     string `json:"token"`
		Team      string `json:"team"`
		Cluster   string `json:"cluster"`
		ApiConfig string `json:"ApiConfig"`
	}
	responseRegistration struct {
		OK bool `json:"ok"`
	}
)

func (r *requestRegistration) Validate() error {
	if r.Token == "" {
		return fmt.Errorf("missing token")
	}
	if r.Team == "" {
		return fmt.Errorf("missing team")
	}
	if r.Cluster == "" {
		return fmt.Errorf("missing cluster")
	}
	if r.ApiConfig == "" {
		return fmt.Errorf("missing apiConfig")
	}
	return nil
}

func (cp *ControlPlane) handleRegistration(ctx context.Context, p2pPub *libp2ppubsub.Topic, logger logr.Logger) func(libp2pnetwork.Stream) {
	lpCli := cp.config.LoopbackKubeClient

	handle := func(p2pStream libp2pnetwork.Stream, logger logr.Logger) error {
		logger.Info("received registration stream")

		// Parse request.
		var req requestRegistration
		if err := json.NewDecoder(p2pStream).Decode(&req); err != nil {
			return fmt.Errorf("decode registration request: %w", err)
		}

		// Validate request.
		if err := req.Validate(); err != nil {
			return fmt.Errorf("invalid registration request: %w", err)
		}

		// Validate type and token.
		cls, err := lpCli.ServerV1().
			Clusters(req.Team).
			Get(ctx, req.Cluster, meta.GetOptions{
				ResourceVersion: "0",
			})
		if err != nil {
			return fmt.Errorf("get cluster: %w", err)
		}
		if cls.Spec.Type != servercore.ClusterTypeReverseProxy {
			return fmt.Errorf("invalid cluster type: %s", cls.Spec.Type)
		}
		if stringx.SumBySHA256(string(cls.UID), cls.Namespace, cls.Name) != req.Token {
			return fmt.Errorf("invalid registration token")
		}

		// Register cluster.
		clsCfg := &server.ClusterConfig{
			ObjectMeta: cls.ObjectMeta,
			Spec: server.ClusterConfigSpec{
				Config: req.ApiConfig,
			},
		}
		_, err = lpCli.ServerV1().
			Clusters(req.Team).
			UpdateConfig(ctx, req.Cluster, clsCfg, meta.UpdateOptions{})
		if err != nil {
			return fmt.Errorf("update cluster config: %w", err)
		}

		// Broadcast registration.
		dpRouteData := getDataPlaneData(
			types.NamespacedName{
				Namespace: req.Team,
				Name:      req.Cluster,
			},
			p2pStream.Conn().RemotePeer(),
		)
		if err = p2pPub.Publish(ctx, dpRouteData); err != nil {
			return fmt.Errorf("broadcast registration cluster: %w", err)
		}

		logger.Info("registered cluster", "team", req.Team, "cluster", req.Cluster)
		return nil
	}

	return func(p2pStream libp2pnetwork.Stream) {
		defer osx.Close(p2pStream)

		logger := logger.
			WithValues("stream", stringifyStream(p2pStream))

		if err := handle(p2pStream, logger); err != nil {
			logger.Error(err, "handle registration request")
			_ = p2pStream.Reset()
		}

		if err := json.NewEncoder(p2pStream).Encode(responseRegistration{OK: true}); err != nil {
			logger.Error(err, "encode registration response")
			_ = p2pStream.Reset()
		}
	}
}

func (cp *ControlPlane) listWatchPeerEndpoints(ctx context.Context) error {
	lpCli := cp.config.LoopbackKubeClient

	ver, _ := kubediscovery.GetVersion(ctx, lpCli.Discovery())
	if ver != nil && ver.Major >= "1" && ver.Minor >= "33" {
		epsCli := lpCli.DiscoveryV1().EndpointSlices(cp.peerRoute.Namespace)

		// List the endpoints first to get the initial state.
		epsExisted := sets.Set[string]{}
		eps, err := epsCli.Get(ctx, cp.peerRoute.Name, meta.GetOptions{ResourceVersion: "0"})
		if err != nil {
			return err
		}
		for _, ep := range eps.Endpoints {
			if ep.Conditions.Serving != nil && *ep.Conditions.Serving {
				for _, addr := range ep.Addresses {
					cp.peerEpCh <- addedPeerEp(addr)
					epsExisted.Insert(addr)
				}
			}
		}

		// Watch the endpoints for changes.
		epsWatcher, err := epsCli.Watch(ctx, meta.ListOptions{
			ResourceVersion: "0",
			FieldSelector:   "metadata.name=" + cp.peerRoute.Name,
		})
		if err != nil {
			return err
		}
		defer epsWatcher.Stop()

		for e := range epsWatcher.ResultChan() {
			if e.Object == nil {
				continue
			}
			eps, ok := e.Object.(*discovery.EndpointSlice)
			if !ok {
				continue
			}

			switch e.Type {
			default:
				continue
			case watch.Deleted:
				for _, ep := range epsExisted.UnsortedList() {
					cp.peerEpCh <- deletedPeerEp(ep)
				}
				epsExisted.Clear()
			case watch.Added, watch.Modified:
				epsUpdated := sets.Set[string]{}
				for _, ep := range eps.Endpoints {
					if ep.Conditions.Serving != nil && *ep.Conditions.Serving {
						for _, addr := range ep.Addresses {
							if !epsExisted.Has(addr) {
								cp.peerEpCh <- addedPeerEp(addr)
							}
							epsUpdated.Insert(addr)
						}
					}
				}
				for addr := range epsExisted.Difference(epsUpdated) {
					cp.peerEpCh <- deletedPeerEp(addr)
				}
				epsExisted = epsUpdated
			}
		}

		return nil
	}

	epCli := lpCli.CoreV1().Endpoints(cp.peerRoute.Namespace)

	// List the endpoints first to get the initial state.
	epExisted := sets.Set[string]{}
	epList, err := epCli.Get(ctx, cp.peerRoute.Name, meta.GetOptions{ResourceVersion: "0"})
	if err != nil {
		return err
	}
	for _, ss := range epList.Subsets {
		for _, addr := range ss.Addresses {
			cp.peerEpCh <- addedPeerEp(addr.IP)
			epExisted.Insert(addr.IP)
		}
	}

	// Watch the endpoints for changes.
	epWatcher, err := epCli.Watch(ctx, meta.ListOptions{
		ResourceVersion: "0",
		FieldSelector:   "metadata.name=" + cp.peerRoute.Name,
	})
	if err != nil {
		return err
	}
	defer epWatcher.Stop()

	for e := range epWatcher.ResultChan() {
		if e.Object == nil {
			continue
		}
		ep, ok := e.Object.(*core.Endpoints)
		if !ok {
			continue
		}
		switch e.Type {
		default:
			continue
		case watch.Deleted:
			for _, ep := range epExisted.UnsortedList() {
				cp.peerEpCh <- deletedPeerEp(ep)
			}
			epExisted.Clear()
		case watch.Added, watch.Modified:
			epsUpdated := sets.Set[string]{}
			for _, ss := range ep.Subsets {
				for _, addr := range ss.Addresses {
					if !epExisted.Has(addr.IP) {
						cp.peerEpCh <- addedPeerEp(addr.IP)
					}
					epsUpdated.Insert(addr.IP)
				}
			}
			for addr := range epExisted.Difference(epsUpdated) {
				cp.peerEpCh <- deletedPeerEp(addr)
			}
			epExisted = epsUpdated
		}
	}

	return nil
}

type peerEpInfo struct {
	IP   string
	Type watch.EventType
}

func addedPeerEp(ip string) peerEpInfo {
	return peerEpInfo{
		IP:   ip,
		Type: watch.Added,
	}
}

func deletedPeerEp(ip string) peerEpInfo {
	return peerEpInfo{
		IP:   ip,
		Type: watch.Deleted,
	}
}

type p2pStreamConn struct {
	libp2pnetwork.Stream
}

func (s *p2pStreamConn) LocalAddr() net.Addr {
	return convertMultiaddrToNetAddr(s.Stream.Conn().LocalMultiaddr())
}

func (s *p2pStreamConn) RemoteAddr() net.Addr {
	return convertMultiaddrToNetAddr(s.Stream.Conn().RemoteMultiaddr())
}

func (s *p2pStreamConn) String() string {
	return stringifyStream(s.Stream)
}

func convertMultiaddrToNetAddr(addr maddr.Multiaddr) net.Addr {
	if port, err := addr.ValueForProtocol(maddr.P_TCP); err == nil {
		ip, _ := addr.ValueForProtocol(maddr.P_IP4)
		if ip == "" {
			ip, _ = addr.ValueForProtocol(maddr.P_IP6)
		}
		return &net.TCPAddr{
			IP:   net.ParseIP(ip),
			Port: funcx.NoError(strconv.Atoi(port)),
		}
	}

	port := funcx.MustNoError(addr.ValueForProtocol(maddr.P_UDP))
	ip, _ := addr.ValueForProtocol(maddr.P_IP4)
	if ip == "" {
		ip, _ = addr.ValueForProtocol(maddr.P_IP6)
	}
	return &net.UDPAddr{
		IP:   net.ParseIP(ip),
		Port: funcx.NoError(strconv.Atoi(port)),
	}
}

func stringifyStream(p2pStream libp2pnetwork.Stream) string {
	c := p2pStream.Conn()
	return fmt.Sprintf("%s (%s) <-> %s (%s)", c.LocalPeer(), c.LocalMultiaddr(), c.RemotePeer(), c.RemoteMultiaddr())
}

const dpRouteDelimiter = "::"

func getDataPlaneData(dpKey types.NamespacedName, peerID libp2ppeer.ID) []byte {
	dpKeyStr := fmt.Sprintf("%s/%s", dpKey.Namespace, dpKey.Name)
	dpRoute := dpKeyStr + dpRouteDelimiter + peerID.String()
	return stringx.ToBytes(&dpRoute)
}

func parseDataPlaneData(data []byte) (types.NamespacedName, libp2ppeer.ID, error) {
	dpRoute := stringx.FromBytes(&data)
	parts := strings.Split(dpRoute, dpRouteDelimiter)
	if len(parts) != 2 {
		return types.NamespacedName{}, "", fmt.Errorf("invalid dataplane route: %s", dpRoute)
	}
	dpKeyStr, peerIDStr := parts[0], parts[1]
	peerID, err := libp2ppeer.Decode(peerIDStr)
	if err != nil {
		return types.NamespacedName{}, "", fmt.Errorf("invalid peer ID in dataplane route: %w", err)
	}
	nsKey := strings.Split(dpKeyStr, "/")
	if len(nsKey) != 2 {
		return types.NamespacedName{}, "", fmt.Errorf("invalid cluster key in dataplane route: %s", dpKeyStr)
	}
	return types.NamespacedName{
		Namespace: nsKey[0],
		Name:      nsKey[1],
	}, peerID, nil
}
