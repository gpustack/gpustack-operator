package worker

import (
	"context"
	"fmt"
	"net/url"
	"strconv"
	"strings"

	core "k8s.io/api/core/v1"
	storage "k8s.io/api/storage/v1"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/apimachinery/pkg/util/validation/field"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/apiserver/pkg/registry/rest"
	"k8s.io/utils/ptr"
	ctrlcli "sigs.k8s.io/controller-runtime/pkg/client"

	worker "gpustack.ai/gpustack/api/worker/v1"
	"gpustack.ai/gpustack/pkg/extensionapi"
	"gpustack.ai/gpustack/pkg/kubeclientset"
	"gpustack.ai/gpustack/pkg/kubemeta"
	"gpustack.ai/gpustack/pkg/systemmeta"
	"gpustack.ai/gpustack/pkg/utils/ctrlclix"
	"gpustack.ai/gpustack/pkg/utils/gox"
	"gpustack.ai/gpustack/pkg/worker/kuberess"
)

const (
	_InstancePersistentVolumeTypeResource = "instancepersistentvolumetypes"
	_InstancePersistentVolumeTypeKind     = "InstancePersistentVolumeType"
)

// InstancePersistentVolumeTypeHandler handles v1.InstancePersistentVolumeType objects.
//
// InstancePersistentVolumeTypeHandler maps the v1.InstancePersistentVolumeType to a Kubernetes StorageClass,
// which is named as the InstancePersistentVolumeType's name.
type InstancePersistentVolumeTypeHandler struct {
	extensionapi.ObjectInfo
	extensionapi.CurdOperations

	Client    ctrlcli.Client
	APIReader ctrlcli.Reader
}

