package server

import (
	"context"
	"fmt"
	"slices"
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
	"gpustack.ai/gpustack/pkg/utils/stringx"
)

const _ProjectResource = "projects"

// ProjectHandler handles v1.Team objects.
//
// ProjectHandler maps the v1.Team object to a Kubernetes Namespace resource,
// which is name as the project's name.
//
// Each v1.Project object will be controlled by a v1.Team object,
// which records in the OwnerReferences of the Namespace resource.
type ProjectHandler struct {
	extensionapi.ObjectInfo
	extensionapi.CurdOperations

	Client    ctrlcli.Client
	APIReader ctrlcli.Reader
}

func (h *ProjectHandler) SetupHandler(
	ctx context.Context,
	opts extensionapi.SetupOptions,
) (gvr schema.GroupVersionResource, srs map[string]rest.Storage, err error) {
	// Configure field indexer.
	fi := opts.Manager.GetFieldIndexer()
	err = fi.IndexField(ctx, &core.Namespace{}, "metadata.name",
		func(obj ctrlcli.Object) []string {
			if obj == nil {
				return nil
			}
			return []string{obj.GetName()}
		})
	if err != nil {
		return gvr, srs, fmt.Errorf("index namespace 'metadata.name': %w", err)
	}

	// Declare GVR.
	gvr = server.SchemeGroupVersionResource(_ProjectResource)

	// Create table convertor to pretty the kubectl's output.
	tc, err := extensionapi.NewJSONPathTableConvertor(
		extensionapi.JSONPathTableColumnDefinition{
			TableColumnDefinition: meta.TableColumnDefinition{
				Name: "Team",
				Type: "string",
			},
			JSONPath: ".status.team",
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
	h.ObjectInfo = &server.Project{}
	h.CurdOperations = extensionapi.WithCurd(tc, h)

	// Set client.
	h.Client = opts.Manager.GetClient()
	h.APIReader = opts.Manager.GetAPIReader()

	// Create subresource handlers.
	srs = map[string]rest.Storage{
		"clusters": newProjectClustersHandler(h.ObjectInfo, opts),
	}

	return gvr, srs, err
}

var (
	_ rest.Storage           = (*ProjectHandler)(nil)
	_ rest.Creater           = (*ProjectHandler)(nil)
	_ rest.Lister            = (*ProjectHandler)(nil)
	_ rest.Watcher           = (*ProjectHandler)(nil)
	_ rest.Getter            = (*ProjectHandler)(nil)
	_ rest.Updater           = (*ProjectHandler)(nil)
	_ rest.Patcher           = (*ProjectHandler)(nil)
	_ rest.GracefulDeleter   = (*ProjectHandler)(nil)
	_ rest.CollectionDeleter = (*ProjectHandler)(nil)
)

func (h *ProjectHandler) New() runtime.Object {
	return &server.Project{}
}

func (h *ProjectHandler) Destroy() {}

func (h *ProjectHandler) OnCreate(ctx context.Context, obj runtime.Object, opts ctrlcli.CreateOptions) (runtime.Object, error) {
	// Validate.
	proj := obj.(*server.Project)
	{
		var errs field.ErrorList
		if !strings.HasPrefix(proj.Name, proj.Namespace+"-") {
			errs = append(errs, field.Invalid(
				field.NewPath("name"), proj.Name, "name must start with the namespace"))
		}
		if stringx.StringWidth(proj.Name) > 30 {
			errs = append(errs, field.TooLong(
				field.NewPath("name"), stringx.StringWidth(proj.Name), 30))
		}
		if stringx.StringWidth(proj.Spec.DisplayName) > 30 {
			errs = append(errs, field.TooLong(
				field.NewPath("spec.displayName"), stringx.StringWidth(proj.Spec.DisplayName), 30))
		}
		if stringx.StringWidth(proj.Spec.Description) > 50 {
			errs = append(errs, field.TooLong(
				field.NewPath("spec.description"), stringx.StringWidth(proj.Spec.Description), 50))
		}
		if len(errs) > 0 {
			return nil, kerrors.NewInvalid(server.Kind(_ProjectResource), proj.Name, errs)
		}
	}

	// Get team.
	team := &server.Team{
		ObjectMeta: meta.ObjectMeta{
			Namespace: kuberess.SystemNamespaceName,
			Name:      proj.Namespace,
		},
	}
	err := h.APIReader.Get(ctx, ctrlcli.ObjectKeyFromObject(team), team)
	if err != nil {
		return nil, kerrors.NewNotFound(server.Resource("teams"), team.Name)
	}

	// Create.
	ns := convertNamespaceFromProject(proj)
	kubemeta.ControlOn(ns, team, server.SchemeGroupVersionKind("Team"))
	err = h.Client.Create(ctx, ns, &opts)
	if err != nil {
		return nil, err
	}

	proj = convertProjectFromNamespace(ns)
	return proj, nil
}

func (h *ProjectHandler) NewList() runtime.Object {
	return &server.ProjectList{}
}

func (h *ProjectHandler) OnList(ctx context.Context, opts ctrlcli.ListOptions) (runtime.Object, error) {
	// List.
	nsList := new(core.NamespaceList)
	err := h.APIReader.List(ctx, nsList,
		convertNamespaceListOptsFromProjectListOpts(opts))
	if err != nil {
		return nil, err
	}

	// Convert.
	eList := convertProjectListFromNamespaceList(nsList, opts)
	return eList, nil
}

func (h *ProjectHandler) OnWatch(ctx context.Context, opts ctrlcli.ListOptions) (watch.Interface, error) {
	// Watch.
	uw, err := h.Client.(ctrlcli.WithWatch).Watch(ctx, new(core.NamespaceList),
		convertNamespaceListOptsFromProjectListOpts(opts))
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
				ns, ok := e.Object.(*core.Namespace)
				if !ok {
					c <- e
					continue
				}

				// Process bookmark.
				if e.Type == watch.Bookmark {
					systemmeta.UnnoteResource(ns)
					e.Object = &server.Project{ObjectMeta: ns.ObjectMeta}
					c <- e
					continue
				}

				// Convert.
				proj := safeConvertProjectFromNamespace(ns, opts.Namespace)
				if proj == nil {
					continue
				}

				// Ignore if not be selected by `kubectl get --field-selector=metadata.namespace=...`.
				if !projectMatchFieldSelector(opts, proj) {
					continue
				}

				// Dispatch.
				e.Object = proj
				c <- e
			}
		}
	})

	return dw, nil
}

