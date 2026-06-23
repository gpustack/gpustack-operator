package deviceplugin

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"google.golang.org/grpc"
	grpccodes "google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	grpcstatus "google.golang.org/grpc/status"
	core "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	klog "k8s.io/klog/v2"
	deviceplugin "k8s.io/kubelet/pkg/apis/deviceplugin/v1beta1"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	"gpustack.ai/gpustack/binding"
	"gpustack.ai/gpustack/pkg/kubemeta"
	"gpustack.ai/gpustack/pkg/nodefeature"
	"gpustack.ai/gpustack/pkg/utils/gox"
	"gpustack.ai/gpustack/pkg/utils/osx"
	"gpustack.ai/gpustack/pkg/utils/slicex"
	"gpustack.ai/gpustack/pkg/utils/stringx"
	"gpustack.ai/gpustack/pkg/utils/waitx"
)

type ResourceServer struct {
	deviceplugin.UnimplementedDevicePluginServer

	Logger         klog.Logger
	Manufacturer   string
	AllocationMode workercore.DeviceAllocationMode
	Reconciler     *DevicesReconciler
	Responder      ContainerAllocateResponder

	server *grpc.Server
}

// GetResourceName returns the resource name to be registered to the Device Manager based on the kind and name.
func (s *ResourceServer) GetResourceName() core.ResourceName {
	// For sliced this is the bare ".sliced" injection-token key; the ".sliced.units"
	// counting key is reported separately via Patch Node, not the device-plugin.
	return nodefeature.GetAcceleratableResourceName(s.Manufacturer, s.AllocationMode)
}

// GetDevicePluginOptions returns options to be communicated with the Device Manager.
func (s *ResourceServer) GetDevicePluginOptions(context.Context, *Empty) (*Options, error) {
	opts := &Options{
		GetPreferredAllocationAvailable: true,
	}
	return opts, nil
}

// ListAndWatch returns a stream of List of Devices.
// Whenever a Device state change or a Device disappears, ListAndWatch returns the new list.
func (s *ResourceServer) ListAndWatch(_ *Empty, srv grpc.ServerStreamingServer[ListAndWatchResponse]) error {
	// Get notifier at the beginning of ListAndWatch to avoid missing any update during the initial ListAndWatch.
	notifier := s.Reconciler.getReconcileNotifier(s.Manufacturer, s.AllocationMode)

	ctx := srv.Context()

	// Send the initial ListAndWatch response.
	err := waitx.PollUntilContextCancel(ctx, 2*time.Second, true, func(ctx context.Context) error {
		resp, err := s.getListAndWatchResponse(ctx)
		if err != nil {
			// Nothing to do, keep looping until success or context cancellation.
			s.Logger.Error(err, "get initial list and watch response, retry later")
		} else if err = srv.Send(resp); err != nil {
			// Return error to restart Device Plugin Server.
			return err
		}
		return nil
	})
	if err != nil {
		s.Logger.Error(err, "initial list and watch")
		return err
	}

	// Watch for updates and send ListAndWatch response whenever there's a change.
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-notifier:
			resp, err := s.getListAndWatchResponse(ctx)
			if err != nil {
				s.Logger.Error(err, "get list and watch response")
				return err
			}
			if err = srv.Send(resp); err != nil {
				s.Logger.Error(err, "send list and watch response")
				return err
			}
		}
	}
}

func (s *ResourceServer) getListAndWatchResponse(ctx context.Context) (*ListAndWatchResponse, error) {
	devs, err := s.Reconciler.getDevices(ctx)
	if err != nil {
		return nil, err
	}

	resp := &deviceplugin.ListAndWatchResponse{}
	for i := range devs.Spec.Groups {
		devGroup := &devs.Spec.Groups[i]
		if devGroup.Manufacturer != s.Manufacturer {
			continue
		}
		for j := range devGroup.Accelerators {
			devAccelerator := &devGroup.Accelerators[j]
			res := Resource{
				Group:  devGroup.ID,
				Device: devAccelerator.ID,
			}
			health := deviceplugin.Healthy
			if devAccelerator.Status.Unhealthy {
				health = deviceplugin.Unhealthy
			}
			var topology *deviceplugin.TopologyInfo
			if numa := binding.StrRangeToList(devAccelerator.Topology.NumaAffinity); len(numa) > 0 {
				topology = &deviceplugin.TopologyInfo{
					Nodes: slicex.Transform(numa, func(n int) *deviceplugin.NUMANode {
						return &deviceplugin.NUMANode{
							ID: int64(n),
						}
					}),
				}
			}
			ids := res.GetDeviceIds(s.AllocationMode, devAccelerator.Features.MaxPartitions)
			for k := range ids {
				resp.Devices = append(resp.Devices,
					&deviceplugin.Device{
						ID:       ids[k],
						Health:   health,
						Topology: topology,
					},
				)
			}
		}
	}
	return resp, nil
}

