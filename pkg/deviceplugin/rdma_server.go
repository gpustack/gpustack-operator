package deviceplugin

import (
	"context"
	"fmt"
	"strings"
	"time"

	"google.golang.org/grpc"
	core "k8s.io/api/core/v1"
	klog "k8s.io/klog/v2"
	deviceplugin "k8s.io/kubelet/pkg/apis/deviceplugin/v1beta1"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	"gpustack.ai/gpustack/pkg/nodefeature"
	"gpustack.ai/gpustack/pkg/utils/stringx"
	"gpustack.ai/gpustack/pkg/utils/waitx"
)

// rdmaServer serves one allocation mode's RDMA endpoints over the device-plugin gRPC API, driven
// by the same reconciler inventory and the same serving lifecycle the accelerator servers use. It
// holds no allocation state of its own: every response is recomputed from
// Devices.spec.interfaces[], so the two pooled families draw on the same HCAs without
// decrementing each other, and a fact the detector has since corrected cannot survive into a
// response built from an older one.
type rdmaServer struct {
	deviceplugin.UnimplementedDevicePluginServer

	Logger         klog.Logger
	AllocationMode workercore.DeviceAllocationMode
	ResourceName   core.ResourceName
	Reconciler     *DevicesReconciler

	serving
}

// NewRDMAServer returns the device-plugin server advertising one allocation mode's RDMA endpoints,
// driven by the reconciler's inventory. It refuses any mode with no RDMA resource key of its own —
// a visibility allocation names an endpoint another container of the same Pod holds, and no RDMA
// allocation record is written to answer that from, so the mode is unserved rather than
// half-served — so a caller cannot construct a server that would register an empty resource name
// with kubelet.
func NewRDMAServer(
	logger klog.Logger,
	mode workercore.DeviceAllocationMode,
	reconciler *DevicesReconciler,
) (Server, error) {
	name := nodefeature.GetRDMAResourceName(mode)
	if name == "" {
		return nil, fmt.Errorf("allocation mode %s serves no RDMA resource", mode)
	}
	return &rdmaServer{
		Logger:         logger.WithName(strings.ToLower(mode.String())),
		AllocationMode: mode,
		ResourceName:   name,
		Reconciler:     reconciler,
	}, nil
}

// rdmaFamilyName is the first component of the socket base name and the notifier's manufacturer
// label. It names the RDMA resource family, which no manufacturer is — an accelerator socket's
// first component is always a manufacturer the detector reported — so an RDMA socket cannot
// collide with a vendor server's, and the four RDMA sockets differ among themselves by the mode
// component.
const rdmaFamilyName = "rdma"

// servingOptions names what this server serves: the plugin answering the gRPC calls, which is
// this server itself; the resource it registers; and its socket beside kubelet's, by base name.
func (s *rdmaServer) servingOptions() servingOptions {
	return servingOptions{
		Plugin:       s,
		ResourceName: s.ResourceName,
		SocketName:   stringx.Join(".", rdmaFamilyName, strings.ToLower(s.AllocationMode.String()), "sock"),
	}
}

// Start serves the plugin, socket and resource opts names to kubelet for as long as ctx allows.
func (s *rdmaServer) Start(ctx context.Context, kubeSocket string) error {
	return s.serving.Start(ctx, kubeSocket, s.servingOptions(), s.Logger)
}

// Stop stops the gRPC server and ends the serving loop.
func (s *rdmaServer) Stop() {
	s.serving.Stop(s.Logger)
}

// GetDevicePluginOptions returns options to be communicated with the Device Manager. No
// preferred allocation is offered: this server expresses no preference among its tokens, so
// kubelet picks freely.
func (s *rdmaServer) GetDevicePluginOptions(context.Context, *Empty) (*Options, error) {
	return &Options{}, nil
}