func (h *ProjectHandler) OnGet(ctx context.Context, key types.NamespacedName, opts ctrlcli.GetOptions) (runtime.Object, error) {
	// Get.
	ns := &core.Namespace{
		ObjectMeta: meta.ObjectMeta{
			Name: key.Name,
		},
	}
	err := h.APIReader.Get(ctx, ctrlcli.ObjectKeyFromObject(ns), ns, &opts)
	if err != nil {
		return nil, kerrors.NewNotFound(server.Resource(_ProjectResource), key.Name)
	}

	// Convert.
	proj := safeConvertProjectFromNamespace(ns, key.Namespace)
	if proj == nil {
		return nil, kerrors.NewNotFound(server.Resource(_ProjectResource), key.Name)
	}

	return proj, nil
}

func (h *ProjectHandler) OnUpdate(ctx context.Context, obj, oldObj runtime.Object, opts ctrlcli.UpdateOptions) (runtime.Object, error) {
	// Validate.
	proj := obj.(*server.Project)
	{
		var errs field.ErrorList
		if stringx.StringWidth(proj.Spec.DisplayName) > 30 {
			errs = append(errs, field.TooLong(
				field.NewPath("spec.displayName"), stringx.StringWidth(proj.Spec.DisplayName), 30))
		}
		if stringx.StringWidth(proj.Spec.Description) > 50 {
			errs = append(errs, field.TooLong(
				field.NewPath("spec.description"), stringx.StringWidth(proj.Spec.Description), 50))
		}
		if len(errs) > 0 {
			return nil, kerrors.NewInvalid(server.Kind(_ProjectResource), proj.Name, errs)
		}
	}

	// Update.
	{
		ns := convertNamespaceFromProject(proj)
		err := h.Client.Update(ctx, ns, &opts)
		if err != nil {
			return nil, err
		}
		proj = convertProjectFromNamespace(ns)
	}

	return proj, nil
}