func (h *InstancePersistentVolumeTypeHandler) SetupHandler(
	ctx context.Context,
	opts extensionapi.SetupOptions,
) (gvr schema.GroupVersionResource, srs map[string]rest.Storage, err error) {
	// Configure field indexer.
	fi := opts.Manager.GetFieldIndexer()
	err = fi.IndexField(ctx, &storage.StorageClass{}, "metadata.name",
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
	gvr = worker.SchemeGroupVersionResource(_InstancePersistentVolumeTypeResource)

	// Create table convertor to pretty the kubectl's output.
	tc, err := extensionapi.NewJSONPathTemplateTableConvertor(
		extensionapi.JSONPathTemplateTableColumnDefinition{
			TableColumnDefinition: meta.TableColumnDefinition{
				Name: "Type",
				Type: "string",
			},
			Render: func(obj runtime.Object) string {
				t := obj.(*worker.InstancePersistentVolumeType)
				switch {
				case t.Spec.NFS != nil:
					return "NFS"
				case t.Spec.S3 != nil:
					return "S3"
				}
				return ""
			},
		},
		extensionapi.JSONPathTemplateTableColumnDefinition{
			TableColumnDefinition: meta.TableColumnDefinition{
				Name: "Endpoint",
				Type: "string",
			},
			Render: func(obj runtime.Object) string {
				t := obj.(*worker.InstancePersistentVolumeType)
				switch {
				case t.Spec.NFS != nil:
					return fmt.Sprintf("%s (%s[%s])",
						t.Spec.NFS.Server, t.Spec.NFS.Share, t.Spec.NFS.SubDirectory)
				case t.Spec.S3 != nil:
					return fmt.Sprintf("%s (%s[%s])",
						t.Spec.S3.Endpoint, t.Spec.S3.Region, t.Spec.S3.Bucket)
				}
				return ""
			},
		},
		extensionapi.JSONPathTemplateTableColumnDefinition{
			TableColumnDefinition: meta.TableColumnDefinition{
				Name: "Account",
				Type: "string",
			},
			Render: func(obj runtime.Object) string {
				t := obj.(*worker.InstancePersistentVolumeType)
				if t.Spec.S3 != nil {
					return t.Spec.S3.AccessKey
				}
				return ""
			},
		})
	if err != nil {
		return gvr, srs, err
	}

	// As storage.
	h.ObjectInfo = &worker.InstancePersistentVolumeType{}
	h.CurdOperations = extensionapi.WithCurd(tc, h)

	// Set client.
	h.Client = opts.Manager.GetClient()
	h.APIReader = opts.Manager.GetAPIReader()

	return gvr, srs, err
}

var (
	_ rest.Storage           = (*InstancePersistentVolumeTypeHandler)(nil)
	_ rest.Creater           = (*InstancePersistentVolumeTypeHandler)(nil)
	_ rest.Lister            = (*InstancePersistentVolumeTypeHandler)(nil)
	_ rest.Watcher           = (*InstancePersistentVolumeTypeHandler)(nil)
	_ rest.Getter            = (*InstancePersistentVolumeTypeHandler)(nil)
	_ rest.Updater           = (*InstancePersistentVolumeTypeHandler)(nil)
	_ rest.Patcher           = (*InstancePersistentVolumeTypeHandler)(nil)
	_ rest.GracefulDeleter   = (*InstancePersistentVolumeTypeHandler)(nil)
	_ rest.CollectionDeleter = (*InstancePersistentVolumeTypeHandler)(nil)
)

func (h *InstancePersistentVolumeTypeHandler) New() runtime.Object {
	return &worker.InstancePersistentVolumeType{}
}

func (h *InstancePersistentVolumeTypeHandler) Destroy() {}

func (h *InstancePersistentVolumeTypeHandler) OnCreate(ctx context.Context, obj runtime.Object, opts ctrlcli.CreateOptions) (runtime.Object, error) {
	instPVType := obj.(*worker.InstancePersistentVolumeType)

	// Validate.
	if instPVType.Spec.NFS == nil && instPVType.Spec.S3 == nil {
		return nil, kerrors.NewBadRequest("one of NFS and S3 must be specified")
	}
	if instPVType.Spec.NFS != nil && instPVType.Spec.S3 != nil {
		return nil, kerrors.NewBadRequest("only one of NFS and S3 can be specified")
	}
	{
		var errs field.ErrorList
		switch {
		case instPVType.Spec.NFS != nil:
			if instPVType.Spec.NFS.Server == "" {
				errs = append(errs, field.Required(
					field.NewPath("spec.nfs.server"), "server is required"),
				)
			}
			if instPVType.Spec.NFS.Share == "" {
				errs = append(errs, field.Required(
					field.NewPath("spec.nfs.share"), "share is required"),
				)
			}
			if instPVType.Spec.NFS.MountPermissions != "" {
				_, err := strconv.ParseUint(instPVType.Spec.NFS.MountPermissions, 8, 64)
				if err != nil {
					errs = append(errs, field.Invalid(
						field.NewPath("spec.nfs.mountPermissions"),
						instPVType.Spec.NFS.MountPermissions, "mountPermissions must be an octal string"))
				}
			}
		case instPVType.Spec.S3 != nil:
			if instPVType.Spec.S3.Endpoint == "" {
				errs = append(errs, field.Required(
					field.NewPath("spec.s3.endpoint"), "endpoint is required"))
			} else if uri, err := url.ParseRequestURI(instPVType.Spec.S3.Endpoint); err != nil {
				errs = append(errs, field.Invalid(
					field.NewPath("spec.s3.endpoint"), instPVType.Spec.S3.Endpoint, "endpoint must be a valid URL"))
			} else if uri.Scheme != "http" && uri.Scheme != "https" {
				errs = append(errs, field.Invalid(
					field.NewPath("spec.s3.endpoint"), instPVType.Spec.S3.Endpoint, "endpoint must have http or https scheme"))
			}
			if instPVType.Spec.S3.Mounter != "" && !sets.New("geesefs", "rclone", "s3fs").Has(instPVType.Spec.S3.Mounter) {
				errs = append(errs, field.Invalid(
					field.NewPath("spec.s3.mounter"), instPVType.Spec.S3.Mounter, "unsupported mounter"))
			}
		}
		if len(errs) > 0 {
			return nil, kerrors.NewInvalid(worker.Kind(_InstancePersistentVolumeTypeKind), instPVType.Name, errs)
		}
	}

	// Default.
	switch {
	case instPVType.Spec.NFS != nil:
		if len(instPVType.Spec.NFS.MountOptions) == 0 {
			instPVType.Spec.NFS.MountOptions = []string{
				"hard",
				"vers=4",
				"rsize=1048576",
				"wsize=1048576",
				"noatime",
				"nodiratime",
			}
		}
	case instPVType.Spec.S3 != nil:
		if instPVType.Spec.S3.Mounter == "" {
			instPVType.Spec.S3.Mounter = "geesefs"
		}
		if len(instPVType.Spec.S3.MountOptions) == 0 {
			instPVType.Spec.S3.MountOptions = []string{
				"--no-checksum",
				"--memory-limit=4000",
				"--max-flushers=32",
				"--max-parallel-parts=32",
				"--part-sizes=25",
				"--list-type=2",
				"--no-specials",
			}
		}
	}

	// Create.
	stgCls := convertStorageClassFromInstancePersistentVolumeType(instPVType)
	err := h.Client.Create(ctx, stgCls)
	if err != nil {
		return nil, err
	}

	// Create asset.
	if instPVType.Spec.S3 != nil {
		eSec := getInstancePersistentVolumeTypeSourceS3AccessData(instPVType)
		kubemeta.ControlOnWithoutBlock(eSec, stgCls, worker.SchemeGroupVersionKind(_InstancePersistentVolumeTypeKind))
		secAlignFn := func(aSec *core.Secret) (_ *core.Secret, skip bool, err error) {
			skip = true
			// Update data.
			if !kubemeta.DeepEqual(aSec.Data, eSec.Data) {
				aSec.Data = eSec.Data
				skip = false
			}
			// Update owner reference.
			if !kubemeta.IsControlledBy(aSec, stgCls) {
				kubemeta.ControlOnWithoutBlock(aSec, stgCls, worker.SchemeGroupVersionKind(_InstancePersistentVolumeTypeKind))
				skip = false
			}
			return aSec, skip, nil
		}
		_, err = kubeclientset.CreateWithCtrlClient(ctx, h.Client, eSec,
			kubeclientset.WithUpdateIfExisted(secAlignFn))
		if err != nil {
			return nil, kerrors.NewInternalError(fmt.Errorf("create s3 access secret: %w", err))
		}
	}

	instPVType = convertInstancePersistentVolumeTypeFromStorageClass(stgCls)
	return instPVType, nil
}

func (h *InstancePersistentVolumeTypeHandler) NewList() runtime.Object {
	return &worker.InstancePersistentVolumeTypeList{}
}

func (h *InstancePersistentVolumeTypeHandler) OnList(ctx context.Context, opts ctrlcli.ListOptions) (runtime.Object, error) {
	// List.
	stgClsList := new(storage.StorageClassList)
	err := h.APIReader.List(ctx, stgClsList,
		convertStorageClassListOptsFromInstancePersistentVolumeTypeListOpts(opts))
	if err != nil {
		return nil, err
	}

	// Convert.
	instPVTypeList := convertInstancePersistentVolumeTypeListFromStorageClassList(stgClsList)
	return instPVTypeList, nil
}

func (h *InstancePersistentVolumeTypeHandler) OnWatch(ctx context.Context, opts ctrlcli.ListOptions) (watch.Interface, error) {
	// Watch.
	uw, err := h.Client.(ctrlcli.WithWatch).Watch(ctx, new(core.SecretList),
		convertStorageClassListOptsFromInstancePersistentVolumeTypeListOpts(opts))
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
				stgCls, ok := e.Object.(*storage.StorageClass)
				if !ok {
					c <- e
					continue
				}

				// Process bookmark.
				if e.Type == watch.Bookmark {
					systemmeta.UnnoteResource(stgCls)
					e.Object = &worker.InstancePersistentVolumeType{ObjectMeta: stgCls.ObjectMeta}
					c <- e
					continue
				}

				// Convert.
				instPVType := convertInstancePersistentVolumeTypeFromStorageClass(stgCls)
				if instPVType == nil {
					continue
				}

				// Ignore if not be selected by `kubectl get --field-selector=metadata.namespace=...`.
				if !instancePersistentVolumeTypeMatchFieldSelector(opts, instPVType) {
					continue
				}

				// Dispatch.
				e.Object = instPVType
				c <- e
			}
		}
	})

	return dw, nil
}

