package deviceplugin

import (
	"context"
	"fmt"
	"path/filepath"
	"sort"
	"strings"

	grpccodes "google.golang.org/grpc/codes"
	grpcstatus "google.golang.org/grpc/status"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
)

// Host trees the allocation response reads. They are vars rather than consts for the reason this
// package's own host paths already give: a test cannot stand a file under the host's real /sys or
// /dev/infiniband, so both roots are redirected to fixture trees while the resolution and the
// existence checks themselves stay real.
var (
	// rdmaSysfsRoot is where the kernel exposes the RDMA device model the verbs resolution reads.
	rdmaSysfsRoot = "/sys"

	// rdmaDevNodesDir is where the kernel places the RDMA character devices, the same directory
	// the verbs resolution derives its answer under. Carried as a root of its own so the
	// response's existence check runs against that directory rather than a compiled-in string,
	// which is what lets a fixture stand the nodes it wants a host to carry.
	rdmaDevNodesDir = devInfinibandDir
)

// rdmaCmName is the node-level RDMA connection-manager character device. It belongs to the host
// rather than to any one endpoint — every RDMA connection goes through it — so a response carries
// it once wherever the host has one.
const rdmaCmName = "rdma_cm"

// ncclIbHCAEnv names the granted RDMA devices to the container, comma-joined in the order the
// sorted device IDs name their endpoints. A container granted a device it cannot name has been
// handed half a resource, which is the one engine-facing shape this feature takes on.
const ncclIbHCAEnv = "NCCL_IB_HCA"

// Allocate is called during container creation so that the Device Plugin can run device specific
// operations and instruct Kubelet of the steps to make the devices available in the container.
//
// Each granted token is resolved back to its endpoint in Devices.spec.interfaces[] at this moment,
// and nothing is written anywhere: no ledger entry, no Pod annotation, no reservation. The
// response is therefore always built from the record as it stands, which is what lets the two
// pooled families draw on the same HCAs without decrementing each other.
//
// A token that does not parse, or that names no endpoint of the family being served, is refused:
// a response that quietly grants less than was asked is the silent half-grant this resource
// exists to end.
func (s *rdmaServer) Allocate(ctx context.Context, req *AllocateRequest) (*AllocateResponse, error) {
	ctrRequests := req.GetContainerRequests()
	if len(ctrRequests) == 0 {
		// Refused rather than indexed. This is an external gRPC boundary and grpc-go does not
		// recover a handler panic, so an empty request would end the whole device-manager
		// process -- every vendor allocator with it -- instead of the one call.
		return nil, grpcstatus.Error(grpccodes.InvalidArgument,
			"the allocate request carries no container requests")
	}
	ctrReq := ctrRequests[0]

	deviceIDs := ctrReq.GetDevicesIds()
	sort.Strings(deviceIDs)

	ctrResp, err := s.allocateContainer(ctx, deviceIDs)
	if err != nil {
		return nil, err
	}

	s.Logger.Info("allocate response", "response", ctrResp)
	return &AllocateResponse{ContainerResponses: []*ContainerAllocateResponse{ctrResp}}, nil
}

