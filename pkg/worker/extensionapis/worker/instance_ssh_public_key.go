package worker

import (
	"context"

	core "k8s.io/api/core/v1"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/apiserver/pkg/registry/rest"
	ctrlcli "sigs.k8s.io/controller-runtime/pkg/client"

	worker "gpustack.ai/gpustack/api/worker/v1"
	"gpustack.ai/gpustack/pkg/extensionapi"
	"gpustack.ai/gpustack/pkg/systemmeta"
	"gpustack.ai/gpustack/pkg/utils/gox"
	"gpustack.ai/gpustack/pkg/utils/stringx"
)

const _InstanceSSHPublicKeyResource = "instancesshpublickeys"

// InstanceSSHPublicKeyHandler handles v1.InstanceSSHPublicKey objects.
//
// InstanceSSHPublicKeyHandler maps the v1.InstanceSSHPublicKey to a Kubernetes Secret resource,
// which is named as the InstanceSSHPublicKey's name.
type InstanceSSHPublicKeyHandler struct {
	extensionapi.ObjectInfo
	extensionapi.CurdOperations

	Client    ctrlcli.Client
	APIReader ctrlcli.Reader
}

func (h *InstanceSSHPublicKeyHandler) SetupHandler(
	ctx context.Context,
	opts extensionapi.SetupOptions,
) (gvr schema.GroupVersionResource, srs map[string]rest.Storage, err error) {
	// Configure field indexer.
	fi := opts.Manager.GetFieldIndexer()
	err = fi.IndexField(ctx, &core.Secret{}, "metadata.name",
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
	gvr = worker.SchemeGroupVersionResource(_InstanceSSHPublicKeyResource)

	// Create table convertor to pretty the kubectl's output.
	tc := extensionapi.NewDefaultTableConvertor()

	// As storage.
	h.ObjectInfo = &worker.InstanceSSHPublicKey{}
	h.CurdOperations = extensionapi.WithCurd(tc, h)

	// Set client.
	h.Client = opts.Manager.GetClient()
	h.APIReader = opts.Manager.GetAPIReader()

	return gvr, srs, err
}

var (
	_ rest.Storage           = (*InstanceSSHPublicKeyHandler)(nil)
	_ rest.Creater           = (*InstanceSSHPublicKeyHandler)(nil)
	_ rest.Lister            = (*InstanceSSHPublicKeyHandler)(nil)
	_ rest.Watcher           = (*InstanceSSHPublicKeyHandler)(nil)
	_ rest.Getter            = (*InstanceSSHPublicKeyHandler)(nil)
	_ rest.Updater           = (*InstanceSSHPublicKeyHandler)(nil)
	_ rest.Patcher           = (*InstanceSSHPublicKeyHandler)(nil)
	_ rest.GracefulDeleter   = (*InstanceSSHPublicKeyHandler)(nil)
	_ rest.CollectionDeleter = (*InstanceSSHPublicKeyHandler)(nil)
)

func (h *InstanceSSHPublicKeyHandler) New() runtime.Object {
	return &worker.InstanceSSHPublicKey{}
}

func (h *InstanceSSHPublicKeyHandler) Destroy() {
}

func (h *InstanceSSHPublicKeyHandler) OnCreate(ctx context.Context, obj runtime.Object, opts ctrlcli.CreateOptions) (runtime.Object, error) {
	instSSHKey := obj.(*worker.InstanceSSHPublicKey)

	// Create.
	sec := convertSecretFromInstanceSSHPublicKey(instSSHKey)
	err := h.Client.Create(ctx, sec, &opts)
	if err != nil {
		return nil, err
	}

	instSSHKey = convertInstanceSSHPublicKeyFromSecret(sec)
	return instSSHKey, nil
}

func (h *InstanceSSHPublicKeyHandler) NewList() runtime.Object {
	return &worker.InstanceSSHPublicKeyList{}
}

func (h *InstanceSSHPublicKeyHandler) OnList(ctx context.Context, opts ctrlcli.ListOptions) (runtime.Object, error) {
	// List.
	secList := new(core.SecretList)
	err := h.APIReader.List(ctx, secList,
		convertSecretListOptsFromInstanceSSHPublicKeyListOpts(opts))
	if err != nil {
		return nil, err
	}

	// Convert.
	instSSHKeyList := convertInstanceSSHPublicKeyListFromSecretList(secList, opts)
	return instSSHKeyList, nil
}

func (h *InstanceSSHPublicKeyHandler) OnWatch(ctx context.Context, opts ctrlcli.ListOptions) (watch.Interface, error) {
	// Watch.
	uw, err := h.Client.(ctrlcli.WithWatch).Watch(ctx, new(core.SecretList),
		convertSecretListOptsFromInstanceSSHPublicKeyListOpts(opts))
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
				sec, ok := e.Object.(*core.Secret)
				if !ok {
					c <- e
					continue
				}

				// Process bookmark.
				if e.Type == watch.Bookmark {
					systemmeta.UnnoteResource(sec)
					e.Object = &worker.InstanceSSHPublicKey{ObjectMeta: sec.ObjectMeta}
					c <- e
					continue
				}

				// Convert.
				instSSHKey := convertInstanceSSHPublicKeyFromSecret(sec)
				if instSSHKey == nil {
					continue
				}

				// Ignore if not be selected by `kubectl get --field-selector=metadata.namespace=...`.
				if !instanceSSHPublicKeyMatchFieldSelector(opts, instSSHKey) {
					continue
				}

				// Dispatch.
				e.Object = instSSHKey
				c <- e
			}
		}
	})

	return dw, nil
}

