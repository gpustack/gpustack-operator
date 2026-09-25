package deviceplugin

import (
	"context"
	"fmt"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	core "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	podresources "k8s.io/kubelet/pkg/apis/podresources/v1"
	ctrllog "sigs.k8s.io/controller-runtime/pkg/log"

	"gpustack.ai/gpustack/pkg/utils/osx"
)

const (
	// KubeletPodResourcesSocket is where kubelet serves its pod-resources API: the pods it holds
	// and, per container, the devices it has already handed out. The Device Manager DaemonSet
	// mounts its directory read-only.
	KubeletPodResourcesSocket = "/var/lib/kubelet/pod-resources/kubelet.sock"

	// IdentifyByKubeletEnv turns the kubelet lookup off when set to "false": every Allocate then
	// identifies its container by the pending-Pod heuristic alone, as it did before the lookup
	// existed.
	IdentifyByKubeletEnv = "GPUSTACK_DEVICE_PLUGIN_IDENTIFY_BY_KUBELET"

	// kubeletPodResourcesTimeout bounds one lookup. kubelet is blocked on the Allocate that asks,
	// so a lookup that cannot answer quickly is abandoned for the heuristic rather than waited on.
	kubeletPodResourcesTimeout = 2 * time.Second
)

// kubeletPodLister returns the pods kubelet currently holds, with the devices each of their
// containers has been allocated.
type kubeletPodLister func(ctx context.Context) ([]*podresources.PodResources, error)

// newKubeletPodLister returns a lister that asks kubelet over the pod-resources socket. It dials on
// every call: a lookup is made only on the allocation path, and a connection held between calls
// would outlive a kubelet restart that replaces the socket.
func newKubeletPodLister(socket string) kubeletPodLister {
	return func(ctx context.Context) ([]*podresources.PodResources, error) {
		cli, err := grpc.NewClient("unix://"+socket,
			grpc.WithTransportCredentials(insecure.NewCredentials()),
		)
		if err != nil {
			return nil, fmt.Errorf("dial kubelet pod-resources socket: %w", err)
		}
		defer osx.Close(cli)

		ctx, cancel := context.WithTimeout(ctx, kubeletPodResourcesTimeout)
		defer cancel()
		resp, err := podresources.NewPodResourcesListerClient(cli).List(ctx, &podresources.ListPodResourcesRequest{})
		if err != nil {
			return nil, fmt.Errorf("list kubelet pod resources: %w", err)
		}
		return resp.GetPodResources(), nil
	}
}

// _KubeletPending is kubelet's own answer to the question the device-plugin RPC leaves open: which
// container an Allocate is for.
//
// kubelet adds a pod to its pod manager before admitting it, admits pods one at a time, and records
// a container's devices only once the Allocate for it has returned. So while an Allocate runs, the
// container it serves is one kubelet holds that has no device of the resource yet, and every
// container kubelet already served has one. A pod kubelet has not reached yet is not in the list at
// all, which is what the creation-time heuristic cannot see: kubelet admits in the order pods reach
// it, and creation timestamps carry whole seconds only.
type _KubeletPending struct {
	// pods maps each pod kubelet holds to what it reports for that pod's containers: true for one
	// that already holds a device of the resource, false for one that does not.
	pods map[types.NamespacedName]map[string]bool
}

// kubeletPending asks kubelet which of its containers still wait for a device of resName, or returns
// nil when the lookup is switched off or kubelet cannot answer, in which case the caller identifies
// by the heuristic alone.
func (r *DevicesReconciler) kubeletPending(ctx context.Context, resName core.ResourceName) *_KubeletPending {
	if r.kubeletPods == nil {
		return nil
	}
	listed, err := r.kubeletPods(ctx)
	if err != nil {
		ctrllog.FromContext(ctx).Info("kubelet pod resources unavailable; identifying the allocating container "+
			"by the pending-Pod heuristic", "resource", resName, "reason", err.Error())
		return nil
	}

	pending := &_KubeletPending{pods: make(map[types.NamespacedName]map[string]bool, len(listed))}
	for _, p := range listed {
		ctrs := make(map[string]bool, len(p.GetContainers()))
		for _, c := range p.GetContainers() {
			held := false
			for _, d := range c.GetDevices() {
				if d.GetResourceName() == string(resName) && len(d.GetDeviceIds()) > 0 {
					held = true
					break
				}
			}
			ctrs[c.GetName()] = held
		}
		pending.pods[types.NamespacedName{Namespace: p.GetNamespace(), Name: p.GetName()}] = ctrs
	}
	return pending
}

// waitsForDevice reports whether kubelet may be allocating to this container right now: kubelet
// holds its pod, and either reports the container without a device of the resource, or does not
// report the container at all. The API leaves a non-restartable init container out of its list, so
// such a container cannot be ruled out and counts as waiting.
func (k *_KubeletPending) waitsForDevice(pod *core.Pod, ctr *core.Container) bool {
	ctrs, ok := k.pods[types.NamespacedName{Namespace: pod.Namespace, Name: pod.Name}]
	if !ok {
		return false
	}
	held, reported := ctrs[ctr.Name]
	return !reported || !held
}
