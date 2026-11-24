package mock

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"google.golang.org/grpc"
	grpccodes "google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	grpcstatus "google.golang.org/grpc/status"
	core "k8s.io/api/core/v1"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	klog "k8s.io/klog/v2"
	kubeletdeviceplugin "k8s.io/kubelet/pkg/apis/deviceplugin/v1beta1"
	ctrlcli "sigs.k8s.io/controller-runtime/pkg/client"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	"gpustack.ai/gpustack/pkg/device"
	"gpustack.ai/gpustack/pkg/devicemanager/mockspec"
	"gpustack.ai/gpustack/pkg/deviceplugin"
	"gpustack.ai/gpustack/pkg/nodefeature"
	"gpustack.ai/gpustack/pkg/utils/gox"
	"gpustack.ai/gpustack/pkg/utils/osx"
	"gpustack.ai/gpustack/pkg/utils/stringx"
)

// RDMAFamilyName is the resource family name served by RDMAAllocator.
const RDMAFamilyName = "rdma"

const (
	envNCCLIbHCA          = "NCCL_IB_HCA"
	envGPUStackRDMADevs   = "GPUSTACK_RDMA_DEVICES"
	mockSocketProbePeriod = time.Second
	mockRegisterTimeout   = 15 * time.Second
)

var _ device.Allocator = (*RDMAAllocator)(nil)

// RDMAAllocator implements device.Allocator for mock RDMA resources in CPU-only mode.
type RDMAAllocator struct {
	logger     klog.Logger
	kubeSocket string
	modes      []workercore.DeviceAllocationMode
	servers    []deviceplugin.Server
	lifecycle  gox.Lifecycle
}

// NewRDMAAllocator constructs a CPU-only mock RDMA allocator serving enabled RDMA allocation modes.
func NewRDMAAllocator(opts device.AllocatorOptions, mockOpts ...Option) *RDMAAllocator {
	cfg := resolveOptions(mockOpts)
	logger := opts.Logger.WithName("mock").WithName(RDMAFamilyName)

	candidateModes := []workercore.DeviceAllocationMode{
		workercore.DeviceAllocationModeExclusive,
	}
	if !opts.NoShared {
		candidateModes = append(candidateModes, workercore.DeviceAllocationModeShared)
	}
	if !opts.NoPartitioned {
		candidateModes = append(candidateModes, workercore.DeviceAllocationModePartitioned)
	}

	modes := make([]workercore.DeviceAllocationMode, 0, len(candidateModes))
	servers := make([]deviceplugin.Server, 0, len(candidateModes))
	for _, mode := range candidateModes {
		srv, err := NewRDMAServer(logger, mode, cfg.reconciler, mockOpts...)
		if err != nil {
			logger.Error(err, "skipping mock RDMA server for mode", "mode", mode.String())
			continue
		}
		modes = append(modes, mode)
		servers = append(servers, srv)
	}

	return &RDMAAllocator{
		logger:     logger,
		kubeSocket: opts.KubeSocket,
		modes:      modes,
		servers:    servers,
	}
}

// RDMAAllocatorCreator returns a creator function matching rdmaAllocatorCreator in
// pkg/devicemanager/allocator.
func RDMAAllocatorCreator(mockOpts ...Option) func(device.AllocatorOptions) device.Allocator {
	copied := slices.Clone(mockOpts)
	return func(opts device.AllocatorOptions) device.Allocator {
		return NewRDMAAllocator(opts, copied...)
	}
}

// Name returns RDMAFamilyName ("rdma").
func (*RDMAAllocator) Name() string {
	return RDMAFamilyName
}

// Modes returns a copy of the allocation modes served by this RDMAAllocator.
func (a *RDMAAllocator) Modes() []workercore.DeviceAllocationMode {
	return slices.Clone(a.modes)
}

// Servers returns a copy of the underlying RDMA device-plugin servers.
func (a *RDMAAllocator) Servers() []deviceplugin.Server {
	return slices.Clone(a.servers)
}

// Start runs all registered mock RDMA servers against the configured kubelet socket.
func (a *RDMAAllocator) Start(ctx context.Context) error {
	a.logger.Info("starting mock rdma allocator")

	tasks := make([]func(context.Context) error, 0, len(a.servers))
	for i := range a.servers {
		srv := a.servers[i]
		tasks = append(tasks, func(ctx context.Context) error {
			return srv.Start(ctx, a.kubeSocket)
		})
	}
	return a.lifecycle.Start(ctx, tasks...)
}

