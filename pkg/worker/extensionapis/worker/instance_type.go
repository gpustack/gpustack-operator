package worker

import (
	"context"

	core "k8s.io/api/core/v1"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/apiserver/pkg/registry/rest"
	ctrlcli "sigs.k8s.io/controller-runtime/pkg/client"
	kueue "sigs.k8s.io/kueue/apis/kueue/v1beta2"

	worker "gpustack.ai/gpustack/api/worker/v1"
	"gpustack.ai/gpustack/pkg/devicefeature"
	"gpustack.ai/gpustack/pkg/extensionapi"
	"gpustack.ai/gpustack/pkg/systemmeta"
	"gpustack.ai/gpustack/pkg/utils/funcx"
	"gpustack.ai/gpustack/pkg/utils/gox"
	"gpustack.ai/gpustack/pkg/utils/strconvx"
	"gpustack.ai/gpustack/pkg/worker/apistatus"
)

const _InstanceTypeResource = "instancetypes"

// InstanceTypeHandler handles v1.InstanceType objects.
//
// InstanceTypeHandler maps the v1.InstanceType to a Kueue ClusterQueue resource,
// which is named as the InstanceType's name.
type InstanceTypeHandler struct {
	extensionapi.ObjectInfo
	extensionapi.ReadOnlyOperations

	Client    ctrlcli.Client
	APIReader ctrlcli.Reader
}

func (h *InstanceTypeHandler) SetupHandler(
	ctx context.Context,
	opts extensionapi.SetupOptions,
) (gvr schema.GroupVersionResource, srs map[string]rest.Storage, err error) {
	// Configure field indexer.
	fi := opts.Manager.GetFieldIndexer()
	err = fi.IndexField(ctx, &kueue.ClusterQueue{}, "metadata.name",
		func(obj ctrlcli.Object) []string {
			if obj == nil {
				return nil
			}
			return []string{obj.GetName()}
		})
	if err != nil {
		return schema.GroupVersionResource{}, srs, err
	}

	// Declare GVR.
	gvr = worker.SchemeGroupVersionResource(_InstanceTypeResource)

	// Create table converter to pretty the kubectl's output.
	tc, err := extensionapi.NewJSONPathTableConvertor(
		extensionapi.JSONPathTableColumnDefinition{
			TableColumnDefinition: meta.TableColumnDefinition{
				Name: "Group",
				Type: "string",
			},
			JSONPath: ".spec.group",
		},
		extensionapi.JSONPathTableColumnDefinition{
			TableColumnDefinition: meta.TableColumnDefinition{
				Name: "Accelerator",
				Type: "string",
			},
			JSONPath: ".status.accelerator.remaining",
		},
		extensionapi.JSONPathTableColumnDefinition{
			TableColumnDefinition: meta.TableColumnDefinition{
				Name: "CPU",
				Type: "string",
			},
			JSONPath: ".status.cpu.remaining",
		},
		extensionapi.JSONPathTableColumnDefinition{
			TableColumnDefinition: meta.TableColumnDefinition{
				Name: "RAM",
				Type: "string",
			},
			JSONPath: ".status.ram.remaining",
		},
		extensionapi.JSONPathTableColumnDefinition{
			TableColumnDefinition: meta.TableColumnDefinition{
				Name: "Local-Storage",
				Type: "string",
			},
			JSONPath: ".status.localStorage.remaining",
		},
		extensionapi.JSONPathTableColumnDefinition{
			TableColumnDefinition: meta.TableColumnDefinition{
				Name: "Phase",
				Type: "string",
			},
			JSONPath: ".status.phase",
		})
	if err != nil {
		return gvr, srs, err
	}

	// As storage.
	h.ObjectInfo = &worker.InstanceType{}
	h.ReadOnlyOperations = extensionapi.WithReadyOnly(tc, h)

	// Set client.
	h.Client = opts.Manager.GetClient()
	h.APIReader = opts.Manager.GetAPIReader()

	return gvr, srs, err
}

var (
	_ rest.Storage = (*InstanceTypeHandler)(nil)
	_ rest.Lister  = (*InstanceTypeHandler)(nil)
	_ rest.Watcher = (*InstanceTypeHandler)(nil)
	_ rest.Getter  = (*InstanceTypeHandler)(nil)
)

func (h *InstanceTypeHandler) New() runtime.Object {
	return &worker.InstanceType{}
}

func (h *InstanceTypeHandler) Destroy() {
}

func (h *InstanceTypeHandler) NewList() runtime.Object {
	return &worker.InstanceTypeList{}
}

func (h *InstanceTypeHandler) OnList(ctx context.Context, opts ctrlcli.ListOptions) (runtime.Object, error) {
	// List.
	cqList := new(kueue.ClusterQueueList)
	err := h.APIReader.List(ctx, cqList,
		convertClusterQueueListOptsFromInstanceTypeListOpts(opts))
	if err != nil {
		return nil, err
	}

	// Convert.
	itList := convertInstanceTypeListFromClusterQueueList(cqList, opts)
	return itList, nil
}