func (h *InstancePersistentVolumeTypeHandler) OnGet(ctx context.Context, key types.NamespacedName, opts ctrlcli.GetOptions) (runtime.Object, error) {
	// Get.
	stgCls := &storage.StorageClass{
		ObjectMeta: meta.ObjectMeta{
			Name: key.Name,
		},
	}
	err := h.APIReader.Get(ctx, ctrlcli.ObjectKeyFromObject(stgCls), stgCls)
	if err != nil {
		return nil, err
	}

	// Convert.
	instPVType := convertInstancePersistentVolumeTypeFromStorageClass(stgCls)
	if instPVType == nil {
		return nil, kerrors.NewNotFound(worker.Resource(_InstancePersistentVolumeTypeResource), key.Name)
	}

	return instPVType, nil
}

func (h *InstancePersistentVolumeTypeHandler) OnUpdate(
	ctx context.Context, obj, oldObj runtime.Object, opts ctrlcli.UpdateOptions,
) (runtime.Object, error) {
	instPVType, instPVTypeOld := obj.(*worker.InstancePersistentVolumeType), oldObj.(*worker.InstancePersistentVolumeType)

	// Validate.
	if instPVType.Spec.NFS != nil && instPVTypeOld.Spec.NFS == nil {
		return nil, field.Forbidden(
			field.NewPath("spec.nfs"), "cannot update to nfs from non-nfs")
	}
	if instPVType.Spec.S3 != nil && instPVTypeOld.Spec.S3 == nil {
		return nil, field.Forbidden(
			field.NewPath("spec.nfs"), "cannot update to s3 from non-s3")
	}
	switch {
	case instPVType.Spec.NFS != nil:
		if instPVType.Spec.NFS.Server != instPVTypeOld.Spec.NFS.Server {
			return nil, field.Forbidden(
				field.NewPath("spec.nfs.server"), "server is immutable")
		}
		if instPVType.Spec.NFS.Share != instPVTypeOld.Spec.NFS.Share {
			return nil, field.Forbidden(
				field.NewPath("spec.nfs.share"), "share is immutable")
		}
	case instPVType.Spec.S3 != nil:
		if instPVType.Spec.S3.Endpoint == "" {
			return nil, field.Required(
				field.NewPath("spec.s3.endpoint"), "endpoint is required")
		}
		if uri, err := url.ParseRequestURI(instPVType.Spec.S3.Endpoint); err != nil {
			return nil, field.Invalid(
				field.NewPath("spec.s3.endpoint"), instPVType.Spec.S3.Endpoint, "endpoint must be a valid URL")
		} else if uri.Scheme != "http" && uri.Scheme != "https" {
			return nil, field.Invalid(
				field.NewPath("spec.s3.endpoint"), instPVType.Spec.S3.Endpoint, "endpoint must have http or https scheme")
		}
		if instPVType.Spec.S3.Bucket != instPVTypeOld.Spec.S3.Bucket {
			return nil, field.Forbidden(
				field.NewPath("spec.s3.bucket"), "bucket is immutable")
		}
		if instPVType.Spec.S3.Prefix != instPVTypeOld.Spec.S3.Prefix {
			return nil, field.Forbidden(
				field.NewPath("spec.s3.prefix"), "prefix is immutable")
		}
		if instPVType.Spec.S3.Mounter != instPVTypeOld.Spec.S3.Mounter {
			return nil, field.Forbidden(
				field.NewPath("spec.s3.mounter"), "mounter is immutable")
		}
		if !kubemeta.DeepEqual(instPVType.Spec.S3.MountOptions, instPVTypeOld.Spec.S3.MountOptions) {
			return nil, field.Forbidden(
				field.NewPath("spec.s3.mountOptions"), "mountOptions is immutable")
		}
	}

	// Update.
	oldStgCls := new(storage.StorageClass)
	err := h.APIReader.Get(ctx, ctrlcli.ObjectKeyFromObject(instPVType), oldStgCls,
		ctrlclix.WithoutQuorum)
	if err != nil {
		return nil, err
	}

	stgCls := oldStgCls.DeepCopy()
	stgCls.ObjectMeta = instPVType.ObjectMeta
	switch {
	case instPVType.Spec.NFS != nil:
		stgCls.MountOptions = instPVType.Spec.NFS.MountOptions
		systemmeta.NoteResource(stgCls, _InstancePersistentVolumeTypeResource, map[string]string{
			"displayName": instPVType.Spec.DisplayName,
			"description": instPVType.Spec.Description,
		})
	case instPVType.Spec.S3 != nil:
		// Update asset.
		eSec := getInstancePersistentVolumeTypeSourceS3AccessData(instPVType)
		kubemeta.ControlOnWithoutBlock(eSec, oldStgCls, worker.SchemeGroupVersionKind(_InstancePersistentVolumeTypeKind))
		_, err := kubeclientset.UpdateWithCtrlClient(ctx, h.Client, eSec,
			kubeclientset.WithCreateIfNotExisted[*core.Secret]())
		if err != nil {
			return nil, kerrors.NewInternalError(fmt.Errorf("update s3 access secret: %w", err))
		}
		// Update other.
		systemmeta.NoteResource(stgCls, _InstancePersistentVolumeTypeResource, map[string]string{
			"displayName": instPVType.Spec.DisplayName,
			"description": instPVType.Spec.Description,
			"endpoint":    instPVType.Spec.S3.Endpoint,
			"region":      instPVType.Spec.S3.Region,
			"insecure":    strconv.FormatBool(instPVType.Spec.S3.Insecure),
			"accessKey":   instPVType.Spec.S3.AccessKey,
		})
	}

	err = h.Client.Patch(ctx, stgCls, ctrlcli.MergeFrom(oldStgCls),
		ctrlclix.ToPatchOptions(opts))
	if err != nil {
		return nil, kerrors.NewInternalError(fmt.Errorf("update corresponding storageclass: %w", err))
	}

	instPVType = convertInstancePersistentVolumeTypeFromStorageClass(stgCls)
	return instPVType, nil
}