// Stop terminates all running mock RDMA servers cleanly.
func (a *RDMAAllocator) Stop() {
	a.logger.Info("stopping mock rdma allocator")
	a.lifecycle.Stop()
}

type mockRDMAServer struct {
	kubeletdeviceplugin.UnimplementedDevicePluginServer

	logger       klog.Logger
	mode         workercore.DeviceAllocationMode
	resourceName core.ResourceName
	reconciler   *deviceplugin.DevicesReconciler
	specStore    *mockspec.Store
	nodeName     string
	basePlugin   kubeletdeviceplugin.DevicePluginServer

	mu         sync.Mutex
	stop       context.CancelFunc
	done       chan struct{}
	grpcServer *grpc.Server
	registered bool
}

// NewRDMAServer constructs one CPU-only mock RDMA device-plugin server for mode.
// It delegates GetDevicePluginOptions, ListAndWatch, and GetPreferredAllocation to
// deviceplugin.NewRDMAServer while answering Allocate purely from the in-cluster Devices
// and mock RDMA inventory without opening /sys/class/infiniband or mounting /dev/infiniband/*.
func NewRDMAServer(
	logger klog.Logger,
	mode workercore.DeviceAllocationMode,
	reconciler *deviceplugin.DevicesReconciler,
	mockOpts ...Option,
) (deviceplugin.Server, error) {
	base, err := deviceplugin.NewRDMAServer(logger, mode, reconciler)
	if err != nil {
		return nil, err
	}
	basePlugin, ok := base.(kubeletdeviceplugin.DevicePluginServer)
	if !ok {
		return nil, errors.New("rdma server does not implement DevicePluginServer")
	}

	var cfg options
	for _, opt := range mockOpts {
		if opt != nil {
			opt(&cfg)
		}
	}

	return &mockRDMAServer{
		logger:       logger.WithName(strings.ToLower(mode.String())),
		mode:         mode,
		resourceName: nodefeature.GetRDMAResourceName(mode),
		reconciler:   reconciler,
		specStore:    cfg.specStore,
		nodeName:     cfg.nodeName,
		basePlugin:   basePlugin,
	}, nil
}

func (s *mockRDMAServer) GetDevicePluginOptions(
	ctx context.Context, empty *kubeletdeviceplugin.Empty,
) (*kubeletdeviceplugin.DevicePluginOptions, error) {
	return s.basePlugin.GetDevicePluginOptions(ctx, empty)
}

func (s *mockRDMAServer) ListAndWatch(
	empty *kubeletdeviceplugin.Empty,
	srv grpc.ServerStreamingServer[kubeletdeviceplugin.ListAndWatchResponse],
) error {
	return s.basePlugin.ListAndWatch(empty, srv)
}

func (s *mockRDMAServer) GetPreferredAllocation(
	ctx context.Context, req *kubeletdeviceplugin.PreferredAllocationRequest,
) (*kubeletdeviceplugin.PreferredAllocationResponse, error) {
	return s.basePlugin.GetPreferredAllocation(ctx, req)
}

// Allocate resolves requested RDMA tokens against Devices.spec.interfaces and any netdev-free
// mock RDMA devices, returning NCCL_IB_HCA, GPUSTACK_RDMA_DEVICES, and GPUSTACK_MOCK_* env vars
// with zero /dev/* device nodes and zero host mounts.
func (s *mockRDMAServer) Allocate(
	ctx context.Context, req *kubeletdeviceplugin.AllocateRequest,
) (*kubeletdeviceplugin.AllocateResponse, error) {
	ctrRequests := req.GetContainerRequests()
	if len(ctrRequests) == 0 {
		return nil, grpcstatus.Error(
			grpccodes.InvalidArgument,
			"the allocate request carries no container requests",
		)
	}

	interfaces, externalHCAs, err := s.captureInterfaces(ctx)
	if err != nil {
		return nil, err
	}

	responses := make([]*kubeletdeviceplugin.ContainerAllocateResponse, 0, len(ctrRequests))
	for _, ctrReq := range ctrRequests {
		deviceIDs := slices.Clone(ctrReq.GetDevicesIds())
		sort.Strings(deviceIDs)
		ctrResp, err := s.allocateContainer(interfaces, externalHCAs, deviceIDs)
		if err != nil {
			return nil, err
		}
		responses = append(responses, ctrResp)
	}
	return &kubeletdeviceplugin.AllocateResponse{ContainerResponses: responses}, nil
}