func (h *InstanceTypeHandler) OnWatch(ctx context.Context, opts ctrlcli.ListOptions) (watch.Interface, error) {
	// Watch.
	uw, err := h.Client.(ctrlcli.WithWatch).Watch(ctx, new(kueue.ClusterQueueList),
		convertClusterQueueListOptsFromInstanceTypeListOpts(opts))
	if err != nil {
		return nil, err
	}

	c := make(chan watch.Event)
	dw := watch.NewProxyWatcher(c)
	gox.Go(func() {
		defer close(c)
		defer uw.Stop()

		for {
			select {
			case <-ctx.Done():
				// Cancel by context.
				return
			case <-dw.StopChan():
				// Stop by downstream.
				return
			case e, ok := <-uw.ResultChan():
				if !ok {
					// Close by upstream.
					return
				}

				// Nothing to do.
				if e.Object == nil {
					c <- e
					continue
				}

				// Type assert.
				cq, ok := e.Object.(*kueue.ClusterQueue)
				if !ok {
					c <- e
					continue
				}

				// Process bookmark.
				if e.Type == watch.Bookmark {
					systemmeta.UnnoteResource(cq)
					e.Object = &worker.InstanceType{ObjectMeta: cq.ObjectMeta}
					c <- e
					continue
				}

				// Convert.
				insType := convertInstanceTypeFromClusterQueue(cq)
				if insType == nil {
					continue
				}

				// Ignore if not be selected by `kubectl get --field-selector=metadata.namespace=...`.
				if !instanceTypeMatchFieldSelector(opts, insType) {
					continue
				}

				// Dispatch.
				e.Object = insType
				c <- e
			}
		}
	})

	return dw, nil
}

func (h *InstanceTypeHandler) OnGet(ctx context.Context, key types.NamespacedName, opts ctrlcli.GetOptions) (runtime.Object, error) {
	// Get.
	cq := &kueue.ClusterQueue{
		ObjectMeta: meta.ObjectMeta{
			Name: key.Name,
		},
	}
	err := h.APIReader.Get(ctx, ctrlcli.ObjectKeyFromObject(cq), cq, &opts)
	if err != nil {
		return nil, err
	}

	// Convert.
	insType := convertInstanceTypeFromClusterQueue(cq)
	if insType == nil {
		return nil, kerrors.NewNotFound(worker.Resource(_InstanceTypeResource), key.Name)
	}

	return insType, nil
}

func convertClusterQueueListOptsFromInstanceTypeListOpts(in ctrlcli.ListOptions) (out *ctrlcli.ListOptions) {
	// Add necessary label selector.
	if lbs := systemmeta.GetResourcesLabelSelectorOfType(_InstanceTypeResource); in.LabelSelector == nil {
		in.LabelSelector = lbs
	} else {
		reqs, _ := lbs.Requirements()
		in.LabelSelector = in.LabelSelector.DeepCopySelector().Add(reqs...)
	}

	return &in
}

