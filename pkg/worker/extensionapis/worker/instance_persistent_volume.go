package worker

import (
	"context"
	"fmt"

	core "k8s.io/api/core/v1"
	storage "k8s.io/api/storage/v1"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation/field"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/apiserver/pkg/registry/rest"
	"k8s.io/utils/ptr"
	ctrlcli "sigs.k8s.io/controller-runtime/pkg/client"

	worker "gpustack.ai/gpustack/api/worker/v1"
	"gpustack.ai/gpustack/pkg/extensionapi"
	"gpustack.ai/gpustack/pkg/systemmeta"
	"gpustack.ai/gpustack/pkg/utils/ctrlclix"
	"gpustack.ai/gpustack/pkg/utils/gox"
)

const (
	_InstancePersistentVolumeResource = "instancepersistentvolumes"
	_InstancePersistentVolumeKind     = "InstancePersistentVolume"
)

// InstancePersistentVolumeHandler handles v1.InstancePersistentVolume objects.
//
// InstancePersistentVolumeHandler maps the v1.InstancePersistentVolume to a Kubernetes PersistentVolumeClaim resource,
// which is named as the InstancePersistentVolume's name.
type InstancePersistentVolumeHandler struct {
	extensionapi.ObjectInfo
	extensionapi.CurdOperations

	Client    ctrlcli.Client
	APIReader ctrlcli.Reader
}

