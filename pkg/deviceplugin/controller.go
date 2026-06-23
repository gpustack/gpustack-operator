package deviceplugin

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	core "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/sets"
	ctrl "sigs.k8s.io/controller-runtime"
	ctrlbuilder "sigs.k8s.io/controller-runtime/pkg/builder"
	ctrlcli "sigs.k8s.io/controller-runtime/pkg/client"
	ctrlevent "sigs.k8s.io/controller-runtime/pkg/event"
	ctrlhandler "sigs.k8s.io/controller-runtime/pkg/handler"
	ctrllog "sigs.k8s.io/controller-runtime/pkg/log"
	ctrlpredicate "sigs.k8s.io/controller-runtime/pkg/predicate"
	ctrlreconcile "sigs.k8s.io/controller-runtime/pkg/reconcile"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	"gpustack.ai/gpustack/pkg/controller"
	"gpustack.ai/gpustack/pkg/kubeclientset"
	"gpustack.ai/gpustack/pkg/kubemeta"
	"gpustack.ai/gpustack/pkg/nodefeature"
	"gpustack.ai/gpustack/pkg/utils/json"
	"gpustack.ai/gpustack/pkg/utils/mapx"
	"gpustack.ai/gpustack/pkg/utils/osx"
	"gpustack.ai/gpustack/pkg/utils/stringx"
)

type (
	_DevicesNotifier struct {
		Manufacturer   string
		AllocationMode workercore.DeviceAllocationMode
		Channel        chan struct{}
	}

	// DevicesReconciler reconciles v1alpha1.Devices objects on a Kubernetes Node
	// and watches the events of Pods scheduled to the Node, to manage the status of Devices.
	DevicesReconciler struct {
		NodeName  string
		Client    ctrlcli.Client
		APIReader ctrlcli.Reader

		notifiersMutex sync.RWMutex
		notifiers      []_DevicesNotifier
	}
)

func (r *DevicesReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	nodeName := req.Name

	logger := ctrllog.FromContext(ctx).WithValues("node", nodeName)

	devs := new(workercore.Devices)
	err := r.Client.Get(ctx, req.NamespacedName, devs)
	if err != nil {
		logger.Error(err, "fetch devices")
		return ctrl.Result{RequeueAfter: time.Second}, err
	}

	podList := new(core.PodList)
	err = r.Client.List(ctx, podList,
		ctrlcli.MatchingFields{IndexingPodsByNodeName: nodeName},
		ctrlcli.UnsafeDisableDeepCopy)
	if err != nil {
		logger.Error(err, "list pods with node name")
		return ctrl.Result{RequeueAfter: time.Second}, err
	}

	// Clear/Initialize devices status.
	eDevsStatus := workercore.DevicesStatus{
		Groups: make([]workercore.DevicesAllocationGroup, 0, len(devs.Spec.Groups)),
	}
	for i := range devs.Spec.Groups {
		devsGroup := &devs.Spec.Groups[i]
		devsStatusGroup := workercore.DevicesAllocationGroup{
			ID:           devsGroup.ID,
			Manufacturer: devsGroup.Manufacturer,
			Accelerators: make([]workercore.AcceleratorAllocation, 0, len(devsGroup.Accelerators)),
		}
		for j := range devsGroup.Accelerators {
			devAccelerator := &devsGroup.Accelerators[j]
			devsStatusGroup.Accelerators = append(devsStatusGroup.Accelerators, workercore.AcceleratorAllocation{
				ID:        devAccelerator.ID,
				Index:     devAccelerator.Index,
				Mode:      workercore.DeviceAllocationModeNone,
				Remaining: nodefeature.ResourceMaxUnits,
			})
		}
		eDevsStatus.Groups = append(eDevsStatus.Groups, devsStatusGroup)
	}

	// Merge allocated accelerators.
	for i := range podList.Items {
		pod := &podList.Items[i]
		if pod.DeletionTimestamp != nil {
			continue
		}

		podDevsStatus, err := extractAllocatedStatusFromPod(pod)
		if err != nil {
			logger.Error(err, "extract allocated accelerators from pod", "pod", ctrlcli.ObjectKeyFromObject(pod))
			continue
		}

		eDevsStatus, err = applyAllocatedStatus(podDevsStatus, eDevsStatus)
		if err != nil {
			logger.Error(err, "merge allocated accelerators into devices status", "pod", ctrlcli.ObjectKeyFromObject(pod))
			continue
		}
	}

	if !kubemeta.DeepEqual(devs.Status, eDevsStatus) {
		devs.Status = eDevsStatus
		err = r.Client.Status().Update(ctx, devs)
		if err != nil {
			logger.Error(err, "update devices status")
			return ctrl.Result{}, ctrlcli.IgnoreNotFound(err)
		}
	}

	r.notifiersMutex.RLock()
	if len(r.notifiers) > 0 {
		for i := range r.notifiers {
			notifier := &r.notifiers[i]
			select {
			case notifier.Channel <- struct{}{}:
			default:
				logger.Error(nil,
					"notifier channel is full, skipping notify",
					"manufacturer", notifier.Manufacturer,
					"mode", notifier.AllocationMode.String(),
				)
			}
		}
	}
	r.notifiersMutex.RUnlock()

	return ctrl.Result{}, nil
}