// allocateContainer builds one container's response from the granted device IDs: the endpoints
// they name under the family being served, the verbs character device each endpoint resolves to
// at this moment, the node-level connection manager where the host carries one, and the
// environment variable naming the granted RDMA devices.
func (s *rdmaServer) allocateContainer(
	ctx context.Context,
	deviceIDs []string,
) (*ContainerAllocateResponse, error) {
	devs, err := s.Reconciler.getDevices(ctx)
	if err != nil {
		return nil, fmt.Errorf("read the interface inventory: %w", err)
	}

	endpoints, err := s.grantedEndpoints(devs, deviceIDs)
	if err != nil {
		return nil, err
	}

	names := make([]string, 0, len(endpoints))
	for i := range endpoints {
		names = append(names, endpoints[i].RDMADevice)
	}

	ctrResp := &ContainerAllocateResponse{
		Envs: map[string]string{ncclIbHCAEnv: strings.Join(names, ",")},
	}

	// Devices are injected one endpoint at a time with NewRWDevice, never with NewDevicesIn over
	// /dev/infiniband. That helper injects every entry of a directory it can read, so it would
	// hand every container every adapter on the node, and it returns nil when it cannot read the
	// directory, so the same code would degrade to handing the container none of them with no
	// error — either way the opposite of the per-endpoint grant this resource exists to make.
	// The verbs node is the grant itself, so a host that exposes no node for a resolved endpoint
	// fails the allocation naming it rather than starting a container that was granted nothing it
	// can open.
	for i := range endpoints {
		verbs, err := verbsDevicePath(rdmaSysfsRoot, endpoints[i].RDMADevice)
		if err != nil {
			return nil, fmt.Errorf("resolve the verbs character device of endpoint %s: %w",
				endpoints[i].Resource, err)
		}
		node := rdmaDeviceNodePath(verbs)
		spec := NewRWDevice(node)
		if spec == nil {
			return nil, fmt.Errorf("endpoint %s has no verbs device node %q",
				endpoints[i].Resource, node)
		}
		ctrResp.Devices = append(ctrResp.Devices, spec)
	}

	// The connection manager belongs to the node, not to any endpoint, so it rides along once
	// per response however many endpoints were granted — and only where the host has one.
	if cm := NewRWDevice(filepath.Join(rdmaDevNodesDir, rdmaCmName)); cm != nil {
		ctrResp.Devices = append(ctrResp.Devices, cm)
	}

	return ctrResp, nil
}

// grantedEndpoints resolves each granted device ID to the RDMA endpoint it names under the family
// being served, first occurrence per endpoint, in the sorted order of the IDs. A second token on
// one endpoint is a second claim on the same thing and resolves to the same endpoint, so it adds
// nothing to the list. An empty grant is refused: kubelet does not send one, and a success the
// container cannot use is worse than the error that says so.
func (s *rdmaServer) grantedEndpoints(
	devs *workercore.Devices,
	deviceIDs []string,
) ([]rdmaEndpoint, error) {
	endpoints := make([]rdmaEndpoint, 0, len(deviceIDs))
	seen := make(map[Resource]struct{}, len(deviceIDs))
	for _, id := range deviceIDs {
		token, err := ParseResourceToken(id)
		if err != nil {
			s.Logger.Error(err, "convert device id", "device id", id)
			return nil, grpcstatus.Errorf(grpccodes.InvalidArgument, "invalid device id %q: %v", id, err)
		}
		if _, dup := seen[token.Resource]; dup {
			continue
		}
		endpoint, ok := s.grantedEndpoint(devs, token.Resource)
		if !ok {
			return nil, grpcstatus.Errorf(grpccodes.InvalidArgument,
				"device id %q names no RDMA endpoint of the %s family", id, s.AllocationMode)
		}
		seen[token.Resource] = struct{}{}
		endpoints = append(endpoints, endpoint)
	}
	if len(endpoints) == 0 {
		return nil, grpcstatus.Errorf(grpccodes.InvalidArgument, "the request grants no RDMA device")
	}
	return endpoints, nil
}

// grantedEndpoint finds the endpoint a locator names in the interface inventory, under the family
// being served. It reads the record as it stands: nothing an earlier allocation did can change
// the answer, because no earlier allocation wrote anything.
func (s *rdmaServer) grantedEndpoint(
	devs *workercore.Devices,
	res Resource,
) (rdmaEndpoint, bool) {
	for i := range devs.Spec.Interfaces {
		iface := &devs.Spec.Interfaces[i]
		for _, endpoint := range rdmaEndpoints(iface, s.AllocationMode) {
			if endpoint.Resource == res {
				return endpoint, true
			}
		}
	}
	return rdmaEndpoint{}, false
}

// rdmaDeviceNodePath anchors a resolved verbs device path in the directory the response injects
// device nodes from. That directory is the kernel's own /dev/infiniband, the same one the
// resolution derives its answer under, so this re-anchors the entry the resolution found rather
// than deriving anything of its own.
func rdmaDeviceNodePath(verbsPath string) string {
	return filepath.Join(rdmaDevNodesDir, filepath.Base(verbsPath))
}