func (h *InstancePersistentVolumeHandler) SetupHandler(
	ctx context.Context,
	opts extensionapi.SetupOptions,
) (gvr schema.GroupVersionResource, srs map[string]rest.Storage, err error) {
	// Configure field indexer.
	fi := opts.Manager.GetFieldIndexer()
	err = fi.IndexField(ctx, &core.PersistentVolumeClaim{}, "metadata.name",
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
	gvr = worker.SchemeGroupVersionResource(_InstancePersistentVolumeResource)

	// Create table convertor to pretty the kubectl's output.
	tc, err := extensionapi.NewJSONPathTableConvertor(
		extensionapi.JSONPathTableColumnDefinition{
			TableColumnDefinition: meta.TableColumnDefinition{
				Name: "Type",
				Type: "string",
			},
			JSONPath: ".spec.type",
		},
		extensionapi.JSONPathTableColumnDefinition{
			TableColumnDefinition: meta.TableColumnDefinition{
				Name: "Capacity",
				Type: "string",
			},
			JSONPath: ".spec.capacity",
		},
		extensionapi.JSONPathTableColumnDefinition{
			TableColumnDefinition: meta.TableColumnDefinition{
				Name: "Access-Mode",
				Type: "string",
			},
			JSONPath: ".spec.accessMode",
		},
		extensionapi.JSONPathTableColumnDefinition{
			TableColumnDefinition: meta.TableColumnDefinition{
				Name: "Reclaim-Policy",
				Type: "string",
			},
			JSONPath: ".spec.reclaimPolicy",
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
	h.ObjectInfo = &worker.InstancePersistentVolume{}
	h.CurdOperations = extensionapi.WithCurd(tc, h)

	// Set client.
	h.Client = opts.Manager.GetClient()
	h.APIReader = opts.Manager.GetAPIReader()

	// Create subresource handlers.
	srs = map[string]rest.Storage{
		"events": newInstancePersistentVolumeEventsHandler(h.ObjectInfo, opts),
	}

	return gvr, srs, err
}

var (
	_ rest.Storage           = (*InstancePersistentVolumeHandler)(nil)
	_ rest.Creater           = (*InstancePersistentVolumeHandler)(nil)
	_ rest.Lister            = (*InstancePersistentVolumeHandler)(nil)
	_ rest.Watcher           = (*InstancePersistentVolumeHandler)(nil)
	_ rest.Getter            = (*InstancePersistentVolumeHandler)(nil)
	_ rest.Updater           = (*InstancePersistentVolumeHandler)(nil)
	_ rest.Patcher           = (*InstancePersistentVolumeHandler)(nil)
	_ rest.GracefulDeleter   = (*InstancePersistentVolumeHandler)(nil)
	_ rest.CollectionDeleter = (*InstancePersistentVolumeHandler)(nil)
)

func (h *InstancePersistentVolumeHandler) New() runtime.Object {
	return &worker.InstancePersistentVolume{}
}

func (h *InstancePersistentVolumeHandler) Destroy() {
}

const (
	_IsDefaultStorageClassAnnotation     = "storageclass.kubernetes.io/is-default-class"
	_BetaIsDefaultStorageClassAnnotation = "storageclass.beta.kubernetes.io/is-default-class"
)

func (h *InstancePersistentVolumeHandler) OnCreate(ctx context.Context, obj runtime.Object, opts ctrlcli.CreateOptions) (runtime.Object, error) {
	instPV := obj.(*worker.InstancePersistentVolume)

	// Validate.
	var stgClass *storage.StorageClass
	if ptr.Deref(instPV.Spec.Type, "") == "" {
		stgClassList := new(storage.StorageClassList)
		err := h.Client.List(ctx, stgClassList,
			ctrlclix.NonQuorum)
		if err != nil {
			return nil, field.InternalError(
				field.NewPath("spec.type"), fmt.Errorf("list storage classes: %w", err))
		}
		for i := range stgClassList.Items {
			if len(stgClassList.Items[i].Annotations) == 0 {
				continue
			}
			if stgClassList.Items[i].Annotations[_IsDefaultStorageClassAnnotation] == "true" ||
				stgClassList.Items[i].Annotations[_BetaIsDefaultStorageClassAnnotation] == "true" {
				stgClass = &stgClassList.Items[i]
				break
			}
		}
		if stgClass == nil {
			return nil, field.Required(
				field.NewPath("spec.type"), "type is required when there is no default storage class")
		}
	} else {
		stgClass = new(storage.StorageClass)
		err := h.Client.Get(ctx, types.NamespacedName{Name: *instPV.Spec.Type}, stgClass)
		if err != nil {
			if !kerrors.IsNotFound(err) {
				return nil, field.InternalError(
					field.NewPath("spec.type"), fmt.Errorf("get storage class: %w", err))
			}
			return nil, field.NotFound(
				field.NewPath("spec.type"), *instPV.Spec.Type)
		}
	}

	// Default.
	if instPV.Spec.Capacity.IsZero() {
		instPV.Spec.Capacity = *resource.NewQuantity(20*1<<30, resource.BinarySI)
	}
	if instPV.Spec.AccessMode == nil {
		if ptr.Deref(stgClass.VolumeBindingMode, storage.VolumeBindingImmediate) == storage.VolumeBindingImmediate {
			if ptr.Deref(stgClass.AllowVolumeExpansion, false) {
				instPV.Spec.AccessMode = ptr.To(core.ReadWriteOnce)
			} else {
				instPV.Spec.AccessMode = ptr.To(core.ReadWriteMany)
			}
		} else {
			instPV.Spec.AccessMode = ptr.To(core.ReadWriteOnce)
		}
	}

	// Create.
	pvc := convertPersistentVolumeClaimFromInstancePersistentVolume(instPV)
	err := h.Client.Create(ctx, pvc, &opts)
	if err != nil {
		return nil, err
	}

	instPV = convertInstancePersistentVolumeFromPersistentVolumeClaim(pvc)
	return instPV, nil
}

func (h *InstancePersistentVolumeHandler) NewList() runtime.Object {
	return &worker.InstancePersistentVolumeList{}
}

func (h *InstancePersistentVolumeHandler) OnList(ctx context.Context, opts ctrlcli.ListOptions) (runtime.Object, error) {
	// List.
	pvcList := new(core.PersistentVolumeClaimList)
	err := h.APIReader.List(ctx, pvcList,
		convertPersistentVolumeClaimListOptsFromInstancePersistentVolumeListOpts(opts))
	if err != nil {
		return nil, err
	}

	// Convert.
	itList := convertInstancePersistentVolumeListFromPersistentVolumeClaimList(pvcList, opts)
	return itList, nil
}

func (h *InstancePersistentVolumeHandler) OnWatch(ctx context.Context, opts ctrlcli.ListOptions) (watch.Interface, error) {
	// Watch.
	uw, err := h.Client.(ctrlcli.WithWatch).Watch(ctx, new(core.PersistentVolumeClaimList),
		convertPersistentVolumeClaimListOptsFromInstancePersistentVolumeListOpts(opts))
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
				pvc, ok := e.Object.(*core.PersistentVolumeClaim)
				if !ok {
					c <- e
					continue
				}

				// Process bookmark.
				if e.Type == watch.Bookmark {
					systemmeta.UnnoteResource(pvc)
					e.Object = &worker.InstancePersistentVolume{ObjectMeta: pvc.ObjectMeta}
					c <- e
					continue
				}

				// Convert.
				instPV := convertInstancePersistentVolumeFromPersistentVolumeClaim(pvc)
				if instPV == nil {
					continue
				}

				// Ignore if not be selected by `kubectl get --field-selector=metadata.namespace=...`.
				if !instancePersistentVolumeMatchFieldSelector(opts, instPV) {
					continue
				}

				// Dispatch.
				e.Object = instPV
				c <- e
			}
		}
	})

	return dw, nil
}

