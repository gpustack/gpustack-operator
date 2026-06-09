package extensionapi

import (
	"context"
	"errors"
	"fmt"

	core "k8s.io/api/core/v1"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/apiserver/pkg/registry/rest"
	ctrlcli "sigs.k8s.io/controller-runtime/pkg/client"

	gpustack "gpustack.ai/gpustack/api/v1"
	"gpustack.ai/gpustack/pkg/kubemeta"
	"gpustack.ai/gpustack/pkg/setting"
	"gpustack.ai/gpustack/pkg/systemmeta"
	"gpustack.ai/gpustack/pkg/utils/gox"
)

const _SettingResource = "settings"

// SettingHandler handles v1.Setting objects.
//
// SettingHandler maps all v1.Setting objects to a Kubernetes Secret resource,
// which is named as "gpustack-system/gpustack-settings".
//
// Each v1.Setting object records as a key-value pair in the Secret's Data field.
type SettingHandler struct {
	index setting.IndexFunc

	ObjectInfo
	ListWatchOperation
	GetOperation
	UpdateOperation

	Client    ctrlcli.Client
	APIReader ctrlcli.Reader
}

// NewSettingHandler creates a new SettingHandler.
func NewSettingHandler(index setting.IndexFunc) *SettingHandler {
	return &SettingHandler{
		index: index,
	}
}

func (h *SettingHandler) SetupHandler(
	ctx context.Context,
	opts SetupOptions,
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
		return gvr, srs, fmt.Errorf("index secret 'metadata.name': %w", err)
	}

	// Declare GVR.
	gvr = gpustack.SchemeGroupVersionResource(_SettingResource)

	// Create table convertor to pretty the kubectl's output.
	tc, err := NewJSONPathTableConvertor(
		JSONPathTableColumnDefinition{
			TableColumnDefinition: meta.TableColumnDefinition{
				Name: "Value",
				Type: "string",
			},
			JSONPath: ".status.value",
		})
	if err != nil {
		return gvr, srs, err
	}

	// As storage.
	h.ObjectInfo = &gpustack.Setting{}
	h.ListWatchOperation = WithListWatch(tc, h)
	h.GetOperation = WithGet(h)
	h.UpdateOperation = WithUpdate(h)

	// Set client.
	h.Client = opts.Manager.GetClient()
	h.APIReader = opts.Manager.GetAPIReader()

	return gvr, srs, err
}

var (
	_ rest.Storage = (*SettingHandler)(nil)
	_ rest.Lister  = (*SettingHandler)(nil)
	_ rest.Watcher = (*SettingHandler)(nil)
	_ rest.Getter  = (*SettingHandler)(nil)
	_ rest.Updater = (*SettingHandler)(nil)
	_ rest.Patcher = (*SettingHandler)(nil)
)

func (h *SettingHandler) New() runtime.Object {
	return &gpustack.Setting{}
}

func (h *SettingHandler) Destroy() {}

func (h *SettingHandler) NewList() runtime.Object {
	return &gpustack.SettingList{}
}

func (h *SettingHandler) OnList(ctx context.Context, opts ctrlcli.ListOptions) (runtime.Object, error) {
	// Support watch with `kubectl get -A`.
	if opts.Namespace == "" {
		opts.Namespace = setting.DelegatedSecretNamespace
	}

	// Get.
	sec := &core.Secret{
		ObjectMeta: meta.ObjectMeta{
			Namespace: opts.Namespace,
			Name:      setting.DelegatedSecretName,
		},
	}
	err := h.APIReader.Get(ctx, ctrlcli.ObjectKeyFromObject(sec), sec)
	if err != nil {
		if !kerrors.IsNotFound(err) {
			return nil, err
		}
		// We return an empty list if the secret is not found.
		return &gpustack.SettingList{}, nil
	}

	// Convert.
	sList := convertSettingListFromSecret(sec, opts, h.index)
	return sList, nil
}