const IndexingPodsByNodeName = "pods.nodeName"

func (r *DevicesReconciler) SetupController(ctx context.Context, opts controller.SetupOptions) error {
	// Configure field indexer.
	fi := opts.Manager.GetFieldIndexer()
	err := fi.IndexField(ctx, &core.Pod{}, IndexingPodsByNodeName,
		func(obj ctrlcli.Object) []string {
			if obj == nil {
				return nil
			}
			pod := obj.(*core.Pod)
			return []string{pod.Spec.NodeName}
		})
	if err != nil {
		return fmt.Errorf("index pod '%s': %w", IndexingPodsByNodeName, err)
	}

	r.NodeName = osx.Getenv("KUBERNETES_NODE_NAME")
	r.Client = opts.Manager.GetClient()

	return ctrl.NewControllerManagedBy(opts.Manager).
		Named("deviceplugin.manage.devices").
		For(
			&workercore.Devices{},
			ctrlbuilder.WithPredicates(
				// Interested in the Devices object corresponding to the current node.
				ctrlpredicate.NewPredicateFuncs(func(object ctrlcli.Object) bool {
					return object.GetName() == r.NodeName
				}),
			),
		).
		Watches(
			// Watch Kubernetes Pods and enqueue the corresponding v1.Devices.
			&core.Pod{},
			ctrlhandler.EnqueueRequestsFromMapFunc(
				r.enqueueDevicesWhenPodChanged,
			),
			ctrlbuilder.WithPredicates(
				// Interested in Pods scheduled to the current node.
				ctrlpredicate.NewPredicateFuncs(func(object ctrlcli.Object) bool {
					pod := object.(*core.Pod)
					return pod.Spec.NodeName == r.NodeName
				}),
				// Interested in Pod updates with changes in accelerator allocation annotations,
				// or deletion of Pods with allocated accelerators.
				ctrlpredicate.Funcs{
					CreateFunc: func(e ctrlevent.CreateEvent) bool {
						return false
					},
					DeleteFunc: func(e ctrlevent.DeleteEvent) bool {
						return true
					},
					UpdateFunc: func(e ctrlevent.UpdateEvent) bool {
						oldPod, newPod := e.ObjectOld, e.ObjectNew
						if newPod.GetDeletionTimestamp() == nil {
							return !mapx.EqualWithKey(oldPod.GetAnnotations(), newPod.GetAnnotations(), AllocatedAcceleratorAnnoKey)
						}
						if kubemeta.HasAnnotation(oldPod, AllocatedAcceleratorAnnoKey) {
							if oldPod.GetDeletionTimestamp() == nil {
								return true
							}
							return !oldPod.GetDeletionTimestamp().Equal(newPod.GetDeletionTimestamp())
						}
						return false
					},
					GenericFunc: func(e ctrlevent.GenericEvent) bool {
						return false
					},
				},
			),
		).
		Complete(r)
}