func (s *mockRDMAServer) allocateContainer(
	interfaces []workercore.DeviceInterface,
	externalHCAs map[string]struct{},
	deviceIDs []string,
) (*kubeletdeviceplugin.ContainerAllocateResponse, error) {
	if len(deviceIDs) == 0 {
		return nil, grpcstatus.Error(grpccodes.InvalidArgument, "the request grants no RDMA device")
	}

	seenEndpoints := make(map[deviceplugin.Resource]struct{}, len(deviceIDs))
	seenHCAs := make(map[string]struct{}, len(deviceIDs))
	grants := make([]string, 0, len(deviceIDs))

	for _, rawID := range deviceIDs {
		tok, err := deviceplugin.ParseResourceToken(rawID)
		if err != nil {
			return nil, grpcstatus.Errorf(grpccodes.InvalidArgument, "invalid device id %q: %v", rawID, err)
		}
		if _, dup := seenEndpoints[tok.Resource]; dup {
			continue
		}
		hca, ok := matchRDMAEndpoint(interfaces, s.mode, tok.Resource)
		if !ok {
			return nil, grpcstatus.Errorf(
				grpccodes.InvalidArgument,
				"device id %q names no RDMA endpoint of the %s family",
				rawID, s.mode,
			)
		}
		seenEndpoints[tok.Resource] = struct{}{}
		if _, external := externalHCAs[hca]; external {
			continue
		}
		if _, dupHCA := seenHCAs[hca]; dupHCA {
			continue
		}
		seenHCAs[hca] = struct{}{}
		grants = append(grants, hca)
	}
	sort.Strings(grants)

	names := strings.Join(grants, ",")
	nccl := ""
	if names != "" {
		nccl = "=" + names
	}

	return &kubeletdeviceplugin.ContainerAllocateResponse{
		Envs: map[string]string{
			envNCCLIbHCA:            nccl,
			envGPUStackRDMADevs:     names,
			EnvMockAllocatedDevices: names,
			EnvMockAllocationMode:   device.PreflightModeOf(s.mode),
			EnvMockManufacturer:     RDMAFamilyName,
		},
	}, nil
}

func (s *mockRDMAServer) captureInterfaces(
	ctx context.Context,
) ([]workercore.DeviceInterface, map[string]struct{}, error) {
	var interfaces []workercore.DeviceInterface
	if s.reconciler != nil && s.reconciler.Client != nil {
		devs := &workercore.Devices{
			ObjectMeta: meta.ObjectMeta{Name: s.reconciler.NodeName},
		}
		err := s.reconciler.Client.Get(ctx, ctrlcli.ObjectKeyFromObject(devs), devs, &ctrlcli.GetOptions{
			Raw: &meta.GetOptions{ResourceVersion: "0"},
		})
		if err != nil {
			return nil, nil, fmt.Errorf("read devices inventory: %w", err)
		}
		interfaces = make([]workercore.DeviceInterface, len(devs.Spec.Interfaces))
		for i := range devs.Spec.Interfaces {
			interfaces[i] = *devs.Spec.Interfaces[i].DeepCopy()
		}
	}

	var rdmaDevs []device.RDMADevice
	if s.reconciler != nil && s.reconciler.RDMAInterfaceInventory != nil {
		if snap := s.reconciler.RDMAInterfaceInventory(); snap != nil && snap.Complete {
			rdmaDevs = snap.Devices
		}
	} else if s.specStore != nil {
		if snap := s.specStore.Current(); snap != nil && snap.Document != nil {
			nodeName := s.nodeName
			if nodeName == "" && s.reconciler != nil {
				nodeName = s.reconciler.NodeName
			}
			resolved := snap.Document.ResolveNode(nodeName)
			rdmaDevs = resolved.RDMADevices
		}
	}

	externalHCAs := augmentRDMAInterfaces(&interfaces, rdmaDevs)
	return interfaces, externalHCAs, nil
}