func (h *SettingHandler) OnWatch(ctx context.Context, opts ctrlcli.ListOptions) (watch.Interface, error) {
	// Support watch with `kubectl get -A`.
	if opts.Namespace == "" {
		opts.Namespace = setting.DelegatedSecretNamespace
	}

	// List and index.
	setIndexer := map[string]gpustack.Setting{} // [sn] -> set
	{
		listObj, err := h.OnList(ctx, opts)
		if err != nil {
			return nil, err
		}
		sList := listObj.(*gpustack.SettingList)
		for i := range sList.Items {
			setIndexKey := sList.Items[i].Name
			setIndexer[setIndexKey] = sList.Items[i]
		}
	}

	// Watch.
	uw, err := h.Client.(ctrlcli.WithWatch).Watch(ctx, new(core.SecretList),
		convertSecretListOptsFromSettingListOpts(opts))
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
					e.Object = &gpustack.Setting{ObjectMeta: sec.ObjectMeta}
					c <- e
					continue
				}

				// Send.
				for name := range sec.Data {
					// Ignore if not be selected by `kubectl get --field-selector=metadata.name=...`.
					if !settingMatchFieldSelector(opts, name) {
						continue
					}

					// Convert.
					set := convertSettingFromSecret(sec, name, h.index)
					if set == nil {
						continue
					}

					// Ignore if the same as previous.
					setIndexKey := set.Name
					prevSet, ok := setIndexer[setIndexKey]
					switch {
					default:
						// ignore
						continue
					case !ok:
						// insert
						setIndexer[setIndexKey] = *set
					case set.Status.Value_ != prevSet.Status.Value_:
						// update
						setIndexer[setIndexKey] = *set
					}

					// Dispatch.
					e2 := e.DeepCopy()
					e2.Object = set
					c <- *e2
				}
			}
		}
	})

	return dw, nil
}

func (h *SettingHandler) OnGet(ctx context.Context, key types.NamespacedName, opts ctrlcli.GetOptions) (runtime.Object, error) {
	// Get.
	sec := &core.Secret{
		ObjectMeta: meta.ObjectMeta{
			Namespace: key.Namespace,
			Name:      setting.DelegatedSecretName,
		},
	}
	err := h.APIReader.Get(ctx, ctrlcli.ObjectKeyFromObject(sec), sec)
	if err != nil {
		return nil, err
	}

	// Convert.
	set := convertSettingFromSecret(sec, key.Name, h.index)
	if set == nil {
		return nil, kerrors.NewNotFound(gpustack.Resource(_SettingResource), key.Name)
	}
	return set, nil
}

func (h *SettingHandler) OnUpdate(ctx context.Context, obj, _ runtime.Object, opts ctrlcli.UpdateOptions) (runtime.Object, error) {
	// Validate.
	set := obj.(*gpustack.Setting)
	if set.Namespace != setting.DelegatedSecretNamespace {
		return nil, kerrors.NewNotFound(gpustack.Resource(_SettingResource), set.Name)
	}
	s, ok := h.index(set.Name)
	if !ok || !s.Editable() {
		return nil, kerrors.NewForbidden(gpustack.Resource(_SettingResource), set.Name,
			errors.New("setting is not editable"))
	}

	// Update.
	if set.Spec.Value != nil {
		err := s.Configure(ctx, *set.Spec.Value)
		if err != nil {
			return nil, kerrors.NewConflict(gpustack.Resource(_SettingResource), set.Name, err)
		}
	}

	// Get.
	return h.OnGet(ctx, ctrlcli.ObjectKeyFromObject(set),
		ctrlcli.GetOptions{
			Raw: &meta.GetOptions{
				ResourceVersion: "0",
			},
		})
}

func convertSecretListOptsFromSettingListOpts(in ctrlcli.ListOptions) (out *ctrlcli.ListOptions) {
	// Lock field selector.
	in.FieldSelector = fields.SelectorFromSet(fields.Set{
		"metadata.namespace": in.Namespace,
		"metadata.name":      setting.DelegatedSecretName,
	})

	// Add necessary label selector.
	if lbs := systemmeta.GetResourcesLabelSelectorOfType(_SettingResource); in.LabelSelector == nil {
		in.LabelSelector = lbs
	} else {
		reqs, _ := lbs.Requirements()
		in.LabelSelector = in.LabelSelector.DeepCopySelector().Add(reqs...)
	}

	return &in
}