func (h *InstancePersistentVolumeHandler) OnGet(ctx context.Context, key types.NamespacedName, opts ctrlcli.GetOptions) (runtime.Object, error) {
	// Get.
	pvc := &core.PersistentVolumeClaim{
		ObjectMeta: meta.ObjectMeta{
			Name:      key.Name,
			Namespace: key.Namespace,
		},
	}
	err := h.APIReader.Get(ctx, ctrlcli.ObjectKeyFromObject(pvc), pvc, &opts)
	if err != nil {
		return nil, err
	}

	// Convert.
	instPV := convertInstancePersistentVolumeFromPersistentVolumeClaim(pvc)
	if instPV == nil {
		return nil, kerrors.NewNotFound(worker.Resource(_InstancePersistentVolumeResource), key.Name)
	}

	return instPV, nil
}

func (h *InstancePersistentVolumeHandler) OnUpdate(
	ctx context.Context, obj, oldObj runtime.Object, opts ctrlcli.UpdateOptions,
) (runtime.Object, error) {
	instPV, instPVOld := obj.(*worker.InstancePersistentVolume), oldObj.(*worker.InstancePersistentVolume)

	// Validate.
	var errs field.ErrorList
	if !ptr.Equal(instPV.Spec.Type, instPVOld.Spec.Type) {
		errs = append(errs, field.Forbidden(
			field.NewPath("spec.type"), "field is immutable"))
	}
	if !instPV.Spec.Capacity.Equal(instPVOld.Spec.Capacity) {
		errs = append(errs, field.Forbidden(
			field.NewPath("spec.capacity"), "field is immutable"))
	}
	if !ptr.Equal(instPV.Spec.AccessMode, instPVOld.Spec.AccessMode) {
		errs = append(errs, field.Forbidden(
			field.NewPath("spec.accessMode"), "field is immutable"))
	}
	if len(errs) > 0 {
		return nil, kerrors.NewInvalid(worker.Kind("InstancePersistentVolume"), instPV.Name, errs)
	}

	// Update.
	oldPvc := new(core.PersistentVolumeClaim)
	err := h.APIReader.Get(ctx, ctrlcli.ObjectKeyFromObject(instPV), oldPvc,
		ctrlclix.NonQuorum)
	if err != nil {
		return nil, err
	}

	pvc := oldPvc.DeepCopy()
	pvc.ObjectMeta = instPV.ObjectMeta
	systemmeta.NoteResource(pvc, _InstancePersistentVolumeResource, map[string]string{
		"displayName": instPV.Spec.DisplayName,
		"description": instPV.Spec.Description,
	})

	err = h.Client.Patch(ctx, pvc, ctrlcli.MergeFrom(oldPvc),
		ctrlclix.ToPatchOptions(opts))
	if err != nil {
		return nil, kerrors.NewInternalError(fmt.Errorf("update corresponding persistent volume claim: %w", err))
	}

	instPV = convertInstancePersistentVolumeFromPersistentVolumeClaim(pvc)
	return instPV, nil
}

func (h *InstancePersistentVolumeHandler) OnDelete(ctx context.Context, obj runtime.Object, opts ctrlcli.DeleteOptions) error {
	instPV := obj.(*worker.InstancePersistentVolume)

	// Delete.
	pvc := &core.PersistentVolumeClaim{ObjectMeta: instPV.ObjectMeta}
	return h.Client.Delete(ctx, pvc, &opts)
}