func augmentRDMAInterfaces(
	interfaces *[]workercore.DeviceInterface, rdmaDevs []device.RDMADevice,
) map[string]struct{} {
	if len(rdmaDevs) == 0 {
		return nil
	}
	ifaces := *interfaces
	var externalHCAs map[string]struct{}

	boundHCAs := make(map[string]struct{}, len(ifaces))
	pfByPCI := make(map[string]int, len(ifaces))
	for i := range ifaces {
		iface := &ifaces[i]
		if iface.RDMADevice != "" {
			boundHCAs[iface.RDMADevice] = struct{}{}
		}
		if iface.PciBusID != "" {
			pfByPCI[iface.PciBusID] = i
		}
		for j := range iface.VirtualFunctions {
			if vf := iface.VirtualFunctions[j].RDMADevice; vf != "" {
				boundHCAs[vf] = struct{}{}
			}
		}
	}

	for i := range rdmaDevs {
		dev := &rdmaDevs[i]
		if dev.Name == "" {
			continue
		}
		if dev.Owner == device.RDMADeviceOwnerExternal {
			if externalHCAs == nil {
				externalHCAs = make(map[string]struct{})
			}
			externalHCAs[dev.Name] = struct{}{}
			continue
		}
		if dev.VirtualFunction {
			continue
		}
		if _, bound := boundHCAs[dev.Name]; bound {
			continue
		}
		ifaces = append(ifaces, workercore.DeviceInterface{
			Name:         dev.Name,
			PciBusID:     dev.PciBusID,
			NumaAffinity: dev.NumaAffinity,
			CpuAffinity:  dev.CpuAffinity,
			RDMA:         true,
			RDMADevice:   dev.Name,
		})
		boundHCAs[dev.Name] = struct{}{}
		if dev.PciBusID != "" {
			if _, exists := pfByPCI[dev.PciBusID]; !exists {
				pfByPCI[dev.PciBusID] = len(ifaces) - 1
			}
		}
	}

	for i := range rdmaDevs {
		dev := &rdmaDevs[i]
		if dev.Name == "" || dev.Owner == device.RDMADeviceOwnerExternal || !dev.VirtualFunction {
			continue
		}
		if _, bound := boundHCAs[dev.Name]; bound {
			continue
		}
		vf := workercore.DeviceInterfaceVirtualFunction{
			Name:         dev.Name,
			PciBusID:     dev.PciBusID,
			NumaAffinity: dev.NumaAffinity,
			CpuAffinity:  dev.CpuAffinity,
			RDMA:         true,
			RDMADevice:   dev.Name,
		}
		if dev.ParentPciBusID != "" {
			if pfIdx, ok := pfByPCI[dev.ParentPciBusID]; ok {
				ifaces[pfIdx].SRIOV = true
				ifaces[pfIdx].VirtualFunctions = append(ifaces[pfIdx].VirtualFunctions, vf)
				boundHCAs[dev.Name] = struct{}{}
				continue
			}
		}
		parentName := dev.ParentPciBusID
		if parentName == "" {
			parentName = dev.Name
		}
		ifaces = append(ifaces, workercore.DeviceInterface{
			Name:             parentName,
			PciBusID:         dev.ParentPciBusID,
			NumaAffinity:     dev.NumaAffinity,
			SRIOV:            true,
			RDMA:             true,
			VirtualFunctions: []workercore.DeviceInterfaceVirtualFunction{vf},
		})
		if dev.ParentPciBusID != "" {
			pfByPCI[dev.ParentPciBusID] = len(ifaces) - 1
		}
		boundHCAs[dev.Name] = struct{}{}
	}

	*interfaces = ifaces
	return externalHCAs
}

func matchRDMAEndpoint(
	interfaces []workercore.DeviceInterface,
	mode workercore.DeviceAllocationMode,
	target deviceplugin.Resource,
) (string, bool) {
	for i := range interfaces {
		iface := &interfaces[i]
		partitioned := iface.SRIOV && len(iface.VirtualFunctions) > 0
		switch mode {
		case workercore.DeviceAllocationModePartitioned:
			if !partitioned || iface.Name != target.Group {
				continue
			}
			for j := range iface.VirtualFunctions {
				vf := &iface.VirtualFunctions[j]
				if vf.RDMADevice == "" {
					continue
				}
				vfName := vf.Name
				if vfName == "" {
					vfName = vf.RDMADevice
				}
				if vfName == target.Device {
					return vf.RDMADevice, true
				}
			}
		case workercore.DeviceAllocationModeExclusive, workercore.DeviceAllocationModeShared:
			if partitioned || iface.RDMADevice == "" {
				continue
			}
			if target == (deviceplugin.Resource{Group: iface.Name, Device: iface.Name}) {
				return iface.RDMADevice, true
			}
		}
	}
	return "", false
}

func (s *mockRDMAServer) socketName() string {
	return stringx.Join(".", RDMAFamilyName, strings.ToLower(s.mode.String()), "sock")
}

