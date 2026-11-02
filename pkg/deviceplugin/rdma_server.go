package deviceplugin

import (
	"context"
	"fmt"
	"strings"
	"time"

	"google.golang.org/grpc"
	grpccodes "google.golang.org/grpc/codes"
	grpcstatus "google.golang.org/grpc/status"
	core "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/sets"
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

// GetDevicePluginOptions returns options to be communicated with the Device Manager. Preferred
// allocation is offered: the shared family's tokens are interchangeable to kubelet but each
// resolves to one endpoint, and the round-robin below is where that difference is expressed.
func (s *rdmaServer) GetDevicePluginOptions(context.Context, *Empty) (*Options, error) {
	return &Options{GetPreferredAllocationAvailable: true}, nil
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
	if err := waitx.RetryUntilSuccess(ctx, 2*time.Second, true, func(ctx context.Context) error {
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
//
// The inventory is captured once and the whole advertisement is derived from that one capture, so
// a response can never mix two revisions of the record.
//
// The tokens are emitted round by round: one index across every advertised endpoint in hardware
// order before the next index. The pooled families carry one token per endpoint and come out in
// the order they always had; the shared family's first tokens are then one per endpoint, matching
// the spread GetPreferredAllocation prefers, so kubelet walking the advertisement in order lands
// on distinct HCAs before a second token of any one of them.
func (s *rdmaServer) getListAndWatchResponse(ctx context.Context) (*ListAndWatchResponse, error) {
	devs, err := s.Reconciler.getDevices(ctx)
	if err != nil {
		return nil, err
	}

	// Every advertised endpoint of the family, in hardware order, with the facts its tokens
	// carry. Collecting them ahead of the emission is what keeps the token order an
	// outer-round / inner-endpoint one rather than an endpoint-major one.
	type advertisedEndpoint struct {
		health   string
		ids      []string
		topology *deviceplugin.TopologyInfo
	}
	resp := &ListAndWatchResponse{}
	snap := s.Reconciler.captureRDMAInterfaceSnapshot(devs)
	endpoints := make([]advertisedEndpoint, 0)
	for i := range snap.interfaces {
		iface := &snap.interfaces[i]
		for _, ep := range rdmaEndpoints(iface, s.AllocationMode) {
			// A link verdict or policy exclusion withholds an endpoint's tokens from new Pods by
			// marking them, never by withdrawing them: removing an advertised ID strands the
			// kubelet's checkpointed allocation for whatever container holds it. No verdict at all
			// is not a verdict of failure — an endpoint reached here carries a bound RDMA device,
			// so a missing link record is the unverified case arriving by a different route.
			health := deviceplugin.Healthy
			if (ep.Link != nil && ep.Link.State == workercore.DeviceInterfaceLinkStateFailed) ||
				!snap.isHCAEligible(ep.RDMADevice) {
				health = deviceplugin.Unhealthy
			}
			// The partition family carries a NUMA hint too, where the accelerator partition
			// pool carries none: an RDMA partition token names exactly one virtual function,
			// whose affinity a hint can honor, while an accelerator partition token names no
			// accelerator at all.
			endpoints = append(endpoints, advertisedEndpoint{
				health:   health,
				ids:      rdmaDeviceIDs(ep.Resource, s.AllocationMode),
				topology: ep.Topology,
			})
		}
	}

	for round := 0; round < int(rdmaPoolSizeOf(s.AllocationMode)); round++ {
		for i := range endpoints {
			resp.Devices = append(resp.Devices, &deviceplugin.Device{
				ID:       endpoints[i].ids[round],
				Health:   endpoints[i].health,
				Topology: endpoints[i].topology,
			})
		}
	}
	return resp, nil
}

// rdmaDeviceIDs returns the interchangeable tokens one endpoint advertises in the mode.
//
// Deliberately not Resource.DeviceIDs. That method's shared branch numbers its tokens by
// accelerator ledger units — stepping by the global denominator divided by the accelerator share
// count — because an accelerator token has to carry how much of an accelerator it is, and it
// ignores any size its caller passes. An RDMA token carries no such quantity: the allocation path
// resolves a token to the endpoint it names and never reads its index, and it deduplicates by
// endpoint, so a second token on one endpoint is a second claim on the same thing.
//
// Routing the RDMA families through that method therefore did two wrong things at once. It made
// the advertised count the accelerator share count whatever this file asked for, and it published
// indices that look like unit offsets into a ledger RDMA does not have.
func rdmaDeviceIDs(res Resource, mode workercore.DeviceAllocationMode) []string {
	size := rdmaPoolSizeOf(mode)
	prefix := res.String() + ":"

	ids := make([]string, 0, size)
	for i := int32(0); i < size; i++ {
		ids = append(ids, prefix+padIndex(uint64(i)))
	}
	return ids
}

// rdmaPoolSizeOf returns the token count one endpoint advertises in the mode: the concurrency
// ceiling for the shared family, and one token for the families where a token is a whole thing.
//
// The ceiling is RDMA's own number rather than the accelerator one it used to borrow. That
// constant counts how many owners an accelerator can be split among, which is a statement about
// accelerators; nothing about an HCA followed from it, and sharing the symbol made the two look
// like one fact that had to move together.
func rdmaPoolSizeOf(mode workercore.DeviceAllocationMode) int32 {
	switch mode {
	case workercore.DeviceAllocationModeShared:
		return int32(nodefeature.RDMAEndpointPoolSize)
	default:
		return 1
	}
}

// GetPreferredAllocation returns a preferred set of devices to allocate from a list of available
// ones. The hint is advisory: kubelet may ignore it, and Allocate answers for whatever arrives.
//
// The preference is a spread, not a packing. A shared token resolves to one endpoint, so a claim
// of N tokens over N endpoints names one token per endpoint before a second token of any one of
// them. The walk runs an outer round over token indices and an inner walk over the eligible
// endpoints in hardware order — round 1 takes the first free token of every endpoint still below
// one pick, round 2 the next, until the claim is full — first under a distinct-HCA rule, so two
// endpoints bound to one HCA name do not both draw it, then without the rule for the inventory
// that genuinely carries several endpoints on one device. A claim the available tokens cannot
// satisfy earns an empty response, which leaves the choice, and the reason, to Allocate.
func (s *rdmaServer) GetPreferredAllocation(ctx context.Context, req *PreferredAllocationRequest) (*PreferredAllocationResponse, error) {
	ctrRequests := req.GetContainerRequests()
	if len(ctrRequests) == 0 {
		// Refused rather than indexed, the way Allocate refuses it: an external gRPC boundary
		// grpc-go does not recover a handler panic behind.
		return nil, grpcstatus.Error(grpccodes.InvalidArgument,
			"the preferred allocation request carries no container requests")
	}

	devs, err := s.Reconciler.getDevices(ctx)
	if err != nil {
		return nil, grpcstatus.Errorf(grpccodes.Internal, "read the interface inventory: %v", err)
	}
	snap := s.Reconciler.captureRDMAInterfaceSnapshot(devs)
	endpoints := s.eligibleRDMAEndpoints(snap)

	resp := &PreferredAllocationResponse{
		ContainerResponses: make([]*ContainerPreferredAllocationResponse, 0, len(ctrRequests)),
	}
	for _, ctrReq := range ctrRequests {
		ctrResp, err := s.getContainerPreferredAllocation(ctrReq, endpoints)
		if err != nil {
			return nil, err
		}
		resp.ContainerResponses = append(resp.ContainerResponses, ctrResp)
	}
	s.Logger.Info("get preferred allocation response", "response", resp)
	return resp, nil
}

// eligibleRDMAEndpoints returns the endpoints of the family being served that a preferred
// allocation may draw on: a failed link withholds an endpoint, as does an HCA the active
// selection excludes — unselected or externally owned. The order is the inventory's hardware
// order, which is the order the spread promises.
func (s *rdmaServer) eligibleRDMAEndpoints(snap rdmaInterfaceSnapshot) []rdmaEndpoint {
	endpoints := make([]rdmaEndpoint, 0)
	for i := range snap.interfaces {
		iface := &snap.interfaces[i]
		for _, ep := range rdmaEndpoints(iface, s.AllocationMode) {
			if (ep.Link != nil && ep.Link.State == workercore.DeviceInterfaceLinkStateFailed) ||
				!snap.isHCAEligible(ep.RDMADevice) {
				continue
			}
			endpoints = append(endpoints, ep)
		}
	}
	return endpoints
}

// getContainerPreferredAllocation picks one container's tokens from the eligible endpoints.
// MustIncludeDeviceIDs are taken first, in sorted order, whatever the walk would have chosen;
// the rounds then spend one index across every endpoint per pass, until the claim is full.
func (s *rdmaServer) getContainerPreferredAllocation(
	ctrReq *ContainerPreferredAllocationRequest,
	endpoints []rdmaEndpoint,
) (*ContainerPreferredAllocationResponse, error) {
	availableByRes, err := parseResourceTokensByAccelerator(ctrReq.GetAvailableDeviceIDs())
	if err != nil {
		return nil, grpcstatus.Errorf(grpccodes.InvalidArgument, "convert available device id: %v", err)
	}

	mustTokens := make(map[string]ResourceToken, len(ctrReq.GetMustIncludeDeviceIDs()))
	for _, id := range ctrReq.GetMustIncludeDeviceIDs() {
		tok, err := ParseResourceToken(id)
		if err != nil {
			return nil, grpcstatus.Errorf(grpccodes.InvalidArgument,
				"invalid must include device id %q: %v", id, err)
		}
		mustTokens[id] = tok
	}

	allocationSize := int(ctrReq.GetAllocationSize())
	if allocationSize <= 0 {
		return &ContainerPreferredAllocationResponse{}, nil
	}

	endpointByRes := make(map[Resource]rdmaEndpoint, len(endpoints))
	for _, ep := range endpoints {
		endpointByRes[ep.Resource] = ep
	}

	selected := make([]string, 0, allocationSize)
	usedIDs := sets.New[string]()
	pickedPerEndpoint := make(map[Resource]int, len(endpoints))
	pickedPerHCA := make(map[string]int, len(endpoints))

	// A set makes the walk independent of the order the request listed the IDs in, and of
	// the same name arriving twice. A must-include naming an endpoint outside the eligible
	// set — a link that failed or an HCA the resolved selection excludes — is skipped, and
	// its pick is replaced from the eligible endpoints by the rounds below.
	for _, id := range sets.List(sets.New(ctrReq.GetMustIncludeDeviceIDs()...)) {
		if len(selected) >= allocationSize || usedIDs.Has(id) {
			continue
		}
		tok := mustTokens[id]
		ep, ok := endpointByRes[tok.Resource]
		if !ok {
			continue
		}
		selected = append(selected, id)
		usedIDs.Insert(id)
		pickedPerEndpoint[tok.Resource]++
		pickedPerHCA[ep.RDMADevice]++
	}

	// One pass of the rounds. Round r sits an endpoint out while it already holds r or more
	// picks, which is what makes the walk a round-robin over endpoints rather than a refill of
	// the first one. requireDistinctHCA applies the same ceiling to each HCA name, so a second
	// endpoint bound to an already-drawn device waits for a round the device is below.
	poolSize := int(rdmaPoolSizeOf(s.AllocationMode))
	walk := func(requireDistinctHCA bool) {
		for round := 1; round <= poolSize && len(selected) < allocationSize; round++ {
			for i := range endpoints {
				if len(selected) >= allocationSize {
					return
				}
				ep := &endpoints[i]
				if pickedPerEndpoint[ep.Resource] >= round {
					continue
				}
				if requireDistinctHCA && pickedPerHCA[ep.RDMADevice] >= round {
					continue
				}
				for _, tok := range availableByRes[ep.Resource] {
					if usedIDs.Has(tok.String()) {
						continue
					}
					selected = append(selected, tok.String())
					usedIDs.Insert(tok.String())
					pickedPerEndpoint[ep.Resource]++
					pickedPerHCA[ep.RDMADevice]++
					break
				}
			}
		}
	}

	// Pass one prefers distinct HCAs; pass two drops the rule, for an inventory that carries
	// more endpoints than HCA names and cannot fill the claim under it.
	walk(true)
	if len(selected) < allocationSize {
		walk(false)
	}

	if len(selected) < allocationSize {
		return &ContainerPreferredAllocationResponse{}, nil
	}
	return &ContainerPreferredAllocationResponse{DeviceIDs: selected}, nil
}