func convertSettingFromSecret(sec *core.Secret, reqName string, index setting.IndexFunc) *gpustack.Setting {
	resType := systemmeta.DescribeResourceType(sec)
	if resType != _SettingResource {
		return nil
	}

	// Filter out.
	s, ok := index(reqName)
	if !ok || s.Private() || sec.Data == nil {
		return nil
	}

	uid := sec.UID
	if uidS := systemmeta.DescribeResourceNote(sec, reqName+"-uid"); len(uidS) != 0 {
		uid = types.UID(uidS)
	}
	var (
		value  = []byte("")
		value_ = sec.Data[reqName]
	)
	if len(value_) != 0 && s.Sensitive() {
		value = []byte("(sensitive)")
	} else if len(value_) != 0 {
		value = value_
	}

	set := &gpustack.Setting{
		ObjectMeta: meta.ObjectMeta{
			Namespace:         sec.Namespace,
			Name:              reqName,
			UID:               uid,
			ResourceVersion:   sec.ResourceVersion,
			CreationTimestamp: sec.CreationTimestamp,
			DeletionTimestamp: sec.DeletionTimestamp,
		},
		Status: gpustack.SettingStatus{
			Description: s.Description(),
			Editable:    s.Editable(),
			Sensitive:   s.Sensitive(),
			Value:       string(value),
			Value_:      string(value_),
		},
	}

	kubemeta.OverwriteLastAppliedAnnotation(set)
	return set
}

func convertSettingListFromSecret(sec *core.Secret, opts ctrlcli.ListOptions, index setting.IndexFunc) *gpustack.SettingList {
	resType, secNotes := systemmeta.DescribeResource(sec)
	if resType != _SettingResource {
		return &gpustack.SettingList{}
	}

	sList := &gpustack.SettingList{
		ListMeta: meta.ListMeta{
			ResourceVersion: sec.ResourceVersion,
		},
		Items: make([]gpustack.Setting, 0, len(sec.Data)),
	}

	// Sort by name.
	for _, name := range sets.List(sets.KeySet(sec.Data)) {
		// Ignore if not be selected by `kubectl get --field-selector=metadata.name=...`.
		if !settingMatchFieldSelector(opts, name) {
			continue
		}

		// Filter out.
		s, ok := index(name)
		if !ok || s.Private() {
			continue
		}

		uid := sec.UID
		if uidS := secNotes[name+"-uid"]; len(uidS) != 0 {
			uid = types.UID(uidS)
		}
		var (
			value  = []byte("")
			value_ = sec.Data[name]
		)
		if len(value_) != 0 && s.Sensitive() {
			value = []byte("(sensitive)")
		} else if len(value_) != 0 {
			value = value_
		}

		set := &gpustack.Setting{
			ObjectMeta: meta.ObjectMeta{
				Namespace:         sec.Namespace,
				Name:              name,
				UID:               uid,
				ResourceVersion:   sec.ResourceVersion,
				CreationTimestamp: sec.CreationTimestamp,
				DeletionTimestamp: sec.DeletionTimestamp,
			},
			Status: gpustack.SettingStatus{
				Description: s.Description(),
				Editable:    s.Editable(),
				Sensitive:   s.Sensitive(),
				Value:       string(value),
				Value_:      string(value_),
			},
		}

		kubemeta.ConfigureLastAppliedAnnotation(set)
		sList.Items = append(sList.Items, *set)
	}

	return sList
}

// settingMatchFieldSelector checks if the Setting matches the field selector in list options.
func settingMatchFieldSelector(opts ctrlcli.ListOptions, name string) bool {
	fs := opts.FieldSelector
	if fs == nil {
		return true
	}
	return fs.Matches(fields.Set{"metadata.name": name})
}