func convertInstanceTypeFromClusterQueue(cq *kueue.ClusterQueue) *worker.InstanceType {
	if cq == nil {
		return nil
	}

	resType, notes := systemmeta.UnnoteResource(cq)
	if resType != _InstanceTypeResource {
		return nil
	}

	sliced := funcx.NoError(strconvx.Atoi[int64](notes["sliced"]))
	acceleratable := notes["acceleratable"] == "true"

	var (
		allAccelerator, remAccelerator, maxAccelerator    resource.Quantity
		allCpu, remCpu, maxCpu                            resource.Quantity
		allRam, remRam, maxRam                            resource.Quantity
		allLocalStorage, remLocalStorage, maxLocalStorage resource.Quantity
	)
	{
		resourceAccelerator := devicefeature.GetCreditsResourceName(notes["manufacturer"])

		flvQuantitiesIndex := make(map[kueue.ResourceFlavorReference]map[core.ResourceName]resource.Quantity)
		for i := range cq.Spec.ResourceGroups {
			rg := &cq.Spec.ResourceGroups[i]
			for j := range rg.Flavors {
				flv := &rg.Flavors[j]
				flvQuantitiesIndex[flv.Name] = make(map[core.ResourceName]resource.Quantity)
				for k := range flv.Resources {
					res := &flv.Resources[k]
					// Index nominal quota for each flavor for later use.
					flvQuantitiesIndex[flv.Name][res.Name] = res.NominalQuota
					// All quantities are added up.
					switch res.Name {
					case resourceAccelerator:
						allAccelerator.Add(res.NominalQuota)
					case core.ResourceCPU:
						allCpu.Add(res.NominalQuota)
					case core.ResourceMemory:
						allRam.Add(res.NominalQuota)
					case core.ResourceEphemeralStorage:
						allLocalStorage.Add(res.NominalQuota)
					}
				}
			}
		}

		remCpu = allCpu.DeepCopy()
		remRam = allRam.DeepCopy()
		remLocalStorage = allLocalStorage.DeepCopy()
		remAccelerator = allAccelerator.DeepCopy()

		var maxCpuTmp, maxRamTmp, maxLocalStorageTmp, maxAcceleratorTmp resource.Quantity
		for i := range cq.Status.FlavorsReservation {
			flv := &cq.Status.FlavorsReservation[i]
			flvQuantities := flvQuantitiesIndex[flv.Name]
			maxCpuTmp = flvQuantities[core.ResourceCPU]
			maxRamTmp = flvQuantities[core.ResourceMemory]
			maxLocalStorageTmp = flvQuantities[core.ResourceEphemeralStorage]
			maxAcceleratorTmp = flvQuantities[resourceAccelerator]
			for j := range flv.Resources {
				res := &flv.Resources[j]
				// Remaining quantities are subtracted by the reserved total quota.
				switch res.Name {
				case resourceAccelerator:
					remAccelerator.Sub(res.Total)
					maxAcceleratorTmp.Sub(res.Total)
				case core.ResourceCPU:
					remCpu.Sub(res.Total)
					maxCpuTmp.Sub(res.Total)
				case core.ResourceMemory:
					remRam.Sub(res.Total)
					maxRamTmp.Sub(res.Total)
				case core.ResourceEphemeralStorage:
					remLocalStorage.Sub(res.Total)
					maxLocalStorageTmp.Sub(res.Total)
				}
			}

			if !acceleratable {
				if maxRamTmp.Cmp(maxRam) > 0 &&
					!maxCpuTmp.IsZero() && !maxLocalStorageTmp.IsZero() {
					maxRam = maxRamTmp
					maxCpu = maxCpuTmp
					maxLocalStorage = maxLocalStorageTmp
				}
			} else if maxAcceleratorTmp.Cmp(maxAccelerator) > 0 &&
				!maxCpuTmp.IsZero() && !maxRamTmp.IsZero() && !maxLocalStorageTmp.IsZero() {
				maxAccelerator = maxAcceleratorTmp
				maxCpu = maxCpuTmp
				maxRam = maxRamTmp
				maxLocalStorage = maxLocalStorageTmp
			}
		}

		if sliced > 0 {
			// Only allow to request 1 slice at most.
			if !maxAccelerator.IsZero() {
				maxAccelerator.Set(1)
			}
			// Align the accelerator resource with the slice.
			remAccelerator = devicefeature.QuantityToSliceCount(remAccelerator, sliced)
			allAccelerator = devicefeature.QuantityToSliceCount(allAccelerator, sliced)
		} else {
			maxAccelerator = devicefeature.QuantityToSliceCount(maxAccelerator, 1)
		}
	}

	insType := &worker.InstanceType{
		ObjectMeta: cq.ObjectMeta,
		Spec: worker.InstanceTypeSpec{
			Group:             string(cq.Spec.CohortName),
			Acceleratable:     acceleratable,
			Manufacturer:      notes["manufacturer"],
			Product:           notes["product"],
			Memory:            notes["memory"],
			Family:            notes["family"],
			ComputeCapability: notes["computeCapability"],
			Sliced:            sliced,
			UnitResources: worker.InstanceTypeUnitResources{
				CPU: notes["unitResCPU"],
				RAM: notes["unitResRAM"],
			},
		},
		Status: worker.InstanceTypeStatus{
			Accelerator: worker.InstanceTypeResource{
				OnceMaxRequest: maxAccelerator,
				Remaining:      remAccelerator,
				Capacity:       allAccelerator,
			},
			CPU: worker.InstanceTypeResource{
				OnceMaxRequest: maxCpu,
				Remaining:      remCpu,
				Capacity:       allCpu,
			},
			RAM: worker.InstanceTypeResource{
				OnceMaxRequest: maxRam,
				Remaining:      remRam,
				Capacity:       allRam,
			},
			LocalStorage: worker.InstanceTypeResource{
				OnceMaxRequest: maxLocalStorage,
				Remaining:      remLocalStorage,
				Capacity:       allLocalStorage,
			},
		},
	}
	insType.Status.Phase, insType.Status.PhaseMessage = apistatus.GetSummaryOfClusterQueue(&cq.Status)

	return insType
}

func convertInstanceTypeListFromClusterQueueList(
	cqList *kueue.ClusterQueueList, opts ctrlcli.ListOptions,
) *worker.InstanceTypeList {
	if cqList == nil {
		return &worker.InstanceTypeList{}
	}

	itList := &worker.InstanceTypeList{
		ListMeta: cqList.ListMeta,
		Items:    make([]worker.InstanceType, 0, len(cqList.Items)),
	}

	for i := range cqList.Items {
		insType := convertInstanceTypeFromClusterQueue(&cqList.Items[i])
		if insType == nil {
			continue
		}

		// Ignore if not be selected by `kubectl get --field-selector=metadata.namespace=...`.
		if !instanceTypeMatchFieldSelector(opts, insType) {
			continue
		}
		itList.Items = append(itList.Items, *insType)
	}

	return itList
}

// instanceTypeMatchFieldSelector checks if the InstanceType matches the field selector in list options.
func instanceTypeMatchFieldSelector(opts ctrlcli.ListOptions, insType *worker.InstanceType) bool {
	fs := opts.FieldSelector
	if fs == nil {
		return true
	}
	return fs.Matches(fields.Set{"metadata.name": insType.Name})
}