func convertPersistentVolumeClaimListOptsFromInstancePersistentVolumeListOpts(in ctrlcli.ListOptions) (out *ctrlcli.ListOptions) {
	// Add necessary label selector.
	if lbs := systemmeta.GetResourcesLabelSelectorOfType(_InstancePersistentVolumeResource); in.LabelSelector == nil {
		in.LabelSelector = lbs
	} else {
		reqs, _ := lbs.Requirements()
		in.LabelSelector = in.LabelSelector.DeepCopySelector().Add(reqs...)
	}

	return &in
}

func convertPersistentVolumeClaimFromInstancePersistentVolume(instPV *worker.InstancePersistentVolume) *core.PersistentVolumeClaim {
	pvc := &core.PersistentVolumeClaim{
		ObjectMeta: instPV.ObjectMeta,
		Spec: core.PersistentVolumeClaimSpec{
			StorageClassName: instPV.Spec.Type,
			AccessModes:      []core.PersistentVolumeAccessMode{*instPV.Spec.AccessMode},
			Resources: core.VolumeResourceRequirements{
				Requests: core.ResourceList{
					core.ResourceStorage: instPV.Spec.Capacity,
				},
			},
		},
	}

	systemmeta.NoteResource(pvc, _InstancePersistentVolumeResource, map[string]string{
		"displayName": instPV.Spec.DisplayName,
		"description": instPV.Spec.Description,
	})

	return pvc
}

func convertInstancePersistentVolumeFromPersistentVolumeClaim(pvc *core.PersistentVolumeClaim) *worker.InstancePersistentVolume {
	if pvc == nil {
		return nil
	}

	resType, notes := systemmeta.UnnoteResource(pvc)
	if resType != _InstancePersistentVolumeResource {
		return nil
	}

	var phase string
	switch pvc.Status.Phase {
	case core.ClaimBound:
		phase = "Available"
	case core.ClaimLost:
		phase = "Unavailable"
	default:
		phase = "Pending"
	}

	return &worker.InstancePersistentVolume{
		ObjectMeta: pvc.ObjectMeta,
		Spec: worker.InstancePersistentVolumeSpec{
			DisplayName: notes["displayName"],
			Description: notes["description"],
			Type:        pvc.Spec.StorageClassName,
			Capacity:    pvc.Spec.Resources.Requests[core.ResourceStorage],
			AccessMode:  &pvc.Spec.AccessModes[0],
		},
		Status: worker.InstancePersistentVolumeStatus{
			Phase: phase,
			Volume: func() *core.ObjectReference {
				if pvc.Spec.VolumeName == "" {
					return nil
				}
				return &core.ObjectReference{
					Name: pvc.Spec.VolumeName,
				}
			}(),
		},
	}
}

func convertInstancePersistentVolumeListFromPersistentVolumeClaimList(
	pvcList *core.PersistentVolumeClaimList, opts ctrlcli.ListOptions,
) *worker.InstancePersistentVolumeList {
	if pvcList == nil {
		return &worker.InstancePersistentVolumeList{}
	}

	instPVList := &worker.InstancePersistentVolumeList{
		ListMeta: pvcList.ListMeta,
		Items:    make([]worker.InstancePersistentVolume, 0, len(pvcList.Items)),
	}

	for i := range pvcList.Items {
		instPV := convertInstancePersistentVolumeFromPersistentVolumeClaim(&pvcList.Items[i])
		if instPV == nil {
			continue
		}

		// Ignore if not be selected by `kubectl get --field-selector=metadata.namespace=...`.
		if !instancePersistentVolumeMatchFieldSelector(opts, instPV) {
			continue
		}

		instPVList.Items = append(instPVList.Items, *instPV)
	}

	return instPVList
}

func instancePersistentVolumeMatchFieldSelector(opts ctrlcli.ListOptions, instPV *worker.InstancePersistentVolume) bool {
	fs := opts.FieldSelector
	if fs == nil {
		return true
	}
	return fs.Matches(fields.Set{"metadata.namespace": instPV.Namespace, "metadata.name": instPV.Name})
}