func (h *InstancePersistentVolumeTypeHandler) OnDelete(ctx context.Context, obj runtime.Object, opts ctrlcli.DeleteOptions) error {
	instPVType := obj.(*worker.InstancePersistentVolumeType)

	// Delete.
	stgCls := &storage.StorageClass{ObjectMeta: instPVType.ObjectMeta}
	return h.Client.Delete(ctx, stgCls)
}

func convertStorageClassListOptsFromInstancePersistentVolumeTypeListOpts(in ctrlcli.ListOptions) (out *ctrlcli.ListOptions) {
	// Add necessary label selector.
	if lbs := systemmeta.GetResourcesLabelSelectorOfType(_InstancePersistentVolumeTypeResource); in.LabelSelector == nil {
		in.LabelSelector = lbs
	} else {
		reqs, _ := lbs.Requirements()
		in.LabelSelector = in.LabelSelector.DeepCopySelector().Add(reqs...)
	}

	return &in
}

func convertStorageClassFromInstancePersistentVolumeType(instPVType *worker.InstancePersistentVolumeType) *storage.StorageClass {
	stgCls := &storage.StorageClass{
		ObjectMeta:        instPVType.ObjectMeta,
		Parameters:        make(map[string]string),
		ReclaimPolicy:     ptr.To(core.PersistentVolumeReclaimDelete),
		VolumeBindingMode: ptr.To(storage.VolumeBindingImmediate),
	}

	switch {
	case instPVType.Spec.NFS != nil:
		stgCls.Provisioner = kuberess.CSIProvisionerNFS

		// Parameters.
		stgCls.Parameters["server"] = instPVType.Spec.NFS.Server
		stgCls.Parameters["share"] = instPVType.Spec.NFS.Share
		if instPVType.Spec.NFS.SubDirectory != "" {
			stgCls.Parameters["subDir"] = instPVType.Spec.NFS.SubDirectory
		}
		if instPVType.Spec.NFS.MountPermissions != "" {
			stgCls.Parameters["mountPermissions"] = instPVType.Spec.NFS.MountPermissions
		}

		// MountOptions.
		stgCls.MountOptions = instPVType.Spec.NFS.MountOptions

		systemmeta.NoteResource(stgCls, _InstancePersistentVolumeTypeResource, map[string]string{
			"displayName": instPVType.Spec.DisplayName,
			"description": instPVType.Spec.Description,
		})

	case instPVType.Spec.S3 != nil:
		stgCls.Provisioner = kuberess.CSIProvisionerS3

		// Parameters.
		stgCls.Parameters["mounter"] = instPVType.Spec.S3.Mounter
		stgCls.Parameters["options"] = strings.Join(instPVType.Spec.S3.MountOptions, " ")
		if instPVType.Spec.S3.Bucket != "" {
			stgCls.Parameters["bucket"] = instPVType.Spec.S3.Bucket
		}
		if instPVType.Spec.S3.Prefix != "" {
			stgCls.Parameters["prefix"] = instPVType.Spec.S3.Prefix
		}
		for _, s := range []string{
			"csi.storage.k8s.io/provisioner-secret-name",
			"csi.storage.k8s.io/controller-publish-secret-name",
			"csi.storage.k8s.io/node-stage-secret-name",
			"csi.storage.k8s.io/node-publish-secret-name",
		} {
			stgCls.Parameters[s] = "gpustack-pvt-s3-" + instPVType.Name
		}
		for _, s := range []string{
			"csi.storage.k8s.io/provisioner-secret-namespace",
			"csi.storage.k8s.io/controller-publish-secret-namespace",
			"csi.storage.k8s.io/node-stage-secret-namespace",
			"csi.storage.k8s.io/node-publish-secret-namespace",
		} {
			stgCls.Parameters[s] = kuberess.SystemNamespaceName
		}

		systemmeta.NoteResource(stgCls, _InstancePersistentVolumeTypeResource, map[string]string{
			"displayName": instPVType.Spec.DisplayName,
			"description": instPVType.Spec.Description,
			"endpoint":    instPVType.Spec.S3.Endpoint,
			"region":      instPVType.Spec.S3.Region,
			"insecure":    strconv.FormatBool(instPVType.Spec.S3.Insecure),
			"accessKey":   instPVType.Spec.S3.AccessKey,
		})
	}

	return stgCls
}

