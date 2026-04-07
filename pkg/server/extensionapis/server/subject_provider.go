package server

import (
	"context"
	"fmt"
	"slices"
	"sort"
	"strings"

	core "k8s.io/api/core/v1"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation/field"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/apiserver/pkg/registry/rest"
	ctrlcli "sigs.k8s.io/controller-runtime/pkg/client"

	server "gpustack.ai/gpustack/api/server/v1"
	"gpustack.ai/gpustack/pkg/extensionapi"
	"gpustack.ai/gpustack/pkg/kubemeta"
	"gpustack.ai/gpustack/pkg/server/kuberess"
	"gpustack.ai/gpustack/pkg/systemmeta"
	"gpustack.ai/gpustack/pkg/utils/gox"
	"gpustack.ai/gpustack/pkg/utils/json"
	"gpustack.ai/gpustack/pkg/utils/stringx"
)

const _SubjectProviderResource = "subjectproviders"

// SubjectProviderHandler handles v1.SubjectProvider objects.
//
// SubjectProviderHandler maps the v1.SubjectProvider object to a Kubernetes Secret resource,
// which is named as "gpustack-subjectprovider-<name>".
type SubjectProviderHandler struct {
	extensionapi.ObjectInfo
	extensionapi.CurdOperations

	Client    ctrlcli.Client
	APIReader ctrlcli.Reader
}

func (h *SubjectProviderHandler) SetupHandler(
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
		return gvr, srs, fmt.Errorf("index secret 'metadata.name': %w", err)
	}

	// Declare GVR.
	gvr = server.SchemeGroupVersionResource(_SubjectProviderResource)

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
				Name: "Password Login",
				Type: "boolean",
			},
			JSONPath: ".status.loginWithPassword",
		})
	if err != nil {
		return gvr, srs, err
	}

	// As storage.
	h.ObjectInfo = &server.SubjectProvider{}
	h.CurdOperations = extensionapi.WithCurd(tc, h)

	// Set client.
	h.Client = opts.Manager.GetClient()
	h.APIReader = opts.Manager.GetAPIReader()

	return gvr, srs, err
}

var (
	_ rest.Storage           = (*SubjectProviderHandler)(nil)
	_ rest.Creater           = (*SubjectProviderHandler)(nil)
	_ rest.Lister            = (*SubjectProviderHandler)(nil)
	_ rest.Watcher           = (*SubjectProviderHandler)(nil)
	_ rest.Getter            = (*SubjectProviderHandler)(nil)
	_ rest.Updater           = (*SubjectProviderHandler)(nil)
	_ rest.Patcher           = (*SubjectProviderHandler)(nil)
	_ rest.GracefulDeleter   = (*SubjectProviderHandler)(nil)
	_ rest.CollectionDeleter = (*SubjectProviderHandler)(nil)
)

func (h *SubjectProviderHandler) New() runtime.Object {
	return &server.SubjectProvider{}
}

func (h *SubjectProviderHandler) Destroy() {
}

func (h *SubjectProviderHandler) OnCreate(ctx context.Context, obj runtime.Object, opts ctrlcli.CreateOptions) (runtime.Object, error) {
	// Validate.
	subjProv := obj.(*server.SubjectProvider)
	{
		var errs field.ErrorList
		if subjProv.Namespace != kuberess.SystemNamespaceName {
			errs = append(errs, field.Invalid(
				field.NewPath("metadata.namespace"), subjProv.Namespace, "subject provider namespace must be "+kuberess.SystemNamespaceName))
		}
		if stringx.StringWidth(subjProv.Name) > 30 {
			errs = append(errs, field.TooLong(
				field.NewPath("metadata.name"), stringx.StringWidth(subjProv.Name), 30))
		}
		switch {
		case subjProv.Name != kuberess.DefaultSubjectProviderName && subjProv.Spec.Type == server.SubjectProviderTypeInternal:
			errs = append(errs, field.Invalid(
				field.NewPath("spec.type"), subjProv.Spec.Type, "internal subject provider must be named as "+kuberess.DefaultSubjectProviderName))
		case subjProv.Name == kuberess.DefaultSubjectProviderName && subjProv.Spec.Type != server.SubjectProviderTypeInternal:
			errs = append(errs, field.Invalid(
				field.NewPath("spec.type"), subjProv.Spec.Type, "default subject provider must be internal"))
		}
		if err := subjProv.Spec.Type.Validate(); err != nil {
			errs = append(errs, field.Invalid(
				field.NewPath("spec.type"), subjProv.Spec.Type, err.Error()))
		}
		if stringx.StringWidth(subjProv.Spec.DisplayName) > 30 {
			errs = append(errs, field.TooLong(
				field.NewPath("spec.displayName"), stringx.StringWidth(subjProv.Spec.DisplayName), 30))
		}
		if stringx.StringWidth(subjProv.Spec.Description) > 50 {
			errs = append(errs, field.TooLong(
				field.NewPath("spec.description"), stringx.StringWidth(subjProv.Spec.Description), 50))
		}
		if err := subjProv.Spec.ExternalConfig.ValidateWithType(subjProv.Spec.Type); err != nil {
			errs = append(errs, field.Invalid(
				field.NewPath("spec.externalConfig"), subjProv.Spec.ExternalConfig, err.Error()))
		}
		if len(errs) > 0 {
			return nil, kerrors.NewInvalid(server.Kind(_SubjectProviderResource), subjProv.Name, errs)
		}
	}

	// Create.
	{
		sec := convertSecretFromSubjectProvider(subjProv)
		err := h.Client.Create(ctx, sec, &opts)
		if err != nil {
			return nil, err
		}
		subjProv = convertSubjectProviderFromSecret(sec)
	}

	return subjProv, nil
}