func (h *InstanceSSHPublicKeyHandler) OnGet(ctx context.Context, key types.NamespacedName, opts ctrlcli.GetOptions) (runtime.Object, error) {
	// Get.
	sec := &core.Secret{
		ObjectMeta: meta.ObjectMeta{
			Name:      key.Name,
			Namespace: key.Namespace,
		},
	}
	err := h.APIReader.Get(ctx, ctrlcli.ObjectKeyFromObject(sec), sec, &opts)
	if err != nil {
		return nil, err
	}

	// Convert.
	instSSHKey := convertInstanceSSHPublicKeyFromSecret(sec)
	if instSSHKey == nil {
		return nil, kerrors.NewNotFound(worker.Resource(_InstanceSSHPublicKeyResource), key.Name)
	}

	return instSSHKey, nil
}

func (h *InstanceSSHPublicKeyHandler) OnUpdate(
	ctx context.Context, obj, oldObj runtime.Object, opts ctrlcli.UpdateOptions,
) (runtime.Object, error) {
	instSSHKey := obj.(*worker.InstanceSSHPublicKey)

	// Update.
	{
		sec := convertSecretFromInstanceSSHPublicKey(instSSHKey)
		err := h.Client.Update(ctx, sec, &opts)
		if err != nil {
			return nil, err
		}
		instSSHKey = convertInstanceSSHPublicKeyFromSecret(sec)
	}

	return instSSHKey, nil
}

func (h *InstanceSSHPublicKeyHandler) OnDelete(ctx context.Context, obj runtime.Object, opts ctrlcli.DeleteOptions) error {
	instSSHKey := obj.(*worker.InstanceSSHPublicKey)

	// Delete.
	sec := &core.Secret{ObjectMeta: instSSHKey.ObjectMeta}
	return h.Client.Delete(ctx, sec, &opts)
}

func convertSecretListOptsFromInstanceSSHPublicKeyListOpts(in ctrlcli.ListOptions) (out *ctrlcli.ListOptions) {
	// Add necessary label selector.
	if lbs := systemmeta.GetResourcesLabelSelectorOfType(_InstanceSSHPublicKeyResource); in.LabelSelector == nil {
		in.LabelSelector = lbs
	} else {
		reqs, _ := lbs.Requirements()
		in.LabelSelector = in.LabelSelector.DeepCopySelector().Add(reqs...)
	}

	return &in
}

func convertSecretFromInstanceSSHPublicKey(instSSHKey *worker.InstanceSSHPublicKey) *core.Secret {
	sec := &core.Secret{
		ObjectMeta: instSSHKey.ObjectMeta,
		Data:       map[string][]byte{"authorized-keys": stringx.ToBytes(&instSSHKey.Spec.Data)},
	}

	systemmeta.NoteResource(sec, _InstanceSSHPublicKeyResource, map[string]string{
		"displayName": instSSHKey.Spec.DisplayName,
		"description": instSSHKey.Spec.Description,
	})

	return sec
}

func convertInstanceSSHPublicKeyFromSecret(sec *core.Secret) *worker.InstanceSSHPublicKey {
	if sec == nil {
		return nil
	}

	resType, notes := systemmeta.UnnoteResource(sec)
	if resType != _InstanceSSHPublicKeyResource {
		return nil
	}

	var secData string
	if v, ok := sec.Data["authorized-keys"]; ok {
		secData = stringx.FromBytes(&v)
	}

	return &worker.InstanceSSHPublicKey{
		ObjectMeta: sec.ObjectMeta,
		Spec: worker.InstanceSSHPublicKeySpec{
			DisplayName: notes["displayName"],
			Description: notes["description"],
			Data:        secData,
		},
	}
}

func convertInstanceSSHPublicKeyListFromSecretList(secList *core.SecretList, opts ctrlcli.ListOptions) *worker.InstanceSSHPublicKeyList {
	if secList == nil {
		return &worker.InstanceSSHPublicKeyList{}
	}

	instSSHKeyList := &worker.InstanceSSHPublicKeyList{
		ListMeta: secList.ListMeta,
		Items:    make([]worker.InstanceSSHPublicKey, 0, len(secList.Items)),
	}

	for i := range secList.Items {
		instSSHKey := convertInstanceSSHPublicKeyFromSecret(&secList.Items[i])
		if instSSHKey == nil {
			continue
		}

		// Ignore if not be selected by `kubectl get --field-selector=metadata.namespace=...`.
		if !instanceSSHPublicKeyMatchFieldSelector(opts, instSSHKey) {
			continue
		}

		instSSHKeyList.Items = append(instSSHKeyList.Items, *instSSHKey)
	}

	return instSSHKeyList
}

// instanceSSHPublicKeyMatchFieldSelector checks if the InstanceSSHPublicKey matches the field select in list options.
func instanceSSHPublicKeyMatchFieldSelector(opts ctrlcli.ListOptions, instSSHKey *worker.InstanceSSHPublicKey) bool {
	fs := opts.FieldSelector
	if fs == nil {
		return true
	}
	return fs.Matches(fields.Set{"metadata.namespace": instSSHKey.Namespace, "metadata.name": instSSHKey.Name})
}