func convertInstancePersistentVolumeTypeFromStorageClass(stgCls *storage.StorageClass) *worker.InstancePersistentVolumeType {
	if stgCls == nil {
		return nil
	}

	resType, notes := systemmeta.UnnoteResource(stgCls)
	if resType != _InstancePersistentVolumeTypeResource {
		return nil
	}

	var volumeSource worker.InstancePersistentVolumeSource
	switch stgCls.Provisioner {
	default:
		return nil
	case kuberess.CSIProvisionerNFS:
		volumeSource.NFS = &worker.NFSInstancePersistentVolumeSource{
			Server:           stgCls.Parameters["server"],
			Share:            stgCls.Parameters["share"],
			SubDirectory:     stgCls.Parameters["subDir"],
			MountPermissions: stgCls.Parameters["mountPermissions"],
			MountOptions:     stgCls.MountOptions,
		}
	case kuberess.CSIProvisionerS3:
		volumeSource.S3 = &worker.S3InstancePersistentVolumeSource{
			Endpoint:     notes["endpoint"],
			Region:       notes["region"],
			Insecure:     notes["insecure"] == "true",
			AccessKey:    notes["accessKey"],
			Bucket:       stgCls.Parameters["bucket"],
			Prefix:       stgCls.Parameters["prefix"],
			MountOptions: strings.Split(stgCls.Parameters["options"], " "),
		}
	}

	return &worker.InstancePersistentVolumeType{
		ObjectMeta: stgCls.ObjectMeta,
		Spec: worker.InstancePersistentVolumeTypeSpec{
			DisplayName:                    notes["displayName"],
			Description:                    notes["description"],
			InstancePersistentVolumeSource: volumeSource,
		},
	}
}