func (h *ProjectHandler) OnDelete(ctx context.Context, obj runtime.Object, opts ctrlcli.DeleteOptions) error {
	proj := obj.(*server.Project)

	// Validate.
	{
		// Prevent deleting default project.
		if proj.Name == kuberess.DefaultProjectName {
			return kerrors.NewBadRequest(fmt.Sprintf("%s project is reserved", kuberess.DefaultProjectName))
		}
		// Prevent deleting if it has projects.
		// resList := new(server.ResourceList)
		// err := h.Client.List(ctx, resList, &ctrlcli.ListOptions{
		// 	Namespace: proj.Name,
		// })
		// if err != nil {
		// 	return kerrors.NewInternalError(fmt.Errorf("list resources below the project: %w", err))
		// }
		// if len(resList.Items) != 0 {
		// 	return kerrors.NewForbidden(server.SchemeResource(_ProjectResource), proj.Name,
		// 		errors.New("project has resources"))
		// }
	}

	// Delete.
	ns := &core.Namespace{ObjectMeta: proj.ObjectMeta}
	ns.Namespace = ""
	return h.Client.Delete(ctx, ns, &opts)
}

func convertNamespaceListOptsFromProjectListOpts(in ctrlcli.ListOptions) (out *ctrlcli.ListOptions) {
	// Ignore namespace selector.
	in.Namespace = ""
	if in.FieldSelector != nil {
		reqs := slices.DeleteFunc(in.FieldSelector.Requirements(), func(req fields.Requirement) bool {
			return req.Field == "metadata.namespace"
		})
		if len(reqs) == 0 {
			in.FieldSelector = nil
		} else {
			in.FieldSelector = kubemeta.FieldSelectorFromRequirements(reqs)
		}
	}

	// Add necessary label selector.
	if lbs := systemmeta.GetResourcesLabelSelectorOfType(_ProjectResource); in.LabelSelector == nil {
		in.LabelSelector = lbs
	} else {
		reqs, _ := lbs.Requirements()
		in.LabelSelector = in.LabelSelector.DeepCopySelector().Add(reqs...)
	}

	return &in
}

func convertNamespaceFromProject(proj *server.Project) *core.Namespace {
	ns := &core.Namespace{
		ObjectMeta: proj.ObjectMeta,
	}
	systemmeta.NoteResource(ns, _ProjectResource, map[string]string{
		"scope": "team",
		"team": kubemeta.GetNamespacedNameKey(types.NamespacedName{
			Namespace: kuberess.SystemNamespaceName,
			Name:      proj.Namespace,
		}),
		"displayName": proj.Spec.DisplayName,
		"description": proj.Spec.Description,
	})
	ns.Namespace = ""
	return ns
}

func convertProjectFromNamespace(ns *core.Namespace) *server.Project {
	if ns == nil {
		return nil
	}

	resType, notes := systemmeta.UnnoteResource(ns)
	if resType != _ProjectResource {
		return nil
	}

	ref := meta.GetControllerOf(ns)
	if ref == nil ||
		ref.APIVersion != server.GroupVersion.String() ||
		ref.Kind != "Team" {
		return nil
	}

	proj := &server.Project{
		ObjectMeta: ns.ObjectMeta,
		Spec: server.ProjectSpec{
			DisplayName: notes["displayName"],
			Description: notes["description"],
		},
		Status: server.ProjectStatus{
			Team:  ref.Name,
			Phase: ns.Status.Phase,
		},
	}
	proj.Namespace = ref.Name
	return proj
}

func safeConvertProjectFromNamespace(ns *core.Namespace, reqNamespace string) *server.Project {
	proj := convertProjectFromNamespace(ns)
	if proj != nil && reqNamespace != "" && reqNamespace != proj.Namespace {
		// NB(thxCode): sanitize if the project's namespace doesn't match requested namespace.
		proj = nil
	}
	return proj
}

func convertProjectListFromNamespaceList(nsList *core.NamespaceList, opts ctrlcli.ListOptions) *server.ProjectList {
	if nsList == nil {
		return &server.ProjectList{}
	}

	eList := &server.ProjectList{
		ListMeta: nsList.ListMeta,
		Items:    make([]server.Project, 0, len(nsList.Items)),
	}

	for i := range nsList.Items {
		proj := safeConvertProjectFromNamespace(&nsList.Items[i], opts.Namespace)
		if proj == nil {
			continue
		}
		// Ignore if not be selected by `kubectl get --field-selector=metadata.namespace=...`.
		if !projectMatchFieldSelector(opts, proj) {
			continue
		}
		eList.Items = append(eList.Items, *proj)
	}

	return eList
}

// projectMatchFieldSelector checks if the Project matches the field select in list options.
func projectMatchFieldSelector(opts ctrlcli.ListOptions, proj *server.Project) bool {
	fs := opts.FieldSelector
	if fs == nil {
		return true
	}
	return fs.Matches(fields.Set{"metadata.namespace": proj.Namespace, "metadata.name": proj.Name})
}