// GetPreferredAllocation returns a preferred set of devices to allocate from a list of available ones.
// The resulting preferred allocation is not guaranteed to be the allocation ultimately performed by the Device Manager.
// It is only designed to help the Device Manager make a more informed allocation decision when possible.
func (s *ResourceServer) GetPreferredAllocation(ctx context.Context, req *PreferredAllocationRequest) (*PreferredAllocationResponse, error) {
	ctrReq := req.GetContainerRequests()[0]

	resName := s.GetResourceName()
	resQuantity := *resource.NewQuantity(int64(ctrReq.GetAllocationSize()), resource.DecimalSI)
	pod, err := s.Reconciler.getAllocatingPodWithRetry(ctx, resName, resQuantity)
	if err != nil {
		s.Logger.Error(err, "get allocating pod for preferred allocation")
		return nil, grpcstatus.Errorf(grpccodes.Internal, "get allocating pod for preferred allocation: %v", err)
	}

	devs, err := s.Reconciler.getDevices(ctx)
	if err != nil {
		s.Logger.Error(err, "get devices for preferred allocation")
		return nil, grpcstatus.Errorf(grpccodes.Internal, "get devices for preferred allocation: %v", err)
	}

	ctrResp, err := s.getContainerPreferredAllocationResponse(ctrReq, pod, devs)
	if err != nil {
		s.Logger.Error(err, "get container preferred allocation response")
		return nil, grpcstatus.Errorf(grpccodes.Internal, "get container preferred allocation response: %v", err)
	}

	resp := &PreferredAllocationResponse{
		ContainerResponses: []*ContainerPreferredAllocationResponse{ctrResp},
	}
	s.Logger.Info("get preferred allocation response", "pod", kubemeta.GetNamespacedNameKey(pod), "response", resp)
	return resp, nil
}

func (s *ResourceServer) getContainerPreferredAllocationResponse(
	ctrReq *ContainerPreferredAllocationRequest,
	pod *core.Pod,
	devs *workercore.Devices,
) (*ContainerPreferredAllocationResponse, error) {
	availableDeviceIDs := ctrReq.GetAvailableDeviceIDs()
	sort.Strings(availableDeviceIDs)
	availableResUnitsMap := make(map[Resource][]ResourceUnit)
	for i := range availableDeviceIDs {
		resUnit, err := ConvertResourceUnitFromDeviceIds(availableDeviceIDs[i])
		if err != nil {
			return nil, fmt.Errorf("convert available device id %q: %w", availableDeviceIDs[i], err)
		}
		availableResUnitsMap[resUnit.Resource] = append(availableResUnitsMap[resUnit.Resource], resUnit)
	}

	mustIncludedDeviceIDs := ctrReq.GetMustIncludeDeviceIDs()
	sort.Strings(mustIncludedDeviceIDs)
	mustIncludedResUnitsMap := make(map[Resource][]ResourceUnit)
	for i := range mustIncludedDeviceIDs {
		resUnit, err := ConvertResourceUnitFromDeviceIds(mustIncludedDeviceIDs[i])
		if err != nil {
			return nil, fmt.Errorf("convert must include device id %q: %w", mustIncludedDeviceIDs[i], err)
		}
		mustIncludedResUnitsMap[resUnit.Resource] = append(mustIncludedResUnitsMap[resUnit.Resource], resUnit)
	}

	allocationSize := ctrReq.GetAllocationSize()
	preferredDeviceIDsSet := extractPreferredAcceleratorIDsFromPod(pod, devs)
	remainingSize := allocationSize

	selectedResUnits := make([]ResourceUnit, 0, allocationSize)
	var unselectedResUnits []ResourceUnit // Only used if provided preferred device IDs.
	for i := range devs.Spec.Groups {
		devsGroup := &devs.Spec.Groups[i]
		if devsGroup.Manufacturer != s.Manufacturer {
			continue
		}
		for j := range devsGroup.Accelerators {
			devsAccelerator := &devsGroup.Accelerators[j]
			res := Resource{
				Group:  devsGroup.ID,
				Device: devsAccelerator.ID,
			}

			// Skip the resource is not in the available list.
			resUnits, existed := availableResUnitsMap[res]
			if !existed {
				continue
			}
			// Skip the resource is occupied by other modes.
			mode := workercore.DeviceAllocationModeNone
			if len(devs.Status.Groups) > i && len(devs.Status.Groups[i].Accelerators) > j {
				mode = devs.Status.Groups[i].Accelerators[j].Mode
			}
			if mode != workercore.DeviceAllocationModeNone && mode != s.AllocationMode {
				continue
			}

			// Exclusive, shared and sliced all select one device unit (token) per
			// card; the per-card concurrency/units accounting lives elsewhere (Kueue
			// credits and the ".sliced.units" capacity), not in the device plugin.
			if miResUnits, existed := mustIncludedResUnitsMap[res]; existed {
				// Only the first must-include unit per card is consumed (one token).
				preferredDeviceIDsSet.Delete(res.Device)
				selectedResUnits = append(selectedResUnits, miResUnits[0])
			} else {
				if preferredDeviceIDsSet.Len() != 0 && !preferredDeviceIDsSet.Has(res.Device) {
					unselectedResUnits = append(unselectedResUnits, resUnits[0])
					continue
				}
				preferredDeviceIDsSet.Delete(res.Device)
				selectedResUnits = append(selectedResUnits, resUnits[0])
			}
			remainingSize -= 1
			if preferredDeviceIDsSet.Len() == 0 && remainingSize <= 0 {
				goto outside
			}
		}
	}
outside:

	if preferredDeviceIDsSet.Len() > 0 {
		s.Logger.Error(nil, "not enough preferred devices: %v", preferredDeviceIDsSet.UnsortedList())
		if len(unselectedResUnits) == 0 {
			return &ContainerPreferredAllocationResponse{}, nil
		}
		if remainingSize <= int32(len(unselectedResUnits)) {
			s.Logger.Info("try to allocate from unselected devices since preferred devices are not enough")
			selectedResUnits = append(selectedResUnits, unselectedResUnits[:remainingSize]...)
			remainingSize = 0
		}
	}

	if remainingSize > 0 {
		s.Logger.Error(nil, "not enough devices: required %d, but only %d available", allocationSize, allocationSize-remainingSize)
		return &ContainerPreferredAllocationResponse{}, nil
	}

	deviceIDs := make([]string, 0, len(selectedResUnits))
	for i := range selectedResUnits {
		deviceIDs = append(deviceIDs, selectedResUnits[i].String())
	}

	resp := &ContainerPreferredAllocationResponse{
		DeviceIDs: deviceIDs,
	}
	return resp, nil
}

