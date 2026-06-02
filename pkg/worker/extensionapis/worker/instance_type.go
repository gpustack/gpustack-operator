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

	_, profSpec, ok := devicefeature.ParseNodeProfile(cq.Name)
	if !ok {
		return nil
	}
	cpuUnit, ramUnit := profSpec.CPU, profSpec.RAM

	sliced := funcx.NoError(strconvx.Atoi[int64](notes["sliced"]))
	acceleratable := notes["acceleratable"] == "true"

	var (
		capAcc, remAcc, ormAcc resource.Quantity
		capCpu, remCpu, ormCpu resource.Quantity
		capRam, remRam, ormRam resource.Quantity
		capStg, remStg, ormStg resource.Quantity
	)
	{
		resourceAccelerator := devicefeature.GetCreditsResourceName(notes["manufacturer"])

		// Index quantities for later use.
		ormRfIndexer := make(map[kueue.ResourceFlavorReference]map[core.ResourceName]resource.Quantity)
		capRfIndexer := make(map[kueue.ResourceFlavorReference]map[core.ResourceName]resource.Quantity)
		for i := range cq.Spec.ResourceGroups {
			rg := &cq.Spec.ResourceGroups[i]
			for j := range rg.Flavors {
				flv := &rg.Flavors[j]

				// Index the once max request for later use.
				ormAccRf, ormCpuRf, ormRamRf, ormStgRf, ok := parseNodeResourceFlavorName(string(flv.Name))
				if !ok {
					continue
				}
				ormRfIndexer[flv.Name] = map[core.ResourceName]resource.Quantity{
					resourceAccelerator:           ormAccRf,
					core.ResourceCPU:              ormCpuRf,
					core.ResourceMemory:           ormRamRf,
					core.ResourceEphemeralStorage: ormStgRf,
				}

				// Calculate the once max request by comparing the once max request of each flavor if there is no reservation.
				if len(cq.Status.FlavorsReservation) == 0 {
					if !acceleratable {
						if ormCpuRf.Cmp(ormCpu) > 0 {
							ormCpu = ormCpuRf
							ormRam = ormRamRf
							ormStg = ormStgRf
						}
					} else if ormAccRf.Cmp(ormAcc) > 0 {
						ormAcc = ormAccRf
						ormCpu = ormCpuRf
						ormRam = ormRamRf
						ormStg = ormStgRf
					}
				}

				// Index the capacity for later use,
				// and meanwhile add up the nominal quota to get the total nominal quota for each resource.
				capRfIndexer[flv.Name] = make(map[core.ResourceName]resource.Quantity)
				for k := range flv.Resources {
					res := &flv.Resources[k]
					capRfIndexer[flv.Name][res.Name] = res.NominalQuota
					switch res.Name {
					case resourceAccelerator:
						capAcc.Add(res.NominalQuota)
					case core.ResourceCPU:
						capCpu.Add(res.NominalQuota)
					case core.ResourceMemory:
						capRam.Add(res.NominalQuota)
					case core.ResourceEphemeralStorage:
						capStg.Add(res.NominalQuota)
					}
				}
			}
		}

		// Initialize the remaining with the capacity.
		remAcc = capAcc.DeepCopy()
		remCpu = capCpu.DeepCopy()
		remRam = capRam.DeepCopy()
		remStg = capStg.DeepCopy()

		// Calculate the remaining for each flavor by subtracting the reserved total quota from the capacity,
		// and meanwhile adjust the once max request if necessary.
		//
		// If the reservation not exists,
		// the once max request is determined by comparing the once max request of each flavor;
		// otherwise, the once max request is determined by comparing the remaining of each flavor after reservation,
		// because the reservation will reduce the remaining and thus may reduce the once max request.
		for i := range cq.Status.FlavorsReservation {
			flv := &cq.Status.FlavorsReservation[i]

			capRf := capRfIndexer[flv.Name]
			remAccRf := capRf[resourceAccelerator]
			remCpuRf := capRf[core.ResourceCPU]
			remRamRf := capRf[core.ResourceMemory]
			remStgRf := capRf[core.ResourceEphemeralStorage]

			// Remaining are subtracted by the reserved total quota.
			for j := range flv.Resources {
				res := &flv.Resources[j]
				switch res.Name {
				case resourceAccelerator:
					remAcc.Sub(res.Total)
					remAccRf.Sub(res.Total)
				case core.ResourceCPU:
					remCpu.Sub(res.Total)
					remCpuRf.Sub(res.Total)
				case core.ResourceMemory:
					remRam.Sub(res.Total)
					remRamRf.Sub(res.Total)
				case core.ResourceEphemeralStorage:
					remStg.Sub(res.Total)
					remStgRf.Sub(res.Total)
				}
			}

			ormRf := ormRfIndexer[flv.Name]
			ormAccRf := ormRf[resourceAccelerator]
			ormCpuRf := ormRf[core.ResourceCPU]
			ormRamRf := ormRf[core.ResourceMemory]
			ormStgRf := ormRf[core.ResourceEphemeralStorage]

			// Adjust the remaining by comparing with the once max request.
			if remAccRf.Cmp(ormAccRf) > 0 {
				remAccRf = ormAccRf
			}
			if remCpuRf.Cmp(ormCpuRf) > 0 {
				remCpuRf = ormCpuRf
			}
			if remRamRf.Cmp(ormRamRf) > 0 {
				remRamRf = ormRamRf
			}
			if remStgRf.Cmp(ormStgRf) > 0 {
				remStgRf = ormStgRf
			}

			// Adjust the display once max request.
			if !acceleratable {
				if remCpuRf.Cmp(ormCpu) > 0 {
					ormCpu = remCpuRf
					ormRam = remRamRf
					ormStg = remStgRf
				}
			} else if remAccRf.Cmp(ormAcc) > 0 {
				ormAcc = remAccRf
				ormCpu = remCpuRf
				ormRam = remRamRf
				ormStg = remStgRf
			}
		}

		if sliced > 0 {
			// Only allow to request 1 slice at most.
			if !ormAcc.IsZero() {
				ormAcc.Set(1)
			}
			// Align the accelerator resource with the slice.
			remAcc = devicefeature.QuantityToSliceCount(remAcc, sliced)
			capAcc = devicefeature.QuantityToSliceCount(capAcc, sliced)
		} else {
			ormAcc = devicefeature.QuantityToSliceCount(ormAcc, 1)
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
				CPU: cpuUnit,
				RAM: ramUnit + "Gi",
			},
		},
		Status: worker.InstanceTypeStatus{
			Accelerator: worker.InstanceTypeResource{
				OnceMaxRequest: ormAcc,
				Remaining:      remAcc,
				Capacity:       capAcc,
			},
			CPU: worker.InstanceTypeResource{
				OnceMaxRequest: ormCpu,
				Remaining:      remCpu,
				Capacity:       capCpu,
			},
			RAM: worker.InstanceTypeResource{
				OnceMaxRequest: ormRam,
				Remaining:      remRam,
				Capacity:       capRam,
			},
			LocalStorage: worker.InstanceTypeResource{
				OnceMaxRequest: ormStg,
				Remaining:      remStg,
				Capacity:       capStg,
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

func parseNodeResourceFlavorName(name string) (ormAccRf, ormCpuRf, ormRamRf, ormStgRf resource.Quantity, ok bool) {
	_, spec, ok := devicefeature.ParseNodeProfile(name)
	if ok {
		if spec.Accelerator != "" {
			ormAccRf = funcx.NoError(resource.ParseQuantity(spec.Accelerator))
		}
		ormCpuRf = funcx.NoError(resource.ParseQuantity(spec.CPU))
		ormRamRf = funcx.NoError(resource.ParseQuantity(spec.RAM + "Gi"))
		ormStgRf = funcx.NoError(resource.ParseQuantity(spec.LocalStorage + "Gi"))
	}
	return ormAccRf, ormCpuRf, ormRamRf, ormStgRf, ok
}