func convertInstancePersistentVolumeTypeListFromStorageClassList(stgClsList *storage.StorageClassList) *worker.InstancePersistentVolumeTypeList {
	if stgClsList == nil {
		return &worker.InstancePersistentVolumeTypeList{}
	}

	instPVTypeList := &worker.InstancePersistentVolumeTypeList{
		ListMeta: stgClsList.ListMeta,
		Items:    make([]worker.InstancePersistentVolumeType, 0, len(stgClsList.Items)),
	}

	for i := range stgClsList.Items {
		instPVType := convertInstancePersistentVolumeTypeFromStorageClass(&stgClsList.Items[i])
		if instPVType == nil {
			continue
		}

		// Ignore if not be selected by `kubectl get --field-selector=metadata.namespace=...`.
		if !instancePersistentVolumeTypeMatchFieldSelector(ctrlcli.ListOptions{}, instPVType) {
			continue
		}

		instPVTypeList.Items = append(instPVTypeList.Items, *instPVType)
	}

	return instPVTypeList
}

func instancePersistentVolumeTypeMatchFieldSelector(opts ctrlcli.ListOptions, instPVType *worker.InstancePersistentVolumeType) bool {
	fs := opts.FieldSelector
	if fs == nil {
		return true
	}
	return fs.Matches(fields.Set{"metadata.name": instPVType.Name})
}

func getInstancePersistentVolumeTypeSourceS3AccessData(instPVType *worker.InstancePersistentVolumeType) *core.Secret {
	data := map[string][]byte{
		"endpoint": []byte(instPVType.Spec.S3.Endpoint),
	}
	if instPVType.Spec.S3.Region != "" {
		data["region"] = []byte(instPVType.Spec.S3.Region)
	}
	if instPVType.Spec.S3.Insecure {
		data["insecure"] = []byte("true")
	}
	if instPVType.Spec.S3.AccessKey != "" {
		data["accessKeyID"] = []byte(instPVType.Spec.S3.AccessKey)
	}
	if instPVType.Spec.S3.SecretKey != "" {
		data["secretAccessKey"] = []byte(instPVType.Spec.S3.SecretKey)
	}

	return &core.Secret{
		ObjectMeta: meta.ObjectMeta{
			Name:      "gpustack-pvt-s3-" + instPVType.Name,
			Namespace: kuberess.SystemNamespaceName,
		},
		Data: data,
	}
}