// Allocate is called during container creation so that
// the Device Plugin can run device specific operations and instruct Kubelet of the steps
// to make the Device available in the container.
func (s *ResourceServer) Allocate(ctx context.Context, req *AllocateRequest) (*AllocateResponse, error) {
	if s.Responder == nil {
		return nil, grpcstatus.Errorf(grpccodes.Internal, "unconfigured responder")
	}

	ctrReq := req.GetContainerRequests()[0]

	allocatedDeviceIDs := ctrReq.GetDevicesIds()
	sort.Strings(allocatedDeviceIDs)
	allocatedResUnitsMap := make(map[Resource][]ResourceUnit)
	for i := range allocatedDeviceIDs {
		resUnit, err := ConvertResourceUnitFromDeviceIds(allocatedDeviceIDs[i])
		if err != nil {
			s.Logger.Error(err, "convert device id", "device id", allocatedDeviceIDs[i])
			return nil, grpcstatus.Errorf(grpccodes.InvalidArgument, "invalid device id %q: %v", allocatedDeviceIDs[i], err)
		}
		allocatedResUnitsMap[resUnit.Resource] = append(allocatedResUnitsMap[resUnit.Resource], resUnit)
	}

	resName := s.GetResourceName()
	resQuantity := *resource.NewQuantity(int64(len(ctrReq.GetDevicesIds())), resource.DecimalSI)
	pod, err := s.Reconciler.getAllocatingPod(ctx, resName, resQuantity)
	if err != nil {
		s.Logger.Error(err, "get allocating pod for allocation")
		return nil, grpcstatus.Errorf(grpccodes.Internal, "get allocating pod for allocation: %v", err)
	}

	devs, err := s.Reconciler.getDevices(ctx)
	if err != nil {
		s.Logger.Error(err, "get devices for allocation")
		return nil, grpcstatus.Errorf(grpccodes.Internal, "get devices for allocation: %v", err)
	}

	var (
		allocatedStatus     workercore.DevicesStatus
		allocatedAllocation = make(map[Resource]int32)
	)
	for i := range devs.Spec.Groups {
		devsGroup := &devs.Spec.Groups[i]
		if devsGroup.Manufacturer != s.Manufacturer {
			continue
		}
		for j := range devsGroup.Accelerators {
			devsAccelerator := &devsGroup.Accelerators[j]
			res := Resource{
				Group:  devsGroup.ID,
				Device: devsAccelerator.ID,
			}
			resUnits, existed := allocatedResUnitsMap[res]
			if !existed {
				continue
			}
			var allocated int32
			switch s.AllocationMode {
			default:
				allocated = nodefeature.ResourceMaxUnits // a whole card
			case workercore.DeviceAllocationModeShared:
				allocated = nodefeature.ResourceMaxUnits / nodefeature.SharedResourceMaxSize // units per owner
			case workercore.DeviceAllocationModeSliced:
				// Bookkeeping only: the loose injection-token count (no isolation).
				allocated = int32(len(resUnits))
			}
			if allocated > nodefeature.ResourceMaxUnits {
				allocated = nodefeature.ResourceMaxUnits
			}
			if len(allocatedStatus.Groups) == 0 || allocatedStatus.Groups[len(allocatedStatus.Groups)-1].ID != devsGroup.ID {
				allocatedStatus.Groups = append(allocatedStatus.Groups, workercore.DevicesAllocationGroup{
					ID:           devsGroup.ID,
					Manufacturer: devsGroup.Manufacturer,
				})
			}
			devsStatusGroup := &allocatedStatus.Groups[len(allocatedStatus.Groups)-1]
			devsStatusGroup.Accelerators = append(devsStatusGroup.Accelerators, workercore.AcceleratorAllocation{
				ID:        devsAccelerator.ID,
				Index:     devsAccelerator.Index,
				Mode:      s.AllocationMode,
				Allocated: allocated,
			})
			allocatedAllocation[res] = allocated
		}
	}

	err = s.Reconciler.patchAllocatingPod(ctx, pod, allocatedStatus)
	if err != nil {
		s.Logger.Error(err, "patch allocating pod for allocation")
		return nil, grpcstatus.Errorf(grpccodes.Internal, "patch allocating pod for allocation: %v", err)
	}

	ctrResp, err := s.Responder.GetContainerAllocateResponse(ctx, pod, devs, allocatedAllocation)
	if err != nil {
		s.Logger.Error(err, "get container allocate response")
		return nil, err
	}

	resp := &AllocateResponse{
		ContainerResponses: []*ContainerAllocateResponse{ctrResp},
	}
	s.Logger.Info("allocate response",
		"pod", kubemeta.GetNamespacedNameKey(pod),
		"response", resp)
	return resp, nil
}