func (r *DevicesReconciler) enqueueDevicesWhenPodChanged(
	ctx context.Context, obj ctrlcli.Object,
) []ctrlreconcile.Request {
	logger := ctrllog.FromContext(ctx).
		WithValues("pod", ctrlcli.ObjectKeyFromObject(obj))

	pod := obj.(*core.Pod)

	// Ensure the Pod is scheduled to the current node, as a safety check.
	if pod.Spec.NodeName != r.NodeName {
		logger.Error(nil, "pod is not scheduled to the current node")
		return nil
	}

	reqs := []ctrlreconcile.Request{
		{
			NamespacedName: ctrlcli.ObjectKey{
				Name: pod.Spec.NodeName,
			},
		},
	}
	logger.V(2).Info("enqueued devices for pod", "requests", reqs)
	return reqs
}

func (r *DevicesReconciler) getReconcileNotifier(manufacturer string, allocationMode workercore.DeviceAllocationMode) <-chan struct{} {
	r.notifiersMutex.Lock()
	defer r.notifiersMutex.Unlock()

	channel := make(chan struct{}, 4)
	r.notifiers = append(r.notifiers, _DevicesNotifier{
		Manufacturer:   manufacturer,
		AllocationMode: allocationMode,
		Channel:        channel,
	})
	return channel
}

func (r *DevicesReconciler) getDevices(ctx context.Context) (*workercore.Devices, error) {
	devs := &workercore.Devices{
		ObjectMeta: meta.ObjectMeta{
			Name: r.NodeName,
		},
	}
	err := r.Client.Get(ctx, ctrlcli.ObjectKeyFromObject(devs), devs,
		&ctrlcli.GetOptions{
			Raw: &meta.GetOptions{
				ResourceVersion: "0",
			},
		})
	return devs, err
}

const (
	AllocatedAcceleratorAnnoKey       = "device.gpustack.ai/accelerator.allocated"
	_PreferredAcceleratorIDAnnoKey    = "device.gpustack.ai/accelerator.preferred-id"
	_PreferredAcceleratorIndexAnnoKey = "device.gpustack.ai/accelerator.preferred-index"
)

func (r *DevicesReconciler) getAllocatingPodWithRetry(
	ctx context.Context, resName core.ResourceName, resQuantity resource.Quantity,
) (*core.Pod, error) {
	var pod *core.Pod
	var err error
	for i := 0; i < 5; i++ {
		pod, err = r.getAllocatingPod(ctx, resName, resQuantity)
		if err == nil {
			return pod, nil
		}
		time.Sleep(3 * time.Second)
	}
	return nil, fmt.Errorf("get allocating pod with retry: %w", err)
}

func (r *DevicesReconciler) getAllocatingPod(
	ctx context.Context, resName core.ResourceName, resQuantity resource.Quantity,
) (*core.Pod, error) {
	podList := new(core.PodList)
	err := r.Client.List(ctx, podList,
		ctrlcli.MatchingFields{IndexingPodsByNodeName: r.NodeName},
		ctrlcli.UnsafeDisableDeepCopy)
	if err != nil {
		return nil, fmt.Errorf("list pods with node name: %w", err)
	}
	if len(podList.Items) == 0 {
		return nil, fmt.Errorf("no pods found with node name %s", r.NodeName)
	}

	sort.Slice(podList.Items, func(i, j int) bool {
		return podList.Items[i].CreationTimestamp.Before(&podList.Items[j].CreationTimestamp)
	})

	for i := range podList.Items {
		pod := &podList.Items[i]
		if p := pod.Status.Phase; p != "" && p != core.PodPending {
			continue
		}
		for j := range pod.Spec.InitContainers {
			ctr := &pod.Spec.InitContainers[j]
			for actualResName, actualResQuantity := range ctr.Resources.Limits {
				if actualResName == resName && actualResQuantity.Equal(resQuantity) {
					return pod, nil
				}
			}
		}
		for j := range pod.Spec.Containers {
			ctr := &pod.Spec.Containers[j]
			for actualResName, actualResQuantity := range ctr.Resources.Limits {
				if actualResName == resName && actualResQuantity.Equal(resQuantity) {
					return pod, nil
				}
			}
		}
	}

	return nil, fmt.Errorf("cannot find pending pod with resource request %s=%s", resName, resQuantity.String())
}