func (h *SubjectProviderHandler) NewList() runtime.Object {
	return &server.SubjectProviderList{}
}

func (h *SubjectProviderHandler) OnList(ctx context.Context, opts ctrlcli.ListOptions) (runtime.Object, error) {
	// List.
	secList := new(core.SecretList)
	err := h.APIReader.List(ctx, secList,
		convertSecretListOptsFromSubjectProviderListOpts(opts))
	if err != nil {
		return nil, err
	}

	// Convert.
	spList := convertSubjectProviderListFromSecretList(secList, opts)
	return spList, nil
}

func (h *SubjectProviderHandler) OnWatch(ctx context.Context, opts ctrlcli.ListOptions) (watch.Interface, error) {
	// Watch.
	uw, err := h.Client.(ctrlcli.WithWatch).Watch(ctx, new(core.SecretList),
		convertSecretListOptsFromSubjectProviderListOpts(opts))
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
					resType := systemmeta.DescribeResourceType(sec)
					if resType == _SubjectProviderResource {
						e.Object = &server.SubjectProvider{ObjectMeta: sec.ObjectMeta}
						c <- e
					}
					continue
				}

				// Convert.
				subj := convertSubjectProviderFromSecret(sec)
				if subj == nil {
					continue
				}

				// Dispatch.
				e.Object = subj
				c <- e
			}
		}
	})

	return dw, nil
}

func (h *SubjectProviderHandler) OnGet(ctx context.Context, key types.NamespacedName, opts ctrlcli.GetOptions) (runtime.Object, error) {
	// Get.
	sec := &core.Secret{
		ObjectMeta: meta.ObjectMeta{
			Namespace: key.Namespace,
			Name:      _SubjectProviderDelegatedSecretNamePrefix + key.Name,
		},
	}
	err := h.APIReader.Get(ctx, ctrlcli.ObjectKeyFromObject(sec), sec, &opts)
	if err != nil {
		return nil, kerrors.NewNotFound(server.Resource(_SubjectProviderResource), key.Name)
	}

	// Convert.
	proj := convertSubjectProviderFromSecret(sec)
	if proj == nil {
		return nil, kerrors.NewNotFound(server.Resource(_SubjectProviderResource), key.Name)
	}
	return proj, nil
}

func (h *SubjectProviderHandler) OnUpdate(ctx context.Context, obj, oldObj runtime.Object, opts ctrlcli.UpdateOptions) (runtime.Object, error) {
	// Validate.
	subjProv := obj.(*server.SubjectProvider)
	{
		oldSubjProvider := oldObj.(*server.SubjectProvider)
		var errs field.ErrorList
		if subjProv.Spec.Type != oldSubjProvider.Spec.Type {
			errs = append(errs, field.Invalid(
				field.NewPath("spec.type"), subjProv.Spec.Type, "type is immutable"))
		}
		if stringx.StringWidth(subjProv.Spec.DisplayName) > 30 {
			errs = append(errs, field.TooLong(
				field.NewPath("spec.displayName"), stringx.StringWidth(subjProv.Spec.DisplayName), 30))
		}
		if stringx.StringWidth(subjProv.Spec.Description) > 50 {
			errs = append(errs, field.TooLong(
				field.NewPath("spec.description"), stringx.StringWidth(subjProv.Spec.Description), 50))
		}
		if err := subjProv.Spec.ExternalConfig.ValidateWithType(subjProv.Spec.Type); err != nil {
			errs = append(errs, field.Invalid(
				field.NewPath("spec.externalConfig"), subjProv.Spec.ExternalConfig, err.Error()))
		}
		if len(errs) > 0 {
			return nil, kerrors.NewInvalid(server.Kind(_SubjectProviderResource), subjProv.Name, errs)
		}
	}

	// Update.
	{
		sec := convertSecretFromSubjectProvider(subjProv)
		err := h.Client.Update(ctx, sec, &opts)
		if err != nil {
			return nil, err
		}
		subjProv = convertSubjectProviderFromSecret(sec)
	}

	return subjProv, nil
}