// ListAndWatch returns a stream of List of Devices.
// Whenever a Device state change or a Device disappears, ListAndWatch returns the new list.
func (s *rdmaServer) ListAndWatch(_ *Empty, srv grpc.ServerStreamingServer[ListAndWatchResponse]) error {
	// Subscribed at the beginning of ListAndWatch and given up on the way out, as the accelerator
	// servers do: kubelet opens a fresh stream on every registration, and a subscription that
	// outlived its stream would accumulate one per kubelet restart for the life of the process.
	notifier, release := s.Reconciler.getReconcileNotifier(rdmaFamilyName, s.AllocationMode)
	defer release()

	ctx := srv.Context()

	s.Logger.Info("sending initial list and watch response")
	if err := waitx.PollUntilContextCancel(ctx, 2*time.Second, true, func(ctx context.Context) error {
		resp, err := s.getListAndWatchResponse(ctx)
		if err != nil {
			// Returned rather than swallowed. This poll ends on a nil return, so logging and
			// falling through would end it as a success having sent nothing, and the stream
			// would go on to the watch loop with kubelet holding no device list at all: the
			// resource reads as zero until some later reconcile happens to fire the notifier.
			s.Logger.Error(err, "get initial list and watch response, retry later")
			return err
		}
		if err = srv.Send(resp); err != nil {
			// Also retried, for the same reason. A stream that will not take the first
			// response is one kubelet has stopped reading, and this poll ends when the
			// stream's context does.
			s.Logger.Error(err, "send initial list and watch response, retry later")
			return err
		}
		return nil
	}); err != nil {
		s.Logger.Error(err, "initial list and watch")
		return err
	}

	s.Logger.Info("watching for device updates")
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-notifier:
			resp, err := s.getListAndWatchResponse(ctx)
			if err != nil {
				s.Logger.Error(err, "get list and watch response on update")
				return err
			}
			if err := srv.Send(resp); err != nil {
				s.Logger.Error(err, "send list and watch response")
				return err
			}
			s.Logger.Info("sent list and watch response")
		}
	}
}

// getListAndWatchResponse builds this server's advertisement from the node's interface inventory.
// It is a pure function of that inventory — the reconciler is read for the object and for nothing
// else — so allocation state anywhere in the process cannot change a response, which is what lets
// the two pooled families serve the same HCAs without interfering with each other.
func (s *rdmaServer) getListAndWatchResponse(ctx context.Context) (*ListAndWatchResponse, error) {
	devs, err := s.Reconciler.getDevices(ctx)
	if err != nil {
		return nil, err
	}

	resp := &ListAndWatchResponse{}
	for i := range devs.Spec.Interfaces {
		iface := &devs.Spec.Interfaces[i]
		for _, ep := range rdmaEndpoints(iface, s.AllocationMode) {
			// A link verdict withholds an endpoint's tokens from new Pods by marking them, never
			// by withdrawing them: removing an advertised ID strands the kubelet's checkpointed
			// allocation for whatever container holds it. No verdict at all is not a verdict of
			// failure — an endpoint reached here carries a bound RDMA device, so a missing link
			// record is the unverified case arriving by a different route.
			health := deviceplugin.Healthy
			if ep.Link != nil && ep.Link.State == workercore.DeviceInterfaceLinkStateFailed {
				health = deviceplugin.Unhealthy
			}
			for _, id := range ep.Resource.DeviceIDs(s.AllocationMode, rdmaPoolSizeOf(s.AllocationMode)) {
				// The partition family carries a NUMA hint too, where the accelerator partition
				// pool carries none: an RDMA partition token names exactly one virtual function,
				// whose affinity a hint can honor, while an accelerator partition token names no
				// accelerator at all.
				resp.Devices = append(resp.Devices, &deviceplugin.Device{
					ID:       id,
					Health:   health,
					Topology: ep.Topology,
				})
			}
		}
	}
	return resp, nil
}

// rdmaPoolSizeOf returns the token count one endpoint advertises in the mode: the shared
// concurrency ceiling for the pooled families, and one token for the exclusive ones. The two
// pooled families publish the same ceiling over the same HCAs — several processes on one HCA is
// ordinary use, with the isolation done by firmware and kernel, so the token count is a
// scheduling knob rather than a hardware limit.
func rdmaPoolSizeOf(mode workercore.DeviceAllocationMode) int32 {
	switch mode {
	case workercore.DeviceAllocationModeShared, workercore.DeviceAllocationModeSliced:
		return int32(nodefeature.SharedResourceMaxSize)
	default:
		return 1
	}
}