func (r *DevicesReconciler) patchAllocatingPod(ctx context.Context, pod *core.Pod, allocatedStatus workercore.DevicesStatus) error {
	allocatedStatusBytes, err := json.Marshal(allocatedStatus)
	if err != nil {
		return fmt.Errorf("marshal allocated groups: %w", err)
	}

	obj := map[string]any{
		"metadata": map[string]any{
			"annotations": map[string]any{
				AllocatedAcceleratorAnnoKey: string(allocatedStatusBytes),
			},
		},
	}
	objBytes, err := json.Marshal(obj)
	if err != nil {
		return fmt.Errorf("marshal object meta: %w", err)
	}

	_, err = kubeclientset.PatchWithCtrlClient(ctx, r.Client, pod, types.StrategicMergePatchType, objBytes)
	return err
}

func extractAllocatedStatusFromPod(pod *core.Pod) (allocatedStatus workercore.DevicesStatus, err error) {
	if pod.Annotations != nil && pod.Annotations[AllocatedAcceleratorAnnoKey] != "" {
		str := pod.Annotations[AllocatedAcceleratorAnnoKey]
		err = json.Unmarshal(stringx.ToBytes(&str), &allocatedStatus)
	}
	return allocatedStatus, err
}

func applyAllocatedStatus(allocatedStatus, remainingStatus workercore.DevicesStatus) (workercore.DevicesStatus, error) {
	if len(allocatedStatus.Groups) == 0 {
		return remainingStatus, nil
	}

	dstAcceleratorIndex := make(map[Resource]*workercore.AcceleratorAllocation)
	for i := range remainingStatus.Groups {
		dstGrp := &remainingStatus.Groups[i]
		for j := range dstGrp.Accelerators {
			dstAccelerator := &dstGrp.Accelerators[j]
			res := Resource{
				Group:  dstGrp.ID,
				Device: dstAccelerator.ID,
			}
			dstAcceleratorIndex[res] = dstAccelerator
		}
	}

	for i := range allocatedStatus.Groups {
		srcGrp := &allocatedStatus.Groups[i]
		for j := range srcGrp.Accelerators {
			srcAccelerator := &srcGrp.Accelerators[j]
			res := Resource{
				Group:  srcGrp.ID,
				Device: srcAccelerator.ID,
			}
			dstAccelerator, exists := dstAcceleratorIndex[res]
			if !exists {
				continue
			}
			if dstAccelerator.Mode != workercore.DeviceAllocationModeNone &&
				dstAccelerator.Mode != srcAccelerator.Mode {
				return remainingStatus, fmt.Errorf("conflicting allocation mode for resource %v: %v vs. %v",
					res, dstAccelerator.Mode, srcAccelerator.Mode)
			}
			dstAccelerator.Mode = srcAccelerator.Mode
			dstAccelerator.Remaining = max(dstAccelerator.Remaining-srcAccelerator.Allocated, 0)
		}
	}

	return remainingStatus, nil
}

func extractPreferredAcceleratorIDsFromPod(pod *core.Pod, devices *workercore.Devices) sets.Set[string] {
	if pod != nil && pod.Annotations != nil {
		str, ok := pod.Annotations[_PreferredAcceleratorIDAnnoKey]
		if ok {
			requiredIDs := strings.Split(str, ",")
			for i := range requiredIDs {
				requiredIDs[i] = strings.TrimSpace(requiredIDs[i])
			}
			return sets.New[string](requiredIDs...)
		}

		str, ok = pod.Annotations[_PreferredAcceleratorIndexAnnoKey]
		if ok {
			requiredIndexes := strings.Split(str, ",")
			for i := range requiredIndexes {
				requiredIndexes[i] = strings.TrimSpace(requiredIndexes[i])
			}
			requiredIndexesSet := sets.New[string](requiredIndexes...)

			requiredIDsSet := sets.New[string]()
			for i := range devices.Spec.Groups {
				devGroup := &devices.Spec.Groups[i]
				for j := range devGroup.Accelerators {
					devAccelerator := &devGroup.Accelerators[j]
					if requiredIndexesSet.Has(strconv.Itoa(int(devAccelerator.Index))) {
						requiredIDsSet.Insert(devAccelerator.ID)
					}
				}
			}
			return requiredIDsSet
		}
	}

	return sets.New[string]()
}