func (h *SubjectProviderHandler) OnDelete(ctx context.Context, obj runtime.Object, opts ctrlcli.DeleteOptions) error {
	subjProv := obj.(*server.SubjectProvider)

	// Validate.
	{
		// Prevent deleting default subject provider.
		if subjProv.Name == kuberess.DefaultSubjectProviderName {
			return kerrors.NewBadRequest("default subject provider is reserved")
		}
	}

	// Delete.
	sec := &core.Secret{ObjectMeta: subjProv.ObjectMeta}
	sec.Name = _SubjectProviderDelegatedSecretNamePrefix + subjProv.Name
	return h.Client.Delete(ctx, sec, &opts)
}

func convertSecretListOptsFromSubjectProviderListOpts(in ctrlcli.ListOptions) (out *ctrlcli.ListOptions) {
	if in.Namespace != kuberess.SystemNamespaceName {
		return &in
	}

	// Ignore name selector
	if in.FieldSelector != nil {
		reqs := slices.DeleteFunc(in.FieldSelector.Requirements(), func(req fields.Requirement) bool {
			return req.Field == "metadata.name"
		})
		if len(reqs) == 0 {
			in.FieldSelector = nil
		} else {
			in.FieldSelector = kubemeta.FieldSelectorFromRequirements(reqs)
		}
	}

	// Add necessary label selector.
	if lbs := systemmeta.GetResourcesLabelSelectorOfType(_SubjectProviderResource); in.LabelSelector == nil {
		in.LabelSelector = lbs
	} else {
		reqs, _ := lbs.Requirements()
		in.LabelSelector = in.LabelSelector.DeepCopySelector().Add(reqs...)
	}

	return &in
}

const _SubjectProviderDelegatedSecretNamePrefix = "gpustack-subjectprovider-"

func convertSecretFromSubjectProvider(subjProv *server.SubjectProvider) *core.Secret {
	sec := &core.Secret{
		ObjectMeta: subjProv.ObjectMeta,
	}
	systemmeta.NoteResource(sec, _SubjectProviderResource, map[string]string{
		"type":        subjProv.Spec.Type.String(),
		"displayName": subjProv.Spec.DisplayName,
		"description": subjProv.Spec.Description,
	})
	sec.Data = map[string][]byte{
		"externalConfig": json.MustMarshal(subjProv.Spec.ExternalConfig),
	}
	sec.Name = _SubjectProviderDelegatedSecretNamePrefix + subjProv.Name
	return sec
}

func convertSubjectProviderFromSecret(sec *core.Secret) *server.SubjectProvider {
	if sec == nil {
		return nil
	}

	resType, notes := systemmeta.UnnoteResource(sec)
	if resType != _SubjectProviderResource {
		return nil
	}
	if !strings.HasPrefix(sec.Name, _SubjectProviderDelegatedSecretNamePrefix) {
		return nil
	}

	subjProv := &server.SubjectProvider{
		ObjectMeta: sec.ObjectMeta,
		Spec: server.SubjectProviderSpec{
			Type:        server.SubjectProviderType(notes["type"]),
			DisplayName: notes["displayName"],
			Description: notes["description"],
		},
	}
	if sec.Data != nil && sec.Data["externalConfig"] != nil {
		json.ShouldUnmarshal(sec.Data["externalConfig"], &subjProv.Spec.ExternalConfig)
	}
	subjProv.Name = strings.TrimPrefix(sec.Name, _SubjectProviderDelegatedSecretNamePrefix)
	switch subjProv.Spec.Type {
	case server.SubjectProviderTypeInternal, server.SubjectProviderTypeLDAP:
		subjProv.Status.LoginWithPassword = true
	}
	return subjProv
}

func convertSubjectProviderListFromSecretList(secList *core.SecretList, opts ctrlcli.ListOptions) *server.SubjectProviderList {
	if secList == nil {
		return &server.SubjectProviderList{}
	}

	// Sort by resource version.
	sort.SliceStable(secList.Items, func(i, j int) bool {
		l, r := secList.Items[i].ResourceVersion, secList.Items[j].ResourceVersion
		return len(l) < len(r) ||
			(len(l) == len(r) && l < r)
	})

	spList := &server.SubjectProviderList{
		ListMeta: secList.ListMeta,
		Items:    make([]server.SubjectProvider, 0, len(secList.Items)),
	}

	for i := range secList.Items {
		subjProv := convertSubjectProviderFromSecret(&secList.Items[i])
		if subjProv == nil {
			continue
		}
		// Ignore if not be selected by `kubectl get --field-selector=metadata.namespace=...`.
		if !subjectProjectMatchFieldSelector(opts, subjProv) {
			continue
		}
		spList.Items = append(spList.Items, *subjProv)
	}

	return spList
}

// subjectProjectMatchFieldSelector checks if the SubjectProvider matches the field selector in list options.
func subjectProjectMatchFieldSelector(opts ctrlcli.ListOptions, subjProv *server.SubjectProvider) bool {
	fs := opts.FieldSelector
	if fs == nil {
		return true
	}
	return fs.Matches(fields.Set{"metadata.namespace": subjProv.Namespace, "metadata.name": subjProv.Name})
}