func (s *mockRDMAServer) Start(ctx context.Context, kubeSocket string) error {
	caller := ctx
	ctx, stop := context.WithCancel(caller)
	defer stop()

	if !s.begin(stop) {
		<-ctx.Done()
		return ctx.Err()
	}
	defer s.end()

	socket := filepath.Join(filepath.Dir(kubeSocket), s.socketName())
	for {
		if err := s.serveGeneration(ctx, kubeSocket, socket); err != nil {
			return err
		}
		if ctx.Err() != nil {
			return caller.Err()
		}
	}
}

func (s *mockRDMAServer) serveGeneration(ctx context.Context, kubeSocket, socket string) error {
	if err := os.Remove(socket); err != nil && !os.IsNotExist(err) {
		return err
	}
	lis, err := net.Listen("unix", socket)
	if err != nil {
		return err
	}
	if ul, ok := lis.(*net.UnixListener); ok {
		ul.SetUnlinkOnClose(false)
	}

	srv := grpc.NewServer(grpc.Creds(insecure.NewCredentials()))
	kubeletdeviceplugin.RegisterDevicePluginServer(srv, s)
	s.publish(srv)
	defer s.retire(srv)

	served := make(chan struct{})
	gox.Go(func() {
		defer close(served)
		if serveErr := srv.Serve(lis); serveErr != nil && !errors.Is(serveErr, grpc.ErrServerStopped) {
			s.logger.Error(serveErr, "serve mock rdma socket", "socket", socket)
		}
	})

	tick := time.NewTicker(mockSocketProbePeriod)
	defer tick.Stop()
	for {
		if ctx.Err() != nil {
			return nil
		}
		if !s.isRegistered() {
			if regErr := s.register(ctx, srv, kubeSocket, socket); regErr != nil {
				s.logger.Error(regErr, "register mock rdma to kubelet, retrying", "socket", kubeSocket)
			}
		}
		select {
		case <-ctx.Done():
			return nil
		case <-served:
			return nil
		case <-tick.C:
		}
		if _, statErr := os.Stat(socket); statErr != nil {
			return nil
		}
	}
}

func (s *mockRDMAServer) register(ctx context.Context, srv *grpc.Server, kubeSocket, socket string) error {
	regOpts, err := s.GetDevicePluginOptions(ctx, &kubeletdeviceplugin.Empty{})
	if err != nil {
		return fmt.Errorf("getting device plugin options: %w", err)
	}

	cli, err := grpc.NewClient("unix://"+kubeSocket, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return fmt.Errorf("dialing kubelet socket: %w", err)
	}
	defer osx.Close(cli)

	regCli := kubeletdeviceplugin.NewRegistrationClient(cli)
	regCtx, cancel := context.WithTimeout(ctx, mockRegisterTimeout)
	defer cancel()
	_, err = regCli.Register(regCtx, &kubeletdeviceplugin.RegisterRequest{
		Version:      kubeletdeviceplugin.Version,
		Endpoint:     filepath.Base(socket),
		ResourceName: string(s.resourceName),
		Options:      regOpts,
	})
	if err != nil {
		return fmt.Errorf("registering %s: %w", s.resourceName, err)
	}

	s.mu.Lock()
	if s.grpcServer == srv {
		s.registered = true
	}
	s.mu.Unlock()
	return nil
}

func (s *mockRDMAServer) Stop() {
	s.mu.Lock()
	stop, srv, done := s.stop, s.grpcServer, s.done
	s.mu.Unlock()
	if stop == nil {
		return
	}
	stop()
	s.retire(srv)
	<-done
}

func (s *mockRDMAServer) begin(stop context.CancelFunc) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.stop != nil {
		return false
	}
	s.stop, s.done = stop, make(chan struct{})
	return true
}

func (s *mockRDMAServer) end() {
	s.mu.Lock()
	done := s.done
	s.stop, s.done = nil, nil
	s.mu.Unlock()
	close(done)
}

func (s *mockRDMAServer) publish(srv *grpc.Server) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.grpcServer, s.registered = srv, false
}

func (s *mockRDMAServer) retire(srv *grpc.Server) {
	s.mu.Lock()
	if srv == nil || s.grpcServer != srv {
		s.mu.Unlock()
		return
	}
	s.grpcServer, s.registered = nil, false
	s.mu.Unlock()
	srv.Stop()
}

func (s *mockRDMAServer) isRegistered() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.registered
}
