// Package rdma owns the RDMA device-plugin servers: the four allocation-mode families that make a
// node's RDMA interfaces allocatable. It is the thin device.Allocator the vendor packages' shape
// describes — it constructs the servers, hands them one shared lifecycle, and starts each once
// against kubelet's socket. The servers themselves and every response they build live in
// pkg/deviceplugin, beside the reconciler inventory they read.
package rdma

import (
	"context"

	klog "k8s.io/klog/v2"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	"gpustack.ai/gpustack/pkg/device"
	"gpustack.ai/gpustack/pkg/devicemanager/controllers"
	"gpustack.ai/gpustack/pkg/deviceplugin"
	"gpustack.ai/gpustack/pkg/utils/gox"
)

// FamilyName names the resource family this allocator serves. A network interface belongs to the
// node rather than to a manufacturer, so this is a family name where the vendor allocators carry a
// manufacturer's; it never reaches the detected-manufacturer loop, which is keyed on a fact RDMA
// does not have.
const FamilyName = "rdma"

// New returns the RDMA allocator: one device-plugin server per allocation-mode family the control
// flags leave in place, each serving the resource name that family owns. The switches mean what
// they mean for a vendor's families — a dropped family registers no resource with kubelet at all —
// with Exclusive ungated, as it is on the vendor side.
//
// Construction cannot fail, because every mode served here has a resource name of its own: the
// constructor NewRDMAServer applies refuses a mode with none, and the set above never offers one.
// A mode the constructor refuses is logged and left unserved rather than ending startup, so were
// the set ever widened past what GetRDMAResourceName names, this allocator would still report
// success while kubelet never saw that resource. Nothing reaches that today, and what catches it
// if anything ever does is the length of modes against servers, which the tests assert on.
func New(opts device.AllocatorOptions) device.Allocator {
	logger := opts.Logger.WithName(FamilyName)

	modes := []workercore.DeviceAllocationMode{
		workercore.DeviceAllocationModeExclusive,
	}
	if !opts.NoShared {
		modes = append(modes, workercore.DeviceAllocationModeShared)
	}
	// No Sliced entry, and no NoSliced switch to read: the RDMA family serves no sliced key, so
	// there is nothing for that switch to turn off. Offering the mode here would build a server
	// whose resource name is empty, which newServers drops anyway.
	if !opts.NoPartitioned {
		modes = append(modes, workercore.DeviceAllocationModePartitioned)
	}

	return &aggregated{
		logger:     logger,
		servers:    newServers(logger, modes),
		modes:      modes,
		kubeSocket: opts.KubeSocket,
	}
}

// newServers builds one server per mode, resolving the reconciler once: the servers read the same
// node inventory, and the constructor's refusal of a mode with no resource name is reported here
// rather than ignored.
func newServers(logger klog.Logger, modes []workercore.DeviceAllocationMode) []deviceplugin.Server {
	reconciler := controllers.Get[*deviceplugin.DevicesReconciler]()

	servers := make([]deviceplugin.Server, 0, len(modes))
	for _, mode := range modes {
		server, err := deviceplugin.NewRDMAServer(logger, mode, reconciler)
		if err != nil {
			logger.Error(err, "serving no resource for the allocation mode", "mode", mode.String())
			continue
		}
		servers = append(servers, server)
	}
	return servers
}

type aggregated struct {
	logger  klog.Logger
	servers []deviceplugin.Server
	// modes carries each server's allocation mode beside it. The servers are pkg/deviceplugin's
	// own unexported type behind its Server interface, so nothing here can ask a server which
	// mode it serves; this slice is the only handle on that, and the tests assert on it. Its
	// length against servers is also what catches a mode the constructor refused: newServers
	// skips such a mode, leaving servers shorter than this, which is the one observable
	// difference between "every mode is served" and "one was silently dropped".
	modes      []workercore.DeviceAllocationMode
	kubeSocket string
	// lifecycle owns the context the servers run under, so that stopping this allocator ends
	// every one of them.
	lifecycle gox.Lifecycle
}

func (*aggregated) Name() string {
	return FamilyName
}

func (in *aggregated) Start(ctx context.Context) error {
	in.logger.Info("starting")

	tasks := make([]func(context.Context) error, 0, len(in.servers))
	for i := range in.servers {
		srv := in.servers[i]
		tasks = append(tasks, func(ctx context.Context) error {
			return srv.Start(ctx, in.kubeSocket)
		})
	}

	return in.lifecycle.Start(ctx, tasks...)
}

func (in *aggregated) Stop() {
	in.logger.Info("stopping")

	in.lifecycle.Stop()
}