func (s *ResourceServer) Start(ctx context.Context, kubeSocket string) error {
	if s.server != nil {
		s.Logger.Error(nil, "server already started")
		<-ctx.Done()
		return ctx.Err()
	}

	socket := filepath.Join(filepath.Dir(kubeSocket),
		stringx.Join(".", s.Manufacturer, strings.ToLower(s.AllocationMode.String()), "sock"))
	if err := os.Remove(socket); err != nil && !os.IsNotExist(err) {
		s.Logger.Error(err, "clean up socket", "socket", socket)
		return err
	}
	defer func() {
		_ = os.Remove(socket)
	}()

	var lis net.Listener
	{
		var err error
		lis, err = net.Listen("unix", socket)
		if err != nil {
			s.Logger.Error(err, "listen on socket", "socket", socket)
			return err
		}
	}
	defer osx.Close(lis)

	s.server = grpc.NewServer(
		grpc.Creds(insecure.NewCredentials()),
	)
	deviceplugin.RegisterDevicePluginServer(s.server, s)

	gp := gox.GroupWithContextIn(ctx)
	gp.Go(func(ctx context.Context) error {
		s.Logger.Info("serving on socket", "socket", socket)
		return s.server.Serve(lis)
	})
	gp.Go(func(ctx context.Context) error {
		err := waitx.PollUntilContextTimeout(ctx, time.Second, 10*time.Second, true, func(ctx context.Context) error {
			if len(s.server.GetServiceInfo()) == 0 {
				return errors.New("gRPC server is not ready")
			}
			return nil
		})
		if err != nil {
			s.Logger.Error(err, "wait for server to be ready")
			return err
		}
		s.Logger.Info("registering to kubelet")
		return s.register(ctx, kubeSocket, socket)
	})
	return gp.Wait()
}

func (s *ResourceServer) register(ctx context.Context, kubeSocket, socket string) error {
	regOpts, err := s.GetDevicePluginOptions(ctx, &Empty{})
	if err != nil {
		s.Logger.Error(err, "get device plugin options")
		return err
	}

	cli, err := grpc.NewClient("unix://"+kubeSocket,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		s.Logger.Error(err, "dial kubelet socket", "socket", kubeSocket)
		return err
	}
	defer osx.Close(cli)

	regCli := deviceplugin.NewRegistrationClient(cli)
	regReq := &deviceplugin.RegisterRequest{
		Version:      deviceplugin.Version,
		Endpoint:     filepath.Base(socket),
		ResourceName: string(s.GetResourceName()),
		Options:      regOpts,
	}
	if _, err = regCli.Register(ctx, regReq); err != nil {
		s.Logger.Error(err, "register to kubelet", "socket", kubeSocket)
		return err
	}

	return nil
}

func (s *ResourceServer) Stop() {
	if s.server == nil {
		s.Logger.Errorf(nil, "server not started")
		return
	}

	s.Logger.Info("stopping")
	s.server.Stop()
	s.server = nil
}
